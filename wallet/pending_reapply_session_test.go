package wallet

import (
	"errors"
	"testing"
	"time"
)

// 本文件为“待审批请求换用有效会话重试”补充回归保障：同一使用账户换用另一条
// 绑定不同设备的有效会话，按原请求编号和原申请内容重复提交时，有效会话只
// 意味着能取回原请求——首次受理确定的提交时间与等待截止时间不变，新会话
// 及其更长的有效期不能重新受理申请、不能延长原审批期限；恰到或超过原截止
// 时间的重复提交只能取回等待审批过期终态，过期留痕只追加一次。

// 各场景共享的时间安排：最长等待 10 分钟，策略窗口与出资账户审批会话覆盖
// 全部测试时刻；首次申请会话与替代会话的到期偏移由各场景指定。
const (
	pendingReapplyApprovalWait = 10 * time.Minute
	pendingReapplyPolicyEnd    = 5 * time.Hour
	pendingReapplyApproverTTL  = 5 * time.Hour
)

// setupPendingReapply 构造一笔待审批请求：出资账户 payer（余额 100），使用
// 账户 u1，申请会话 s1（绑定 dev1，session1TTL 后到期）与替代会话 s1b
// （绑定 dev1b，session2TTL 后到期），出资账户审批会话 sa；策略 p1 单次
// 上限 30、累计 1000、审批门槛 10、最长等待 10 分钟，时间窗覆盖全部测试
// 时刻。在基刻以 s1 提交 r1（预估 20 > 门槛 10）进入待审批，返回首次受理
// 视图与可原样重放的申请内容。等待截止时间为提交时刻、提交+10 分钟、策略
// 结束与 s1 到期四者中的最早值。
func setupPendingReapply(t *testing.T, session1TTL, session2TTL time.Duration) (*Wallet, *clock, RequestInput, RequestView) {
	t.Helper()
	w, c := newTestWallet()
	base := c.t
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", base.Add(session1TTL)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s1b", "u1", "dev1b", base.Add(session2TTL)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "dev-approve", base.Add(pendingReapplyApproverTTL)); err != nil {
		t.Fatal(err)
	}
	p := PolicySpec{
		ID:                "p1",
		PayerAccountID:    "payer",
		AllowedAccountIDs: []string{"u1"},
		Operation:         "charge",
		Payee:             "shop",
		StartsAt:          base.Add(-time.Hour),
		EndsAt:            base.Add(pendingReapplyPolicyEnd),
		MaxPerRequest:     30,
		MaxTotal:          1000,
		ApprovalThreshold: 10,
		ApprovalWait:      pendingReapplyApprovalWait,
	}
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	in := RequestInput{
		PolicyID:     "p1",
		RequestID:    "r1",
		AccountID:    "u1",
		SessionID:    "s1",
		DeviceID:     "dev1",
		Operation:    "charge",
		Payee:        "shop",
		EstimatedFee: 20,
	}
	first, err := w.Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != RequestPendingApproval {
		t.Fatalf("setup: first state = %v, want pending approval", first.State)
	}
	if !first.CreatedAt.Equal(base) {
		t.Fatalf("setup: created at = %v, want submission time %v", first.CreatedAt, base)
	}
	return w, c, in, first
}

// replayWithReplacement 把申请内容原样重放，只换用替代会话 s1b 与其设备。
func replayWithReplacement(in RequestInput) RequestInput {
	replay := in
	replay.SessionID = "s1b"
	replay.DeviceID = "dev1b"
	return replay
}

// assertPendingReplayUnchanged 断言有效重试只取回了原待审批请求：状态仍是
// 待审批，提交时间与等待截止时间保持首次受理的值；出资账户 100 可用、
// 0 预留，策略预留/已花费总额为 0，账本没有新增任何记录。
func assertPendingReplayUnchanged(t *testing.T, w *Wallet, got, first RequestView, ledgerLenBefore int) {
	t.Helper()
	if got.State != RequestPendingApproval {
		t.Fatalf("replay state = %v, want still pending approval (a valid retry must not re-accept or decide the request)", got.State)
	}
	if got.RequestID != "r1" || got.AccountID != "u1" || got.PolicyID != "p1" {
		t.Fatalf("replay returned a different request: %+v", got)
	}
	if !got.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("created at moved by retry: %v, want first acceptance %v", got.CreatedAt, first.CreatedAt)
	}
	if !got.WaitDeadline.Equal(first.WaitDeadline) {
		t.Fatalf("wait deadline extended by retry: %v, want original %v", got.WaitDeadline, first.WaitDeadline)
	}
	if got.DecidedAt != (time.Time{}) || got.ApproverAccountID != "" || got.RejectReason != "" {
		t.Fatalf("pending replay carries a decision: %+v", got)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("payer balance = %+v, want untouched {100 0}", bal)
	}
	if pv, _ := w.Policy("p1"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}
	if len(w.Ledger()) != ledgerLenBefore {
		t.Fatalf("ledger grew on valid pending retry: %d -> %d", ledgerLenBefore, len(w.Ledger()))
	}
}

// assertExpiredReplayOnce 断言原截止时间后的重复提交只取回等待审批过期终态：
// 提交时间与等待截止时间保持首次受理的值，账本较待审批阶段只追加了一条
// 关联使用账户与请求编号的零金额过期记录，没有预留、扣减或退款，资金与
// 额度不变。
func assertExpiredReplayOnce(t *testing.T, w *Wallet, got, first RequestView, ledgerLenBefore int) {
	t.Helper()
	if got.State != RequestExpired {
		t.Fatalf("replay at/after original deadline state = %v, want expired terminal (must not re-enter pending)", got.State)
	}
	if !got.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("expired request created at changed: %v, want %v", got.CreatedAt, first.CreatedAt)
	}
	if !got.WaitDeadline.Equal(first.WaitDeadline) {
		t.Fatalf("expired request wait deadline changed: %v, want %v", got.WaitDeadline, first.WaitDeadline)
	}
	if got.DecidedAt.IsZero() {
		t.Fatal("expired request should carry a decision time")
	}
	if got.RejectReason != "" || got.ApproverAccountID != "" {
		t.Fatalf("expiry must not look like a rejection: %+v", got)
	}
	if got.ReservedAt != (time.Time{}) || got.ActualFee != 0 {
		t.Fatalf("expired request must never have reserved or settled: %+v", got)
	}
	all := w.Ledger()
	if len(all) != ledgerLenBefore+1 {
		t.Fatalf("ledger entries = %d, want exactly one appended expiration (%d)", len(all), ledgerLenBefore+1)
	}
	if n := countRequestEntries(all, LedgerExpiration, "r1"); n != 1 {
		t.Fatalf("expiration entries = %d, want exactly one", n)
	}
	rec, ok := ledgerEntryFor(all, LedgerExpiration, "r1")
	if !ok {
		t.Fatal("missing LedgerExpiration entry")
	}
	if rec.AccountID != "u1" || rec.Amount != 0 {
		t.Fatalf("expiration entry = %+v, want zero-amount record linked to usage account u1", rec)
	}
	if n := countRequestEntries(all, LedgerReserve, "r1"); n != 0 {
		t.Fatalf("expiry produced %d reserve entries", n)
	}
	if n := countRequestEntries(all, LedgerSettle, "r1"); n != 0 {
		t.Fatalf("expiry produced %d settle entries", n)
	}
	if n := countRequestEntries(all, LedgerRefund, "r1"); n != 0 {
		t.Fatalf("expiry produced %d refund entries", n)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("payer balance = %+v, want untouched {100 0}", bal)
	}
	if pv, _ := w.Policy("p1"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}
}

// TestPendingReapplyReplacementSessionKeepsOriginalDeadline：首次申请会话
// （5 分钟到期）先于最长等待（10 分钟）到期，等待截止时间为提交后 5 分钟。
// 换用 20 分钟才到期、绑定另一台设备的有效会话重试：截止前只能取回原待
// 审批请求，提交时间与等待截止时间不变，不重新受理、不冻结费用、不追加
// 账本记录；恰到或超过原截止时间时只能取回过期终态，不能改用新会话的
// 到期时间或从重试时刻重新计算等待时长；过期后出资账户批准仍返回审批已
// 过期错误。
func TestPendingReapplyReplacementSessionKeepsOriginalDeadline(t *testing.T) {
	w, c, in, first := setupPendingReapply(t, 5*time.Minute, 20*time.Minute)
	base := first.CreatedAt
	wantDeadline := base.Add(5 * time.Minute) // min(提交+10m, 策略结束, s1 到期 T+5m)
	if !first.WaitDeadline.Equal(wantDeadline) {
		t.Fatalf("setup: wait deadline = %v, want first session expiry %v", first.WaitDeadline, wantDeadline)
	}
	ledgerLen := len(w.Ledger())

	// 提交后 2 分钟换用有效的新会话重试：新会话与新设备不算申请内容冲突，
	// 取回原待审批请求，受理信息不变。
	c.t = base.Add(2 * time.Minute)
	got, err := w.Apply(replayWithReplacement(in))
	if err != nil {
		t.Fatalf("retry with a different valid session must return the original request, got err = %v", err)
	}
	assertPendingReplayUnchanged(t, w, got, first, ledgerLen)

	// 原截止时间之前继续重试（T+4m，新会话与策略都仍有效）：仍只能取回
	// 原待审批结果，等待截止时间不被重试推迟。
	c.t = base.Add(4 * time.Minute)
	got, err = w.Apply(replayWithReplacement(in))
	if err != nil {
		t.Fatalf("second retry before original deadline: %v", err)
	}
	assertPendingReplayUnchanged(t, w, got, first, ledgerLen)

	// 恰到原截止时间（T+5m）：新会话还有 15 分钟有效期、策略窗口仍开放，
	// 但相同内容的重复申请只能返回过期终态。
	c.t = base.Add(5 * time.Minute)
	got, err = w.Apply(replayWithReplacement(in))
	if err != nil {
		t.Fatalf("retry at original deadline must return the expired view, got err = %v", err)
	}
	assertExpiredReplayOnce(t, w, got, first, ledgerLen)

	// 超过原截止时间（T+6m）再重试：仍是同一过期终态，过期留痕不重复追加。
	c.t = base.Add(6 * time.Minute)
	got, err = w.Apply(replayWithReplacement(in))
	if err != nil {
		t.Fatalf("retry after original deadline: %v", err)
	}
	assertExpiredReplayOnce(t, w, got, first, ledgerLen)

	// 过期后出资账户用自身有效会话批准：现有的审批已过期错误，资金与额度不变。
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("approve after expiry err = %v, want ErrApprovalExpired", err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("payer balance after failed approve = %+v, want {100 0}", bal)
	}
	if pv, _ := w.Policy("p1"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals after failed approve = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}
	assertExpiredReplayOnce(t, w, got, first, ledgerLen)
}

// TestPendingReapplyLongerSessionCannotExtendWaitDeadline：最长等待时长
// （10 分钟）先于首次申请会话到期（30 分钟）的同类条件——等待截止时间为
// 提交后 10 分钟。换用更长有效期（60 分钟）的会话重试，同样不能把首次
// 提交时确定的截止时间向后推。
func TestPendingReapplyLongerSessionCannotExtendWaitDeadline(t *testing.T) {
	w, c, in, first := setupPendingReapply(t, 30*time.Minute, 60*time.Minute)
	base := first.CreatedAt
	wantDeadline := base.Add(pendingReapplyApprovalWait) // min(提交+10m, s1 到期 T+30m, 策略结束)
	if !first.WaitDeadline.Equal(wantDeadline) {
		t.Fatalf("setup: wait deadline = %v, want submission+wait %v", first.WaitDeadline, wantDeadline)
	}
	ledgerLen := len(w.Ledger())

	// 截止前用更长的会话重试：取回原待审批请求，截止时间仍是提交+10 分钟。
	c.t = base.Add(5 * time.Minute)
	got, err := w.Apply(replayWithReplacement(in))
	if err != nil {
		t.Fatalf("retry with longer-lived session: %v", err)
	}
	assertPendingReplayUnchanged(t, w, got, first, ledgerLen)

	// 恰到原截止时间（T+10m）：首次会话与替代会话都还有效，仍只能取回
	// 过期终态，截止时间不能向后推。
	c.t = base.Add(10 * time.Minute)
	got, err = w.Apply(replayWithReplacement(in))
	if err != nil {
		t.Fatalf("retry at wait-based deadline: %v", err)
	}
	assertExpiredReplayOnce(t, w, got, first, ledgerLen)

	// 更晚重试（T+12m）与批准都只能是过期结果。
	c.t = base.Add(12 * time.Minute)
	got, err = w.Apply(replayWithReplacement(in))
	if err != nil {
		t.Fatalf("later retry: %v", err)
	}
	assertExpiredReplayOnce(t, w, got, first, ledgerLen)
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("approve after wait-based deadline err = %v, want ErrApprovalExpired", err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("payer balance = %+v, want {100 0}", bal)
	}
}
