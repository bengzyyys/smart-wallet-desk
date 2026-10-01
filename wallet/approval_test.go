package wallet

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// approvalSpec 在 validPolicy 基础上开启大额审批。
func approvalSpec(c *clock, threshold int64, wait time.Duration) PolicySpec {
	spec := validPolicy(c) // 单次 30、共享 50、窗口 [-1h, +1h)
	spec.ApprovalThreshold = threshold
	spec.ApprovalWait = wait
	return spec
}

// setupApproval 构造：payer(100) + 使用账户 u1/u2；使用会话 30 分钟到期；
// 出资账户会话 pa/pdev 30 分钟到期；p1 按给定门槛与等待时长开启审批。
func setupApproval(t *testing.T, threshold int64, wait time.Duration) (*Wallet, *clock) {
	t.Helper()
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s2", "u2", "dev2", c.t.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("pa", "payer", "pdev", c.t.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(approvalSpec(c, threshold, wait)); err != nil {
		t.Fatal(err)
	}
	return w, c
}

func payerApproval() ApprovalInput {
	return ApprovalInput{ApproverAccountID: "payer", SessionID: "pa", DeviceID: "pdev"}
}

// applyLarge 以 u1 会话提交 fee=25 的申请（门槛 10 时进入待审批）。
func applyPending(t *testing.T, w *Wallet, requestID string, fee int64) RequestView {
	t.Helper()
	in := baseApply()
	in.RequestID = requestID
	in.EstimatedFee = fee
	view, err := w.Apply(in)
	if err != nil {
		t.Fatalf("apply pending request: %v", err)
	}
	return view
}

func countEntries(entries []LedgerEntry, kind LedgerKind, requestID string) int {
	n := 0
	for _, e := range entries {
		if e.Kind == kind && (requestID == "" || e.RequestID == requestID) {
			n++
		}
	}
	return n
}

func TestSavePolicyApprovalValidation(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)

	base := approvalSpec(c, 0, 0)
	base.AllowedAccountIDs = []string{"u1"}

	// 门槛为零表示关闭：等待时长被忽略（零甚至为负都可以保存）。
	off := base
	if err := w.SavePolicy(off); err != nil {
		t.Fatalf("approval off with zero wait: %v", err)
	}
	offNeg := base
	offNeg.ID = "p-off-neg"
	offNeg.ApprovalWait = -time.Second
	if err := w.SavePolicy(offNeg); err != nil {
		t.Fatalf("approval off should ignore wait: %v", err)
	}

	cases := []struct {
		name      string
		mutate    func(*PolicySpec)
		wantExact bool
	}{
		{"negative threshold", func(s *PolicySpec) { s.ApprovalThreshold = -1 }, false},
		{"threshold over per-request", func(s *PolicySpec) {
			s.ApprovalThreshold = s.MaxPerRequest + 1
		}, false},
		{"enabled but zero wait", func(s *PolicySpec) {
			s.ApprovalThreshold = 10
			s.ApprovalWait = 0
		}, false},
		{"enabled but negative wait", func(s *PolicySpec) {
			s.ApprovalThreshold = 10
			s.ApprovalWait = -time.Minute
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := base
			spec.ID = "p-" + strings.ReplaceAll(tc.name, " ", "-")
			tc.mutate(&spec)
			if err := w.SavePolicy(spec); !errors.Is(err, ErrPolicyInvalid) {
				t.Fatalf("err = %v, want ErrPolicyInvalid", err)
			}
		})
	}

	// 门槛等于单次上限、等待为正：允许。
	eq := base
	eq.ID = "p-eq"
	eq.ApprovalThreshold = eq.MaxPerRequest
	eq.ApprovalWait = time.Minute
	if err := w.SavePolicy(eq); err != nil {
		t.Fatalf("threshold == per-request limit: %v", err)
	}
	pv, _ := w.Policy("p-eq")
	if pv.ApprovalThreshold != eq.MaxPerRequest || pv.ApprovalWait != time.Minute {
		t.Fatalf("policy view lost approval config: %+v", pv)
	}
}

func TestApplyRoutesByThreshold(t *testing.T) {
	w, c := setupApproval(t, 10, 10*time.Minute)
	now := c.t

	// 费用等于门槛：未严格超过，直接预留（原有行为）。
	atThreshold := applyPending(t, w, "r-eq", 10)
	if atThreshold.State != RequestReserved {
		t.Fatalf("fee == threshold: state = %v, want reserved", atThreshold.State)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 90, Reserved: 10}) {
		t.Fatalf("balance = %+v", bal)
	}

	// 费用严格超过门槛：待审批，不冻结余额、不占用共享额度。
	view := applyPending(t, w, "r-big", 25)
	if view.State != RequestPendingApproval {
		t.Fatalf("state = %v, want pending approval", view.State)
	}
	wantWait := now.Add(10 * time.Minute) // wait=10m 早于策略结束(+1h)与会话到期(+30m)
	if !view.WaitUntil.Equal(wantWait) {
		t.Fatalf("wait until = %s, want %s", view.WaitUntil, wantWait)
	}
	if !view.DecidedAt.IsZero() || view.ApproverAccountID != "" || view.RejectReason != "" {
		t.Fatalf("pending view should have no decision: %+v", view)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 90, Reserved: 10}) {
		t.Fatalf("pending request must not freeze balance: %+v", bal)
	}
	pv, _ := w.Policy("p1")
	if pv.ReservedTotal != 10 {
		t.Fatalf("pending request must not occupy shared quota: reserved %d", pv.ReservedTotal)
	}
	all := w.Ledger()
	pendingEntries := 0
	for _, e := range all {
		if e.RequestID == "r-big" {
			if e.Kind != LedgerPendingApproval {
				t.Fatalf("pending request ledger kind = %v, want pending", e.Kind)
			}
			if e.Amount != 0 || e.AccountID != "" || e.UsageAccountID != "u1" {
				t.Fatalf("pending entry must not move money and link usage account: %+v", e)
			}
			pendingEntries++
		}
	}
	if pendingEntries != 1 {
		t.Fatalf("pending entries = %d, want 1", pendingEntries)
	}
}

func TestApproveReservesAndSettles(t *testing.T) {
	w, c := setupApproval(t, 10, 10*time.Minute)
	_ = c
	view := applyPending(t, w, "r1", 25)
	if view.State != RequestPendingApproval {
		t.Fatalf("state = %v", view.State)
	}

	approved, err := w.Approve("u1", "r1", payerApproval())
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.State != RequestReserved {
		t.Fatalf("approved = %+v", approved)
	}
	if approved.ApproverAccountID != "payer" || approved.DecidedAt.IsZero() {
		t.Fatalf("decision fields missing: %+v", approved)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 75, Reserved: 25}) {
		t.Fatalf("balance after approve = %+v, want {75 25}", bal)
	}
	pv, _ := w.Policy("p1")
	if pv.ReservedTotal != 25 {
		t.Fatalf("quota after approve = %d, want 25", pv.ReservedTotal)
	}
	// 账本顺序：待审批 -> 批准状态(金额 0) -> 预留资金。
	entries := []LedgerEntry{}
	for _, e := range w.Ledger() {
		if e.RequestID == "r1" {
			entries = append(entries, e)
		}
	}
	if len(entries) != 3 ||
		entries[0].Kind != LedgerPendingApproval ||
		entries[1].Kind != LedgerApproval || entries[1].Amount != 0 ||
		entries[2].Kind != LedgerReserve || entries[2].Amount != 25 || entries[2].AccountID != "payer" {
		t.Fatalf("approval ledger = %+v", entries)
	}

	// 批准预留后的结算仍走原资金账本规则。
	settled, err := w.Settle("u1", "r1", 20)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != RequestSettled {
		t.Fatalf("settled = %+v", settled)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 80, Reserved: 0}) {
		t.Fatalf("balance after settle = %+v", bal)
	}
}

func TestApproveRechecksBalance(t *testing.T) {
	// 自定义策略：门槛 10、单次 100、共享 200，使额度充足只暴露余额问题。
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("pa", "payer", "pdev", c.t.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	spec := approvalSpec(c, 10, 10*time.Minute)
	spec.MaxPerRequest = 100
	spec.MaxTotal = 200
	if err := w.SavePolicy(spec); err != nil {
		t.Fatal(err)
	}
	pending := applyPending(t, w, "r-big", 90)
	if pending.State != RequestPendingApproval {
		t.Fatalf("state = %v", pending.State)
	}

	// 用 8 笔小额直接预留把可用余额压到 20（< 90），共享额度仍充足。
	for i := 0; i < 8; i++ {
		applyPending(t, w, fmt.Sprintf("small-%d", i), 10)
	}
	if bal, _ := w.Balance("payer"); bal.Available != 20 {
		t.Fatalf("setup balance = %+v", bal)
	}
	if _, err := w.Approve("u1", "r-big", payerApproval()); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("approve err = %v, want ErrInsufficientBalance", err)
	}
	view, _ := w.Request("u1", "r-big")
	if view.State != RequestPendingApproval {
		t.Fatalf("request must stay pending, got %v", view.State)
	}

	// 释放余额后，期限内可再次批准并成功。
	for i := 0; i < 8; i++ {
		if _, err := w.Cancel("u1", fmt.Sprintf("small-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	approved, err := w.Approve("u1", "r-big", payerApproval())
	if err != nil {
		t.Fatalf("re-approve after top-up: %v", err)
	}
	if approved.State != RequestReserved {
		t.Fatalf("state = %v", approved.State)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 10, Reserved: 90}) {
		t.Fatalf("balance = %+v", bal)
	}
}

func TestApproveRechecksSharedQuota(t *testing.T) {
	w, _ := setupApproval(t, 10, 10*time.Minute) // 共享 50
	applyPending(t, w, "r-big", 30)

	// 三笔直接预留各 10：已用 30，30+30=60 > 50；余额 70 仍充足。
	for i := 0; i < 3; i++ {
		applyPending(t, w, fmt.Sprintf("small-%d", i), 10)
	}
	if _, err := w.Approve("u1", "r-big", payerApproval()); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("approve err = %v, want ErrQuotaExceeded", err)
	}
	view, _ := w.Request("u1", "r-big")
	if view.State != RequestPendingApproval {
		t.Fatalf("request must stay pending, got %v", view.State)
	}
	// 余额不应变化。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 70, Reserved: 30}) {
		t.Fatalf("balance changed on failed approve: %+v", bal)
	}

	// 取消一笔释放 10 额度后批准成功。
	if _, err := w.Cancel("u1", "small-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Approve("u1", "r-big", payerApproval()); err != nil {
		t.Fatalf("approve after quota freed: %v", err)
	}
	pv, _ := w.Policy("p1")
	if pv.ReservedTotal != 50 {
		t.Fatalf("quota = %d, want 50", pv.ReservedTotal)
	}
}

func TestApprovalAuthorization(t *testing.T) {
	w, _ := setupApproval(t, 10, 10*time.Minute)
	mustAccount(t, w, "other", 0)
	if _, err := w.CreateSession("so", "other", "odev", cPlus(w, 30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	applyPending(t, w, "r1", 25)

	entriesBefore := len(w.Ledger())
	cases := []struct {
		name string
		in   ApprovalInput
	}{
		{"other account", ApprovalInput{ApproverAccountID: "other", SessionID: "so", DeviceID: "odev"}},
		{"payer wrong device", ApprovalInput{ApproverAccountID: "payer", SessionID: "pa", DeviceID: "wrong"}},
		{"unknown session", ApprovalInput{ApproverAccountID: "payer", SessionID: "ghost", DeviceID: "pdev"}},
		{"session of another account", ApprovalInput{ApproverAccountID: "payer", SessionID: "s1", DeviceID: "dev1"}},
		{"missing fields", ApprovalInput{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := w.Approve("u1", "r1", tc.in); !errors.Is(err, ErrApprovalUnauthorized) {
				t.Fatalf("approve err = %v, want ErrApprovalUnauthorized", err)
			}
			view, _ := w.Request("u1", "r1")
			if view.State != RequestPendingApproval {
				t.Fatalf("request changed after unauthorized approve: %v", view.State)
			}
		})
	}
	if got := len(w.Ledger()); got != entriesBefore {
		t.Fatalf("unauthorized approve produced ledger entries: %d vs %d", got, entriesBefore)
	}

	// 出资账户会话过期或吊销同样无权审批。
	if err := w.RevokeSession("pa"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Approve("u1", "r1", payerApproval()); !errors.Is(err, ErrApprovalUnauthorized) {
		t.Fatalf("revoked approver session err = %v", err)
	}
}

// cPlus 通过钱包时钟取“当前时刻 + d”，避免测试直接依赖 clock 变量。
func cPlus(w *Wallet, d time.Duration) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now().Add(d)
}

func TestRejectFlow(t *testing.T) {
	w, _ := setupApproval(t, 10, 10*time.Minute)
	applyPending(t, w, "r1", 25)

	// 空或全空白理由：参数错误，请求保持待审批。
	for _, blank := range []string{"", "   ", "\t\n"} {
		in := payerApproval()
		in.Reason = blank
		if _, err := w.Reject("u1", "r1", in); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("blank reason %q err = %v", blank, err)
		}
	}
	view, _ := w.Request("u1", "r1")
	if view.State != RequestPendingApproval {
		t.Fatalf("request changed after blank rejects: %v", view.State)
	}
	ledgerBefore := len(w.Ledger())

	// 非出资账户不能拒绝。
	bad := ApprovalInput{ApproverAccountID: "u1", SessionID: "s1", DeviceID: "dev1", Reason: "nope"}
	if _, err := w.Reject("u1", "r1", bad); !errors.Is(err, ErrApprovalUnauthorized) {
		t.Fatalf("unauthorized reject err = %v", err)
	}

	// 正常拒绝：终态 + 原因 + 审批账户，无资金变动。
	in := payerApproval()
	in.Reason = "suspicious merchant"
	rejected, err := w.Reject("u1", "r1", in)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.State != RequestRejected || rejected.RejectReason != in.Reason || rejected.ApproverAccountID != "payer" {
		t.Fatalf("rejected = %+v", rejected)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("reject must not move money: %+v", bal)
	}
	if got := countEntries(w.Ledger(), LedgerApprovalRejected, "r1"); got != 1 {
		t.Fatalf("rejection entries = %d, want 1", got)
	}

	// 相同理由重复拒绝：幂等，不重复留痕。
	if _, err := w.Reject("u1", "r1", in); err != nil {
		t.Fatalf("idempotent reject: %v", err)
	}
	if got := countEntries(w.Ledger(), LedgerApprovalRejected, "r1"); got != 1 {
		t.Fatalf("repeated reject left extra trace: %d", got)
	}
	// 不同理由：冲突，请求保持原理由。
	in2 := in
	in2.Reason = "changed mind"
	if _, err := w.Reject("u1", "r1", in2); !errors.Is(err, ErrConflict) {
		t.Fatalf("different reason reject err = %v, want ErrConflict", err)
	}
	view, _ = w.Request("u1", "r1")
	if view.RejectReason != "suspicious merchant" {
		t.Fatalf("reason changed after conflicting reject: %q", view.RejectReason)
	}
	// 已拒绝不能再批准或结算。
	if _, err := w.Approve("u1", "r1", payerApproval()); !errors.Is(err, ErrRequestNotPending) {
		t.Fatalf("approve rejected err = %v", err)
	}
	if _, err := w.Settle("u1", "r1", 0); !errors.Is(err, ErrRequestNotReserved) {
		t.Fatalf("settle rejected err = %v", err)
	}
	if got := len(w.Ledger()); got != ledgerBefore+1 {
		t.Fatalf("unexpected ledger growth: %d vs %d", got, ledgerBefore+1)
	}
}

func TestApprovalExpiry(t *testing.T) {
	t.Run("wait duration deadline", func(t *testing.T) {
		w, c := setupApproval(t, 10, 10*time.Minute)
		view := applyPending(t, w, "r1", 25)
		want := c.t.Add(10 * time.Minute)
		if !view.WaitUntil.Equal(want) {
			t.Fatalf("wait until = %s, want %s", view.WaitUntil, want)
		}

		c.t = want.Add(-time.Nanosecond)
		if v, _ := w.Request("u1", "r1"); v.State != RequestPendingApproval {
			t.Fatalf("just before deadline should stay pending: %v", v.State)
		}
		if _, err := w.Approve("u1", "r1", payerApproval()); err != nil {
			t.Fatalf("approve just before deadline: %v", err)
		}
	})

	t.Run("expire exactly at deadline", func(t *testing.T) {
		w, c := setupApproval(t, 10, 10*time.Minute)
		applyPending(t, w, "r2", 25)
		c.t = c.t.Add(10 * time.Minute) // 恰为期限：过期

		view, err := w.Request("u1", "r2")
		if err != nil {
			t.Fatal(err)
		}
		if view.State != RequestExpired || view.DecidedAt.IsZero() {
			t.Fatalf("view = %+v, want expired with decision time", view)
		}
		if _, err := w.Approve("u1", "r2", payerApproval()); !errors.Is(err, ErrApprovalDeadline) {
			t.Fatalf("approve at deadline err = %v", err)
		}
		in := payerApproval()
		in.Reason = "too late"
		if _, err := w.Reject("u1", "r2", in); !errors.Is(err, ErrApprovalDeadline) {
			t.Fatalf("reject at deadline err = %v", err)
		}
		if _, err := w.Settle("u1", "r2", 0); !errors.Is(err, ErrRequestNotReserved) {
			t.Fatalf("settle expired err = %v", err)
		}
		if got := countEntries(w.Ledger(), LedgerExpired, "r2"); got != 1 {
			t.Fatalf("expired entries = %d, want 1", got)
		}
		if got := countEntries(w.Ledger(), LedgerExpired, "r2"); got != 1 {
			t.Fatal("expiry must be recorded exactly once")
		}
		if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
			t.Fatalf("expiry must not move money: %+v", bal)
		}
	})

	t.Run("deadline capped by policy end", func(t *testing.T) {
		w, c := newTestWallet()
		mustAccount(t, w, "payer", 100)
		mustAccount(t, w, "u1", 0)
		mustAccount(t, w, "u2", 0)
		if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := w.CreateSession("pa", "payer", "pdev", c.t.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		spec := approvalSpec(c, 10, time.Hour) // 等待 1h
		spec.EndsAt = c.t.Add(20 * time.Minute)
		if err := w.SavePolicy(spec); err != nil {
			t.Fatal(err)
		}
		view := applyPending(t, w, "r3", 25)
		if !view.WaitUntil.Equal(spec.EndsAt) {
			t.Fatalf("wait until = %s, want policy end %s", view.WaitUntil, spec.EndsAt)
		}
		c.t = spec.EndsAt
		if v, _ := w.Request("u1", "r3"); v.State != RequestExpired {
			t.Fatalf("state at policy end = %v, want expired", v.State)
		}
	})

	t.Run("deadline capped by session expiry", func(t *testing.T) {
		w, c := setupApproval(t, 10, time.Hour)
		// setupApproval 中使用会话 30 分钟到期，早于等待 1h 与策略结束 1h。
		view := applyPending(t, w, "r4", 25)
		want := c.t.Add(30 * time.Minute)
		if !view.WaitUntil.Equal(want) {
			t.Fatalf("wait until = %s, want session expiry %s", view.WaitUntil, want)
		}
		c.t = want
		if v, _ := w.Request("u1", "r4"); v.State != RequestExpired {
			t.Fatalf("state at session expiry = %v, want expired", v.State)
		}
	})
}

func TestRevokeApplicationSessionRejectsPending(t *testing.T) {
	w, _ := setupApproval(t, 10, 10*time.Minute)

	// r1 待审批；r2 已批准并预留。吊销使用会话 s1 后：
	applyPending(t, w, "r1", 25)
	applyPending(t, w, "r2", 10) // 等于门槛，直接预留
	r3 := baseApply()
	r3.RequestID, r3.EstimatedFee = "r3", 25
	if _, err := w.Apply(r3); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Approve("u1", "r3", payerApproval()); err != nil {
		t.Fatal(err)
	}

	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}

	r1, _ := w.Request("u1", "r1")
	if r1.State != RequestRejected {
		t.Fatalf("pending request state = %v, want rejected", r1.State)
	}
	if r1.ApproverAccountID != "" || !strings.Contains(r1.RejectReason, "revoked") {
		t.Fatalf("system rejection fields = %+v", r1)
	}
	if got := countEntries(w.Ledger(), LedgerApprovalRejected, "r1"); got != 1 {
		t.Fatalf("revocation rejection entries = %d", got)
	}
	// 已预留的两笔不受影响：仍可结算或取消。
	// r2 按实际 10 结算（扣 10），r3 批准预留 25 后取消（全额退回）。
	if _, err := w.Settle("u1", "r2", 10); err != nil {
		t.Fatalf("directly reserved request should still settle: %v", err)
	}
	if _, err := w.Cancel("u1", "r3"); err != nil {
		t.Fatalf("approved request should still cancel: %v", err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 90, Reserved: 0}) {
		t.Fatalf("balance after revoke + closures = %+v", bal)
	}

	// 重复吊销不再重复留痕。
	entriesBefore := len(w.Ledger())
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	if got := len(w.Ledger()); got != entriesBefore {
		t.Fatalf("repeat revoke produced entries: %d vs %d", got, entriesBefore)
	}
}

func TestCancelPendingAndReserved(t *testing.T) {
	w, _ := setupApproval(t, 10, 10*time.Minute)
	applyPending(t, w, "pending", 25)
	applyPending(t, w, "reserved", 25)
	if _, err := w.Approve("u1", "reserved", payerApproval()); err != nil {
		t.Fatal(err)
	}

	// 待审批不能直接结算。
	if _, err := w.Settle("u1", "pending", 0); !errors.Is(err, ErrRequestNotReserved) {
		t.Fatalf("settle pending err = %v", err)
	}

	// 取消待审批：取消终态、无退款、金额不变。
	cancelled, err := w.Cancel("u1", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != RequestCancelled {
		t.Fatalf("cancelled = %+v", cancelled)
	}
	if got := countEntries(w.Ledger(), LedgerCancelled, "pending"); got != 1 {
		t.Fatalf("pending cancel entries = %d", got)
	}
	if got := countEntries(w.Ledger(), LedgerRefund, "pending"); got != 0 {
		t.Fatalf("pending cancel must not refund, got %d refund entries", got)
	}
	// 重复取消幂等，不重复留痕；再批准/拒绝均被拒。
	if _, err := w.Cancel("u1", "pending"); err != nil {
		t.Fatalf("repeat cancel pending: %v", err)
	}
	if got := countEntries(w.Ledger(), LedgerCancelled, "pending"); got != 1 {
		t.Fatalf("repeat cancel left trace: %d", got)
	}
	if _, err := w.Approve("u1", "pending", payerApproval()); !errors.Is(err, ErrRequestNotPending) {
		t.Fatalf("approve cancelled err = %v", err)
	}
	in := payerApproval()
	in.Reason = "x"
	if _, err := w.Reject("u1", "pending", in); !errors.Is(err, ErrRequestNotPending) {
		t.Fatalf("reject cancelled err = %v", err)
	}

	// 取消已预留：全部退回，并同时留下取消状态记录。
	if _, err := w.Cancel("u1", "reserved"); err != nil {
		t.Fatal(err)
	}
	if got := countEntries(w.Ledger(), LedgerRefund, "reserved"); got != 1 {
		t.Fatalf("reserved cancel refund entries = %d", got)
	}
	if got := countEntries(w.Ledger(), LedgerCancelled, "reserved"); got != 1 {
		t.Fatalf("reserved cancel state entries = %d", got)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want fully restored", bal)
	}
}

func TestApprovalIdempotentReapply(t *testing.T) {
	w, c := setupApproval(t, 10, 10*time.Minute)
	in := baseApply()
	in.RequestID, in.EstimatedFee = "r1", 25

	stage := func(want RequestState) {
		t.Helper()
		before := len(w.Ledger())
		bal, _ := w.Balance("payer")
		view, err := w.Apply(in)
		if err != nil {
			t.Fatalf("re-apply: %v", err)
		}
		if view.State != want {
			t.Fatalf("re-apply state = %v, want %v", view.State, want)
		}
		if len(w.Ledger()) != before {
			t.Fatal("re-apply produced ledger entries")
		}
		if got, _ := w.Balance("payer"); got != bal {
			t.Fatalf("re-apply moved money: %+v -> %+v", bal, got)
		}
	}

	// 首次申请正常创建待审批请求，之后的重复申请只验证幂等。
	if first, err := w.Apply(in); err != nil || first.State != RequestPendingApproval {
		t.Fatalf("first apply = %+v err=%v", first, err)
	}
	stage(RequestPendingApproval)
	if _, err := w.Approve("u1", "r1", payerApproval()); err != nil {
		t.Fatal(err)
	}
	stage(RequestReserved) // 已批准预留：重复申请不得再次预留
	if _, err := w.Settle("u1", "r1", 25); err != nil {
		t.Fatal(err)
	}
	stage(RequestSettled)

	// 终态（拒绝/过期/取消）重复申请同样只返回已有结果。
	for _, tc := range []struct {
		id     string
		finish func(id string)
		want   RequestState
	}{
		{"rj", func(id string) {
			in := payerApproval()
			in.Reason = "no"
			if _, err := w.Reject("u1", id, in); err != nil {
				t.Fatal(err)
			}
		}, RequestRejected},
		{"ex", func(id string) {}, RequestExpired}, // 仅推进时钟
		{"cx", func(id string) {
			if _, err := w.Cancel("u1", id); err != nil {
				t.Fatal(err)
			}
		}, RequestCancelled},
	} {
		req := baseApply()
		req.RequestID, req.EstimatedFee = tc.id, 25
		if _, err := w.Apply(req); err != nil {
			t.Fatal(err)
		}
		if tc.want == RequestExpired {
			c.t = c.t.Add(10 * time.Minute)
			// 查询触发惰性流转，过期留痕在此时产生。
			if v, err := w.Request("u1", tc.id); err != nil || v.State != RequestExpired {
				t.Fatalf("precondition for %s: view=%+v err=%v", tc.id, v, err)
			}
		} else {
			tc.finish(tc.id)
		}
		before := len(w.Ledger())
		view, err := w.Apply(req)
		if err != nil {
			t.Fatalf("re-apply terminal: %v", err)
		}
		if view.State != tc.want {
			t.Fatalf("re-apply %s state = %v, want %v", tc.id, view.State, tc.want)
		}
		if len(w.Ledger()) != before {
			t.Fatalf("re-apply %s produced ledger entries", tc.id)
		}
	}
}

func TestRepeatedApproveAfterDecision(t *testing.T) {
	w, _ := setupApproval(t, 10, 10*time.Minute)
	applyPending(t, w, "r1", 25)
	if _, err := w.Approve("u1", "r1", payerApproval()); err != nil {
		t.Fatal(err)
	}
	// 已批准预留后重复批准：返回当前结果，不再预留。
	if v, err := w.Approve("u1", "r1", payerApproval()); err != nil || v.State != RequestReserved {
		t.Fatalf("repeat approve reserved: view=%+v err=%v", v, err)
	}
	if got := countEntries(w.Ledger(), LedgerReserve, "r1"); got != 1 {
		t.Fatalf("reserve entries = %d, want 1", got)
	}
	if _, err := w.Settle("u1", "r1", 25); err != nil {
		t.Fatal(err)
	}
	if v, err := w.Approve("u1", "r1", payerApproval()); err != nil || v.State != RequestSettled {
		t.Fatalf("repeat approve settled: view=%+v err=%v", v, err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 75, Reserved: 0}) {
		t.Fatalf("balance = %+v", bal)
	}
	// 已批准不能再拒绝。
	in := payerApproval()
	in.Reason = "late"
	if _, err := w.Reject("u1", "r1", in); !errors.Is(err, ErrRequestNotPending) {
		t.Fatalf("reject approved err = %v", err)
	}
}

func TestConcurrentApproveReject(t *testing.T) {
	w, _ := setupApproval(t, 10, 10*time.Minute)
	applyPending(t, w, "r1", 25)

	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	views := make([]RequestView, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				views[i], errs[i] = w.Approve("u1", "r1", payerApproval())
			} else {
				in := payerApproval()
				in.Reason = "concurrent no"
				views[i], errs[i] = w.Reject("u1", "r1", in)
			}
		}()
	}
	wg.Wait()

	// 先成功的决定唯一：其他批准或拒绝都会收到“不再处于待审批”；
	// 批准在批准胜出后重复调用则幂等返回当前结果（nil）。
	for i, err := range errs {
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrRequestNotPending) {
			t.Fatalf("unexpected race err at %d: %v", i, err)
		}
	}
	if a, b := countEntries(w.Ledger(), LedgerApproval, "r1"), countEntries(w.Ledger(), LedgerApprovalRejected, "r1"); a+b != 1 {
		t.Fatalf("expected exactly one decision trace, got approval=%d rejection=%d", a, b)
	}
	if countEntries(w.Ledger(), LedgerReserve, "r1") > 1 {
		t.Fatal("reservation happened more than once")
	}
	view, _ := w.Request("u1", "r1")
	switch view.State {
	case RequestReserved:
		if bal, _ := w.Balance("payer"); bal != (Balances{Available: 75, Reserved: 25}) {
			t.Fatalf("approved race balance = %+v", bal)
		}
	case RequestRejected:
		if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
			t.Fatalf("rejected race balance = %+v", bal)
		}
	default:
		t.Fatalf("unexpected final state: %v", view.State)
	}
}

func TestConcurrentApproveCancel(t *testing.T) {
	const rounds = 20
	for round := 0; round < rounds; round++ {
		w, _ := setupApproval(t, 10, 10*time.Minute)
		applyPending(t, w, "r1", 25)

		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Add(2)
		go func() { defer wg.Done(); _, errs[0] = w.Approve("u1", "r1", payerApproval()) }()
		go func() { defer wg.Done(); _, errs[1] = w.Cancel("u1", "r1") }()
		wg.Wait()

		// 取消先完成：批准返回“不再待审批”；批准先完成：取消仍可按
		// 已预留规则成功退回。因此最终状态恒为已取消，余额恒为全额。
		if errs[1] != nil {
			t.Fatalf("round %d cancel err: %v", round, errs[1])
		}
		if errs[0] != nil && !errors.Is(errs[0], ErrRequestNotPending) {
			t.Fatalf("round %d approve err: %v", round, errs[0])
		}
		view, _ := w.Request("u1", "r1")
		if view.State != RequestCancelled {
			t.Fatalf("round %d final state = %v, want cancelled", round, view.State)
		}
		bal, _ := w.Balance("payer")
		pv, _ := w.Policy("p1")
		if bal != (Balances{Available: 100, Reserved: 0}) || pv.ReservedTotal != 0 {
			t.Fatalf("round %d balance=%+v policy=%+v", round, bal, pv)
		}
		// 批准先完成时必然产生过一次预留且随后被一次退回归还；
		// 取消先完成时没有任何资金记录。
		reserves := countEntries(w.Ledger(), LedgerReserve, "r1")
		refunds := countEntries(w.Ledger(), LedgerRefund, "r1")
		if reserves != refunds || reserves > 1 {
			t.Fatalf("round %d reserve/refund mismatch: %d vs %d", round, reserves, refunds)
		}
		if errs[0] == nil && reserves != 1 {
			t.Fatalf("round %d approve succeeded but no reserve recorded", round)
		}
	}
}

func TestApprovalLedgerDistinguishesStates(t *testing.T) {
	w, _ := setupApproval(t, 10, 10*time.Minute)
	applyPending(t, w, "approved", 25)
	applyPending(t, w, "rejected", 25)
	applyPending(t, w, "cancelled", 25)

	if _, err := w.Approve("u1", "approved", payerApproval()); err != nil {
		t.Fatal(err)
	}
	in := payerApproval()
	in.Reason = "deny"
	if _, err := w.Reject("u1", "rejected", in); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Cancel("u1", "cancelled"); err != nil {
		t.Fatal(err)
	}

	wantKinds := map[string]LedgerKind{
		"approved":  LedgerApproval,
		"rejected":  LedgerApprovalRejected,
		"cancelled": LedgerCancelled,
	}
	for rid, kind := range wantKinds {
		if got := countEntries(w.Ledger(), kind, rid); got != 1 {
			t.Fatalf("request %s kind %v entries = %d", rid, kind, got)
		}
	}
	// 所有请求关联记录都带使用账户；资金记录额外带出资账户。
	for _, e := range w.Ledger() {
		if e.UsageAccountID != "u1" {
			t.Fatalf("entry missing usage account link: %+v", e)
		}
		if (e.Kind == LedgerReserve || e.Kind == LedgerSettle || e.Kind == LedgerRefund) && e.AccountID != "payer" {
			t.Fatalf("money entry missing payer account: %+v", e)
		}
		if e.Kind != LedgerReserve && e.Kind != LedgerSettle && e.Kind != LedgerRefund {
			if e.Amount != 0 {
				t.Fatalf("state/rejection entry must carry zero amount: %+v", e)
			}
		}
	}

	// AccountLedger 仍只包含出资账户的资金变动（1 条批准预留）。
	acct := w.AccountLedger("payer")
	if len(acct) != 1 || acct[0].Kind != LedgerReserve || acct[0].Amount != 25 {
		t.Fatalf("payer account ledger = %+v", acct)
	}
}
