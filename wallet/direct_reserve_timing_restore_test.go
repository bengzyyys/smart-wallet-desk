package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// directReserveState 命名一笔“发生过预留”的请求所处的后续状态，覆盖直接
// 受理请求可能到达的全部状态。
type directReserveState int

const (
	drReserved directReserveState = iota
	drSettled
	drCancelledFromReserved
	drReservationExpired
)

// buildDirectReserveBackup 构造一笔直接受理请求的自洽备份（题述场景）：
// 使用账户 u1 于 t0（十二点）提交费用 5，策略最长预留十分钟，提交成功即
// 预留，正常截止 t0+10m。默认开启审批门槛 10（费用 5 未严格超过门槛，故
// 仍为直接受理）；approvalEnabled=false 时关闭审批。其余计时与金额完全
// 自洽，因此恢复能否成功只取决于 mutate 对字段的调整。
func buildDirectReserveBackup(t *testing.T, t0 time.Time, st directReserveState, approvalEnabled bool, mutate func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)) []byte {
	t.Helper()
	const fee int64 = 5
	state := map[directReserveState]RequestState{
		drReserved:              RequestReserved,
		drSettled:               RequestSettled,
		drCancelledFromReserved: RequestCancelled,
		drReservationExpired:    RequestReservationExpired,
	}[st]
	r := requestBackupV1{
		PolicyID: "p", RequestID: "rq", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee:    fee,
		State:           int(state),
		CreatedAt:       timeJSON(t0),
		ReservedAt:      timeJSON(t0),
		ReserveDuration: durationJSON(10 * time.Minute),
		ReserveDeadline: timeJSON(t0.Add(10 * time.Minute)),
	}
	pol := policyBackupV1{
		ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "charge", Payee: "shop",
		StartsAt:           timeJSON(t0.Add(-time.Hour)),
		EndsAt:             timeJSON(t0.Add(24 * time.Hour)),
		MaxPerRequest:      100,
		MaxTotal:           1000,
		ApprovalWait:       durationJSON(time.Hour),
		MaxReserveDuration: durationJSON(10 * time.Minute),
	}
	if approvalEnabled {
		pol.ApprovalThreshold = 10
	}
	sess := sessionBackupV1{
		ID: "s1", AccountID: "u1", DeviceID: "d1",
		ExpiresAt: timeJSON(t0.Add(24 * time.Hour)), CreatedAt: timeJSON(t0.Add(-2 * time.Hour)),
	}
	payerAvailable, payerReserved := int64(100), int64(0)
	switch st {
	case drReserved:
		payerAvailable, payerReserved = 95, 5
		pol.ReservedTotal = 5
	case drSettled:
		r.ActualFee = 3
		r.SettledAt = timeJSON(t0.Add(5 * time.Minute))
		payerAvailable = 97
		pol.SpentTotal = 3
	case drCancelledFromReserved:
		// 已预留后取消：费用全额退回。
	case drReservationExpired:
		r.ReserveExpiredAt = r.ReserveDeadline
	}
	if mutate != nil {
		mutate(&r, &pol, &sess)
	}
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0.Add(-time.Minute)),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: payerAvailable, Reserved: payerReserved, CreatedAt: timeJSON(t0.Add(-2 * time.Hour))},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0.Add(-2 * time.Hour))},
		},
		Sessions: []sessionBackupV1{sess},
		Policies: []policyBackupV1{pol},
		Requests: []requestBackupV1{r},
		Ledger:   []ledgerEntryBackupV1{},
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// assertDirectReserveRejected 恢复必须失败、包装 ErrBackupInvalid，错误信息
// 点名使用账户与请求编号并说明两个时刻不一致，且不得返回部分钱包。
func assertDirectReserveRejected(t *testing.T, data []byte, at time.Time) string {
	t.Helper()
	w2, err := restoreAt(data, at)
	if err == nil {
		if w2 != nil {
			t.Fatalf("invalid direct-reserve history restored a wallet: %+v", w2)
		}
		t.Fatal("invalid direct-reserve history restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, `"u1"`) || !strings.Contains(msg, `"rq"`) {
		t.Fatalf("error %q must name usage account and request id", msg)
	}
	if !strings.Contains(msg, "reserved_at") || !strings.Contains(msg, "created_at") {
		t.Fatalf("error %q must state reserved_at disagrees with created_at", msg)
	}
	return msg
}

// TestRestoreDirectReserveShiftedReservedAtRejected 验证题述核心场景：十二点
// 提交、最长预留十分钟的直接受理请求，备份把实际预留改为十二点零五分、截止
// 改为十二点十五分（截止时刻与预留起点、时长仍然相符，账户预留余额、策略
// 累计金额与请求金额全部对得上），无论请求仍在预留、已经结算、已经取消还是
// 已经预留超时，都必须让整份备份恢复失败；关闭审批与“费用未严格超过门槛”
// 两种直接受理情形一致。
func TestRestoreDirectReserveShiftedReservedAtRejected(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	shift := func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		r.ReservedAt = timeJSON(t0.Add(5 * time.Minute))
		r.ReserveDeadline = timeJSON(t0.Add(15 * time.Minute))
		if r.State == int(RequestReservationExpired) {
			r.ReserveExpiredAt = r.ReserveDeadline
		}
	}
	for _, approvalEnabled := range []bool{false, true} {
		pathName := map[bool]string{false: "approval disabled", true: "fee below threshold"}[approvalEnabled]
		for _, st := range []directReserveState{drReserved, drSettled, drCancelledFromReserved, drReservationExpired} {
			stateName := map[directReserveState]string{
				drReserved:              "reserved",
				drSettled:               "settled",
				drCancelledFromReserved: "cancelled",
				drReservationExpired:    "reservation-expired",
			}[st]
			t.Run(pathName+"/"+stateName, func(t *testing.T) {
				data := buildDirectReserveBackup(t, t0, st, approvalEnabled, shift)
				// 错误信息必须同时给出两个不一致的时刻（十二点 vs 十二点零五分）。
				msg := assertDirectReserveRejected(t, data, t0.Add(time.Minute))
				if !strings.Contains(msg, "12:00:00") || !strings.Contains(msg, "12:05:00") {
					t.Fatalf("error %q must show both timestamps", msg)
				}
			})
		}
	}
}

// TestRestoreDirectReserveShiftedDeadlinePastAtRestore 验证被篡改的截止时刻
// 即使在恢复时已经过去（恢复时本会先按超时全额退款），仍必须拒绝恢复，不能
// 先退款、再把有矛盾的历史当成合法备份。
func TestRestoreDirectReserveShiftedDeadlinePastAtRestore(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	shift := func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		r.ReservedAt = timeJSON(t0.Add(5 * time.Minute))
		r.ReserveDeadline = timeJSON(t0.Add(15 * time.Minute))
	}
	data := buildDirectReserveBackup(t, t0, drReserved, true, shift)
	// 恢复于十二点三十分：伪造的截止（十二点十五）也已过去，仍须拒绝。
	assertDirectReserveRejected(t, data, t0.Add(30*time.Minute))

	// 已经处于预留超时终态的同样拒绝（伪造起点/截止均已过去，不能洗白）。
	shiftExpired := func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		r.ReservedAt = timeJSON(t0.Add(5 * time.Minute))
		r.ReserveDeadline = timeJSON(t0.Add(15 * time.Minute))
		r.ReserveExpiredAt = timeJSON(t0.Add(15 * time.Minute))
	}
	data = buildDirectReserveBackup(t, t0, drReservationExpired, true, shiftExpired)
	assertDirectReserveRejected(t, data, t0.Add(30*time.Minute))
}

// TestRestoreDirectReserveNanosecondShiftRejected 验证预留起点提前或推迟
// 一纳秒都必须拒绝；只按整秒判断会漏掉这种损坏。
func TestRestoreDirectReserveNanosecondShiftRejected(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		reservedAt time.Time
	}{
		{"reserved one nanosecond before created", t0.Add(-time.Nanosecond)},
		{"reserved one nanosecond after created", t0.Add(time.Nanosecond)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutate := func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.ReservedAt = timeJSON(tc.reservedAt)
				// 截止时刻按伪造起点保持自洽：三元组相符也不能通过。
				r.ReserveDeadline = timeJSON(tc.reservedAt.Add(10 * time.Minute))
			}
			data := buildDirectReserveBackup(t, t0, drReserved, true, mutate)
			assertDirectReserveRejected(t, data, t0.Add(time.Minute))
		})
	}
}

// TestRestoreDirectReserveSameInstantTimezoneAccepted 验证两个时刻以不同时区
// 写法表示同一绝对瞬间时正常接受：比较的是完整绝对时刻，不是文本或整秒。
func TestRestoreDirectReserveSameInstantTimezoneAccepted(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	loc := time.FixedZone("east", 5*60*60)
	expressInOtherZone := func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		// 预留起点与截止都换 +05:00 表示；提交时刻保持 UTC，二者是同一瞬间。
		same := t0.In(loc)
		if !same.Equal(t0) {
			t.Fatalf("test setup: timezone conversion moved the instant")
		}
		r.ReservedAt = timeJSON(same)
		r.ReserveDeadline = timeJSON(t0.Add(10 * time.Minute).In(loc))
	}
	for _, st := range []directReserveState{drReserved, drSettled, drCancelledFromReserved, drReservationExpired} {
		data := buildDirectReserveBackup(t, t0, st, true, expressInOtherZone)
		if _, err := restoreAt(data, t0.Add(30*time.Minute)); err != nil {
			t.Fatalf("same instant written in another timezone must restore: %v", err)
		}
	}
}

// TestRestoreDirectReserveTimeoutDisabledShiftRejected 验证关闭预留超时的
// 策略（没有截止时刻）同样不能让预留起点与提交时刻脱离：损坏备份无截止
// 三元组可依赖，起点必须恰好等于提交时刻。
func TestRestoreDirectReserveTimeoutDisabledShiftRejected(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	disableAndShift := func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
		p.MaxReserveDuration = 0
		r.ReserveDuration = durationJSON(0)
		r.ReserveDeadline = timeJSON(time.Time{})
		r.ReservedAt = timeJSON(t0.Add(time.Nanosecond))
	}
	data := buildDirectReserveBackup(t, t0, drReserved, false, disableAndShift)
	assertDirectReserveRejected(t, data, t0.Add(24*time.Hour))

	// 关闭超时且起点等于提交时刻：照常恢复，并长期保持已预留。
	disableOnly := func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
		p.MaxReserveDuration = 0
		r.ReserveDuration = durationJSON(0)
		r.ReserveDeadline = timeJSON(time.Time{})
	}
	data = buildDirectReserveBackup(t, t0, drReserved, false, disableOnly)
	w2, err := restoreAt(data, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("timeout-disabled direct reserve with matching origin must restore: %v", err)
	}
	if r, _ := w2.Request("u1", "rq"); r.State != RequestReserved || !r.ReserveDeadline.IsZero() {
		t.Fatalf("timeout-disabled reserve changed: %+v", r)
	}
}

// TestRestoreApprovedReserveMayStartLater 验证经大额审批后才预留的请求从
// 批准成功时刻起算、实际预留允许晚于提交，不得被新校验误拒绝；仍在预留、
// 已结算、已取消、已预留超时四种情形都照常恢复，时刻原样保留。
func TestRestoreApprovedReserveMayStartLater(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// buildApprovedTimingBackup 的默认字段即“提交 t0、等待截止 t0+1h、
	// t0+30m 批准并实际预留、预留截止 t0+2h30”。
	for _, st := range []RequestState{RequestReserved, RequestSettled, RequestCancelled, RequestReservationExpired} {
		data := buildApprovedTimingBackup(t, t0, st, nil)
		w2, err := restoreAt(data, t0.Add(24*time.Hour))
		if err != nil {
			t.Fatalf("approved request reserved after created must restore: %v", err)
		}
		r, _ := w2.Request("u1", "big")
		if !r.ReservedAt.Equal(t0.Add(30*time.Minute)) || !r.CreatedAt.Equal(t0) {
			t.Fatalf("approval timing rewritten: %+v", r)
		}
	}
}

// TestRestoreDirectReserveFeeEqualThresholdIsDirect 验证费用恰好等于门槛
// （没有严格超过）时仍属直接受理：预留起点必须等于提交时刻。
func TestRestoreDirectReserveFeeEqualThresholdIsDirect(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// builder 默认费用 5；把门槛也设为 5：费用不严格超过门槛，仍直接受理。
	data := buildDirectReserveBackup(t, t0, drReserved, true, func(_ *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
		p.ApprovalThreshold = 5
	})
	if _, err := restoreAt(data, t0.Add(time.Minute)); err != nil {
		t.Fatalf("fee equal to threshold is directly reserved and must restore: %v", err)
	}

	// 同样费用门槛相等、起点被推迟一纳秒：仍按直接受理规则拒绝。
	var b backupV1
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	b.Requests[0].ReservedAt = timeJSON(t0.Add(time.Nanosecond))
	b.Requests[0].ReserveDeadline = timeJSON(t0.Add(10*time.Minute + time.Nanosecond))
	shifted, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	assertDirectReserveRejected(t, shifted, t0.Add(time.Minute))
}

// TestRestoreDirectReserveShiftFailsWholeBackup 验证同一备份中只要有一笔
// 直接受理请求的预留起点与提交时刻不一致，整份备份恢复失败、不返回部分
// 可用的钱包，即使另一笔请求完全合法；也不通过改写时间补救。
func TestRestoreDirectReserveShiftFailsWholeBackup(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildDirectReserveBackup(t, t0, drReserved, true, nil)
	var b backupV1
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	// 追加一笔合法直接预留 ok（十二点提交即预留），把被篡改的 rq 起点推迟。
	b.Requests = append(b.Requests, requestBackupV1{
		PolicyID: "p", RequestID: "ok", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee: 5, State: int(RequestReserved),
		CreatedAt: timeJSON(t0), ReservedAt: timeJSON(t0),
		ReserveDuration: durationJSON(10 * time.Minute),
		ReserveDeadline: timeJSON(t0.Add(10 * time.Minute)),
	})
	b.Requests[0].ReservedAt = timeJSON(t0.Add(5 * time.Minute))
	b.Requests[0].ReserveDeadline = timeJSON(t0.Add(15 * time.Minute))
	// 两笔现存预留各 5：账户预留余额与策略预留总额调整为 10，可用 90。
	for i := range b.Accounts {
		if b.Accounts[i].ID == "payer" {
			b.Accounts[i].Available = 90
			b.Accounts[i].Reserved = 10
		}
	}
	b.Policies[0].ReservedTotal = 10
	corrupt, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	assertDirectReserveRejected(t, corrupt, t0.Add(time.Minute))
}

// TestRestoreLegalDirectReservePreservedUnchanged 验证合法直接受理请求恢复
// 后保留原来的金额、决定信息与账本：尚未结束的按原截止时刻继续处理，不
// 重新开始预留计时。
func TestRestoreLegalDirectReservePreservedUnchanged(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// 已预留、恢复时刻在截止之前：时刻原样保留。
	data := buildDirectReserveBackup(t, t0, drReserved, true, nil)
	w2, err := restoreAt(data, t0.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("legal direct reserve must restore: %v", err)
	}
	r, _ := w2.Request("u1", "rq")
	if r.State != RequestReserved || !r.ReservedAt.Equal(t0) || !r.ReserveDeadline.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("direct reserve timing rewritten: %+v", r)
	}

	// 已结算：金额与时刻原样保留，不追加退款或超时记录。
	data = buildDirectReserveBackup(t, t0, drSettled, true, nil)
	w2, err = restoreAt(data, t0.Add(30*24*time.Hour))
	if err != nil {
		t.Fatalf("legal settled direct reserve must restore: %v", err)
	}
	r, _ = w2.Request("u1", "rq")
	if r.State != RequestSettled || r.ActualFee != 3 || !r.SettledAt.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("settled history changed: %+v", r)
	}
	if n := len(w2.Ledger()); n != 0 {
		t.Fatalf("restore appended %d ledger entries", n)
	}
	if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 97, Reserved: 0}) {
		t.Fatalf("balance = %+v, want {97 0}", bal)
	}
}
