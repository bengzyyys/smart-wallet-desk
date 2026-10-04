package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildSubmissionTimingBackup 构造一笔请求计时完全由提交时刻 created 推出的
// 备份：策略窗口 [t0-1h, t0+24h)、申请会话到期 t0+36h、审批门槛 10、等待 1h、
// 预留时长 2h。fee>10 走审批路径（携带等待截止与批准信息），否则直接预留。
// 请求的等待截止、预留、决定等时刻均由 created 推导，因此只要 created 落在
// 合法范围内，整份备份就是自洽的；把 created 挪到窗口外即可构造“唯一问题
// 是提交时刻”的备份。mutate 在组装前调整请求/策略/会话字段。
func buildSubmissionTimingBackup(t *testing.T, t0, created time.Time, state RequestState, fee int64, mutate func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)) []byte {
	t.Helper()
	r := requestBackupV1{
		PolicyID: "p", RequestID: "r1", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee: fee,
		State:        int(state),
		CreatedAt:    timeJSON(created),
	}
	approvalRequired := fee > 10
	reservedAt := created.Add(30 * time.Minute)

	var payerAvail, payerReserved, policyReserved, policySpent int64
	switch state {
	case RequestReserved, RequestSettled, RequestCancelled, RequestReservationExpired:
		// 发生过预留的状态：预留计时三元组与审批路径字段齐全。
		r.ReservedAt = timeJSON(reservedAt)
		r.ReserveDuration = durationJSON(2 * time.Hour)
		r.ReserveDeadline = timeJSON(reservedAt.Add(2 * time.Hour))
		if approvalRequired {
			r.WaitDeadline = timeJSON(created.Add(time.Hour))
			r.DecidedAt = timeJSON(reservedAt)
			r.ApproverAccountID = "payer"
		}
	}
	switch state {
	case RequestReserved:
		payerAvail, payerReserved, policyReserved = 100-fee, fee, fee
	case RequestSettled:
		r.ActualFee = fee
		r.SettledAt = r.ReservedAt
		payerAvail, policySpent = 100-fee, fee
	case RequestCancelled:
		// 已预留后取消：费用已全额退回。
		payerAvail = 100
	case RequestReservationExpired:
		r.ReserveExpiredAt = r.ReserveDeadline
		payerAvail = 100
	case RequestPendingApproval:
		r.WaitDeadline = timeJSON(created.Add(time.Hour))
		payerAvail = 100
	case RequestRejected:
		r.WaitDeadline = timeJSON(created.Add(time.Hour))
		r.DecidedAt = timeJSON(created.Add(30 * time.Minute))
		r.ApproverAccountID = "payer"
		r.RejectReason = "payer declined"
		payerAvail = 100
	case RequestExpired:
		r.WaitDeadline = timeJSON(created.Add(time.Hour))
		r.DecidedAt = timeJSON(created.Add(time.Hour))
		payerAvail = 100
	default:
		t.Fatalf("unsupported state %v", state)
	}

	pol := policyBackupV1{
		ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "charge", Payee: "shop",
		StartsAt:           timeJSON(t0.Add(-time.Hour)),
		EndsAt:             timeJSON(t0.Add(24 * time.Hour)),
		MaxPerRequest:      100,
		MaxTotal:           1000,
		ApprovalThreshold:  10,
		ApprovalWait:       durationJSON(time.Hour),
		MaxReserveDuration: durationJSON(2 * time.Hour),
		ReservedTotal:      policyReserved,
		SpentTotal:         policySpent,
	}
	sess := sessionBackupV1{
		ID: "s1", AccountID: "u1", DeviceID: "d1",
		ExpiresAt: timeJSON(t0.Add(36 * time.Hour)), CreatedAt: timeJSON(t0.Add(-2 * time.Hour)),
	}
	if mutate != nil {
		mutate(&r, &pol, &sess)
	}

	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: payerAvail, Reserved: payerReserved, CreatedAt: timeJSON(t0.Add(-2 * time.Hour))},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0.Add(-2 * time.Hour))},
		},
		Sessions: []sessionBackupV1{sess},
		Policies: []policyBackupV1{pol},
		Requests: []requestBackupV1{r},
		Ledger:   []ledgerEntryBackupV1{},
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// assertSubmissionTimingRejected 恢复必须失败、包装 ErrBackupInvalid，并在错误
// 信息中点名使用账户与请求编号；不得返回部分恢复的钱包。
func assertSubmissionTimingRejected(t *testing.T, data []byte, at time.Time) string {
	t.Helper()
	w2, err := restoreAt(data, at)
	if err == nil {
		if w2 != nil {
			t.Fatalf("invalid submission history restored a wallet: %+v", w2)
		}
		t.Fatal("invalid submission history restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, `"u1"`) || !strings.Contains(msg, `"r1"`) {
		t.Fatalf("error %q must name usage account and request id", msg)
	}
	return msg
}

// TestRestoreSubmissionTimingAccepted 验证落在策略窗口内且早于会话到期的提交
// 时刻合法：恰在策略开始时刻、窗口结束前一纳秒、会话到期前一纳秒提交都可以
// 接受；申请当时符合条件的请求即使恢复时会话已到期、策略已结束也照常恢复，
// 不重新计时。
func TestRestoreSubmissionTimingAccepted(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("submitted exactly at policy start", func(t *testing.T) {
		data := buildSubmissionTimingBackup(t, t0, t0.Add(-time.Hour), RequestReserved, 5, nil)
		if _, err := restoreAt(data, t0); err != nil {
			t.Fatalf("submission exactly at policy start must be legal: %v", err)
		}
	})

	t.Run("submitted one nanosecond before policy end", func(t *testing.T) {
		created := t0.Add(24*time.Hour - time.Nanosecond)
		data := buildSubmissionTimingBackup(t, t0, created, RequestReserved, 5, nil)
		if _, err := restoreAt(data, created.Add(time.Minute)); err != nil {
			t.Fatalf("submission 1ns before policy end must be legal: %v", err)
		}
	})

	t.Run("submitted one nanosecond before session expiry", func(t *testing.T) {
		created := t0.Add(12*time.Hour - time.Nanosecond)
		data := buildSubmissionTimingBackup(t, t0, created, RequestReserved, 5, func(_ *requestBackupV1, _ *policyBackupV1, s *sessionBackupV1) {
			s.ExpiresAt = timeJSON(t0.Add(12 * time.Hour))
		})
		if _, err := restoreAt(data, created.Add(time.Minute)); err != nil {
			t.Fatalf("submission 1ns before session expiry must be legal: %v", err)
		}
	})

	t.Run("approved request submitted within window", func(t *testing.T) {
		data := buildSubmissionTimingBackup(t, t0, t0, RequestReserved, 20, nil)
		if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
			t.Fatalf("in-window approved request: %v", err)
		}
	})

	t.Run("valid submission restores long after windows end without retiming", func(t *testing.T) {
		// 关闭预留超时，使请求在很久以后仍处于已预留，专注核对提交时间。
		data := buildSubmissionTimingBackup(t, t0, t0, RequestReserved, 5, func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
			p.MaxReserveDuration = 0
			r.ReserveDuration = 0
			r.ReserveDeadline = timeJSON(time.Time{})
		})
		// 恢复时会话（t0+36h）与策略窗口（t0+24h）均已结束：申请当时合法即可。
		w2, err := restoreAt(data, t0.Add(72*time.Hour))
		if err != nil {
			t.Fatalf("in-window submission must restore after windows end: %v", err)
		}
		r, _ := w2.Request("u1", "r1")
		if r.State != RequestReserved || !r.CreatedAt.Equal(t0) {
			t.Fatalf("restored request changed: %+v", r)
		}
	})

	t.Run("same instant in another timezone judged identically", func(t *testing.T) {
		// 恰在策略开始时刻提交，合法；把备份中的 created_at 改写为同一瞬间的
		// +08:00 表示，判定必须相同。
		data := buildSubmissionTimingBackup(t, t0, t0.Add(-time.Hour), RequestReserved, 5, nil)
		rewrite := func(createdAt string) []byte {
			var m map[string]interface{}
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatal(err)
			}
			m["requests"].([]interface{})[0].(map[string]interface{})["created_at"] = createdAt
			out, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		if _, err := restoreAt(rewrite("2026-01-01T19:00:00+08:00"), t0); err != nil {
			t.Fatalf("same instant in +08:00 must be accepted: %v", err)
		}
		// 同一瞬间的另一种表示若落在窗口开始前 1 纳秒，同样必须拒绝。
		msg := assertSubmissionTimingRejected(t, rewrite("2026-01-01T18:59:59.999999999+08:00"), t0)
		if !strings.Contains(msg, "window start") {
			t.Fatalf("error %q must explain policy window start", msg)
		}
	})
}

// TestRestoreRejectsSubmissionBeforePolicyStart 验证提交时刻早于策略开始时刻
// 的备份必须整体拒绝——对所有状态统一适用（直接预留与审批路径、后来已结算、
// 取消、拒绝或过期的请求都不豁免），即使余额与策略累计金额完全一致，即使该
// 笔预留在恢复时早已应当超时退回。
func TestRestoreRejectsSubmissionBeforePolicyStart(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	created := t0.Add(-2 * time.Hour) // 策略窗口自 t0-1h 起，提交早了 1 小时。

	cases := []struct {
		name  string
		state RequestState
		fee   int64
	}{
		{"direct reserved", RequestReserved, 5},
		{"approved reserved", RequestReserved, 20},
		{"settled", RequestSettled, 20},
		{"cancelled after reserve", RequestCancelled, 20},
		{"reservation expired", RequestReservationExpired, 20},
		{"pending approval", RequestPendingApproval, 20},
		{"rejected", RequestRejected, 20},
		{"expired", RequestExpired, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := buildSubmissionTimingBackup(t, t0, created, tc.state, tc.fee, nil)
			msg := assertSubmissionTimingRejected(t, data, t0)
			if !strings.Contains(msg, "window start") {
				t.Fatalf("error %q must explain policy window start", msg)
			}
		})
	}

	t.Run("rejected even when reservation would be refunded at restore", func(t *testing.T) {
		// 恢复时刻远晚于预留截止（created+2h30m）：不能先退款再接受这份备份。
		data := buildSubmissionTimingBackup(t, t0, created, RequestReserved, 5, nil)
		msg := assertSubmissionTimingRejected(t, data, t0.Add(72*time.Hour))
		if !strings.Contains(msg, "window start") {
			t.Fatalf("error %q must explain policy window start", msg)
		}
	})
}

// TestRestoreRejectsSubmissionAtOrAfterPolicyEnd 验证恰在策略结束时刻或之后
// 提交的请求必须拒绝（窗口含开始、不含结束），直接预留与审批路径一致。
func TestRestoreRejectsSubmissionAtOrAfterPolicyEnd(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name    string
		created time.Time
		fee     int64
	}{
		{"direct exactly at policy end", t0.Add(24 * time.Hour), 5},
		{"direct one nanosecond after policy end", t0.Add(24*time.Hour + time.Nanosecond), 5},
		{"approved exactly at policy end", t0.Add(24 * time.Hour), 20},
		{"approved after policy end", t0.Add(25 * time.Hour), 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := buildSubmissionTimingBackup(t, t0, tc.created, RequestReserved, tc.fee, nil)
			msg := assertSubmissionTimingRejected(t, data, tc.created.Add(time.Minute))
			if !strings.Contains(msg, "window end") {
				t.Fatalf("error %q must explain policy window end", msg)
			}
		})
	}
}

// TestRestoreRejectsSubmissionAtOrAfterSessionExpiry 验证恰在申请会话到期时刻
// 或之后提交的请求必须拒绝，直接预留与审批路径一致；核对依据是备份记载的
// 提交时刻与会话到期时刻，与恢复时的当前时间无关。
func TestRestoreRejectsSubmissionAtOrAfterSessionExpiry(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// 会话到期 t0+12h，落在策略窗口内：隔离出纯粹的会话期限违反。
	shortenSession := func(_ *requestBackupV1, _ *policyBackupV1, s *sessionBackupV1) {
		s.ExpiresAt = timeJSON(t0.Add(12 * time.Hour))
	}

	for _, tc := range []struct {
		name    string
		created time.Time
		fee     int64
	}{
		{"direct exactly at session expiry", t0.Add(12 * time.Hour), 5},
		{"direct one nanosecond after session expiry", t0.Add(12*time.Hour + time.Nanosecond), 5},
		{"approved exactly at session expiry", t0.Add(12 * time.Hour), 20},
		{"approved after session expiry", t0.Add(13 * time.Hour), 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := buildSubmissionTimingBackup(t, t0, tc.created, RequestReserved, tc.fee, shortenSession)
			msg := assertSubmissionTimingRejected(t, data, tc.created.Add(time.Minute))
			if !strings.Contains(msg, "session") {
				t.Fatalf("error %q must explain session expiry", msg)
			}
		})
	}
}

// TestRestoreOrphanRejectionLedgerPreserved 验证未被受理的申请留下的独立拒绝
// 账本记录原样保留：没有可关联的账户或请求，不按已受理请求的提交时间规则
// 拒绝恢复。
func TestRestoreOrphanRejectionLedgerPreserved(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: 100, CreatedAt: timeJSON(t0.Add(-2 * time.Hour))},
			{ID: "u1", Available: 0, CreatedAt: timeJSON(t0.Add(-2 * time.Hour))},
		},
		Sessions: []sessionBackupV1{
			{ID: "s1", AccountID: "u1", DeviceID: "d1", ExpiresAt: timeJSON(t0.Add(36 * time.Hour)), CreatedAt: timeJSON(t0.Add(-2 * time.Hour))},
		},
		Policies: []policyBackupV1{{
			ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
			Operation: "charge", Payee: "shop",
			StartsAt: timeJSON(t0.Add(-time.Hour)), EndsAt: timeJSON(t0.Add(24 * time.Hour)),
			MaxPerRequest: 100, MaxTotal: 1000,
			ApprovalThreshold: 10, ApprovalWait: durationJSON(time.Hour),
		}},
		Requests: []requestBackupV1{},
		Ledger: []ledgerEntryBackupV1{
			// 指向不存在的请求、不存在的账户、以及完全无关联的拒绝记录。
			{Kind: int(LedgerRejection), AccountID: "u1", RequestID: "ghost", Reason: "session expired", At: timeJSON(t0)},
			{Kind: int(LedgerRejection), AccountID: "unknown", RequestID: "r-x", Reason: "account not found", At: timeJSON(t0)},
			{Kind: int(LedgerRejection), Reason: "missing required fields", At: timeJSON(t0)},
		},
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, t0)
	if err != nil {
		t.Fatalf("orphan rejection ledger entries must not block restore: %v", err)
	}
	led := w2.Ledger()
	if len(led) != 3 {
		t.Fatalf("ledger = %+v, want 3 preserved entries", led)
	}
	for i, want := range []struct{ account, request, reason string }{
		{"u1", "ghost", "session expired"},
		{"unknown", "r-x", "account not found"},
		{"", "", "missing required fields"},
	} {
		if led[i].Kind != LedgerRejection || led[i].AccountID != want.account ||
			led[i].RequestID != want.request || led[i].Reason != want.reason {
			t.Fatalf("ledger[%d] = %+v, want rejection %+v preserved", i, led[i], want)
		}
	}
}
