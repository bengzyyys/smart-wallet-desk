package wallet

import (
	"errors"
	"testing"
	"time"
)

// 本文件为“待审批请求换用有效会话重试”补充自动化回归保障：
// 首次申请进入待审批后，原申请会话在等待期限内到期，使用账户换用另一条
// 绑定不同设备、有效期更长的会话，按原请求编号与原申请内容重复提交时，
// 会话有效只表示调用方有权取回原请求——既不能重新受理申请（不产生第二条
// 待审批记录、不改写提交时间），也不能把首次提交时按“等待时长、策略结束
// 时间、申请会话到期时间三者最早值”确定的等待截止时间向后推（不能改用新
// 会话的到期时间，也不能从重试时刻重新计算等待时长）。
//
// 原截止时间之前的重试一律取回原待审批结果；恰到或晚于原截止时间后，即使
// 新会话与策略都仍有效，也只能取回“等待审批过期”终态：只追加一条零金额
// 过期记录，不产生预留、扣减或退款，此后出资账户批准只得到审批已过期错误。
//
// 场景参数固定为：出资账户 payer 余额 100，审批门槛 10，预估费用 20，
// 单次限额 100、共享累计 1000、策略时间窗均足够；替代会话 s2 同属使用
// 账户 u1、绑定另一台设备 dev2，在基刻后 20 分钟才到期。两种“最先到期”
// 条件各覆盖一次：申请会话先到期（截止由会话到期决定）与最长等待时长先
// 到期（截止由等待时长决定）。

// pendingReapplyRegime 描述一组时间安排：等待时长与两条会话的到期偏移；
// wantDeadlineOffset 为首次受理时应确定的等待截止偏移。
type pendingReapplyRegime struct {
	name              string
	approvalWait      time.Duration
	firstExpiryOffset time.Duration // 首次申请会话 s1 的到期偏移
	wantDeadline      time.Duration // 期望等待截止 = 基刻 + wantDeadline
}

func pendingReapplyRegimes() []pendingReapplyRegime {
	return []pendingReapplyRegime{
		{
			name:              "application session expires before approval wait",
			approvalWait:      10 * time.Minute,
			firstExpiryOffset: 5 * time.Minute,
			wantDeadline:      5 * time.Minute,
		},
		{
			name:              "approval wait expires before application session",
			approvalWait:      3 * time.Minute,
			firstExpiryOffset: 10 * time.Minute,
			wantDeadline:      3 * time.Minute,
		},
	}
}

// pendingReapplyFixture 持有构造好的钱包与首次受理结果。
type pendingReapplyFixture struct {
	w        *Wallet
	c        *clock
	in       RequestInput // 首次申请内容（使用 s1/dev1）
	first    RequestView  // 首次受理返回的待审批视图
	deadline time.Time    // 首次受理确定的等待截止时刻
}

// setupPendingReapply 按给定时间安排构造场景：payer 余额 100、u1 为使用
// 账户；s1 绑定 dev1（按 regime 到期），s2 同属 u1 但绑定 dev2（基刻后
// 20 分钟到期），sa 为出资账户绑定 dev-approve 的审批会话（基刻后 1 小时
// 到期）；策略 p1 门槛 10、等待时长按 regime，窗口与限额都远大于本场景。
// 在基刻以 s1 提交费用 20 的 r1，断言其进入待审批且截止时间符合预期。
func setupPendingReapply(t *testing.T, regime pendingReapplyRegime) pendingReapplyFixture {
	t.Helper()
	w, c := newTestWallet()
	base := c.t
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", base.Add(regime.firstExpiryOffset)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s2", "u1", "dev2", base.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "dev-approve", base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	p := PolicySpec{
		ID:                "p1",
		PayerAccountID:    "payer",
		AllowedAccountIDs: []string{"u1"},
		Operation:         "charge",
		Payee:             "shop",
		StartsAt:          base.Add(-time.Hour),
		EndsAt:            base.Add(time.Hour),
		MaxPerRequest:     100,
		MaxTotal:          1000,
		ApprovalThreshold: 10,
		ApprovalWait:      regime.approvalWait,
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
		EstimatedFee: 20,
	}
	first, err := w.Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != RequestPendingApproval {
		t.Fatalf("setup: first state = %v, want pending approval", first.State)
	}
	deadline := base.Add(regime.wantDeadline)
	if !first.WaitDeadline.Equal(deadline) {
		t.Fatalf("setup: wait deadline = %v, want %v", first.WaitDeadline, deadline)
	}
	return pendingReapplyFixture{w: w, c: c, in: in, first: first, deadline: deadline}
}

// replayWithReplacementSession 把原申请改为使用替代会话 s2/dev2，其余内容
// （使用账户、请求编号、策略、操作类型、收款方、预估费用）保持完全一致。
func replayWithReplacementSession(in RequestInput) RequestInput {
	replay := in
	replay.SessionID = "s2"
	replay.DeviceID = "dev2"
	return replay
}

// assertPendingMoneyUntouched 断言待审批从未冻结费用：payer 可用 100、
// 预留 0，策略预留总额与已花费总额均为 0。
func assertPendingMoneyUntouched(t *testing.T, w *Wallet) {
	t.Helper()
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("payer balance = %+v, want untouched {100 0}", bal)
	}
	if pv, _ := w.Policy("p1"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}
}

// assertPendingViewUnchanged 断言取回的仍是首次受理的待审批请求：状态、
// 提交时间与等待截止时间保持首次受理的值，且没有任何预留或审批决定信息。
func assertPendingViewUnchanged(t *testing.T, got RequestView, f pendingReapplyFixture, at time.Time) {
	t.Helper()
	if got.State != RequestPendingApproval {
		t.Fatalf("at %v: state = %v, want still pending approval (must not be re-accepted)", at, got.State)
	}
	if got.RequestID != "r1" || got.AccountID != "u1" || got.PolicyID != "p1" ||
		got.Operation != "charge" || got.Payee != "shop" || got.EstimatedFee != 20 {
		t.Fatalf("at %v: request content changed: %+v", at, got)
	}
	if !got.CreatedAt.Equal(f.first.CreatedAt) {
		t.Fatalf("at %v: submission time rewritten to %v, want original %v", at, got.CreatedAt, f.first.CreatedAt)
	}
	if !got.WaitDeadline.Equal(f.deadline) {
		t.Fatalf("at %v: wait deadline moved to %v, want original %v (new session must not extend it)", at, got.WaitDeadline, f.deadline)
	}
	if !got.ReservedAt.IsZero() || got.ReserveDuration != 0 || !got.ReserveDeadline.IsZero() {
		t.Fatalf("at %v: pending retry must not reserve fees: %+v", at, got)
	}
	if got.ActualFee != 0 || got.ApproverAccountID != "" || got.RejectReason != "" || !got.DecidedAt.IsZero() {
		t.Fatalf("at %v: pending retry carries settlement/decision fields: %+v", at, got)
	}
}

// TestPendingReapplyValidSessionBeforeDeadlineReturnsOriginal：原等待截止
// 时间之前，换用同账户、另一设备的有效会话按原编号、原内容重复提交，只能
// 取回原待审批请求：新会话/设备不算内容冲突，提交时间与等待截止保持首次
// 受理的值，不重新受理、不重新计时，不追加任何账本记录，资金与额度不变。
func TestPendingReapplyValidSessionBeforeDeadlineReturnsOriginal(t *testing.T) {
	for _, regime := range pendingReapplyRegimes() {
		t.Run(regime.name, func(t *testing.T) {
			f := setupPendingReapply(t, regime)
			w, c := f.w, f.c
			ledgerAfterFirst := len(w.Ledger())
			if ledgerAfterFirst != 1 {
				t.Fatalf("ledger after first apply = %d entries, want exactly one pending record", ledgerAfterFirst)
			}

			// 提交后 2 分钟换用 s2/dev2 重试（此时 s2 与策略都有效）。
			c.t = c.t.Add(2 * time.Minute)
			got, err := w.Apply(replayWithReplacementSession(f.in))
			if err != nil {
				t.Fatalf("re-apply with a different valid session before deadline must return original: %v", err)
			}
			assertPendingViewUnchanged(t, got, f, c.t)
			assertPendingMoneyUntouched(t, w)
			// 成功重试不增加账本记录：首次待审批记录保留且只有一条。
			if len(w.Ledger()) != ledgerAfterFirst {
				t.Fatalf("ledger entries changed on valid retry: %d -> %d", ledgerAfterFirst, len(w.Ledger()))
			}
			if n := countRequestEntries(w.Ledger(), LedgerPendingApproval, "r1"); n != 1 {
				t.Fatalf("pending-approval entries = %d, want exactly the original one", n)
			}

			// 会话/设备更换不算冲突，但通过会话校验后费用不同仍返回冲突，
			// 且不覆盖原请求、不追加记录。
			conflict := replayWithReplacementSession(f.in)
			conflict.EstimatedFee = 21
			if _, err := w.Apply(conflict); !errors.Is(err, ErrConflict) {
				t.Fatalf("different fee on valid session err = %v, want ErrConflict", err)
			}
			if len(w.Ledger()) != ledgerAfterFirst {
				t.Fatalf("conflicting retry appended ledger entries: %d -> %d", ledgerAfterFirst, len(w.Ledger()))
			}

			// 原截止前 1 分钟继续用有效新会话重复提交：仍取回原待审批结果，
			// 截止时间不能被推到“重试时刻 + 等待时长”或 s2 的到期时间。
			c.t = f.deadline.Add(-time.Minute)
			again, err := w.Apply(replayWithReplacementSession(f.in))
			if err != nil {
				t.Fatalf("second valid retry before deadline: %v", err)
			}
			assertPendingViewUnchanged(t, again, f, c.t)
			cur, err := w.Request("u1", "r1")
			if err != nil {
				t.Fatal(err)
			}
			assertPendingViewUnchanged(t, cur, f, c.t)
			assertPendingMoneyUntouched(t, w)
			if len(w.Ledger()) != 1 || countRequestEntries(w.Ledger(), LedgerPendingApproval, "r1") != 1 {
				t.Fatalf("ledger before deadline = %+v, want only the original pending record", w.Ledger())
			}
		})
	}
}

// TestPendingReapplyAfterOriginalDeadlineStaysExpired：恰到原等待截止时间
// 或更晚时，即使新会话与策略都仍有效，相同内容的重复申请也只能返回等待
// 审批过期的终态：不能再次进入待审批、不能改用新会话到期时间、不能从重
// 试时刻重新计算等待时长。过期请求保留原提交时间与原等待截止时间，只追加
// 一次零金额过期记录；此后出资账户批准只得到审批已过期错误，资金与额度不变。
func TestPendingReapplyAfterOriginalDeadlineStaysExpired(t *testing.T) {
	for _, regime := range pendingReapplyRegimes() {
		t.Run(regime.name, func(t *testing.T) {
			f := setupPendingReapply(t, regime)
			w, c := f.w, f.c

			// 截止前先换用有效新会话取回一次原待审批请求。
			c.t = c.t.Add(2 * time.Minute)
			if got, err := w.Apply(replayWithReplacementSession(f.in)); err != nil || got.State != RequestPendingApproval {
				t.Fatalf("pre-deadline retry = %+v err %v, want original pending", got, err)
			}

			// 推进到原等待截止时刻（含恰到）：s1 在“会话先到期”安排中此刻
			// 恰好到期，但 s2 与策略都仍有效。
			c.t = f.deadline
			s2, err := w.Session("s2")
			if err != nil {
				t.Fatal(err)
			}
			if s2.State != SessionActive || !c.t.Before(s2.ExpiresAt) {
				t.Fatalf("replacement session must still be valid at deadline: %+v", s2)
			}
			atDeadline, err := w.Apply(replayWithReplacementSession(f.in))
			if err != nil {
				t.Fatalf("re-apply exactly at deadline must return the expired terminal view: %v", err)
			}
			if atDeadline.State != RequestExpired {
				t.Fatalf("state at deadline = %v, want request expired (must not re-enter pending)", atDeadline.State)
			}
			if !atDeadline.CreatedAt.Equal(f.first.CreatedAt) {
				t.Fatalf("submission time changed: %v, want original %v", atDeadline.CreatedAt, f.first.CreatedAt)
			}
			if !atDeadline.WaitDeadline.Equal(f.deadline) {
				t.Fatalf("deadline changed: %v, want original %v", atDeadline.WaitDeadline, f.deadline)
			}
			if !atDeadline.DecidedAt.Equal(f.deadline) {
				t.Fatalf("expiration decided at %v, want %v", atDeadline.DecidedAt, f.deadline)
			}
			if atDeadline.ApproverAccountID != "" || atDeadline.RejectReason != "" {
				t.Fatalf("expiration must carry no approver or reject reason: %+v", atDeadline)
			}
			if !atDeadline.ReservedAt.IsZero() || atDeadline.ActualFee != 0 {
				t.Fatalf("expired request must never have reserved fees: %+v", atDeadline)
			}
			assertPendingMoneyUntouched(t, w)
			// 恰到截止时间的重试只追加一次关联 u1/r1 的零金额过期记录。
			if n := countRequestEntries(w.Ledger(), LedgerExpiration, "r1"); n != 1 {
				t.Fatalf("expiration entries = %d, want exactly one", n)
			}
			expRec, ok := ledgerEntryFor(w.Ledger(), LedgerExpiration, "r1")
			if !ok || expRec.AccountID != "u1" || expRec.Amount != 0 || expRec.PolicyID != "" || !expRec.At.Equal(f.deadline) {
				t.Fatalf("expiration ledger entry wrong: %+v ok=%v", expRec, ok)
			}

			// 更晚时刻（s2 仍有效、策略窗口仍有效）再次以相同内容重复申请：
			// 仍是同一过期终态，不新增第二条过期记录或任何资金记录。
			c.t = f.deadline.Add(3 * time.Minute)
			if got, err := w.Apply(replayWithReplacementSession(f.in)); err != nil || got.State != RequestExpired {
				t.Fatalf("re-apply after deadline = %+v err %v, want the same expired view", got, err)
			} else {
				if !got.CreatedAt.Equal(f.first.CreatedAt) || !got.WaitDeadline.Equal(f.deadline) {
					t.Fatalf("expired request times changed on later retry: %+v", got)
				}
			}
			all := w.Ledger()
			if n := countRequestEntries(all, LedgerExpiration, "r1"); n != 1 {
				t.Fatalf("expiration entries after later retry = %d, want still exactly one", n)
			}
			if n := countRequestEntries(all, LedgerPendingApproval, "r1"); n != 1 {
				t.Fatalf("pending entries = %d, want only the original first-acceptance record", n)
			}
			for _, kind := range []LedgerKind{LedgerReserve, LedgerSettle, LedgerRefund, LedgerRejection, LedgerApproval, LedgerCancellation} {
				if n := countRequestEntries(all, kind, "r1"); n != 0 {
					t.Fatalf("expired retry produced %d entries of kind %v, want none", n, kind)
				}
			}

			// 出资账户用自身有效会话批准：得到现有的审批已过期错误，
			// 请求保持过期终态，资金与额度不变。
			if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); !errors.Is(err, ErrApprovalExpired) {
				t.Fatalf("approve after deadline err = %v, want ErrApprovalExpired", err)
			}
			cur, err := w.Request("u1", "r1")
			if err != nil {
				t.Fatal(err)
			}
			if cur.State != RequestExpired || !cur.CreatedAt.Equal(f.first.CreatedAt) || !cur.WaitDeadline.Equal(f.deadline) {
				t.Fatalf("approve attempt changed the expired request: %+v", cur)
			}
			assertPendingMoneyUntouched(t, w)
			// 终态账本只剩首次待审批记录与一次零金额过期记录；出资账户无任何
			// 资金变动记录。
			if len(w.Ledger()) != 2 {
				t.Fatalf("final ledger = %d entries, want 2 (pending + expiration)", len(w.Ledger()))
			}
			if len(w.AccountLedger("payer")) != 0 {
				t.Fatalf("payer ledger = %+v, want no money-moving entries", w.AccountLedger("payer"))
			}
		})
	}
}
