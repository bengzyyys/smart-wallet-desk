package wallet

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// setupSharedQuotaApproval 构造共享累计额度的审批场景：出资账户 payer
// （200），使用账户 u1/u2 各持绑定自己设备的有效会话，出资账户持审批会话
// sa；一条允许 u1/u2 的策略 p-shared：单次 60、共享累计 100、审批门槛 10、
// 等待 1 分钟、关闭预留超时。
func setupSharedQuotaApproval(t *testing.T) (*Wallet, *clock) {
	t.Helper()
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 200)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s2", "u2", "dev2", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "dev-approve", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	p := validPolicy(c)
	p.ID = "p-shared"
	p.MaxPerRequest = 60
	p.MaxTotal = 100
	p.ApprovalThreshold = 10
	p.ApprovalWait = time.Minute
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	return w, c
}

// sharedQuotaApply 构造指定使用账户、预估费用 60 的申请；两名使用账户使用
// 相同的请求编号，但各自账户下仍是独立请求。
func sharedQuotaApply(accountID, sessionID, deviceID string) RequestInput {
	return RequestInput{
		PolicyID:     "p-shared",
		RequestID:    "r-shared",
		AccountID:    accountID,
		SessionID:    sessionID,
		DeviceID:     deviceID,
		Operation:    "charge",
		Payee:        "shop",
		EstimatedFee: 60,
	}
}

func ledgerKindCount(w *Wallet, kind LedgerKind) int {
	n := 0
	for _, e := range w.Ledger() {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// 两名使用账户共用一条策略的累计额度，同时提交超门槛申请并同时获批时，
// 只有一笔能预留成功；另一笔返回额度超限并继续待审批，不得转为拒绝终态，
// 也不能因出资余额充足而两笔都获批。
func TestConcurrentApproveSharedQuotaAcrossAccounts(t *testing.T) {
	w, c := setupSharedQuotaApproval(t)

	// 两人各提交预估费用 60 的申请（同一请求编号、各自账户下独立）。
	in1 := sharedQuotaApply("u1", "s1", "dev1")
	in2 := sharedQuotaApply("u2", "s2", "dev2")
	pend1, err := w.Apply(in1)
	if err != nil {
		t.Fatal(err)
	}
	pend2, err := w.Apply(in2)
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []RequestView{pend1, pend2} {
		if req.State != RequestPendingApproval {
			t.Fatalf("state = %v, want pending approval", req.State)
		}
		if !req.CreatedAt.Equal(c.t) || !req.WaitDeadline.Equal(c.t.Add(time.Minute)) {
			t.Fatalf("pending timing = created %v deadline %v", req.CreatedAt, req.WaitDeadline)
		}
	}
	// 待审批不冻结余额、不占用共享累计额度。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 200, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {200 0}", bal)
	}
	if pv, _ := w.Policy("p-shared"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}

	// 出资账户用有效审批会话同时批准这两笔申请。
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	results := make(map[string]error, 2)
	var mu sync.Mutex
	for _, accountID := range []string{"u1", "u2"} {
		accountID := accountID
		go func() {
			defer wg.Done()
			<-start
			_, err := w.Approve(accountID, "r-shared", "sa", "dev-approve")
			mu.Lock()
			results[accountID] = err
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	// 只能有一笔成功；另一笔返回可识别的额度超限错误，且不能是余额不足
	// （出资 200 足够覆盖两笔 60）。
	var winner, loser string
	for accountID, err := range results {
		switch {
		case err == nil:
			if winner != "" {
				t.Fatal("both approvals succeeded despite shared quota 100 < 120")
			}
			winner = accountID
		case errors.Is(err, ErrQuotaExceeded):
			loser = accountID
		default:
			t.Fatalf("approve %s err = %v, want nil or ErrQuotaExceeded", accountID, err)
		}
	}
	if winner == "" || loser == "" {
		t.Fatalf("results = %v, want exactly one success and one quota failure", results)
	}

	// 出资账户可用 140、预留 60；策略预留总额 60、已花费 0。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 140, Reserved: 60}) {
		t.Fatalf("balance = %+v, want {140 60}", bal)
	}
	if pv, _ := w.Policy("p-shared"); pv.ReservedTotal != 60 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 60/0", pv.ReservedTotal, pv.SpentTotal)
	}

	// 胜出申请转为已预留，记录批准决定。
	winView, err := w.Request(winner, "r-shared")
	if err != nil {
		t.Fatal(err)
	}
	if winView.State != RequestReserved {
		t.Fatalf("winner state = %v, want reserved", winView.State)
	}
	if winView.ApproverAccountID != "payer" || !winView.DecidedAt.Equal(c.t) || !winView.ReservedAt.Equal(c.t) {
		t.Fatalf("winner decision fields = %+v", winView)
	}

	// 失败申请继续待审批，不能转成拒绝终态；提交时间与等待截止时间保持
	// 原值，且没有审批决定。
	loseView, err := w.Request(loser, "r-shared")
	if err != nil {
		t.Fatal(err)
	}
	if loseView.State != RequestPendingApproval {
		t.Fatalf("loser state = %v, want still pending (not a terminal state)", loseView.State)
	}
	wantPending := pend1
	if loser == "u2" {
		wantPending = pend2
	}
	if !loseView.CreatedAt.Equal(wantPending.CreatedAt) || !loseView.WaitDeadline.Equal(wantPending.WaitDeadline) {
		t.Fatalf("loser timing changed: created %v deadline %v, want %v / %v",
			loseView.CreatedAt, loseView.WaitDeadline, wantPending.CreatedAt, wantPending.WaitDeadline)
	}
	if !loseView.DecidedAt.IsZero() || loseView.ApproverAccountID != "" || loseView.RejectReason != "" {
		t.Fatalf("failed approve must not record a decision: %+v", loseView)
	}

	// 账本：两份待审批留痕保留；只有成功申请新增一条预留资金记录和一条
	// 零金额批准记录；失败批准不新增任何记录。
	if n := ledgerKindCount(w, LedgerPendingApproval); n != 2 {
		t.Fatalf("pending entries = %d, want 2", n)
	}
	if n := ledgerKindCount(w, LedgerReserve); n != 1 {
		t.Fatalf("reserve entries = %d, want 1", n)
	}
	if n := ledgerKindCount(w, LedgerApproval); n != 1 {
		t.Fatalf("approval entries = %d, want 1", n)
	}
	if n := len(w.Ledger()); n != 4 {
		t.Fatalf("ledger length = %d, want 4 (2 pending + 1 reserve + 1 approval)", n)
	}
	for _, e := range w.Ledger() {
		switch e.Kind {
		case LedgerReserve:
			if e.AccountID != "payer" || e.RequestID != "r-shared" || e.Amount != 60 {
				t.Fatalf("reserve entry = %+v", e)
			}
		case LedgerApproval:
			if e.AccountID != winner || e.RequestID != "r-shared" || e.Amount != 0 {
				t.Fatalf("approval entry = %+v", e)
			}
		}
	}

	// 胜出申请按实际费用 40 结算：差额 20 退回出资账户，策略只保留 40
	// 已花费额度。
	settled, err := w.Settle(winner, "r-shared", 40)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != RequestSettled || settled.ActualFee != 40 {
		t.Fatalf("settled view = %+v", settled)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 160, Reserved: 0}) {
		t.Fatalf("balance after settle = %+v, want {160 0}", bal)
	}
	if pv, _ := w.Policy("p-shared"); pv.ReservedTotal != 0 || pv.SpentTotal != 40 {
		t.Fatalf("policy totals after settle = reserved %d spent %d, want 0/40", pv.ReservedTotal, pv.SpentTotal)
	}
	if n := ledgerKindCount(w, LedgerSettle); n != 1 {
		t.Fatalf("settle entries = %d, want 1", n)
	}
	if n := ledgerKindCount(w, LedgerRefund); n != 1 {
		t.Fatalf("refund entries = %d, want 1", n)
	}
	for _, e := range w.Ledger() {
		switch e.Kind {
		case LedgerSettle:
			if e.AccountID != "payer" || e.RequestID != "r-shared" || e.Amount != 40 {
				t.Fatalf("settle entry = %+v", e)
			}
		case LedgerRefund:
			if e.AccountID != "payer" || e.RequestID != "r-shared" || e.Amount != 20 {
				t.Fatalf("refund entry = %+v", e)
			}
		}
	}

	// 等待期限内再批准另一笔：40 已花费 + 60 预留恰为 100，应允许批准。
	approved, err := w.Approve(loser, "r-shared", "sa", "dev-approve")
	if err != nil {
		t.Fatalf("approve loser after settle: %v", err)
	}
	if approved.State != RequestReserved {
		t.Fatalf("loser state after approve = %v, want reserved", approved.State)
	}
	if approved.ApproverAccountID != "payer" || approved.DecidedAt.IsZero() {
		t.Fatalf("loser decision fields missing: %+v", approved)
	}

	// 最终：出资账户可用 100、预留 60；策略已花费 40、预留 60；前一笔保持
	// 已结算、后一笔已预留。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 60}) {
		t.Fatalf("final balance = %+v, want {100 60}", bal)
	}
	if pv, _ := w.Policy("p-shared"); pv.ReservedTotal != 60 || pv.SpentTotal != 40 {
		t.Fatalf("final policy totals = reserved %d spent %d, want 60/40", pv.ReservedTotal, pv.SpentTotal)
	}
	first, err := w.Request(winner, "r-shared")
	if err != nil {
		t.Fatal(err)
	}
	if first.State != RequestSettled || first.ActualFee != 40 {
		t.Fatalf("winner view changed after second approval: %+v", first)
	}
	if n := ledgerKindCount(w, LedgerReserve); n != 2 {
		t.Fatalf("reserve entries = %d, want 2", n)
	}
	if n := ledgerKindCount(w, LedgerApproval); n != 2 {
		t.Fatalf("approval entries = %d, want 2", n)
	}
	// 2 待审批 + 2 预留 + 2 批准 + 1 扣减 + 1 退款，账本完整解释余额变化。
	if n := len(w.Ledger()); n != 8 {
		t.Fatalf("final ledger length = %d, want 8", n)
	}
}
