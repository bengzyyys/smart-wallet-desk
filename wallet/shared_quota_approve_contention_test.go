package wallet

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件为大额代付批准功能补充回归保障：不同使用账户各自持有效设备会话、
// 共用同一条策略的共享累计额度时，并发批准各自的待审批请求只能按整条策略
// 的现存预留加已结算实际费用合计放行——待审批不占额度，出资账户余额充足
// 也不能让两笔合计超过累计上限的批准都成功。
//
// 固定场景（见 setupSharedQuotaApproveContest）：
//   - 出资账户 payer 初始可用余额 200、预留 0；使用账户 u1/u2 各自余额为 0、
//     分别持有绑定自有设备的有效会话 s1/d1、s2/d2；
//   - 策略 p-shared 允许 u1、u2 申请：单次上限 60、共享累计上限 100、
//     审批门槛 10、审批等待 1 小时，关闭预留超时；
//   - u1、u2 各提交一笔预估费用 60 的申请，两笔使用相同请求编号 share-r，
//     但请求按使用账户维度识别，因此仍是各自账户下相互独立的两笔请求；
//   - 两笔费用严格超过门槛且申请时现存预留与已花费均为 0（0+60 ≤ 100），
//     都进入待审批：payer 仍为可用 200 / 预留 0，策略预留与已花费均为 0，
//     账本只有两条零金额待审批留痕。
const (
	contestPolicyID        = "p-shared"
	contestRequestID       = "share-r"
	contestFee       int64 = 60
	contestActualFee int64 = 40
	contestPayerInit int64 = 200
	contestPerReq    int64 = 60
	contestMaxTotal  int64 = 100
	contestThreshold int64 = 10
)

// setupSharedQuotaApproveContest 构造上述固定场景，提交两笔申请并断言起点：
// 两笔均待审批、提交与等待截止时间各自独立记录；payer 可用 200 / 预留 0；
// 策略预留总额 0、已花费总额 0；账本恰有两条待审批留痕，无任何资金记录。
// 返回提交时刻（两笔相同）与等待截止时刻（两笔相同）。
func setupSharedQuotaApproveContest(t *testing.T) (*Wallet, *clock, time.Time, time.Time) {
	t.Helper()
	w, c := newTestWallet()
	mustAccount(t, w, "payer", contestPayerInit)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "d1", c.t.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s2", "u2", "d2", c.t.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "d-approve", c.t.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(PolicySpec{
		ID:                contestPolicyID,
		PayerAccountID:    "payer",
		AllowedAccountIDs: []string{"u1", "u2"},
		Operation:         "charge",
		Payee:             "shop",
		StartsAt:          c.t.Add(-time.Hour),
		EndsAt:            c.t.Add(24 * time.Hour),
		MaxPerRequest:     contestPerReq,
		MaxTotal:          contestMaxTotal,
		ApprovalThreshold: contestThreshold,
		ApprovalWait:      time.Hour,
		// MaxReserveDuration 留空（零）：关闭预留超时。
	}); err != nil {
		t.Fatal(err)
	}

	apply := func(account, session, device string) RequestView {
		req, err := w.Apply(RequestInput{
			PolicyID:     contestPolicyID,
			RequestID:    contestRequestID,
			AccountID:    account,
			SessionID:    session,
			DeviceID:     device,
			Operation:    "charge",
			Payee:        "shop",
			EstimatedFee: contestFee,
		})
		if err != nil {
			t.Fatalf("apply for %s: %v", account, err)
		}
		return req
	}
	r1 := apply("u1", "s1", "d1")
	r2 := apply("u2", "s2", "d2")
	if r1.State != RequestPendingApproval || r2.State != RequestPendingApproval {
		t.Fatalf("both requests must be pending approval, got %v and %v", r1.State, r2.State)
	}
	if !r1.CreatedAt.Equal(c.t) || !r2.CreatedAt.Equal(c.t) {
		t.Fatalf("created at = %v / %v, want %v", r1.CreatedAt, r2.CreatedAt, c.t)
	}
	wantDeadline := c.t.Add(time.Hour)
	if !r1.WaitDeadline.Equal(wantDeadline) || !r2.WaitDeadline.Equal(wantDeadline) {
		t.Fatalf("wait deadlines = %v / %v, want %v", r1.WaitDeadline, r2.WaitDeadline, wantDeadline)
	}
	// 待审批不允许有任何预留或决定信息。
	for _, r := range []RequestView{r1, r2} {
		if !r.ReservedAt.IsZero() || !r.DecidedAt.IsZero() || r.ApproverAccountID != "" || r.RejectReason != "" {
			t.Fatalf("pending request must carry no reservation or decision: %+v", r)
		}
	}

	// 待审批不冻结余额、不占用共享累计额度：出资账户仍有 200 可用、零预留，
	// 策略预留总额与已花费总额也都为零。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: contestPayerInit, Reserved: 0}) {
		t.Fatalf("payer balance after two pending applies = %+v, want {200 0}", bal)
	}
	if pv, _ := w.Policy(contestPolicyID); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals after two pending applies = reserved %d spent %d, want 0/0",
			pv.ReservedTotal, pv.SpentTotal)
	}
	// 账本只有两条零金额待审批留痕，出资账户名下没有任何资金记录。
	all := w.Ledger()
	if len(all) != 2 {
		t.Fatalf("ledger after applies = %d entries, want 2 pending entries: %+v", len(all), all)
	}
	for _, e := range all {
		if e.Kind != LedgerPendingApproval || e.Amount != 0 {
			t.Fatalf("entry = %+v, want zero-amount pending-approval entry", e)
		}
	}
	if got := countKind(all, LedgerPendingApproval); got != 2 {
		t.Fatalf("pending entries = %d, want 2", got)
	}
	if len(w.AccountLedger("payer")) != 0 {
		t.Fatalf("pending approvals must not create payer money entries: %+v", w.AccountLedger("payer"))
	}
	return w, c, c.t, wantDeadline
}

// contestRequests 按账户编号返回竞争中的两笔请求（同请求编号、不同账户）。
func contestRequests(t *testing.T, w *Wallet) (u1req, u2req RequestView) {
	t.Helper()
	r1, err := w.Request("u1", contestRequestID)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := w.Request("u2", contestRequestID)
	if err != nil {
		t.Fatal(err)
	}
	return r1, r2
}

// checkContestOutcome 校验“两笔待审批同时批准”后的统一结果，不预设胜出者：
//   - winner 账户下的请求已预留 60：带批准决定（出资账户、批准时刻），
//     预留时刻即批准时刻；提交时间与等待截止时间保持申请时原值；
//   - loser 账户下的请求仍待审批：没有预留计时或任何审批决定字段，提交
//     时间与等待截止时间保持原值（失败批准不推进期限、不开始预留计时）；
//   - payer 可用 140 / 预留 60；策略预留总额 60、已花费总额 0；
//   - 原有两条待审批留痕保留；只为胜出请求新增一条 payer 的 60 预留记录与
//     一条使用账户的零金额批准记录；失败批准不新增资金记录或审批决定。
func checkContestOutcome(t *testing.T, w *Wallet, winner, loser string,
	createdAt, waitDeadline, approveAt time.Time) {
	t.Helper()
	if winner == loser {
		t.Fatalf("winner and loser must be different accounts, got %q", winner)
	}

	winReq, err := w.Request(winner, contestRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if winReq.State != RequestReserved {
		t.Fatalf("winner %s state = %v, want reserved", winner, winReq.State)
	}
	if winReq.EstimatedFee != contestFee {
		t.Fatalf("winner estimated fee = %d, want 60", winReq.EstimatedFee)
	}
	if winReq.ApproverAccountID != "payer" {
		t.Fatalf("winner approver = %q, want payer", winReq.ApproverAccountID)
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

	loseReq, err := w.Request(loser, contestRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if loseReq.State != RequestPendingApproval {
		t.Fatalf("loser %s state = %v, want still pending approval (must not become rejected)",
			loser, loseReq.State)
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
	if loseReq.EstimatedFee != contestFee {
		t.Fatalf("loser estimated fee = %d, want 60", loseReq.EstimatedFee)
	}

	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 140, Reserved: 60}) {
		t.Fatalf("payer balance = %+v, want {140 60}", bal)
	}
	if pv, _ := w.Policy(contestPolicyID); pv.ReservedTotal != 60 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 60/0", pv.ReservedTotal, pv.SpentTotal)
	}

	// 账本：两条原待审批留痕 + 胜出请求的一条 60 预留 + 一条零金额批准，
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
	res, ok := ledgerEntryFor(all, LedgerReserve, contestRequestID)
	if !ok {
		t.Fatal("missing reserve entry for winner")
	}
	if res.AccountID != "payer" || res.Amount != 60 || !res.At.Equal(approveAt) {
		t.Fatalf("winner reserve entry = %+v, want payer 60 at approve time", res)
	}
	app, ok := ledgerEntryFor(all, LedgerApproval, contestRequestID)
	if !ok {
		t.Fatal("missing zero-amount approval entry for winner")
	}
	if app.AccountID != winner || app.Amount != 0 || !app.At.Equal(approveAt) {
		t.Fatalf("winner approval entry = %+v, want zero-amount entry for %s at approve time", app, winner)
	}
	// 两条待审批留痕仍分别归属 u1、u2，失败批准没有把失败方转成拒绝留痕。
	pendingAccounts := map[string]int{}
	for _, e := range all {
		if e.Kind == LedgerPendingApproval {
			pendingAccounts[e.AccountID]++
		}
	}
	if pendingAccounts["u1"] != 1 || pendingAccounts["u2"] != 1 {
		t.Fatalf("pending traces = %+v, want one each for u1 and u2", pendingAccounts)
	}
}

// approveContest 用出资账户的有效审批会话批准指定使用账户的竞争请求。
func approveContest(w *Wallet, account string) (RequestView, error) {
	return w.Approve(account, contestRequestID, "sa", "d-approve")
}

// TestSequentialContestedApprovalsOnlyOneSucceeds 顺序复现争用：在策略与会话
// 有效、等待期限未到的同一时刻连续批准两笔待审批请求，只能有一笔成功预留
// 60，另一笔返回可识别的 ErrQuotaExceeded 并继续待审批——不能转成拒绝终态，
// 也不能因出资账户余额充足（200 ≥ 120）就让两笔都获批。
func TestSequentialContestedApprovalsOnlyOneSucceeds(t *testing.T) {
	for _, order := range []struct {
		name        string
		first       string
		second      string
		firstOK     bool
		secondQuota bool
	}{
		{"u1-first", "u1", "u2", true, true},
		{"u2-first", "u2", "u1", true, true},
	} {
		t.Run(order.name, func(t *testing.T) {
			w, c, createdAt, waitDeadline := setupSharedQuotaApproveContest(t)
			approveAt := c.t.Add(10 * time.Second)
			c.t = approveAt

			first, err := approveContest(w, order.first)
			if err != nil {
				t.Fatalf("first approve by %s: %v", order.first, err)
			}
			if first.State != RequestReserved {
				t.Fatalf("first approve state = %v, want reserved", first.State)
			}
			// 第二笔：payer 可用 140 ≥ 60，余额充足，但共享额度 60+60 = 120 > 100，
			// 必须按超额度失败而非余额不足。
			if _, err := approveContest(w, order.second); !errors.Is(err, ErrQuotaExceeded) {
				t.Fatalf("second approve by %s err = %v, want ErrQuotaExceeded", order.second, err)
			} else if errors.Is(err, ErrInsufficientBalance) {
				t.Fatalf("second approve must fail on shared quota, not payer balance: %v", err)
			}
			checkContestOutcome(t, w, order.first, order.second, createdAt, waitDeadline, approveAt)

			// 期限内立即再次批准失败方：额度仍被胜出预留占用，依旧超额度、
			// 依旧待审批，账本不增长。
			ledgerBefore := len(w.Ledger())
			if _, err := approveContest(w, order.second); !errors.Is(err, ErrQuotaExceeded) {
				t.Fatalf("retry approve by %s err = %v, want ErrQuotaExceeded", order.second, err)
			}
			if len(w.Ledger()) != ledgerBefore {
				t.Fatalf("retry approve grew ledger %d -> %d", ledgerBefore, len(w.Ledger()))
			}
			checkContestOutcome(t, w, order.first, order.second, createdAt, waitDeadline, approveAt)
		})
	}
}

// raceApproveBias 控制两个批准 goroutine 的起跑方式，使“u1 先获批”与
// “u2 先获批”两种先后顺序都能在并发 harness 下被确定性地观察到：偏向模式下
// 后一个 goroutine 等前一个的 Approve 完全返回后才起跑，先后顺序不依赖
// 调度器或睡眠时间。
type raceApproveBias int

const (
	approveSimultaneous raceApproveBias = iota
	approveU1First
	approveU2First
)

// raceContestedApprovals 用出资账户的同一个有效审批会话从两个 goroutine
// 同时批准 u1、u2 的待审批请求，断言恰有一笔成功、另一笔 ErrQuotaExceeded，
// 返回胜出与失败的使用账户编号。bias 用于确定性地覆盖两种先后顺序。
func raceContestedApprovals(t *testing.T, w *Wallet, bias raceApproveBias) (winner, loser string) {
	t.Helper()
	accounts := []string{"u1", "u2"}
	views := make([]RequestView, 2)
	errs := make([]error, 2)
	gates := []chan struct{}{make(chan struct{}), make(chan struct{})}
	done := []chan struct{}{make(chan struct{}), make(chan struct{})}

	var wg sync.WaitGroup
	for i, account := range accounts {
		i, account := i, account
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gates[i]
			views[i], errs[i] = approveContest(w, account)
			close(done[i])
		}()
	}
	// 偏向模式下先放行指定的一方，并等它的批准完整结束后再放行另一方，
	// 确定性地产生先后顺序；同时模式下两个闸门一起关闭。
	switch bias {
	case approveU1First:
		close(gates[0])
		<-done[0]
		close(gates[1])
	case approveU2First:
		close(gates[1])
		<-done[1]
		close(gates[0])
	default:
		close(gates[0])
		close(gates[1])
	}
	wg.Wait()

	successes := 0
	for i, account := range accounts {
		switch {
		case errs[i] == nil:
			successes++
			winner = account
			if views[i].State != RequestReserved || views[i].ApproverAccountID != "payer" {
				t.Fatalf("winning approve view for %s = %+v, want reserved by payer", account, views[i])
			}
		case errors.Is(errs[i], ErrQuotaExceeded):
			loser = account
		default:
			t.Fatalf("approve for %s err = %v, want nil or ErrQuotaExceeded", account, errs[i])
		}
	}
	if successes != 1 || winner == "" || loser == "" {
		t.Fatalf("contested approvals must yield exactly one success and one quota error, got winner %q loser %q errs %v",
			winner, loser, errs)
	}
	return winner, loser
}

// TestConcurrentContestedApprovalsAcrossAccounts 并发复现题述场景：出资账户
// 用有效审批会话同时批准两个不同使用账户的同号待审批请求时，只能有一笔
// 成功预留 60，另一笔返回 ErrQuotaExceeded 并继续待审批；无需指定哪名使用
// 账户先获批——u1 胜出与 u2 胜出两种结果都必须符合同一规则。三种起跑方式
// 各跑多轮，并要求两个账户都至少胜出一次，确保两种先后顺序都被真正验证。
func TestConcurrentContestedApprovalsAcrossAccounts(t *testing.T) {
	const iterations = 24
	wins := map[string]int{}
	for _, bias := range []struct {
		name string
		mode raceApproveBias
	}{
		{"simultaneous", approveSimultaneous},
		{"u1-first", approveU1First},
		{"u2-first", approveU2First},
	} {
		for i := 0; i < iterations; i++ {
			w, c, createdAt, waitDeadline := setupSharedQuotaApproveContest(t)
			approveAt := c.t.Add(10 * time.Second)
			c.t = approveAt

			winner, loser := raceContestedApprovals(t, w, bias.mode)
			wins[winner]++
			checkContestOutcome(t, w, winner, loser, createdAt, waitDeadline, approveAt)
		}
	}
	if len(wins) != 2 {
		t.Fatalf("each usage account must win at least once, got winner distribution %v", wins)
	}
	t.Logf("winner distribution: %v", wins)
}

// TestContestedApproveSettleFreesQuotaForOther 验证题述的完整后续：胜出请求
// 按实际费用 40 结算后，差额 20 退回出资账户，策略只保留 40 已花费额度；
// 在等待期限内再批准另一笔时，现存预留 0 + 已花费 40 + 本次预留 60 恰好
// 等于累计上限 100，必须允许批准。最终 payer 可用 100 / 预留 60，策略
// 已花费 40 / 预留 60；前一笔保持已结算、后一笔成为已预留，账本中的扣减
// 与退款能够解释全部资金变化。两种胜出顺序适用同一规则，这里都覆盖。
func TestContestedApproveSettleFreesQuotaForOther(t *testing.T) {
	for _, tc := range []struct {
		name   string
		winner string
		loser  string
	}{
		{"u1-wins-then-u2-approved", "u1", "u2"},
		{"u2-wins-then-u1-approved", "u2", "u1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, c, createdAt, waitDeadline := setupSharedQuotaApproveContest(t)
			approveAt := c.t.Add(10 * time.Second)
			c.t = approveAt

			// 并发同时批准，用起跑偏向确定性地产生用例指定的胜出顺序；
			// 两种胜出结果的统一规则已在并发测试中覆盖。
			bias := approveU1First
			if tc.winner == "u2" {
				bias = approveU2First
			}
			winner, loser := raceContestedApprovals(t, w, bias)
			if winner != tc.winner || loser != tc.loser {
				t.Fatalf("biased race winner = %s loser = %s, want %s/%s", winner, loser, tc.winner, tc.loser)
			}
			checkContestOutcome(t, w, winner, loser, createdAt, waitDeadline, approveAt)

			// 胜出请求按实际费用 40 结算：扣 40、差额 20 退回 payer；
			// 策略预留总额清零、已花费总额 40。
			settleAt := approveAt.Add(10 * time.Second)
			c.t = settleAt
			settled, err := w.Settle(winner, contestRequestID, contestActualFee)
			if err != nil {
				t.Fatalf("settle winner %s: %v", winner, err)
			}
			if settled.State != RequestSettled || settled.ActualFee != contestActualFee {
				t.Fatalf("settled view = %+v, want settled actual 40", settled)
			}
			if bal, _ := w.Balance("payer"); bal != (Balances{Available: 160, Reserved: 0}) {
				t.Fatalf("payer balance after settle = %+v, want {160 0}", bal)
			}
			if pv, _ := w.Policy(contestPolicyID); pv.ReservedTotal != 0 || pv.SpentTotal != 40 {
				t.Fatalf("policy totals after settle = reserved %d spent %d, want 0/40",
					pv.ReservedTotal, pv.SpentTotal)
			}

			// 失败方仍待审批，提交时间与等待截止时间保持原值，且仍在期限内
			// （批准时刻早于 waitDeadline）。
			pending, err := w.Request(loser, contestRequestID)
			if err != nil {
				t.Fatal(err)
			}
			if pending.State != RequestPendingApproval {
				t.Fatalf("loser state after settle = %v, want still pending", pending.State)
			}
			if !pending.CreatedAt.Equal(createdAt) || !pending.WaitDeadline.Equal(waitDeadline) {
				t.Fatalf("loser timing changed before second approve: %+v", pending)
			}
			if !settleAt.Before(waitDeadline) {
				t.Fatalf("test setup: settle time %v must be within wait deadline %v", settleAt, waitDeadline)
			}

			// 等待期限内再批准失败方：40 已花费 + 60 本次预留恰为 100 上限，
			// 必须允许；余额 160 也足以预留 60。
			secondApproveAt := settleAt.Add(10 * time.Second)
			c.t = secondApproveAt
			if !secondApproveAt.Before(waitDeadline) {
				t.Fatalf("test setup: second approve %v must be within wait deadline %v", secondApproveAt, waitDeadline)
			}
			approved, err := approveContest(w, loser)
			if err != nil {
				t.Fatalf("approve loser %s after quota freed by settle: %v", loser, err)
			}
			if approved.State != RequestReserved {
				t.Fatalf("second approved state = %v, want reserved", approved.State)
			}

			// 最终：payer 可用 100 / 预留 60；策略已花费 40 / 预留 60。
			if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 60}) {
				t.Fatalf("final payer balance = %+v, want {100 60}", bal)
			}
			pv, _ := w.Policy(contestPolicyID)
			if pv.ReservedTotal != 60 || pv.SpentTotal != 40 {
				t.Fatalf("final policy totals = reserved %d spent %d, want 60/40", pv.ReservedTotal, pv.SpentTotal)
			}
			if pv.ReservedTotal+pv.SpentTotal != contestMaxTotal {
				t.Fatalf("reserved+spent = %d, want exactly shared cap 100", pv.ReservedTotal+pv.SpentTotal)
			}

			// 请求终态：前一笔保持已结算，后一笔成为已预留。
			firstReq, err := w.Request(winner, contestRequestID)
			if err != nil {
				t.Fatal(err)
			}
			if firstReq.State != RequestSettled || firstReq.ActualFee != 40 {
				t.Fatalf("winner final = %+v, want settled actual 40", firstReq)
			}
			secondReq, err := w.Request(loser, contestRequestID)
			if err != nil {
				t.Fatal(err)
			}
			if secondReq.State != RequestReserved || secondReq.ActualFee != 0 {
				t.Fatalf("loser final = %+v, want reserved with no actual fee", secondReq)
			}
			if secondReq.ApproverAccountID != "payer" || !secondReq.ReservedAt.Equal(secondApproveAt) {
				t.Fatalf("second approval decision fields = %+v", secondReq)
			}
			// 两笔的提交时间与等待截止时间始终保持申请时原值。
			for _, r := range []RequestView{firstReq, secondReq} {
				if !r.CreatedAt.Equal(createdAt) || !r.WaitDeadline.Equal(waitDeadline) {
					t.Fatalf("request timing changed: %+v", r)
				}
			}

			// 账本逐条核对（按追加顺序）：
			//  1. u1 待审批留痕（0）
			//  2. u2 待审批留痕（0）
			//  3. 胜出批准预留：payer -60（LedgerReserve, 60）
			//  4. 胜出批准留痕：winner（LedgerApproval, 0）
			//  5. 胜出结算扣减：payer（LedgerSettle, 40）
			//  6. 胜出差额退款：payer（LedgerRefund, 20）
			//  7. 第二笔批准预留：payer -60（LedgerReserve, 60）
			//  8. 第二笔批准留痕：loser（LedgerApproval, 0）
			all := w.Ledger()
			if len(all) != 8 {
				t.Fatalf("final ledger entries = %d, want 8: %+v", len(all), all)
			}
			if all[0].Kind != LedgerPendingApproval || all[0].AccountID != "u1" || all[0].Amount != 0 {
				t.Fatalf("entry 0 = %+v, want u1 pending", all[0])
			}
			if all[1].Kind != LedgerPendingApproval || all[1].AccountID != "u2" || all[1].Amount != 0 {
				t.Fatalf("entry 1 = %+v, want u2 pending", all[1])
			}
			if all[2].Kind != LedgerReserve || all[2].AccountID != "payer" || all[2].Amount != 60 || !all[2].At.Equal(approveAt) {
				t.Fatalf("entry 2 = %+v, want payer reserve 60 at first approve", all[2])
			}
			if all[3].Kind != LedgerApproval || all[3].AccountID != winner || all[3].Amount != 0 {
				t.Fatalf("entry 3 = %+v, want zero-amount approval for winner %s", all[3], winner)
			}
			if all[4].Kind != LedgerSettle || all[4].AccountID != "payer" || all[4].Amount != 40 || !all[4].At.Equal(settleAt) {
				t.Fatalf("entry 4 = %+v, want payer settle 40", all[4])
			}
			if all[5].Kind != LedgerRefund || all[5].AccountID != "payer" || all[5].Amount != 20 || !all[5].At.Equal(settleAt) {
				t.Fatalf("entry 5 = %+v, want payer refund 20", all[5])
			}
			if all[6].Kind != LedgerReserve || all[6].AccountID != "payer" || all[6].Amount != 60 || !all[6].At.Equal(secondApproveAt) {
				t.Fatalf("entry 6 = %+v, want payer reserve 60 at second approve", all[6])
			}
			if all[7].Kind != LedgerApproval || all[7].AccountID != loser || all[7].Amount != 0 {
				t.Fatalf("entry 7 = %+v, want zero-amount approval for loser %s", all[7], loser)
			}

			// 出资账户账本的资金求和解释最终余额：
			// 预留不改变可用总额；扣减 -40；退款 +20；现存预留合计 60。
			// 可用 = 200 - 40 + 20 - 60 = 100，预留 = 60 + 60 - 60 = 60。
			payerLedger := w.AccountLedger("payer")
			var reservedSum, settledSum, refundedSum int64
			for _, e := range payerLedger {
				switch e.Kind {
				case LedgerReserve:
					reservedSum += e.Amount
				case LedgerSettle:
					settledSum += e.Amount
				case LedgerRefund:
					refundedSum += e.Amount
				default:
					t.Fatalf("unexpected payer ledger entry: %+v", e)
				}
			}
			if reservedSum != 120 || settledSum != 40 || refundedSum != 20 {
				t.Fatalf("payer money sums = reserved %d settled %d refunded %d, want 120/40/20",
					reservedSum, settledSum, refundedSum)
			}
			// 可用 = 初始 - 两次预留 120 + 已结算退回的差额 20 = 100；
			// 预留余额 = 预留合计 120 - 结算释放的（扣减 40 + 退款 20）= 60。
			wantAvailable := contestPayerInit - reservedSum + refundedSum
			wantReserved := reservedSum - (settledSum + refundedSum)
			if wantAvailable != 100 || wantReserved != 60 {
				t.Fatalf("ledger sums recompute balances = available %d reserved %d, want 100/60",
					wantAvailable, wantReserved)
			}
			if bal, _ := w.Balance("payer"); bal.Available != wantAvailable || bal.Reserved != wantReserved {
				t.Fatalf("ledger sums do not explain balances: %+v", bal)
			}
		})
	}
}
