package wallet

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// 本文件为“同一使用账户 + 相同请求编号”的重复申请补充会话校验回归保障：
// 幂等取回已有结果之前仍必须通过会话校验（会话属于该使用账户、绑定当前
// 设备且有效），不能因为请求已经受理就跳过；失效会话的重试只留下零金额
// 的申请拒绝记录，原请求、资金与策略累计一律不变；合法更换会话后重复
// 申请照常返回原请求，且不再次冻结费用、不追加账本记录。

// reapplyClockBase 等为各场景共享的时间安排：申请在基刻受理，s1 在 1 小时
// 后到期，替代会话 s1b 与策略窗口都覆盖全部测试时刻（且关闭预留超时）。
var (
	reapplySession1ExpiryOffset = time.Hour
	reapplySession2ExpiryOffset = 4 * time.Hour
	reapplyPolicyEndOffset      = 5 * time.Hour
)

// setupReapplyReserved 构造一笔已成功预留费用的请求：
// 出资账户 payer（余额 1000），使用账户 u1，会话 s1（绑定 dev1）与替代
// 会话 s1b（绑定 dev1b），策略 p1 关闭审批与预留超时；在基刻以 s1 提交
// r1（预估 100）并直接预留，返回首次受理视图与可原样重放的申请内容。
func setupReapplyReserved(t *testing.T) (*Wallet, *clock, RequestInput, RequestView) {
	t.Helper()
	w, c := newTestWallet()
	base := c.t
	mustAccount(t, w, "payer", 1000)
	mustAccount(t, w, "u1", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", base.Add(reapplySession1ExpiryOffset)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s1b", "u1", "dev1b", base.Add(reapplySession2ExpiryOffset)); err != nil {
		t.Fatal(err)
	}
	p := PolicySpec{
		ID:                "p1",
		PayerAccountID:    "payer",
		AllowedAccountIDs: []string{"u1"},
		Operation:         "charge",
		Payee:             "shop",
		StartsAt:          base.Add(-time.Hour),
		EndsAt:            base.Add(reapplyPolicyEndOffset),
		MaxPerRequest:     100,
		MaxTotal:          1000,
	}
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	in := RequestInput{
		PolicyID:     "p1",
		RequestID:    "r1",
		AccountID:    "u1",
		SessionID:    "s1",
		DeviceID:     "dev1",
		Operation:    "charge",
		Payee:        "shop",
		EstimatedFee: 100,
	}
	first, err := w.Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != RequestReserved {
		t.Fatalf("setup: first state = %v, want reserved", first.State)
	}
	return w, c, in, first
}

// countRequestEntries 统计关联指定请求的某类账本记录条数。
func countRequestEntries(entries []LedgerEntry, kind LedgerKind, requestID string) int {
	n := 0
	for _, e := range entries {
		if e.Kind == kind && e.RequestID == requestID {
			n++
		}
	}
	return n
}

// latestRejection 返回指定请求最后一条申请拒绝记录。
func latestRejection(entries []LedgerEntry, requestID string) (LedgerEntry, bool) {
	var found LedgerEntry
	ok := false
	for _, e := range entries {
		if e.Kind == LedgerRejection && e.RequestID == requestID {
			found, ok = e, true
		}
	}
	return found, ok
}

// assertReservedRequestUnchanged 断言原已预留请求没有被失败的重复申请改写：
// 状态仍是已预留、受理/预留时刻与预估费用不变、不携带任何拒绝或审批决定；
// 出资账户余额与策略预留/已花费累计也保持调用方给定的快照。
func assertReservedRequestUnchanged(t *testing.T, w *Wallet, first RequestView, wantBal Balances, wantReserved, wantSpent int64) {
	t.Helper()
	got, err := w.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestReserved {
		t.Fatalf("original request state = %v, want still reserved (the failed call must not reject the original request)", got.State)
	}
	if !got.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("created at changed: %v, want %v", got.CreatedAt, first.CreatedAt)
	}
	if !got.ReservedAt.Equal(first.ReservedAt) {
		t.Fatalf("reserved at changed: %v, want %v", got.ReservedAt, first.ReservedAt)
	}
	if !got.ReserveDeadline.Equal(first.ReserveDeadline) || got.ReserveDuration != first.ReserveDuration {
		t.Fatalf("reserve timing changed: deadline %v/%v duration %v/%v",
			got.ReserveDeadline, first.ReserveDeadline, got.ReserveDuration, first.ReserveDuration)
	}
	if got.EstimatedFee != first.EstimatedFee || got.EstimatedFee != 100 {
		t.Fatalf("estimated fee changed: %d, want %d", got.EstimatedFee, first.EstimatedFee)
	}
	if got.ActualFee != 0 {
		t.Fatalf("failed re-apply must not settle: actual fee = %d", got.ActualFee)
	}
	if got.RejectReason != "" || got.ApproverAccountID != "" || !got.DecidedAt.IsZero() {
		t.Fatalf("original request carries decision fields from the failed call: reason %q approver %q decided %v",
			got.RejectReason, got.ApproverAccountID, got.DecidedAt)
	}
	if bal, _ := w.Balance("payer"); bal != wantBal {
		t.Fatalf("payer balance = %+v, want %+v", bal, wantBal)
	}
	if pv, _ := w.Policy("p1"); pv.ReservedTotal != wantReserved || pv.SpentTotal != wantSpent {
		t.Fatalf("policy totals = reserved %d spent %d, want %d/%d",
			pv.ReservedTotal, pv.SpentTotal, wantReserved, wantSpent)
	}
}

// assertOnlyRejectionAppended 断言失败的重复申请只新增了一条零金额申请拒绝
// 记录（关联本次使用账户与请求编号、原因含会话失败说明、时间为当前时刻），
// 原有资金记录（一条全额预留）保留且没有新增任何退款、扣减或预留。
func assertOnlyRejectionAppended(t *testing.T, w *Wallet, ledgerLenBefore int, requestID, reasonPart string, at time.Time) {
	t.Helper()
	all := w.Ledger()
	if len(all) != ledgerLenBefore+1 {
		t.Fatalf("ledger entries = %d, want exactly one appended rejection (%d)", len(all), ledgerLenBefore+1)
	}
	rec, ok := latestRejection(all, requestID)
	if !ok {
		t.Fatal("missing zero-amount LedgerRejection for the failed duplicate apply")
	}
	if rec.AccountID != "u1" {
		t.Fatalf("rejection account = %q, want using account u1", rec.AccountID)
	}
	if rec.Amount != 0 {
		t.Fatalf("rejection amount = %d, want 0", rec.Amount)
	}
	if rec.PolicyID != "" {
		t.Fatalf("application rejection must not carry policy id, got %q", rec.PolicyID)
	}
	if !strings.Contains(rec.Reason, reasonPart) {
		t.Fatalf("rejection reason = %q, want it to mention %q", rec.Reason, reasonPart)
	}
	if !rec.At.Equal(at) {
		t.Fatalf("rejection at %v, want %v", rec.At, at)
	}
	// 原预留保留；失败的重复申请不产生退款、扣减或第二条预留。
	if n := countRequestEntries(all, LedgerReserve, requestID); n != 1 {
		t.Fatalf("reserve entries = %d, want exactly the original one", n)
	}
	if n := countRequestEntries(all, LedgerSettle, requestID); n != 0 {
		t.Fatalf("failed re-apply produced %d settle entries", n)
	}
	if n := countRequestEntries(all, LedgerRefund, requestID); n != 0 {
		t.Fatalf("failed re-apply produced %d refund entries", n)
	}
	if e, ok := ledgerEntryFor(all, LedgerReserve, requestID); !ok || e.Amount != 100 || e.AccountID != "payer" {
		t.Fatalf("original reserve entry altered: %+v ok=%v", e, ok)
	}
}

// TestDuplicateApplyRevokedSessionRejected：申请会话被吊销后，用原会话提交
// 相同申请必须返回会话已吊销错误，而不是取回已预留结果。
func TestDuplicateApplyRevokedSessionRejected(t *testing.T) {
	w, c, in, first := setupReapplyReserved(t)

	c.t = c.t.Add(5 * time.Minute)
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	before := len(w.Ledger())

	replay, err := w.Apply(in)
	if !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("re-apply on revoked session err = %v, want ErrSessionRevoked", err)
	}
	if replay.State != 0 || replay.RequestID != "" {
		t.Fatalf("failed call must not return a request view: %+v", replay)
	}

	// 原请求仍是已预留，资金与策略累计不变（可用 900 / 预留 100）。
	assertReservedRequestUnchanged(t, w, first, Balances{Available: 900, Reserved: 100}, 100, 0)
	// 只留下一条说明会话已吊销的零金额拒绝记录。
	assertOnlyRejectionAppended(t, w, before, "r1", "revoked", c.t)

	// 原请求继续存在并可正常结算：本次调用被拒绝不等于原请求被拒绝。
	if settled, err := w.Settle("u1", "r1", 100); err != nil || settled.State != RequestSettled {
		t.Fatalf("original reserved request should still settle after rejected replay: %+v %v", settled, err)
	}
}

// TestDuplicateApplyExpiredSessionRejected：会话自然到期后，相同操作返回会话
// 已过期错误；到期时刻本身也算过期。
func TestDuplicateApplyExpiredSessionRejected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset time.Duration
	}{
		{"exactly at expiry", time.Hour},
		{"after expiry", time.Hour + 5*time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, c, in, first := setupReapplyReserved(t)
			c.t = c.t.Add(tc.offset)
			before := len(w.Ledger())

			replay, err := w.Apply(in)
			if !errors.Is(err, ErrSessionExpired) {
				t.Fatalf("re-apply on expired session err = %v, want ErrSessionExpired", err)
			}
			if replay.RequestID != "" {
				t.Fatalf("failed call must not return a request view: %+v", replay)
			}

			assertReservedRequestUnchanged(t, w, first, Balances{Available: 900, Reserved: 100}, 100, 0)
			assertOnlyRejectionAppended(t, w, before, "r1", "expired", c.t)
		})
	}
}

// TestDuplicateApplyRevokedAndExpiredReportsRevoked：会话同时已吊销和已到期
// 时，仍按现有校验顺序报告已吊销，而不是已过期。
func TestDuplicateApplyRevokedAndExpiredReportsRevoked(t *testing.T) {
	w, c, in, first := setupReapplyReserved(t)

	// 先越过到期时刻，再吊销已到期的会话（吊销允许作用于已到期会话）。
	c.t = c.t.Add(reapplySession1ExpiryOffset + 5*time.Minute)
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	before := len(w.Ledger())

	_, err := w.Apply(in)
	if !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("err = %v, want ErrSessionRevoked to take precedence", err)
	}
	if errors.Is(err, ErrSessionExpired) {
		t.Fatalf("revoked+expired session must report revoked, got expired: %v", err)
	}

	assertReservedRequestUnchanged(t, w, first, Balances{Available: 900, Reserved: 100}, 100, 0)
	assertOnlyRejectionAppended(t, w, before, "r1", "revoked", c.t)
}

// TestDuplicateApplyValidReplacementSessionReturnsOriginal：同一使用账户换用
// 另一条绑定其他设备的有效会话、设备随之更改，其余内容一致时，重复提交
// 必须返回原请求：不因会话编号不同报内容冲突，首次受理信息不变，不再次
// 冻结费用、不追加账本记录。
func TestDuplicateApplyValidReplacementSessionReturnsOriginal(t *testing.T) {
	w, c, in, first := setupReapplyReserved(t)
	c.t = c.t.Add(10 * time.Minute)
	before := len(w.Ledger())

	replay := in
	replay.SessionID = "s1b"
	replay.DeviceID = "dev1b"
	got, err := w.Apply(replay)
	if err != nil {
		t.Fatalf("re-apply with a different valid session must return original: %v", err)
	}
	if got.RequestID != "r1" || got.State != RequestReserved {
		t.Fatalf("replay result = %+v, want original reserved request", got)
	}
	// 首次受理信息（提交时间、预留时刻、预估费用）保持不变。
	if !got.CreatedAt.Equal(first.CreatedAt) || !got.ReservedAt.Equal(first.ReservedAt) {
		t.Fatalf("acceptance info changed: created %v/%v reserved %v/%v",
			got.CreatedAt, first.CreatedAt, got.ReservedAt, first.ReservedAt)
	}
	if got.EstimatedFee != 100 {
		t.Fatalf("estimated fee = %d, want 100", got.EstimatedFee)
	}
	// 不再次冻结费用：余额与策略累计与首次受理后一致。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 900, Reserved: 100}) {
		t.Fatalf("balance = %+v, want {900 100}", bal)
	}
	if pv, _ := w.Policy("p1"); pv.ReservedTotal != 100 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 100/0", pv.ReservedTotal, pv.SpentTotal)
	}
	// 不追加任何账本记录（连零金额状态记录也没有）。
	if len(w.Ledger()) != before {
		t.Fatalf("ledger entries changed on valid idempotent re-apply: %d -> %d", before, len(w.Ledger()))
	}

	// 通过会话校验后若预估费用与原申请不同，仍返回原有冲突错误：不能覆盖
	// 原请求，也不能再次预留。
	conflict := replay
	conflict.EstimatedFee = 90
	if _, err := w.Apply(conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("different fee on valid session err = %v, want ErrConflict", err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 900, Reserved: 100}) {
		t.Fatalf("balance changed after conflicting re-apply: %+v", bal)
	}
	if pv, _ := w.Policy("p1"); pv.ReservedTotal != 100 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals changed after conflict: reserved %d spent %d", pv.ReservedTotal, pv.SpentTotal)
	}
	if len(w.Ledger()) != before {
		t.Fatalf("conflicting re-apply appended ledger entries: %d -> %d", before, len(w.Ledger()))
	}
	cur, _ := w.Request("u1", "r1")
	if cur.State != RequestReserved || cur.EstimatedFee != 100 || !cur.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("original request overwritten by conflicting re-apply: %+v", cur)
	}
}

// setupReapplySettled 在已预留场景上把 r1 按实际费用 60 结算，返回受理视图
// 与结算视图。结算后 payer 可用 940 / 预留 0，策略预留 0 / 已花费 60。
func setupReapplySettled(t *testing.T) (*Wallet, *clock, RequestInput, RequestView, RequestView) {
	t.Helper()
	w, c, in, first := setupReapplyReserved(t)
	c.t = c.t.Add(5 * time.Minute)
	settledAt := c.t
	settled, err := w.Settle("u1", "r1", 60)
	if err != nil {
		t.Fatal(err)
	}
	if !settled.SettledAt.Equal(settledAt) {
		t.Fatalf("setup: settled at %v, want %v", settled.SettledAt, settledAt)
	}
	return w, c, in, first, settled
}

// TestDuplicateApplySettledRequestSessionRules：已经结算的请求遵守同一会话
// 规则——失效会话重试仍被拒绝（只留零金额拒绝记录，结算结果与资金不变）；
// 有效替代会话重试返回原已结算结果，实际费用与结算时间保持不变；通过会话
// 校验后费用不同仍报冲突，不覆盖、不再次预留。
func TestDuplicateApplySettledRequestSessionRules(t *testing.T) {
	t.Run("revoked session rejected, settlement untouched", func(t *testing.T) {
		w, c, in, first, settled := setupReapplySettled(t)
		c.t = c.t.Add(10 * time.Minute)
		if err := w.RevokeSession("s1"); err != nil {
			t.Fatal(err)
		}
		ledgerBefore := len(w.Ledger())

		if _, err := w.Apply(in); !errors.Is(err, ErrSessionRevoked) {
			t.Fatalf("re-apply settled request on revoked session err = %v, want ErrSessionRevoked", err)
		}
		// 只新增一条零金额拒绝记录，资金账本（预留、扣减、差额退回）原样保留。
		all := w.Ledger()
		if len(all) != ledgerBefore+1 {
			t.Fatalf("ledger entries = %d, want one rejection appended", len(all))
		}
		rec, ok := latestRejection(all, "r1")
		if !ok || rec.AccountID != "u1" || rec.Amount != 0 || !strings.Contains(rec.Reason, "revoked") {
			t.Fatalf("rejection entry wrong: %+v ok=%v", rec, ok)
		}
		if n := countRequestEntries(all, LedgerReserve, "r1"); n != 1 {
			t.Fatalf("reserve entries = %d, want 1", n)
		}
		if n := countRequestEntries(all, LedgerSettle, "r1"); n != 1 {
			t.Fatalf("settle entries = %d, want the original single settlement", n)
		}
		if n := countRequestEntries(all, LedgerRefund, "r1"); n != 1 {
			t.Fatalf("refund entries = %d, want the original change refund", n)
		}
		// 结算结果不变：实际费用与结算时间保持，余额与策略累计不变。
		assertSettledRequestStays(t, w, first, settled)
	})

	t.Run("expired session rejected", func(t *testing.T) {
		w, c, in, first, settled := setupReapplySettled(t)
		c.t = c.t.Add(reapplySession1ExpiryOffset - 5*time.Minute) // 结算在 T+5m，此刻为会话到期时刻 T+1h
		ledgerBefore := len(w.Ledger())

		if _, err := w.Apply(in); !errors.Is(err, ErrSessionExpired) {
			t.Fatalf("re-apply settled request on expired session err = %v, want ErrSessionExpired", err)
		}
		if len(w.Ledger()) != ledgerBefore+1 {
			t.Fatalf("ledger entries = %d, want one rejection appended", len(w.Ledger()))
		}
		assertSettledRequestStays(t, w, first, settled)
	})

	t.Run("valid replacement session returns settled result", func(t *testing.T) {
		w, c, in, first, settled := setupReapplySettled(t)
		c.t = c.t.Add(10 * time.Minute)
		ledgerBefore := len(w.Ledger())

		replay := in
		replay.SessionID = "s1b"
		replay.DeviceID = "dev1b"
		got, err := w.Apply(replay)
		if err != nil {
			t.Fatalf("valid replacement session should return settled result: %v", err)
		}
		if got.State != RequestSettled || got.ActualFee != 60 || !got.SettledAt.Equal(settled.SettledAt) {
			t.Fatalf("replay result = %+v, want original settled result actual 60 at %v", got, settled.SettledAt)
		}
		if !got.CreatedAt.Equal(first.CreatedAt) {
			t.Fatalf("created at changed: %v, want %v", got.CreatedAt, first.CreatedAt)
		}
		// 不追加账本记录、不再次冻结或扣款。
		if len(w.Ledger()) != ledgerBefore {
			t.Fatalf("ledger entries changed on idempotent settled re-apply: %d -> %d", ledgerBefore, len(w.Ledger()))
		}
		if bal, _ := w.Balance("payer"); bal != (Balances{Available: 940, Reserved: 0}) {
			t.Fatalf("balance = %+v, want {940 0}", bal)
		}
		if pv, _ := w.Policy("p1"); pv.ReservedTotal != 0 || pv.SpentTotal != 60 {
			t.Fatalf("policy totals = reserved %d spent %d, want 0/60", pv.ReservedTotal, pv.SpentTotal)
		}

		// 通过会话校验后费用不同：仍是原有冲突错误，结算结果不被覆盖。
		conflict := replay
		conflict.EstimatedFee = 40
		if _, err := w.Apply(conflict); !errors.Is(err, ErrConflict) {
			t.Fatalf("different fee err = %v, want ErrConflict", err)
		}
		assertSettledRequestStays(t, w, first, settled)
		if len(w.Ledger()) != ledgerBefore {
			t.Fatalf("conflicting settled re-apply appended ledger entries: %d -> %d", ledgerBefore, len(w.Ledger()))
		}
	})
}

// assertSettledRequestStays 复核已结算请求的实际费用、结算时间与资金快照。
func assertSettledRequestStays(t *testing.T, w *Wallet, first, settled RequestView) {
	t.Helper()
	got, err := w.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestSettled || got.ActualFee != 60 || !got.SettledAt.Equal(settled.SettledAt) {
		t.Fatalf("settled result rewritten: %+v, want actual 60 at %v", got, settled.SettledAt)
	}
	if !got.CreatedAt.Equal(first.CreatedAt) || got.EstimatedFee != 100 {
		t.Fatalf("acceptance info changed: %+v", got)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 940, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {940 0}", bal)
	}
	if pv, _ := w.Policy("p1"); pv.ReservedTotal != 0 || pv.SpentTotal != 60 {
		t.Fatalf("policy totals = reserved %d spent %d, want 0/60", pv.ReservedTotal, pv.SpentTotal)
	}
}
