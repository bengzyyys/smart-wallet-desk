package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildDeactivatedPolicyBackup 构造一个账户/余额/计时完全自洽的备份：
//   - 出资账户 payer（余额随 needReserve 调整）与使用账户 u1；会话 s1 属于
//     u1（t0+24h 到期），sa 属于 payer；
//   - 策略 p-deact 开启审批（门槛 10、等待 1h、不启用预留超时），于 t0-1m
//     被 payer 以 "stop" 停用；其历史请求全部形成于停用之前（t0-2h）；
//   - 策略 p-ok 条件相同但未停用，下有一笔 t0 提交、t0+1h 到期的合法待审批
//     请求；
//   - conflict 为 true 时，p-deact 额外携带一笔 t0 提交、仍处于待审批状态的
//     请求——这是正常停用流程不可能产生的矛盾状态。
//
// 历史请求：held（费用 5，直接受理后一直已预留）、rej（费用 20，提交当时
// 被拒绝）、exp（费用 20，等待到期后过期）。
func buildDeactivatedPolicyBackup(t *testing.T, t0 time.Time, conflict bool) []byte {
	t.Helper()
	old := t0.Add(-2 * time.Hour) // 停用前历史请求的提交时刻
	reserve := int64(5)
	available := int64(100) - reserve

	pDeact := policyBackupV1{
		ID: "p-deact", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "charge", Payee: "shop",
		StartsAt:             timeJSON(t0.Add(-24 * time.Hour)),
		EndsAt:               timeJSON(t0.Add(24 * time.Hour)),
		MaxPerRequest:        100,
		MaxTotal:             1000,
		ApprovalThreshold:    10,
		ApprovalWait:         durationJSON(time.Hour),
		ReservedTotal:        reserve,
		Deactivated:          true,
		DeactivatedAt:        timeJSON(t0.Add(-time.Minute)),
		DeactivatorAccountID: "payer",
		DeactivateReason:     "stop",
	}
	pOK := policyBackupV1{
		ID: "p-ok", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "charge", Payee: "shop",
		StartsAt:          timeJSON(t0.Add(-24 * time.Hour)),
		EndsAt:            timeJSON(t0.Add(24 * time.Hour)),
		MaxPerRequest:     100,
		MaxTotal:          1000,
		ApprovalThreshold: 10,
		ApprovalWait:      durationJSON(time.Hour),
	}

	requests := []requestBackupV1{
		// 停用前直接受理、一直已预留的请求继续占用 5。
		{
			PolicyID: "p-deact", RequestID: "held", AccountID: "u1", PayerAccountID: "payer",
			SessionID: "s1", Operation: "charge", Payee: "shop",
			EstimatedFee: 5, State: int(RequestReserved),
			CreatedAt:  timeJSON(old),
			ReservedAt: timeJSON(old),
		},
		// 停用前已被拒绝的请求：决定信息与理由原样保留。
		{
			PolicyID: "p-deact", RequestID: "rej", AccountID: "u1", PayerAccountID: "payer",
			SessionID: "s1", Operation: "charge", Payee: "shop",
			EstimatedFee: 20, State: int(RequestRejected),
			CreatedAt: timeJSON(old), WaitDeadline: timeJSON(old.Add(time.Hour)),
			DecidedAt: timeJSON(old), ApproverAccountID: "payer", RejectReason: "manual no",
		},
		// 停用前已等待过期的请求：过期决定原样保留。
		{
			PolicyID: "p-deact", RequestID: "exp", AccountID: "u1", PayerAccountID: "payer",
			SessionID: "s1", Operation: "charge", Payee: "shop",
			EstimatedFee: 20, State: int(RequestExpired),
			CreatedAt: timeJSON(old), WaitDeadline: timeJSON(old.Add(time.Hour)),
			DecidedAt: timeJSON(old.Add(time.Hour)),
		},
		// 未停用策略下的合法待审批请求。
		{
			PolicyID: "p-ok", RequestID: "pend", AccountID: "u1", PayerAccountID: "payer",
			SessionID: "s1", Operation: "charge", Payee: "shop",
			EstimatedFee: 20, State: int(RequestPendingApproval),
			CreatedAt: timeJSON(t0), WaitDeadline: timeJSON(t0.Add(time.Hour)),
		},
	}
	if conflict {
		requests = append(requests, requestBackupV1{
			PolicyID: "p-deact", RequestID: "conflict", AccountID: "u1", PayerAccountID: "payer",
			SessionID: "s1", Operation: "charge", Payee: "shop",
			EstimatedFee: 20, State: int(RequestPendingApproval),
			CreatedAt: timeJSON(t0), WaitDeadline: timeJSON(t0.Add(time.Hour)),
		})
	}

	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: available, Reserved: reserve, CreatedAt: timeJSON(t0.Add(-48 * time.Hour))},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0.Add(-48 * time.Hour))},
		},
		Sessions: []sessionBackupV1{
			{ID: "s1", AccountID: "u1", DeviceID: "d1", ExpiresAt: timeJSON(t0.Add(24 * time.Hour)), CreatedAt: timeJSON(t0.Add(-48 * time.Hour))},
			{ID: "sa", AccountID: "payer", DeviceID: "dev-pay", ExpiresAt: timeJSON(t0.Add(24 * time.Hour)), CreatedAt: timeJSON(t0.Add(-48 * time.Hour))},
		},
		Policies: []policyBackupV1{pDeact, pOK},
		Requests: requests,
		Ledger:   []ledgerEntryBackupV1{},
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// assertDeactivatedPendingRejected 断言恢复失败、包装 ErrBackupInvalid，错误
// 信息点名使用账户、请求编号与策略编号并说明已停用策略不能保留待审批请求，
// 且不返回任何钱包。
func assertDeactivatedPendingRejected(t *testing.T, data []byte, at time.Time) string {
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
	for _, want := range []string{`"u1"`, `"conflict"`, `"p-deact"`, "deactivated", "pending"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q must contain %q so callers can locate the conflict", msg, want)
		}
	}
	return msg
}

// TestRestoreRejectsPendingUnderDeactivatedPolicy 验证核心修复：备份中只要有
// 一笔待审批请求关联已停用策略，无论恢复时刻在等待期限之前、恰好到达还是
// 早已过去，都整体拒绝恢复——不能先把请求自动变成过期再接受备份。
func TestRestoreRejectsPendingUnderDeactivatedPolicy(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildDeactivatedPolicyBackup(t, t0, true)

	// conflict 于 t0 提交，等待截止 t0+1h。
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"before wait deadline", t0.Add(10 * time.Minute)},
		{"exactly at wait deadline", t0.Add(time.Hour)},
		{"long after wait deadline", t0.Add(48 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertDeactivatedPendingRejected(t, data, tc.at)
		})
	}

	// 公开入口同样拒绝（真实当前时钟），结论只取决于备份状态。
	if w2, err := Restore(data); err == nil || w2 != nil {
		t.Fatalf("public Restore: w2=%v err=%v", w2, err)
	} else if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("public Restore err = %v, want ErrBackupInvalid", err)
	}
}

// TestRestoreRejectsDeactivatedPendingDespiteOtherLegalRecords 验证同一备份中
// 其他请求与策略全部合法（停用策略下的已预留/已拒绝/已过期历史、另一未停用
// 策略下的合法待审批请求）也不能让这笔冲突被忽略；错误必须定位到冲突记录
// 而不是其他合法记录。
func TestRestoreRejectsDeactivatedPendingDespiteOtherLegalRecords(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildDeactivatedPolicyBackup(t, t0, true)
	msg := assertDeactivatedPendingRejected(t, data, t0.Add(10*time.Minute))
	// 合法记录不应被误报为冲突。
	if strings.Contains(msg, "p-ok") || strings.Contains(msg, `"held"`) ||
		strings.Contains(msg, `"rej"`) || strings.Contains(msg, `"exp"`) || strings.Contains(msg, `"pend"`) {
		t.Fatalf("error must pinpoint the conflicting record only: %q", msg)
	}
}

// TestRestoreAcceptsDeactivatedPolicyHistory 验证已停用策略本身仍是可恢复的
// 数据：停用前已预留、已拒绝、已过期的合法请求与停用信息、余额、账本顺序
// 全部原样保留；未停用策略下的合法待审批请求仍可恢复、继续审批，到期时仍
// 按原规则过期；恢复不替任何请求追加停用或拒绝记录。
func TestRestoreAcceptsDeactivatedPolicyHistory(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("history preserved and active pending still approvable", func(t *testing.T) {
		data := buildDeactivatedPolicyBackup(t, t0, false)
		w2, err := restoreAt(data, t0.Add(10*time.Minute))
		if err != nil {
			t.Fatalf("legal deactivated-policy history must restore: %v", err)
		}

		pd, err := w2.Policy("p-deact")
		if err != nil {
			t.Fatal(err)
		}
		if !pd.Deactivated || pd.DeactivatorAccountID != "payer" || pd.DeactivateReason != "stop" ||
			!pd.DeactivatedAt.Equal(t0.Add(-time.Minute)) {
			t.Fatalf("deactivation info not preserved: %+v", pd)
		}

		if r, _ := w2.Request("u1", "held"); r.State != RequestReserved {
			t.Fatalf("held state = %v, want reserved", r.State)
		}
		if r, _ := w2.Request("u1", "rej"); r.State != RequestRejected || r.RejectReason != "manual no" ||
			r.ApproverAccountID != "payer" {
			t.Fatalf("rejected history rewritten: %+v", r)
		}
		if r, _ := w2.Request("u1", "exp"); r.State != RequestExpired || r.RejectReason != "" {
			t.Fatalf("expired history rewritten: %+v", r)
		}
		// 余额不变：held 的 5 仍预留，其余历史不占用资金。
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 95, Reserved: 5}) {
			t.Fatalf("balance = %+v, want {95 5}", bal)
		}

		// 未停用策略下的待审批请求仍可被出资账户批准并预留费用。
		if r, _ := w2.Request("u1", "pend"); r.State != RequestPendingApproval {
			t.Fatalf("pend state = %v, want still pending", r.State)
		}
		if _, err := w2.Approve("u1", "pend", "sa", "dev-pay"); err != nil {
			t.Fatalf("approve legal pending after restore: %v", err)
		}
		if r, _ := w2.Request("u1", "pend"); r.State != RequestReserved {
			t.Fatalf("pend state = %v, want reserved after approval", r.State)
		}
		// 批准只影响 p-ok：p-deact 的已预留请求不动。
		if r, _ := w2.Request("u1", "held"); r.State != RequestReserved {
			t.Fatalf("deactivated policy reservation touched by other approval: %+v", r)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 75, Reserved: 25}) {
			t.Fatalf("balance after approval = %+v, want {75 25}", bal)
		}
		pv, _ := w2.Policy("p-deact")
		if pv.ReservedTotal != 5 {
			t.Fatalf("deactivated policy reserved total changed: %d", pv.ReservedTotal)
		}
	})

	t.Run("active pending still expires by existing rule long after restore", func(t *testing.T) {
		data := buildDeactivatedPolicyBackup(t, t0, false)
		w2, err := restoreAt(data, t0.Add(48*time.Hour))
		if err != nil {
			t.Fatalf("legal history must restore long after deadline: %v", err)
		}
		// 未停用策略下到期的待审批请求按原规则进入过期终态。
		if r, _ := w2.Request("u1", "pend"); r.State != RequestExpired {
			t.Fatalf("pend state = %v, want expired at restore", r.State)
		}
		// 停用策略的历史终态一律不改写。
		if r, _ := w2.Request("u1", "held"); r.State != RequestReserved {
			t.Fatalf("held state = %v, want still reserved", r.State)
		}
		if r, _ := w2.Request("u1", "rej"); r.State != RequestRejected {
			t.Fatalf("rej state = %v, want rejected preserved", r.State)
		}
		if r, _ := w2.Request("u1", "exp"); r.State != RequestExpired {
			t.Fatalf("exp state = %v, want expired preserved", r.State)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 95, Reserved: 5}) {
			t.Fatalf("balance = %+v, want {95 5}", bal)
		}
	})
}

// TestRestoreRejectsExportTamperedToDeactivatePolicyWithPending 从真实钱包导出
// 一份完全合法的备份（含一笔等待期限内的待审批请求），仅把策略篡改为已停用
// 并补全停用信息：请求的账户与会话引用、审批门槛、等待截止、账户与策略金额
// 全部仍符合其他校验，恢复也必须失败。
func TestRestoreRejectsExportTamperedToDeactivatePolicyWithPending(t *testing.T) {
	w, c := setupApproval(t)
	if _, err := w.Apply(approvalApply()); err != nil { // r1 费用 20，等待截止 t0+1m
		t.Fatal(err)
	}
	good, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	m := mustMap(t, good)
	for _, p := range m["policies"].([]interface{}) {
		pm := p.(map[string]interface{})
		if pm["id"] == "p-approval" {
			pm["deactivated"] = true
			pm["deactivated_at"] = m["exported_at"]
			pm["deactivator_account_id"] = "payer"
			pm["deactivate_reason"] = "stop"
		}
	}
	bad := mustRemap(t, m)

	t0 := c.t
	// 篡改前该备份在各时刻均可恢复，作为对照。
	if _, err := restoreAt(good, t0); err != nil {
		t.Fatalf("untampered backup must restore: %v", err)
	}
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"before deadline", t0},
		{"exactly at deadline", t0.Add(time.Minute)},
		{"long after deadline", t0.Add(30 * time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w2, err := restoreAt(bad, tc.at)
			if err == nil || w2 != nil {
				t.Fatalf("tampered backup restored at %s: w2=%v err=%v", tc.name, w2, err)
			}
			if !errors.Is(err, ErrBackupInvalid) {
				t.Fatalf("err = %v, want ErrBackupInvalid", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, `"u1"`) || !strings.Contains(msg, `"r1"`) ||
				!strings.Contains(msg, `"p-approval"`) || !strings.Contains(msg, "deactivated") {
				t.Fatalf("error %q must name account, request, policy and the deactivation conflict", msg)
			}
		})
	}
}
