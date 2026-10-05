package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildRevokedPendingBackup 构造一个正常流程无法产生的矛盾备份：
// 使用账户 u1 的申请会话 s1 已吊销，但其下存在一笔仍处于待审批状态的请求
// r1。除这处矛盾外，其余字段（账户、会话归属、策略、审批门槛、费用、等待
// 截止时刻、余额）全部自洽合法；出资账户 payer 自己用于审批的会话 sa 仍然
// 有效且余额充足，用于验证“审批会话有效”不能使矛盾备份恢复成功。
// withOtherLegalRecords 为 true 时再加入使用账户 u2 的未吊销会话 s2 及其
// 合法待审批请求 r2，验证“其他记录完全合法”不能让冲突被忽略。
func buildRevokedPendingBackup(t *testing.T, t0 time.Time, withOtherLegalRecords bool) []byte {
	t.Helper()
	created := t0.Add(-time.Minute)
	allowed := []string{"u1"}
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: 100, Reserved: 0, CreatedAt: timeJSON(t0)},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)},
		},
		Sessions: []sessionBackupV1{
			// 申请会话已吊销。
			{ID: "s1", AccountID: "u1", DeviceID: "dev1", ExpiresAt: timeJSON(t0.Add(time.Hour)), Revoked: true, CreatedAt: timeJSON(t0)},
			// 出资账户自己的审批会话仍然有效。
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
		allowed = append(allowed, "u2")
		b.Accounts = append(b.Accounts, accountBackupV1{ID: "u2", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)})
		b.Sessions = append(b.Sessions, sessionBackupV1{
			ID: "s2", AccountID: "u2", DeviceID: "dev2", ExpiresAt: timeJSON(t0.Add(time.Hour)), CreatedAt: timeJSON(t0),
		})
		b.Policies[0].AllowedAccountIDs = allowed
		b.Requests = append(b.Requests, requestBackupV1{
			PolicyID: "p", RequestID: "r2", AccountID: "u2", PayerAccountID: "payer",
			SessionID: "s2", Operation: "charge", Payee: "shop",
			EstimatedFee: 20,
			State:        int(RequestPendingApproval),
			CreatedAt:    timeJSON(created),
			WaitDeadline: timeJSON(created.Add(5 * time.Minute)),
		})
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestRestoreRejectsPendingUnderRevokedSession 验证核心修复：备份中只要有
// 一笔待审批请求关联已吊销的申请会话，恢复必须整体失败、不返回钱包，错误可
// 识别为 ErrBackupInvalid 并指出使用账户、请求编号、申请会话编号与冲突原因。
// 判断只看备份保存的吊销标记与请求状态，与恢复时刻无关：等待期限未到、恰到、
// 早已过去结果相同，不会先把该请求按当前时间转成过期再接受备份；出资账户
// 自己的有效审批会话、同份备份中其他完全合法的请求都不能放行。
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

// TestRestoreValidApproverSessionCannotSaveRevokedPending 是攻击场景的对照：
// 矛盾备份中出资账户的审批会话有效、余额充足，一旦错误放行恢复，出资账户就能
// 把本应无法继续等待的请求批准成已预留。先断言带吊销标记的备份恢复失败；再
// 仅清除吊销标记（其余一字段不动），同一份备份即可恢复且批准成功，证明矛盾点
// 正是“已吊销申请会话 + 待审批请求”，而非任何其他校验。
func TestRestoreValidApproverSessionCannotSaveRevokedPending(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildRevokedPendingBackup(t, t0, false)
	if w2, err := restoreAt(data, t0.Add(time.Minute)); err == nil {
		t.Fatalf("contradictory backup restored: %+v, err nil", w2)
	}

	// 仅清除会话吊销标记，重新序列化：其余内容（含出资账户有效审批会话）不变。
	var b backupV1
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	for i := range b.Sessions {
		if b.Sessions[i].ID == "s1" {
			b.Sessions[i].Revoked = false
		}
	}
	sane, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(sane, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("same backup without revoke flag should restore: %v", err)
	}
	// 恢复出的请求仍在等待期限内，出资账户用自己的有效会话即可批准并占用余额。
	if r, err := w2.Request("u1", "r1"); err != nil || r.State != RequestPendingApproval {
		t.Fatalf("state = %v err = %v, want pending", r.State, err)
	}
	if _, err := w2.Approve("u1", "r1", "sa", "deva"); err != nil {
		t.Fatalf("approve after restore: %v", err)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance after approve = %+v, want 80/20", bal)
	}
}

// TestRestoreKeepsRevokedSessionWithLegalHistories 验证已吊销会话本身及其
// 合法历史请求仍可恢复：吊销前已批准预留的请求继续保持已预留（可继续结算/
// 取消/等待预留超时），吊销在期限内产生的拒绝保持拒绝，吊销时已到期限产生
// 的过期保持过期，吊销标记、决定信息与账本顺序原样保留，恢复不新增吊销、
// 拒绝或过期记录；同钱包中未吊销会话下的合法待审批请求也照常恢复、可继续
// 审批，到期按原规则过期；独立的申请拒绝账本记录不关联请求，原样保留。
func TestRestoreKeepsRevokedSessionWithLegalHistories(t *testing.T) {
	t.Run("reserved before revocation stays reserved", func(t *testing.T) {
		w, c := setupApproval(t)
		t0 := c.t
		if _, err := w.Apply(approvalApply()); err != nil {
			t.Fatal(err)
		}
		// 超门槛：出资账户批准后预留，再吊销申请会话。
		if _, err := w.Approve("u1", "r1", "sa", "dev-approve"); err != nil {
			t.Fatal(err)
		}
		c.t = t0.Add(30 * time.Second)
		if err := w.RevokeSession("s1"); err != nil {
			t.Fatal(err)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		// 无论在什么时刻恢复（策略未启用预留超时，预留不会自动释放），吊销前
		// 已预留的请求都保持已预留。
		for _, at := range []time.Time{c.t, t0.Add(2 * time.Minute), t0.Add(time.Hour)} {
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
			sess, err := w2.Session("s1")
			if err != nil {
				t.Fatal(err)
			}
			if sess.State != SessionRevoked {
				t.Fatalf("session state = %v, want revoked", sess.State)
			}
			if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 80, Reserved: 20}) {
				t.Fatalf("balance after restore = %+v, want 80/20", bal)
			}
		}
		// 恢复后已预留请求仍可正常结算。
		w2, err := restoreAt(data, t0.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w2.Settle("u1", "r1", 20); err != nil {
			t.Fatalf("settle reserved request after restore: %v", err)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 80, Reserved: 0}) {
			t.Fatalf("balance after settle = %+v, want 80/0", bal)
		}
		// 恢复不新增账本记录：同刻导出/恢复再导出逐字节一致。
		w3, err := restoreAt(data, c.t)
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
		w, c := setupApproval(t)
		t0 := c.t
		if _, err := w.Apply(approvalApply()); err != nil {
			t.Fatal(err)
		}
		// 等待期限内吊销：待审批请求随吊销立即拒绝。
		c.t = t0.Add(30 * time.Second)
		if err := w.RevokeSession("s1"); err != nil {
			t.Fatal(err)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		// 很久以后恢复：仍是吊销拒绝，不追加过期记录、不产生资金变动。
		w2, err := restoreAt(data, t0.Add(time.Hour))
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
		r, err := w2.Request("u1", "r1")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestRejected || !strings.Contains(r.RejectReason, "session revoked") {
			t.Fatalf("request = %+v, want rejected with revocation reason", r)
		}
		if r.ApproverAccountID != "" {
			t.Fatalf("revocation rejection must not carry approver: %q", r.ApproverAccountID)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
			t.Fatalf("balance = %+v, want no funds held", bal)
		}
		if got := len(w2.Ledger()); got != len(w.Ledger()) {
			t.Fatalf("ledger entries changed: got %d want %d", got, len(w.Ledger()))
		}
	})

	t.Run("expired at deadline then revoked stays expired", func(t *testing.T) {
		w, c := setupApproval(t)
		t0 := c.t
		if _, err := w.Apply(approvalApply()); err != nil {
			t.Fatal(err)
		}
		// 越过等待截止时刻后吊销：请求按自身截止时间进入过期终态。
		c.t = t0.Add(time.Minute + time.Second)
		if err := w.RevokeSession("s1"); err != nil {
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
		if r.State != RequestExpired || r.RejectReason != "" || r.ApproverAccountID != "" {
			t.Fatalf("request = %+v, want plain expiration", r)
		}
		if got := len(w2.Ledger()); got != len(w.Ledger()) {
			t.Fatalf("ledger entries changed: got %d want %d", got, len(w.Ledger()))
		}
	})

	t.Run("pending under another non-revoked session still restores", func(t *testing.T) {
		w, c := setupApproval(t)
		t0 := c.t
		// s1/u1 与 s2/u2 各一笔待审批；吊销 s1 只结清 r1，r2 不受影响。
		if _, err := w.Apply(approvalApply()); err != nil {
			t.Fatal(err)
		}
		in2 := approvalApply()
		in2.AccountID, in2.SessionID, in2.DeviceID, in2.RequestID = "u2", "s2", "dev2", "r2"
		if _, err := w.Apply(in2); err != nil {
			t.Fatal(err)
		}
		c.t = t0.Add(30 * time.Second)
		if err := w.RevokeSession("s1"); err != nil {
			t.Fatal(err)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		// 期限未到恢复：r2 待审批原样恢复，仍可被批准并预留费用；r1 保持吊销拒绝。
		w2, err := restoreAt(data, c.t)
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
		r1, _ := w2.Request("u1", "r1")
		if r1.State != RequestRejected {
			t.Fatalf("r1 state = %v, want rejection preserved", r1.State)
		}
		r2, err := w2.Request("u2", "r2")
		if err != nil {
			t.Fatal(err)
		}
		if r2.State != RequestPendingApproval {
			t.Fatalf("r2 state = %v, want pending under non-revoked session", r2.State)
		}
		if _, err := w2.Approve("u2", "r2", "sa", "dev-approve"); err != nil {
			t.Fatalf("approve after restore: %v", err)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 80, Reserved: 20}) {
			t.Fatalf("balance after approve = %+v, want 80/20", bal)
		}
		// 到期之后恢复：r2 按原规则进入过期终态。
		w3, err := restoreAt(data, t0.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("restore after deadline: %v", err)
		}
		r3, err := w3.Request("u2", "r2")
		if err != nil {
			t.Fatal(err)
		}
		if r3.State != RequestExpired {
			t.Fatalf("r2 state = %v, want expired at restore time", r3.State)
		}
	})

	t.Run("standalone application rejection ledger survives", func(t *testing.T) {
		// 已吊销会话、没有任何请求，仅保留一条独立的申请拒绝账本记录：它不属于
		// 待审批请求，不携带会话关联，恢复必须成功且原样保留该记录。
		t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		b := backupV1{
			Version:    backupVersion,
			ExportedAt: timeJSON(t0),
			Accounts: []accountBackupV1{
				{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)},
			},
			Sessions: []sessionBackupV1{
				{ID: "s1", AccountID: "u1", DeviceID: "dev1", ExpiresAt: timeJSON(t0.Add(time.Hour)), Revoked: true, CreatedAt: timeJSON(t0)},
			},
			Policies: []policyBackupV1{},
			Requests: []requestBackupV1{},
			Ledger: []ledgerEntryBackupV1{{
				Kind:      int(LedgerRejection),
				AccountID: "u1",
				RequestID: "lone-reject",
				Reason:    "session revoked before a new application",
				At:        timeJSON(t0.Add(-time.Minute)),
			}},
		}
		data, err := json.Marshal(&b)
		if err != nil {
			t.Fatal(err)
		}
		w2, err := restoreAt(data, t0.Add(time.Hour))
		if err != nil {
			t.Fatalf("backup with only a standalone rejection record must restore: %v", err)
		}
		entries := w2.AccountLedger("u1")
		if len(entries) != 1 || entries[0].Kind != LedgerRejection || entries[0].RequestID != "lone-reject" {
			t.Fatalf("standalone rejection ledger not preserved: %+v", entries)
		}
	})
}
