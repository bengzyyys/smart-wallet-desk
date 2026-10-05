package wallet

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件覆盖申请会话吊销与大额批准竞争同一笔待审批请求的回归场景：
// 两种合法的先后结果（批准先成功 / 吊销先完成）都必须完整且彼此一致，
// 不允许出现“请求已拒绝却仍冻结费用”或“批准成功后费用被吊销退回”。

// setupRevokeApproveRace 构造一笔费用 20（门槛 10）的待审批请求，出资账户
// 可用余额 100，随后把时钟推进到仍在会话到期与等待期限之内的时刻。
func setupRevokeApproveRace(t *testing.T) (*Wallet, *clock, RequestView) {
	t.Helper()
	w, c := setupApproval(t)
	req, err := w.Apply(approvalApply())
	if err != nil {
		t.Fatal(err)
	}
	if req.State != RequestPendingApproval {
		t.Fatalf("state = %v, want pending approval", req.State)
	}
	// 推进 10 秒：未到会话到期（1 分钟）与等待截止（1 分钟），策略仍在
	// 授权时间窗内且未停用，预留超时关闭。
	c.t = c.t.Add(10 * time.Second)
	return w, c, req
}

// TestApproveThenRevokeKeepsReservation：批准先成功，请求保持已预留；随后
// 吊销只改变申请会话的状态，不动已预留费用、批准决定与账本。
func TestApproveThenRevokeKeepsReservation(t *testing.T) {
	w, c, req := setupRevokeApproveRace(t)
	createdAt, waitDeadline := req.CreatedAt, req.WaitDeadline

	approveAt := c.t
	got, err := w.Approve("u1", "r1", "sa", "dev-approve")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestReserved {
		t.Fatalf("state = %v, want reserved", got.State)
	}
	if got.ApproverAccountID != "payer" || !got.DecidedAt.Equal(approveAt) || !got.ReservedAt.Equal(approveAt) {
		t.Fatalf("approval decision/timing wrong: %+v", got)
	}
	before := len(w.Ledger())

	// 吊销申请会话：只改变会话状态，不影响已预留请求。
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	if sv, _ := w.Session("s1"); sv.State != SessionRevoked {
		t.Fatalf("application session state = %v, want revoked", sv.State)
	}
	if sv, _ := w.Session("sa"); sv.State != SessionActive {
		t.Fatalf("payer session state = %v, want still active", sv.State)
	}

	// 请求保持已预留，批准决定与实际预留时间原样保留，提交时间与等待
	// 截止时间不变。
	view, err := w.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if view.State != RequestReserved {
		t.Fatalf("state after revoke = %v, want still reserved", view.State)
	}
	if view.ApproverAccountID != "payer" || !view.DecidedAt.Equal(approveAt) || !view.ReservedAt.Equal(approveAt) {
		t.Fatalf("revoke rewrote approval decision: %+v", view)
	}
	if view.RejectReason != "" {
		t.Fatalf("reserved request carries reject reason %q", view.RejectReason)
	}
	if !view.CreatedAt.Equal(createdAt) || !view.WaitDeadline.Equal(waitDeadline) {
		t.Fatalf("timing changed: created %v/%v deadline %v/%v",
			view.CreatedAt, createdAt, view.WaitDeadline, waitDeadline)
	}

	// 已预留费用不因吊销提前退回：余额与策略金额保持批准后的结果。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance = %+v, want {80 20}", bal)
	}
	if pv, _ := w.Policy("p-approval"); pv.ReservedTotal != 20 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 20/0", pv.ReservedTotal, pv.SpentTotal)
	}

	// 账本：吊销不新增任何记录；全程只有待审批、全额预留与零金额批准
	// 各一条，不出现吊销拒绝或退款记录。
	all := w.Ledger()
	if len(all) != before {
		t.Fatalf("ledger grew on revoke: %d -> %d", before, len(all))
	}
	if e, ok := ledgerEntryFor(all, LedgerReserve, "r1"); !ok || e.AccountID != "payer" || e.Amount != 20 {
		t.Fatalf("reserve entry wrong: %+v ok=%v", e, ok)
	}
	if e, ok := ledgerEntryFor(all, LedgerApproval, "r1"); !ok || e.AccountID != "u1" || e.Amount != 0 || !e.At.Equal(approveAt) {
		t.Fatalf("approval entry wrong: %+v ok=%v", e, ok)
	}
	if n := countKind(all, LedgerRejection); n != 0 {
		t.Fatalf("rejection entries = %d, want 0", n)
	}
	if n := countKind(all, LedgerRefund); n != 0 {
		t.Fatalf("refund entries = %d, want 0 (revoke must not release reserved fee)", n)
	}
}

// TestRevokeThenApproveReturnsNotPending：吊销先完成，请求进入拒绝终态；
// 随后用仍然有效的出资账户会话批准，返回请求不处于待审批状态错误，且不
// 产生任何资金变动。
func TestRevokeThenApproveReturnsNotPending(t *testing.T) {
	w, c, req := setupRevokeApproveRace(t)
	createdAt, waitDeadline := req.CreatedAt, req.WaitDeadline

	revokeAt := c.t
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}

	// 请求进入拒绝终态：理由说明申请会话已被吊销，决定时间为吊销时刻，
	// 审批账户为空，提交时间与等待截止时间不变。
	view, err := w.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if view.State != RequestRejected {
		t.Fatalf("state = %v, want rejected", view.State)
	}
	if view.RejectReason != sessionRevokedRejectReason {
		t.Fatalf("reject reason = %q, want %q", view.RejectReason, sessionRevokedRejectReason)
	}
	if !view.DecidedAt.Equal(revokeAt) {
		t.Fatalf("decided at %v, want revoke time %v", view.DecidedAt, revokeAt)
	}
	if view.ApproverAccountID != "" {
		t.Fatalf("revocation rejection must not fill approver, got %q", view.ApproverAccountID)
	}
	if !view.ReservedAt.IsZero() {
		t.Fatalf("rejected request must never have reserved, reserved at %v", view.ReservedAt)
	}
	if !view.CreatedAt.Equal(createdAt) || !view.WaitDeadline.Equal(waitDeadline) {
		t.Fatalf("timing changed: created %v/%v deadline %v/%v",
			view.CreatedAt, createdAt, view.WaitDeadline, waitDeadline)
	}

	// 出资账户的审批会话始终有效：吊销的是申请会话，不能误当成审批人
	// 无权操作；批准应返回请求不处于待审批状态错误。
	before := len(w.Ledger())
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrRequestNotPending) {
		t.Fatalf("approve after revoke err = %v, want ErrRequestNotPending", err)
	}
	if sv, _ := w.Session("sa"); sv.State != SessionActive {
		t.Fatalf("payer session state = %v, want still active", sv.State)
	}

	// 待审批从未冻结费用，吊销拒绝也不退款：余额与策略金额保持原样。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want untouched {100 0}", bal)
	}
	if pv, _ := w.Policy("p-approval"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}

	// 账本：吊销只新增一条零金额拒绝记录，失败的批准不再留痕；全程不
	// 产生批准、预留或退款记录。
	all := w.Ledger()
	if len(all) != before {
		t.Fatalf("ledger grew on failed approve: %d -> %d", before, len(all))
	}
	if n := countKind(all, LedgerRejection); n != 1 {
		t.Fatalf("rejection entries = %d, want exactly 1", n)
	}
	e, ok := ledgerEntryFor(all, LedgerRejection, "r1")
	if !ok || e.AccountID != "u1" || e.Amount != 0 || e.Reason != sessionRevokedRejectReason || !e.At.Equal(revokeAt) {
		t.Fatalf("rejection entry wrong: %+v ok=%v", e, ok)
	}
	for _, kind := range []LedgerKind{LedgerApproval, LedgerReserve, LedgerRefund} {
		if n := countKind(all, kind); n != 0 {
			t.Fatalf("ledger kind %v entries = %d, want 0", kind, n)
		}
	}
}

// TestConcurrentRevokeAndApprove：吊销与批准同时发起时，只能出现其中一种
// 完整结果——批准先成功则请求已预留且吊销不动费用，吊销先完成则请求拒绝
// 终态且批准失败；不允许出现混合状态。
func TestConcurrentRevokeAndApprove(t *testing.T) {
	for i := 0; i < 16; i++ {
		w, _, req := setupRevokeApproveRace(t)
		createdAt, waitDeadline := req.CreatedAt, req.WaitDeadline

		start := make(chan struct{})
		var wg sync.WaitGroup
		var approveErr, revokeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, approveErr = w.Approve("u1", "r1", "sa", "dev-approve")
		}()
		go func() {
			defer wg.Done()
			<-start
			revokeErr = w.RevokeSession("s1")
		}()
		close(start)
		wg.Wait()

		// 吊销调用本身总是成功：两项调用结束后申请会话均已吊销。
		if revokeErr != nil {
			t.Fatalf("revoke err = %v", revokeErr)
		}
		if sv, _ := w.Session("s1"); sv.State != SessionRevoked {
			t.Fatalf("application session state = %v, want revoked", sv.State)
		}

		view, err := w.Request("u1", "r1")
		if err != nil {
			t.Fatal(err)
		}
		// 无论哪种结果，提交时间与等待截止时间都保持不变。
		if !view.CreatedAt.Equal(createdAt) || !view.WaitDeadline.Equal(waitDeadline) {
			t.Fatalf("timing changed: created %v/%v deadline %v/%v",
				view.CreatedAt, createdAt, view.WaitDeadline, waitDeadline)
		}

		bal, _ := w.Balance("payer")
		pv, _ := w.Policy("p-approval")
		all := w.Ledger()
		switch {
		case approveErr == nil:
			// 批准先成功：请求已预留，吊销只改变会话状态。
			if view.State != RequestReserved {
				t.Fatalf("approve succeeded but state = %v", view.State)
			}
			if view.ApproverAccountID != "payer" || view.ReservedAt.IsZero() {
				t.Fatalf("approval decision missing: %+v", view)
			}
			if bal != (Balances{Available: 80, Reserved: 20}) {
				t.Fatalf("balance = %+v, want {80 20}", bal)
			}
			if pv.ReservedTotal != 20 || pv.SpentTotal != 0 {
				t.Fatalf("policy totals = reserved %d spent %d, want 20/0", pv.ReservedTotal, pv.SpentTotal)
			}
			if countKind(all, LedgerReserve) != 1 || countKind(all, LedgerApproval) != 1 {
				t.Fatalf("approved outcome must have exactly one reserve and one approval entry: %+v", all)
			}
			if countKind(all, LedgerRejection) != 0 || countKind(all, LedgerRefund) != 0 {
				t.Fatalf("approved outcome must have no rejection/refund entries: %+v", all)
			}
		case errors.Is(approveErr, ErrRequestNotPending):
			// 吊销先完成：请求拒绝终态，批准失败，费用从未冻结。
			if view.State != RequestRejected {
				t.Fatalf("revoke won but state = %v", view.State)
			}
			if view.RejectReason != sessionRevokedRejectReason || view.ApproverAccountID != "" {
				t.Fatalf("revocation rejection fields wrong: %+v", view)
			}
			if bal != (Balances{Available: 100, Reserved: 0}) {
				t.Fatalf("balance = %+v, want untouched {100 0}", bal)
			}
			if pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
				t.Fatalf("policy totals = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
			}
			if countKind(all, LedgerRejection) != 1 {
				t.Fatalf("rejected outcome must have exactly one rejection entry: %+v", all)
			}
			if countKind(all, LedgerApproval) != 0 || countKind(all, LedgerReserve) != 0 || countKind(all, LedgerRefund) != 0 {
				t.Fatalf("rejected outcome must have no approval/reserve/refund entries: %+v", all)
			}
		default:
			t.Fatalf("unexpected approve err = %v", approveErr)
		}
	}
}
