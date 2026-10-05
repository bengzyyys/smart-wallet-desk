package wallet

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// assertUnsettledTerminalSettlementRejected 恢复必须失败、包装
// ErrBackupInvalid，错误信息点名使用账户与请求编号，并按 want 子串说清是
// 未结算终态携带了实际费用还是结算时间；不得返回部分恢复的钱包。
func assertUnsettledTerminalSettlementRejected(t *testing.T, data []byte, at time.Time, want string) {
	t.Helper()
	w2, err := restoreAt(data, at)
	if err == nil {
		if w2 != nil {
			t.Fatalf("contradictory unsettled-terminal request restored a wallet: %+v", w2)
		}
		t.Fatal("contradictory unsettled-terminal request restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, `"u1"`) || !strings.Contains(msg, `"big"`) {
		t.Fatalf("error %q must name usage account and request id", msg)
	}
	if !strings.Contains(msg, want) {
		t.Fatalf("error %q must explain %q", msg, want)
	}
}

// TestRestoreRejectsActualFeeOnRejectedRequest 验证已拒绝请求（三条拒绝路径：
// 出资账户主动拒绝、申请会话吊销、策略停用）携带正数实际费用时必须整体拒绝：
// 即使费用不超过预估费用、出资账户预留余额与策略已花费总额均为零、其余引用与
// 审批时刻合法也不能放行；恢复时刻远近不改变结论。
func TestRestoreRejectsActualFeeOnRejectedRequest(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	for _, cause := range []string{"payer", "revoke", "deactivate"} {
		for _, fee := range []int64{1, 5, 20} { // 1/5 小于预估 20，20 恰等于预估
			t.Run(cause+" actual fee "+strconv.FormatInt(fee, 10), func(t *testing.T) {
				data := buildRejectedTimingBackup(t, t0, cause, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
					r.ActualFee = fee
				})
				for _, at := range []time.Time{t0.Add(time.Minute), t0.Add(48 * time.Hour)} {
					assertUnsettledTerminalSettlementRejected(t, data, at, "actual fee")
				}
			})
		}
	}
}

// TestRestoreRejectsSettledAtOnRejectedRequest 验证实际费用为零但携带非空结算
// 时间的已拒绝请求同样无效；三条拒绝路径一致，决定时间（decided_at）照常
// 存在且不被当成结算时间。
func TestRestoreRejectsSettledAtOnRejectedRequest(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	for _, cause := range []string{"payer", "revoke", "deactivate"} {
		t.Run(cause+" zero fee with settled_at", func(t *testing.T) {
			data := buildRejectedTimingBackup(t, t0, cause, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.ActualFee = 0
				r.SettledAt = timeJSON(t0.Add(45 * time.Minute))
			})
			for _, at := range []time.Time{t0.Add(time.Minute), t0.Add(48 * time.Hour)} {
				assertUnsettledTerminalSettlementRejected(t, data, at, "settled-at")
			}
		})

		t.Run(cause+" both actual fee and settled_at", func(t *testing.T) {
			data := buildRejectedTimingBackup(t, t0, cause, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.ActualFee = 5
				r.SettledAt = timeJSON(t0.Add(45 * time.Minute))
			})
			// 两项矛盾并存时，先报实际费用矛盾；整份备份仍必须失败。
			assertUnsettledTerminalSettlementRejected(t, data, t0.Add(48*time.Hour), "actual fee")
		})
	}
}

// TestRestoreRejectsSettlementInfoOnExpiredRequest 验证等待审批过期的请求携带
// 正数实际费用或非空结算时间（或二者并存）时必须整体拒绝，与已拒绝终态同一
// 规则，恢复时刻远近不改变结论。
func TestRestoreRejectsSettlementInfoOnExpiredRequest(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		fee  int64
		at   time.Time
		want string
	}{
		{"positive fee under estimate", 5, time.Time{}, "actual fee"},
		{"positive fee equal to estimate", 20, time.Time{}, "actual fee"},
		{"zero fee with settled_at", 0, t0.Add(90 * time.Minute), "settled-at"},
		{"both fee and settled_at", 5, t0.Add(90 * time.Minute), "actual fee"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settled := tc.at
			data := buildExpiredPendingTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.ActualFee = tc.fee
				if !settled.IsZero() {
					r.SettledAt = timeJSON(settled)
				}
			})
			for _, at := range []time.Time{t0.Add(time.Minute), t0.Add(48 * time.Hour)} {
				assertUnsettledTerminalSettlementRejected(t, data, at, tc.want)
			}
		})
	}
}

// TestRestoreLegalRejectedAndExpiredHistoriesKeepZeroSettlement 验证合法的拒绝
// 与等待审批过期历史照常恢复：保留原状态、预估费用、提交时间、等待截止时间与
// 决定时间，实际费用为零、结算时间为空，决定时间不被误当成结算时间，账本顺序
// 与余额不变；三条拒绝路径的拒绝原因与审批账户信息照常保留。
func TestRestoreLegalRejectedAndExpiredHistoriesKeepZeroSettlement(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	for _, cause := range []string{"payer", "revoke", "deactivate"} {
		t.Run(cause+" legal rejection", func(t *testing.T) {
			data := buildRejectedTimingBackup(t, t0, cause, nil)
			w2, err := restoreAt(data, t0.Add(48*time.Hour))
			if err != nil {
				t.Fatalf("legal rejection must restore: %v", err)
			}
			r, err := w2.Request("u1", "big")
			if err != nil {
				t.Fatal(err)
			}
			if r.State != RequestRejected || r.ActualFee != 0 || !r.SettledAt.IsZero() {
				t.Fatalf("legal rejected request = %+v, want rejected with zero fee and empty settled_at", r)
			}
			if r.DecidedAt.IsZero() || !r.DecidedAt.Equal(t0.Add(30*time.Minute)) {
				t.Fatalf("decision time must be preserved: %v", r.DecidedAt)
			}
			if r.RejectReason == "" {
				t.Fatal("reject reason lost")
			}
			if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
				t.Fatalf("balance = %+v, want untouched", bal)
			}
		})
	}

	t.Run("legal pending-approval expiration", func(t *testing.T) {
		data := buildExpiredPendingTimingBackup(t, t0, nil)
		w2, err := restoreAt(data, t0.Add(48*time.Hour))
		if err != nil {
			t.Fatalf("legal expiration must restore: %v", err)
		}
		r, err := w2.Request("u1", "big")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestExpired || r.ActualFee != 0 || !r.SettledAt.IsZero() {
			t.Fatalf("legal expired request = %+v, want expired with zero fee and empty settled_at", r)
		}
		if !r.DecidedAt.Equal(t0.Add(2 * time.Hour)) {
			t.Fatalf("decision time must be preserved: %v", r.DecidedAt)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
			t.Fatalf("balance = %+v, want untouched", bal)
		}
	})
}

// TestRestoreContradictoryTerminalNotSkippedAlongsideLegalRecords 验证同一备份
// 还包含完全合法的请求时，矛盾记录不能被跳过、清零或清空结算时间后继续恢复：
// 整份备份必须失败，调用方拿不到包含其余合法记录的部分钱包。
func TestRestoreContradictoryTerminalNotSkippedAlongsideLegalRecords(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// big：预估 20、已拒绝，却写入实际费用 5；其余全部合法。
	data := buildRejectedTimingBackup(t, t0, "payer", func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		r.ActualFee = 5
	})
	var b backupV1
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	// 追加另一笔完全合法的已拒绝历史 ok：不占用任何余额或额度，金额核对
	// 依然全部一致——仍必须因为 big 而整体失败。
	b.Requests = append(b.Requests, requestBackupV1{
		PolicyID: "p", RequestID: "ok", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee: 20, State: int(RequestRejected),
		CreatedAt:         timeJSON(t0),
		WaitDeadline:      timeJSON(t0.Add(time.Hour)),
		DecidedAt:         timeJSON(t0.Add(15 * time.Minute)),
		ApproverAccountID: "payer",
		RejectReason:      "duplicate order",
	})
	corrupt, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(corrupt, t0.Add(48*time.Hour))
	if err == nil {
		t.Fatalf("whole backup must fail, got wallet with %d requests", len(w2.requests))
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, `"big"`) || !strings.Contains(msg, "actual fee") {
		t.Fatalf("error must pinpoint the contradictory request big and its fee: %q", msg)
	}
}

// TestRestoreZeroFeeSettledHistoryStillAccepted 区分两类“实际费用为零”的
// 历史：真正完成结算的已结算请求（RequestSettled）携带结算时间与全额退款结果
// 仍合法恢复；只有未结算终态（已拒绝/等待审批过期）携带结算信息才矛盾。
func TestRestoreZeroFeeSettledHistoryStillAccepted(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildSettleWindowBackup(t, t0, settleWindowOpts{
		actualFee: 0, settledAt: t0.Add(time.Hour),
	})
	w2, err := restoreAt(data, t0.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("zero-fee settled history must restore with its settled_at: %v", err)
	}
	r, err := w2.Request("u1", "late")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != RequestSettled || r.ActualFee != 0 || r.SettledAt.IsZero() {
		t.Fatalf("zero-fee settled request = %+v", r)
	}
}
