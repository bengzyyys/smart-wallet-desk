package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// submissionState 命名一组待构造的请求状态变体，覆盖直接预留与审批两条
// 路径以及全部后续状态（含终态）。
type submissionState int

const (
	subDirectReserved submissionState = iota
	subApprovedReserved
	subDirectSettled
	subApprovedSettled
	subCancelledFromReserved
	subCancelledFromPending
	subPending
	subRejected
	subExpired
	subReservationExpired
)

// buildSubmissionTimingBackup 构造一份除提交时刻外完全自洽的备份：策略时间窗
// [t0, t0+4h)、审批门槛 10（等待 1h）、最长预留时长 2h，申请会话 s1 到期
// t0+4h；请求提交于 created，其余计时字段（等待截止、批准/决定、预留、结算、
// 超时）都由 created 推导，因此恢复能否成功只取决于提交时刻是否落在策略
// 时间窗内且早于会话到期。mutate 可在序列化前进一步调整请求/策略/会话字段。
func buildSubmissionTimingBackup(t *testing.T, t0 time.Time, state submissionState, created time.Time, mutate func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)) []byte {
	t.Helper()
	const (
		directFee   int64 = 5  // 未超门槛：直接预留路径
		approvalFee int64 = 20 // 严格超门槛：审批路径
		actualFee   int64 = 3
	)
	pol := policyBackupV1{
		ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "charge", Payee: "shop",
		StartsAt:           timeJSON(t0),
		EndsAt:             timeJSON(t0.Add(4 * time.Hour)),
		MaxPerRequest:      100,
		MaxTotal:           1000,
		ApprovalThreshold:  10,
		ApprovalWait:       durationJSON(time.Hour),
		MaxReserveDuration: durationJSON(2 * time.Hour),
	}
	sess := sessionBackupV1{
		ID: "s1", AccountID: "u1", DeviceID: "d1",
		ExpiresAt: timeJSON(t0.Add(4 * time.Hour)), CreatedAt: timeJSON(t0.Add(-time.Hour)),
	}
	r := requestBackupV1{
		PolicyID: "p", RequestID: "rq", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		CreatedAt: timeJSON(created),
	}
	// 审批路径的等待截止时刻：提交+等待时长、策略结束、会话到期三者最早值。
	waitDeadline := created.Add(time.Hour)
	if wl := time.Time(pol.EndsAt); wl.Before(waitDeadline) {
		waitDeadline = wl
	}
	if wl := time.Time(sess.ExpiresAt); wl.Before(waitDeadline) {
		waitDeadline = wl
	}
	payerAvailable, payerReserved := int64(100), int64(0)
	switch state {
	case subDirectReserved:
		r.State = int(RequestReserved)
		r.EstimatedFee = directFee
		r.ReservedAt = timeJSON(created)
		r.ReserveDuration = durationJSON(2 * time.Hour)
		r.ReserveDeadline = timeJSON(created.Add(2 * time.Hour))
		payerAvailable, payerReserved = 95, 5
		pol.ReservedTotal = 5
	case subApprovedReserved:
		r.State = int(RequestReserved)
		r.EstimatedFee = approvalFee
		r.WaitDeadline = timeJSON(waitDeadline)
		r.DecidedAt = timeJSON(created.Add(30 * time.Minute))
		r.ApproverAccountID = "payer"
		r.ReservedAt = timeJSON(created.Add(30 * time.Minute))
		r.ReserveDuration = durationJSON(2 * time.Hour)
		r.ReserveDeadline = timeJSON(created.Add(2*time.Hour + 30*time.Minute))
		payerAvailable, payerReserved = 80, 20
		pol.ReservedTotal = 20
	case subDirectSettled:
		r.State = int(RequestSettled)
		r.EstimatedFee = directFee
		r.ActualFee = actualFee
		r.ReservedAt = timeJSON(created)
		r.ReserveDuration = durationJSON(2 * time.Hour)
		r.ReserveDeadline = timeJSON(created.Add(2 * time.Hour))
		r.SettledAt = timeJSON(created.Add(30 * time.Minute))
		payerAvailable = 97
		pol.SpentTotal = actualFee
	case subApprovedSettled:
		r.State = int(RequestSettled)
		r.EstimatedFee = approvalFee
		r.ActualFee = actualFee
		r.WaitDeadline = timeJSON(waitDeadline)
		r.DecidedAt = timeJSON(created.Add(30 * time.Minute))
		r.ApproverAccountID = "payer"
		r.ReservedAt = timeJSON(created.Add(30 * time.Minute))
		r.ReserveDuration = durationJSON(2 * time.Hour)
		r.ReserveDeadline = timeJSON(created.Add(2*time.Hour + 30*time.Minute))
		r.SettledAt = timeJSON(created.Add(time.Hour))
		payerAvailable = 97
		pol.SpentTotal = actualFee
	case subCancelledFromReserved:
		r.State = int(RequestCancelled)
		r.EstimatedFee = directFee
		r.ReservedAt = timeJSON(created)
		r.ReserveDuration = durationJSON(2 * time.Hour)
		r.ReserveDeadline = timeJSON(created.Add(2 * time.Hour))
	case subCancelledFromPending:
		r.State = int(RequestCancelled)
		r.EstimatedFee = approvalFee
		r.WaitDeadline = timeJSON(waitDeadline)
		r.DecidedAt = timeJSON(created.Add(30 * time.Minute))
	case subPending:
		r.State = int(RequestPendingApproval)
		r.EstimatedFee = approvalFee
		r.WaitDeadline = timeJSON(waitDeadline)
	case subRejected:
		r.State = int(RequestRejected)
		r.EstimatedFee = approvalFee
		r.WaitDeadline = timeJSON(waitDeadline)
		r.DecidedAt = timeJSON(created.Add(30 * time.Minute))
		r.ApproverAccountID = "payer"
		r.RejectReason = "not now"
	case subExpired:
		r.State = int(RequestExpired)
		r.EstimatedFee = approvalFee
		r.WaitDeadline = timeJSON(waitDeadline)
		r.DecidedAt = timeJSON(waitDeadline)
	case subReservationExpired:
		r.State = int(RequestReservationExpired)
		r.EstimatedFee = directFee
		r.ReservedAt = timeJSON(created)
		r.ReserveDuration = durationJSON(2 * time.Hour)
		r.ReserveDeadline = timeJSON(created.Add(2 * time.Hour))
		r.ReserveExpiredAt = timeJSON(created.Add(2 * time.Hour))
	default:
		t.Fatalf("unknown submission state variant %d", state)
	}
	if mutate != nil {
		mutate(&r, &pol, &sess)
	}
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(created),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: payerAvailable, Reserved: payerReserved, CreatedAt: timeJSON(t0.Add(-time.Hour))},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0.Add(-time.Hour))},
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

// assertSubmissionRejected 恢复必须失败、包装 ErrBackupInvalid，错误信息点名
// 使用账户与请求编号并说明违反的是策略时间窗还是会话期限，且不得返回部分钱包。
func assertSubmissionRejected(t *testing.T, data []byte, at time.Time, want string) {
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
	if !strings.Contains(msg, `"u1"`) || !strings.Contains(msg, `"rq"`) {
		t.Fatalf("error %q must name usage account and request id", msg)
	}
	if !strings.Contains(msg, want) {
		t.Fatalf("error %q must explain the violation is about %q", msg, want)
	}
}

// TestRestoreSubmissionWindowBoundaries 验证提交时刻相对策略时间窗的边界：
// 恰在策略开始时提交可接受，窗口内（含结束前最后一纳秒）可接受；早于开始、
// 恰在结束或晚于结束都必须拒绝。
func TestRestoreSubmissionWindowBoundaries(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	startsAt, endsAt := t0, t0.Add(4*time.Hour)

	accept := []struct {
		name    string
		created time.Time
	}{
		{"exactly at policy start", startsAt},
		{"one nanosecond after start", startsAt.Add(time.Nanosecond)},
		{"one nanosecond before end", endsAt.Add(-time.Nanosecond)},
	}
	for _, tc := range accept {
		t.Run("accept "+tc.name, func(t *testing.T) {
			data := buildSubmissionTimingBackup(t, t0, subDirectReserved, tc.created, nil)
			if _, err := restoreAt(data, tc.created); err != nil {
				t.Fatalf("submission %s must be legal: %v", tc.name, err)
			}
		})
	}

	reject := []struct {
		name    string
		created time.Time
	}{
		{"one nanosecond before start", startsAt.Add(-time.Nanosecond)},
		{"long before start", startsAt.Add(-24 * time.Hour)},
		{"exactly at policy end", endsAt},
		{"one nanosecond after end", endsAt.Add(time.Nanosecond)},
		{"long after end", endsAt.Add(24 * time.Hour)},
	}
	for _, tc := range reject {
		t.Run("reject "+tc.name, func(t *testing.T) {
			data := buildSubmissionTimingBackup(t, t0, subDirectReserved, tc.created, nil)
			assertSubmissionRejected(t, data, t0.Add(48*time.Hour), "policy")
		})
	}
}

// TestRestoreSubmissionSessionExpiryBoundary 验证提交时刻相对申请会话到期的
// 边界：到期前一纳秒提交可接受；恰在到期时刻或之后提交必须拒绝。
func TestRestoreSubmissionSessionExpiryBoundary(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sessionExpires := t0.Add(2 * time.Hour)
	shortSession := func(_ *requestBackupV1, _ *policyBackupV1, s *sessionBackupV1) {
		s.ExpiresAt = timeJSON(sessionExpires)
	}

	data := buildSubmissionTimingBackup(t, t0, subDirectReserved, sessionExpires.Add(-time.Nanosecond), shortSession)
	if _, err := restoreAt(data, t0); err != nil {
		t.Fatalf("submission 1ns before session expiry must be legal: %v", err)
	}

	for _, created := range []time.Time{sessionExpires, sessionExpires.Add(time.Nanosecond), sessionExpires.Add(time.Hour)} {
		data := buildSubmissionTimingBackup(t, t0, subDirectReserved, created, shortSession)
		assertSubmissionRejected(t, data, t0.Add(48*time.Hour), "session")
	}
}

// TestRestoreSubmissionTimingAppliesToAllStates 验证提交时刻规则对直接预留与
// 审批两条路径、以及全部后续状态（已预留、已结算、已取消、待审批、已拒绝、
// 已过期、预留超时）统一适用：提交早于策略开始的备份一律整体拒绝，即使余额、
// 策略累计金额与请求求和完全一致。
func TestRestoreSubmissionTimingAppliesToAllStates(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	states := map[string]submissionState{
		"direct reserved":         subDirectReserved,
		"approved reserved":       subApprovedReserved,
		"direct settled":          subDirectSettled,
		"approved settled":        subApprovedSettled,
		"cancelled from reserved": subCancelledFromReserved,
		"cancelled from pending":  subCancelledFromPending,
		"pending approval":        subPending,
		"rejected":                subRejected,
		"expired":                 subExpired,
		"reservation expired":     subReservationExpired,
	}
	for name, st := range states {
		t.Run(name, func(t *testing.T) {
			// 提交于策略开始前一纳秒：其余字段均由提交时刻推导、完全自洽。
			data := buildSubmissionTimingBackup(t, t0, st, t0.Add(-time.Nanosecond), nil)
			assertSubmissionRejected(t, data, t0.Add(48*time.Hour), "policy")
		})
	}
}

// TestRestoreSubmissionTimingApprovedPathSessionViolation 验证审批路径的请求
// 同样核对会话期限：提交恰在会话到期时刻的已批准预留必须拒绝（提交时刻校验
// 先于其他计时校验触发，错误说明违反的是会话期限）。
func TestRestoreSubmissionTimingApprovedPathSessionViolation(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	sessionExpires := t0.Add(time.Hour)
	shortSession := func(_ *requestBackupV1, _ *policyBackupV1, s *sessionBackupV1) {
		s.ExpiresAt = timeJSON(sessionExpires)
	}
	data := buildSubmissionTimingBackup(t, t0, subApprovedReserved, sessionExpires, shortSession)
	assertSubmissionRejected(t, data, t0.Add(48*time.Hour), "session")
}

// TestRestoreSubmissionTimingNotSkippedForTimedOutReservation 验证即使某笔预留
// 在恢复时早已超过预留截止、按规则本应超时退回，违反提交时刻条件的备份仍必须
// 整体拒绝，不能先退款或改成终态再接受。
func TestRestoreSubmissionTimingNotSkippedForTimedOutReservation(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// 提交早于策略开始，预留截止 t0+2h 也远早于恢复时刻：不能按超时退款洗白。
	data := buildSubmissionTimingBackup(t, t0, subDirectReserved, t0.Add(-time.Nanosecond), nil)
	assertSubmissionRejected(t, data, t0.Add(30*24*time.Hour), "policy")
}

// TestRestoreSubmissionTimingTimezoneAndNanosecond 验证提交时刻按完整绝对瞬间
// 比较：纳秒精度保留，不同时区表示的同一时刻判定相同。
func TestRestoreSubmissionTimingTimezoneAndNanosecond(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	endsAt := t0.Add(4 * time.Hour)
	loc := time.FixedZone("east", 5*60*60)

	// 策略开始时刻换 +05:00 时区表示：仍是同一瞬间，恰在开始时刻必须接受。
	data := buildSubmissionTimingBackup(t, t0, subDirectReserved, t0.In(loc), nil)
	if _, err := restoreAt(data, t0); err != nil {
		t.Fatalf("submission at policy start expressed in another timezone must restore: %v", err)
	}

	// 结束前一纳秒换时区表示：必须接受。
	data = buildSubmissionTimingBackup(t, t0, subDirectReserved, endsAt.Add(-time.Nanosecond).In(loc), nil)
	if _, err := restoreAt(data, t0); err != nil {
		t.Fatalf("submission 1ns before end in another timezone must restore: %v", err)
	}

	// 策略结束时刻换时区表示：仍是同一瞬间，必须拒绝。
	data = buildSubmissionTimingBackup(t, t0, subDirectReserved, endsAt.In(loc), nil)
	assertSubmissionRejected(t, data, t0.Add(48*time.Hour), "policy")
}

// TestRestoreSubmissionValidThenLifecycleEnded 验证申请当时符合条件、之后会话
// 到期或被吊销、策略结束或被停用的请求仍按现有规则恢复：已有终态不改写，未
// 结束请求沿用原期限，正常的到期处理继续执行。
func TestRestoreSubmissionValidThenLifecycleEnded(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// 会话在提交后 30 分钟到期、策略在提交后被停用：提交时刻本身合法。
	lifecycle := func(_ *requestBackupV1, p *policyBackupV1, s *sessionBackupV1) {
		s.ExpiresAt = timeJSON(t0.Add(time.Hour + 30*time.Minute))
		p.Deactivated = true
		p.DeactivatedAt = timeJSON(t0.Add(2 * time.Hour))
		p.DeactivatorAccountID = "payer"
		p.DeactivateReason = "stopped"
	}
	created := t0.Add(time.Hour)

	// 已结算：终态与金额原样保留。
	data := buildSubmissionTimingBackup(t, t0, subDirectSettled, created, lifecycle)
	w2, err := restoreAt(data, t0.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("settled request submitted in-window must restore: %v", err)
	}
	if r, _ := w2.Request("u1", "rq"); r.State != RequestSettled || r.ActualFee != 3 {
		t.Fatalf("settled history changed: %+v", r)
	}

	// 已预留：恢复时早过预留截止，按正常到期处理全额退回并进入预留超时终态。
	data = buildSubmissionTimingBackup(t, t0, subDirectReserved, created, lifecycle)
	w2, err = restoreAt(data, t0.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("reserved request submitted in-window must restore: %v", err)
	}
	r, _ := w2.Request("u1", "rq")
	if r.State != RequestReservationExpired || !r.ReserveExpiredAt.Equal(created.Add(2*time.Hour)) {
		t.Fatalf("reservation must expire at its original deadline: %+v", r)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance after timeout refund = %+v, want {100 0}", bal)
	}
	if n := countKind(w2.Ledger(), LedgerReservationExpiration); n != 1 {
		t.Fatalf("expected one reservation-expiration entry, got %d", n)
	}
}

// TestRestoreSubmissionTimingIgnoresStandaloneRejections 验证未被受理的申请留
// 下的独立拒绝账本记录（不关联任何已受理请求，甚至没有可关联的账户或请求
// 编号）仍原样保留：即使记录时刻落在策略时间窗之前，也不按已受理请求的提交
// 时间规则拒绝恢复。
func TestRestoreSubmissionTimingIgnoresStandaloneRejections(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0),
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
		}},
		Requests: []requestBackupV1{},
		Ledger: []ledgerEntryBackupV1{
			// 策略开始前的拒绝留痕：申请未被受理，没有对应请求。
			{Kind: int(LedgerRejection), AccountID: "u1", RequestID: "too-early",
				Reason: "outside policy window", At: timeJSON(t0.Add(-time.Hour))},
			// 账户与请求编号均无法关联的拒绝留痕。
			{Kind: int(LedgerRejection), Reason: "missing required fields", At: timeJSON(t0.Add(-30 * time.Minute))},
		},
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, t0.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("standalone rejection ledger entries must not fail restore: %v", err)
	}
	led := w2.Ledger()
	if len(led) != 2 || led[0].Kind != LedgerRejection || led[1].Kind != LedgerRejection {
		t.Fatalf("rejection ledger entries not preserved: %+v", led)
	}
	if led[0].RequestID != "too-early" || led[1].AccountID != "" {
		t.Fatalf("rejection ledger entries rewritten: %+v", led)
	}
}

// TestRestoreLegalExportSubmissionTimingRoundTrips 端到端验证当前版本合法导出
// 文本继续兼容：真实钱包中在时间窗内提交、之后策略结束且会话到期的备份照常
// 恢复。
func TestRestoreLegalExportSubmissionTimingRoundTrips(t *testing.T) {
	w, c := setupTimeout(t, time.Hour)
	if _, err := w.Apply(timeoutApply()); err != nil { // r1 预留于 t0
		t.Fatal(err)
	}
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	// 很久以后恢复：策略窗口与会话早已结束，预留按原截止时刻超时退回。
	w2, err := restoreAt(data, c.t.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("legal export must restore long after policy and session end: %v", err)
	}
	if r, _ := w2.Request("u1", "r1"); r.State != RequestReservationExpired {
		t.Fatalf("state = %v, want reservation expired via normal timeout", r.State)
	}
}
