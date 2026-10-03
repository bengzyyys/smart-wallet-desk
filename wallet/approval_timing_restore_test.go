package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildApprovedTimingBackup 构造一笔“超门槛经批准后预留”的请求备份，所有
// 计时字段默认合法：提交于 t0、等待截止 t0+1h（策略窗口至 t0+24h、会话至
// t0+24h，均不抢先）、于 t0+30m 批准并预留、预留时长 2h。mutate 在余额
// 核对前调整请求/策略/会话的计时字段，以便构造各类不合法历史。
func buildApprovedTimingBackup(t *testing.T, t0 time.Time, state RequestState, mutate func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)) []byte {
	t.Helper()
	const fee int64 = 20
	r := requestBackupV1{
		PolicyID: "p", RequestID: "big", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee:      fee,
		State:             int(state),
		CreatedAt:         timeJSON(t0),
		WaitDeadline:      timeJSON(t0.Add(time.Hour)),
		ReservedAt:        timeJSON(t0.Add(30 * time.Minute)),
		ReserveDuration:   durationJSON(2 * time.Hour),
		ReserveDeadline:   timeJSON(t0.Add(2*time.Hour + 30*time.Minute)),
		ApproverAccountID: "payer",
		DecidedAt:         timeJSON(t0.Add(30 * time.Minute)),
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

	// 按终态补齐结算字段与自洽余额（校验在恢复时到期处理之前完成，故已预留
	// 状态按备份记载的占用填写）。
	var payerAvail, payerReserved, policyReserved, policySpent int64
	switch state {
	case RequestReserved:
		payerAvail, payerReserved = 80, fee
		policyReserved = fee
	case RequestSettled:
		r.ActualFee = fee
		if time.Time(r.SettledAt).IsZero() {
			r.SettledAt = r.ReservedAt
		}
		payerAvail = 80
		policySpent = fee
	case RequestCancelled:
		// 已预留后取消：费用已全额退回。
		payerAvail = 100
	case RequestReservationExpired:
		r.ReserveExpiredAt = r.ReserveDeadline
		payerAvail = 100
	default:
		t.Fatalf("unsupported state %v", state)
	}
	pol.ReservedTotal, pol.SpentTotal = policyReserved, policySpent

	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0.Add(-time.Minute)),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: payerAvail, Reserved: payerReserved, CreatedAt: timeJSON(t0.Add(-2 * time.Hour))},
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

// assertApprovalTimingRejected 恢复必须失败、包装 ErrBackupInvalid，并在错误
// 信息中点名使用账户与请求编号；不得返回部分恢复的钱包。
func assertApprovalTimingRejected(t *testing.T, data []byte, at time.Time) string {
	t.Helper()
	w2, err := restoreAt(data, at)
	if err == nil {
		if w2 != nil {
			t.Fatalf("invalid approval history restored a wallet: %+v", w2)
		}
		t.Fatal("invalid approval history restored without error")
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

// TestRestoreApprovedRequestTimingAccepted 验证期限内批准的请求（含恰在提交
// 时刻批准）合法；即使恢复时等待期限、申请会话与策略窗口均已结束，甚至该
// 笔预留在恢复时已超时将自动退回，也照常恢复且不重新计时。
func TestRestoreApprovedRequestTimingAccepted(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("approved within window restores", func(t *testing.T) {
		data := buildApprovedTimingBackup(t, t0, RequestReserved, nil)
		w2, err := restoreAt(data, t0.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("legal approved request: %v", err)
		}
		r, err := w2.Request("u1", "big")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestReserved {
			t.Fatalf("state = %v, want reserved", r.State)
		}
	})

	t.Run("approved exactly at created_at restores", func(t *testing.T) {
		data := buildApprovedTimingBackup(t, t0, RequestReserved, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.ReservedAt = timeJSON(t0)
			r.ReserveDeadline = timeJSON(t0.Add(2 * time.Hour))
			r.DecidedAt = timeJSON(t0)
		})
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("approval exactly at created_at must be legal: %v", err)
		}
	})

	t.Run("all windows ended at restore time still restores without retiming", func(t *testing.T) {
		data := buildApprovedTimingBackup(t, t0, RequestReserved, func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
			// 关闭预留超时，使请求在很久以后仍处于已预留，专注核对审批计时。
			p.MaxReserveDuration = 0
			r.ReserveDuration = 0
			r.ReserveDeadline = timeJSON(time.Time{})
		})
		w2, err := restoreAt(data, t0.Add(48*time.Hour))
		if err != nil {
			t.Fatalf("in-window approval must restore long after windows end: %v", err)
		}
		r, _ := w2.Request("u1", "big")
		if !r.WaitDeadline.Equal(t0.Add(time.Hour)) || !r.ReservedAt.Equal(t0.Add(30*time.Minute)) {
			t.Fatalf("timing rewritten: deadline %v reserved %v", r.WaitDeadline, r.ReservedAt)
		}
		if r.State != RequestReserved {
			t.Fatalf("reserve with no timeout changed state = %v", r.State)
		}
	})

	t.Run("reservation due at restore time refunds but history still valid", func(t *testing.T) {
		// 预留截止 t0+2h30：恢复时已超时，按原截止时刻全额退回；审批历史的
		// 合法性只看备份记载的时间，不看恢复时刻。
		data := buildApprovedTimingBackup(t, t0, RequestReserved, nil)
		w2, err := restoreAt(data, t0.Add(24*time.Hour))
		if err != nil {
			t.Fatalf("valid history with due reservation: %v", err)
		}
		r, _ := w2.Request("u1", "big")
		if r.State != RequestReservationExpired {
			t.Fatalf("state = %v, want reservation expired", r.State)
		}
		if !r.ReserveExpiredAt.Equal(t0.Add(2*time.Hour + 30*time.Minute)) {
			t.Fatalf("release time = %v, want original reserve deadline", r.ReserveExpiredAt)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
			t.Fatalf("balance = %+v, want full refund", bal)
		}
	})

	t.Run("deadline bounded by policy end", func(t *testing.T) {
		data := buildApprovedTimingBackup(t, t0, RequestReserved, func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
			p.EndsAt = timeJSON(t0.Add(30 * time.Minute))
			// 策略结束最先发生；批准必须严格早于它。
			r.WaitDeadline = timeJSON(t0.Add(30 * time.Minute))
			r.ReservedAt = timeJSON(t0.Add(15 * time.Minute))
			r.DecidedAt = timeJSON(t0.Add(15 * time.Minute))
			r.ReserveDeadline = timeJSON(t0.Add(2*time.Hour + 15*time.Minute))
		})
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("policy-end bounded deadline: %v", err)
		}
	})

	t.Run("deadline bounded by session expiry", func(t *testing.T) {
		data := buildApprovedTimingBackup(t, t0, RequestReserved, func(r *requestBackupV1, _ *policyBackupV1, s *sessionBackupV1) {
			s.ExpiresAt = timeJSON(t0.Add(20 * time.Minute))
			r.WaitDeadline = timeJSON(t0.Add(20 * time.Minute))
			r.ReservedAt = timeJSON(t0.Add(10 * time.Minute))
			r.DecidedAt = timeJSON(t0.Add(10 * time.Minute))
			r.ReserveDeadline = timeJSON(t0.Add(2*time.Hour + 10*time.Minute))
		})
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("session-expiry bounded deadline: %v", err)
		}
	})
}

// TestRestoreRejectsApprovedTimingOnReserved 验证已预留的批准路径请求：
// 等待截止缺失、被改长/改短，或批准发生在提交前、截止时刻或之后，都必须
// 让整个备份被拒绝——即使恢复时该预留已到期、原本将自动退回。
func TestRestoreRejectsApprovedTimingOnReserved(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		restoreAt time.Time
		mutate    func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)
		reason    string
	}{
		{
			name:      "missing wait deadline",
			restoreAt: t0.Add(time.Minute),
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.WaitDeadline = timeJSON(time.Time{})
			},
			reason: "missing wait deadline",
		},
		{
			name:      "lengthened wait deadline",
			restoreAt: t0.Add(time.Minute),
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.WaitDeadline = timeJSON(t0.Add(2 * time.Hour))
			},
			reason: "does not match min",
		},
		{
			name:      "shortened wait deadline",
			restoreAt: t0.Add(time.Minute),
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.WaitDeadline = timeJSON(t0.Add(15 * time.Minute))
			},
			reason: "wait deadline",
		},
		{
			name:      "deadline claims policy end but stored later",
			restoreAt: t0.Add(time.Minute),
			mutate: func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
				p.EndsAt = timeJSON(t0.Add(30 * time.Minute))
				// 正确截止应为 t0+30m，备份仍保存 t0+1h。
			},
			reason: "does not match min",
		},
		{
			name:      "approval exactly at deadline",
			restoreAt: t0.Add(time.Minute),
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.ReservedAt = timeJSON(t0.Add(time.Hour))
				r.DecidedAt = timeJSON(t0.Add(time.Hour))
				r.ReserveDeadline = timeJSON(t0.Add(3 * time.Hour))
			},
			reason: "strictly before wait deadline",
		},
		{
			name:      "approval after deadline",
			restoreAt: t0.Add(time.Minute),
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.ReservedAt = timeJSON(t0.Add(time.Hour + time.Nanosecond))
				r.DecidedAt = timeJSON(t0.Add(time.Hour + time.Nanosecond))
				r.ReserveDeadline = timeJSON(t0.Add(3*time.Hour + time.Nanosecond))
			},
			reason: "strictly before wait deadline",
		},
		{
			name:      "approval before created_at",
			restoreAt: t0.Add(time.Minute),
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.ReservedAt = timeJSON(t0.Add(-time.Nanosecond))
				r.DecidedAt = timeJSON(t0.Add(-time.Nanosecond))
				r.ReserveDeadline = timeJSON(t0.Add(2*time.Hour - time.Nanosecond))
			},
			reason: "before created_at",
		},
		{
			name:      "invalid history rejected even though reservation due for refund",
			restoreAt: t0.Add(24 * time.Hour),
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				// 批准恰在截止时刻；恢复时预留早就该自动退回，仍必须拒绝。
				r.ReservedAt = timeJSON(t0.Add(time.Hour))
				r.DecidedAt = timeJSON(t0.Add(time.Hour))
				r.ReserveDeadline = timeJSON(t0.Add(3 * time.Hour))
			},
			reason: "strictly before wait deadline",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := buildApprovedTimingBackup(t, t0, RequestReserved, tc.mutate)
			msg := assertApprovalTimingRejected(t, data, tc.restoreAt)
			if !strings.Contains(msg, tc.reason) {
				t.Fatalf("error %q must explain %q", msg, tc.reason)
			}
		})
	}
}

// TestRestoreRejectsApprovedTimingAcrossTerminalStates 验证审批时限核对对所有
// “曾批准并实际预留”的状态统一适用：后来已结算、已预留后取消、预留超时的
// 请求也不能携带正常审批无法产生的时间。
func TestRestoreRejectsApprovedTimingAcrossTerminalStates(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// approveAtDeadline 把批准（=预留）时刻挪到等待截止时刻——正常审批中该
	// 时刻只能进入待审批过期终态，绝不可能预留成功。
	approveAtDeadline := func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		r.ReservedAt = timeJSON(t0.Add(time.Hour))
		r.DecidedAt = timeJSON(t0.Add(time.Hour))
		r.ReserveDeadline = timeJSON(t0.Add(3 * time.Hour))
	}
	dropDeadline := func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		r.WaitDeadline = timeJSON(time.Time{})
	}

	for _, tc := range []struct {
		name   string
		state  RequestState
		mutate func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)
	}{
		{"settled approval at deadline", RequestSettled, approveAtDeadline},
		{"settled missing deadline", RequestSettled, dropDeadline},
		{"reserved-cancelled approval at deadline", RequestCancelled, approveAtDeadline},
		{"reserved-cancelled missing deadline", RequestCancelled, dropDeadline},
		{"reservation-expired approval at deadline", RequestReservationExpired, approveAtDeadline},
		{"reservation-expired missing deadline", RequestReservationExpired, dropDeadline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := buildApprovedTimingBackup(t, t0, tc.state, tc.mutate)
			assertApprovalTimingRejected(t, data, t0.Add(time.Minute))
		})
	}

	// 对照：同样终态但批准时刻合法时应正常恢复。
	for _, state := range []RequestState{RequestSettled, RequestCancelled, RequestReservationExpired} {
		name := map[RequestState]string{
			RequestSettled:            "settled",
			RequestCancelled:          "reserved-cancelled",
			RequestReservationExpired: "reservation-expired",
		}[state]
		t.Run(name+" legal approval restores", func(t *testing.T) {
			data := buildApprovedTimingBackup(t, t0, state, nil)
			if _, err := restoreAt(data, t0.Add(24*time.Hour)); err != nil {
				t.Fatalf("legal %s request: %v", name, err)
			}
		})
	}
}

// TestRestoreDirectReserveNeedsNoApprovalInfo 验证未超门槛直接预留的请求不
// 被要求补上等待截止等审批信息（保持既有行为）。
func TestRestoreDirectReserveNeedsNoApprovalInfo(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildApprovedTimingBackup(t, t0, RequestReserved, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		// 费用 5 不超门槛 10：直接受理，不得携带批准/等待信息。
		r.EstimatedFee = 5
		r.ApproverAccountID = ""
		r.DecidedAt = timeJSON(time.Time{})
		r.WaitDeadline = timeJSON(time.Time{})
		r.ReservedAt = timeJSON(t0)
		r.ReserveDeadline = timeJSON(t0.Add(2 * time.Hour))
	})
	// 直接预留 5：调整账户预留余额与策略预留总额与之自洽。
	var b backupV1
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	for i := range b.Accounts {
		if b.Accounts[i].ID == "payer" {
			b.Accounts[i].Available = 95
			b.Accounts[i].Reserved = 5
		}
	}
	for i := range b.Policies {
		if b.Policies[i].ID == "p" {
			b.Policies[i].ReservedTotal = 5
		}
	}
	fixed, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(fixed, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("below-threshold direct reserve must not require approval info: %v", err)
	}
	r, _ := w2.Request("u1", "big")
	if r.State != RequestReserved || !r.WaitDeadline.IsZero() || r.ApproverAccountID != "" {
		t.Fatalf("direct reserve restored wrong: %+v", r)
	}
}
