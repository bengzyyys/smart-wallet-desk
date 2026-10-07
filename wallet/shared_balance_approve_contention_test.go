package wallet

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件为大额代付批准功能补充回归保障：每条策略有自己的累计额度，但出资
// 余额属于账户——两条不同策略下的待审批请求同时批准时，不能各自把同一笔
// 出资余额用两次。额度充足（两条策略各自的预留加已花费都远低于累计上限）
// 也不能让合计超过出资账户可用余额的两笔批准都成功。
//
// 固定场景（见 setupSharedBalanceApproveContest）：
//   - 出资账户 payer 初始可用余额 70（边界用例为 90）、预留 0；使用账户
//     u1/u2 各自余额为 0、分别持有绑定自有设备的有效会话 s1/d1、s2/d2；
//   - 两条策略 p-fee50（允许 u1）与 p-fee40（允许 u2）都以 payer 为出资
//     账户：单次上限 60、各自的累计上限 100、审批门槛 10、审批等待 1 小时，
//     关闭预留超时；
//   - u1 经 p-fee50 提交预估费用 50 的申请（请求编号 r-50），u2 经 p-fee40
//     提交预估费用 40 的申请（请求编号 r-40）；两笔都严格超过门槛且各自
//     策略额度充足，都进入待审批：payer 仍为可用 70 / 预留 0，两条策略的
//     预留与已花费金额均为 0，账本只有两条零金额待审批留痕。
const (
	balContestPayerID         = "payer"
	balContestPolicy50        = "p-fee50"
	balContestPolicy40        = "p-fee40"
	balContestRequest50       = "r-50"
	balContestRequest40       = "r-40"
	balContestFee50     int64 = 50
	balContestFee40     int64 = 40
	balContestPayer70   int64 = 70
	balContestPayer90   int64 = 90
	balContestPerReq    int64 = 60
	balContestMaxTotal  int64 = 100
	balContestThreshold int64 = 10
)

// balanceContestSide 描述争用中的一方：使用账户、会话、策略与请求编号。
type balanceContestSide struct {
	account   string
	session   string
	device    string
	policyID  string
	requestID string
	fee       int64
}

// 两个固定争用方：u1 走 50 元策略，u2 走 40 元策略。
var (
	balContestSide50 = balanceContestSide{
		account: "u1", session: "s1", device: "d1",
		policyID: balContestPolicy50, requestID: balContestRequest50, fee: balContestFee50,
	}
	balContestSide40 = balanceContestSide{
		account: "u2", session: "s2", device: "d2",
		policyID: balContestPolicy40, requestID: balContestRequest40, fee: balContestFee40,
	}
)

// setupSharedBalanceApproveContest 以 payerInit 为出资账户初始余额构造上述
// 固定场景，提交两笔申请并断言起点：两笔均待审批、提交与等待截止时间各自
// 记录；payer 可用 payerInit / 预留 0；两条策略的预留与已花费总额均为 0；
// 账本恰有两条待审批留痕，无任何资金记录。返回提交时刻与等待截止时刻
// （两笔相同）。
func setupSharedBalanceApproveContest(t *testing.T, payerInit int64) (*Wallet, *clock, time.Time, time.Time) {
	t.Helper()
	w, c := newTestWallet()
	mustAccount(t, w, balContestPayerID, payerInit)
	for _, side := range []balanceContestSide{balContestSide50, balContestSide40} {
		mustAccount(t, w, side.account, 0)
		if _, err := w.CreateSession(side.session, side.account, side.device, c.t.Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		// 两条策略共用同一出资账户，但累计额度各自独立。
		if err := w.SavePolicy(PolicySpec{
			ID:                side.policyID,
			PayerAccountID:    balContestPayerID,
			AllowedAccountIDs: []string{side.account},
			Operation:         "charge",
			Payee:             "shop",
			StartsAt:          c.t.Add(-time.Hour),
			EndsAt:            c.t.Add(24 * time.Hour),
			MaxPerRequest:     balContestPerReq,
			MaxTotal:          balContestMaxTotal,
			ApprovalThreshold: balContestThreshold,
			ApprovalWait:      time.Hour,
			// MaxReserveDuration 留空（零）：关闭预留超时。
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.CreateSession("sa", balContestPayerID, "d-approve", c.t.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	for _, side := range []balanceContestSide{balContestSide50, balContestSide40} {
		req, err := w.Apply(RequestInput{
			PolicyID:     side.policyID,
			RequestID:    side.requestID,
			AccountID:    side.account,
			SessionID:    side.session,
			DeviceID:     side.device,
			Operation:    "charge",
			Payee:        "shop",
			EstimatedFee: side.fee,
		})
		if err != nil {
			t.Fatalf("apply for %s: %v", side.account, err)
		}
		if req.State != RequestPendingApproval {
			t.Fatalf("request %s state = %v, want pending approval", side.requestID, req.State)
		}
		if !req.CreatedAt.Equal(c.t) {
			t.Fatalf("request %s created at = %v, want %v", side.requestID, req.CreatedAt, c.t)
		}
		// 待审批不允许有任何预留或决定信息。
		if !req.ReservedAt.IsZero() || !req.DecidedAt.IsZero() || req.ApproverAccountID != "" || req.RejectReason != "" {
			t.Fatalf("pending request must carry no reservation or decision: %+v", req)
		}
	}
	wantDeadline := c.t.Add(time.Hour)
	for _, side := range []balanceContestSide{balContestSide50, balContestSide40} {
		req, err := w.Request(side.account, side.requestID)
		if err != nil {
			t.Fatal(err)
		}
		if !req.WaitDeadline.Equal(wantDeadline) {
			t.Fatalf("request %s wait deadline = %v, want %v", side.requestID, req.WaitDeadline, wantDeadline)
		}
	}

	// 待审批不冻结余额、不占用任何策略的累计额度。
	if bal, _ := w.Balance(balContestPayerID); bal != (Balances{Available: payerInit, Reserved: 0}) {
		t.Fatalf("payer balance after two pending applies = %+v, want {%d 0}", bal, payerInit)
	}
	for _, side := range []balanceContestSide{balContestSide50, balContestSide40} {
		pv, err := w.Policy(side.policyID)
		if err != nil {
			t.Fatal(err)
		}
		if pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
			t.Fatalf("policy %s totals after pending applies = reserved %d spent %d, want 0/0",
				side.policyID, pv.ReservedTotal, pv.SpentTotal)
		}
	}
	// 账本只有两条零金额待审批留痕，出资账户名下没有任何资金记录。
	all := w.Ledger()
	if len(all) != 2 {
		t.Fatalf("ledger after applies = %d entries, want 2 pending entries: %+v", len(all), all)
	}
	if got := countKind(all, LedgerPendingApproval); got != 2 {
		t.Fatalf("pending entries = %d, want 2", got)
	}
	for _, e := range all {
		if e.Amount != 0 {
			t.Fatalf("pending entry = %+v, want zero amount", e)
		}
	}
	if len(w.AccountLedger(balContestPayerID)) != 0 {
		t.Fatalf("pending approvals must not create payer money entries: %+v", w.AccountLedger(balContestPayerID))
	}
	return w, c, c.t, wantDeadline
}

// approveBalanceContest 用出资账户的有效审批会话批准指定一方的待审批请求。
func approveBalanceContest(w *Wallet, side balanceContestSide) (RequestView, error) {
	return w.Approve(side.account, side.requestID, "sa", "d-approve")
}

// checkBalanceContestOutcome 校验“两笔不同策略的待审批同时批准、恰有一笔
// 成功”后的统一结果，不预设胜出者：
//   - 胜出请求已预留其费用：带出资账户的批准决定，实际预留时刻即批准成功
//     时刻；提交时间与等待截止时间保持申请时原值；
//   - 失败请求仍待审批：保留提交时间与等待截止时间，不出现批准决定或预留
//     计时（失败的批准不推进期限、不开始预留计时）；
//   - payer 可用 70-胜出费用 / 预留胜出费用；只有胜出请求所属策略增加对应
//     预留，另一条策略保持零预留，两条策略的已花费金额都不增加；
//   - 原有两条待审批留痕保留；只为胜出请求新增一条 payer 的全额预留记录与
//     一条使用账户的零金额批准记录；失败批准不产生资金或审批决定记录。
func checkBalanceContestOutcome(t *testing.T, w *Wallet, winner, loser balanceContestSide,
	createdAt, waitDeadline, approveAt time.Time) {
	t.Helper()
	if winner.requestID == loser.requestID {
		t.Fatalf("winner and loser must be different requests, got %q", winner.requestID)
	}

	winReq, err := w.Request(winner.account, winner.requestID)
	if err != nil {
		t.Fatal(err)
	}
	if winReq.State != RequestReserved {
		t.Fatalf("winner %s state = %v, want reserved", winner.requestID, winReq.State)
	}
	if winReq.EstimatedFee != winner.fee {
		t.Fatalf("winner estimated fee = %d, want %d", winReq.EstimatedFee, winner.fee)
	}
	if winReq.ApproverAccountID != balContestPayerID {
		t.Fatalf("winner approver = %q, want %s", winReq.ApproverAccountID, balContestPayerID)
	}
	if !winReq.DecidedAt.Equal(approveAt) || !winReq.ReservedAt.Equal(approveAt) {
		t.Fatalf("winner decided/reserved at %v/%v, want approve time %v",
			winReq.DecidedAt, winReq.ReservedAt, approveAt)
	}
	// 批准成功保留原提交时刻与等待截止时刻。
	if !winReq.CreatedAt.Equal(createdAt) || !winReq.WaitDeadline.Equal(waitDeadline) {
		t.Fatalf("winner timing changed: created %v want %v, deadline %v want %v",
			winReq.CreatedAt, createdAt, winReq.WaitDeadline, waitDeadline)
	}
	if winReq.RejectReason != "" {
		t.Fatalf("reserved winner must not carry reject reason: %q", winReq.RejectReason)
	}

	loseReq, err := w.Request(loser.account, loser.requestID)
	if err != nil {
		t.Fatal(err)
	}
	if loseReq.State != RequestPendingApproval {
		t.Fatalf("loser %s state = %v, want still pending approval (must not become rejected)",
			loser.requestID, loseReq.State)
	}
	// 失败批准不留任何决定、不开始预留计时。
	if !loseReq.DecidedAt.IsZero() || loseReq.ApproverAccountID != "" || loseReq.RejectReason != "" {
		t.Fatalf("loser decision fields changed on failed approve: %+v", loseReq)
	}
	if !loseReq.ReservedAt.IsZero() || loseReq.ReserveDuration != 0 || !loseReq.ReserveDeadline.IsZero() {
		t.Fatalf("loser reserve timing started on failed approve: %+v", loseReq)
	}
	// 提交时间与等待截止时间保持原值。
	if !loseReq.CreatedAt.Equal(createdAt) || !loseReq.WaitDeadline.Equal(waitDeadline) {
		t.Fatalf("loser timing changed: created %v want %v, deadline %v want %v",
			loseReq.CreatedAt, createdAt, loseReq.WaitDeadline, waitDeadline)
	}
	if loseReq.EstimatedFee != loser.fee {
		t.Fatalf("loser estimated fee = %d, want %d", loseReq.EstimatedFee, loser.fee)
	}

	// 出资余额属于账户：胜出预留全额从可用转入预留。
	want := Balances{Available: balContestPayer70 - winner.fee, Reserved: winner.fee}
	if bal, _ := w.Balance(balContestPayerID); bal != want {
		t.Fatalf("payer balance = %+v, want %+v", bal, want)
	}
	// 只有胜出请求所属策略增加对应预留；另一条策略保持零预留；
	// 两条策略的已花费金额都不增加。
	winPolicy, err := w.Policy(winner.policyID)
	if err != nil {
		t.Fatal(err)
	}
	if winPolicy.ReservedTotal != winner.fee || winPolicy.SpentTotal != 0 {
		t.Fatalf("winner policy %s totals = reserved %d spent %d, want %d/0",
			winner.policyID, winPolicy.ReservedTotal, winPolicy.SpentTotal, winner.fee)
	}
	losePolicy, err := w.Policy(loser.policyID)
	if err != nil {
		t.Fatal(err)
	}
	if losePolicy.ReservedTotal != 0 || losePolicy.SpentTotal != 0 {
		t.Fatalf("loser policy %s totals = reserved %d spent %d, want 0/0",
			loser.policyID, losePolicy.ReservedTotal, losePolicy.SpentTotal)
	}

	// 账本：两条原待审批留痕 + 胜出请求的一条全额预留 + 一条零金额批准，
	// 共 4 条；失败批准不新增任何记录。
	all := w.Ledger()
	if len(all) != 4 {
		t.Fatalf("ledger entries = %d, want 4 (2 pending + reserve + approval): %+v", len(all), all)
	}
	if got := countKind(all, LedgerPendingApproval); got != 2 {
		t.Fatalf("pending entries = %d, want original 2 preserved", got)
	}
	if got := countKind(all, LedgerReserve); got != 1 {
		t.Fatalf("reserve entries = %d, want exactly 1 for the winner", got)
	}
	if got := countKind(all, LedgerApproval); got != 1 {
		t.Fatalf("approval entries = %d, want exactly 1 for the winner", got)
	}
	for _, kind := range []LedgerKind{LedgerRejection, LedgerRefund, LedgerSettle, LedgerExpiration, LedgerCancellation} {
		if got := countKind(all, kind); got != 0 {
			t.Fatalf("ledger kind %v entries = %d, want none after contested approvals", kind, got)
		}
	}
	res, ok := ledgerEntryFor(all, LedgerReserve, winner.requestID)
	if !ok {
		t.Fatalf("missing reserve entry for winner %s", winner.requestID)
	}
	if res.AccountID != balContestPayerID || res.Amount != winner.fee || !res.At.Equal(approveAt) {
		t.Fatalf("winner reserve entry = %+v, want payer %d at approve time", res, winner.fee)
	}
	app, ok := ledgerEntryFor(all, LedgerApproval, winner.requestID)
	if !ok {
		t.Fatalf("missing zero-amount approval entry for winner %s", winner.requestID)
	}
	if app.AccountID != winner.account || app.Amount != 0 || !app.At.Equal(approveAt) {
		t.Fatalf("winner approval entry = %+v, want zero-amount entry for %s at approve time", app, winner.account)
	}
	// 失败的一方不产生资金或审批决定记录。
	if _, ok := ledgerEntryFor(all, LedgerReserve, loser.requestID); ok {
		t.Fatalf("loser %s must not have a reserve entry", loser.requestID)
	}
	if _, ok := ledgerEntryFor(all, LedgerApproval, loser.requestID); ok {
		t.Fatalf("loser %s must not have an approval entry", loser.requestID)
	}
	// 两条待审批留痕仍分别归属两笔请求，失败批准没有把失败方转成拒绝留痕。
	pendingRequests := map[string]int{}
	for _, e := range all {
		if e.Kind == LedgerPendingApproval {
			pendingRequests[e.RequestID]++
		}
	}
	if pendingRequests[balContestRequest50] != 1 || pendingRequests[balContestRequest40] != 1 {
		t.Fatalf("pending traces = %+v, want one each for r-50 and r-40", pendingRequests)
	}
}

// TestSharedBalanceSequentialApprovalsOnlyOneSucceeds 顺序复现争用：两条策略
// 各自的额度都充足（单次 50/40 ≤ 60，累计 50/40 ≤ 100），但出资账户可用
// 余额 70 装不下 50+40=90。同一时刻连续批准两笔待审批请求，只能有一笔成功，
// 另一笔必须返回 ErrInsufficientBalance（不能误报 ErrQuotaExceeded）并继续
// 待审批，不能转为拒绝终态。两种先后结果都覆盖。
func TestSharedBalanceSequentialApprovalsOnlyOneSucceeds(t *testing.T) {
	for _, order := range []struct {
		name   string
		first  balanceContestSide
		second balanceContestSide
	}{
		{"fee50-first", balContestSide50, balContestSide40},
		{"fee40-first", balContestSide40, balContestSide50},
	} {
		t.Run(order.name, func(t *testing.T) {
			w, c, createdAt, waitDeadline := setupSharedBalanceApproveContest(t, balContestPayer70)
			approveAt := c.t.Add(10 * time.Second)
			c.t = approveAt

			first, err := approveBalanceContest(w, order.first)
			if err != nil {
				t.Fatalf("first approve %s: %v", order.first.requestID, err)
			}
			if first.State != RequestReserved {
				t.Fatalf("first approve state = %v, want reserved", first.State)
			}
			// 第二笔：两条策略各自的额度都充足，但第一笔预留后 payer 可用
			// 余额已不足第二笔费用，必须按余额不足失败而非额度超限。
			if _, err := approveBalanceContest(w, order.second); !errors.Is(err, ErrInsufficientBalance) {
				t.Fatalf("second approve %s err = %v, want ErrInsufficientBalance", order.second.requestID, err)
			} else if errors.Is(err, ErrQuotaExceeded) {
				t.Fatalf("second approve must fail on shared payer balance, not policy quota: %v", err)
			}
			checkBalanceContestOutcome(t, w, order.first, order.second, createdAt, waitDeadline, approveAt)

			// 期限内立即再次批准失败方：余额仍被胜出预留占用，依旧余额不足、
			// 依旧待审批，账本不增长。
			ledgerBefore := len(w.Ledger())
			if _, err := approveBalanceContest(w, order.second); !errors.Is(err, ErrInsufficientBalance) {
				t.Fatalf("retry approve %s err = %v, want ErrInsufficientBalance", order.second.requestID, err)
			}
			if len(w.Ledger()) != ledgerBefore {
				t.Fatalf("retry approve grew ledger %d -> %d", ledgerBefore, len(w.Ledger()))
			}
			checkBalanceContestOutcome(t, w, order.first, order.second, createdAt, waitDeadline, approveAt)
		})
	}
}

// raceBalanceBias 控制两个批准 goroutine 的起跑方式，使“50 先获批”与
// “40 先获批”两种先后顺序都能在并发 harness 下被确定性地观察到：偏向模式下
// 后一个 goroutine 等前一个的 Approve 完全返回后才起跑，先后顺序不依赖
// 调度器或睡眠时间。
type raceBalanceBias int

const (
	balanceSimultaneous raceBalanceBias = iota
	balanceFee50First
	balanceFee40First
)

// raceSharedBalanceApprovals 用出资账户的同一个有效审批会话从两个 goroutine
// 同时批准两条策略下的待审批请求，断言恰有一笔成功、另一笔
// ErrInsufficientBalance，返回胜出与失败的一方。bias 用于确定性地覆盖两种
// 先后顺序。
func raceSharedBalanceApprovals(t *testing.T, w *Wallet, bias raceBalanceBias) (winner, loser balanceContestSide) {
	t.Helper()
	sides := []balanceContestSide{balContestSide50, balContestSide40}
	views := make([]RequestView, 2)
	errs := make([]error, 2)
	gates := []chan struct{}{make(chan struct{}), make(chan struct{})}
	done := []chan struct{}{make(chan struct{}), make(chan struct{})}

	var wg sync.WaitGroup
	for i, side := range sides {
		i, side := i, side
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gates[i]
			views[i], errs[i] = approveBalanceContest(w, side)
			close(done[i])
		}()
	}
	// 偏向模式下先放行指定的一方，并等它的批准完整结束后再放行另一方，
	// 确定性地产生先后顺序；同时模式下两个闸门一起关闭。
	switch bias {
	case balanceFee50First:
		close(gates[0])
		<-done[0]
		close(gates[1])
	case balanceFee40First:
		close(gates[1])
		<-done[1]
		close(gates[0])
	default:
		close(gates[0])
		close(gates[1])
	}
	wg.Wait()

	successes := 0
	for i, side := range sides {
		switch {
		case errs[i] == nil:
			successes++
			winner = side
			if views[i].State != RequestReserved || views[i].ApproverAccountID != balContestPayerID {
				t.Fatalf("winning approve view for %s = %+v, want reserved by payer", side.requestID, views[i])
			}
		case errors.Is(errs[i], ErrInsufficientBalance):
			loser = side
		default:
			t.Fatalf("approve for %s err = %v, want nil or ErrInsufficientBalance", side.requestID, errs[i])
		}
	}
	if successes != 1 || winner.requestID == "" || loser.requestID == "" {
		t.Fatalf("contested approvals must yield exactly one success and one balance error, got winner %q loser %q errs %v",
			winner.requestID, loser.requestID, errs)
	}
	return winner, loser
}

// TestSharedBalanceConcurrentApprovalsAcrossPolicies 并发复现题述场景：出资
// 账户用有效审批会话同时批准两条不同策略下的待审批请求时，只能有一笔成功，
// 另一笔返回 ErrInsufficientBalance 并继续待审批；不预设哪笔先成功——50
// 获批与 40 获批两种结果都必须符合同一规则。三种起跑方式各跑多轮，并要求
// 两方都至少胜出一次，确保两种先后顺序都被真正验证。
func TestSharedBalanceConcurrentApprovalsAcrossPolicies(t *testing.T) {
	const iterations = 24
	wins := map[string]int{}
	for _, bias := range []struct {
		name string
		mode raceBalanceBias
	}{
		{"simultaneous", balanceSimultaneous},
		{"fee50-first", balanceFee50First},
		{"fee40-first", balanceFee40First},
	} {
		for i := 0; i < iterations; i++ {
			w, c, createdAt, waitDeadline := setupSharedBalanceApproveContest(t, balContestPayer70)
			approveAt := c.t.Add(10 * time.Second)
			c.t = approveAt

			winner, loser := raceSharedBalanceApprovals(t, w, bias.mode)
			wins[winner.requestID]++
			checkBalanceContestOutcome(t, w, winner, loser, createdAt, waitDeadline, approveAt)
		}
	}
	if len(wins) != 2 {
		t.Fatalf("each request must win at least once, got winner distribution %v", wins)
	}
	t.Logf("winner distribution: %v", wins)
}

// TestSharedBalanceExactBoundaryBothApproved 覆盖边界：出资账户可用余额恰为
// 90（= 50 + 40），其余条件相同。两次批准都应成功：第一笔预留后剩余可用
// 恰好等于第二笔费用，不能因余额恰好用尽而拒绝后完成的批准。最终可用 0、
// 预留 90；两条策略分别预留 50 和 40——各策略额度充足时，不能把不同策略的
// 费用合并成同一条策略的额度占用。两种批准顺序都覆盖。
func TestSharedBalanceExactBoundaryBothApproved(t *testing.T) {
	for _, order := range []struct {
		name   string
		first  balanceContestSide
		second balanceContestSide
	}{
		{"fee50-first", balContestSide50, balContestSide40},
		{"fee40-first", balContestSide40, balContestSide50},
	} {
		t.Run(order.name, func(t *testing.T) {
			w, c, createdAt, waitDeadline := setupSharedBalanceApproveContest(t, balContestPayer90)
			approveAt := c.t.Add(10 * time.Second)
			c.t = approveAt

			first, err := approveBalanceContest(w, order.first)
			if err != nil {
				t.Fatalf("first approve %s: %v", order.first.requestID, err)
			}
			if first.State != RequestReserved {
				t.Fatalf("first approve state = %v, want reserved", first.State)
			}
			// 第一笔预留后可用余额恰好等于第二笔费用（90-50=40 或 90-40=50），
			// 第二笔必须照样获批。
			secondApproveAt := approveAt.Add(10 * time.Second)
			c.t = secondApproveAt
			second, err := approveBalanceContest(w, order.second)
			if err != nil {
				t.Fatalf("second approve %s must succeed at exact boundary: %v", order.second.requestID, err)
			}
			if second.State != RequestReserved {
				t.Fatalf("second approve state = %v, want reserved", second.State)
			}

			// 可用余额恰好用尽：0 可用、90 预留。
			if bal, _ := w.Balance(balContestPayerID); bal != (Balances{Available: 0, Reserved: 90}) {
				t.Fatalf("payer balance = %+v, want {0 90}", bal)
			}
			// 两条策略各自只累计自己那笔预留，已花费金额都不增加。
			pv50, err := w.Policy(balContestPolicy50)
			if err != nil {
				t.Fatal(err)
			}
			if pv50.ReservedTotal != balContestFee50 || pv50.SpentTotal != 0 {
				t.Fatalf("policy %s totals = reserved %d spent %d, want 50/0 (must not absorb the other policy's fee)",
					balContestPolicy50, pv50.ReservedTotal, pv50.SpentTotal)
			}
			pv40, err := w.Policy(balContestPolicy40)
			if err != nil {
				t.Fatal(err)
			}
			if pv40.ReservedTotal != balContestFee40 || pv40.SpentTotal != 0 {
				t.Fatalf("policy %s totals = reserved %d spent %d, want 40/0 (must not absorb the other policy's fee)",
					balContestPolicy40, pv40.ReservedTotal, pv40.SpentTotal)
			}

			// 两笔请求都已预留，各带出资账户在各自批准时刻的批准决定，
			// 提交时间与等待截止时间保持申请时原值。
			approveTimes := map[string]time.Time{
				order.first.requestID:  approveAt,
				order.second.requestID: secondApproveAt,
			}
			for _, side := range []balanceContestSide{balContestSide50, balContestSide40} {
				req, err := w.Request(side.account, side.requestID)
				if err != nil {
					t.Fatal(err)
				}
				if req.State != RequestReserved {
					t.Fatalf("request %s state = %v, want reserved", side.requestID, req.State)
				}
				at := approveTimes[side.requestID]
				if req.ApproverAccountID != balContestPayerID || !req.DecidedAt.Equal(at) || !req.ReservedAt.Equal(at) {
					t.Fatalf("request %s decision fields = %+v, want payer decision at %v", side.requestID, req, at)
				}
				if !req.CreatedAt.Equal(createdAt) || !req.WaitDeadline.Equal(waitDeadline) {
					t.Fatalf("request %s timing changed: %+v", side.requestID, req)
				}
			}

			// 账本：两条待审批留痕 + 各一条全额预留与零金额批准，共 6 条。
			all := w.Ledger()
			if len(all) != 6 {
				t.Fatalf("ledger entries = %d, want 6 (2 pending + 2 reserve + 2 approval): %+v", len(all), all)
			}
			if got := countKind(all, LedgerPendingApproval); got != 2 {
				t.Fatalf("pending entries = %d, want 2", got)
			}
			if got := countKind(all, LedgerReserve); got != 2 {
				t.Fatalf("reserve entries = %d, want 2", got)
			}
			if got := countKind(all, LedgerApproval); got != 2 {
				t.Fatalf("approval entries = %d, want 2", got)
			}
			for _, side := range []balanceContestSide{balContestSide50, balContestSide40} {
				res, ok := ledgerEntryFor(all, LedgerReserve, side.requestID)
				if !ok {
					t.Fatalf("missing reserve entry for %s", side.requestID)
				}
				if res.AccountID != balContestPayerID || res.Amount != side.fee {
					t.Fatalf("reserve entry for %s = %+v, want payer %d", side.requestID, res, side.fee)
				}
				app, ok := ledgerEntryFor(all, LedgerApproval, side.requestID)
				if !ok {
					t.Fatalf("missing approval entry for %s", side.requestID)
				}
				if app.AccountID != side.account || app.Amount != 0 {
					t.Fatalf("approval entry for %s = %+v, want zero-amount entry for %s", side.requestID, app, side.account)
				}
			}
		})
	}
}
