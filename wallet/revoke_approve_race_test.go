package wallet

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// applyPendingForRace 按本文件共用的夹具提交一笔待审批请求：预估费用 20、
// 审批门槛 10，出资账户 payer 可用 100，钱包内无其他请求，策略单次与共享
// 限额均足够，预留超时关闭。申请会话 s1 属于使用账户 u1，审批会话 sa 属于
// 出资账户 payer，设备绑定正确；返回提交时刻与等待截止时刻供后续比对。
func applyPendingForRace(t *testing.T, w *Wallet) (createdAt, waitDeadline time.Time) {
	t.Helper()
	req, err := w.Apply(approvalApply())
	if err != nil {
		t.Fatal(err)
	}
	if req.State != RequestPendingApproval {
		t.Fatalf("state = %v, want pending approval", req.State)
	}
	return req.CreatedAt, req.WaitDeadline
}

// checkTimingKept 断言请求的提交时刻与等待截止时刻保持申请时的值。
func checkTimingKept(t *testing.T, got RequestView, createdAt, waitDeadline time.Time) {
	t.Helper()
	if !got.CreatedAt.Equal(createdAt) || !got.WaitDeadline.Equal(waitDeadline) {
		t.Fatalf("timing changed: created %v/%v deadline %v/%v",
			got.CreatedAt, createdAt, got.WaitDeadline, waitDeadline)
	}
}

// checkSessionsAfterRace 断言两项操作结束后申请会话已吊销、审批会话仍有效。
func checkSessionsAfterRace(t *testing.T, w *Wallet) {
	t.Helper()
	s1, err := w.Session("s1")
	if err != nil {
		t.Fatal(err)
	}
	if s1.State != SessionRevoked {
		t.Fatalf("applicant session state = %v, want revoked", s1.State)
	}
	sa, err := w.Session("sa")
	if err != nil {
		t.Fatal(err)
	}
	if sa.State != SessionActive {
		t.Fatalf("approver session state = %v, want still active", sa.State)
	}
}

// checkApproveWonOutcome 校验“批准先成功”的完整结果：请求保持已预留并保留
// 批准决定与实际预留时间；出资账户 80 可用 / 20 预留；策略预留 20、已花费 0；
// 账本新增一次全额预留与一次零金额批准记录，无吊销拒绝或退款记录。
func checkApproveWonOutcome(t *testing.T, w *Wallet, createdAt, waitDeadline, approveAt time.Time) {
	t.Helper()

	got, err := w.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestReserved {
		t.Fatalf("state = %v, want reserved after approve-then-revoke", got.State)
	}
	if got.ApproverAccountID != "payer" {
		t.Fatalf("approver = %q, want payer's approval decision kept", got.ApproverAccountID)
	}
	if !got.DecidedAt.Equal(approveAt) {
		t.Fatalf("decided at %v, want approve time %v", got.DecidedAt, approveAt)
	}
	if !got.ReservedAt.Equal(approveAt) {
		t.Fatalf("reserved at %v, want approve time %v", got.ReservedAt, approveAt)
	}
	if got.RejectReason != "" {
		t.Fatalf("reserved request must not carry reject reason: %q", got.RejectReason)
	}
	checkTimingKept(t, got, createdAt, waitDeadline)

	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance = %+v, want {80 20}; revocation must not release the reservation", bal)
	}
	if pv, _ := w.Policy("p-approval"); pv.ReservedTotal != 20 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 20/0", pv.ReservedTotal, pv.SpentTotal)
	}

	// 账本：申请时的待审批记录之外，恰好新增一次全额预留与一次零金额批准。
	all := w.Ledger()
	if len(all) != 3 {
		t.Fatalf("ledger entries = %d, want 3 (pending + reserve + approval)", len(all))
	}
	res, ok := ledgerEntryFor(all, LedgerReserve, "r1")
	if !ok {
		t.Fatal("missing LedgerReserve entry")
	}
	if res.AccountID != "payer" || res.Amount != 20 || !res.At.Equal(approveAt) {
		t.Fatalf("reserve entry = %+v, want payer full-amount 20 at approve time", res)
	}
	app, ok := ledgerEntryFor(all, LedgerApproval, "r1")
	if !ok {
		t.Fatal("missing LedgerApproval entry")
	}
	if app.AccountID != "u1" || app.Amount != 0 || !app.At.Equal(approveAt) {
		t.Fatalf("approval entry = %+v, want zero-amount entry for u1 at approve time", app)
	}
	if n := countKind(all, LedgerRejection); n != 0 {
		t.Fatalf("rejection entries = %d, want none: approved request must not be revoked-rejected", n)
	}
	if n := countKind(all, LedgerRefund); n != 0 {
		t.Fatalf("refund entries = %d, want none: revocation must not refund the reservation", n)
	}
}

// checkRevokeWonOutcome 校验“吊销先完成”的完整结果：请求进入拒绝终态，理由
// 说明申请会话已被吊销，决定时间为吊销时刻，审批账户为空；出资账户 100 可用 /
// 0 预留；策略预留与已花费均为 0；账本只新增一次零金额拒绝记录。
func checkRevokeWonOutcome(t *testing.T, w *Wallet, createdAt, waitDeadline, revokeAt time.Time) {
	t.Helper()

	got, err := w.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestRejected {
		t.Fatalf("state = %v, want rejected after revoke-then-approve", got.State)
	}
	if got.RejectReason != sessionRevokedRejectReason {
		t.Fatalf("reject reason = %q, want %q", got.RejectReason, sessionRevokedRejectReason)
	}
	if !got.DecidedAt.Equal(revokeAt) {
		t.Fatalf("decided at %v, want revoke time %v", got.DecidedAt, revokeAt)
	}
	if got.ApproverAccountID != "" {
		t.Fatalf("revocation rejection must not fill approver, got %q", got.ApproverAccountID)
	}
	if !got.ReservedAt.IsZero() {
		t.Fatalf("rejected request must never have been reserved, reserved at %v", got.ReservedAt)
	}
	checkTimingKept(t, got, createdAt, waitDeadline)

	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {100 0}; rejected request must not freeze funds", bal)
	}
	if pv, _ := w.Policy("p-approval"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}

	// 账本：申请时的待审批记录之外，只新增一次零金额拒绝记录。
	all := w.Ledger()
	if len(all) != 2 {
		t.Fatalf("ledger entries = %d, want 2 (pending + rejection)", len(all))
	}
	rej, ok := ledgerEntryFor(all, LedgerRejection, "r1")
	if !ok {
		t.Fatal("missing LedgerRejection entry")
	}
	if rej.AccountID != "u1" || rej.Amount != 0 || rej.Reason != sessionRevokedRejectReason || !rej.At.Equal(revokeAt) {
		t.Fatalf("rejection entry = %+v, want zero-amount revocation rejection for u1 at revoke time", rej)
	}
	for _, kind := range []LedgerKind{LedgerApproval, LedgerReserve, LedgerRefund} {
		if n := countKind(all, kind); n != 0 {
			t.Fatalf("ledger kind %v entries = %d, want none after revocation rejection", kind, n)
		}
	}
}

// TestApproveThenRevokeKeepsReservation：批准先成功时请求保持已预留，随后的
// 吊销只改变申请会话状态，不释放已预留费用、不追加拒绝或退款记录。
func TestApproveThenRevokeKeepsReservation(t *testing.T) {
	w, c := setupApproval(t)
	createdAt, waitDeadline := applyPendingForRace(t, w)

	// 未到会话到期、未到等待期限、策略在授权时间窗内时批准。
	c.t = c.t.Add(10 * time.Second)
	approveAt := c.t
	got, err := w.Approve("u1", "r1", "sa", "dev-approve")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestReserved || got.ApproverAccountID != "payer" {
		t.Fatalf("approve view = %+v, want reserved by payer", got)
	}

	// 吊销提交申请所用的会话：只改变会话状态，不动已预留费用。
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	checkSessionsAfterRace(t, w)
	checkApproveWonOutcome(t, w, createdAt, waitDeadline, approveAt)
}

// TestRevokeThenApproveRejected：吊销先完成时请求进入拒绝终态；随后用仍然
// 有效的出资账户会话批准，返回请求不处于待审批状态错误，且不改变任何结果。
func TestRevokeThenApproveRejected(t *testing.T) {
	w, c := setupApproval(t)
	createdAt, waitDeadline := applyPendingForRace(t, w)

	c.t = c.t.Add(10 * time.Second)
	revokeAt := c.t
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	checkSessionsAfterRace(t, w)
	checkRevokeWonOutcome(t, w, createdAt, waitDeadline, revokeAt)

	// 出资账户的审批会话始终有效：不能把申请会话被吊销误当成审批人无权
	// 操作，错误必须是请求不处于待审批状态，而不是无权审批。
	before := len(w.Ledger())
	_, approveErr := w.Approve("u1", "r1", "sa", "dev-approve")
	if !errors.Is(approveErr, ErrRequestNotPending) {
		t.Fatalf("approve after revocation err = %v, want ErrRequestNotPending", approveErr)
	}
	if errors.Is(approveErr, ErrNotApprover) {
		t.Fatal("approver session is still valid; must not report ErrNotApprover")
	}
	if len(w.Ledger()) != before {
		t.Fatalf("failed approve grew ledger %d -> %d", before, len(w.Ledger()))
	}
	// 失败的批准不改变任何已结清的结果。
	checkRevokeWonOutcome(t, w, createdAt, waitDeadline, revokeAt)
}

// TestConcurrentRevokeAndApprove：吊销与批准同时竞争同一笔待审批请求时，
// 只能出现“批准先成功”或“吊销先完成”其中一种完整结果；两项调用结束后申请
// 会话均已吊销，请求提交时间与等待截止时间不变，调用结果、请求查询、余额、
// 策略金额与账本彼此一致。
func TestConcurrentRevokeAndApprove(t *testing.T) {
	const iterations = 32
	outcomes := map[string]int{}
	for i := 0; i < iterations; i++ {
		w, c := setupApproval(t)
		createdAt, waitDeadline := applyPendingForRace(t, w)
		c.t = c.t.Add(10 * time.Second)
		opAt := c.t

		start := make(chan struct{})
		var wg sync.WaitGroup
		var approveErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, approveErr = w.Approve("u1", "r1", "sa", "dev-approve")
		}()
		var revokeErr error
		go func() {
			defer wg.Done()
			<-start
			revokeErr = w.RevokeSession("s1")
		}()
		close(start)
		wg.Wait()

		// 吊销本身总是成功；无论哪种结果，申请会话都已吊销、审批会话仍有效。
		if revokeErr != nil {
			t.Fatalf("iter %d: revoke err = %v", i, revokeErr)
		}
		checkSessionsAfterRace(t, w)

		switch {
		case approveErr == nil:
			// 批准先成功：吊销只改变会话状态，预留完整保留。
			outcomes["approve-first"]++
			checkApproveWonOutcome(t, w, createdAt, waitDeadline, opAt)
		case errors.Is(approveErr, ErrRequestNotPending):
			// 吊销先完成：请求已拒绝，批准按非待审批失败。
			outcomes["revoke-first"]++
			checkRevokeWonOutcome(t, w, createdAt, waitDeadline, opAt)
		default:
			t.Fatalf("iter %d: approve err = %v, want nil or ErrRequestNotPending", i, approveErr)
		}
	}
	t.Logf("outcomes: %v", outcomes)
}
