package wallet

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// deactivatedAt 为测试中策略 p 的首次停用时刻（相对 t0 固定），提交/批准
// 时刻围绕它构造合法与矛盾变体。
func deactivatePolicyAt(deactivatedAt time.Time) func(*requestBackupV1, *policyBackupV1, *sessionBackupV1) {
	return func(_ *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
		p.Deactivated = true
		p.DeactivatedAt = timeJSON(deactivatedAt)
		p.DeactivatorAccountID = "payer"
		p.DeactivateReason = "stop it"
	}
}

// assertDeactivationRejected 恢复必须整体失败、包装 ErrBackupInvalid，错误
// 信息点名使用账户、请求编号与策略编号，并说明是停用后提交还是停用后批准，
// 且不得返回部分钱包。
func assertDeactivationRejected(t *testing.T, data []byte, at time.Time, want string) {
	t.Helper()
	w2, err := restoreAt(data, at)
	if err == nil {
		if w2 != nil {
			t.Fatalf("post-deactivation history restored a wallet: %+v", w2)
		}
		t.Fatal("post-deactivation history restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	for _, part := range []string{`"u1"`, `"rq"`, `"p"`, "deactivated", want} {
		if !strings.Contains(msg, part) {
			t.Fatalf("error %q must identify %q (usage account, request, policy, conflict)", msg, part)
		}
	}
}

// TestRestoreRejectsSubmissionAfterDeactivation 验证核心修复：关联已停用策略
// 的已受理请求，其保存的提交时刻严格晚于策略首次停用时刻时，整份备份必须
// 无效。该核对对直接预留与审批两条路径、以及全部后续状态（已预留、已结算、
// 已取消、已拒绝、已过期、预留超时）统一适用：后续处理时刻不是新的提交时刻，
// 余额与策略累计金额完全对得上也不能放行。
func TestRestoreRejectsSubmissionAfterDeactivation(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	deact := t0.Add(2 * time.Hour)
	states := map[string]submissionState{
		"direct reserved":         subDirectReserved,
		"approved reserved":       subApprovedReserved,
		"direct settled":          subDirectSettled,
		"approved settled":        subApprovedSettled,
		"cancelled from reserved": subCancelledFromReserved,
		"cancelled from pending":  subCancelledFromPending,
		"rejected":                subRejected,
		"expired":                 subExpired,
		"reservation expired":     subReservationExpired,
	}
	for name, st := range states {
		t.Run(name, func(t *testing.T) {
			// 提交于停用后一纳秒；其余计时字段均由提交时刻推导、完全自洽。
			data := buildSubmissionTimingBackup(t, t0, st, deact.Add(time.Nanosecond), deactivatePolicyAt(deact))
			assertDeactivationRejected(t, data, t0.Add(48*time.Hour), "submitted")
		})
	}
}

// TestRestoreRejectsApprovalAfterDeactivation 验证超过审批门槛、经批准才预留
// 费用的请求，其保存的批准时刻严格晚于策略首次停用时刻时，整份备份必须无效：
// 申请在停用前提交、批准仍在等待期限内、余额与额度完全对得上也不能接受停用后
// 批准的历史；已预留与后来已结算两条路径同样适用。
func TestRestoreRejectsApprovalAfterDeactivation(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	deact := t0.Add(2 * time.Hour)
	// 提交于 t0+1.5h（停用前），等待截止 t0+2.5h；批准被篡改为停用后一纳秒，
	// 仍落在等待期限内。
	created := t0.Add(90 * time.Minute)
	approvedLate := func(r *requestBackupV1, p *policyBackupV1, s *sessionBackupV1) {
		deactivatePolicyAt(deact)(r, p, s)
		approved := deact.Add(time.Nanosecond)
		r.DecidedAt = timeJSON(approved)
		r.ReservedAt = timeJSON(approved)
		r.ReserveDeadline = timeJSON(approved.Add(2 * time.Hour))
	}
	for name, st := range map[string]submissionState{
		"approved reserved": subApprovedReserved,
		"approved settled":  subApprovedSettled,
	} {
		t.Run(name, func(t *testing.T) {
			data := buildSubmissionTimingBackup(t, t0, st, created, approvedLate)
			assertDeactivationRejected(t, data, t0.Add(48*time.Hour), "approved")
		})
	}
}

// TestRestoreAcceptsSubmissionOrApprovalExactlyAtDeactivation 验证边界相等不能
// 仅凭相等拒绝：先完成申请或批准、再停用，二者可能共享同一个时间戳。提交恰在
// 首次停用时刻的直接受理请求、批准恰在首次停用时刻的超门槛请求都必须正常恢复。
func TestRestoreAcceptsSubmissionOrApprovalExactlyAtDeactivation(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	deact := t0.Add(2 * time.Hour)

	// 提交恰在停用时刻（直接受理，无审批信息）。
	data := buildSubmissionTimingBackup(t, t0, subDirectReserved, deact, deactivatePolicyAt(deact))
	if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
		t.Fatalf("submission exactly at deactivation must restore: %v", err)
	}

	// 提交于 t0+1.5h，批准恰在停用时刻（等待截止 t0+2.5h，期限核对通过）。
	data = buildSubmissionTimingBackup(t, t0, subApprovedReserved, t0.Add(90*time.Minute), deactivatePolicyAt(deact))
	if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
		t.Fatalf("approval exactly at deactivation must restore: %v", err)
	}
}

// TestRestoreDeactivationTimingTimezoneAndNanosecond 验证提交/停用时刻按完整
// 绝对瞬间比较：纳秒精度保留，不同时区表示的同一时刻判定相同。
func TestRestoreDeactivationTimingTimezoneAndNanosecond(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	loc := time.FixedZone("east", 5*60*60)
	deact := t0.Add(2 * time.Hour)

	// 停用时刻换 +05:00 时区表示：提交恰在同一瞬间必须接受。
	data := buildSubmissionTimingBackup(t, t0, subDirectReserved, deact, deactivatePolicyAt(deact.In(loc)))
	if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
		t.Fatalf("submission at deactivation instant expressed in another timezone must restore: %v", err)
	}

	// 提交换时区表示且晚停用一纳秒：必须拒绝。
	data = buildSubmissionTimingBackup(t, t0, subDirectReserved, deact.Add(time.Nanosecond).In(loc), deactivatePolicyAt(deact))
	assertDeactivationRejected(t, data, t0.Add(48*time.Hour), "submitted")
}

// TestRestoreDeactivationTimingNotLaunderedByRestoreTime 验证校验只依据备份
// 保存的历史时间：即使相关预留在恢复时早已超过预留截止、按规则本应超时退回，
// 也必须整体拒绝，不能先按当前时间退款再接受本不合法的备份。
func TestRestoreDeactivationTimingNotLaunderedByRestoreTime(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	deact := t0.Add(2 * time.Hour)

	// 停用后提交的直接预留：预留截止 t0+4h+1ns 远早于恢复时刻。
	data := buildSubmissionTimingBackup(t, t0, subDirectReserved, deact.Add(time.Nanosecond), deactivatePolicyAt(deact))
	assertDeactivationRejected(t, data, t0.Add(30*24*time.Hour), "submitted")

	// 停用后批准的预留：同样不能洗白。
	approvedLate := func(r *requestBackupV1, p *policyBackupV1, s *sessionBackupV1) {
		deactivatePolicyAt(deact)(r, p, s)
		approved := deact.Add(time.Nanosecond)
		r.DecidedAt = timeJSON(approved)
		r.ReservedAt = timeJSON(approved)
		r.ReserveDeadline = timeJSON(approved.Add(2 * time.Hour))
	}
	data = buildSubmissionTimingBackup(t, t0, subApprovedReserved, t0.Add(90*time.Minute), approvedLate)
	assertDeactivationRejected(t, data, t0.Add(30*24*time.Hour), "approved")
}

// TestRestoreKeepsPreDeactivationHistory 验证停用前合法提交并完成预留的请求
// 继续沿用已有行为：停用后才结算或取消的历史正常恢复，后续处理时间不会被
// 当成新的提交或批准时间；状态、金额与既有账本原样保留，不补写停用、拒绝
// 或退款记录。
func TestRestoreKeepsPreDeactivationHistory(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	deact := t0.Add(2 * time.Hour)
	created := t0.Add(time.Hour)

	// 停用后结算：结算时刻 t0+2.5h 晚于停用，但提交与预留均在停用前。
	settledAfter := func(r *requestBackupV1, p *policyBackupV1, s *sessionBackupV1) {
		deactivatePolicyAt(deact)(r, p, s)
		r.SettledAt = timeJSON(deact.Add(30 * time.Minute))
	}
	data := buildSubmissionTimingBackup(t, t0, subDirectSettled, created, settledAfter)
	w2, err := restoreAt(data, t0.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("request settled after deactivation must restore: %v", err)
	}
	if r, _ := w2.Request("u1", "rq"); r.State != RequestSettled || r.ActualFee != 3 {
		t.Fatalf("settled history changed: %+v", r)
	}
	if n := len(w2.Ledger()); n != 0 {
		t.Fatalf("restore added ledger records: %+v", w2.Ledger())
	}

	// 停用后取消（已预留路径）：提交与预留均在停用前，取消不记录决定时间。
	data = buildSubmissionTimingBackup(t, t0, subCancelledFromReserved, created, deactivatePolicyAt(deact))
	w2, err = restoreAt(data, t0.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("request cancelled after deactivation must restore: %v", err)
	}
	if r, _ := w2.Request("u1", "rq"); r.State != RequestCancelled {
		t.Fatalf("cancelled history changed: %+v", r)
	}

	// 停用后批准路径的合法历史：停用前提交并批准，停用后结算。
	approvedSettledAfter := func(r *requestBackupV1, p *policyBackupV1, s *sessionBackupV1) {
		deactivatePolicyAt(deact)(r, p, s)
		r.SettledAt = timeJSON(deact.Add(30 * time.Minute))
	}
	data = buildSubmissionTimingBackup(t, t0, subApprovedSettled, created, approvedSettledAfter)
	w2, err = restoreAt(data, t0.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("approved request settled after deactivation must restore: %v", err)
	}
	if r, _ := w2.Request("u1", "rq"); r.State != RequestSettled || r.ApproverAccountID != "payer" {
		t.Fatalf("approved settled history changed: %+v", r)
	}
}
