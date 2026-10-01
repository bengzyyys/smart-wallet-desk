package wallet

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testEnv 提供一个时钟可推进的钱包。
type testEnv struct {
	w   *Wallet
	now time.Time
}

func newTestEnv() *testEnv {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	e := &testEnv{now: base}
	e.w = New()
	e.w.now = func() time.Time { return e.now }
	return e
}

func (e *testEnv) advance(d time.Duration) { e.now = e.now.Add(d) }

func (e *testEnv) mustAccount(t *testing.T, id string, balance int64) {
	t.Helper()
	if _, err := e.w.CreateAccount(id, balance); err != nil {
		t.Fatalf("CreateAccount(%s): %v", id, err)
	}
}

func (e *testEnv) mustSession(t *testing.T, accountID, deviceID string, expiresAt time.Time) *Session {
	t.Helper()
	s, err := e.w.CreateSession(accountID, deviceID, expiresAt)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return s
}

func (e *testEnv) mustPolicy(t *testing.T, id, funding string, allowed []string, perTx, cumulative int64) {
	t.Helper()
	_, err := e.w.CreatePolicy(PolicySpec{
		ID: id, FundingAccountID: funding, AllowedAccounts: allowed,
		OpType: "pay", Payee: "shop",
		StartAt: e.now.Add(-time.Hour), EndAt: e.now.Add(24 * time.Hour),
		PerTxCap: perTx, CumulativeCap: cumulative,
	})
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
}

func (e *testEnv) apply(t *testing.T, user string, s *Session, policy, no string, fee int64) *PaymentRequest {
	t.Helper()
	r, err := e.w.Apply(ApplyRequest{
		UserAccountID: user, SessionID: s.ID, DeviceID: s.DeviceID,
		PolicyID: policy, RequestNo: no, OpType: "pay", Payee: "shop",
		EstimatedFee: fee,
	})
	if err != nil {
		t.Fatalf("Apply(%s): %v", no, err)
	}
	return r
}

// ---- 账户 ----

func TestCreateAccount(t *testing.T) {
	e := newTestEnv()

	a, err := e.w.CreateAccount("a", 100)
	if err != nil {
		t.Fatal(err)
	}
	if a.Available != 100 || a.Reserved != 0 {
		t.Fatalf("account = %+v", a)
	}

	// 初始余额为负：拒绝且不留存。
	if _, err := e.w.CreateAccount("neg", -1); err != ErrInvalidAmount {
		t.Fatalf("negative balance err = %v, want ErrInvalidAmount", err)
	}
	if _, err := e.w.GetAccount("neg"); err != ErrAccountNotFound {
		t.Fatalf("GetAccount(neg) err = %v, want ErrAccountNotFound", err)
	}

	// 空编号：拒绝。
	if _, err := e.w.CreateAccount("", 10); err != ErrInvalidAmount {
		t.Fatalf("empty id err = %v, want ErrInvalidAmount", err)
	}

	// 重复账户：拒绝且原账户不变。
	if _, err := e.w.CreateAccount("a", 50); err != ErrAccountExists {
		t.Fatalf("duplicate err = %v, want ErrAccountExists", err)
	}
	a2, _ := e.w.GetAccount("a")
	if a2.Available != 100 {
		t.Fatalf("original account changed: %+v", a2)
	}
}

func TestGetAccountNotFound(t *testing.T) {
	e := newTestEnv()
	if _, err := e.w.GetAccount("ghost"); err != ErrAccountNotFound {
		t.Fatalf("err = %v, want ErrAccountNotFound", err)
	}
}

// ---- 会话 ----

func TestSessionLifecycle(t *testing.T) {
	e := newTestEnv()
	e.mustAccount(t, "u1", 0)

	s := e.mustSession(t, "u1", "dev1", e.now.Add(time.Hour))
	if s.Revoked {
		t.Fatal("new session should not be revoked")
	}

	// 未知账户。
	if _, err := e.w.CreateSession("ghost", "d", e.now.Add(time.Hour)); err != ErrAccountNotFound {
		t.Fatalf("err = %v, want ErrAccountNotFound", err)
	}
	// 空设备。
	if _, err := e.w.CreateSession("u1", "", e.now.Add(time.Hour)); err != ErrDeviceRequired {
		t.Fatalf("err = %v, want ErrDeviceRequired", err)
	}
	// 查询不存在的会话。
	if _, err := e.w.GetSession("nope"); err != ErrSessionNotFound {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}
	// 吊销不存在的会话。
	if err := e.w.RevokeSession("nope"); err != ErrSessionNotFound {
		t.Fatalf("err = %v, want ErrSessionNotFound", err)
	}

	// 到期时刻及之后视为过期。
	expiring := e.mustSession(t, "u1", "dev2", e.now.Add(time.Hour))
	e.advance(time.Hour)
	_, err := e.w.Apply(ApplyRequest{
		UserAccountID: "u1", SessionID: expiring.ID, DeviceID: "dev2",
		PolicyID: "p", RequestNo: "r1", OpType: "pay", Payee: "shop", EstimatedFee: 1,
	})
	if err != ErrSessionExpired {
		t.Fatalf("at expiry err = %v, want ErrSessionExpired", err)
	}
	e.advance(-time.Nanosecond) // 到期前一刻仍然有效（此处仅校验不报错到会话层）
	if err := e.w.RevokeSession(expiring.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.w.Apply(ApplyRequest{
		UserAccountID: "u1", SessionID: expiring.ID, DeviceID: "dev2",
		PolicyID: "p", RequestNo: "r2", OpType: "pay", Payee: "shop", EstimatedFee: 1,
	})
	if err != ErrSessionRevoked {
		t.Fatalf("after revoke err = %v, want ErrSessionRevoked", err)
	}
}

func TestSessionWrongAccountAndDevice(t *testing.T) {
	e := newTestEnv()
	e.mustAccount(t, "u1", 0)
	e.mustAccount(t, "u2", 0)
	s := e.mustSession(t, "u1", "dev1", e.now.Add(time.Hour))

	// 会话属于 u1，u2 拿来用：账户不符。
	_, err := e.w.Apply(ApplyRequest{
		UserAccountID: "u2", SessionID: s.ID, DeviceID: "dev1",
		PolicyID: "p", RequestNo: "r1", OpType: "pay", Payee: "shop", EstimatedFee: 1,
	})
	if err != ErrSessionWrongAccount {
		t.Fatalf("err = %v, want ErrSessionWrongAccount", err)
	}
	// 设备不符。
	_, err = e.w.Apply(ApplyRequest{
		UserAccountID: "u1", SessionID: s.ID, DeviceID: "dev2",
		PolicyID: "p", RequestNo: "r2", OpType: "pay", Payee: "shop", EstimatedFee: 1,
	})
	if err != ErrSessionWrongDevice {
		t.Fatalf("err = %v, want ErrSessionWrongDevice", err)
	}
}

func TestRevokedSessionSettleCancelStillWorks(t *testing.T) {
	e := newTestEnv()
	e.mustAccount(t, "fund", 1000)
	e.mustAccount(t, "u1", 0)
	s := e.mustSession(t, "u1", "dev1", e.now.Add(time.Hour))
	e.mustPolicy(t, "p", "fund", []string{"u1"}, 100, 1000)

	r := e.apply(t, "u1", s, "p", "r1", 30)
	r2 := e.apply(t, "u1", s, "p", "r3", 30)
	if err := e.w.RevokeSession(s.ID); err != nil {
		t.Fatal(err)
	}
	// 吊销后新申请不再受理。
	if _, err := e.w.Apply(ApplyRequest{
		UserAccountID: "u1", SessionID: s.ID, DeviceID: "dev1",
		PolicyID: "p", RequestNo: "r2", OpType: "pay", Payee: "shop", EstimatedFee: 10,
	}); err != ErrSessionRevoked {
		t.Fatalf("new apply after revoke err = %v, want ErrSessionRevoked", err)
	}
	// 已预留请求仍可结算。
	settled, err := e.w.Settle("u1", r.RequestNo, 20)
	if err != nil {
		t.Fatalf("settle after revoke: %v", err)
	}
	if settled.Status != StatusSettled || settled.ActualFee != 20 {
		t.Fatalf("settled = %+v", settled)
	}
	// 已预留请求仍可取消。
	cancelled, err := e.w.Cancel("u1", r2.RequestNo)
	if err != nil {
		t.Fatalf("cancel after revoke: %v", err)
	}
	if cancelled.Status != StatusCancelled {
		t.Fatalf("cancelled = %+v", cancelled)
	}
}

// ---- 策略 ----

func TestCreatePolicyValidation(t *testing.T) {
	e := newTestEnv()
	e.mustAccount(t, "fund", 0)
	e.mustAccount(t, "u1", 0)

	valid := func() PolicySpec {
		return PolicySpec{
			ID: "p1", FundingAccountID: "fund", AllowedAccounts: []string{"u1"},
			OpType: "pay", Payee: "shop",
			StartAt: e.now.Add(-time.Hour), EndAt: e.now.Add(time.Hour),
			PerTxCap: 100, CumulativeCap: 1000,
		}
	}

	// 正常保存。
	if _, err := e.w.CreatePolicy(valid()); err != nil {
		t.Fatal(err)
	}
	// 重复编号。
	if _, err := e.w.CreatePolicy(valid()); err != ErrPolicyExists {
		t.Fatalf("duplicate policy err = %v, want ErrPolicyExists", err)
	}

	cases := []struct {
		name string
		mod  func(*PolicySpec)
	}{
		{"missing funding account", func(s *PolicySpec) { s.FundingAccountID = "ghost" }},
		{"missing allowed account", func(s *PolicySpec) { s.AllowedAccounts = []string{"ghost"} }},
		{"empty allowed", func(s *PolicySpec) { s.AllowedAccounts = nil }},
		{"zero per-tx cap", func(s *PolicySpec) { s.PerTxCap = 0 }},
		{"negative per-tx cap", func(s *PolicySpec) { s.PerTxCap = -1 }},
		{"zero cumulative cap", func(s *PolicySpec) { s.CumulativeCap = 0 }},
		{"start equal end", func(s *PolicySpec) { s.StartAt = s.EndAt }},
		{"start after end", func(s *PolicySpec) { s.StartAt = s.EndAt.Add(time.Second) }},
		{"empty op type", func(s *PolicySpec) { s.OpType = "" }},
		{"empty payee", func(s *PolicySpec) { s.Payee = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(tt *testing.T) {
			spec := valid()
			spec.ID = "p-" + tc.name
			tc.mod(&spec)
			if _, err := e.w.CreatePolicy(spec); err != ErrInvalidPolicy {
				tt.Fatalf("err = %v, want ErrInvalidPolicy", err)
			}
		})
	}

	// 不存在的策略。
	if _, err := e.w.GetPolicy("nope"); err != ErrPolicyNotFound {
		t.Fatalf("err = %v, want ErrPolicyNotFound", err)
	}
}

// ---- 申请：授权与拒绝 ----

func TestApplySuccessReservesAndRecordsLedger(t *testing.T) {
	e := newTestEnv()
	e.mustAccount(t, "fund", 1000)
	e.mustAccount(t, "u1", 0)
	s := e.mustSession(t, "u1", "dev1", e.now.Add(time.Hour))
	e.mustPolicy(t, "p", "fund", []string{"u1"}, 100, 500)

	r := e.apply(t, "u1", s, "p", "r1", 30)
	if r.Status != StatusPending {
		t.Fatalf("status = %s", r.Status)
	}

	fund, _ := e.w.GetAccount("fund")
	if fund.Available != 970 || fund.Reserved != 30 {
		t.Fatalf("fund = %+v", fund)
	}

	entries, err := e.w.Ledger("fund")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("ledger = %+v", entries)
	}
	le := entries[0]
	if le.Type != "reserve" || le.Amount != 30 || le.RequestID != r.ID {
		t.Fatalf("ledger entry = %+v", le)
	}
}

func TestApplyRejections(t *testing.T) {
	e := newTestEnv()
	e.mustAccount(t, "fund", 1000)
	e.mustAccount(t, "u1", 0)
	e.mustAccount(t, "u2", 0)
	s1 := e.mustSession(t, "u1", "dev1", e.now.Add(24*time.Hour))
	s2 := e.mustSession(t, "u2", "dev2", e.now.Add(24*time.Hour))
	// 策略只允许 u1。
	e.mustPolicy(t, "p", "fund", []string{"u1"}, 100, 500)

	try := func(name, user string, s *Session, policy, op, payee, no string, fee int64, wantErr error) {
		t.Helper()
		_, err := e.w.Apply(ApplyRequest{
			UserAccountID: user, SessionID: s.ID, DeviceID: s.DeviceID,
			PolicyID: policy, RequestNo: no, OpType: op, Payee: payee, EstimatedFee: fee,
		})
		if err != wantErr {
			t.Fatalf("%s: err = %v, want %v", name, err, wantErr)
		}
	}

	try("unknown policy", "u1", s1, "nope", "pay", "shop", "r1", 10, ErrPolicyNotFound)
	try("user not allowed", "u2", s2, "p", "pay", "shop", "r2", 10, ErrUnauthorized)
	try("op mismatch", "u1", s1, "p", "refund", "shop", "r3", 10, ErrUnauthorized)
	try("payee mismatch", "u1", s1, "p", "pay", "other", "r4", 10, ErrUnauthorized)
	try("zero fee", "u1", s1, "p", "pay", "shop", "r5", 0, ErrInvalidFee)
	try("negative fee", "u1", s1, "p", "pay", "shop", "r6", -5, ErrInvalidFee)
	try("fee over per-tx cap", "u1", s1, "p", "pay", "shop", "r7", 101, ErrFeeExceedsCap)
	try("empty request no", "u1", s1, "p", "pay", "shop", "", 10, ErrRequestNoRequired)

	// 时间窗口：含开始、不含结束。
	winStart := e.now.Add(-time.Minute)
	winEnd := e.now.Add(time.Hour)
	_, err := e.w.CreatePolicy(PolicySpec{
		ID: "win", FundingAccountID: "fund", AllowedAccounts: []string{"u1"},
		OpType: "pay", Payee: "shop", StartAt: winStart, EndAt: winEnd,
		PerTxCap: 100, CumulativeCap: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 开始时刻可以受理。
	if _, err := e.w.Apply(ApplyRequest{
		UserAccountID: "u1", SessionID: s1.ID, DeviceID: "dev1",
		PolicyID: "win", RequestNo: "r8", OpType: "pay", Payee: "shop", EstimatedFee: 10,
	}); err != nil {
		t.Fatalf("at window start: %v", err)
	}
	e.advance(time.Hour + time.Minute) // 越过结束时刻
	_, err = e.w.Apply(ApplyRequest{
		UserAccountID: "u1", SessionID: s1.ID, DeviceID: "dev1",
		PolicyID: "win", RequestNo: "r9", OpType: "pay", Payee: "shop", EstimatedFee: 10,
	})
	if err != ErrUnauthorized {
		t.Fatalf("at window end: err = %v, want ErrUnauthorized", err)
	}

	// 出资余额不足。
	_, err = e.w.CreatePolicy(PolicySpec{
		ID: "poor", FundingAccountID: "fund", AllowedAccounts: []string{"u1"},
		OpType: "pay", Payee: "shop",
		StartAt: e.now.Add(-time.Hour), EndAt: e.now.Add(24 * time.Hour),
		PerTxCap: 10000, CumulativeCap: 100000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.w.Apply(ApplyRequest{
		UserAccountID: "u1", SessionID: s1.ID, DeviceID: "dev1",
		PolicyID: "poor", RequestNo: "r10", OpType: "pay", Payee: "shop", EstimatedFee: 9999,
	}); err != ErrInsufficientFunds {
		t.Fatalf("insufficient funds err = %v, want ErrInsufficientFunds", err)
	}

	// 拒绝都留下了具体原因，且没有任何扣款。
	rej := e.w.Rejections("u1")
	if len(rej) != 9 {
		t.Fatalf("rejections = %d, want 9: %+v", len(rej), rej)
	}
	if u2rej := e.w.Rejections("u2"); len(u2rej) != 1 {
		t.Fatalf("u2 rejections = %d, want 1", len(u2rej))
	}
	for _, r := range rej {
		if r.Reason == "" {
			t.Fatalf("rejection without reason: %+v", r)
		}
	}
	fund, _ := e.w.GetAccount("fund")
	if fund.Available != 990 || fund.Reserved != 10 {
		t.Fatalf("fund after rejections = %+v", fund)
	}
	entries, _ := e.w.Ledger("fund")
	if len(entries) != 1 { // 只有 r8 一笔预留
		t.Fatalf("ledger entries = %d, want 1", len(entries))
	}
}

func TestCumulativeCapSharedAndReleased(t *testing.T) {
	e := newTestEnv()
	e.mustAccount(t, "fund", 100000)
	e.mustAccount(t, "u1", 0)
	e.mustAccount(t, "u2", 0)
	s1 := e.mustSession(t, "u1", "d1", e.now.Add(time.Hour))
	s2 := e.mustSession(t, "u2", "d2", e.now.Add(time.Hour))
	e.mustPolicy(t, "p", "fund", []string{"u1", "u2"}, 100, 100)

	// 共享累计额度 100：u1 占 60。
	e.apply(t, "u1", s1, "p", "a1", 60)
	// u2 再要 50：超累计上限。
	_, err := e.w.Apply(ApplyRequest{
		UserAccountID: "u2", SessionID: s2.ID, DeviceID: "d2",
		PolicyID: "p", RequestNo: "b1", OpType: "pay", Payee: "shop", EstimatedFee: 50,
	})
	if err != ErrFeeExceedsCap {
		t.Fatalf("err = %v, want ErrFeeExceedsCap", err)
	}
	// u2 要 40：恰好占满。
	e.apply(t, "u2", s2, "p", "b2", 40)

	// 取消 u1 的 60，释放额度。
	if _, err := e.w.Cancel("u1", "a1"); err != nil {
		t.Fatal(err)
	}
	// u2 再用 60：可以（40+60=100）。
	e.apply(t, "u2", s2, "p", "b3", 60)

	// 结算 b3 实际 30：预留 60 释放、实付 30，累计占用变为 40+30=70。
	if _, err := e.w.Settle("u2", "b3", 30); err != nil {
		t.Fatal(err)
	}
	// 再用 30：恰好 100。
	e.apply(t, "u1", s1, "p", "a2", 30)
	// 再用 1：超出。
	_, err = e.w.Apply(ApplyRequest{
		UserAccountID: "u1", SessionID: s1.ID, DeviceID: "d1",
		PolicyID: "p", RequestNo: "a3", OpType: "pay", Payee: "shop", EstimatedFee: 1,
	})
	if err != ErrFeeExceedsCap {
		t.Fatalf("err = %v, want ErrFeeExceedsCap", err)
	}
}

// ---- 申请：幂等 ----

func TestApplyIdempotentReplay(t *testing.T) {
	e := newTestEnv()
	e.mustAccount(t, "fund", 1000)
	e.mustAccount(t, "u1", 0)
	s := e.mustSession(t, "u1", "d1", e.now.Add(time.Hour))
	e.mustPolicy(t, "p", "fund", []string{"u1"}, 100, 1000)

	r1 := e.apply(t, "u1", s, "p", "r1", 30)
	entriesBefore, _ := e.w.Ledger("fund")

	// 完全一致的重放：返回已有结果，不再预留。
	r2, err := e.w.Apply(ApplyRequest{
		UserAccountID: "u1", SessionID: s.ID, DeviceID: "d1",
		PolicyID: "p", RequestNo: "r1", OpType: "pay", Payee: "shop", EstimatedFee: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r2.ID != r1.ID || r2.Status != r1.Status {
		t.Fatalf("replay returned different request: %+v vs %+v", r2, r1)
	}
	entriesAfter, _ := e.w.Ledger("fund")
	if len(entriesAfter) != len(entriesBefore) {
		t.Fatal("replay created new ledger entries")
	}

	// 任一内容不同：冲突。
	conflicts := []ApplyRequest{
		{PolicyID: "p", RequestNo: "r1", OpType: "pay", Payee: "shop", EstimatedFee: 31},
		{PolicyID: "p", RequestNo: "r1", OpType: "pay", Payee: "other", EstimatedFee: 30},
		{PolicyID: "p", RequestNo: "r1", OpType: "refund", Payee: "shop", EstimatedFee: 30},
		{PolicyID: "other", RequestNo: "r1", OpType: "pay", Payee: "shop", EstimatedFee: 30},
	}
	for i, c := range conflicts {
		c.UserAccountID = "u1"
		c.SessionID = s.ID
		c.DeviceID = "d1"
		if _, err := e.w.Apply(c); err != ErrRequestConflict {
			t.Fatalf("conflict case %d: err = %v, want ErrRequestConflict", i, err)
		}
	}

	// 取消后用同样的请求编号重新申请：不能重新扣款。
	if _, err := e.w.Cancel("u1", "r1"); err != nil {
		t.Fatal(err)
	}
	r3, err := e.w.Apply(ApplyRequest{
		UserAccountID: "u1", SessionID: s.ID, DeviceID: "d1",
		PolicyID: "p", RequestNo: "r1", OpType: "pay", Payee: "shop", EstimatedFee: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r3.Status != StatusCancelled || r3.ID != r1.ID {
		t.Fatalf("re-apply after cancel = %+v", r3)
	}
	fund, _ := e.w.GetAccount("fund")
	if fund.Available != 1000 || fund.Reserved != 0 {
		t.Fatalf("fund after re-apply = %+v", fund)
	}
}

// ---- 结算 ----

func TestSettle(t *testing.T) {
	e := newTestEnv()
	e.mustAccount(t, "fund", 1000)
	e.mustAccount(t, "u1", 0)
	s := e.mustSession(t, "u1", "d1", e.now.Add(time.Hour))
	e.mustPolicy(t, "p", "fund", []string{"u1"}, 100, 1000)

	// 实际费用等于预估：无退回。
	e.apply(t, "u1", s, "p", "r1", 30)
	if _, err := e.w.Settle("u1", "r1", 30); err != nil {
		t.Fatal(err)
	}
	fund, _ := e.w.GetAccount("fund")
	if fund.Available != 970 || fund.Reserved != 0 {
		t.Fatalf("fund = %+v", fund)
	}
	entries, _ := e.w.Ledger("fund")
	if len(entries) != 2 || entries[1].Type != "deduct" || entries[1].Amount != 30 {
		t.Fatalf("ledger = %+v", entries)
	}

	// 实际费用小于预估：扣实际、退差额。
	e.apply(t, "u1", s, "p", "r2", 30)
	if _, err := e.w.Settle("u1", "r2", 20); err != nil {
		t.Fatal(err)
	}
	fund, _ = e.w.GetAccount("fund")
	if fund.Available != 950 || fund.Reserved != 0 {
		t.Fatalf("fund = %+v", fund)
	}
	entries, _ = e.w.Ledger("fund")
	if len(entries) != 5 {
		t.Fatalf("ledger = %+v", entries)
	}
	if entries[3].Type != "deduct" || entries[3].Amount != 20 {
		t.Fatalf("deduct entry = %+v", entries[3])
	}
	if entries[4].Type != "refund" || entries[4].Amount != 10 {
		t.Fatalf("refund entry = %+v", entries[4])
	}

	// 实际费用为零：全额退回，扣 0。
	e.apply(t, "u1", s, "p", "r3", 30)
	if _, err := e.w.Settle("u1", "r3", 0); err != nil {
		t.Fatal(err)
	}
	fund, _ = e.w.GetAccount("fund")
	if fund.Available != 950 || fund.Reserved != 0 {
		t.Fatalf("fund = %+v", fund)
	}

	// 超预估：拒绝并保留预留。
	e.apply(t, "u1", s, "p", "r4", 30)
	if _, err := e.w.Settle("u1", "r4", 31); err != ErrInvalidActualFee {
		t.Fatalf("over-estimate err = %v, want ErrInvalidActualFee", err)
	}
	r4, _ := e.w.GetRequest("u1", "r4")
	if r4.Status != StatusPending {
		t.Fatalf("r4 status = %s, want pending", r4.Status)
	}
	fund, _ = e.w.GetAccount("fund")
	if fund.Available != 920 || fund.Reserved != 30 {
		t.Fatalf("fund after rejected settle = %+v", fund)
	}
	// 负数：拒绝。
	if _, err := e.w.Settle("u1", "r4", -1); err != ErrInvalidActualFee {
		t.Fatalf("negative err = %v, want ErrInvalidActualFee", err)
	}

	// 重复结算：相同费用幂等，不重复记账。
	before, _ := e.w.Ledger("fund")
	if _, err := e.w.Settle("u1", "r2", 20); err != nil {
		t.Fatal(err)
	}
	after, _ := e.w.Ledger("fund")
	if len(after) != len(before) {
		t.Fatal("duplicate settle created ledger entries")
	}
	// 不同费用：冲突。
	if _, err := e.w.Settle("u1", "r2", 21); err != ErrRequestConflict {
		t.Fatalf("different-fee settle err = %v, want ErrRequestConflict", err)
	}

	// 已结算不能取消。
	if _, err := e.w.Cancel("u1", "r1"); err != ErrAlreadySettled {
		t.Fatalf("cancel settled err = %v, want ErrAlreadySettled", err)
	}
}

// ---- 取消 ----

func TestCancel(t *testing.T) {
	e := newTestEnv()
	e.mustAccount(t, "fund", 1000)
	e.mustAccount(t, "u1", 0)
	s := e.mustSession(t, "u1", "d1", e.now.Add(time.Hour))
	e.mustPolicy(t, "p", "fund", []string{"u1"}, 100, 1000)

	e.apply(t, "u1", s, "p", "r1", 30)
	if _, err := e.w.Cancel("u1", "r1"); err != nil {
		t.Fatal(err)
	}
	fund, _ := e.w.GetAccount("fund")
	if fund.Available != 1000 || fund.Reserved != 0 {
		t.Fatalf("fund = %+v", fund)
	}
	entries, _ := e.w.Ledger("fund")
	if len(entries) != 2 || entries[1].Type != "refund" || entries[1].Amount != 30 {
		t.Fatalf("ledger = %+v", entries)
	}

	// 重复取消幂等，不重复记账。
	if _, err := e.w.Cancel("u1", "r1"); err != nil {
		t.Fatal(err)
	}
	entries, _ = e.w.Ledger("fund")
	if len(entries) != 2 {
		t.Fatal("duplicate cancel created ledger entries")
	}

	// 已取消不能结算。
	if _, err := e.w.Settle("u1", "r1", 10); err != ErrAlreadyCancelled {
		t.Fatalf("settle cancelled err = %v, want ErrAlreadyCancelled", err)
	}

	// 不存在的请求。
	if _, err := e.w.Cancel("u1", "nope"); err != ErrRequestNotFound {
		t.Fatalf("err = %v, want ErrRequestNotFound", err)
	}
	if _, err := e.w.Settle("u1", "nope", 10); err != ErrRequestNotFound {
		t.Fatalf("err = %v, want ErrRequestNotFound", err)
	}
}

// ---- 并发 ----

func TestConcurrencyApplyNoOverDeduction(t *testing.T) {
	e := newTestEnv()
	e.mustAccount(t, "fund", 10000)
	e.mustAccount(t, "u1", 0)
	s := e.mustSession(t, "u1", "d1", time.Now().Add(time.Hour))
	e.mustPolicy(t, "p", "fund", []string{"u1"}, 10, 1<<60)

	const n = 2000
	var wg sync.WaitGroup
	var accepted int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := e.w.Apply(ApplyRequest{
				UserAccountID: "u1", SessionID: s.ID, DeviceID: "d1",
				PolicyID: "p", RequestNo: fmt.Sprintf("r-%d", i), OpType: "pay", Payee: "shop",
				EstimatedFee: 10,
			})
			if err == nil {
				atomic.AddInt64(&accepted, 1)
			}
		}(i)
	}
	wg.Wait()

	if accepted != 1000 {
		t.Fatalf("accepted = %d, want 1000", accepted)
	}
	fund, _ := e.w.GetAccount("fund")
	if fund.Available != 0 || fund.Reserved != 10000 {
		t.Fatalf("fund = %+v", fund)
	}
	entries, _ := e.w.Ledger("fund")
	var deducts int64
	for _, le := range entries {
		if le.Type == "deduct" {
			deducts += le.Amount
		}
	}
	if fund.Available+fund.Reserved != 10000-deducts {
		t.Fatalf("fund = %+v, deducts = %d", fund, deducts)
	}
}

func TestConcurrencySharedCumulativeCap(t *testing.T) {
	e := newTestEnv()
	e.mustAccount(t, "fund", 1<<60)
	e.mustAccount(t, "u1", 0)
	e.mustAccount(t, "u2", 0)
	s1 := e.mustSession(t, "u1", "d1", time.Now().Add(time.Hour))
	s2 := e.mustSession(t, "u2", "d2", time.Now().Add(time.Hour))
	e.mustPolicy(t, "p", "fund", []string{"u1", "u2"}, 10, 1000)

	const perUser = 500
	var wg sync.WaitGroup
	var accepted int64
	for i := 0; i < perUser; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if _, err := e.w.Apply(ApplyRequest{
				UserAccountID: "u1", SessionID: s1.ID, DeviceID: "d1",
				PolicyID: "p", RequestNo: fmt.Sprintf("u1-%d", i), OpType: "pay", Payee: "shop",
				EstimatedFee: 10,
			}); err == nil {
				atomic.AddInt64(&accepted, 1)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if _, err := e.w.Apply(ApplyRequest{
				UserAccountID: "u2", SessionID: s2.ID, DeviceID: "d2",
				PolicyID: "p", RequestNo: fmt.Sprintf("u2-%d", i), OpType: "pay", Payee: "shop",
				EstimatedFee: 10,
			}); err == nil {
				atomic.AddInt64(&accepted, 1)
			}
		}(i)
	}
	wg.Wait()

	if accepted != 100 {
		t.Fatalf("accepted = %d, want 100", accepted)
	}
	pol, _ := e.w.GetPolicy("p")
	if pol.cumulativeUsed != 1000 {
		t.Fatalf("cumulativeUsed = %d, want 1000", pol.cumulativeUsed)
	}
	fund, _ := e.w.GetAccount("fund")
	if fund.Reserved != 1000 {
		t.Fatalf("reserved = %d, want 1000", fund.Reserved)
	}
}

func TestConcurrencySettleVsCancelOneTerminalState(t *testing.T) {
	for iter := 0; iter < 200; iter++ {
		e := newTestEnv()
		e.mustAccount(t, "fund", 1000)
		e.mustAccount(t, "u1", 0)
		s := e.mustSession(t, "u1", "d1", time.Now().Add(time.Hour))
		e.mustPolicy(t, "p", "fund", []string{"u1"}, 100, 1000)
		e.apply(t, "u1", s, "p", "r1", 100)

		var wg sync.WaitGroup
		wg.Add(2)
		var settleErr, cancelErr error
		go func() { defer wg.Done(); _, settleErr = e.w.Settle("u1", "r1", 80) }()
		go func() { defer wg.Done(); _, cancelErr = e.w.Cancel("u1", "r1") }()
		wg.Wait()

		// 恰好一个成功。
		if (settleErr == nil) == (cancelErr == nil) {
			t.Fatalf("iter %d: settleErr = %v, cancelErr = %v, want exactly one nil", iter, settleErr, cancelErr)
		}
		r, _ := e.w.GetRequest("u1", "r1")
		if r.Status != StatusSettled && r.Status != StatusCancelled {
			t.Fatalf("iter %d: status = %s", iter, r.Status)
		}

		fund, _ := e.w.GetAccount("fund")
		entries, _ := e.w.Ledger("fund")
		var deducts int64
		for _, le := range entries {
			if le.Type == "deduct" {
				deducts += le.Amount
			}
		}
		if fund.Available+fund.Reserved != 1000-deducts {
			t.Fatalf("iter %d: fund = %+v, deducts = %d", iter, fund, deducts)
		}
		if fund.Reserved != 0 {
			t.Fatalf("iter %d: reserved = %d, want 0", iter, fund.Reserved)
		}
	}
}

// ---- 不变量：余额变化与账本金额一致 ----

func TestBalanceLedgerInvariant(t *testing.T) {
	e := newTestEnv()
	const initial = 100000
	e.mustAccount(t, "fund", initial)
	for _, u := range []string{"u1", "u2", "u3"} {
		e.mustAccount(t, u, 0)
	}
	sessions := map[string]*Session{}
	for _, u := range []string{"u1", "u2", "u3"} {
		sessions[u] = e.mustSession(t, u, "dev-"+u, e.now.Add(365*24*time.Hour))
	}
	for _, p := range []string{"p1", "p2"} {
		_, err := e.w.CreatePolicy(PolicySpec{
			ID: p, FundingAccountID: "fund", AllowedAccounts: []string{"u1", "u2", "u3"},
			OpType: "pay", Payee: "shop",
			StartAt: e.now.Add(-time.Hour), EndAt: e.now.Add(365 * 24 * time.Hour),
			PerTxCap: 500, CumulativeCap: 5000,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 500; i++ {
		u := []string{"u1", "u2", "u3"}[rng.Intn(3)]
		pol := []string{"p1", "p2"}[rng.Intn(2)]
		s := sessions[u]
		switch rng.Intn(3) {
		case 0:
			fee := int64(rng.Intn(600)) + 1
			_, _ = e.w.Apply(ApplyRequest{
				UserAccountID: u, SessionID: s.ID, DeviceID: s.DeviceID,
				PolicyID: pol, RequestNo: fmt.Sprintf("r-%s-%d", u, i),
				OpType: "pay", Payee: "shop", EstimatedFee: fee,
			})
		case 1:
			reqs := e.w.ListRequests(u)
			if len(reqs) > 0 {
				r := reqs[rng.Intn(len(reqs))]
				if r.Status == StatusPending {
					actual := int64(rng.Intn(int(r.EstimatedFee) + 1))
					_, _ = e.w.Settle(u, r.RequestNo, actual)
				}
			}
		case 2:
			reqs := e.w.ListRequests(u)
			if len(reqs) > 0 {
				r := reqs[rng.Intn(len(reqs))]
				if r.Status == StatusPending {
					_, _ = e.w.Cancel(u, r.RequestNo)
				}
			}
		}

		// 不变量：可用余额 = 初始余额 + 账本金额合计；预留 = 所有未结算申请的预估费。
		fund, err := e.w.GetAccount("fund")
		if err != nil {
			t.Fatal(err)
		}
		entries, err := e.w.Ledger("fund")
		if err != nil {
			t.Fatal(err)
		}
		var deducts int64
		for _, le := range entries {
			if le.Type == "deduct" {
				deducts += le.Amount
			}
		}
		if got := initial - deducts; fund.Available+fund.Reserved != got {
			t.Fatalf("step %d: available+reserved = %d, want %d (initial %d - deducts %d)", i, fund.Available+fund.Reserved, got, initial, deducts)
		}
		var reserved int64
		for _, pr := range e.w.requests {
			if pr.Status == StatusPending {
				reserved += pr.EstimatedFee
			}
		}
		if reserved != fund.Reserved {
			t.Fatalf("step %d: reserved = %d, want %d", i, fund.Reserved, reserved)
		}
		// 策略累计占用 = 预留中预估费 + 已结算实际费。
		for _, pid := range []string{"p1", "p2"} {
			p, _ := e.w.GetPolicy(pid)
			var used int64
			for _, pr := range e.w.requests {
				if pr.PolicyID != pid {
					continue
				}
				switch pr.Status {
				case StatusPending:
					used += pr.EstimatedFee
				case StatusSettled:
					used += pr.ActualFee
				}
			}
			if used != p.cumulativeUsed {
				t.Fatalf("step %d: policy %s cumulativeUsed = %d, want %d", i, pid, p.cumulativeUsed, used)
			}
		}
	}
}
