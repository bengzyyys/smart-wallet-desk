package wallet

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// 本文件验证恢复时的逐笔额度核对：备份保存的每笔请求，其预估费用都不得
// 严格超过关联策略保存的完整累计上限。现存“预留 + 已花费”合计的核对是
// 另一条独立规则，见 backup_test.go 中的累计额度用例。

// restoreQuotaBackup 用既有的自洽备份构造器生成备份并在 at 时刻恢复；
// 备份是否合法由调用方按用例断言。
func restoreQuotaBackup(t *testing.T, t0 time.Time, policies []quotaPolicySpec, at time.Time) (*Wallet, error) {
	t.Helper()
	data := buildCumulativeQuotaBackup(t, t0, policies)
	return restoreAt(data, at)
}

// restoreQuotaBackupWithPayerAvailable 同上，但把出资账户可用余额改为
// available（自洽构造器默认余额为零），用于区分“余额不足”与“额度不足”。
func restoreQuotaBackupWithPayerAvailable(t *testing.T, t0 time.Time, policies []quotaPolicySpec, at time.Time, available int64) (*Wallet, error) {
	t.Helper()
	data := buildCumulativeQuotaBackup(t, t0, policies)
	m := mustMap(t, data)
	for _, a := range m["accounts"].([]interface{}) {
		am := a.(map[string]interface{})
		if am["id"] == "payer" {
			am["available"] = float64(available)
		}
	}
	return restoreAt(mustRemap(t, m), at)
}

// TestRestoreRejectsEstimatedFeeExceedingFullTotal 题述主场景：策略累计
// 上限 10、单次上限 100、审批门槛 10 时，预估费用 30 的新申请会因超出
// 累计上限被拒绝，不能进入待审批；因此备份中同类待审批请求是正常流程
// 无法产生的记录，必须整体拒绝、不返回钱包，错误需指出使用账户、请求
// 编号、策略编号以及保存的预估费用与累计上限。
func TestRestoreRejectsEstimatedFeeExceedingFullTotal(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildCumulativeQuotaBackup(t, t0, []quotaPolicySpec{{
		id: "p-small", per: 100, total: 10, threshold: 10,
		wait: 5 * time.Minute,
		reqs: []quotaReqSpec{
			{id: "big-pending", account: "u1", estFee: 30, state: RequestPendingApproval},
		},
	}})

	w2, err := restoreAt(data, t0)
	if err == nil {
		if w2 != nil {
			t.Fatalf("contradictory backup restored a wallet %+v", w2)
		}
		t.Fatal("contradictory backup restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	for _, want := range []string{"u1", "big-pending", "p-small", "30", "10", "cumulative total limit"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q must mention %q (usage account, request id, policy id, saved fee and full limit)", msg, want)
		}
	}
}

// TestRestoreEstimatedFeeRuleAppliesToEveryState 验证逐笔核对不因请求后来
// 不占用额度而豁免：待审批、被拒绝、待审批取消、待审批过期、已预留后
// 取消、预留超时，以及曾预留后按较低实际费用结算的历史，预估费用严格
// 超过完整累计上限时一律拒绝。当前仍占用额度的已预留请求同样先在逐笔
// 核对处被拒绝（早于现存占用合计核对）。
func TestRestoreEstimatedFeeRuleAppliesToEveryState(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		threshold int64
		wait      time.Duration
		req       quotaReqSpec
		restoreAt time.Time
	}{
		{
			name:      "pending approval",
			threshold: 10, wait: 5 * time.Minute,
			req:       quotaReqSpec{id: "r", account: "u1", estFee: 30, state: RequestPendingApproval},
			restoreAt: t0,
		},
		{
			name:      "rejected",
			threshold: 10, wait: 5 * time.Minute,
			req:       quotaReqSpec{id: "r", account: "u1", estFee: 30, state: RequestRejected, rejectReason: "no"},
			restoreAt: t0,
		},
		{
			name:      "cancelled while pending",
			threshold: 10, wait: 5 * time.Minute,
			req:       quotaReqSpec{id: "r", account: "u1", estFee: 30, state: RequestCancelled},
			restoreAt: t0,
		},
		{
			name:      "pending-approval expired, deadline long past at restore",
			threshold: 10, wait: 5 * time.Minute,
			req: quotaReqSpec{id: "r", account: "u1", estFee: 30, state: RequestExpired},
			// 恢复时期限早已过去：不能先自动按到期处理再接受矛盾备份。
			restoreAt: t0.Add(time.Hour),
		},
		{
			name: "still reserved",
			req:  quotaReqSpec{id: "r", account: "u1", estFee: 30, state: RequestReserved},
			// restoreAt 早于其（关闭的）预留超时，不影响结论。
			restoreAt: t0,
		},
		{
			name:      "cancelled after reservation",
			req:       quotaReqSpec{id: "r", account: "u1", estFee: 30, state: RequestCancelled, wasReserved: true},
			restoreAt: t0,
		},
		{
			name:      "reservation expired before restore",
			req:       quotaReqSpec{id: "r", account: "u1", estFee: 30, state: RequestReservationExpired},
			restoreAt: t0,
		},
		{
			name: "reserved then settled at a lower actual fee",
			req:  quotaReqSpec{id: "r", account: "u1", estFee: 30, actualFee: 5, state: RequestSettled},
			// 当前只占用实际费用 5，并未超额；仍须按保存的预估费用 30 拒绝。
			restoreAt: t0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w2, err := restoreQuotaBackup(t, t0, []quotaPolicySpec{{
				id: "p-small", per: 100, total: 10,
				threshold:  tc.threshold,
				wait:       tc.wait,
				reserveDur: time.Minute,
				reqs:       []quotaReqSpec{tc.req},
			}}, tc.restoreAt)
			if err == nil {
				t.Fatalf("backup restored: %+v", w2)
			}
			if !errors.Is(err, ErrBackupInvalid) {
				t.Fatalf("err = %v, want ErrBackupInvalid", err)
			}
			// 必须命中逐笔核对（使用账户、请求编号、保存费用与完整上限），
			// 而不是仅被现存占用合计核对拦截。
			msg := err.Error()
			for _, want := range []string{"u1", "r", "p-small", "30", "10"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("error %q must mention %q", msg, want)
				}
			}
			if !strings.Contains(msg, "full cumulative total limit") {
				t.Fatalf("error %q must be the per-request full-limit check", msg)
			}
		})
	}
}

// TestRestoreAcceptsEstimatedFeeEqualToFullTotal 预估费用恰等于完整累计
// 上限时，只要其余校验全部通过就接受：逐笔核对是严格大于判断，不与现存
// 占用合计核对混同。
func TestRestoreAcceptsEstimatedFeeEqualToFullTotal(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// 恰等于上限的待审批请求（门槛严格低于费用，确实属于审批路径）。
	w2, err := restoreQuotaBackup(t, t0, []quotaPolicySpec{{
		id: "p-eq", per: 100, total: 10, threshold: 5, wait: 5 * time.Minute,
		reqs: []quotaReqSpec{
			{id: "pending-eq", account: "u1", estFee: 10, state: RequestPendingApproval},
			{id: "rejected-eq", account: "u1", estFee: 10, state: RequestRejected, rejectReason: "no"},
		},
	}}, t0)
	if err != nil {
		t.Fatalf("fee == full total pending/rejected: %v", err)
	}
	if r, _ := w2.Request("u1", "pending-eq"); r.State != RequestPendingApproval {
		t.Fatalf("pending-eq state = %v", r.State)
	}

	// 恰等于上限的预估、按更低实际费用结算：占用仅为实际费用，合法。
	w2, err = restoreQuotaBackup(t, t0, []quotaPolicySpec{{
		id: "p-eq2", per: 100, total: 10,
		reqs: []quotaReqSpec{
			{id: "settled-eq", account: "u1", estFee: 10, actualFee: 0, state: RequestSettled},
		},
	}}, t0)
	if err != nil {
		t.Fatalf("fee == full total settled low: %v", err)
	}
}

// TestRestoreFullTotalRuleUsesFullLimitNotRemaining 题述额度例：累计上限
// 50 的策略，一笔历史请求预估 40、最终扣减 10，另有一笔现存预留 40，
// 当前占用恰为 50——两笔各自的预估费用都不超过完整上限 50，备份必须
// 恢复；不能把历史预估 40 再加到当前占用上而误判超限。
func TestRestoreFullTotalRuleUsesFullLimitNotRemaining(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	w2, err := restoreQuotaBackup(t, t0, []quotaPolicySpec{{
		id: "p-50", per: 50, total: 50, reserveDur: time.Minute,
		reqs: []quotaReqSpec{
			{id: "history", account: "u1", estFee: 40, actualFee: 10, state: RequestSettled},
			{id: "live", account: "u1", estFee: 40, state: RequestReserved},
		},
	}}, t0)
	if err != nil {
		t.Fatalf("spent 10 + reserved 40 == 50 with historical estimate 40: %v", err)
	}
	pv, _ := w2.Policy("p-50")
	if pv.ReservedTotal != 40 || pv.SpentTotal != 10 {
		t.Fatalf("policy totals = reserved %d spent %d, want 40/10", pv.ReservedTotal, pv.SpentTotal)
	}
}

// TestRestoreRefundedHistoricalEstimatesMaySumOverTotal 同一策略下已经
// 退回、不再占用额度的历史预估费用合计超过完整上限，也不仅因此拒绝。
func TestRestoreRefundedHistoricalEstimatesMaySumOverTotal(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// 完整上限 10：三笔费用各为 10（恰等于上限）的历史分别处于已预留后
	// 取消、预留超时、待审批被拒绝，历史预估合计 30 > 10，当前占用为零。
	w2, err := restoreQuotaBackup(t, t0, []quotaPolicySpec{{
		id: "p-refunds", per: 100, total: 10, threshold: 5,
		wait: 5 * time.Minute, reserveDur: time.Minute,
		reqs: []quotaReqSpec{
			{id: "c", account: "u1", estFee: 10, state: RequestCancelled, wasReserved: true},
			{id: "e", account: "u1", estFee: 10, state: RequestReservationExpired},
			{id: "j", account: "u1", estFee: 10, state: RequestRejected, rejectReason: "no"},
		},
	}}, t0)
	if err != nil {
		t.Fatalf("refunded historical estimates summing over total: %v", err)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 0, Reserved: 0}) {
		t.Fatalf("balance = %+v, want nothing occupied", bal)
	}
}

// TestRestorePendingTemporarilyUnapprovableIsStillValid 合法的待审批请求
// 可能因其他请求后来占用了额度而在恢复时暂时无法批准：这不构成备份无效。
func TestRestorePendingTemporarilyUnapprovableIsStillValid(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// 出资账户保留足够余额，确保批准失败原因是额度而不是余额。
	w2, err := restoreQuotaBackupWithPayerAvailable(t, t0, []quotaPolicySpec{{
		id: "p-50", per: 50, total: 50, threshold: 5, wait: time.Hour,
		reqs: []quotaReqSpec{
			{id: "occupying", account: "u1", estFee: 40, state: RequestReserved},
			{id: "pending-20", account: "u1", estFee: 20, state: RequestPendingApproval},
		},
	}}, t0, 100)
	if err != nil {
		t.Fatalf("pending temporarily over remaining quota: %v", err)
	}
	r, err := w2.Request("u1", "pending-20")
	if err != nil || r.State != RequestPendingApproval {
		t.Fatalf("pending-20 = %+v err = %v, want still pending", r, err)
	}

	// 补上出资账户的审批会话后尝试批准：现存占用 40 + 20 严格超过上限 50，
	// 批准按现有规则返回超额度错误、请求继续待审批——这是运行期行为，不
	// 影响备份本身的有效性。
	if _, err := w2.CreateSession("sa", "payer", "deva", t0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w2.Approve("u1", "pending-20", "sa", "deva"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("approve err = %v, want ErrQuotaExceeded", err)
	}
	if r, _ := w2.Request("u1", "pending-20"); r.State != RequestPendingApproval {
		t.Fatalf("pending-20 state after failed approve = %v", r.State)
	}
}

// TestRestoreOneOverLimitRequestRejectsWholeBackup 同一备份中的合法请求
// 不能被部分恢复：只要存在一笔严格超限，整个 Restore 返回错误且不返回
// 钱包，哪怕另一条策略（或同一策略）的请求完全合法。
func TestRestoreOneOverLimitRequestRejectsWholeBackup(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	w2, err := restoreQuotaBackup(t, t0, []quotaPolicySpec{
		{
			id: "p-ok", per: 100, total: 100,
			reqs: []quotaReqSpec{
				{id: "legal", account: "u1", estFee: 5, state: RequestReserved},
			},
		},
		{
			id: "p-bad", per: 100, total: 10, threshold: 5, wait: 5 * time.Minute,
			reqs: []quotaReqSpec{
				{id: "illegal", account: "u2", estFee: 11, state: RequestPendingApproval},
			},
		},
	}, t0)
	if err == nil {
		t.Fatalf("backup with one illegal request restored: %+v", w2)
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	for _, want := range []string{"u2", "illegal", "p-bad", "11", "10"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q must mention %q", msg, want)
		}
	}
}
