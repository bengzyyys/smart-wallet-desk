package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildDeactivatedCancelBackup 构造一笔“超门槛待审批后被取消、从未预留费用”
// 的请求备份，关联策略已停用：提交于 t0、等待截止 t0+1h（策略窗口至 t0+24h、
// 会话至 t0+24h，均不抢先）、策略于 t0+10m 首次停用，请求默认保存为 t0+20m
// 取消——即取消决定晚于首次停用的矛盾历史。mutate 在恢复前调整请求/策略/
// 会话字段，以便构造取消早于停用、与停用同刻等合法历史及其他变体。
func buildDeactivatedCancelBackup(t *testing.T, t0 time.Time, mutate func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)) []byte {
	t.Helper()
	r := requestBackupV1{
		PolicyID: "p", RequestID: "big", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee: 20,
		State:        int(RequestCancelled),
		CreatedAt:    timeJSON(t0),
		WaitDeadline: timeJSON(t0.Add(time.Hour)),
		DecidedAt:    timeJSON(t0.Add(20 * time.Minute)),
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

// assertDeactivatedCancelRejected 恢复必须失败、包装 ErrBackupInvalid、不返回
// 钱包，且错误信息点名使用账户、请求编号与策略编号，并说明取消决定晚于策略
// 首次停用。
func assertDeactivatedCancelRejected(t *testing.T, data []byte, at time.Time) string {
	t.Helper()
	w2, err := restoreAt(data, at)
	if err == nil {
		if w2 != nil {
			t.Fatalf("contradictory cancellation history restored a wallet: %+v", w2)
		}
		t.Fatal("contradictory cancellation history restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	for _, want := range []string{`"u1"`, `"big"`, `"p"`, "cancelled from pending approval", "first deactivated"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q must identify %q (usage account, request, policy, conflict)", msg, want)
		}
	}
	return msg
}

// TestRestoreRejectsPendingCancelAfterDeactivation 验证核心修复：策略停用会
// 立即结清其全部待审批请求，因此“待审批取消的决定时刻严格晚于策略首次停用
// 时刻”是正常流程无法产生的历史——哪怕会话、策略条件、余额与累计金额全部
// 合法，也必须整体拒绝，且与恢复时刻是否已过等待期限无关。
func TestRestoreRejectsPendingCancelAfterDeactivation(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("cancel after deactivation", func(t *testing.T) {
		// 提交 12:00、等待截止 13:00、策略 12:10 停用、保存为 12:20 取消。
		data := buildDeactivatedCancelBackup(t, t0, nil)
		// 等待期限未到、恰到、早已过去：结论相同，只按备份记载的历史判断。
		for _, at := range []time.Time{
			t0.Add(15 * time.Minute),
			t0.Add(time.Hour),
			t0.Add(48 * time.Hour),
		} {
			assertDeactivatedCancelRejected(t, data, at)
		}
	})

	t.Run("cancel one nanosecond after deactivation", func(t *testing.T) {
		data := buildDeactivatedCancelBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0.Add(10*time.Minute + time.Nanosecond))
		})
		assertDeactivatedCancelRejected(t, data, t0.Add(15*time.Minute))
	})

	t.Run("same instant later by one nanosecond in another zone", func(t *testing.T) {
		// 晚一纳秒用另一时区表示，仍属于停用后取消。
		data := buildDeactivatedCancelBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0.Add(10*time.Minute + time.Nanosecond).In(time.FixedZone("UTC+2", 2*3600)))
		})
		assertDeactivatedCancelRejected(t, data, t0.Add(15*time.Minute))
	})

	t.Run("other legal records cannot rescue it", func(t *testing.T) {
		// 同一备份再包含一条未停用策略及其合法待审批请求，矛盾备份仍整体拒绝。
		data := buildDeactivatedCancelBackup(t, t0, nil)
		var b backupV1
		if err := json.Unmarshal(data, &b); err != nil {
			t.Fatal(err)
		}
		b.Policies = append(b.Policies, policyBackupV1{
			ID: "p2", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
			Operation: "charge", Payee: "shop",
			StartsAt:          timeJSON(t0.Add(-time.Hour)),
			EndsAt:            timeJSON(t0.Add(24 * time.Hour)),
			MaxPerRequest:     100,
			MaxTotal:          1000,
			ApprovalThreshold: 10,
			ApprovalWait:      durationJSON(time.Hour),
		})
		b.Requests = append(b.Requests, requestBackupV1{
			PolicyID: "p2", RequestID: "ok", AccountID: "u1", PayerAccountID: "payer",
			SessionID: "s1", Operation: "charge", Payee: "shop",
			EstimatedFee: 20,
			State:        int(RequestPendingApproval),
			CreatedAt:    timeJSON(t0),
			WaitDeadline: timeJSON(t0.Add(time.Hour)),
		})
		data, err := json.Marshal(&b)
		if err != nil {
			t.Fatal(err)
		}
		assertDeactivatedCancelRejected(t, data, t0.Add(15*time.Minute))
	})
}

// TestRestoreAcceptsPendingCancelAtOrBeforeDeactivation 验证取消决定不晚于策略
// 首次停用的合法历史照常恢复：取消与停用同一绝对时刻（取消先完成、停用随后
// 完成）可以接受，取消早于停用也可以接受；恢复后保持已取消状态、全部时刻、
// 余额与账本原样，不新增退款或停用拒绝记录。
func TestRestoreAcceptsPendingCancelAtOrBeforeDeactivation(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("cancel before deactivation restores unchanged", func(t *testing.T) {
		data := buildDeactivatedCancelBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0.Add(5 * time.Minute))
		})
		w2, err := restoreAt(data, t0.Add(48*time.Hour))
		if err != nil {
			t.Fatalf("cancellation before deactivation must restore: %v", err)
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

	t.Run("cancel exactly at deactivation instant restores", func(t *testing.T) {
		data := buildDeactivatedCancelBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0.Add(10 * time.Minute))
		})
		if _, err := restoreAt(data, t0.Add(15*time.Minute)); err != nil {
			t.Fatalf("cancellation at the deactivation instant must be legal: %v", err)
		}
	})

	t.Run("cancel one nanosecond before deactivation restores", func(t *testing.T) {
		data := buildDeactivatedCancelBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0.Add(10*time.Minute - time.Nanosecond))
		})
		if _, err := restoreAt(data, t0.Add(15*time.Minute)); err != nil {
			t.Fatalf("cancellation 1ns before deactivation must be legal: %v", err)
		}
	})

	t.Run("deactivation instant in another zone restores", func(t *testing.T) {
		// 取消与停用为同一绝对时刻，只是用了不同时区表示：判定相同。
		data := buildDeactivatedCancelBackup(t, t0, func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
			p.DeactivatedAt = timeJSON(t0.Add(10 * time.Minute).In(time.FixedZone("UTC-5", -5*3600)))
			r.DecidedAt = timeJSON(t0.Add(10 * time.Minute).In(time.FixedZone("UTC+2", 2*3600)))
		})
		if _, err := restoreAt(data, t0.Add(15*time.Minute)); err != nil {
			t.Fatalf("same instant in another zone must restore: %v", err)
		}
	})
}

// TestRestoreReservedThenCancelledAfterDeactivationUnaffected 验证两种已预留
// 费用的取消不被本次核对误伤：停用前已实际预留的请求本就允许在停用后取消
// 并退回预留；经批准后预留再取消的请求其 decided_at 是批准决定时间，不能
// 被当成取消时间。两条路径均通过真实钱包导出构造。
func TestRestoreReservedThenCancelledAfterDeactivationUnaffected(t *testing.T) {
	build := func(t *testing.T, threshold int64, fee int64, approve bool) ([]byte, *Wallet) {
		t.Helper()
		w, c := newTestWallet()
		t0 := c.t
		mustAccount(t, w, "payer", 1000)
		mustAccount(t, w, "u1", 0)
		if _, err := w.CreateSession("s1", "u1", "dev1", t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := w.CreateSession("sa", "payer", "deva", t0.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := w.SavePolicy(PolicySpec{
			ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
			Operation: "charge", Payee: "shop",
			StartsAt: t0.Add(-time.Hour), EndsAt: t0.Add(time.Hour),
			MaxPerRequest: 100, MaxTotal: 1000,
			ApprovalThreshold: threshold, ApprovalWait: 5 * time.Minute,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Apply(RequestInput{
			PolicyID: "p", RequestID: "r1", AccountID: "u1", SessionID: "s1",
			DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: fee,
		}); err != nil {
			t.Fatal(err)
		}
		if approve {
			if _, err := w.Approve("u1", "r1", "sa", "deva"); err != nil {
				t.Fatal(err)
			}
		}
		// 停用已预留请求所在的策略，随后在停用之后取消该请求并退回预留。
		c.t = t0.Add(time.Minute)
		if _, err := w.DeactivatePolicy("p", "sa", "deva", "stop it"); err != nil {
			t.Fatal(err)
		}
		c.t = t0.Add(2 * time.Minute)
		if _, err := w.Cancel("u1", "r1"); err != nil {
			t.Fatal(err)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		return data, w
	}

	t.Run("directly reserved then cancelled after deactivation", func(t *testing.T) {
		data, w := build(t, 0, 5, false)
		w2, err := restoreAt(data, w.now().Add(time.Hour))
		if err != nil {
			t.Fatalf("reserved-then-cancelled after deactivation must restore: %v", err)
		}
		r, err := w2.Request("u1", "r1")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestCancelled {
			t.Fatalf("state = %v, want cancelled", r.State)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 1000, Reserved: 0}) {
			t.Fatalf("payer balance = %+v, want refunded", bal)
		}
		if got := len(w2.Ledger()); got != len(w.Ledger()) {
			t.Fatalf("ledger entries changed: got %d want %d", got, len(w.Ledger()))
		}
	})

	t.Run("approved then cancelled after deactivation", func(t *testing.T) {
		data, w := build(t, 10, 20, true)
		w2, err := restoreAt(data, w.now().Add(time.Hour))
		if err != nil {
			t.Fatalf("approved-then-cancelled after deactivation must restore: %v", err)
		}
		r, err := w2.Request("u1", "r1")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestCancelled {
			t.Fatalf("state = %v, want cancelled", r.State)
		}
		// decided_at 保存的是批准决定时间（停用之前），不是取消时间。
		if !r.DecidedAt.Equal(r.ReservedAt) || r.ApproverAccountID != "payer" {
			t.Fatalf("approval info not preserved: %+v", r)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 1000, Reserved: 0}) {
			t.Fatalf("payer balance = %+v, want refunded", bal)
		}
		if got := len(w2.Ledger()); got != len(w.Ledger()) {
			t.Fatalf("ledger entries changed: got %d want %d", got, len(w.Ledger()))
		}
	})
}
