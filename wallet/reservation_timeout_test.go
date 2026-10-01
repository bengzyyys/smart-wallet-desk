package wallet

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// timeoutPolicy 在 validPolicy 基础上开启预留超时：最长预留时长 d，
// 默认关闭大额审批（申请直接预留）。
func timeoutPolicy(c *clock, d time.Duration) PolicySpec {
	p := validPolicy(c)
	p.ID = "p-timeout"
	p.MaxReserveDuration = d
	return p
}

// timeoutApply 构造一条 p-timeout 下的申请（默认费用 20）。
func timeoutApply() RequestInput {
	in := baseApply()
	in.PolicyID = "p-timeout"
	return in
}

// setupTimeout 构造开启预留超时的钱包：出资账户 payer（100），使用账户
// u1/u2 各自一分钟会话，策略 p-timeout（单次 30、累计 50、最长预留 d）。
func setupTimeout(t *testing.T, d time.Duration) (*Wallet, *clock) {
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
	if err := w.SavePolicy(timeoutPolicy(c, d)); err != nil {
		t.Fatal(err)
	}
	return w, c
}

func TestSaveReserveTimeoutPolicyValidation(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)

	// 正值：接受并可查询。
	good := timeoutPolicy(c, 30*time.Second)
	if err := w.SavePolicy(good); err != nil {
		t.Fatal(err)
	}
	pv, err := w.Policy("p-timeout")
	if err != nil {
		t.Fatal(err)
	}
	if pv.MaxReserveDuration != 30*time.Second {
		t.Fatalf("max reserve duration = %v, want 30s", pv.MaxReserveDuration)
	}

	// 负值：策略无效且不创建策略。
	neg := good
	neg.ID = "p-neg"
	neg.MaxReserveDuration = -time.Nanosecond
	if err := w.SavePolicy(neg); !errors.Is(err, ErrPolicyInvalid) {
		t.Fatalf("negative duration err = %v, want ErrPolicyInvalid", err)
	}
	if _, err := w.Policy("p-neg"); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("negative-duration policy must not be created: %v", err)
	}

	// 零：关闭超时，保持既有行为，允许保存并可查询为零。
	off := timeoutPolicy(c, 0)
	off.ID = "p-off"
	if err := w.SavePolicy(off); err != nil {
		t.Fatalf("zero duration should be accepted: %v", err)
	}
	offView, err := w.Policy("p-off")
	if err != nil {
		t.Fatal(err)
	}
	if offView.MaxReserveDuration != 0 {
		t.Fatalf("disabled duration = %v, want 0", offView.MaxReserveDuration)
	}
}

func TestDirectApplyStartsTimerAtAcceptance(t *testing.T) {
	w, c := setupTimeout(t, 30*time.Second)

	req, err := w.Apply(timeoutApply())
	if err != nil {
		t.Fatal(err)
	}
	if req.State != RequestReserved {
		t.Fatalf("state = %v", req.State)
	}
	// 直接受理：实际预留时间为受理时刻，截止时间为受理时刻 + 最长预留时长。
	if !req.ReservedAt.Equal(c.t) {
		t.Fatalf("reservedAt = %v, want acceptance time %v", req.ReservedAt, c.t)
	}
	if req.ReserveDuration != 30*time.Second {
		t.Fatalf("reserve duration = %v", req.ReserveDuration)
	}
	if !req.ReserveDeadline.Equal(c.t.Add(30 * time.Second)) {
		t.Fatalf("deadline = %v, want %v", req.ReserveDeadline, c.t.Add(30*time.Second))
	}
	if !req.ReserveExpiredAt.IsZero() {
		t.Fatalf("release time before timeout = %v, want zero", req.ReserveExpiredAt)
	}
}

func TestApprovalStartsTimerAtApprovalNotSubmission(t *testing.T) {
	w, c := setupTimeout(t, 30*time.Second)
	// 改造为开启审批的超时策略。
	p := timeoutPolicy(c, 30*time.Second)
	p.ID = "p-approval-timeout"
	p.ApprovalThreshold = 10
	p.ApprovalWait = time.Hour
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "dev-approve", c.t.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	in := timeoutApply()
	in.PolicyID = "p-approval-timeout"
	pending, err := w.Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	if pending.State != RequestPendingApproval {
		t.Fatalf("state = %v", pending.State)
	}
	// 待审批期间不预留，所有预留计时字段为空。
	if !pending.ReservedAt.IsZero() || pending.ReserveDuration != 0 ||
		!pending.ReserveDeadline.IsZero() || !pending.ReserveExpiredAt.IsZero() {
		t.Fatalf("pending request must carry no reserve timing: %+v", pending)
	}

	// 等待审批耗时 20 秒（不计入预留时长）。
	c.t = c.t.Add(20 * time.Second)
	approved, err := w.Approve("u1", "r1", "sa", "dev-approve")
	if err != nil {
		t.Fatal(err)
	}
	if !approved.ReservedAt.Equal(c.t) {
		t.Fatalf("reservedAt = %v, want approval time %v", approved.ReservedAt, c.t)
	}
	wantDeadline := c.t.Add(30 * time.Second) // 自批准成功起算，而非提交时刻 + 30s
	if !approved.ReserveDeadline.Equal(wantDeadline) {
		t.Fatalf("deadline = %v, want %v (approval wait must not count)", approved.ReserveDeadline, wantDeadline)
	}
	if !approved.ReserveExpiredAt.IsZero() {
		t.Fatalf("release time right after approval = %v, want zero", approved.ReserveExpiredAt)
	}

	// 提交后 45 秒（若从提交起算早已超时）、批准后 25 秒：仍在预留中。
	c.t = c.t.Add(25 * time.Second)
	if req, _ := w.Request("u1", "r1"); req.State != RequestReserved {
		t.Fatalf("state = %v, want still reserved before approval-based deadline", req.State)
	}
	// 批准后 30 秒：超时。
	c.t = c.t.Add(5 * time.Second)
	req, _ := w.Request("u1", "r1")
	if req.State != RequestReservationExpired {
		t.Fatalf("state = %v, want reservation expired", req.State)
	}
	if !req.ReserveExpiredAt.Equal(wantDeadline) {
		t.Fatalf("release time = %v, want deadline %v", req.ReserveExpiredAt, wantDeadline)
	}
}

func TestFailedApprovalDoesNotStartTimer(t *testing.T) {
	w, c := setupTimeout(t, 30*time.Second)
	// 一条关闭超时的策略，用门槛内的直接预留压占出资账户余额。
	fill := validPolicy(c)
	fill.ID = "p-fill"
	fill.MaxTotal = 1000
	if err := w.SavePolicy(fill); err != nil {
		t.Fatal(err)
	}
	// 开启审批的超时策略：等待期限 3 小时，预留超时 30 秒，累计额度放宽。
	p := timeoutPolicy(c, 30*time.Second)
	p.ID = "p-approval-timeout"
	p.MaxTotal = 1000
	p.ApprovalThreshold = 10
	p.ApprovalWait = 3 * time.Hour
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "dev-approve", c.t.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}

	in := timeoutApply()
	in.PolicyID = "p-approval-timeout"
	in.RequestID = "big"
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	// 再用九笔费用恰好等于门槛的直接预留各 10（在不超时的 p-fill 下），
	// 把可用余额压到 10（< 待审批费用 20）。
	for i := 0; i < 9; i++ {
		drop := baseApply()
		drop.PolicyID = "p-fill"
		drop.RequestID = fmt.Sprintf("drop%d", i)
		drop.EstimatedFee = 10
		if _, err := w.Apply(drop); err != nil {
			t.Fatal(err)
		}
	}
	// 批准时余额不足：失败且不能提前开始预留计时。
	if _, err := w.Approve("u1", "big", "sa", "dev-approve"); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("approve err = %v, want ErrInsufficientBalance", err)
	}

	// 即使越过“提交时刻 + 30 秒”，请求仍在待审批，没有任何预留计时信息。
	c.t = c.t.Add(40 * time.Second)
	req, _ := w.Request("u1", "big")
	if req.State != RequestPendingApproval {
		t.Fatalf("state = %v, want still pending", req.State)
	}
	if !req.ReservedAt.IsZero() || !req.ReserveDeadline.IsZero() || !req.ReserveExpiredAt.IsZero() {
		t.Fatalf("failed approval must not start timer: %+v", req)
	}

	// 释放一笔直接预留后，批准成功；截止时刻自批准成功时刻起算（t+40s+30s）。
	if _, err := w.Cancel("u1", "drop0"); err != nil {
		t.Fatal(err)
	}
	approved, err := w.Approve("u1", "big", "sa", "dev-approve")
	if err != nil {
		t.Fatal(err)
	}
	wantDeadline := c.t.Add(30 * time.Second)
	if !approved.ReservedAt.Equal(c.t) || !approved.ReserveDeadline.Equal(wantDeadline) {
		t.Fatalf("timing = %+v, want reservedAt %v deadline %v", approved, c.t, wantDeadline)
	}
	c.t = wantDeadline
	r, _ := w.Request("u1", "big")
	if r.State != RequestReservationExpired || !r.ReserveExpiredAt.Equal(wantDeadline) {
		t.Fatalf("final request = %+v, want timeout at %v", r, wantDeadline)
	}
}

func TestReservationTimeoutReleasesOnEveryQuery(t *testing.T) {
	for _, trigger := range []string{"Request", "Balance", "Account", "Policy", "Ledger", "AccountLedger"} {
		t.Run(trigger, func(t *testing.T) {
			w, c := setupTimeout(t, 30*time.Second)
			if _, err := w.Apply(timeoutApply()); err != nil {
				t.Fatal(err)
			}
			// 超过截止时刻很久才访问：释放时间仍必须是截止时刻。
			c.t = c.t.Add(10 * time.Minute)

			switch trigger {
			case "Request":
				if req, err := w.Request("u1", "r1"); err != nil || req.State != RequestReservationExpired {
					t.Fatalf("Request: %+v %v", req, err)
				}
			case "Balance":
				if bal, err := w.Balance("payer"); err != nil || bal != (Balances{Available: 100, Reserved: 0}) {
					t.Fatalf("Balance = %+v %v", bal, err)
				}
			case "Account":
				if acc, err := w.Account("payer"); err != nil || acc.Balances != (Balances{Available: 100, Reserved: 0}) {
					t.Fatalf("Account = %+v %v", acc, err)
				}
			case "Policy":
				if pv, err := w.Policy("p-timeout"); err != nil || pv.ReservedTotal != 0 {
					t.Fatalf("Policy reserved = %d %v", pv.ReservedTotal, err)
				}
			case "Ledger":
				if n := countKind(w.Ledger(), LedgerReservationExpiration); n != 1 {
					t.Fatalf("timeout entries = %d, want 1", n)
				}
			case "AccountLedger":
				if n := countKind(w.AccountLedger("payer"), LedgerRefund); n != 1 {
					t.Fatalf("refund entries = %d, want 1", n)
				}
			}

			// 无论从哪个入口触发，请求都已转为超时终态，资金全部释放。
			req, _ := w.Request("u1", "r1")
			if req.State != RequestReservationExpired {
				t.Fatalf("state = %v, want reservation expired", req.State)
			}
			if !req.ReserveExpiredAt.Equal(req.ReserveDeadline) {
				t.Fatalf("release time %v must equal deadline %v, not access time", req.ReserveExpiredAt, req.ReserveDeadline)
			}
			if req.ActualFee != 0 {
				t.Fatalf("actual fee = %d, want 0", req.ActualFee)
			}
			if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
				t.Fatalf("balance = %+v, want {100 0}", bal)
			}
			pv, _ := w.Policy("p-timeout")
			if pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
				t.Fatalf("totals = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
			}
		})
	}
}

func TestReservationTimeoutLedgerEntries(t *testing.T) {
	w, c := setupTimeout(t, 30*time.Second)
	if _, err := w.Apply(timeoutApply()); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Minute)
	if _, err := w.Request("u1", "r1"); err != nil {
		t.Fatal(err)
	}

	// 出资账户：原预留保留，新增一条全额退款（金额 20，关联请求）。
	payerLedger := w.AccountLedger("payer")
	if len(payerLedger) != 2 ||
		payerLedger[0].Kind != LedgerReserve || payerLedger[0].Amount != 20 ||
		payerLedger[1].Kind != LedgerRefund || payerLedger[1].Amount != 20 {
		t.Fatalf("payer ledger = %+v", payerLedger)
	}
	for _, e := range payerLedger {
		if e.RequestID != "r1" {
			t.Fatalf("entry missing request link: %+v", e)
		}
	}
	if payerLedger[1].Reason == "" {
		t.Fatal("refund reason must explain the timeout")
	}
	if payerLedger[1].At != c.t.Add(-30*time.Second) {
		t.Fatalf("refund recorded at %v, want deadline %v", payerLedger[1].At, c.t.Add(-30*time.Second))
	}

	// 使用账户：一条零金额超时记录，关联请求并说明原因。
	usageLedger := w.AccountLedger("u1")
	if len(usageLedger) != 1 ||
		usageLedger[0].Kind != LedgerReservationExpiration ||
		usageLedger[0].Amount != 0 || usageLedger[0].RequestID != "r1" ||
		usageLedger[0].Reason == "" {
		t.Fatalf("usage ledger = %+v", usageLedger)
	}
	// 状态记录不得混入出资账户账本，资金记录不得混入使用账户账本。
	if countKind(w.AccountLedger("payer"), LedgerReservationExpiration) != 0 {
		t.Fatal("payer ledger must not carry timeout status entry")
	}
	for _, e := range w.AccountLedger("u1") {
		if e.Kind == LedgerReserve || e.Kind == LedgerRefund {
			t.Fatalf("usage ledger must not carry money movement: %+v", e)
		}
	}
}

func TestSettleAfterReservationTimeout(t *testing.T) {
	w, c := setupTimeout(t, 30*time.Second)
	if _, err := w.Apply(timeoutApply()); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(30 * time.Second) // 恰为截止时刻

	// 非负实际费用的结算返回明确的预留已超时错误，不能扣款。
	if _, err := w.Settle("u1", "r1", 20); !errors.Is(err, ErrReservationExpired) {
		t.Fatalf("settle at deadline err = %v, want ErrReservationExpired", err)
	}
	if _, err := w.Settle("u1", "r1", 0); !errors.Is(err, ErrReservationExpired) {
		t.Fatalf("zero-fee settle err = %v, want ErrReservationExpired", err)
	}
	// 重复迟到结算仍然失败，绝不补扣。
	if _, err := w.Settle("u1", "r1", 5); !errors.Is(err, ErrReservationExpired) {
		t.Fatalf("late settle err = %v, want ErrReservationExpired", err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want fully refunded {100 0}", bal)
	}
	pv, _ := w.Policy("p-timeout")
	if pv.SpentTotal != 0 {
		t.Fatalf("spent total = %d, want 0", pv.SpentTotal)
	}
	req, _ := w.Request("u1", "r1")
	if req.State != RequestReservationExpired || req.ActualFee != 0 {
		t.Fatalf("request = %+v", req)
	}
}

func TestCancelAfterReservationTimeoutIsIdempotent(t *testing.T) {
	w, c := setupTimeout(t, 30*time.Second)
	if _, err := w.Apply(timeoutApply()); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Minute)

	// 取消返回已有的超时结果，不重复退回。
	view, err := w.Cancel("u1", "r1")
	if err != nil {
		t.Fatalf("cancel after timeout: %v", err)
	}
	if view.State != RequestReservationExpired {
		t.Fatalf("cancel view state = %v, want reservation expired", view.State)
	}
	if again, err := w.Cancel("u1", "r1"); err != nil || again.State != RequestReservationExpired {
		t.Fatalf("repeat cancel: %+v %v", again, err)
	}
	// 退款只发生一次。
	if n := countKind(w.Ledger(), LedgerRefund); n != 1 {
		t.Fatalf("refund entries = %d, want exactly 1", n)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v", bal)
	}
}

func TestReapplyAfterReservationTimeout(t *testing.T) {
	w, c := setupTimeout(t, 30*time.Second)
	if _, err := w.Apply(timeoutApply()); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Minute)
	before := len(w.Ledger())

	// 通过既有会话校验的同号同内容申请：返回超时结果，不重复预留、不留痕。
	view, err := w.Apply(timeoutApply())
	if err != nil {
		t.Fatalf("idempotent re-apply: %v", err)
	}
	if view.State != RequestReservationExpired {
		t.Fatalf("re-apply state = %v, want reservation expired", view.State)
	}
	if len(w.Ledger()) != before {
		t.Fatal("re-apply produced ledger entries")
	}
	// 同号不同内容仍返回冲突。
	conflict := timeoutApply()
	conflict.EstimatedFee = 19
	if _, err := w.Apply(conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict err = %v, want ErrConflict", err)
	}
	// 重新发起必须换请求编号：新编号可正常受理并复用已退回的费用。
	retry := timeoutApply()
	retry.RequestID = "r2"
	if r2, err := w.Apply(retry); err != nil || r2.State != RequestReserved {
		t.Fatalf("fresh request after timeout: %+v %v", r2, err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance = %+v, want {80 20}", bal)
	}
}

func TestReapproveAfterReservationTimeout(t *testing.T) {
	w, c := setupTimeout(t, 30*time.Second)
	p := timeoutPolicy(c, 30*time.Second)
	p.ID = "p-approval-timeout"
	p.ApprovalThreshold = 10
	p.ApprovalWait = 2 * time.Hour
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "dev-approve", c.t.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	in := timeoutApply()
	in.PolicyID = "p-approval-timeout"
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Minute)
	if _, err := w.Request("u1", "r1"); err != nil {
		t.Fatal(err)
	}

	// 重复批准不能恢复预留。
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrRequestNotPending) {
		t.Fatalf("re-approve after timeout err = %v, want ErrRequestNotPending", err)
	}
	// 审批身份校验仍按现有规则：无效会话先返回无权审批。
	if _, err := w.Approve("u1", "r1", "nope", "dev-approve"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("bad approver session err = %v, want ErrNotApprover", err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v", bal)
	}
}

func TestRefundedFundsReusableAcrossPolicies(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 20)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 同一出资账户下两条策略，各 20 累计额度、30 秒预留超时。
	mk := func(id string) {
		t.Helper()
		p := validPolicy(c)
		p.ID = id
		p.MaxPerRequest = 20
		p.MaxTotal = 20
		p.MaxReserveDuration = 30 * time.Second
		if err := w.SavePolicy(p); err != nil {
			t.Fatal(err)
		}
	}
	mk("pa")
	mk("pb")

	r1 := baseApply()
	r1.PolicyID, r1.RequestID = "pa", "ra"
	if _, err := w.Apply(r1); err != nil {
		t.Fatal(err)
	}
	// 另一策略在超时前无法使用同一出资账户余额与自己的额度。
	r2 := baseApply()
	r2.PolicyID, r2.RequestID = "pb", "rb"
	if _, err := w.Apply(r2); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("apply before release err = %v, want ErrInsufficientBalance", err)
	}

	// 先查余额与先申请结果一致：两者都会先释放 pa 下已到期的预留。
	c.t = c.t.Add(30 * time.Second)
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 20, Reserved: 0}) {
		t.Fatalf("balance after timeout = %+v", bal)
	}
	pa, _ := w.Policy("pa")
	if pa.ReservedTotal != 0 {
		t.Fatalf("pa reserved total = %d, want 0", pa.ReservedTotal)
	}
	if _, err := w.Apply(r2); err != nil {
		t.Fatalf("pb request should reuse refunded funds: %v", err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 0, Reserved: 20}) {
		t.Fatalf("balance = %+v, want {0 20}", bal)
	}

	// 迟到结算不能把已被新申请使用的费用扣走。
	if _, err := w.Settle("u1", "ra", 20); !errors.Is(err, ErrReservationExpired) {
		t.Fatalf("late settle err = %v, want ErrReservationExpired", err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 0, Reserved: 20}) {
		t.Fatalf("balance changed after late settle: %+v", bal)
	}
}

func TestTimeoutDisabledKeepsExistingBehavior(t *testing.T) {
	w, c := setupTimeout(t, 0)
	if _, err := w.Apply(timeoutApply()); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(24 * time.Hour)

	req, _ := w.Request("u1", "r1")
	if req.State != RequestReserved {
		t.Fatalf("state = %v, want reserved forever when timeout disabled", req.State)
	}
	if !req.ReservedAt.Equal(c.t.Add(-24*time.Hour)) || !req.ReserveDeadline.IsZero() ||
		!req.ReserveExpiredAt.IsZero() {
		t.Fatalf("timing fields = %+v, want no deadline/release when disabled", req)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance = %+v", bal)
	}
	// 结算与取消行为不变。
	if _, err := w.Settle("u1", "r1", 7); err != nil {
		t.Fatalf("settle without timeout: %v", err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 93, Reserved: 0}) {
		t.Fatalf("balance after settle = %+v", bal)
	}
	if countKind(w.Ledger(), LedgerReservationExpiration) != 0 {
		t.Fatal("disabled timeout must not leave timeout records")
	}
}

func TestReservationTimeoutBoundary(t *testing.T) {
	w, c := setupTimeout(t, 30*time.Second)
	if _, err := w.Apply(timeoutApply()); err != nil {
		t.Fatal(err)
	}

	// 截止前一刻：仍预留。
	c.t = c.t.Add(29 * time.Second)
	if req, _ := w.Request("u1", "r1"); req.State != RequestReserved {
		t.Fatalf("just before deadline state = %v", req.State)
	}
	// 恰为截止时刻：进入预留超时终态，结算（含零费用）返回超时错误。
	c.t = c.t.Add(time.Second)
	if _, err := w.Settle("u1", "r1", 0); !errors.Is(err, ErrReservationExpired) {
		t.Fatalf("settle exactly at deadline err = %v", err)
	}
	req, _ := w.Request("u1", "r1")
	if req.State != RequestReservationExpired {
		t.Fatalf("state at deadline = %v", req.State)
	}
	if !req.ReserveExpiredAt.Equal(c.t) {
		t.Fatalf("release at deadline = %v, want %v", req.ReserveExpiredAt, c.t)
	}
}

func TestSessionPolicyLifecycleDoesNotShortenDeadline(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	// 申请会话仅 2 分钟有效；策略窗口长达 4 小时、预留超时 1 小时。
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "dev-approve", c.t.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	p := validPolicy(c)
	p.ID = "p-long"
	p.EndsAt = c.t.Add(4 * time.Hour)
	p.MaxReserveDuration = time.Hour
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	in := baseApply()
	in.PolicyID = "p-long"
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	deadline := c.t.Add(time.Hour)

	// 申请会话在 2 分钟后到期且被吊销：已预留请求不受影响，截止时间不变。
	c.t = c.t.Add(90 * time.Second)
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	req, _ := w.Request("u1", "r1")
	if req.State != RequestReserved {
		t.Fatalf("session expiry/revoke must not release reservation: %v", req.State)
	}

	// 越过出资账户会话到期等事件，直到截止时刻之后：进入超时终态，
	// 释放时间仍是原截止时刻，而非很久之后的访问时刻。
	c.t = c.t.Add(90 * time.Minute)
	req, _ = w.Request("u1", "r1")
	if req.State != RequestReservationExpired {
		t.Fatalf("state = %v, want timeout (not released early)", req.State)
	}
	if !req.ReserveDeadline.Equal(deadline) || !req.ReserveExpiredAt.Equal(deadline) {
		t.Fatalf("deadline moved: deadline %v release %v, want %v", req.ReserveDeadline, req.ReserveExpiredAt, deadline)
	}

	// 停用策略不提前缩短未到期预留：构造第二笔，在截止前停用，仍需等到截止时刻。
	w2, c2 := setupTimeout(t, time.Hour)
	if _, err := w2.CreateSession("sa", "payer", "dev-approve", c2.t.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	in2 := timeoutApply()
	in2.RequestID = "r2"
	if _, err := w2.Apply(in2); err != nil {
		t.Fatal(err)
	}
	deadline2 := c2.t.Add(time.Hour)
	c2.t = c2.t.Add(30 * time.Minute)
	if _, err := w2.DeactivatePolicy("p-timeout", "sa", "dev-approve", "stop"); err != nil {
		t.Fatal(err)
	}
	if r, _ := w2.Request("u1", "r2"); r.State != RequestReserved {
		t.Fatalf("deactivation must not expire reservation early: %v", r.State)
	}
	c2.t = deadline2 // 到达原截止时刻
	r, _ := w2.Request("u1", "r2")
	if r.State != RequestReservationExpired {
		t.Fatalf("state at original deadline = %v, want reservation expired", r.State)
	}
	if !r.ReserveExpiredAt.Equal(deadline2) {
		t.Fatalf("release time = %v, want original deadline %v", r.ReserveExpiredAt, deadline2)
	}
	// 超时退款而非停用拒绝。
	if r.RejectReason != "" {
		t.Fatalf("timeout request must not carry reject reason: %q", r.RejectReason)
	}
	if countKind(w2.Ledger(), LedgerRejection) != 0 {
		t.Fatal("reservation timeout must not be recorded as rejection")
	}
}

func TestPolicyEndDoesNotShortenDeadline(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 策略 3 小时后结束，预留超时 2 小时：策略结束早于截止时刻但不释放预留。
	p := validPolicy(c)
	p.ID = "p-long"
	p.EndsAt = c.t.Add(3 * time.Hour)
	p.MaxReserveDuration = 2 * time.Hour
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	in := baseApply()
	in.PolicyID = "p-long"
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	deadline := c.t.Add(2 * time.Hour)
	c.t = c.t.Add(90 * time.Minute)
	if req, _ := w.Request("u1", "r1"); req.State != RequestReserved {
		t.Fatalf("policy end must not release reservation early: %v", req.State)
	}
	c.t = c.t.Add(30 * time.Minute)
	req, _ := w.Request("u1", "r1")
	if req.State != RequestReservationExpired || !req.ReserveDeadline.Equal(deadline) {
		t.Fatalf("state = %v deadline %v, want timeout at %v", req.State, req.ReserveDeadline, deadline)
	}
}

func TestSettleBeforeDeadlineTerminalUnchanged(t *testing.T) {
	w, c := setupTimeout(t, time.Minute)
	if _, err := w.Apply(timeoutApply()); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(30 * time.Second)
	if _, err := w.Settle("u1", "r1", 12); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Hour)
	// 已结算终态不被超时改写，不产生退款。
	req, _ := w.Request("u1", "r1")
	if req.State != RequestSettled || req.ActualFee != 12 || !req.ReserveExpiredAt.IsZero() {
		t.Fatalf("settled request rewritten: %+v", req)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 88, Reserved: 0}) {
		t.Fatalf("balance = %+v", bal)
	}
	if n := countKind(w.Ledger(), LedgerReservationExpiration); n != 0 {
		t.Fatalf("settled request produced timeout entries: %d", n)
	}

	// 取消路径同理：截止前取消的终态保持不变。
	w2, c2 := setupTimeout(t, time.Minute)
	in2 := timeoutApply()
	in2.RequestID = "r2"
	if _, err := w2.Apply(in2); err != nil {
		t.Fatal(err)
	}
	c2.t = c2.t.Add(30 * time.Second)
	if _, err := w2.Cancel("u1", "r2"); err != nil {
		t.Fatal(err)
	}
	c2.t = c2.t.Add(time.Hour)
	if r, _ := w2.Request("u1", "r2"); r.State != RequestCancelled {
		t.Fatalf("cancelled request rewritten: %v", r.State)
	}
	if n := countKind(w2.Ledger(), LedgerReservationExpiration); n != 0 {
		t.Fatalf("cancelled request produced timeout entries: %d", n)
	}
}

func TestConcurrentSettleCancelTimeoutSingleRefund(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		w, c := setupTimeout(t, time.Minute)
		in := timeoutApply()
		in.RequestID = fmt.Sprintf("race-%d", iter)
		if _, err := w.Apply(in); err != nil {
			t.Fatal(err)
		}
		c.t = c.t.Add(time.Minute) // 所有操作都在截止时刻及之后进入

		const n = 12
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			i := i
			go func() {
				defer wg.Done()
				switch i % 3 {
				case 0:
					_, _ = w.Settle("u1", fmt.Sprintf("race-%d", iter), 20)
				case 1:
					_, _ = w.Cancel("u1", fmt.Sprintf("race-%d", iter))
				case 2:
					_, _ = w.Request("u1", fmt.Sprintf("race-%d", iter))
				}
			}()
		}
		wg.Wait()

		req, _ := w.Request("u1", fmt.Sprintf("race-%d", iter))
		if req.State != RequestReservationExpired {
			t.Fatalf("iter %d: state = %v, want reservation expired", iter, req.State)
		}
		// 退款只能发生一次，超时留痕只能一条。
		if n := countKind(w.Ledger(), LedgerRefund); n != 1 {
			t.Fatalf("iter %d: refund entries = %d, want 1", iter, n)
		}
		if n := countKind(w.Ledger(), LedgerReservationExpiration); n != 1 {
			t.Fatalf("iter %d: timeout entries = %d, want 1", iter, n)
		}
		if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
			t.Fatalf("iter %d: balance = %+v", iter, bal)
		}
		pv, _ := w.Policy("p-timeout")
		if pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
			t.Fatalf("iter %d: totals = %+v", iter, pv)
		}
	}
}

func TestTimeoutRefundedMoneyReusedLateSettleBlocked(t *testing.T) {
	w, c := setupTimeout(t, 30*time.Second)
	if _, err := w.Apply(timeoutApply()); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(30 * time.Second)
	// 仅凭余额查询触发释放，随后新申请立即把退回的费用重新预留。
	if _, err := w.Balance("payer"); err != nil {
		t.Fatal(err)
	}
	next := timeoutApply()
	next.RequestID = "r2"
	if _, err := w.Apply(next); err != nil {
		t.Fatal(err)
	}
	// 迟到结算旧请求：超时错误，不能扣走已被 r2 使用的费用。
	if _, err := w.Settle("u1", "r1", 20); !errors.Is(err, ErrReservationExpired) {
		t.Fatalf("late settle err = %v, want ErrReservationExpired", err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance = %+v, want {80 20}", bal)
	}
	// 新请求仍可正常结算。
	if _, err := w.Settle("u1", "r2", 20); err != nil {
		t.Fatalf("new request settle: %v", err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 80, Reserved: 0}) {
		t.Fatalf("final balance = %+v, want {80 0}", bal)
	}
}
