package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildPendingWithDecidedAtBackup 构造一个正常流程无法产生的矛盾备份：
// 请求 r1 仍处于待审批状态（无审批账户、无拒绝原因），却保存了非空的决定
// 时间。除这处矛盾外，其余字段（账户与会话引用、审批门槛、预估费用、等待
// 截止时刻、金额）全部自洽合法。decidedAt 指定保存的决定时间；withOtherLegal
// 为 true 时再加入一笔未开启审批策略 p2 下的合法已预留请求 r2，验证“其他
// 记录合法”不能让冲突被忽略或跳过。
func buildPendingWithDecidedAtBackup(t *testing.T, t0 time.Time, decidedAt time.Time, withOtherLegal bool) []byte {
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
			StartsAt:          timeJSON(t0.Add(-time.Hour)),
			EndsAt:            timeJSON(t0.Add(time.Hour)),
			MaxPerRequest:     100,
			MaxTotal:          1000,
			ApprovalThreshold: 10,
			ApprovalWait:      durationJSON(5 * time.Minute),
		}},
		Requests: []requestBackupV1{{
			PolicyID: "p", RequestID: "r1", AccountID: "u1", PayerAccountID: "payer",
			SessionID: "s1", Operation: "charge", Payee: "shop",
			EstimatedFee: 20,
			State:        int(RequestPendingApproval),
			CreatedAt:    timeJSON(created),
			// 与提交时刻 + 5 分钟等待时长一致，通过等待期限核对。
			WaitDeadline: timeJSON(created.Add(5 * time.Minute)),
			// 矛盾点：待审批却保存了非空决定时间；审批账户与拒绝原因均为空。
			DecidedAt: timeJSON(decidedAt),
		}},
		Ledger: []ledgerEntryBackupV1{},
	}
	if withOtherLegal {
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

// TestRestoreRejectsPendingWithDecidedAt 验证核心修复：备份中只要有一笔待审批
// 请求保存了非空决定时间，恢复必须整体失败、不返回钱包，错误可识别为
// ErrBackupInvalid 并指出使用账户、请求编号与“待审批请求不应带有决定时间”。
// 判断只看备份保存的请求状态，与恢复时刻无关，也不看决定时间落在何处：
// 恰好等于提交时间、落在正常等待期间、晚于等待截止时间，且无论恢复时期限未到、
// 恰到还是早已过去，结果都相同；不能先把该请求自动改成过期再接受备份。
func TestRestoreRejectsPendingWithDecidedAt(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	created := t0.Add(-time.Minute)
	for _, dc := range []struct {
		name      string
		decidedAt time.Time
	}{
		{"decided exactly at submission", created},
		{"decided within the wait window", created.Add(2 * time.Minute)},
		{"decided exactly at wait deadline", created.Add(5 * time.Minute)},
		{"decided after wait deadline", created.Add(time.Hour)},
	} {
		for _, rc := range []struct {
			name      string
			restoreAt time.Time
		}{
			{"wait deadline still in the future", t0.Add(1 * time.Minute)},
			{"exactly at wait deadline", t0.Add(4 * time.Minute)},
			{"long after wait deadline", t0.Add(time.Hour)},
		} {
			for _, mixed := range []bool{false, true} {
				name := dc.name + " / " + rc.name
				if mixed {
					name += " (other records legal)"
				}
				t.Run(name, func(t *testing.T) {
					data := buildPendingWithDecidedAtBackup(t, t0, dc.decidedAt, mixed)
					w2, err := restoreAt(data, rc.restoreAt)
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
					for _, want := range []string{"u1", "r1", "pending approval", "decided"} {
						if !strings.Contains(msg, want) {
							t.Fatalf("error %q must identify %q (usage account, request, pending state, decided-at conflict)", msg, want)
						}
					}
				})
			}
		}
	}
}

// TestRestoreLegalPendingHasNoDecidedAt 验证合法的待审批请求（决定时间为空）
// 仍按已有功能恢复：尚未到期的继续等待、出资账户可在期限内批准；到期的照常
// 进入等待审批过期终态，不冻结或退回费用。
func TestRestoreLegalPendingHasNoDecidedAt(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	created := t0.Add(-time.Minute)
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: 1000, Reserved: 0, CreatedAt: timeJSON(t0)},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)},
		},
		Sessions: []sessionBackupV1{
			{ID: "s1", AccountID: "u1", DeviceID: "dev1", ExpiresAt: timeJSON(t0.Add(time.Hour)), CreatedAt: timeJSON(t0)},
			{ID: "sa", AccountID: "payer", DeviceID: "deva", ExpiresAt: timeJSON(t0.Add(2 * time.Hour)), CreatedAt: timeJSON(t0)},
		},
		Policies: []policyBackupV1{{
			ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
			Operation: "charge", Payee: "shop",
			StartsAt:          timeJSON(t0.Add(-time.Hour)),
			EndsAt:            timeJSON(t0.Add(time.Hour)),
			MaxPerRequest:     100,
			MaxTotal:          1000,
			ApprovalThreshold: 10,
			ApprovalWait:      durationJSON(5 * time.Minute),
		}},
		Requests: []requestBackupV1{{
			PolicyID: "p", RequestID: "r1", AccountID: "u1", PayerAccountID: "payer",
			SessionID: "s1", Operation: "charge", Payee: "shop",
			EstimatedFee: 20,
			State:        int(RequestPendingApproval),
			CreatedAt:    timeJSON(created),
			WaitDeadline: timeJSON(created.Add(5 * time.Minute)),
			// 决定时间为空：合法待审批。
		}},
		Ledger: []ledgerEntryBackupV1{},
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}

	// 期限未到：继续等待，出资账户仍可在期限内批准并预留费用。
	w2, err := restoreAt(data, t0)
	if err != nil {
		t.Fatalf("legal pending request must restore before deadline: %v", err)
	}
	r, err := w2.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != RequestPendingApproval || !r.DecidedAt.IsZero() {
		t.Fatalf("restored request = %+v, want pending with zero decided-at", r)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 1000, Reserved: 0}) {
		t.Fatalf("pending request must not hold funds, balance = %+v", bal)
	}
	if _, err := w2.Approve("u1", "r1", "sa", "deva"); err != nil {
		t.Fatalf("approve after restore: %v", err)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 980, Reserved: 20}) {
		t.Fatalf("balance after approve = %+v, want 980/20", bal)
	}

	// 到期恢复：照常进入过期终态并留下状态记录，不冻结或退回费用。
	w3, err := restoreAt(data, created.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("legal pending request must restore at deadline: %v", err)
	}
	r3, err := w3.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if r3.State != RequestExpired {
		t.Fatalf("state = %v, want expired at restore time", r3.State)
	}
	if r3.DecidedAt.IsZero() {
		t.Fatal("expired request must record the expiration decided-at")
	}
	if bal, _ := w3.Balance("payer"); bal != (Balances{Available: 1000, Reserved: 0}) {
		t.Fatalf("expiration must not move funds, balance = %+v", bal)
	}
}

// TestRestoreKeepsDecidedAtForLegalTerminalHistories 验证已经批准、拒绝、取消
// 或过期的合法历史保留原有决定时间，不因本次校验被误拒绝或清空。
func TestRestoreKeepsDecidedAtForLegalTerminalHistories(t *testing.T) {
	w, c := newTestWallet()
	t0 := c.t
	mustAccount(t, w, "payer", 1000)
	mustAccount(t, w, "u1", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "deva", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(PolicySpec{
		ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "charge", Payee: "shop",
		StartsAt: t0.Add(-time.Hour), EndsAt: t0.Add(2 * time.Hour),
		MaxPerRequest: 100, MaxTotal: 1000,
		ApprovalThreshold: 10, ApprovalWait: 5 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}

	// r-approve：期限内批准后预留（决定时间即预留时间）。
	if _, err := w.Apply(RequestInput{
		PolicyID: "p", RequestID: "r-approve", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 20,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Approve("u1", "r-approve", "sa", "deva"); err != nil {
		t.Fatal(err)
	}
	// r-reject：出资账户期限内拒绝。
	if _, err := w.Apply(RequestInput{
		PolicyID: "p", RequestID: "r-reject", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 21,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Reject("u1", "r-reject", "sa", "deva", "no"); err != nil {
		t.Fatal(err)
	}
	// r-cancel：待审批阶段取消（从未预留）。
	if _, err := w.Apply(RequestInput{
		PolicyID: "p", RequestID: "r-cancel", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 22,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Cancel("u1", "r-cancel"); err != nil {
		t.Fatal(err)
	}
	// r-expire：越过等待截止时刻后惰性过期。
	if _, err := w.Apply(RequestInput{
		PolicyID: "p", RequestID: "r-expire", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 23,
	}); err != nil {
		t.Fatal(err)
	}
	c.t = t0.Add(10 * time.Minute)
	if _, err := w.Request("u1", "r-expire"); err != nil {
		t.Fatal(err)
	}

	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("legal terminal histories with decided-at must restore: %v", err)
	}
	want := map[string]RequestState{
		"r-approve": RequestReserved,
		"r-reject":  RequestRejected,
		"r-cancel":  RequestCancelled,
		"r-expire":  RequestExpired,
	}
	for id, state := range want {
		r, err := w2.Request("u1", id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State != state {
			t.Fatalf("request %s state = %v, want %v", id, r.State, state)
		}
		if r.DecidedAt.IsZero() {
			t.Fatalf("request %s decided-at was cleared on restore", id)
		}
	}
}
