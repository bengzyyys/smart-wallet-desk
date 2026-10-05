package wallet

import (
	"errors"
	"testing"
	"time"
)

// 两笔预留分别在同一时刻后的第 10、20 秒到期；第 30 秒先查询后者：一次
// 请求查询就应按截止时刻先后把两笔都释放，而不是只释放被查询的后者。
func TestRequestQuerySettlesAllDueReservationsInDeadlineOrder(t *testing.T) {
	w, c := setupMultiTimeout(t)
	// pa 与 pb 使用各自的出资账户，验证释放范围跨使用账户、策略与出资账户。
	mustAccount(t, w, "payer2", 1000)

	pa := multiTimeoutPolicy(c, "pa", 10*time.Second)
	pa.PayerAccountID = "payer"
	if err := w.SavePolicy(pa); err != nil {
		t.Fatal(err)
	}
	pb := multiTimeoutPolicy(c, "pb", 20*time.Second)
	pb.PayerAccountID = "payer2"
	pb.AllowedAccountIDs = []string{"u1", "u2"}
	if err := w.SavePolicy(pb); err != nil {
		t.Fatal(err)
	}

	t0 := c.t
	applyMulti(t, w, "pa", "u1", "early", 20) // 截止 t0+10，出资 payer
	applyMulti(t, w, "pb", "u2", "later", 30) // 截止 t0+20，出资 payer2
	before := w.Ledger()

	// 第 30 秒先查询后者（截止更晚的那笔）。
	c.t = t0.Add(30 * time.Second)
	got, err := w.Request("u2", "later")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestReservationExpired ||
		!got.ReserveExpiredAt.Equal(t0.Add(20*time.Second)) || got.ActualFee != 0 {
		t.Fatalf("queried later request = %+v, want expired at its deadline", got)
	}

	// 两笔都应在同一次查询中释放，账本只追加 4 条，且早到期的排在前面。
	ledger := w.Ledger()
	if len(ledger) != len(before)+4 {
		t.Fatalf("ledger = %d entries, want %d (both reservations released in one query)",
			len(ledger), len(before)+4)
	}
	newEntries := ledger[len(before):]
	want := []struct {
		at      time.Time
		account string
		request string
		payer   string
		amount  int64
	}{
		{t0.Add(10 * time.Second), "u1", "early", "payer", 20},
		{t0.Add(20 * time.Second), "u2", "later", "payer2", 30},
	}
	for i, e := range want {
		refund, exp := newEntries[2*i], newEntries[2*i+1]
		if refund.Kind != LedgerRefund || refund.AccountID != e.payer ||
			refund.RequestID != e.request || refund.Amount != e.amount ||
			!refund.At.Equal(e.at) {
			t.Fatalf("refund %d = %+v, want %s refund %d at %v", i, refund, e.payer, e.amount, e.at)
		}
		if exp.Kind != LedgerReservationExpiration || exp.AccountID != e.account ||
			exp.RequestID != e.request || exp.Amount != 0 || !exp.At.Equal(e.at) {
			t.Fatalf("expiration %d = %+v, want zero timeout for %s/%s at %v",
				i, exp, e.account, e.request, e.at)
		}
	}

	// 两个出资账户都已全额退回，各自策略预留总额归零、已花费不增加。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 1000, Reserved: 0}) {
		t.Fatalf("payer balance = %+v, want {1000 0}", bal)
	}
	if bal, _ := w.Balance("payer2"); bal != (Balances{Available: 1000, Reserved: 0}) {
		t.Fatalf("payer2 balance = %+v, want {1000 0}", bal)
	}
	if pv, _ := w.Policy("pa"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("pa totals = %d/%d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}
	if pv, _ := w.Policy("pb"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("pb totals = %d/%d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}

	// 再查一次不重复释放、不重排既有账本。
	if r, err := w.Request("u1", "early"); err != nil || r.State != RequestReservationExpired {
		t.Fatalf("re-query early = %+v %v", r, err)
	}
	if len(w.Ledger()) != len(ledger) {
		t.Fatal("re-query appended duplicate timeout entries")
	}
}

// 查询存在但未到期 / 未启用超时 / 待审批 / 已终态的请求，也应触发其他已
// 到期预留的释放；被查询对象自身按原规则返回，不提前退款或改写终态。
func TestRequestQueryTriggersSweepRegardlessOfTargetState(t *testing.T) {
	w, c := setupMultiTimeout(t)
	// pa：10 秒超时；pb：关闭超时；pc：开启大额审批。
	save := func(p PolicySpec) {
		t.Helper()
		if err := w.SavePolicy(p); err != nil {
			t.Fatal(err)
		}
	}
	save(multiTimeoutPolicy(c, "pa", 10*time.Second))
	off := multiTimeoutPolicy(c, "pb", 0)
	save(off)
	ap := multiTimeoutPolicy(c, "pc", 10*time.Second)
	ap.ApprovalThreshold = 10
	ap.ApprovalWait = time.Hour
	save(ap)

	t0 := c.t
	// 一笔 pa 下将于 t0+10 到期的“其他预留”。
	applyMulti(t, w, "pa", "u2", "due", 20)

	// 被查询目标：未到期的已预留请求。
	applyMulti(t, w, "pa", "u1", "notdue", 5) // 截止 t0+10，与 due 同刻

	// 第 5 秒查询未到期目标：其他预留与它自己都未到期，不释放。
	c.t = t0.Add(5 * time.Second)
	if r, _ := w.Request("u1", "notdue"); r.State != RequestReserved {
		t.Fatalf("not-due target = %v, want reserved", r.State)
	}
	if n := countKind(w.Ledger(), LedgerReservationExpiration); n != 0 {
		t.Fatalf("early query released reservations: %d timeout entries", n)
	}

	// 新构造：关闭超时的目标 + 已到期的其他预留。
	w2, c2 := setupMultiTimeout(t)
	save2 := func(p PolicySpec) {
		t.Helper()
		if err := w2.SavePolicy(p); err != nil {
			t.Fatal(err)
		}
	}
	save2(multiTimeoutPolicy(c2, "pa", 10*time.Second))
	save2(multiTimeoutPolicy(c2, "pb", 0))
	applyMulti(t, w2, "pa", "u2", "due", 20)
	applyMulti(t, w2, "pb", "u1", "forever", 10) // 关闭超时，永不自动释放
	c2.t = t0.Add(30 * time.Second)
	r, err := w2.Request("u1", "forever")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != RequestReserved {
		t.Fatalf("timeout-disabled target = %v, want still reserved", r.State)
	}
	if dr, _ := w2.Request("u2", "due"); dr.State != RequestReservationExpired {
		t.Fatalf("other due reservation = %v, want expired", dr.State)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 990, Reserved: 10}) {
		t.Fatalf("balance = %+v, want only due 20 refunded, forever 10 held", bal)
	}

	// 待审批目标：触发其他到期预留释放，自身不退款；待审批过期只改自身状态。
	w3, c3 := setupMultiTimeout(t)
	save3 := func(p PolicySpec) {
		t.Helper()
		if err := w3.SavePolicy(p); err != nil {
			t.Fatal(err)
		}
	}
	save3(multiTimeoutPolicy(c3, "pa", 10*time.Second))
	ap3 := multiTimeoutPolicy(c3, "pc", 10*time.Second)
	ap3.ApprovalThreshold = 10
	ap3.ApprovalWait = 5 * time.Second
	save3(ap3)
	applyMulti(t, w3, "pa", "u2", "due", 20)
	applyMulti(t, w3, "pc", "u1", "pending", 20) // 进入待审批，等待期限 t0+5
	c3.t = t0.Add(30 * time.Second)
	pr, err := w3.Request("u1", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if pr.State != RequestExpired {
		t.Fatalf("pending target past wait deadline = %v, want expired", pr.State)
	}
	if !pr.ReserveExpiredAt.IsZero() {
		t.Fatalf("pending-expired target must carry no reservation release: %+v", pr)
	}
	if dr, _ := w3.Request("u2", "due"); dr.State != RequestReservationExpired {
		t.Fatalf("other due reservation = %v, want expired", dr.State)
	}
	if bal, _ := w3.Balance("payer"); bal != (Balances{Available: 1000, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {1000 0}", bal)
	}

	// 已终态目标（已结算）触发其他到期预留释放，自身决定不被重写。
	w4, c4 := setupMultiTimeout(t)
	save4 := func(p PolicySpec) {
		t.Helper()
		if err := w4.SavePolicy(p); err != nil {
			t.Fatal(err)
		}
	}
	save4(multiTimeoutPolicy(c4, "pa", 10*time.Second))
	applyMulti(t, w4, "pa", "u2", "due", 20)
	applyMulti(t, w4, "pa", "u1", "done", 10)
	if _, err := w4.Settle("u1", "done", 7); err != nil {
		t.Fatal(err)
	}
	c4.t = t0.Add(30 * time.Second)
	sr, err := w4.Request("u1", "done")
	if err != nil {
		t.Fatal(err)
	}
	if sr.State != RequestSettled || sr.ActualFee != 7 || !sr.ReserveExpiredAt.IsZero() {
		t.Fatalf("settled target rewritten: %+v", sr)
	}
	if dr, _ := w4.Request("u2", "due"); dr.State != RequestReservationExpired {
		t.Fatalf("other due reservation = %v, want expired", dr.State)
	}
}

// 请求不存在时返回 ErrRequestNotFound，且失败查询不释放任何到期预留。
func TestRequestNotFoundDoesNotSettleReservations(t *testing.T) {
	w, c := setupMultiTimeout(t)
	if err := w.SavePolicy(multiTimeoutPolicy(c, "pa", 10*time.Second)); err != nil {
		t.Fatal(err)
	}
	applyMulti(t, w, "pa", "u1", "r1", 20)
	c.t = c.t.Add(30 * time.Second)

	if _, err := w.Request("u1", "missing"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("err = %v, want ErrRequestNotFound", err)
	}
	// 失败查询后预留仍未释放。注意不能用 Ledger/Balance 等公开查询核对：
	// 它们自身也会触发到期释放，因此这里直接检查钱包内部状态。
	w.mu.Lock()
	if n := countKind(w.ledger, LedgerReservationExpiration); n != 0 {
		t.Fatalf("failed query released reservations: %d timeout entries", n)
	}
	if payer := w.accounts["payer"]; payer.available != 980 || payer.reserved != 20 {
		t.Fatalf("balance after failed query = %+v, want {980 20}", payer)
	}
	if req := w.requests[requestKey{accountID: "u1", requestID: "r1"}]; req.state != RequestReserved {
		t.Fatalf("r1 state = %v, want still reserved", req.state)
	}
	w.mu.Unlock()

	// 同号请求按使用账户分别识别：u2/r1 不存在不影响 u1/r1，反之亦然。
	if _, err := w.Request("u2", "r1"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("err = %v, want ErrRequestNotFound for u2/r1", err)
	}
	w.mu.Lock()
	if n := countKind(w.ledger, LedgerReservationExpiration); n != 0 {
		t.Fatalf("cross-account failed query released reservations: %d", n)
	}
	if req := w.requests[requestKey{accountID: "u1", requestID: "r1"}]; req.state != RequestReserved {
		t.Fatalf("r1 state = %v, want still reserved", req.state)
	}
	w.mu.Unlock()
}
