package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// approvedHistorySpec 描述一笔“曾经批准成功并实际预留费用”的大额请求备份。
// 时间字段均可显式控制，用于构造正常审批流程不可能产生的历史。
type approvedHistorySpec struct {
	state RequestState

	created       time.Time // 申请提交时刻
	decided       time.Time // 批准决定时刻（= 实际预留时刻）
	waitDeadline  time.Time // 备份记载的等待截止时刻；零值表示按规则应得的最早值
	omitDeadline  bool      // 显式不写等待截止时刻（构造缺失损坏）
	policyEnd     time.Time // 策略结束时间
	sessionExpiry time.Time // 申请会话到期时间
	wait          time.Duration
	reserveDur    time.Duration

	fee       int64
	threshold int64
	actualFee int64 // 已结算时使用

	restoreTime time.Time
}

// buildApprovedHistoryBackup 按 spec 构造一份除“审批历史时间”外完全自洽的
// 备份：账户余额、策略预留/已花费总额随请求状态匹配。出资账户 payer 初始
// 余额 1000，费用 20；使用账户 u1（会话 s1/设备 d1）。
func buildApprovedHistoryBackup(t *testing.T, spec approvedHistorySpec) []byte {
	t.Helper()
	const initial int64 = 1000
	t0 := spec.created

	deadline := spec.waitDeadline
	if !spec.omitDeadline && deadline.IsZero() {
		deadline = t0.Add(spec.wait)
		if spec.policyEnd.Before(deadline) {
			deadline = spec.policyEnd
		}
		if spec.sessionExpiry.Before(deadline) {
			deadline = spec.sessionExpiry
		}
	}

	r := requestBackupV1{
		PolicyID: "p", RequestID: "big", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee:      spec.fee,
		ActualFee:         spec.actualFee,
		State:             int(spec.state),
		CreatedAt:         timeJSON(spec.created),
		WaitDeadline:      timeJSON(deadline),
		ReservedAt:        timeJSON(spec.decided),
		ReserveDuration:   durationJSON(spec.reserveDur),
		ApproverAccountID: "payer",
		DecidedAt:         timeJSON(spec.decided),
	}
	if spec.reserveDur > 0 {
		r.ReserveDeadline = timeJSON(spec.decided.Add(spec.reserveDur))
	}

	var available, reserved, reservedTotal, spentTotal int64
	switch spec.state {
	case RequestReserved:
		available = initial - spec.fee
		reserved = spec.fee
		reservedTotal = spec.fee
	case RequestSettled:
		available = initial - spec.actualFee
		r.SettledAt = timeJSON(spec.decided.Add(time.Minute))
		spentTotal = spec.actualFee
	case RequestCancelled:
		available = initial
	case RequestReservationExpired:
		available = initial
		r.ReserveExpiredAt = r.ReserveDeadline
	default:
		t.Fatalf("unsupported state in builder: %v", spec.state)
	}

	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(spec.restoreTime),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: available, Reserved: reserved, CreatedAt: timeJSON(t0)},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)},
		},
		Sessions: []sessionBackupV1{
			{ID: "s1", AccountID: "u1", DeviceID: "d1", ExpiresAt: timeJSON(spec.sessionExpiry), CreatedAt: timeJSON(t0)},
			{ID: "sa", AccountID: "payer", DeviceID: "deva", ExpiresAt: timeJSON(spec.sessionExpiry), CreatedAt: timeJSON(t0)},
		},
		Policies: []policyBackupV1{{
			ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
			Operation: "charge", Payee: "shop",
			StartsAt:           timeJSON(t0.Add(-time.Hour)),
			EndsAt:             timeJSON(spec.policyEnd),
			MaxPerRequest:      100,
			MaxTotal:           1000,
			ApprovalThreshold:  spec.threshold,
			ApprovalWait:       durationJSON(spec.wait),
			MaxReserveDuration: durationJSON(spec.reserveDur),
			ReservedTotal:      reservedTotal,
			SpentTotal:         spentTotal,
		}},
		Requests: []requestBackupV1{r},
		Ledger:   []ledgerEntryBackupV1{},
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// validApprovedSpec 返回一份合法的“提交后 5 分钟批准、等待期限 10 分钟”
// 已预留大额请求规格，各用例在此基础上改坏单个时间字段。
func validApprovedSpec(state RequestState, restoreTime time.Time) approvedHistorySpec {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	spec := approvedHistorySpec{
		state:         state,
		created:       t0,
		decided:       t0.Add(5 * time.Minute),
		policyEnd:     t0.Add(time.Hour),
		sessionExpiry: t0.Add(2 * time.Hour),
		wait:          10 * time.Minute,
		reserveDur:    30 * time.Minute,
		fee:           20,
		threshold:     10,
		restoreTime:   restoreTime,
	}
	if state == RequestSettled {
		spec.actualFee = 12
	}
	if state == RequestReservationExpired {
		// 预留超时终态：恢复时刻晚于预留截止时刻（decided+30m）。
		spec.restoreTime = t0.Add(2 * time.Hour)
	}
	return spec
}

// TestRestoreRejectsImpossibleApprovalHistory 验证：对曾经批准成功并实际预留
// 费用的大额请求（恢复时仍已预留，或后来已结算、已取消、预留超时），等待
// 截止时间缺失/被改长/改短，或批准发生在提交时刻之前、等待截止时刻及之后，
// 都是正常审批无法产生的历史，Restore 必须返回 ErrBackupInvalid 并说明使用
// 账户、请求编号与时间不合法的原因，且不返回部分恢复的钱包。
func TestRestoreRejectsImpossibleApprovalHistory(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	later := t0.Add(24 * time.Hour)

	states := []struct {
		name  string
		state RequestState
	}{
		{"still reserved", RequestReserved},
		{"later settled", RequestSettled},
		{"later cancelled", RequestCancelled},
		{"later reservation expired", RequestReservationExpired},
	}

	wantFor := func(t *testing.T, err error, parts ...string) {
		t.Helper()
		if err == nil {
			t.Fatalf("corrupt approval history restored without error")
		}
		if !errors.Is(err, ErrBackupInvalid) {
			t.Fatalf("err = %v, want ErrBackupInvalid", err)
		}
		msg := err.Error()
		for _, p := range parts {
			if !strings.Contains(msg, p) {
				t.Fatalf("error %q must mention %q", msg, p)
			}
		}
	}

	for _, sc := range states {
		t.Run(sc.name, func(t *testing.T) {
			// 1) 等待截止时间缺失。
			spec := validApprovedSpec(sc.state, later)
			spec.omitDeadline = true
			data := buildApprovedHistoryBackup(t, spec)
			w2, err := restoreAt(data, spec.restoreTime)
			if w2 != nil {
				t.Fatalf("missing deadline backup returned a partial wallet")
			}
			wantFor(t, err, "u1", "big", "wait deadline")

			// 2) 等待截止时间被改长（多给 1 分钟）。
			spec = validApprovedSpec(sc.state, later)
			spec.waitDeadline = spec.created.Add(11 * time.Minute)
			data = buildApprovedHistoryBackup(t, spec)
			w2, err = restoreAt(data, spec.restoreTime)
			if w2 != nil {
				t.Fatalf("lengthened deadline backup returned a partial wallet")
			}
			wantFor(t, err, "u1", "big", "wait deadline")

			// 3) 等待截止时间被改短（少 1 分钟）——批准时刻反而落在线外。
			spec = validApprovedSpec(sc.state, later)
			spec.waitDeadline = spec.created.Add(4 * time.Minute)
			data = buildApprovedHistoryBackup(t, spec)
			w2, err = restoreAt(data, spec.restoreTime)
			if w2 != nil {
				t.Fatalf("shortened deadline backup returned a partial wallet")
			}
			wantFor(t, err, "u1", "big", "wait deadline")

			// 4) 恰在等待截止时刻批准：必须拒绝（批准窗口为 [提交, 截止)）。
			spec = validApprovedSpec(sc.state, later)
			spec.decided = spec.created.Add(10 * time.Minute)
			data = buildApprovedHistoryBackup(t, spec)
			w2, err = restoreAt(data, spec.restoreTime)
			if w2 != nil {
				t.Fatalf("approval exactly at deadline returned a partial wallet")
			}
			wantFor(t, err, "u1", "big", "wait deadline")

			// 5) 晚于等待截止时刻才批准：必须拒绝。
			spec = validApprovedSpec(sc.state, later)
			spec.decided = spec.created.Add(12 * time.Minute)
			data = buildApprovedHistoryBackup(t, spec)
			w2, err = restoreAt(data, spec.restoreTime)
			if w2 != nil {
				t.Fatalf("late approval returned a partial wallet")
			}
			wantFor(t, err, "u1", "big", "wait deadline")

			// 6) 批准决定早于申请提交：必须拒绝。
			spec = validApprovedSpec(sc.state, later)
			spec.decided = spec.created.Add(-time.Minute)
			data = buildApprovedHistoryBackup(t, spec)
			w2, err = restoreAt(data, spec.restoreTime)
			if w2 != nil {
				t.Fatalf("approval before creation returned a partial wallet")
			}
			wantFor(t, err, "u1", "big", "created_at")
		})
	}
}

// TestRestoreRejectsDueReservedImpossibleHistory 验证：即使损坏的大额预留
// 在恢复时已过预留截止时刻、按现有规则本来会自动全额退回，整个备份仍必须
// 被拒绝——不能借“恢复即超时退回”让绕过等待期限的资金占用过关。
func TestRestoreRejectsDueReservedImpossibleHistory(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	spec := validApprovedSpec(RequestReserved, t0.Add(2*time.Hour))
	// 批准被改到等待截止时刻（非法），预留截止 t0+10m+1m 早已过去。
	spec.decided = spec.created.Add(10 * time.Minute)
	spec.reserveDur = time.Minute
	data := buildApprovedHistoryBackup(t, spec)

	w2, err := restoreAt(data, spec.restoreTime)
	if err == nil {
		t.Fatalf("corrupt approval history that would auto-refund at restore was accepted")
	}
	if w2 != nil {
		t.Fatalf("corrupt backup returned a partial wallet")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "u1") || !strings.Contains(msg, "big") {
		t.Fatalf("error %q must name usage account and request id", msg)
	}
}

// TestRestoreAcceptsValidApprovalBoundaries 验证审批窗口边界：恰在提交时刻
// 批准合法；截止时刻前 1 纳秒批准合法；等待截止由策略结束时间或申请会话
// 到期时间决定时，只要严格早于该截止也合法。
func TestRestoreAcceptsValidApprovalBoundaries(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	later := t0.Add(24 * time.Hour)

	// 批准恰在提交时刻。
	spec := validApprovedSpec(RequestReserved, later)
	spec.decided = spec.created
	data := buildApprovedHistoryBackup(t, spec)
	if _, err := restoreAt(data, later); err != nil {
		t.Fatalf("approval exactly at created_at must be accepted: %v", err)
	}

	// 批准在截止时刻前 1 纳秒。
	spec = validApprovedSpec(RequestSettled, later)
	spec.decided = spec.created.Add(10*time.Minute - time.Nanosecond)
	data = buildApprovedHistoryBackup(t, spec)
	if _, err := restoreAt(data, later); err != nil {
		t.Fatalf("approval 1ns before deadline must be accepted: %v", err)
	}

	// 等待期限由策略结束时间决定：截止 == policyEnd，此前批准合法。
	spec = validApprovedSpec(RequestReserved, later)
	spec.policyEnd = spec.created.Add(3 * time.Minute)
	spec.decided = spec.created.Add(2 * time.Minute)
	data = buildApprovedHistoryBackup(t, spec)
	if _, err := restoreAt(data, later); err != nil {
		t.Fatalf("approval before policy-end-bounded deadline must be accepted: %v", err)
	}
	// 恰在策略结束时刻批准必须拒绝。
	spec.decided = spec.created.Add(3 * time.Minute)
	data = buildApprovedHistoryBackup(t, spec)
	if _, err := restoreAt(data, later); err == nil {
		t.Fatalf("approval exactly at policy end must be rejected")
	} else if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}

	// 等待期限由申请会话到期时间决定：截止 == sessionExpiry，此前批准合法。
	spec = validApprovedSpec(RequestReservationExpired, later)
	spec.sessionExpiry = spec.created.Add(7 * time.Minute)
	spec.decided = spec.created.Add(6 * time.Minute)
	data = buildApprovedHistoryBackup(t, spec)
	if _, err := restoreAt(data, spec.restoreTime); err != nil {
		t.Fatalf("approval before session-expiry-bounded deadline must be accepted: %v", err)
	}
}

// TestRestoreApprovedWithinWindowDoesNotRetime 端到端验证：期限内经正常
// 申请/批准流程预留的大额请求，即使恢复时原等待期限、申请会话与策略时间窗
// 都已结束，仍原样恢复、不重新计时；恰在提交时刻批准也不例外。未到预留
// 截止时间的继续保留原预留，已到期的沿用全额退回。
func TestRestoreApprovedWithinWindowDoesNotRetime(t *testing.T) {
	w, c := newTestWallet()
	t0 := c.t
	mustAccount(t, w, "payer", 1000)
	mustAccount(t, w, "u1", 0)
	if _, err := w.CreateSession("s1", "u1", "d1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "da", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(PolicySpec{
		ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "charge", Payee: "shop",
		StartsAt: t0.Add(-time.Hour), EndsAt: t0.Add(time.Hour),
		MaxPerRequest: 100, MaxTotal: 1000,
		ApprovalThreshold: 10, ApprovalWait: 5 * time.Minute,
		MaxReserveDuration: 10 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}

	applyIn := func(rid string, fee int64) {
		t.Helper()
		if _, err := w.Apply(RequestInput{
			PolicyID: "p", RequestID: rid, AccountID: "u1", SessionID: "s1",
			DeviceID: "d1", Operation: "charge", Payee: "shop", EstimatedFee: fee,
		}); err != nil {
			t.Fatalf("apply %s: %v", rid, err)
		}
	}

	// 时钟停在 t0：申请与批准同一时刻完成（决定时刻恰为提交时刻），关闭预留
	// 超时效果不受影响（这里启用 10 分钟预留超时）。
	applyIn("instant", 20)
	if _, err := w.Approve("u1", "instant", "sa", "da"); err != nil {
		t.Fatalf("approve instant: %v", err)
	}

	// 另一笔在等待期限内批准、预留截止早于恢复时刻：恢复时应沿用全额退回。
	applyIn("due", 20)
	c.t = t0.Add(2 * time.Minute)
	if _, err := w.Approve("u1", "due", "sa", "da"); err != nil {
		t.Fatalf("approve due: %v", err)
	}

	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	// 远在等待期限（t0+5m）、策略窗口（t0+1h）、会话到期（t0+1h）之后恢复：
	// instant 预留截止 t0+10m、due 预留截止 t0+12m，均已过，自动全额退回，
	// 但请求历史合法，不得被判为非法备份。
	w2, err2 := restoreAt(data, t0.Add(24*time.Hour))
	if err2 != nil {
		t.Fatalf("valid in-window approvals must restore long after windows ended: %v", err2)
	}
	for _, rid := range []string{"instant", "due"} {
		r, err := w2.Request("u1", rid)
		if err != nil {
			t.Fatalf("request %s: %v", rid, err)
		}
		if r.State != RequestReservationExpired {
			t.Fatalf("request %s state = %v, want reservation expired", rid, r.State)
		}
		if !r.ReserveExpiredAt.Equal(r.ReserveDeadline) {
			t.Fatalf("request %s expired at %v, want deadline %v", rid, r.ReserveExpiredAt, r.ReserveDeadline)
		}
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 1000, Reserved: 0}) {
		t.Fatalf("balance = %+v, want full refund", bal)
	}
}

// TestRestoreDirectReservedNeedsNoApprovalInfo 验证未超过审批门槛、直接预留
// 的请求不被要求补上等等待截止或批准信息（即便策略开启了审批）。
func TestRestoreDirectReservedNeedsNoApprovalInfo(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	spec := validApprovedSpec(RequestReserved, t0)
	spec.fee = 5 // 不严格超过门槛 10。
	// builder 以 decided 兼作预留时刻：给一个正常受理时刻，随后在 JSON 层
	// 删除批准人、决定时刻与等待截止时间，模拟直接预留备份；恢复时刻取
	// t0，预留尚未到期。
	spec.decided = spec.created
	spec.restoreTime = t0
	data := buildApprovedHistoryBackup(t, spec)

	m := mustMap(t, data)
	rm := m["requests"].([]interface{})[0].(map[string]interface{})
	delete(rm, "approver_account_id")
	delete(rm, "decided_at")
	delete(rm, "wait_deadline")
	// 余额按费用 5 重新对齐。
	for _, a := range m["accounts"].([]interface{}) {
		am := a.(map[string]interface{})
		if am["id"] == "payer" {
			am["available"] = int64(1000 - spec.fee)
			am["reserved"] = int64(spec.fee)
		}
	}
	for _, p := range m["policies"].([]interface{}) {
		pm := p.(map[string]interface{})
		pm["reserved_total"] = int64(spec.fee)
	}
	if _, err := restoreAt(mustRemap(t, m), t0); err != nil {
		t.Fatalf("below-threshold directly reserved request must not require approval info: %v", err)
	}
}
