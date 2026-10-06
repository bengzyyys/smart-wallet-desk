package wallet

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件为“同一笔已预留代付请求同时收到不同实际费用的结算回调”补充回归
// 保障：第一次成功结算确定唯一实际费用，相同金额的后续结算幂等返回已有
// 结果，不同金额返回 ErrConflict；回调并发到达时也只能有一个金额胜出，
// 不能各自成功扣除不同费用，后来的金额也不能覆盖已完成的结算。
//
// 固定场景（见 setupConcurrentSettle）：
//   - 出资账户 payer 初始可用余额 300，使用账户 user 与其不同、自有余额 50；
//   - 同一出资账户、同一策略下两笔直接受理的预留：r-main 预估 80、r-other
//     预估 40；策略无需审批、未启用预留超时，单次与共享限额、余额均足够；
//   - 两笔预留后 payer 为可用 180 / 预留 120，策略预留总额 120、已花费 0；
//   - r-main 按两个不同的合法实际费用并发结算，r-other 始终保持已预留。
const (
	racePayerInitial  int64 = 300
	raceUserInitial   int64 = 50
	raceMainRequest         = "r-main"
	raceMainEstimate  int64 = 80
	raceOtherRequest        = "r-other"
	raceOtherEstimate int64 = 40
)

// setupConcurrentSettle 构造上述固定场景，并断言两笔预留完成后的起点状态：
// payer 可用 180 / 预留 120，user 自身余额不变，策略预留 120、已花费 0。
func setupConcurrentSettle(t *testing.T) (*Wallet, *clock) {
	t.Helper()
	w, c := newTestWallet()
	mustAccount(t, w, "payer", racePayerInitial)
	mustAccount(t, w, "user", raceUserInitial)
	if _, err := w.CreateSession("s-user", "user", "dev-user", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	spec := PolicySpec{
		ID:                "p-settle",
		PayerAccountID:    "payer",
		AllowedAccountIDs: []string{"user"},
		Operation:         "charge",
		Payee:             "shop",
		StartsAt:          c.t.Add(-time.Hour),
		EndsAt:            c.t.Add(time.Hour),
		// 限额只需容纳两笔预留；不开启审批与预留超时。
		MaxPerRequest: 100,
		MaxTotal:      1000,
	}
	if err := w.SavePolicy(spec); err != nil {
		t.Fatal(err)
	}
	for _, in := range []RequestInput{
		settleRaceApply(raceMainRequest, raceMainEstimate),
		settleRaceApply(raceOtherRequest, raceOtherEstimate),
	} {
		view, err := w.Apply(in)
		if err != nil {
			t.Fatalf("apply %s: %v", in.RequestID, err)
		}
		if view.State != RequestReserved {
			t.Fatalf("request %s state = %v, want reserved", in.RequestID, view.State)
		}
	}

	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 180, Reserved: 120}) {
		t.Fatalf("payer starting balance = %+v, want {180 120}", bal)
	}
	if bal, _ := w.Balance("user"); bal != (Balances{Available: raceUserInitial, Reserved: 0}) {
		t.Fatalf("user starting balance = %+v, want own balance untouched", bal)
	}
	if pv, _ := w.Policy("p-settle"); pv.ReservedTotal != 120 || pv.SpentTotal != 0 {
		t.Fatalf("policy starting totals = reserved %d spent %d, want 120/0", pv.ReservedTotal, pv.SpentTotal)
	}
	return w, c
}

func settleRaceApply(requestID string, estimate int64) RequestInput {
	return RequestInput{
		PolicyID:     "p-settle",
		RequestID:    requestID,
		AccountID:    "user",
		SessionID:    "s-user",
		DeviceID:     "dev-user",
		Operation:    "charge",
		Payee:        "shop",
		EstimatedFee: estimate,
	}
}

// expectedSettledLedgerLen 返回 r-main 以 winnerFee 胜出后账本的总条数：
// 两条原有预留，加一条实际费用扣减；实际费用小于预估时再加一条差额退款，
// 实际费用等于预估（含零差额）时不产生退款。
func expectedSettledLedgerLen(winnerFee int64) int {
	n := 3 // r-main 预留 + r-other 预留 + 一条实际费用扣减
	if winnerFee < raceMainEstimate {
		n++ // 差额退款（胜出金额为零时也是全额退款一条）
	}
	return n
}

// checkSettledOutcome 校验 r-main 以 winnerFee 成为最终实际费用后的全部可
// 观察结果（结算时刻必须是 settledAt，不被任何重复调用改写）：
//   - 最终请求为已结算，实际费用为 winnerFee，预估仍为 80，结算时间固定；
//   - r-other 保持已预留、实际费用为零，其账本记录原样不变；
//   - payer 可用 = 300 - 40(r-other 仍预留) - winnerFee，预留恒为 40；
//   - user 自身余额不因代付结算变动；资金守恒：可用 + 预留 + 已花费 = 300；
//   - 策略预留总额 40、已花费总额只计胜出实际费用 winnerFee；
//   - r-main 只新增一条 payer 的实际扣减（winnerFee，零费用时为零金额记录），
//     winnerFee < 80 时另增一条 payer 的差额退款（80-winnerFee），金额为零时
//     不产生零金额退款；r-other 仍只有一条 40 的预留记录；
//   - 全部资金记录都归 payer，user 名下没有任何资金记录，账本总条数固定，
//     重复回调不增加记录。
func checkSettledOutcome(t *testing.T, w *Wallet, winnerFee int64, settledAt time.Time) {
	t.Helper()

	got, err := w.Request("user", raceMainRequest)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestSettled {
		t.Fatalf("r-main state = %v, want settled", got.State)
	}
	if got.EstimatedFee != raceMainEstimate || got.ActualFee != winnerFee {
		t.Fatalf("r-main fees = estimated %d actual %d, want estimated 80 actual %d",
			got.EstimatedFee, got.ActualFee, winnerFee)
	}
	if got.SettledAt.IsZero() || !got.SettledAt.Equal(settledAt) {
		t.Fatalf("r-main settled at %v, want %v (must not be rewritten)", got.SettledAt, settledAt)
	}

	// 同一出资账户、同一策略下的另一笔预留不受影响。
	other, err := w.Request("user", raceOtherRequest)
	if err != nil {
		t.Fatal(err)
	}
	if other.State != RequestReserved || other.EstimatedFee != raceOtherEstimate || other.ActualFee != 0 {
		t.Fatalf("r-other = %+v, want still reserved for 40 with no actual fee", other)
	}

	wantAvailable := racePayerInitial - raceOtherEstimate - winnerFee
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: wantAvailable, Reserved: raceOtherEstimate}) {
		t.Fatalf("payer balance (winner %d) = %+v, want {%d 40}", winnerFee, bal, wantAvailable)
	}
	if bal, _ := w.Balance("user"); bal != (Balances{Available: raceUserInitial, Reserved: 0}) {
		t.Fatalf("user balance = %+v, want unchanged {%d 0}", bal, raceUserInitial)
	}
	pv, err := w.Policy("p-settle")
	if err != nil {
		t.Fatal(err)
	}
	if pv.ReservedTotal != raceOtherEstimate || pv.SpentTotal != winnerFee {
		t.Fatalf("policy totals (winner %d) = reserved %d spent %d, want 40/%d",
			winnerFee, pv.ReservedTotal, pv.SpentTotal, winnerFee)
	}
	if payerBal, _ := w.Balance("payer"); payerBal.Available+payerBal.Reserved+pv.SpentTotal != racePayerInitial {
		t.Fatalf("funds not conserved: %+v + spent %d", payerBal, pv.SpentTotal)
	}

	all := w.Ledger()
	if want := expectedSettledLedgerLen(winnerFee); len(all) != want {
		t.Fatalf("ledger len (winner %d) = %d, want %d: %+v", winnerFee, len(all), want, all)
	}

	// r-main 的全部记录只能是：一条 80 预留、一条 winnerFee 扣减，以及（实际
	// 小于预估时）一条 80-winnerFee 退款；全部归出资账户。
	mainEntries := settleRaceEntries(all, raceMainRequest)
	wantMain := 2
	if winnerFee < raceMainEstimate {
		wantMain = 3
	}
	if len(mainEntries) != wantMain {
		t.Fatalf("r-main ledger entries = %d, want %d: %+v", len(mainEntries), wantMain, mainEntries)
	}
	var settleCount, refundCount int
	for _, e := range mainEntries {
		if e.AccountID != "payer" {
			t.Fatalf("r-main money entry must belong to payer, got %+v", e)
		}
		switch e.Kind {
		case LedgerReserve:
			if e.Amount != raceMainEstimate {
				t.Fatalf("r-main reserve amount = %d, want 80", e.Amount)
			}
		case LedgerSettle:
			settleCount++
			if e.Amount != winnerFee {
				t.Fatalf("r-main settle amount = %d, want winner fee %d", e.Amount, winnerFee)
			}
			if !e.At.Equal(settledAt) {
				t.Fatalf("r-main settle at %v, want %v", e.At, settledAt)
			}
		case LedgerRefund:
			refundCount++
			if want := raceMainEstimate - winnerFee; e.Amount != want {
				t.Fatalf("r-main refund amount = %d, want %d", e.Amount, want)
			}
		default:
			t.Fatalf("r-main has unexpected ledger entry: %+v", e)
		}
	}
	if settleCount != 1 {
		t.Fatalf("r-main settle records = %d, want exactly 1", settleCount)
	}
	if winnerFee < raceMainEstimate {
		if refundCount != 1 {
			t.Fatalf("r-main refund records = %d, want exactly 1", refundCount)
		}
	} else if refundCount != 0 {
		t.Fatalf("r-main refund records = %d, want none when actual equals estimated", refundCount)
	}

	// r-other 保持原样：只有一条 40 的预留，没有扣减或退款。
	otherEntries := settleRaceEntries(all, raceOtherRequest)
	if len(otherEntries) != 1 {
		t.Fatalf("r-other ledger entries = %d, want 1: %+v", len(otherEntries), otherEntries)
	}
	if e := otherEntries[0]; e.Kind != LedgerReserve || e.Amount != raceOtherEstimate || e.AccountID != "payer" {
		t.Fatalf("r-other ledger entry = %+v, want single payer reserve of 40", e)
	}

	// 全部资金记录归出资账户；使用账户名下没有任何资金或状态记录。
	payerLedger := w.AccountLedger("payer")
	if got := countKind(payerLedger, LedgerReserve); got != 2 {
		t.Fatalf("payer reserve records = %d, want 2", got)
	}
	if got := countKind(payerLedger, LedgerSettle); got != 1 {
		t.Fatalf("payer settle records = %d, want 1", got)
	}
	if got := countKind(payerLedger, LedgerRefund); got != boolToInt(winnerFee < raceMainEstimate) {
		t.Fatalf("payer refund records = %d, want %d", got, boolToInt(winnerFee < raceMainEstimate))
	}
	if userLedger := w.AccountLedger("user"); len(userLedger) != 0 {
		t.Fatalf("user ledger must stay empty, got %+v", userLedger)
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// settleRaceEntries 返回账本中关联指定请求编号的全部记录。
func settleRaceEntries(entries []LedgerEntry, requestID string) []LedgerEntry {
	var out []LedgerEntry
	for _, e := range entries {
		if e.RequestID == requestID {
			out = append(out, e)
		}
	}
	return out
}

// checkViewIsFinal 断言一次成功返回的结算视图与最终请求完全一致。
func checkViewIsFinal(t *testing.T, w *Wallet, got RequestView, winnerFee int64, settledAt time.Time) {
	t.Helper()
	if got.State != RequestSettled || got.ActualFee != winnerFee || got.EstimatedFee != raceMainEstimate {
		t.Fatalf("successful settle view = %+v, want settled actual %d", got, winnerFee)
	}
	if !got.SettledAt.Equal(settledAt) {
		t.Fatalf("successful settle view settled at %v, want %v", got.SettledAt, settledAt)
	}
	final, err := w.Request("user", raceMainRequest)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != got.State || final.ActualFee != got.ActualFee || !final.SettledAt.Equal(got.SettledAt) {
		t.Fatalf("successful view %+v disagrees with final request %+v", got, final)
	}
}

// checkRepeatedCallbacks 校验终态确定后的迟到回调：把时钟向后拨，再次提交
// 胜出金额必须原样返回已有已结算结果（实际费用与结算时间都不改写、不重新
// 冻结或扣款），提交另一金额仍返回 ErrConflict；这些调用都不增加账本记录，
// 其余额、策略金额与 r-other 状态与终态完全一致。
func checkRepeatedCallbacks(t *testing.T, w *Wallet, c *clock, winnerFee, loserFee int64, settledAt time.Time) {
	t.Helper()

	// 时间明显晚于结算时刻：任何“按当前时刻重写结算时间”的实现都会暴露。
	c.t = settledAt.Add(time.Minute)
	ledgerBefore := len(w.Ledger())

	again, err := w.Settle("user", raceMainRequest, winnerFee)
	if err != nil {
		t.Fatalf("repeat settle with winner fee %d: %v", winnerFee, err)
	}
	checkViewIsFinal(t, w, again, winnerFee, settledAt)

	if _, err := w.Settle("user", raceMainRequest, loserFee); !errors.Is(err, ErrConflict) {
		t.Fatalf("settle with other fee %d err = %v, want ErrConflict", loserFee, err)
	}
	// 冲突之后再次提交胜出金额：仍是原结果，冲突调用不得“重新打开”请求。
	again2, err := w.Settle("user", raceMainRequest, winnerFee)
	if err != nil {
		t.Fatalf("repeat winner settle after conflict: %v", err)
	}
	checkViewIsFinal(t, w, again2, winnerFee, settledAt)
	// 另一金额再来一次仍然冲突。
	if _, err := w.Settle("user", raceMainRequest, loserFee); !errors.Is(err, ErrConflict) {
		t.Fatalf("second settle with other fee %d err = %v, want ErrConflict", loserFee, err)
	}

	if len(w.Ledger()) != ledgerBefore {
		t.Fatalf("repeated callbacks changed ledger %d -> %d", ledgerBefore, len(w.Ledger()))
	}
	// 账本不增、余额不改写、预留不重新冻结、结算时间不变。
	checkSettledOutcome(t, w, winnerFee, settledAt)
}

// raceSettleBias 控制两个结算 goroutine 的起跑方式：simultaneous 同时放行；
// aFirst/bFirst 让指定 goroutine 先取得一小段起跑优势，使“任一金额先完成”
// 的两种先后顺序都能在并发 harness 下被确定性地观察到，而不依赖调度器运气。
type raceSettleBias int

const (
	raceSimultaneous raceSettleBias = iota
	raceAFirst
	raceBFirst
)

// raceSettlePair 让 feeA 与 feeB 两个不同的合法实际费用从各自的 goroutine
// 并发结算同一笔 r-main，断言恰好一个金额成功、另一个返回 ErrConflict，且
// 成功视图与最终请求一致。bias 控制起跑先后（见 raceSettleBias）。返回胜出
// 金额与失败金额。
func raceSettlePair(t *testing.T, w *Wallet, settledAt time.Time, feeA, feeB int64, bias raceSettleBias) (winner, loser int64) {
	t.Helper()
	if feeA == feeB {
		t.Fatalf("race pair must use distinct fees, got %d/%d", feeA, feeB)
	}
	fees := []int64{feeA, feeB}
	views := make([]RequestView, 2)
	errs := make([]error, 2)
	// 每个 goroutine 独立起跑闸门：同时放行时一起关闭，偏向某一方时先关闭
	// 它的闸门、稍候再关闭另一个。
	gates := []chan struct{}{make(chan struct{}), make(chan struct{})}
	release := func() {
		switch bias {
		case raceAFirst:
			close(gates[0])
			time.Sleep(2 * time.Millisecond)
			close(gates[1])
		case raceBFirst:
			close(gates[1])
			time.Sleep(2 * time.Millisecond)
			close(gates[0])
		default:
			close(gates[0])
			close(gates[1])
		}
	}

	var wg sync.WaitGroup
	for i, fee := range fees {
		i, fee := i, fee
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gates[i]
			views[i], errs[i] = w.Settle("user", raceMainRequest, fee)
		}()
	}
	release()
	wg.Wait()

	successes := 0
	winner, loser = -1, -1
	for i, fee := range fees {
		switch {
		case errs[i] == nil:
			successes++
			winner = fee
			checkViewIsFinal(t, w, views[i], fee, settledAt)
		case errors.Is(errs[i], ErrConflict):
			loser = fee
		default:
			t.Fatalf("concurrent settle fee %d err = %v, want nil or ErrConflict", fee, errs[i])
		}
	}
	if successes != 1 || winner < 0 || loser < 0 {
		t.Fatalf("concurrent settles must yield exactly one success and one conflict, got winner %d loser %d errs %v",
			winner, loser, errs)
	}
	return winner, loser
}

// TestSettleWinnerOutcomes 顺序确定每种合法胜出金额下的完整结果，保证并发
// 测试中无论哪个金额先完成，期望口径都一致：
//   - 30 胜出：可用 230 / 预留 40，已花费 30，差额退款 50；
//   - 70 胜出：可用 190 / 预留 40，已花费 70，差额退款 10；
//   - 0  胜出（零费用边界）：仍为已结算，可用 260 / 预留 40，已花费 0，
//     全额退回 80，扣减记录为零金额；
//   - 80 胜出（预估全额边界）：可用 180 / 预留 40，已花费 80，只扣减、
//     不产生零金额退款。
func TestSettleWinnerOutcomes(t *testing.T) {
	cases := []struct {
		name          string
		winner, loser int64
		wantAvailable int64
		wantRefund    int64
	}{
		{"winner-30", 30, 70, 230, 50},
		{"winner-70", 70, 30, 190, 10},
		{"winner-zero-full-refund", 0, 80, 260, 80},
		{"winner-full-no-refund", 80, 0, 180, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, c := setupConcurrentSettle(t)
			settledAt := c.t.Add(10 * time.Second)
			c.t = settledAt

			view, err := w.Settle("user", raceMainRequest, tc.winner)
			if err != nil {
				t.Fatalf("settle winner %d: %v", tc.winner, err)
			}
			checkViewIsFinal(t, w, view, tc.winner, settledAt)
			checkSettledOutcome(t, w, tc.winner, settledAt)

			// 另一金额（零或预估全额同样合法）不能改写已完成的结算。
			if _, err := w.Settle("user", raceMainRequest, tc.loser); !errors.Is(err, ErrConflict) {
				t.Fatalf("settle other fee %d err = %v, want ErrConflict", tc.loser, err)
			}
			checkRepeatedCallbacks(t, w, c, tc.winner, tc.loser, settledAt)

			// 显式复核本用例点名的可用余额与差额退款金额。
			if bal, _ := w.Balance("payer"); bal.Available != tc.wantAvailable || bal.Reserved != raceOtherEstimate {
				t.Fatalf("payer balance = %+v, want {%d 40}", bal, tc.wantAvailable)
			}
			all := w.Ledger()
			refunds := settleRaceEntries(all, raceMainRequest)
			var refundAmount int64 = -1
			for _, e := range refunds {
				if e.Kind == LedgerRefund {
					refundAmount = e.Amount
				}
			}
			if tc.wantRefund == 0 {
				if refundAmount != -1 {
					t.Fatalf("unexpected refund %d, want none", refundAmount)
				}
			} else if refundAmount != tc.wantRefund {
				t.Fatalf("refund amount = %d, want %d", refundAmount, tc.wantRefund)
			}
		})
	}
}

// TestConcurrentSettleDifferentFees：同一笔预留同时收到两笔不同实际费用的
// 结算回调时，先成功者确定唯一实际费用，另一回调只能 ErrConflict；任一金
// 额先完成都合法，最终余额、策略已花费、使用账户余额、另一笔预留与账本
// 都只按胜出金额解释。随后的重复回调沿用幂等与冲突规则且不改写终态。
//
// 金额对覆盖常规差额（30/70）与两个合法边界（零/预估全额）。每对金额都在
// 三种起跑方式下多轮运行：同时放行、feeA 先放行、feeB 先放行，从而确定性
// 地覆盖两种回调先后顺序而不依赖调度运气；断言对实际胜出者自适应，但要求
// 两个金额都至少胜出一次，否则该先后顺序未被真正验证。
func TestConcurrentSettleDifferentFees(t *testing.T) {
	const iterations = 24
	biases := []struct {
		name string
		mode raceSettleBias
	}{
		{"simultaneous", raceSimultaneous},
		{"feeA-first", raceAFirst},
		{"feeB-first", raceBFirst},
	}
	pairs := [][2]int64{
		{30, 70},
		{0, raceMainEstimate},
	}
	for _, pair := range pairs {
		wins := map[int64]int{}
		for _, b := range biases {
			for i := 0; i < iterations; i++ {
				w, c := setupConcurrentSettle(t)
				settledAt := c.t.Add(10 * time.Second)
				c.t = settledAt

				winner, loser := raceSettlePair(t, w, settledAt, pair[0], pair[1], b.mode)
				wins[winner]++
				// 唯一终态：余额、额度、账本全部只按胜出金额解释。
				checkSettledOutcome(t, w, winner, settledAt)
				// 迟到的胜出金额幂等、失败金额持续冲突，均不再扣款或记账。
				checkRepeatedCallbacks(t, w, c, winner, loser, settledAt)
			}
		}
		if len(wins) != 2 {
			t.Fatalf("pair %v: each fee must win at least once, got winner distribution %v", pair, wins)
		}
		t.Logf("pair %v winner distribution: %v", pair, wins)
	}
}
