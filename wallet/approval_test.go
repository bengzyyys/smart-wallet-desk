package wallet

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// approvalPolicy 构造一条开启大额审批的策略：门槛 10，等待 1 分钟，
// 其余与 validPolicy 一致（单次 30、累计 50、窗口 1 小时）。
func approvalPolicy(c *clock) PolicySpec {
	p := validPolicy(c)
	p.ID = "p-approval"
	p.ApprovalThreshold = 10
	p.ApprovalWait = time.Minute
	return p
}

// setupApproval 构造带审批策略的钱包：出资账户 payer（100），使用账户
// u1/u2，会话 s1/s2，以及出资账户绑定审批设备的会话 sa。
func setupApproval(t *testing.T) (*Wallet, *clock) {
	t.Helper()
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s2", "u2", "dev2", c.t.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "dev-approve", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(approvalPolicy(c)); err != nil {
		t.Fatal(err)
	}
	return w, c
}

func approvalApply() RequestInput {
	return RequestInput{
		PolicyID:     "p-approval",
		RequestID:    "r1",
		AccountID:    "u1",
		SessionID:    "s1",
		DeviceID:     "dev1",
		Operation:    "charge",
		Payee:        "shop",
		EstimatedFee: 20,
	}
}

func TestSaveApprovalPolicyValidation(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)

	good := approvalPolicy(c)
	if err := w.SavePolicy(good); err != nil {
		t.Fatal(err)
	}
	// 视图能读回审批配置。
	pv, err := w.Policy("p-approval")
	if err != nil {
		t.Fatal(err)
	}
	if pv.ApprovalThreshold != 10 || pv.ApprovalWait != time.Minute {
		t.Fatalf("approval config = threshold %d wait %v", pv.ApprovalThreshold, pv.ApprovalWait)
	}

	cases := []struct {
		name   string
		mutate func(*PolicySpec)
	}{
		{"negative threshold", func(p *PolicySpec) { p.ApprovalThreshold = -1 }},
		{"threshold exceeds per-request", func(p *PolicySpec) { p.ApprovalThreshold = 31 }},
		{"zero wait when enabled", func(p *PolicySpec) { p.ApprovalWait = 0 }},
		{"negative wait when enabled", func(p *PolicySpec) { p.ApprovalWait = -time.Second }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := good
			bad.ID = "p-bad-" + tc.name
			tc.mutate(&bad)
			if err := w.SavePolicy(bad); !errors.Is(err, ErrPolicyInvalid) {
				t.Fatalf("err = %v, want ErrPolicyInvalid", err)
			}
		})
	}

	// 门槛为零（关闭审批）时等待时长不做要求。
	off := good
	off.ID = "p-off"
	off.ApprovalThreshold = 0
	off.ApprovalWait = 0
	if err := w.SavePolicy(off); err != nil {
		t.Fatalf("disabled approval with zero wait should be accepted: %v", err)
	}
	// 门槛恰好等于单次上限允许保存（费用不可能严格超过它）。
	eq := good
	eq.ID = "p-eq"
	eq.ApprovalThreshold = 30
	if err := w.SavePolicy(eq); err != nil {
		t.Fatalf("threshold equal to per-request cap should be accepted: %v", err)
	}
}

func TestApplyGoesPendingWithoutFreezing(t *testing.T) {
	w, c := setupApproval(t)

	in := approvalApply()
	req, err := w.Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	if req.State != RequestPendingApproval {
		t.Fatalf("state = %v, want pending approval", req.State)
	}
	// 等待期限 = min(提交+1分钟, 策略结束, 会话到期) = 提交+1分钟。
	wantDeadline := c.t.Add(time.Minute)
	if !req.WaitDeadline.Equal(wantDeadline) {
		t.Fatalf("wait deadline = %v, want %v", req.WaitDeadline, wantDeadline)
	}
	if req.DecidedAt != (time.Time{}) || req.ApproverAccountID != "" || req.RejectReason != "" {
		t.Fatalf("pending request should have no decision yet: %+v", req)
	}

	// 待审批不冻结余额、不占用共享累计额度。
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want untouched {100 0}", bal)
	}
	pv, _ := w.Policy("p-approval")
	if pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}
	if len(w.AccountLedger("payer")) != 0 {
		t.Fatal("pending approval must not create money-moving ledger entries")
	}

	// 账本含待审批状态留痕，关联使用账户与请求号。
	var found bool
	for _, e := range w.Ledger() {
		if e.Kind == LedgerPendingApproval {
			found = true
			if e.AccountID != "u1" || e.RequestID != "r1" || e.Amount != 0 {
				t.Fatalf("pending ledger entry = %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("missing LedgerPendingApproval entry")
	}

	// 等于门槛的费用直接预留，不进入待审批。
	eq := approvalApply()
	eq.RequestID = "r-eq"
	eq.EstimatedFee = 10
	eqReq, err := w.Apply(eq)
	if err != nil {
		t.Fatal(err)
	}
	if eqReq.State != RequestReserved {
		t.Fatalf("fee equal to threshold state = %v, want reserved", eqReq.State)
	}
	// 低于门槛直接预留。
	low := approvalApply()
	low.RequestID = "r-low"
	low.EstimatedFee = 9
	lowReq, err := w.Apply(low)
	if err != nil {
		t.Fatal(err)
	}
	if lowReq.State != RequestReserved {
		t.Fatalf("fee below threshold state = %v, want reserved", lowReq.State)
	}
}

func TestApproveHappyPath(t *testing.T) {
	w, _ := setupApproval(t)

	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}
	req, err := w.Approve("u1", "r1", "sa", "dev-approve")
	if err != nil {
		t.Fatal(err)
	}
	if req.State != RequestReserved {
		t.Fatalf("state = %v, want reserved", req.State)
	}
	if req.ApproverAccountID != "payer" || req.DecidedAt.IsZero() {
		t.Fatalf("approval decision fields missing: %+v", req)
	}

	// 批准时一次性预留全部预估费用。
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance = %+v, want {80 20}", bal)
	}
	pv, _ := w.Policy("p-approval")
	if pv.ReservedTotal != 20 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = %+v", pv)
	}

	// 账本：一条预留（资金）+ 一条批准（状态）。
	var reserve, approval bool
	for _, e := range w.Ledger() {
		switch e.Kind {
		case LedgerReserve:
			reserve = true
			if e.AccountID != "payer" || e.Amount != 20 || e.RequestID != "r1" {
				t.Fatalf("reserve entry = %+v", e)
			}
		case LedgerApproval:
			approval = true
			if e.AccountID != "u1" || e.Amount != 0 || e.RequestID != "r1" {
				t.Fatalf("approval entry = %+v", e)
			}
		}
	}
	if !reserve || !approval {
		t.Fatalf("ledger missing reserve=%v approval=%v", reserve, approval)
	}

	// 批准后可正常结算。
	if _, err := w.Settle("u1", "r1", 8); err != nil {
		t.Fatal(err)
	}
	bal, _ = w.Balance("payer")
	if bal != (Balances{Available: 92, Reserved: 0}) {
		t.Fatalf("balance after settle = %+v, want {92 0}", bal)
	}
}

func TestApproveAuthorization(t *testing.T) {
	w, _ := setupApproval(t)
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		session string
		device  string
	}{
		{"user account session", "s1", "dev1"},
		{"wrong device", "sa", "wrong-device"},
		{"unknown session", "nope", "dev-approve"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := w.Approve("u1", "r1", tc.session, tc.device); !errors.Is(err, ErrNotApprover) {
				t.Fatalf("err = %v, want ErrNotApprover", err)
			}
		})
	}

	// 请求保持待审批、余额未变。
	req, _ := w.Request("u1", "r1")
	if req.State != RequestPendingApproval {
		t.Fatalf("state = %v, want still pending", req.State)
	}
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance changed after failed approvals: %+v", bal)
	}

	// 出资会话过期后无权审批。
	// （将在 TestApproveExpiry 中覆盖到期场景，这里只吊销。）
	if err := w.RevokeSession("sa"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("revoked payer session err = %v, want ErrNotApprover", err)
	}
}

func TestApproveInsufficientBalanceKeepsPending(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "dev-approve", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	p := approvalPolicy(c)
	p.MaxTotal = 200 // 允许连续预留以降低余额
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}

	in := approvalApply()
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	// 连续预留 9 笔各 10，把出资余额降到 10（< 待审批费用 20）。
	for i := 0; i < 9; i++ {
		reserved := approvalApply()
		reserved.RequestID = fmt.Sprintf("drop%d", i)
		reserved.EstimatedFee = 10
		if _, err := w.Apply(reserved); err != nil {
			t.Fatal(err)
		}
	}
	bal, _ := w.Balance("payer")
	if bal.Available != 10 {
		t.Fatalf("available = %d, want 10", bal.Available)
	}
	// 批准时余额不足：返回具体原因，保留待审批状态。
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	req, _ := w.Request("u1", "r1")
	if req.State != RequestPendingApproval {
		t.Fatalf("state = %v, want still pending", req.State)
	}
	// 期限内可再次批准（余额仍不足则仍失败）。
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("second approve err = %v, want ErrInsufficientBalance", err)
	}
}

func TestApproveQuotaRecheck(t *testing.T) {
	w, c := setupApproval(t)
	// 门槛 10、累计上限 50：直接预留费用 ≤ 10，待审批费用 > 10。
	p := approvalPolicy(c)
	p.ID = "p-quota"
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	apply := func(rid string, fee int64) RequestView {
		in := approvalApply()
		in.PolicyID = "p-quota"
		in.RequestID = rid
		in.EstimatedFee = fee
		req, err := w.Apply(in)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	// 第一笔直接预留 10，占用累计额度 10。
	if r := apply("r-a", 10); r.State != RequestReserved {
		t.Fatalf("fee 10 state = %v, want reserved", r.State)
	}
	// 第二笔 20 待审批（申请时 used=10，10+20=30 ≤ 50）。
	if r := apply("r1", 20); r.State != RequestPendingApproval {
		t.Fatalf("fee 20 state = %v, want pending", r.State)
	}
	// 第三笔 20 待审批（申请时 used 仍为 10，10+20=30 ≤ 50）。
	if r := apply("r3", 20); r.State != RequestPendingApproval {
		t.Fatalf("fee 20 state = %v, want pending", r.State)
	}
	// 批准第二笔：used=10+20=30 ≤ 50，成功。
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); err != nil {
		t.Fatalf("approve r1: %v", err)
	}
	pv, _ := w.Policy("p-quota")
	if pv.ReservedTotal != 30 {
		t.Fatalf("reserved total = %d, want 30", pv.ReservedTotal)
	}
	// 再预留一笔 10，used 升到 40。
	if r := apply("r-b", 10); r.State != RequestReserved {
		t.Fatalf("fee 10 state = %v, want reserved", r.State)
	}
	// 批准第三笔：40+20=60 > 50，拒绝批准并保留待审批。
	if _, err := w.Approve("u1", "r3", "sa", "dev-approve"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("err = %v, want ErrQuotaExceeded", err)
	}
	req, _ := w.Request("u1", "r3")
	if req.State != RequestPendingApproval {
		t.Fatalf("state = %v, want still pending", req.State)
	}
	// 取消第一笔释放额度（used 降为 30）后，第三笔可批准。
	if _, err := w.Cancel("u1", "r-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Approve("u1", "r3", "sa", "dev-approve"); err != nil {
		t.Fatalf("approve after quota freed: %v", err)
	}
}

func TestRejectFlow(t *testing.T) {
	w, _ := setupApproval(t)
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}

	// 空理由、全空白理由：参数错误，请求保持不变。
	for _, reason := range []string{"", "   ", "\t\n"} {
		if _, err := w.Reject("u1", "r1", "sa", "dev-approve", reason); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("reason %q err = %v, want ErrInvalidArgument", reason, err)
		}
	}
	req, _ := w.Request("u1", "r1")
	if req.State != RequestPendingApproval {
		t.Fatalf("state = %v, want still pending", req.State)
	}

	// 无权会话不能拒绝。
	if _, err := w.Reject("u1", "r1", "s1", "dev1", "no"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("err = %v, want ErrNotApprover", err)
	}

	// 正常拒绝：拒绝终态，不产生退款（本就没冻结）。
	req, err := w.Reject("u1", "r1", "sa", "dev-approve", "duplicate order")
	if err != nil {
		t.Fatal(err)
	}
	if req.State != RequestRejected || req.RejectReason != "duplicate order" {
		t.Fatalf("rejected request = %+v", req)
	}
	if req.DecidedAt.IsZero() || req.ApproverAccountID != "payer" {
		t.Fatalf("decision fields missing: %+v", req)
	}
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want untouched", bal)
	}

	// 账本含拒绝留痕。
	var found bool
	for _, e := range w.Ledger() {
		if e.Kind == LedgerRejection && e.RequestID == "r1" {
			found = true
			if e.AccountID != "u1" || e.Amount != 0 || e.Reason != "duplicate order" {
				t.Fatalf("rejection entry = %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("missing LedgerRejection entry")
	}

	// 重复拒绝且理由一致：幂等，不重复留痕。
	before := len(w.Ledger())
	if _, err := w.Reject("u1", "r1", "sa", "dev-approve", "duplicate order"); err != nil {
		t.Fatalf("idempotent reject: %v", err)
	}
	if len(w.Ledger()) != before {
		t.Fatal("ledger grew on idempotent reject")
	}
	// 理由不同：冲突。
	if _, err := w.Reject("u1", "r1", "sa", "dev-approve", "different reason"); !errors.Is(err, ErrConflict) {
		t.Fatalf("different reason err = %v, want ErrConflict", err)
	}
}

func TestApprovedCannotBeRejectedAndViceVersa(t *testing.T) {
	w, _ := setupApproval(t)
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); err != nil {
		t.Fatal(err)
	}
	// 已批准不能再拒绝。
	if _, err := w.Reject("u1", "r1", "sa", "dev-approve", "too late"); !errors.Is(err, ErrRequestNotPending) {
		t.Fatalf("reject approved err = %v, want ErrRequestNotPending", err)
	}
	// 重复批准幂等，不重复预留。
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); err != nil {
		t.Fatalf("repeat approve: %v", err)
	}
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance = %+v, want {80 20} (no double reserve)", bal)
	}
}

func TestCancelPendingAndTerminal(t *testing.T) {
	w, _ := setupApproval(t)
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}

	// 取消待审批：取消终态，不产生退款。
	req, err := w.Cancel("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if req.State != RequestCancelled {
		t.Fatalf("state = %v, want cancelled", req.State)
	}
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want untouched", bal)
	}
	var found bool
	for _, e := range w.Ledger() {
		if e.Kind == LedgerCancellation && e.RequestID == "r1" {
			found = true
			if e.AccountID != "u1" || e.Amount != 0 {
				t.Fatalf("cancellation entry = %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("missing LedgerCancellation entry")
	}
	// 重复取消幂等。
	before := len(w.Ledger())
	if _, err := w.Cancel("u1", "r1"); err != nil {
		t.Fatalf("repeat cancel: %v", err)
	}
	if len(w.Ledger()) != before {
		t.Fatal("ledger grew on repeat cancel")
	}
	// 已取消不能批准、不能结算。
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrRequestNotPending) {
		t.Fatalf("approve cancelled err = %v, want ErrRequestNotPending", err)
	}
	if _, err := w.Settle("u1", "r1", 0); !errors.Is(err, ErrAlreadyCancelled) {
		t.Fatalf("settle cancelled err = %v", err)
	}

	// 已预留请求的取消仍退回全部预留。
	reserved := approvalApply()
	reserved.RequestID = "r2"
	reserved.EstimatedFee = 8
	if _, err := w.Apply(reserved); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Cancel("u1", "r2"); err != nil {
		t.Fatal(err)
	}
	bal, _ = w.Balance("payer")
	if bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance after reserved cancel = %+v, want {100 0}", bal)
	}
}

func TestExpiry(t *testing.T) {
	w, c := setupApproval(t)
	// 用一条 1 小时后才到期的会话，避免重申请时先被会话过期拦截。
	if _, err := w.CreateSession("s-long", "u1", "dev1", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	in := approvalApply()
	in.SessionID = "s-long"
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}

	// 到期前查询仍为待审批。
	if req, _ := w.Request("u1", "r1"); req.State != RequestPendingApproval {
		t.Fatalf("state before expiry = %v", req.State)
	}

	// 推进到等待期限（1 分钟）：到期时刻即过期。
	c.t = c.t.Add(time.Minute)
	req, err := w.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if req.State != RequestExpired {
		t.Fatalf("state = %v, want expired", req.State)
	}
	if req.DecidedAt.IsZero() {
		t.Fatal("expired request should have decision time")
	}

	// 账本含过期留痕。
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

	// 截止时刻及之后不能批准。
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("approve after deadline err = %v, want ErrApprovalExpired", err)
	}
	// 过期不能拒绝、不能取消、不能结算。
	if _, err := w.Reject("u1", "r1", "sa", "dev-approve", "late"); !errors.Is(err, ErrRequestNotPending) {
		t.Fatalf("reject expired err = %v, want ErrRequestNotPending", err)
	}
	if _, err := w.Cancel("u1", "r1"); !errors.Is(err, ErrRequestNotReserved) {
		t.Fatalf("cancel expired err = %v, want ErrRequestNotReserved", err)
	}
	if _, err := w.Settle("u1", "r1", 0); !errors.Is(err, ErrRequestNotReserved) {
		t.Fatalf("settle expired err = %v, want ErrRequestNotReserved", err)
	}

	// 过期请求不能通过重复申请重新创建（会话仍有效，应返回过期视图）。
	if req, err := w.Apply(in); err != nil || req.State != RequestExpired {
		t.Fatalf("re-apply expired: view=%+v err=%v", req, err)
	}
}

func TestDeadlineIsMinOfThree(t *testing.T) {
	// 场景 1：策略结束时间早于等待时长 → 期限取策略结束。
	w, c := setupApproval(t)
	p := approvalPolicy(c)
	p.ID = "p-short-window"
	p.EndsAt = c.t.Add(30 * time.Second)
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	in := approvalApply()
	in.PolicyID = "p-short-window"
	in.RequestID = "rw"
	req, err := w.Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	if !req.WaitDeadline.Equal(c.t.Add(30 * time.Second)) {
		t.Fatalf("deadline = %v, want policy end %v", req.WaitDeadline, c.t.Add(30*time.Second))
	}

	// 场景 2：会话到期早于等待时长 → 期限取会话到期。
	w2, c2 := setupApproval(t)
	sessExp := c2.t.Add(20 * time.Second)
	if _, err := w2.CreateSession("s-short", "u1", "dev1", sessExp); err != nil {
		t.Fatal(err)
	}
	in2 := approvalApply()
	in2.SessionID = "s-short"
	in2.RequestID = "rs"
	req2, err := w2.Apply(in2)
	if err != nil {
		t.Fatal(err)
	}
	if !req2.WaitDeadline.Equal(sessExp) {
		t.Fatalf("deadline = %v, want session expiry %v", req2.WaitDeadline, sessExp)
	}
}

func TestSessionRevocationRejectsPendingOnly(t *testing.T) {
	w, _ := setupApproval(t)

	// 两笔待审批（u1 的 s1，费用均超过门槛 10）。
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}
	second := approvalApply()
	second.RequestID = "r2"
	second.EstimatedFee = 11
	if _, err := w.Apply(second); err != nil {
		t.Fatal(err)
	}
	// 两笔已批准并预留（费用 ≤ 门槛，直接预留），在吊销前完成。
	reserved := approvalApply()
	reserved.RequestID = "r3"
	reserved.EstimatedFee = 8
	if _, err := w.Apply(reserved); err != nil {
		t.Fatal(err)
	}
	reserved2 := approvalApply()
	reserved2.RequestID = "r4"
	reserved2.EstimatedFee = 7
	if _, err := w.Apply(reserved2); err != nil {
		t.Fatal(err)
	}
	// 吊销 s1：两笔待审批进入拒绝终态，已预留的两笔不受影响。
	if err := w.RevokeSession("s1"); err != nil {
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
		if req.RejectReason == "" {
			t.Fatalf("%s rejection reason missing", rid)
		}
		if req.DecidedAt.IsZero() {
			t.Fatalf("%s decision time missing", rid)
		}
	}
	// 余额：待审批未冻结，两笔预留共 15。
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 85, Reserved: 15}) {
		t.Fatalf("balance = %+v, want {85 15}", bal)
	}
	// 账本含两笔拒绝留痕。
	rejections := 0
	for _, e := range w.Ledger() {
		if e.Kind == LedgerRejection && e.RequestID != "" {
			rejections++
		}
	}
	if rejections != 2 {
		t.Fatalf("rejection entries = %d, want 2", rejections)
	}

	// 已预留请求：会话吊销后仍可结算。
	if _, err := w.Settle("u1", "r3", 8); err != nil {
		t.Fatalf("settle reserved after session revoke: %v", err)
	}
	// 已预留请求吊销后仍可取消。
	if _, err := w.Cancel("u1", "r4"); err != nil {
		t.Fatalf("cancel reserved after session revoke: %v", err)
	}
	// r3 结算扣除 8（不退回），r4 取消退回 7：余额 = 100 - 8 = 92。
	bal, _ = w.Balance("payer")
	if bal != (Balances{Available: 92, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {92 0}", bal)
	}
}

func TestApplyIdempotencyForPendingAndTerminal(t *testing.T) {
	w, _ := setupApproval(t)
	in := approvalApply()
	first, err := w.Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	// 待审批期间重复申请：返回已有待审批结果，不重复留痕。
	before := len(w.Ledger())
	second, err := w.Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	if second.State != RequestPendingApproval || second.RequestID != first.RequestID {
		t.Fatalf("idempotent pending apply = %+v", second)
	}
	if len(w.Ledger()) != before {
		t.Fatal("ledger grew on idempotent pending apply")
	}
	// 内容不同：冲突。
	conflict := in
	conflict.EstimatedFee = 21
	if _, err := w.Apply(conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	// 批准后重复申请：返回已预留结果，不重复预留。
	if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); err != nil {
		t.Fatal(err)
	}
	if req, err := w.Apply(in); err != nil || req.State != RequestReserved {
		t.Fatalf("re-apply after approve: view=%+v err=%v", req, err)
	}
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance = %+v, want {80 20} (no double reserve)", bal)
	}
}

func TestAccountLedgerLinksUsageAccount(t *testing.T) {
	w, _ := setupApproval(t)
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Reject("u1", "r1", "sa", "dev-approve", "no"); err != nil {
		t.Fatal(err)
	}
	// 使用账户的账本应包含状态留痕。
	led := w.AccountLedger("u1")
	if len(led) != 2 {
		t.Fatalf("usage account ledger = %+v, want 2 status entries", led)
	}
	if led[0].Kind != LedgerPendingApproval || led[1].Kind != LedgerRejection {
		t.Fatalf("usage ledger kinds = %v, %v", led[0].Kind, led[1].Kind)
	}
	// 出资账户账本不含状态留痕。
	if len(w.AccountLedger("payer")) != 0 {
		t.Fatalf("payer ledger = %+v, want empty", w.AccountLedger("payer"))
	}
}

func TestConcurrentApproveReject(t *testing.T) {
	w, _ := setupApproval(t)
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				_, _ = w.Approve("u1", "r1", "sa", "dev-approve")
			} else {
				_, _ = w.Reject("u1", "r1", "sa", "dev-approve", "concurrent reject")
			}
		}()
	}
	wg.Wait()

	// 以账本为准：有且仅有一个决定（批准留痕或拒绝留痕），且只出现一次。
	approvals, rejections := 0, 0
	for _, e := range w.Ledger() {
		switch e.Kind {
		case LedgerApproval:
			approvals++
		case LedgerRejection:
			rejections++
		}
	}
	if approvals+rejections != 1 {
		t.Fatalf("decisions = %d (approve %d reject %d), want exactly 1", approvals+rejections, approvals, rejections)
	}
	req, _ := w.Request("u1", "r1")
	if approvals == 1 && req.State != RequestReserved {
		t.Fatalf("approved but final state = %v", req.State)
	}
	if rejections == 1 && req.State != RequestRejected {
		t.Fatalf("rejected but final state = %v", req.State)
	}
	// 余额与状态一致：批准则预留 20，拒绝则不动。
	bal, _ := w.Balance("payer")
	if req.State == RequestReserved {
		if bal != (Balances{Available: 80, Reserved: 20}) {
			t.Fatalf("balance = %+v, want {80 20}", bal)
		}
	} else {
		if bal != (Balances{Available: 100, Reserved: 0}) {
			t.Fatalf("balance = %+v, want untouched", bal)
		}
	}
}

func TestConcurrentCancelApprove(t *testing.T) {
	w, _ := setupApproval(t)
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	var cancelErr, approveErr error
	go func() { defer wg.Done(); _, cancelErr = w.Cancel("u1", "r1") }()
	go func() { defer wg.Done(); _, approveErr = w.Approve("u1", "r1", "sa", "dev-approve") }()
	wg.Wait()

	if cancelErr != nil {
		t.Fatalf("cancel err = %v", cancelErr)
	}
	// 两种合法结局：
	//  - 取消先完成：批准失败 ErrRequestNotPending，余额不动；
	//  - 批准先完成：取消按已预留规则退回，余额全额退回。
	bal, _ := w.Balance("payer")
	if approveErr == nil {
		// 批准先完成，随后取消退回全部预留。
		if bal != (Balances{Available: 100, Reserved: 0}) {
			t.Fatalf("balance = %+v, want full refund {100 0}", bal)
		}
		req, _ := w.Request("u1", "r1")
		if req.State != RequestCancelled {
			t.Fatalf("state = %v, want cancelled after approve-then-cancel", req.State)
		}
	} else if errors.Is(approveErr, ErrRequestNotPending) {
		// 取消先完成，批准失败。
		if bal != (Balances{Available: 100, Reserved: 0}) {
			t.Fatalf("balance = %+v, want untouched {100 0}", bal)
		}
		req, _ := w.Request("u1", "r1")
		if req.State != RequestCancelled {
			t.Fatalf("state = %v, want cancelled", req.State)
		}
	} else {
		t.Fatalf("unexpected approve err = %v", approveErr)
	}
}

// TestApproveDirectlyReservedRequestIsNotRepeatApproval 验证批准入口返回成功
// 必须对应一次真实批准（或对该批准结果的重复确认）：审批关闭，或审批开启但
// 预估费用低于、恰好等于门槛时，申请直接预留，从未等待审批；这类请求无论处于
// 已预留还是已结算，用合法出资会话调用 Approve 都必须返回 ErrRequestNotPending，
// 错误说明明确其未经审批直接受理，且不改变任何状态、时间、金额与账本，也不再次
// 冻结费用。真正超门槛经批准的请求重复批准（预留与结算状态）仍返回已有结果。
func TestApproveDirectlyReservedRequestIsNotRepeatApproval(t *testing.T) {
	// 场景 1：审批关闭，大额费用直接预留。
	w, c := setupApproval(t)
	off := approvalPolicy(c)
	off.ID = "p-off"
	off.ApprovalThreshold = 0
	off.ApprovalWait = 0
	if err := w.SavePolicy(off); err != nil {
		t.Fatal(err)
	}
	in := approvalApply()
	in.PolicyID = "p-off"
	req, err := w.Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	if req.State != RequestReserved || req.ApproverAccountID != "" || !req.WaitDeadline.IsZero() {
		t.Fatalf("approval-off request should be directly reserved without decision: %+v", req)
	}

	assertDirectApproveRejected := func(label string) {
		t.Helper()
		_, err := w.Approve("u1", "r1", "sa", "dev-approve")
		if !errors.Is(err, ErrRequestNotPending) {
			t.Fatalf("%s: err = %v, want ErrRequestNotPending", label, err)
		}
		if !strings.Contains(err.Error(), "without approval") {
			t.Fatalf("%s: error should state the request was accepted without approval: %v", label, err)
		}
	}
	assertDirectApproveRejected("reserved while approval off")

	// 目标请求状态、提交/预留时间、决定信息保持不变。
	got, _ := w.Request("u1", "r1")
	if got.State != RequestReserved || got.ApproverAccountID != "" || !got.DecidedAt.IsZero() {
		t.Fatalf("request changed after rejected approve: %+v", got)
	}
	if !got.ReservedAt.Equal(req.CreatedAt) || !got.ReserveDeadline.Equal(req.ReserveDeadline) {
		t.Fatalf("reservation timing changed: got %+v", got)
	}
	// 余额、策略累计不重复冻结，账本不补写批准记录。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance changed: %+v", bal)
	}
	pv, _ := w.Policy("p-off")
	if pv.ReservedTotal != 20 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals changed: %+v", pv)
	}
	for _, e := range w.Ledger() {
		if e.Kind == LedgerApproval {
			t.Fatalf("approval ledger entry must not be backfilled: %+v", e)
		}
	}

	// 未结算的直接预留请求仍能按原规则结算；结算后重复批准同样被拒绝，
	// 实际费用保持不变。
	if _, err := w.Settle("u1", "r1", 8); err != nil {
		t.Fatalf("settle directly reserved request: %v", err)
	}
	assertDirectApproveRejected("settled while approval off")
	got, _ = w.Request("u1", "r1")
	if got.State != RequestSettled || got.ActualFee != 8 || got.ApproverAccountID != "" {
		t.Fatalf("settled direct request changed after rejected approve: %+v", got)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 92, Reserved: 0}) {
		t.Fatalf("balance after settle = %+v, want {92 0}", bal)
	}

	// 场景 2：审批开启（门槛 10），费用恰好等于门槛与低于门槛均直接预留。
	w2, _ := setupApproval(t)
	for _, fee := range []int64{10, 9} {
		in := approvalApply()
		in.RequestID = fmt.Sprintf("r-fee-%d", fee)
		in.EstimatedFee = fee
		if r, err := w2.Apply(in); err != nil || r.State != RequestReserved {
			t.Fatalf("fee %d apply: view=%+v err=%v", fee, r, err)
		}
		if _, err := w2.Approve("u1", fmt.Sprintf("r-fee-%d", fee), "sa", "dev-approve"); !errors.Is(err, ErrRequestNotPending) {
			t.Fatalf("fee %d approve err = %v, want ErrRequestNotPending", fee, err)
		}
		if r, _ := w2.Request("u1", fmt.Sprintf("r-fee-%d", fee)); r.ApproverAccountID != "" {
			t.Fatalf("fee %d got backfilled approver: %+v", fee, r)
		}
	}

	// 场景 3：费用 11 确实超过门槛，经出资账户批准后预留：重复批准在预留
	// 与结算状态下都返回已有结果，首次批准账户、决定时间与预留截止不变，
	// 余额、策略累计与账本不重复变化。
	big := approvalApply()
	big.RequestID = "r-big"
	big.EstimatedFee = 11
	if r, err := w2.Apply(big); err != nil || r.State != RequestPendingApproval {
		t.Fatalf("fee 11 apply: view=%+v err=%v", r, err)
	}
	first, err := w2.Approve("u1", "r-big", "sa", "dev-approve")
	if err != nil {
		t.Fatalf("first approve: %v", err)
	}
	if first.ApproverAccountID != "payer" || first.DecidedAt.IsZero() || !first.ReservedAt.Equal(first.DecidedAt) {
		t.Fatalf("first approval decision fields wrong: %+v", first)
	}
	ledgerBefore := len(w2.Ledger())
	repeat, err := w2.Approve("u1", "r-big", "sa", "dev-approve")
	if err != nil {
		t.Fatalf("repeat approved-while-reserved should confirm: %v", err)
	}
	if repeat.ApproverAccountID != "payer" || !repeat.DecidedAt.Equal(first.DecidedAt) ||
		!repeat.ReservedAt.Equal(first.ReservedAt) || !repeat.ReserveDeadline.Equal(first.ReserveDeadline) {
		t.Fatalf("repeat approve rewrote first decision: first=%+v repeat=%+v", first, repeat)
	}
	if len(w2.Ledger()) != ledgerBefore {
		t.Fatal("ledger grew on repeat approve")
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100 - 10 - 9 - 11, Reserved: 10 + 9 + 11}) {
		t.Fatalf("balance changed on repeat approve: %+v", bal)
	}
	if _, err := w2.Settle("u1", "r-big", 11); err != nil {
		t.Fatalf("settle approved request: %v", err)
	}
	if _, err := w2.Approve("u1", "r-big", "sa", "dev-approve"); err != nil {
		t.Fatalf("repeat approved-while-settled should confirm: %v", err)
	}
	if r, _ := w2.Request("u1", "r-big"); r.State != RequestSettled || r.ActualFee != 11 ||
		r.ApproverAccountID != "payer" || !r.DecidedAt.Equal(first.DecidedAt) {
		t.Fatalf("approved-then-settled request changed on repeat approve: %+v", r)
	}
}

// TestApproveDirectlyReservedStillChecksApproverFirst 验证身份校验次序不变：
// 对从未等待审批、直接预留的请求，审批会话不属于出资账户、设备不符、不存在、
// 已吊销或已到期时仍先返回 ErrNotApprover，不能因为请求无需审批而先返回状态错误。
func TestApproveDirectlyReservedStillChecksApproverFirst(t *testing.T) {
	w, c := setupApproval(t)
	// 费用恰好等于门槛 10：直接预留，从未待审批。
	in := approvalApply()
	in.RequestID = "r-direct"
	in.EstimatedFee = 10
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		session string
		device  string
	}{
		{"user account session", "s1", "dev1"},
		{"wrong device", "sa", "wrong-device"},
		{"unknown session", "nope", "dev-approve"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := w.Approve("u1", "r-direct", tc.session, tc.device); !errors.Is(err, ErrNotApprover) {
				t.Fatalf("err = %v, want ErrNotApprover", err)
			}
		})
	}

	// 已吊销的出资会话：仍返回无权审批。
	if err := w.RevokeSession("sa"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Approve("u1", "r-direct", "sa", "dev-approve"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("revoked session err = %v, want ErrNotApprover", err)
	}

	// 已到期的出资会话：同样先返回无权审批。
	if _, err := w.CreateSession("sa-exp", "payer", "dev-exp", c.t.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Approve("u1", "r-direct", "sa-exp", "dev-exp"); !errors.Is(err, ErrNotApprover) {
		t.Fatalf("expired session err = %v, want ErrNotApprover", err)
	}

	// 请求未被这些失败调用改变，仍是可结算的直接预留。
	if r, _ := w.Request("u1", "r-direct"); r.State != RequestReserved || r.ApproverAccountID != "" {
		t.Fatalf("direct request changed after failed approvals: %+v", r)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 90, Reserved: 10}) {
		t.Fatalf("balance changed after failed approvals: %+v", bal)
	}
}

func TestPendingDoesNotOccupySharedQuota(t *testing.T) {
	w, _ := setupApproval(t)
	// 多笔待审批不占用共享累计额度：直接预留仍可进行。
	for i := 0; i < 3; i++ {
		in := approvalApply()
		in.RequestID = fmt.Sprintf("p%d", i)
		if _, err := w.Apply(in); err != nil {
			t.Fatal(err)
		}
	}
	pv, _ := w.Policy("p-approval")
	if pv.ReservedTotal != 0 {
		t.Fatalf("pending occupies quota: reserved %d", pv.ReservedTotal)
	}
	// 直接预留一笔（费用等于门槛，不进入待审批）不受待审批影响。
	reserved := approvalApply()
	reserved.RequestID = "r-direct"
	reserved.EstimatedFee = 10
	if _, err := w.Apply(reserved); err != nil {
		t.Fatal(err)
	}
	pv, _ = w.Policy("p-approval")
	if pv.ReservedTotal != 10 {
		t.Fatalf("reserved total = %d, want 10", pv.ReservedTotal)
	}
}
