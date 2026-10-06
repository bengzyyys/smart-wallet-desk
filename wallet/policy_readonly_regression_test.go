package wallet

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// setupPolicyIsolation 构造一条费用由出资账户承担、允许多个使用账户申请的
// 策略（p-isolate：出资账户 payer 1000，允许 u1/u2；另建一个存在但未获授权
// 的账户 u3），全部使用账户与出资账户各持有绑定当前设备的有效会话。
// 策略在当前时间窗内有效，费用与额度余量充足，申请都会直达预留路径。
func setupPolicyIsolation(t *testing.T) (*Wallet, *clock) {
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
	if _, err := w.CreateSession("sa", "payer", "dev-approve", c.t.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	spec := validPolicy(c)
	spec.ID = "p-isolate"
	// 单次 30、累计 100、出资余额 1000：本组测试中的申请不会触及费用或额度上限。
	spec.MaxPerRequest = 30
	spec.MaxTotal = 100
	if err := w.SavePolicy(spec); err != nil {
		t.Fatal(err)
	}
	return w, c
}

// isolationApply 构造 setupPolicyIsolation 策略下使用账户 u1 的一笔有效申请。
func isolationApply(rid string) RequestInput {
	return RequestInput{
		PolicyID:     "p-isolate",
		RequestID:    rid,
		AccountID:    "u1",
		SessionID:    "s1",
		DeviceID:     "dev1",
		Operation:    "charge",
		Payee:        "shop",
		EstimatedFee: 20,
	}
}

// allowedSet 把授权名单归一成集合判断：功能没有约定返回顺序，按集合比较。
func allowedSet(ids []string) map[string]struct{} {
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}

func assertAllowedSet(t *testing.T, view PolicyView, want map[string]struct{}, context string) {
	t.Helper()
	if got := allowedSet(view.AllowedAccountIDs); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: allowed accounts = %v, want %v", context, got, want)
	}
}

// TestSavePolicyCopiesCallerAllowedList 验证保存入口复制调用方名单：保存成功
// 后修改调用方仍持有的原授权名单（替换或移除已授权账户），钱包中保存的授权
// 集合不能被改变。
func TestSavePolicyCopiesCallerAllowedList(t *testing.T) {
	w, c := newTestWallet()
	mustAccount(t, w, "payer", 1000)
	mustAccount(t, w, "u1", 0)
	mustAccount(t, w, "u2", 0)
	mustAccount(t, w, "u3", 0)
	want := map[string]struct{}{"u1": {}, "u2": {}}

	// 保存时同一账户重复列出仍按一份授权处理。
	spec := validPolicy(c)
	spec.ID = "p-copy"
	spec.AllowedAccountIDs = []string{"u1", "u2", "u1"}
	spec.MaxTotal = 100
	if err := w.SavePolicy(spec); err != nil {
		t.Fatal(err)
	}
	pv, err := w.Policy("p-copy")
	if err != nil {
		t.Fatal(err)
	}
	assertAllowedSet(t, pv, want, "dedup at save")

	// 把已授权账户替换成另一个已存在但未获授权的账户。
	spec.AllowedAccountIDs[0] = "u3"
	pv, err = w.Policy("p-copy")
	if err != nil {
		t.Fatal(err)
	}
	assertAllowedSet(t, pv, want, "after replacing an authorized account in caller spec")

	// 直接清空调用方持有的名单。
	spec.AllowedAccountIDs = spec.AllowedAccountIDs[:0]
	pv, err = w.Policy("p-copy")
	if err != nil {
		t.Fatal(err)
	}
	assertAllowedSet(t, pv, want, "after clearing caller spec")
}

// TestPolicyViewIsIndependentCopy 验证查询结果互不共享底层存储：修改一份返回
// 视图的授权名单（就地改写元素、缩容后追加、整体替换）不能改变另一份，也不能
// 污染随后取得的结果。
func TestPolicyViewIsIndependentCopy(t *testing.T) {
	w, _ := setupPolicyIsolation(t)
	want := map[string]struct{}{"u1": {}, "u2": {}}

	first, err := w.Policy("p-isolate")
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.Policy("p-isolate")
	if err != nil {
		t.Fatal(err)
	}

	// 就地改写第一份名单中的元素：若与第二份或钱包存储共享底层数组，
	// 这一写入会直接污染另一份结果。
	if len(first.AllowedAccountIDs) != 2 {
		t.Fatalf("setup expects 2 allowed accounts, got %v", first.AllowedAccountIDs)
	}
	first.AllowedAccountIDs[0] = "u3"
	first.AllowedAccountIDs[1] = "u3"
	assertAllowedSet(t, second, want, "the other held query result after in-place edit of first")

	// 缩容后向第一份追加，同样不能串到另一份。
	first.AllowedAccountIDs = append(first.AllowedAccountIDs[:1], "injected")
	assertAllowedSet(t, second, want, "the other held query result after append into first")

	// 把第二份名单整个替换为未授权账户（本地头部替换）。
	second.AllowedAccountIDs = []string{"u3"}

	// 随后取得的结果仍是钱包保存的原授权集合。
	fresh, err := w.Policy("p-isolate")
	if err != nil {
		t.Fatal(err)
	}
	assertAllowedSet(t, fresh, want, "fresh query after mutating held views")
}

// TestCallerMutationCannotBroadenAuthorization 是“只读数据”约定的端到端回归：
// 无论调用方修改保存时持有的原名单还是查询返回的名单，钱包的实际受理结果都
// 必须遵守保存时的原授权——而不是被本地副本的改动扩大或缩小。
func TestCallerMutationCannotBroadenAuthorization(t *testing.T) {
	w, c := setupPolicyIsolation(t)
	want := map[string]struct{}{"u1": {}, "u2": {}}

	// 保存后修改调用方仍持有的原授权名单：用已存在但未获授权的 u3 替换 u1。
	spec := validPolicy(c)
	spec.ID = "p-isolate2"
	spec.MaxPerRequest = 30
	spec.MaxTotal = 100
	spec.AllowedAccountIDs = []string{"u1", "u2"}
	if err := w.SavePolicy(spec); err != nil {
		t.Fatal(err)
	}
	spec.AllowedAccountIDs[0] = "u3"

	// 再修改查询返回的名单：移除全部已授权账户。
	view, err := w.Policy("p-isolate2")
	if err != nil {
		t.Fatal(err)
	}
	view.AllowedAccountIDs = view.AllowedAccountIDs[:0]

	// 钱包再次查询仍显示原授权集合。
	pv, err := w.Policy("p-isolate2")
	if err != nil {
		t.Fatal(err)
	}
	assertAllowedSet(t, pv, want, "stored policy after caller list mutations")

	// 原获授权账户 u1 在策略有效、费用与额度充足、出资余额充足时，
	// 用绑定当前设备的有效会话提交申请，仍按策略预留费用。
	in := RequestInput{
		PolicyID:     "p-isolate2",
		RequestID:    "r-allowed",
		AccountID:    "u1",
		SessionID:    "s1",
		DeviceID:     "dev1",
		Operation:    "charge",
		Payee:        "shop",
		EstimatedFee: 20,
	}
	req, err := w.Apply(in)
	if err != nil {
		t.Fatalf("authorized account apply: %v", err)
	}
	if req.State != RequestReserved || req.PayerAccountID != "payer" {
		t.Fatalf("authorized apply = %+v, want reserved charged to payer", req)
	}
	// 费用由原出资账户承担：可用 1000-20、预留 20。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 980, Reserved: 20}) {
		t.Fatalf("payer balance = %+v, want {980 20}", bal)
	}
	if pv, _ := w.Policy("p-isolate2"); pv.ReservedTotal != 20 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 20/0", pv.ReservedTotal, pv.SpentTotal)
	}

	// 未获授权账户 u3 用各自绑定当前设备的有效会话提交新申请：
	// 仍返回 ErrPolicyDenied，并留下明确拒绝原因。
	denied := in
	denied.RequestID = "r-denied"
	denied.AccountID = "u3"
	denied.SessionID = "s3"
	denied.DeviceID = "dev3"
	_, err = w.Apply(denied)
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("unauthorized apply err = %v, want ErrPolicyDenied", err)
	}
	if !strings.Contains(err.Error(), "u3") || !strings.Contains(err.Error(), "p-isolate2") {
		t.Fatalf("denial error should name account and policy: %v", err)
	}

	// 拒绝不产生该申请的费用预留：没有为它新增请求，账本只留一条零金额拒绝。
	if _, err := w.Request("u3", "r-denied"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("denied request should not be stored, err = %v", err)
	}
	var rejections []LedgerEntry
	for _, e := range w.Ledger() {
		if e.Kind == LedgerRejection && e.RequestID == "r-denied" {
			rejections = append(rejections, e)
		}
	}
	if len(rejections) != 1 {
		t.Fatalf("denial ledger entries = %d, want exactly 1", len(rejections))
	}
	if rejections[0].AccountID != "u3" || rejections[0].Amount != 0 {
		t.Fatalf("denial ledger entry = %+v, want u3 zero-amount rejection", rejections[0])
	}
	// 拒绝原因明确指出该账户不在策略授权名单中。
	if !strings.Contains(rejections[0].Reason, "u3") || !strings.Contains(rejections[0].Reason, "not allowed") {
		t.Fatalf("denial ledger reason = %q, want it to name u3 as not allowed", rejections[0].Reason)
	}
	// 拒绝不能误改已受理请求的状态、出资账户余额或策略累计金额。
	if got, _ := w.Request("u1", "r-allowed"); got.State != RequestReserved {
		t.Fatalf("accepted request state changed to %v", got.State)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 980, Reserved: 20}) {
		t.Fatalf("payer balance changed on denial: %+v", bal)
	}
	if pv, _ := w.Policy("p-isolate2"); pv.ReservedTotal != 20 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals changed on denial: reserved %d spent %d", pv.ReservedTotal, pv.SpentTotal)
	}
	// 被拒绝的使用账户自身不发生资金变动。
	if led := w.AccountLedger("u3"); len(led) != 1 || led[0].Kind != LedgerRejection {
		t.Fatalf("u3 ledger = %+v, want only a zero-amount rejection", led)
	}
}

// TestCallerMutationOfReturnedListCannotRemoveAuthorization 覆盖“从本地名单移除
// 已授权账户”这一改动方式：查询结果里的删除不能撤销钱包保存的授权，实际受理
// 仍按原授权集合放行。
func TestCallerMutationOfReturnedListCannotRemoveAuthorization(t *testing.T) {
	w, _ := setupPolicyIsolation(t)

	view, err := w.Policy("p-isolate")
	if err != nil {
		t.Fatal(err)
	}
	kept := view.AllowedAccountIDs[0]
	// 在本地副本中删除保留下来的那个授权账户之外的全部账户。
	view.AllowedAccountIDs = []string{kept}

	// 被本地删除的账户若原本已授权，仍可成功申请；以 u1、u2 分别验证。
	applyAs := func(account, session, device, rid string) {
		in := isolationApply(rid)
		in.AccountID = account
		in.SessionID = session
		in.DeviceID = device
		req, err := w.Apply(in)
		if err != nil {
			t.Fatalf("apply as %s after local list edit: %v", account, err)
		}
		if req.State != RequestReserved {
			t.Fatalf("apply as %s state = %v, want reserved", account, req.State)
		}
	}
	applyAs("u1", "s1", "dev1", "r-u1")
	applyAs("u2", "s2", "dev2", "r-u2")

	// 两笔预留都由出资账户承担，累计 40。
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 960, Reserved: 40}) {
		t.Fatalf("payer balance = %+v, want {960 40}", bal)
	}
	pv, _ := w.Policy("p-isolate")
	assertAllowedSet(t, pv, map[string]struct{}{"u1": {}, "u2": {}}, "stored list after local removal")
	if pv.ReservedTotal != 40 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 40/0", pv.ReservedTotal, pv.SpentTotal)
	}

	// 未获授权的 u3 始终被拒绝，不会因为任何本地编辑而获得授权。
	in := isolationApply("r-u3")
	in.AccountID, in.SessionID, in.DeviceID = "u3", "s3", "dev3"
	if _, err := w.Apply(in); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("u3 apply err = %v, want ErrPolicyDenied", err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 960, Reserved: 40}) {
		t.Fatalf("payer balance changed on u3 denial: %+v", bal)
	}
}

// TestDeactivatePolicyViewIsImmutableSnapshot 验证停用操作返回的策略视图是只读
// 快照：调用方改写其中的授权名单、停用标记或理由后，再查询仍看到首次停用的
// 真实信息。
func TestDeactivatePolicyViewIsImmutableSnapshot(t *testing.T) {
	w, c := setupPolicyIsolation(t)

	deactView, err := w.DeactivatePolicy("p-isolate", "sa", "dev-approve", "  first stop reason  ")
	if err != nil {
		t.Fatal(err)
	}
	firstAt := deactView.DeactivatedAt

	// 改写返回视图：清空授权名单、把停用标记改回未停用、篡改理由与停用时间。
	deactView.AllowedAccountIDs = nil
	deactView.Deactivated = false
	deactView.DeactivateReason = "tampered reason"
	deactView.DeactivatorAccountID = "u1"
	deactView.DeactivatedAt = time.Time{}

	got, err := w.Policy("p-isolate")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Deactivated {
		t.Fatal("stored policy lost deactivated flag after returned view was edited")
	}
	if !got.DeactivatedAt.Equal(firstAt) || !got.DeactivatedAt.Equal(c.t) {
		t.Fatalf("deactivated at = %v, want first deactivation %v", got.DeactivatedAt, firstAt)
	}
	if got.DeactivatorAccountID != "payer" {
		t.Fatalf("deactivator = %q, want payer", got.DeactivatorAccountID)
	}
	if got.DeactivateReason != "first stop reason" {
		t.Fatalf("reason = %q, want first stop reason", got.DeactivateReason)
	}
	assertAllowedSet(t, got, map[string]struct{}{"u1": {}, "u2": {}}, "stored allowed list after returned view edit")

	// 再次停用返回的视图同样可被随意改写，不影响钱包中的首次停用信息。
	again, err := w.DeactivatePolicy("p-isolate", "sa", "dev-approve", "first stop reason")
	if err != nil {
		t.Fatalf("idempotent deactivate: %v", err)
	}
	again.Deactivated = false
	again.DeactivateReason = "other"
	got, _ = w.Policy("p-isolate")
	if !got.Deactivated || got.DeactivateReason != "first stop reason" {
		t.Fatalf("first deactivation info changed: %+v", got)
	}
}

// TestDeactivatedPolicyStaysRejectedDespiteLocalFlagEdit 验证停用后的实际受理：
// 调用方在本地把停用标记改为未停用不能恢复受理；有效会话的新申请继续返回
// ErrPolicyDeactivated 并记录拒绝原因。
func TestDeactivatedPolicyStaysRejectedDespiteLocalFlagEdit(t *testing.T) {
	w, _ := setupPolicyIsolation(t)

	// 停用前先受理一笔，确认已受理请求与出资账户余额在停用和后续拒绝中都不被改写。
	accepted, err := w.Apply(isolationApply("r-before"))
	if err != nil {
		t.Fatal(err)
	}
	if accepted.State != RequestReserved {
		t.Fatalf("accepted state = %v, want reserved", accepted.State)
	}

	deactView, err := w.DeactivatePolicy("p-isolate", "sa", "dev-approve", "fraud suspected")
	if err != nil {
		t.Fatal(err)
	}
	ledgerAfterDeact := len(w.Ledger())

	// 调用方本地把停用标记改为未停用、改写理由：纯本地操作，不产生任何副作用。
	deactView.Deactivated = false
	deactView.DeactivateReason = "locally re-enabled"
	deactView.AllowedAccountIDs = append(deactView.AllowedAccountIDs, "u3")
	if got := len(w.Ledger()); got != ledgerAfterDeact {
		t.Fatalf("editing returned view added ledger entries: %d vs %d", got, ledgerAfterDeact)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 980, Reserved: 20}) {
		t.Fatalf("balance changed while editing returned view: %+v", bal)
	}

	// 已停用策略在有效会话提交新申请：继续返回 ErrPolicyDeactivated 并记录拒绝原因。
	_, err = w.Apply(isolationApply("r-after"))
	if !errors.Is(err, ErrPolicyDeactivated) {
		t.Fatalf("apply after deactivation err = %v, want ErrPolicyDeactivated", err)
	}
	if !strings.Contains(err.Error(), "fraud suspected") {
		t.Fatalf("error should carry first reason: %v", err)
	}
	// 不能因本地把停用标记改为未停用而创建请求。
	if _, err := w.Request("u1", "r-after"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("post-deactivation request should not exist, err = %v", err)
	}
	var rejections []LedgerEntry
	for _, e := range w.Ledger() {
		if e.Kind == LedgerRejection && e.RequestID == "r-after" {
			rejections = append(rejections, e)
		}
	}
	if len(rejections) != 1 {
		t.Fatalf("post-deactivation rejection entries = %d, want 1", len(rejections))
	}
	if !strings.Contains(rejections[0].Reason, "deactivat") || !strings.Contains(rejections[0].Reason, "fraud suspected") {
		t.Fatalf("rejection reason = %q, want deactivation explanation with first reason", rejections[0].Reason)
	}

	// 已受理请求的状态、出资账户余额、策略累计金额都不被拒绝误改。
	if got, _ := w.Request("u1", "r-before"); got.State != RequestReserved {
		t.Fatalf("previously accepted request state = %v, want still reserved", got.State)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 980, Reserved: 20}) {
		t.Fatalf("payer balance = %+v, want unchanged {980 20}", bal)
	}
	pv, _ := w.Policy("p-isolate")
	if !pv.Deactivated || pv.ReservedTotal != 20 || pv.SpentTotal != 0 {
		t.Fatalf("policy view = %+v, want deactivated with reserved 20 / spent 0", pv)
	}

	// 本地编辑不会恢复授权：另一个未授权账户同样被停用错误（而非授权错误）拦截。
	in := isolationApply("r-u3-after")
	in.AccountID, in.SessionID, in.DeviceID = "u3", "s3", "dev3"
	if _, err := w.Apply(in); !errors.Is(err, ErrPolicyDeactivated) {
		t.Fatalf("u3 apply err = %v, want ErrPolicyDeactivated (deactivation checked first)", err)
	}

	// 停用留痕只有首次停用一条；编辑本地视图不新增账本记录。
	if n := countKind(w.Ledger(), LedgerPolicyDeactivation); n != 1 {
		t.Fatalf("deactivation ledger entries = %d, want exactly 1", n)
	}
	// 已预留请求仍可正常结算，费用仍归原出资账户承担。
	if _, err := w.Settle("u1", "r-before", 20); err != nil {
		t.Fatalf("settle pre-deactivation reservation: %v", err)
	}
	if bal, _ := w.Balance("payer"); bal != (Balances{Available: 980, Reserved: 0}) {
		t.Fatalf("payer balance after settle = %+v, want {980 0}", bal)
	}
	if pv, _ := w.Policy("p-isolate"); pv.SpentTotal != 20 {
		t.Fatalf("spent total = %d, want 20", pv.SpentTotal)
	}
}

// TestMutatingPolicyViewsNeverCreatesRecords 直接保证“修改本地策略内容”这一动作
// 本身是纯本地的：不新增账本记录、不改变账户余额、不创建请求；由正常申请或
// 停用产生的记录仍按现有规则保留。
func TestMutatingPolicyViewsNeverCreatesRecords(t *testing.T) {
	w, _ := setupPolicyIsolation(t)

	// 一笔正常申请产生一条预留记录。
	if _, err := w.Apply(isolationApply("r-ok")); err != nil {
		t.Fatal(err)
	}
	// 一次停用产生一条停用留痕。
	if _, err := w.DeactivatePolicy("p-isolate", "sa", "dev-approve", "stop"); err != nil {
		t.Fatal(err)
	}
	// 停用后一笔被拒申请产生一条拒绝记录。
	if _, err := w.Apply(isolationApply("r-rejected")); !errors.Is(err, ErrPolicyDeactivated) {
		t.Fatal("post-deactivation apply should be denied")
	}

	snapshot := w.Ledger()
	baseline := len(snapshot)
	balBefore, _ := w.Balance("payer")

	// 对多份本地策略视图（查询结果与停用结果）做各种字段级改写。
	for i := 0; i < 3; i++ {
		v, err := w.Policy("p-isolate")
		if err != nil {
			t.Fatal(err)
		}
		v.AllowedAccountIDs = append(v.AllowedAccountIDs, "u3", "u1")
		sort.Strings(v.AllowedAccountIDs)
		v.AllowedAccountIDs[0] = "rewritten"
		v.Deactivated = !v.Deactivated
		v.DeactivateReason = "rewritten"
		v.DeactivatorAccountID = "rewritten"
		v.ReservedTotal = 999
		v.SpentTotal = 999
		v.MaxTotal = 1
	}
	dv, err := w.DeactivatePolicy("p-isolate", "sa", "dev-approve", "stop")
	if err != nil {
		t.Fatal(err)
	}
	dv.Deactivated = false
	dv.DeactivateReason = ""
	dv.AllowedAccountIDs = nil

	// 账本：长度与逐条内容都与编辑前完全一致。
	after := w.Ledger()
	if len(after) != baseline {
		t.Fatalf("ledger length changed: %d vs %d", len(after), baseline)
	}
	for i := range snapshot {
		if !reflect.DeepEqual(snapshot[i], after[i]) {
			t.Fatalf("ledger entry %d changed: %+v vs %+v", i, snapshot[i], after[i])
		}
	}
	// 余额不变，编辑本地视图没有创建或受理任何请求。
	if bal, _ := w.Balance("payer"); bal != balBefore {
		t.Fatalf("balance changed: %+v vs %+v", bal, balBefore)
	}
	if _, err := w.Request("u3", "r-ok"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("view editing must not create requests, err = %v", err)
	}
	// 由正常申请与停用产生的既有记录仍按原规则保留，可被查询到。
	kinds := map[LedgerKind]int{}
	for _, e := range after {
		kinds[e.Kind]++
	}
	if kinds[LedgerReserve] != 1 || kinds[LedgerPolicyDeactivation] != 1 || kinds[LedgerRejection] < 1 {
		t.Fatalf("existing records not preserved: %+v", kinds)
	}
}
