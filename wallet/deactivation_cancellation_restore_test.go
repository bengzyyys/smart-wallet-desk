package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildDeactivatedPendingCancelBackup 构造一笔“超门槛进入待审批、从未预留
// 费用、随后被取消”的请求备份，关联策略已停用。默认历史合法：提交于 t0、
// 等待截止 t0+1h（策略窗口至 t0+24h、会话至 t0+24h，均不抢先）、策略于
// t0+10m 首次停用、请求于 t0+5m（停用之前）取消。mutate 在余额核对前调整
// 请求/策略/会话字段，以便构造各类停用与取消时间矛盾的历史。
func buildDeactivatedPendingCancelBackup(t *testing.T, t0 time.Time, mutate func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)) []byte {
	t.Helper()
	r := requestBackupV1{
		PolicyID: "p", RequestID: "big", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee: 20,
		State:        int(RequestCancelled),
		CreatedAt:    timeJSON(t0),
		WaitDeadline: timeJSON(t0.Add(time.Hour)),
		DecidedAt:    timeJSON(t0.Add(5 * time.Minute)),
	}
	pol := policyBackupV1{
		ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "charge", Payee: "shop",
		StartsAt:             timeJSON(t0.Add(-time.Hour)),
		EndsAt:               timeJSON(t0.Add(24 * time.Hour)),
		MaxPerRequest:        100,
		MaxTotal:             1000,
		ApprovalThreshold:    10,
		ApprovalWait:         durationJSON(time.Hour),
		MaxReserveDuration:   durationJSON(2 * time.Hour),
		Deactivated:          true,
		DeactivatedAt:        timeJSON(t0.Add(10 * time.Minute)),
		DeactivatorAccountID: "payer",
		DeactivateReason:     "stop it",
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
		ExportedAt: timeJSON(t0.Add(30 * time.Minute)),
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

// assertDeactivatedPendingCancelRejected 恢复必须失败、包装 ErrBackupInvalid，
// 并在错误信息中点名使用账户、请求编号与策略编号；不得返回部分恢复的钱包。
func assertDeactivatedPendingCancelRejected(t *testing.T, data []byte, at time.Time) string {
	t.Helper()
	w2, err := restoreAt(data, at)
	if err == nil {
		if w2 != nil {
			t.Fatalf("contradictory backup restored a wallet: %+v", w2)
		}
		t.Fatal("contradictory backup restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	for _, want := range []string{`"u1"`, `"big"`, `"p"`} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q must identify %s (usage account, request, policy)", msg, want)
		}
	}
	return msg
}

// TestRestoreRejectsPendingCancelAfterDeactivation 验证核心修复：待审批阶段
// 取消、从未预留费用的请求，其取消决定时刻严格晚于关联策略的首次停用时刻时，
// 整个备份必须被拒绝——停用瞬间全部待审批请求已结清，正常流程不可能在停用
// 之后才取消。即使会话、策略条件、余额及累计金额全部合法也不能放行；判断只
// 看备份记载的历史，与恢复时等待期限是否已经过去无关。
func TestRestoreRejectsPendingCancelAfterDeactivation(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		mutate func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)
	}{
		{
			// 题目示例：12:00 提交、13:00 等待截止、12:10 停用、12:20 取消。
			name: "cancel decided after deactivation",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(t0.Add(20 * time.Minute))
			},
		},
		{
			name: "cancel one nanosecond after deactivation",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(t0.Add(10*time.Minute + time.Nanosecond))
			},
		},
		{
			// 停用时刻用另一时区表示同一瞬间，取消仍晚一纳秒：时区写法不改变结论。
			name: "timezone representation does not hide the contradiction",
			mutate: func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
				p.DeactivatedAt = timeJSON(t0.Add(10 * time.Minute).In(time.FixedZone("UTC+2", 2*3600)))
				r.DecidedAt = timeJSON(t0.Add(10*time.Minute + time.Nanosecond))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := buildDeactivatedPendingCancelBackup(t, t0, tc.mutate)
			// 恢复时刻远近都不影响结论：等待期限未到、早已过去结果相同。
			for _, at := range []time.Time{t0.Add(30 * time.Minute), t0.Add(48 * time.Hour)} {
				msg := assertDeactivatedPendingCancelRejected(t, data, at)
				if !strings.Contains(msg, "cancelled") || !strings.Contains(msg, "deactivated") {
					t.Fatalf("error %q must explain the cancellation is later than the first deactivation", msg)
				}
			}
		})
	}
}

// TestRestorePendingCancelBeforeDeactivationAccepted 验证合法历史照常恢复：
// 取消决定早于首次停用、或与停用记为同一绝对时刻（取消先完成、停用随后完成）
// 的待审批取消，仍恢复为已取消，提交时间、等待截止时间、取消决定时间、账户
// 余额与策略累计金额保持原样，不新增退款或停用拒绝记录。
func TestRestorePendingCancelBeforeDeactivationAccepted(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("cancel before deactivation restores unchanged", func(t *testing.T) {
		data := buildDeactivatedPendingCancelBackup(t, t0, nil)
		w2, err := restoreAt(data, t0.Add(48*time.Hour))
		if err != nil {
			t.Fatalf("legal cancellation before deactivation must restore: %v", err)
		}
		r, err := w2.Request("u1", "big")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestCancelled {
			t.Fatalf("state = %v, want cancelled", r.State)
		}
		if !r.CreatedAt.Equal(t0) || !r.WaitDeadline.Equal(t0.Add(time.Hour)) ||
			!r.DecidedAt.Equal(t0.Add(5*time.Minute)) {
			t.Fatalf("timing rewritten: created %v deadline %v decided %v",
				r.CreatedAt, r.WaitDeadline, r.DecidedAt)
		}
		// 终态不改写：不追加退款、停用拒绝或过期记录，余额与额度不变。
		if got := len(w2.Ledger()); got != 0 {
			t.Fatalf("restore appended %d ledger entries", got)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
			t.Fatalf("payer balance = %+v, want untouched", bal)
		}
		pv, err := w2.Policy("p")
		if err != nil {
			t.Fatal(err)
		}
		if !pv.Deactivated || !pv.DeactivatedAt.Equal(t0.Add(10*time.Minute)) ||
			pv.DeactivatorAccountID != "payer" || pv.DeactivateReason != "stop it" {
			t.Fatalf("deactivation info not preserved: %+v", pv)
		}
	})

	t.Run("cancel at the exact deactivation instant restores", func(t *testing.T) {
		data := buildDeactivatedPendingCancelBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0.Add(10 * time.Minute))
		})
		if _, err := restoreAt(data, t0.Add(30*time.Minute)); err != nil {
			t.Fatalf("cancellation at the deactivation instant must be legal: %v", err)
		}
	})

	t.Run("same instant in another timezone restores", func(t *testing.T) {
		data := buildDeactivatedPendingCancelBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0.Add(10 * time.Minute).In(time.FixedZone("UTC+2", 2*3600)))
		})
		if _, err := restoreAt(data, t0.Add(30*time.Minute)); err != nil {
			t.Fatalf("same instant in another zone must restore: %v", err)
		}
	})

	t.Run("cancel one nanosecond before deactivation restores", func(t *testing.T) {
		data := buildDeactivatedPendingCancelBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0.Add(10*time.Minute - time.Nanosecond))
		})
		if _, err := restoreAt(data, t0.Add(30*time.Minute)); err != nil {
			t.Fatalf("cancellation 1ns before deactivation must be legal: %v", err)
		}
	})

	t.Run("pending cancel under an active policy keeps existing rules", func(t *testing.T) {
		// 未停用策略下的待审批取消不受本核对限制：取消晚于“假想停用时刻”也合法。
		data := buildDeactivatedPendingCancelBackup(t, t0, func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
			p.Deactivated = false
			p.DeactivatedAt = timeJSON(time.Time{})
			p.DeactivatorAccountID = ""
			p.DeactivateReason = ""
			r.DecidedAt = timeJSON(t0.Add(20 * time.Minute))
		})
		w2, err := restoreAt(data, t0.Add(30*time.Minute))
		if err != nil {
			t.Fatalf("pending cancel under an active policy must restore: %v", err)
		}
		r, err := w2.Request("u1", "big")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestCancelled || !r.DecidedAt.Equal(t0.Add(20*time.Minute)) {
			t.Fatalf("request = %+v, want cancelled at t0+20m", r)
		}
	})
}

// TestRestoreReservedCancelAfterDeactivationUnaffected 验证两种已预留费用的
// 取消不被本次核对误伤：停用前已实际预留的请求本就可以在停用后取消并退回
// 预留；经批准后预留再取消的请求沿用既有规则，其审批决定时间不是取消时间。
func TestRestoreReservedCancelAfterDeactivationUnaffected(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("directly reserved before deactivation, cancelled after", func(t *testing.T) {
		// 未超门槛直接预留（费用 5 ≤ 门槛 10），提交时刻 t0 即完成预留，
		// 之后取消：直接受理的请求本就不携带决定时间。
		data := buildDeactivatedPendingCancelBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
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
		if r.State != RequestCancelled || !r.ReservedAt.Equal(t0) || !r.DecidedAt.IsZero() {
			t.Fatalf("reserved-origin cancellation rewritten: %+v", r)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
			t.Fatalf("payer balance = %+v, want untouched", bal)
		}
	})

	t.Run("approved before deactivation, reserved then cancelled after", func(t *testing.T) {
		// 超门槛：t0+5m（停用前）批准并预留，停用后才取消。保存的决定时间是
		// 批准时刻，不是取消时刻，不得被当成停用后取消而拒绝。
		data := buildDeactivatedPendingCancelBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.ReservedAt = timeJSON(t0.Add(5 * time.Minute))
			r.DecidedAt = timeJSON(t0.Add(5 * time.Minute))
			r.ApproverAccountID = "payer"
			r.ReserveDuration = durationJSON(2 * time.Hour)
			r.ReserveDeadline = timeJSON(t0.Add(5*time.Minute + 2*time.Hour))
		})
		w2, err := restoreAt(data, t0.Add(48*time.Hour))
		if err != nil {
			t.Fatalf("approved-then-cancelled request must restore: %v", err)
		}
		r, err := w2.Request("u1", "big")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestCancelled || r.ApproverAccountID != "payer" ||
			!r.DecidedAt.Equal(t0.Add(5*time.Minute)) {
			t.Fatalf("approved-then-cancelled request rewritten: %+v", r)
		}
	})
}

// TestRestorePendingCancelAfterDeactivationEndToEnd 用真实钱包操作生成合法
// 备份（等待期限内先取消、再停用），确认恢复后逐字节一致；再手工把取消决定
// 时刻改到停用之后，确认同一份备份立即变成整体无效。
func TestRestorePendingCancelAfterDeactivationEndToEnd(t *testing.T) {
	w, c := newTestWallet()
	t0 := c.t
	mustAccount(t, w, "payer", 1000)
	mustAccount(t, w, "u1", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", t0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "deva", t0.Add(24*time.Hour)); err != nil {
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
	if _, err := w.Apply(RequestInput{
		PolicyID: "p", RequestID: "big", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 20,
	}); err != nil {
		t.Fatal(err)
	}
	// 等待期限内先取消，再停用策略：合法的“取消早于停用”历史。
	c.t = t0.Add(5 * time.Minute)
	if _, err := w.Cancel("u1", "big"); err != nil {
		t.Fatal(err)
	}
	c.t = t0.Add(10 * time.Minute)
	if _, err := w.DeactivatePolicy("p", "sa", "deva", "stop it"); err != nil {
		t.Fatal(err)
	}
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}

	w2, err := restoreAt(data, t0.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("legal cancel-before-deactivation history must restore: %v", err)
	}
	r, err := w2.Request("u1", "big")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != RequestCancelled || !r.DecidedAt.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("request = %+v, want cancelled at t0+5m", r)
	}
	// 恢复不新增账本记录：同刻恢复再导出逐字节一致。
	w3, err := restoreAt(data, t0.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(w3.Ledger()); got != len(w.Ledger()) {
		t.Fatalf("ledger entries changed: got %d want %d", got, len(w.Ledger()))
	}
	data2, err := w3.Export()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(data2) {
		t.Fatal("restore added records for a legal cancel-before-deactivation backup")
	}

	// 把取消决定时刻改到首次停用之后：同一份备份立即整体无效。
	var b backupV1
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	for i := range b.Requests {
		if b.Requests[i].RequestID == "big" {
			b.Requests[i].DecidedAt = timeJSON(t0.Add(20 * time.Minute))
		}
	}
	tampered, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	msg := assertDeactivatedPendingCancelRejected(t, tampered, t0.Add(48*time.Hour))
	if !strings.Contains(msg, "cancelled") || !strings.Contains(msg, "deactivated") {
		t.Fatalf("error %q must explain the cancellation is later than the first deactivation", msg)
	}
}
