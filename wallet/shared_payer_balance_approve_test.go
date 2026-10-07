package wallet

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 本文件为大额代付批准功能补充回归保障：每条策略有自己的累计额度，但出资
// 余额属于账户。两个使用账户通过两条不同策略提交的待审批请求，同时批准时
// 竞争的是同一个出资账户的可用余额——两条策略各自的累计额度都充足，也不
// 能让两笔合计超过出资余额的批准都成功；同样不能把不同策略的费用合并成
// 同一条策略的额度占用而误报额度超限。
//
// 固定场景（见 setupSharedPayerApproveContest）：
//   - 出资账户 payer 初始可用余额由用例给定（70 或恰为 90）、预留 0；
//     使用账户 u1/u2 余额为 0，分别持有绑定自有设备的有效会话 s1/d1、s2/d2；
//   - 策略 p-fee50 允许 u1、策略 p-fee40 允许 u2，出资账户同为 payer，
//     操作与收款方相同：单次上限各 60、累计上限各 100、审批门槛各 10、
//     审批等待各 1 小时，都关闭预留超时；
//   - u1 经 p-fee50 提交预估费用 50 的申请 r-50，u2 经 p-fee40 提交预估
//     费用 40 的申请 r-40；两笔都严格超过门槛且各自策略额度充足，都进入
//     待审批：payer 余额不变，两条策略的预留与已花费均为 0，账本只有两条
//     零金额待审批留痕。
const (
	sharePayerPolicy50     = "p-fee50"
	sharePayerPolicy40     = "p-fee40"
	sharePayerRequest50    = "r-50"
	sharePayerRequest40    = "r-40"
	sharePayerFee50  int64 = 50
	sharePayerFee40  int64 = 40
	sharePayerPerReq int64 = 60
	sharePayerMax    int64 = 100
	sharePayerThresh int64 = 10
)

// payerContestSide 描述竞争中一笔请求的定位信息（账户、策略、请求编号、费用）。
type payerContestSide struct {
	account   string
	policyID  string
	requestID string
	fee       int64
}

var (
	sharePayerSide50 = payerContestSide{"u1", sharePayerPolicy50, sharePayerRequest50, sharePayerFee50}
	sharePayerSide40 = payerContestSide{"u2", sharePayerPolicy40, sharePayerRequest40, sharePayerFee40}
)

// setupSharedPayerApproveContest 构造上述固定场景，提交两笔申请并断言起点：
// 两笔均待审批、提交与等待截止时间各自记录；payer 可用 payerInit / 预留 0；
// 两条策略的预留总额与已花费总额都为 0；账本恰有两条待审批留痕，出资账户
// 名下没有任何资金记录。返回提交时刻（两笔相同）与等待截止时刻（两笔相同）。
func setupSharedPayerApproveContest(t *testing.T, payerInit int64) (*Wallet, *clock, time.Time, time.Time) {
	t.Helper()
	w, c := newTestWallet()
	mustAccount(t, w, "payer", payerInit)
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
	// 两条策略共用出资账户，其余配置相同，仅编号与授权账户不同。
	for _, p := range []PolicySpec{
		{ID: sharePayerPolicy50, AllowedAccountIDs: []string{"u1"}},
		{ID: sharePayerPolicy40, AllowedAccountIDs: []string{"u2"}},
	} {
		p.PayerAccountID = "payer"
		p.Operation = "charge"
		p.Payee = "shop"
		p.StartsAt = c.t.Add(-time.Hour)
		p.EndsAt = c.t.Add(24 * time.Hour)
		p.MaxPerRequest = sharePayerPerReq
		p.MaxTotal = sharePayerMax
		p.ApprovalThreshold = sharePayerThresh
		p.ApprovalWait = time.Hour
		// MaxReserveDuration 留空（零）：关闭预留超时。
		if err := w.SavePolicy(p); err != nil {
			t.Fatal(err)
		}
	}

	apply := func(side payerContestSide, session, device string) RequestView {
		req, err := w.Apply(RequestInput{
			PolicyID:     side.policyID,
			RequestID:    side.requestID,
			AccountID:    side.account,
			SessionID:    session,
			DeviceID:     device,
			Operation:    "charge",
			Payee:        "shop",
			EstimatedFee: side.fee,
		})
		if err != nil {
			t.Fatalf("apply for %s: %v", side.account, err)
		}
		return req
	}
	r50 := apply(sharePayerSide50, "s1", "d1")
	r40 := apply(sharePayerSide40, "s2", "d2")
	if r50.State != RequestPendingApproval || r40.State != RequestPendingApproval {
		t.Fatalf("both requests must be pending approval, got %v and %v", r50.State, r40.State)
	}
	if !r50.CreatedAt.Equal(c.t) || !r40.CreatedAt.Equal(c.t) {
		t.Fatalf("created at = %v / %v, want %v", r50.CreatedAt, r40.CreatedAt, c.t)
	}
	wantDeadline := c.t.Add(time.Hour)
	if !r50.WaitDeadline.Equal(wantDeadline) || !r40.WaitDeadline.Equal(wantDeadline) {
		t.Fatalf("wait deadlines = %v / %v, want %v", r50.WaitDeadline, r40.WaitDeadline, wantDeadline)
	}
	// 待审批不允许有任何预留或决定信息。
	for _, r := range []RequestView{r50, r40} {
		if !r.ReservedAt.IsZero() || !r.DecidedAt.IsZero() || r.ApproverAccountID != "" || r.RejectReason != "" {
			t.Fatalf("pending request must carry no reservation or decision: %+v", r)
		}
	}

	// 待审批不冻结余额、不占用任何策略的累计额度：出资账户余额不变，
	// 两条策略的预留总额与已花费总额都为零。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: payerInit, Reserved: 0}) {
		t.Fatalf("payer balance after two pending applies = %+v, want {%d 0}", bal, payerInit)
	}
	for _, pid := range []string{sharePayerPolicy50, sharePayerPolicy40} {
		if pv, _ := w.Policy(pid); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
			t.Fatalf("policy %s totals after two pending applies = reserved %d spent %d, want 0/0",
				pid, pv.ReservedTotal, pv.SpentTotal)
		}
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
	if len(w.AccountLedger("payer")) != 0 {
		t.Fatalf("pending approvals must not create payer money entries: %+v", w.AccountLedger("payer"))
	}
	return w, c, c.t, wantDeadline
}

// checkSharedPayerOutcome 校验“两笔待审批同时批准、恰有一笔成功”后的统一
// 结果，不预设胜出者：
//   - winner 一侧的请求已预留其费用：带出资账户的批准决定，预留时刻即批准
//     时刻；提交时间与等待截止时间保持申请时原值；
//   - loser 一侧的请求仍待审批：没有预留计时或任何审批决定字段，提交时间
//     与等待截止时间保持原值（失败批准不推进期限、不开始预留计时）；
//   - payer 可用 payerInit-winner.fee / 预留 winner.fee；只有 winner 所属
//     策略的预留总额增加对应费用，loser 所属策略保持零预留，两条策略的
//     已花费总额都不增加；
//   - 原有两条待审批留痕保留；只为胜出请求新增一条 payer 的全额预留记录与
//     一条使用账户的零金额批准记录；失败批准不新增资金记录或审批决定。
func checkSharedPayerOutcome(t *testing.T, w *Wallet, payerInit int64,
	winner, loser payerContestSide, createdAt, waitDeadline, approveAt time.Time) {
	t.Helper()
	if winner == loser {
		t.Fatalf("winner and loser must be different sides, got %+v", winner)
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
	if winReq.PolicyID != winner.policyID {
		t.Fatalf("winner policy = %q, want %q", winReq.PolicyID, winner.policyID)
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

	// 出资余额属于账户：胜出预留从 payer 可用余额划出，与策略条数无关。
	wantBal := Balances{Available: payerInit - winner.fee, Reserved: winner.fee}
	if bal, _ := w.Balance("payer"); bal != wantBal {
		t.Fatalf("payer balance = %+v, want %+v", bal, wantBal)
	}
	// 只有胜出请求所属策略增加对应预留；另一条策略保持零预留；
	// 两条策略的已花费总额都不增加。
	if pv, _ := w.Policy(winner.policyID); pv.ReservedTotal != winner.fee || pv.SpentTotal != 0 {
		t.Fatalf("winner policy %s totals = reserved %d spent %d, want %d/0",
			winner.policyID, pv.ReservedTotal, pv.SpentTotal, winner.fee)
	}
	if pv, _ := w.Policy(loser.policyID); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("loser policy %s totals = reserved %d spent %d, want 0/0",
			loser.policyID, pv.ReservedTotal, pv.SpentTotal)
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
		t.Fatal("missing reserve entry for winner")
	}
	if res.AccountID != "payer" || res.Amount != winner.fee || !res.At.Equal(approveAt) {
		t.Fatalf("winner reserve entry = %+v, want payer %d at approve time", res, winner.fee)
	}
	app, ok := ledgerEntryFor(all, LedgerApproval, winner.requestID)
	if !ok {
		t.Fatal("missing zero-amount approval entry for winner")
	}
	if app.AccountID != winner.account || app.Amount != 0 || !app.At.Equal(approveAt) {
		t.Fatalf("winner approval entry = %+v, want zero-amount entry for %s at approve time", app, winner.account)
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

// approveSharedPayer 用出资账户的有效审批会话批准指定一侧的竞争请求。
func approveSharedPayer(w *Wallet, side payerContestSide) (RequestView, error) {
	return w.Approve(side.account, side.requestID, "sa", "d-approve")
}

// TestSharedPayerSequentialContestedApprovals 顺序复现跨策略争用：出资账户
// 可用 70，两条策略各自的累计额度都充足（50 ≤ 100、40 ≤ 100），但两笔费用
// 合计 90 超过出资余额。同一时刻连续批准两笔待审批请求，只能有一笔成功
// 预留，另一笔返回 ErrInsufficientBalance 并继续待审批——不能误报额度超限
// （每条策略自己的额度都够），也不能转成拒绝终态。两种先后顺序都覆盖：
// 50 先获批时余额为 20 可用 / 50 预留，40 先获批时为 30 可用 / 40 预留。
func TestSharedPayerSequentialContestedApprovals(t *testing.T) {
	const payerInit int64 = 70
	for _, order := range []struct {
		name   string
		first  payerContestSide
		second payerContestSide
	}{
		{"fee50-first", sharePayerSide50, sharePayerSide40},
		{"fee40-first", sharePayerSide40, sharePayerSide50},
	} {
		t.Run(order.name, func(t *testing.T) {
			w, c, createdAt, waitDeadline := setupSharedPayerApproveContest(t, payerInit)
			approveAt := c.t.Add(10 * time.Second)
			c.t = approveAt

			first, err := approveSharedPayer(w, order.first)
			if err != nil {
				t.Fatalf("first approve of %s: %v", order.first.requestID, err)
			}
			if first.State != RequestReserved {
				t.Fatalf("first approve state = %v, want reserved", first.State)
			}
			// 第二笔：两条策略各自的累计额度都充足，但第一笔预留后 payer
			// 可用余额已不足第二笔费用，必须按余额不足失败而非额度超限。
			if _, err := approveSharedPayer(w, order.second); !errors.Is(err, ErrInsufficientBalance) {
				t.Fatalf("second approve of %s err = %v, want ErrInsufficientBalance", order.second.requestID, err)
			} else if errors.Is(err, ErrQuotaExceeded) {
				t.Fatalf("second approve must fail on payer balance, not policy quota: %v", err)
			}
			checkSharedPayerOutcome(t, w, payerInit, order.first, order.second, createdAt, waitDeadline, approveAt)

			// 期限内立即再次批准失败方：余额仍被胜出预留占用，依旧余额不足、
			// 依旧待审批，账本不增长。
			ledgerBefore := len(w.Ledger())
			if _, err := approveSharedPayer(w, order.second); !errors.Is(err, ErrInsufficientBalance) {
				t.Fatalf("retry approve of %s err = %v, want ErrInsufficientBalance", order.second.requestID, err)
			}
			if len(w.Ledger()) != ledgerBefore {
				t.Fatalf("retry approve grew ledger %d -> %d", ledgerBefore, len(w.Ledger()))
			}
			checkSharedPayerOutcome(t, w, payerInit, order.first, order.second, createdAt, waitDeadline, approveAt)
		})
	}
}

// payerRaceBias 控制两个批准 goroutine 的起跑方式，使“50 先获批”与“40 先
// 获批”两种先后顺序都能在并发 harness 下被确定性地观察到：偏向模式下后一
// 个 goroutine 等前一个的 Approve 完全返回后才起跑，先后顺序不依赖调度器
// 或睡眠时间。
type payerRaceBias int

const (
	payerRaceSimultaneous payerRaceBias = iota
	payerRace50First
	payerRace40First
)

// raceSharedPayerApprovals 用出资账户的同一个有效审批会话从两个 goroutine
// 同时批准两笔跨策略的待审批请求，断言恰有一笔成功、另一笔返回
// ErrInsufficientBalance，返回胜出与失败的一侧。bias 用于确定性地覆盖两种
// 先后顺序。
func raceSharedPayerApprovals(t *testing.T, w *Wallet, bias payerRaceBias) (winner, loser payerContestSide) {
	t.Helper()
	sides := []payerContestSide{sharePayerSide50, sharePayerSide40}
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
			views[i], errs[i] = approveSharedPayer(w, side)
			close(done[i])
		}()
	}
	// 偏向模式下先放行指定的一侧，并等它的批准完整结束后再放行另一侧，
	// 确定性地产生先后顺序；同时模式下两个闸门一起关闭。
	switch bias {
	case payerRace50First:
		close(gates[0])
		<-done[0]
		close(gates[1])
	case payerRace40First:
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
			if views[i].State != RequestReserved || views[i].ApproverAccountID != "payer" {
				t.Fatalf("winning approve view for %s = %+v, want reserved by payer", side.requestID, views[i])
			}
		case errors.Is(errs[i], ErrInsufficientBalance):
			loser = side
		default:
			t.Fatalf("approve for %s err = %v, want nil or ErrInsufficientBalance", side.requestID, errs[i])
		}
	}
	if successes != 1 || winner == (payerContestSide{}) || loser == (payerContestSide{}) {
		t.Fatalf("contested approvals must yield exactly one success and one balance error, got winner %+v loser %+v errs %v",
			winner, loser, errs)
	}
	return winner, loser
}

// TestSharedPayerConcurrentContestedApprovals 并发复现题述场景：出资账户
// 可用 70，用有效审批会话同时批准两条不同策略下的待审批请求（50 与 40）
// 时，只能有一笔成功，另一笔返回 ErrInsufficientBalance 并继续待审批——
// 两笔请求来自不同策略也不能各自使用同一笔余额。无需指定哪笔先获批：
// 50 胜出与 40 胜出两种结果都必须符合同一规则。三种起跑方式各跑多轮，
// 并要求两侧都至少胜出一次，确保两种先后顺序都被真正验证。
func TestSharedPayerConcurrentContestedApprovals(t *testing.T) {
	const (
		payerInit  int64 = 70
		iterations       = 24
	)
	wins := map[string]int{}
	for _, bias := range []struct {
		name string
		mode payerRaceBias
	}{
		{"simultaneous", payerRaceSimultaneous},
		{"fee50-first", payerRace50First},
		{"fee40-first", payerRace40First},
	} {
		for i := 0; i < iterations; i++ {
			w, c, createdAt, waitDeadline := setupSharedPayerApproveContest(t, payerInit)
			approveAt := c.t.Add(10 * time.Second)
			c.t = approveAt

			winner, loser := raceSharedPayerApprovals(t, w, bias.mode)
			wins[winner.requestID]++
			checkSharedPayerOutcome(t, w, payerInit, winner, loser, createdAt, waitDeadline, approveAt)
		}
	}
	if len(wins) != 2 {
		t.Fatalf("each side must win at least once, got winner distribution %v", wins)
	}
	t.Logf("winner distribution: %v", wins)
}

// TestSharedPayerExactBalanceBothApproved 覆盖边界：出资账户可用余额恰为
// 90（两笔费用 50+40 之和），其余条件相同。两次批准都应成功：可用余额
// 恰好用尽为 0、预留余额 90，两条策略分别预留 50 和 40。各策略额度充足
// 时，不能把不同策略的费用合并成同一条策略的额度占用（任一条策略的
// 预留都不超过自己的累计上限 100），也不能因余额恰好用尽而拒绝后完成
// 的批准。两种批准顺序都覆盖。
func TestSharedPayerExactBalanceBothApproved(t *testing.T) {
	const payerInit int64 = 90
	for _, order := range []struct {
		name   string
		first  payerContestSide
		second payerContestSide
	}{
		{"fee50-first", sharePayerSide50, sharePayerSide40},
		{"fee40-first", sharePayerSide40, sharePayerSide50},
	} {
		t.Run(order.name, func(t *testing.T) {
			w, c, createdAt, waitDeadline := setupSharedPayerApproveContest(t, payerInit)
			approveAt := c.t.Add(10 * time.Second)
			c.t = approveAt

			first, err := approveSharedPayer(w, order.first)
			if err != nil {
				t.Fatalf("first approve of %s: %v", order.first.requestID, err)
			}
			if first.State != RequestReserved {
				t.Fatalf("first approve state = %v, want reserved", first.State)
			}
			// 第一笔预留后可用余额恰好等于第二笔费用：第二笔必须批准成功，
			// 不能因余额恰好用尽而被拒绝。
			second, err := approveSharedPayer(w, order.second)
			if err != nil {
				t.Fatalf("second approve of %s with exactly enough balance: %v", order.second.requestID, err)
			}
			if second.State != RequestReserved {
				t.Fatalf("second approve state = %v, want reserved", second.State)
			}

			// 两笔都已预留：各带出资账户的批准决定，预留时刻即批准时刻，
			// 提交时间与等待截止时间保持申请时原值。
			for _, side := range []payerContestSide{order.first, order.second} {
				req, err := w.Request(side.account, side.requestID)
				if err != nil {
					t.Fatal(err)
				}
				if req.State != RequestReserved || req.EstimatedFee != side.fee {
					t.Fatalf("%s view = %+v, want reserved fee %d", side.requestID, req, side.fee)
				}
				if req.ApproverAccountID != "payer" || !req.DecidedAt.Equal(approveAt) || !req.ReservedAt.Equal(approveAt) {
					t.Fatalf("%s decision/reserve timing = %+v, want payer decision at %v", side.requestID, req, approveAt)
				}
				if !req.CreatedAt.Equal(createdAt) || !req.WaitDeadline.Equal(waitDeadline) {
					t.Fatalf("%s timing changed: created %v want %v, deadline %v want %v",
						side.requestID, req.CreatedAt, createdAt, req.WaitDeadline, waitDeadline)
				}
			}

			// 余额恰好用尽：可用 0、预留 90。
			if bal, _ := w.Balance("payer"); bal != (Balances{Available: 0, Reserved: 90}) {
				t.Fatalf("payer balance = %+v, want {0 90}", bal)
			}
			// 两条策略分别预留 50 和 40，互不合并占用，已花费都为 0。
			if pv, _ := w.Policy(sharePayerPolicy50); pv.ReservedTotal != sharePayerFee50 || pv.SpentTotal != 0 {
				t.Fatalf("policy %s totals = reserved %d spent %d, want 50/0",
					sharePayerPolicy50, pv.ReservedTotal, pv.SpentTotal)
			}
			if pv, _ := w.Policy(sharePayerPolicy40); pv.ReservedTotal != sharePayerFee40 || pv.SpentTotal != 0 {
				t.Fatalf("policy %s totals = reserved %d spent %d, want 40/0",
					sharePayerPolicy40, pv.ReservedTotal, pv.SpentTotal)
			}

			// 账本：两条待审批留痕 + 两笔全额预留 + 两条零金额批准，共 6 条。
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
			for _, kind := range []LedgerKind{LedgerRejection, LedgerRefund, LedgerSettle, LedgerExpiration, LedgerCancellation} {
				if got := countKind(all, kind); got != 0 {
					t.Fatalf("ledger kind %v entries = %d, want none", kind, got)
				}
			}
			// 请求与账本相互核对：每笔请求都能找到自己的全额预留记录
			// （出资账户、批准时刻）与零金额批准记录（使用账户、批准时刻）。
			for _, side := range []payerContestSide{order.first, order.second} {
				res, ok := ledgerEntryFor(all, LedgerReserve, side.requestID)
				if !ok {
					t.Fatalf("missing reserve entry for %s", side.requestID)
				}
				if res.AccountID != "payer" || res.Amount != side.fee || !res.At.Equal(approveAt) {
					t.Fatalf("reserve entry for %s = %+v, want payer %d at approve time", side.requestID, res, side.fee)
				}
				app, ok := ledgerEntryFor(all, LedgerApproval, side.requestID)
				if !ok {
					t.Fatalf("missing approval entry for %s", side.requestID)
				}
				if app.AccountID != side.account || app.Amount != 0 || !app.At.Equal(approveAt) {
					t.Fatalf("approval entry for %s = %+v, want zero-amount entry for %s at approve time",
						side.requestID, app, side.account)
				}
			}
			// 出资账户账本的资金求和解释最终余额：可用 = 90 - 50 - 40 = 0，
			// 预留 = 50 + 40 = 90。
			var reservedSum int64
			for _, e := range w.AccountLedger("payer") {
				if e.Kind != LedgerReserve {
					t.Fatalf("unexpected payer ledger entry: %+v", e)
				}
				reservedSum += e.Amount
			}
			if reservedSum != 90 {
				t.Fatalf("payer reserve sum = %d, want 90", reservedSum)
			}
			if bal, _ := w.Balance("payer"); bal.Available != payerInit-reservedSum || bal.Reserved != reservedSum {
				t.Fatalf("ledger sums do not explain balances: %+v", bal)
			}
		})
	}
}
