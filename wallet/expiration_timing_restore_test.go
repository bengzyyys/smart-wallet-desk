package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildExpiredPendingTimingBackup 构造一笔“超门槛待审批、从未预留费用、因等待
// 审批到期进入已过期终态”的请求备份，所有计时字段默认合法：提交于 t0、等待
// 截止 t0+1h（策略窗口至 t0+24h、会话至 t0+24h，均不抢先）、于 t0+2h 才发现
// 并记录过期决定。mutate 在余额核对前调整请求/策略/会话的计时字段，以便构造
// 各类不合法历史。
func buildExpiredPendingTimingBackup(t *testing.T, t0 time.Time, mutate func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)) []byte {
	t.Helper()
	const fee int64 = 20
	r := requestBackupV1{
		PolicyID: "p", RequestID: "big", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee: fee,
		State:        int(RequestExpired),
		CreatedAt:    timeJSON(t0),
		WaitDeadline: timeJSON(t0.Add(time.Hour)),
		DecidedAt:    timeJSON(t0.Add(2 * time.Hour)),
	}
	pol := policyBackupV1{
		ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "charge", Payee: "shop",
		StartsAt:           timeJSON(t0.Add(-time.Hour)),
		EndsAt:             timeJSON(t0.Add(24 * time.Hour)),
		MaxPerRequest:      100,
		MaxTotal:           1000,
		ApprovalThreshold:  10,
		ApprovalWait:       durationJSON(time.Hour),
		MaxReserveDuration: durationJSON(2 * time.Hour),
	}
	sess := sessionBackupV1{
		ID: "s1", AccountID: "u1", DeviceID: "d1",
		ExpiresAt: timeJSON(t0.Add(24 * time.Hour)), CreatedAt: timeJSON(t0.Add(-2 * time.Hour)),
	}
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
		Ledger: []ledgerEntryBackupV1{
			{Kind: int(LedgerPendingApproval), AccountID: "u1", RequestID: "big", At: timeJSON(t0)},
			{Kind: int(LedgerExpiration), AccountID: "u1", RequestID: "big", Reason: "approval period expired", At: timeJSON(t0.Add(2 * time.Hour))},
		},
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// assertExpirationTimingRejected 恢复必须失败、包装 ErrBackupInvalid，并在
// 错误信息中点名使用账户与请求编号；不得返回部分恢复的钱包。
func assertExpirationTimingRejected(t *testing.T, data []byte, at time.Time) string {
	t.Helper()
	w2, err := restoreAt(data, at)
	if err == nil {
		if w2 != nil {
			t.Fatalf("invalid expiration history restored a wallet: %+v", w2)
		}
		t.Fatal("invalid expiration history restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, `"u1"`) || !strings.Contains(msg, `"big"`) {
		t.Fatalf("error %q must name usage account and request id", msg)
	}
	return msg
}

// TestRestoreExpiredPendingTimingAccepted 验证合法的待审批过期历史照常恢复：
// 决定恰在截止时刻、或到期很久后才发现过期均可；即使恢复时会话已到期或被
// 吊销、策略已结束或停用，仍保持原过期状态与全部时刻，不追加过期记录、不
// 产生资金变动。
func TestRestoreExpiredPendingTimingAccepted(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("expired restores long after windows end", func(t *testing.T) {
		data := buildExpiredPendingTimingBackup(t, t0, nil)
		w2, err := restoreAt(data, t0.Add(48*time.Hour))
		if err != nil {
			t.Fatalf("legal expiration must restore: %v", err)
		}
		r, err := w2.Request("u1", "big")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestExpired {
			t.Fatalf("state = %v, want expired", r.State)
		}
		// 时刻原样保留：决定时刻不得被改写为截止时刻或恢复时的当前时间。
		if !r.CreatedAt.Equal(t0) || !r.WaitDeadline.Equal(t0.Add(time.Hour)) ||
			!r.DecidedAt.Equal(t0.Add(2*time.Hour)) {
			t.Fatalf("timing rewritten: created %v deadline %v decided %v",
				r.CreatedAt, r.WaitDeadline, r.DecidedAt)
		}
		if r.ApproverAccountID != "" || r.RejectReason != "" {
			t.Fatalf("expired request must not carry approver/reason: %+v", r)
		}
		// 终态不改写：不追加任何记录，余额与额度不变。
		if got := len(w2.Ledger()); got != 2 {
			t.Fatalf("restore appended ledger entries: got %d entries, want 2", got)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
			t.Fatalf("payer balance = %+v, want untouched", bal)
		}
	})

	t.Run("decided exactly at deadline restores", func(t *testing.T) {
		data := buildExpiredPendingTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0.Add(time.Hour))
		})
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("expiration exactly at deadline must be legal: %v", err)
		}
	})

	t.Run("decided one nanosecond after deadline restores", func(t *testing.T) {
		data := buildExpiredPendingTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0.Add(time.Hour + time.Nanosecond))
		})
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("expiration 1ns after deadline must be legal: %v", err)
		}
	})

	t.Run("decided long after deadline restores", func(t *testing.T) {
		// 钱包可能在到期很久后才发现请求过期：决定时刻远晚于截止时刻合法。
		data := buildExpiredPendingTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0.Add(30 * 24 * time.Hour))
		})
		w2, err := restoreAt(data, t0.Add(60*24*time.Hour))
		if err != nil {
			t.Fatalf("late-discovered expiration must restore: %v", err)
		}
		r, err := w2.Request("u1", "big")
		if err != nil {
			t.Fatal(err)
		}
		if !r.DecidedAt.Equal(t0.Add(30 * 24 * time.Hour)) {
			t.Fatalf("decided_at rewritten to %v", r.DecidedAt)
		}
	})

	t.Run("deadline bounded by policy end", func(t *testing.T) {
		data := buildExpiredPendingTimingBackup(t, t0, func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
			p.EndsAt = timeJSON(t0.Add(30 * time.Minute))
			r.WaitDeadline = timeJSON(t0.Add(30 * time.Minute))
			r.DecidedAt = timeJSON(t0.Add(40 * time.Minute))
		})
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("policy-end bounded deadline: %v", err)
		}
	})

	t.Run("deadline bounded by session expiry", func(t *testing.T) {
		// 任务示例：最长等待允许到 12:10，申请会话 12:05 到期，截止应为
		// 12:05；12:08 才查询并记录过期决定的历史可以恢复。
		data := buildExpiredPendingTimingBackup(t, t0, func(r *requestBackupV1, p *policyBackupV1, s *sessionBackupV1) {
			p.ApprovalWait = durationJSON(10 * time.Minute)
			s.ExpiresAt = timeJSON(t0.Add(5 * time.Minute))
			r.WaitDeadline = timeJSON(t0.Add(5 * time.Minute))
			r.DecidedAt = timeJSON(t0.Add(8 * time.Minute))
		})
		w2, err := restoreAt(data, t0.Add(48*time.Hour))
		if err != nil {
			t.Fatalf("session-expiry bounded deadline: %v", err)
		}
		r, err := w2.Request("u1", "big")
		if err != nil {
			t.Fatal(err)
		}
		if !r.WaitDeadline.Equal(t0.Add(5*time.Minute)) || !r.DecidedAt.Equal(t0.Add(8*time.Minute)) {
			t.Fatalf("timing rewritten: deadline %v decided %v", r.WaitDeadline, r.DecidedAt)
		}
	})

	t.Run("session revoked and policy deactivated at restore", func(t *testing.T) {
		data := buildExpiredPendingTimingBackup(t, t0, func(r *requestBackupV1, p *policyBackupV1, s *sessionBackupV1) {
			s.Revoked = true
			p.Deactivated = true
			p.DeactivatedAt = timeJSON(t0.Add(3 * time.Hour))
			p.DeactivatorAccountID = "payer"
			p.DeactivateReason = "no longer needed"
		})
		if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
			t.Fatalf("legal expiration must restore despite revoked session and deactivated policy: %v", err)
		}
	})

	t.Run("timezone representation does not change outcome", func(t *testing.T) {
		// 同一时刻的不同时区写法：t0+1h UTC 即该瞬间在 +02:00 的 14:00。
		data := buildExpiredPendingTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.WaitDeadline = timeJSON(t0.Add(time.Hour).In(time.FixedZone("UTC+2", 2*3600)))
			r.DecidedAt = timeJSON(t0.Add(2 * time.Hour).In(time.FixedZone("UTC-5", -5*3600)))
		})
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("same instant in another zone must restore: %v", err)
		}
	})
}

// TestRestoreRejectsExpiredPendingTiming 验证待审批过期的请求：等待截止缺失、
// 被改长/改短（含只按最长等待时长计算而忽略更早的策略结束或会话到期），或
// 过期决定早于截止时刻，都必须让整个备份被拒绝——余额与累计金额自洽也不能
// 放行。
func TestRestoreRejectsExpiredPendingTiming(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		mutate func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)
		reason string
	}{
		{
			name: "missing wait deadline",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.WaitDeadline = timeJSON(time.Time{})
			},
			reason: "missing wait deadline",
		},
		{
			name: "lengthened wait deadline",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.WaitDeadline = timeJSON(t0.Add(2 * time.Hour))
			},
			reason: "does not match min",
		},
		{
			name: "shortened wait deadline",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.WaitDeadline = timeJSON(t0.Add(15 * time.Minute))
			},
			reason: "does not match min",
		},
		{
			name: "deadline moved but still before decision",
			mutate: func(r *requestBackupV1, p *policyBackupV1, s *sessionBackupV1) {
				// 任务示例：最长等待允许到 12:10、会话 12:05 到期，正确截止
				// 为 12:05；改成 12:06 后决定时刻 12:08 仍晚于截止，也必须拒绝。
				p.ApprovalWait = durationJSON(10 * time.Minute)
				s.ExpiresAt = timeJSON(t0.Add(5 * time.Minute))
				r.WaitDeadline = timeJSON(t0.Add(6 * time.Minute))
				r.DecidedAt = timeJSON(t0.Add(8 * time.Minute))
			},
			reason: "does not match min",
		},
		{
			name: "deadline ignores earlier session expiry",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, s *sessionBackupV1) {
				s.ExpiresAt = timeJSON(t0.Add(20 * time.Minute))
				// 正确截止应为 t0+20m，备份仍按最长等待时长保存 t0+1h。
			},
			reason: "does not match min",
		},
		{
			name: "deadline ignores earlier policy end",
			mutate: func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
				p.EndsAt = timeJSON(t0.Add(30 * time.Minute))
				// 正确截止应为 t0+30m，备份仍按最长等待时长保存 t0+1h。
			},
			reason: "does not match min",
		},
		{
			name: "decision before deadline",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(t0.Add(time.Hour - time.Nanosecond))
			},
			reason: "before wait deadline",
		},
		{
			name: "missing decided_at",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(time.Time{})
			},
			reason: "missing decided_at",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := buildExpiredPendingTimingBackup(t, t0, tc.mutate)
			// 恢复时刻远近都不影响结论：只按备份记载的时间判断。
			for _, at := range []time.Time{t0.Add(time.Minute), t0.Add(48 * time.Hour)} {
				msg := assertExpirationTimingRejected(t, data, at)
				if !strings.Contains(msg, tc.reason) {
					t.Fatalf("error %q must explain %q", msg, tc.reason)
				}
			}
		})
	}
}

// TestRestoreExpiredTimingConsistentBalancesCannotPass 验证余额与策略累计金额
// 完全自洽的备份，只要过期时间矛盾仍必须整体拒绝。
func TestRestoreExpiredTimingConsistentBalancesCannotPass(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildExpiredPendingTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		// 截止时刻被清空：余额与累计金额丝毫不受影响，也必须拒绝。
		r.WaitDeadline = timeJSON(time.Time{})
	})
	assertExpirationTimingRejected(t, data, t0.Add(time.Minute))
}

// TestRestoreReservationExpiredUnaffectedByExpirationTiming 验证已预留费用的
// 预留超时不适用待审批过期的时限核对：其等待截止字段本就不存在，仍按既有
// 预留超时规则恢复。
func TestRestoreReservationExpiredUnaffectedByExpirationTiming(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// 未超门槛直接预留（费用 5 ≤ 门槛 10），提交时刻 t0 即完成预留、预留
	// 时长 2h，截止时刻 t0+2h 超时全额退回。
	data := buildExpiredPendingTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		r.EstimatedFee = 5
		r.State = int(RequestReservationExpired)
		r.WaitDeadline = timeJSON(time.Time{})
		r.DecidedAt = timeJSON(time.Time{})
		r.ReservedAt = timeJSON(t0)
		r.ReserveDuration = durationJSON(2 * time.Hour)
		r.ReserveDeadline = timeJSON(t0.Add(2 * time.Hour))
		r.ReserveExpiredAt = timeJSON(t0.Add(2 * time.Hour))
	})
	w2, err := restoreAt(data, t0.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("reservation-expired request must restore: %v", err)
	}
	r, err := w2.Request("u1", "big")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != RequestReservationExpired {
		t.Fatalf("state = %v, want reservation expired", r.State)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("payer balance = %+v, want untouched", bal)
	}
}
