package wallet

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// applyForSession 在给定会话下提交一笔费用 20（超过门槛 10）的待审批申请。
func applyForSession(policyID, sessionID, deviceID, requestID string) RequestInput {
	return RequestInput{
		PolicyID:     policyID,
		RequestID:    requestID,
		AccountID:    "u1",
		SessionID:    sessionID,
		DeviceID:     deviceID,
		Operation:    "charge",
		Payee:        "shop",
		EstimatedFee: 20,
	}
}

// TestRevokeExpiresPendingAtOrAfterDeadlineWithoutQuery 覆盖核心缺陷：
// 申请已到等待截止时刻但从未被查询，吊销会话时必须进入过期终态，而不是
// 被记成因会话吊销而拒绝。
func TestRevokeExpiresPendingAtOrAfterDeadlineWithoutQuery(t *testing.T) {
	w, c := setupApproval(t)
	// 会话与策略窗口都长于等待时长：期限完全由 1 分钟等待时长决定。
	if _, err := w.CreateSession("s-long", "u1", "dev1", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	in := applyForSession("p-approval", "s-long", "dev1", "r1")
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}

	// 推进到恰达等待期限，但不查询这笔申请。
	c.t = c.t.Add(time.Minute)
	revokeAt := c.t
	if err := w.RevokeSession("s-long"); err != nil {
		t.Fatal(err)
	}

	req, _ := w.Request("u1", "r1")
	if req.State != RequestExpired {
		t.Fatalf("state = %v, want expired", req.State)
	}
	if req.RejectReason != "" {
		t.Fatalf("expired request must not carry revoke reason: %q", req.RejectReason)
	}
	if req.ApproverAccountID != "" {
		t.Fatalf("expired request must not carry approver: %q", req.ApproverAccountID)
	}
	if !req.DecidedAt.Equal(revokeAt) {
		t.Fatalf("decidedAt = %v, want revoke time %v", req.DecidedAt, revokeAt)
	}
	// 提交时间与等待截止时间保持不变。
	if !req.WaitDeadline.Equal(revokeAt) {
		t.Fatalf("wait deadline changed: %v, want %v", req.WaitDeadline, revokeAt)
	}

	// 账本：一条零金额过期记录，关联使用账户与请求号；不得有拒绝记录。
	if countKind(w.Ledger(), LedgerRejection) != 0 {
		t.Fatalf("due request must not be recorded as rejection: %+v", w.Ledger())
	}
	exps := entriesForRequest(w.Ledger(), "r1", LedgerExpiration)
	if len(exps) != 1 {
		t.Fatalf("expiration entries = %d, want 1", len(exps))
	}
	if exps[0].AccountID != "u1" || exps[0].Amount != 0 || !exps[0].At.Equal(revokeAt) {
		t.Fatalf("expiration entry = %+v", exps[0])
	}

	// 待审批没有冻结费用：余额与策略总额不变，也没有退款记录。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want untouched {100 0}", bal)
	}
	pv, _ := w.Policy("p-approval")
	if pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals changed: reserved %d spent %d", pv.ReservedTotal, pv.SpentTotal)
	}
	if countKind(w.Ledger(), LedgerRefund) != 0 {
		t.Fatal("pending expiry must not create refund entries")
	}

	// 会话已被吊销，新申请被拒绝。
	if _, err := w.Apply(in); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("apply after revoke err = %v, want ErrSessionRevoked", err)
	}
}

// TestRevokeExpiryParityWithPriorQuery 终态与结束原因不应取决于是否有人
// 先查过：先查询再吊销与不查询直接吊销，结果同为过期、同无吊销原因。
func TestRevokeExpiryParityWithPriorQuery(t *testing.T) {
	makeWallet := func(t *testing.T) (*Wallet, *clock) {
		w, c := setupApproval(t)
		if _, err := w.CreateSession("s-long", "u1", "dev1", c.t.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Apply(applyForSession("p-approval", "s-long", "dev1", "r1")); err != nil {
			t.Fatal(err)
		}
		return w, c
	}

	// 路径 A：到期后先查询，再吊销。
	wA, cA := makeWallet(t)
	cA.t = cA.t.Add(time.Minute)
	queried, err := wA.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if queried.State != RequestExpired {
		t.Fatalf("queried state = %v, want expired", queried.State)
	}
	cA.t = cA.t.Add(30 * time.Second)
	if err := wA.RevokeSession("s-long"); err != nil {
		t.Fatal(err)
	}
	reqA, _ := wA.Request("u1", "r1")

	// 路径 B：到期后不查询直接吊销。
	wB, cB := makeWallet(t)
	cB.t = cB.t.Add(90 * time.Second)
	if err := wB.RevokeSession("s-long"); err != nil {
		t.Fatal(err)
	}
	reqB, _ := wB.Request("u1", "r1")

	if reqA.State != RequestExpired || reqB.State != RequestExpired {
		t.Fatalf("states = %v / %v, both want expired", reqA.State, reqB.State)
	}
	if reqA.RejectReason != "" || reqB.RejectReason != "" {
		t.Fatalf("reject reasons = %q / %q, both want empty", reqA.RejectReason, reqB.RejectReason)
	}
	if reqA.ApproverAccountID != "" || reqB.ApproverAccountID != "" {
		t.Fatalf("approvers = %q / %q, both want empty", reqA.ApproverAccountID, reqB.ApproverAccountID)
	}
	if !reqA.WaitDeadline.Equal(reqB.WaitDeadline) {
		t.Fatalf("wait deadlines differ: %v vs %v", reqA.WaitDeadline, reqB.WaitDeadline)
	}
	// 先前已进入终态的申请保留原决定信息：A 的决定时间是查询时刻，不被吊销改写。
	if !reqA.DecidedAt.Equal(cA.t.Add(-30 * time.Second)) {
		t.Fatalf("pre-existing decision rewritten: %v", reqA.DecidedAt)
	}
	// B 的决定时间是本次吊销时刻。
	if !reqB.DecidedAt.Equal(cB.t) {
		t.Fatalf("decision at processing time = %v, want %v", reqB.DecidedAt, cB.t)
	}
	// 两条路径都只有一条过期记录，且都没有拒绝记录。
	for name, w := range map[string]*Wallet{"queried": wA, "unqueried": wB} {
		if got := countKind(w.Ledger(), LedgerExpiration); got != 1 {
			t.Fatalf("%s: expiration entries = %d, want 1", name, got)
		}
		if got := countKind(w.Ledger(), LedgerRejection); got != 0 {
			t.Fatalf("%s: rejection entries = %d, want 0", name, got)
		}
	}
}

// TestRevokeRejectsPendingStrictlyBeforeDeadline 严格早于截止时间的待审批
// 请求继续随吊销进入拒绝终态。
func TestRevokeRejectsPendingStrictlyBeforeDeadline(t *testing.T) {
	w, c := setupApproval(t)
	if _, err := w.CreateSession("s-long", "u1", "dev1", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(applyForSession("p-approval", "s-long", "dev1", "r1")); err != nil {
		t.Fatal(err)
	}

	c.t = c.t.Add(30 * time.Second) // 距离 1 分钟期限还有 30 秒
	revokeAt := c.t
	if err := w.RevokeSession("s-long"); err != nil {
		t.Fatal(err)
	}
	req, _ := w.Request("u1", "r1")
	if req.State != RequestRejected {
		t.Fatalf("state = %v, want rejected", req.State)
	}
	if !strings.Contains(req.RejectReason, "revoked") {
		t.Fatalf("reject reason = %q, want session-revoked explanation", req.RejectReason)
	}
	if !req.DecidedAt.Equal(revokeAt) {
		t.Fatalf("decidedAt = %v, want revoke time %v", req.DecidedAt, revokeAt)
	}
	rejs := entriesForRequest(w.Ledger(), "r1", LedgerRejection)
	if len(rejs) != 1 || rejs[0].AccountID != "u1" || rejs[0].Amount != 0 {
		t.Fatalf("rejection entries = %+v", rejs)
	}
	if countKind(w.Ledger(), LedgerExpiration) != 0 {
		t.Fatal("before-deadline request must not be expired")
	}
}

// TestRevokeSplitsPendingOfSameSessionByOwnDeadline 同一会话的多笔申请可能
// 处于不同阶段，吊销时必须分别按自身等待截止时间得到结果。
func TestRevokeSplitsPendingOfSameSessionByOwnDeadline(t *testing.T) {
	w, c := setupApproval(t)
	if _, err := w.CreateSession("s-long", "u1", "dev1", c.t.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// r1 在 t0 提交，期限 t0+60s；r2 在 t0+40s 提交，期限 t0+100s。
	if _, err := w.Apply(applyForSession("p-approval", "s-long", "dev1", "r1")); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(40 * time.Second)
	if _, err := w.Apply(applyForSession("p-approval", "s-long", "dev1", "r2")); err != nil {
		t.Fatal(err)
	}
	// t0+80s 吊销：r1 已过自身期限 → 过期；r2 未到自身期限 → 拒绝。
	c.t = c.t.Add(40 * time.Second)
	revokeAt := c.t
	if err := w.RevokeSession("s-long"); err != nil {
		t.Fatal(err)
	}

	r1, _ := w.Request("u1", "r1")
	if r1.State != RequestExpired || r1.RejectReason != "" || r1.ApproverAccountID != "" {
		t.Fatalf("r1 = %+v, want plain expired", r1)
	}
	if !r1.DecidedAt.Equal(revokeAt) {
		t.Fatalf("r1 decidedAt = %v, want %v", r1.DecidedAt, revokeAt)
	}
	r2, _ := w.Request("u1", "r2")
	if r2.State != RequestRejected || !strings.Contains(r2.RejectReason, "revoked") {
		t.Fatalf("r2 = %+v, want rejected with revoke reason", r2)
	}
	if !r2.DecidedAt.Equal(revokeAt) {
		t.Fatalf("r2 decidedAt = %v, want %v", r2.DecidedAt, revokeAt)
	}
	// 账本各一条：过期（r1）与拒绝（r2），金额均为零。
	if len(entriesForRequest(w.Ledger(), "r1", LedgerExpiration)) != 1 {
		t.Fatal("r1 must have one expiration entry")
	}
	if len(entriesForRequest(w.Ledger(), "r2", LedgerRejection)) != 1 {
		t.Fatal("r2 must have one rejection entry")
	}
	// 截止时刻更早的 r1 的过期记录先入账本。
	idxExp, idxRej := -1, -1
	for i, e := range w.Ledger() {
		if e.Kind == LedgerExpiration && e.RequestID == "r1" {
			idxExp = i
		}
		if e.Kind == LedgerRejection && e.RequestID == "r2" {
			idxRej = i
		}
	}
	if !(idxExp >= 0 && idxRej >= 0 && idxExp < idxRej) {
		t.Fatalf("ledger order: expiration idx %d should precede rejection idx %d", idxExp, idxRej)
	}
}

// TestRevokePendingExpiredByPolicyEndAndSessionExpiry 期限由策略结束或申请
// 会话到期先决定时，吊销同样按该期限区分结果。
func TestRevokePendingExpiredByPolicyEndAndSessionExpiry(t *testing.T) {
	// 场景 1：策略结束早于等待时长，吊销时已过策略结束时刻 → 过期。
	w, c := setupApproval(t)
	if _, err := w.CreateSession("s-long", "u1", "dev1", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	short := approvalPolicy(c)
	short.ID = "p-short"
	short.EndsAt = c.t.Add(30 * time.Second)
	if err := w.SavePolicy(short); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(applyForSession("p-short", "s-long", "dev1", "r1")); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(45 * time.Second)
	if err := w.RevokeSession("s-long"); err != nil {
		t.Fatal(err)
	}
	if r, _ := w.Request("u1", "r1"); r.State != RequestExpired || r.RejectReason != "" {
		t.Fatalf("policy-end-driven deadline request = %+v, want expired", r)
	}

	// 场景 2：会话到期早于等待时长，会话已到期后主动吊销 → 过期而非拒绝。
	w2, c2 := setupApproval(t)
	sessExp := c2.t.Add(20 * time.Second)
	if _, err := w2.CreateSession("s-short", "u1", "dev1", sessExp); err != nil {
		t.Fatal(err)
	}
	if _, err := w2.Apply(applyForSession("p-approval", "s-short", "dev1", "r1")); err != nil {
		t.Fatal(err)
	}
	// 已到期的会话也允许主动吊销。
	c2.t = c2.t.Add(30 * time.Second)
	if err := w2.RevokeSession("s-short"); err != nil {
		t.Fatalf("expired session should be revocable: %v", err)
	}
	r, _ := w2.Request("u1", "r1")
	if r.State != RequestExpired {
		t.Fatalf("state = %v, want expired", r.State)
	}
	if r.RejectReason != "" || r.ApproverAccountID != "" {
		t.Fatalf("expired request carries revoke decision: reason=%q approver=%q", r.RejectReason, r.ApproverAccountID)
	}
	if view, _ := w2.Session("s-short"); view.State != SessionRevoked {
		t.Fatalf("session state = %v, want revoked", view.State)
	}
	if len(entriesForRequest(w2.Ledger(), "r1", LedgerExpiration)) != 1 {
		t.Fatal("want one expiration entry")
	}
}

// TestRevokeIdempotentDoesNotRewriteDueRequest 吊销后再吊销返回成功，不
// 重复追加状态记录，也不改写已产生的终态。
func TestRevokeIdempotentDoesNotRewriteDueRequest(t *testing.T) {
	w, c := setupApproval(t)
	if _, err := w.CreateSession("s-long", "u1", "dev1", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(applyForSession("p-approval", "s-long", "dev1", "r1")); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Minute)
	firstRevoke := c.t
	if err := w.RevokeSession("s-long"); err != nil {
		t.Fatal(err)
	}
	before := len(w.Ledger())

	// 时间继续推进后重复吊销：成功但不留痕、决定时间保持首次吊销时刻。
	c.t = c.t.Add(5 * time.Minute)
	if err := w.RevokeSession("s-long"); err != nil {
		t.Fatalf("repeat revoke should succeed: %v", err)
	}
	if len(w.Ledger()) != before {
		t.Fatal("ledger grew on repeat revoke")
	}
	req, _ := w.Request("u1", "r1")
	if req.State != RequestExpired || !req.DecidedAt.Equal(firstRevoke) {
		t.Fatalf("request rewritten by repeat revoke: %+v", req)
	}

	// 不存在的会话仍返回 ErrSessionNotFound，不改变请求或账本。
	if err := w.RevokeSession("missing"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("missing session err = %v, want ErrSessionNotFound", err)
	}
	if len(w.Ledger()) != before {
		t.Fatal("ledger changed after revoking missing session")
	}
}

// TestRevokeLeavesOtherSessionsAndTerminalRequestsUntouched 吊销只处理本
// 会话仍在待审批的申请；其他会话的待审批申请保持可批准，已有终态（含
// 早先过期）的请求不被改写。
func TestRevokeLeavesOtherSessionsAndTerminalRequestsUntouched(t *testing.T) {
	w, c := setupApproval(t)
	if _, err := w.CreateSession("s-long", "u1", "dev1", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// 本会话一笔未到期限的待审批（将被拒绝）。
	if _, err := w.Apply(applyForSession("p-approval", "s-long", "dev1", "r1")); err != nil {
		t.Fatal(err)
	}
	// 本会话一笔已过期限且已查询过（早已进入过期终态，决定时间为查询时刻）。
	if _, err := w.Apply(applyForSession("p-approval", "s-long", "dev1", "r-old")); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Minute)
	oldDecidedAt := c.t
	if r, err := w.Request("u1", "r-old"); err != nil || r.State != RequestExpired {
		t.Fatalf("r-old should already be expired: %+v %v", r, err)
	}
	// 另一会话（u2/s2-long）的待审批申请不应受本次吊销影响。
	if _, err := w.CreateSession("s2-long", "u2", "dev2", c.t.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	other := RequestInput{
		PolicyID: "p-approval", RequestID: "ro1", AccountID: "u2",
		SessionID: "s2-long", DeviceID: "dev2", Operation: "charge", Payee: "shop", EstimatedFee: 20,
	}
	c.t = c.t.Add(10 * time.Second)
	if _, err := w.Apply(other); err != nil {
		t.Fatal(err)
	}
	// 一笔已预留请求：吊销不影响其费用与状态。
	reserved := applyForSession("p-approval", "s-long", "dev1", "r-res")
	reserved.EstimatedFee = 8
	if _, err := w.Apply(reserved); err != nil {
		t.Fatal(err)
	}
	before := len(w.Ledger())

	c.t = c.t.Add(20 * time.Second) // 距 r1 提交 90s（已过期），吊销时刻
	if err := w.RevokeSession("s-long"); err != nil {
		t.Fatal(err)
	}
	// 吊销只新增一条 r1 的过期记录。
	if got := len(w.Ledger()) - before; got != 1 {
		t.Fatalf("ledger grew by %d, want 1 (r1 expiration)", got)
	}

	// r1：未查询过、已到期限 → 过期。
	if r, _ := w.Request("u1", "r1"); r.State != RequestExpired || r.RejectReason != "" {
		t.Fatalf("r1 = %+v, want expired without revoke reason", r)
	}
	// r-old：终态与原决定时间保留，不重复追加过期记录。
	old, _ := w.Request("u1", "r-old")
	if old.State != RequestExpired || !old.DecidedAt.Equal(oldDecidedAt) {
		t.Fatalf("r-old rewritten: %+v", old)
	}
	if len(entriesForRequest(w.Ledger(), "r-old", LedgerExpiration)) != 1 {
		t.Fatal("r-old expiration entry must not be duplicated")
	}
	// 其他会话的待审批申请保持待审批且仍可批准。
	if r, _ := w.Request("u2", "ro1"); r.State != RequestPendingApproval {
		t.Fatalf("other session request = %v, want still pending", r.State)
	}
	if _, err := w.Approve("u2", "ro1", "sa", "dev-approve"); err != nil {
		t.Fatalf("other session request should still be approvable: %v", err)
	}
	// 已预留请求保持预留，费用未被提前释放。
	if r, _ := w.Request("u1", "r-res"); r.State != RequestReserved {
		t.Fatalf("reserved request = %v, want reserved", r.State)
	}
	if n := countKind(w.Ledger(), LedgerExpiration); n != 2 {
		t.Fatalf("expiration entries = %d, want 2 (r1 and r-old only)", n)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 72, Reserved: 28}) {
		// r-res 预留 8 + ro1 批准预留 20。
		t.Fatalf("balance = %+v, want {72 28}", bal)
	}
}

// entriesForRequest 返回指定请求的指定类型账本记录。
func entriesForRequest(entries []LedgerEntry, requestID string, kind LedgerKind) []LedgerEntry {
	var out []LedgerEntry
	for _, e := range entries {
		if e.Kind == kind && e.RequestID == requestID {
			out = append(out, e)
		}
	}
	return out
}
