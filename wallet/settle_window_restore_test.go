package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// settleWindowOpts 控制一笔“已结算”请求备份的构造：
//   - approved：false 为直接受理路径，true 为超门槛经批准后预留路径；
//   - actualFee：结算实际费用（允许为零）；
//   - settledAt：备份记载的结算时刻；
//   - mutate：在序列化前进一步调整请求/策略/会话字段。
//
// 除结算时刻由参数指定外，其余计时与金额完全自洽，因此恢复能否成功只取决于
// 结算时刻是否落在有效预留期内。两路径均：费用 20、最长预留时长 2h；直接
// 受理于 t0，批准路径提交于 t0、于 t0+30m 批准并预留（等待 1h）。
type settleWindowOpts struct {
	approved  bool
	actualFee int64
	settledAt time.Time
	mutate    func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)
}

func buildSettleWindowBackup(t *testing.T, t0 time.Time, opts settleWindowOpts) []byte {
	t.Helper()
	const fee int64 = 20
	r := requestBackupV1{
		PolicyID: "p", RequestID: "late", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee: fee,
		ActualFee:    opts.actualFee,
		State:        int(RequestSettled),
		CreatedAt:    timeJSON(t0),
		SettledAt:    timeJSON(opts.settledAt),
	}
	pol := policyBackupV1{
		ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "charge", Payee: "shop",
		StartsAt:           timeJSON(t0.Add(-time.Hour)),
		EndsAt:             timeJSON(t0.Add(24 * time.Hour)),
		MaxPerRequest:      100,
		MaxTotal:           1000,
		MaxReserveDuration: durationJSON(2 * time.Hour),
		SpentTotal:         opts.actualFee,
	}
	sess := sessionBackupV1{
		ID: "s1", AccountID: "u1", DeviceID: "d1",
		ExpiresAt: timeJSON(t0.Add(24 * time.Hour)), CreatedAt: timeJSON(t0.Add(-2 * time.Hour)),
	}
	if opts.approved {
		pol.ApprovalThreshold = 10
		pol.ApprovalWait = durationJSON(time.Hour)
		// 提交 t0、等待截止 t0+1h，于 t0+30m 批准并实际预留；预留自批准时刻
		// 起算，截止 t0+2h30m（等待审批的 30m 不计入）。
		r.WaitDeadline = timeJSON(t0.Add(time.Hour))
		r.ReservedAt = timeJSON(t0.Add(30 * time.Minute))
		r.ReserveDuration = durationJSON(2 * time.Hour)
		r.ReserveDeadline = timeJSON(t0.Add(2*time.Hour + 30*time.Minute))
		r.ApproverAccountID = "payer"
		r.DecidedAt = timeJSON(t0.Add(30 * time.Minute))
	} else {
		r.ReservedAt = timeJSON(t0)
		r.ReserveDuration = durationJSON(2 * time.Hour)
		r.ReserveDeadline = timeJSON(t0.Add(2 * time.Hour))
	}
	if opts.mutate != nil {
		opts.mutate(&r, &pol, &sess)
	}

	// 结算已完成：预留余额为零，可用 = 初始 100 - 实际费用，策略已花费 = 实际费用。
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0.Add(-time.Minute)),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: 100 - opts.actualFee, Reserved: 0, CreatedAt: timeJSON(t0.Add(-2 * time.Hour))},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0.Add(-2 * time.Hour))},
		},
		Sessions: []sessionBackupV1{sess},
		Policies: []policyBackupV1{pol},
		Requests: []requestBackupV1{r},
		// 既有账本：一条结算扣减记录；恢复后必须原样保留、不追加退款或超时记录。
		Ledger: []ledgerEntryBackupV1{{
			Kind: int(LedgerSettle), AccountID: "payer", RequestID: "late",
			Amount: opts.actualFee, Reason: "settle actual fee", At: timeJSON(opts.settledAt),
		}},
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// assertSettleWindowRejected 恢复必须失败、包装 ErrBackupInvalid，错误信息点名
// 使用账户与请求编号并说明结算时刻不在有效预留期内，且不得返回部分钱包。
func assertSettleWindowRejected(t *testing.T, data []byte, at time.Time) string {
	t.Helper()
	w2, err := restoreAt(data, at)
	if err == nil {
		if w2 != nil {
			t.Fatalf("invalid settled history restored a wallet: %+v", w2)
		}
		t.Fatal("invalid settled history restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, `"u1"`) || !strings.Contains(msg, `"late"`) {
		t.Fatalf("error %q must name usage account and request id", msg)
	}
	if !strings.Contains(msg, "reserve") {
		t.Fatalf("error %q must explain the settlement is outside the reserve window", msg)
	}
	return msg
}

// TestRestoreSettledWithinReserveWindowAccepted 验证合法结算时刻边界：恰在
// 预留完成时刻、截止前一纳秒都可接受；即使很久以后恢复（预留期早结束、申请
// 会话已到期/被吊销、策略已结束或被停用），已结算历史原样保留。
func TestRestoreSettledWithinReserveWindowAccepted(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	for _, approved := range []bool{false, true} {
		pathName := map[bool]string{false: "direct", true: "approved"}[approved]
		reservedAt := t0
		deadline := t0.Add(2 * time.Hour)
		if approved {
			reservedAt = t0.Add(30 * time.Minute)
			deadline = t0.Add(2*time.Hour + 30*time.Minute)
		}

		t.Run(pathName+" settle exactly at reserved_at restores", func(t *testing.T) {
			data := buildSettleWindowBackup(t, t0, settleWindowOpts{
				approved: approved, actualFee: 20, settledAt: reservedAt,
			})
			if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
				t.Fatalf("settle exactly when reservation completes must be legal: %v", err)
			}
		})

		t.Run(pathName+" settle one nanosecond before deadline restores", func(t *testing.T) {
			data := buildSettleWindowBackup(t, t0, settleWindowOpts{
				approved: approved, actualFee: 20, settledAt: deadline.Add(-time.Nanosecond),
			})
			if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
				t.Fatalf("settle 1ns before deadline must be legal: %v", err)
			}
		})

		t.Run(pathName+" zero-fee settle within window restores", func(t *testing.T) {
			data := buildSettleWindowBackup(t, t0, settleWindowOpts{
				approved: approved, actualFee: 0, settledAt: deadline.Add(-time.Nanosecond),
			})
			w2, err := restoreAt(data, t0.Add(48*time.Hour))
			if err != nil {
				t.Fatalf("zero-fee in-window settle: %v", err)
			}
			r, _ := w2.Request("u1", "late")
			if r.State != RequestSettled || r.ActualFee != 0 {
				t.Fatalf("zero-fee settled request = %+v", r)
			}
			// 实际费用为零：预留已全额退回，策略已花费总额为零。
			if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
				t.Fatalf("balance = %+v, want full refund {100 0}", bal)
			}
			if pv, _ := w2.Policy("p"); pv.SpentTotal != 0 {
				t.Fatalf("spent total = %d, want 0", pv.SpentTotal)
			}
		})
	}
}

// TestRestoreSettledHistoryPreservedLongAfterRestore 验证很久以后恢复时，期限内
// 已完成的结算仍保持已结算：实际费用、结算时间、余额、策略已花费总额与既有
// 账本原样保留，不追加退款或超时记录。
func TestRestoreSettledHistoryPreservedLongAfterRestore(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	settledAt := t0.Add(90 * time.Minute) // 直接受理路径截止 t0+2h 之内
	data := buildSettleWindowBackup(t, t0, settleWindowOpts{actualFee: 12, settledAt: settledAt})

	w2, err := restoreAt(data, t0.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("legal settled history must restore long after deadline: %v", err)
	}
	r, _ := w2.Request("u1", "late")
	if r.State != RequestSettled {
		t.Fatalf("state = %v, want settled", r.State)
	}
	if r.ActualFee != 12 || !r.SettledAt.Equal(settledAt) {
		t.Fatalf("settled history changed: fee %d at %v", r.ActualFee, r.SettledAt)
	}
	if !r.ReserveExpiredAt.IsZero() {
		t.Fatalf("settled request must not gain a reserve-expired time: %v", r.ReserveExpiredAt)
	}
	// 初始 100：实际扣 12，退回差额 8。
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 88, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {88 0}", bal)
	}
	if pv, _ := w2.Policy("p"); pv.ReservedTotal != 0 || pv.SpentTotal != 12 {
		t.Fatalf("policy totals = reserved %d spent %d, want 0/12", pv.ReservedTotal, pv.SpentTotal)
	}
	// 既有账本只有原结算记录，未因很久以后恢复而追加退款或超时记录。
	led := w2.Ledger()
	if len(led) != 1 || led[0].Kind != LedgerSettle || led[0].Amount != 12 {
		t.Fatalf("ledger = %+v, want only the original settle entry", led)
	}
}

// TestRestoreRejectsSettledAtOrAfterReserveDeadline 验证核心规则：启用预留
// 超时时，结算时刻等于或晚于预留截止时刻（含零实际费用）的已结算记录必须
// 拒绝；早于实际预留时刻也拒绝。直接受理与批准后预留两条路径一致。
func TestRestoreRejectsSettledAtOrAfterReserveDeadline(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	for _, approved := range []bool{false, true} {
		pathName := map[bool]string{false: "direct", true: "approved"}[approved]
		reservedAt := t0
		deadline := t0.Add(2 * time.Hour)
		if approved {
			reservedAt = t0.Add(30 * time.Minute)
			deadline = t0.Add(2*time.Hour + 30*time.Minute)
		}

		cases := []struct {
			name      string
			actualFee int64
			settledAt time.Time
			reason    string
		}{
			{"settle exactly at deadline", 20, deadline, "reserve window"},
			{"settle one nanosecond after deadline", 20, deadline.Add(time.Nanosecond), "reserve window"},
			{"settle long after deadline", 20, deadline.Add(24 * time.Hour), "reserve window"},
			{"zero-fee settle exactly at deadline", 0, deadline, "reserve window"},
			{"zero-fee settle after deadline", 0, deadline.Add(time.Hour), "reserve window"},
			{"settle before reserved_at", 20, reservedAt.Add(-time.Nanosecond), "before reserved_at"},
		}
		for _, tc := range cases {
			t.Run(pathName+" "+tc.name, func(t *testing.T) {
				data := buildSettleWindowBackup(t, t0, settleWindowOpts{
					approved: approved, actualFee: tc.actualFee, settledAt: tc.settledAt,
				})
				// 即使恢复时刻远晚于截止、按超时规则本该全额退回，损坏的已结算
				// 历史仍必须拒绝，不能当作有效扣费带回钱包。
				msg := assertSettleWindowRejected(t, data, t0.Add(30*24*time.Hour))
				if !strings.Contains(msg, tc.reason) {
					t.Fatalf("error %q must explain %q", msg, tc.reason)
				}
			})
		}
	}
}

// TestRestoreSettleWindowUsesApprovalReservedAt 验证审批路径的预留期自批准
// 成功、实际冻结费用的时刻起算，等待审批的时间不计入：晚于等待截止、但仍
// 早于“批准时刻+预留时长”的结算合法；用提交时间或审批等待期限替代预留期限
// 都会错误拒绝/接受。
func TestRestoreSettleWindowUsesApprovalReservedAt(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// 提交 t0、等待截止 t0+1h、t0+30m 批准预留、预留截止 t0+2h30m。

	// t0+2h：已过等待截止（t0+1h）、申请会话早已可过期，但仍在预留窗口
	// [t0+30m, t0+2h30m) 内，必须接受——证明用的是实际预留时刻而非提交/等待期。
	data := buildSettleWindowBackup(t, t0, settleWindowOpts{
		approved: true, actualFee: 20, settledAt: t0.Add(2 * time.Hour),
	})
	if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
		t.Fatalf("settle after approval-wait deadline but within reserve window must be legal: %v", err)
	}

	// 恰在预留截止 t0+2h30m：拒绝（对比若误用提交+2h=t0+2h 会被当成超时，
	// 上一例已证明不会；本例锁定真正的截止时刻）。
	data = buildSettleWindowBackup(t, t0, settleWindowOpts{
		approved: true, actualFee: 20, settledAt: t0.Add(2*time.Hour + 30*time.Minute),
	})
	assertSettleWindowRejected(t, data, t0.Add(48*time.Hour))
}

// TestRestoreSettleWindowSubSecondAndTimezoneInsensitive 验证时间比较依据保存
// 的完整时刻：纳秒精度保留，且不同时区表示的同一时刻判断一致。
func TestRestoreSettleWindowSubSecondAndTimezoneInsensitive(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	deadline := t0.Add(2 * time.Hour)
	loc := time.FixedZone("east", 5*60*60)

	// 截止前一纳秒，换成 +05:00 时区表示：仍是同一瞬间，必须接受。
	before := deadline.Add(-time.Nanosecond).In(loc)
	if before.Equal(deadline) {
		t.Fatal("test setup: timezone conversion collapsed the instant")
	}
	data := buildSettleWindowBackup(t, t0, settleWindowOpts{actualFee: 20, settledAt: before})
	if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
		t.Fatalf("same instant in a different timezone must restore: %v", err)
	}

	// 截止时刻本身换时区表示：仍是同一瞬间，必须拒绝。
	atDeadline := deadline.In(loc)
	data = buildSettleWindowBackup(t, t0, settleWindowOpts{actualFee: 20, settledAt: atDeadline})
	assertSettleWindowRejected(t, data, t0.Add(48*time.Hour))
}

// TestRestoreSettledWithTimeoutDisabledHasNoSettleDeadline 验证关闭预留超时的
// 请求沿用原有结算规则：没有截止时刻，很久以后结算仍合法，不人为增加期限。
func TestRestoreSettledWithTimeoutDisabledHasNoSettleDeadline(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	disable := func(_ *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
		p.MaxReserveDuration = 0
	}
	// 结算时刻远在预留之后，且不携带任何截止时刻：照常恢复。
	data := buildSettleWindowBackup(t, t0, settleWindowOpts{
		actualFee: 20, settledAt: t0.Add(24 * time.Hour), mutate: disable,
	})
	// builder 默认写了 2h 的预留计时；关闭超时时必须清空，保持备份自洽。
	var b backupV1
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	b.Requests[0].ReserveDuration = durationJSON(0)
	b.Requests[0].ReserveDeadline = timeJSON(time.Time{})
	fixed, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(fixed, t0.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("settle with reserve timeout disabled must have no deadline: %v", err)
	}
	r, _ := w2.Request("u1", "late")
	if r.State != RequestSettled || !r.ReserveDeadline.IsZero() {
		t.Fatalf("timeout-disabled settled request = %+v", r)
	}
}

// TestRestoreSettledUnaffectedByLaterSessionPolicyLifecycle 验证只要结算在预留
// 有效期内合法完成，申请会话后来到期或被吊销、策略结束或被停用，都不应把已
// 合法完成的结算变成错误历史。
func TestRestoreSettledUnaffectedByLaterSessionPolicyLifecycle(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// 直接受理预留 [t0, t0+2h)；在 t0+1h 结算，此时会话、策略均已结束。
	sessionExpired := func(_ *requestBackupV1, _ *policyBackupV1, s *sessionBackupV1) {
		s.ExpiresAt = timeJSON(t0.Add(30 * time.Minute))
	}
	data := buildSettleWindowBackup(t, t0, settleWindowOpts{
		actualFee: 20, settledAt: t0.Add(time.Hour), mutate: sessionExpired,
	})
	if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
		t.Fatalf("session expiring before a legal settle must not invalidate history: %v", err)
	}

	sessionRevoked := func(_ *requestBackupV1, _ *policyBackupV1, s *sessionBackupV1) {
		s.ExpiresAt = timeJSON(t0.Add(24 * time.Hour))
		s.Revoked = true
	}
	data = buildSettleWindowBackup(t, t0, settleWindowOpts{
		actualFee: 20, settledAt: t0.Add(time.Hour), mutate: sessionRevoked,
	})
	if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
		t.Fatalf("revoked application session must not invalidate a settled reservation: %v", err)
	}

	// 策略在结算前已结束，但策略结束不缩短预留：截止 t0+2h 前的结算仍合法。
	policyEnded := func(_ *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
		p.EndsAt = timeJSON(t0.Add(30 * time.Minute))
	}
	data = buildSettleWindowBackup(t, t0, settleWindowOpts{
		actualFee: 20, settledAt: t0.Add(time.Hour), mutate: policyEnded,
	})
	if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
		t.Fatalf("policy ending before deadline must not shorten the reserve window: %v", err)
	}

	// 策略后来被停用：已合法完成的结算照常恢复。
	policyDeactivated := func(_ *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
		p.Deactivated = true
		p.DeactivatedAt = timeJSON(t0.Add(90 * time.Minute))
		p.DeactivatorAccountID = "payer"
		p.DeactivateReason = "stopped"
	}
	data = buildSettleWindowBackup(t, t0, settleWindowOpts{
		actualFee: 20, settledAt: t0.Add(time.Hour), mutate: policyDeactivated,
	})
	if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
		t.Fatalf("later policy deactivation must not invalidate a legal settle: %v", err)
	}
}

// TestRestoreRejectsBackupWhenAnySettledRequestOutOfWindow 验证同一备份中只要
// 有一笔已结算请求违反预留期限，整个恢复失败、不返回可用钱包，即使另一笔
// 已结算请求完全合法。
func TestRestoreRejectsBackupWhenAnySettledRequestOutOfWindow(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// 两笔零实际费用的已结算请求：late 恰在截止时刻（非法），ok 在窗口内。
	data := buildSettleWindowBackup(t, t0, settleWindowOpts{
		actualFee: 0, settledAt: t0.Add(2 * time.Hour), // late：恰在截止 t0+2h
	})
	var b backupV1
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	b.Requests = append(b.Requests, requestBackupV1{
		PolicyID: "p", RequestID: "ok", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee: 20, ActualFee: 0, State: int(RequestSettled),
		CreatedAt: timeJSON(t0), SettledAt: timeJSON(t0.Add(time.Hour)),
		ReservedAt: timeJSON(t0), ReserveDuration: durationJSON(2 * time.Hour),
		ReserveDeadline: timeJSON(t0.Add(2 * time.Hour)),
	})
	corrupt, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	assertSettleWindowRejected(t, corrupt, t0.Add(48*time.Hour))
}

// TestRestoreLegalExportWithTimeoutSettleRoundTrips 端到端验证当前版本合法导出
// 文本继续兼容：在真实钱包中于预留窗口内结算，导出后于很久以后恢复，已结算
// 状态、金额与账本完整保留。
func TestRestoreLegalExportWithTimeoutSettleRoundTrips(t *testing.T) {
	w, c := setupTimeout(t, time.Hour)
	if _, err := w.Apply(timeoutApply()); err != nil { // r1 预留于 t0，截止 t0+1h
		t.Fatal(err)
	}
	c.t = c.t.Add(30 * time.Minute)
	if _, err := w.Settle("u1", "r1", 7); err != nil { // 窗口内结算
		t.Fatal(err)
	}
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, c.t.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("legal in-window settled export must restore long after: %v", err)
	}
	r, _ := w2.Request("u1", "r1")
	if r.State != RequestSettled || r.ActualFee != 7 {
		t.Fatalf("restored settled request = %+v", r)
	}
	if l1, l2 := len(w.Ledger()), len(w2.Ledger()); l1 != l2 {
		t.Fatalf("ledger entries added on restore: source %d restored %d", l1, l2)
	}
	if n := countKind(w2.Ledger(), LedgerReservationExpiration); n != 0 {
		t.Fatalf("settled request gained timeout entries on restore: %d", n)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 93, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {93 0}", bal)
	}
}
