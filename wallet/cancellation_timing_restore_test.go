package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildCancelledPendingTimingBackup 构造一笔“超门槛待审批后被取消、从未预留
// 费用”的请求备份，所有计时字段默认合法：提交于 t0、等待截止 t0+1h（策略
// 窗口至 t0+24h、会话至 t0+24h，均不抢先）、于 t0+30m 取消。mutate 在余额
// 核对前调整请求/策略/会话的计时字段，以便构造各类不合法历史。
func buildCancelledPendingTimingBackup(t *testing.T, t0 time.Time, mutate func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)) []byte {
	t.Helper()
	const fee int64 = 20
	r := requestBackupV1{
		PolicyID: "p", RequestID: "big", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee: fee,
		State:        int(RequestCancelled),
		CreatedAt:    timeJSON(t0),
		WaitDeadline: timeJSON(t0.Add(time.Hour)),
		DecidedAt:    timeJSON(t0.Add(30 * time.Minute)),
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
		Ledger:   []ledgerEntryBackupV1{},
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// assertCancellationTimingRejected 恢复必须失败、包装 ErrBackupInvalid，并在
// 错误信息中点名使用账户与请求编号；不得返回部分恢复的钱包。
func assertCancellationTimingRejected(t *testing.T, data []byte, at time.Time) string {
	t.Helper()
	w2, err := restoreAt(data, at)
	if err == nil {
		if w2 != nil {
			t.Fatalf("invalid cancellation history restored a wallet: %+v", w2)
		}
		t.Fatal("invalid cancellation history restored without error")
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

// TestRestoreCancelledPendingTimingAccepted 验证等待期限内完成的合法取消
// （含恰在提交时刻取消）照常恢复；即使恢复时会话已到期或被吊销、策略已结束
// 或停用，仍保持原取消状态与决定时间，不追加过期或退款记录、不产生资金变动。
func TestRestoreCancelledPendingTimingAccepted(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("cancel restores long after windows end", func(t *testing.T) {
		data := buildCancelledPendingTimingBackup(t, t0, nil)
		w2, err := restoreAt(data, t0.Add(48*time.Hour))
		if err != nil {
			t.Fatalf("legal cancellation must restore: %v", err)
		}
		r, err := w2.Request("u1", "big")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestCancelled {
			t.Fatalf("state = %v, want cancelled", r.State)
		}
		if !r.CreatedAt.Equal(t0) || !r.WaitDeadline.Equal(t0.Add(time.Hour)) ||
			!r.DecidedAt.Equal(t0.Add(30*time.Minute)) {
			t.Fatalf("timing rewritten: created %v deadline %v decided %v",
				r.CreatedAt, r.WaitDeadline, r.DecidedAt)
		}
		if r.ApproverAccountID != "" || r.RejectReason != "" {
			t.Fatalf("cancelled request must not carry approver/reason: %+v", r)
		}
		// 终态不改写：不追加过期或退款记录，余额与额度不变。
		if got := len(w2.Ledger()); got != 0 {
			t.Fatalf("restore appended %d ledger entries", got)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
			t.Fatalf("payer balance = %+v, want untouched", bal)
		}
	})

	t.Run("cancelled exactly at created_at restores", func(t *testing.T) {
		data := buildCancelledPendingTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0)
		})
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("cancellation exactly at created_at must be legal: %v", err)
		}
	})

	t.Run("cancellation one nanosecond before deadline restores", func(t *testing.T) {
		data := buildCancelledPendingTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0.Add(time.Hour - time.Nanosecond))
		})
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("cancellation 1ns before deadline must be legal: %v", err)
		}
	})

	t.Run("deadline bounded by policy end", func(t *testing.T) {
		data := buildCancelledPendingTimingBackup(t, t0, func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
			p.EndsAt = timeJSON(t0.Add(30 * time.Minute))
			r.WaitDeadline = timeJSON(t0.Add(30 * time.Minute))
			r.DecidedAt = timeJSON(t0.Add(15 * time.Minute))
		})
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("policy-end bounded deadline: %v", err)
		}
	})

	t.Run("deadline bounded by session expiry", func(t *testing.T) {
		data := buildCancelledPendingTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, s *sessionBackupV1) {
			s.ExpiresAt = timeJSON(t0.Add(20 * time.Minute))
			r.WaitDeadline = timeJSON(t0.Add(20 * time.Minute))
			r.DecidedAt = timeJSON(t0.Add(10 * time.Minute))
		})
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("session-expiry bounded deadline: %v", err)
		}
	})

	t.Run("timezone representation does not change outcome", func(t *testing.T) {
		// 同一时刻的不同时区写法：t0+30m UTC 即该瞬间在 +02:00 的 14:30。
		data := buildCancelledPendingTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0.Add(30 * time.Minute).In(time.FixedZone("UTC+2", 2*3600)))
		})
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("same instant in another zone must restore: %v", err)
		}
	})
}

// TestRestoreRejectsCancelledPendingTiming 验证待审批取消的请求：等待截止缺失、
// 被改长/改短，或取消决定发生在提交前、截止时刻或之后，都必须让整个备份被
// 拒绝——余额与累计金额自洽也不能放行。
func TestRestoreRejectsCancelledPendingTiming(t *testing.T) {
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
			reason: "wait deadline",
		},
		{
			name: "deadline claims session expiry but stored later",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, s *sessionBackupV1) {
				s.ExpiresAt = timeJSON(t0.Add(20 * time.Minute))
				// 正确截止应为 t0+20m，备份仍保存 t0+1h。
			},
			reason: "does not match min",
		},
		{
			name: "cancellation before created_at",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(t0.Add(-time.Nanosecond))
			},
			reason: "before created_at",
		},
		{
			name: "cancellation exactly at deadline",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(t0.Add(time.Hour))
			},
			reason: "strictly before wait deadline",
		},
		{
			name: "cancellation after deadline",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(t0.Add(time.Hour + time.Nanosecond))
			},
			reason: "strictly before wait deadline",
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
			data := buildCancelledPendingTimingBackup(t, t0, tc.mutate)
			// 恢复时刻远近都不影响结论：只按备份记载的时间判断。
			for _, at := range []time.Time{t0.Add(time.Minute), t0.Add(48 * time.Hour)} {
				msg := assertCancellationTimingRejected(t, data, at)
				if !strings.Contains(msg, tc.reason) {
					t.Fatalf("error %q must explain %q", msg, tc.reason)
				}
			}
		})
	}
}

// TestRestoreCancelledTimingConsistentBalancesCannotPass 验证余额与策略累计
// 金额完全自洽的备份，只要取消时间矛盾仍必须整体拒绝。
func TestRestoreCancelledTimingConsistentBalancesCannotPass(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildCancelledPendingTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		// 截止当时才取消：正常流程中该时刻请求已进入过期终态。
		r.DecidedAt = timeJSON(t0.Add(time.Hour))
	})
	assertCancellationTimingRejected(t, data, t0.Add(time.Minute))
}

// TestRestoreReservedThenCancelledUnaffected 验证已预留后再取消的请求沿用既有
// 规则：不适用待审批取消的时限核对，也不因缺少待审批取消的决定时间被拒绝。
func TestRestoreReservedThenCancelledUnaffected(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// 未超门槛直接预留（费用 5 ≤ 门槛 10），提交时刻 t0 即完成预留、之后取消：
	// 直接受理的请求本就不携带决定时间。
	data := buildCancelledPendingTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		r.EstimatedFee = 5
		r.WaitDeadline = timeJSON(time.Time{})
		r.DecidedAt = timeJSON(time.Time{})
		r.ReservedAt = timeJSON(t0)
		r.ReserveDuration = durationJSON(2 * time.Hour)
		r.ReserveDeadline = timeJSON(t0.Add(2 * time.Hour))
	})
	w2, err := restoreAt(data, t0.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("reserved-then-cancelled request must restore: %v", err)
	}
	r, err := w2.Request("u1", "big")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != RequestCancelled {
		t.Fatalf("state = %v, want cancelled", r.State)
	}
	if !r.ReservedAt.Equal(t0) || !r.DecidedAt.IsZero() {
		t.Fatalf("reserved-origin cancellation rewritten: %+v", r)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("payer balance = %+v, want untouched", bal)
	}
}
