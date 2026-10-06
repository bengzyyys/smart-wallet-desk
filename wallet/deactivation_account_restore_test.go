package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildDeactivatorBackup 构造一份最小但合法的备份：两个账户（payer、u1）与
// 一条由 payer 出资、允许 u1 使用的已停用策略 p。停用时刻、理由与执行账户
// 均合法；deactivator 为 "" 时 JSON 中仍显式写出空字符串，dropField 为 true
// 时直接删除该字段，分别模拟“填空”与“字段缺失”。withOtherLegalRecords 为
// true 时追加另一条由 payer2 出资的合法未停用策略 p2，验证其他记录合法不能
// 掩盖本策略的身份矛盾。extraAccounts/extraSessions 允许构造“另一个确实存在
// 的账户/会话”等场景。
func buildDeactivatorBackup(t *testing.T, t0 time.Time, deactivator string, dropField, withOtherLegalRecords bool, extraAccounts []accountBackupV1, extraSessions []sessionBackupV1, mutate func(p *policyBackupV1)) []byte {
	t.Helper()
	pol := policyBackupV1{
		ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "charge", Payee: "shop",
		StartsAt:             timeJSON(t0.Add(-time.Hour)),
		EndsAt:               timeJSON(t0.Add(time.Hour)),
		MaxPerRequest:        100,
		MaxTotal:             1000,
		Deactivated:          true,
		DeactivatedAt:        timeJSON(t0.Add(-time.Minute)),
		DeactivatorAccountID: deactivator,
		DeactivateReason:     "stop it",
	}
	if mutate != nil {
		mutate(&pol)
	}
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0),
		Accounts: append([]accountBackupV1{
			{ID: "payer", Available: 100, Reserved: 0, CreatedAt: timeJSON(t0)},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)},
		}, extraAccounts...),
		Sessions: append([]sessionBackupV1{}, extraSessions...),
		Policies: []policyBackupV1{pol},
		Requests: []requestBackupV1{},
		Ledger: []ledgerEntryBackupV1{{
			Kind:      int(LedgerPolicyDeactivation),
			AccountID: "payer",
			PolicyID:  "p",
			Reason:    "stop it",
			At:        timeJSON(t0.Add(-time.Minute)),
		}},
	}
	if withOtherLegalRecords {
		b.Policies = append(b.Policies, policyBackupV1{
			ID: "p2", PayerAccountID: "payer2", AllowedAccountIDs: []string{"u1"},
			Operation: "charge", Payee: "shop",
			StartsAt:      timeJSON(t0.Add(-time.Hour)),
			EndsAt:        timeJSON(t0.Add(time.Hour)),
			MaxPerRequest: 100,
			MaxTotal:      1000,
		})
	}
	raw, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	if !dropField {
		return raw
	}
	// 删除 deactivator_account_id 键，模拟字段缺失（区别于显式空字符串）。
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, v := range m["policies"].([]interface{}) {
		pm := v.(map[string]interface{})
		if pm["id"] == "p" {
			delete(pm, "deactivator_account_id")
		}
	}
	out, err := json.Marshal(&m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRestoreRejectsDeactivatedPolicyWithoutDeactivator 验证已停用策略缺少执行
// 账户（字段缺失或空字符串）时整份备份必须无效：错误可识别为 ErrBackupInvalid、
// 指出策略编号与缺少执行账户的身份问题，且不返回钱包；其他记录合法也不能放行。
func TestRestoreRejectsDeactivatedPolicyWithoutDeactivator(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		dropField bool
	}{
		{"empty string deactivator", false},
		{"missing deactivator field", true},
	} {
		for _, mixed := range []bool{false, true} {
			name := tc.name
			if mixed {
				name += " (other records legal)"
			}
			t.Run(name, func(t *testing.T) {
				data := buildDeactivatorBackup(t, t0, "", tc.dropField, mixed, nil, nil, nil)
				w2, err := Restore(data)
				if err == nil {
					if w2 != nil {
						t.Fatalf("deactivated policy without deactivator restored a wallet: %+v", w2)
					}
					t.Fatal("deactivated policy without deactivator restored without error")
				}
				if !errors.Is(err, ErrBackupInvalid) {
					t.Fatalf("err = %v, want ErrBackupInvalid", err)
				}
				msg := err.Error()
				for _, want := range []string{`"p"`, "missing deactivator account", `"payer"`} {
					if !strings.Contains(msg, want) {
						t.Fatalf("error %q must identify %q (policy, missing-account problem, payer)", msg, want)
					}
				}
			})
		}
	}
}

// TestRestoreRejectsDeactivatedPolicyWithForeignDeactivator 验证执行账户不是本
// 策略出资账户时整份备份必须无效：该账户确实存在、在策略允许使用的账户列表中，
// 或是另一条策略的出资账户，都不能代替本策略的出资账户。错误必须同时给出保存
// 的执行账户与本应执行停用的出资账户；同一备份其他记录合法仍整体拒绝。
func TestRestoreRejectsDeactivatedPolicyWithForeignDeactivator(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	payer2 := accountBackupV1{ID: "payer2", Available: 50, Reserved: 0, CreatedAt: timeJSON(t0)}
	for _, tc := range []struct {
		name        string
		deactivator string
		extra       []accountBackupV1
		mixed       bool
	}{
		// u1 确实存在且是本策略允许使用的账户。
		{"allowed usage account", "u1", nil, false},
		{"allowed usage account with other legal records", "u1", nil, true},
		// payer2 是另一条策略 p2 的出资账户，但不能停用 p。
		{"payer of another policy", "payer2", []accountBackupV1{payer2}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := buildDeactivatorBackup(t, t0, tc.deactivator, false, tc.mixed, tc.extra, nil, nil)
			w2, err := Restore(data)
			if err == nil {
				if w2 != nil {
					t.Fatalf("foreign deactivator restored a wallet: %+v", w2)
				}
				t.Fatal("foreign deactivator restored without error")
			}
			if !errors.Is(err, ErrBackupInvalid) {
				t.Fatalf("err = %v, want ErrBackupInvalid", err)
			}
			msg := err.Error()
			for _, want := range []string{`"p"`, "deactivator account", `"` + tc.deactivator + `"`, `"payer"`, "does not match"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("error %q must identify %q (policy, saved deactivator, payer, mismatch)", msg, want)
				}
			}
		})
	}
}

// TestRestoreRejectsDeactivatedPolicyWithUnknownDeactivator 验证对不存在账户的
// 拒绝仍保留：填入钱包里不存在的执行账户编号，按既有“引用未知账户”规则拒绝。
func TestRestoreRejectsDeactivatedPolicyWithUnknownDeactivator(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildDeactivatorBackup(t, t0, "ghost", false, false, nil, nil, nil)
	w2, err := Restore(data)
	if err == nil {
		if w2 != nil {
			t.Fatalf("unknown deactivator restored a wallet: %+v", w2)
		}
		t.Fatal("unknown deactivator restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	for _, want := range []string{`"p"`, "references unknown account", `"ghost"`} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q must identify %q (policy, unknown-account problem, saved id)", msg, want)
		}
	}
}

// TestRestoreKeepsLegalDeactivationRegardlessOfSessionsOrWindow 验证合法停用
// 记录（执行账户恰为出资账户）在各种合法时间线下仍可恢复，停用状态、首次停用
// 时间、执行账户与理由原样保留：这是在核对已保存的停用历史，不要求出资账户在
// 恢复时仍持有有效会话——会话缺失、已到期或已吊销都不否定此前合法的停用；尚未
// 开始或已经结束的策略原本都允许停用，其记录同样接受。恢复后再次导出与原备份
// 逐字节一致，合法停用记录不会被丢弃或改写。
func TestRestoreKeepsLegalDeactivationRegardlessOfSessionsOrWindow(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// 出资账户没有任何会话：合法停用历史照常恢复。
	t.Run("payer has no session at all", func(t *testing.T) {
		data := buildDeactivatorBackup(t, t0, "payer", false, false, nil, nil, nil)
		w2, err := restoreAt(data, t0)
		if err != nil {
			t.Fatalf("legal deactivation without payer sessions must restore: %v", err)
		}
		assertLegalDeactivationPreserved(t, w2, data, t0.Add(-time.Minute))
	})

	// 出资账户会话已到期、已吊销：不否定此前合法的停用。
	for _, tc := range []struct {
		name    string
		session sessionBackupV1
	}{
		{"payer session expired", sessionBackupV1{
			ID: "sa", AccountID: "payer", DeviceID: "deva",
			ExpiresAt: timeJSON(t0.Add(-time.Hour)), CreatedAt: timeJSON(t0.Add(-2 * time.Hour)),
		}},
		{"payer session revoked", sessionBackupV1{
			ID: "sa", AccountID: "payer", DeviceID: "deva",
			ExpiresAt: timeJSON(t0.Add(time.Hour)), Revoked: true, CreatedAt: timeJSON(t0.Add(-2 * time.Hour)),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := buildDeactivatorBackup(t, t0, "payer", false, false, nil, []sessionBackupV1{tc.session}, nil)
			w2, err := restoreAt(data, t0)
			if err != nil {
				t.Fatalf("legal deactivation must restore despite %s: %v", tc.name, err)
			}
			assertLegalDeactivationPreserved(t, w2, data, t0.Add(-time.Minute))
		})
	}

	// 尚未开始的策略：停用时刻早于策略开始时刻，合法。
	t.Run("policy not yet started", func(t *testing.T) {
		mutate := func(p *policyBackupV1) {
			p.StartsAt = timeJSON(t0.Add(time.Hour))
			p.EndsAt = timeJSON(t0.Add(2 * time.Hour))
			p.DeactivatedAt = timeJSON(t0)
		}
		data := buildDeactivatorBackup(t, t0, "payer", false, false, nil, nil, mutate)
		// 同步账本停用时刻。
		data = rewriteLedgerDeactivationTime(t, data, t0)
		w2, err := restoreAt(data, t0)
		if err != nil {
			t.Fatalf("legal deactivation of a not-yet-started policy must restore: %v", err)
		}
		assertLegalDeactivationPreserved(t, w2, data, t0)
	})

	// 已经结束的策略：停用时刻晚于策略结束时刻，合法。
	t.Run("policy already ended", func(t *testing.T) {
		deact := t0.Add(-30 * time.Minute)
		mutate := func(p *policyBackupV1) {
			p.StartsAt = timeJSON(t0.Add(-2 * time.Hour))
			p.EndsAt = timeJSON(t0.Add(-time.Hour))
			p.DeactivatedAt = timeJSON(deact)
		}
		data := buildDeactivatorBackup(t, t0, "payer", false, false, nil, nil, mutate)
		data = rewriteLedgerDeactivationTime(t, data, deact)
		w2, err := restoreAt(data, t0)
		if err != nil {
			t.Fatalf("legal deactivation of an already-ended policy must restore: %v", err)
		}
		assertLegalDeactivationPreserved(t, w2, data, deact)
	})
}

// assertLegalDeactivationPreserved 核对恢复出的钱包保留了完整停用信息，且再次
// 导出与原备份逐字节一致（不新增任何记录）。
func assertLegalDeactivationPreserved(t *testing.T, w2 *Wallet, data []byte, deactAt time.Time) {
	t.Helper()
	pv, err := w2.Policy("p")
	if err != nil {
		t.Fatal(err)
	}
	if !pv.Deactivated {
		t.Fatal("restored policy lost deactivated flag")
	}
	if !pv.DeactivatedAt.Equal(deactAt) {
		t.Fatalf("deactivated at = %v, want %v", pv.DeactivatedAt, deactAt)
	}
	if pv.DeactivatorAccountID != "payer" {
		t.Fatalf("deactivator = %q, want payer", pv.DeactivatorAccountID)
	}
	if pv.DeactivateReason != "stop it" {
		t.Fatalf("reason = %q, want \"stop it\"", pv.DeactivateReason)
	}
	out, err := w2.Export()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(data) {
		t.Fatalf("re-export changed a legal deactivation backup:\n got %s\nwant %s", out, data)
	}
}

// rewriteLedgerDeactivationTime 把账本中唯一的策略停用记录的时刻改为 at。
func rewriteLedgerDeactivationTime(t *testing.T, data []byte, at time.Time) []byte {
	t.Helper()
	// 经 backupV1 重新编码，保持与 Export 一致的结构体字段顺序，便于逐字节比对。
	var b backupV1
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	for i := range b.Ledger {
		if b.Ledger[i].PolicyID == "p" {
			b.Ledger[i].At = timeJSON(at)
		}
	}
	out, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRestoreRejectsActivePolicyCarryingDeactivator 验证未停用策略携带执行账户
// 的既有校验保持兼容：未停用不得携带任何停用信息。
func TestRestoreRejectsActivePolicyCarryingDeactivator(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	mutate := func(p *policyBackupV1) {
		p.Deactivated = false
		p.DeactivatedAt = timeJSON{}
		p.DeactivateReason = ""
		// 保留 DeactivatorAccountID: "payer"：未停用却带着执行账户。
	}
	data := buildDeactivatorBackup(t, t0, "payer", false, false, nil, nil, mutate)
	// 去掉那条策略停用账本记录，避免与未停用状态产生其他矛盾先被命中。
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m["ledger"] = []interface{}{}
	data, err := json.Marshal(&m)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := Restore(data)
	if err == nil {
		if w2 != nil {
			t.Fatalf("active policy carrying deactivator restored a wallet: %+v", w2)
		}
		t.Fatal("active policy carrying deactivator restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	if !strings.Contains(err.Error(), "not deactivated but carries deactivation info") {
		t.Fatalf("error %q must reject deactivation info on an active policy", err.Error())
	}
}
