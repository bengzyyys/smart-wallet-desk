package wallet

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 已拒绝与等待审批过期是未结算终态：正常流程从未预留费用、也未执行结算，
// 因此备份中这两类请求的实际费用必须为零、结算时间必须为空。本文件验证
// 携带实际费用或结算时间的矛盾备份被整体拒绝，且判断不随恢复时刻改变。

// assertUnsettledSettlementRejected 恢复必须失败、包装 ErrBackupInvalid、
// 点名使用账户与请求编号，并说明是实际费用还是结算时间矛盾；不得返回部分
// 恢复的钱包。
func assertUnsettledSettlementRejected(t *testing.T, data []byte, at time.Time, wantSub string) {
	t.Helper()
	msg := assertRejectionTimingRejected(t, data, at)
	if !strings.Contains(msg, wantSub) {
		t.Fatalf("error %q must mention %q", msg, wantSub)
	}
}

// TestRestoreRejectedCarryingSettlementInfoRejected 验证三条拒绝路径（出资
// 账户主动拒绝、申请会话吊销、策略停用）下，已拒绝请求携带实际费用或结算
// 时间的备份均整体拒绝：正数实际费用即使不超过预估费用也不能接受，实际
// 费用为零但结算时间非空同样无效。
func TestRestoreRejectedCarryingSettlementInfoRejected(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// 恢复时刻覆盖等待期限之前、恰到截止与期限过去很久：结果必须相同。
	restoreTimes := []time.Time{
		t0.Add(45 * time.Minute),
		t0.Add(time.Hour),
		t0.Add(72 * time.Hour),
	}

	for _, cause := range []string{"payer", "revoke", "deactivate"} {
		t.Run(cause+" rejection with positive actual fee", func(t *testing.T) {
			// 预估 20、实际 5：不超过预估也不能接受；出资账户预留余额与
			// 策略已花费总额均为零，其余引用与审批时刻均合法。
			data := buildRejectedTimingBackup(t, t0, cause, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.ActualFee = 5
			})
			for _, at := range restoreTimes {
				assertUnsettledSettlementRejected(t, data, at, "actual fee")
			}
		})
		t.Run(cause+" rejection with actual fee equal to estimate", func(t *testing.T) {
			data := buildRejectedTimingBackup(t, t0, cause, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.ActualFee = 20
			})
			assertUnsettledSettlementRejected(t, data, t0.Add(72*time.Hour), "actual fee")
		})
		t.Run(cause+" rejection with settled time only", func(t *testing.T) {
			data := buildRejectedTimingBackup(t, t0, cause, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.SettledAt = r.DecidedAt
			})
			for _, at := range restoreTimes {
				assertUnsettledSettlementRejected(t, data, at, "settled")
			}
		})
		t.Run(cause+" rejection with fee and settled time", func(t *testing.T) {
			data := buildRejectedTimingBackup(t, t0, cause, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.ActualFee = 5
				r.SettledAt = r.DecidedAt
			})
			assertUnsettledSettlementRejected(t, data, t0.Add(72*time.Hour), "actual fee")
		})
	}
}

// TestRestoreExpiredCarryingSettlementInfoRejected 验证等待审批过期的请求
// 携带实际费用或结算时间的备份同样整体拒绝，且与恢复时距离审批期限过去
// 多久无关。
func TestRestoreExpiredCarryingSettlementInfoRejected(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	restoreTimes := []time.Time{
		t0.Add(2 * time.Hour),
		t0.Add(72 * time.Hour),
	}

	t.Run("expired with positive actual fee", func(t *testing.T) {
		data := buildExpiredPendingTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.ActualFee = 5
		})
		for _, at := range restoreTimes {
			assertUnsettledSettlementRejected(t, data, at, "actual fee")
		}
	})
	t.Run("expired with settled time only", func(t *testing.T) {
		data := buildExpiredPendingTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.SettledAt = r.DecidedAt
		})
		for _, at := range restoreTimes {
			assertUnsettledSettlementRejected(t, data, at, "settled")
		}
	})
	t.Run("expired with fee and settled time", func(t *testing.T) {
		data := buildExpiredPendingTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.ActualFee = 20
			r.SettledAt = r.DecidedAt
		})
		assertUnsettledSettlementRejected(t, data, t0.Add(72*time.Hour), "actual fee")
	})
}

// TestRestoreUnsettledSettlementContradictionFailsWholeBackup 验证同一备份
// 还包含合法账户、策略与一笔合法已结算请求时，也不能只跳过矛盾记录继续
// 恢复：整份备份必须失败，调用方不能获得部分恢复的钱包。
func TestRestoreUnsettledSettlementContradictionFailsWholeBackup(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	data := buildRejectedTimingBackup(t, t0, "payer", func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		r.ActualFee = 5
	})
	var b backupV1
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	// 追加一笔合法的超门槛已结算请求：提交 t0、等待截止 t0+1h，t0+30m 批准
	// 并预留，t0+1h 结算 7，预留截止 t0+2h30m；策略已花费总额与出资账户
	// 可用余额同步调整。
	settled := requestBackupV1{
		PolicyID: "p", RequestID: "done", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee: 20, ActualFee: 7,
		State:             int(RequestSettled),
		CreatedAt:         timeJSON(t0),
		SettledAt:         timeJSON(t0.Add(time.Hour)),
		WaitDeadline:      timeJSON(t0.Add(time.Hour)),
		ReservedAt:        timeJSON(t0.Add(30 * time.Minute)),
		ReserveDuration:   durationJSON(2 * time.Hour),
		ReserveDeadline:   timeJSON(t0.Add(2*time.Hour + 30*time.Minute)),
		DecidedAt:         timeJSON(t0.Add(30 * time.Minute)),
		ApproverAccountID: "payer",
	}
	b.Requests = append(b.Requests, settled)
	b.Policies[0].SpentTotal = 7
	b.Accounts[0].Available = 93
	b.Ledger = append(b.Ledger, ledgerEntryBackupV1{
		Kind: int(LedgerSettle), AccountID: "payer", RequestID: "done",
		Amount: 7, Reason: "settle actual fee", At: timeJSON(t0.Add(time.Hour)),
	})
	full, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}

	w2, err := restoreAt(full, t0.Add(72*time.Hour))
	if err == nil {
		if w2 != nil {
			t.Fatalf("backup with one contradictory rejected request restored a partial wallet: %+v", w2)
		}
		t.Fatal("backup with one contradictory rejected request restored without error")
	}
	assertUnsettledSettlementRejected(t, full, t0.Add(72*time.Hour), "actual fee")

	// 对照：去掉矛盾的实际费用后，同一备份（含合法已结算请求）必须能
	// 整体恢复，证明失败只由被拒绝请求的结算信息引起。
	for i := range b.Requests {
		if b.Requests[i].RequestID == "big" {
			b.Requests[i].ActualFee = 0
		}
	}
	clean, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restoreAt(clean, t0.Add(72*time.Hour)); err != nil {
		t.Fatalf("same backup without the contradiction must restore: %v", err)
	}
}

// TestRestoreLegitUnsettledTerminalsHaveNoSettlementInfo 验证合法的拒绝与
// 等待审批过期历史（实际费用为零、无结算时间）继续恢复，保留原状态、预估
// 费用、提交时间、等待截止时间与决定时间；决定时间不被误当成结算时间。
func TestRestoreLegitUnsettledTerminalsHaveNoSettlementInfo(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	for _, cause := range []string{"payer", "revoke", "deactivate"} {
		t.Run(cause+" rejection keeps decision time without settled_at", func(t *testing.T) {
			data := buildRejectedTimingBackup(t, t0, cause, nil)
			w2, err := restoreAt(data, t0.Add(72*time.Hour))
			if err != nil {
				t.Fatalf("legal rejection must restore: %v", err)
			}
			r, err := w2.Request("u1", "big")
			if err != nil {
				t.Fatal(err)
			}
			if r.State != RequestRejected {
				t.Fatalf("state = %v, want RequestRejected", r.State)
			}
			if r.ActualFee != 0 || !r.SettledAt.IsZero() {
				t.Fatalf("rejected request must have no settlement info: fee %d settled_at %v", r.ActualFee, r.SettledAt)
			}
			if !r.DecidedAt.Equal(t0.Add(30 * time.Minute)) {
				t.Fatalf("decided_at = %v, want %v", r.DecidedAt, t0.Add(30*time.Minute))
			}
		})
	}
	t.Run("expiration keeps decision time without settled_at", func(t *testing.T) {
		data := buildExpiredPendingTimingBackup(t, t0, nil)
		w2, err := restoreAt(data, t0.Add(72*time.Hour))
		if err != nil {
			t.Fatalf("legal expiration must restore: %v", err)
		}
		r, err := w2.Request("u1", "big")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestExpired {
			t.Fatalf("state = %v, want RequestExpired", r.State)
		}
		if r.ActualFee != 0 || !r.SettledAt.IsZero() {
			t.Fatalf("expired request must have no settlement info: fee %d settled_at %v", r.ActualFee, r.SettledAt)
		}
		if !r.DecidedAt.Equal(t0.Add(2 * time.Hour)) {
			t.Fatalf("decided_at = %v, want %v", r.DecidedAt, t0.Add(2*time.Hour))
		}
	})
}

// TestRestoreSettledWithZeroActualFeeRemainsLegal 验证实际完成结算且实际
// 费用为零的请求仍是合法的已结算历史：保留结算时间，策略已花费总额为零，
// 恢复不追加退款记录。
func TestRestoreSettledWithZeroActualFeeRemainsLegal(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	settledAt := t0.Add(time.Hour)

	for _, approved := range []bool{false, true} {
		name := "direct"
		if approved {
			name = "approved"
		}
		t.Run(name, func(t *testing.T) {
			data := buildSettleWindowBackup(t, t0, settleWindowOpts{
				approved:  approved,
				actualFee: 0,
				settledAt: settledAt,
			})
			w2, err := restoreAt(data, t0.Add(72*time.Hour))
			if err != nil {
				t.Fatalf("zero-fee settled request must restore: %v", err)
			}
			r, err := w2.Request("u1", "late")
			if err != nil {
				t.Fatal(err)
			}
			if r.State != RequestSettled {
				t.Fatalf("state = %v, want RequestSettled", r.State)
			}
			if r.ActualFee != 0 {
				t.Fatalf("actual fee = %d, want 0", r.ActualFee)
			}
			if !r.SettledAt.Equal(settledAt) {
				t.Fatalf("settled_at = %v, want %v", r.SettledAt, settledAt)
			}
		})
	}
}
