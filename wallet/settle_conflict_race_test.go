package wallet

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// settleRacePolicy 构造本文件共用的代付策略：出资账户 payer、使用账户 u1，
// 单次上限 80、共享累计上限 200，关闭审批与预留超时，窗口覆盖测试时钟。
func settleRacePolicy(c *clock) PolicySpec {
	return PolicySpec{
		ID:                "p-settle",
		PayerAccountID:    "payer",
		AllowedAccountIDs: []string{"u1"},
		Operation:         "charge",
		Payee:             "shop",
		StartsAt:          c.t.Add(-time.Hour),
		EndsAt:            c.t.Add(time.Hour),
		MaxPerRequest:     80,
		MaxTotal:          200,
	}
}

// setupSettleRace 构造并发结算夹具：出资账户 payer 可用 300，使用账户 u1
// 自带余额 50（代付结算不得动用它）。同一策略下 u1 提交两笔无需审批、未启用
// 预留超时的申请：r1 预估 80（本文件并发结算的对象）与 r2 预估 40（始终保持
// 已预留，用于验证另一笔预留不受干扰）。夹具就绪后出资账户为 180 可用 /
// 120 预留，策略预留总额 120、已花费 0。
func setupSettleRace(t *testing.T) (*Wallet, *clock) {
	t.Helper()
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 300)
	mustAccount(t, w, "u1", 50)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(settleRacePolicy(c)); err != nil {
		t.Fatal(err)
	}
	for _, in := range []RequestInput{
		{PolicyID: "p-settle", RequestID: "r1", AccountID: "u1", SessionID: "s1", DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 80},
		{PolicyID: "p-settle", RequestID: "r2", AccountID: "u1", SessionID: "s1", DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 40},
	} {
		req, err := w.Apply(in)
		if err != nil {
			t.Fatalf("apply %s: %v", in.RequestID, err)
		}
		if req.State != RequestReserved {
			t.Fatalf("apply %s state = %v, want reserved", in.RequestID, req.State)
		}
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 180, Reserved: 120}) {
		t.Fatalf("fixture balance = %+v, want {180 120}", bal)
	}
	return w, c
}

// countRequestKind 统计账本中指定请求编号、指定类型的记录条数。
func countRequestKind(entries []LedgerEntry, kind LedgerKind, requestID string) int {
	n := 0
	for _, e := range entries {
		if e.Kind == kind && e.RequestID == requestID {
			n++
		}
	}
	return n
}

// entriesForRequest 统计账本中关联指定请求编号的记录总条数。
func entriesForRequest(entries []LedgerEntry, requestID string) int {
	n := 0
	for _, e := range entries {
		if e.RequestID == requestID {
			n++
		}
	}
	return n
}

// checkSettledOutcome 校验 r1 以 winner 为最终实际费用结算后的完整结果：
//   - r1 已结算，实际费用与结算时间保持胜出回调写下的值，预估费用不变；
//   - r2 保持已预留，预估 40；
//   - 出资账户可用 300-40-winner、预留 40（r2 的预留原样保留）；使用账户
//     自身余额 50 不因代付结算减少；
//   - 策略预留总额 40（只剩 r2），已花费总额只计入胜出的实际费用；
//   - 账本对 r1 恰好有一条预留（80）、一条实际费用扣减（winner，归出资账户），
//     差额为正时恰好一条差额退款（归出资账户），差额为零（全额结算）时没有
//     退款记录；r2 只有原来的一条预留记录。
func checkSettledOutcome(t *testing.T, w *Wallet, settleAt time.Time, winner int64) {
	t.Helper()

	req, err := w.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if req.State != RequestSettled {
		t.Fatalf("r1 state = %v, want settled", req.State)
	}
	if req.ActualFee != winner {
		t.Fatalf("r1 actual fee = %d, want winning fee %d", req.ActualFee, winner)
	}
	if !req.SettledAt.Equal(settleAt) {
		t.Fatalf("r1 settled at %v, want first winning settle time %v", req.SettledAt, settleAt)
	}
	if req.EstimatedFee != 80 {
		t.Fatalf("r1 estimated fee = %d, want unchanged 80", req.EstimatedFee)
	}

	r2, err := w.Request("u1", "r2")
	if err != nil {
		t.Fatal(err)
	}
	if r2.State != RequestReserved || r2.EstimatedFee != 40 {
		t.Fatalf("r2 = state %v estimated %d, want reserved 40", r2.State, r2.EstimatedFee)
	}

	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 300 - 40 - winner, Reserved: 40}) {
		t.Fatalf("payer balance = %+v, want {%d 40}", bal, 300-40-winner)
	}
	if bal, _ := w.Balance("u1"); bal != (Balances{Available: 50, Reserved: 0}) {
		t.Fatalf("user balance = %+v, want {50 0}: payer-funded settle must not touch it", bal)
	}
	if pv, _ := w.Policy("p-settle"); pv.ReservedTotal != 40 || pv.SpentTotal != winner {
		t.Fatalf("policy totals = reserved %d spent %d, want 40/%d", pv.ReservedTotal, pv.SpentTotal, winner)
	}

	all := w.Ledger()
	refund := int64(80) - winner
	wantLen := 3 // r1 预留 + r2 预留 + r1 扣减
	if refund > 0 {
		wantLen++ // 差额为正时多一条退款
	}
	if len(all) != wantLen {
		t.Fatalf("ledger entries = %d, want %d: %v", len(all), wantLen, all)
	}
	if n := countRequestKind(all, LedgerReserve, "r1"); n != 1 {
		t.Fatalf("r1 reserve entries = %d, want 1", n)
	}
	if n := countRequestKind(all, LedgerSettle, "r1"); n != 1 {
		t.Fatalf("r1 settle entries = %d, want exactly 1: repeated callbacks must not double-charge", n)
	}
	settle, ok := ledgerEntryFor(all, LedgerSettle, "r1")
	if !ok {
		t.Fatal("missing r1 settle entry")
	}
	if settle.AccountID != "payer" || settle.Amount != winner || !settle.At.Equal(settleAt) {
		t.Fatalf("settle entry = %+v, want payer amount %d at settle time", settle, winner)
	}
	if n := countRequestKind(all, LedgerRefund, "r1"); n != b2i(refund > 0) {
		t.Fatalf("r1 refund entries = %d, want %d", n, b2i(refund > 0))
	}
	if refund > 0 {
		ref, ok := ledgerEntryFor(all, LedgerRefund, "r1")
		if !ok {
			t.Fatal("missing r1 refund entry")
		}
		if ref.AccountID != "payer" || ref.Amount != refund || !ref.At.Equal(settleAt) {
			t.Fatalf("refund entry = %+v, want payer amount %d at settle time", ref, refund)
		}
	}
	// 另一笔请求的账本记录保持原样：只有申请时的一条预留。
	if n := entriesForRequest(all, "r2"); n != 1 {
		t.Fatalf("r2 ledger entries = %d, want only its original reserve", n)
	}
	res2, ok := ledgerEntryFor(all, LedgerReserve, "r2")
	if !ok {
		t.Fatal("missing r2 reserve entry")
	}
	if res2.AccountID != "payer" || res2.Amount != 40 {
		t.Fatalf("r2 reserve entry = %+v, want payer amount 40", res2)
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// checkSettleReplay 校验结算完成后的重复回调：再次提交胜出金额幂等返回原来的
// 已结算结果（实际费用与结算时间都不改写，即使时钟已前进），再次提交另一金额
// 仍返回 ErrConflict；两者都不新增账本记录、不重新冻结或扣除费用。
func checkSettleReplay(t *testing.T, w *Wallet, c *clock, settleAt time.Time, winner, loser int64) {
	t.Helper()

	before := len(w.Ledger())
	c.t = c.t.Add(time.Second) // 推进时钟：结算时间若被改写会立即暴露

	again, err := w.Settle("u1", "r1", winner)
	if err != nil {
		t.Fatalf("replay winning settle: %v", err)
	}
	if again.State != RequestSettled || again.ActualFee != winner {
		t.Fatalf("replayed settle = state %v actual %d, want settled %d", again.State, again.ActualFee, winner)
	}
	if !again.SettledAt.Equal(settleAt) {
		t.Fatalf("replayed settle rewrote settled-at to %v, want original %v", again.SettledAt, settleAt)
	}
	if _, err := w.Settle("u1", "r1", loser); !errors.Is(err, ErrConflict) {
		t.Fatalf("replayed losing settle err = %v, want ErrConflict", err)
	}
	if len(w.Ledger()) != before {
		t.Fatalf("replayed callbacks grew ledger %d -> %d", before, len(w.Ledger()))
	}
	// 重复回调之后，资金与状态仍与首次胜出时完全一致。
	checkSettledOutcome(t, w, settleAt, winner)
}

// TestSettleDifferentFeesFirstWins：同一笔已预留请求先后收到两个不同实际费用
// 的结算回调时，先到的合法金额成为最终费用，后到的不同金额返回 ErrConflict；
// 覆盖 30/70 两个普通金额与 0/预估全额两个合法边界，两个方向都验证。
func TestSettleDifferentFeesFirstWins(t *testing.T) {
	cases := []struct {
		name          string
		first, second int64
	}{
		{"30 then 70", 30, 70},
		{"70 then 30", 70, 30},
		{"zero then full estimate", 0, 80},
		{"full estimate then zero", 80, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, c := setupSettleRace(t)
			settleAt := c.t

			got, err := w.Settle("u1", "r1", tc.first)
			if err != nil {
				t.Fatalf("first settle: %v", err)
			}
			if got.State != RequestSettled || got.ActualFee != tc.first || !got.SettledAt.Equal(settleAt) {
				t.Fatalf("first settle = %+v, want settled with actual %d", got, tc.first)
			}
			if _, err := w.Settle("u1", "r1", tc.second); !errors.Is(err, ErrConflict) {
				t.Fatalf("conflicting settle err = %v, want ErrConflict", err)
			}
			checkSettledOutcome(t, w, settleAt, tc.first)
			checkSettleReplay(t, w, c, settleAt, tc.first, tc.second)
		})
	}
}

// TestConcurrentSettleDifferentFees：同一笔预留 80 的请求同时收到 30 与 70 两笔
// 结算回调时，恰好一个成功、另一个返回 ErrConflict；胜出资费成为唯一最终费用，
// 出资账户余额、另一笔预留、使用账户余额、策略累计与账本都与胜出结果一致。
func TestConcurrentSettleDifferentFees(t *testing.T) {
	const iterations = 32
	outcomes := map[string]int{}
	for i := 0; i < iterations; i++ {
		w, c := setupSettleRace(t)
		settleAt := c.t

		start := make(chan struct{})
		var wg sync.WaitGroup
		var view30, view70 RequestView
		var err30, err70 error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			view30, err30 = w.Settle("u1", "r1", 30)
		}()
		go func() {
			defer wg.Done()
			<-start
			view70, err70 = w.Settle("u1", "r1", 70)
		}()
		close(start)
		wg.Wait()

		var winner int64
		var winView RequestView
		switch {
		case err30 == nil && errors.Is(err70, ErrConflict):
			winner, winView = 30, view30
			outcomes["30-wins"]++
		case err70 == nil && errors.Is(err30, ErrConflict):
			winner, winView = 70, view70
			outcomes["70-wins"]++
		default:
			t.Fatalf("iter %d: err30 = %v, err70 = %v, want exactly one success and one ErrConflict", i, err30, err70)
		}

		// 成功返回的结算结果必须与最终请求一致。
		final, err := w.Request("u1", "r1")
		if err != nil {
			t.Fatal(err)
		}
		if winView.State != RequestSettled || winView.ActualFee != winner {
			t.Fatalf("iter %d: winning view = %+v, want settled with actual %d", i, winView, winner)
		}
		if final.ActualFee != winner || !final.SettledAt.Equal(winView.SettledAt) {
			t.Fatalf("iter %d: final request = actual %d settled %v, winning view = actual %d settled %v",
				i, final.ActualFee, final.SettledAt, winView.ActualFee, winView.SettledAt)
		}

		checkSettledOutcome(t, w, settleAt, winner)
		checkSettleReplay(t, w, c, settleAt, winner, 100-winner)
	}
	t.Logf("outcomes: %v", outcomes)
}

// TestConcurrentSettleZeroAndFullFee：并发结算覆盖零与预估全额两个合法边界。
// 零费用胜出时请求仍是已结算，预留全额退回；全额费用胜出时只扣减，不产生
// 零金额退款记录。
func TestConcurrentSettleZeroAndFullFee(t *testing.T) {
	const iterations = 32
	outcomes := map[string]int{}
	for i := 0; i < iterations; i++ {
		w, c := setupSettleRace(t)
		settleAt := c.t

		start := make(chan struct{})
		var wg sync.WaitGroup
		var errZero, errFull error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, errZero = w.Settle("u1", "r1", 0)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, errFull = w.Settle("u1", "r1", 80)
		}()
		close(start)
		wg.Wait()

		var winner int64
		switch {
		case errZero == nil && errors.Is(errFull, ErrConflict):
			winner = 0
			outcomes["zero-wins"]++
		case errFull == nil && errors.Is(errZero, ErrConflict):
			winner = 80
			outcomes["full-wins"]++
		default:
			t.Fatalf("iter %d: errZero = %v, errFull = %v, want exactly one success and one ErrConflict", i, errZero, errFull)
		}

		checkSettledOutcome(t, w, settleAt, winner)
		all := w.Ledger()
		if winner == 0 {
			// 零费用胜出：仍是已结算，80 全额经一条退款记录退回。
			if n := countRequestKind(all, LedgerRefund, "r1"); n != 1 {
				t.Fatalf("iter %d: refund entries = %d, want 1 full refund on zero-fee settle", i, n)
			}
			if bal, _ := w.Balance("payer"); bal != (Balances{Available: 260, Reserved: 40}) {
				t.Fatalf("iter %d: balance = %+v, want {260 40}", i, bal)
			}
		} else {
			// 全额费用胜出：只扣减，不能产生零金额退款记录。
			if n := countRequestKind(all, LedgerRefund, "r1"); n != 0 {
				t.Fatalf("iter %d: refund entries = %d, want none on full-fee settle", i, n)
			}
			if bal, _ := w.Balance("payer"); bal != (Balances{Available: 180, Reserved: 40}) {
				t.Fatalf("iter %d: balance = %+v, want {180 40}", i, bal)
			}
		}
		checkSettleReplay(t, w, c, settleAt, winner, 80-winner)
	}
	t.Logf("outcomes: %v", outcomes)
}
