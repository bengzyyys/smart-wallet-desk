package wallet

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// multiTimeoutPolicy 构造一条出资账户 payer、使用账户 u1/u2、在整个测试时间窗
// 内有效且开启预留超时（最长预留时长 d）的策略。
func multiTimeoutPolicy(c *clock, id string, d time.Duration) PolicySpec {
	p := validPolicy(c)
	p.ID = id
	p.MaxPerRequest = 100
	p.MaxTotal = 100
	p.MaxReserveDuration = d
	return p
}

// setupMultiTimeout 构造多策略共用一个出资账户的钱包：payer（1000）、使用账户
// u1/u2 各自的长会话，以及出资账户用于审批的会话 sa。
func setupMultiTimeout(t *testing.T) (*Wallet, *clock) {
	t.Helper()
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 1000)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s2", "u2", "dev2", c.t.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "dev-approve", c.t.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return w, c
}

// applyMulti 以对应账户的会话提交一笔申请。
func applyMulti(t *testing.T, w *Wallet, policyID, account, requestID string, fee int64) {
	t.Helper()
	in := baseApply()
	in.PolicyID = policyID
	in.AccountID = account
	in.RequestID = requestID
	in.EstimatedFee = fee
	if account == "u2" {
		in.SessionID, in.DeviceID = "s2", "dev2"
	}
	if _, err := w.Apply(in); err != nil {
		t.Fatalf("apply %s/%s: %v", account, requestID, err)
	}
}

// 一次账本查询同时发现多笔到期预留：新增超时记录按截止时刻升序（同截止按
// 使用账户、请求编号升序）追加在旧账本末尾，每笔先出资账户全额退款、再使用
// 账户零金额超时记录，记录时间为各笔自己的截止时刻；未到期请求继续预留。
func TestMultiReservationTimeoutReleaseOrdering(t *testing.T) {
	w, c := setupMultiTimeout(t)
	save := func(p PolicySpec) {
		t.Helper()
		if err := w.SavePolicy(p); err != nil {
			t.Fatal(err)
		}
	}
	// 同一出资账户 payer 经不同策略代付，各策略最长预留时长不同。
	save(multiTimeoutPolicy(c, "pa", 10*time.Second))
	save(multiTimeoutPolicy(c, "pb", 30*time.Second))
	save(multiTimeoutPolicy(c, "pc", 20*time.Second))
	ap := multiTimeoutPolicy(c, "pd", 10*time.Second)
	ap.ApprovalThreshold = 10
	ap.ApprovalWait = 2 * time.Hour
	save(ap)

	t0 := c.t
	// 提交次序与截止次序刻意不同；r1/r2/r4 同号分属 u1 与 u2，不得合并。
	applyMulti(t, w, "pc", "u2", "r9", 7)  // 截止 t0+20
	applyMulti(t, w, "pa", "u1", "r1", 20) // 截止 t0+10
	applyMulti(t, w, "pd", "u1", "r7", 20) // 超门槛进入待审批，批准后才起算
	c.t = t0.Add(time.Second)
	applyMulti(t, w, "pb", "u2", "r1", 30) // 截止 t0+31
	c.t = t0.Add(2 * time.Second)
	applyMulti(t, w, "pc", "u1", "r2", 15) // 截止 t0+22
	c.t = t0.Add(3 * time.Second)
	applyMulti(t, w, "pa", "u2", "r2", 25) // 截止 t0+13
	c.t = t0.Add(4 * time.Second)
	applyMulti(t, w, "pb", "u1", "r3", 10) // 截止 t0+34：首轮查询时未到期
	c.t = t0.Add(5 * time.Second)
	applyMulti(t, w, "pa", "u2", "r4", 5) // 截止 t0+15
	applyMulti(t, w, "pa", "u1", "r5", 6) // 截止 t0+15
	applyMulti(t, w, "pa", "u1", "r4", 8) // 截止 t0+15
	c.t = t0.Add(6 * time.Second)
	if _, err := w.Approve("u1", "r7", "sa", "dev-approve"); err != nil {
		t.Fatalf("approve r7: %v", err)
	} // r7 自批准成功时刻起算：截止 t0+16，等待审批的时间不计入

	// 查询前：10 笔共预留 146；账本快照含 9 条预留、1 条待审批、
	// 1 条批准带来的预留与 1 条批准记录。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 854, Reserved: 146}) {
		t.Fatalf("balance before query = %+v, want {854 146}", bal)
	}
	before := w.Ledger()
	if len(before) != 12 {
		t.Fatalf("ledger before query = %d entries, want 12", len(before))
	}

	// 恰为 u2/r1 的截止时刻（到达截止时刻本身即释放）：一次查询发现 9 笔到期。
	c.t = t0.Add(31 * time.Second)
	got := w.Ledger()

	// 查询前已有的历史记录保持原位置与内容，新增记录只追加在末尾。
	if len(got) != len(before)+18 {
		t.Fatalf("ledger after query = %d entries, want %d", len(got), len(before)+18)
	}
	if !reflect.DeepEqual(got[:len(before)], before) {
		t.Fatal("existing ledger entries were rewritten or reordered")
	}

	// 新增超时记录按截止时刻升序，同截止按使用账户、请求编号升序；
	// 每笔先出资账户全额退款、紧接使用账户零金额超时记录，两条都关联请求
	// 编号并说明原因，记录时间为该笔自己的截止时刻而非查询时刻。
	want := []struct {
		at      time.Time
		account string
		request string
		amount  int64
	}{
		{t0.Add(10 * time.Second), "u1", "r1", 20},
		{t0.Add(13 * time.Second), "u2", "r2", 25},
		{t0.Add(15 * time.Second), "u1", "r4", 8},
		{t0.Add(15 * time.Second), "u1", "r5", 6},
		{t0.Add(15 * time.Second), "u2", "r4", 5},
		{t0.Add(16 * time.Second), "u1", "r7", 20},
		{t0.Add(20 * time.Second), "u2", "r9", 7},
		{t0.Add(22 * time.Second), "u1", "r2", 15},
		{t0.Add(31 * time.Second), "u2", "r1", 30},
	}
	var refundSum int64
	for i, e := range want {
		refund := got[len(before)+2*i]
		exp := got[len(before)+2*i+1]
		if refund.Kind != LedgerRefund || refund.AccountID != "payer" ||
			refund.RequestID != e.request || refund.Amount != e.amount ||
			!refund.At.Equal(e.at) || refund.Reason == "" {
			t.Fatalf("refund entry %d = %+v, want payer refund of %d for %s at %v",
				i, refund, e.amount, e.request, e.at)
		}
		if exp.Kind != LedgerReservationExpiration || exp.AccountID != e.account ||
			exp.RequestID != e.request || exp.Amount != 0 ||
			!exp.At.Equal(e.at) || exp.Reason == "" {
			t.Fatalf("expiration entry %d = %+v, want zero-amount timeout for %s/%s at %v",
				i, exp, e.account, e.request, e.at)
		}
		refundSum += refund.Amount
	}
	if refundSum != 136 {
		t.Fatalf("refund sum = %d, want 136", refundSum)
	}

	// 退款合计与查询前后的余额变化对上：854 + 136 = 990，未到期 r3 仍占 10。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 990, Reserved: 10}) {
		t.Fatalf("balance after query = %+v, want {990 10}", bal)
	}
	// 各策略预留总额减少同样金额，已花费总额不增加。
	for id, totals := range map[string][2]int64{
		"pa": {0, 0}, "pb": {10, 0}, "pc": {0, 0}, "pd": {0, 0},
	} {
		pv, err := w.Policy(id)
		if err != nil {
			t.Fatal(err)
		}
		if pv.ReservedTotal != totals[0] || pv.SpentTotal != totals[1] {
			t.Fatalf("policy %s totals = reserved %d spent %d, want %d/%d",
				id, pv.ReservedTotal, pv.SpentTotal, totals[0], totals[1])
		}
	}

	// 各请求（含同号不同账户）分别进入预留超时终态：释放时间为自己的截止
	// 时刻，实际费用为零。
	for _, e := range want {
		req, err := w.Request(e.account, e.request)
		if err != nil {
			t.Fatal(err)
		}
		if req.State != RequestReservationExpired ||
			!req.ReserveDeadline.Equal(e.at) || !req.ReserveExpiredAt.Equal(e.at) ||
			req.ActualFee != 0 {
			t.Fatalf("request %s/%s = %+v, want reservation expired at %v",
				e.account, e.request, req, e.at)
		}
	}

	// 截止时刻晚于查询时刻的 r3 继续预留：不产生退款或超时记录。
	req, _ := w.Request("u1", "r3")
	if req.State != RequestReserved || !req.ReserveExpiredAt.IsZero() {
		t.Fatalf("r3 = %+v, want still reserved", req)
	}
	for _, e := range got {
		if e.RequestID == "r3" && e.Kind != LedgerReserve {
			t.Fatalf("r3 produced early release entry: %+v", e)
		}
	}

	// 很久之后才再次查询：r3 的释放时间仍是它原来的截止时刻 t0+34 而非
	// 查询时刻，新记录继续追加在末尾，不重排旧账本。
	c.t = t0.Add(40 * time.Second)
	again := w.Ledger()
	if len(again) != len(got)+2 {
		t.Fatalf("ledger after second query = %d entries, want %d", len(again), len(got)+2)
	}
	if !reflect.DeepEqual(again[:len(got)], got) {
		t.Fatal("second query rewrote existing ledger entries")
	}
	tail := again[len(got):]
	if tail[0].Kind != LedgerRefund || tail[0].AccountID != "payer" ||
		tail[0].RequestID != "r3" || tail[0].Amount != 10 ||
		!tail[0].At.Equal(t0.Add(34*time.Second)) || tail[0].Reason == "" {
		t.Fatalf("r3 refund = %+v, want payer refund of 10 at t0+34s", tail[0])
	}
	if tail[1].Kind != LedgerReservationExpiration || tail[1].AccountID != "u1" ||
		tail[1].RequestID != "r3" || tail[1].Amount != 0 ||
		!tail[1].At.Equal(t0.Add(34*time.Second)) || tail[1].Reason == "" {
		t.Fatalf("r3 expiration = %+v, want zero-amount timeout at t0+34s", tail[1])
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 1000, Reserved: 0}) {
		t.Fatalf("final balance = %+v, want {1000 0}", bal)
	}
	if pv, _ := w.Policy("pb"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("pb totals = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}
}

// 同一请求编号分属不同使用账户、截止时刻相同：一次余额查询触发后两笔分别
// 释放、按使用账户升序留痕，互不合并或替代；未到期请求继续占用余额与策略
// 共享额度。
func TestMultiReservationTimeoutBalanceQuotaAndSameID(t *testing.T) {
	w, c := setupMultiTimeout(t)
	pa := multiTimeoutPolicy(c, "pa", 10*time.Second)
	pa.MaxPerRequest, pa.MaxTotal = 50, 50
	if err := w.SavePolicy(pa); err != nil {
		t.Fatal(err)
	}
	pb := multiTimeoutPolicy(c, "pb", time.Hour)
	pb.MaxPerRequest, pb.MaxTotal = 40, 40
	if err := w.SavePolicy(pb); err != nil {
		t.Fatal(err)
	}

	t0 := c.t
	applyMulti(t, w, "pa", "u2", "r1", 20) // 截止 t0+10
	applyMulti(t, w, "pa", "u1", "r1", 20) // 截止 t0+10：同号同截止，分属 u1/u2
	applyMulti(t, w, "pb", "u1", "r2", 30) // 截止 t0+1h：继续预留

	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 930, Reserved: 70}) {
		t.Fatalf("balance = %+v, want {930 70}", bal)
	}

	// 恰为两笔 r1 的截止时刻：到达截止时刻本身就释放；用余额查询触发，
	// 退款合计 40 与余额变化对上。
	c.t = t0.Add(10 * time.Second)
	bal, err := w.Balance("payer")
	if err != nil {
		t.Fatal(err)
	}
	if bal != (Balances{Available: 970, Reserved: 30}) {
		t.Fatalf("balance after expiry = %+v, want {970 30} (refund 40 = delta)", bal)
	}

	// 同截止时刻按使用账户升序：u1/r1 的两条记录在前，u2/r1 的在后，
	// 两笔同号请求各自成对出现，不被合并。
	ledger := w.Ledger()
	if len(ledger) != 7 {
		t.Fatalf("ledger = %d entries, want 7", len(ledger))
	}
	newEntries := ledger[3:]
	for i, acc := range []string{"u1", "u2"} {
		refund, exp := newEntries[2*i], newEntries[2*i+1]
		if refund.Kind != LedgerRefund || refund.AccountID != "payer" ||
			refund.RequestID != "r1" || refund.Amount != 20 ||
			!refund.At.Equal(t0.Add(10*time.Second)) || refund.Reason == "" {
			t.Fatalf("refund %d = %+v, want payer refund of 20 for r1 at deadline", i, refund)
		}
		if exp.Kind != LedgerReservationExpiration || exp.AccountID != acc ||
			exp.RequestID != "r1" || exp.Amount != 0 ||
			!exp.At.Equal(t0.Add(10*time.Second)) || exp.Reason == "" {
			t.Fatalf("expiration %d = %+v, want zero-amount timeout for %s/r1", i, exp, acc)
		}
	}
	for _, acc := range []string{"u1", "u2"} {
		req, _ := w.Request(acc, "r1")
		if req.State != RequestReservationExpired || req.ActualFee != 0 {
			t.Fatalf("%s/r1 = %+v, want reservation expired with zero actual fee", acc, req)
		}
	}

	// 未到期的 r2 继续预留：金额仍占用余额与策略共享额度（pb 已用 30/40）。
	if r2, _ := w.Request("u1", "r2"); r2.State != RequestReserved {
		t.Fatalf("r2 state = %v, want reserved", r2.State)
	}
	if pv, _ := w.Policy("pb"); pv.ReservedTotal != 30 {
		t.Fatalf("pb reserved = %d, want 30 (r2 still holds quota)", pv.ReservedTotal)
	}
	over := baseApply()
	over.PolicyID, over.RequestID, over.EstimatedFee = "pb", "r3", 11
	if _, err := w.Apply(over); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("apply over quota err = %v, want ErrQuotaExceeded", err)
	}
	// 额度内的申请正常受理。
	ok := baseApply()
	ok.PolicyID, ok.RequestID, ok.EstimatedFee = "pb", "r4", 10
	if _, err := w.Apply(ok); err != nil {
		t.Fatalf("apply within quota: %v", err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 960, Reserved: 40}) {
		t.Fatalf("final balance = %+v, want {960 40}", bal)
	}
}
