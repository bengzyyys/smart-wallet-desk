package wallet

import (
	"errors"
	"testing"
	"time"
)

// 本文件为“同一账户既发起申请又承担代付费用”的结算功能补充回归保障：
// 策略允许把出资账户列入允许使用的账户，同一笔请求的使用账户与出资账户
// 因此可以相同；账户承担两个角色时仍只有一份余额和一笔费用。固定场景
// （见 setupSelfPayerSettle）采用无需审批、未启用预留超时的策略，围绕
// 已经直接受理并完成预留的请求结算：
//   - 账户 solo 初始余额 100，既是策略 p-self 的出资账户也是唯一允许的
//     使用账户，持有绑定自有设备的有效会话 s-self；
//   - 策略 p-self 关闭审批（门槛为零）、关闭预留超时（时长为零），
//     单次上限 50、累计上限 100；
//   - solo 提交两笔申请：r-self-1 预估 40、r-self-2 预估 20，均直接受理
//     并预留。此时可用余额 40、预留余额 60，策略预留总额 60、已花费 0。
const (
	selfPayerAccount  = "solo"
	selfPayerPolicy   = "p-self"
	selfPayerSession  = "s-self"
	selfPayerDevice   = "d-self"
	selfPayerRequest1 = "r-self-1"
	selfPayerRequest2 = "r-self-2"

	selfPayerInit   int64 = 100
	selfPayerFee1   int64 = 40
	selfPayerFee2   int64 = 20
	selfPayerActual int64 = 15
)

// setupSelfPayerSettle 构造上述固定场景并断言起点：两笔请求均已预留、
// 各自记录提交与预留时刻；solo 可用 40 / 预留 60；策略预留 60、已花费 0；
// 账本恰有两条 solo 的预留记录。返回两笔申请各自的提交时刻（相同）。
func setupSelfPayerSettle(t *testing.T) (*Wallet, *clock, time.Time) {
	t.Helper()
	w, c := newTestWallet()
	mustAccount(t, w, selfPayerAccount, selfPayerInit)
	if _, err := w.CreateSession(selfPayerSession, selfPayerAccount, selfPayerDevice, c.t.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 出资账户同时列入允许使用的账户；关闭审批与预留超时。
	if err := w.SavePolicy(PolicySpec{
		ID:                selfPayerPolicy,
		PayerAccountID:    selfPayerAccount,
		AllowedAccountIDs: []string{selfPayerAccount},
		Operation:         "charge",
		Payee:             "shop",
		StartsAt:          c.t.Add(-time.Hour),
		EndsAt:            c.t.Add(24 * time.Hour),
		MaxPerRequest:     50,
		MaxTotal:          100,
		// ApprovalThreshold 与 MaxReserveDuration 留空（零）：无需审批、
		// 未启用预留超时。
	}); err != nil {
		t.Fatal(err)
	}

	apply := func(requestID string, fee int64) RequestView {
		req, err := w.Apply(RequestInput{
			PolicyID:     selfPayerPolicy,
			RequestID:    requestID,
			AccountID:    selfPayerAccount,
			SessionID:    selfPayerSession,
			DeviceID:     selfPayerDevice,
			Operation:    "charge",
			Payee:        "shop",
			EstimatedFee: fee,
		})
		if err != nil {
			t.Fatalf("apply %s: %v", requestID, err)
		}
		return req
	}
	createdAt := c.t
	r1 := apply(selfPayerRequest1, selfPayerFee1)
	r2 := apply(selfPayerRequest2, selfPayerFee2)
	for _, r := range []RequestView{r1, r2} {
		if r.State != RequestReserved {
			t.Fatalf("request %s state = %v, want reserved", r.RequestID, r.State)
		}
		if r.PayerAccountID != selfPayerAccount || r.AccountID != selfPayerAccount {
			t.Fatalf("request %s must have the same account as user and payer: %+v", r.RequestID, r)
		}
		if !r.CreatedAt.Equal(createdAt) || !r.ReservedAt.Equal(createdAt) {
			t.Fatalf("request %s timing = created %v reserved %v, want %v",
				r.RequestID, r.CreatedAt, r.ReservedAt, createdAt)
		}
		// 直接受理：无审批决定信息；未启用预留超时：无截止与释放时刻。
		if !r.WaitDeadline.IsZero() || !r.DecidedAt.IsZero() || r.ApproverAccountID != "" {
			t.Fatalf("directly accepted request %s must carry no approval fields: %+v", r.RequestID, r)
		}
		if r.ReserveDuration != 0 || !r.ReserveDeadline.IsZero() || !r.ReserveExpiredAt.IsZero() {
			t.Fatalf("request %s must have no reserve-timeout timing: %+v", r.RequestID, r)
		}
	}

	if bal, _ := w.Balance(selfPayerAccount); bal != (Balances{Available: 40, Reserved: 60}) {
		t.Fatalf("balance after two applies = %+v, want {40 60}", bal)
	}
	if pv, _ := w.Policy(selfPayerPolicy); pv.ReservedTotal != 60 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals after two applies = reserved %d spent %d, want 60/0",
			pv.ReservedTotal, pv.SpentTotal)
	}
	all := w.Ledger()
	if len(all) != 2 {
		t.Fatalf("ledger after applies = %d entries, want 2 reserves: %+v", len(all), all)
	}
	for i, want := range []struct {
		requestID string
		amount    int64
	}{{selfPayerRequest1, selfPayerFee1}, {selfPayerRequest2, selfPayerFee2}} {
		e := all[i]
		if e.Kind != LedgerReserve || e.AccountID != selfPayerAccount ||
			e.RequestID != want.requestID || e.Amount != want.amount {
			t.Fatalf("reserve entry %d = %+v, want reserve %d for %s by %s",
				i, e, want.amount, want.requestID, selfPayerAccount)
		}
	}
	return w, c, createdAt
}

// checkSelfPayerLedger 校验账本归属与次序：全部账本与按账户查询的结果都
// 恰好包含 want 中按顺序给出的记录；账户同时承担出资与使用两个角色时，
// 每条记录只出现一次，既不漏记也不重复，两种查询的内容与相对顺序一致。
func checkSelfPayerLedger(t *testing.T, w *Wallet, want []LedgerEntry) {
	t.Helper()
	all := w.Ledger()
	if len(all) != len(want) {
		t.Fatalf("ledger entries = %d, want %d: %+v", len(all), len(want), all)
	}
	for i, we := range want {
		e := all[i]
		if e.Kind != we.Kind || e.AccountID != selfPayerAccount ||
			e.RequestID != we.RequestID || e.Amount != we.Amount {
			t.Fatalf("ledger entry %d = %+v, want kind %v request %s amount %d for %s",
				i, e, we.Kind, we.RequestID, we.Amount, selfPayerAccount)
		}
	}
	// 全部记录都属于这个身兼两职的账户：按账户查询必须逐条一致地返回，
	// 不能漏记录或重复列出。
	perAccount := w.AccountLedger(selfPayerAccount)
	if len(perAccount) != len(all) {
		t.Fatalf("account ledger entries = %d, want %d (same as full ledger): %+v",
			len(perAccount), len(all), perAccount)
	}
	for i := range all {
		if perAccount[i] != all[i] {
			t.Fatalf("account ledger entry %d = %+v, want identical to full ledger entry %+v",
				i, perAccount[i], all[i])
		}
	}
}

// TestSelfPayerSettleReleasesOnlyThisRequest 是主场景：同一账户在同一策略下
// 还有另一笔未结算预留时，结算只能释放本笔费用。第一笔按实际费用 15 结算后：
// 该请求成为已结算，保留预估 40、显示实际 15 与结算时间；账户可用 65、
// 预留 20；策略预留总额 20、已花费 15；另一笔请求保持原来的预留状态与金额，
// 提交与预留信息不被改写。
func TestSelfPayerSettleReleasesOnlyThisRequest(t *testing.T) {
	w, c, createdAt := setupSelfPayerSettle(t)

	settleAt := createdAt.Add(10 * time.Second)
	c.t = settleAt
	settled, err := w.Settle(selfPayerAccount, selfPayerRequest1, selfPayerActual)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != RequestSettled {
		t.Fatalf("settled state = %v, want settled", settled.State)
	}
	if settled.EstimatedFee != selfPayerFee1 || settled.ActualFee != selfPayerActual {
		t.Fatalf("settled fees = estimated %d actual %d, want %d/%d",
			settled.EstimatedFee, settled.ActualFee, selfPayerFee1, selfPayerActual)
	}
	if !settled.SettledAt.Equal(settleAt) {
		t.Fatalf("settled at = %v, want %v", settled.SettledAt, settleAt)
	}
	// 提交与预留信息不因结算被改写。
	if !settled.CreatedAt.Equal(createdAt) || !settled.ReservedAt.Equal(createdAt) {
		t.Fatalf("settled request timing changed: created %v reserved %v, want %v",
			settled.CreatedAt, settled.ReservedAt, createdAt)
	}

	// 账户只有一份余额：可用 65（100 - 20 预留 - 15 实扣）、预留 20。
	if bal, _ := w.Balance(selfPayerAccount); bal != (Balances{Available: 65, Reserved: 20}) {
		t.Fatalf("balance after settle = %+v, want {65 20}", bal)
	}
	// 策略只释放本笔预留：预留总额 20、已花费总额 15。
	if pv, _ := w.Policy(selfPayerPolicy); pv.ReservedTotal != 20 || pv.SpentTotal != 15 {
		t.Fatalf("policy totals after settle = reserved %d spent %d, want 20/15",
			pv.ReservedTotal, pv.SpentTotal)
	}

	// 另一笔请求仍保持原来的预留状态和金额，提交与预留信息不被改写。
	other, err := w.Request(selfPayerAccount, selfPayerRequest2)
	if err != nil {
		t.Fatal(err)
	}
	if other.State != RequestReserved || other.EstimatedFee != selfPayerFee2 {
		t.Fatalf("other request = %+v, want still reserved with estimated %d", other, selfPayerFee2)
	}
	if other.ActualFee != 0 || !other.SettledAt.IsZero() {
		t.Fatalf("other request must not gain settle info: %+v", other)
	}
	if !other.CreatedAt.Equal(createdAt) || !other.ReservedAt.Equal(createdAt) {
		t.Fatalf("other request timing changed: created %v reserved %v, want %v",
			other.CreatedAt, other.ReservedAt, createdAt)
	}

	// 账本：原有预留记录保留，末尾按现有次序增加实际扣减 15 和差额退回 25，
	// 两条记录都属于这个账户并关联被结算的请求；另一笔预留不能被写成本次退款。
	checkSelfPayerLedger(t, w, []LedgerEntry{
		{Kind: LedgerReserve, RequestID: selfPayerRequest1, Amount: 40},
		{Kind: LedgerReserve, RequestID: selfPayerRequest2, Amount: 20},
		{Kind: LedgerSettle, RequestID: selfPayerRequest1, Amount: 15},
		{Kind: LedgerRefund, RequestID: selfPayerRequest1, Amount: 25},
	})
	if got := countKind(w.Ledger(), LedgerRefund); got != 1 {
		t.Fatalf("refund entries = %d, want exactly 1 (only for the settled request)", got)
	}
	if _, ok := ledgerEntryFor(w.Ledger(), LedgerRefund, selfPayerRequest2); ok {
		t.Fatal("the other reservation must not be recorded as a refund of this settlement")
	}
}

// TestSelfPayerSettleZeroFeeRefundsWholeReservation 覆盖边界：实际费用为零
// 是合法结算，全额退回本笔预留，保留零金额扣减记录，策略已花费总额不增加，
// 另一笔预留照常保留。
func TestSelfPayerSettleZeroFeeRefundsWholeReservation(t *testing.T) {
	w, c, createdAt := setupSelfPayerSettle(t)

	settleAt := createdAt.Add(10 * time.Second)
	c.t = settleAt
	settled, err := w.Settle(selfPayerAccount, selfPayerRequest1, 0)
	if err != nil {
		t.Fatalf("zero-fee settle: %v", err)
	}
	if settled.State != RequestSettled || settled.ActualFee != 0 || !settled.SettledAt.Equal(settleAt) {
		t.Fatalf("zero-fee settled view = %+v", settled)
	}

	// 全额退回本笔预留：可用 80（100 - 20 预留）、预留 20；已花费不增加。
	if bal, _ := w.Balance(selfPayerAccount); bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance after zero-fee settle = %+v, want {80 20}", bal)
	}
	if pv, _ := w.Policy(selfPayerPolicy); pv.ReservedTotal != 20 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals after zero-fee settle = reserved %d spent %d, want 20/0",
			pv.ReservedTotal, pv.SpentTotal)
	}

	// 另一笔预留照常保留。
	other, err := w.Request(selfPayerAccount, selfPayerRequest2)
	if err != nil {
		t.Fatal(err)
	}
	if other.State != RequestReserved || other.EstimatedFee != selfPayerFee2 {
		t.Fatalf("other request = %+v, want still reserved with estimated %d", other, selfPayerFee2)
	}

	// 账本保留零金额扣减记录与全额退回记录。
	checkSelfPayerLedger(t, w, []LedgerEntry{
		{Kind: LedgerReserve, RequestID: selfPayerRequest1, Amount: 40},
		{Kind: LedgerReserve, RequestID: selfPayerRequest2, Amount: 20},
		{Kind: LedgerSettle, RequestID: selfPayerRequest1, Amount: 0},
		{Kind: LedgerRefund, RequestID: selfPayerRequest1, Amount: 40},
	})
	settleEntry, ok := ledgerEntryFor(w.Ledger(), LedgerSettle, selfPayerRequest1)
	if !ok {
		t.Fatal("zero-amount settle entry must be kept")
	}
	if settleEntry.Amount != 0 || settleEntry.AccountID != selfPayerAccount {
		t.Fatalf("zero-amount settle entry = %+v", settleEntry)
	}
}

// TestSelfPayerSettleInvalidFeesKeepEverything 覆盖边界：实际费用为负或超过
// 本笔预估费用时，分别返回现有的金额非法或超额结算错误；本笔仍为已预留，
// 实际费用和结算时间不被写入，账户余额、策略累计金额及既有账本都不因失败
// 而变化。
func TestSelfPayerSettleInvalidFeesKeepEverything(t *testing.T) {
	w, c, createdAt := setupSelfPayerSettle(t)

	c.t = createdAt.Add(10 * time.Second)
	if _, err := w.Settle(selfPayerAccount, selfPayerRequest1, -1); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("negative actual fee err = %v, want ErrInvalidAmount", err)
	}
	if _, err := w.Settle(selfPayerAccount, selfPayerRequest1, selfPayerFee1+1); !errors.Is(err, ErrSettleTooLarge) {
		t.Fatalf("over-estimate actual fee err = %v, want ErrSettleTooLarge", err)
	}

	// 本笔仍为已预留，实际费用与结算时间不被写入。
	req, err := w.Request(selfPayerAccount, selfPayerRequest1)
	if err != nil {
		t.Fatal(err)
	}
	if req.State != RequestReserved {
		t.Fatalf("state after failed settles = %v, want still reserved", req.State)
	}
	if req.ActualFee != 0 || !req.SettledAt.IsZero() {
		t.Fatalf("failed settles must not write actual fee or settle time: %+v", req)
	}
	// 账户余额、策略累计金额及既有账本都不因失败而变化。
	if bal, _ := w.Balance(selfPayerAccount); bal != (Balances{Available: 40, Reserved: 60}) {
		t.Fatalf("balance after failed settles = %+v, want {40 60}", bal)
	}
	if pv, _ := w.Policy(selfPayerPolicy); pv.ReservedTotal != 60 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals after failed settles = reserved %d spent %d, want 60/0",
			pv.ReservedTotal, pv.SpentTotal)
	}
	checkSelfPayerLedger(t, w, []LedgerEntry{
		{Kind: LedgerReserve, RequestID: selfPayerRequest1, Amount: 40},
		{Kind: LedgerReserve, RequestID: selfPayerRequest2, Amount: 20},
	})

	// 失败后合法结算仍可进行。
	if _, err := w.Settle(selfPayerAccount, selfPayerRequest1, selfPayerActual); err != nil {
		t.Fatalf("settle after failed attempts: %v", err)
	}
	if bal, _ := w.Balance(selfPayerAccount); bal != (Balances{Available: 65, Reserved: 20}) {
		t.Fatalf("balance after eventual settle = %+v, want {65 20}", bal)
	}
}

// TestDistinctPayerSettleStillWorks 保障出资账户与使用账户不同的既有结算
// 行为继续保持：费用只在出资账户上划转，使用账户余额不受影响。
func TestDistinctPayerSettleStillWorks(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(PolicySpec{
		ID:                "p1",
		PayerAccountID:    "payer",
		AllowedAccountIDs: []string{"u1"},
		Operation:         "charge",
		Payee:             "shop",
		StartsAt:          c.t.Add(-time.Hour),
		EndsAt:            c.t.Add(24 * time.Hour),
		MaxPerRequest:     50,
		MaxTotal:          100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(RequestInput{
		PolicyID:     "p1",
		RequestID:    "r1",
		AccountID:    "u1",
		SessionID:    "s1",
		DeviceID:     "dev1",
		Operation:    "charge",
		Payee:        "shop",
		EstimatedFee: 40,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Settle("u1", "r1", 15); err != nil {
		t.Fatal(err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 85, Reserved: 0}) {
		t.Fatalf("payer balance = %+v, want {85 0}", bal)
	}
	if bal, _ := w.Balance("u1"); bal != (Balances{Available: 0, Reserved: 0}) {
		t.Fatalf("user balance must stay untouched: %+v", bal)
	}
	if pv, _ := w.Policy("p1"); pv.ReservedTotal != 0 || pv.SpentTotal != 15 {
		t.Fatalf("policy totals = reserved %d spent %d, want 0/15", pv.ReservedTotal, pv.SpentTotal)
	}
	// 资金记录归属出资账户，使用账户名下没有资金记录。
	payerLedger := w.AccountLedger("payer")
	if len(payerLedger) != 3 {
		t.Fatalf("payer ledger = %+v, want reserve/settle/refund", payerLedger)
	}
	for i, want := range []struct {
		kind   LedgerKind
		amount int64
	}{{LedgerReserve, 40}, {LedgerSettle, 15}, {LedgerRefund, 25}} {
		if payerLedger[i].Kind != want.kind || payerLedger[i].Amount != want.amount ||
			payerLedger[i].RequestID != "r1" {
			t.Fatalf("payer ledger entry %d = %+v, want kind %v amount %d", i, payerLedger[i], want.kind, want.amount)
		}
	}
	if got := w.AccountLedger("u1"); len(got) != 0 {
		t.Fatalf("user ledger must have no money entries: %+v", got)
	}
}
