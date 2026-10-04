package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildRejectedTimingBackup 构造一笔“超门槛待审批后被拒绝”的请求备份，所有
// 计时字段默认合法：提交于 t0、等待截止 t0+1h（策略窗口至 t0+24h、会话至
// t0+24h，均不抢先）、于 t0+30m 拒绝。mutate 在余额核对前调整请求/策略/
// 会话的计时与拒绝字段，以便构造各类不合法历史。cause 选择拒绝路径：
// 出资账户主动拒绝、申请会话吊销或策略停用。
func buildRejectedTimingBackup(t *testing.T, t0 time.Time, cause string, mutate func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)) []byte {
	t.Helper()
	const fee int64 = 20
	r := requestBackupV1{
		PolicyID: "p", RequestID: "big", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee: fee,
		State:        int(RequestRejected),
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
	switch cause {
	case "payer":
		r.RejectReason = "duplicate order"
		r.ApproverAccountID = "payer"
	case "revoke":
		r.RejectReason = "application session revoked before approval"
		sess.Revoked = true
	case "deactivate":
		r.RejectReason = "policy p deactivated by payer: stop it"
		r.ApproverAccountID = "payer"
		pol.Deactivated = true
		pol.DeactivatedAt = timeJSON(t0.Add(30 * time.Minute))
		pol.DeactivatorAccountID = "payer"
		pol.DeactivateReason = "stop it"
	default:
		t.Fatalf("unknown cause %q", cause)
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
	if !strings.Contains(msg, `"u1"`) || !strings.Contains(msg, `"big"`) {
		t.Fatalf("error %q must name usage account and request id", msg)
	}
	return msg
}

// TestRestoreRejectedRequestTimingAccepted 验证期限内完成的合法拒绝（含恰在
// 提交时刻拒绝）照常恢复；即使恢复时会话已到期或被吊销、策略已结束或停用，
// 仍保持已拒绝终态，全部计时与决定信息原样保留，不追加过期记录、不产生
// 资金变动。三条拒绝路径（出资账户主动拒绝、会话吊销、策略停用）一致。
func TestRestoreRejectedRequestTimingAccepted(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	for _, cause := range []string{"payer", "revoke", "deactivate"} {
		t.Run(cause+" rejection restores long after windows end", func(t *testing.T) {
			data := buildRejectedTimingBackup(t, t0, cause, nil)
			w2, err := restoreAt(data, t0.Add(48*time.Hour))
			if err != nil {
				t.Fatalf("legal rejection must restore: %v", err)
			}
			r, err := w2.Request("u1", "big")
			if err != nil {
				t.Fatal(err)
			}
			if r.State != RequestRejected {
				t.Fatalf("state = %v, want rejected", r.State)
			}
			if !r.CreatedAt.Equal(t0) || !r.WaitDeadline.Equal(t0.Add(time.Hour)) ||
				!r.DecidedAt.Equal(t0.Add(30*time.Minute)) {
				t.Fatalf("timing rewritten: created %v deadline %v decided %v",
					r.CreatedAt, r.WaitDeadline, r.DecidedAt)
			}
			wantApprover := "payer"
			if cause == "revoke" {
				wantApprover = ""
			}
			if r.ApproverAccountID != wantApprover {
				t.Fatalf("approver = %q, want %q", r.ApproverAccountID, wantApprover)
			}
			if r.RejectReason == "" {
				t.Fatal("reject reason lost")
			}
			// 终态不改写：不追加过期记录，余额不变。
			if got := len(w2.Ledger()); got != 0 {
				t.Fatalf("restore appended %d ledger entries", got)
			}
			if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
				t.Fatalf("payer balance = %+v, want untouched", bal)
			}
		})
	}

	t.Run("rejected exactly at created_at restores", func(t *testing.T) {
		data := buildRejectedTimingBackup(t, t0, "payer", func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0)
		})
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("rejection exactly at created_at must be legal: %v", err)
		}
	})

	t.Run("rejection one nanosecond before deadline restores", func(t *testing.T) {
		data := buildRejectedTimingBackup(t, t0, "payer", func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0.Add(time.Hour - time.Nanosecond))
		})
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("rejection 1ns before deadline must be legal: %v", err)
		}
	})

	t.Run("deadline bounded by policy end", func(t *testing.T) {
		data := buildRejectedTimingBackup(t, t0, "payer", func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
			p.EndsAt = timeJSON(t0.Add(30 * time.Minute))
			r.WaitDeadline = timeJSON(t0.Add(30 * time.Minute))
			r.DecidedAt = timeJSON(t0.Add(15 * time.Minute))
		})
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("policy-end bounded deadline: %v", err)
		}
	})

	t.Run("deadline bounded by session expiry", func(t *testing.T) {
		data := buildRejectedTimingBackup(t, t0, "revoke", func(r *requestBackupV1, _ *policyBackupV1, s *sessionBackupV1) {
			s.ExpiresAt = timeJSON(t0.Add(20 * time.Minute))
			r.WaitDeadline = timeJSON(t0.Add(20 * time.Minute))
			r.DecidedAt = timeJSON(t0.Add(10 * time.Minute))
		})
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("session-expiry bounded deadline: %v", err)
		}
	})

	t.Run("timezone representation does not change outcome", func(t *testing.T) {
		// 同一时刻的不同时区写法：t0+30m UTC 即 t0+31m30s +02:00 之前的瞬间。
		data := buildRejectedTimingBackup(t, t0, "payer", func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0.Add(30 * time.Minute).In(time.FixedZone("UTC+2", 2*3600)))
		})
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("same instant in another zone must restore: %v", err)
		}
	})
}

// TestRestoreRejectsRejectedTiming 验证已拒绝请求：等待截止缺失、被改长/改短，
// 或拒绝决定发生在提交前、截止时刻或之后，都必须让整个备份被拒绝——三条
// 拒绝路径统一适用，余额与累计金额自洽也不能放行。
func TestRestoreRejectsRejectedTiming(t *testing.T) {
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
			name: "rejection before created_at",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(t0.Add(-time.Nanosecond))
			},
			reason: "before created_at",
		},
		{
			name: "rejection exactly at deadline",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(t0.Add(time.Hour))
			},
			reason: "strictly before wait deadline",
		},
		{
			name: "rejection after deadline",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(t0.Add(time.Hour + time.Nanosecond))
			},
			reason: "strictly before wait deadline",
		},
	}
	for _, cause := range []string{"payer", "revoke", "deactivate"} {
		for _, tc := range cases {
			t.Run(cause+"/"+tc.name, func(t *testing.T) {
				data := buildRejectedTimingBackup(t, t0, cause, tc.mutate)
				// 恢复时刻远近都不影响结论：只按备份记载的时间判断。
				for _, at := range []time.Time{t0.Add(time.Minute), t0.Add(48 * time.Hour)} {
					msg := assertRejectionTimingRejected(t, data, at)
					if !strings.Contains(msg, tc.reason) {
						t.Fatalf("error %q must explain %q", msg, tc.reason)
					}
				}
			})
		}
	}
}

// TestRestoreRejectedTimingConsistentBalancesCannotPass 验证余额与策略累计
// 金额完全自洽的备份，只要拒绝时间矛盾仍必须整体拒绝。
func TestRestoreRejectedTimingConsistentBalancesCannotPass(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildRejectedTimingBackup(t, t0, "payer", func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		// 截止当时才拒绝：正常流程中该时刻请求已进入过期终态。
		r.DecidedAt = timeJSON(t0.Add(time.Hour))
	})
	assertRejectionTimingRejected(t, data, t0.Add(time.Minute))
}

// TestRestoreStandaloneRejectionLedgerEntriesKept 验证未被受理申请留下的独立
// 拒绝账本记录（没有对应请求或会话）不受拒绝时限核对影响，仍按原有规则保留。
func TestRestoreStandaloneRejectionLedgerEntriesKept(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildRejectedTimingBackup(t, t0, "payer", nil)
	var b backupV1
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	// 追加两条没有对应请求的拒绝留痕：一条指向未知账户/请求，一条指向未知
	// 会话时代的申请；时间任意，不参与请求时限核对。
	b.Ledger = append(b.Ledger,
		ledgerEntryBackupV1{
			Kind: int(LedgerRejection), AccountID: "ghost", RequestID: "no-such-request",
			Reason: "wallet: account not found: ghost", At: timeJSON(t0.Add(-time.Hour)),
		},
		ledgerEntryBackupV1{
			Kind: int(LedgerRejection), AccountID: "u1", RequestID: "unknown",
			Reason: "wallet: session not found: s9", At: timeJSON(t0.Add(90 * time.Minute)),
		},
	)
	fixed, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(fixed, t0.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("standalone rejection ledger entries must restore: %v", err)
	}
	entries := w2.Ledger()
	if len(entries) != 2 {
		t.Fatalf("ledger len = %d, want 2", len(entries))
	}
	for i, e := range entries {
		if e.Kind != LedgerRejection {
			t.Fatalf("entry %d kind = %v, want rejection", i, e.Kind)
		}
	}
	if entries[0].AccountID != "ghost" || entries[1].RequestID != "unknown" {
		t.Fatalf("standalone rejection entries not preserved: %+v", entries)
	}
}
