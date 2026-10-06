package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildDeactivatorBackup 构造一份只含账户与一条已停用策略的备份：策略 p 的
// 出资账户为 payer、允许 u1 使用，停用时刻 t0+2h、理由 "stop it"，执行账户
// 由 deactivator 指定（空字符串表示字段留空）。其余金额均为零、完全自洽，
// 因此恢复能否成功只取决于停用执行账户是否合法。
func buildDeactivatorBackup(t *testing.T, t0 time.Time, deactivator string, mutate func(*backupV1)) []byte {
	t.Helper()
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0.Add(3 * time.Hour)),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: 100, CreatedAt: timeJSON(t0.Add(-time.Hour))},
			{ID: "u1", Available: 0, CreatedAt: timeJSON(t0.Add(-time.Hour))},
		},
		Sessions: []sessionBackupV1{{
			ID: "s1", AccountID: "u1", DeviceID: "d1",
			ExpiresAt: timeJSON(t0.Add(4 * time.Hour)), CreatedAt: timeJSON(t0.Add(-time.Hour)),
		}},
		Policies: []policyBackupV1{{
			ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
			Operation: "charge", Payee: "shop",
			StartsAt: timeJSON(t0), EndsAt: timeJSON(t0.Add(4 * time.Hour)),
			MaxPerRequest: 100, MaxTotal: 1000,
			Deactivated:          true,
			DeactivatedAt:        timeJSON(t0.Add(2 * time.Hour)),
			DeactivatorAccountID: deactivator,
			DeactivateReason:     "stop it",
		}},
		Requests: []requestBackupV1{},
		Ledger:   []ledgerEntryBackupV1{},
	}
	if mutate != nil {
		mutate(&b)
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// assertDeactivatorRejected 恢复必须整体失败、包装 ErrBackupInvalid，错误
// 信息点名策略编号及全部给定身份细节，且不得返回部分钱包。
func assertDeactivatorRejected(t *testing.T, data []byte, at time.Time, want ...string) {
	t.Helper()
	w2, err := restoreAt(data, at)
	if err == nil {
		if w2 != nil {
			t.Fatalf("invalid deactivator history restored a wallet: %+v", w2)
		}
		t.Fatal("invalid deactivator history restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	for _, part := range append([]string{`"p"`}, want...) {
		if !strings.Contains(msg, part) {
			t.Fatalf("error %q must identify %q (policy and identity problem)", msg, part)
		}
	}
}

// TestRestoreRejectsDeactivatedPolicyMissingDeactivator 验证核心修复：标为已
// 停用的策略必须保存非空的执行账户。字段缺失与空字符串都按缺少执行账户处理，
// 整份备份必须无效——正常停用一定会记录执行账户。
func TestRestoreRejectsDeactivatedPolicyMissingDeactivator(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// 空字符串。
	data := buildDeactivatorBackup(t, t0, "", nil)
	assertDeactivatorRejected(t, data, t0.Add(48*time.Hour), "deactivator")

	// 字段在 JSON 中整体缺失（解码后同样为空字符串）。
	raw := buildDeactivatorBackup(t, t0, "payer", nil)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m["policies"].([]any)[0].(map[string]any), "deactivator_account_id")
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	assertDeactivatorRejected(t, data, t0.Add(48*time.Hour), "deactivator")
}

// TestRestoreRejectsDeactivatorOtherThanPayer 验证执行账户必须与策略出资账户
// 完全一致：填入钱包里另一个真实存在的账户——即使它在策略允许使用的账户
// 列表里，或是另一条策略的出资账户——都不能代替本策略的出资账户。错误必须
// 同时给出保存的执行账户与本应执行停用的出资账户。
func TestRestoreRejectsDeactivatorOtherThanPayer(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// 执行账户换成允许使用账户 u1：真实存在且在允许列表里，仍必须拒绝。
	data := buildDeactivatorBackup(t, t0, "u1", nil)
	assertDeactivatorRejected(t, data, t0.Add(48*time.Hour), `"u1"`, `"payer"`)

	// 执行账户换成另一条策略的出资账户 other-payer：仍必须拒绝。
	data = buildDeactivatorBackup(t, t0, "other-payer", func(b *backupV1) {
		b.Accounts = append(b.Accounts, accountBackupV1{ID: "other-payer", Available: 50, CreatedAt: timeJSON(t0.Add(-time.Hour))})
		b.Policies = append(b.Policies, policyBackupV1{
			ID: "q", PayerAccountID: "other-payer", AllowedAccountIDs: []string{"u1"},
			Operation: "charge", Payee: "shop",
			StartsAt: timeJSON(t0), EndsAt: timeJSON(t0.Add(4 * time.Hour)),
			MaxPerRequest: 100, MaxTotal: 1000,
		})
	})
	assertDeactivatorRejected(t, data, t0.Add(48*time.Hour), `"other-payer"`, `"payer"`)
}

// TestRestoreRejectsDeactivatorUnknownAccount 验证既有对不存在账户的拒绝仍然
// 保留：执行账户指向备份中不存在的账户时整份备份无效。
func TestRestoreRejectsDeactivatorUnknownAccount(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildDeactivatorBackup(t, t0, "ghost", nil)
	assertDeactivatorRejected(t, data, t0.Add(48*time.Hour), "ghost")
}

// TestRestoreRejectsDeactivatorMismatchDespiteConsistentTotals 验证余额与请求
// 金额核对一致、停用时间与理由合法，都不能使执行账户矛盾的记录通过恢复；
// 同一备份的其他记录全部合法也必须整体拒绝，不能略过出错策略后继续。
func TestRestoreRejectsDeactivatorMismatchDespiteConsistentTotals(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildDeactivatorBackup(t, t0, "u1", func(b *backupV1) {
		// 另一条完全合法的已停用策略与一条停用账本留痕：其余记录合法也
		// 不能放行出错的那一条。
		b.Policies = append(b.Policies, policyBackupV1{
			ID: "q", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
			Operation: "charge", Payee: "shop",
			StartsAt: timeJSON(t0), EndsAt: timeJSON(t0.Add(4 * time.Hour)),
			MaxPerRequest: 100, MaxTotal: 1000,
			Deactivated:          true,
			DeactivatedAt:        timeJSON(t0.Add(3 * time.Hour)),
			DeactivatorAccountID: "payer",
			DeactivateReason:     "stop q",
		})
		b.Ledger = append(b.Ledger, ledgerEntryBackupV1{
			Kind: int(LedgerPolicyDeactivation), AccountID: "payer", PolicyID: "q",
			Reason: "stop q", At: timeJSON(t0.Add(3 * time.Hour)),
		})
	})
	assertDeactivatorRejected(t, data, t0.Add(48*time.Hour), `"u1"`, `"payer"`)
}

// TestRestoreAcceptsLegitimateDeactivation 验证合法的已停用策略仍可恢复：
// 执行账户即出资账户时，原停用状态、首次停用时间、执行账户与理由原样保留；
// 出资账户的原会话已到期或被吊销、策略尚未开始或已经结束，都不否定此前
// 合法的停用。
func TestRestoreAcceptsLegitimateDeactivation(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	deact := t0.Add(2 * time.Hour)

	// 出资账户会话已吊销且早已到期：合法停用历史不受影响。
	data := buildDeactivatorBackup(t, t0, "payer", func(b *backupV1) {
		b.Sessions = append(b.Sessions, sessionBackupV1{
			ID: "sp", AccountID: "payer", DeviceID: "dp",
			ExpiresAt: timeJSON(t0.Add(time.Hour)), Revoked: true,
			CreatedAt: timeJSON(t0.Add(-time.Hour)),
		})
	})
	w2, err := restoreAt(data, t0.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("legitimate deactivation must restore despite expired/revoked payer session: %v", err)
	}
	pv, err := w2.Policy("p")
	if err != nil {
		t.Fatal(err)
	}
	if !pv.Deactivated || !pv.DeactivatedAt.Equal(deact) || pv.DeactivatorAccountID != "payer" || pv.DeactivateReason != "stop it" {
		t.Fatalf("deactivation info changed: %+v", pv)
	}

	// 策略尚未开始就被停用的合法记录。
	data = buildDeactivatorBackup(t, t0, "payer", func(b *backupV1) {
		b.Policies[0].StartsAt = timeJSON(t0.Add(3 * time.Hour))
		b.Policies[0].EndsAt = timeJSON(t0.Add(7 * time.Hour))
	})
	if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
		t.Fatalf("deactivation before policy start must restore: %v", err)
	}

	// 策略已经结束才被停用的合法记录。
	data = buildDeactivatorBackup(t, t0, "payer", func(b *backupV1) {
		b.Policies[0].DeactivatedAt = timeJSON(t0.Add(5 * time.Hour))
	})
	w2, err = restoreAt(data, t0.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("deactivation after policy end must restore: %v", err)
	}
	if pv, _ := w2.Policy("p"); !pv.Deactivated || pv.DeactivatorAccountID != "payer" {
		t.Fatalf("deactivation after policy end changed: %+v", pv)
	}
}

// TestRestoreDeactivatorRoundTrip 端到端验证真实停用并导出的备份照常恢复：
// 正常停用入口记录的执行账户就是出资账户，恢复后停用信息逐字段一致。
func TestRestoreDeactivatorRoundTrip(t *testing.T) {
	w, c := setupTimeout(t, time.Hour)
	if _, err := w.CreateSession("sp", "payer", "dp", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.DeactivatePolicy("p-timeout", "sp", "dp", "stop it"); err != nil {
		t.Fatal(err)
	}
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, c.t.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("export of a normally deactivated policy must restore: %v", err)
	}
	pv, err := w2.Policy("p-timeout")
	if err != nil {
		t.Fatal(err)
	}
	if !pv.Deactivated || pv.DeactivatorAccountID != "payer" || pv.DeactivateReason != "stop it" {
		t.Fatalf("deactivation info not preserved: %+v", pv)
	}
}
