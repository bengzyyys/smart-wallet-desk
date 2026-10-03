package wallet

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// backupSetup 构造一个覆盖全部状态的钱包：
//   - 账户 payer/u1/u2；会话 s1、s2（使用）、sa（出资审批）、s3（吊销）、
//     s4（已到期）；
//   - p-app 开审批（等待 5 分钟，预留超时 10 小时）、p-to 开预留超时
//     （1 分钟、无审批）、p-notime 不启用任何超时、p-deact 已停用；
//   - 请求覆盖直接预留、已结算、已预留后取消、待审批、已批准预留、被拒、
//     待审批取消、待审批过期、预留超时，以及同号不同使用账户；
//   - 账本含未被受理申请（空字段、未知账户、未知会话）的拒绝记录。
//
// 所有存活请求的计时基准都在 t0；构造终态时临时拨快时钟后再拨回 t0。
func backupSetup(t *testing.T) (*Wallet, *clock) {
	t.Helper()
	w, c := newTestWallet() // t0 = 2026-01-01 12:00:00 UTC
	t0 := c.t

	mustAccount(t, w, "payer", 1000)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s2", "u2", "dev2", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "deva", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s3", "u1", "dev3", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s4", "u2", "dev4", t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	mkPolicy := func(id string, threshold int64, wait, reserve time.Duration, per, total int64) PolicySpec {
		return PolicySpec{
			ID:                 id,
			PayerAccountID:     "payer",
			AllowedAccountIDs:  []string{"u1", "u2"},
			Operation:          "charge",
			Payee:              "shop",
			StartsAt:           t0.Add(-time.Hour),
			EndsAt:             t0.Add(time.Hour),
			MaxPerRequest:      per,
			MaxTotal:           total,
			ApprovalThreshold:  threshold,
			ApprovalWait:       wait,
			MaxReserveDuration: reserve,
		}
	}
	if err := w.SavePolicy(mkPolicy("p-app", 10, 5*time.Minute, 10*time.Hour, 100, 1000)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(mkPolicy("p-to", 0, 0, time.Minute, 100, 1000)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(mkPolicy("p-notime", 0, 0, 0, 100, 1000)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(mkPolicy("p-deact", 0, 0, 0, 100, 1000)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.DeactivatePolicy("p-deact", "sa", "deva", "stop it"); err != nil {
		t.Fatal(err)
	}
	if err := w.RevokeSession("s3"); err != nil {
		t.Fatal(err)
	}

	apply := func(policy, account, sess, dev, rid string, fee int64) RequestView {
		in := RequestInput{
			PolicyID: policy, RequestID: rid, AccountID: account, SessionID: sess,
			DeviceID: dev, Operation: "charge", Payee: "shop", EstimatedFee: fee,
		}
		v, err := w.Apply(in)
		if err != nil {
			t.Fatalf("apply %s/%s: %v", account, rid, err)
		}
		return v
	}

	// 直接预留（p-app，费用 5 不超门槛）。
	apply("p-app", "u1", "s1", "dev1", "direct", 5)
	// 已结算（实际 3）。
	apply("p-app", "u1", "s1", "dev1", "settled", 5)
	if _, err := w.Settle("u1", "settled", 3); err != nil {
		t.Fatal(err)
	}
	// 已预留后取消。
	apply("p-app", "u1", "s1", "dev1", "cancel-reserved", 5)
	if _, err := w.Cancel("u1", "cancel-reserved"); err != nil {
		t.Fatal(err)
	}
	// 待审批（恢复时才到期）。
	apply("p-app", "u1", "s1", "dev1", "pending-due", 20)
	// 批准后预留。
	apply("p-app", "u2", "s2", "dev2", "approved", 20)
	if _, err := w.Approve("u2", "approved", "sa", "deva"); err != nil {
		t.Fatal(err)
	}
	// 审批拒绝。
	apply("p-app", "u2", "s2", "dev2", "rejected", 20)
	if _, err := w.Reject("u2", "rejected", "sa", "deva", "no way"); err != nil {
		t.Fatal(err)
	}
	// 待审批取消。
	apply("p-app", "u2", "s2", "dev2", "cancel-pending", 20)
	if _, err := w.Cancel("u2", "cancel-pending"); err != nil {
		t.Fatal(err)
	}
	// 待审批等待过期（终态）：临时拨快时钟触发后拨回。
	apply("p-app", "u2", "s2", "dev2", "pending-expired", 20)
	c.t = t0.Add(6 * time.Minute)
	if r, err := w.Request("u2", "pending-expired"); err != nil || r.State != RequestExpired {
		t.Fatalf("pending-expired state = %v err = %v", r.State, err)
	}
	c.t = t0
	// 预留超时（终态，释放时刻必须是原截止时刻 t0+1m）：拨快触发后拨回。
	apply("p-to", "u1", "s1", "dev1", "resv-expired", 5)
	c.t = t0.Add(2 * time.Minute)
	if r, err := w.Request("u1", "resv-expired"); err != nil || r.State != RequestReservationExpired {
		t.Fatalf("resv-expired state = %v err = %v", r.State, err)
	}
	c.t = t0
	// 恢复时才到期的预留（截止 t0+1m，导出时仍存活）。
	apply("p-to", "u1", "s1", "dev1", "resv-due", 5)
	// 关闭预留超时：很久以后仍等待结算或取消。
	apply("p-notime", "u1", "s1", "dev1", "no-timeout", 5)
	// 同号不同使用账户：分别识别。
	apply("p-notime", "u1", "s1", "dev1", "same-id", 5)
	apply("p-notime", "u2", "s2", "dev2", "same-id", 5)

	// 未被受理申请的拒绝记录：空字段、未知账户、未知会话各一条。
	if _, err := w.Apply(RequestInput{EstimatedFee: -1}); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("empty/invalid apply err = %v", err)
	}
	if _, err := w.Apply(RequestInput{
		PolicyID: "p-app", RequestID: "ghost-rid", AccountID: "ghost",
		SessionID: "s1", DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 5,
	}); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("ghost account apply err = %v", err)
	}
	if _, err := w.Apply(RequestInput{
		PolicyID: "p-app", RequestID: "no-session", AccountID: "u1",
		SessionID: "missing", DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 5,
	}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("missing session apply err = %v", err)
	}

	return w, c
}

// exportAndRestoreAt 在指定时刻导出（导出时钟由 c 控制）并用指定恢复时刻
// 恢复成新钱包。
func exportAndRestoreAt(t *testing.T, w *Wallet, restoreTime time.Time) *Wallet {
	t.Helper()
	data, err := w.Export()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	w2, err := restoreAt(data, restoreTime)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	return w2
}

func TestBackupVersionField(t *testing.T) {
	w, _ := newTestWallet()
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		t.Fatal(err)
	}
	if head.Version != backupVersion {
		t.Fatalf("version = %d, want %d", head.Version, backupVersion)
	}
}

func TestEmptyWalletExportRestore(t *testing.T) {
	w, _ := newTestWallet()
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty wallet export is empty")
	}
	w2, err := restoreAt(data, w.now())
	if err != nil {
		t.Fatalf("restore empty: %v", err)
	}
	if got := w2.Ledger(); len(got) != 0 {
		t.Fatalf("restored empty wallet ledger = %d entries", len(got))
	}
	// 导出不新增账本记录；恢复后可正常使用。
	if _, err := w2.CreateAccount("a", 10); err != nil {
		t.Fatal(err)
	}
	if bal, _ := w2.Balance("a"); bal != (Balances{Available: 10, Reserved: 0}) {
		t.Fatalf("balance = %+v", bal)
	}
}

func TestExportAddsNoLedgerEntryAndIsDeterministic(t *testing.T) {
	w, _ := backupSetup(t)
	before := len(w.Ledger())
	d1, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(w.Ledger()); got != before {
		t.Fatalf("export added ledger entries: %d -> %d", before, got)
	}
	// 同一时刻再次导出：逐字节一致（无到期项需要处理）。
	d2, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	if string(d1) != string(d2) {
		t.Fatal("repeated export at same instant is not deterministic")
	}
}

func TestRoundTripPreservesEverything(t *testing.T) {
	w, c := backupSetup(t)
	t0 := c.t
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, t0)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	for _, id := range []string{"payer", "u1", "u2"} {
		want, _ := w.Account(id)
		got, err := w2.Account(id)
		if err != nil {
			t.Fatalf("account %s: %v", id, err)
		}
		if got != want {
			t.Fatalf("account %s = %+v, want %+v", id, got, want)
		}
	}
	for id, wantAcct := range map[string]string{
		"s1": "u1", "s2": "u2", "sa": "payer", "s3": "u1", "s4": "u2",
	} {
		g, err := w2.Session(id)
		if err != nil {
			t.Fatalf("session %s: %v", id, err)
		}
		if g.AccountID != wantAcct {
			t.Fatalf("session %s account = %q, want %q", id, g.AccountID, wantAcct)
		}
	}
	if s3, _ := w2.Session("s3"); s3.State != SessionRevoked {
		t.Fatalf("s3 state = %v, want revoked", s3.State)
	}
	for _, id := range []string{"p-app", "p-to", "p-notime", "p-deact"} {
		want, err := w.Policy(id)
		if err != nil {
			t.Fatal(err)
		}
		got, err := w2.Policy(id)
		if err != nil {
			t.Fatalf("policy %s: %v", id, err)
		}
		if !policyViewsEqual(got, want) {
			t.Fatalf("policy %s = %+v, want %+v", id, got, want)
		}
	}
	pd, _ := w2.Policy("p-deact")
	if !pd.Deactivated || pd.DeactivateReason != "stop it" || pd.DeactivatorAccountID != "payer" {
		t.Fatalf("deactivation info lost: %+v", pd)
	}

	wantStates := map[requestKey]RequestState{
		{accountID: "u1", requestID: "direct"}:          RequestReserved,
		{accountID: "u1", requestID: "settled"}:         RequestSettled,
		{accountID: "u1", requestID: "cancel-reserved"}: RequestCancelled,
		{accountID: "u1", requestID: "pending-due"}:     RequestPendingApproval,
		{accountID: "u2", requestID: "approved"}:        RequestReserved,
		{accountID: "u2", requestID: "rejected"}:        RequestRejected,
		{accountID: "u2", requestID: "cancel-pending"}:  RequestCancelled,
		{accountID: "u2", requestID: "pending-expired"}: RequestExpired,
		{accountID: "u1", requestID: "resv-expired"}:    RequestReservationExpired,
		{accountID: "u1", requestID: "resv-due"}:        RequestReserved,
		{accountID: "u1", requestID: "no-timeout"}:      RequestReserved,
		{accountID: "u1", requestID: "same-id"}:         RequestReserved,
		{accountID: "u2", requestID: "same-id"}:         RequestReserved,
	}
	for k, wantState := range wantStates {
		got, err := w2.Request(k.accountID, k.requestID)
		if err != nil {
			t.Fatalf("request %v: %v", k, err)
		}
		if got.State != wantState {
			t.Fatalf("request %v state = %v, want %v", k, got.State, wantState)
		}
		want, _ := w.Request(k.accountID, k.requestID)
		if !requestViewsEqual(got, want) {
			t.Fatalf("request %v view = %+v, want %+v", k, got, want)
		}
	}

	// 账本严格按原顺序、逐条一致（含未被受理申请的拒绝记录）。
	wantLedger := w.Ledger()
	gotLedger := w2.Ledger()
	if len(gotLedger) != len(wantLedger) {
		t.Fatalf("ledger len = %d, want %d", len(gotLedger), len(wantLedger))
	}
	for i := range wantLedger {
		if gotLedger[i] != wantLedger[i] {
			t.Fatalf("ledger[%d] = %+v, want %+v", i, gotLedger[i], wantLedger[i])
		}
	}
}

func policyViewsEqual(a, b PolicyView) bool {
	// AllowedAccountIDs 由 map 生成，顺序不稳定；按集合比较。
	aAllowed := append([]string(nil), a.AllowedAccountIDs...)
	bAllowed := append([]string(nil), b.AllowedAccountIDs...)
	sort.Strings(aAllowed)
	sort.Strings(bAllowed)
	if len(aAllowed) != len(bAllowed) {
		return false
	}
	for i := range aAllowed {
		if aAllowed[i] != bAllowed[i] {
			return false
		}
	}
	a.AllowedAccountIDs, b.AllowedAccountIDs = nil, nil
	// PolicySpec 含切片字段，不能直接用 != 比较；逐字段核对。
	if a.ID != b.ID || a.PayerAccountID != b.PayerAccountID ||
		a.Operation != b.Operation || a.Payee != b.Payee ||
		!a.StartsAt.Equal(b.StartsAt) || !a.EndsAt.Equal(b.EndsAt) ||
		a.MaxPerRequest != b.MaxPerRequest || a.MaxTotal != b.MaxTotal ||
		a.ApprovalThreshold != b.ApprovalThreshold || a.ApprovalWait != b.ApprovalWait ||
		a.MaxReserveDuration != b.MaxReserveDuration {
		return false
	}
	return a.ReservedTotal == b.ReservedTotal && a.SpentTotal == b.SpentTotal &&
		a.Deactivated == b.Deactivated && a.DeactivatedAt.Equal(b.DeactivatedAt) &&
		a.DeactivatorAccountID == b.DeactivatorAccountID && a.DeactivateReason == b.DeactivateReason
}

func requestViewsEqual(a, b RequestView) bool {
	return a.PolicyID == b.PolicyID && a.RequestID == b.RequestID && a.AccountID == b.AccountID &&
		a.PayerAccountID == b.PayerAccountID && a.Operation == b.Operation && a.Payee == b.Payee &&
		a.EstimatedFee == b.EstimatedFee && a.ActualFee == b.ActualFee && a.State == b.State &&
		a.CreatedAt.Equal(b.CreatedAt) && a.SettledAt.Equal(b.SettledAt) &&
		a.WaitDeadline.Equal(b.WaitDeadline) && a.ReservedAt.Equal(b.ReservedAt) &&
		a.ReserveDuration == b.ReserveDuration && a.ReserveDeadline.Equal(b.ReserveDeadline) &&
		a.ReserveExpiredAt.Equal(b.ReserveExpiredAt) && a.DecidedAt.Equal(b.DecidedAt) &&
		a.ApproverAccountID == b.ApproverAccountID && a.RejectReason == b.RejectReason
}

func TestRestoreProcessesDueAtRestoreTime(t *testing.T) {
	w, c := backupSetup(t)
	t0 := c.t
	// 导出时刻 t0：pending-due（截止 t0+5m）与 resv-due（截止 t0+1m）均未到期。
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	srcBal, _ := w.Balance("payer")

	// 恢复时刻 t0+30m：待审批已过期；预留已超时全额退回，释放时间仍是 t0+1m。
	w2, err := restoreAt(data, t0.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	pd, _ := w2.Request("u1", "pending-due")
	if pd.State != RequestExpired {
		t.Fatalf("pending-due state = %v, want expired", pd.State)
	}
	if pd.DecidedAt.IsZero() {
		t.Fatal("expired request must record decided_at")
	}
	rd, _ := w2.Request("u1", "resv-due")
	if rd.State != RequestReservationExpired {
		t.Fatalf("resv-due state = %v, want reservation expired", rd.State)
	}
	if want := t0.Add(time.Minute); !rd.ReserveExpiredAt.Equal(want) {
		t.Fatalf("release time = %v, want original deadline %v (not restore time)", rd.ReserveExpiredAt, want)
	}
	// 退回的 5 立即可用；预留余额相应下降。
	gotBal, _ := w2.Balance("payer")
	if gotBal.Available != srcBal.Available+5 || gotBal.Reserved != srcBal.Reserved-5 {
		t.Fatalf("balance after restore = %+v, want available %d reserved %d", gotBal, srcBal.Available+5, srcBal.Reserved-5)
	}
	// 账本：在原账本之后追加 预留超时退款 + 超时记录 + 待审批过期记录。
	var kinds []LedgerKind
	for _, e := range w2.Ledger() {
		switch {
		case e.Kind == LedgerRefund && e.RequestID == "resv-due":
			kinds = append(kinds, LedgerRefund)
			if e.Amount != 5 || !e.At.Equal(t0.Add(time.Minute)) {
				t.Fatalf("resv-due refund entry = %+v", e)
			}
			if e.AccountID != "payer" {
				t.Fatalf("refund account = %q, want payer", e.AccountID)
			}
		case e.Kind == LedgerReservationExpiration && e.RequestID == "resv-due":
			kinds = append(kinds, LedgerReservationExpiration)
			if !e.At.Equal(t0.Add(time.Minute)) {
				t.Fatalf("timeout entry at %v, want deadline", e.At)
			}
		case e.Kind == LedgerExpiration && e.RequestID == "pending-due":
			kinds = append(kinds, LedgerExpiration)
		}
	}
	if len(kinds) != 3 {
		t.Fatalf("expected 3 new entries, got %v", kinds)
	}

	// 未到期（相对 t0+30m）的请求继续等待或预留。
	if d, _ := w2.Request("u1", "direct"); d.State != RequestReserved {
		t.Fatalf("direct state = %v, want still reserved", d.State)
	}
	if a, _ := w2.Request("u2", "approved"); a.State != RequestReserved {
		t.Fatalf("approved state = %v, want still reserved", a.State)
	}
	// 既有终态不改写。
	re, _ := w2.Request("u1", "resv-expired")
	if !re.ReserveExpiredAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("pre-existing timeout release time rewritten: %v", re.ReserveExpiredAt)
	}
}

func TestExportProcessesDueAtExportTime(t *testing.T) {
	w, c := backupSetup(t)
	t0 := c.t
	// 导出时刻拨到 t0+30m：导出前先处理到期请求。
	c.t = t0.Add(30 * time.Minute)
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	// 原钱包也已在导出时完成到期处理。
	rd, _ := w.Request("u1", "resv-due")
	if rd.State != RequestReservationExpired || !rd.ReserveExpiredAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("source resv-due = %+v", rd)
	}
	pd, _ := w.Request("u1", "pending-due")
	if pd.State != RequestExpired {
		t.Fatalf("source pending-due state = %v", pd.State)
	}
	// 在同一时刻恢复：不得重复记账、不得重复退款。
	w2, err := restoreAt(data, c.t)
	if err != nil {
		t.Fatal(err)
	}
	if l1, l2 := len(w.Ledger()), len(w2.Ledger()); l1 != l2 {
		t.Fatalf("ledger length drift on same-time restore: source %d restored %d", l1, l2)
	}
	b1, _ := w.Balance("payer")
	b2, _ := w2.Balance("payer")
	if b1 != b2 {
		t.Fatalf("balance drift: source %+v restored %+v", b1, b2)
	}
}

func TestRestoreDoesNotRetimeDeadlines(t *testing.T) {
	w, c := backupSetup(t)
	t0 := c.t
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	// 即使恢复时刻远晚于导出，截止时刻仍是绝对时刻，不随恢复时刻平移。
	w2, err := restoreAt(data, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	rd, _ := w2.Request("u1", "resv-due")
	if !rd.ReserveDeadline.Equal(t0.Add(time.Minute)) {
		t.Fatalf("reserve deadline shifted to %v", rd.ReserveDeadline)
	}
	ap, _ := w2.Request("u2", "approved")
	if !ap.ReserveDeadline.Equal(t0.Add(10*time.Hour)) || ap.ReservedAt != t0 {
		t.Fatalf("approved timing changed: %+v", ap)
	}
}

func TestRestoredSessionsAndPoliciesDoNotRegainAuthorization(t *testing.T) {
	w, c := backupSetup(t)
	t0 := c.t
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, t0.Add(2*time.Hour)) // 会话、策略窗口均已结束
	if err != nil {
		t.Fatal(err)
	}
	in := RequestInput{
		PolicyID: "p-notime", RequestID: "new-1", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 5,
	}
	if _, err := w2.Apply(in); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expired session apply err = %v, want ErrSessionExpired", err)
	}
	// 吊销会话仍拒绝。
	in2 := RequestInput{
		PolicyID: "p-notime", RequestID: "new-2", AccountID: "u1", SessionID: "s3",
		DeviceID: "dev3", Operation: "charge", Payee: "shop", EstimatedFee: 5,
	}
	if _, err := w2.Apply(in2); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("revoked session apply err = %v, want ErrSessionRevoked", err)
	}
	// 已停用策略不恢复授权（用尚未到期的会话也不行——这里直接在窗口内验证）。
	w3, err := restoreAt(data, t0.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	in3 := RequestInput{
		PolicyID: "p-deact", RequestID: "new-3", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 5,
	}
	if _, err := w3.Apply(in3); !errors.Is(err, ErrPolicyDeactivated) {
		t.Fatalf("deactivated policy apply err = %v, want ErrPolicyDeactivated", err)
	}
}

func TestRefundedMoneyReusableAndLateSettleBlocked(t *testing.T) {
	w, c := backupSetup(t)
	t0 := c.t
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	// t0+30m：resv-due 已超时退回，但仍在会话/策略窗口内。
	w2, err := restoreAt(data, t0.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// 迟到结算不能扣款。
	balBefore, _ := w2.Balance("payer")
	if _, err := w2.Settle("u1", "resv-due", 5); !errors.Is(err, ErrReservationExpired) {
		t.Fatalf("late settle err = %v, want ErrReservationExpired", err)
	}
	if bal, _ := w2.Balance("payer"); bal != balBefore {
		t.Fatalf("late settle moved money: %+v -> %+v", balBefore, bal)
	}
	// 取消对超时终态幂等，不重复退回。
	if r, err := w2.Cancel("u1", "resv-due"); err != nil || r.State != RequestReservationExpired {
		t.Fatalf("cancel after timeout: %+v %v", r, err)
	}
	if bal, _ := w2.Balance("payer"); bal != balBefore {
		t.Fatalf("idempotent cancel moved money: %+v", bal)
	}
	// 退回的余额可立即用于新申请。
	in := RequestInput{
		PolicyID: "p-notime", RequestID: "fresh", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 5,
	}
	if _, err := w2.Apply(in); err != nil {
		t.Fatalf("new apply with refunded balance: %v", err)
	}
}

func TestNoTimeoutRequestStillWaitsAfterRestore(t *testing.T) {
	w, c := backupSetup(t)
	t0 := c.t
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// 关闭预留超时的请求：很久以后仍可正常结算与取消。
	r, err := w2.Request("u1", "no-timeout")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != RequestReserved {
		t.Fatalf("no-timeout state = %v, want reserved", r.State)
	}
	if _, err := w2.Settle("u1", "no-timeout", 4); err != nil {
		t.Fatalf("settle long after restore: %v", err)
	}
}

func TestIdempotencyRulesSurviveRestore(t *testing.T) {
	w, c := backupSetup(t)
	t0 := c.t
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, t0)
	if err != nil {
		t.Fatal(err)
	}

	// 同号同内容重复申请：返回已有结果，不重复预留、不记账。
	same := RequestInput{
		PolicyID: "p-app", RequestID: "direct", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 5,
	}
	ledgerBefore := len(w2.Ledger())
	balBefore, _ := w2.Balance("payer")
	v, err := w2.Apply(same)
	if err != nil || v.State != RequestReserved {
		t.Fatalf("idempotent re-apply: %+v %v", v, err)
	}
	if got := len(w2.Ledger()); got != ledgerBefore {
		t.Fatalf("idempotent re-apply appended ledger: %d -> %d", ledgerBefore, got)
	}
	if bal, _ := w2.Balance("payer"); bal != balBefore {
		t.Fatalf("idempotent re-apply moved money: %+v", bal)
	}
	// 同号不同内容：冲突。
	diff := same
	diff.EstimatedFee = 4
	if _, err := w2.Apply(diff); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting re-apply err = %v, want ErrConflict", err)
	}
	// 不同使用账户同号：各自识别，互不冲突。
	other := same
	other.AccountID, other.SessionID, other.DeviceID, other.EstimatedFee = "u2", "s2", "dev2", 5
	other.RequestID = "direct"
	other.PolicyID = "p-notime"
	if _, err := w2.Apply(other); err != nil {
		t.Fatalf("same request id under different account: %v", err)
	}

	// 重复结算幂等、不同金额冲突；重复取消幂等。
	if v, err := w2.Settle("u1", "settled", 3); err != nil || v.State != RequestSettled {
		t.Fatalf("repeat settle: %+v %v", v, err)
	}
	if _, err := w2.Settle("u1", "settled", 2); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting settle err = %v", err)
	}
	if v, err := w2.Cancel("u1", "cancel-reserved"); err != nil || v.State != RequestCancelled {
		t.Fatalf("repeat cancel: %+v %v", v, err)
	}
}

func TestRestoredWalletsAreIndependent(t *testing.T) {
	w, c := backupSetup(t)
	t0 := c.t
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, t0)
	if err != nil {
		t.Fatal(err)
	}
	w3, err := restoreAt(data, t0)
	if err != nil {
		t.Fatal(err)
	}

	// 在 w2 结算 no-timeout（实际 1），w3、原钱包不受影响。
	if _, err := w2.Settle("u1", "no-timeout", 1); err != nil {
		t.Fatal(err)
	}
	for name, x := range map[string]*Wallet{"source": w, "w3": w3} {
		r, err := x.Request("u1", "no-timeout")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestReserved || r.ActualFee != 0 {
			t.Fatalf("%s mutated by w2 settle: %+v", name, r)
		}
	}
	// 原钱包继续操作不影响恢复出的钱包。
	if _, err := w.Cancel("u1", "direct"); err != nil {
		t.Fatal(err)
	}
	r, err := w2.Request("u1", "direct")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != RequestReserved {
		t.Fatalf("w2 affected by source cancel: %+v", r)
	}
}

func TestOrphanRejectionRecordsRestored(t *testing.T) {
	w, c := backupSetup(t)
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, c.t)
	if err != nil {
		t.Fatalf("restore with orphan rejection records: %v", err)
	}
	var sawEmpty, sawGhost, sawMissingSession bool
	for _, e := range w2.Ledger() {
		if e.Kind != LedgerRejection {
			continue
		}
		switch {
		case e.AccountID == "" && e.RequestID == "":
			sawEmpty = true
		case e.AccountID == "ghost" && e.RequestID == "ghost-rid":
			sawGhost = true
		case e.AccountID == "u1" && e.RequestID == "no-session":
			sawMissingSession = true
		}
	}
	if !sawEmpty || !sawGhost || !sawMissingSession {
		t.Fatalf("orphan rejections lost: empty=%v ghost=%v missing-session=%v", sawEmpty, sawGhost, sawMissingSession)
	}
}

// ---- 损坏备份拒绝 ----

func mustMap(t *testing.T, data []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func mustRemap(t *testing.T, m map[string]interface{}) []byte {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// corruptCases 基于完整备份做单点损坏，每种都必须被拒绝。
func TestRestoreRejectsCorruptBackups(t *testing.T) {
	w, _ := backupSetup(t)
	good, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}

	type tc struct {
		name   string
		mutate func(m map[string]interface{})
	}
	cases := []tc{
		{"unsupported version", func(m map[string]interface{}) { m["version"] = 2 }},
		{"missing version", func(m map[string]interface{}) { delete(m, "version") }},
		{"missing accounts", func(m map[string]interface{}) { m["accounts"] = nil }},
		{"missing sessions", func(m map[string]interface{}) { m["sessions"] = nil }},
		{"missing policies", func(m map[string]interface{}) { m["policies"] = nil }},
		{"missing requests", func(m map[string]interface{}) { m["requests"] = nil }},
		{"missing ledger", func(m map[string]interface{}) { m["ledger"] = nil }},
		{"duplicate account", func(m map[string]interface{}) {
			a := m["accounts"].([]interface{})
			a = append(a, a[0])
			m["accounts"] = a
		}},
		{"duplicate session", func(m map[string]interface{}) {
			s := m["sessions"].([]interface{})
			s = append(s, s[0])
			m["sessions"] = s
		}},
		{"duplicate policy", func(m map[string]interface{}) {
			p := m["policies"].([]interface{})
			p = append(p, p[0])
			m["policies"] = p
		}},
		{"duplicate request under same account", func(m map[string]interface{}) {
			r := m["requests"].([]interface{})
			r = append(r, r[0])
			m["requests"] = r
		}},
		{"unknown request state", func(m map[string]interface{}) {
			r := m["requests"].([]interface{})
			r[0].(map[string]interface{})["state"] = 99
		}},
		{"unknown ledger kind", func(m map[string]interface{}) {
			l := m["ledger"].([]interface{})
			l[0].(map[string]interface{})["kind"] = 999
		}},
		{"negative account available", func(m map[string]interface{}) {
			a := m["accounts"].([]interface{})
			a[0].(map[string]interface{})["available"] = -1
		}},
		{"negative account reserved", func(m map[string]interface{}) {
			a := m["accounts"].([]interface{})
			a[0].(map[string]interface{})["reserved"] = -1
		}},
		{"negative ledger amount", func(m map[string]interface{}) {
			l := m["ledger"].([]interface{})
			l[0].(map[string]interface{})["amount"] = -1
		}},
		{"non-positive per-request limit", func(m map[string]interface{}) {
			for _, p := range m["policies"].([]interface{}) {
				pm := p.(map[string]interface{})
				if pm["id"] == "p-app" {
					pm["max_per_request"] = 0
				}
			}
		}},
		{"policy window reversed", func(m map[string]interface{}) {
			for _, p := range m["policies"].([]interface{}) {
				pm := p.(map[string]interface{})
				if pm["id"] == "p-app" {
					pm["ends_at"] = pm["starts_at"]
				}
			}
		}},
		{"session references unknown account", func(m map[string]interface{}) {
			m["sessions"].([]interface{})[0].(map[string]interface{})["account_id"] = "nobody"
		}},
		{"request references unknown session", func(m map[string]interface{}) {
			m["requests"].([]interface{})[0].(map[string]interface{})["session_id"] = "nobody"
		}},
		{"request references unknown policy", func(m map[string]interface{}) {
			m["requests"].([]interface{})[0].(map[string]interface{})["policy_id"] = "nobody"
		}},
		{"request references unknown usage account", func(m map[string]interface{}) {
			m["requests"].([]interface{})[0].(map[string]interface{})["account_id"] = "nobody"
		}},
		{"request payer mismatches policy", func(m map[string]interface{}) {
			m["requests"].([]interface{})[0].(map[string]interface{})["payer_account_id"] = "u1"
		}},
		{"application session belongs to another account", func(m map[string]interface{}) {
			// 找一条 u1 的请求，把它的会话改成属于 u2 的 s2。
			for _, r := range m["requests"].([]interface{}) {
				rm := r.(map[string]interface{})
				if rm["account_id"] == "u1" {
					rm["session_id"] = "s2"
					break
				}
			}
		}},
		{"usage account not allowed by policy", func(m map[string]interface{}) {
			// payer 账户存在、会话 sa 属于 payer，但不在任何策略的 allowed
			// 列表中；把一条请求改成 payer 发起且使用 sa，必须被拒绝。
			for _, r := range m["requests"].([]interface{}) {
				rm := r.(map[string]interface{})
				if rm["request_id"] == "direct" {
					rm["account_id"] = "payer"
					rm["session_id"] = "sa"
					break
				}
			}
		}},
		{"duplicate allowed account in policy", func(m map[string]interface{}) {
			for _, p := range m["policies"].([]interface{}) {
				pm := p.(map[string]interface{})
				if pm["id"] == "p-app" {
					allowed := pm["allowed_account_ids"].([]interface{})
					pm["allowed_account_ids"] = append(allowed, allowed[0])
				}
			}
		}},
		{"account reserved mismatch", func(m map[string]interface{}) {
			for _, a := range m["accounts"].([]interface{}) {
				am := a.(map[string]interface{})
				if am["id"] == "payer" {
					am["reserved"] = int64(am["reserved"].(float64)) + 1
				}
			}
		}},
		{"policy reserved total mismatch", func(m map[string]interface{}) {
			for _, p := range m["policies"].([]interface{}) {
				pm := p.(map[string]interface{})
				if pm["id"] == "p-app" {
					pm["reserved_total"] = int64(pm["reserved_total"].(float64)) + 1
				}
			}
		}},
		{"policy spent total mismatch", func(m map[string]interface{}) {
			for _, p := range m["policies"].([]interface{}) {
				pm := p.(map[string]interface{})
				if pm["id"] == "p-app" {
					pm["spent_total"] = int64(pm["spent_total"].(float64)) + 1
				}
			}
		}},
		{"reserved request missing reserved_at", func(m map[string]interface{}) {
			for _, r := range m["requests"].([]interface{}) {
				rm := r.(map[string]interface{})
				if rm["request_id"] == "direct" {
					rm["reserved_at"] = ""
				}
			}
		}},
		{"reserve deadline inconsistent", func(m map[string]interface{}) {
			for _, r := range m["requests"].([]interface{}) {
				rm := r.(map[string]interface{})
				if rm["request_id"] == "resv-due" {
					rm["reserve_deadline"] = "2026-01-01T12:00:30Z"
				}
			}
		}},
		{"deactivated policy missing reason", func(m map[string]interface{}) {
			for _, p := range m["policies"].([]interface{}) {
				pm := p.(map[string]interface{})
				if pm["id"] == "p-deact" {
					pm["deactivate_reason"] = ""
				}
			}
		}},
		{"ledger references unknown policy", func(m map[string]interface{}) {
			l := m["ledger"].([]interface{})
			l[0].(map[string]interface{})["policy_id"] = "p-ghost"
		}},
		{"non-positive estimated fee", func(m map[string]interface{}) {
			m["requests"].([]interface{})[0].(map[string]interface{})["estimated_fee"] = 0
		}},
		{"actual fee exceeds estimated", func(m map[string]interface{}) {
			for _, r := range m["requests"].([]interface{}) {
				rm := r.(map[string]interface{})
				if rm["request_id"] == "settled" {
					rm["actual_fee"] = 99
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := mustMap(t, good)
			tc.mutate(m)
			w2, err := Restore(mustRemap(t, m))
			if err == nil {
				if w2 != nil {
					t.Fatalf("corrupt backup %q restored a wallet", tc.name)
				}
				t.Fatalf("corrupt backup %q restored without error", tc.name)
			}
			if !errors.Is(err, ErrBackupInvalid) {
				t.Fatalf("corrupt backup %q err = %v, want ErrBackupInvalid", tc.name, err)
			}
		})
	}

	// 文本级损坏。
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"empty", []byte("")},
		{"whitespace", []byte("   \n\t ")},
		{"malformed", []byte("{not json")},
		{"not an object", []byte(`[1,2,3]`)},
		{"trailing value", append(append([]byte{}, good...), []byte(" {}")...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w2, err := Restore(tc.data)
			if err == nil || w2 != nil {
				t.Fatalf("corrupt text %q: w2=%v err=%v", tc.name, w2, err)
			}
			if !errors.Is(err, ErrBackupInvalid) {
				t.Fatalf("corrupt text %q err = %v", tc.name, err)
			}
		})
	}
}

func TestRestoreRejectsInt64Overflow(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: 0, Reserved: math.MaxInt64, CreatedAt: timeJSON(t0)},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)},
		},
		Sessions: []sessionBackupV1{
			{ID: "s1", AccountID: "u1", DeviceID: "d1", ExpiresAt: timeJSON(t0.Add(time.Hour)), CreatedAt: timeJSON(t0)},
		},
		Policies: []policyBackupV1{{
			ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
			Operation: "charge", Payee: "shop",
			StartsAt: timeJSON(t0.Add(-time.Hour)), EndsAt: timeJSON(t0.Add(time.Hour)),
			MaxPerRequest: math.MaxInt64, MaxTotal: math.MaxInt64,
			ReservedTotal: math.MaxInt64,
		}},
		Requests: []requestBackupV1{
			{
				PolicyID: "p", RequestID: "r1", AccountID: "u1", PayerAccountID: "payer",
				SessionID: "s1", Operation: "charge", Payee: "shop",
				EstimatedFee: math.MaxInt64, State: int(RequestReserved),
				CreatedAt: timeJSON(t0), ReservedAt: timeJSON(t0),
			},
			{
				PolicyID: "p", RequestID: "r2", AccountID: "u1", PayerAccountID: "payer",
				SessionID: "s1", Operation: "charge", Payee: "shop",
				EstimatedFee: math.MaxInt64, State: int(RequestReserved),
				CreatedAt: timeJSON(t0), ReservedAt: timeJSON(t0),
			},
		},
		Ledger: []ledgerEntryBackupV1{},
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(data); err == nil {
		t.Fatal("overflow backup restored")
	} else if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("overflow err = %v, want ErrBackupInvalid", err)
	}
}

// buildRefundOverflowBackup 构造一个自洽备份：出资账户 payer 的可用余额为
// available，fees 为“策略编号 -> 一笔仍处于已预留状态的请求费用”映射
// （每策略一笔，便于验证跨策略合计）。reserveDur 为这些预留的最长预留
// 时长（零表示关闭超时）；账户/策略的预留总额按 fees 自洽填写。
func buildRefundOverflowBackup(t *testing.T, t0 time.Time, available int64, reserveDur time.Duration, fees map[string]int64) []byte {
	t.Helper()
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: available, Reserved: 0, CreatedAt: timeJSON(t0)},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)},
		},
		Sessions: []sessionBackupV1{
			{ID: "s1", AccountID: "u1", DeviceID: "d1", ExpiresAt: timeJSON(t0.Add(time.Hour)), CreatedAt: timeJSON(t0)},
		},
		Policies: make([]policyBackupV1, 0, len(fees)),
		Requests: make([]requestBackupV1, 0, len(fees)),
		Ledger:   []ledgerEntryBackupV1{},
	}
	var reservedTotal int64
	pids := make([]string, 0, len(fees))
	for pid := range fees {
		pids = append(pids, pid)
	}
	sort.Strings(pids)
	for _, pid := range pids {
		fee := fees[pid]
		b.Policies = append(b.Policies, policyBackupV1{
			ID: pid, PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
			Operation: "charge", Payee: "shop",
			StartsAt: timeJSON(t0.Add(-time.Hour)), EndsAt: timeJSON(t0.Add(time.Hour)),
			MaxPerRequest:      math.MaxInt64,
			MaxTotal:           math.MaxInt64,
			MaxReserveDuration: durationJSON(reserveDur),
			ReservedTotal:      fee,
		})
		r := requestBackupV1{
			PolicyID: pid, RequestID: "req-" + pid, AccountID: "u1", PayerAccountID: "payer",
			SessionID: "s1", Operation: "charge", Payee: "shop",
			EstimatedFee:    fee,
			State:           int(RequestReserved),
			CreatedAt:       timeJSON(t0),
			ReservedAt:      timeJSON(t0),
			ReserveDuration: durationJSON(reserveDur),
		}
		if reserveDur > 0 {
			r.ReserveDeadline = timeJSON(t0.Add(reserveDur))
		}
		b.Requests = append(b.Requests, r)
		sum, ok := addInt64(reservedTotal, fee)
		if !ok {
			t.Fatalf("test setup reserved total overflow")
		}
		reservedTotal = sum
	}
	for i := range b.Accounts {
		if b.Accounts[i].ID == "payer" {
			b.Accounts[i].Reserved = reservedTotal
		}
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestRestoreRejectsAvailablePlusReservedOverflow 验证可用余额与现存预留
// 合计越过 int64 上限的备份必须被拒绝——即使各请求与策略金额分别合法。
func TestRestoreRejectsAvailablePlusReservedOverflow(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		fees       map[string]int64
		available  int64
		reserveDur time.Duration
		restoreAt  time.Time
	}{
		{
			name:      "max available plus one reserved, not yet due",
			fees:      map[string]int64{"p": 1},
			available: math.MaxInt64,
			restoreAt: t0,
		},
		{
			name:       "max available plus one reserved, due at restore time",
			fees:       map[string]int64{"p": 1},
			available:  math.MaxInt64,
			reserveDur: time.Minute,
			restoreAt:  t0.Add(30 * time.Minute),
		},
		{
			name:      "cross-policy reserved sum pushes total over max",
			fees:      map[string]int64{"p1": 1, "p2": 1},
			available: math.MaxInt64 - 1,
			restoreAt: t0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := buildRefundOverflowBackup(t, t0, tc.available, tc.reserveDur, tc.fees)
			w2, err := restoreAt(data, tc.restoreAt)
			if err == nil {
				if w2 != nil {
					t.Fatalf("overflow backup restored a wallet")
				}
				t.Fatalf("overflow backup restored without error")
			}
			if !errors.Is(err, ErrBackupInvalid) {
				t.Fatalf("err = %v, want ErrBackupInvalid", err)
			}
			if msg := err.Error(); !strings.Contains(msg, "payer") || !strings.Contains(msg, "overflows int64") {
				t.Fatalf("error %q must name account and the overflow", msg)
			}
		})
	}
}

// TestRestoreAcceptsAvailablePlusReservedAtBoundary 验证合计恰好等于上限
// 仍然合法：取消或超时退回后可用余额恰好为 MaxInt64；预留为零而可用
// 余额等于上限也能恢复。
func TestRestoreAcceptsAvailablePlusReservedAtBoundary(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// 可用 = 上限-1，预留 1（未启用超时）：恢复后取消得到上限。
	data := buildRefundOverflowBackup(t, t0, math.MaxInt64-1, 0, map[string]int64{"p": 1})
	w2, err := restoreAt(data, t0)
	if err != nil {
		t.Fatalf("boundary restore: %v", err)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: math.MaxInt64 - 1, Reserved: 1}) {
		t.Fatalf("boundary balance = %+v", bal)
	}
	if _, err := w2.Cancel("u1", "req-p"); err != nil {
		t.Fatalf("cancel boundary reservation: %v", err)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: math.MaxInt64, Reserved: 0}) {
		t.Fatalf("balance after cancel = %+v, want available MaxInt64", bal)
	}

	// 跨两笔策略的预留合计恰为 2：可用 = 上限-2，逐笔取消后得到上限。
	data = buildRefundOverflowBackup(t, t0, math.MaxInt64-2, 0, map[string]int64{"p1": 1, "p2": 1})
	w2, err = restoreAt(data, t0)
	if err != nil {
		t.Fatalf("cross-policy boundary restore: %v", err)
	}
	for _, rid := range []string{"req-p1", "req-p2"} {
		if _, err := w2.Cancel("u1", rid); err != nil {
			t.Fatalf("cancel %s: %v", rid, err)
		}
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: math.MaxInt64, Reserved: 0}) {
		t.Fatalf("balance after cancelling both = %+v, want available MaxInt64", bal)
	}

	// 恢复时预留恰好超时：自动退回 1 后可用余额恰好为上限，不留负数。
	data = buildRefundOverflowBackup(t, t0, math.MaxInt64-1, time.Minute, map[string]int64{"p": 1})
	w2, err = restoreAt(data, t0.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("boundary timeout restore: %v", err)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: math.MaxInt64, Reserved: 0}) {
		t.Fatalf("balance after timeout restore = %+v, want available MaxInt64", bal)
	}
	r, err := w2.Request("u1", "req-p")
	if err != nil || r.State != RequestReservationExpired {
		t.Fatalf("request state = %v err = %v, want reservation expired", r.State, err)
	}

	// 预留为零、可用余额等于上限：直接恢复。
	data = buildRefundOverflowBackup(t, t0, math.MaxInt64, 0, map[string]int64{})
	if _, err := restoreAt(data, t0); err != nil {
		t.Fatalf("max available with no reserved: %v", err)
	}
}

// ---- 策略共享累计额度 ----

// quotaReqSpec 描述累计额度测试备份中的一条请求。
type quotaReqSpec struct {
	id           string
	account      string // "u1" 或 "u2"
	estFee       int64
	actualFee    int64
	state        RequestState
	due          bool   // 已预留请求的截止时刻是否早于恢复时刻 t0
	wasReserved  bool   // RequestCancelled 时标识“已预留后取消”
	rejectReason string // RequestRejected 使用
}

// quotaPolicySpec 描述一条策略及其请求。
type quotaPolicySpec struct {
	id                    string
	per, total, threshold int64
	wait, reserveDur      time.Duration
	reqs                  []quotaReqSpec
}

// buildCumulativeQuotaBackup 构造资金完全自洽的备份：账户预留余额、各策略
// 预留/已花费总额均按“现存已预留请求的预估费用”和“已结算请求的实际费用”
// 求和填写，因此余额与各项总额核对必然通过，能否恢复只取决于累计额度判断。
func buildCumulativeQuotaBackup(t *testing.T, t0 time.Time, policies []quotaPolicySpec) []byte {
	t.Helper()
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)},
			{ID: "u2", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)},
		},
		Sessions: []sessionBackupV1{
			{ID: "s1", AccountID: "u1", DeviceID: "d1", ExpiresAt: timeJSON(t0.Add(24 * time.Hour)), CreatedAt: timeJSON(t0)},
			{ID: "s2", AccountID: "u2", DeviceID: "d2", ExpiresAt: timeJSON(t0.Add(24 * time.Hour)), CreatedAt: timeJSON(t0)},
		},
		Policies: make([]policyBackupV1, 0, len(policies)),
		Requests: make([]requestBackupV1, 0),
		Ledger:   []ledgerEntryBackupV1{},
	}
	sessionOf := map[string]string{"u1": "s1", "u2": "s2"}

	var accountReserved int64
	for _, ps := range policies {
		var reservedTotal, spentTotal int64
		b.Policies = append(b.Policies, policyBackupV1{
			ID: ps.id, PayerAccountID: "payer", AllowedAccountIDs: []string{"u1", "u2"},
			Operation: "charge", Payee: "shop",
			StartsAt: timeJSON(t0.Add(-time.Hour)), EndsAt: timeJSON(t0.Add(24 * time.Hour)),
			MaxPerRequest:      ps.per,
			MaxTotal:           ps.total,
			ApprovalThreshold:  ps.threshold,
			ApprovalWait:       durationJSON(ps.wait),
			MaxReserveDuration: durationJSON(ps.reserveDur),
		})
		for _, rs := range ps.reqs {
			r := requestBackupV1{
				PolicyID: ps.id, RequestID: rs.id, AccountID: rs.account, PayerAccountID: "payer",
				SessionID: sessionOf[rs.account], Operation: "charge", Payee: "shop",
				EstimatedFee: rs.estFee, ActualFee: rs.actualFee,
				State:     int(rs.state),
				CreatedAt: timeJSON(t0),
			}
			reservedOrigin := rs.state == RequestReserved || rs.state == RequestSettled ||
				rs.state == RequestReservationExpired ||
				(rs.state == RequestCancelled && rs.wasReserved)
			switch rs.state {
			case RequestPendingApproval, RequestRejected, RequestExpired:
				r.WaitDeadline = timeJSON(t0.Add(ps.wait))
			case RequestCancelled:
				if !reservedOrigin {
					r.DecidedAt = timeJSON(t0)
				}
			}
			if rs.state == RequestRejected {
				r.DecidedAt = timeJSON(t0)
				r.RejectReason = rs.rejectReason
			}
			if rs.state == RequestExpired {
				r.DecidedAt = timeJSON(t0.Add(ps.wait))
			}
			if reservedOrigin {
				reservedAt := t0
				if rs.due || rs.state == RequestReservationExpired {
					reservedAt = t0.Add(-2 * ps.reserveDur)
				}
				r.ReservedAt = timeJSON(reservedAt)
				r.ReserveDuration = durationJSON(ps.reserveDur)
				if ps.reserveDur > 0 {
					r.ReserveDeadline = timeJSON(reservedAt.Add(ps.reserveDur))
				}
				if ps.threshold > 0 && rs.estFee > ps.threshold {
					r.ApproverAccountID = "payer"
					r.DecidedAt = timeJSON(reservedAt)
				}
			}
			if rs.state == RequestSettled {
				r.SettledAt = timeJSON(time.Time(r.ReservedAt).Add(ps.reserveDur))
			}
			if rs.state == RequestReservationExpired {
				r.ReserveExpiredAt = r.ReserveDeadline
			}
			b.Requests = append(b.Requests, r)

			switch rs.state {
			case RequestReserved:
				sum, ok := addInt64(reservedTotal, rs.estFee)
				if !ok {
					t.Fatalf("test setup: reserved sum overflow")
				}
				reservedTotal = sum
				sum, ok = addInt64(accountReserved, rs.estFee)
				if !ok {
					t.Fatalf("test setup: account reserved overflow")
				}
				accountReserved = sum
			case RequestSettled:
				sum, ok := addInt64(spentTotal, rs.actualFee)
				if !ok {
					t.Fatalf("test setup: spent sum overflow")
				}
				spentTotal = sum
			}
		}
		for i := range b.Policies {
			if b.Policies[i].ID == ps.id {
				b.Policies[i].ReservedTotal = reservedTotal
				b.Policies[i].SpentTotal = spentTotal
			}
		}
	}
	for i := range b.Accounts {
		if b.Accounts[i].ID == "payer" {
			b.Accounts[i].Reserved = accountReserved
		}
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestRestoreRejectsPolicyCumulativeQuotaExceeded 验证：即使账户预留余额、
// 策略预留总额与已花费总额分别与请求求和一致，任一策略“现存预留 + 已结算
// 实际费用”严格超过共享累计上限时，整个备份必须被拒绝。
func TestRestoreRejectsPolicyCumulativeQuotaExceeded(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		policies   []quotaPolicySpec
		restoreAt  time.Time
		wantPolicy string
	}{
		{
			name: "settled 30 plus reserved 30 over total 50",
			policies: []quotaPolicySpec{{
				id: "p-over", per: 30, total: 50,
				reqs: []quotaReqSpec{
					{id: "settled", account: "u1", estFee: 30, actualFee: 30, state: RequestSettled},
					{id: "reserved", account: "u1", estFee: 30, state: RequestReserved},
				},
			}},
			restoreAt:  t0,
			wantPolicy: "p-over",
		},
		{
			name: "shared across usage accounts",
			policies: []quotaPolicySpec{{
				id: "p-shared", per: 30, total: 50,
				reqs: []quotaReqSpec{
					{id: "by-u1", account: "u1", estFee: 30, actualFee: 30, state: RequestSettled},
					{id: "by-u2", account: "u2", estFee: 30, state: RequestReserved},
				},
			}},
			restoreAt:  t0,
			wantPolicy: "p-shared",
		},
		{
			name: "reserved already due at restore is still counted",
			policies: []quotaPolicySpec{{
				id: "p-due", per: 30, total: 50, reserveDur: time.Minute,
				reqs: []quotaReqSpec{
					{id: "settled", account: "u1", estFee: 30, actualFee: 30, state: RequestSettled},
					{id: "reserved-due", account: "u1", estFee: 30, state: RequestReserved, due: true},
				},
			}},
			// 恢复时该预留已过截止时刻，按正常规则将全额退回，但仍须先拒绝。
			restoreAt:  t0,
			wantPolicy: "p-due",
		},
		{
			name: "second policy over limit is named",
			policies: []quotaPolicySpec{
				{
					id: "p1", per: 100, total: 50,
					reqs: []quotaReqSpec{
						{id: "s1", account: "u1", estFee: 30, actualFee: 20, state: RequestSettled},
						{id: "r1", account: "u1", estFee: 30, state: RequestReserved},
					},
				},
				{
					id: "p2", per: 100, total: 50,
					reqs: []quotaReqSpec{
						{id: "s2", account: "u1", estFee: 30, actualFee: 21, state: RequestSettled},
						{id: "r2", account: "u1", estFee: 30, state: RequestReserved},
					},
				},
			},
			restoreAt:  t0,
			wantPolicy: "p2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := buildCumulativeQuotaBackup(t, t0, tc.policies)
			w2, err := restoreAt(data, tc.restoreAt)
			if err == nil {
				if w2 != nil {
					t.Fatalf("over-quota backup restored a wallet")
				}
				t.Fatalf("over-quota backup restored without error")
			}
			if !errors.Is(err, ErrBackupInvalid) {
				t.Fatalf("err = %v, want ErrBackupInvalid", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantPolicy) {
				t.Fatalf("error %q must name policy %q", msg, tc.wantPolicy)
			}
			if !strings.Contains(msg, "cumulative total limit") {
				t.Fatalf("error %q must explain the cumulative total limit", msg)
			}
		})
	}
}

// TestRestoreRejectsPolicyCumulativeQuotaOverflow 验证预留与已花费各自都能
// 由 int64 表示、合计却越过 int64 上限时必须拒绝，不能因金额越界误判为
// 额度充足。
func TestRestoreRejectsPolicyCumulativeQuotaOverflow(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildCumulativeQuotaBackup(t, t0, []quotaPolicySpec{{
		id: "p-big", per: math.MaxInt64, total: math.MaxInt64,
		reqs: []quotaReqSpec{
			{id: "settled", account: "u1", estFee: math.MaxInt64, actualFee: math.MaxInt64, state: RequestSettled},
			{id: "reserved", account: "u1", estFee: 1, state: RequestReserved},
		},
	}})
	w2, err := restoreAt(data, t0)
	if err == nil {
		if w2 != nil {
			t.Fatalf("overflowing-quota backup restored a wallet")
		}
		t.Fatalf("overflowing-quota backup restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "p-big") || !strings.Contains(msg, "cumulative total limit") {
		t.Fatalf("error %q must name policy and cumulative limit", msg)
	}
}

// TestRestoreAcceptsCumulativeQuotaAtBoundary 验证合计恰好等于累计上限合法，
// 包括上限恰为 int64 最大值的情况；不缩小现有金额范围。
func TestRestoreAcceptsCumulativeQuotaAtBoundary(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// 上限 50：结算实际 20（预估 30，差额已退回）+ 现存预留 30 = 50。
	data := buildCumulativeQuotaBackup(t, t0, []quotaPolicySpec{{
		id: "p-eq", per: 30, total: 50,
		reqs: []quotaReqSpec{
			{id: "settled", account: "u1", estFee: 30, actualFee: 20, state: RequestSettled},
			{id: "reserved", account: "u1", estFee: 30, state: RequestReserved},
		},
	}})
	if _, err := restoreAt(data, t0); err != nil {
		t.Fatalf("used == total restore: %v", err)
	}

	// 上限 MaxInt64：已花费 MaxInt64-1 + 预留 1 = MaxInt64。
	data = buildCumulativeQuotaBackup(t, t0, []quotaPolicySpec{{
		id: "p-max", per: math.MaxInt64, total: math.MaxInt64,
		reqs: []quotaReqSpec{
			{id: "settled", account: "u1", estFee: math.MaxInt64, actualFee: math.MaxInt64 - 1, state: RequestSettled},
			{id: "reserved", account: "u1", estFee: 1, state: RequestReserved},
		},
	}})
	if _, err := restoreAt(data, t0); err != nil {
		t.Fatalf("used == MaxInt64 restore: %v", err)
	}

	// 上限 MaxInt64：单笔已结算实际费用 MaxInt64，占满额度。
	data = buildCumulativeQuotaBackup(t, t0, []quotaPolicySpec{{
		id: "p-spent-max", per: math.MaxInt64, total: math.MaxInt64,
		reqs: []quotaReqSpec{
			{id: "settled", account: "u1", estFee: math.MaxInt64, actualFee: math.MaxInt64, state: RequestSettled},
		},
	}})
	if _, err := restoreAt(data, t0); err != nil {
		t.Fatalf("spent == MaxInt64 restore: %v", err)
	}

	// 上限 MaxInt64：单笔预估费用 MaxInt64 的现存预留占满额度。
	data = buildCumulativeQuotaBackup(t, t0, []quotaPolicySpec{{
		id: "p-reserved-max", per: math.MaxInt64, total: math.MaxInt64,
		reqs: []quotaReqSpec{
			{id: "reserved", account: "u1", estFee: math.MaxInt64, state: RequestReserved},
		},
	}})
	if _, err := restoreAt(data, t0); err != nil {
		t.Fatalf("reserved == MaxInt64 restore: %v", err)
	}

	// 不同策略分别判断：同一出资账户的两条策略各自恰好占满 50，
	// 跨策略合计 100 不影响合法性。
	data = buildCumulativeQuotaBackup(t, t0, []quotaPolicySpec{
		{
			id: "p1", per: 100, total: 50,
			reqs: []quotaReqSpec{
				{id: "s1", account: "u1", estFee: 30, actualFee: 20, state: RequestSettled},
				{id: "r1", account: "u1", estFee: 30, state: RequestReserved},
			},
		},
		{
			id: "p2", per: 100, total: 50,
			reqs: []quotaReqSpec{
				{id: "s2", account: "u2", estFee: 30, actualFee: 20, state: RequestSettled},
				{id: "r2", account: "u2", estFee: 30, state: RequestReserved},
			},
		},
	})
	if _, err := restoreAt(data, t0); err != nil {
		t.Fatalf("two policies each at their own limit: %v", err)
	}
}

// TestRestoreCumulativeQuotaIgnoresNonOccupyingStates 验证只有现存预留与已
// 结算实际费用占用累计额度：待审批、审批拒绝、待审批取消、待审批过期、已
// 预留后取消、预留超时的请求都不占用；结算退回的差额也不计入已花费。
func TestRestoreCumulativeQuotaIgnoresNonOccupyingStates(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// 累计上限仅 10：一笔实际费用 10 的已结算请求占满额度；其余请求费用均
	// 为 30（待审批系严格超过门槛 10），但都不占用额度，备份仍合法。
	data := buildCumulativeQuotaBackup(t, t0, []quotaPolicySpec{{
		id: "p-free", per: 100, total: 10, threshold: 10,
		wait: 5 * time.Minute, reserveDur: time.Minute,
		reqs: []quotaReqSpec{
			{id: "settled", account: "u1", estFee: 10, actualFee: 10, state: RequestSettled},
			{id: "reserved-cancelled", account: "u1", estFee: 10, state: RequestCancelled, wasReserved: true},
			{id: "reservation-expired", account: "u1", estFee: 10, state: RequestReservationExpired},
			{id: "pending", account: "u1", estFee: 30, state: RequestPendingApproval},
			{id: "rejected", account: "u1", estFee: 30, state: RequestRejected, rejectReason: "no"},
			{id: "pending-expired", account: "u1", estFee: 30, state: RequestExpired},
			{id: "pending-cancelled", account: "u1", estFee: 30, state: RequestCancelled},
		},
	}})
	w2, err := restoreAt(data, t0)
	if err != nil {
		t.Fatalf("non-counting states restore: %v", err)
	}
	// 未到期的待审批请求不被改写；没有任何现存预留，账户预留余额为零。
	r, err := w2.Request("u1", "pending")
	if err != nil || r.State != RequestPendingApproval {
		t.Fatalf("pending request state = %v err = %v, want still pending", r.State, err)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 0, Reserved: 0}) {
		t.Fatalf("payer balance = %+v, want zero reserved", bal)
	}
}

// ---- 并发 ----

func TestExportConcurrentWithMutations(t *testing.T) {
	w, c := newTestWallet()
	t0 := c.t
	mustAccount(t, w, "payer", 1_000_000)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", t0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s2", "u2", "dev2", t0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(PolicySpec{
		ID: "p1", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1", "u2"},
		Operation: "charge", Payee: "shop",
		StartsAt: t0.Add(-time.Hour), EndsAt: t0.Add(24 * time.Hour),
		MaxPerRequest: 100, MaxTotal: 1_000_000,
		MaxReserveDuration: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}

	const n = 200
	var wg sync.WaitGroup
	var backups [][]byte
	var mu sync.Mutex

	// 申请/结算/取消/导出 并发；每个备份都必须是可恢复的完整状态。
	for g := 0; g < 4; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < n; i++ {
				rid := fmt.Sprintf("g%d-r%d", g, i)
				in := RequestInput{
					PolicyID: "p1", RequestID: rid, AccountID: "u2", SessionID: "s2",
					DeviceID: "dev2", Operation: "charge", Payee: "shop", EstimatedFee: 1,
				}
				if _, err := w.Apply(in); err != nil {
					t.Errorf("apply: %v", err)
					return
				}
				switch i % 3 {
				case 0:
					if _, err := w.Settle("u2", rid, 1); err != nil {
						t.Errorf("settle: %v", err)
					}
				case 1:
					if _, err := w.Cancel("u2", rid); err != nil {
						t.Errorf("cancel: %v", err)
					}
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			data, err := w.Export()
			if err != nil {
				t.Errorf("export: %v", err)
				return
			}
			mu.Lock()
			backups = append(backups, data)
			mu.Unlock()
		}
	}()
	wg.Wait()

	if len(backups) == 0 {
		t.Fatal("no backups captured")
	}
	for i, data := range backups {
		w2, err := Restore(data)
		if err != nil {
			t.Fatalf("backup %d not restorable: %v", i, err)
		}
		// 恢复出的钱包余额自洽：非负且预留不凭空消失。
		for _, id := range []string{"payer"} {
			bal, err := w2.Balance(id)
			if err != nil {
				t.Fatalf("backup %d balance: %v", i, err)
			}
			if bal.Available < 0 || bal.Reserved < 0 {
				t.Fatalf("backup %d negative balance: %+v", i, bal)
			}
		}
	}

	// 最终状态本身也必须可导出/恢复且账本完整（用与源钱包相同的受控时刻，
	// 避免恢复时钟把未到期预留结算掉而混淆比较）。
	final, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(final, c.t)
	if err != nil {
		t.Fatal(err)
	}
	if l1, l2 := len(w.Ledger()), len(w2.Ledger()); l1 != l2 {
		t.Fatalf("final ledger length = %d, want %d", l2, l1)
	}
}
