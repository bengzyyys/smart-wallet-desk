package wallet

import (
	"errors"
	"testing"
	"time"
)

// 本文件锁定“重复提交仍须通过会话校验”的回归行为：同一使用账户用相同
// 请求编号重复申请时，会话检查先于幂等命中——失效（吊销/到期）会话不能
// 借重复申请取回已有结果，失败只新增一条零金额申请拒绝记录，不改写原
// 请求、余额、策略累计与既有账本；换用同属该账户的有效会话（设备随之
// 切换）则正常命中幂等，返回首次受理结果且不重复预留。

// reservedApply 构造一笔会被直接受理（费用 20 不超过单次上限 30、策略未
// 开启审批）的申请内容。
func reservedApply() RequestInput {
	return RequestInput{
		PolicyID:     "p1",
		RequestID:    "r1",
		AccountID:    "u1",
		SessionID:    "s1",
		DeviceID:     "dev1",
		Operation:    "charge",
		Payee:        "shop",
		EstimatedFee: 20,
	}
}

// setupReserved 构造一笔已成功预留费用的请求：出资账户 payer（100）、
// 使用账户 u1、会话 s1（绑定 dev1，30 分钟到期，先于策略窗口结束），
// 策略 p1 在测试时钟内有效、余额与额度充足、未启用预留超时。
func setupReserved(t *testing.T) (*Wallet, *clock, RequestView) {
	t.Helper()
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(validPolicy(c)); err != nil {
		t.Fatal(err)
	}
	req, err := w.Apply(reservedApply())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if req.State != RequestReserved {
		t.Fatalf("state = %v, want reserved", req.State)
	}
	return w, c, req
}

// assertReservedUntouched 校验一次失败的重复申请（错误为 applyErr）没有
// 改写任何既有状态：原请求仍为已预留且提交/预留时间不变、不携带任何审批
// 拒绝信息；出资账户可用/预留余额与策略预留/已花费总额不变；账本只新增
// 一条关联 u1/r1 的零金额拒绝记录（原因即本次会话失败），既有预留记录
// 保留，没有退款、扣减或新的预留。
func assertReservedUntouched(t *testing.T, w *Wallet, c *clock, first RequestView, ledgerBefore int, applyErr error) {
	t.Helper()

	got, err := w.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestReserved {
		t.Fatalf("state = %v, want reserved: failed duplicate must not rewrite the original request", got.State)
	}
	if !got.CreatedAt.Equal(first.CreatedAt) || !got.ReservedAt.Equal(first.ReservedAt) {
		t.Fatalf("timing rewritten: created %v/%v reserved %v/%v",
			got.CreatedAt, first.CreatedAt, got.ReservedAt, first.ReservedAt)
	}
	if got.RejectReason != "" || !got.DecidedAt.IsZero() || got.ApproverAccountID != "" {
		t.Fatalf("request must not look approval-rejected: %+v", got)
	}

	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance = %+v, want {80 20}", bal)
	}
	if pv, _ := w.Policy("p1"); pv.ReservedTotal != 20 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 20/0", pv.ReservedTotal, pv.SpentTotal)
	}

	all := w.Ledger()
	if len(all) != ledgerBefore+1 {
		t.Fatalf("ledger entries = %d, want %d (exactly one new rejection record)", len(all), ledgerBefore+1)
	}
	last := all[len(all)-1]
	if last.Kind != LedgerRejection || last.AccountID != "u1" || last.RequestID != "r1" || last.Amount != 0 {
		t.Fatalf("last entry = %+v, want zero-amount rejection linked to u1/r1", last)
	}
	if last.Reason != applyErr.Error() {
		t.Fatalf("rejection reason %q, want the session failure %q", last.Reason, applyErr)
	}
	if !last.At.Equal(c.t) {
		t.Fatalf("rejection at %v, want failed-apply time %v", last.At, c.t)
	}
	if countKind(all, LedgerReserve) != 1 || countKind(all, LedgerRefund) != 0 || countKind(all, LedgerSettle) != 0 {
		t.Fatalf("fund entries changed: reserve %d refund %d settle %d, want 1/0/0",
			countKind(all, LedgerReserve), countKind(all, LedgerRefund), countKind(all, LedgerSettle))
	}
}

// TestDuplicateApplyRevokedSession：申请会话被吊销后，用原会话提交相同
// 申请返回会话已吊销错误，而不是取回已受理结果；失败不改变原请求、余额
// 与策略累计，只留下零金额拒绝记录。
func TestDuplicateApplyRevokedSession(t *testing.T) {
	w, c, first := setupReserved(t)
	ledgerBefore := len(w.Ledger())

	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	// 吊销本身不影响已预留请求，也不留痕。
	if len(w.Ledger()) != ledgerBefore {
		t.Fatalf("revoke appended ledger entries: %d -> %d", ledgerBefore, len(w.Ledger()))
	}

	_, err := w.Apply(reservedApply())
	if !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("duplicate apply err = %v, want ErrSessionRevoked", err)
	}
	assertReservedUntouched(t, w, c, first, ledgerBefore, err)

	// 原预留真实保留：仍可正常结算。
	if _, err := w.Settle("u1", "r1", 20); err != nil {
		t.Fatalf("settle original reservation after failed duplicate: %v", err)
	}
}

// TestDuplicateApplyRevokedSessionReserveTimeoutEnabled：策略启用预留超时
// 但预留尚未到期时，吊销会话的重复申请同样被拒绝，且预留计时信息不变。
func TestDuplicateApplyRevokedSessionReserveTimeoutEnabled(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	p := validPolicy(c)
	p.MaxReserveDuration = 2 * time.Hour
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	first, err := w.Apply(reservedApply())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if first.State != RequestReserved || first.ReserveDeadline.IsZero() {
		t.Fatalf("state = %v deadline = %v, want reserved with deadline", first.State, first.ReserveDeadline)
	}
	ledgerBefore := len(w.Ledger())

	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	_, applyErr := w.Apply(reservedApply())
	if !errors.Is(applyErr, ErrSessionRevoked) {
		t.Fatalf("duplicate apply err = %v, want ErrSessionRevoked", applyErr)
	}

	got, _ := w.Request("u1", "r1")
	if got.State != RequestReserved {
		t.Fatalf("state = %v, want reserved (reservation not yet due)", got.State)
	}
	if !got.ReservedAt.Equal(first.ReservedAt) || !got.ReserveDeadline.Equal(first.ReserveDeadline) ||
		got.ReserveDuration != first.ReserveDuration || !got.ReserveExpiredAt.IsZero() {
		t.Fatalf("reservation timing rewritten: %+v vs %+v", got, first)
	}
	assertReservedUntouched(t, w, c, first, ledgerBefore, applyErr)
}

// TestDuplicateApplyExpiredSession：会话自然到期后，相同申请返回会话已
// 过期错误；到期时刻本身即算过期。
func TestDuplicateApplyExpiredSession(t *testing.T) {
	cases := []struct {
		name    string
		advance func(expiry time.Time) time.Time
	}{
		{"exactly at expiry", func(expiry time.Time) time.Time { return expiry }},
		{"after expiry", func(expiry time.Time) time.Time { return expiry.Add(time.Minute) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, c, first := setupReserved(t)
			ledgerBefore := len(w.Ledger())

			sess, err := w.Session("s1")
			if err != nil {
				t.Fatal(err)
			}
			c.t = tc.advance(sess.ExpiresAt)

			_, err = w.Apply(reservedApply())
			if !errors.Is(err, ErrSessionExpired) {
				t.Fatalf("duplicate apply err = %v, want ErrSessionExpired", err)
			}
			assertReservedUntouched(t, w, c, first, ledgerBefore, err)
		})
	}
}

// TestDuplicateApplyRevokedAndExpired：会话同时已吊销和已到期时，仍按
// 现有校验顺序报告已吊销。
func TestDuplicateApplyRevokedAndExpired(t *testing.T) {
	w, c, first := setupReserved(t)
	ledgerBefore := len(w.Ledger())

	// 先让会话自然到期，再吊销（已到期会话允许吊销）。
	c.t = c.t.Add(45 * time.Minute)
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}

	_, err := w.Apply(reservedApply())
	if !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("duplicate apply err = %v, want ErrSessionRevoked (revoked reported before expired)", err)
	}
	assertReservedUntouched(t, w, c, first, ledgerBefore, err)
}

// TestDuplicateApplyReplacementSession：原会话失效后，换用同一使用账户的
// 另一条有效会话（设备改为该会话绑定的设备）、其余内容保持一致，重复提交
// 命中幂等返回原请求：会话编号与首次不同不构成内容冲突，首次受理信息
// 不变，不再次冻结费用，不追加账本记录。
func TestDuplicateApplyReplacementSession(t *testing.T) {
	w, c, first := setupReserved(t)

	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s2", "u1", "dev2", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ledgerBefore := len(w.Ledger())

	in := reservedApply()
	in.SessionID = "s2"
	in.DeviceID = "dev2"
	got, err := w.Apply(in)
	if err != nil {
		t.Fatalf("duplicate apply with replacement session: %v", err)
	}
	if got.State != RequestReserved {
		t.Fatalf("state = %v, want the original reserved request", got.State)
	}
	if !got.CreatedAt.Equal(first.CreatedAt) || !got.ReservedAt.Equal(first.ReservedAt) ||
		got.EstimatedFee != first.EstimatedFee {
		t.Fatalf("first acceptance rewritten: %+v vs %+v", got, first)
	}

	// 不再次冻结费用、不追加账本记录。
	if len(w.Ledger()) != ledgerBefore {
		t.Fatalf("ledger grew on idempotent hit: %d -> %d", ledgerBefore, len(w.Ledger()))
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance = %+v, want {80 20} (no second reservation)", bal)
	}
	if pv, _ := w.Policy("p1"); pv.ReservedTotal != 20 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = %d/%d, want 20/0", pv.ReservedTotal, pv.SpentTotal)
	}
}

// TestDuplicateApplySettledRequest：已结算的请求遵守同一规则——失效会话
// 重试仍被拒绝并留拒绝记录，有效替代会话重试返回原已结算结果，实际费用
// 与结算时间保持不变。
func TestDuplicateApplySettledRequest(t *testing.T) {
	w, c, first := setupReserved(t)

	settled, err := w.Settle("u1", "r1", 12)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != RequestSettled || settled.ActualFee != 12 {
		t.Fatalf("settled view = %+v", settled)
	}
	ledgerAfterSettle := len(w.Ledger())

	// 失效会话重试：拒绝，已结算结果与资金不变。
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(reservedApply()); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("duplicate apply err = %v, want ErrSessionRevoked", err)
	}
	got, _ := w.Request("u1", "r1")
	if got.State != RequestSettled || got.ActualFee != 12 || !got.SettledAt.Equal(settled.SettledAt) {
		t.Fatalf("settled request rewritten: %+v", got)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 88, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {88 0}", bal)
	}
	if pv, _ := w.Policy("p1"); pv.ReservedTotal != 0 || pv.SpentTotal != 12 {
		t.Fatalf("policy totals = %d/%d, want 0/12", pv.ReservedTotal, pv.SpentTotal)
	}
	all := w.Ledger()
	if len(all) != ledgerAfterSettle+1 {
		t.Fatalf("ledger entries = %d, want %d (one rejection record)", len(all), ledgerAfterSettle+1)
	}
	last := all[len(all)-1]
	if last.Kind != LedgerRejection || last.AccountID != "u1" || last.RequestID != "r1" || last.Amount != 0 {
		t.Fatalf("last entry = %+v, want zero-amount rejection linked to u1/r1", last)
	}

	// 有效替代会话重试：返回原已结算结果，不追加账本记录。
	if _, err := w.CreateSession("s2", "u1", "dev2", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	in := reservedApply()
	in.SessionID = "s2"
	in.DeviceID = "dev2"
	dup, err := w.Apply(in)
	if err != nil {
		t.Fatalf("duplicate apply with replacement session: %v", err)
	}
	if dup.State != RequestSettled || dup.ActualFee != 12 || !dup.SettledAt.Equal(settled.SettledAt) {
		t.Fatalf("settled result changed: %+v, want actual 12 settled at %v", dup, settled.SettledAt)
	}
	if !dup.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("created time rewritten: %v vs %v", dup.CreatedAt, first.CreatedAt)
	}
	if len(w.Ledger()) != ledgerAfterSettle+1 {
		t.Fatalf("ledger grew on idempotent hit: %d -> %d", ledgerAfterSettle+1, len(w.Ledger()))
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 88, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {88 0}", bal)
	}
}

// TestDuplicateApplyConflictAfterSessionCheck：通过会话校验后，预估费用与
// 原申请不同仍返回冲突错误，不覆盖原请求、不再次预留；会话校验本身先于
// 内容比较，失效会话即使内容不同也先报会话错误。
func TestDuplicateApplyConflictAfterSessionCheck(t *testing.T) {
	w, c, first := setupReserved(t)

	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s2", "u1", "dev2", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ledgerBefore := len(w.Ledger())

	// 有效替代会话 + 不同预估费用：冲突。
	in := reservedApply()
	in.SessionID = "s2"
	in.DeviceID = "dev2"
	in.EstimatedFee = 25
	if _, err := w.Apply(in); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate apply with different fee err = %v, want ErrConflict", err)
	}

	// 失效会话 + 不同内容：会话检查在前，仍报会话错误而非冲突。
	in.SessionID = "s1"
	in.DeviceID = "dev1"
	if _, err := w.Apply(in); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("duplicate apply with revoked session err = %v, want ErrSessionRevoked", err)
	}

	// 原请求不被覆盖：费用、状态、计时不变，不再次预留。
	got, _ := w.Request("u1", "r1")
	if got.State != RequestReserved || got.EstimatedFee != 20 ||
		!got.CreatedAt.Equal(first.CreatedAt) || !got.ReservedAt.Equal(first.ReservedAt) {
		t.Fatalf("original request overwritten: %+v", got)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance = %+v, want {80 20} (no second reservation)", bal)
	}
	if pv, _ := w.Policy("p1"); pv.ReservedTotal != 20 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = %d/%d, want 20/0", pv.ReservedTotal, pv.SpentTotal)
	}
	// 冲突不记账；只有失效会话那次留下一条拒绝记录。
	all := w.Ledger()
	if len(all) != ledgerBefore+1 {
		t.Fatalf("ledger entries = %d, want %d (conflict records nothing)", len(all), ledgerBefore+1)
	}
	if countKind(all, LedgerReserve) != 1 {
		t.Fatalf("reserve entries = %d, want 1", countKind(all, LedgerReserve))
	}
}
