package wallet

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// firstLedgerIndex 返回备份账本中第一条指定类型记录的下标；该类型必须存在。
func firstLedgerIndex(t *testing.T, b *backupV1, kind LedgerKind) int {
	t.Helper()
	for i, e := range b.Ledger {
		if LedgerKind(e.Kind) == kind {
			return i
		}
	}
	t.Fatalf("backup ledger has no %s entry", ledgerKindName(kind))
	return -1
}

// mutateLedgerBackup 解析一份完整备份文本，对账本中的指定记录执行 mutate，
// 再序列化回 JSON。只动账本，不碰账户、策略与请求，因此账户余额、策略累计
// 金额与请求求和仍完全一致——用于验证金额核对不能被一致的求和掩盖。
func mutateLedgerBackup(t *testing.T, data []byte, mutate func(*backupV1)) []byte {
	t.Helper()
	var b backupV1
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	mutate(&b)
	out, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// assertLedgerAmountInvalid 恢复必须整体失败、包装 ErrBackupInvalid，且不返回
// 钱包；错误信息必须点名问题记录在原账本中的位置、记录类型、保存的金额与
// 违反的金额要求。
func assertLedgerAmountInvalid(t *testing.T, data []byte, idx int, kind LedgerKind, amount int64, requirement string) {
	t.Helper()
	w2, err := Restore(data)
	if err == nil {
		if w2 != nil {
			t.Fatalf("invalid ledger amount restored a wallet: %+v", w2)
		}
		t.Fatal("invalid ledger amount restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"ledger[" + strconv.Itoa(idx) + "]",
		ledgerKindName(kind),
		strconv.FormatInt(amount, 10),
		requirement,
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q must contain %q (position, kind, amount, requirement)", msg, want)
		}
	}
}

// TestRestoreRejectsLedgerEntryAmountMismatch 基于一份账户余额、策略累计金额
// 与请求求和完全一致的正常备份，逐条验证每条账本记录自身的金额规则：
//   - 预留、退款金额必须严格大于零：零金额与负金额都拒绝；
//   - 扣减允许零金额但拒绝负金额；
//   - 待审批、批准、拒绝、待审批取消、待审批过期、策略停用、预留超时状态等
//     所有状态留痕金额必须为零：正数与负数都拒绝。
//
// 损坏只作用于单条账本记录，其余数据（含余额与各项求和）原封不动。
func TestRestoreRejectsLedgerEntryAmountMismatch(t *testing.T) {
	w, _ := backupSetup(t)
	good, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}

	// 状态留痕类型：正常备份中金额为零，带上正数即非法（负数同样非法）。
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
		kind := kind
		t.Run(ledgerKindName(kind)+" with positive amount", func(t *testing.T) {
			var idx int
			data := mutateLedgerBackup(t, good, func(b *backupV1) {
				idx = firstLedgerIndex(t, b, kind)
				b.Ledger[idx].Amount = 7
			})
			assertLedgerAmountInvalid(t, data, idx, kind, 7, "must be zero")
		})
		t.Run(ledgerKindName(kind)+" with negative amount", func(t *testing.T) {
			var idx int
			data := mutateLedgerBackup(t, good, func(b *backupV1) {
				idx = firstLedgerIndex(t, b, kind)
				b.Ledger[idx].Amount = -3
			})
			assertLedgerAmountInvalid(t, data, idx, kind, -3, "must be zero")
		})
	}

	// 预留与退款：必须严格为正。
	for _, kind := range []LedgerKind{LedgerReserve, LedgerRefund} {
		kind := kind
		t.Run(ledgerKindName(kind)+" with zero amount", func(t *testing.T) {
			var idx int
			data := mutateLedgerBackup(t, good, func(b *backupV1) {
				idx = firstLedgerIndex(t, b, kind)
				b.Ledger[idx].Amount = 0
			})
			assertLedgerAmountInvalid(t, data, idx, kind, 0, "must be positive")
		})
		t.Run(ledgerKindName(kind)+" with negative amount", func(t *testing.T) {
			var idx int
			data := mutateLedgerBackup(t, good, func(b *backupV1) {
				idx = firstLedgerIndex(t, b, kind)
				b.Ledger[idx].Amount = -9
			})
			assertLedgerAmountInvalid(t, data, idx, kind, -9, "must be positive")
		})
	}

	// 扣减：允许零（合法零费用结算），但负数仍拒绝。
	t.Run("settle with negative amount", func(t *testing.T) {
		var idx int
		data := mutateLedgerBackup(t, good, func(b *backupV1) {
			idx = firstLedgerIndex(t, b, LedgerSettle)
			b.Ledger[idx].Amount = -1
		})
		assertLedgerAmountInvalid(t, data, idx, LedgerSettle, -1, "zero or positive")
	})
}

// TestRestoreRejectsTimeoutStatusEntryCarryingRefundAmount 专门锁定预留超时：
// 正常流程对一笔预留超时记两条记录——正数全额退款（LedgerRefund）与零金额
// 超时状态（LedgerReservationExpiration）。把退款金额再抄进状态记录必须拒绝，
// 即使账户余额与请求求和不变。
func TestRestoreRejectsTimeoutStatusEntryCarryingRefundAmount(t *testing.T) {
	w, c := backupSetup(t)
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	var idx int
	var amount int64
	corrupt := mutateLedgerBackup(t, data, func(b *backupV1) {
		refundIdx := -1
		statusIdx := -1
		for i, e := range b.Ledger {
			if LedgerKind(e.Kind) == LedgerRefund && e.RequestID == "resv-expired" {
				refundIdx = i
			}
			if LedgerKind(e.Kind) == LedgerReservationExpiration && e.RequestID == "resv-expired" {
				statusIdx = i
			}
		}
		if refundIdx < 0 || statusIdx < 0 {
			t.Fatalf("timeout refund/status entries not found: %d %d", refundIdx, statusIdx)
		}
		idx = statusIdx
		amount = b.Ledger[refundIdx].Amount
		b.Ledger[statusIdx].Amount = amount
	})
	assertLedgerAmountInvalid(t, corrupt, idx, LedgerReservationExpiration, amount, "must be zero")

	// 同一备份未经篡改：正数退款 + 零金额状态记录正常恢复，顺序、时间保留。
	w2, err := restoreAt(data, c.t)
	if err != nil {
		t.Fatalf("legal timeout ledger must restore: %v", err)
	}
	var sawRefund, sawStatus bool
	for _, e := range w2.Ledger() {
		if e.RequestID != "resv-expired" {
			continue
		}
		switch e.Kind {
		case LedgerRefund:
			if e.Amount <= 0 {
				t.Fatalf("timeout refund amount = %d, must be positive", e.Amount)
			}
			sawRefund = true
		case LedgerReservationExpiration:
			if e.Amount != 0 {
				t.Fatalf("timeout status amount = %d, must be zero", e.Amount)
			}
			sawStatus = true
		}
	}
	if !sawRefund || !sawStatus {
		t.Fatalf("timeout ledger entries lost: refund=%v status=%v", sawRefund, sawStatus)
	}
}

// TestRestoreZeroFeeSettleKeepsZeroSettleAndPositiveRefund 验证合法零费用结算：
// 零金额扣减记录与原有的正数全额退款记录都必须保留，请求仍为已结算，不能把
// 零金额扣减当成没有发生结算。
func TestRestoreZeroFeeSettleKeepsZeroSettleAndPositiveRefund(t *testing.T) {
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
		PolicyID: "p", RequestID: "zero", AccountID: "u1", SessionID: "s1",
		DeviceID: "dev1", Operation: "charge", Payee: "shop", EstimatedFee: 20,
	}); err != nil {
		t.Fatal(err)
	}
	if r, err := w.Settle("u1", "zero", 0); err != nil || r.State != RequestSettled {
		t.Fatalf("zero-fee settle: %+v %v", r, err)
	}

	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, t0)
	if err != nil {
		t.Fatalf("zero-fee settled backup must restore: %v", err)
	}
	r, _ := w2.Request("u1", "zero")
	if r.State != RequestSettled || r.ActualFee != 0 {
		t.Fatalf("zero-fee request = %+v, want settled with actual fee 0", r)
	}
	// 实际费用为零：预留 20 已由正数退款全额退回。
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {100 0}", bal)
	}
	var settle, refund LedgerEntry
	var nSettle, nRefund int
	for _, e := range w2.Ledger() {
		if e.RequestID != "zero" {
			continue
		}
		switch e.Kind {
		case LedgerSettle:
			settle, nSettle = e, nSettle+1
		case LedgerRefund:
			refund, nRefund = e, nRefund+1
		}
	}
	if nSettle != 1 || nRefund != 1 {
		t.Fatalf("zero-fee settle ledger: %d settle, %d refund entries", nSettle, nRefund)
	}
	if settle.Amount != 0 {
		t.Fatalf("settle amount = %d, want 0", settle.Amount)
	}
	if refund.Amount != 20 {
		t.Fatalf("refund amount = %d, want positive full refund 20", refund.Amount)
	}
}

// TestRestoreOrphanZeroAmountRejectionAndEmptyLedger 验证金额核对不增加关联
// 对象必须存在的要求：金额为零的拒绝记录即使账户或请求编号缺失、关联对象
// 不存在，仍按原规则恢复；空账本也保持可恢复。
func TestRestoreOrphanZeroAmountRejectionAndEmptyLedger(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0),
		Accounts:   []accountBackupV1{},
		Sessions:   []sessionBackupV1{},
		Policies:   []policyBackupV1{},
		Requests:   []requestBackupV1{},
		Ledger: []ledgerEntryBackupV1{
			// 账户与请求编号都缺失。
			{Kind: int(LedgerRejection), Amount: 0, Reason: "missing required fields", At: timeJSON(t0.Add(-time.Minute))},
			// 编号指向从不存在的账户/请求。
			{Kind: int(LedgerRejection), AccountID: "ghost", RequestID: "ghost-rid", Amount: 0, Reason: "unknown account", At: timeJSON(t0)},
			// 同样零金额的待审批留痕，悬空编号。
			{Kind: int(LedgerPendingApproval), AccountID: "ghost", RequestID: "ghost-pending", Amount: 0, At: timeJSON(t0)},
		},
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, t0)
	if err != nil {
		t.Fatalf("orphan zero-amount status entries must restore: %v", err)
	}
	if got := len(w2.Ledger()); got != 3 {
		t.Fatalf("ledger len = %d, want 3 orphan entries preserved", got)
	}

	// 空账本同样可恢复。
	b.Ledger = []ledgerEntryBackupV1{}
	empty, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	w3, err := restoreAt(empty, t0)
	if err != nil {
		t.Fatalf("empty ledger must restore: %v", err)
	}
	if got := len(w3.Ledger()); got != 0 {
		t.Fatalf("empty ledger restored with %d entries", got)
	}
}

// TestRestoreNormalBackupLedgerUnchangedByAmountCheck 验证正常备份不受新核对
// 影响：账本顺序、类型、金额、时间、原因与关联编号逐条原样保留，不因校验
// 增添记录或改动余额（端到端往返）。
func TestRestoreNormalBackupLedgerUnchangedByAmountCheck(t *testing.T) {
	w, c := backupSetup(t)
	data, err := w.Export()
	if err != nil {
		t.Fatal(err)
	}
	w2, err := restoreAt(data, c.t)
	if err != nil {
		t.Fatalf("normal backup restore: %v", err)
	}
	src, got := w.Ledger(), w2.Ledger()
	if len(got) != len(src) {
		t.Fatalf("ledger len = %d, want %d", len(got), len(src))
	}
	for i := range src {
		if got[i] != src[i] {
			t.Fatalf("ledger[%d] = %+v, want %+v", i, got[i], src[i])
		}
	}
}
