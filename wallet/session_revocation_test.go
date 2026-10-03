package wallet

import (
	"errors"
	"testing"
	"time"
)

// ledgerEntryFor 找出指定请求的最后一条指定类型账本记录。
func ledgerEntryFor(entries []LedgerEntry, kind LedgerKind, requestID string) (LedgerEntry, bool) {
	var found LedgerEntry
	var ok bool
	for _, e := range entries {
		if e.Kind == kind && e.RequestID == requestID {
			found, ok = e, true
		}
	}
	return found, ok
}

// TestRevokeSessionExpiresDuePendingWithoutQuery：申请已到等待截止时刻但
// 从未被查询过，吊销时必须按其原有截止时间进入过期终态，而不是吊销拒绝。
func TestRevokeSessionExpiresDuePendingWithoutQuery(t *testing.T) {
	w, c := setupApproval(t)
	req, err := w.Apply(approvalApply())
	if err != nil {
		t.Fatal(err)
	}
	deadline := req.WaitDeadline
	createdAt := req.CreatedAt
	before := len(w.Ledger())

	// 直接越过等待截止时刻，中途不查询这笔申请。
	c.t = deadline.Add(time.Second)
	revokeAt := c.t
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}

	got, err := w.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestExpired {
		t.Fatalf("state = %v, want expired", got.State)
	}
	if got.RejectReason != "" {
		t.Fatalf("expired by deadline must carry no revoke reason: %q", got.RejectReason)
	}
	if got.ApproverAccountID != "" {
		t.Fatalf("expired request must not record approver: %q", got.ApproverAccountID)
	}
	if !got.DecidedAt.Equal(revokeAt) {
		t.Fatalf("decided at %v, want revoke time %v", got.DecidedAt, revokeAt)
	}
	// 提交时间与等待截止时间保持不变。
	if !got.CreatedAt.Equal(createdAt) || !got.WaitDeadline.Equal(deadline) {
		t.Fatalf("timing changed: created %v/%v deadline %v/%v", got.CreatedAt, createdAt, got.WaitDeadline, deadline)
	}

	// 账本：一条零金额过期留痕，没有拒绝记录。
	all := w.Ledger()
	if len(all) != before+1 {
		t.Fatalf("ledger entries = %d, want %d", len(all), before+1)
	}
	e, ok := ledgerEntryFor(all, LedgerExpiration, "r1")
	if !ok {
		t.Fatal("missing LedgerExpiration entry")
	}
	if e.AccountID != "u1" || e.Amount != 0 {
		t.Fatalf("expiration entry = %+v", e)
	}
	if !e.At.Equal(revokeAt) {
		t.Fatalf("ledger at %v, want processing (revoke) time %v", e.At, revokeAt)
	}
	if countKind(all, LedgerRejection) != 0 {
		t.Fatal("due pending request must not be recorded as revocation rejection")
	}

	// 待审批没有冻结费用：余额与策略累计均不变。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want untouched", bal)
	}
	if pv, _ := w.Policy("p-approval"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}
}

// TestRevokeSessionDeadlineBoundary：严格早于截止拒绝；等于或晚于截止过期。
func TestRevokeSessionDeadlineBoundary(t *testing.T) {
	cases := []struct {
		name   string
		offset time.Duration
		want   RequestState
	}{
		{"strictly before deadline", -time.Second, RequestRejected},
		{"exactly at deadline", 0, RequestExpired},
		{"after deadline", time.Second, RequestExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, c := setupApproval(t)
			req, err := w.Apply(approvalApply())
			if err != nil {
				t.Fatal(err)
			}
			c.t = req.WaitDeadline.Add(tc.offset)
			if err := w.RevokeSession("s1"); err != nil {
				t.Fatal(err)
			}
			got, _ := w.Request("u1", "r1")
			if got.State != tc.want {
				t.Fatalf("state = %v, want %v", got.State, tc.want)
			}
			if tc.want == RequestExpired {
				if got.RejectReason != "" || got.ApproverAccountID != "" {
					t.Fatalf("expired request carries rejection fields: reason %q approver %q", got.RejectReason, got.ApproverAccountID)
				}
				if countKind(w.Ledger(), LedgerRejection) != 0 {
					t.Fatal("must not append rejection for due request")
				}
			} else {
				if got.RejectReason == "" {
					t.Fatal("revoked-before-deadline request must carry reason")
				}
				if got.ApproverAccountID != "" {
					t.Fatalf("revocation rejection must not fill approver, got %q", got.ApproverAccountID)
				}
			}
		})
	}
}

// TestRevokeSessionSameAsQueryOutcome：先查询再吊销与不查询直接吊销，终态、
// 原因和账本类型必须一致，差异只在惰性处理记录的决定/账本时间上。
func TestRevokeSessionSameAsQueryOutcome(t *testing.T) {
	setup := func(t *testing.T) (*Wallet, *clock, time.Time) {
		w, c := setupApproval(t)
		req, err := w.Apply(approvalApply())
		if err != nil {
			t.Fatal(err)
		}
		return w, c, req.WaitDeadline
	}

	// 路径 A：先查询触发惰性过期，再吊销。
	wA, cA, deadline := setup(t)
	cA.t = deadline.Add(time.Second)
	queriedAt := cA.t // 复制时刻值，避免随后推进时钟时被改写。
	if r, err := wA.Request("u1", "r1"); err != nil || r.State != RequestExpired {
		t.Fatalf("query should lazily expire: state=%v err=%v", r.State, err)
	}
	cA.t = cA.t.Add(time.Minute)
	if err := wA.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	rA, _ := wA.Request("u1", "r1")

	// 路径 B：不查询直接在同一相对时刻吊销。
	wB, cB, deadlineB := setup(t)
	cB.t = deadlineB.Add(time.Second + time.Minute)
	revokeAt := cB
	if err := wB.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	rB, _ := wB.Request("u1", "r1")

	if rA.State != RequestExpired || rB.State != RequestExpired {
		t.Fatalf("states = %v, %v, both want expired", rA.State, rB.State)
	}
	if rA.RejectReason != rB.RejectReason || rA.ApproverAccountID != rB.ApproverAccountID {
		t.Fatalf("rejection fields differ: A(%q,%q) B(%q,%q)",
			rA.RejectReason, rA.ApproverAccountID, rB.RejectReason, rB.ApproverAccountID)
	}
	if !rA.WaitDeadline.Equal(rB.WaitDeadline) || !rA.CreatedAt.Equal(rB.CreatedAt) {
		t.Fatal("deadline or created time differs")
	}
	// 决定/账本时间沿用既有惰性口径：各自在被处理时记录。
	if !rA.DecidedAt.Equal(queriedAt) || !rB.DecidedAt.Equal(revokeAt.t) {
		t.Fatalf("decided times = %v, %v, want processing times %v, %v", rA.DecidedAt, rB.DecidedAt, queriedAt, revokeAt.t)
	}
	for _, w := range []*Wallet{wA, wB} {
		if countKind(w.Ledger(), LedgerExpiration) != 1 || countKind(w.Ledger(), LedgerRejection) != 0 {
			t.Fatalf("ledger kinds wrong: expirations %d rejections %d",
				countKind(w.Ledger(), LedgerExpiration), countKind(w.Ledger(), LedgerRejection))
		}
	}
}

// TestRevokeSessionMixedStagesPerRequest：同一会话多笔申请处于不同阶段时，
// 各笔按自身期限分别得到结果；终态与已预留请求不被改写。
func TestRevokeSessionMixedStagesPerRequest(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	// 会话与策略窗口都足够长，等待期限只由提交时刻 + 等待时长决定。
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	p := approvalPolicy(c)
	p.EndsAt = c.t.Add(4 * time.Hour)
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	apply := func(rid string, fee int64) RequestView {
		in := approvalApply()
		in.RequestID = rid
		in.EstimatedFee = fee
		req, err := w.Apply(in)
		if err != nil {
			t.Fatalf("apply %s: %v", rid, err)
		}
		return req
	}

	// r1 在 t=0 提交：截止 60s。
	r1 := apply("r1", 20)
	// t=30s 再提交 r2：截止 90s。
	c.t = c.t.Add(30 * time.Second)
	r2 := apply("r2", 20)
	if r2.WaitDeadline.Equal(r1.WaitDeadline) {
		t.Fatalf("r2 deadline %v should be later than r1 %v", r2.WaitDeadline, r1.WaitDeadline)
	}
	// r3：待审批后取消（终态）。
	apply("r3", 20)
	if _, err := w.Cancel("u1", "r3"); err != nil {
		t.Fatal(err)
	}
	// r4：费用不超门槛，直接预留。
	apply("r4", 5)

	// 在 60 秒处吊销：r1 已到截止（60s），r2 未到（90s）。
	c.t = r1.WaitDeadline
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}

	got1, _ := w.Request("u1", "r1")
	got2, _ := w.Request("u1", "r2")
	got3, _ := w.Request("u1", "r3")
	got4, _ := w.Request("u1", "r4")
	if got1.State != RequestExpired {
		t.Fatalf("r1 state = %v, want expired (deadline %s <= revoke %s)", got1.State, r1.WaitDeadline, c.t)
	}
	if got1.RejectReason != "" {
		t.Fatalf("r1 must not carry revoke reason: %q", got1.RejectReason)
	}
	if got2.State != RequestRejected {
		t.Fatalf("r2 state = %v, want rejected before its own deadline", got2.State)
	}
	if got3.State != RequestCancelled {
		t.Fatalf("r3 terminal state rewritten: %v", got3.State)
	}
	if got4.State != RequestReserved {
		t.Fatalf("r4 reserved state rewritten: %v", got4.State)
	}

	all := w.Ledger()
	// r1 恰好一条过期记录，r2 恰好一条拒绝记录；r3/r4 不新增状态记录。
	if e, ok := ledgerEntryFor(all, LedgerExpiration, "r1"); !ok || e.Amount != 0 || e.AccountID != "u1" {
		t.Fatalf("r1 expiration entry wrong: %+v ok=%v", e, ok)
	}
	if e, ok := ledgerEntryFor(all, LedgerRejection, "r2"); !ok || e.Amount != 0 || e.AccountID != "u1" {
		t.Fatalf("r2 rejection entry wrong: %+v ok=%v", e, ok)
	}
	if _, ok := ledgerEntryFor(all, LedgerExpiration, "r2"); ok {
		t.Fatal("r2 must not have expiration entry")
	}
	if _, ok := ledgerEntryFor(all, LedgerRejection, "r1"); ok {
		t.Fatal("r1 must not have rejection entry")
	}
	// 取消记录仍是 r3 唯一的终态记录。
	if n := countKind(all, LedgerCancellation); n != 1 {
		t.Fatalf("cancellation entries = %d, want 1", n)
	}

	// 余额只反映 r4 的 5 预留；吊销不提前释放、不新增任何资金记录。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 95, Reserved: 5}) {
		t.Fatalf("balance = %+v, want {95 5}", bal)
	}
	if pv, _ := w.Policy("p-approval"); pv.ReservedTotal != 5 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 5/0", pv.ReservedTotal, pv.SpentTotal)
	}
	// 已预留请求吊销后仍可结算。
	if _, err := w.Settle("u1", "r4", 5); err != nil {
		t.Fatalf("settle reserved after revoke: %v", err)
	}
}

// TestRevokeSessionDeadlineFromSessionAndPolicy：截止由申请会话到期或策略
// 结束先发生时，吊销落在截止之后同样进入过期而非拒绝；已到期会话允许吊销。
func TestRevokeSessionDeadlineFromSessionAndPolicy(t *testing.T) {
	t.Run("session expiry first", func(t *testing.T) {
		w, c := setupApproval(t)
		// s1 原本 1 分钟到期，等待时长也是 1 分钟；新建 30 秒到期的会话。
		if _, err := w.CreateSession("s-short", "u1", "dev1", c.t.Add(30*time.Second)); err != nil {
			t.Fatal(err)
		}
		in := approvalApply()
		in.SessionID = "s-short"
		in.RequestID = "rs"
		req, err := w.Apply(in)
		if err != nil {
			t.Fatal(err)
		}
		if !req.WaitDeadline.Equal(c.t.Add(30 * time.Second)) {
			t.Fatalf("deadline = %v, want session expiry", req.WaitDeadline)
		}
		// 会话已到期后主动吊销。
		c.t = c.t.Add(30 * time.Second)
		if err := w.RevokeSession("s-short"); err != nil {
			t.Fatalf("revoking expired session should succeed: %v", err)
		}
		got, _ := w.Request("u1", "rs")
		if got.State != RequestExpired || got.RejectReason != "" {
			t.Fatalf("state = %v reason %q, want plain expiration", got.State, got.RejectReason)
		}
	})

	t.Run("policy end first", func(t *testing.T) {
		w, c := setupApproval(t)
		p := approvalPolicy(c)
		p.ID = "p-short"
		p.EndsAt = c.t.Add(20 * time.Second)
		if err := w.SavePolicy(p); err != nil {
			t.Fatal(err)
		}
		in := approvalApply()
		in.PolicyID = "p-short"
		in.RequestID = "rp"
		req, err := w.Apply(in)
		if err != nil {
			t.Fatal(err)
		}
		if !req.WaitDeadline.Equal(p.EndsAt) {
			t.Fatalf("deadline = %v, want policy end %v", req.WaitDeadline, p.EndsAt)
		}
		c.t = p.EndsAt // 恰为截止时刻：过期而非拒绝。
		if err := w.RevokeSession("s1"); err != nil {
			t.Fatal(err)
		}
		got, _ := w.Request("u1", "rp")
		if got.State != RequestExpired || got.ApproverAccountID != "" {
			t.Fatalf("state = %v approver %q, want plain expiration", got.State, got.ApproverAccountID)
		}
	})
}

// TestRevokeSessionIdempotentAndIsolated：重复吊销不重复留痕；其他会话的
// 待审批申请保持不变；会话不存在返回 ErrSessionNotFound 且不改账本。
func TestRevokeSessionIdempotentAndIsolated(t *testing.T) {
	w, c := setupApproval(t)

	// s1 一笔未到期、s2 一笔未到期。
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}
	in2 := approvalApply()
	in2.AccountID, in2.SessionID, in2.DeviceID, in2.RequestID = "u2", "s2", "dev2", "r2"
	if _, err := w.Apply(in2); err != nil {
		t.Fatal(err)
	}

	c.t = c.t.Add(10 * time.Second)
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	entriesAfterFirst := len(w.Ledger())

	// 重复吊销成功，不重复追加状态记录。
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatalf("repeat revoke: %v", err)
	}
	if len(w.Ledger()) != entriesAfterFirst {
		t.Fatalf("ledger grew on repeat revoke: %d -> %d", entriesAfterFirst, len(w.Ledger()))
	}

	// 其他会话的待审批申请不受影响，仍可被批准。
	got2, _ := w.Request("u2", "r2")
	if got2.State != RequestPendingApproval {
		t.Fatalf("other session request = %v, want still pending", got2.State)
	}
	if _, err := w.Approve("u2", "r2", "sa", "dev-approve"); err != nil {
		t.Fatalf("approve on unaffected session: %v", err)
	}

	// 吊销后该会话的新申请被拒绝。
	if _, err := w.Apply(approvalApply()); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("apply after revoke err = %v, want ErrSessionRevoked", err)
	}

	// 不存在的会话：ErrSessionNotFound，账本不变。
	before := len(w.Ledger())
	if err := w.RevokeSession("ghost"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("revoke missing err = %v, want ErrSessionNotFound", err)
	}
	if len(w.Ledger()) != before {
		t.Fatal("ledger changed on revoking unknown session")
	}
}

// TestRevokeSessionPriorQueryThenRevokeNoDuplicate：先查询使申请惰性过期后，
// 吊销不得再追加任何状态记录。
func TestRevokeSessionPriorQueryThenRevokeNoDuplicate(t *testing.T) {
	w, c := setupApproval(t)
	if _, err := w.Apply(approvalApply()); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Minute)
	if r, _ := w.Request("u1", "r1"); r.State != RequestExpired {
		t.Fatalf("state = %v, want expired", r.State)
	}
	before := len(w.Ledger())
	c.t = c.t.Add(time.Minute)
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	if len(w.Ledger()) != before {
		t.Fatalf("ledger grew %d -> %d on revoking already-expired request", before, len(w.Ledger()))
	}
	r, _ := w.Request("u1", "r1")
	if r.State != RequestExpired || r.RejectReason != "" {
		t.Fatalf("terminal request rewritten: %+v", r)
	}
}
