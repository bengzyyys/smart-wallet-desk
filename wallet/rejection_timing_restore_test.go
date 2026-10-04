package wallet

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// rejectionOrigin 标识备份中已拒绝请求的拒绝来源，三种来源共用同一条时限
// 规则，但保留各自的原因与决定账户区别。
type rejectionOrigin int

const (
	// rejectByPayer 出资账户在等待期限内主动拒绝：决定账户为出资账户。
	rejectByPayer rejectionOrigin = iota

	// rejectByRevocation 申请会话在等待期限内被吊销：不填写决定账户。
	rejectByRevocation

	// rejectByDeactivation 策略在等待期限内被停用：决定账户为出资账户，
	// 原因包含停用理由。
	rejectByDeactivation
)

// buildRejectedTimingBackup 构造一笔“待审批后被拒绝”的请求备份，所有计时
// 字段默认合法：提交于 t0、等待截止 t0+1h（策略窗口至 t0+24h、会话至
// t0+24h，均不抢先）、于 t0+30m 拒绝。被拒请求从未预留，不占用任何余额或
// 共享额度。mutate 在序列化前调整请求/策略/会话字段，以便构造各类不合法
// 历史。
func buildRejectedTimingBackup(t *testing.T, t0 time.Time, origin rejectionOrigin, mutate func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)) []byte {
	t.Helper()
	const fee int64 = 20
	r := requestBackupV1{
		PolicyID: "p", RequestID: "rj", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee: fee,
		State:       int(RequestRejected),
		CreatedAt:   timeJSON(t0),
		WaitDeadline: timeJSON(t0.Add(time.Hour)),
		DecidedAt:   timeJSON(t0.Add(30 * time.Minute)),
	}
	pol := policyBackupV1{
		ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "charge", Payee: "shop",
		StartsAt:          timeJSON(t0.Add(-time.Hour)),
		EndsAt:            timeJSON(t0.Add(24 * time.Hour)),
		MaxPerRequest:     100,
		MaxTotal:          1000,
		ApprovalThreshold: 10,
		ApprovalWait:      durationJSON(time.Hour),
		// 被拒请求从未预留；保留一个正的最长预留时长也不影响本核对。
		MaxReserveDuration: durationJSON(2 * time.Hour),
	}
	sess := sessionBackupV1{
		ID: "s1", AccountID: "u1", DeviceID: "d1",
		ExpiresAt: timeJSON(t0.Add(24 * time.Hour)), CreatedAt: timeJSON(t0.Add(-2 * time.Hour)),
	}
	ledger := []ledgerEntryBackupV1{{
		// 申请进入待审批的留痕。
		Kind: int(LedgerPendingApproval), AccountID: "u1", RequestID: "rj",
		Reason: "pending approval for large amount", At: timeJSON(t0),
	}}
	switch origin {
	case rejectByPayer:
		r.RejectReason = "no way"
		r.ApproverAccountID = "payer"
	case rejectByRevocation:
		r.RejectReason = "application session revoked before approval"
		sess.Revoked = true
	case rejectByDeactivation:
		r.RejectReason = "policy p deactivated by payer: stop it"
		r.ApproverAccountID = "payer"
		pol.Deactivated = true
		pol.DeactivatedAt = timeJSON(t0.Add(30 * time.Minute))
		pol.DeactivatorAccountID = "payer"
		pol.DeactivateReason = "stop it"
		ledger = append(ledger, ledgerEntryBackupV1{
			Kind: int(LedgerPolicyDeactivation), AccountID: "payer", PolicyID: "p",
			Reason: "stop it", At: timeJSON(t0.Add(30 * time.Minute)),
		})
	}
	ledger = append(ledger, ledgerEntryBackupV1{
		Kind: int(LedgerRejection), AccountID: "u1", RequestID: "rj",
		Reason: r.RejectReason, At: r.DecidedAt,
	})
	if mutate != nil {
		mutate(&r, &pol, &sess)
	}

	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0.Add(-time.Minute)),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: 100, Reserved: 0, CreatedAt: timeJSON(t0.Add(-2 * time.Hour))},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0.Add(-2 * time.Hour))},
		},
		Sessions: []sessionBackupV1{sess},
		Policies: []policyBackupV1{pol},
		Requests: []requestBackupV1{r},
		// 既有账本原样恢复：拒绝不产生资金变动。
		Ledger: ledger,
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// assertRejectionTimingRejected 恢复必须失败、包装 ErrBackupInvalid，并在错误
// 信息中点名使用账户与请求编号；不得返回部分恢复的钱包。
func assertRejectionTimingRejected(t *testing.T, data []byte, at time.Time) string {
	t.Helper()
	w2, err := restoreAt(data, at)
	if err == nil {
		if w2 != nil {
			t.Fatalf("invalid rejection history restored a wallet: %+v", w2)
		}
		t.Fatal("invalid rejection history restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, `"u1"`) || !strings.Contains(msg, `"rj"`) {
		t.Fatalf("error %q must name usage account and request id", msg)
	}
	return msg
}

// allRejectionOrins 返回三种拒绝来源，供统一适用的核对用例遍历。
func allRejectionOrigins() []struct {
	name   string
	origin rejectionOrigin
} {
	return []struct {
		name   string
		origin rejectionOrigin
	}{
		{"payer reject", rejectByPayer},
		{"session revocation", rejectByRevocation},
		{"policy deactivation", rejectByDeactivation},
	}
}

// TestRestoreRejectedRequestTimingAccepted 验证等待期限内完成的合法拒绝：
// 恰在提交时刻拒绝、截止前一纳秒拒绝都可接受；即使恢复时会话已到期或已被
// 吊销、策略已结束或已停用，仍保持已拒绝，提交时间、等待期限、决定时间、
// 原因、决定账户与既有账本原样保留，不追加过期记录或产生资金变动。
func TestRestoreRejectedRequestTimingAccepted(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	for _, oc := range allRejectionOrigins() {
		t.Run(oc.name+" rejected within window restores", func(t *testing.T) {
			data := buildRejectedTimingBackup(t, t0, oc.origin, nil)
			w2, err := restoreAt(data, t0.Add(2*time.Minute))
			if err != nil {
				t.Fatalf("legal rejected request: %v", err)
			}
			r, err := w2.Request("u1", "rj")
			if err != nil {
				t.Fatal(err)
			}
			if r.State != RequestRejected {
				t.Fatalf("state = %v, want rejected", r.State)
			}
		})

		t.Run(oc.name+" long after sessions and policies ended stays rejected", func(t *testing.T) {
			data := buildRejectedTimingBackup(t, t0, oc.origin, nil)
			w2, err := restoreAt(data, t0.Add(30*24*time.Hour))
			if err != nil {
				t.Fatalf("in-window rejection must restore long after windows end: %v", err)
			}
			r, _ := w2.Request("u1", "rj")
			if r.State != RequestRejected {
				t.Fatalf("state = %v, want rejected (not expired/rewritten)", r.State)
			}
			if !r.CreatedAt.Equal(t0) || !r.WaitDeadline.Equal(t0.Add(time.Hour)) {
				t.Fatalf("submit/wait timing rewritten: created %v deadline %v", r.CreatedAt, r.WaitDeadline)
			}
			if !r.DecidedAt.Equal(t0.Add(30 * time.Minute)) {
				t.Fatalf("decision time moved: %v", r.DecidedAt)
			}
			if r.RejectReason == "" {
				t.Fatal("reject reason lost")
			}
			if oc.origin == rejectByRevocation && r.ApproverAccountID != "" {
				t.Fatalf("revocation rejection must keep empty deciding account, got %q", r.ApproverAccountID)
			}
			if oc.origin != rejectByRevocation && r.ApproverAccountID != "payer" {
				t.Fatalf("deciding account = %q, want payer", r.ApproverAccountID)
			}
			// 不产生资金变动。
			if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
				t.Fatalf("balance = %+v, want unchanged {100 0}", bal)
			}
			// 既有账本原样保留，不追加过期记录。
			led := w2.Ledger()
			for _, e := range led {
				if e.Kind == LedgerExpiration || e.Kind == LedgerRefund || e.Kind == LedgerReserve {
					t.Fatalf("in-window rejection gained an entry on restore: %+v", e)
				}
			}
		})

		t.Run(oc.name+" rejected exactly at created_at restores", func(t *testing.T) {
			data := buildRejectedTimingBackup(t, t0, oc.origin, func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(t0)
				if p.Deactivated {
					// 停用与其拒绝同时发生。
					p.DeactivatedAt = timeJSON(t0)
				}
			})
			if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
				t.Fatalf("rejection exactly at created_at must be legal: %v", err)
			}
		})

		t.Run(oc.name+" rejected one nanosecond before deadline restores", func(t *testing.T) {
			data := buildRejectedTimingBackup(t, t0, oc.origin, func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(t0.Add(time.Hour - time.Nanosecond))
				if p.Deactivated {
					p.DeactivatedAt = r.DecidedAt
				}
			})
			if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
				t.Fatalf("rejection 1ns before deadline must be legal: %v", err)
			}
		})
	}

	t.Run("deadline bounded by policy end", func(t *testing.T) {
		data := buildRejectedTimingBackup(t, t0, rejectByPayer, func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
			p.EndsAt = timeJSON(t0.Add(30 * time.Minute))
			// 策略结束最先发生；拒绝必须严格早于它。
			r.WaitDeadline = timeJSON(t0.Add(30 * time.Minute))
			r.DecidedAt = timeJSON(t0.Add(15 * time.Minute))
		})
		if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
			t.Fatalf("policy-end bounded deadline: %v", err)
		}
	})

	t.Run("deadline bounded by session expiry", func(t *testing.T) {
		data := buildRejectedTimingBackup(t, t0, rejectByRevocation, func(r *requestBackupV1, _ *policyBackupV1, s *sessionBackupV1) {
			s.ExpiresAt = timeJSON(t0.Add(20 * time.Minute))
			r.WaitDeadline = timeJSON(t0.Add(20 * time.Minute))
			r.DecidedAt = timeJSON(t0.Add(10 * time.Minute))
		})
		if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
			t.Fatalf("session-expiry bounded deadline: %v", err)
		}
	})

	t.Run("same instant written in another timezone restores", func(t *testing.T) {
		loc := time.FixedZone("east", 5*60*60)
		decided := t0.Add(30 * time.Minute).In(loc)
		data := buildRejectedTimingBackup(t, t0, rejectByPayer, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			// 与 UTC 的 t0+30m 是同一瞬间，仅时区写法不同。
			r.DecidedAt = timeJSON(decided)
		})
		if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
			t.Fatalf("same instant in a different timezone must restore: %v", err)
		}
	})

	t.Run("dangling rejection ledger without request or session is kept", func(t *testing.T) {
		// 未被受理申请留下的独立拒绝账本记录：没有对应请求或会话，不属于
		// 本核对范围，必须按原有规则原样保留。
		data := buildRejectedTimingBackup(t, t0, rejectByPayer, nil)
		var b backupV1
		if err := json.Unmarshal(data, &b); err != nil {
			t.Fatal(err)
		}
		b.Ledger = append(b.Ledger, ledgerEntryBackupV1{
			Kind: int(LedgerRejection), AccountID: "ghost", RequestID: "never-accepted",
			Reason: "session not found", At: timeJSON(t0),
		})
		fixed, err := json.Marshal(&b)
		if err != nil {
			t.Fatal(err)
		}
		w2, err := restoreAt(fixed, t0.Add(48*time.Hour))
		if err != nil {
			t.Fatalf("dangling rejection history must not block restore: %v", err)
		}
		var found bool
		for _, e := range w2.Ledger() {
			if e.AccountID == "ghost" && e.RequestID == "never-accepted" {
				found = true
			}
		}
		if !found {
			t.Fatal("dangling rejection ledger entry was dropped on restore")
		}
	})
}

// TestRestoreRejectsRejectedTiming 验证三种拒绝来源统一适用时限核对：等待
// 截止缺失、被改长/改短、与策略结束或会话到期不符，或决定时刻早于提交、
// 等于/晚于截止时刻，都必须让整个备份被拒绝——即使余额与策略累计金额核对
// 一致，即使恢复时所有时间窗早就结束。
func TestRestoreRejectsRejectedTiming(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		restoreAt time.Time
		mutate    func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)
		reason    string
	}{
		{
			name:      "missing wait deadline",
			restoreAt: t0.Add(2 * time.Minute),
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.WaitDeadline = timeJSON(time.Time{})
			},
			reason: "missing wait deadline",
		},
		{
			name:      "lengthened wait deadline",
			restoreAt: t0.Add(2 * time.Minute),
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.WaitDeadline = timeJSON(t0.Add(2 * time.Hour))
			},
			reason: "does not match min",
		},
		{
			name:      "shortened wait deadline",
			restoreAt: t0.Add(2 * time.Minute),
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.WaitDeadline = timeJSON(t0.Add(15 * time.Minute))
			},
			reason: "does not match min",
		},
		{
			name:      "deadline ignores earlier policy end",
			restoreAt: t0.Add(2 * time.Minute),
			mutate: func(_ *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
				p.EndsAt = timeJSON(t0.Add(30 * time.Minute))
				// 正确截止应为 t0+30m，备份仍保存 t0+1h。
			},
			reason: "does not match min",
		},
		{
			name:      "deadline ignores earlier session expiry",
			restoreAt: t0.Add(2 * time.Minute),
			mutate: func(_ *requestBackupV1, _ *policyBackupV1, s *sessionBackupV1) {
				s.ExpiresAt = timeJSON(t0.Add(20 * time.Minute))
				// 正确截止应为 t0+20m，备份仍保存 t0+1h。
			},
			reason: "does not match min",
		},
		{
			name:      "decision exactly at deadline",
			restoreAt: t0.Add(2 * time.Minute),
			mutate: func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(t0.Add(time.Hour))
				if p.Deactivated {
					p.DeactivatedAt = r.DecidedAt
				}
			},
			reason: "strictly before wait deadline",
		},
		{
			name:      "decision one nanosecond after deadline",
			restoreAt: t0.Add(2 * time.Minute),
			mutate: func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(t0.Add(time.Hour + time.Nanosecond))
				if p.Deactivated {
					p.DeactivatedAt = r.DecidedAt
				}
			},
			reason: "strictly before wait deadline",
		},
		{
			name:      "decision long after deadline",
			restoreAt: t0.Add(2 * time.Minute),
			mutate: func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(t0.Add(24 * time.Hour))
				if p.Deactivated {
					p.DeactivatedAt = r.DecidedAt
				}
			},
			reason: "strictly before wait deadline",
		},
		{
			name:      "decision before created_at",
			restoreAt: t0.Add(2 * time.Minute),
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(t0.Add(-time.Nanosecond))
			},
			reason: "before created_at",
		},
		{
			name:      "invalid history rejected even long after restore time",
			restoreAt: t0.Add(30 * 24 * time.Hour),
			mutate: func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
				// 决定恰在截止时刻；恢复时所有期限早结束，损坏的拒绝历史仍须拒绝，
				// 不能当作过期或补造合法期限带回。
				r.DecidedAt = timeJSON(t0.Add(time.Hour))
				if p.Deactivated {
					p.DeactivatedAt = r.DecidedAt
				}
			},
			reason: "strictly before wait deadline",
		},
		{
			name:      "at-deadline decision in another timezone is the same instant",
			restoreAt: t0.Add(30 * 24 * time.Hour),
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				loc := time.FixedZone("east", 5*60*60)
				r.DecidedAt = timeJSON(t0.Add(time.Hour).In(loc))
			},
			reason: "strictly before wait deadline",
		},
	}

	for _, oc := range allRejectionOrigins() {
		for _, tc := range cases {
			t.Run(oc.name+" / "+tc.name, func(t *testing.T) {
				data := buildRejectedTimingBackup(t, t0, oc.origin, tc.mutate)
				msg := assertRejectionTimingRejected(t, data, tc.restoreAt)
				if !strings.Contains(msg, tc.reason) {
					t.Fatalf("error %q must explain %q", msg, tc.reason)
				}
			})
		}
	}
}

// TestRestoreRejectedTimingBalancesDoNotSalvage 验证余额与策略累计金额核对
// 一致不能让时间矛盾的备份通过：被拒请求本就不占用资金，账单完全自洽时
// 仍须整体拒绝。
func TestRestoreRejectedTimingBalancesDoNotSalvage(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, oc := range allRejectionOrigins() {
		data := buildRejectedTimingBackup(t, t0, oc.origin, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0.Add(time.Hour)) // 恰在截止时刻
		})
		assertRejectionTimingRejected(t, data, t0.Add(2*time.Minute))
	}
}

// TestRestoreRejectedVsExpiredBoundary 对照同一决定时刻在不同状态下的合法
// 性：截止时刻及之后的决定只能属于待审批过期终态（无拒绝原因、无决定
// 账户），把同样的时刻搭配到已拒绝状态必须拒绝——恢复不能替调用方把请求
// 改成过期。
func TestRestoreRejectedVsExpiredBoundary(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// 过期请求：决定时刻恰为截止时刻（惰性过期记录的是处理时刻），合法。
	data := buildRejectedTimingBackup(t, t0, rejectByPayer, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		r.State = int(RequestExpired)
		r.DecidedAt = timeJSON(t0.Add(time.Hour))
		r.RejectReason = ""
		r.ApproverAccountID = ""
	})
	if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
		t.Fatalf("expired request decided at deadline must restore: %v", err)
	}

	// 同一时刻、同一等待期限，但状态为已拒绝且带原因/决定账户：必须拒绝，
	// 不能恢复成过期或移动决定时间。
	data = buildRejectedTimingBackup(t, t0, rejectByPayer, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		r.DecidedAt = timeJSON(t0.Add(time.Hour))
	})
	msg := assertRejectionTimingRejected(t, data, t0.Add(48*time.Hour))
	if !strings.Contains(msg, "strictly before wait deadline") {
		t.Fatalf("error %q must point at the rejection timing contradiction", msg)
	}
}

// TestRestoreRejectsBackupWhenAnyRejectedRequestInvalid 验证同一备份中只要
// 有一笔已拒绝请求违反等待期限，整个恢复失败、不返回可用钱包，即使另一笔
// 请求完全合法。
func TestRestoreRejectsBackupWhenAnyRejectedRequestInvalid(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildRejectedTimingBackup(t, t0, rejectByPayer, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		r.DecidedAt = timeJSON(t0.Add(time.Hour)) // 非法：恰在截止时刻
	})
	var b backupV1
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	// 追加一笔期限内合法拒绝的请求（同策略、同使用账户、不同请求编号）。
	ok := b.Requests[0]
	ok.RequestID = "ok"
	ok.DecidedAt = timeJSON(t0.Add(15 * time.Minute))
	ok.RejectReason = "fine"
	for i := range b.Ledger {
		if b.Ledger[i].RequestID == "rj" {
			b.Ledger[i].RequestID = "rj"
		}
	}
	b.Requests = append(b.Requests, ok)
	corrupt, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	assertRejectionTimingRejected(t, corrupt, t0.Add(48*time.Hour))
}

// setupRejectFlowWallet 构造一个开启审批（门槛 10、等待 1 小时）的钱包：
// 出资账户 payer 带审批会话 sa/deva，使用账户 u1 带申请会话 s1/dev1；
// 返回钱包、时钟与提交一笔费用 20 的待审批申请的方法。
func setupRejectFlowWallet(t *testing.T) (*Wallet, *clock) {
	t.Helper()
	w, c := newTestWallet()
	t0 := c.t
	mustAccount(t, w, "payer", 1000)
	mustAccount(t, w, "u1", 0)
	if _, err := w.CreateSession("sa", "payer", "deva", t0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s1", "u1", "dev1", t0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(PolicySpec{
		ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "charge", Payee: "shop",
		StartsAt: t0.Add(-time.Hour), EndsAt: t0.Add(24 * time.Hour),
		MaxPerRequest: 100, MaxTotal: 1000,
		ApprovalThreshold: 10, ApprovalWait: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	in := RequestInput{
		PolicyID: "p", RequestID: "rj", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 20,
	}
	if v, err := w.Apply(in); err != nil || v.State != RequestPendingApproval {
		t.Fatalf("apply: v=%+v err=%v, want pending", v, err)
	}
	return w, c
}

// TestRestoreLegalRejectionRoundTrips 端到端验证三种来源在真实公开入口下
// 产生的期限内拒绝：导出后于很久以后恢复，已拒绝状态、全部计时信息、原因、
// 决定账户与账本完整保留，不追加过期记录、不产生资金变动。
func TestRestoreLegalRejectionRoundTrips(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("payer reject one nanosecond before deadline", func(t *testing.T) {
		w, c := setupRejectFlowWallet(t)
		c.t = t0.Add(time.Hour - time.Nanosecond)
		if _, err := w.Reject("u1", "rj", "sa", "deva", "no way"); err != nil {
			t.Fatal(err)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		w2, err := restoreAt(data, t0.Add(30*24*time.Hour))
		if err != nil {
			t.Fatalf("legal in-window rejection must restore long after: %v", err)
		}
		r, _ := w2.Request("u1", "rj")
		if r.State != RequestRejected || r.ApproverAccountID != "payer" || r.RejectReason != "no way" {
			t.Fatalf("restored rejection = %+v", r)
		}
		if !r.DecidedAt.Equal(t0.Add(time.Hour-time.Nanosecond)) || !r.WaitDeadline.Equal(t0.Add(time.Hour)) {
			t.Fatalf("timing rewritten: decided %v deadline %v", r.DecidedAt, r.WaitDeadline)
		}
		if l1, l2 := len(w.Ledger()), len(w2.Ledger()); l1 != l2 {
			t.Fatalf("ledger entries added on restore: source %d restored %d", l1, l2)
		}
		if n := countKind(w2.Ledger(), LedgerExpiration); n != 0 {
			t.Fatalf("rejected request gained expiration entries on restore: %d", n)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 1000, Reserved: 0}) {
			t.Fatalf("balance = %+v, want unchanged", bal)
		}
	})

	t.Run("session revocation within window", func(t *testing.T) {
		w, c := setupRejectFlowWallet(t)
		c.t = t0.Add(30 * time.Minute)
		if err := w.RevokeSession("s1"); err != nil {
			t.Fatal(err)
		}
		before, _ := w.Request("u1", "rj")
		if before.State != RequestRejected {
			t.Fatalf("setup state = %v, want rejected", before.State)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		w2, err := restoreAt(data, t0.Add(30*24*time.Hour))
		if err != nil {
			t.Fatalf("revocation rejection must restore long after: %v", err)
		}
		r, _ := w2.Request("u1", "rj")
		if r.State != RequestRejected || r.ApproverAccountID != "" {
			t.Fatalf("restored revocation rejection = %+v", r)
		}
		if !r.DecidedAt.Equal(before.DecidedAt) || !r.WaitDeadline.Equal(before.WaitDeadline) {
			t.Fatalf("timing changed: decided %v/%v deadline %v/%v",
				r.DecidedAt, before.DecidedAt, r.WaitDeadline, before.WaitDeadline)
		}
		if len(w2.Ledger()) != len(w.Ledger()) {
			t.Fatal("ledger entries added on restore")
		}
	})

	t.Run("policy deactivation within window", func(t *testing.T) {
		w, c := setupRejectFlowWallet(t)
		c.t = t0.Add(30 * time.Minute)
		if _, err := w.DeactivatePolicy("p", "sa", "deva", "stop it"); err != nil {
			t.Fatal(err)
		}
		before, _ := w.Request("u1", "rj")
		if before.State != RequestRejected || before.ApproverAccountID != "payer" {
			t.Fatalf("setup = %+v, want payer-attributed rejection", before)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		w2, err := restoreAt(data, t0.Add(30*24*time.Hour))
		if err != nil {
			t.Fatalf("deactivation rejection must restore long after: %v", err)
		}
		r, _ := w2.Request("u1", "rj")
		if r.State != RequestRejected || r.ApproverAccountID != "payer" ||
			r.RejectReason != fmt.Sprintf("policy p deactivated by payer: %s", "stop it") {
			t.Fatalf("restored deactivation rejection = %+v", r)
		}
		pv, _ := w2.Policy("p")
		if !pv.Deactivated || pv.DeactivateReason != "stop it" {
			t.Fatalf("deactivation info lost: %+v", pv)
		}
		if len(w2.Ledger()) != len(w.Ledger()) {
			t.Fatal("ledger entries added on restore")
		}
	})
}

// TestRestoreRejectAtDeadlineProducesExpiredNotRejection 对照正常公开入口：
// 恰在等待截止时刻处理（拒绝/吊销/停用）只能得到待审批过期终态，因此正常
// 操作根本不可能产生“截止当时拒绝”的备份；恢复侧拒绝该类历史与此一致。
func TestRestoreRejectAtDeadlineProducesExpiredNotRejection(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("reject exactly at deadline expires the request", func(t *testing.T) {
		w, c := setupRejectFlowWallet(t)
		c.t = t0.Add(time.Hour) // 恰在截止时刻
		if _, err := w.Reject("u1", "rj", "sa", "deva", "no way"); !errors.Is(err, ErrRequestNotPending) {
			t.Fatalf("reject at deadline err = %v, want ErrRequestNotPending", err)
		}
		r, _ := w.Request("u1", "rj")
		if r.State != RequestExpired || r.RejectReason != "" || r.ApproverAccountID != "" {
			t.Fatalf("at-deadline handling = %+v, want expired without rejection info", r)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		w2, err := restoreAt(data, t0.Add(48*time.Hour))
		if err != nil {
			t.Fatalf("naturally expired request must restore: %v", err)
		}
		got, _ := w2.Request("u1", "rj")
		if got.State != RequestExpired {
			t.Fatalf("state = %v, want expired", got.State)
		}
	})

	t.Run("revoke exactly at deadline expires the request", func(t *testing.T) {
		w, c := setupRejectFlowWallet(t)
		c.t = t0.Add(time.Hour)
		if err := w.RevokeSession("s1"); err != nil {
			t.Fatal(err)
		}
		r, _ := w.Request("u1", "rj")
		if r.State != RequestExpired || r.RejectReason != "" {
			t.Fatalf("at-deadline revoke = %+v, want expired without rejection reason", r)
		}
	})
}
