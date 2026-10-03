package wallet

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

// buildMaxQuotaBackup 构造一份资金自洽的备份：策略 p-big 的单次与累计上限
// 均为 int64 最大值，含一笔已结算请求（预估 MaxInt64、实际 MaxInt64-1，
// 即已花费 MaxInt64-1），没有现存预留；出资账户可用余额由 available 指定。
// 使用账户 u1（会话 s1/设备 d1）、u2（会话 s2/设备 d2）共享额度。
// threshold>0 时策略开启审批（等待 1 小时），已结算大额请求按备份自洽要求
// 携带批准信息。
func buildMaxQuotaBackup(t *testing.T, t0 time.Time, threshold, available int64) []byte {
	t.Helper()
	const spent = math.MaxInt64 - 1
	r := requestBackupV1{
		PolicyID: "p-big", RequestID: "settled", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee: math.MaxInt64, ActualFee: spent,
		State:      int(RequestSettled),
		CreatedAt:  timeJSON(t0.Add(-2 * time.Hour)),
		ReservedAt: timeJSON(t0.Add(-2 * time.Hour)),
		SettledAt:  timeJSON(t0.Add(-time.Hour)),
	}
	// 开启审批时该笔费用严格超过门槛，备份自洽要求带批准人、决定时刻与
	// 等待截止时刻（提交于 t0-2h，等待 1h，故截止 t0-1h，批准于提交时刻）。
	if threshold > 0 {
		r.ApproverAccountID = "payer"
		r.DecidedAt = r.ReservedAt
		r.WaitDeadline = timeJSON(time.Time(r.CreatedAt).Add(time.Hour))
	}
	b := backupV1{
		Version: backupVersion,
		// ExportedAt 在校验中不使用，给出 t0 保持可读。
		ExportedAt: timeJSON(t0),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: available, Reserved: 0, CreatedAt: timeJSON(t0)},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)},
			{ID: "u2", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)},
		},
		Sessions: []sessionBackupV1{
			{ID: "s1", AccountID: "u1", DeviceID: "d1", ExpiresAt: timeJSON(t0.Add(24 * time.Hour)), CreatedAt: timeJSON(t0)},
			{ID: "s2", AccountID: "u2", DeviceID: "d2", ExpiresAt: timeJSON(t0.Add(24 * time.Hour)), CreatedAt: timeJSON(t0)},
		},
		Policies: []policyBackupV1{{
			ID: "p-big", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1", "u2"},
			Operation: "charge", Payee: "shop",
			StartsAt: timeJSON(t0.Add(-time.Hour)), EndsAt: timeJSON(t0.Add(24 * time.Hour)),
			MaxPerRequest:     math.MaxInt64,
			MaxTotal:          math.MaxInt64,
			ApprovalThreshold: threshold,
			ApprovalWait:      durationJSON(time.Hour),
			SpentTotal:        spent,
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

// maxQuotaApply 构造一笔针对 p-big 策略的申请。
func maxQuotaApply(account, session, device, rid string, fee int64) RequestInput {
	return RequestInput{
		PolicyID:     "p-big",
		RequestID:    rid,
		AccountID:    account,
		SessionID:    session,
		DeviceID:     device,
		Operation:    "charge",
		Payee:        "shop",
		EstimatedFee: fee,
	}
}

// TestApplySharedQuotaOverflowRejected 验证题述场景：累计上限为 int64 最大
// 值、已花费 MaxInt64-1、无现存预留、出资账户可用余额为 10 时，新申请预估
// 费用 2 必须按超额度拒绝（不能因求和越过 int64 上限被当成额度充足），费用
// 1 恰好把合计推到上限，应正常受理。
func TestApplySharedQuotaOverflowRejected(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildMaxQuotaBackup(t, t0, 0, 10)
	w, err := restoreAt(data, t0)
	if err != nil {
		t.Fatalf("valid at-boundary backup must restore: %v", err)
	}

	// 费用 2：used=MaxInt64-1，合计越过 int64 上限 → 超额度拒绝。
	in := maxQuotaApply("u1", "s1", "d1", "over", 2)
	if _, err := w.Apply(in); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("apply fee 2 err = %v, want ErrQuotaExceeded", err)
	}
	// 不创建请求。
	if _, err := w.Request("u1", "over"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("rejected apply created request: %v", err)
	}
	// 不扣减可用余额、不新增预留。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 10, Reserved: 0}) {
		t.Fatalf("balance after rejected apply = %+v, want {10 0}", bal)
	}
	pv, _ := w.Policy("p-big")
	if pv.ReservedTotal != 0 || pv.SpentTotal != math.MaxInt64-1 {
		t.Fatalf("policy totals after rejected apply = reserved %d spent %d", pv.ReservedTotal, pv.SpentTotal)
	}
	// 按现有申请拒绝规则留下一条带原因的拒绝记录，且无资金变动。
	led := w.Ledger()
	if len(led) != 1 || led[0].Kind != LedgerRejection || led[0].RequestID != "over" || led[0].Amount != 0 {
		t.Fatalf("ledger after rejected apply = %+v", led)
	}
	if led[0].Reason == "" {
		t.Fatal("rejection reason must not be empty")
	}
	if len(w.AccountLedger("payer")) != 0 {
		t.Fatalf("rejected apply must not move payer money: %+v", w.AccountLedger("payer"))
	}

	// 费用 1：合计恰好等于 MaxInt64 上限，正常受理并预留。
	fit := maxQuotaApply("u1", "s1", "d1", "fit", 1)
	req, err := w.Apply(fit)
	if err != nil {
		t.Fatalf("apply fee 1 at exact boundary: %v", err)
	}
	if req.State != RequestReserved {
		t.Fatalf("fee 1 state = %v, want reserved", req.State)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 9, Reserved: 1}) {
		t.Fatalf("balance after accepted apply = %+v, want {9 1}", bal)
	}
	pv, _ = w.Policy("p-big")
	if pv.ReservedTotal != 1 || pv.SpentTotal != math.MaxInt64-1 {
		t.Fatalf("policy totals after accepted apply = %+v", pv)
	}
}

// TestApplyOverflowRejectedBeforePending 验证进入待审批之前同样执行溢出安全
// 的累计额度判断：超过门槛、本应进入待审批的申请在超额时直接被拒绝，不留下
// 待审批请求或待审批留痕；恰好不超额的费用正常受理。
func TestApplyOverflowRejectedBeforePending(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildMaxQuotaBackup(t, t0, 1, 10)
	w, err := restoreAt(data, t0)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	// 费用 2 严格超过门槛 1，本应待审批；但合计越过 int64 上限，必须在进入
	// 待审批前以超额度拒绝。
	in := maxQuotaApply("u1", "s1", "d1", "over-pending", 2)
	if _, err := w.Apply(in); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("apply err = %v, want ErrQuotaExceeded", err)
	}
	if _, err := w.Request("u1", "over-pending"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("rejected apply created pending request: %v", err)
	}
	for _, e := range w.Ledger() {
		if e.Kind == LedgerPendingApproval {
			t.Fatalf("over-quota apply must not leave a pending-approval entry: %+v", e)
		}
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 10, Reserved: 0}) {
		t.Fatalf("balance changed = %+v", bal)
	}

	// 费用 1 不严格超过门槛，直接预留且合计恰好等于上限。
	fit := maxQuotaApply("u1", "s1", "d1", "fit", 1)
	req, err := w.Apply(fit)
	if err != nil {
		t.Fatalf("apply fee 1: %v", err)
	}
	if req.State != RequestReserved {
		t.Fatalf("fee 1 state = %v, want reserved", req.State)
	}
}

// TestApplySharedQuotaOverflowAcrossAccounts 验证多个使用账户共用同一累计
// 额度：u1 已花费 MaxInt64-1 后，u2 的新申请费用 2 同样必须被拒绝。
func TestApplySharedQuotaOverflowAcrossAccounts(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildMaxQuotaBackup(t, t0, 0, 10)
	w, err := restoreAt(data, t0)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	in := maxQuotaApply("u2", "s2", "d2", "over-by-u2", 2)
	if _, err := w.Apply(in); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("u2 apply err = %v, want ErrQuotaExceeded", err)
	}
	if _, err := w.Request("u2", "over-by-u2"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("rejected apply created request: %v", err)
	}
}

// buildApproveOverflowBackup 构造批准溢出场景的自洽备份：
//   - 策略 p-big 单次/累计上限均为 MaxInt64，门槛 1、等待 1 小时，
//     最长预留时长 reserveDur（零表示关闭预留超时）；
//   - u1 一笔已结算实际费用 1（已花费 1，直接受理路径）；
//   - u2 一笔现存预留 big，预估 MaxInt64-2（批准路径，带批准信息），
//     预留时刻 t0、截止 t0+reserveDur；
//   - u1 一笔待审批 small，预估费用 2，提交于 t0，等待截止 t0+1h；
//   - 出资账户 payer 可用余额 2、预留余额 MaxInt64-2（可用+预留恰为
//     MaxInt64，备份自洽），现存预留与已花费合计 MaxInt64-1：再计入
//     small 的 2 会越过 int64 上限；
//   - 另有出资账户会话 sa（设备 dev-approve）供批准使用。
func buildApproveOverflowBackup(t *testing.T, t0 time.Time, reserveDur time.Duration) []byte {
	t.Helper()
	const (
		spentActual = 1
		bigFee      = math.MaxInt64 - 2
		smallFee    = 2
	)
	settled := requestBackupV1{
		PolicyID: "p-big", RequestID: "settled", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee: spentActual, ActualFee: spentActual,
		State:      int(RequestSettled),
		CreatedAt:  timeJSON(t0.Add(-2 * time.Hour)),
		ReservedAt: timeJSON(t0.Add(-2 * time.Hour)),
		SettledAt:  timeJSON(t0.Add(-time.Hour)),
	}
	big := requestBackupV1{
		PolicyID: "p-big", RequestID: "big", AccountID: "u2", PayerAccountID: "payer",
		SessionID: "s2", Operation: "charge", Payee: "shop",
		EstimatedFee:      bigFee,
		State:             int(RequestReserved),
		CreatedAt:         timeJSON(t0),
		WaitDeadline:      timeJSON(t0.Add(time.Hour)),
		ReservedAt:        timeJSON(t0),
		ReserveDuration:   durationJSON(reserveDur),
		ApproverAccountID: "payer",
		DecidedAt:         timeJSON(t0),
	}
	if reserveDur > 0 {
		big.ReserveDeadline = timeJSON(t0.Add(reserveDur))
	}
	small := requestBackupV1{
		PolicyID: "p-big", RequestID: "small", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee: smallFee,
		State:        int(RequestPendingApproval),
		CreatedAt:    timeJSON(t0),
		WaitDeadline: timeJSON(t0.Add(time.Hour)),
	}
	// 已结算请求的预留时长快照必须与策略一致（备份自洽要求）。
	settled.ReserveDuration = durationJSON(reserveDur)
	if reserveDur > 0 {
		settled.ReserveDeadline = timeJSON(t0.Add(-2 * time.Hour).Add(reserveDur))
	}
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: 2, Reserved: bigFee, CreatedAt: timeJSON(t0)},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)},
			{ID: "u2", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)},
		},
		Sessions: []sessionBackupV1{
			{ID: "s1", AccountID: "u1", DeviceID: "d1", ExpiresAt: timeJSON(t0.Add(24 * time.Hour)), CreatedAt: timeJSON(t0)},
			{ID: "s2", AccountID: "u2", DeviceID: "d2", ExpiresAt: timeJSON(t0.Add(24 * time.Hour)), CreatedAt: timeJSON(t0)},
			{ID: "sa", AccountID: "payer", DeviceID: "dev-approve", ExpiresAt: timeJSON(t0.Add(24 * time.Hour)), CreatedAt: timeJSON(t0)},
		},
		Policies: []policyBackupV1{{
			ID: "p-big", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1", "u2"},
			Operation: "charge", Payee: "shop",
			StartsAt: timeJSON(t0.Add(-time.Hour)), EndsAt: timeJSON(t0.Add(24 * time.Hour)),
			MaxPerRequest:      math.MaxInt64,
			MaxTotal:           math.MaxInt64,
			ApprovalThreshold:  1,
			ApprovalWait:       durationJSON(time.Hour),
			MaxReserveDuration: durationJSON(reserveDur),
			ReservedTotal:      bigFee,
			SpentTotal:         spentActual,
		}},
		Requests: []requestBackupV1{settled, big, small},
		Ledger:   []ledgerEntryBackupV1{},
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// approveSmall 以出资账户会话 sa 批准 u1 的待审批请求 small。
func approveSmall(w *Wallet) (RequestView, error) {
	return w.Approve("u1", "small", "sa", "dev-approve")
}

// TestApproveSharedQuotaOverflowKeepsPending 验证待审批请求批准前重新执行
// 溢出安全的累计额度判断：其他请求已占用额度使本次批准越过 int64 上限时，
// 批准返回 ErrQuotaExceeded，请求继续待审批，提交时间、等待截止时间与决定
// 信息不变，不开始预留计时、不新增预留或批准记录；取消释放额度后期限内仍
// 可再次批准。
func TestApproveSharedQuotaOverflowKeepsPending(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildApproveOverflowBackup(t, t0, 0)
	w, err := restoreAt(data, t0)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	before, err := w.Request("u1", "small")
	if err != nil {
		t.Fatal(err)
	}
	if before.State != RequestPendingApproval {
		t.Fatalf("setup state = %v, want pending", before.State)
	}
	// 余额恰好够（2），但额度合计 MaxInt64-1 再加 2 越过 int64 上限。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 2, Reserved: math.MaxInt64 - 2}) {
		t.Fatalf("setup balance = %+v", bal)
	}

	ledgerBefore := len(w.Ledger())
	if _, err := approveSmall(w); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("approve err = %v, want ErrQuotaExceeded", err)
	}

	// 请求继续待审批，提交时间、等待截止时间与已有决定信息保持不变。
	req, _ := w.Request("u1", "small")
	if req.State != RequestPendingApproval {
		t.Fatalf("state = %v, want still pending approval", req.State)
	}
	if !req.CreatedAt.Equal(before.CreatedAt) || !req.WaitDeadline.Equal(before.WaitDeadline) {
		t.Fatalf("timing changed: created %v->%v deadline %v->%v",
			before.CreatedAt, req.CreatedAt, before.WaitDeadline, req.WaitDeadline)
	}
	if !req.DecidedAt.Equal(time.Time{}) || req.ApproverAccountID != "" || req.RejectReason != "" {
		t.Fatalf("decision fields changed on failed approve: %+v", req)
	}
	// 不开始预留计时。
	if !req.ReservedAt.Equal(time.Time{}) || req.ReserveDuration != 0 || !req.ReserveDeadline.Equal(time.Time{}) {
		t.Fatalf("reserve timing started on failed approve: %+v", req)
	}
	// 不新增预留或批准记录，余额与额度不变。
	if got := len(w.Ledger()); got != ledgerBefore {
		t.Fatalf("ledger grew by %d on failed approve", got-ledgerBefore)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 2, Reserved: math.MaxInt64 - 2}) {
		t.Fatalf("balance changed after failed approve: %+v", bal)
	}
	pv, _ := w.Policy("p-big")
	if pv.ReservedTotal != math.MaxInt64-2 || pv.SpentTotal != 1 {
		t.Fatalf("policy totals changed after failed approve: %+v", pv)
	}

	// 期限内额度未释放时再次批准仍被拒绝。
	if _, err := approveSmall(w); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("second approve err = %v, want ErrQuotaExceeded", err)
	}

	// 取消 u2 的大额预留，释放 MaxInt64-2 共享额度并全额退回余额。
	if _, err := w.Cancel("u2", "big"); err != nil {
		t.Fatal(err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: math.MaxInt64, Reserved: 0}) {
		t.Fatalf("balance after cancel big = %+v", bal)
	}
	// 仍在等待期限内（时钟停在 t0，截止 t0+1h），释放后批准成功并预留 2。
	req, err = approveSmall(w)
	if err != nil {
		t.Fatalf("approve after quota freed: %v", err)
	}
	if req.State != RequestReserved {
		t.Fatalf("state = %v, want reserved", req.State)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: math.MaxInt64 - 2, Reserved: 2}) {
		t.Fatalf("balance after approve small = %+v", bal)
	}
	pv, _ = w.Policy("p-big")
	if pv.ReservedTotal != 2 || pv.SpentTotal != 1 {
		t.Fatalf("policy totals after approve small = %+v", pv)
	}
}

// TestApproveOverflowReleasedByReservationTimeout 验证批准前的额度检查会计入
// 按现有规则已经到期的预留释放：大额预留超时自动全额退回、释放额度后，期限
// 内的待审批请求可以成功批准；释放之前仍按超额度拒绝。
func TestApproveOverflowReleasedByReservationTimeout(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildApproveOverflowBackup(t, t0, time.Minute)

	// 恢复于 t0：大额预留截止 t0+1m 尚未到期，批准 small 仍超额度。
	w, err := restoreAt(data, t0)
	if err != nil {
		t.Fatalf("restore at t0: %v", err)
	}
	if _, err := approveSmall(w); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("approve before timeout release err = %v, want ErrQuotaExceeded", err)
	}
	if r, _ := w.Request("u1", "small"); r.State != RequestPendingApproval {
		t.Fatalf("small state = %v, want still pending", r.State)
	}

	// 恢复于 t0+2m：大额预留已过截止时刻，恢复时自动全额退回并释放额度；
	// small 的等待截止 t0+1h 尚未到，仍可批准。
	w2, err := restoreAt(data, t0.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("restore after deadline: %v", err)
	}
	if r, _ := w2.Request("u2", "big"); r.State != RequestReservationExpired {
		t.Fatalf("big state = %v, want reservation expired", r.State)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: math.MaxInt64, Reserved: 0}) {
		t.Fatalf("balance after timeout = %+v, want full refund", bal)
	}
	req, err := approveSmall(w2)
	if err != nil {
		t.Fatalf("approve after timeout release: %v", err)
	}
	if req.State != RequestReserved {
		t.Fatalf("state = %v, want reserved", req.State)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: math.MaxInt64 - 2, Reserved: 2}) {
		t.Fatalf("balance after approve small = %+v", bal)
	}
	pv, _ := w2.Policy("p-big")
	if pv.ReservedTotal != 2 || pv.SpentTotal != 1 {
		t.Fatalf("policy totals = %+v", pv)
	}
}

// TestApplyExactMaxInt64FillsQuota 验证单笔费用与上限均为 int64 最大值时
// 仍可正常受理（不缩小金额范围），占满后任何新费用都按超额度拒绝。
func TestApplyExactMaxInt64FillsQuota(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", math.MaxInt64)
	mustAccount(t, w, "u1", 0)
	if _, err := w.CreateSession("s1", "u1", "d1", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 关闭审批：费用 MaxInt64 直接预留。
	if err := w.SavePolicy(PolicySpec{
		ID:                "p-direct",
		PayerAccountID:    "payer",
		AllowedAccountIDs: []string{"u1"},
		Operation:         "charge",
		Payee:             "shop",
		StartsAt:          c.t.Add(-time.Hour),
		EndsAt:            c.t.Add(24 * time.Hour),
		MaxPerRequest:     math.MaxInt64,
		MaxTotal:          math.MaxInt64,
	}); err != nil {
		t.Fatal(err)
	}
	in := RequestInput{
		PolicyID: "p-direct", RequestID: "full", AccountID: "u1",
		SessionID: "s1", DeviceID: "d1", Operation: "charge", Payee: "shop",
		EstimatedFee: math.MaxInt64,
	}
	req, err := w.Apply(in)
	if err != nil {
		t.Fatalf("apply MaxInt64 fee: %v", err)
	}
	if req.State != RequestReserved {
		t.Fatalf("state = %v, want reserved", req.State)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 0, Reserved: math.MaxInt64}) {
		t.Fatalf("balance = %+v, want {0 MaxInt64}", bal)
	}
	// 额度已占满：再申请费用 1 必须超额度拒绝（used+1 越过 int64 上限；
	// 余额虽同样不足，但额度判断在前，仍返回 ErrQuotaExceeded）。
	in.RequestID = "one-more"
	in.EstimatedFee = 1
	if _, err := w.Apply(in); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("apply after quota full err = %v, want ErrQuotaExceeded", err)
	}
}
