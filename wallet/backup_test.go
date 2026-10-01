package wallet

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- 测试辅助函数 ----

func mustSession(t *testing.T, w *Wallet, id, accountID, deviceID string, exp time.Time) {
	t.Helper()
	if _, err := w.CreateSession(id, accountID, deviceID, exp); err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

// backupTestWallet 创建一个含账户、会话、策略的钱包，返回钱包与时钟。
func backupTestWallet(t *testing.T) (*Wallet, *clock) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 5000)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	exp := c.t.Add(2 * time.Hour)
	mustSession(t, w, "s1", "u1", "dev1", exp)
	mustSession(t, w, "s2", "u2", "dev2", exp)
	pol := PolicySpec{
		ID: "pol1", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1", "u2"},
		Operation: "pay", Payee: "shop",
		StartsAt: c.t.Add(-time.Hour), EndsAt: c.t.Add(24 * time.Hour),
		MaxPerRequest: 500, MaxTotal: 3000,
	}
	if err := w.SavePolicy(pol); err != nil {
		t.Fatalf("save policy: %v", err)
	}
	return w, c
}

// exportRestore 在同一时刻导出并恢复，返回恢复后的钱包。
func exportRestore(t *testing.T, w *Wallet, c *clock) *Wallet {
	t.Helper()
	data, err := w.Export()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	w2, err := restore(data, func() time.Time { return c.t })
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	return w2
}

// policyViewsEqual 比较两个策略视图，忽略允许使用账户列表的顺序
// （原钱包按 map 迭代顺序返回，恢复后按排序顺序返回）。
func policyViewsEqual(a, b PolicyView) bool {
	if a.PolicySpec.ID != b.PolicySpec.ID ||
		a.PolicySpec.PayerAccountID != b.PolicySpec.PayerAccountID ||
		a.PolicySpec.Operation != b.PolicySpec.Operation ||
		a.PolicySpec.Payee != b.PolicySpec.Payee ||
		!a.PolicySpec.StartsAt.Equal(b.PolicySpec.StartsAt) ||
		!a.PolicySpec.EndsAt.Equal(b.PolicySpec.EndsAt) ||
		a.PolicySpec.MaxPerRequest != b.PolicySpec.MaxPerRequest ||
		a.PolicySpec.MaxTotal != b.PolicySpec.MaxTotal ||
		a.PolicySpec.ApprovalThreshold != b.PolicySpec.ApprovalThreshold ||
		a.PolicySpec.ApprovalWait != b.PolicySpec.ApprovalWait ||
		a.PolicySpec.MaxReserveDuration != b.PolicySpec.MaxReserveDuration ||
		a.ReservedTotal != b.ReservedTotal ||
		a.SpentTotal != b.SpentTotal ||
		a.Deactivated != b.Deactivated ||
		!a.DeactivatedAt.Equal(b.DeactivatedAt) ||
		a.DeactivatorAccountID != b.DeactivatorAccountID ||
		a.DeactivateReason != b.DeactivateReason {
		return false
	}
	if len(a.AllowedAccountIDs) != len(b.AllowedAccountIDs) {
		return false
	}
	aa := make([]string, len(a.AllowedAccountIDs))
	bb := make([]string, len(b.AllowedAccountIDs))
	copy(aa, a.AllowedAccountIDs)
	copy(bb, b.AllowedAccountIDs)
	sort.Strings(aa)
	sort.Strings(bb)
	return reflect.DeepEqual(aa, bb)
}

// ---- 基本往返测试 ----

func TestBackupRoundTrip(t *testing.T) {
	w, c := backupTestWallet(t)

	// 受理一笔直接预留的请求。
	if _, err := w.Apply(RequestInput{
		PolicyID: "pol1", RequestID: "r1", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "pay", Payee: "shop", EstimatedFee: 100,
	}); err != nil {
		t.Fatal(err)
	}
	// 结算一部分。
	if _, err := w.Settle("u1", "r1", 60); err != nil {
		t.Fatal(err)
	}

	// 再受理一笔预留请求（不结算）。
	if _, err := w.Apply(RequestInput{
		PolicyID: "pol1", RequestID: "r2", AccountID: "u2", SessionID: "s2",
		DeviceID: "dev2", Operation: "pay", Payee: "shop", EstimatedFee: 200,
	}); err != nil {
		t.Fatal(err)
	}

	// 导出后恢复，同一时刻不应有到期请求。
	data1, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restore(data1, func() time.Time { return c.t })
	if err != nil {
		t.Fatal(err)
	}
	data2, err := w2.Export()
	if err != nil {
		t.Fatal(err)
	}
	if data1 != data2 {
		t.Fatalf("round-trip JSON mismatch:\n--- original ---\n%s\n--- restored ---\n%s", data1, data2)
	}

	// 逐视图核对。
	acct1, _ := w.Account("payer")
	acct2, _ := w2.Account("payer")
	if !reflect.DeepEqual(acct1, acct2) {
		t.Fatalf("payer account: %+v vs %+v", acct1, acct2)
	}
	sess1, _ := w.Session("s1")
	sess2, _ := w2.Session("s1")
	if !reflect.DeepEqual(sess1, sess2) {
		t.Fatalf("session: %+v vs %+v", sess1, sess2)
	}
	pol1, _ := w.Policy("pol1")
	pol2, _ := w2.Policy("pol1")
	if !policyViewsEqual(pol1, pol2) {
		t.Fatalf("policy: %+v vs %+v", pol1, pol2)
	}
	req1, _ := w.Request("u2", "r2")
	req2, _ := w2.Request("u2", "r2")
	if !reflect.DeepEqual(req1, req2) {
		t.Fatalf("request: %+v vs %+v", req1, req2)
	}
	led1 := w.Ledger()
	led2 := w2.Ledger()
	if !reflect.DeepEqual(led1, led2) {
		t.Fatalf("ledger: %+v vs %+v", led1, led2)
	}
}

func TestBackupEmptyWallet(t *testing.T) {
	w := New()
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := Restore(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(w2.Ledger()) != 0 {
		t.Fatalf("empty restored wallet has ledger entries: %+v", w2.Ledger())
	}
	// 空钱包也能导出，导出不新增备份账本记录。
	data2, err := w2.Export()
	if err != nil {
		t.Fatal(err)
	}
	if data != data2 {
		t.Fatalf("empty wallet round-trip mismatch:\n%s\nvs\n%s", data, data2)
	}
}

// ---- 各类状态的往返 ----

func TestBackupRoundTripAllStates(t *testing.T) {
	w, c := backupTestWallet(t)

	// u3 用于吊销会话场景，需在策略保存前创建。
	mustAccount(t, w, "u3", 0)

	// 待审批请求（大额）。
	pol2 := PolicySpec{
		ID: "pol2", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1", "u3"},
		Operation: "pay", Payee: "shop",
		StartsAt: c.t.Add(-time.Hour), EndsAt: c.t.Add(24 * time.Hour),
		MaxPerRequest: 1000, MaxTotal: 5000,
		ApprovalThreshold: 500, ApprovalWait: time.Hour,
	}
	if err := w.SavePolicy(pol2); err != nil {
		t.Fatal(err)
	}
	// 出资账户会话，用于审批拒绝。
	mustSession(t, w, "spayer", "payer", "devpayer", c.t.Add(2*time.Hour))
	if _, err := w.Apply(RequestInput{
		PolicyID: "pol2", RequestID: "pending", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "pay", Payee: "shop", EstimatedFee: 800,
	}); err != nil {
		t.Fatal(err)
	}

	// 已预留请求。
	if _, err := w.Apply(RequestInput{
		PolicyID: "pol1", RequestID: "reserved", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "pay", Payee: "shop", EstimatedFee: 100,
	}); err != nil {
		t.Fatal(err)
	}

	// 已取消请求。
	if _, err := w.Apply(RequestInput{
		PolicyID: "pol1", RequestID: "cancelled", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "pay", Payee: "shop", EstimatedFee: 50,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Cancel("u1", "cancelled"); err != nil {
		t.Fatal(err)
	}

	// 已拒绝请求（审批拒绝）。
	if _, err := w.Apply(RequestInput{
		PolicyID: "pol2", RequestID: "rejected", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "pay", Payee: "shop", EstimatedFee: 900,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Reject("u1", "rejected", "spayer", "devpayer", "no good"); err != nil {
		t.Fatal(err)
	}

	// 被拒绝的申请（不满足策略条件）也留下账本记录。
	if _, err := w.Apply(RequestInput{
		PolicyID: "pol1", RequestID: "denied", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "pay", Payee: "other", EstimatedFee: 100,
	}); err == nil {
		t.Fatal("expected denial")
	}

	// 吊销会话，使待审批请求进入拒绝终态。
	mustSession(t, w, "s3", "u3", "dev3", c.t.Add(time.Hour))
	if _, err := w.Apply(RequestInput{
		PolicyID: "pol2", RequestID: "revoked", AccountID: "u3", SessionID: "s3",
		DeviceID: "dev3", Operation: "pay", Payee: "shop", EstimatedFee: 700,
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.RevokeSession("s3"); err != nil {
		t.Fatal(err)
	}

	// 停用策略。
	if _, err := w.DeactivatePolicy("pol1", "spayer", "devpayer", "done"); err != nil {
		t.Fatal(err)
	}

	// 导出并恢复。
	data1, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restore(data1, func() time.Time { return c.t })
	if err != nil {
		t.Fatal(err)
	}
	data2, err := w2.Export()
	if err != nil {
		t.Fatal(err)
	}
	if data1 != data2 {
		t.Fatalf("all-states round-trip mismatch:\n--- original ---\n%s\n--- restored ---\n%s", data1, data2)
	}

	// 核对各请求状态。
	checks := []struct {
		account, request string
		state            RequestState
	}{
		{"u1", "pending", RequestPendingApproval},
		{"u1", "reserved", RequestReserved},
		{"u1", "cancelled", RequestCancelled},
		{"u1", "rejected", RequestRejected},
		{"u3", "revoked", RequestRejected},
	}
	for _, chk := range checks {
		r1, err := w.Request(chk.account, chk.request)
		if err != nil {
			t.Fatalf("original request %s/%s: %v", chk.account, chk.request, err)
		}
		r2, err := w2.Request(chk.account, chk.request)
		if err != nil {
			t.Fatalf("restored request %s/%s: %v", chk.account, chk.request, err)
		}
		if r1.State != chk.state || r2.State != chk.state {
			t.Fatalf("request %s/%s state: original=%v restored=%v want=%v",
				chk.account, chk.request, r1.State, r2.State, chk.state)
		}
	}

	// 被拒绝申请的账本记录必须保留。
	led := w2.Ledger()
	found := false
	for _, e := range led {
		if e.Kind == LedgerRejection && e.RequestID == "denied" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("denied application ledger record not preserved: %+v", led)
	}
}

// ---- 校验错误测试 ----

// validBackupJSON 构造一份合法的备份 JSON 文本。
func validBackupJSON(t *testing.T) string {
	w, c := backupTestWallet(t)
	if _, err := w.Apply(RequestInput{
		PolicyID: "pol1", RequestID: "r1", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "pay", Payee: "shop", EstimatedFee: 100,
	}); err != nil {
		t.Fatal(err)
	}
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	_ = c
	return data
}

// mutateBackup 解析备份 JSON，修改后重新序列化。
func mutateBackup(t *testing.T, data string, fn func(*backupFile)) string {
	t.Helper()
	var bf backupFile
	if err := json.Unmarshal([]byte(data), &bf); err != nil {
		t.Fatal(err)
	}
	fn(&bf)
	out, err := json.Marshal(bf)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestBackupRestoreValidation(t *testing.T) {
	valid := validBackupJSON(t)

	tests := []struct {
		name string
		data string
		want string // 期望错误包含的子串
	}{
		{"empty text", "   ", "empty backup data"},
		{"invalid json", "{not json", "invalid backup json"},
		{"unsupported version", `{"version":99}`, "unsupported backup version"},
		{"missing version", `{}`, "unsupported backup version"},
		{"duplicate account", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Accounts = append(bf.Accounts, bf.Accounts[0])
		}), "duplicate account id"},
		{"duplicate session", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Sessions = append(bf.Sessions, bf.Sessions[0])
		}), "duplicate session id"},
		{"duplicate policy", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Policies = append(bf.Policies, bf.Policies[0])
		}), "duplicate policy id"},
		{"duplicate request", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Requests = append(bf.Requests, bf.Requests[0])
		}), "duplicate request"},
		{"unknown request state", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Requests[0].State = RequestState(99)
		}), "unknown state"},
		{"unknown ledger kind", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Ledger[0].Kind = LedgerKind(99)
		}), "unknown ledger kind"},
		{"negative account balance", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Accounts[0].Available = -1
		}), "negative balance"},
		{"negative ledger amount", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Ledger[0].Amount = -1
		}), "negative amount"},
		{"policy missing operation", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Policies[0].Operation = ""
		}), "missing operation or payee"},
		{"policy negative max total", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Policies[0].MaxTotal = -1
		}), "limits must be positive"},
		{"policy invalid window", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Policies[0].StartsAt = bf.Policies[0].EndsAt
		}), "starts-at must be before ends-at"},
		{"policy threshold exceeds limit", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Policies[0].ApprovalThreshold = 99999
		}), "approval threshold"},
		{"policy references missing payer", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Policies[0].PayerAccountID = "ghost"
		}), "missing payer account"},
		{"policy references missing allowed account", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Policies[0].AllowedAccountIDs = append(bf.Policies[0].AllowedAccountIDs, "ghost")
		}), "missing allowed account"},
		{"session references missing account", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Sessions[0].AccountID = "ghost"
		}), "references missing account"},
		{"request references missing account", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Requests[0].AccountID = "ghost"
		}), "references missing account"},
		{"request references missing session", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Requests[0].SessionID = "ghost"
		}), "references missing session"},
		{"request references missing policy", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Requests[0].PolicyID = "ghost"
		}), "references missing policy"},
		{"request session belongs to another account", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Requests[0].SessionID = "s2" // s2 属于 u2，但请求属于 u1
		}), "belongs to account"},
		{"request payer mismatch policy", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Requests[0].PayerAccountID = "u1"
		}), "does not match policy payer"},
		{"request estimated fee not positive", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Requests[0].EstimatedFee = 0
		}), "estimated fee must be positive"},
		{"request negative actual fee", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Requests[0].ActualFee = -1
		}), "actual fee must not be negative"},
		{"ledger missing account id", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Ledger[0].AccountID = ""
		}), "missing account id"},
		{"ledger references missing account", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Ledger[0].AccountID = "ghost"
		}), "references missing account"},
		{"ledger missing request id", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Ledger[0].RequestID = ""
		}), "missing request id"},
		{"deactivated policy missing deactivated-at", mutateBackup(t, valid, func(bf *backupFile) {
			bf.Policies[0].Deactivated = true
			bf.Policies[0].DeactivatedAt = time.Time{}
		}), "missing deactivated-at"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Restore(tc.data)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestBackupRestoreConsistency(t *testing.T) {
	valid := validBackupJSON(t)

	t.Run("account reserved balance mismatch", func(t *testing.T) {
		data := mutateBackup(t, valid, func(bf *backupFile) {
			bf.Accounts[0].Reserved++
		})
		_, err := Restore(data)
		if err == nil || !strings.Contains(err.Error(), "reserved balance") {
			t.Fatalf("expected reserved balance mismatch error, got %v", err)
		}
	})

	t.Run("policy reserved total mismatch", func(t *testing.T) {
		data := mutateBackup(t, valid, func(bf *backupFile) {
			bf.Policies[0].ReservedTotal++
		})
		_, err := Restore(data)
		if err == nil || !strings.Contains(err.Error(), "reserved total") {
			t.Fatalf("expected reserved total mismatch error, got %v", err)
		}
	})

	t.Run("policy spent total mismatch", func(t *testing.T) {
		// 有效备份中没有已结算请求，spentTotal 为 0。
		// 构造一笔已结算请求使 spentTotal 不为 0，但不修改。
		data := mutateBackup(t, valid, func(bf *backupFile) {
			bf.Policies[0].SpentTotal = 999
		})
		_, err := Restore(data)
		if err == nil || !strings.Contains(err.Error(), "spent total") {
			t.Fatalf("expected spent total mismatch error, got %v", err)
		}
	})

	t.Run("account reserved sum overflows int64", func(t *testing.T) {
		// 两笔已预留请求，预估费用均为 MaxInt64，求和溢出。
		data := mutateBackup(t, valid, func(bf *backupFile) {
			bf.Requests = []backupRequest{
				{
					PolicyID: "pol1", RequestID: "r1", AccountID: "u1",
					PayerAccountID: "payer", SessionID: "s1",
					Operation: "pay", Payee: "shop",
					EstimatedFee: math.MaxInt64, State: RequestReserved,
					CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				},
				{
					PolicyID: "pol1", RequestID: "r2", AccountID: "u1",
					PayerAccountID: "payer", SessionID: "s1",
					Operation: "pay", Payee: "shop",
					EstimatedFee: math.MaxInt64, State: RequestReserved,
					CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				},
			}
			// 账户预留余额设为非负（不一致会先被溢出检测捕获）。
			bf.Accounts[0].Reserved = 0
			bf.Policies[0].ReservedTotal = 0
		})
		_, err := Restore(data)
		if err == nil || !strings.Contains(err.Error(), "overflows int64") {
			t.Fatalf("expected overflow error, got %v", err)
		}
	})
}

// ---- 到期处理测试 ----

func TestBackupRestoreTimeExpiration(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 10000)
	mustAccount(t, w, "u1", 0)
	mustSession(t, w, "s1", "u1", "dev1", c.t.Add(2*time.Hour))

	// 预留超时策略：MaxReserveDuration = 1 分钟。
	pol1 := PolicySpec{
		ID: "pol1", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "pay", Payee: "shop",
		StartsAt: c.t.Add(-time.Hour), EndsAt: c.t.Add(24 * time.Hour),
		MaxPerRequest: 500, MaxTotal: 3000, MaxReserveDuration: time.Minute,
	}
	if err := w.SavePolicy(pol1); err != nil {
		t.Fatal(err)
	}

	// 待审批策略：ApprovalWait = 1 分钟。
	pol2 := PolicySpec{
		ID: "pol2", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "pay", Payee: "shop",
		StartsAt: c.t.Add(-time.Hour), EndsAt: c.t.Add(24 * time.Hour),
		MaxPerRequest: 1000, MaxTotal: 5000,
		ApprovalThreshold: 500, ApprovalWait: time.Minute,
	}
	if err := w.SavePolicy(pol2); err != nil {
		t.Fatal(err)
	}

	// T0 时刻：受理一笔预留请求（100，超时 1 分钟）。
	if _, err := w.Apply(RequestInput{
		PolicyID: "pol1", RequestID: "reserved", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "pay", Payee: "shop", EstimatedFee: 100,
	}); err != nil {
		t.Fatal(err)
	}
	// T0 时刻：提交一笔待审批请求（800，等待 1 分钟）。
	if _, err := w.Apply(RequestInput{
		PolicyID: "pol2", RequestID: "pending", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "pay", Payee: "shop", EstimatedFee: 800,
	}); err != nil {
		t.Fatal(err)
	}

	// 推进到 T0 + 2 分钟，两笔请求都应过期。
	c.t = c.t.Add(2 * time.Minute)

	// 导出（处理到期）并恢复。
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restore(data, func() time.Time { return c.t })
	if err != nil {
		t.Fatal(err)
	}

	// 核对预留超时请求。
	r1, err := w.Request("u1", "reserved")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := w2.Request("u1", "reserved")
	if err != nil {
		t.Fatal(err)
	}
	if r1.State != RequestReservationExpired || r2.State != RequestReservationExpired {
		t.Fatalf("reserved state: original=%v restored=%v", r1.State, r2.State)
	}
	// 释放时间是原截止时刻（T0 + 1 分钟），不是导出/恢复时刻。
	wantDeadline := c.t.Add(-time.Minute)
	if !r2.ReserveExpiredAt.Equal(wantDeadline) {
		t.Fatalf("reserve expired at = %v, want %v", r2.ReserveExpiredAt, wantDeadline)
	}
	if !r2.ReserveDeadline.Equal(wantDeadline) {
		t.Fatalf("reserve deadline = %v, want %v", r2.ReserveDeadline, wantDeadline)
	}

	// 核对待审批过期请求。
	p1, err := w.Request("u1", "pending")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := w2.Request("u1", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if p1.State != RequestExpired || p2.State != RequestExpired {
		t.Fatalf("pending state: original=%v restored=%v", p1.State, p2.State)
	}

	// 核对余额：预留超时已全额退回。
	bal1, _ := w.Balance("payer")
	bal2, _ := w2.Balance("payer")
	if !reflect.DeepEqual(bal1, bal2) {
		t.Fatalf("balances: %+v vs %+v", bal1, bal2)
	}
	if bal2.Reserved != 0 {
		t.Fatalf("reserved balance = %d, want 0", bal2.Reserved)
	}

	// 核对账本：退款与超时记录齐全。
	led := w2.Ledger()
	var refund, resExp bool
	for _, e := range led {
		if e.Kind == LedgerRefund && e.RequestID == "reserved" && e.Amount == 100 {
			refund = true
		}
		if e.Kind == LedgerReservationExpiration && e.RequestID == "reserved" {
			resExp = true
		}
	}
	if !refund || !resExp {
		t.Fatalf("missing refund or reservation-expiration record: refund=%v resExp=%v", refund, resExp)
	}
}

func TestBackupRestoreDoesNotRestartTiming(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 10000)
	mustAccount(t, w, "u1", 0)
	mustSession(t, w, "s1", "u1", "dev1", c.t.Add(2*time.Hour))
	pol := PolicySpec{
		ID: "pol1", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "pay", Payee: "shop",
		StartsAt: c.t.Add(-time.Hour), EndsAt: c.t.Add(24 * time.Hour),
		MaxPerRequest: 500, MaxTotal: 3000, MaxReserveDuration: time.Minute,
	}
	if err := w.SavePolicy(pol); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(RequestInput{
		PolicyID: "pol1", RequestID: "r1", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "pay", Payee: "shop", EstimatedFee: 100,
	}); err != nil {
		t.Fatal(err)
	}

	// 在 T0 + 30 秒导出恢复（未超时）。
	c.t = c.t.Add(30 * time.Second)
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restore(data, func() time.Time { return c.t })
	if err != nil {
		t.Fatal(err)
	}

	// 恢复后再推进 40 秒（超过原截止 T0 + 60 秒），应超时。
	c.t = c.t.Add(40 * time.Second)
	r, err := w2.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != RequestReservationExpired {
		t.Fatalf("state = %v, want reservation expired (timing must not restart)", r.State)
	}
	// 释放时间仍是原截止时刻（T0 + 60 秒），不是恢复时刻或查询时刻。
	wantDeadline := c.t.Add(-10 * time.Second)
	if !r.ReserveExpiredAt.Equal(wantDeadline) {
		t.Fatalf("reserve expired at = %v, want %v", r.ReserveExpiredAt, wantDeadline)
	}
}

// ---- 恢复后继续处理 ----

func TestBackupRestoreContinueProcessing(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 1000)
	mustAccount(t, w, "u1", 0)
	mustSession(t, w, "s1", "u1", "dev1", c.t.Add(time.Hour))
	pol := PolicySpec{
		ID: "pol1", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "pay", Payee: "shop",
		StartsAt: c.t.Add(-time.Hour), EndsAt: c.t.Add(24 * time.Hour),
		MaxPerRequest: 500, MaxTotal: 3000,
	}
	if err := w.SavePolicy(pol); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(RequestInput{
		PolicyID: "pol1", RequestID: "r1", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "pay", Payee: "shop", EstimatedFee: 100,
	}); err != nil {
		t.Fatal(err)
	}

	w2 := exportRestore(t, w, c)

	// 恢复后继续结算。
	if _, err := w2.Settle("u1", "r1", 70); err != nil {
		t.Fatalf("settle after restore: %v", err)
	}
	r, _ := w2.Request("u1", "r1")
	if r.State != RequestSettled || r.ActualFee != 70 {
		t.Fatalf("after settle: state=%v actual=%d", r.State, r.ActualFee)
	}
	bal, _ := w2.Balance("payer")
	if bal.Available != 930 || bal.Reserved != 0 {
		t.Fatalf("balance after settle: %+v", bal)
	}

	// 恢复后继续申请。
	if _, err := w2.Apply(RequestInput{
		PolicyID: "pol1", RequestID: "r2", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "pay", Payee: "shop", EstimatedFee: 50,
	}); err != nil {
		t.Fatalf("apply after restore: %v", err)
	}
	bal, _ = w2.Balance("payer")
	if bal.Reserved != 50 {
		t.Fatalf("reserved after apply: %d, want 50", bal.Reserved)
	}
}

func TestBackupRestoreIdempotency(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 1000)
	mustAccount(t, w, "u1", 0)
	mustSession(t, w, "s1", "u1", "dev1", c.t.Add(time.Hour))
	pol := PolicySpec{
		ID: "pol1", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "pay", Payee: "shop",
		StartsAt: c.t.Add(-time.Hour), EndsAt: c.t.Add(24 * time.Hour),
		MaxPerRequest: 500, MaxTotal: 3000,
	}
	if err := w.SavePolicy(pol); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(RequestInput{
		PolicyID: "pol1", RequestID: "r1", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "pay", Payee: "shop", EstimatedFee: 100,
	}); err != nil {
		t.Fatal(err)
	}

	w2 := exportRestore(t, w, c)

	// 同号同内容重复申请：幂等返回，不重复预留。
	r, err := w2.Apply(RequestInput{
		PolicyID: "pol1", RequestID: "r1", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "pay", Payee: "shop", EstimatedFee: 100,
	})
	if err != nil {
		t.Fatalf("idempotent apply: %v", err)
	}
	if r.State != RequestReserved {
		t.Fatalf("idempotent apply state = %v, want reserved", r.State)
	}
	bal, _ := w2.Balance("payer")
	if bal.Reserved != 100 {
		t.Fatalf("reserved after idempotent apply = %d, want 100", bal.Reserved)
	}

	// 同号不同内容：冲突。
	_, err = w2.Apply(RequestInput{
		PolicyID: "pol1", RequestID: "r1", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "pay", Payee: "shop", EstimatedFee: 200,
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict apply err = %v, want ErrConflict", err)
	}
}

// ---- 恢复后钱包互不影响 ----

func TestBackupRestoreIndependence(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 1000)
	mustAccount(t, w, "u1", 0)
	mustSession(t, w, "s1", "u1", "dev1", c.t.Add(time.Hour))
	pol := PolicySpec{
		ID: "pol1", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "pay", Payee: "shop",
		StartsAt: c.t.Add(-time.Hour), EndsAt: c.t.Add(24 * time.Hour),
		MaxPerRequest: 500, MaxTotal: 3000,
	}
	if err := w.SavePolicy(pol); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(RequestInput{
		PolicyID: "pol1", RequestID: "r1", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "pay", Payee: "shop", EstimatedFee: 100,
	}); err != nil {
		t.Fatal(err)
	}

	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w1, err := restore(data, func() time.Time { return c.t })
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restore(data, func() time.Time { return c.t })
	if err != nil {
		t.Fatal(err)
	}

	// 在 w1 中结算。
	if _, err := w1.Settle("u1", "r1", 60); err != nil {
		t.Fatal(err)
	}
	// w2 不受影响。
	r2, _ := w2.Request("u1", "r1")
	if r2.State != RequestReserved {
		t.Fatalf("w2 state = %v, want reserved (independence)", r2.State)
	}
	bal2, _ := w2.Balance("payer")
	if bal2.Reserved != 100 {
		t.Fatalf("w2 reserved = %d, want 100", bal2.Reserved)
	}

	// 原钱包也不受影响。
	r0, _ := w.Request("u1", "r1")
	if r0.State != RequestReserved {
		t.Fatalf("original state = %v, want reserved", r0.State)
	}
}

// ---- 不同使用账户的同号请求分别识别 ----

func TestBackupRestoreSameRequestIDDifferentAccounts(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 2000)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	mustSession(t, w, "s1", "u1", "dev1", c.t.Add(time.Hour))
	mustSession(t, w, "s2", "u2", "dev2", c.t.Add(time.Hour))
	pol := PolicySpec{
		ID: "pol1", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1", "u2"},
		Operation: "pay", Payee: "shop",
		StartsAt: c.t.Add(-time.Hour), EndsAt: c.t.Add(24 * time.Hour),
		MaxPerRequest: 500, MaxTotal: 3000,
	}
	if err := w.SavePolicy(pol); err != nil {
		t.Fatal(err)
	}
	// 两个使用账户各自申请同号请求。
	for _, acct := range []string{"u1", "u2"} {
		sess := "s1"
		dev := "dev1"
		if acct == "u2" {
			sess = "s2"
			dev = "dev2"
		}
		if _, err := w.Apply(RequestInput{
			PolicyID: "pol1", RequestID: "same", AccountID: acct, SessionID: sess,
			DeviceID: dev, Operation: "pay", Payee: "shop", EstimatedFee: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}

	w2 := exportRestore(t, w, c)

	// 两个请求分别识别。
	r1, err := w2.Request("u1", "same")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := w2.Request("u2", "same")
	if err != nil {
		t.Fatal(err)
	}
	if r1.AccountID != "u1" || r2.AccountID != "u2" {
		t.Fatalf("accounts: %s vs %s", r1.AccountID, r2.AccountID)
	}
	// 结算 u1 的请求不影响 u2。
	if _, err := w2.Settle("u1", "same", 80); err != nil {
		t.Fatal(err)
	}
	r2, _ = w2.Request("u2", "same")
	if r2.State != RequestReserved {
		t.Fatalf("u2 state = %v, want reserved", r2.State)
	}
}

// ---- 导出并发测试 ----

func TestBackupExportConcurrency(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100000)
	mustAccount(t, w, "u1", 0)
	mustSession(t, w, "s1", "u1", "dev1", c.t.Add(time.Hour))
	pol := PolicySpec{
		ID: "pol1", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "pay", Payee: "shop",
		StartsAt: c.t.Add(-time.Hour), EndsAt: c.t.Add(24 * time.Hour),
		MaxPerRequest: 500, MaxTotal: 100000,
	}
	if err := w.SavePolicy(pol); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	// 并发申请。
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rid := strings.Repeat("r", 0) + string(rune('a'+i))
			_, _ = w.Apply(RequestInput{
				PolicyID: "pol1", RequestID: rid, AccountID: "u1", SessionID: "s1",
				DeviceID: "dev1", Operation: "pay", Payee: "shop", EstimatedFee: 10,
			})
		}(i)
	}
	// 并发导出。
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = w.Export()
		}()
	}
	wg.Wait()

	// 导出的 JSON 必须能恢复，且恢复后余额与请求一致。
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restore(data, func() time.Time { return c.t })
	if err != nil {
		t.Fatal(err)
	}
	bal1, _ := w.Balance("payer")
	bal2, _ := w2.Balance("payer")
	if !reflect.DeepEqual(bal1, bal2) {
		t.Fatalf("balances after concurrent export: %+v vs %+v", bal1, bal2)
	}
}

// ---- 过期会话/策略恢复后不能重新获得授权 ----

func TestBackupRestoreExpiredSessionAndPolicy(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 1000)
	mustAccount(t, w, "u1", 0)
	// 已过期会话。
	mustSession(t, w, "s1", "u1", "dev1", c.t.Add(-time.Hour))
	// 已结束策略。
	pol := PolicySpec{
		ID: "pol1", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "pay", Payee: "shop",
		StartsAt: c.t.Add(-2 * time.Hour), EndsAt: c.t.Add(-time.Hour),
		MaxPerRequest: 500, MaxTotal: 3000,
	}
	if err := w.SavePolicy(pol); err != nil {
		t.Fatal(err)
	}

	w2 := exportRestore(t, w, c)

	// 恢复后用过期会话申请：被拒绝。
	_, err := w2.Apply(RequestInput{
		PolicyID: "pol1", RequestID: "r1", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "pay", Payee: "shop", EstimatedFee: 100,
	})
	if err == nil {
		t.Fatal("expected rejection for expired session")
	}
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("err = %v, want ErrSessionExpired", err)
	}

	// 恢复后用已结束策略申请：被拒绝。
	mustAccount(t, w2, "u2", 0)
	mustSession(t, w2, "s2", "u2", "dev2", c.t.Add(time.Hour))
	_, err = w2.Apply(RequestInput{
		PolicyID: "pol1", RequestID: "r2", AccountID: "u2", SessionID: "s2",
		DeviceID: "dev2", Operation: "pay", Payee: "shop", EstimatedFee: 100,
	})
	if err == nil {
		t.Fatal("expected rejection for ended policy")
	}
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("err = %v, want ErrPolicyDenied", err)
	}
}
