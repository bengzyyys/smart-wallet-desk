package wallet

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// setupDisable 构造带策略的钱包：出资账户 payer（100），使用账户 u1/u2，
// 会话 s1/s2，出资账户绑定停用设备的会话 sd。
func setupDisable(t *testing.T) (*Wallet, *clock) {
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
	if _, err := w.CreateSession("sd", "payer", "dev-disable", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(validPolicy(c)); err != nil {
		t.Fatal(err)
	}
	return w, c
}

func TestDisablePolicyHappyPath(t *testing.T) {
	w, c := setupDisable(t)

	// 新保存的策略默认未停用。
	pv, err := w.Policy("p1")
	if err != nil {
		t.Fatal(err)
	}
	if pv.Disabled || !pv.DisabledAt.IsZero() || pv.DisabledBy != "" || pv.DisableReason != "" {
		t.Fatalf("new policy should not be disabled: %+v", pv)
	}

	before := c.t
	view, err := w.DisablePolicy("p1", "sd", "dev-disable", "  fraud detected  ")
	if err != nil {
		t.Fatal(err)
	}
	// 理由保存去掉首尾空白后的内容。
	if !view.Disabled || view.DisableReason != "fraud detected" {
		t.Fatalf("disabled view = %+v", view)
	}
	if view.DisabledBy != "payer" || !view.DisabledAt.Equal(before) {
		t.Fatalf("disabled metadata = by %q at %v", view.DisabledBy, view.DisabledAt)
	}

	// 查询策略能看到停用状态、首次停用时间、执行账户和理由。
	got, err := w.Policy("p1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Disabled || got.DisabledBy != "payer" || got.DisableReason != "fraud detected" {
		t.Fatalf("queried disabled view = %+v", got)
	}
	if !got.DisabledAt.Equal(before) {
		t.Fatalf("disabled at = %v, want %v", got.DisabledAt, before)
	}

	// 原来的策略条件及累计金额仍可查询。
	if got.MaxPerRequest != 30 || got.MaxTotal != 50 || got.Operation != "charge" || got.Payee != "shop" {
		t.Fatalf("policy conditions changed after disable: %+v", got)
	}

	// 账本含一条停用记录：出资账户 + 策略，金额为零。
	var found bool
	for _, e := range w.Ledger() {
		if e.Kind == LedgerPolicyDisabled {
			found = true
			if e.AccountID != "payer" || e.PolicyID != "p1" || e.Amount != 0 || e.Reason != "fraud detected" {
				t.Fatalf("disable ledger entry = %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("missing LedgerPolicyDisabled entry")
	}
}

func TestDisablePolicyIrrevocableAndIdempotent(t *testing.T) {
	w, _ := setupDisable(t)
	if _, err := w.DisablePolicy("p1", "sd", "dev-disable", "first reason"); err != nil {
		t.Fatal(err)
	}

	// 重复停用且理由一致：返回首次结果，不新增账本记录。
	before := len(w.Ledger())
	view, err := w.DisablePolicy("p1", "sd", "dev-disable", "first reason")
	if err != nil {
		t.Fatalf("idempotent disable: %v", err)
	}
	if view.DisableReason != "first reason" {
		t.Fatalf("idempotent disable changed reason: %q", view.DisableReason)
	}
	if len(w.Ledger()) != before {
		t.Fatal("ledger grew on idempotent disable")
	}

	// 理由不同：冲突，不改写首次信息。
	if _, err := w.DisablePolicy("p1", "sd", "dev-disable", "second reason"); !errors.Is(err, ErrConflict) {
		t.Fatalf("different reason err = %v, want ErrConflict", err)
	}
	got, _ := w.Policy("p1")
	if got.DisableReason != "first reason" {
		t.Fatalf("reason rewritten after conflict: %q", got.DisableReason)
	}
	if len(w.Ledger()) != before {
		t.Fatal("ledger grew on conflicting disable")
	}

	// 带空白的相同理由视为一致（保存及比较均使用去掉首尾空白后的内容）。
	if _, err := w.DisablePolicy("p1", "sd", "dev-disable", "  first reason  "); err != nil {
		t.Fatalf("whitespace-padded same reason should be idempotent: %v", err)
	}
}

func TestDisablePolicyValidation(t *testing.T) {
	w, _ := setupDisable(t)

	cases := []struct {
		name           string
		policyID       string
		sessionID      string
		deviceID       string
		reason         string
		wantErr        error
		wantUnchanged  bool
	}{
		{"missing policy id", "", "sd", "dev-disable", "r", ErrInvalidArgument, true},
		{"missing session id", "p1", "", "dev-disable", "r", ErrInvalidArgument, true},
		{"missing device id", "p1", "sd", "", "r", ErrInvalidArgument, true},
		{"missing reason", "p1", "sd", "dev-disable", "", ErrInvalidArgument, true},
		{"blank reason", "p1", "sd", "dev-disable", "   ", ErrInvalidArgument, true},
		{"policy not found", "ghost", "sd", "dev-disable", "r", ErrPolicyNotFound, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := w.DisablePolicy(tc.policyID, tc.sessionID, tc.deviceID, tc.reason)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantUnchanged {
				// 策略未被停用、账本未新增。
				pv, _ := w.Policy("p1")
				if pv.Disabled {
					t.Fatal("policy disabled after failed validation")
				}
			}
		})
	}
}

func TestDisablePolicyUnauthorized(t *testing.T) {
	w, _ := setupDisable(t)

	cases := []struct {
		name      string
		sessionID string
		deviceID  string
	}{
		{"unknown session", "nope", "dev-disable"},
		{"user account session", "s1", "dev1"},
		{"wrong device", "sd", "wrong-device"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := w.DisablePolicy("p1", tc.sessionID, tc.deviceID, "r")
			if !errors.Is(err, ErrNotPayer) {
				t.Fatalf("err = %v, want ErrNotPayer", err)
			}
		})
	}

	// 吊销后无权停用。
	if err := w.RevokeSession("sd"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.DisablePolicy("p1", "sd", "dev-disable", "r"); !errors.Is(err, ErrNotPayer) {
		t.Fatalf("revoked session err = %v, want ErrNotPayer", err)
	}

	// 到期后无权停用。
	w2, c2 := setupDisable(t)
	c2.t = c2.t.Add(time.Hour)
	if _, err := w2.DisablePolicy("p1", "sd", "dev-disable", "r"); !errors.Is(err, ErrNotPayer) {
		t.Fatalf("expired session err = %v, want ErrNotPayer", err)
	}

	// 上述失败均不改变策略、请求或账本。
	pv, _ := w.Policy("p1")
	if pv.Disabled {
		t.Fatal("policy disabled after unauthorized attempts")
	}
	if len(w.Ledger()) != 0 {
		t.Fatal("ledger grew after unauthorized attempts")
	}
}

func TestDisablePolicyNotStartedOrEnded(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("sd", "payer", "dev-disable", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 尚未开始的策略。
	notStarted := validPolicy(c)
	notStarted.ID = "p-future"
	notStarted.StartsAt = c.t.Add(time.Hour)
	notStarted.EndsAt = c.t.Add(2 * time.Hour)
	if err := w.SavePolicy(notStarted); err != nil {
		t.Fatal(err)
	}
	// 已经结束的策略。
	ended := validPolicy(c)
	ended.ID = "p-past"
	ended.StartsAt = c.t.Add(-2 * time.Hour)
	ended.EndsAt = c.t.Add(-time.Hour)
	if err := w.SavePolicy(ended); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"p-future", "p-past"} {
		if _, err := w.DisablePolicy(id, "sd", "dev-disable", "window closed"); err != nil {
			t.Fatalf("disable %s: %v", id, err)
		}
		pv, _ := w.Policy(id)
		if !pv.Disabled {
			t.Fatalf("policy %s should be disabled", id)
		}
	}
}

func TestDisabledPolicyIDCannotBeResaved(t *testing.T) {
	w, c := setupDisable(t)
	if _, err := w.DisablePolicy("p1", "sd", "dev-disable", "done"); err != nil {
		t.Fatal(err)
	}
	// 停用后的编号不能重新保存成新策略。
	if err := w.SavePolicy(validPolicy(c)); !errors.Is(err, ErrPolicyExists) {
		t.Fatalf("resave disabled id err = %v, want ErrPolicyExists", err)
	}
}

func TestDisableRejectsPendingBeforeDeadline(t *testing.T) {
	w, c := setupApproval(t)
	// 两笔待审批：r1（费用 20，期限 1 分钟后）、r2（费用 11）。
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}
	second := approvalApply()
	second.RequestID = "r2"
	second.EstimatedFee = 11
	if _, err := w.Apply(second); err != nil {
		t.Fatal(err)
	}
	// 一笔已预留（费用 ≤ 门槛，直接预留）。
	reserved := approvalApply()
	reserved.RequestID = "r3"
	reserved.EstimatedFee = 8
	if _, err := w.Apply(reserved); err != nil {
		t.Fatal(err)
	}

	// 停用时未到等待期限：两笔待审批立即转为拒绝终态。
	view, err := w.DisablePolicy("p-approval", "sa", "dev-approve", "  risk control  ")
	if err != nil {
		t.Fatal(err)
	}
	if !view.Disabled {
		t.Fatal("policy should be disabled")
	}

	for _, rid := range []string{"r1", "r2"} {
		req, err := w.Request("u1", rid)
		if err != nil {
			t.Fatal(err)
		}
		if req.State != RequestRejected {
			t.Fatalf("%s state = %v, want rejected", rid, req.State)
		}
		if req.DecidedAt.IsZero() {
			t.Fatalf("%s missing decision time", rid)
		}
		wantReason := "policy p-approval disabled: risk control"
		if req.RejectReason != wantReason {
			t.Fatalf("%s reject reason = %q, want %q", rid, req.RejectReason, wantReason)
		}
	}

	// 已预留请求保留原有状态和金额。
	req3, _ := w.Request("u1", "r3")
	if req3.State != RequestReserved {
		t.Fatalf("r3 state = %v, want reserved", req3.State)
	}
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 92, Reserved: 8}) {
		t.Fatalf("balance = %+v, want {92 8}", bal)
	}

	// 账本：拒绝记录关联使用账户与请求，金额为零。
	rejections := 0
	for _, e := range w.Ledger() {
		if e.Kind == LedgerRejection && e.RequestID != "" {
			rejections++
			if e.AccountID != "u1" || e.Amount != 0 {
				t.Fatalf("rejection entry = %+v", e)
			}
		}
	}
	if rejections != 2 {
		t.Fatalf("rejection entries = %d, want 2", rejections)
	}

	// 推进到等待期限之后再查询，状态不被改写。
	c.t = c.t.Add(2 * time.Minute)
	for _, rid := range []string{"r1", "r2"} {
		req, _ := w.Request("u1", rid)
		if req.State != RequestRejected {
			t.Fatalf("%s state after deadline = %v, want still rejected", rid, req.State)
		}
	}
}

func TestDisableExpiresPendingAtOrAfterDeadline(t *testing.T) {
	w, c := setupApproval(t)
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}
	// 另一笔待审批，期限相同。
	second := approvalApply()
	second.RequestID = "r2"
	second.EstimatedFee = 11
	if _, err := w.Apply(second); err != nil {
		t.Fatal(err)
	}

	// 推进到等待期限（1 分钟）：到期时刻即过期。
	c.t = c.t.Add(time.Minute)
	if _, err := w.DisablePolicy("p-approval", "sa", "dev-approve", "expired risk"); err != nil {
		t.Fatal(err)
	}

	for _, rid := range []string{"r1", "r2"} {
		req, err := w.Request("u1", rid)
		if err != nil {
			t.Fatal(err)
		}
		if req.State != RequestExpired {
			t.Fatalf("%s state = %v, want expired", rid, req.State)
		}
		if req.DecidedAt.IsZero() {
			t.Fatalf("%s missing decision time", rid)
		}
	}

	// 账本含过期留痕，金额为零。
	var found bool
	for _, e := range w.Ledger() {
		if e.Kind == LedgerExpiration && e.RequestID == "r1" {
			found = true
			if e.AccountID != "u1" || e.Amount != 0 {
				t.Fatalf("expiration entry = %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("missing LedgerExpiration entry")
	}

	// 余额与额度不变（待审批本就未冻结）。
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want untouched", bal)
	}
}

func TestDisableMixedPendingStates(t *testing.T) {
	w, c := setupApproval(t)
	// r1 期限 1 分钟后（未到），r2 期限 30 秒后（先到期）。
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}
	short := approvalApply()
	short.RequestID = "r2"
	short.EstimatedFee = 11
	// 用一条更短的会话制造更早的期限。
	if _, err := w.CreateSession("s-short", "u1", "dev1", c.t.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	short.SessionID = "s-short"
	if _, err := w.Apply(short); err != nil {
		t.Fatal(err)
	}

	// 推进 30 秒：r2 到期，r1 未到期。
	c.t = c.t.Add(30 * time.Second)
	if _, err := w.DisablePolicy("p-approval", "sa", "dev-approve", "mixed"); err != nil {
		t.Fatal(err)
	}

	r1, _ := w.Request("u1", "r1")
	r2, _ := w.Request("u1", "r2")
	if r1.State != RequestRejected {
		t.Fatalf("r1 state = %v, want rejected (not yet at deadline)", r1.State)
	}
	if r2.State != RequestExpired {
		t.Fatalf("r2 state = %v, want expired (at deadline)", r2.State)
	}
}

func TestDisableDoesNotAffectOtherPolicies(t *testing.T) {
	w, c := setupApproval(t)
	// 另一条策略 p-other，使用相同的出资账户与使用账户。
	other := approvalPolicy(c)
	other.ID = "p-other"
	if err := w.SavePolicy(other); err != nil {
		t.Fatal(err)
	}
	// 在 p-other 上申请一笔待审批。
	in := approvalApply()
	in.PolicyID = "p-other"
	in.RequestID = "ro"
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}

	// 停用 p-approval：p-other 不受影响。
	if _, err := w.DisablePolicy("p-approval", "sa", "dev-approve", "only first"); err != nil {
		t.Fatal(err)
	}

	// p-other 仍可申请（待审批）。
	again := approvalApply()
	again.PolicyID = "p-other"
	again.RequestID = "ro2"
	if _, err := w.Apply(again); err != nil {
		t.Fatalf("apply on other policy after disable: %v", err)
	}
	// p-other 的待审批请求仍可批准。
	if _, err := w.Approve("u1", "ro", "sa", "dev-approve"); err != nil {
		t.Fatalf("approve on other policy after disable: %v", err)
	}
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance = %+v, want {80 20}", bal)
	}
}

func TestApplyAfterDisableRejected(t *testing.T) {
	w, _ := setupDisable(t)
	if _, err := w.DisablePolicy("p1", "sd", "dev-disable", "nope"); err != nil {
		t.Fatal(err)
	}

	// 使用有效会话提交新请求：返回明确的策略已停用错误并留下拒绝原因。
	in := baseApply()
	in.RequestID = "r-new"
	req, err := w.Apply(in)
	if !errors.Is(err, ErrPolicyDisabled) {
		t.Fatalf("err = %v, want ErrPolicyDisabled", err)
	}
	if req.State != 0 {
		t.Fatalf("returned request should be zero value: %+v", req)
	}

	// 账本含拒绝记录，原因说明策略已停用。
	var found bool
	for _, e := range w.Ledger() {
		if e.Kind == LedgerRejection && e.RequestID == "r-new" {
			found = true
			if e.AccountID != "u1" || e.Amount != 0 {
				t.Fatalf("rejection entry = %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("missing rejection entry for apply on disabled policy")
	}

	// 余额和额度保持不变。
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want untouched", bal)
	}
	pv, _ := w.Policy("p1")
	if pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}
}

func TestApplyAfterDisableIdempotent(t *testing.T) {
	w, _ := setupDisable(t)
	// 停用前先受理一笔请求。
	in := baseApply()
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	if _, err := w.DisablePolicy("p1", "sd", "dev-disable", "done"); err != nil {
		t.Fatal(err)
	}

	// 同号同内容：继续返回已有结果，不重新受理。
	req, err := w.Apply(in)
	if err != nil {
		t.Fatalf("idempotent re-apply after disable: %v", err)
	}
	if req.State != RequestReserved {
		t.Fatalf("state = %v, want reserved (existing result)", req.State)
	}
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance = %+v, want {80 20} (no double reserve)", bal)
	}

	// 同号不同内容：冲突，不重新受理。
	conflict := in
	conflict.EstimatedFee = 21
	if _, err := w.Apply(conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("different content err = %v, want ErrConflict", err)
	}
	bal, _ = w.Balance("payer")
	if bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance changed after conflicting apply: %+v", bal)
	}
}

func TestDisableLedgerAccountLinks(t *testing.T) {
	w, _ := setupDisable(t)
	if _, err := w.DisablePolicy("p1", "sd", "dev-disable", "audit"); err != nil {
		t.Fatal(err)
	}

	// 按出资账户查询能核对到停用记录。
	payerLedger := w.AccountLedger("payer")
	var found bool
	for _, e := range payerLedger {
		if e.Kind == LedgerPolicyDisabled {
			found = true
			if e.PolicyID != "p1" || e.Amount != 0 {
				t.Fatalf("disable entry in payer ledger = %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("disable entry not found in payer account ledger")
	}
}

func TestConcurrentDisableOnlyOnce(t *testing.T) {
	w, _ := setupDisable(t)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = w.DisablePolicy("p1", "sd", "dev-disable", "concurrent")
		}()
	}
	wg.Wait()

	// 有且仅有一个停用成功，其余为幂等返回（理由一致）。
	disabled := 0
	for _, err := range errs {
		if err == nil {
			disabled++
		}
	}
	if disabled != 8 {
		t.Fatalf("successful disables = %d, want 8 (all idempotent)", disabled)
	}

	// 账本中停用记录只出现一次。
	count := 0
	for _, e := range w.Ledger() {
		if e.Kind == LedgerPolicyDisabled {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("disable ledger entries = %d, want 1", count)
	}

	// 余额与累计额度未重复变动。
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {100 0}", bal)
	}
}

func TestConcurrentDisableAndApply(t *testing.T) {
	w, _ := setupDisable(t)
	var wg sync.WaitGroup
	applyErrs := make([]error, 8)
	disableErr := make([]error, 1)

	// 并发：一部分申请，一部分停用。
	for i := 0; i < 8; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := baseApply()
			in.RequestID = fmt.Sprintf("race%d", i)
			_, applyErrs[i] = w.Apply(in)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, disableErr[0] = w.DisablePolicy("p1", "sd", "dev-disable", "race")
	}()
	wg.Wait()

	if disableErr[0] != nil {
		t.Fatalf("disable err = %v", disableErr[0])
	}

	// 统计受理、拒绝与因额度不足被拒的申请。
	accepted, rejected, quotaDenied := 0, 0, 0
	for _, err := range applyErrs {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrPolicyDisabled):
			rejected++
		case errors.Is(err, ErrQuotaExceeded), errors.Is(err, ErrInsufficientBalance):
			// 停用前已被额度或余额拦截，属合法结局。
			quotaDenied++
		default:
			t.Fatalf("unexpected apply err: %v", err)
		}
	}
	_ = quotaDenied

	// 最终状态必须自洽：停用后受理的申请为 0，或停用前受理的申请保持预留。
	pv, _ := w.Policy("p1")
	if !pv.Disabled {
		t.Fatal("policy should be disabled")
	}
	// 任何读取都不能看到策略已停用而仍有可批准的待审批请求。
	for _, req := range w.requests {
		if req.policyID == "p1" && req.state == RequestPendingApproval {
			t.Fatalf("request %s still pending after disable", req.requestID)
		}
	}

	// 余额守恒：预留总额 = 受理数 * 20。
	bal, _ := w.Balance("payer")
	if bal.Available+bal.Reserved != 100 {
		t.Fatalf("balance not conserved: %+v", bal)
	}
	if bal.Reserved != int64(accepted)*20 {
		t.Fatalf("reserved = %d, want %d (accepted %d)", bal.Reserved, accepted*20, accepted)
	}
}

func TestConcurrentDisableAndApprove(t *testing.T) {
	w, _ := setupApproval(t)
	// 先申请一笔待审批。
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var approveErr, disableErr error
	go func() { defer wg.Done(); _, approveErr = w.Approve("u1", "r1", "sa", "dev-approve") }()
	go func() { defer wg.Done(); _, disableErr = w.DisablePolicy("p-approval", "sa", "dev-approve", "race") }()
	wg.Wait()

	if disableErr != nil {
		t.Fatalf("disable err = %v", disableErr)
	}

	// 两种合法结局：
	//  - 批准先完成：请求预留，停用不影响已预留；
	//  - 停用先完成：请求拒绝，批准失败。
	req, _ := w.Request("u1", "r1")
	bal, _ := w.Balance("payer")
	if approveErr == nil {
		if req.State != RequestReserved {
			t.Fatalf("state = %v, want reserved (approve won)", req.State)
		}
		if bal != (Balances{Available: 80, Reserved: 20}) {
			t.Fatalf("balance = %+v, want {80 20}", bal)
		}
	} else if errors.Is(approveErr, ErrNotApprover) || errors.Is(approveErr, ErrRequestNotPending) {
		if req.State != RequestRejected {
			t.Fatalf("state = %v, want rejected (disable won)", req.State)
		}
		if bal != (Balances{Available: 100, Reserved: 0}) {
			t.Fatalf("balance = %+v, want untouched", bal)
		}
	} else {
		t.Fatalf("unexpected approve err = %v", approveErr)
	}
}

func TestDisableDoesNotRewriteTerminalRecords(t *testing.T) {
	w, _ := setupApproval(t)
	// 已结算、已拒绝、已过期、已取消的记录在停用后不改写。
	// r1: 批准后结算。
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Settle("u1", "r1", 20); err != nil {
		t.Fatal(err)
	}
	// r2: 拒绝。
	reject := approvalApply()
	reject.RequestID = "r2"
	reject.EstimatedFee = 11
	if _, err := w.Apply(reject); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Reject("u1", "r2", "sa", "dev-approve", "no"); err != nil {
		t.Fatal(err)
	}
	// r3: 取消。
	cancel := approvalApply()
	cancel.RequestID = "r3"
	cancel.EstimatedFee = 11
	if _, err := w.Apply(cancel); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Cancel("u1", "r3"); err != nil {
		t.Fatal(err)
	}

	if _, err := w.DisablePolicy("p-approval", "sa", "dev-approve", "cleanup"); err != nil {
		t.Fatal(err)
	}

	r1, _ := w.Request("u1", "r1")
	r2, _ := w.Request("u1", "r2")
	r3, _ := w.Request("u1", "r3")
	if r1.State != RequestSettled || r1.ActualFee != 20 {
		t.Fatalf("r1 = %+v, want settled", r1)
	}
	if r2.State != RequestRejected || r2.RejectReason != "no" {
		t.Fatalf("r2 = %+v, want rejected with original reason", r2)
	}
	if r3.State != RequestCancelled {
		t.Fatalf("r3 = %+v, want cancelled", r3)
	}
}
