package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// deactState 命名一组请求状态变体，覆盖直接预留与审批两条路径以及全部
// 后续状态（含终态），用于验证“停用后提交/停用后批准”核对不豁免任何状态。
type deactState int

const (
	deactDirectReserved deactState = iota
	deactApprovedReserved
	deactDirectSettled
	deactApprovedSettled
	deactCancelledFromReserved
	deactCancelledFromPending
	deactRejected
	deactExpired
	deactReservationExpired
)

// deactPolicyAt 为构造器使用的策略首次停用时刻（t0+30min）。
func deactPolicyAt(t0 time.Time) time.Time { return t0.Add(30 * time.Minute) }

// buildDeactivationTimingBackup 构造一份策略 p 已于 t0+30min 首次停用的备份：
// 策略时间窗 [t0-1h, t0+4h)、审批门槛 10（等待 1h）、最长预留时长 2h，申请
// 会话 s1 到期 t0+4h；请求提交于 created、审批/决定发生于 decided，其余计时
// 字段（等待截止、预留、结算、超时）都由二者推导，账户余额与策略累计金额随
// 状态配套，因此除“提交/批准与停用时刻的关系”外全部自洽合法。
func buildDeactivationTimingBackup(t *testing.T, t0 time.Time, state deactState, created, decided time.Time) []byte {
	t.Helper()
	const (
		directFee   int64 = 5  // 未超门槛：直接预留路径
		approvalFee int64 = 20 // 严格超门槛：审批路径
		actualFee   int64 = 3
	)
	deact := deactPolicyAt(t0)
	pol := policyBackupV1{
		ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "charge", Payee: "shop",
		StartsAt:             timeJSON(t0.Add(-time.Hour)),
		EndsAt:               timeJSON(t0.Add(4 * time.Hour)),
		MaxPerRequest:        100,
		MaxTotal:             1000,
		ApprovalThreshold:    10,
		ApprovalWait:         durationJSON(time.Hour),
		MaxReserveDuration:   durationJSON(2 * time.Hour),
		Deactivated:          true,
		DeactivatedAt:        timeJSON(deact),
		DeactivatorAccountID: "payer",
		DeactivateReason:     "stop it",
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
	directReserved := func() {
		r.EstimatedFee = directFee
		r.ReservedAt = timeJSON(created)
		r.ReserveDuration = durationJSON(2 * time.Hour)
		r.ReserveDeadline = timeJSON(created.Add(2 * time.Hour))
	}
	approvedReserved := func() {
		r.EstimatedFee = approvalFee
		r.WaitDeadline = timeJSON(waitDeadline)
		r.DecidedAt = timeJSON(decided)
		r.ApproverAccountID = "payer"
		r.ReservedAt = timeJSON(decided)
		r.ReserveDuration = durationJSON(2 * time.Hour)
		r.ReserveDeadline = timeJSON(decided.Add(2 * time.Hour))
	}
	switch state {
	case deactDirectReserved:
		r.State = int(RequestReserved)
		directReserved()
		payerAvailable, payerReserved = 95, 5
		pol.ReservedTotal = 5
	case deactApprovedReserved:
		r.State = int(RequestReserved)
		approvedReserved()
		payerAvailable, payerReserved = 80, 20
		pol.ReservedTotal = 20
	case deactDirectSettled:
		r.State = int(RequestSettled)
		directReserved()
		r.ActualFee = actualFee
		r.SettledAt = timeJSON(decided)
		payerAvailable = 97
		pol.SpentTotal = 3
	case deactApprovedSettled:
		r.State = int(RequestSettled)
		approvedReserved()
		r.ActualFee = actualFee
		r.SettledAt = timeJSON(decided)
		payerAvailable = 97
		pol.SpentTotal = 3
	case deactCancelledFromReserved:
		r.State = int(RequestCancelled)
		directReserved()
	case deactCancelledFromPending:
		r.State = int(RequestCancelled)
		r.EstimatedFee = approvalFee
		r.WaitDeadline = timeJSON(waitDeadline)
		r.DecidedAt = timeJSON(decided)
	case deactRejected:
		r.State = int(RequestRejected)
		r.EstimatedFee = approvalFee
		r.WaitDeadline = timeJSON(waitDeadline)
		r.DecidedAt = timeJSON(decided)
		r.ApproverAccountID = "payer"
		r.RejectReason = "no"
	case deactExpired:
		r.State = int(RequestExpired)
		r.EstimatedFee = approvalFee
		r.WaitDeadline = timeJSON(waitDeadline)
		r.DecidedAt = timeJSON(decided)
	case deactReservationExpired:
		r.State = int(RequestReservationExpired)
		directReserved()
		r.ReserveExpiredAt = r.ReserveDeadline
	}
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: payerAvailable, Reserved: payerReserved, CreatedAt: timeJSON(t0)},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)},
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

// requireDeactivationConflict 断言恢复整体失败：错误可识别为 ErrBackupInvalid，
// 不返回钱包，且指出使用账户、请求编号、策略编号与矛盾类型（停用后提交或
// 停用后批准）。
func requireDeactivationConflict(t *testing.T, data []byte, at time.Time, kind string) {
	t.Helper()
	w2, err := restoreAt(data, at)
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
	for _, want := range []string{"u1", "rq", `"p"`, "deactivat", kind} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q must identify %q (usage account, request, policy, conflict kind)", msg, want)
		}
	}
}

// TestRestoreRejectsSubmissionAfterDeactivation 验证核心修复：已停用策略下
// 任何已受理请求的提交时刻严格晚于首次停用时刻时，整份备份无效。该核对
// 不豁免任何后续状态（已预留、已结算、已取消、被拒绝、待审批过期、预留
// 超时），也与恢复时刻无关——即使恢复时相关预留早已到期，也不能先按当前
// 时间退回再接受备份。
func TestRestoreRejectsSubmissionAfterDeactivation(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	deact := deactPolicyAt(t0)
	created := deact.Add(time.Nanosecond) // 停用后一纳秒提交
	decided := created.Add(time.Minute)   // 各终态的决定/结算时刻
	restoreAts := []struct {
		name string
		at   time.Time
	}{
		{"restore before deactivation time", t0},
		{"restore shortly after deactivation", deact.Add(time.Hour)},
		{"restore long after all deadlines", t0.Add(10 * time.Hour)},
	}
	for _, st := range []struct {
		name  string
		state deactState
	}{
		{"direct reserved", deactDirectReserved},
		{"approved reserved", deactApprovedReserved},
		{"direct settled", deactDirectSettled},
		{"approved settled", deactApprovedSettled},
		{"cancelled from reserved", deactCancelledFromReserved},
		{"cancelled from pending", deactCancelledFromPending},
		{"rejected", deactRejected},
		{"expired", deactExpired},
		{"reservation expired", deactReservationExpired},
	} {
		for _, ra := range restoreAts {
			t.Run(st.name+"/"+ra.name, func(t *testing.T) {
				dec := decided
				if st.state == deactExpired {
					// 过期决定不得早于等待截止时刻（提交+1h）。
					dec = created.Add(2 * time.Hour)
				}
				data := buildDeactivationTimingBackup(t, t0, st.state, created, dec)
				requireDeactivationConflict(t, data, ra.at, "submitted")
			})
		}
	}
}

// TestRestoreRejectsApprovalAfterDeactivation 验证超门槛请求的批准时刻严格
// 晚于策略首次停用时刻时整份备份无效：即使申请在停用前提交、批准仍在等待
// 期限内、余额与额度完全对得上，也不能接受停用后批准的历史；该核对只看
// 备份保存的批准时间，与恢复时的当前时间无关。
func TestRestoreRejectsApprovalAfterDeactivation(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	deact := deactPolicyAt(t0)
	created := deact.Add(-10 * time.Minute) // 停用前合法提交
	decided := deact.Add(time.Nanosecond)   // 停用后一纳秒批准（仍在等待期限内）
	for _, st := range []struct {
		name  string
		state deactState
	}{
		{"approved reserved", deactApprovedReserved},
		{"approved settled", deactApprovedSettled},
	} {
		for _, at := range []time.Time{t0, deact.Add(time.Hour), t0.Add(10 * time.Hour)} {
			t.Run(st.name, func(t *testing.T) {
				data := buildDeactivationTimingBackup(t, t0, st.state, created, decided)
				requireDeactivationConflict(t, data, at, "approved")
			})
		}
	}
}

// TestRestoreAllowsSubmissionAndApprovalAtExactDeactivation 验证边界：提交或
// 批准与首次停用时刻完全相同时不能仅凭相等拒绝——先完成申请或批准、再停用，
// 可能共享同一个时间戳。纳秒精度保留：晚一纳秒必须拒绝，早一纳秒必须接受；
// 不同时区表示的同一时刻判定相同。
func TestRestoreAllowsSubmissionAndApprovalAtExactDeactivation(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	deact := deactPolicyAt(t0)
	restoreNow := t0.Add(time.Hour)

	t.Run("direct reserved submitted exactly at deactivation", func(t *testing.T) {
		data := buildDeactivationTimingBackup(t, t0, deactDirectReserved, deact, time.Time{})
		if _, err := restoreAt(data, restoreNow); err != nil {
			t.Fatalf("submission exactly at deactivation must restore: %v", err)
		}
	})
	t.Run("direct reserved submitted one nanosecond before", func(t *testing.T) {
		data := buildDeactivationTimingBackup(t, t0, deactDirectReserved, deact.Add(-time.Nanosecond), time.Time{})
		if _, err := restoreAt(data, restoreNow); err != nil {
			t.Fatalf("submission one nanosecond before deactivation must restore: %v", err)
		}
	})
	t.Run("approved exactly at deactivation", func(t *testing.T) {
		data := buildDeactivationTimingBackup(t, t0, deactApprovedReserved, deact.Add(-10*time.Minute), deact)
		if _, err := restoreAt(data, restoreNow); err != nil {
			t.Fatalf("approval exactly at deactivation must restore: %v", err)
		}
	})
	t.Run("same instant in another timezone", func(t *testing.T) {
		// deact 的同一绝对时刻改用 +08:00 表示，结论必须一致（接受）。
		created := deact.In(time.FixedZone("UTC+8", 8*3600))
		data := buildDeactivationTimingBackup(t, t0, deactDirectReserved, created, time.Time{})
		if _, err := restoreAt(data, restoreNow); err != nil {
			t.Fatalf("same instant in another timezone must restore: %v", err)
		}
	})
	t.Run("one nanosecond after in another timezone", func(t *testing.T) {
		// deact+1ns 的同一绝对时刻改用 -05:00 表示，结论必须一致（拒绝）。
		created := deact.Add(time.Nanosecond).In(time.FixedZone("UTC-5", -5*3600))
		data := buildDeactivationTimingBackup(t, t0, deactDirectReserved, created, time.Time{})
		requireDeactivationConflict(t, data, restoreNow, "submitted")
	})
	t.Run("approval one nanosecond after in another timezone", func(t *testing.T) {
		decided := deact.Add(time.Nanosecond).In(time.FixedZone("UTC+8", 8*3600))
		data := buildDeactivationTimingBackup(t, t0, deactApprovedReserved, deact.Add(-10*time.Minute), decided)
		requireDeactivationConflict(t, data, restoreNow, "approved")
	})
}

// TestRestoreKeepsLegalHistoryAcrossDeactivation 验证停用前合法提交并完成
// 预留的请求继续沿用已有行为：停用后才结算或取消的历史正常恢复，恢复保留
// 请求状态、金额、审批信息、预留期限与已有账本，不因这项核对补写停用、
// 拒绝或退款记录。
func TestRestoreKeepsLegalHistoryAcrossDeactivation(t *testing.T) {
	setup := func(t *testing.T) (*Wallet, *clock) {
		t.Helper()
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

	t.Run("settled after deactivation restores as settled", func(t *testing.T) {
		w, c := setup(t)
		t0 := c.t
		if _, err := w.Apply(RequestInput{
			PolicyID: "p", RequestID: "r1", AccountID: "u1", SessionID: "s1",
			DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 20,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Approve("u1", "r1", "sa", "deva"); err != nil {
			t.Fatal(err)
		}
		c.t = t0.Add(time.Minute)
		if _, err := w.DeactivatePolicy("p", "sa", "deva", "stop it"); err != nil {
			t.Fatal(err)
		}
		// 停用后结算已预留请求：合法历史，恢复不得把它当成新的提交或批准。
		c.t = t0.Add(2 * time.Minute)
		if _, err := w.Settle("u1", "r1", 12); err != nil {
			t.Fatal(err)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		w2, err := restoreAt(data, t0.Add(time.Hour))
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
		r, err := w2.Request("u1", "r1")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestSettled || r.ActualFee != 12 || r.ApproverAccountID != "payer" {
			t.Fatalf("request = %+v, want settled 12 with approval info kept", r)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 988, Reserved: 0}) {
			t.Fatalf("balance = %+v, want 988/0", bal)
		}
		if got := len(w2.Ledger()); got != len(w.Ledger()) {
			t.Fatalf("ledger entries changed: got %d want %d", got, len(w.Ledger()))
		}
	})

	t.Run("cancelled after deactivation restores as cancelled", func(t *testing.T) {
		w, c := setup(t)
		t0 := c.t
		if _, err := w.Apply(RequestInput{
			PolicyID: "p", RequestID: "r1", AccountID: "u1", SessionID: "s1",
			DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 5,
		}); err != nil {
			t.Fatal(err)
		}
		c.t = t0.Add(time.Minute)
		if _, err := w.DeactivatePolicy("p", "sa", "deva", "stop it"); err != nil {
			t.Fatal(err)
		}
		// 停用后取消已预留请求：合法历史，全额退回，恢复保持已取消。
		c.t = t0.Add(2 * time.Minute)
		if _, err := w.Cancel("u1", "r1"); err != nil {
			t.Fatal(err)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		w2, err := restoreAt(data, t0.Add(time.Hour))
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
		r, err := w2.Request("u1", "r1")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestCancelled {
			t.Fatalf("state = %v, want cancelled", r.State)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 1000, Reserved: 0}) {
			t.Fatalf("balance = %+v, want 1000/0", bal)
		}
		if got := len(w2.Ledger()); got != len(w.Ledger()) {
			t.Fatalf("ledger entries changed: got %d want %d", got, len(w.Ledger()))
		}
	})

	t.Run("requests under never-deactivated policies unaffected", func(t *testing.T) {
		w, c := setup(t)
		t0 := c.t
		if err := w.SavePolicy(PolicySpec{
			ID: "p2", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
			Operation: "charge", Payee: "shop",
			StartsAt: t0.Add(-time.Hour), EndsAt: t0.Add(time.Hour),
			MaxPerRequest: 100, MaxTotal: 1000,
			ApprovalThreshold: 10, ApprovalWait: 5 * time.Minute,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.DeactivatePolicy("p", "sa", "deva", "stop it"); err != nil {
			t.Fatal(err)
		}
		// 未停用策略下的申请发生在 p 停用之后：不受 p 的停用时刻约束。
		c.t = t0.Add(2 * time.Minute)
		if _, err := w.Apply(RequestInput{
			PolicyID: "p2", RequestID: "r2", AccountID: "u1", SessionID: "s1",
			DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 5,
		}); err != nil {
			t.Fatal(err)
		}
		data, err := w.Export()
		if err != nil {
			t.Fatal(err)
		}
		w2, err := restoreAt(data, t0.Add(3*time.Minute))
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
		r, err := w2.Request("u1", "r2")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestReserved {
			t.Fatalf("state = %v, want reserved", r.State)
		}
	})
}
