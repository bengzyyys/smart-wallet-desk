package wallet

import (
	"errors"
	"testing"
	"time"
)

// queryExpirySetup 构造两个出资账户（payerA/payerB）、两个使用账户
// （u1/u2）及各自长会话的钱包，策略由调用方用 saveQueryPolicy 保存。
func queryExpirySetup(t *testing.T) (*Wallet, *clock) {
	t.Helper()
	w, c := newTestWallet()
	mustAccount(t, w, "payerA", 1000)
	mustAccount(t, w, "payerB", 1000)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s2", "u2", "dev2", c.t.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return w, c
}

// saveQueryPolicy 保存一条指定编号、出资账户与最长预留时长的策略，
// 允许 u1/u2 对 charge/shop 代付，窗口覆盖整个测试时间窗。
func saveQueryPolicy(t *testing.T, w *Wallet, c *clock, id, payer string, reserve time.Duration) {
	t.Helper()
	p := PolicySpec{
		ID:                 id,
		PayerAccountID:     payer,
		AllowedAccountIDs:  []string{"u1", "u2"},
		Operation:          "charge",
		Payee:              "shop",
		StartsAt:           c.t.Add(-time.Hour),
		EndsAt:             c.t.Add(time.Hour),
		MaxPerRequest:      100,
		MaxTotal:           1000,
		MaxReserveDuration: reserve,
	}
	if err := w.SavePolicy(p); err != nil {
		t.Fatalf("save policy %s: %v", id, err)
	}
}

// applyQuery 以 u1/u2 各自会话在指定策略下提交一笔申请。
func applyQuery(t *testing.T, w *Wallet, policy, account, request string, fee int64) {
	t.Helper()
	in := baseApply()
	in.PolicyID = policy
	in.AccountID = account
	in.RequestID = request
	in.EstimatedFee = fee
	if account == "u2" {
		in.SessionID, in.DeviceID = "s2", "dev2"
	}
	if _, err := w.Apply(in); err != nil {
		t.Fatalf("apply %s/%s under %s: %v", account, request, policy, err)
	}
}

// 查询后到期的预留（第十秒）与先被查询的、后到期的预留（第二十分）分属不同
// 使用账户、策略与出资账户：第三十秒先查询后者，一次 Request 查询必须把两笔
// 都结清，并按各自截止时刻从早到晚在原账本末尾追加记录，而不是只释放被查的
// 那笔、把另一笔拖到之后查账本时才追加。
func TestRequestQueryReleasesAllDueReservationsAcrossScopes(t *testing.T) {
	w, c := queryExpirySetup(t)
	saveQueryPolicy(t, w, c, "pa", "payerA", 10*time.Second)
	saveQueryPolicy(t, w, c, "pb", "payerB", 20*time.Second)

	t0 := c.t
	applyQuery(t, w, "pa", "u1", "r1", 30) // payerA，截止 t0+10
	applyQuery(t, w, "pb", "u2", "r2", 40) // payerB，截止 t0+20

	before := w.Ledger()
	if len(before) != 2 {
		t.Fatalf("ledger before query = %d entries, want 2", len(before))
	}

	// 第三十秒先查询后者（r2）：它自身已超时，同时前者（r1）也必须被结清。
	c.t = t0.Add(30 * time.Second)
	view, err := w.Request("u2", "r2")
	if err != nil {
		t.Fatal(err)
	}
	if view.State != RequestReservationExpired || !view.ReserveExpiredAt.Equal(t0.Add(20*time.Second)) {
		t.Fatalf("queried request = %+v, want reservation expired at its deadline t0+20s", view)
	}

	got := w.Ledger()
	if len(got) != len(before)+4 {
		t.Fatalf("ledger after query = %d entries, want %d", len(got), len(before)+4)
	}
	// 既有历史不改写、不重排。
	if !equalLedger(got[:len(before)], before) {
		t.Fatal("existing ledger history was rewritten or reordered")
	}

	want := []struct {
		at      time.Time
		payer   string
		account string
		request string
		amount  int64
	}{
		{t0.Add(10 * time.Second), "payerA", "u1", "r1", 30},
		{t0.Add(20 * time.Second), "payerB", "u2", "r2", 40},
	}
	for i, e := range want {
		refund := got[len(before)+2*i]
		exp := got[len(before)+2*i+1]
		if refund.Kind != LedgerRefund || refund.AccountID != e.payer ||
			refund.RequestID != e.request || refund.Amount != e.amount ||
			!refund.At.Equal(e.at) || refund.Reason == "" {
			t.Fatalf("refund %d = %+v, want %s refund of %d for %s at %v",
				i, refund, e.payer, e.amount, e.request, e.at)
		}
		if exp.Kind != LedgerReservationExpiration || exp.AccountID != e.account ||
			exp.RequestID != e.request || exp.Amount != 0 ||
			!exp.At.Equal(e.at) || exp.Reason == "" {
			t.Fatalf("expiration %d = %+v, want zero-amount timeout for %s/%s at %v",
				i, exp, e.account, e.request, e.at)
		}
	}

	// 两笔都全额退回各自策略指定的出资账户，预留/已花费总额同步减少或不增。
	if bal, _ := w.Balance("payerA"); bal != (Balances{Available: 1000, Reserved: 0}) {
		t.Fatalf("payerA balance = %+v, want {1000 0}", bal)
	}
	if bal, _ := w.Balance("payerB"); bal != (Balances{Available: 1000, Reserved: 0}) {
		t.Fatalf("payerB balance = %+v, want {1000 0}", bal)
	}
	for _, id := range []string{"pa", "pb"} {
		if pv, _ := w.Policy(id); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
			t.Fatalf("policy %s totals = reserved %d spent %d, want 0/0", id, pv.ReservedTotal, pv.SpentTotal)
		}
	}

	// 每笔只释放一次：再查 r1 不新增记录，它的释放时间仍是 t0+10。
	r1, err := w.Request("u1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if r1.State != RequestReservationExpired || !r1.ReserveExpiredAt.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("r1 = %+v, want expired at t0+10s", r1)
	}
	if len(w.Ledger()) != len(got) {
		t.Fatal("re-query released a reservation twice or appended entries")
	}
}

// 恰为较早一笔的截止时刻（第十秒）查询尚未到期（第二十秒）的后者：较早一笔
// 当场释放（截止时刻本身算到期、释放时间即截止时刻），被查询的后者仍预留、
// 不提前退款。
func TestRequestQueryAtEarlierDeadlineReleasesOnlyDue(t *testing.T) {
	w, c := queryExpirySetup(t)
	saveQueryPolicy(t, w, c, "pa", "payerA", 10*time.Second)
	saveQueryPolicy(t, w, c, "pb", "payerB", 20*time.Second)

	t0 := c.t
	applyQuery(t, w, "pa", "u1", "r1", 30) // 截止 t0+10
	applyQuery(t, w, "pb", "u2", "r2", 40) // 截止 t0+20

	c.t = t0.Add(10 * time.Second)
	queried, err := w.Request("u2", "r2")
	if err != nil {
		t.Fatal(err)
	}
	if queried.State != RequestReserved || !queried.ReserveExpiredAt.IsZero() {
		t.Fatalf("queried not-yet-due request = %+v, want still reserved", queried)
	}

	// r1 在自己的截止时刻被这次查询释放，新增两条记录的时间均为 t0+10。
	ledger := w.Ledger()
	if len(ledger) != 4 {
		t.Fatalf("ledger = %d entries, want 4", len(ledger))
	}
	if ledger[2].Kind != LedgerRefund || ledger[2].AccountID != "payerA" ||
		ledger[2].RequestID != "r1" || ledger[2].Amount != 30 ||
		!ledger[2].At.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("r1 refund = %+v, want payerA refund 30 at t0+10s", ledger[2])
	}
	if ledger[3].Kind != LedgerReservationExpiration || ledger[3].AccountID != "u1" ||
		ledger[3].RequestID != "r1" || ledger[3].Amount != 0 ||
		!ledger[3].At.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("r1 expiration = %+v, want zero-amount timeout at t0+10s", ledger[3])
	}
	// r2 未到期：payerB 的预留仍在，不产生 r2 的退款或超时记录。
	if bal, _ := w.Balance("payerB"); bal != (Balances{Available: 960, Reserved: 40}) {
		t.Fatalf("payerB balance = %+v, want {960 40}", bal)
	}
	for _, e := range ledger {
		if e.RequestID == "r2" && e.Kind != LedgerReserve {
			t.Fatalf("r2 released early: %+v", e)
		}
	}
}

// 被查询对象处于未启用预留超时、待审批或已结算终态时，只要请求存在就仍触发
// 其他到期预留的释放；它自身按原有规则返回，不提前退款、不重写终态。
func TestRequestQueryTriggersReleaseRegardlessOfQueriedState(t *testing.T) {
	t.Run("timeout disabled", func(t *testing.T) {
		w, c := queryExpirySetup(t)
		saveQueryPolicy(t, w, c, "pa", "payerA", 0) // 关闭预留超时
		saveQueryPolicy(t, w, c, "pb", "payerB", 10*time.Second)

		t0 := c.t
		applyQuery(t, w, "pa", "u1", "keep", 10) // 永不超时
		applyQuery(t, w, "pb", "u2", "due", 40)  // 截止 t0+10

		c.t = t0.Add(30 * time.Second)
		keep, err := w.Request("u1", "keep")
		if err != nil {
			t.Fatal(err)
		}
		if keep.State != RequestReserved || !keep.ReserveDeadline.IsZero() {
			t.Fatalf("timeout-disabled request = %+v, want reserved with no deadline", keep)
		}
		// 另一笔已到期预留被释放；关闭超时策略的预留不受影响。
		if bal, _ := w.Balance("payerA"); bal != (Balances{Available: 990, Reserved: 10}) {
			t.Fatalf("payerA balance = %+v, want {990 10}", bal)
		}
		if bal, _ := w.Balance("payerB"); bal != (Balances{Available: 1000, Reserved: 0}) {
			t.Fatalf("payerB balance = %+v, want {1000 0}", bal)
		}
	})

	t.Run("pending approval", func(t *testing.T) {
		w, c := queryExpirySetup(t)
		// pa 开启审批：门槛 10、等待 2 小时，rBig 进入待审批、不冻结余额。
		ap := PolicySpec{
			ID: "pa", PayerAccountID: "payerA", AllowedAccountIDs: []string{"u1", "u2"},
			Operation: "charge", Payee: "shop",
			StartsAt: c.t.Add(-time.Hour), EndsAt: c.t.Add(time.Hour),
			MaxPerRequest: 100, MaxTotal: 1000,
			ApprovalThreshold: 10, ApprovalWait: 2 * time.Hour,
			MaxReserveDuration: 30 * time.Second,
		}
		if err := w.SavePolicy(ap); err != nil {
			t.Fatal(err)
		}
		saveQueryPolicy(t, w, c, "pb", "payerB", 10*time.Second)

		t0 := c.t
		applyQuery(t, w, "pa", "u1", "big", 20) // 待审批，等待期限 t0+1h（受策略结束限制）
		applyQuery(t, w, "pb", "u2", "due", 40) // 截止 t0+10

		c.t = t0.Add(30 * time.Second)
		pending, err := w.Request("u1", "big")
		if err != nil {
			t.Fatal(err)
		}
		if pending.State != RequestPendingApproval || !pending.ReservedAt.IsZero() {
			t.Fatalf("pending request = %+v, want still pending with no reservation", pending)
		}
		// 待审批本身不退款；其他到期预留仍被释放。
		if bal, _ := w.Balance("payerA"); bal != (Balances{Available: 1000, Reserved: 0}) {
			t.Fatalf("payerA balance = %+v, want {1000 0} (pending freezes nothing)", bal)
		}
		if bal, _ := w.Balance("payerB"); bal != (Balances{Available: 1000, Reserved: 0}) {
			t.Fatalf("payerB balance = %+v, want {1000 0}", bal)
		}
		if n := countKind(w.Ledger(), LedgerRefund); n != 1 {
			t.Fatalf("refund entries = %d, want only the released reservation's", n)
		}

		// 被查询的待审批请求到达自己的等待期限后转为过期，仍不产生退款。
		c.t = t0.Add(time.Hour)
		expired, err := w.Request("u1", "big")
		if err != nil {
			t.Fatal(err)
		}
		if expired.State != RequestExpired || !expired.ReservedAt.IsZero() {
			t.Fatalf("pending request after wait deadline = %+v, want expired with no reservation", expired)
		}
		if n := countKind(w.Ledger(), LedgerRefund); n != 1 {
			t.Fatalf("refund entries after pending expiry = %d, want still 1", n)
		}
	})

	t.Run("settled terminal", func(t *testing.T) {
		w, c := queryExpirySetup(t)
		saveQueryPolicy(t, w, c, "pa", "payerA", time.Minute)
		saveQueryPolicy(t, w, c, "pb", "payerB", 10*time.Second)

		t0 := c.t
		applyQuery(t, w, "pa", "u1", "done", 20) // 截止 t0+1min，提前结算
		applyQuery(t, w, "pb", "u2", "due", 40)  // 截止 t0+10
		if _, err := w.Settle("u1", "done", 7); err != nil {
			t.Fatal(err)
		}

		c.t = t0.Add(30 * time.Second)
		done, err := w.Request("u1", "done")
		if err != nil {
			t.Fatal(err)
		}
		if done.State != RequestSettled || done.ActualFee != 7 || !done.ReserveExpiredAt.IsZero() {
			t.Fatalf("settled request rewritten = %+v", done)
		}
		// 已结算终态不补退款；另一笔到期预留正常释放。
		if n := countKind(w.AccountLedger("payerA"), LedgerReservationExpiration); n != 0 {
			t.Fatal("settled request must not gain a timeout record")
		}
		if pv, _ := w.Policy("pb"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
			t.Fatalf("pb totals = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
		}
		if d, _ := w.Request("u2", "due"); d.State != RequestReservationExpired {
			t.Fatalf("due request = %v, want reservation expired", d.State)
		}
	})
}

// 同号请求按使用账户分别识别：两笔同号、截止时刻相同但分属 u1/u2、不同策略
// 与出资账户，查询其中一笔会同时释放两笔，同截止时刻按使用账户升序留痕，
// 互不合并或替代。
func TestRequestQuerySameIDAcrossAccountsReleasesBoth(t *testing.T) {
	w, c := queryExpirySetup(t)
	saveQueryPolicy(t, w, c, "pa", "payerA", 10*time.Second)
	saveQueryPolicy(t, w, c, "pb", "payerB", 10*time.Second)

	t0 := c.t
	applyQuery(t, w, "pa", "u2", "r1", 40) // 同号、同截止，分属 u2
	applyQuery(t, w, "pb", "u1", "r1", 30) // 与 u1

	c.t = t0.Add(10 * time.Second)
	if q, err := w.Request("u2", "r1"); err != nil || q.State != RequestReservationExpired {
		t.Fatalf("query u2/r1 = %+v %v", q, err)
	}

	ledger := w.Ledger()
	if len(ledger) != 6 {
		t.Fatalf("ledger = %d entries, want 6", len(ledger))
	}
	tail := ledger[2:]
	want := []struct {
		payer   string
		account string
		amount  int64
	}{
		{"payerB", "u1", 30}, // 同截止时刻按使用账户升序：u1 在前
		{"payerA", "u2", 40},
	}
	for i, e := range want {
		refund, exp := tail[2*i], tail[2*i+1]
		if refund.Kind != LedgerRefund || refund.AccountID != e.payer ||
			refund.RequestID != "r1" || refund.Amount != e.amount ||
			!refund.At.Equal(t0.Add(10*time.Second)) {
			t.Fatalf("refund %d = %+v, want %s refund %d for r1", i, refund, e.payer, e.amount)
		}
		if exp.Kind != LedgerReservationExpiration || exp.AccountID != e.account ||
			exp.RequestID != "r1" || exp.Amount != 0 ||
			!exp.At.Equal(t0.Add(10*time.Second)) {
			t.Fatalf("expiration %d = %+v, want zero-amount timeout for %s/r1", i, exp, e.account)
		}
	}
}

// 请求不存在时返回 ErrRequestNotFound，失败查询不改变任何预留：已到期的预留
// 仍保持预留，账本不追加退款或超时记录。
func TestRequestQueryNotFoundChangesNothing(t *testing.T) {
	w, c := queryExpirySetup(t)
	saveQueryPolicy(t, w, c, "pb", "payerB", 10*time.Second)

	t0 := c.t
	applyQuery(t, w, "pb", "u2", "due", 40) // 截止 t0+10

	c.t = t0.Add(30 * time.Second)
	if _, err := w.Request("u1", "missing"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("missing request err = %v, want ErrRequestNotFound", err)
	}
	// 直接在锁内检查，避免余额/账本查询自身触发到期释放而掩盖失败查询的效果。
	w.mu.Lock()
	ledgerLen := len(w.ledger)
	reserved := w.accounts["payerB"].reserved
	state := w.requests[requestKey{accountID: "u2", requestID: "due"}].state
	w.mu.Unlock()
	if ledgerLen != 1 {
		t.Fatalf("failed query appended ledger entries: %d, want 1", ledgerLen)
	}
	if reserved != 40 {
		t.Fatalf("failed query released reservation: payerB reserved = %d, want 40", reserved)
	}
	if state != RequestReserved {
		t.Fatalf("due request state after failed query = %v, want still reserved", state)
	}
}

// equalLedger 比较两段账本是否逐记录一致。
func equalLedger(a, b []LedgerEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
