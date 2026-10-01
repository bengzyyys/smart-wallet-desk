package wallet

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// clock 是可控时钟，便于精确测试到期边界。
type clock struct{ t time.Time }

func newTestWallet() (*Wallet, *clock) {
	w := New()
	c := &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	w.now = func() time.Time { return c.t }
	return w, c
}

func mustAccount(t *testing.T, w *Wallet, id string, bal int64) {
	t.Helper()
	if _, err := w.CreateAccount(id, bal); err != nil {
		t.Fatalf("create account %s: %v", id, err)
	}
}

func TestReadyBaselineKept(t *testing.T) {
	if !Ready() {
		t.Fatal("baseline entry Ready() must keep returning true")
	}
}

func TestCreateAccountAndBalance(t *testing.T) {
	w, _ := newTestWallet()

	mustAccount(t, w, "payer", 100)
	got, err := w.Balance("payer")
	if err != nil {
		t.Fatal(err)
	}
	if got.Available != 100 || got.Reserved != 0 {
		t.Fatalf("balance = %+v, want {100 0}", got)
	}

	if _, err := w.CreateAccount("payer", 1); !errors.Is(err, ErrAccountExists) {
		t.Fatalf("dup account err = %v, want ErrAccountExists", err)
	}
	if _, err := w.CreateAccount("bad", -1); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("negative balance err = %v, want ErrInvalidAmount", err)
	}
	if _, err := w.Account("missing"); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("missing account err = %v, want ErrAccountNotFound", err)
	}
	// 失败的创建不得改变已有记录。
	got, _ = w.Balance("payer")
	if got.Available != 100 {
		t.Fatalf("balance changed after failed creates: %+v", got)
	}
}

func TestSessions(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "u1", 0)

	exp := c.t.Add(time.Minute)
	sess, err := w.CreateSession("s1", "u1", "dev1", exp)
	if err != nil {
		t.Fatal(err)
	}
	if sess.State != SessionActive || sess.ExpiresAt != exp {
		t.Fatalf("session view = %+v", sess)
	}
	if _, err := w.CreateSession("s1", "u1", "dev1", exp); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("dup session err = %v", err)
	}
	if _, err := w.CreateSession("sx", "ghost", "d", exp); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("session for missing account err = %v", err)
	}

	// 到期时刻本身即视为过期。
	c.t = exp
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	view, _ := w.Session("s1")
	if view.State != SessionRevoked {
		t.Fatalf("state = %v, want revoked", view.State)
	}
	if err := w.RevokeSession("ghost"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("revoke missing err = %v", err)
	}
	// 重复吊销幂等。
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatalf("repeat revoke: %v", err)
	}
}

// validPolicy 构造一条在整个测试时间窗内有效的策略。
func validPolicy(c *clock) PolicySpec {
	return PolicySpec{
		ID:                "p1",
		PayerAccountID:    "payer",
		AllowedAccountIDs: []string{"u1", "u2"},
		Operation:         "charge",
		Payee:             "shop",
		StartsAt:          c.t.Add(-time.Hour),
		EndsAt:            c.t.Add(time.Hour),
		MaxPerRequest:     30,
		MaxTotal:          50,
	}
}

func TestSavePolicyValidation(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)

	good := validPolicy(c)
	if err := w.SavePolicy(good); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(good); !errors.Is(err, ErrPolicyExists) {
		t.Fatalf("dup policy err = %v", err)
	}

	bad := good
	bad.ID = "p-neg"
	bad.MaxPerRequest = 0
	if err := w.SavePolicy(bad); !errors.Is(err, ErrPolicyInvalid) {
		t.Fatalf("non-positive per-request err = %v", err)
	}
	bad.ID, bad.MaxPerRequest, bad.MaxTotal = "p-neg2", 10, -1
	if err := w.SavePolicy(bad); !errors.Is(err, ErrPolicyInvalid) {
		t.Fatalf("negative total err = %v", err)
	}
	bad.ID, bad.MaxTotal, bad.EndsAt = "p-time", 50, bad.StartsAt
	if err := w.SavePolicy(bad); !errors.Is(err, ErrPolicyInvalid) {
		t.Fatalf("start==end err = %v", err)
	}
	bad.ID, bad.EndsAt, bad.PayerAccountID = "p-payer", c.t.Add(time.Hour), "ghost"
	if err := w.SavePolicy(bad); !errors.Is(err, ErrPolicyInvalid) {
		t.Fatalf("missing payer err = %v", err)
	}
	bad.ID, bad.PayerAccountID, bad.AllowedAccountIDs = "p-allowed", "payer", []string{"ghost"}
	if err := w.SavePolicy(bad); !errors.Is(err, ErrPolicyInvalid) {
		t.Fatalf("missing allowed account err = %v", err)
	}
}

func setupApply(t *testing.T) (*Wallet, *clock) {
	t.Helper()
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s2", "u2", "dev2", c.t.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(validPolicy(c)); err != nil {
		t.Fatal(err)
	}
	return w, c
}

func baseApply() RequestInput {
	return RequestInput{
		PolicyID:     "p1",
		RequestID:    "r1",
		AccountID:    "u1",
		SessionID:    "s1",
		DeviceID:     "dev1",
		Operation:    "charge",
		Payee:        "shop",
		EstimatedFee: 20,
	}
}

func TestApplyReservesFee(t *testing.T) {
	w, c := setupApply(t)

	in := baseApply()
	req, err := w.Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	if req.State != RequestReserved || req.PayerAccountID != "payer" {
		t.Fatalf("request = %+v", req)
	}
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("payer balance = %+v, want {80 20}", bal)
	}
	pv, _ := w.Policy("p1")
	if pv.ReservedTotal != 20 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d", pv.ReservedTotal, pv.SpentTotal)
	}

	// 账本含一条预留记录，关联请求与金额。
	led := w.AccountLedger("payer")
	if len(led) != 1 || led[0].Kind != LedgerReserve || led[0].Amount != 20 || led[0].RequestID != "r1" {
		t.Fatalf("ledger = %+v", led)
	}

	// 结算：实际 5，退回 15。
	settled, err := w.Settle("u1", "r1", 5)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != RequestSettled || settled.ActualFee != 5 {
		t.Fatalf("settled = %+v", settled)
	}
	bal, _ = w.Balance("payer")
	if bal != (Balances{Available: 95, Reserved: 0}) {
		t.Fatalf("payer balance after settle = %+v, want {95 0}", bal)
	}
	pv, _ = w.Policy("p1")
	if pv.ReservedTotal != 0 || pv.SpentTotal != 5 {
		t.Fatalf("policy totals after settle = %+v", pv)
	}
	_ = c
}

func TestSettleZeroFeeAndRepeatedSettle(t *testing.T) {
	w, _ := setupApply(t)
	if _, err := w.Apply(baseApply()); err != nil {
		t.Fatal(err)
	}

	// 实际费用允许为零：全部退回。
	if _, err := w.Settle("u1", "r1", 0); err != nil {
		t.Fatalf("zero settle: %v", err)
	}
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v", bal)
	}
	entriesBefore := len(w.AccountLedger("payer"))
	// 相同实际费用重复结算：幂等，不重复记账。
	if _, err := w.Settle("u1", "r1", 0); err != nil {
		t.Fatalf("idempotent settle: %v", err)
	}
	if got := len(w.AccountLedger("payer")); got != entriesBefore {
		t.Fatalf("ledger grew on repeated settle: %d vs %d", got, entriesBefore)
	}
	// 不同实际费用：冲突。
	if _, err := w.Settle("u1", "r1", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting settle err = %v", err)
	}
	// 已结算不能取消。
	if _, err := w.Cancel("u1", "r1"); !errors.Is(err, ErrAlreadySettled) {
		t.Fatalf("cancel settled err = %v", err)
	}
}

func TestSettleOverEstimateKeepsReservation(t *testing.T) {
	w, _ := setupApply(t)
	if _, err := w.Apply(baseApply()); err != nil {
		t.Fatal(err)
	}

	// 超出预估值或负数：拒绝且保留预留。
	if _, err := w.Settle("u1", "r1", 21); !errors.Is(err, ErrSettleTooLarge) {
		t.Fatalf("over-estimate settle err = %v", err)
	}
	if _, err := w.Settle("u1", "r1", -1); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("negative settle err = %v", err)
	}
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("reservation not kept: %+v", bal)
	}
	req, _ := w.Request("u1", "r1")
	if req.State != RequestReserved {
		t.Fatalf("state = %v, want still reserved", req.State)
	}
	// 合法结算仍可进行。
	if _, err := w.Settle("u1", "r1", 20); err != nil {
		t.Fatal(err)
	}
}

func TestCancelFlow(t *testing.T) {
	w, _ := setupApply(t)
	if _, err := w.Apply(baseApply()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Cancel("u1", "r1"); err != nil {
		t.Fatal(err)
	}
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance after cancel = %+v", bal)
	}
	// 重复取消不重复记账。
	entriesBefore := len(w.AccountLedger("payer"))
	if _, err := w.Cancel("u1", "r1"); err != nil {
		t.Fatalf("repeat cancel: %v", err)
	}
	if got := len(w.AccountLedger("payer")); got != entriesBefore {
		t.Fatalf("ledger grew on repeat cancel")
	}
	// 已取消不能结算。
	if _, err := w.Settle("u1", "r1", 0); !errors.Is(err, ErrAlreadyCancelled) {
		t.Fatalf("settle cancelled err = %v", err)
	}
	// 已取消请求重复申请不能重新扣款。
	before := len(w.Ledger())
	if view, err := w.Apply(baseApply()); err != nil || view.State != RequestCancelled {
		t.Fatalf("re-apply cancelled request: view=%+v err=%v", view, err)
	}
	if got := len(w.Ledger()); got != before {
		t.Fatalf("re-apply cancelled request produced ledger entries: %d vs %d", got, before)
	}
	bal, _ = w.Balance("payer")
	if bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance changed on re-apply: %+v", bal)
	}
}

func TestApplyIdempotencyAndConflict(t *testing.T) {
	w, _ := setupApply(t)
	in := baseApply()
	first, err := w.Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.Apply(in)
	if err != nil {
		t.Fatalf("idempotent re-apply: %v", err)
	}
	if second.RequestID != first.RequestID || second.State != first.State || second.EstimatedFee != first.EstimatedFee {
		t.Fatalf("idempotent result differs: %+v vs %+v", first, second)
	}
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("double reservation on idempotent re-apply: %+v", bal)
	}

	for _, mutate := range []func(*RequestInput){
		func(x *RequestInput) { x.PolicyID = "other" },
		func(x *RequestInput) { x.Operation = "refund" },
		func(x *RequestInput) { x.Payee = "other-shop" },
		func(x *RequestInput) { x.EstimatedFee = 19 },
	} {
		conflict := in
		mutate(&conflict)
		// 不存在的策略会先报策略不存在；其余三类应报冲突。这里只验证“同号不同内容绝不二次预留”。
		if _, err := w.Apply(conflict); err == nil {
			t.Fatalf("conflicting re-apply accepted: %+v", conflict)
		} else if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrPolicyNotFound) {
			t.Fatalf("conflicting re-apply err = %v", err)
		}
	}
	bal, _ = w.Balance("payer")
	if bal != (Balances{Available: 80, Reserved: 20}) {
		t.Fatalf("balance changed after conflicting applies: %+v", bal)
	}
}

func TestSessionChecks(t *testing.T) {
	w, c := setupApply(t)

	cases := []struct {
		name    string
		mutate  func(*RequestInput)
		wantErr error
	}{
		{"missing session", func(in *RequestInput) { in.SessionID = "nope" }, ErrSessionNotFound},
		{"account mismatch", func(in *RequestInput) {
			in.AccountID = "u2"
			in.SessionID = "s1"
		}, ErrSessionAccountMismatch},
		{"device mismatch", func(in *RequestInput) { in.DeviceID = "wrong" }, ErrSessionDeviceMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseApply()
			tc.mutate(&in)
			if _, err := w.Apply(in); !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}

	// 到期边界：到期时刻及之后拒绝。
	in := baseApply()
	in.RequestID = "r-expiry"
	c.t = c.t.Add(time.Minute)
	if _, err := w.Apply(in); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("at expiry err = %v", err)
	}
	c.t = c.t.Add(-time.Minute).Add(time.Second) // 到期前 59 秒仍有效，但换个请求号
	in.RequestID = "r-valid"
	if _, err := w.Apply(in); err != nil {
		t.Fatalf("just before expiry should pass session check: %v", err)
	}

	// 吊销：新申请拒绝；已预留请求仍可结算。
	if err := w.RevokeSession("s2"); err != nil {
		t.Fatal(err)
	}
	in2 := baseApply()
	in2.AccountID, in2.SessionID, in2.DeviceID, in2.RequestID = "u2", "s2", "dev2", "r2"
	if _, err := w.Apply(in2); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("revoked session err = %v", err)
	}
	if _, err := w.Settle("u1", "r-valid", 10); err != nil {
		t.Fatalf("settle after another session revoked: %v", err)
	}
}

func TestPolicyAuthorization(t *testing.T) {
	w, c := setupApply(t)

	// 窗口边界：含开始、不含结束（窗口完全落在会话有效期内）。
	p := validPolicy(c)
	p.ID = "p-window"
	p.StartsAt = c.t.Add(30 * time.Second)
	p.EndsAt = c.t.Add(50 * time.Second)
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	in := baseApply()
	in.PolicyID, in.RequestID = "p-window", "w1"
	if _, err := w.Apply(in); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("before start err = %v", err)
	}
	c.t = c.t.Add(30 * time.Second) // 恰为开始时刻：允许
	if _, err := w.Apply(in); err != nil {
		t.Fatalf("at start should be allowed: %v", err)
	}
	c.t = c.t.Add(20 * time.Second) // 恰为结束时刻：拒绝
	in.RequestID = "w2"
	if _, err := w.Apply(in); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("at end err = %v", err)
	}

	// 默认策略 p1 的各类不匹配。
	c.t = c.t.Add(-time.Minute)
	for _, tc := range []struct {
		name   string
		mutate func(*RequestInput)
	}{
		{"unallowed account", func(in *RequestInput) {
			mustAccount(t, w, "u3", 0)
			in.AccountID, in.SessionID, in.DeviceID = "u3", "s3", "dev3"
			if _, err := w.CreateSession("s3", "u3", "dev3", c.t.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong operation", func(in *RequestInput) { in.Operation = "x" }},
		{"wrong payee", func(in *RequestInput) { in.Payee = "x" }},
		{"unknown policy", func(in *RequestInput) { in.PolicyID = "nope" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := baseApply()
			in.RequestID = "deny-" + tc.name
			tc.mutate(&in)
			_, err := w.Apply(in)
			if err == nil {
				t.Fatal("expected denial")
			}
			if !errors.Is(err, ErrPolicyDenied) && !errors.Is(err, ErrPolicyNotFound) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestPerRequestAndSharedQuota(t *testing.T) {
	w, _ := setupApply(t) // MaxPerRequest=30, MaxTotal=50，两个使用账户共享

	big := baseApply()
	big.EstimatedFee = 31
	big.RequestID = "big"
	if _, err := w.Apply(big); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("per-request limit err = %v", err)
	}

	first := baseApply()
	first.EstimatedFee = 30
	if _, err := w.Apply(first); err != nil {
		t.Fatal(err)
	}
	// 第二个使用账户共享累计额度：只剩 20。
	second := baseApply()
	second.AccountID, second.SessionID, second.DeviceID, second.RequestID = "u2", "s2", "dev2", "r2"
	second.EstimatedFee = 30
	if _, err := w.Apply(second); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("shared quota err = %v", err)
	}
	second.EstimatedFee = 20
	if _, err := w.Apply(second); err != nil {
		t.Fatalf("within remaining shared quota: %v", err)
	}
	// 取消第一笔，释放 30 额度并退回余额。
	if _, err := w.Cancel("u1", "r1"); err != nil {
		t.Fatal(err)
	}
	pv, _ := w.Policy("p1")
	if pv.ReservedTotal != 20 || pv.SpentTotal != 0 {
		t.Fatalf("quota after cancel = reserved %d spent %d", pv.ReservedTotal, pv.SpentTotal)
	}
	// 结算第二笔为 10，结算差额同样释放额度。
	if _, err := w.Settle("u2", "r2", 10); err != nil {
		t.Fatal(err)
	}
	pv, _ = w.Policy("p1")
	if pv.ReservedTotal != 0 || pv.SpentTotal != 10 {
		t.Fatalf("quota after settle = %+v", pv)
	}
}

func TestInsufficientBalance(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "poor", 5)
	mustAccount(t, w, "u1", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	spec := validPolicy(c)
	spec.PayerAccountID = "poor"
	spec.AllowedAccountIDs = []string{"u1"}
	if err := w.SavePolicy(spec); err != nil {
		t.Fatal(err)
	}
	in := baseApply()
	if _, err := w.Apply(in); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
	bal, _ := w.Balance("poor")
	if bal != (Balances{Available: 5, Reserved: 0}) {
		t.Fatalf("balance changed on rejection: %+v", bal)
	}
}

func TestRejectionLeavesLedgerReasonAndNoMoneyMovement(t *testing.T) {
	w, _ := setupApply(t)
	in := baseApply()
	in.DeviceID = "wrong"
	if _, err := w.Apply(in); !errors.Is(err, ErrSessionDeviceMismatch) {
		t.Fatal(err)
	}
	all := w.Ledger()
	if len(all) != 1 || all[0].Kind != LedgerRejection || all[0].RequestID != "r1" || all[0].Amount != 0 {
		t.Fatalf("rejection entry = %+v", all)
	}
	if all[0].Reason == "" {
		t.Fatal("rejection reason must not be empty")
	}
	if len(w.AccountLedger("payer")) != 0 {
		t.Fatal("rejection must not create money-moving entries")
	}
	bal, _ := w.Balance("payer")
	if bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance changed: %+v", bal)
	}
}

func TestLedgerConsistencyAcrossLifecycle(t *testing.T) {
	w, _ := setupApply(t)
	if _, err := w.Apply(baseApply()); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Cancel("u1", "r1"); err != nil {
		t.Fatal(err)
	}
	led := w.AccountLedger("payer")
	if len(led) != 2 || led[0].Kind != LedgerReserve || led[0].Amount != 20 ||
		led[1].Kind != LedgerRefund || led[1].Amount != 20 {
		t.Fatalf("ledger = %+v", led)
	}
	for _, e := range led {
		if e.RequestID != "r1" {
			t.Fatalf("entry missing request link: %+v", e)
		}
	}
	// 余额 = 初始 + 账本求和（settle 为负向，reserve 不影响总额，这里直接核对可用/预留）。
	bal, _ := w.Balance("payer")
	if bal.Available != 100 || bal.Reserved != 0 {
		t.Fatalf("final balance = %+v", bal)
	}
}

func TestUnknownRequestTerminalOps(t *testing.T) {
	w, _ := setupApply(t)
	if _, err := w.Settle("u1", "ghost", 1); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("settle missing err = %v", err)
	}
	if _, err := w.Cancel("u1", "ghost"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("cancel missing err = %v", err)
	}
}

func TestConcurrentAppliesSettlesCancels(t *testing.T) {
	w, c := setupApply(t)
	// 额度 50：仅能容纳两笔（20+20），并发下不能超扣。
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			in := baseApply()
			in.AccountID, in.SessionID, in.DeviceID = "u2", "s2", "dev2"
			in.RequestID = fmt.Sprintf("c%d", i)
			_, errs[i] = w.Apply(in)
		}()
	}
	wg.Wait()
	accepted := 0
	for _, err := range errs {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrQuotaExceeded), errors.Is(err, ErrInsufficientBalance):
		default:
			t.Fatalf("unexpected apply err: %v", err)
		}
	}
	if accepted != 2 {
		t.Fatalf("accepted = %d, want exactly 2", accepted)
	}
	bal, _ := w.Balance("payer")
	if bal.Available+bal.Reserved != 100 || bal.Reserved != 40 {
		t.Fatalf("balance = %+v, want available 60 / reserved 40", bal)
	}
	pv, _ := w.Policy("p1")
	if pv.ReservedTotal != 40 || pv.ReservedTotal+pv.SpentTotal > 50 {
		t.Fatalf("shared quota overrun: %+v", pv)
	}

	// 释放前两笔占用的额度（忽略未受理编号的错误），再构造单请求终态竞争。
	for i := 0; i < n; i++ {
		_, _ = w.Cancel("u2", fmt.Sprintf("c%d", i))
	}
	bal, _ = w.Balance("payer")
	if bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance after cleanup = %+v", bal)
	}

	in := baseApply()
	in.RequestID = "race1"
	if _, err := w.Apply(in); err != nil {
		t.Fatal(err)
	}
	var wg2 sync.WaitGroup
	results := make([]RequestView, 4)
	opErrs := make([]error, 4)
	for i := 0; i < 4; i++ {
		i := i
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			if i%2 == 0 {
				results[i], opErrs[i] = w.Settle("u1", "race1", 12)
			} else {
				results[i], opErrs[i] = w.Cancel("u1", "race1")
			}
		}()
	}
	wg2.Wait()
	states := map[RequestState]int{}
	for i, err := range opErrs {
		if err == nil {
			states[results[i].State]++
		} else if !errors.Is(err, ErrAlreadySettled) && !errors.Is(err, ErrAlreadyCancelled) {
			t.Fatalf("unexpected race err: %v", err)
		}
	}
	// 第一个成功操作决定唯一终态：settled 与 cancelled 互斥。
	if states[RequestSettled] > 0 && states[RequestCancelled] > 0 {
		t.Fatalf("request reached both terminal states: %v", states)
	}
	req, _ := w.Request("u1", "race1")
	if req.State != RequestSettled && req.State != RequestCancelled {
		t.Fatalf("request not terminal: %v", req.State)
	}
	bal, _ = w.Balance("payer")
	pv, _ = w.Policy("p1")
	if bal.Available+bal.Reserved+pv.SpentTotal != 100 || bal.Reserved != 0 {
		t.Fatalf("balance not conserved after race: %+v policy=%+v", bal, pv)
	}

	// 同一请求编号并发重复申请：只预留一次，最终预留 20。
	const m = 16
	var wg3 sync.WaitGroup
	wg3.Add(m)
	dups := make([]error, m)
	for i := 0; i < m; i++ {
		i := i
		go func() {
			defer wg3.Done()
			in := baseApply()
			in.RequestID = "dup1"
			_, dups[i] = w.Apply(in)
		}()
	}
	wg3.Wait()
	for _, err := range dups {
		if err != nil {
			t.Fatalf("duplicate apply should be idempotent: %v", err)
		}
	}
	bal, _ = w.Balance("payer")
	pv2, _ := w.Policy("p1")
	if bal.Available+bal.Reserved+pv2.SpentTotal != 100 || bal.Reserved != 20 {
		t.Fatalf("final balance = %+v, want reserved 20", bal)
	}
	if pv2.ReservedTotal != 20 || pv2.ReservedTotal+pv2.SpentTotal > 50 {
		t.Fatalf("shared quota after races: %+v", pv2)
	}
	_ = c
}
