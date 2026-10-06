package wallet

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// loadBackupAsStruct 将备份文本解析为可逐条修改的备份结构。
func loadBackupAsStruct(t *testing.T, data []byte) backupV1 {
	t.Helper()
	var b backupV1
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func remapStruct(t *testing.T, b *backupV1) []byte {
	t.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// assertLedgerAmountInvalid 恢复必须整体失败：包装 ErrBackupInvalid、不返回
// 钱包，错误信息必须指出记录在原账本中的位置（ledger[idx]）、记录类型名称、
// 保存的金额以及违反的金额要求片段。
func assertLedgerAmountInvalid(t *testing.T, data []byte, idx int, kind LedgerKind, amount int64, requirement string) {
	t.Helper()
	w2, err := Restore(data)
	if err == nil {
		if w2 != nil {
			t.Fatalf("kind %s entry with amount %d restored a wallet", kind, amount)
		}
		t.Fatalf("kind %s entry with amount %d restored without error", kind, amount)
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("kind %s entry with amount %d err = %v, want ErrBackupInvalid", kind, amount, err)
	}
	msg := err.Error()
	for _, want := range []string{
		fmt.Sprintf("ledger[%d]", idx),
		kind.String(),
		fmt.Sprintf("%d", amount),
		requirement,
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q must contain %q (position/kind/saved amount/requirement)", msg, want)
		}
	}
}

// TestRestoreRejectsPositiveAmountsOnStatusEntries 验证待审批、批准、拒绝、
// 取消、待审批过期、策略停用与预留超时等不发生资金变化的状态留痕，只要
// 带着正数金额就必须使整个备份失败——即使备份的账户余额、策略累计金额与
// 请求求和完全一致（这里直接取自真实钱包的导出，只改动账本金额）。
func TestRestoreRejectsPositiveAmountsOnStatusEntries(t *testing.T) {
	w, _ := backupSetup(t)
	good, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}

	statusKinds := []LedgerKind{
		LedgerRejection,
		LedgerPendingApproval,
		LedgerApproval,
		LedgerCancellation,
		LedgerExpiration,
		LedgerPolicyDeactivation,
		LedgerReservationExpiration,
	}
	for _, kind := range statusKinds {
		base := loadBackupAsStruct(t, good)
		var indices []int
		for i, e := range base.Ledger {
			if LedgerKind(e.Kind) == kind {
				indices = append(indices, i)
			}
		}
		if len(indices) == 0 {
			t.Fatalf("test backup has no %s entry to corrupt", kind)
		}
		for _, idx := range indices {
			kind, idx := kind, idx
			t.Run(kind.String()+fmt.Sprintf(" at %d", idx), func(t *testing.T) {
				b := loadBackupAsStruct(t, good)
				b.Ledger[idx].Amount = 7
				assertLedgerAmountInvalid(t, remapStruct(t, &b), idx, kind, 7, "zero amount")
			})
		}
	}
}

// TestRestoreRejectsReservationExpirationCarryingRefundAmount 验证预留超时
// 已有的全额退款不能再次写进那条零金额的预留超时状态记录：把状态记录的
// 金额改成与退款相同的预留全额，备份仍必须被拒绝。
func TestRestoreRejectsReservationExpirationCarryingRefundAmount(t *testing.T) {
	w, _ := backupSetup(t)
	good, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	b := loadBackupAsStruct(t, good)

	// 找到 resv-expired 请求对应的退款记录与超时状态记录。
	var refundAmount int64
	var statusIdx = -1
	for i, e := range b.Ledger {
		if e.RequestID != "resv-expired" {
			continue
		}
		switch LedgerKind(e.Kind) {
		case LedgerRefund:
			refundAmount = e.Amount
		case LedgerReservationExpiration:
			statusIdx = i
		}
	}
	if refundAmount != 5 || statusIdx < 0 {
		t.Fatalf("did not find resv-expired refund (%d) and status entry (%d)", refundAmount, statusIdx)
	}
	b.Ledger[statusIdx].Amount = refundAmount
	assertLedgerAmountInvalid(t, remapStruct(t, &b), statusIdx, LedgerReservationExpiration, refundAmount, "zero amount")
}

// TestRestoreRejectsPendingCancellationCarryingAmount 验证待审批取消从未
// 冻结费用，其取消留痕不能携带一笔资金金额。
func TestRestoreRejectsPendingCancellationCarryingAmount(t *testing.T) {
	w, _ := backupSetup(t)
	good, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	b := loadBackupAsStruct(t, good)

	idx := -1
	for i, e := range b.Ledger {
		if LedgerKind(e.Kind) == LedgerCancellation && e.RequestID == "cancel-pending" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("test backup has no pending-approval cancellation entry")
	}
	b.Ledger[idx].Amount = 20
	assertLedgerAmountInvalid(t, remapStruct(t, &b), idx, LedgerCancellation, 20, "zero amount")
}

// TestRestoreRejectsNonPositiveReserveAndRefund 验证预留与退款记录的金额
// 必须严格大于零：零金额同样无效，不能把零金额预留/退款当成合法留痕。
func TestRestoreRejectsNonPositiveReserveAndRefund(t *testing.T) {
	w, _ := backupSetup(t)
	good, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []LedgerKind{LedgerReserve, LedgerRefund} {
		kind := kind
		t.Run(kind.String()+" zero amount", func(t *testing.T) {
			b := loadBackupAsStruct(t, good)
			idx := -1
			for i, e := range b.Ledger {
				if LedgerKind(e.Kind) == kind {
					idx = i
					break
				}
			}
			if idx < 0 {
				t.Fatalf("test backup has no %s entry", kind)
			}
			b.Ledger[idx].Amount = 0
			assertLedgerAmountInvalid(t, remapStruct(t, &b), idx, kind, 0, "positive amount")
		})
	}
}

// TestRestoreRejectsNegativeAmountForEveryKind 验证任何账本类型都不接受
// 负金额。
func TestRestoreRejectsNegativeAmountForEveryKind(t *testing.T) {
	w, _ := backupSetup(t)
	good, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	allKinds := []LedgerKind{
		LedgerReserve,
		LedgerSettle,
		LedgerRefund,
		LedgerRejection,
		LedgerPendingApproval,
		LedgerApproval,
		LedgerCancellation,
		LedgerExpiration,
		LedgerPolicyDeactivation,
		LedgerReservationExpiration,
	}
	for _, kind := range allKinds {
		kind := kind
		t.Run(kind.String()+" negative", func(t *testing.T) {
			b := loadBackupAsStruct(t, good)
			idx := -1
			for i, e := range b.Ledger {
				if LedgerKind(e.Kind) == kind {
					idx = i
					break
				}
			}
			if idx < 0 {
				t.Fatalf("test backup has no %s entry", kind)
			}
			b.Ledger[idx].Amount = -1
			assertLedgerAmountInvalid(t, remapStruct(t, &b), idx, kind, -1, "must not be negative")
		})
	}
}

// TestZeroFeeSettlementLedgerRoundTrips 端到端验证合法的零费用结算：
// 零金额扣减记录与正数全额退款记录原样保留，恢复后的请求仍是已结算
// （不能把零金额扣减当成没有发生结算），结算时间保留，余额全额退回。
func TestZeroFeeSettlementLedgerRoundTrips(t *testing.T) {
	w, c := newTestWallet()
	t0 := c.t
	mustAccount(t, w, "payer", 100)
	mustAccount(t, w, "u1", 0)
	if _, err := w.CreateSession("s1", "u1", "dev1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := w.SavePolicy(PolicySpec{
		ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "charge", Payee: "shop",
		StartsAt: t0.Add(-time.Hour), EndsAt: t0.Add(time.Hour),
		MaxPerRequest: 100, MaxTotal: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(RequestInput{
		PolicyID: "p", RequestID: "rz", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 5,
	}); err != nil {
		t.Fatal(err)
	}
	if r, err := w.Settle("u1", "rz", 0); err != nil || r.State != RequestSettled {
		t.Fatalf("zero-fee settle: state=%v err=%v", r.State, err)
	}
	srcLedger := w.Ledger()
	if len(srcLedger) != 3 ||
		srcLedger[0].Kind != LedgerReserve || srcLedger[0].Amount != 5 ||
		srcLedger[1].Kind != LedgerSettle || srcLedger[1].Amount != 0 ||
		srcLedger[2].Kind != LedgerRefund || srcLedger[2].Amount != 5 {
		t.Fatalf("source ledger = %+v, want reserve(5) + zero settle(0) + refund(5)", srcLedger)
	}

	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, t0)
	if err != nil {
		t.Fatalf("zero-fee settlement backup must restore: %v", err)
	}
	r, err := w2.Request("u1", "rz")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != RequestSettled || r.ActualFee != 0 || !r.SettledAt.Equal(srcLedger[1].At) {
		t.Fatalf("restored zero-fee request = %+v, want settled with fee 0 and original settled_at", r)
	}
	got := w2.Ledger()
	if len(got) != len(srcLedger) {
		t.Fatalf("restored ledger len = %d, want %d", len(got), len(srcLedger))
	}
	for i := range srcLedger {
		if got[i] != srcLedger[i] {
			t.Fatalf("ledger[%d] = %+v, want %+v", i, got[i], srcLedger[i])
		}
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want full refund {100 0}", bal)
	}
	if pv, _ := w2.Policy("p"); pv.ReservedTotal != 0 || pv.SpentTotal != 0 {
		t.Fatalf("policy totals = reserved %d spent %d, want 0/0", pv.ReservedTotal, pv.SpentTotal)
	}
}

// buildLedgerOnlyBackup 构造只含账本（及一个账户）的最小备份，便于验证
// 金额规则独立于账户/请求引用。
func buildLedgerOnlyBackup(t *testing.T, entry ledgerEntryBackupV1) []byte {
	t.Helper()
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0),
		Accounts:   []accountBackupV1{{ID: "payer", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0)}},
		Sessions:   []sessionBackupV1{},
		Policies:   []policyBackupV1{},
		Requests:   []requestBackupV1{},
		Ledger:     []ledgerEntryBackupV1{entry},
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestOrphanRejectionEntryAmountRule 验证未被受理申请留下的拒绝记录不要求
// 账户或请求存在，但金额规则照样适用：零金额（含编号缺失、关联对象不
// 存在）继续按原规则恢复；正数即使没有任何关联请求也必须拒绝，不能因
// 余额核对无歧义而放行。
func TestOrphanRejectionEntryAmountRule(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// 零金额、编号缺失或关联对象不存在：照常恢复。
	for _, e := range []ledgerEntryBackupV1{
		{Kind: int(LedgerRejection), Reason: "missing required fields", At: timeJSON(t0)},
		{Kind: int(LedgerRejection), AccountID: "ghost", RequestID: "no-such-request",
			Reason: "wallet: account not found: ghost", At: timeJSON(t0)},
	} {
		data := buildLedgerOnlyBackup(t, e)
		w2, err := restoreAt(data, t0)
		if err != nil {
			t.Fatalf("zero-amount orphan rejection must restore: %v", err)
		}
		got := w2.Ledger()
		if len(got) != 1 || got[0].Kind != LedgerRejection || got[0].Amount != 0 {
			t.Fatalf("restored orphan rejection = %+v", got)
		}
	}

	// 正数金额：即使没有任何账户/请求关联，也必须整体拒绝。
	bad := ledgerEntryBackupV1{
		Kind: int(LedgerRejection), AccountID: "ghost", RequestID: "no-such-request",
		Amount: 9, Reason: "wallet: account not found: ghost", At: timeJSON(t0),
	}
	assertLedgerAmountInvalid(t, buildLedgerOnlyBackup(t, bad), 0, LedgerRejection, 9, "zero amount")
}

// TestRestoreEmptyLedgerStillWorks 显式验证空账本继续可恢复，不因金额
// 校验增添任何记录。
func TestRestoreEmptyLedgerStillWorks(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0),
		Accounts:   []accountBackupV1{{ID: "payer", Available: 3, Reserved: 0, CreatedAt: timeJSON(t0)}},
		Sessions:   []sessionBackupV1{},
		Policies:   []policyBackupV1{},
		Requests:   []requestBackupV1{},
		Ledger:     []ledgerEntryBackupV1{},
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, t0)
	if err != nil {
		t.Fatalf("empty ledger restore: %v", err)
	}
	if got := w2.Ledger(); len(got) != 0 {
		t.Fatalf("empty ledger restored %d entries: %+v", len(got), got)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 3, Reserved: 0}) {
		t.Fatalf("balance = %+v", bal)
	}
}
