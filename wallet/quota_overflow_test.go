package wallet

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

// decodeBackup 把备份 JSON 解析回 backupV1，便于测试在自洽备份上做小幅
// 改动（如给出资账户设置可用余额、追加出资审批会话）。
func decodeBackup(t *testing.T, data []byte) backupV1 {
	t.Helper()
	var b backupV1
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// withPayerApprovalSession 给出资账户 payer 追加一条绑定审批设备的有效
// 会话 sa，并把出资账户可用余额改为 available，再重新序列化。
func withPayerApprovalSession(t *testing.T, data []byte, t0 time.Time, available int64) []byte {
	t.Helper()
	b := decodeBackup(t, data)
	for i := range b.Accounts {
		if b.Accounts[i].ID == "payer" {
			b.Accounts[i].Available = available
		}
	}
	b.Sessions = append(b.Sessions, sessionBackupV1{
		ID:        "sa",
		AccountID: "payer",
		DeviceID:  "deva",
		ExpiresAt: timeJSON(t0.Add(24 * time.Hour)),
		CreatedAt: timeJSON(t0),
	})
	out, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// quotaOverflowApplyInput 构造通过现有授权与会话校验的申请内容。
func quotaOverflowApplyInput(rid string, fee int64) RequestInput {
	return RequestInput{
		PolicyID:     "p",
		RequestID:    rid,
		AccountID:    "u1",
		SessionID:    "s1",
		DeviceID:     "d1",
		Operation:    "charge",
		Payee:        "shop",
		EstimatedFee: fee,
	}
}

// TestApplyCumulativeQuotaInt64Overflow 复现任务给定场景：策略累计上限为
// MaxInt64、已花费 MaxInt64-1、无当前预留、出资账户可用 10。费用 2 计入
// 后合计越过 int64 上限，必须按超额度拒绝（旧实现因相加回绕会受理）；
// 费用 1 恰好补齐上限，应正常受理。
func TestApplyCumulativeQuotaInt64Overflow(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildCumulativeQuotaBackup(t, t0, []quotaPolicySpec{{
		id: "p", per: math.MaxInt64, total: math.MaxInt64,
		reqs: []quotaReqSpec{
			{id: "spent", account: "u1", estFee: math.MaxInt64 - 1, actualFee: math.MaxInt64 - 1, state: RequestSettled},
		},
	}})
	data = withPayerApprovalSession(t, data, t0, 10)
	w, err := restoreAt(data, t0)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	// 费用 2：合计 (MaxInt64-1)+2 越过 int64 上限，必须拒绝。
	if _, err := w.Apply(quotaOverflowApplyInput("r-over", 2)); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("apply fee 2 err = %v, want ErrQuotaExceeded", err)
	}
	// 不创建请求。
	if _, err := w.Request("u1", "r-over"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("rejected request should not exist, err = %v", err)
	}
	// 不扣减可用余额、不新增预留。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 10, Reserved: 0}) {
		t.Fatalf("balance after rejected apply = %+v, want {10 0}", bal)
	}
	pv, _ := w.Policy("p")
	if pv.ReservedTotal != 0 || pv.SpentTotal != math.MaxInt64-1 {
		t.Fatalf("policy totals after rejected apply = reserved %d spent %d", pv.ReservedTotal, pv.SpentTotal)
	}
	// 按现有申请拒绝规则留下原因。
	var rejected bool
	for _, e := range w.AccountLedger("u1") {
		if e.Kind == LedgerRejection && e.RequestID == "r-over" {
			rejected = true
			if e.Reason == "" {
				t.Fatal("rejection entry missing reason")
			}
		}
	}
	if !rejected {
		t.Fatal("missing rejection ledger entry for overflow apply")
	}

	// 费用 1：合计恰好等于 MaxInt64，正常受理并预留。
	req, err := w.Apply(quotaOverflowApplyInput("r-ok", 1))
	if err != nil {
		t.Fatalf("apply fee 1 at exact cap: %v", err)
	}
	if req.State != RequestReserved {
		t.Fatalf("fee 1 state = %v, want reserved", req.State)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 9, Reserved: 1}) {
		t.Fatalf("balance after accepted apply = %+v, want {9 1}", bal)
	}
	pv, _ = w.Policy("p")
	if pv.ReservedTotal != 1 || pv.SpentTotal != math.MaxInt64-1 {
		t.Fatalf("policy totals after accepted apply = reserved %d spent %d", pv.ReservedTotal, pv.SpentTotal)
	}
}

// overflowApproveBackup 构造批准溢出场景的自洽备份：
//   - 已结算实际费用 MaxInt64-6（占用已花费额度）；
//   - 一笔费用 1 的现存预留 occupy（占用预留额度，可取消或超时释放）；
//   - 一笔费用 6 的待审批请求 hold（严格超过审批门槛 5）。
//
// 批准 hold 前合计 (MaxInt64-6)+1+6 越过 int64 上限，必须拒绝；释放
// occupy 后 (MaxInt64-6)+6 恰好等于上限，可以批准。
func overflowApproveBackup(t *testing.T, t0 time.Time, reserveDur time.Duration) []byte {
	t.Helper()
	data := buildCumulativeQuotaBackup(t, t0, []quotaPolicySpec{{
		id: "p", per: math.MaxInt64, total: math.MaxInt64,
		threshold: 5, wait: 10 * time.Minute, reserveDur: reserveDur,
		reqs: []quotaReqSpec{
			{id: "spent", account: "u1", estFee: math.MaxInt64 - 6, actualFee: math.MaxInt64 - 6, state: RequestSettled},
			{id: "occupy", account: "u1", estFee: 1, state: RequestReserved},
			{id: "hold", account: "u1", estFee: 6, state: RequestPendingApproval},
		},
	}})
	// 可用余额 10：足以批准费用 6，使额度判断先于余额失败被观察到。
	return withPayerApprovalSession(t, data, t0, 10)
}

// assertHoldUnchanged 校验失败的批准没有改动待审批请求与资金状态。
func assertHoldUnchanged(t *testing.T, w *Wallet, createdAt, waitDeadline time.Time, wantBal Balances, wantReserved int64, label string) {
	t.Helper()
	req, err := w.Request("u1", "hold")
	if err != nil {
		t.Fatalf("%s: request lookup: %v", label, err)
	}
	if req.State != RequestPendingApproval {
		t.Fatalf("%s: state = %v, want still pending", label, req.State)
	}
	if !req.CreatedAt.Equal(createdAt) || !req.WaitDeadline.Equal(waitDeadline) {
		t.Fatalf("%s: timestamps changed: created %v->%v deadline %v->%v",
			label, createdAt, req.CreatedAt, waitDeadline, req.WaitDeadline)
	}
	if !req.DecidedAt.IsZero() || req.ApproverAccountID != "" {
		t.Fatalf("%s: failed approval left decision info: decided %v approver %q",
			label, req.DecidedAt, req.ApproverAccountID)
	}
	if bal, _ := w.Balance("payer"); bal != wantBal {
		t.Fatalf("%s: balance = %+v, want %+v", label, bal, wantBal)
	}
	pv, _ := w.Policy("p")
	if pv.ReservedTotal != wantReserved || pv.SpentTotal != math.MaxInt64-6 {
		t.Fatalf("%s: policy totals = reserved %d spent %d, want reserved %d",
			label, pv.ReservedTotal, pv.SpentTotal, wantReserved)
	}
	for _, e := range w.AccountLedger("u1") {
		if e.RequestID == "hold" && (e.Kind == LedgerReserve || e.Kind == LedgerApproval) {
			t.Fatalf("%s: failed approval created a %v ledger entry", label, e.Kind)
		}
	}
}

// TestApproveCumulativeQuotaInt64OverflowKeepsPending 验证待审批请求在
// 合计越过 int64 上限时无法被批准：返回 ErrQuotaExceeded，请求保持待审批，
// 提交时间、等待截止时间与决定信息不变，不开始预留计时、不留批准/预留
// 记录；取消其他请求释放额度后，期限内仍可再次批准成功。
func TestApproveCumulativeQuotaInt64OverflowKeepsPending(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// 关闭预留超时：靠取消占用额度的预留来释放。
	data := overflowApproveBackup(t, t0, 0)
	w, err := restoreAt(data, t0)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	before, _ := w.Request("u1", "hold")
	if before.State != RequestPendingApproval {
		t.Fatalf("initial state = %v, want pending", before.State)
	}

	// 批准会越过 int64 上限：拒绝并保留待审批。
	if _, err := w.Approve("u1", "hold", "sa", "deva"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("approve overflow err = %v, want ErrQuotaExceeded", err)
	}
	assertHoldUnchanged(t, w, before.CreatedAt, before.WaitDeadline,
		Balances{Available: 10, Reserved: 1}, 1, "after overflow approve")

	// 期限内再次批准仍超额，仍保持待审批。
	if _, err := w.Approve("u1", "hold", "sa", "deva"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("second approve err = %v, want ErrQuotaExceeded", err)
	}
	assertHoldUnchanged(t, w, before.CreatedAt, before.WaitDeadline,
		Balances{Available: 10, Reserved: 1}, 1, "after second overflow approve")

	// 取消占用额度的现存预留，释放 1 单位共享额度。
	if _, err := w.Cancel("u1", "occupy"); err != nil {
		t.Fatalf("cancel occupy: %v", err)
	}
	// 释放后 (MaxInt64-6)+6 恰好等于上限，批准成功。
	req, err := w.Approve("u1", "hold", "sa", "deva")
	if err != nil {
		t.Fatalf("approve after quota released: %v", err)
	}
	if req.State != RequestReserved {
		t.Fatalf("approved state = %v, want reserved", req.State)
	}
	if !req.ReservedAt.Equal(t0) || !req.DecidedAt.Equal(t0) {
		t.Fatalf("reserve timing should start at approval: reserved %v decided %v", req.ReservedAt, req.DecidedAt)
	}
	if req.ApproverAccountID != "payer" {
		t.Fatalf("approver = %q, want payer", req.ApproverAccountID)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 5, Reserved: 6}) {
		t.Fatalf("balance after approval = %+v, want {5 6}", bal)
	}
	pv, _ := w.Policy("p")
	if pv.ReservedTotal != 6 || pv.SpentTotal != math.MaxInt64-6 {
		t.Fatalf("policy totals after approval = reserved %d spent %d", pv.ReservedTotal, pv.SpentTotal)
	}
}

// TestApproveOverflowAfterReservationTimeoutRelease 验证批准前的额度检查
// 继续计入已到期预留的自动释放：占用额度的预留超时退回后，原本超额的
// 待审批请求可在期限内批准成功。
func TestApproveOverflowAfterReservationTimeoutRelease(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// occupy 的预留截止 t0+1m；hold 的等待截止 t0+10m。
	data := overflowApproveBackup(t, t0, time.Minute)
	w, err := restoreAt(data, t0)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	c := &clock{t: t0}
	w.now = func() time.Time { return c.t }

	before, _ := w.Request("u1", "hold")

	// t0+30s：occupy 尚未到期，批准仍超额。
	c.t = t0.Add(30 * time.Second)
	if _, err := w.Approve("u1", "hold", "sa", "deva"); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("approve before timeout err = %v, want ErrQuotaExceeded", err)
	}
	assertHoldUnchanged(t, w, before.CreatedAt, before.WaitDeadline,
		Balances{Available: 10, Reserved: 1}, 1, "before timeout release")

	// 到达 occupy 的预留截止时刻：自动退回释放额度，批准成功。
	c.t = t0.Add(time.Minute)
	req, err := w.Approve("u1", "hold", "sa", "deva")
	if err != nil {
		t.Fatalf("approve after timeout release: %v", err)
	}
	if req.State != RequestReserved {
		t.Fatalf("state = %v, want reserved", req.State)
	}
	// 占用预留已超时退回，hold 预留 6：可用 = 10+1-6 = 5。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 5, Reserved: 6}) {
		t.Fatalf("balance = %+v, want {5 6}", bal)
	}
	pv, _ := w.Policy("p")
	if pv.ReservedTotal != 6 || pv.SpentTotal != math.MaxInt64-6 {
		t.Fatalf("policy totals = reserved %d spent %d", pv.ReservedTotal, pv.SpentTotal)
	}
	// hold 的提交与等待截止时间不被失败和成功的批准改写。
	if !req.CreatedAt.Equal(before.CreatedAt) || !req.WaitDeadline.Equal(before.WaitDeadline) {
		t.Fatalf("hold timestamps changed: created %v->%v deadline %v->%v",
			before.CreatedAt, req.CreatedAt, before.WaitDeadline, req.WaitDeadline)
	}
}
