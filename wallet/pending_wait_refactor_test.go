package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// 本文件锁定“结束待审批等待”在查询、吊销申请会话、停用所属策略三个入口共用
// 一套收口逻辑后的关键不变量：以每笔请求自身的等待截止时间为准分别处理、
// 账本按等待截止时间追加、各触发方式的原因/审批账户差异保留、查询先形成的
// 过期终态不被后续吊销或停用补写，以及导出/恢复沿用同一口径与备份格式。

const (
	testRevokedReason  = "application session revoked before approval"
	testExpiredReason  = "approval period expired"
	testDeactivateNote = "stop pending now"
)

// setupSharedWait 构造会话与策略窗口都长达 4 小时的审批钱包，使等待期限只由
// “提交时刻 + 等待时长”决定，便于在同一会话/策略下构造不同的等待截止时间。
func setupSharedWait(t *testing.T) (*Wallet, *clock) {
	t.Helper()
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s2", "u2", "dev2", c.t.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "dev-approve", c.t.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	p := approvalPolicy(c)
	p.EndsAt = c.t.Add(4 * time.Hour)
	if err := w.SavePolicy(p); err != nil {
		t.Fatal(err)
	}
	return w, c
}

func waitApply(account, session, device, rid string) RequestInput {
	return RequestInput{
		PolicyID: "p-approval", RequestID: rid, AccountID: account, SessionID: session,
		DeviceID: device, Operation: "charge", Payee: "shop", EstimatedFee: 20,
	}
}

// requestLedgerTail 返回账本中从 start 起、类型为拒绝或过期的状态记录对应的
// (请求编号, 记录类型) 序列，用于核对多笔请求分别处理时的账本追加顺序。
func requestLedgerTail(entries []LedgerEntry, start int) []struct {
	rid  string
	kind LedgerKind
} {
	var out []struct {
		rid  string
		kind LedgerKind
	}
	for i := start; i < len(entries); i++ {
		e := entries[i]
		if e.Kind == LedgerRejection || e.Kind == LedgerExpiration {
			out = append(out, struct {
				rid  string
				kind LedgerKind
			}{e.RequestID, e.Kind})
		}
	}
	return out
}

// TestSharedWaitRevokeSplitsByOwnDeadlineAndOrdersLedger：一次吊销涉及同一会话
// 多笔请求时，已到截止的进入过期、未到的按吊销拒绝，账本严格按等待截止时间
// 追加；提交时间与等待截止时间不变，资金完全不动。
func TestSharedWaitRevokeSplitsByOwnDeadlineAndOrdersLedger(t *testing.T) {
	w, c := setupSharedWait(t)

	r1, err := w.Apply(waitApply("u1", "s1", "dev1", "r1"))
	if err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(20 * time.Second)
	r2, err := w.Apply(waitApply("u1", "s1", "dev1", "r2"))
	if err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(20 * time.Second) // t=40s
	r3, err := w.Apply(waitApply("u1", "s1", "dev1", "r3"))
	if err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(20 * time.Second) // t=60s：恰为 r1 的截止时刻。

	before := len(w.Ledger())
	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	revokeAt := c.t

	g1, _ := w.Request("u1", "r1")
	g2, _ := w.Request("u1", "r2")
	g3, _ := w.Request("u1", "r3")

	// r1 恰到截止：等待过期；r2/r3 仍在各自截止之前：吊销拒绝。
	if g1.State != RequestExpired {
		t.Fatalf("r1 at deadline state = %v, want expired", g1.State)
	}
	if g1.RejectReason != "" || g1.ApproverAccountID != "" {
		t.Fatalf("expired r1 carries reject fields: %+v", g1)
	}
	if !g1.DecidedAt.Equal(revokeAt) || !g1.CreatedAt.Equal(r1.CreatedAt) || !g1.WaitDeadline.Equal(r1.WaitDeadline) {
		t.Fatalf("r1 timing changed: %+v", g1)
	}
	for _, g := range []RequestView{g2, g3} {
		if g.State != RequestRejected {
			t.Fatalf("%s state = %v, want revoked-before-deadline rejection", g.RequestID, g.State)
		}
		if g.RejectReason != testRevokedReason {
			t.Fatalf("%s reason = %q, want %q", g.RequestID, g.RejectReason, testRevokedReason)
		}
		if g.ApproverAccountID != "" {
			t.Fatalf("revocation rejection must not fill approver, got %q", g.ApproverAccountID)
		}
		if !g.DecidedAt.Equal(revokeAt) {
			t.Fatalf("%s decidedAt = %v, want revoke time %v", g.RequestID, g.DecidedAt, revokeAt)
		}
	}
	if !g2.WaitDeadline.Equal(r2.WaitDeadline) || !g3.WaitDeadline.Equal(r3.WaitDeadline) {
		t.Fatal("wait deadlines must be preserved")
	}

	// 账本尾部三笔按等待截止时间排序：r1(60s 过期) -> r2(80s 拒绝) -> r3(100s 拒绝)。
	all := w.Ledger()
	tail := requestLedgerTail(all, before)
	want := []struct {
		rid  string
		kind LedgerKind
	}{
		{"r1", LedgerExpiration},
		{"r2", LedgerRejection},
		{"r3", LedgerRejection},
	}
	if len(tail) != len(want) {
		t.Fatalf("tail = %+v, want %+v", tail, want)
	}
	for i := range want {
		if tail[i] != want[i] {
			t.Fatalf("tail[%d] = %+v, want %+v (full tail %+v)", i, tail[i], want[i], tail)
		}
	}
	// 每条状态记录都是关联使用账户、零金额；过期原因沿用统一文本。
	for _, e := range all[before:] {
		if e.AccountID != "u1" || e.Amount != 0 {
			t.Fatalf("status entry must be zero-amount on usage account: %+v", e)
		}
		if e.Kind == LedgerExpiration && e.Reason != testExpiredReason {
			t.Fatalf("expiration reason = %q", e.Reason)
		}
		if e.Kind == LedgerExpiration && !e.At.Equal(revokeAt) {
			t.Fatalf("expiration recorded at %v, want processing time %v", e.At, revokeAt)
		}
	}

	// 等待结束始终不冻结/退回费用：余额与策略总额不变。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want untouched", bal)
	}
	if pv, _ := w.Policy("p-approval"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals changed: %+v", pv)
	}
}

// TestSharedWaitDeactivateSplitsAcrossAccountsAndOrdersLedger：一次停用涉及同一
// 策略下两个使用账户的多笔请求时，到期的过期、未到期的按停用拒绝，账本按等待
// 截止时间排序（同截止时间按使用账户排序）；停用自身留痕保留且排在请求记录
// 之前，停用拒绝的审批账户为出资账户。
func TestSharedWaitDeactivateSplitsAcrossAccountsAndOrdersLedger(t *testing.T) {
	w, c := setupSharedWait(t)

	r1, err := w.Apply(waitApply("u1", "s1", "dev1", "r1")) // 截止 60s
	if err != nil {
		t.Fatal(err)
	}
	q1, err := w.Apply(waitApply("u2", "s2", "dev2", "q1")) // 截止 60s（同刻、不同账户）
	if err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(30 * time.Second)
	r2, err := w.Apply(waitApply("u1", "s1", "dev1", "r2")) // 截止 90s
	if err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(30 * time.Second) // t=60s：r1/q1 恰到截止，r2 未到。

	before := len(w.Ledger())
	pv, err := w.DeactivatePolicy("p-approval", "sa", "dev-approve", "  "+testDeactivateNote+"  ")
	if err != nil {
		t.Fatal(err)
	}
	deactAt := c.t

	g1, _ := w.Request("u1", "r1")
	gq1, _ := w.Request("u2", "q1")
	g2, _ := w.Request("u1", "r2")

	// 到期的两笔进入等待过期：无理由、无审批账户，决定时间为停用时刻。
	for _, g := range []RequestView{g1, gq1} {
		if g.State != RequestExpired {
			t.Fatalf("%s state = %v, want expired", g.RequestID, g.State)
		}
		if g.RejectReason != "" || g.ApproverAccountID != "" {
			t.Fatalf("expired %s carries reject fields: %+v", g.RequestID, g)
		}
		if !g.DecidedAt.Equal(deactAt) {
			t.Fatalf("%s decidedAt = %v, want deactivate time %v", g.RequestID, g.DecidedAt, deactAt)
		}
	}
	if !g1.WaitDeadline.Equal(r1.WaitDeadline) || !gq1.WaitDeadline.Equal(q1.WaitDeadline) {
		t.Fatal("wait deadlines must be preserved")
	}
	// 未到期的一笔随停用拒绝：理由含停用说明与理由，审批账户为出资账户。
	if g2.State != RequestRejected {
		t.Fatalf("r2 before deadline state = %v, want rejected", g2.State)
	}
	if !strings.Contains(g2.RejectReason, "deactivat") || !strings.Contains(g2.RejectReason, testDeactivateNote) {
		t.Fatalf("r2 reject reason = %q", g2.RejectReason)
	}
	if g2.ApproverAccountID != "payer" {
		t.Fatalf("r2 approver = %q, want payer", g2.ApproverAccountID)
	}
	if !g2.WaitDeadline.Equal(r2.WaitDeadline) {
		t.Fatal("r2 wait deadline changed")
	}

	all := w.Ledger()
	tail := all[before:]
	// 尾部首条必须是停用自身留痕（出资账户 + 策略、零金额、trim 后理由）。
	if len(tail) < 1 || tail[0].Kind != LedgerPolicyDeactivation {
		t.Fatalf("first appended entry must be policy deactivation: %+v", tail)
	}
	d := tail[0]
	if d.AccountID != "payer" || d.PolicyID != "p-approval" || d.RequestID != "" || d.Amount != 0 ||
		d.Reason != testDeactivateNote || !d.At.Equal(deactAt) {
		t.Fatalf("deactivation entry = %+v", d)
	}
	// 请求记录按等待截止时间排序：r1、q1 同为 60s，按使用账户 u1<u2；r2 为 90s。
	got := requestLedgerTail(all, before)
	want := []struct {
		rid  string
		kind LedgerKind
	}{
		{"r1", LedgerExpiration},
		{"q1", LedgerExpiration},
		{"r2", LedgerRejection},
	}
	if len(got) != len(want) {
		t.Fatalf("request tail = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("request tail[%d] = %+v, want %+v (full %+v)", i, got[i], want[i], got)
		}
	}
	// 停用拒绝记录关联使用账户与请求编号、零金额，且与停用留痕是两类记录。
	var rej LedgerEntry
	for _, e := range tail {
		if e.Kind == LedgerRejection && e.RequestID == "r2" {
			rej = e
		}
	}
	if rej.AccountID != "u1" || rej.Amount != 0 || rej.PolicyID != "" {
		t.Fatalf("r2 rejection entry = %+v", rej)
	}
	if countKind(all, LedgerPolicyDeactivation) != 1 {
		t.Fatal("exactly one deactivation entry expected")
	}
	_ = pv
	// 不冻结、不占用、不退款。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want untouched", bal)
	}
}

// TestSharedWaitQueryExpiredThenDeactivatePreservesFirstResult：请求先被查询进入
// 过期终态后，再停用策略必须保留第一次的结果——不补写拒绝记录，也不补写第二
// 条过期记录，第一次的决定时间不被改写；同一操作中其他到期/未到期请求仍分别
// 处理；停用自身留痕保留。
func TestSharedWaitQueryExpiredThenDeactivatePreservesFirstResult(t *testing.T) {
	w, c := setupSharedWait(t)

	if _, err := w.Apply(waitApply("u1", "s1", "dev1", "r1")); err != nil { // 截止 60s
		t.Fatal(err)
	}
	if _, err := w.Apply(waitApply("u1", "s1", "dev1", "r2")); err != nil { // 截止 60s
		t.Fatal(err)
	}

	// t=60s：仅查询 r1，使其先形成等待过期终态（决定时间 60s，一条过期记录）。
	c.t = c.t.Add(time.Minute)
	queryAt := c.t
	g1, _ := w.Request("u1", "r1")
	if g1.State != RequestExpired || !g1.DecidedAt.Equal(queryAt) {
		t.Fatalf("queried r1 = %+v, want expired at %v", g1, queryAt)
	}

	// t=80s 再提交 r3（截止 140s，停用时尚未到期）。
	c.t = c.t.Add(20 * time.Second)
	if _, err := w.Apply(waitApply("u1", "s1", "dev1", "r3")); err != nil {
		t.Fatal(err)
	}
	// 在停用前一刻快照账本（r3 的待审批留痕已计入其中）。
	before := len(w.Ledger())
	c.t = c.t.Add(10 * time.Second) // t=90s
	deactAt := c.t
	if _, err := w.DeactivatePolicy("p-approval", "sa", "dev-approve", testDeactivateNote); err != nil {
		t.Fatal(err)
	}

	all := w.Ledger()
	tail := all[before:]

	// 停用后尾部：一条停用留痕；r2 到期进入过期；r3 未到期随停用拒绝。
	// r1 已是终态：既不新增过期记录，也不被改写成停用拒绝。
	var kinds []LedgerKind
	var rids []string
	for _, e := range tail {
		kinds = append(kinds, e.Kind)
		rids = append(rids, e.RequestID)
	}
	wantKinds := []LedgerKind{LedgerPolicyDeactivation, LedgerExpiration, LedgerRejection}
	wantRids := []string{"", "r2", "r3"}
	if len(kinds) != 3 {
		t.Fatalf("tail kinds = %v rids %v, want 3 entries deactivation/expire/reject", kinds, rids)
	}
	for i := range wantKinds {
		if kinds[i] != wantKinds[i] || rids[i] != wantRids[i] {
			t.Fatalf("tail[%d] kind=%v rid=%q, want %v %q (all kinds=%v rids=%v)",
				i, kinds[i], rids[i], wantKinds[i], wantRids[i], kinds, rids)
		}
	}

	// r1 保留第一次结果：终态仍是过期、决定时间停留在查询时刻、无理由无审批账户。
	g1, _ = w.Request("u1", "r1")
	if g1.State != RequestExpired || !g1.DecidedAt.Equal(queryAt) || g1.RejectReason != "" || g1.ApproverAccountID != "" {
		t.Fatalf("first expiration rewritten by later deactivation: %+v", g1)
	}
	if e, ok := ledgerEntryFor(all, LedgerExpiration, "r1"); !ok || !e.At.Equal(queryAt) {
		t.Fatalf("r1 expiration entry wrong/missing: %+v ok=%v", e, ok)
	}
	if _, ok := ledgerEntryFor(all, LedgerRejection, "r1"); ok {
		t.Fatal("r1 must not gain a deactivation rejection")
	}
	if n := countKindFor(all, LedgerExpiration, "r1"); n != 1 {
		t.Fatalf("r1 expiration entries = %d, want 1", n)
	}

	// r2 在停用时刻过期；r3 截止前随停用拒绝。
	g2, _ := w.Request("u1", "r2")
	if g2.State != RequestExpired || !g2.DecidedAt.Equal(deactAt) || g2.RejectReason != "" || g2.ApproverAccountID != "" {
		t.Fatalf("r2 = %+v, want expired at deactivate time", g2)
	}
	g3, _ := w.Request("u1", "r3")
	if g3.State != RequestRejected || g3.ApproverAccountID != "payer" ||
		!strings.Contains(g3.RejectReason, testDeactivateNote) {
		t.Fatalf("r3 = %+v, want deactivation rejection by payer", g3)
	}
	// 停用留痕仍恰好一条。
	if countKind(all, LedgerPolicyDeactivation) != 1 {
		t.Fatal("deactivation must still be recorded exactly once")
	}
	// 资金不动。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want untouched", bal)
	}
}

// countKindFor 统计指定请求的某类账本记录条数。
func countKindFor(entries []LedgerEntry, kind LedgerKind, requestID string) int {
	n := 0
	for _, e := range entries {
		if e.Kind == kind && e.RequestID == requestID {
			n++
		}
	}
	return n
}

// TestSharedWaitExportRestoreUsesSameExpiration：导出时到期的待审批请求在快照内
// 即按统一口径进入过期终态并只留一条零金额过期记录；同时刻恢复不重复；导出时
// 未到期、恢复时到期的请求在恢复时补一条过期记录。备份版本与可恢复性不变。
func TestSharedWaitExportRestoreUsesSameExpiration(t *testing.T) {
	w, c := setupSharedWait(t)

	r1, err := w.Apply(waitApply("u1", "s1", "dev1", "r1")) // 截止 60s
	if err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(30 * time.Second)
	r3, err := w.Apply(waitApply("u1", "s1", "dev1", "r3")) // 截止 90s
	if err != nil {
		t.Fatal(err)
	}

	// t=60s 导出：r1 到期（决定时间 60s），r3 仍待审批。
	c.t = c.t.Add(30 * time.Second)
	exportAt := c.t
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	g1, _ := w.Request("u1", "r1")
	if g1.State != RequestExpired || g1.RejectReason != "" || g1.ApproverAccountID != "" || !g1.DecidedAt.Equal(exportAt) {
		t.Fatalf("exported r1 = %+v, want expired at %v", g1, exportAt)
	}
	if e, ok := ledgerEntryFor(w.Ledger(), LedgerExpiration, "r1"); !ok || e.AccountID != "u1" ||
		e.Amount != 0 || e.Reason != testExpiredReason || !e.At.Equal(exportAt) {
		t.Fatalf("r1 expiration ledger = %+v ok=%v", e, ok)
	}
	g3, _ := w.Request("u1", "r3")
	if g3.State != RequestPendingApproval || !g3.WaitDeadline.Equal(r3.WaitDeadline) {
		t.Fatalf("r3 should remain pending before its deadline: %+v", g3)
	}
	_ = r1

	// 同时刻恢复：不重复记账、不重复过期，r1 视图与原钱包一致。
	same, err := restoreAt(data, exportAt)
	if err != nil {
		t.Fatal(err)
	}
	if l1, l2 := len(w.Ledger()), len(same.Ledger()); l1 != l2 {
		t.Fatalf("same-time restore duplicated ledger: %d vs %d", l1, l2)
	}
	rg1, _ := same.Request("u1", "r1")
	if rg1.State != RequestExpired || !rg1.DecidedAt.Equal(exportAt) || rg1.RejectReason != "" || rg1.ApproverAccountID != "" {
		t.Fatalf("restored r1 = %+v", rg1)
	}

	// 推进到 t=120s（> r3 截止 90s）恢复：r3 按恢复时刻进入过期终态，r1 不新增记录。
	later := exportAt.Add(time.Minute)
	w2, err := restoreAt(data, later)
	if err != nil {
		t.Fatal(err)
	}
	rg3, _ := w2.Request("u1", "r3")
	if rg3.State != RequestExpired || !rg3.DecidedAt.Equal(later) || rg3.RejectReason != "" || rg3.ApproverAccountID != "" {
		t.Fatalf("restored r3 = %+v, want expired at restore time %v", rg3, later)
	}
	if !rg3.CreatedAt.Equal(r3.CreatedAt) || !rg3.WaitDeadline.Equal(r3.WaitDeadline) {
		t.Fatal("r3 created/deadline times must be preserved")
	}
	if e, ok := ledgerEntryFor(w2.Ledger(), LedgerExpiration, "r3"); !ok || e.AccountID != "u1" ||
		e.Amount != 0 || e.Reason != testExpiredReason || !e.At.Equal(later) {
		t.Fatalf("r3 expiration ledger = %+v ok=%v", e, ok)
	}
	if n := countKindFor(w2.Ledger(), LedgerExpiration, "r1"); n != 1 {
		t.Fatalf("r1 expiration entries after restore = %d, want 1", n)
	}
	if n := countKindFor(w2.Ledger(), LedgerRejection, "r1"); n != 0 {
		t.Fatalf("r1 gained rejection entries = %d", n)
	}
	// 备份格式版本保持不变。
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		t.Fatal(err)
	}
	if head.Version != backupVersion {
		t.Fatalf("backup version = %d, want %d", head.Version, backupVersion)
	}
}

// TestSharedWaitApprovedReservedAndTerminalsUntouched：已批准预留的请求继续按
// 既有规则结算，已拒绝/已取消/已过期的请求不改写；结束等待只追加、不改变
// 既有账本内容与顺序，也不动余额与策略总额。
func TestSharedWaitApprovedReservedAndTerminalsUntouched(t *testing.T) {
	w, c := setupSharedWait(t)

	// rApprove：超门槛进入待审批，随后批准完成预留。
	if _, err := w.Apply(waitApply("u1", "s1", "dev1", "rApprove")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Approve("u1", "rApprove", "sa", "dev-approve"); err != nil {
		t.Fatal(err)
	}
	// rReject：审批拒绝终态。
	if _, err := w.Apply(waitApply("u1", "s1", "dev1", "rReject")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Reject("u1", "rReject", "sa", "dev-approve", "manual no"); err != nil {
		t.Fatal(err)
	}
	// rCancel：待审批取消终态。
	if _, err := w.Apply(waitApply("u1", "s1", "dev1", "rCancel")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Cancel("u1", "rCancel"); err != nil {
		t.Fatal(err)
	}
	// rExpire：查询触发的等待过期终态（截止 60s）。
	if _, err := w.Apply(waitApply("u1", "s1", "dev1", "rExpire")); err != nil {
		t.Fatal(err)
	}
	// rDue：越过截止后不查询，留给停用/吊销统一收口。
	if _, err := w.Apply(waitApply("u1", "s1", "dev1", "rDue")); err != nil {
		t.Fatal(err)
	}

	c.t = c.t.Add(time.Minute) // 所有未决定请求的等待截止（60s）均已到。
	g, _ := w.Request("u1", "rExpire")
	if g.State != RequestExpired {
		t.Fatalf("rExpire = %v, want expired", g.State)
	}
	snapshotLen := len(w.Ledger())
	snapshot := append([]LedgerEntry(nil), w.Ledger()...)

	if err := w.RevokeSession("s1"); err != nil {
		t.Fatal(err)
	}
	// 吊销只应把从未查询过、已到期的 rDue 补成过期，其余终态/已预留请求不动。
	if n := len(w.Ledger()) - snapshotLen; n != 1 {
		t.Fatalf("revoke appended %d entries, want 1 (rDue expiration)", n)
	}
	// 既有账本内容与先后顺序保持不变。
	for i, e := range snapshot {
		if w.Ledger()[i] != e {
			t.Fatalf("existing ledger[%d] rewritten: %+v -> %+v", i, e, w.Ledger()[i])
		}
	}

	ra, _ := w.Request("u1", "rApprove")
	if ra.State != RequestReserved {
		t.Fatalf("approved-reserved request rewritten: %v", ra.State)
	}
	// 已批准并完成预留的请求继续按既有规则结算（不被改成等待过期）。
	if _, err := w.Settle("u1", "rApprove", 20); err != nil {
		t.Fatalf("settle approved reservation: %v", err)
	}
	if rr, _ := w.Request("u1", "rReject"); rr.State != RequestRejected || rr.RejectReason != "manual no" {
		t.Fatalf("rejected rewritten: %+v", rr)
	}
	if rc, _ := w.Request("u1", "rCancel"); rc.State != RequestCancelled {
		t.Fatalf("cancelled rewritten: %v", rc.State)
	}
	if re, _ := w.Request("u1", "rExpire"); re.State != RequestExpired {
		t.Fatalf("expired rewritten: %v", re.State)
	}
	if rd, _ := w.Request("u1", "rDue"); rd.State != RequestExpired || rd.RejectReason != "" || rd.ApproverAccountID != "" {
		t.Fatalf("rDue = %+v, want plain expiration", rd)
	}
	// 结算实际扣除 20：可用 100-20=80，预留归零；策略已花费 20。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 80, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {80 0}", bal)
	}
	if pv, _ := w.Policy("p-approval"); pv.ReservedTotal != 0 || pv.SpentTotal != 20 {
		t.Fatalf("policy totals = reserved %d spent %d, want 0/20", pv.ReservedTotal, pv.SpentTotal)
	}
	// 已过期请求不能再批准/拒绝/结算/取消。
	if _, err := w.Approve("u1", "rDue", "sa", "dev-approve"); !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("approve expired err = %v, want ErrApprovalExpired", err)
	}
}
