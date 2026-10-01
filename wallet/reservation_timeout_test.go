package wallet

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// timeoutPolicy 构造一条带预留超时的策略：预留超时 1 分钟，
// 其余与 validPolicy 一致（单次 30、累计 50、窗口 1 小时）。
func timeoutPolicy(c *clock) PolicySpec {
	p := validPolicy(c)
	p.ID = "p-timeout"
	p.ReservationTimeout = time.Minute
	return p
}

// setupTimeout 构造带预留超时策略的钱包：出资账户 payer（100），
// 使用账户 u1/u2，会话 s1/s2。
func setupTimeout(t *testing.T) (*Wallet, *clock) {
	t.Helper()
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s2", "u2", "dev2", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(timeoutPolicy(c)); err != nil {
		t.Fatal(err)
	}
	return w, c
}

func timeoutApply() RequestInput {
	return RequestInput{
		PolicyID:     "p-timeout",
		RequestID:    "r1",
		AccountID:    "u1",
		SessionID:    "s1",
		DeviceID:     "dev1",
		Operation:    "charge",
		Payee:        "shop",
		EstimatedFee: 20,
	}
}

func TestSavePolicyReservationTimeoutValidation(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)

	// 负数超时：拒绝保存。
	bad := timeoutPolicy(c)
	bad.ID = "p-bad-negative"
	bad.ReservationTimeout = -time.Second
	if err := w.SavePolicy(bad); !errors.Is(err, ErrPolicyInvalid) {
		t.Fatalf("negative timeout err = %v, want ErrPolicyInvalid", err)
	}

	// 零超时：接受（保持现有行为）。
	zero := timeoutPolicy(c)
	zero.ID = "p-zero"
	zero.ReservationTimeout = 0
	if err := w.SavePolicy(zero); err != nil {
		t.Fatalf("zero timeout should be accepted: %v", err)
	}

	// 正数超时：接受。
	good := timeoutPolicy(c)
	good.ID = "p-good"
	good.ReservationTimeout = 2 * time.Minute
	if err := w.SavePolicy(good); err != nil {
		t.Fatalf("positive timeout should be accepted: %v", err)
	}
}

func TestPolicyViewReturnsReservationTimeout(t *testing.T) {
	w, _ := setupTimeout(t)

	pv, err := w.Policy("p-timeout")
	if err != nil {
		t.Fatal(err)
	}
	if pv.ReservationTimeout != time.Minute {
		t.Fatalf("ReservationTimeout = %v, want 1m", pv.ReservationTimeout)
	}
}

func TestDirectReservationTimeout(t *testing.T) {
	w, c := setupTimeout(t)

	in := timeoutApply()
	req, err := w.Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	if req.State != RequestReserved {
		t.Fatalf("state = %v, want reserved", req.State)
	}
	// 预留时间与截止时间。
	if !req.ReservedAt.Equal(c.t) {
		t.Fatalf("ReservedAt = %v, want %v", req.ReservedAt, c.t)
	}
	wantDeadline := c.t.Add(time.Minute)
	if !req.ReservationDeadline.Equal(wantDeadline) {
		t.Fatalf("ReservationDeadline = %v, want %v", req.ReservationDeadline, wantDeadline)
	}
	if !req.TimedOutAt.IsZero() {
		t.Fatalf("TimedOutAt should be zero before timeout: %v", req.TimedOutAt)
	}

	// 超时前查询：仍为已预留。
	c.t = c.t.Add(59 * time.Second)
	if req, _ := w.Request("u1", "r1"); req.State != RequestReserved {
		t.Fatalf("state before deadline = %v, want reserved", req.State)
	}

	// 到达截止时刻：超时释放。
	c.t = c.t.Add(time.Second)
	req, err = w.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if req.State != RequestReservationTimeout {
		t.Fatalf("state = %v, want reservation timeout", req.State)
	}
	// 释放时间记录截止时刻，不是查询时刻。
	if !req.TimedOutAt.Equal(wantDeadline) {
		t.Fatalf("TimedOutAt = %v, want deadline %v", req.TimedOutAt, wantDeadline)
	}
	// 实际预留时间不变。
	if !req.ReservedAt.Equal(c.t.Add(-time.Minute)) {
		t.Fatalf("ReservedAt changed: %v", req.ReservedAt)
	}

	// 余额：预留全部退回。
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {100 0}", bal)
	}
	// 策略预留总额减少，已花费总额不增加。
	pv, _ := w.Policy("p-timeout")
	if pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}
}

func TestTimeoutReleaseTimeIsDeadlineNotQueryTime(t *testing.T) {
	w, c := setupTimeout(t)

	in := timeoutApply()
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	wantDeadline := c.t.Add(time.Minute)

	// 过了很久才访问钱包。
	c.t = c.t.Add(time.Hour)
	req, err := w.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if req.State != RequestReservationTimeout {
		t.Fatalf("state = %v, want timeout", req.State)
	}
	// 释放时间仍是截止时刻，不能改成访问时刻。
	if !req.TimedOutAt.Equal(wantDeadline) {
		t.Fatalf("TimedOutAt = %v, want deadline %v (not query time %v)", req.TimedOutAt, wantDeadline, c.t)
	}
}

func TestSettleAfterTimeoutReturnsError(t *testing.T) {
	w, c := setupTimeout(t)

	in := timeoutApply()
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}

	// 超时后结算：返回明确的预留已超时错误，不能扣款。
	c.t = c.t.Add(time.Minute)
	if _, err := w.Settle("u1", "r1", 10); !errors.Is(err, ErrReservationTimeout) {
		t.Fatalf("settle after timeout err = %v, want ErrReservationTimeout", err)
	}
	// 零实际费用也不行。
	if _, err := w.Settle("u1", "r1", 0); !errors.Is(err, ErrReservationTimeout) {
		t.Fatalf("zero settle after timeout err = %v, want ErrReservationTimeout", err)
	}
	// 余额不变（已全部退回）。
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {100 0}", bal)
	}
}

func TestCancelAfterTimeoutReturnsExistingResult(t *testing.T) {
	w, c := setupTimeout(t)

	in := timeoutApply()
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}

	// 超时后取消：返回已有超时结果，不重复退回。
	c.t = c.t.Add(time.Minute)
	req, err := w.Cancel("u1", "r1")
	if err != nil {
		t.Fatalf("cancel after timeout should return existing result: %v", err)
	}
	if req.State != RequestReservationTimeout {
		t.Fatalf("state = %v, want timeout", req.State)
	}
	// 账本不重复记录退款。
	refunds := 0
	for _, e := range w.Ledger() {
		if e.Kind == LedgerRefund && e.RequestID == "r1" {
			refunds++
		}
	}
	if refunds != 1 {
		t.Fatalf("refund entries = %d, want 1 (no double refund)", refunds)
	}
}

func TestApplyIdempotencyAfterTimeout(t *testing.T) {
	w, c := setupTimeout(t)

	in := timeoutApply()
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}

	// 超时后同号同内容申请：返回超时结果。
	c.t = c.t.Add(time.Minute)
	req, err := w.Apply(in)
	if err != nil {
		t.Fatalf("idempotent apply after timeout should return existing result: %v", err)
	}
	if req.State != RequestReservationTimeout {
		t.Fatalf("state = %v, want timeout", req.State)
	}

	// 同号不同内容：仍返回冲突。
	conflict := in
	conflict.EstimatedFee = 19
	if _, err := w.Apply(conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting apply after timeout err = %v, want ErrConflict", err)
	}

	// 重新发起必须换请求编号。
	newIn := timeoutApply()
	newIn.RequestID = "r2"
	if _, err := w.Apply(newIn); err != nil {
		t.Fatalf("re-apply with new request id should succeed: %v", err)
	}
}

func TestApproveAfterTimeoutCannotRestore(t *testing.T) {
	w, c := setupTimeout(t)

	// 用审批策略来测试批准后超时。
	ap := approvalPolicy(c)
	ap.ID = "p-approval-timeout"
	ap.ReservationTimeout = time.Minute
	if err := w.SavePolicy(ap); err != nil {
		t.Fatal(err)
	}
	// 出资账户审批会话。
	if _, err := w.CreateSession("sa", "payer", "dev-approve", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	in := approvalApply()
	in.PolicyID = "p-approval-timeout"
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	// 批准。
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); err != nil {
		t.Fatal(err)
	}
	// 超时后重复批准：不能恢复预留。
	c.t = c.t.Add(time.Minute)
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrRequestNotPending) {
		t.Fatalf("approve after timeout err = %v, want ErrRequestNotPending", err)
	}
	// 审批身份校验仍按现有规则执行（无权会话返回 ErrNotApprover）。
	if _, err := w.Approve("u1", "r1", "s1", "dev1"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("approve with wrong session err = %v, want ErrNotApprover", err)
	}
}

func TestLedgerEntriesOnTimeout(t *testing.T) {
	w, c := setupTimeout(t)

	in := timeoutApply()
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Minute)
	if _, err := w.Request("u1", "r1"); err != nil {
		t.Fatal(err)
	}

	// 账本：一条出资账户的全额退款 + 一条使用账户的零金额超时记录。
	var refund, timeoutRec bool
	for _, e := range w.Ledger() {
		switch e.Kind {
		case LedgerRefund:
			if e.RequestID == "r1" && e.AccountID == "payer" && e.Amount == 20 {
				refund = true
			}
		case LedgerReservationTimeout:
			if e.RequestID == "r1" && e.AccountID == "u1" && e.Amount == 0 {
				timeoutRec = true
			}
		}
	}
	if !refund || !timeoutRec {
		t.Fatalf("ledger missing refund=%v timeout=%v", refund, timeoutRec)
	}

	// 原有的预留记录仍保留。
	var reserve bool
	for _, e := range w.AccountLedger("payer") {
		if e.Kind == LedgerReserve && e.RequestID == "r1" && e.Amount == 20 {
			reserve = true
		}
	}
	if !reserve {
		t.Fatal("original reserve record should be kept")
	}
}

func TestApprovedReservationTimeoutStartsFromApproval(t *testing.T) {
	w, c := setupTimeout(t)

	// 用审批策略。
	ap := approvalPolicy(c)
	ap.ID = "p-approval-timeout"
	ap.ReservationTimeout = time.Minute
	if err := w.SavePolicy(ap); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "dev-approve", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	in := approvalApply()
	in.PolicyID = "p-approval-timeout"
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	// 等待审批时间不计入预留超时：在待审批期间推进时间。
	c.t = c.t.Add(30 * time.Second)
	// 批准。
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); err != nil {
		t.Fatal(err)
	}
	approvalTime := c.t
	req, _ := w.Request("u1", "r1")
	// 预留截止时间 = 批准时刻 + 1 分钟。
	wantDeadline := approvalTime.Add(time.Minute)
	if !req.ReservationDeadline.Equal(wantDeadline) {
		t.Fatalf("ReservationDeadline = %v, want %v (approval time + 1m)", req.ReservationDeadline, wantDeadline)
	}
	if !req.ReservedAt.Equal(approvalTime) {
		t.Fatalf("ReservedAt = %v, want approval time %v", req.ReservedAt, approvalTime)
	}

	// 从批准时刻起算 1 分钟后超时。
	c.t = c.t.Add(time.Minute)
	req, _ = w.Request("u1", "r1")
	if req.State != RequestReservationTimeout {
		t.Fatalf("state = %v, want timeout", req.State)
	}
	if !req.TimedOutAt.Equal(wantDeadline) {
		t.Fatalf("TimedOutAt = %v, want %v", req.TimedOutAt, wantDeadline)
	}
}

func TestApprovalFailureDoesNotStartTimer(t *testing.T) {
	w, c := setupTimeout(t)

	// 用审批策略，预留超时 1 分钟。
	ap := approvalPolicy(c)
	ap.ID = "p-approval-timeout"
	ap.ReservationTimeout = time.Minute
	ap.MaxTotal = 200 // 允许连续预留以降低余额
	if err := w.SavePolicy(ap); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "dev-approve", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	in := approvalApply()
	in.PolicyID = "p-approval-timeout"
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	// 连续预留 9 笔各 10，把出资余额降到 10（< 待审批费用 20）。
	for i := 0; i < 9; i++ {
		reserved := approvalApply()
		reserved.PolicyID = "p-approval-timeout"
		reserved.RequestID = fmt.Sprintf("drop%d", i)
		reserved.EstimatedFee = 10
		if _, err := w.Apply(reserved); err != nil {
			t.Fatal(err)
		}
	}
	// 批准时余额不足：返回具体原因，保留待审批状态，不能提前开始计时。
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	req, _ := w.Request("u1", "r1")
	if req.State != RequestPendingApproval {
		t.Fatalf("state = %v, want still pending", req.State)
	}
	// 预留截止时间应为零（尚未预留）。
	if !req.ReservationDeadline.IsZero() {
		t.Fatalf("ReservationDeadline should be zero after failed approval: %v", req.ReservationDeadline)
	}
}

func TestCrossPolicyBalanceReflection(t *testing.T) {
	w, c := setupTimeout(t)

	// 另一条策略，同一出资账户，无超时，累计上限 100。
	other := validPolicy(c)
	other.ID = "p-other"
	other.MaxTotal = 100
	if err := w.SavePolicy(other); err != nil {
		t.Fatal(err)
	}

	// 在 p-timeout 下预留一笔 20。
	in := timeoutApply()
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	// 在 p-other 下预留一笔 30。
	otherIn := baseApply()
	otherIn.PolicyID = "p-other"
	otherIn.RequestID = "r-other"
	otherIn.EstimatedFee = 30
	if _, err := w.Apply(otherIn); err != nil {
		t.Fatal(err)
	}
	// 余额：可用 50，预留 50。
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 50, Reserved: 50}) {
		t.Fatalf("balance = %+v, want {50 50}", bal)
	}

	// p-timeout 的请求超时：余额应反映退回。
	c.t = c.t.Add(time.Minute)
	bal, _ = w.Balance("payer")
	if bal != (Balances{Available: 70, Reserved: 30}) {
		t.Fatalf("balance after timeout = %+v, want {70 30}", bal)
	}

	// 新申请检查资金和额度时，应计入已到期预留的退回。
	// 再申请一笔 30：可用余额 70，足够；p-other 累计 used=30+30=60 ≤ 100。
	newIn := baseApply()
	newIn.PolicyID = "p-other"
	newIn.RequestID = "r-new"
	newIn.EstimatedFee = 30
	if _, err := w.Apply(newIn); err != nil {
		t.Fatalf("new apply should reflect timeout refund: %v", err)
	}
	// 先查余额还是先申请，可用金额应一致。
	bal2, _ := w.Balance("payer")
	if bal2 != (Balances{Available: 40, Reserved: 60}) {
		t.Fatalf("balance after new apply = %+v, want {40 60}", bal2)
	}
}

func TestTimeoutDoesNotAffectNonExpiredRequests(t *testing.T) {
	w, c := setupTimeout(t)

	// 两笔预留，一笔超时，一笔不超时。
	in1 := timeoutApply()
	if _, err := w.Apply(in1); err != nil {
		t.Fatal(err)
	}
	in2 := timeoutApply()
	in2.RequestID = "r2"
	if _, err := w.Apply(in2); err != nil {
		t.Fatal(err)
	}

	// 只让第一笔超时（通过控制时间不行，因为两笔同时预留）。
	// 改为：第一笔预留后，第二笔在超时后预留。
	c.t = c.t.Add(time.Minute)
	// 第一笔已超时。
	req1, _ := w.Request("u1", "r1")
	if req1.State != RequestReservationTimeout {
		t.Fatalf("r1 state = %v, want timeout", req1.State)
	}
	// 第二笔在超时后预留，不应受影响。
	in3 := timeoutApply()
	in3.RequestID = "r3"
	if _, err := w.Apply(in3); err != nil {
		t.Fatal(err)
	}
	req3, _ := w.Request("u1", "r3")
	if req3.State != RequestReserved {
		t.Fatalf("r3 state = %v, want reserved", req3.State)
	}
}

func TestSessionExpiryDoesNotShortenReservationDeadline(t *testing.T) {
	w, c := setupTimeout(t)

	// 会话 1 分钟后到期，但预留超时 1 小时。
	if _, err := w.CreateSession("s-short", "u1", "dev1", c.t.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// 预留超时设为 1 小时。
	p := timeoutPolicy(c)
	p.ID = "p-long-timeout"
	p.ReservationTimeout = time.Hour
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	in := timeoutApply()
	in.PolicyID = "p-long-timeout"
	in.SessionID = "s-short"
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	req, _ := w.Request("u1", "r1")
	// 预留截止时间 = 预留时刻 + 1 小时（不是会话到期时间）。
	wantDeadline := c.t.Add(time.Hour)
	if !req.ReservationDeadline.Equal(wantDeadline) {
		t.Fatalf("ReservationDeadline = %v, want %v (session expiry must not shorten deadline)", req.ReservationDeadline, wantDeadline)
	}
}

func TestPolicyEndDoesNotShortenReservationDeadline(t *testing.T) {
	w, c := setupTimeout(t)

	// 策略窗口 30 分钟后结束，但预留超时 1 小时。
	p := timeoutPolicy(c)
	p.ID = "p-short-window"
	p.EndsAt = c.t.Add(30 * time.Minute)
	p.ReservationTimeout = time.Hour
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	in := timeoutApply()
	in.PolicyID = "p-short-window"
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	req, _ := w.Request("u1", "r1")
	// 预留截止时间 = 预留时刻 + 1 小时（不是策略结束时间）。
	wantDeadline := c.t.Add(time.Hour)
	if !req.ReservationDeadline.Equal(wantDeadline) {
		t.Fatalf("ReservationDeadline = %v, want %v (policy end must not shorten deadline)", req.ReservationDeadline, wantDeadline)
	}
}

func TestZeroTimeoutKeepsExistingBehavior(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 零超时策略。
	p := validPolicy(c)
	p.ID = "p-zero"
	p.ReservationTimeout = 0
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	in := baseApply()
	in.PolicyID = "p-zero"
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	// 预留截止时间应为零。
	req, _ := w.Request("u1", "r1")
	if !req.ReservationDeadline.IsZero() {
		t.Fatalf("ReservationDeadline should be zero for zero timeout: %v", req.ReservationDeadline)
	}
	// 很久以后也不会超时。
	c.t = c.t.Add(24 * time.Hour)
	req, _ = w.Request("u1", "r1")
	if req.State != RequestReserved {
		t.Fatalf("state = %v, want reserved (zero timeout = no timeout)", req.State)
	}
	// 结算正常进行。
	if _, err := w.Settle("u1", "r1", 20); err != nil {
		t.Fatalf("settle should work: %v", err)
	}
}

func TestConcurrentSettleCancelTimeout(t *testing.T) {
	w, c := setupTimeout(t)

	in := timeoutApply()
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}

	// 并发结算、取消与超时处理：只能有一个结果。
	c.t = c.t.Add(time.Minute)
	var wg sync.WaitGroup
	results := make([]RequestView, 4)
	opErrs := make([]error, 4)
	for i := 0; i < 4; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 3 {
			case 0:
				results[i], opErrs[i] = w.Settle("u1", "r1", 10)
			case 1:
				results[i], opErrs[i] = w.Cancel("u1", "r1")
			case 2:
				results[i], opErrs[i] = w.Request("u1", "r1")
			}
		}()
	}
	wg.Wait()

	// 最终状态只能是已结算、已取消或预留超时之一。
	req, _ := w.Request("u1", "r1")
	states := map[RequestState]int{}
	for i, err := range opErrs {
		if err == nil {
			states[results[i].State]++
		}
	}
	// 超时处理一定会发生（因为时间已到），结算和取消不能成功扣款。
	if req.State != RequestReservationTimeout {
		t.Fatalf("final state = %v, want reservation timeout", req.State)
	}
	// 余额：全部退回。
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {100 0}", bal)
	}
	// 退款只发生一次。
	refunds := 0
	for _, e := range w.Ledger() {
		if e.Kind == LedgerRefund && e.RequestID == "r1" {
			refunds++
		}
	}
	if refunds != 1 {
		t.Fatalf("refund entries = %d, want 1", refunds)
	}
}

func TestTimeoutThenNewApplicationUsesRefundedFee(t *testing.T) {
	w, c := setupTimeout(t)

	// 预留一笔 20，超时后退回。
	in := timeoutApply()
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Minute)
	// 触发超时。
	if _, err := w.Request("u1", "r1"); err != nil {
		t.Fatal(err)
	}
	// 新申请使用退回的费用。
	newIn := timeoutApply()
	newIn.RequestID = "r2"
	if _, err := w.Apply(newIn); err != nil {
		t.Fatal(err)
	}
	// 新申请的结算正常（在超时前完成）。
	if _, err := w.Settle("u1", "r2", 20); err != nil {
		t.Fatalf("settle new request should work: %v", err)
	}
	// 迟到的结算不能扣走已被新申请使用的费用。
	if _, err := w.Settle("u1", "r1", 10); !errors.Is(err, ErrReservationTimeout) {
		t.Fatalf("late settle err = %v, want ErrReservationTimeout", err)
	}
	// 余额：100 - 20（r2 结算扣除）= 80。
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 80, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {80 0}", bal)
	}
}

func TestDeactivationDoesNotShortenReservationDeadline(t *testing.T) {
	w, c := setupTimeout(t)

	// 出资账户会话。
	if _, err := w.CreateSession("sa", "payer", "dev-approve", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	in := timeoutApply()
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	// 停用策略：已预留请求的截止时间不应缩短。
	if _, err := w.DeactivatePolicy("p-timeout", "sa", "dev-approve", "stop"); err != nil {
		t.Fatal(err)
	}
	req, _ := w.Request("u1", "r1")
	// 预留截止时间不变。
	wantDeadline := c.t.Add(time.Minute)
	if !req.ReservationDeadline.Equal(wantDeadline) {
		t.Fatalf("ReservationDeadline = %v, want %v (deactivation must not shorten deadline)", req.ReservationDeadline, wantDeadline)
	}
	// 超时后仍正常退回。
	c.t = c.t.Add(time.Minute)
	req, _ = w.Request("u1", "r1")
	if req.State != RequestReservationTimeout {
		t.Fatalf("state = %v, want timeout", req.State)
	}
}

func TestRevokeSessionDoesNotShortenReservationDeadline(t *testing.T) {
	w, c := setupTimeout(t)

	in := timeoutApply()
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	// 吊销会话：已预留请求的截止时间不应缩短。
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	req, _ := w.Request("u1", "r1")
	wantDeadline := c.t.Add(time.Minute)
	if !req.ReservationDeadline.Equal(wantDeadline) {
		t.Fatalf("ReservationDeadline = %v, want %v (revocation must not shorten deadline)", req.ReservationDeadline, wantDeadline)
	}
	// 超时后仍正常退回。
	c.t = c.t.Add(time.Minute)
	req, _ = w.Request("u1", "r1")
	if req.State != RequestReservationTimeout {
		t.Fatalf("state = %v, want timeout", req.State)
	}
}
