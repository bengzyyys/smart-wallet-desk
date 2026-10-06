package wallet

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// 本组测试回归保障代付策略只读数据的约定：调用方修改自己持有的策略内容
// （SavePolicy 的入参、Policy / DeactivatePolicy 返回的视图），不能改变
// 钱包已经保存的授权名单或停用状态；实际受理结果也必须以钱包保存的授权
// 为准，而不是调用方手里的副本。

// setupPolicyIsolation 构造一条由出资账户 payer 承担费用、允许多个使用
// 账户（u1、u2）申请的策略所需的钱包：payer 余额充足，u1/u2/u3 均为已
// 存在的账户且各自持有绑定当前设备的有效会话，其中 u3 从未获授权。
// 返回的 spec 尚未保存，调用方可自行改写授权名单后再保存。
func setupPolicyIsolation(t *testing.T) (*Wallet, *clock, PolicySpec) {
	t.Helper()
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 1000)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	mustAccount(t, w, "u3", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s2", "u2", "dev2", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("s3", "u3", "dev3", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.CreateSession("sa", "payer", "dev-owner", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	spec := PolicySpec{
		ID:                "p1",
		PayerAccountID:    "payer",
		AllowedAccountIDs: []string{"u1", "u2"},
		Operation:         "charge",
		Payee:             "shop",
		StartsAt:          c.t.Add(-time.Hour),
		EndsAt:            c.t.Add(time.Hour),
		MaxPerRequest:     30,
		MaxTotal:          500,
	}
	return w, c, spec
}

func isolationApply(account, session, device, requestID string, fee int64) RequestInput {
	return RequestInput{
		PolicyID:     "p1",
		RequestID:    requestID,
		AccountID:    account,
		SessionID:    session,
		DeviceID:     device,
		Operation:    "charge",
		Payee:        "shop",
		EstimatedFee: fee,
	}
}

// allowedSet 以账户集合形式读取钱包保存的授权名单。授权名单按集合判断，
// 现有功能没有约定返回顺序，因此测试也不依赖顺序。
func allowedSet(t *testing.T, w *Wallet, policyID string) map[string]struct{} {
	t.Helper()
	pv, err := w.Policy(policyID)
	if err != nil {
		t.Fatalf("Policy(%q): %v", policyID, err)
	}
	set := make(map[string]struct{}, len(pv.AllowedAccountIDs))
	for _, id := range pv.AllowedAccountIDs {
		set[id] = struct{}{}
	}
	return set
}

func assertAllowedSet(t *testing.T, w *Wallet, policyID string, want ...string) {
	t.Helper()
	got := allowedSet(t, w, policyID)
	if len(got) != len(want) {
		t.Fatalf("policy %s allowed set = %v, want %v", policyID, got, want)
	}
	for _, id := range want {
		if _, ok := got[id]; !ok {
			t.Fatalf("policy %s allowed set = %v, want %v (missing %q)", policyID, got, want, id)
		}
	}
}

// 保存成功后，调用方改写自己手里的原授权名单（整体替换为另一个已存在但
// 未获授权的账户，或本地移除已授权账户），钱包再次查询仍显示原授权集合。
func TestSavePolicyInputMutationDoesNotChangeSavedAllowList(t *testing.T) {
	w, _, spec := setupPolicyIsolation(t)
	if err := w.SavePolicy(spec); err != nil {
		t.Fatal(err)
	}
	assertAllowedSet(t, w, "p1", "u1", "u2")

	// 把本地入参中的已授权账户整体替换为另一个已存在但未获授权的账户。
	spec.AllowedAccountIDs[0] = "u3"
	assertAllowedSet(t, w, "p1", "u1", "u2")

	// 从本地名单中移除已授权账户。
	spec.AllowedAccountIDs = []string{"u2"}
	assertAllowedSet(t, w, "p1", "u1", "u2")

	// 清空本地名单同样不影响钱包保存的授权。
	spec.AllowedAccountIDs = nil
	assertAllowedSet(t, w, "p1", "u1", "u2")
}

// Policy 两次查询分别返回独立副本：改写其中一份的授权名单，另一份与随后
// 取得的新结果都不被污染。
func TestPolicyViewsAreIndependentCopies(t *testing.T) {
	w, _, spec := setupPolicyIsolation(t)
	if err := w.SavePolicy(spec); err != nil {
		t.Fatal(err)
	}

	first, err := w.Policy("p1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.Policy("p1")
	if err != nil {
		t.Fatal(err)
	}
	// 留底第二份的原始授权，修改第一份后用它核对第二份未被牵连。
	secondSnap := append([]string(nil), second.AllowedAccountIDs...)

	// 改写第一份：替换元素并追加一个未获授权账户。
	first.AllowedAccountIDs[0] = "u3"
	first.AllowedAccountIDs = append(first.AllowedAccountIDs, "u3")
	firstAfterMutation := append([]string(nil), first.AllowedAccountIDs...)
	// 第二份结果不受第一份改写影响。
	if !sameStringSet(second.AllowedAccountIDs, secondSnap) {
		t.Fatalf("second view = %v, want %v after mutating first", second.AllowedAccountIDs, secondSnap)
	}

	// 改写第二份：本地移除已授权账户。第一份（已被本地改写）也不被牵连，
	// 钱包随后取得的结果仍是原授权集合。
	second.AllowedAccountIDs = second.AllowedAccountIDs[:1]
	if !sameStringSet(first.AllowedAccountIDs, firstAfterMutation) {
		t.Fatalf("first view = %v, want %v after mutating second", first.AllowedAccountIDs, firstAfterMutation)
	}
	assertAllowedSet(t, w, "p1", "u1", "u2")

	// 新取得的结果同样是独立副本：改写后再次查询仍是原授权集合。
	fresh, err := w.Policy("p1")
	if err != nil {
		t.Fatal(err)
	}
	if !sameStringSet(fresh.AllowedAccountIDs, secondSnap) {
		t.Fatalf("fresh view = %v, want %v", fresh.AllowedAccountIDs, secondSnap)
	}
	fresh.AllowedAccountIDs[0] = "u3"
	assertAllowedSet(t, w, "p1", "u1", "u2")
}

// sameStringSet 以集合语义比较两组字符串是否相同（授权名单不约定顺序）。
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, s := range a {
		set[s] = struct{}{}
	}
	for _, s := range b {
		if _, ok := set[s]; !ok {
			return false
		}
	}
	return true
}

// 保存时重复列出同一账户仍按一份授权处理（集合语义），返回顺序不作约定。
func TestSavePolicyDeduplicatesRepeatedAllowedAccount(t *testing.T) {
	w, _, spec := setupPolicyIsolation(t)
	spec.AllowedAccountIDs = []string{"u1", "u1", "u2"}
	if err := w.SavePolicy(spec); err != nil {
		t.Fatal(err)
	}
	pv, err := w.Policy("p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.AllowedAccountIDs) != 2 {
		t.Fatalf("allowed list = %v, want 2 distinct accounts", pv.AllowedAccountIDs)
	}
	assertAllowedSet(t, w, "p1", "u1", "u2")

	// u1 只有一份授权：正常受理一笔申请，只产生一次费用预留。
	if req, err := w.Apply(isolationApply("u1", "s1", "dev1", "r-ok", 10)); err != nil || req.State != RequestReserved {
		t.Fatalf("authorized apply: view=%+v err=%v", req, err)
	}
	if n := countKind(w.AccountLedger("payer"), LedgerReserve); n != 1 {
		t.Fatalf("reserve entries = %d, want 1", n)
	}
	// 从未列入授权的 u3 仍被拒绝。
	if _, err := w.Apply(isolationApply("u3", "s3", "dev3", "r-deny", 5)); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("unauthorized apply err = %v, want ErrPolicyDenied", err)
	}
}

// 除了查看授权名单，实际受理结果也必须遵守钱包保存的原授权：本地名单的
// 任何改写都不能让未获授权账户被受理，也不能撤销已获授权账户的受理。
func TestApplyHonorsSavedAuthorizationDespiteLocalMutation(t *testing.T) {
	w, _, spec := setupPolicyIsolation(t)
	if err := w.SavePolicy(spec); err != nil {
		t.Fatal(err)
	}

	// 原获授权账户 u1 在策略有效、费用未超限、出资余额充足时正常受理，
	// 费用由原出资账户 payer 承担。
	okReq, err := w.Apply(isolationApply("u1", "s1", "dev1", "r-ok", 10))
	if err != nil {
		t.Fatalf("authorized apply: %v", err)
	}
	if okReq.State != RequestReserved || okReq.PayerAccountID != "payer" {
		t.Fatalf("accepted request = %+v", okReq)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 990, Reserved: 10}) {
		t.Fatalf("payer balance = %+v, want {990 10}", bal)
	}
	if pv, _ := w.Policy("p1"); pv.ReservedTotal != 10 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = %+v, want reserved 10", pv)
	}

	// 调用方手里的名单无论怎么改（保存入参与查询返回各改一次），都不能
	// 扩大授权：把 u3 写进本地副本后再以 u3 的有效会话申请。
	spec.AllowedAccountIDs = []string{"u3"}
	view, err := w.Policy("p1")
	if err != nil {
		t.Fatal(err)
	}
	view.AllowedAccountIDs = []string{"u1", "u2", "u3"}

	denied, err := w.Apply(isolationApply("u3", "s3", "dev3", "r-deny", 5))
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("unauthorized apply err = %v, want ErrPolicyDenied", err)
	}
	if !strings.Contains(err.Error(), "u3") || !strings.Contains(err.Error(), "p1") {
		t.Fatalf("denial reason should name account and policy: %v", err)
	}
	if denied.State != 0 || denied.RequestID != "" {
		t.Fatalf("denied apply must return zero view: %+v", denied)
	}

	// 拒绝不产生该申请的费用预留，也不动已受理请求、出资余额或策略累计。
	if _, err := w.Request("u3", "r-deny"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("denied request should not be created: %v", err)
	}
	if got := countKind(w.AccountLedger("payer"), LedgerReserve); got != 1 {
		t.Fatalf("reserve entries = %d, want only the accepted one", got)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 990, Reserved: 10}) {
		t.Fatalf("payer balance after denial = %+v, want unchanged {990 10}", bal)
	}
	if pv, _ := w.Policy("p1"); pv.ReservedTotal != 10 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals after denial = %+v, want unchanged", pv)
	}
	stillOK, err := w.Request("u1", "r-ok")
	if err != nil {
		t.Fatal(err)
	}
	if stillOK.State != RequestReserved || stillOK.PayerAccountID != "payer" || stillOK.EstimatedFee != 10 {
		t.Fatalf("accepted request changed by denial: %+v", stillOK)
	}

	// 拒绝留下明确的零金额拒绝原因，正常受理的预留记录仍归出资账户保留。
	var rejection LedgerEntry
	for _, e := range w.Ledger() {
		if e.Kind == LedgerRejection && e.RequestID == "r-deny" {
			rejection = e
		}
	}
	if rejection.RequestID == "" {
		t.Fatal("missing LedgerRejection for denied apply")
	}
	if rejection.AccountID != "u3" || rejection.Amount != 0 || !strings.Contains(rejection.Reason, "not allowed") {
		t.Fatalf("rejection entry = %+v", rejection)
	}
	reserve := w.AccountLedger("payer")
	if len(reserve) != 1 || reserve[0].Kind != LedgerReserve || reserve[0].AccountID != "payer" ||
		reserve[0].RequestID != "r-ok" || reserve[0].Amount != 10 {
		t.Fatalf("payer ledger = %+v, want the single accepted reserve", reserve)
	}

	// 本地副本改写不影响钱包保存的授权集合本身。
	assertAllowedSet(t, w, "p1", "u1", "u2")
}

// DeactivatePolicy 返回的策略视图同样是只读副本：调用方改写其中的授权
// 名单、停用标记或理由后，再查询仍看到首次停用的真实信息。
func TestDeactivatePolicyViewIsolation(t *testing.T) {
	w, _, spec := setupPolicyIsolation(t)
	if err := w.SavePolicy(spec); err != nil {
		t.Fatal(err)
	}

	dv, err := w.DeactivatePolicy("p1", "sa", "dev-owner", "  stop now  ")
	if err != nil {
		t.Fatal(err)
	}
	if !dv.Deactivated || dv.DeactivateReason != "stop now" || dv.DeactivatorAccountID != "payer" {
		t.Fatalf("deactivation view = %+v", dv)
	}
	deactivatedAt := dv.DeactivatedAt
	ledgerBefore := len(w.Ledger())

	// 改写调用方手里的停用视图：授权名单、停用标记、理由、执行账户与时间。
	dv.AllowedAccountIDs = []string{"u3"}
	dv.Deactivated = false
	dv.DeactivateReason = "locally changed"
	dv.DeactivatorAccountID = "u1"
	dv.DeactivatedAt = time.Time{}

	// 仅修改本地内容不新增账本记录、不改变余额；随后查询仍是首次停用信息。
	if len(w.Ledger()) != ledgerBefore {
		t.Fatal("mutating local deactivation view created ledger entries")
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 1000, Reserved: 0}) {
		t.Fatalf("balance changed by local mutation: %+v", bal)
	}
	got, err := w.Policy("p1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Deactivated {
		t.Fatal("saved policy appears reactivated after local flag flip")
	}
	if !got.DeactivatedAt.Equal(deactivatedAt) {
		t.Fatalf("deactivated at = %v, want %v", got.DeactivatedAt, deactivatedAt)
	}
	if got.DeactivatorAccountID != "payer" || got.DeactivateReason != "stop now" {
		t.Fatalf("first deactivation info rewritten: %+v", got)
	}
	assertAllowedSet(t, w, "p1", "u1", "u2")

	// 本地把停用标记改成未停用不能恢复受理：已获授权账户用有效会话提交
	// 新申请仍返回 ErrPolicyDeactivated，并记录首次停用理由。
	_, err = w.Apply(isolationApply("u1", "s1", "dev1", "r-after-stop", 10))
	if !errors.Is(err, ErrPolicyDeactivated) {
		t.Fatalf("apply after deactivation err = %v, want ErrPolicyDeactivated", err)
	}
	if !strings.Contains(err.Error(), "stop now") {
		t.Fatalf("error should carry first reason: %v", err)
	}
	if _, err := w.Request("u1", "r-after-stop"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("rejected-by-deactivation request should not be created: %v", err)
	}
	// 拒绝只增加一条零金额拒绝记录，不产生预留；停用留痕仍是首次那一条。
	if n := countKind(w.Ledger(), LedgerPolicyDeactivation); n != 1 {
		t.Fatalf("deactivation entries = %d, want 1", n)
	}
	if n := countKind(w.Ledger(), LedgerReserve); n != 0 {
		t.Fatalf("reserve entries after deactivated apply = %d, want 0", n)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 1000, Reserved: 0}) {
		t.Fatalf("balance after deactivated apply = %+v, want untouched", bal)
	}
	// 由正常停用产生的留痕仍按现有规则保留。
	var found bool
	for _, e := range w.Ledger() {
		if e.Kind == LedgerPolicyDeactivation && e.AccountID == "payer" && e.PolicyID == "p1" &&
			e.Amount == 0 && e.Reason == "stop now" {
			found = true
		}
	}
	if !found {
		t.Fatal("original deactivation ledger entry lost or rewritten")
	}
}
