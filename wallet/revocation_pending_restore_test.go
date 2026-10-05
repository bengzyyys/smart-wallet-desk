package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildRevokedPendingBackup 构造一个正常流程无法产生的矛盾备份：
// 申请会话 s1 已吊销，但其下存在一笔仍处于待审批状态的请求 r1。除这处矛盾
// 外，其余字段（账户与会话引用、审批门槛、等待截止时刻、金额）全部自洽
// 合法；出资账户用于审批的会话 sa 仍然有效，证明“审批会话有效”不能消除
// 申请会话吊销与待审批请求之间的矛盾。withOtherLegalRecords 为 true 时再
// 加入一条未开启审批策略 p2 下的合法已预留请求 r2（同样提交于已吊销会话
// s1，即吊销前已预留的合法历史），验证“其他记录合法”不能让冲突被忽略。
func buildRevokedPendingBackup(t *testing.T, t0 time.Time, withOtherLegalRecords bool) []byte {
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
			// 提交申请所用的会话已吊销。
			{ID: "s1", AccountID: "u1", DeviceID: "dev1", ExpiresAt: timeJSON(t0.Add(time.Hour)), Revoked: true, CreatedAt: timeJSON(t0)},
			// 出资账户用于审批的会话仍有效：不能使矛盾备份恢复成功。
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
		// 吊销前已直接预留的合法历史：即使申请会话已吊销也照常恢复。
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

// TestRestoreRejectsPendingUnderRevokedSession 验证核心修复：备份中只要有
// 一笔待审批请求关联已吊销的申请会话，恢复必须整体失败、不返回钱包，错误
// 可识别为 ErrBackupInvalid 并指出使用账户、请求编号、申请会话编号与冲突
// 原因。判断只看备份保存的吊销标记与请求状态，与恢复时刻无关：等待期限未
// 到、恰到、早已过去结果相同，不会先把该请求按当前时间自动过期再接受备份；
// 出资账户的审批会话有效也不能放行。
func TestRestoreRejectsPendingUnderRevokedSession(t *testing.T) {
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
				data := buildRevokedPendingBackup(t, t0, mixed)
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
				for _, want := range []string{"u1", "r1", `"s1"`, "revoked", "pending approval"} {
					if !strings.Contains(msg, want) {
						t.Fatalf("error %q must identify %q (usage account, request, session, conflict)", msg, want)
					}
				}
			})
		}
	}
}

// TestRestoreKeepsRevokedSessionWithLegalHistories 验证已吊销会话本身及其
// 合法历史请求仍可恢复：吊销前已批准/直接预留的请求继续保持已预留（可继续
// 结算/取消/等待预留超时），吊销导致的拒绝保持拒绝，恰到或超过等待期限后
// 吊销导致的过期保持过期，状态、金额、决定信息与账本顺序原样保留，恢复不
// 新增吊销拒绝或过期记录；未吊销会话下的合法待审批请求也照常恢复，期限内
// 可继续审批、到期按原规则过期。
func TestRestoreKeepsRevokedSessionWithLegalHistories(t *testing.T) {
	// setupRevokeHistories 构造：payer(1000)/u1，申请会话 s1 与审批会话 sa，
	// 开启审批（门槛 10、等待 5 分钟）的策略 p。
	setupRevokeHistories := func(t *testing.T) (*Wallet, *clock) {
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
		return w, c
	}

	t.Run("reserved before revocation stays reserved", func(t *testing.T) {
		w, c := setupRevokeHistories(t)
		t0 := c.t
		// 费用不超门槛：直接预留后再吊销会话。
		if _, err := w.Apply(RequestInput{
			PolicyID: "p", RequestID: "r1", AccountID: "u1", SessionID: "s1",
			DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 5,
		}); err != nil {
			t.Fatal(err)
		}
		if err := w.RevokeSession("s1"); err != nil {
			t.Fatal(err)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		// 无论在什么时刻恢复，吊销前已预留的请求都保持已预留，吊销标记原样保留。
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
			s, err := w2.Session("s1")
			if err != nil {
				t.Fatal(err)
			}
			if s.State != SessionRevoked {
				t.Fatalf("session state = %v, want revoked", s.State)
			}
			if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 995, Reserved: 5}) {
				t.Fatalf("balance after restore = %+v, want 995/5", bal)
			}
		}
		// 恢复出的已预留请求仍可正常结算；恢复不新增账本记录，再导出逐字节一致。
		w2, err := restoreAt(data, t0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w2.Settle("u1", "r1", 5); err != nil {
			t.Fatalf("settle reserved after restore: %v", err)
		}
		w3, err := restoreAt(data, t0)
		if err != nil {
			t.Fatal(err)
		}
		if got := w3.Ledger(); len(got) != len(w.Ledger()) {
			t.Fatalf("ledger entries changed: got %d want %d", len(got), len(w.Ledger()))
		}
		data2, err := w3.Export()
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != string(data2) {
			t.Fatal("restore added records for a legal revoked-session backup")
		}
	})

	t.Run("pending rejected by revocation stays rejected", func(t *testing.T) {
		w, c := setupRevokeHistories(t)
		t0 := c.t
		if _, err := w.Apply(RequestInput{
			PolicyID: "p", RequestID: "r1", AccountID: "u1", SessionID: "s1",
			DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 20,
		}); err != nil {
			t.Fatal(err)
		}
		// 等待期限内吊销：待审批请求随吊销立即拒绝。
		c.t = t0.Add(time.Minute)
		if err := w.RevokeSession("s1"); err != nil {
			t.Fatal(err)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		// 很久以后恢复：仍是吊销拒绝（无审批账户），不追加过期记录、不动资金。
		w2, err := restoreAt(data, t0.Add(time.Hour))
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
		r, err := w2.Request("u1", "r1")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestRejected {
			t.Fatalf("state = %v, want rejected", r.State)
		}
		if !strings.Contains(r.RejectReason, "revoked") {
			t.Fatalf("reject reason = %q, want revocation reason", r.RejectReason)
		}
		if r.ApproverAccountID != "" {
			t.Fatalf("revocation rejection approver = %q, want empty", r.ApproverAccountID)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 1000, Reserved: 0}) {
			t.Fatalf("balance = %+v, want no funds held", bal)
		}
		if got := len(w2.Ledger()); got != len(w.Ledger()) {
			t.Fatalf("ledger entries changed: got %d want %d", got, len(w.Ledger()))
		}
	})

	t.Run("due pending expired by revocation stays expired", func(t *testing.T) {
		w, c := setupRevokeHistories(t)
		t0 := c.t
		if _, err := w.Apply(RequestInput{
			PolicyID: "p", RequestID: "r1", AccountID: "u1", SessionID: "s1",
			DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 20,
		}); err != nil {
			t.Fatal(err)
		}
		// 越过等待截止时刻（中途不查询该请求）再吊销：按吊销语义进入过期终态。
		c.t = t0.Add(6 * time.Minute)
		revokeAt := c.t
		if err := w.RevokeSession("s1"); err != nil {
			t.Fatal(err)
		}
		r0, _ := w.Request("u1", "r1")
		if r0.State != RequestExpired || r0.RejectReason != "" || r0.ApproverAccountID != "" {
			t.Fatalf("source request = %+v, want plain expiration", r0)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		// 很久以后恢复：保持过期终态与吊销时的决定时刻，不补拒绝记录、不动资金。
		w2, err := restoreAt(data, t0.Add(2*time.Hour))
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
		if r.RejectReason != "" || r.ApproverAccountID != "" {
			t.Fatalf("expired request carries revocation fields: reason %q approver %q", r.RejectReason, r.ApproverAccountID)
		}
		if !r.DecidedAt.Equal(revokeAt) {
			t.Fatalf("decided at %v, want revoke time %v", r.DecidedAt, revokeAt)
		}
		if got := len(w2.Ledger()); got != len(w.Ledger()) {
			t.Fatalf("ledger entries changed: got %d want %d", got, len(w.Ledger()))
		}
	})

	t.Run("pending under a non-revoked session still restores", func(t *testing.T) {
		w, c := setupRevokeHistories(t)
		t0 := c.t
		// 另一个已吊销但没有待审批请求的会话：不影响 s1 下请求的恢复。
		if _, err := w.CreateSession("s3", "u1", "dev3", t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Apply(RequestInput{
			PolicyID: "p", RequestID: "r1", AccountID: "u1", SessionID: "s1",
			DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 20,
		}); err != nil {
			t.Fatal(err)
		}
		if err := w.RevokeSession("s3"); err != nil {
			t.Fatal(err)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		// 期限未到：待审批原样恢复，仍可被出资账户批准并预留费用。
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
		w3, err := restoreAt(data, t0.Add(5*time.Minute))
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

// TestRestoreStandaloneRejectionEntriesUnrelatedToRevokedSession 验证独立的
// 申请拒绝账本记录不属于待审批请求：已吊销会话存在时，这些没有请求（因而
// 也没有会话关联）的拒绝留痕仍原样恢复，本次修复不删除它们、也不对它们
// 新增会话关联要求。
func TestRestoreStandaloneRejectionEntriesUnrelatedToRevokedSession(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: 100, Reserved: 0, CreatedAt: timeJSON(t0)},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)},
		},
		// 已吊销会话下没有任何请求。
		Sessions: []sessionBackupV1{
			{ID: "s1", AccountID: "u1", DeviceID: "dev1", ExpiresAt: timeJSON(t0.Add(time.Hour)), Revoked: true, CreatedAt: timeJSON(t0)},
		},
		Policies: []policyBackupV1{},
		Requests: []requestBackupV1{},
		// 两条未被受理申请留下的独立拒绝留痕：不关联请求、不要求会话。
		Ledger: []ledgerEntryBackupV1{
			{
				Kind: int(LedgerRejection), AccountID: "ghost", RequestID: "no-such-request",
				Reason: "wallet: account not found: ghost", At: timeJSON(t0.Add(-time.Hour)),
			},
			{
				Kind: int(LedgerRejection), AccountID: "u1", RequestID: "unknown",
				Reason: "wallet: session revoked: s1", At: timeJSON(t0.Add(-time.Minute)),
			},
		},
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("standalone rejection entries must restore with revoked session: %v", err)
	}
	entries := w2.Ledger()
	if len(entries) != 2 {
		t.Fatalf("ledger len = %d, want 2", len(entries))
	}
	for i, e := range entries {
		if e.Kind != LedgerRejection {
			t.Fatalf("entry %d kind = %v, want rejection", i, e.Kind)
		}
	}
	if entries[0].AccountID != "ghost" || entries[1].RequestID != "unknown" {
		t.Fatalf("standalone rejection entries not preserved: %+v", entries)
	}
}
