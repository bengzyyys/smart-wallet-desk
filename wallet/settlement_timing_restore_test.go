package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildDirectSettledBackup 构造一笔“未超门槛直接预留后已结算”的请求备份：
// t0 受理并预留、预留时长 1h（截止 t0+1h）、默认 t0+30m 按全额 20 结算。
// mutate 在余额核对前调整请求/策略字段；调整结束后按请求实际费用重算出资
// 账户余额与策略已花费总额，保证金额汇总始终自洽，测试只需专注计时字段。
func buildDirectSettledBackup(t *testing.T, t0 time.Time, mutate func(*requestBackupV1, *policyBackupV1)) []byte {
	t.Helper()
	const fee int64 = 20
	r := requestBackupV1{
		PolicyID: "p", RequestID: "r1", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee:    fee,
		ActualFee:       fee,
		State:           int(RequestSettled),
		CreatedAt:       timeJSON(t0),
		ReservedAt:      timeJSON(t0),
		ReserveDuration: durationJSON(time.Hour),
		ReserveDeadline: timeJSON(t0.Add(time.Hour)),
		SettledAt:       timeJSON(t0.Add(30 * time.Minute)),
	}
	pol := policyBackupV1{
		ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "charge", Payee: "shop",
		StartsAt:           timeJSON(t0.Add(-time.Hour)),
		EndsAt:             timeJSON(t0.Add(24 * time.Hour)),
		MaxPerRequest:      100,
		MaxTotal:           1000,
		MaxReserveDuration: durationJSON(time.Hour),
	}
	if mutate != nil {
		mutate(&r, &pol)
	}
	// 金额自洽：已结算实际费用从出资账户扣除并计入策略已花费总额。
	pol.SpentTotal = r.ActualFee
	payerAvail := 100 - r.ActualFee

	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0.Add(2 * time.Hour)),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: payerAvail, CreatedAt: timeJSON(t0.Add(-time.Hour))},
			{ID: "u1", CreatedAt: timeJSON(t0.Add(-time.Hour))},
		},
		Sessions: []sessionBackupV1{
			{ID: "s1", AccountID: "u1", DeviceID: "d1", ExpiresAt: timeJSON(t0.Add(24 * time.Hour)), CreatedAt: timeJSON(t0.Add(-time.Hour))},
		},
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

// assertSettleTimingRejected 恢复必须失败、包装 ErrBackupInvalid，并在错误
// 信息中点名使用账户与请求编号、说明结算时间不在有效预留期内；不得返回
// 部分恢复的钱包。
func assertSettleTimingRejected(t *testing.T, data []byte, at time.Time) string {
	t.Helper()
	w2, err := restoreAt(data, at)
	if err == nil {
		if w2 != nil {
			t.Fatalf("invalid settlement history restored a wallet: %+v", w2)
		}
		t.Fatal("invalid settlement history restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	namesRequest := strings.Contains(msg, `"r1"`) || strings.Contains(msg, `"big"`)
	if !strings.Contains(msg, `"u1"`) || !namesRequest {
		t.Fatalf("error %q must name usage account and request id", msg)
	}
	if !strings.Contains(msg, "not within the valid reservation period") {
		t.Fatalf("error %q must explain settled_at is outside the valid reservation period", msg)
	}
	return msg
}

// TestRestoreSettledWithinReservationPeriod 验证启用预留超时时，有效预留期
// [预留时刻, 截止时刻) 内的结算合法：恰在预留完成时结算、截止前不足一秒
// 结算、实际费用为零的结算都接受；很久以后恢复也不改写已结算终态。
func TestRestoreSettledWithinReservationPeriod(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("settled exactly at reserved_at restores", func(t *testing.T) {
		data := buildDirectSettledBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1) {
			r.SettledAt = r.ReservedAt
		})
		if _, err := restoreAt(data, t0.Add(30*time.Minute)); err != nil {
			t.Fatalf("settle exactly at reserved_at must be legal: %v", err)
		}
	})

	t.Run("settled one nanosecond before deadline restores", func(t *testing.T) {
		data := buildDirectSettledBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1) {
			r.SettledAt = timeJSON(t0.Add(time.Hour - time.Nanosecond))
		})
		if _, err := restoreAt(data, t0.Add(30*time.Minute)); err != nil {
			t.Fatalf("settle just before deadline must be legal: %v", err)
		}
	})

	t.Run("zero actual fee settled within window restores", func(t *testing.T) {
		data := buildDirectSettledBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1) {
			r.ActualFee = 0
		})
		w2, err := restoreAt(data, t0.Add(30*time.Minute))
		if err != nil {
			t.Fatalf("zero-fee settle within window must be legal: %v", err)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
			t.Fatalf("balance = %+v, want full amount available after zero-fee settle", bal)
		}
	})

	t.Run("in-window settlement stays settled long after deadline", func(t *testing.T) {
		data := buildDirectSettledBackup(t, t0, nil)
		// 恢复时刻远晚于预留截止时刻：期限内已完成的结算保持已结算，不追加
		// 退款或超时记录，余额与已花费总额原样保留。
		w2, err := restoreAt(data, t0.Add(100*24*time.Hour))
		if err != nil {
			t.Fatalf("legal settled history must restore long after deadline: %v", err)
		}
		r, err := w2.Request("u1", "r1")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestSettled || r.ActualFee != 20 {
			t.Fatalf("settled request rewritten: %+v", r)
		}
		if !r.SettledAt.Equal(t0.Add(30*time.Minute)) || !r.ReservedAt.Equal(t0) ||
			!r.ReserveDeadline.Equal(t0.Add(time.Hour)) || !r.ReserveExpiredAt.IsZero() {
			t.Fatalf("timing rewritten: %+v", r)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 80, Reserved: 0}) {
			t.Fatalf("balance = %+v, want {80 0}", bal)
		}
		pv, _ := w2.Policy("p")
		if pv.SpentTotal != 20 || pv.ReservedTotal != 0 {
			t.Fatalf("totals = spent %d reserved %d, want 20/0", pv.SpentTotal, pv.ReservedTotal)
		}
		if n := len(w2.Ledger()); n != 0 {
			t.Fatalf("restore appended %d ledger entries, want none", n)
		}
	})

	t.Run("timezone representation does not change the instant", func(t *testing.T) {
		// 截止时刻 t0+1h（UTC）；用 +08:00 表示同一物理时刻前 1 纳秒的结算
		// 必须接受，用 +08:00 表示恰为截止时刻的结算必须拒绝。
		east := time.FixedZone("UTC+8", 8*3600)
		data := buildDirectSettledBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1) {
			r.SettledAt = timeJSON(t0.Add(time.Hour - time.Nanosecond).In(east))
		})
		if _, err := restoreAt(data, t0.Add(30*time.Minute)); err != nil {
			t.Fatalf("same instant in another timezone must compare equal: %v", err)
		}
		data = buildDirectSettledBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1) {
			r.SettledAt = timeJSON(t0.Add(time.Hour).In(east))
		})
		assertSettleTimingRejected(t, data, t0.Add(30*time.Minute))
	})
}

// TestRestoreRejectsSettledAtOrAfterDeadline 验证启用预留超时时，结算时刻
// 等于或晚于预留截止时刻的已结算记录必须让整个备份被拒绝：正常流程在截止
// 时刻已全额退回，不可能再产生结算；实际费用为零同样受限。
func TestRestoreRejectsSettledAtOrAfterDeadline(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		restoreAt time.Time
		mutate    func(*requestBackupV1, *policyBackupV1)
	}{
		{
			name:      "settled exactly at reserve deadline",
			restoreAt: t0.Add(30 * time.Minute),
			mutate: func(r *requestBackupV1, _ *policyBackupV1) {
				r.SettledAt = r.ReserveDeadline
			},
		},
		{
			name:      "settled one nanosecond after deadline",
			restoreAt: t0.Add(30 * time.Minute),
			mutate: func(r *requestBackupV1, _ *policyBackupV1) {
				r.SettledAt = timeJSON(t0.Add(time.Hour + time.Nanosecond))
			},
		},
		{
			name:      "settled long after deadline",
			restoreAt: t0.Add(30 * time.Minute),
			mutate: func(r *requestBackupV1, _ *policyBackupV1) {
				r.SettledAt = timeJSON(t0.Add(10 * time.Hour))
			},
		},
		{
			name:      "zero actual fee settled at deadline",
			restoreAt: t0.Add(30 * time.Minute),
			mutate: func(r *requestBackupV1, _ *policyBackupV1) {
				r.ActualFee = 0
				r.SettledAt = r.ReserveDeadline
			},
		},
		{
			// 恢复时刻早于结算时刻也不影响判断：核对只看备份记载的时间。
			name:      "rejected even when restore time precedes the forged settle",
			restoreAt: t0.Add(time.Minute),
			mutate: func(r *requestBackupV1, _ *policyBackupV1) {
				r.SettledAt = timeJSON(t0.Add(2 * time.Hour))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := buildDirectSettledBackup(t, t0, tc.mutate)
			assertSettleTimingRejected(t, data, tc.restoreAt)
		})
	}
}

// TestRestoreSettledDeadlineOnApprovalPath 验证待审批后获批的已结算请求同样
// 遵守预留截止限制：预留期自批准成功（实际冻结费用）时刻起算，等待审批的
// 时间不计入；恰到或超过截止时刻的结算必须拒绝。
func TestRestoreSettledDeadlineOnApprovalPath(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// 批准并预留于 t0+30m，预留时长 2h，截止 t0+2h30m。
	deadline := t0.Add(2*time.Hour + 30*time.Minute)

	t.Run("approved then settled within window restores", func(t *testing.T) {
		data := buildApprovedTimingBackup(t, t0, RequestSettled, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.SettledAt = timeJSON(deadline.Add(-time.Nanosecond))
		})
		w2, err := restoreAt(data, t0.Add(24*time.Hour))
		if err != nil {
			t.Fatalf("approved request settled before its reserve deadline: %v", err)
		}
		r, _ := w2.Request("u1", "big")
		if r.State != RequestSettled || r.ActualFee != 20 {
			t.Fatalf("settled request rewritten: %+v", r)
		}
	})

	t.Run("approved then settled exactly at reserve deadline rejected", func(t *testing.T) {
		data := buildApprovedTimingBackup(t, t0, RequestSettled, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.SettledAt = timeJSON(deadline)
		})
		assertSettleTimingRejected(t, data, t0.Add(time.Hour))
	})

	t.Run("approved then settled after reserve deadline rejected", func(t *testing.T) {
		data := buildApprovedTimingBackup(t, t0, RequestSettled, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.SettledAt = timeJSON(deadline.Add(time.Second))
		})
		assertSettleTimingRejected(t, data, t0.Add(time.Hour))
	})
}

// TestRestoreSettledWithoutReserveTimeoutKeepsUnlimited 验证关闭预留超时的
// 请求沿用原有结算规则：只要结算不早于预留时刻，多晚结算都合法，不人为
// 增加结算期限。
func TestRestoreSettledWithoutReserveTimeoutKeepsUnlimited(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildDirectSettledBackup(t, t0, func(r *requestBackupV1, p *policyBackupV1) {
		p.MaxReserveDuration = 0
		r.ReserveDuration = 0
		r.ReserveDeadline = timeJSON(time.Time{})
		r.SettledAt = timeJSON(t0.Add(100 * 24 * time.Hour))
	})
	w2, err := restoreAt(data, t0.Add(200*24*time.Hour))
	if err != nil {
		t.Fatalf("settled request without reserve timeout must not gain a deadline: %v", err)
	}
	r, _ := w2.Request("u1", "r1")
	if r.State != RequestSettled || !r.SettledAt.Equal(t0.Add(100*24*time.Hour)) {
		t.Fatalf("settled request rewritten: %+v", r)
	}
}
