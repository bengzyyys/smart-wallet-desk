package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// policyParamsCase 描述一组策略参数取值及其在两个入口（保存、恢复）应当
// 得到的同一判定：wantErr 为空时两个入口都必须接受（保存成功、备份恢复
// 成功且配置原值保留）；非空时保存返回 ErrPolicyInvalid、恢复返回
// ErrBackupInvalid，且两者错误说明都包含同一个具体原因。
type policyParamsCase struct {
	name              string
	operation         string
	payee             string
	maxPerRequest     int64
	maxTotal          int64
	approvalThreshold int64
	approvalWait      time.Duration
	maxReserve        time.Duration
	startsAt          time.Duration // 相对基准时间
	endsAt            time.Duration
	allowedAccounts   []string
	payerAccountID    string
	wantErr           string
}

func policyParamsCases() []policyParamsCase {
	return []policyParamsCase{
		{
			name:      "valid baseline",
			operation: "charge", payee: "shop",
			maxPerRequest: 30, maxTotal: 50,
			approvalThreshold: 10, approvalWait: time.Minute,
			startsAt: -time.Hour, endsAt: time.Hour,
			allowedAccounts: []string{"u1"}, payerAccountID: "payer",
		},
		{
			name:      "approval off keeps negative wait",
			operation: "charge", payee: "shop",
			maxPerRequest: 30, maxTotal: 50,
			approvalThreshold: 0, approvalWait: -90 * time.Second,
			startsAt: -time.Hour, endsAt: time.Hour,
			allowedAccounts: []string{"u1"}, payerAccountID: "payer",
		},
		{
			name:      "approval off keeps zero wait",
			operation: "charge", payee: "shop",
			maxPerRequest: 30, maxTotal: 50,
			startsAt: -time.Hour, endsAt: time.Hour,
			allowedAccounts: []string{"u1"}, payerAccountID: "payer",
		},
		{
			name:      "threshold equal to per-request cap",
			operation: "charge", payee: "shop",
			maxPerRequest: 30, maxTotal: 50,
			approvalThreshold: 30, approvalWait: time.Minute,
			startsAt: -time.Hour, endsAt: time.Hour,
			allowedAccounts: []string{"u1"}, payerAccountID: "payer",
		},
		{
			name:      "reserve timeout disabled",
			operation: "charge", payee: "shop",
			maxPerRequest: 30, maxTotal: 50,
			approvalThreshold: 10, approvalWait: time.Minute, maxReserve: 0,
			startsAt: -time.Hour, endsAt: time.Hour,
			allowedAccounts: []string{"u1"}, payerAccountID: "payer",
		},
		{
			name:      "window entirely in the past is still valid",
			operation: "charge", payee: "shop",
			maxPerRequest: 30, maxTotal: 50,
			startsAt: -2 * time.Hour, endsAt: -time.Hour,
			allowedAccounts: []string{"u1"}, payerAccountID: "payer",
		},
		{
			name:      "window entirely in the future is still valid",
			operation: "charge", payee: "shop",
			maxPerRequest: 30, maxTotal: 50,
			startsAt: time.Hour, endsAt: 2 * time.Hour,
			allowedAccounts: []string{"u1"}, payerAccountID: "payer",
		},
		{
			name:      "missing operation",
			operation: "", payee: "shop",
			maxPerRequest: 30, maxTotal: 50,
			approvalThreshold: 10, approvalWait: time.Minute,
			startsAt: -time.Hour, endsAt: time.Hour,
			allowedAccounts: []string{"u1"}, payerAccountID: "payer",
			wantErr: "operation and payee are required",
		},
		{
			name:      "non-positive per-request limit",
			operation: "charge", payee: "shop",
			maxPerRequest: 0, maxTotal: 50,
			approvalThreshold: 10, approvalWait: time.Minute,
			startsAt: -time.Hour, endsAt: time.Hour,
			allowedAccounts: []string{"u1"}, payerAccountID: "payer",
			wantErr: "limits must be positive",
		},
		{
			name:      "negative approval threshold",
			operation: "charge", payee: "shop",
			maxPerRequest: 30, maxTotal: 50,
			approvalThreshold: -1, approvalWait: time.Minute,
			startsAt: -time.Hour, endsAt: time.Hour,
			allowedAccounts: []string{"u1"}, payerAccountID: "payer",
			wantErr: "approval threshold must not be negative",
		},
		{
			name:      "threshold exceeds per-request cap",
			operation: "charge", payee: "shop",
			maxPerRequest: 30, maxTotal: 50,
			approvalThreshold: 31, approvalWait: time.Minute,
			startsAt: -time.Hour, endsAt: time.Hour,
			allowedAccounts: []string{"u1"}, payerAccountID: "payer",
			wantErr: "approval threshold 31 exceeds per-request limit 30",
		},
		{
			name:      "zero wait when approval enabled",
			operation: "charge", payee: "shop",
			maxPerRequest: 30, maxTotal: 50,
			approvalThreshold: 10, approvalWait: 0,
			startsAt: -time.Hour, endsAt: time.Hour,
			allowedAccounts: []string{"u1"}, payerAccountID: "payer",
			wantErr: "approval wait must be positive when approval is enabled",
		},
		{
			name:      "negative wait when approval enabled",
			operation: "charge", payee: "shop",
			maxPerRequest: 30, maxTotal: 50,
			approvalThreshold: 10, approvalWait: -time.Second,
			startsAt: -time.Hour, endsAt: time.Hour,
			allowedAccounts: []string{"u1"}, payerAccountID: "payer",
			wantErr: "approval wait must be positive when approval is enabled",
		},
		{
			name:      "negative max reserve duration even with approval off",
			operation: "charge", payee: "shop",
			maxPerRequest: 30, maxTotal: 50,
			approvalThreshold: 0, approvalWait: -time.Second, maxReserve: -time.Nanosecond,
			startsAt: -time.Hour, endsAt: time.Hour,
			allowedAccounts: []string{"u1"}, payerAccountID: "payer",
			wantErr: "max reserve duration must not be negative",
		},
		{
			name:      "reversed window",
			operation: "charge", payee: "shop",
			maxPerRequest: 30, maxTotal: 50,
			startsAt: time.Hour, endsAt: -time.Hour,
			allowedAccounts: []string{"u1"}, payerAccountID: "payer",
			wantErr: "starts-at must be before ends-at",
		},
		{
			name:      "empty window start equals end",
			operation: "charge", payee: "shop",
			maxPerRequest: 30, maxTotal: 50,
			startsAt: -time.Hour, endsAt: -time.Hour,
			allowedAccounts: []string{"u1"}, payerAccountID: "payer",
			wantErr: "starts-at must be before ends-at",
		},
		{
			name:      "no allowed accounts",
			operation: "charge", payee: "shop",
			maxPerRequest: 30, maxTotal: 50,
			startsAt: -time.Hour, endsAt: time.Hour,
			allowedAccounts: nil, payerAccountID: "payer",
			wantErr: "payer account and at least one allowed account are required",
		},
		{
			name:      "empty payer account",
			operation: "charge", payee: "shop",
			maxPerRequest: 30, maxTotal: 50,
			startsAt: -time.Hour, endsAt: time.Hour,
			allowedAccounts: []string{"u1"}, payerAccountID: "",
			wantErr: "payer account and at least one allowed account are required",
		},
	}
}

func policySpecAt(t0 time.Time, tc policyParamsCase) PolicySpec {
	return PolicySpec{
		ID:                 "p-case",
		PayerAccountID:     tc.payerAccountID,
		AllowedAccountIDs:  tc.allowedAccounts,
		Operation:          tc.operation,
		Payee:              tc.payee,
		StartsAt:           t0.Add(tc.startsAt),
		EndsAt:             t0.Add(tc.endsAt),
		MaxPerRequest:      tc.maxPerRequest,
		MaxTotal:           tc.maxTotal,
		ApprovalThreshold:  tc.approvalThreshold,
		ApprovalWait:       tc.approvalWait,
		MaxReserveDuration: tc.maxReserve,
	}
}

// policyCaseBaseTime 是表驱动用例的固定基准时间，与 newTestWallet 的时钟一致。
var policyCaseBaseTime = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// TestPolicyParamsSaveAndRestoreAgree 验证同一组策略参数在保存与恢复两个
// 入口得到完全一致的合法性结论与具体原因——参数规则只维护一份。
func TestPolicyParamsSaveAndRestoreAgree(t *testing.T) {
	t0 := policyCaseBaseTime
	for _, tc := range policyParamsCases() {
		t.Run(tc.name, func(t *testing.T) {
			// ---- 保存入口 ----
			w := New()
			w.now = func() time.Time { return t0 }
			mustAccount(t, w, "payer", 100)
			mustAccount(t, w, "u1", 0)
			spec := policySpecAt(t0, tc)
			saveErr := w.SavePolicy(spec)

			// ---- 恢复入口：用同一组参数构造只含账户与该策略的最小备份 ----
			backup := backupV1{
				Version:    backupVersion,
				ExportedAt: timeJSON(t0),
				Accounts: []accountBackupV1{
					{ID: "payer", CreatedAt: timeJSON(t0)},
					{ID: "u1", CreatedAt: timeJSON(t0)},
				},
				Sessions: []sessionBackupV1{},
				Policies: []policyBackupV1{{
					ID:                 "p-case",
					PayerAccountID:     tc.payerAccountID,
					AllowedAccountIDs:  tc.allowedAccounts,
					Operation:          tc.operation,
					Payee:              tc.payee,
					StartsAt:           timeJSON(t0.Add(tc.startsAt)),
					EndsAt:             timeJSON(t0.Add(tc.endsAt)),
					MaxPerRequest:      tc.maxPerRequest,
					MaxTotal:           tc.maxTotal,
					ApprovalThreshold:  tc.approvalThreshold,
					ApprovalWait:       durationJSON(tc.approvalWait),
					MaxReserveDuration: durationJSON(tc.maxReserve),
				}},
				Requests: []requestBackupV1{},
				Ledger:   []ledgerEntryBackupV1{},
			}
			data, err := json.Marshal(&backup)
			if err != nil {
				t.Fatal(err)
			}
			restored, restoreErr := restoreAt(data, t0)

			if tc.wantErr == "" {
				if saveErr != nil {
					t.Fatalf("SavePolicy: %v", saveErr)
				}
				if restoreErr != nil {
					t.Fatalf("Restore: %v", restoreErr)
				}
				pv, err := w.Policy("p-case")
				if err != nil {
					t.Fatal(err)
				}
				assertPolicyParamsPreserved(t, pv, tc)
				rv, err := restored.Policy("p-case")
				if err != nil {
					t.Fatal(err)
				}
				assertPolicyParamsPreserved(t, rv, tc)
				return
			}

			if saveErr == nil {
				t.Fatalf("SavePolicy accepted invalid params")
			}
			if !errors.Is(saveErr, ErrPolicyInvalid) {
				t.Fatalf("save err = %v, want ErrPolicyInvalid", saveErr)
			}
			if !strings.Contains(saveErr.Error(), tc.wantErr) {
				t.Fatalf("save err = %q, want reason %q", saveErr.Error(), tc.wantErr)
			}
			if _, err := w.Policy("p-case"); !errors.Is(err, ErrPolicyNotFound) {
				t.Fatalf("failed save must not leave a policy behind: %v", err)
			}
			if restoreErr == nil || restored != nil {
				t.Fatalf("Restore accepted invalid params (wallet=%v)", restored)
			}
			if !errors.Is(restoreErr, ErrBackupInvalid) {
				t.Fatalf("restore err = %v, want ErrBackupInvalid", restoreErr)
			}
			if !strings.Contains(restoreErr.Error(), tc.wantErr) {
				t.Fatalf("restore err = %q, want reason %q", restoreErr.Error(), tc.wantErr)
			}
			// 恢复错误说明保留策略编号与具体原因。
			if !strings.Contains(restoreErr.Error(), `"p-case"`) {
				t.Fatalf("restore err = %q must identify policy p-case", restoreErr.Error())
			}
		})
	}
}

func assertPolicyParamsPreserved(t *testing.T, pv PolicyView, tc policyParamsCase) {
	t.Helper()
	if pv.ApprovalThreshold != tc.approvalThreshold ||
		pv.ApprovalWait != tc.approvalWait ||
		pv.MaxReserveDuration != tc.maxReserve ||
		pv.MaxPerRequest != tc.maxPerRequest ||
		pv.MaxTotal != tc.maxTotal {
		t.Fatalf("policy params not preserved: got threshold=%d wait=%v reserve=%v per=%d total=%d; want threshold=%d wait=%v reserve=%v per=%d total=%d",
			pv.ApprovalThreshold, pv.ApprovalWait, pv.MaxReserveDuration, pv.MaxPerRequest, pv.MaxTotal,
			tc.approvalThreshold, tc.approvalWait, tc.maxReserve, tc.maxPerRequest, tc.maxTotal)
	}
}

// TestNegativeApprovalWaitRoundTripsAndKeepsServing 关闭审批时负的等待时长
// 可以保存、导出并恢复，查询保留原值；恢复后的钱包仍可继续处理原有的直接
// 代付请求（结算成功），策略配置不变。
func TestNegativeApprovalWaitRoundTripsAndKeepsServing(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	spec := validPolicy(c)
	spec.ApprovalThreshold = 0
	spec.ApprovalWait = -90 * time.Second
	spec.MaxReserveDuration = 0
	if err := w.SavePolicy(spec); err != nil {
		t.Fatalf("save negative wait with approval off: %v", err)
	}
	in := baseApply()
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}

	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, c.t)
	if err != nil {
		t.Fatalf("restore must keep negative wait when approval is off: %v", err)
	}
	pv, err := w2.Policy("p1")
	if err != nil {
		t.Fatal(err)
	}
	if pv.ApprovalWait != -90*time.Second {
		t.Fatalf("ApprovalWait = %v, want -90s", pv.ApprovalWait)
	}
	// 原有代付请求仍处于已预留，可在恢复出的钱包里继续结算。
	req, err := w2.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if req.State != RequestReserved {
		t.Fatalf("request state = %v, want RequestReserved", req.State)
	}
	if _, err := w2.Settle("u1", "r1", 20); err != nil {
		t.Fatalf("settle after restore: %v", err)
	}
}

// TestDuplicateAllowedAccountsStayEntrySpecific 保存时同一允许账户重复出现
// 按同一个授权账户接受；备份中同一策略重复列出授权账户仍被拒绝——共用
// 参数判断不能把这类备份悄悄去重修正。
func TestDuplicateAllowedAccountsStayEntrySpecific(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)

	spec := validPolicy(c)
	spec.AllowedAccountIDs = []string{"u1", "u1"}
	if err := w.SavePolicy(spec); err != nil {
		t.Fatalf("save dedups repeated allowed account: %v", err)
	}

	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	m := mustMap(t, data)
	for _, p := range m["policies"].([]interface{}) {
		pm := p.(map[string]interface{})
		if pm["id"] == "p1" {
			allowed := pm["allowed_account_ids"].([]interface{})
			pm["allowed_account_ids"] = append(allowed, allowed[0])
		}
	}
	if w2, err := Restore(mustRemap(t, m)); err == nil {
		t.Fatalf("restore with duplicated allowed account must fail, got wallet %v", w2)
	} else if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("restore err = %v, want ErrBackupInvalid", err)
	}
}

// TestSavePolicyInvalidLeavesWalletUntouched 参数不合法的保存不得创建策略，
// 也不得改动已有余额、请求与账本。
func TestSavePolicyInvalidLeavesWalletUntouched(t *testing.T) {
	w, c := setupApply(t)
	balancesBefore, err := w.Balance("payer")
	if err != nil {
		t.Fatal(err)
	}
	ledgerBefore := len(w.Ledger())

	bad := validPolicy(c)
	bad.ID = "p-bad"
	bad.MaxReserveDuration = -time.Nanosecond
	if err := w.SavePolicy(bad); !errors.Is(err, ErrPolicyInvalid) {
		t.Fatalf("err = %v, want ErrPolicyInvalid", err)
	}
	if _, err := w.Policy("p-bad"); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("invalid policy was created: %v", err)
	}
	// 原策略仍可查询且内容不变。
	if _, err := w.Policy("p1"); err != nil {
		t.Fatalf("existing policy touched: %v", err)
	}
	balancesAfter, err := w.Balance("payer")
	if err != nil {
		t.Fatal(err)
	}
	if balancesAfter != balancesBefore {
		t.Fatalf("balances changed: before %+v after %+v", balancesBefore, balancesAfter)
	}
	if got := len(w.Ledger()); got != ledgerBefore {
		t.Fatalf("ledger entries changed: before %d after %d", ledgerBefore, got)
	}
}

// TestSavePolicyUnknownAccountStaysRecognizable 引用不存在账户时仍同时可
// 识别为策略无效与账户不存在（参数检查先通过、账户引用在锁内报告）。
func TestSavePolicyUnknownAccountStaysRecognizable(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)

	bad := validPolicy(c)
	bad.ID = "p-ghost-payer"
	bad.PayerAccountID = "ghost"
	if err := w.SavePolicy(bad); !errors.Is(err, ErrPolicyInvalid) {
		t.Fatalf("err = %v, want ErrPolicyInvalid", err)
	} else if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("err = %v, want ErrAccountNotFound", err)
	}

	bad = validPolicy(c)
	bad.ID = "p-ghost-allowed"
	bad.AllowedAccountIDs = []string{"ghost"}
	if err := w.SavePolicy(bad); !errors.Is(err, ErrPolicyInvalid) {
		t.Fatalf("err = %v, want ErrPolicyInvalid", err)
	} else if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("err = %v, want ErrAccountNotFound", err)
	}
}
