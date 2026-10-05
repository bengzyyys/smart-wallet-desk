package wallet

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件回归“出资账户用于审批的会话被吊销”与“批准代付请求”同时发生时的
// 行为。它与 revoke_approve_race_test.go 的关键区别在于：那里吊销的是提交
// 申请的使用账户会话 s1（吊销会把该会话仍在待审批的请求按等待期限结清）；
// 这里吊销的是出资账户 payer 用于审批的会话 sa，而提交申请的 s1 始终有效。
// 吊销审批会话只影响“谁还能作出批准决定”，绝不能被当成申请会话被吊销而把
// 请求拒绝，也不能释放或补写已预留请求的任何资金与记录。

// setupApproverSessionRevoke 在标准审批夹具上再为出资账户创建第二个绑定
// 当前审批设备（dev-approve）的有效会话 sa2：吊销 sa 后可用它继续审批，
// 以验证换用同账户的另一有效会话不受影响，而已吊销的 sa 不能靠重复调用
// 绕过会话校验。
func setupApproverSessionRevoke(t *testing.T) (*Wallet, *clock) {
	t.Helper()
	w, c := setupApproval(t)
	if _, err := w.CreateSession("sa2", "payer", "dev-approve", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return w, c
}

// checkApproverAndApplicantSessions 断言两项操作结束后：出资账户的审批会话
// sa 已吊销，而提交申请所用的使用账户会话 s1 仍有效。吊销审批会话不能被
// 误记成申请会话失效。
func checkApproverAndApplicantSessions(t *testing.T, w *Wallet) {
	t.Helper()
	sa, err := w.Session("sa")
	if err != nil {
		t.Fatal(err)
	}
	if sa.State != SessionRevoked {
		t.Fatalf("approver session state = %v, want revoked", sa.State)
	}
	s1, err := w.Session("s1")
	if err != nil {
		t.Fatal(err)
	}
	if s1.State != SessionActive {
		t.Fatalf("application session state = %v, want still active", s1.State)
	}
}

// checkRequestStillPendingUntouched 校验“审批会话吊销先完成”的完整结果：
// 请求仍在待审批，不填写任何批准决定、不预留费用；出资账户余额与策略金额
// 保持申请后的数值（100 可用 / 0 预留，策略预留与已花费均为 0）；账本只
// 保留申请时的一条待审批留痕，不补写拒绝、取消、过期或任何资金记录；
// 原提交时间与等待截止时间不变。
func checkRequestStillPendingUntouched(t *testing.T, w *Wallet, createdAt, waitDeadline time.Time) {
	t.Helper()

	got, err := w.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestPendingApproval {
		t.Fatalf("state = %v, want still pending approval (approver-session revoke must not reject it)", got.State)
	}
	if got.ApproverAccountID != "" || !got.DecidedAt.IsZero() || got.RejectReason != "" {
		t.Fatalf("pending request must carry no approval decision: %+v", got)
	}
	if !got.ReservedAt.IsZero() {
		t.Fatalf("pending request must never have been reserved, reserved at %v", got.ReservedAt)
	}
	checkTimingKept(t, got, createdAt, waitDeadline)

	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want untouched {100 0}", bal)
	}
	if pv, _ := w.Policy("p-approval"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}

	all := w.Ledger()
	if len(all) != 1 || all[0].Kind != LedgerPendingApproval {
		t.Fatalf("ledger = %+v, want exactly the single pending-approval entry", all)
	}
	for _, kind := range []LedgerKind{LedgerReserve, LedgerApproval, LedgerRejection, LedgerRefund, LedgerCancellation, LedgerExpiration} {
		if n := countKind(all, kind); n != 0 {
			t.Fatalf("ledger kind %v entries = %d, want none after approver-session revocation", kind, n)
		}
	}
}

// checkRequestReservedAfterApproval 校验“批准成功”的完整结果：请求保持
// 已预留并保留出资账户的批准决定与批准时刻的预留时间，拒绝原因为空；
// 出资账户 80 可用 / 20 预留，策略预留 20、已花费 0；账本恰好为申请时的
// 待审批记录加一次全额预留与一次零金额批准，且不被吊销补写拒绝、取消或
// 退款；原提交时间与等待截止时间不变。
func checkRequestReservedAfterApproval(t *testing.T, w *Wallet, createdAt, waitDeadline, approveAt time.Time) {
	t.Helper()

	got, err := w.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestReserved {
		t.Fatalf("state = %v, want reserved", got.State)
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
		t.Fatalf("approved request must not carry reject reason: %q", got.RejectReason)
	}
	checkTimingKept(t, got, createdAt, waitDeadline)

	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance = %+v, want {80 20}", bal)
	}
	if pv, _ := w.Policy("p-approval"); pv.ReservedTotal != 20 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 20/0", pv.ReservedTotal, pv.SpentTotal)
	}

	all := w.Ledger()
	if len(all) != 3 {
		t.Fatalf("ledger entries = %d, want 3 (pending + reserve + approval)", len(all))
	}
	res, ok := ledgerEntryFor(all, LedgerReserve, "r1")
	if !ok || res.AccountID != "payer" || res.Amount != 20 || !res.At.Equal(approveAt) {
		t.Fatalf("reserve entry = %+v ok=%v, want payer full-amount 20 at approve time", res, ok)
	}
	app, ok := ledgerEntryFor(all, LedgerApproval, "r1")
	if !ok || app.AccountID != "u1" || app.Amount != 0 || !app.At.Equal(approveAt) {
		t.Fatalf("approval entry = %+v ok=%v, want zero-amount entry for u1 at approve time", app, ok)
	}
	for _, kind := range []LedgerKind{LedgerRejection, LedgerRefund, LedgerCancellation, LedgerExpiration} {
		if n := countKind(all, kind); n != 0 {
			t.Fatalf("ledger kind %v entries = %d, want none", kind, n)
		}
	}
}

// TestApproveThenRevokeApproverSessionKeepsReservation：先批准成功、再吊销
// 出资账户的审批会话时，请求保留已预留状态与首次批准决定，资金与账本不被
// 吊销改动，申请会话仍然有效。
func TestApproveThenRevokeApproverSessionKeepsReservation(t *testing.T) {
	w, c := setupApproverSessionRevoke(t)
	createdAt, waitDeadline := applyPendingForRace(t, w)

	// 未到等待期限、策略有效、余额与额度充足时批准成功。
	c.t = c.t.Add(10 * time.Second)
	approveAt := c.t
	got, err := w.Approve("u1", "r1", "sa", "dev-approve")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestReserved || got.ApproverAccountID != "payer" {
		t.Fatalf("approve view = %+v, want reserved by payer", got)
	}

	// 吊销的是出资账户用于审批的会话：只改变该会话状态，不动已预留费用、
	// 不补写拒绝/取消/退款，提交申请的会话仍然有效。
	if err := w.RevokeSession("sa"); err != nil {
		t.Fatal(err)
	}
	checkApproverAndApplicantSessions(t, w)
	checkRequestReservedAfterApproval(t, w, createdAt, waitDeadline, approveAt)

	// 随后继续用已吊销会话批准同一请求（现已经预留）：身份校验先于重复批准
	// 的幂等返回，必须报无权，不能借“重复批准”绕过会话校验；首次决定、
	// 资金与账本都不变。
	before := len(w.Ledger())
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("re-approve with revoked approver session err = %v, want ErrNotApprover", err)
	}
	if len(w.Ledger()) != before {
		t.Fatalf("unauthorized re-approve grew ledger %d -> %d", before, len(w.Ledger()))
	}
	checkApproverAndApplicantSessions(t, w)
	checkRequestReservedAfterApproval(t, w, createdAt, waitDeadline, approveAt)
}

// TestRevokeApproverSessionThenApprove：先吊销审批会话、再批准时，批准返回
// 可识别的 ErrNotApprover；请求仍待审批、不填写批准决定、不预留、不记账，
// 资金与额度保持申请后的数值。换用同账户另一个绑定当前设备的有效会话后可
// 正常批准；已吊销会话始终无权，重复批准不二次记账。
func TestRevokeApproverSessionThenApprove(t *testing.T) {
	w, c := setupApproverSessionRevoke(t)
	createdAt, waitDeadline := applyPendingForRace(t, w)

	c.t = c.t.Add(10 * time.Second)
	if err := w.RevokeSession("sa"); err != nil {
		t.Fatal(err)
	}
	checkApproverAndApplicantSessions(t, w)

	// 吊销先完成：用已吊销的审批会话批准必须报无权，而不是把请求当作申请
	// 会话被吊销而拒绝（那会是 ErrRequestNotPending）。
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("approve with revoked approver session err = %v, want ErrNotApprover", err)
	}
	checkRequestStillPendingUntouched(t, w, createdAt, waitDeadline)

	// 再次用已吊销会话批准：重复调用同样无权，不能借重试预留费用或写决定。
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("second approve with revoked approver session err = %v, want ErrNotApprover", err)
	}
	checkRequestStillPendingUntouched(t, w, createdAt, waitDeadline)

	// 换用同一出资账户另一个绑定当前设备且有效的会话：仍待审批的请求正常
	// 批准，一次性完成预留与批准留痕。
	approveAt := c.t
	got, err := w.Approve("u1", "r1", "sa2", "dev-approve")
	if err != nil {
		t.Fatalf("approve pending request with a fresh payer session: %v", err)
	}
	if got.State != RequestReserved || got.ApproverAccountID != "payer" {
		t.Fatalf("approve with fresh session view = %+v, want reserved by payer", got)
	}
	checkRequestReservedAfterApproval(t, w, createdAt, waitDeadline, approveAt)

	// 请求已预留后，已吊销会话再来批准：仍先报无权，不能借重复批准绕过校验。
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("re-approve reserved request with revoked session err = %v, want ErrNotApprover", err)
	}
	checkRequestReservedAfterApproval(t, w, createdAt, waitDeadline, approveAt)

	// 有效会话重复批准只返回原结果：不改写首次决定、不重复预留或记账。
	again, err := w.Approve("u1", "r1", "sa2", "dev-approve")
	if err != nil {
		t.Fatalf("idempotent re-approve with valid session: %v", err)
	}
	if again.State != RequestReserved || again.ApproverAccountID != "payer" || !again.DecidedAt.Equal(approveAt) {
		t.Fatalf("idempotent approve view = %+v, want original reserved decision", again)
	}
	checkRequestReservedAfterApproval(t, w, createdAt, waitDeadline, approveAt)
}

// TestConcurrentRevokeApproverSessionAndApprove：吊销审批会话与批准并发时，
// 不要求固定先后，但批准返回、请求查询、余额、策略金额与账本必须共同符合
// “批准先成功”或“吊销先完成”其中一种完整结果。结束后审批会话已吊销、
// 申请会话仍有效；不能出现批准报无权却已预留、或批准成功却无资金与记录的
// 撕裂状态。之后已吊销会话始终无权，换同账户另一有效会话可把仍待审批的
// 请求批准或对已预留请求幂等返回原结果，最终恰好一次完整预留与批准。
func TestConcurrentRevokeApproverSessionAndApprove(t *testing.T) {
	const iterations = 64
	outcomes := map[string]int{}
	for i := 0; i < iterations; i++ {
		w, c := setupApproverSessionRevoke(t)
		createdAt, waitDeadline := applyPendingForRace(t, w)
		c.t = c.t.Add(10 * time.Second)
		opAt := c.t

		// 两项调用仍并发竞争同一把钱包锁；用两个独立门控交替地让批准或吊销
		// 先起跑一个很短的持锁窗口，确定性地分别制造两种先后顺序，避免调度
		// 总是偏向极短的吊销路径而漏掉“批准先成功”的并发分支。
		ready := make(chan struct{}, 2)
		goApprove := make(chan struct{})
		goRevoke := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var approveErr error
		go func() {
			defer wg.Done()
			ready <- struct{}{}
			<-goApprove
			_, approveErr = w.Approve("u1", "r1", "sa", "dev-approve")
		}()
		var revokeErr error
		go func() {
			defer wg.Done()
			ready <- struct{}{}
			<-goRevoke
			revokeErr = w.RevokeSession("sa")
		}()
		<-ready
		<-ready
		if i%2 == 0 {
			close(goApprove)
			time.Sleep(100 * time.Microsecond)
			close(goRevoke)
		} else {
			close(goRevoke)
			time.Sleep(100 * time.Microsecond)
			close(goApprove)
		}
		wg.Wait()

		if revokeErr != nil {
			t.Fatalf("iter %d: revoke err = %v", i, revokeErr)
		}
		// 无论谁先完成：审批会话已吊销、申请会话仍有效。
		checkApproverAndApplicantSessions(t, w)

		switch {
		case approveErr == nil:
			// 批准先成功：吊销审批会话不补写拒绝/取消/退款，预留完整保留。
			outcomes["approve-first"]++
			checkRequestReservedAfterApproval(t, w, createdAt, waitDeadline, opAt)
		case errors.Is(approveErr, ErrNotApprover):
			// 吊销先完成：批准报无权，请求仍待审批，无预留、无决定、无记录。
			outcomes["revoke-first"]++
			checkRequestStillPendingUntouched(t, w, createdAt, waitDeadline)
		case errors.Is(approveErr, ErrRequestNotPending):
			// 申请会话并未吊销，待审批请求不可能因吊销审批会话而被拒绝。
			t.Fatalf("iter %d: revoking the approver session must not reject the pending application: %v", i, approveErr)
		default:
			t.Fatalf("iter %d: approve err = %v, want nil or ErrNotApprover", i, approveErr)
		}

		// 两项调用结束后，继续用已吊销会话批准同一请求：无论它仍待审批还是
		// 已经预留，都必须无权，不能借重复批准绕过会话校验。
		if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrNotApprover) {
			t.Fatalf("iter %d: re-approve with revoked session err = %v, want ErrNotApprover", i, err)
		}

		// 换用同一出资账户另一个绑定当前设备的有效会话：仍待审批的正常批准，
		// 已预留的返回原结果。两条路径最终都只有一次完整预留与批准。
		view, err := w.Approve("u1", "r1", "sa2", "dev-approve")
		if err != nil || view.State != RequestReserved || view.ApproverAccountID != "payer" {
			t.Fatalf("iter %d: approve via fresh payer session view=%+v err=%v", i, view, err)
		}
		checkRequestReservedAfterApproval(t, w, createdAt, waitDeadline, opAt)

		// 重复批准不改写首次决定、不重复记账：已吊销会话仍无权，有效会话
		// 幂等返回原结果，账本与资金不再变化。
		before := len(w.Ledger())
		if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrNotApprover) {
			t.Fatalf("iter %d: final revoked-session approve err = %v, want ErrNotApprover", i, err)
		}
		if v, err := w.Approve("u1", "r1", "sa2", "dev-approve"); err != nil || v.State != RequestReserved {
			t.Fatalf("iter %d: final idempotent approve view=%+v err=%v", i, v, err)
		}
		if len(w.Ledger()) != before {
			t.Fatalf("iter %d: ledger grew %d -> %d on repeated approvals", i, before, len(w.Ledger()))
		}
		checkApproverAndApplicantSessions(t, w)
		checkRequestReservedAfterApproval(t, w, createdAt, waitDeadline, opAt)
	}
	t.Logf("outcomes: %v", outcomes)
}
