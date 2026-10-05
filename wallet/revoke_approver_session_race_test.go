package wallet

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件覆盖“吊销出资账户用于审批的会话”与“批准代付请求”同时发生的回归
// 场景：被吊销的是审批会话 sa（出资账户 payer 所有），提交申请的使用账户
// 会话 s1 仍然有效；不能把这种情况当成申请会话被吊销而将请求拒绝。夹具沿用
// setupApproval/applyPendingForRace：预估费用 20 严格超过门槛 10，出资账户
// 可用 100，策略窗口与等待期限均未到达，预留超时关闭；申请进入待审批后出资
// 账户仍是 100 可用 / 0 预留，策略预留与已花费总额均为 0。

// checkApproverRevokedApplicantActive 断言两项操作结束后审批会话 sa 已吊销、
// 申请会话 s1 仍有效。
func checkApproverRevokedApplicantActive(t *testing.T, w *Wallet) {
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
		t.Fatalf("applicant session state = %v, want still active", s1.State)
	}
}

// checkPendingUntouchedOutcome 校验“吊销审批会话先完成”的完整结果：请求仍在
// 待审批（吊销审批会话不等于申请会话被吊销，不得补写拒绝、取消或退款），不
// 填写批准决定；出资账户 100 可用 / 0 预留；策略预留与已花费均为 0；账本只
// 有申请时的待审批记录。
func checkPendingUntouchedOutcome(t *testing.T, w *Wallet, createdAt, waitDeadline time.Time) {
	t.Helper()

	got, err := w.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestPendingApproval {
		t.Fatalf("state = %v, want still pending after approver session revoked", got.State)
	}
	if got.ApproverAccountID != "" || !got.DecidedAt.IsZero() {
		t.Fatalf("pending request must not carry approval decision: approver %q decided %v",
			got.ApproverAccountID, got.DecidedAt)
	}
	if got.RejectReason != "" {
		t.Fatalf("revoking the approver session must not reject the request: %q", got.RejectReason)
	}
	if !got.ReservedAt.IsZero() {
		t.Fatalf("pending request must never have been reserved, reserved at %v", got.ReservedAt)
	}
	checkTimingKept(t, got, createdAt, waitDeadline)

	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {100 0}; pending request must not freeze funds", bal)
	}
	if pv, _ := w.Policy("p-approval"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}

	// 账本：只有申请时的待审批记录，不因吊销审批会话补写任何记录。
	all := w.Ledger()
	if len(all) != 1 {
		t.Fatalf("ledger entries = %d, want 1 (pending only)", len(all))
	}
	if all[0].Kind != LedgerPendingApproval || all[0].RequestID != "r1" || all[0].AccountID != "u1" || all[0].Amount != 0 {
		t.Fatalf("pending entry = %+v, want zero-amount pending record for u1/r1", all[0])
	}
	for _, kind := range []LedgerKind{LedgerReserve, LedgerApproval, LedgerRejection, LedgerCancellation, LedgerRefund} {
		if n := countKind(all, kind); n != 0 {
			t.Fatalf("ledger kind %v entries = %d, want none while still pending", kind, n)
		}
	}
}

// TestApproveThenRevokeApproverSessionKeepsReservation：批准先成功再吊销审批
// 会话时，请求保留已预留状态与批准决定，出资账户 80 可用 / 20 预留，策略预留
// 20、已花费 0；账本保留待审批记录及一次预留、一次批准，不因吊销补写拒绝、
// 取消或退款，提交时间、等待截止时间与批准决定信息保持不变。
func TestApproveThenRevokeApproverSessionKeepsReservation(t *testing.T) {
	w, c := setupApproval(t)
	createdAt, waitDeadline := applyPendingForRace(t, w)

	c.t = c.t.Add(10 * time.Second)
	approveAt := c.t
	got, err := w.Approve("u1", "r1", "sa", "dev-approve")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestReserved || got.ApproverAccountID != "payer" {
		t.Fatalf("approve view = %+v, want reserved by payer", got)
	}

	// 吊销出资账户用于审批的会话：只改变会话状态，不动已预留费用。
	if err := w.RevokeSession("sa"); err != nil {
		t.Fatal(err)
	}
	checkApproverRevokedApplicantActive(t, w)
	checkApproveWonOutcome(t, w, createdAt, waitDeadline, approveAt)
	if n := countKind(w.Ledger(), LedgerCancellation); n != 0 {
		t.Fatalf("cancellation entries = %d, want none: revocation must not cancel the reservation", n)
	}
}

// TestRevokeApproverSessionThenApproveNotApprover：先吊销审批会话再批准时，
// 请求仍在待审批（不能当成申请会话被吊销而拒绝），批准返回可识别的
// ErrNotApprover，不填写批准决定，不产生预留或批准记录，资金与额度保持
// 申请后的数值。
func TestRevokeApproverSessionThenApproveNotApprover(t *testing.T) {
	w, c := setupApproval(t)
	createdAt, waitDeadline := applyPendingForRace(t, w)

	c.t = c.t.Add(10 * time.Second)
	if err := w.RevokeSession("sa"); err != nil {
		t.Fatal(err)
	}
	checkApproverRevokedApplicantActive(t, w)
	// 吊销的是审批会话而非申请会话：请求必须仍在待审批，资金与账本不变。
	checkPendingUntouchedOutcome(t, w, createdAt, waitDeadline)

	before := len(w.Ledger())
	_, approveErr := w.Approve("u1", "r1", "sa", "dev-approve")
	if !errors.Is(approveErr, ErrNotApprover) {
		t.Fatalf("approve with revoked approver session err = %v, want ErrNotApprover", approveErr)
	}
	if len(w.Ledger()) != before {
		t.Fatalf("failed approve grew ledger %d -> %d", before, len(w.Ledger()))
	}
	// 失败的批准不改变任何结果：请求仍待审批，资金与额度保持申请后的数值。
	checkPendingUntouchedOutcome(t, w, createdAt, waitDeadline)
}

// TestConcurrentRevokeApproverSessionAndApprove：吊销审批会话与批准同时竞争
// 同一笔待审批请求时，不要求固定哪一项先完成，但批准的返回结果、请求查询、
// 余额、策略金额和账本必须共同符合“批准先成功”或“吊销先完成”其中一种完整
// 结果；两项调用结束后审批会话已吊销、申请会话仍有效，不能出现批准报无权
// 但资金已被预留，或批准成功却没有对应资金和记录的情况。
func TestConcurrentRevokeApproverSessionAndApprove(t *testing.T) {
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
			revokeErr = w.RevokeSession("sa")
		}()
		close(start)
		wg.Wait()

		// 吊销本身总是成功；无论哪种结果，审批会话已吊销、申请会话仍有效。
		if revokeErr != nil {
			t.Fatalf("iter %d: revoke err = %v", i, revokeErr)
		}
		checkApproverRevokedApplicantActive(t, w)

		switch {
		case approveErr == nil:
			// 批准先成功：吊销只改变审批会话状态，预留与批准记录完整保留。
			outcomes["approve-first"]++
			checkApproveWonOutcome(t, w, createdAt, waitDeadline, opAt)
		case errors.Is(approveErr, ErrNotApprover):
			// 吊销先完成：批准报无权，请求仍在待审批，资金与账本保持原样。
			outcomes["revoke-first"]++
			checkPendingUntouchedOutcome(t, w, createdAt, waitDeadline)
		default:
			t.Fatalf("iter %d: approve err = %v, want nil or ErrNotApprover", i, approveErr)
		}
	}
	t.Logf("outcomes: %v", outcomes)
}

// TestRevokedApproverSessionCannotApproveAgain：用已吊销的审批会话继续批准
// 同一请求，无论它仍待审批还是已经预留，都必须返回无权审批，不能借重复批准
// 绕过会话校验。
func TestRevokedApproverSessionCannotApproveAgain(t *testing.T) {
	t.Run("still pending", func(t *testing.T) {
		w, c := setupApproval(t)
		createdAt, waitDeadline := applyPendingForRace(t, w)
		c.t = c.t.Add(10 * time.Second)
		if err := w.RevokeSession("sa"); err != nil {
			t.Fatal(err)
		}
		for attempt := 0; attempt < 2; attempt++ {
			if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrNotApprover) {
				t.Fatalf("attempt %d: err = %v, want ErrNotApprover", attempt, err)
			}
		}
		checkPendingUntouchedOutcome(t, w, createdAt, waitDeadline)
	})

	t.Run("already reserved", func(t *testing.T) {
		w, c := setupApproval(t)
		createdAt, waitDeadline := applyPendingForRace(t, w)
		c.t = c.t.Add(10 * time.Second)
		approveAt := c.t
		if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); err != nil {
			t.Fatal(err)
		}
		if err := w.RevokeSession("sa"); err != nil {
			t.Fatal(err)
		}
		// 已预留也不能借重复批准绕过会话校验：已吊销会话一律无权。
		for attempt := 0; attempt < 2; attempt++ {
			if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrNotApprover) {
				t.Fatalf("attempt %d: err = %v, want ErrNotApprover", attempt, err)
			}
		}
		// 无权批准不改写已预留结果。
		checkApproveWonOutcome(t, w, createdAt, waitDeadline, approveAt)
	})
}

// TestReplacementApproverSessionApproves：换用同一出资账户另一个绑定当前设备
// 且有效的会话后，仍待审批的请求可以正常批准，已预留的请求返回原结果；两条
// 路径最终都只有一次完整预留与批准，重复批准不改写首次决定，也不重复记账。
func TestReplacementApproverSessionApproves(t *testing.T) {
	// 吊销 sa 后，为出资账户再建一个绑定同一审批设备的有效会话。
	replacement := func(t *testing.T, w *Wallet, c *clock) {
		t.Helper()
		if _, err := w.CreateSession("sa2", "payer", "dev-approve", c.t.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("still pending", func(t *testing.T) {
		w, c := setupApproval(t)
		createdAt, waitDeadline := applyPendingForRace(t, w)
		c.t = c.t.Add(10 * time.Second)
		if err := w.RevokeSession("sa"); err != nil {
			t.Fatal(err)
		}
		replacement(t, w, c)

		c.t = c.t.Add(5 * time.Second)
		approveAt := c.t
		got, err := w.Approve("u1", "r1", "sa2", "dev-approve")
		if err != nil {
			t.Fatalf("approve with replacement session: %v", err)
		}
		if got.State != RequestReserved || got.ApproverAccountID != "payer" {
			t.Fatalf("approve view = %+v, want reserved by payer", got)
		}
		checkApproveWonOutcome(t, w, createdAt, waitDeadline, approveAt)

		// 重复批准返回已有结果，不改写首次决定，也不重复记账。
		again, err := w.Approve("u1", "r1", "sa2", "dev-approve")
		if err != nil {
			t.Fatalf("repeat approve: %v", err)
		}
		if !again.DecidedAt.Equal(approveAt) || !again.ReservedAt.Equal(approveAt) {
			t.Fatalf("repeat approve rewrote decision: decided %v reserved %v, want %v",
				again.DecidedAt, again.ReservedAt, approveAt)
		}
		checkApproveWonOutcome(t, w, createdAt, waitDeadline, approveAt)
	})

	t.Run("already reserved", func(t *testing.T) {
		w, c := setupApproval(t)
		createdAt, waitDeadline := applyPendingForRace(t, w)
		c.t = c.t.Add(10 * time.Second)
		approveAt := c.t
		if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); err != nil {
			t.Fatal(err)
		}
		if err := w.RevokeSession("sa"); err != nil {
			t.Fatal(err)
		}
		replacement(t, w, c)

		// 已预留的请求用新会话重复批准：返回原结果，不重复预留、不重复记账。
		again, err := w.Approve("u1", "r1", "sa2", "dev-approve")
		if err != nil {
			t.Fatalf("repeat approve with replacement session: %v", err)
		}
		if again.State != RequestReserved || again.ApproverAccountID != "payer" {
			t.Fatalf("repeat approve view = %+v, want original reserved result", again)
		}
		if !again.DecidedAt.Equal(approveAt) || !again.ReservedAt.Equal(approveAt) {
			t.Fatalf("repeat approve rewrote first decision: decided %v reserved %v, want %v",
				again.DecidedAt, again.ReservedAt, approveAt)
		}
		checkApproveWonOutcome(t, w, createdAt, waitDeadline, approveAt)
	})
}
