package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildDeactivatedPendingBackup 构造一个正常流程无法产生的矛盾备份：
// 策略 p 已停用，但其下存在一笔仍处于待审批状态的请求 r1。除这处矛盾外，
// 其余字段（账户与会话引用、审批门槛、等待截止时刻、金额）全部自洽合法。
// withOtherLegalRecords 为 true 时再加入一条未停用策略 p2 及其合法已预留
// 请求，验证“其他记录合法”不能让冲突被忽略。
func buildDeactivatedPendingBackup(t *testing.T, t0 time.Time, withOtherLegalRecords bool) []byte {
	t.Helper()
	created := t0.Add(-time.Minute)
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: 100, Reserved: 0, CreatedAt: timeJSON(t0)},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)},
		},
		Sessions: []sessionBackupV1{
			{ID: "s1", AccountID: "u1", DeviceID: "dev1", ExpiresAt: timeJSON(t0.Add(time.Hour)), CreatedAt: timeJSON(t0)},
			{ID: "sa", AccountID: "payer", DeviceID: "deva", ExpiresAt: timeJSON(t0.Add(2 * time.Hour)), CreatedAt: timeJSON(t0)},
		},
		Policies: []policyBackupV1{{
			ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
			Operation: "charge", Payee: "shop",
			StartsAt:             timeJSON(t0.Add(-time.Hour)),
			EndsAt:               timeJSON(t0.Add(time.Hour)),
			MaxPerRequest:        100,
			MaxTotal:             1000,
			ApprovalThreshold:    10,
			ApprovalWait:         durationJSON(5 * time.Minute),
			Deactivated:          true,
			DeactivatedAt:        timeJSON(t0.Add(-2 * time.Minute)),
			DeactivatorAccountID: "payer",
			DeactivateReason:     "stop it",
		}},
		Requests: []requestBackupV1{{
			PolicyID: "p", RequestID: "r1", AccountID: "u1", PayerAccountID: "payer",
			SessionID: "s1", Operation: "charge", Payee: "shop",
			EstimatedFee: 20,
			State:        int(RequestPendingApproval),
			CreatedAt:    timeJSON(created),
			// 与提交时刻 + 5 分钟等待时长一致，通过等待期限核对。
			WaitDeadline: timeJSON(created.Add(5 * time.Minute)),
		}},
		Ledger: []ledgerEntryBackupV1{},
	}
	if withOtherLegalRecords {
		b.Policies = append(b.Policies, policyBackupV1{
			ID: "p2", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
			Operation: "charge", Payee: "shop",
			StartsAt:      timeJSON(t0.Add(-time.Hour)),
			EndsAt:        timeJSON(t0.Add(time.Hour)),
			MaxPerRequest: 100,
			MaxTotal:      1000,
			ReservedTotal: 5,
		})
		b.Requests = append(b.Requests, requestBackupV1{
			PolicyID: "p2", RequestID: "r2", AccountID: "u1", PayerAccountID: "payer",
			SessionID: "s1", Operation: "charge", Payee: "shop",
			EstimatedFee: 5,
			State:        int(RequestReserved),
			CreatedAt:    timeJSON(t0),
			ReservedAt:   timeJSON(t0),
		})
		for i := range b.Accounts {
			if b.Accounts[i].ID == "payer" {
				b.Accounts[i].Available = 95
				b.Accounts[i].Reserved = 5
			}
		}
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestRestoreRejectsPendingUnderDeactivatedPolicy 验证核心修复：备份中只要有
// 一笔待审批请求关联已停用策略，恢复必须整体失败、不返回钱包，错误可识别为
// ErrBackupInvalid 并指出使用账户、请求编号、策略编号与冲突原因。判断只看
// 备份保存的状态，与恢复时刻无关：等待期限未到、恰到、早已过去结果相同，
// 不会先把该请求自动过期再接受备份。
func TestRestoreRejectsPendingUnderDeactivatedPolicy(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// r1 提交于 t0-1m，等待截止 t0+4m。
	for _, tc := range []struct {
		name      string
		restoreAt time.Time
	}{
		{"wait deadline still in the future", t0.Add(1 * time.Minute)},
		{"exactly at wait deadline", t0.Add(4 * time.Minute)},
		{"long after wait deadline", t0.Add(time.Hour)},
	} {
		for _, mixed := range []bool{false, true} {
			name := tc.name
			if mixed {
				name += " (other records legal)"
			}
			t.Run(name, func(t *testing.T) {
				data := buildDeactivatedPendingBackup(t, t0, mixed)
				w2, err := restoreAt(data, tc.restoreAt)
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
				for _, want := range []string{"u1", "r1", `"p"`, "deactivated policy", "pending approval"} {
					if !strings.Contains(msg, want) {
						t.Fatalf("error %q must identify %q (usage account, request, policy, conflict)", msg, want)
					}
				}
			})
		}
	}
}

// TestRestoreKeepsDeactivatedPolicyWithLegalHistories 验证已停用策略本身及其
// 合法历史请求仍可恢复：停用前已批准预留的请求继续保持已预留（可继续结算/
// 取消/等待预留超时），停用导致的拒绝保持拒绝，停用前已过期的保持过期，
// 首次停用时间、执行账户、理由与账本顺序原样保留，恢复不新增停用或拒绝
// 记录；同钱包中未停用策略下的合法待审批请求也照常恢复。
func TestRestoreKeepsDeactivatedPolicyWithLegalHistories(t *testing.T) {
	t.Run("reserved before deactivation stays reserved", func(t *testing.T) {
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
			ApprovalThreshold: 10, ApprovalWait: 5 * time.Minute,
		}); err != nil {
			t.Fatal(err)
		}
		// 超门槛：先待审批，出资账户批准后预留。
		if _, err := w.Apply(RequestInput{
			PolicyID: "p", RequestID: "r1", AccountID: "u1", SessionID: "s1",
			DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 20,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Approve("u1", "r1", "sa", "deva"); err != nil {
			t.Fatal(err)
		}
		if _, err := w.DeactivatePolicy("p", "sa", "deva", "stop it"); err != nil {
			t.Fatal(err)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		// 无论在什么时刻恢复，停用前已预留的请求都保持已预留，停用信息原样保留。
		for _, at := range []time.Time{t0, t0.Add(4 * time.Minute), t0.Add(time.Hour)} {
			w2, err := restoreAt(data, at)
			if err != nil {
				t.Fatalf("restore at %v: %v", at, err)
			}
			r, err := w2.Request("u1", "r1")
			if err != nil {
				t.Fatal(err)
			}
			if r.State != RequestReserved {
				t.Fatalf("restore at %v: state = %v, want reserved", at, r.State)
			}
			pv, err := w2.Policy("p")
			if err != nil {
				t.Fatal(err)
			}
			if !pv.Deactivated || pv.DeactivatorAccountID != "payer" || pv.DeactivateReason != "stop it" {
				t.Fatalf("deactivation info not preserved: %+v", pv)
			}
			if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 980, Reserved: 20}) {
				t.Fatalf("balance after restore = %+v, want 980/20", bal)
			}
		}
		// 恢复不新增账本记录：同刻导出/恢复再导出逐字节一致。
		w2, err := restoreAt(data, t0)
		if err != nil {
			t.Fatal(err)
		}
		if got := w2.Ledger(); len(got) != len(w.Ledger()) {
			t.Fatalf("ledger entries changed: got %d want %d", len(got), len(w.Ledger()))
		}
		data2, err := w2.Export()
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != string(data2) {
			t.Fatal("restore added records for a legal deactivated-policy backup")
		}
	})

	t.Run("pending rejected by deactivation stays rejected", func(t *testing.T) {
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
			ApprovalThreshold: 10, ApprovalWait: 5 * time.Minute,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Apply(RequestInput{
			PolicyID: "p", RequestID: "r1", AccountID: "u1", SessionID: "s1",
			DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 20,
		}); err != nil {
			t.Fatal(err)
		}
		// 等待期限内停用：待审批请求随停用立即拒绝。
		c.t = t0.Add(time.Minute)
		if _, err := w.DeactivatePolicy("p", "sa", "deva", "stop it"); err != nil {
			t.Fatal(err)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		// 很久以后恢复：仍是停用拒绝，不追加过期记录、不产生资金变动。
		w2, err := restoreAt(data, t0.Add(time.Hour))
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
		r, err := w2.Request("u1", "r1")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestRejected || !strings.Contains(r.RejectReason, "stop it") {
			t.Fatalf("request = %+v, want rejected with deactivation reason", r)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 1000, Reserved: 0}) {
			t.Fatalf("balance = %+v, want no funds held", bal)
		}
		if got := len(w2.Ledger()); got != len(w.Ledger()) {
			t.Fatalf("ledger entries changed: got %d want %d", got, len(w.Ledger()))
		}
	})

	t.Run("expired before deactivation stays expired", func(t *testing.T) {
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
			ApprovalThreshold: 10, ApprovalWait: 5 * time.Minute,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Apply(RequestInput{
			PolicyID: "p", RequestID: "r1", AccountID: "u1", SessionID: "s1",
			DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 20,
		}); err != nil {
			t.Fatal(err)
		}
		// 先等待审批过期，再停用策略：请求保持过期终态。
		c.t = t0.Add(6 * time.Minute)
		if r, err := w.Request("u1", "r1"); err != nil || r.State != RequestExpired {
			t.Fatalf("state = %v err = %v, want expired", r.State, err)
		}
		if _, err := w.DeactivatePolicy("p", "sa", "deva", "stop it"); err != nil {
			t.Fatal(err)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		w2, err := restoreAt(data, c.t.Add(time.Hour))
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
		r, err := w2.Request("u1", "r1")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestExpired {
			t.Fatalf("state = %v, want expired", r.State)
		}
		if got := len(w2.Ledger()); got != len(w.Ledger()) {
			t.Fatalf("ledger entries changed: got %d want %d", got, len(w.Ledger()))
		}
	})

	t.Run("pending under another active policy still restores", func(t *testing.T) {
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
		spec := func(id string) PolicySpec {
			return PolicySpec{
				ID: id, PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
				Operation: "charge", Payee: "shop",
				StartsAt: t0.Add(-time.Hour), EndsAt: t0.Add(time.Hour),
				MaxPerRequest: 100, MaxTotal: 1000,
				ApprovalThreshold: 10, ApprovalWait: 5 * time.Minute,
			}
		}
		if err := w.SavePolicy(spec("p-off")); err != nil {
			t.Fatal(err)
		}
		if err := w.SavePolicy(spec("p-on")); err != nil {
			t.Fatal(err)
		}
		if _, err := w.DeactivatePolicy("p-off", "sa", "deva", "stop it"); err != nil {
			t.Fatal(err)
		}
		// 未停用策略下的合法待审批请求。
		if _, err := w.Apply(RequestInput{
			PolicyID: "p-on", RequestID: "r1", AccountID: "u1", SessionID: "s1",
			DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 20,
		}); err != nil {
			t.Fatal(err)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		// 期限未到：待审批原样恢复，仍可被批准并预留费用。
		w2, err := restoreAt(data, t0.Add(time.Minute))
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
		r, err := w2.Request("u1", "r1")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestPendingApproval {
			t.Fatalf("state = %v, want pending", r.State)
		}
		if _, err := w2.Approve("u1", "r1", "sa", "deva"); err != nil {
			t.Fatalf("approve after restore: %v", err)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 980, Reserved: 20}) {
			t.Fatalf("balance after approve = %+v, want 980/20", bal)
		}
		// 到期时刻恢复：按既有规则自动过期。
		w3, err := restoreAt(data, t0.Add(6*time.Minute))
		if err != nil {
			t.Fatalf("restore at expiry: %v", err)
		}
		r3, err := w3.Request("u1", "r1")
		if err != nil {
			t.Fatal(err)
		}
		if r3.State != RequestExpired {
			t.Fatalf("state = %v, want expired at restore time", r3.State)
		}
	})
}
