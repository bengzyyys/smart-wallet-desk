package wallet

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// setupDeactivate 构造带审批策略的钱包，与 setupApproval 相同：
// 出资账户 payer（100），使用账户 u1/u2，会话 s1/s2，出资账户会话 sa。
func setupDeactivate(t *testing.T) (*Wallet, *clock) {
	t.Helper()
	return setupApproval(t)
}

func TestDeactivatePolicyViewAndLedger(t *testing.T) {
	w, c := setupDeactivate(t)

	// 新保存的策略默认未停用。
	before, err := w.Policy("p-approval")
	if err != nil {
		t.Fatal(err)
	}
	if before.Deactivated || !before.DeactivatedAt.IsZero() || before.DeactivatorAccountID != "" || before.DeactivateReason != "" {
		t.Fatalf("new policy should not be deactivated: %+v", before)
	}

	pv, err := w.DeactivatePolicy("p-approval", "sa", "dev-approve", "  fraud suspected  ")
	if err != nil {
		t.Fatal(err)
	}
	if !pv.Deactivated {
		t.Fatal("Deactivated = false, want true")
	}
	if !pv.DeactivatedAt.Equal(c.t) {
		t.Fatalf("deactivated at = %v, want %v", pv.DeactivatedAt, c.t)
	}
	if pv.DeactivatorAccountID != "payer" {
		t.Fatalf("deactivator = %q, want payer", pv.DeactivatorAccountID)
	}
	// 理由按去掉首尾空白后的内容保存。
	if pv.DeactivateReason != "fraud suspected" {
		t.Fatalf("reason = %q, want trimmed", pv.DeactivateReason)
	}
	// 原有策略条件与累计金额仍可查询。
	if pv.Operation != "charge" || pv.Payee != "shop" || pv.MaxPerRequest != 30 || pv.MaxTotal != 50 {
		t.Fatalf("policy conditions lost: %+v", pv.PolicySpec)
	}
	if pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("totals = %d/%d", pv.ReservedTotal, pv.SpentTotal)
	}

	// 账本：一条策略停用留痕，金额为零，关联出资账户与策略。
	var found bool
	for _, e := range w.Ledger() {
		if e.Kind == LedgerPolicyDeactivation {
			found = true
			if e.AccountID != "payer" || e.PolicyID != "p-approval" || e.RequestID != "" || e.Amount != 0 {
				t.Fatalf("deactivation entry = %+v", e)
			}
			if e.Reason != "fraud suspected" {
				t.Fatalf("ledger reason = %q", e.Reason)
			}
		}
	}
	if !found {
		t.Fatal("missing LedgerPolicyDeactivation entry")
	}
	// 按出资账户查询也能核对。
	var acct bool
	for _, e := range w.AccountLedger("payer") {
		if e.Kind == LedgerPolicyDeactivation {
			acct = true
		}
	}
	if !acct {
		t.Fatal("payer account ledger missing deactivation entry")
	}
	// 停用本身不动余额。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want untouched", bal)
	}
}

func TestDeactivatePolicyValidation(t *testing.T) {
	w, _ := setupDeactivate(t)

	for _, tc := range []struct {
		name    string
		policy  string
		session string
		device  string
		reason  string
	}{
		{"missing policy", "", "sa", "dev-approve", "stop"},
		{"missing session", "p-approval", "", "dev-approve", "stop"},
		{"missing device", "p-approval", "sa", "", "stop"},
		{"missing reason", "p-approval", "sa", "dev-approve", ""},
		{"blank reason", "p-approval", "sa", "dev-approve", "   "},
		{"whitespace reason", "p-approval", "sa", "dev-approve", "\t\n "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := w.DeactivatePolicy(tc.policy, tc.session, tc.device, tc.reason); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v, want ErrInvalidArgument", err)
			}
		})
	}
	// 参数校验失败不改策略与账本。
	if pv, _ := w.Policy("p-approval"); pv.Deactivated {
		t.Fatal("policy changed after invalid-argument calls")
	}
	if len(w.Ledger()) != 0 {
		t.Fatalf("ledger changed: %+v", w.Ledger())
	}

	// 策略不存在。
	if _, err := w.DeactivatePolicy("nope", "sa", "dev-approve", "stop"); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("err = %v, want ErrPolicyNotFound", err)
	}
}

func TestDeactivatePolicyAuthorization(t *testing.T) {
	w, c := setupDeactivate(t)

	// 先建一笔待审批，确认无权停用时请求也不变。
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}

	// 另一个使用账户的会话（不属于出资账户）。
	if _, err := w.CreateSession("su", "u1", "dev-approve", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 出资账户但设备不同。
	if _, err := w.CreateSession("sb", "payer", "other-device", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 已到期的出资账户会话。
	if _, err := w.CreateSession("se", "payer", "dev-approve", c.t.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		session string
		device  string
	}{
		{"unknown session", "nope", "dev-approve"},
		{"session of other account", "su", "dev-approve"},
		{"device mismatch", "sa", "wrong-device"},
		{"session bound to other device", "sb", "other-deviceX"},
		{"expired session", "se", "dev-approve"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := w.DeactivatePolicy("p-approval", tc.session, tc.device, "stop"); !errors.Is(err, ErrNotPolicyOwner) {
				t.Fatalf("err = %v, want ErrNotPolicyOwner", err)
			}
		})
	}

	// 已吊销的出资账户会话。
	if _, err := w.CreateSession("sr", "payer", "dev-approve", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := w.RevokeSession("sr"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.DeactivatePolicy("p-approval", "sr", "dev-approve", "stop"); !errors.Is(err, ErrNotPolicyOwner) {
		t.Fatalf("revoked session err = %v, want ErrNotPolicyOwner", err)
	}

	// 全部无权尝试后：策略未停用、请求仍待审批、账本只有原始待审批留痕。
	pv, _ := w.Policy("p-approval")
	if pv.Deactivated {
		t.Fatal("policy should still be active")
	}
	req, _ := w.Request("u1", "r1")
	if req.State != RequestPendingApproval {
		t.Fatalf("request state = %v, want still pending", req.State)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v", bal)
	}
	for _, e := range w.Ledger() {
		if e.Kind == LedgerPolicyDeactivation {
			t.Fatal("deactivation must not be recorded on unauthorized call")
		}
	}
}

func TestDeactivateConvertsPendingBeforeDeadlineToRejected(t *testing.T) {
	w, c := setupDeactivate(t)

	// 两笔待审批（费用均 > 门槛 10，等待期限为提交后 1 分钟）。
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}
	second := approvalApply()
	second.RequestID = "r2"
	second.EstimatedFee = 11
	if _, err := w.Apply(second); err != nil {
		t.Fatal(err)
	}
	// 一笔已预留：保留原状态与金额。
	reserved := approvalApply()
	reserved.RequestID = "r3"
	reserved.EstimatedFee = 8
	if _, err := w.Apply(reserved); err != nil {
		t.Fatal(err)
	}

	c.t = c.t.Add(30 * time.Second) // 未到 1 分钟等待期限
	pv, err := w.DeactivatePolicy("p-approval", "sa", "dev-approve", "fraud suspected")
	if err != nil {
		t.Fatal(err)
	}

	for _, rid := range []string{"r1", "r2"} {
		req, err := w.Request("u1", rid)
		if err != nil {
			t.Fatal(err)
		}
		if req.State != RequestRejected {
			t.Fatalf("%s state = %v, want rejected", rid, req.State)
		}
		if !req.DecidedAt.Equal(pv.DeactivatedAt) {
			t.Fatalf("%s decidedAt = %v, want deactivation time %v", rid, req.DecidedAt, pv.DeactivatedAt)
		}
		if req.ApproverAccountID != "payer" {
			t.Fatalf("%s approver = %q", rid, req.ApproverAccountID)
		}
		// 拒绝信息说明策略被停用并包含停用理由。
		if req.RejectReason == "" || !strings.Contains(req.RejectReason, "fraud suspected") || !strings.Contains(req.RejectReason, "deactivat") {
			t.Fatalf("%s reject reason = %q", rid, req.RejectReason)
		}
	}
	// 已预留请求保持预留。
	r3, _ := w.Request("u1", "r3")
	if r3.State != RequestReserved {
		t.Fatalf("reserved request state = %v", r3.State)
	}

	// 不冻结、不占用、不退款：只有 r3 的 8 处于预留。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 92, Reserved: 8}) {
		t.Fatalf("balance = %+v, want {92 8}", bal)
	}
	got, _ := w.Policy("p-approval")
	if got.ReservedTotal != 8 || got.SpentTotal != 0 {
		t.Fatalf("totals = reserved %d spent %d, want 8/0", got.ReservedTotal, got.SpentTotal)
	}

	// 账本：1 条停用（出资账户）+ 2 条拒绝（使用账户，各带请求号），金额均为零。
	deacts, rejs := 0, 0
	for _, e := range w.Ledger() {
		switch e.Kind {
		case LedgerPolicyDeactivation:
			deacts++
			if e.AccountID != "payer" || e.Amount != 0 {
				t.Fatalf("deactivation entry = %+v", e)
			}
		case LedgerRejection:
			if e.RequestID == "r1" || e.RequestID == "r2" {
				rejs++
				if e.AccountID != "u1" || e.Amount != 0 || !strings.Contains(e.Reason, "fraud suspected") {
					t.Fatalf("rejection entry = %+v", e)
				}
			}
		}
	}
	if deacts != 1 || rejs != 2 {
		t.Fatalf("deactivations = %d rejections = %d, want 1/2", deacts, rejs)
	}
	// 使用账户账本按账户可核对到两条拒绝。
	if n := countKind(w.AccountLedger("u1"), LedgerRejection); n != 2 {
		t.Fatalf("u1 rejection entries = %d, want 2", n)
	}

	// 已预留请求仍可结算或取消。
	if _, err := w.Settle("u1", "r3", 3); err != nil {
		t.Fatalf("settle reserved after deactivation: %v", err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 97, Reserved: 0}) {
		t.Fatalf("balance after settle = %+v, want {97 0}", bal)
	}
}

func TestDeactivateExpiresPendingAtOrAfterDeadline(t *testing.T) {
	w, c := setupDeactivate(t)
	in := approvalApply()
	in.SessionID = "s1"
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}

	// 恰到达等待期限（提交后 1 分钟）：过期而非停用拒绝，即使此前从未查询。
	c.t = c.t.Add(time.Minute)
	if _, err := w.DeactivatePolicy("p-approval", "sa", "dev-approve", "stop"); err != nil {
		t.Fatal(err)
	}
	req, _ := w.Request("u1", "r1")
	if req.State != RequestExpired {
		t.Fatalf("state = %v, want expired", req.State)
	}
	if !req.DecidedAt.Equal(c.t) {
		t.Fatalf("decidedAt = %v, want %v", req.DecidedAt, c.t)
	}
	for _, e := range w.Ledger() {
		if e.Kind == LedgerRejection && e.RequestID == "r1" {
			t.Fatalf("expired-by-deadline request must not be recorded as rejection: %+v", e)
		}
	}
	if countKind(w.Ledger(), LedgerExpiration) != 1 {
		t.Fatal("want exactly one expiration entry")
	}
	if countKind(w.Ledger(), LedgerPolicyDeactivation) != 1 {
		t.Fatal("want exactly one deactivation entry")
	}
	// 过期的拒绝信息不得被覆盖成停用理由。
	if strings.Contains(req.RejectReason, "stop") {
		t.Fatalf("expired request carries deactivation reason: %q", req.RejectReason)
	}
}

func TestDeactivateLeavesTerminalAndOtherPoliciesUntouched(t *testing.T) {
	w, c := setupDeactivate(t)

	// 另一条策略，带一笔待审批，停用 p-approval 时不应受影响。
	other := approvalPolicy(c)
	other.ID = "p-other"
	if err := w.SavePolicy(other); err != nil {
		t.Fatal(err)
	}
	otherIn := approvalApply()
	otherIn.PolicyID = "p-other"
	otherIn.RequestID = "ro1"
	if _, err := w.Apply(otherIn); err != nil {
		t.Fatal(err)
	}

	// p-approval 下构造各种状态。
	if _, err := w.Apply(approvalApply()); err != nil { // r1 pending
		t.Fatal(err)
	}
	if _, err := w.Reject("u1", "r1", "sa", "dev-approve", "manual no"); err != nil {
		t.Fatal(err)
	}
	reserved := approvalApply()
	reserved.RequestID = "r2"
	reserved.EstimatedFee = 8
	if _, err := w.Apply(reserved); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Settle("u1", "r2", 8); err != nil {
		t.Fatal(err)
	}
	cancelable := approvalApply()
	cancelable.RequestID = "r3"
	cancelable.EstimatedFee = 7
	if _, err := w.Apply(cancelable); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Cancel("u1", "r3"); err != nil {
		t.Fatal(err)
	}
	before := len(w.Ledger())

	if _, err := w.DeactivatePolicy("p-approval", "sa", "dev-approve", "stop"); err != nil {
		t.Fatal(err)
	}
	// 停用只增加 1 条停用留痕（p-approval 下已无待审批请求）。
	if got := len(w.Ledger()) - before; got != 1 {
		t.Fatalf("ledger grew by %d, want 1", got)
	}

	// 终态记录不改写。
	if r, _ := w.Request("u1", "r1"); r.State != RequestRejected || r.RejectReason != "manual no" {
		t.Fatalf("rejected rewritten: %+v", r)
	}
	if r, _ := w.Request("u1", "r2"); r.State != RequestSettled || r.ActualFee != 8 {
		t.Fatalf("settled rewritten: %+v", r)
	}
	if r, _ := w.Request("u1", "r3"); r.State != RequestCancelled {
		t.Fatalf("cancelled rewritten: %+v", r)
	}
	// 另一条策略的待审批请求保持可批准。
	if r, _ := w.Request("u1", "ro1"); r.State != RequestPendingApproval {
		t.Fatalf("other policy request state = %v", r.State)
	}
	if _, err := w.Approve("u1", "ro1", "sa", "dev-approve"); err != nil {
		t.Fatalf("other policy pending should still be approvable: %v", err)
	}
	if pv, _ := w.Policy("p-other"); pv.Deactivated {
		t.Fatal("other policy should stay active")
	}
}

func TestDeactivateNotStartedAndEndedPolicies(t *testing.T) {
	w, c := setupDeactivate(t)

	future := approvalPolicy(c)
	future.ID = "p-future"
	future.StartsAt = c.t.Add(time.Hour)
	future.EndsAt = c.t.Add(2 * time.Hour)
	if err := w.SavePolicy(future); err != nil {
		t.Fatal(err)
	}
	past := approvalPolicy(c)
	past.ID = "p-past"
	past.StartsAt = c.t.Add(-2 * time.Hour)
	past.EndsAt = c.t.Add(-time.Hour)
	if err := w.SavePolicy(past); err != nil {
		t.Fatal(err)
	}

	if _, err := w.DeactivatePolicy("p-future", "sa", "dev-approve", "stop"); err != nil {
		t.Fatalf("not-started policy should be deactivatable: %v", err)
	}
	if _, err := w.DeactivatePolicy("p-past", "sa", "dev-approve", "ended"); err != nil {
		t.Fatalf("ended policy should be deactivatable: %v", err)
	}
	// 停用后的编号不能重新保存成新策略。
	if err := w.SavePolicy(future); !errors.Is(err, ErrPolicyExists) {
		t.Fatalf("re-save deactivated id err = %v, want ErrPolicyExists", err)
	}
}

func TestDeactivateIdempotentAndConflict(t *testing.T) {
	w, _ := setupDeactivate(t)
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}

	first, err := w.DeactivatePolicy("p-approval", "sa", "dev-approve", "first reason")
	if err != nil {
		t.Fatal(err)
	}
	before := len(w.Ledger())

	// 理由带首尾空白，trim 后相同：返回首次结果，不留痕、不改写。
	again, err := w.DeactivatePolicy("p-approval", "sa", "dev-approve", "  first reason\n")
	if err != nil {
		t.Fatalf("idempotent deactivate: %v", err)
	}
	if !again.DeactivatedAt.Equal(first.DeactivatedAt) || again.DeactivateReason != "first reason" {
		t.Fatalf("repeat result differs: %+v vs %+v", again, first)
	}
	if len(w.Ledger()) != before {
		t.Fatal("ledger grew on idempotent deactivate")
	}
	// 首次信息未被改写。
	pv, _ := w.Policy("p-approval")
	if pv.DeactivateReason != "first reason" {
		t.Fatalf("first reason rewritten: %q", pv.DeactivateReason)
	}

	// 理由不同：冲突，仍不留痕、不改写。
	if _, err := w.DeactivatePolicy("p-approval", "sa", "dev-approve", "second reason"); !errors.Is(err, ErrConflict) {
		t.Fatalf("different reason err = %v, want ErrConflict", err)
	}
	if len(w.Ledger()) != before {
		t.Fatal("ledger grew on conflicting deactivate")
	}
	pv, _ = w.Policy("p-approval")
	if pv.DeactivateReason != "first reason" {
		t.Fatalf("first reason overwritten: %q", pv.DeactivateReason)
	}

	// 再次停用仍须通过会话校验。
	if _, err := w.DeactivatePolicy("p-approval", "s1", "dev1", "first reason"); !errors.Is(err, ErrNotPolicyOwner) {
		t.Fatalf("repeat with bad session err = %v, want ErrNotPolicyOwner", err)
	}
	if _, err := w.DeactivatePolicy("p-approval", "nope", "dev-approve", "first reason"); !errors.Is(err, ErrNotPolicyOwner) {
		t.Fatalf("repeat with missing session err = %v, want ErrNotPolicyOwner", err)
	}
	if len(w.Ledger()) != before {
		t.Fatal("ledger grew on unauthorized repeat")
	}
	// 待审批请求只被拒绝一次。
	if countKind(w.Ledger(), LedgerRejection) != 1 {
		t.Fatal("pending request was rejected more than once")
	}
}

func TestApplyAfterDeactivation(t *testing.T) {
	w, c := setupDeactivate(t)
	if _, err := w.DeactivatePolicy("p-approval", "sa", "dev-approve", "fraud suspected"); err != nil {
		t.Fatal(err)
	}
	before := len(w.Ledger())

	// 使用有效会话提交新申请：明确的策略已停用错误。
	in := approvalApply()
	_, err := w.Apply(in)
	if !errors.Is(err, ErrPolicyDeactivated) {
		t.Fatalf("err = %v, want ErrPolicyDeactivated", err)
	}
	if !strings.Contains(err.Error(), "fraud suspected") {
		t.Fatalf("error should carry reason: %v", err)
	}
	// 留下拒绝原因。
	if got := len(w.Ledger()) - before; got != 1 {
		t.Fatalf("ledger grew by %d, want 1 rejection", got)
	}
	last := w.Ledger()[len(w.Ledger())-1]
	if last.Kind != LedgerRejection || last.AccountID != "u1" || last.RequestID != "r1" || last.Amount != 0 || !strings.Contains(last.Reason, "fraud suspected") {
		t.Fatalf("rejection entry = %+v", last)
	}
	// 余额和额度保持不变。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v", bal)
	}
	if pv, _ := w.Policy("p-approval"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("totals changed: %+v", pv)
	}
	_ = c
}

func TestReapplyHistoricalRequestAfterDeactivation(t *testing.T) {
	w, _ := setupDeactivate(t)

	// 停用前受理一笔已预留、一笔待审批（会被停用拒绝）。
	reserved := approvalApply()
	reserved.RequestID = "rd"
	reserved.EstimatedFee = 8
	if _, err := w.Apply(reserved); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.DeactivatePolicy("p-approval", "sa", "dev-approve", "stop"); err != nil {
		t.Fatal(err)
	}
	before := len(w.Ledger())

	// 同号同内容：继续返回已有结果，不重新受理、不留痕。
	if view, err := w.Apply(approvalApply()); err != nil || view.State != RequestRejected {
		t.Fatalf("re-apply rejected request: view=%+v err=%v", view, err)
	}
	if view, err := w.Apply(reserved); err != nil || view.State != RequestReserved {
		t.Fatalf("re-apply reserved request: view=%+v err=%v", view, err)
	}
	if len(w.Ledger()) != before {
		t.Fatal("historical re-apply must not add ledger entries")
	}
	// 同号不同内容：冲突，历史请求不被重新受理。
	conflict := approvalApply()
	conflict.EstimatedFee = 19
	if _, err := w.Apply(conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict err = %v, want ErrConflict", err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 92, Reserved: 8}) {
		t.Fatalf("balance changed on historical re-apply: %+v", bal)
	}
}

func TestConcurrentDeactivateAndApprove(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		w, _ := setupDeactivate(t)
		if _, err := w.Apply(approvalApply()); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		var approveErr, deactivateErr error
		go func() { defer wg.Done(); _, approveErr = w.Approve("u1", "r1", "sa", "dev-approve") }()
		go func() {
			defer wg.Done()
			_, deactivateErr = w.DeactivatePolicy("p-approval", "sa", "dev-approve", "concurrent stop")
		}()
		wg.Wait()

		req, _ := w.Request("u1", "r1")
		pv, _ := w.Policy("p-approval")
		bal, _ := w.Balance("payer")

		if approveErr == nil {
			// 批准先完成：请求已预留并保留预留，随后停用不再处理它。
			if deactivateErr != nil {
				t.Fatalf("deactivate after approve should succeed: %v", deactivateErr)
			}
			if req.State != RequestReserved {
				t.Fatalf("iter %d: approve first but state = %v", iter, req.State)
			}
			if bal != (Balances{Available: 80, Reserved: 20}) {
				t.Fatalf("iter %d: balance = %+v, want {80 20}", iter, bal)
			}
			// 已预留仍可结算。
			if _, err := w.Settle("u1", "r1", 20); err != nil {
				t.Fatalf("iter %d: settle after approve+deactivate: %v", iter, err)
			}
		} else {
			// 停用先完成：请求必须是拒绝终态，不能再批准。
			if req.State != RequestRejected {
				t.Fatalf("iter %d: deactivate first but state = %v approveErr=%v", iter, req.State, approveErr)
			}
			if !errors.Is(approveErr, ErrRequestNotPending) {
				t.Fatalf("iter %d: approve after deactivate err = %v, want ErrRequestNotPending", iter, approveErr)
			}
			if bal != (Balances{Available: 100, Reserved: 0}) {
				t.Fatalf("iter %d: balance = %+v, want untouched", iter, bal)
			}
		}
		// 不变量：策略已停用时绝不存在可批准的待审批请求。
		if pv.Deactivated && req.State == RequestPendingApproval {
			t.Fatalf("iter %d: deactivated policy still has approvable pending request", iter)
		}
		// 停用留痕恰好一条。
		if countKind(w.Ledger(), LedgerPolicyDeactivation) != 1 {
			t.Fatalf("iter %d: deactivation entries != 1", iter)
		}
	}
}

func TestConcurrentDeactivateAndApply(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		w, _ := setupDeactivate(t)

		var wg sync.WaitGroup
		wg.Add(2)
		var applyErr, deactivateErr error
		in := approvalApply() // 费用 20 > 门槛 10，申请成功会进入待审批，不冻结余额
		go func() { defer wg.Done(); _, applyErr = w.Apply(in) }()
		go func() {
			defer wg.Done()
			_, deactivateErr = w.DeactivatePolicy("p-approval", "sa", "dev-approve", "race stop")
		}()
		wg.Wait()

		if deactivateErr != nil {
			t.Fatalf("deactivate failed: %v", deactivateErr)
		}
		req, _ := w.Request("u1", "r1")
		if applyErr == nil {
			// 申请先完成：请求停留审批时被停用立即拒绝（未到期限）。
			if req.State != RequestRejected {
				t.Fatalf("iter %d: apply first, want rejected after deactivation, got %v", iter, req.State)
			}
		} else {
			// 停用先完成：新申请被策略已停用拒绝，请求不存在。
			if !errors.Is(applyErr, ErrPolicyDeactivated) {
				t.Fatalf("iter %d: apply err = %v, want ErrPolicyDeactivated", iter, applyErr)
			}
			if _, err := w.Request("u1", "r1"); !errors.Is(err, ErrRequestNotFound) {
				t.Fatalf("iter %d: rejected-by-deactivation request should not exist", iter)
			}
		}
		// 待审批与停用拒绝都不冻结余额。
		if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
			t.Fatalf("iter %d: balance = %+v, want untouched", iter, bal)
		}
		if countKind(w.Ledger(), LedgerPolicyDeactivation) != 1 {
			t.Fatalf("iter %d: deactivation entries != 1", iter)
		}
	}
}

func TestConcurrentRepeatDeactivation(t *testing.T) {
	w, _ := setupDeactivate(t)
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = w.DeactivatePolicy("p-approval", "sa", "dev-approve", "concurrent stop")
		}()
	}
	wg.Wait()

	// 并发重复停用只能记录一次；待审批只拒绝一次。
	if countKind(w.Ledger(), LedgerPolicyDeactivation) != 1 {
		t.Fatal("deactivation recorded more than once")
	}
	if countKind(w.Ledger(), LedgerRejection) != 1 {
		t.Fatal("pending request rejected more than once")
	}
	pv, _ := w.Policy("p-approval")
	if !pv.Deactivated || pv.DeactivateReason != "concurrent stop" {
		t.Fatalf("policy view = %+v", pv)
	}
	// 余额与累计额度无变动。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v", bal)
	}
	if pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("totals changed: %+v", pv)
	}
}

func TestDeactivateDistinguishesLedgerKinds(t *testing.T) {
	w, _ := setupDeactivate(t)
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.DeactivatePolicy("p-approval", "sa", "dev-approve", "stop"); err != nil {
		t.Fatal(err)
	}

	// 停用与请求拒绝是两类记录：分别关联出资账户+策略、使用账户+请求。
	var deact, rej int
	for _, e := range w.Ledger() {
		switch e.Kind {
		case LedgerPolicyDeactivation:
			deact++
			if e.AccountID != "payer" || e.PolicyID != "p-approval" || e.RequestID != "" || e.Amount != 0 {
				t.Fatalf("bad deactivation entry: %+v", e)
			}
		case LedgerRejection:
			rej++
			if e.AccountID != "u1" || e.PolicyID != "" || e.RequestID != "r1" || e.Amount != 0 {
				t.Fatalf("bad rejection entry: %+v", e)
			}
		}
	}
	if deact != 1 || rej != 1 {
		t.Fatalf("deactivation = %d rejection = %d, want 1/1", deact, rej)
	}
	// 按账户查询也能核对：出资账户只见停用，使用账户只见拒绝。
	if countKind(w.AccountLedger("payer"), LedgerPolicyDeactivation) != 1 ||
		countKind(w.AccountLedger("payer"), LedgerRejection) != 0 {
		t.Fatalf("payer ledger = %+v", w.AccountLedger("payer"))
	}
	if countKind(w.AccountLedger("u1"), LedgerRejection) != 1 ||
		countKind(w.AccountLedger("u1"), LedgerPolicyDeactivation) != 0 {
		t.Fatalf("u1 ledger = %+v", w.AccountLedger("u1"))
	}
}

func countKind(entries []LedgerEntry, kind LedgerKind) int {
	n := 0
	for _, e := range entries {
		if e.Kind == kind {
			n++
		}
	}
	return n
}
