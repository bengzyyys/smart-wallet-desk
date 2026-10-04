package wallet

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// shiftDirectReserve 把直接受理请求的预留起点整体平移 delta：预留截止与
// 超时记录随预留时长联动，保持计时三元组自洽（reservedAt+duration ==
// deadline），模拟“提交时刻不变、实际预留时刻被拆开”的篡改备份。
func shiftDirectReserve(delta time.Duration) func(*requestBackupV1, *policyBackupV1, *sessionBackupV1) {
	return func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		r.ReservedAt = timeJSON(time.Time(r.ReservedAt).Add(delta))
		if d := time.Duration(r.ReserveDuration); d > 0 {
			r.ReserveDeadline = timeJSON(time.Time(r.ReservedAt).Add(d))
		}
		if !time.Time(r.ReserveExpiredAt).IsZero() {
			r.ReserveExpiredAt = r.ReserveDeadline
		}
	}
}

// assertReserveOriginRejected 恢复必须整体失败、包装 ErrBackupInvalid，错误
// 信息点名使用账户与请求编号并说明是实际预留时刻与提交时刻不一致，且不得
// 返回部分可用的钱包。
func assertReserveOriginRejected(t *testing.T, data []byte, at time.Time) {
	t.Helper()
	w2, err := restoreAt(data, at)
	if err == nil {
		if w2 != nil {
			t.Fatalf("tampered reserve origin restored a wallet: %+v", w2)
		}
		t.Fatal("tampered reserve origin restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, `"u1"`) || !strings.Contains(msg, `"rq"`) {
		t.Fatalf("error %q must name usage account and request id", msg)
	}
	if !strings.Contains(msg, "reserved_at") || !strings.Contains(msg, "created_at") {
		t.Fatalf("error %q must explain the reserved_at/created_at mismatch", msg)
	}
}

// TestRestoreDirectReserveOriginShifted 验证直接受理（未超审批门槛）的请求
// 一旦预留起点与提交时刻脱离就必须整体拒绝：预留仍在、已结算、已取消、
// 已预留超时四种状态统一适用，提前或推迟哪怕一纳秒都拒绝，即使账户预留
// 余额、策略累计金额与请求金额全部对得上。
func TestRestoreDirectReserveOriginShifted(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	created := t0.Add(time.Hour)
	states := map[string]submissionState{
		"reserved":            subDirectReserved,
		"settled":             subDirectSettled,
		"cancelled":           subCancelledFromReserved,
		"reservation expired": subReservationExpired,
	}
	deltas := map[string]time.Duration{
		"later 5 minutes":   5 * time.Minute,
		"earlier 5 minutes": -5 * time.Minute,
		"later 1ns":         time.Nanosecond,
		"earlier 1ns":       -time.Nanosecond,
	}
	for name, st := range states {
		for dname, delta := range deltas {
			t.Run(name+"/"+dname, func(t *testing.T) {
				data := buildSubmissionTimingBackup(t, t0, st, created, shiftDirectReserve(delta))
				assertReserveOriginRejected(t, data, created.Add(48*time.Hour))
			})
		}
	}
}

// TestRestoreDirectReserveOriginTamperedDeadlineAlreadyPast 复现任务场景：
// 请求当天十二点提交、最长预留十分钟，备份把实际预留改为十二点零五分、
// 截止改为十二点十五分；即使恢复时这个被修改的截止时间也已过去，仍必须
// 整体拒绝，不能先按超时退款、再把矛盾历史当成合法备份。
func TestRestoreDirectReserveOriginTamperedDeadlineAlreadyPast(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tamper := func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
		p.MaxReserveDuration = durationJSON(10 * time.Minute)
		r.ReserveDuration = durationJSON(10 * time.Minute)
		r.ReservedAt = timeJSON(t0.Add(5 * time.Minute))
		r.ReserveDeadline = timeJSON(t0.Add(15 * time.Minute))
	}
	data := buildSubmissionTimingBackup(t, t0, subDirectReserved, t0, tamper)
	// 恢复时刻远晚于被篡改的截止 12:15：不能洗白为正常超时。
	assertReserveOriginRejected(t, data, t0.Add(48*time.Hour))
}

// TestRestoreDirectReserveOriginApprovalDisabled 验证策略关闭审批
// （ApprovalThreshold 为零）时同样适用：预留起点与提交时刻必须一致。
func TestRestoreDirectReserveOriginApprovalDisabled(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	created := t0.Add(time.Hour)
	disable := func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
		p.ApprovalThreshold = 0
		p.ApprovalWait = 0
		shiftDirectReserve(5 * time.Minute)(r, p, nil)
	}
	data := buildSubmissionTimingBackup(t, t0, subDirectReserved, created, disable)
	assertReserveOriginRejected(t, data, created.Add(48*time.Hour))
}

// TestRestoreDirectReserveOriginNoReserveTimeout 验证关闭预留超时（最长预留
// 时长为零）的策略也不能让预留起点与提交时刻脱离。
func TestRestoreDirectReserveOriginNoReserveTimeout(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	created := t0.Add(time.Hour)
	noTimeout := func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
		p.MaxReserveDuration = 0
		r.ReserveDuration = 0
		r.ReserveDeadline = timeJSON(time.Time{})
		r.ReservedAt = timeJSON(time.Time(r.ReservedAt).Add(time.Nanosecond))
	}
	data := buildSubmissionTimingBackup(t, t0, subDirectReserved, created, noTimeout)
	assertReserveOriginRejected(t, data, created.Add(48*time.Hour))
}

// TestRestoreDirectReserveOriginTimezone 验证两种带不同时区的时间写法若表示
// 同一绝对时刻则正常接受：提交与预留时刻同时换 +05:00 表示，仍是同一瞬间。
func TestRestoreDirectReserveOriginTimezone(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	loc := time.FixedZone("east", 5*60*60)
	created := t0.Add(time.Hour)
	// 提交与预留时刻以 +05:00 时区书写，二者仍是同一瞬间。
	data := buildSubmissionTimingBackup(t, t0, subDirectReserved, created.In(loc), nil)
	w2, err := restoreAt(data, created)
	if err != nil {
		t.Fatalf("same instant expressed in another timezone must restore: %v", err)
	}
	r, _ := w2.Request("u1", "rq")
	if !r.ReservedAt.Equal(created) || !r.CreatedAt.Equal(created) {
		t.Fatalf("timezone-equivalent instants rewritten: %+v", r)
	}
	if !r.ReserveDeadline.Equal(created.Add(2 * time.Hour)) {
		t.Fatalf("reserve deadline must follow the original duration: %+v", r)
	}
}

// TestRestoreDirectReserveOriginLegitRoundTrip 验证合法直接受理备份照常恢复：
// 预留起点即提交时刻，尚未结束的请求按原有截止时间继续处理，不重新开始
// 预留计时。
func TestRestoreDirectReserveOriginLegitRoundTrip(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	created := t0.Add(time.Hour)
	data := buildSubmissionTimingBackup(t, t0, subDirectReserved, created, nil)
	// 恢复时刻仍在原预留窗口内：状态、时刻与余额原样保留。
	w2, err := restoreAt(data, created.Add(time.Hour))
	if err != nil {
		t.Fatalf("legit direct reservation must restore: %v", err)
	}
	r, _ := w2.Request("u1", "rq")
	if r.State != RequestReserved || !r.ReservedAt.Equal(created) ||
		!r.ReserveDeadline.Equal(created.Add(2*time.Hour)) {
		t.Fatalf("reserved request must keep its original timing: %+v", r)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 95, Reserved: 5}) {
		t.Fatalf("balances = %+v, want {95 5}", bal)
	}
}

// TestRestoreApprovedReserveOriginUnaffected 验证经过大额审批后才预留的请求
// 不被这项校验误拒绝：实际预留晚于提交（批准成功时刻）是合法历史。
func TestRestoreApprovedReserveOriginUnaffected(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	created := t0.Add(time.Hour)
	states := map[string]submissionState{
		"reserved": subApprovedReserved,
		"settled":  subApprovedSettled,
	}
	for name, st := range states {
		t.Run(name, func(t *testing.T) {
			// 构造器令批准/预留时刻为提交后 30 分钟，等待期限自洽。
			data := buildSubmissionTimingBackup(t, t0, st, created, nil)
			if _, err := restoreAt(data, created.Add(30*time.Minute)); err != nil {
				t.Fatalf("approved reservation later than submission must restore: %v", err)
			}
		})
	}
}
