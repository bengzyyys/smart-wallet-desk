package wallet

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// 本文件覆盖“一次查询同时发现多笔预留到期”的批量释放回归：
//
//   - 同一出资账户经多条策略（最长预留时长不同）代付多个使用账户的请求，
//     提交次序与截止次序不同；
//   - 新增超时记录必须先按各请求的预留截止时刻从早到晚，同截止时刻先按
//     使用账户编号、再按该账户下请求编号排列；同号请求分属不同账户不得
//     被合并、遗漏或互相替代；
//   - 每笔请求的两条记录紧挨出现：先出资账户全额退款，再使用账户零金额
//     超时记录，均带请求编号与原因，记录时间是各请求原来的截止时刻，
//     而不是很久之后的查询时刻；
//   - 查询前的历史记录保持原位置与内容，新增记录一律追加在末尾；
//   - 到期恰在截止时刻发生；截止时刻晚于查询时刻的请求继续预留，仍占用
//     余额与策略额度，不提前出现退款或超时记录；
//   - 多笔退款合计与查询前后的余额、策略预留总额变化对得上，已花费总额
//     不增加。

// batchFingerprint 只保留核对账本次序所需的字段，便于整段比对。
type batchFingerprint struct {
	kind      LedgerKind
	accountID string
	requestID string
	amount    int64
	at        time.Time
}

func fingerprintLedger(entries []LedgerEntry) []batchFingerprint {
	out := make([]batchFingerprint, len(entries))
	for i, e := range entries {
		out[i] = batchFingerprint{
			kind:      e.Kind,
			accountID: e.AccountID,
			requestID: e.RequestID,
			amount:    e.Amount,
			at:        e.At,
		}
	}
	return out
}

// rawLedger 在不触发任何到期处理的前提下复制内部账本，用于核对“查询触发
// 批量释放之前”的历史记录原貌（所有公开查询入口都会先释放到期预留）。
func rawLedger(w *Wallet) []LedgerEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]LedgerEntry(nil), w.ledger...)
}

// batchExpiredRequest 描述一笔预期在某截止时刻超时的请求。
type batchExpiredRequest struct {
	accountID string
	requestID string
	fee       int64
	deadline  time.Time
}

// timeoutPair 按“先出资账户全额退款、再使用账户零金额超时记录”的顺序，
// 返回某笔超时请求应紧挨追加的两条账本指纹。
func timeoutPair(payerID string, x batchExpiredRequest) []batchFingerprint {
	return []batchFingerprint{
		{kind: LedgerRefund, accountID: payerID, requestID: x.requestID, amount: x.fee, at: x.deadline},
		{kind: LedgerReservationExpiration, accountID: x.accountID, requestID: x.requestID, amount: 0, at: x.deadline},
	}
}

// setupBatchTimeout 构造同一出资账户 payer 下两条不同最长预留时长的策略：
// p-a 为 60 秒、p-b 为 30 秒，均允许使用账户 u1/u2；另备长有效期会话，
// 避免会话到期干扰预留超时本身。返回钱包、可控时钟与基准时刻。
func setupBatchTimeout(t *testing.T) (*Wallet, *clock, time.Time) {
	t.Helper()
	w, c := newTestWallet()
	t0 := c.t
	mustAccount(t, w, "payer", 1000)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", t0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s2", "u2", "dev2", t0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	save := func(id string, maxReserve time.Duration) {
		t.Helper()
		p := PolicySpec{
			ID:                 id,
			PayerAccountID:     "payer",
			AllowedAccountIDs:  []string{"u1", "u2"},
			Operation:          "charge",
			Payee:              "shop",
			StartsAt:           t0.Add(-time.Hour),
			EndsAt:             t0.Add(time.Hour),
			MaxPerRequest:      100,
			MaxTotal:           1000,
			MaxReserveDuration: maxReserve,
		}
		if err := w.SavePolicy(p); err != nil {
			t.Fatal(err)
		}
	}
	save("p-a", time.Minute)
	save("p-b", 30*time.Second)
	return w, c, t0
}

func batchApply(policyID, accountID, sessionID, deviceID, requestID string, fee int64) RequestInput {
	return RequestInput{
		PolicyID:     policyID,
		RequestID:    requestID,
		AccountID:    accountID,
		SessionID:    sessionID,
		DeviceID:     deviceID,
		Operation:    "charge",
		Payee:        "shop",
		EstimatedFee: fee,
	}
}

// batchRequests 返回五笔申请，提交次序刻意与截止次序不同，且同一请求编号
// r1/r2 分别属于 u1、u2 两个使用账户：
//
//	提交次序              所属账户/策略         费用   截止时刻
//	(u2,r1) -> p-a 60s    u2                   20    t0+60s
//	(u1,r2) -> p-a 60s    u1                   30    t0+60s
//	(u2,r3) -> p-a 60s    u2                   50    t0+60s
//	(u2,r2) -> p-b 30s    u2                   40    t0+30s
//	(u1,r1) -> p-b 30s    u1                   10    t0+30s
//
// 因而截止次序（同截止时刻先使用账户、再请求编号）应为：
// (u1,r1)@30、(u2,r2)@30、(u1,r2)@60、(u2,r1)@60、(u2,r3)@60。
func batchRequests(t0 time.Time) []struct {
	in       RequestInput
	account  string
	fee      int64
	deadline time.Time
} {
	return []struct {
		in       RequestInput
		account  string
		fee      int64
		deadline time.Time
	}{
		{batchApply("p-a", "u2", "s2", "dev2", "r1", 20), "u2", 20, t0.Add(time.Minute)},
		{batchApply("p-a", "u1", "s1", "dev1", "r2", 30), "u1", 30, t0.Add(time.Minute)},
		{batchApply("p-a", "u2", "s2", "dev2", "r3", 50), "u2", 50, t0.Add(time.Minute)},
		{batchApply("p-b", "u2", "s2", "dev2", "r2", 40), "u2", 40, t0.Add(30 * time.Second)},
		{batchApply("p-b", "u1", "s1", "dev1", "r1", 10), "u1", 10, t0.Add(30 * time.Second)},
	}
}

// batchExpectedOrder 返回批量释放后追加记录应遵循的请求次序。
func batchExpectedOrder(t0 time.Time) []batchExpiredRequest {
	return []batchExpiredRequest{
		{accountID: "u1", requestID: "r1", fee: 10, deadline: t0.Add(30 * time.Second)},
		{accountID: "u2", requestID: "r2", fee: 40, deadline: t0.Add(30 * time.Second)},
		{accountID: "u1", requestID: "r2", fee: 30, deadline: t0.Add(time.Minute)},
		{accountID: "u2", requestID: "r1", fee: 20, deadline: t0.Add(time.Minute)},
		{accountID: "u2", requestID: "r3", fee: 50, deadline: t0.Add(time.Minute)},
	}
}

// TestMultipleReservationTimeoutsBatchLedgerOrder 是核心回归：很久之后一次
// Ledger 查询同时发现五笔到期预留，账本必须以确定次序说明每笔费用去向。
// 多轮重建钱包以覆盖内部 map 遍历次序的随机性，任何一轮次序漂移都会失败。
func TestMultipleReservationTimeoutsBatchLedgerOrder(t *testing.T) {
	for iter := 0; iter < 25; iter++ {
		w, c, t0 := setupBatchTimeout(t)

		// 历史记录 1：t0 时刻一条设备不符的拒绝（使用账户 u1），它与资金无关，
		// 之后不得被挪动或改写。
		bad := batchApply("p-a", "u1", "s1", "dev1", "bad", 10)
		bad.DeviceID = "wrong-device"
		if _, err := w.Apply(bad); !errors.Is(err, ErrSessionDeviceMismatch) {
			t.Fatalf("iter %d: seeded rejection err = %v, want ErrSessionDeviceMismatch", iter, err)
		}

		// 按刻意打乱的次序提交五笔，全部直接预留。
		for _, tc := range batchRequests(t0) {
			view, err := w.Apply(tc.in)
			if err != nil {
				t.Fatalf("iter %d: apply %+v: %v", iter, tc.in, err)
			}
			if view.State != RequestReserved {
				t.Fatalf("iter %d: state = %v, want reserved", iter, view.State)
			}
		}

		// 历史记录 2：另一条开启审批的策略下，u1 用独立会话 s3 提交一笔待
		// 审批请求（不冻结费用），随后在 t0+45s 吊销 s3，产生一条 t0+45s 的
		// 拒绝记录。吊销会话不会触发预留超时处理，因此此刻五笔预留仍保持
		// 预留。该拒绝的记录时间晚于稍后追加的第一批超时记录（t0+30s）：
		// 若实现错误地把整个旧账本按时间重排，它的位置就会移动。
		if _, err := w.CreateSession("s3", "u1", "dev3", t0.Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		approval := PolicySpec{
			ID:                 "p-c",
			PayerAccountID:     "payer",
			AllowedAccountIDs:  []string{"u1"},
			Operation:          "charge",
			Payee:              "shop",
			StartsAt:           t0.Add(-time.Hour),
			EndsAt:             t0.Add(2 * time.Hour),
			MaxPerRequest:      100,
			MaxTotal:           1000,
			ApprovalThreshold:  5,
			ApprovalWait:       time.Hour,
			MaxReserveDuration: time.Minute,
		}
		if err := w.SavePolicy(approval); err != nil {
			t.Fatal(err)
		}
		pendingIn := batchApply("p-c", "u1", "s3", "dev3", "pending1", 10)
		if pv, err := w.Apply(pendingIn); err != nil || pv.State != RequestPendingApproval {
			t.Fatalf("iter %d: pending apply = %+v %v", iter, pv, err)
		}
		c.t = t0.Add(45 * time.Second)
		if err := w.RevokeSession("s3"); err != nil {
			t.Fatal(err)
		}

		// 查询前快照（不触发到期处理）：五笔超时预留共冻结 150，待审批不
		// 占款；历史账本包含 1 条拒绝、5 条预留、1 条待审批、1 条吊销拒绝。
		// 注意：Balance 等公开入口都会触发到期处理，因此这里直接核对内部余额。
		w.mu.Lock()
		rawPayer := w.accounts["payer"]
		preAvailable, preReserved := rawPayer.available, rawPayer.reserved
		w.mu.Unlock()
		if preAvailable != 850 || preReserved != 150 {
			t.Fatalf("iter %d: internal balance before query = available %d reserved %d, want 850/150",
				iter, preAvailable, preReserved)
		}
		oldLedger := rawLedger(w)
		if len(oldLedger) != 8 {
			t.Fatalf("iter %d: history entries = %d, want 8", iter, len(oldLedger))
		}

		// 很久之后才查询：所有新增记录时间仍必须是各请求自己的截止时刻。
		queryAt := t0.Add(2 * time.Hour)
		c.t = queryAt
		got := w.Ledger()

		// 历史记录保持原来的位置与内容：前缀与查询前逐条一致，新记录只能
		// 追加在末尾，绝不为了时间次序重整旧账本。
		if len(got) < len(oldLedger) {
			t.Fatalf("iter %d: ledger shrank: %d < %d", iter, len(got), len(oldLedger))
		}
		if !reflect.DeepEqual(got[:len(oldLedger)], oldLedger) {
			t.Fatalf("iter %d: pre-existing ledger entries were reordered or rewritten:\nold %+v\nnow %+v",
				iter, oldLedger, got[:len(oldLedger)])
		}

		wantOrder := batchExpectedOrder(t0)
		tail := got[len(oldLedger):]
		if len(tail) != 2*len(wantOrder) {
			t.Fatalf("iter %d: appended entries = %d, want %d", iter, len(tail), 2*len(wantOrder))
		}
		// 逐笔核对：两条记录紧挨出现，先出资账户全额退款，再使用账户零金额
		// 超时记录；同号请求分属不同账户在这里靠账户与相邻配对区分，不能合并。
		var refundSum int64
		for i, x := range wantOrder {
			refund, status := tail[2*i], tail[2*i+1]
			wantPair := timeoutPair("payer", x)
			if fp := fingerprintLedger([]LedgerEntry{refund, status}); !reflect.DeepEqual(fp, wantPair) {
				t.Fatalf("iter %d: pair for (%s,%s) = %+v, want %+v",
					iter, x.accountID, x.requestID, fp, wantPair)
			}
			if refund.Reason == "" || status.Reason == "" {
				t.Fatalf("iter %d: pair for (%s,%s) missing reason: %+v %+v",
					iter, x.accountID, x.requestID, refund, status)
			}
			if refund.At.Equal(queryAt) || status.At.Equal(queryAt) {
				t.Fatalf("iter %d: pair for (%s,%s) stamped at query time %v instead of deadline %v",
					iter, x.accountID, x.requestID, queryAt, x.deadline)
			}
			refundSum += refund.Amount
		}

		// 余额：五笔全额退款合计 150 全部回到可用余额。
		afterBal, _ := w.Balance("payer")
		if afterBal != (Balances{Available: 1000, Reserved: 0}) {
			t.Fatalf("iter %d: balance after query = %+v, want {1000 0}", iter, afterBal)
		}
		// 多笔退款合计与查询前后余额变化必须对得上。
		if refundSum != 150 ||
			afterBal.Available-preAvailable != refundSum ||
			preReserved-afterBal.Reserved != refundSum ||
			preAvailable+preReserved != afterBal.Available+afterBal.Reserved {
			t.Fatalf("iter %d: refund sum %d does not reconcile balances: before %d/%d after %+v",
				iter, refundSum, preAvailable, preReserved, afterBal)
		}

		// 策略预留总额按各请求费用减少，已花费总额一律不增加。
		for _, id := range []string{"p-a", "p-b"} {
			pv, err := w.Policy(id)
			if err != nil {
				t.Fatal(err)
			}
			if pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
				t.Fatalf("iter %d: policy %s totals = reserved %d spent %d, want 0/0",
					iter, id, pv.ReservedTotal, pv.SpentTotal)
			}
		}

		// 每笔请求进入预留超时终态：实际费用不增加，释放时间为原截止时刻。
		for _, x := range wantOrder {
			view, err := w.Request(x.accountID, x.requestID)
			if err != nil {
				t.Fatal(err)
			}
			if view.State != RequestReservationExpired || view.ActualFee != 0 {
				t.Fatalf("iter %d: request (%s,%s) = %+v, want reservation expired with zero actual fee",
					iter, x.accountID, x.requestID, view)
			}
			if !view.ReserveExpiredAt.Equal(x.deadline) || !view.ReserveDeadline.Equal(x.deadline) {
				t.Fatalf("iter %d: request (%s,%s) timing = deadline %v expired %v, want %v",
					iter, x.accountID, x.requestID, view.ReserveDeadline, view.ReserveExpiredAt, x.deadline)
			}
		}

		// 出资账户账本：五笔预留按提交次序在前，五笔退款按截止次序追加在后
		// （待审批、拒绝等状态记录不属于出资账户）。
		wantPayer := []batchFingerprint{
			{kind: LedgerReserve, accountID: "payer", requestID: "r1", amount: 20, at: t0},
			{kind: LedgerReserve, accountID: "payer", requestID: "r2", amount: 30, at: t0},
			{kind: LedgerReserve, accountID: "payer", requestID: "r3", amount: 50, at: t0},
			{kind: LedgerReserve, accountID: "payer", requestID: "r2", amount: 40, at: t0},
			{kind: LedgerReserve, accountID: "payer", requestID: "r1", amount: 10, at: t0},
		}
		for _, x := range wantOrder {
			wantPayer = append(wantPayer,
				batchFingerprint{kind: LedgerRefund, accountID: "payer", requestID: x.requestID, amount: x.fee, at: x.deadline})
		}
		if fp := fingerprintLedger(w.AccountLedger("payer")); !reflect.DeepEqual(fp, wantPayer) {
			t.Fatalf("iter %d: payer ledger = %+v\nwant %+v", iter, fp, wantPayer)
		}

		// 使用账户 u1：两条历史状态记录（t0 的拒绝、t0+45s 的吊销拒绝，中间
		// 还有一条 t0 的待审批记录）保持原位，超时记录按截止时刻追加在末尾。
		wantU1 := []batchFingerprint{
			{kind: LedgerRejection, accountID: "u1", requestID: "bad", amount: 0, at: t0},
			{kind: LedgerPendingApproval, accountID: "u1", requestID: "pending1", amount: 0, at: t0},
			{kind: LedgerRejection, accountID: "u1", requestID: "pending1", amount: 0, at: t0.Add(45 * time.Second)},
			{kind: LedgerReservationExpiration, accountID: "u1", requestID: "r1", amount: 0, at: t0.Add(30 * time.Second)},
			{kind: LedgerReservationExpiration, accountID: "u1", requestID: "r2", amount: 0, at: t0.Add(time.Minute)},
		}
		if fp := fingerprintLedger(w.AccountLedger("u1")); !reflect.DeepEqual(fp, wantU1) {
			t.Fatalf("iter %d: u1 ledger = %+v, want %+v", iter, fp, wantU1)
		}
		// 使用账户 u2：r2 截止更早，必须排在 r1 之前，不按请求编号或提交次序；
		// 同一账户下不同编号分别释放、互不替代。
		wantU2 := []batchFingerprint{
			{kind: LedgerReservationExpiration, accountID: "u2", requestID: "r2", amount: 0, at: t0.Add(30 * time.Second)},
			{kind: LedgerReservationExpiration, accountID: "u2", requestID: "r1", amount: 0, at: t0.Add(time.Minute)},
			{kind: LedgerReservationExpiration, accountID: "u2", requestID: "r3", amount: 0, at: t0.Add(time.Minute)},
		}
		if fp := fingerprintLedger(w.AccountLedger("u2")); !reflect.DeepEqual(fp, wantU2) {
			t.Fatalf("iter %d: u2 ledger = %+v, want %+v", iter, fp, wantU2)
		}
		// 使用账户账本不得混入任何资金变动记录。
		for _, id := range []string{"u1", "u2"} {
			for _, e := range w.AccountLedger(id) {
				if e.Kind == LedgerReserve || e.Kind == LedgerRefund || e.Kind == LedgerSettle {
					t.Fatalf("iter %d: usage account %s ledger carries money movement: %+v", iter, id, e)
				}
			}
		}

		// 再次查询不重复释放、不重复留痕。
		if again := w.Ledger(); len(again) != len(got) {
			t.Fatalf("iter %d: repeated query appended entries: %d vs %d", iter, len(again), len(got))
		}
	}
}

// TestBatchReservationTimeoutBoundaryReleasesDueKeepsFuture 覆盖批量释放的
// 时间边界：恰到较早截止时刻只释放该刻到期的两笔，截止时刻更晚的三笔继续
// 预留、继续占用余额与策略额度，账本不提前出现它们的任何记录；直到恰到较
// 晚截止时刻才按确定次序全部释放。
func TestBatchReservationTimeoutBoundaryReleasesDueKeepsFuture(t *testing.T) {
	w, c, t0 := setupBatchTimeout(t)

	for _, tc := range batchRequests(t0) {
		if _, err := w.Apply(tc.in); err != nil {
			t.Fatal(err)
		}
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 850, Reserved: 150}) {
		t.Fatalf("balance before deadline = %+v, want {850 150}", bal)
	}

	// 恰到较早截止时刻 t0+30s：经余额查询入口触发批量释放。
	c.t = t0.Add(30 * time.Second)
	bal30, err := w.Balance("payer")
	if err != nil {
		t.Fatal(err)
	}
	if bal30 != (Balances{Available: 900, Reserved: 100}) {
		t.Fatalf("balance at first deadline = %+v, want {900 100}", bal30)
	}

	led30 := w.Ledger()
	wantTail30 := []batchFingerprint{}
	for _, x := range []batchExpiredRequest{
		{accountID: "u1", requestID: "r1", fee: 10, deadline: t0.Add(30 * time.Second)},
		{accountID: "u2", requestID: "r2", fee: 40, deadline: t0.Add(30 * time.Second)},
	} {
		wantTail30 = append(wantTail30, timeoutPair("payer", x)...)
	}
	tail30 := led30[5:] // 前五条为提交时的预留记录
	if fp := fingerprintLedger(tail30); !reflect.DeepEqual(fp, wantTail30) {
		t.Fatalf("appended entries at first deadline = %+v, want %+v", fp, wantTail30)
	}

	// 截止时刻晚于查询时刻的三笔：继续预留、无终态时间。
	for _, x := range []batchExpiredRequest{
		{accountID: "u1", requestID: "r2", fee: 30, deadline: t0.Add(time.Minute)},
		{accountID: "u2", requestID: "r1", fee: 20, deadline: t0.Add(time.Minute)},
		{accountID: "u2", requestID: "r3", fee: 50, deadline: t0.Add(time.Minute)},
	} {
		view, err := w.Request(x.accountID, x.requestID)
		if err != nil {
			t.Fatal(err)
		}
		if view.State != RequestReserved || !view.ReserveExpiredAt.IsZero() {
			t.Fatalf("future request (%s,%s) released early: %+v", x.accountID, x.requestID, view)
		}
	}
	// 它们的金额仍占用策略额度：p-a 下三笔共 100；p-b 已全部释放。
	if pv, _ := w.Policy("p-a"); pv.ReservedTotal != 100 || pv.SpentTotal != 0 {
		t.Fatalf("p-a totals at first deadline = reserved %d spent %d, want 100/0", pv.ReservedTotal, pv.SpentTotal)
	}
	if pv, _ := w.Policy("p-b"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("p-b totals at first deadline = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}
	// 账本里不得提前出现未来请求的退款或超时记录：退款只有 10 与 40 两笔，
	// 超时状态记录只属于 (u1,r1) 与 (u2,r2)。
	if n := countKind(led30, LedgerRefund); n != 2 {
		t.Fatalf("refund entries at first deadline = %d, want 2", n)
	}
	for _, e := range led30 {
		if e.Kind != LedgerReservationExpiration {
			continue
		}
		if (e.AccountID == "u1" && e.RequestID == "r2") ||
			(e.AccountID == "u2" && (e.RequestID == "r1" || e.RequestID == "r3")) {
			t.Fatalf("future request got an early timeout record: %+v", e)
		}
	}
	for _, e := range led30 {
		if e.Kind == LedgerRefund && e.Amount != 10 && e.Amount != 40 {
			t.Fatalf("unexpected early refund of %d: %+v", e.Amount, e)
		}
	}

	// 截止前一刻 t0+59s：三笔仍预留，账本不增长。
	c.t = t0.Add(time.Minute - time.Second)
	if n := len(w.Ledger()); n != len(led30) {
		t.Fatalf("ledger grew one second before second deadline: %d vs %d", n, len(led30))
	}
	for _, key := range [][2]string{{"u1", "r2"}, {"u2", "r1"}, {"u2", "r3"}} {
		view, _ := w.Request(key[0], key[1])
		if view.State != RequestReserved {
			t.Fatalf("(%s,%s) released before its deadline: %v", key[0], key[1], view.State)
		}
	}

	// 恰到较晚截止时刻 t0+60s：剩余三笔一次性释放，同截止时刻先按使用
	// 账户（u1 的 r2 在前），u2 内再按请求编号（r1 在 r3 前）。
	c.t = t0.Add(time.Minute)
	led60 := w.Ledger()
	wantTail60 := []batchFingerprint{}
	for _, x := range []batchExpiredRequest{
		{accountID: "u1", requestID: "r2", fee: 30, deadline: t0.Add(time.Minute)},
		{accountID: "u2", requestID: "r1", fee: 20, deadline: t0.Add(time.Minute)},
		{accountID: "u2", requestID: "r3", fee: 50, deadline: t0.Add(time.Minute)},
	} {
		wantTail60 = append(wantTail60, timeoutPair("payer", x)...)
	}
	newTail := led60[len(led30):]
	if fp := fingerprintLedger(newTail); !reflect.DeepEqual(fp, wantTail60) {
		t.Fatalf("appended entries at second deadline = %+v, want %+v", fp, wantTail60)
	}
	// 第一批记录位置与内容不变。
	if !reflect.DeepEqual(led60[:len(led30)], led30) {
		t.Fatal("first batch entries were rewritten by the second batch")
	}

	// 全部释放：余额恢复，两条策略预留/已花费总额均为 0，实际费用不增加。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 1000, Reserved: 0}) {
		t.Fatalf("balance after all deadlines = %+v, want {1000 0}", bal)
	}
	for _, id := range []string{"p-a", "p-b"} {
		pv, _ := w.Policy(id)
		if pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
			t.Fatalf("policy %s totals = reserved %d spent %d, want 0/0", id, pv.ReservedTotal, pv.SpentTotal)
		}
	}
	for _, x := range batchExpectedOrder(t0) {
		view, _ := w.Request(x.accountID, x.requestID)
		if view.State != RequestReservationExpired || view.ActualFee != 0 ||
			!view.ReserveExpiredAt.Equal(x.deadline) {
			t.Fatalf("request (%s,%s) = %+v, want expired at %v with zero actual fee",
				x.accountID, x.requestID, view, x.deadline)
		}
	}

	// 五笔退款合计 150，恰为查询前后可用余额的全部增量。
	var refunds int64
	for _, e := range led60 {
		if e.Kind == LedgerRefund {
			refunds += e.Amount
		}
	}
	if refunds != 150 {
		t.Fatalf("total refunds = %d, want 150", refunds)
	}
	// 再次查询不追加任何记录。
	if again := w.Ledger(); len(again) != len(led60) {
		t.Fatalf("repeated query appended entries: %d vs %d", len(again), len(led60))
	}
}
