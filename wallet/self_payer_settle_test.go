package wallet

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// selfPayerPolicy 构造一条“出资账户即使用账户”的策略：无需审批
// （ApprovalThreshold 为零）、未启用预留超时（MaxReserveDuration 为零），
// 时间窗覆盖整个测试时钟，限额足以同时容纳多笔预留。
func selfPayerPolicy(c *clock) PolicySpec {
	return PolicySpec{
		ID:                 "p",
		PayerAccountID:     "a",
		AllowedAccountIDs:  []string{"a"},
		Operation:          "charge",
		Payee:              "shop",
		StartsAt:           c.t.Add(-time.Hour),
		EndsAt:             c.t.Add(time.Hour),
		MaxPerRequest:      100,
		MaxTotal:           200,
		ApprovalThreshold:  0,
		MaxReserveDuration: 0,
	}
}

// setupSelfPayerWallet 准备一个账户 a（余额 100）：它既是策略 p 的出资账户，
// 又是唯一允许的使用账户；会话 s1 绑定 dev1，直接受理无需审批。
func setupSelfPayerWallet(t *testing.T) (*Wallet, *clock) {
	t.Helper()
	w, c := newTestWallet()
	mustAccount(t, w, "a", 100)
	if _, err := w.CreateSession("s1", "a", "dev1", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(selfPayerPolicy(c)); err != nil {
		t.Fatal(err)
	}
	return w, c
}

func selfApply(policyID, requestID string, fee int64) RequestInput {
	return RequestInput{
		PolicyID:     policyID,
		RequestID:    requestID,
		AccountID:    "a",
		SessionID:    "s1",
		DeviceID:     "dev1",
		Operation:    "charge",
		Payee:        "shop",
		EstimatedFee: fee,
	}
}

// ledgerCore 是账本比较使用的核心字段：类型、账户归属、关联请求/策略、金额与
// 时间。说明文本（Reason）不作为本回归的断言重点，统一忽略；相对顺序由切片
// 位置保证。
type ledgerCore struct {
	Kind      LedgerKind
	AccountID string
	RequestID string
	PolicyID  string
	Amount    int64
	At        time.Time
}

func cores(entries []LedgerEntry) []ledgerCore {
	out := make([]ledgerCore, len(entries))
	for i, e := range entries {
		out[i] = ledgerCore{
			Kind:      e.Kind,
			AccountID: e.AccountID,
			RequestID: e.RequestID,
			PolicyID:  e.PolicyID,
			Amount:    e.Amount,
			At:        e.At,
		}
	}
	return out
}

// assertLedger 比较实际账本与期望记录的类型、归属、关联、金额、时间及相对
// 顺序（忽略 Reason），不一致时输出差异并返回 false。
func assertLedger(t *testing.T, got, want []LedgerEntry) bool {
	t.Helper()
	if !reflect.DeepEqual(cores(got), cores(want)) {
		t.Errorf("ledger mismatch:\n got %+v\nwant %+v", cores(got), cores(want))
		return false
	}
	return true
}

// entryAt 构造带期望时间的账本记录（Reason 留空，比较时统一忽略）。
func entryAt(kind LedgerKind, accountID, requestID string, amount int64, at time.Time) LedgerEntry {
	return LedgerEntry{Kind: kind, AccountID: accountID, RequestID: requestID, Amount: amount, At: at}
}

// TestSelfPayerTwoReservationsSettleOne 覆盖同一账户既发起申请又承担费用时，
// 结算只能释放本笔预留：账户 a 初始余额 100，两笔直接受理的请求分别预留 40
// 和 20（可用 40、预留 60），将第一笔按实际费用 15 结算后，第二笔预留及其
// 提交/预留信息必须原样保留。
func TestSelfPayerTwoReservationsSettleOne(t *testing.T) {
	w, c := setupSelfPayerWallet(t)

	r1, err := w.Apply(selfApply("p", "r1", 40))
	if err != nil {
		t.Fatalf("apply r1: %v", err)
	}
	if r1.State != RequestReserved || r1.PayerAccountID != "a" || r1.AccountID != "a" {
		t.Fatalf("r1 = %+v, want reserved under self-payer account a", r1)
	}
	r2Before, err := w.Apply(selfApply("p", "r2", 20))
	if err != nil {
		t.Fatalf("apply r2: %v", err)
	}
	if r2Before.State != RequestReserved {
		t.Fatalf("r2 = %+v, want reserved", r2Before)
	}

	// 两笔预留共用同一份余额：可用 40、预留 60，只有一份余额、一笔费用各。
	if bal, _ := w.Balance("a"); bal != (Balances{Available: 40, Reserved: 60}) {
		t.Fatalf("balance after two reservations = %+v, want {40 60}", bal)
	}
	if pv, _ := w.Policy("p"); pv.ReservedTotal != 60 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals before settle = reserved %d spent %d, want 60/0", pv.ReservedTotal, pv.SpentTotal)
	}
	assertLedger(t, w.Ledger(), []LedgerEntry{
		entryAt(LedgerReserve, "a", "r1", 40, c.t),
		entryAt(LedgerReserve, "a", "r2", 20, c.t),
	})

	// 将第一笔按实际费用 15 结算：扣 15、退回差额 25，只释放本笔 40 的预留。
	applyAt := c.t
	c.t = c.t.Add(time.Minute)
	settled, err := w.Settle("a", "r1", 15)
	if err != nil {
		t.Fatalf("settle r1: %v", err)
	}
	if settled.State != RequestSettled || settled.EstimatedFee != 40 ||
		settled.ActualFee != 15 || !settled.SettledAt.Equal(c.t) {
		t.Fatalf("settled r1 = %+v, want settled estimated 40 actual 15 at %s", settled, c.t)
	}

	// 账户：可用 65（40+25）、预留只剩第二笔的 20。
	if bal, _ := w.Balance("a"); bal != (Balances{Available: 65, Reserved: 20}) {
		t.Fatalf("balance after settle = %+v, want {65 20}", bal)
	}
	// 策略：预留总额只剩 20，已花费总额为 15。
	if pv, _ := w.Policy("p"); pv.ReservedTotal != 20 || pv.SpentTotal != 15 {
		t.Fatalf("policy totals after settle = reserved %d spent %d, want 20/15", pv.ReservedTotal, pv.SpentTotal)
	}

	// 第二笔请求仍是原来的已预留状态：预估费用不变，实际费用与结算时间未被
	// 写入，提交与预留时刻等信息不被这次结算改写。
	r2After, err := w.Request("a", "r2")
	if err != nil {
		t.Fatalf("query r2: %v", err)
	}
	if !reflect.DeepEqual(r2After, r2Before) {
		t.Fatalf("r2 changed by settling r1:\nbefore %+v\nafter  %+v", r2Before, r2After)
	}
	if r2After.State != RequestReserved || r2After.EstimatedFee != 20 ||
		r2After.ActualFee != 0 || !r2After.SettledAt.IsZero() {
		t.Fatalf("r2 = %+v, want reserved fee 20 without settlement info", r2After)
	}

	// 账本：原两条预留保留，末尾依次追加实际扣减 15 与差额退回 25，均属于
	// 账户 a 并关联 r1；不产生任何其他记录。
	assertLedger(t, w.Ledger(), []LedgerEntry{
		entryAt(LedgerReserve, "a", "r1", 40, applyAt),
		entryAt(LedgerReserve, "a", "r2", 20, applyAt),
		entryAt(LedgerSettle, "a", "r1", 15, c.t),
		entryAt(LedgerRefund, "a", "r1", 25, c.t),
	})

	// 账户同时承担使用与出资两个角色，按账户查询既不能漏记录也不能重复列出：
	// 账户账本的内容与相对顺序必须与全部账本中属于该账户的记录完全一致。
	full := w.Ledger()
	accountLedger := w.AccountLedger("a")
	var filtered []LedgerEntry
	for _, e := range full {
		if e.AccountID == "a" {
			filtered = append(filtered, e)
		}
	}
	if !reflect.DeepEqual(accountLedger, filtered) {
		t.Fatalf("account ledger =\n%+v\nwant filtered full ledger\n%+v", accountLedger, filtered)
	}
	if len(accountLedger) != 4 {
		t.Fatalf("account ledger len = %d, want 4 (no missing or duplicated entries)", len(accountLedger))
	}
	// 末尾两条必须是本次结算的扣减与退款，不能把另一笔预留写成退款。
	if accountLedger[2].Kind != LedgerSettle || accountLedger[2].Amount != 15 ||
		accountLedger[2].RequestID != "r1" ||
		accountLedger[3].Kind != LedgerRefund || accountLedger[3].Amount != 25 ||
		accountLedger[3].RequestID != "r1" {
		t.Fatalf("settlement ledger entries = %+v %+v, want settle 15/refund 25 for r1",
			accountLedger[2], accountLedger[3])
	}

	// 第二笔按实际费用等于预估费用（20）结算：没有差额退回，20 作为已花费
	// 离开余额，最终可用 65、预留 0、已花费 35（65+35=100，资金守恒）。
	if _, err := w.Settle("a", "r2", 20); err != nil {
		t.Fatalf("settle r2 after r1: %v", err)
	}
	if bal, _ := w.Balance("a"); bal != (Balances{Available: 65, Reserved: 0}) {
		t.Fatalf("balance after settling r2 = %+v, want {65 0}", bal)
	}
	if pv, _ := w.Policy("p"); pv.ReservedTotal != 0 || pv.SpentTotal != 35 {
		t.Fatalf("policy totals after both settled = reserved %d spent %d, want 0/35",
			pv.ReservedTotal, pv.SpentTotal)
	}
}

// TestSelfPayerZeroFeeSettleRefundsFullReservation 覆盖实际费用为零的合法
// 结算：本笔预留全额退回，保留零金额扣减记录，策略已花费总额不增加，另一笔
// 预留照常保留。
func TestSelfPayerZeroFeeSettleRefundsFullReservation(t *testing.T) {
	w, c := setupSelfPayerWallet(t)

	if _, err := w.Apply(selfApply("p", "r1", 30)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(selfApply("p", "r2", 10)); err != nil {
		t.Fatal(err)
	}
	if bal, _ := w.Balance("a"); bal != (Balances{Available: 60, Reserved: 40}) {
		t.Fatalf("balance = %+v, want {60 40}", bal)
	}

	applyAt := c.t
	c.t = c.t.Add(time.Minute)
	settled, err := w.Settle("a", "r1", 0)
	if err != nil {
		t.Fatalf("zero-fee settle: %v", err)
	}
	if settled.State != RequestSettled || settled.ActualFee != 0 || !settled.SettledAt.Equal(c.t) {
		t.Fatalf("settled = %+v, want settled with zero actual fee", settled)
	}

	// 本笔 30 全额退回：可用 90、预留只剩另一笔的 10。
	if bal, _ := w.Balance("a"); bal != (Balances{Available: 90, Reserved: 10}) {
		t.Fatalf("balance after zero settle = %+v, want {90 10}", bal)
	}
	// 策略预留总额只剩另一笔的 10，已花费总额不增加。
	if pv, _ := w.Policy("p"); pv.ReservedTotal != 10 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 10/0", pv.ReservedTotal, pv.SpentTotal)
	}
	// 另一笔预留不受影响。
	if r2, _ := w.Request("a", "r2"); r2.State != RequestReserved || r2.EstimatedFee != 10 {
		t.Fatalf("r2 = %+v, want reserved fee 10", r2)
	}

	// 账本：零金额扣减记录保留，随后是本笔全额退款 30；两条都关联 r1。
	assertLedger(t, w.Ledger(), []LedgerEntry{
		entryAt(LedgerReserve, "a", "r1", 30, applyAt),
		entryAt(LedgerReserve, "a", "r2", 10, applyAt),
		entryAt(LedgerSettle, "a", "r1", 0, c.t),
		entryAt(LedgerRefund, "a", "r1", 30, c.t),
	})
	if n := len(w.AccountLedger("a")); n != 4 {
		t.Fatalf("account ledger len = %d, want 4 entries each listed once", n)
	}
}

// TestSelfPayerInvalidSettleKeepsReservation 覆盖结算金额边界：实际费用为负
// 返回金额非法，超过本笔预估费用返回超额结算错误；失败后本笔仍为已预留，
// 实际费用与结算时间不写入，账户余额、策略累计与既有账本都不变化。
func TestSelfPayerInvalidSettleKeepsReservation(t *testing.T) {
	w, c := setupSelfPayerWallet(t)

	if _, err := w.Apply(selfApply("p", "r1", 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(selfApply("p", "r2", 20)); err != nil {
		t.Fatal(err)
	}

	ledgerBefore := w.Ledger()
	c.t = c.t.Add(time.Minute)

	if _, err := w.Settle("a", "r1", -1); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("negative settle err = %v, want ErrInvalidAmount", err)
	}
	if _, err := w.Settle("a", "r1", 41); !errors.Is(err, ErrSettleTooLarge) {
		t.Fatalf("over-estimate settle err = %v, want ErrSettleTooLarge", err)
	}

	// 本笔仍为已预留，实际费用与结算时间未被写入。
	r1, _ := w.Request("a", "r1")
	if r1.State != RequestReserved || r1.ActualFee != 0 || !r1.SettledAt.IsZero() {
		t.Fatalf("r1 after failed settles = %+v, want reserved without settlement info", r1)
	}
	// 账户余额不变：可用 40、预留 60。
	if bal, _ := w.Balance("a"); bal != (Balances{Available: 40, Reserved: 60}) {
		t.Fatalf("balance changed after failed settles = %+v, want {40 60}", bal)
	}
	// 策略累计不变。
	if pv, _ := w.Policy("p"); pv.ReservedTotal != 60 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals changed = reserved %d spent %d, want 60/0",
			pv.ReservedTotal, pv.SpentTotal)
	}
	// 既有账本不增不改。
	assertLedger(t, w.Ledger(), ledgerBefore)
	// 另一笔预留同样保持原状。
	if r2, _ := w.Request("a", "r2"); r2.State != RequestReserved || r2.EstimatedFee != 20 {
		t.Fatalf("r2 = %+v, want reserved fee 20", r2)
	}

	// 失败后合法结算仍可进行，且只释放本笔预留：r1 按全额 40 结算，无差额
	// 退回（可用保持 40），预留只剩 r2 的 20。
	if _, err := w.Settle("a", "r1", 40); err != nil {
		t.Fatalf("valid settle after failures: %v", err)
	}
	if bal, _ := w.Balance("a"); bal != (Balances{Available: 40, Reserved: 20}) {
		t.Fatalf("balance after valid settle = %+v, want {40 20}", bal)
	}
}

// TestDistinctPayerSettleBehaviorKept 锁定出资账户与使用账户不同的既有结算
// 行为：资金记录只归出资账户，使用账户的账本不出现资金记录。
func TestDistinctPayerSettleBehaviorKept(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	spec := selfPayerPolicy(c)
	spec.PayerAccountID = "payer"
	spec.AllowedAccountIDs = []string{"u1"}
	if err := w.SavePolicy(spec); err != nil {
		t.Fatal(err)
	}

	in := selfApply("p", "r1", 20)
	in.AccountID = "u1"
	in.SessionID = "s1"
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	applyAt := c.t
	c.t = c.t.Add(time.Minute)
	if settled, err := w.Settle("u1", "r1", 5); err != nil || settled.State != RequestSettled {
		t.Fatalf("settle = %+v, %v", settled, err)
	}

	assertLedger(t, w.AccountLedger("payer"), []LedgerEntry{
		entryAt(LedgerReserve, "payer", "r1", 20, applyAt),
		entryAt(LedgerSettle, "payer", "r1", 5, c.t),
		entryAt(LedgerRefund, "payer", "r1", 15, c.t),
	})
	// 直接受理不经过审批，使用账户名下没有任何状态或资金记录。
	if got := w.AccountLedger("u1"); len(got) != 0 {
		t.Fatalf("using account ledger = %+v, want no entries", got)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 95, Reserved: 0}) {
		t.Fatalf("payer balance = %+v, want {95 0}", bal)
	}
	if bal, _ := w.Balance("u1"); bal != (Balances{Available: 0, Reserved: 0}) {
		t.Fatalf("using account balance = %+v, want untouched {0 0}", bal)
	}
}
