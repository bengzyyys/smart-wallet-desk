package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildPendingDecidedBackup 构造一个正常流程无法产生的矛盾备份：请求 r1 仍
// 处于待审批状态（审批账户与拒绝原因均为空），却保存了非零的决定时间
// decidedAt。除这处矛盾外，其余字段（账户与会话引用、审批门槛、预估费用、
// 等待截止时刻、金额）全部自洽合法。withOtherLegalRecords 为 true 时再加入
// 一条未开启审批策略 p2 下的合法已预留请求 r2，验证“同一备份中还有其他合法
// 请求”不能让恢复只跳过出错请求或清空其决定时间后继续。
func buildPendingDecidedBackup(t *testing.T, t0 time.Time, decidedAt time.Time, withOtherLegalRecords bool) []byte {
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
			// 矛盾所在：待审批请求保存了非零决定时间。
			DecidedAt: timeJSON(decidedAt),
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

// TestRestoreRejectsPendingRequestWithDecidedAt 验证核心修复：备份中只要有一笔
// 待审批请求保存了非零决定时间，恢复必须整体失败、不返回钱包，错误可识别为
// ErrBackupInvalid 并指出使用账户、请求编号与冲突原因（待审批不应带有决定
// 时间）。判断只看备份保存的请求状态，与恢复时刻无关：等待期限未到、恰到、
// 早已过去结果相同，不会先把该请求按当前时间自动过期再接受备份；决定时间
// 无论恰好等于提交时刻、落在正常等待期间还是晚于等待截止时刻，都不能与
// 待审批状态并存；同一备份中的其他合法请求也不能让恢复放行。
func TestRestoreRejectsPendingRequestWithDecidedAt(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	created := t0.Add(-time.Minute)
	// r1 提交于 t0-1m，等待截止 t0+4m。
	for _, dc := range []struct {
		name      string
		decidedAt time.Time
	}{
		{"decided exactly at submission", created},
		{"decided within the wait window", t0.Add(time.Minute)},
		{"decided after the wait deadline", t0.Add(10 * time.Minute)},
	} {
		for _, tc := range []struct {
			name      string
			restoreAt time.Time
		}{
			{"wait deadline still in the future", t0.Add(time.Minute)},
			{"exactly at wait deadline", t0.Add(4 * time.Minute)},
			{"long after wait deadline", t0.Add(time.Hour)},
		} {
			for _, mixed := range []bool{false, true} {
				name := dc.name + ", " + tc.name
				if mixed {
					name += " (other records legal)"
				}
				t.Run(name, func(t *testing.T) {
					data := buildPendingDecidedBackup(t, t0, dc.decidedAt, mixed)
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
					for _, want := range []string{"u1", "r1", "pending approval", "decision time"} {
						if !strings.Contains(msg, want) {
							t.Fatalf("error %q must identify %q (usage account, request, conflict)", msg, want)
						}
					}
				})
			}
		}
	}
}

// TestRestoreKeepsLegalPendingRequestWithoutDecidedAt 验证合法待审批请求（决定
// 时间为空、其余条件合法）仍按已有功能恢复：尚未到期的继续等待，出资账户可在
// 期限内批准或拒绝；到期的照常进入等待审批过期终态并留下原有状态记录，不冻结
// 或退回费用。
func TestRestoreKeepsLegalPendingRequestWithoutDecidedAt(t *testing.T) {
	setup := func(t *testing.T) (*Wallet, *clock) {
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
		return w, c
	}

	t.Run("not yet due keeps waiting and can be approved or rejected", func(t *testing.T) {
		w, c := setup(t)
		t0 := c.t
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		// 期限未到恢复：待审批原样恢复，决定时间仍为空，不冻结费用。
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
		if !r.DecidedAt.IsZero() {
			t.Fatalf("decided at = %v, want empty", r.DecidedAt)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 1000, Reserved: 0}) {
			t.Fatalf("balance = %+v, want no funds held", bal)
		}
		// 期限内出资账户仍可批准并预留费用。
		if _, err := w2.Approve("u1", "r1", "sa", "deva"); err != nil {
			t.Fatalf("approve after restore: %v", err)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 980, Reserved: 20}) {
			t.Fatalf("balance after approve = %+v, want 980/20", bal)
		}
		// 同样期限内出资账户也可以拒绝。
		w3, err := restoreAt(data, t0.Add(time.Minute))
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
		if _, err := w3.Reject("u1", "r1", "sa", "deva", "not now"); err != nil {
			t.Fatalf("reject after restore: %v", err)
		}
		r3, err := w3.Request("u1", "r1")
		if err != nil {
			t.Fatal(err)
		}
		if r3.State != RequestRejected {
			t.Fatalf("state = %v, want rejected", r3.State)
		}
	})

	t.Run("due pending expires with its record and no fund movement", func(t *testing.T) {
		w, c := setup(t)
		t0 := c.t
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		ledgerBefore := len(w.Ledger())
		// 到期后恢复：照常进入等待审批过期终态并留下状态记录，不冻结或退回费用。
		w2, err := restoreAt(data, t0.Add(time.Hour))
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
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 1000, Reserved: 0}) {
			t.Fatalf("balance = %+v, want no funds held", bal)
		}
		entries := w2.Ledger()
		if len(entries) != ledgerBefore+1 {
			t.Fatalf("ledger len = %d, want %d (one expiration record)", len(entries), ledgerBefore+1)
		}
		last := entries[len(entries)-1]
		if last.Kind != LedgerExpiration || last.Amount != 0 {
			t.Fatalf("last entry = %+v, want zero-amount expiration record", last)
		}
	})
}

// TestRestoreKeepsDecidedAtOfLegalTerminalHistories 验证已经批准、拒绝、取消或
// 过期的合法历史保留原有决定时间，不因本次校验被误拒绝或清空。
func TestRestoreKeepsDecidedAtOfLegalTerminalHistories(t *testing.T) {
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
	apply := func(id string) {
		t.Helper()
		if _, err := w.Apply(RequestInput{
			PolicyID: "p", RequestID: id, AccountID: "u1", SessionID: "s1",
			DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 20,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 批准：r1 在期限内被出资账户批准并预留。
	apply("r1")
	c.t = t0.Add(time.Minute)
	approvedAt := c.t
	if _, err := w.Approve("u1", "r1", "sa", "deva"); err != nil {
		t.Fatal(err)
	}
	// 拒绝：r2 在期限内被出资账户拒绝。
	apply("r2")
	c.t = t0.Add(2 * time.Minute)
	rejectedAt := c.t
	if _, err := w.Reject("u1", "r2", "sa", "deva", "no"); err != nil {
		t.Fatal(err)
	}
	// 待审批取消：r3 在期限内由使用账户取消。
	apply("r3")
	c.t = t0.Add(3 * time.Minute)
	cancelledAt := c.t
	if _, err := w.Cancel("u1", "r3"); err != nil {
		t.Fatal(err)
	}
	// 过期：r4 越过等待期限后进入过期终态。
	apply("r4")
	c.t = t0.Add(10 * time.Minute)
	if _, err := w.Export(); err != nil {
		t.Fatal(err)
	}
	r4, err := w.Request("u1", "r4")
	if err != nil {
		t.Fatal(err)
	}
	if r4.State != RequestExpired {
		t.Fatalf("r4 state = %v, want expired", r4.State)
	}
	expiredAt := r4.DecidedAt

	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	// 很久以后恢复：四类历史的决定时间都原样保留，不被误拒绝或清空。
	w2, err := restoreAt(data, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, tc := range []struct {
		id        string
		state     RequestState
		decidedAt time.Time
	}{
		{"r1", RequestReserved, approvedAt},
		{"r2", RequestRejected, rejectedAt},
		{"r3", RequestCancelled, cancelledAt},
		{"r4", RequestExpired, expiredAt},
	} {
		r, err := w2.Request("u1", tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State != tc.state {
			t.Fatalf("%s state = %v, want %v", tc.id, r.State, tc.state)
		}
		if !r.DecidedAt.Equal(tc.decidedAt) {
			t.Fatalf("%s decided at = %v, want %v (preserved)", tc.id, r.DecidedAt, tc.decidedAt)
		}
	}
	if got := len(w2.Ledger()); got != len(w.Ledger()) {
		t.Fatalf("ledger entries changed: got %d want %d", got, len(w.Ledger()))
	}
}
