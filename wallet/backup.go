package wallet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"time"
)

// backupVersion 为当前支持的备份格式版本号。
const backupVersion = 1

// ErrBackupInvalid 表示备份文本为空、无法解析、缺少必需数据、版本不支持
// 或内容自相矛盾。恢复接口返回包装了具体原因的错误；任何校验失败都不会
// 返回部分恢复的钱包。
var ErrBackupInvalid = errors.New("wallet: invalid backup")

// timeJSON 以 RFC3339Nano（带纳秒与时区）序列化时间，零值时间序列化为
// 空字符串，避免把“尚未发生”的计时信息误恢复成公元元年。
type timeJSON time.Time

func (t timeJSON) MarshalJSON() ([]byte, error) {
	tm := time.Time(t)
	if tm.IsZero() {
		return []byte(`""`), nil
	}
	return json.Marshal(tm.Format(time.RFC3339Nano))
}

func (t *timeJSON) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*t = timeJSON(time.Time{})
		return nil
	}
	tm, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		// 宽容解析无纳秒的 RFC3339。
		tm, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return err
		}
	}
	*t = timeJSON(tm)
	return nil
}

func (t timeJSON) std() time.Time { return time.Time(t) }

// durationJSON 以 int64 纳秒序列化时长，保持精确往返。
type durationJSON time.Duration

func (d durationJSON) MarshalJSON() ([]byte, error) {
	return json.Marshal(int64(d))
}

func (d *durationJSON) UnmarshalJSON(data []byte) error {
	var n int64
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*d = durationJSON(n)
	return nil
}

func (d durationJSON) std() time.Duration { return time.Duration(d) }

type accountBackupV1 struct {
	ID        string   `json:"id"`
	Available int64    `json:"available"`
	Reserved  int64    `json:"reserved"`
	CreatedAt timeJSON `json:"created_at"`
}

type sessionBackupV1 struct {
	ID        string   `json:"id"`
	AccountID string   `json:"account_id"`
	DeviceID  string   `json:"device_id"`
	ExpiresAt timeJSON `json:"expires_at"`
	Revoked   bool     `json:"revoked"`
	CreatedAt timeJSON `json:"created_at"`
}

type policyBackupV1 struct {
	ID                   string       `json:"id"`
	PayerAccountID       string       `json:"payer_account_id"`
	AllowedAccountIDs    []string     `json:"allowed_account_ids"`
	Operation            string       `json:"operation"`
	Payee                string       `json:"payee"`
	StartsAt             timeJSON     `json:"starts_at"`
	EndsAt               timeJSON     `json:"ends_at"`
	MaxPerRequest        int64        `json:"max_per_request"`
	MaxTotal             int64        `json:"max_total"`
	ApprovalThreshold    int64        `json:"approval_threshold"`
	ApprovalWait         durationJSON `json:"approval_wait_nanos"`
	MaxReserveDuration   durationJSON `json:"max_reserve_duration_nanos"`
	ReservedTotal        int64        `json:"reserved_total"`
	SpentTotal           int64        `json:"spent_total"`
	Deactivated          bool         `json:"deactivated"`
	DeactivatedAt        timeJSON     `json:"deactivated_at"`
	DeactivatorAccountID string       `json:"deactivator_account_id"`
	DeactivateReason     string       `json:"deactivate_reason"`
}

type requestBackupV1 struct {
	PolicyID          string       `json:"policy_id"`
	RequestID         string       `json:"request_id"`
	AccountID         string       `json:"account_id"`
	PayerAccountID    string       `json:"payer_account_id"`
	SessionID         string       `json:"session_id"`
	Operation         string       `json:"operation"`
	Payee             string       `json:"payee"`
	EstimatedFee      int64        `json:"estimated_fee"`
	ActualFee         int64        `json:"actual_fee"`
	State             int          `json:"state"`
	CreatedAt         timeJSON     `json:"created_at"`
	SettledAt         timeJSON     `json:"settled_at"`
	WaitDeadline      timeJSON     `json:"wait_deadline"`
	ReservedAt        timeJSON     `json:"reserved_at"`
	ReserveDuration   durationJSON `json:"reserve_duration_nanos"`
	ReserveDeadline   timeJSON     `json:"reserve_deadline"`
	ReserveExpiredAt  timeJSON     `json:"reserve_expired_at"`
	DecidedAt         timeJSON     `json:"decided_at"`
	ApproverAccountID string       `json:"approver_account_id"`
	RejectReason      string       `json:"reject_reason"`
}

type ledgerEntryBackupV1 struct {
	Kind      int      `json:"kind"`
	AccountID string   `json:"account_id"`
	RequestID string   `json:"request_id"`
	PolicyID  string   `json:"policy_id"`
	Amount    int64    `json:"amount"`
	Reason    string   `json:"reason"`
	At        timeJSON `json:"at"`
}

type backupV1 struct {
	Version    int                   `json:"version"`
	ExportedAt timeJSON              `json:"exported_at"`
	Accounts   []accountBackupV1     `json:"accounts"`
	Sessions   []sessionBackupV1     `json:"sessions"`
	Policies   []policyBackupV1      `json:"policies"`
	Requests   []requestBackupV1     `json:"requests"`
	Ledger     []ledgerEntryBackupV1 `json:"ledger"`
}

// Export 将当前钱包导出为带版本号的 JSON 文本。
//
// 导出在钱包同一把全局锁内完成，对应某一时刻的完整状态：可与申请、
// 批准、拒绝、吊销、停用、结算或取消并发，不会出现只有余额变化却缺少
// 对应请求或账本的半截状态。导出前先按导出时的当前时间处理全部到期
// 请求——待审批到达等待截止时刻后进入过期终态并留痕，已预留到达预留
// 截止时刻后全额退回（释放时间仍为原截止时刻）并留下退款及超时记录；
// 未到期请求继续等待或预留。
//
// 备份保留账户余额、会话的设备绑定与吊销状态、策略条件及停用信息、
// 全部请求的申请内容、状态、审批决定与全部计时信息，以及按原顺序排列
// 的账本（包括未被受理申请的拒绝记录）。空钱包也能导出并恢复；导出
// 本身不新增任何账本记录（到期处理带来的记录除外）。
//
// 备份不能暂停期限，因此导出不改变任何请求的计时基准；导出后的钱包
// 与备份文本、以及从该备份恢复出的钱包之间互不影响。
func (w *Wallet) Export() ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.now()
	// 导出快照前结清全部到期项，保证备份对应一个完整状态：已预留超时的按
	// 原截止时刻全额退回并留两条记录（按截止时刻排序），待审批过期的留
	// 状态记录（按等待截止时刻排序）。
	w.expireReservationsLocked(now)
	w.expirePendingApprovalsLocked(now)

	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(now),
		Accounts:   make([]accountBackupV1, 0, len(w.accounts)),
		Sessions:   make([]sessionBackupV1, 0, len(w.sessions)),
		Policies:   make([]policyBackupV1, 0, len(w.policies)),
		Requests:   make([]requestBackupV1, 0, len(w.requests)),
		Ledger:     make([]ledgerEntryBackupV1, 0, len(w.ledger)),
	}

	// map 迭代顺序不确定；集合类数据按编号排序，使备份可逐字节复现。
	for _, a := range w.accounts {
		b.Accounts = append(b.Accounts, accountBackupV1{
			ID:        a.id,
			Available: a.available,
			Reserved:  a.reserved,
			CreatedAt: timeJSON(a.createdAt),
		})
	}
	sort.Slice(b.Accounts, func(i, j int) bool { return b.Accounts[i].ID < b.Accounts[j].ID })

	for _, s := range w.sessions {
		b.Sessions = append(b.Sessions, sessionBackupV1{
			ID:        s.id,
			AccountID: s.accountID,
			DeviceID:  s.deviceID,
			ExpiresAt: timeJSON(s.expiresAt),
			Revoked:   s.revoked,
			CreatedAt: timeJSON(s.createdAt),
		})
	}
	sort.Slice(b.Sessions, func(i, j int) bool { return b.Sessions[i].ID < b.Sessions[j].ID })

	for _, p := range w.policies {
		allowed := make([]string, 0, len(p.allowedAccounts))
		for id := range p.allowedAccounts {
			allowed = append(allowed, id)
		}
		sort.Strings(allowed)
		b.Policies = append(b.Policies, policyBackupV1{
			ID:                   p.id,
			PayerAccountID:       p.payerAccountID,
			AllowedAccountIDs:    allowed,
			Operation:            p.operation,
			Payee:                p.payee,
			StartsAt:             timeJSON(p.startsAt),
			EndsAt:               timeJSON(p.endsAt),
			MaxPerRequest:        p.maxPerRequest,
			MaxTotal:             p.maxTotal,
			ApprovalThreshold:    p.approvalThreshold,
			ApprovalWait:         durationJSON(p.approvalWait),
			MaxReserveDuration:   durationJSON(p.maxReserveDuration),
			ReservedTotal:        p.reservedTotal,
			SpentTotal:           p.spentTotal,
			Deactivated:          p.deactivated,
			DeactivatedAt:        timeJSON(p.deactivatedAt),
			DeactivatorAccountID: p.deactivatorAccountID,
			DeactivateReason:     p.deactivateReason,
		})
	}
	sort.Slice(b.Policies, func(i, j int) bool { return b.Policies[i].ID < b.Policies[j].ID })

	for _, r := range w.requests {
		b.Requests = append(b.Requests, requestBackupV1{
			PolicyID:          r.policyID,
			RequestID:         r.requestID,
			AccountID:         r.accountID,
			PayerAccountID:    r.payerAccountID,
			SessionID:         r.sessionID,
			Operation:         r.operation,
			Payee:             r.payee,
			EstimatedFee:      r.estimatedFee,
			ActualFee:         r.actualFee,
			State:             int(r.state),
			CreatedAt:         timeJSON(r.createdAt),
			SettledAt:         timeJSON(r.settledAt),
			WaitDeadline:      timeJSON(r.waitDeadline),
			ReservedAt:        timeJSON(r.reservedAt),
			ReserveDuration:   durationJSON(r.reserveDuration),
			ReserveDeadline:   timeJSON(r.reserveDeadline),
			ReserveExpiredAt:  timeJSON(r.reserveExpiredAt),
			DecidedAt:         timeJSON(r.decidedAt),
			ApproverAccountID: r.approverAccountID,
			RejectReason:      r.rejectReason,
		})
	}
	sort.Slice(b.Requests, func(i, j int) bool {
		if b.Requests[i].AccountID != b.Requests[j].AccountID {
			return b.Requests[i].AccountID < b.Requests[j].AccountID
		}
		return b.Requests[i].RequestID < b.Requests[j].RequestID
	})

	// 账本严格按原顺序导出，不重新排序。
	for _, e := range w.ledger {
		b.Ledger = append(b.Ledger, ledgerEntryBackupV1{
			Kind:      int(e.Kind),
			AccountID: e.AccountID,
			RequestID: e.RequestID,
			PolicyID:  e.PolicyID,
			Amount:    e.Amount,
			Reason:    e.Reason,
			At:        timeJSON(e.At),
		})
	}

	out, err := json.Marshal(&b)
	if err != nil {
		return nil, fmt.Errorf("%w: encode: %v", ErrBackupInvalid, err)
	}
	return out, nil
}

// addInt64 在求和时检测 int64 溢出；超出范围返回 false。
func addInt64(sum, x int64) (int64, bool) {
	if x > 0 && sum > math.MaxInt64-x {
		return 0, false
	}
	if x < 0 && sum < math.MinInt64-x {
		return 0, false
	}
	return sum + x, true
}

// Restore 从 Export 产生的 JSON 文本创建一个独立的钱包。
//
// 恢复不重新计时：所有截止时刻与会话到期时间沿用备份中的绝对时刻，
// 恢复前先按恢复时的当前时间处理全部到期请求（待审批过期、已预留超时
// 全额退回，释放时间仍为原截止时刻）。既有终态与历史记录不改写；过期或
// 吊销的会话、停用或结束的策略可以恢复，但不会重新获得授权。恢复后被
// 超时退回的余额可立即用于新申请，旧请求的迟到结算不能再扣走这笔钱。
//
// 空文本、非法 JSON、缺少必需数据、不支持的版本、编号重复、未知状态或
// 账本类型、非法金额或策略参数、悬空引用、申请会话不属于使用账户、
// 请求出资账户与策略不符，或余额/累计金额与请求求和不一致（含求和超出
// int64 范围），都返回包装了具体原因的 ErrBackupInvalid，绝不会返回
// 部分恢复的钱包。任一策略现存预留费用与已结算实际费用的合计严格超过其
// 共享累计上限时同样拒绝（同一策略的多个使用账户合并计算，不同策略分别
// 判断；待审批、已取消、被拒绝、已过期及预留超时的请求不占用额度），
// 该检查针对备份保存的资金占用状态、在恢复时的到期自动退回之前完成：
// 合计恰好等于上限合法，上限恰为 MaxInt64 时也不缩小可接受的金额范围。
// 任一账户的可用余额加上该账户在所有策略下仍处于已预留
// 状态的费用之和超出 int64 上限时同样拒绝（即使各请求、各策略金额分别
// 合法）：否则取消或预留超时的退回会使余额越界；校验在恢复时的到期
// 自动退回之前完成，预留尚未到期也当场拒绝。恢复出的钱包与原钱包、同一
// 备份恢复出的其他钱包互不影响。
func Restore(data []byte) (*Wallet, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%w: empty backup", ErrBackupInvalid)
	}

	var b backupV1
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("%w: malformed json: %v", ErrBackupInvalid, err)
	}
	// 不允许备份对象之后还有多余的 JSON 值；忽略空白。
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: unexpected trailing data after backup object", ErrBackupInvalid)
	}
	if b.Version != backupVersion {
		return nil, fmt.Errorf("%w: unsupported version %d (supported: %d)", ErrBackupInvalid, b.Version, backupVersion)
	}

	w := New()
	if err := w.restoreLocked(&b); err != nil {
		return nil, err
	}
	return w, nil
}

// restoreAt 仅供同包测试使用：与 Restore 相同，但显式指定恢复时刻，
// 便于确定性地验证恢复时的到期处理。
func restoreAt(data []byte, now time.Time) (*Wallet, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%w: empty backup", ErrBackupInvalid)
	}
	var b backupV1
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("%w: malformed json: %v", ErrBackupInvalid, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: unexpected trailing data after backup object", ErrBackupInvalid)
	}
	if b.Version != backupVersion {
		return nil, fmt.Errorf("%w: unsupported version %d (supported: %d)", ErrBackupInvalid, b.Version, backupVersion)
	}
	w := New()
	w.now = func() time.Time { return now }
	if err := w.restoreLocked(&b); err != nil {
		return nil, err
	}
	return w, nil
}

// restoreLocked 逐段构建并校验新钱包；任何失败都返回错误，调用方因此
// 绝不会拿到部分恢复的钱包。
func (w *Wallet) restoreLocked(b *backupV1) error {
	// ---- 账户 ----
	if b.Accounts == nil {
		return fmt.Errorf("%w: missing accounts", ErrBackupInvalid)
	}
	accountIDs := make(map[string]struct{}, len(b.Accounts))
	reservedByAccount := make(map[string]int64)
	for i, a := range b.Accounts {
		if a.ID == "" {
			return fmt.Errorf("%w: account[%d] has empty id", ErrBackupInvalid, i)
		}
		if _, dup := accountIDs[a.ID]; dup {
			return fmt.Errorf("%w: duplicate account id %q", ErrBackupInvalid, a.ID)
		}
		if a.Available < 0 {
			return fmt.Errorf("%w: account %q available balance is negative: %d", ErrBackupInvalid, a.ID, a.Available)
		}
		if a.Reserved < 0 {
			return fmt.Errorf("%w: account %q reserved balance is negative: %d", ErrBackupInvalid, a.ID, a.Reserved)
		}
		accountIDs[a.ID] = struct{}{}
		w.accounts[a.ID] = &account{
			id:        a.ID,
			available: a.Available,
			reserved:  a.Reserved,
			createdAt: a.CreatedAt.std(),
		}
	}

	// ---- 会话 ----
	if b.Sessions == nil {
		return fmt.Errorf("%w: missing sessions", ErrBackupInvalid)
	}
	sessionByID := make(map[string]*session, len(b.Sessions))
	for i, s := range b.Sessions {
		if s.ID == "" {
			return fmt.Errorf("%w: session[%d] has empty id", ErrBackupInvalid, i)
		}
		if _, dup := sessionByID[s.ID]; dup {
			return fmt.Errorf("%w: duplicate session id %q", ErrBackupInvalid, s.ID)
		}
		if s.AccountID == "" || s.DeviceID == "" {
			return fmt.Errorf("%w: session %q missing account or device id", ErrBackupInvalid, s.ID)
		}
		if _, ok := accountIDs[s.AccountID]; !ok {
			return fmt.Errorf("%w: session %q references unknown account %q", ErrBackupInvalid, s.ID, s.AccountID)
		}
		sess := &session{
			id:        s.ID,
			accountID: s.AccountID,
			deviceID:  s.DeviceID,
			expiresAt: s.ExpiresAt.std(),
			revoked:   s.Revoked,
			createdAt: s.CreatedAt.std(),
		}
		sessionByID[s.ID] = sess
		w.sessions[s.ID] = sess
	}

	// ---- 策略 ----
	if b.Policies == nil {
		return fmt.Errorf("%w: missing policies", ErrBackupInvalid)
	}
	policyByID := make(map[string]*policy, len(b.Policies))
	for i, p := range b.Policies {
		if p.ID == "" {
			return fmt.Errorf("%w: policy[%d] has empty id", ErrBackupInvalid, i)
		}
		if _, dup := policyByID[p.ID]; dup {
			return fmt.Errorf("%w: duplicate policy id %q", ErrBackupInvalid, p.ID)
		}
		if err := validatePolicyParams(p); err != nil {
			return fmt.Errorf("%w: policy %q: %v", ErrBackupInvalid, p.ID, err)
		}
		if _, ok := accountIDs[p.PayerAccountID]; !ok {
			return fmt.Errorf("%w: policy %q references unknown payer account %q", ErrBackupInvalid, p.ID, p.PayerAccountID)
		}
		allowed := make(map[string]struct{}, len(p.AllowedAccountIDs))
		for j, id := range p.AllowedAccountIDs {
			if id == "" {
				return fmt.Errorf("%w: policy %q allowed account[%d] is empty", ErrBackupInvalid, p.ID, j)
			}
			if _, ok := accountIDs[id]; !ok {
				return fmt.Errorf("%w: policy %q references unknown allowed account %q", ErrBackupInvalid, p.ID, id)
			}
			if _, dup := allowed[id]; dup {
				return fmt.Errorf("%w: policy %q lists allowed account %q more than once", ErrBackupInvalid, p.ID, id)
			}
			allowed[id] = struct{}{}
		}
		if p.ReservedTotal < 0 || p.SpentTotal < 0 {
			return fmt.Errorf("%w: policy %q totals must not be negative (reserved %d spent %d)", ErrBackupInvalid, p.ID, p.ReservedTotal, p.SpentTotal)
		}
		po := &policy{
			id:                   p.ID,
			payerAccountID:       p.PayerAccountID,
			allowedAccounts:      allowed,
			operation:            p.Operation,
			payee:                p.Payee,
			startsAt:             p.StartsAt.std(),
			endsAt:               p.EndsAt.std(),
			maxPerRequest:        p.MaxPerRequest,
			maxTotal:             p.MaxTotal,
			approvalThreshold:    p.ApprovalThreshold,
			approvalWait:         p.ApprovalWait.std(),
			maxReserveDuration:   p.MaxReserveDuration.std(),
			reservedTotal:        p.ReservedTotal,
			spentTotal:           p.SpentTotal,
			deactivated:          p.Deactivated,
			deactivatedAt:        p.DeactivatedAt.std(),
			deactivatorAccountID: p.DeactivatorAccountID,
			deactivateReason:     p.DeactivateReason,
		}
		// 停用信息自洽：停用标志必须与停用时间/理由同时出现；停用账户必须
		// 存在；未停用不得携带任何停用记录。
		if po.deactivated {
			if po.deactivatedAt.IsZero() || po.deactivateReason == "" {
				return fmt.Errorf("%w: policy %q marked deactivated but missing deactivation time or reason", ErrBackupInvalid, p.ID)
			}
			if po.deactivatorAccountID != "" {
				if _, ok := accountIDs[po.deactivatorAccountID]; !ok {
					return fmt.Errorf("%w: policy %q deactivator references unknown account %q", ErrBackupInvalid, p.ID, po.deactivatorAccountID)
				}
			}
		} else if !po.deactivatedAt.IsZero() || po.deactivatorAccountID != "" || po.deactivateReason != "" {
			return fmt.Errorf("%w: policy %q not deactivated but carries deactivation info", ErrBackupInvalid, p.ID)
		}
		policyByID[p.ID] = po
		w.policies[p.ID] = po
	}

	// ---- 请求 ----
	if b.Requests == nil {
		return fmt.Errorf("%w: missing requests", ErrBackupInvalid)
	}
	reqKeys := make(map[requestKey]struct{}, len(b.Requests))
	reservedSumByPolicy := make(map[string]int64)
	spentSumByPolicy := make(map[string]int64)
	for i, r := range b.Requests {
		if err := validateRequestRef(r, i, accountIDs, sessionByID, policyByID); err != nil {
			return err
		}
		key := requestKey{accountID: r.AccountID, requestID: r.RequestID}
		if _, dup := reqKeys[key]; dup {
			return fmt.Errorf("%w: duplicate request id %q under account %q", ErrBackupInvalid, r.RequestID, r.AccountID)
		}
		reqKeys[key] = struct{}{}

		state := RequestState(r.State)
		if state < RequestReserved || state > RequestReservationExpired {
			return fmt.Errorf("%w: request %q/%q has unknown state %d", ErrBackupInvalid, r.AccountID, r.RequestID, r.State)
		}
		if r.EstimatedFee <= 0 {
			return fmt.Errorf("%w: request %q/%q estimated fee must be positive: %d", ErrBackupInvalid, r.AccountID, r.RequestID, r.EstimatedFee)
		}
		if r.ActualFee < 0 {
			return fmt.Errorf("%w: request %q/%q actual fee is negative: %d", ErrBackupInvalid, r.AccountID, r.RequestID, r.ActualFee)
		}
		if r.ReserveDuration.std() < 0 {
			return fmt.Errorf("%w: request %q/%q reserve duration is negative", ErrBackupInvalid, r.AccountID, r.RequestID)
		}

		req := &request{
			policyID:          r.PolicyID,
			requestID:         r.RequestID,
			accountID:         r.AccountID,
			payerAccountID:    r.PayerAccountID,
			sessionID:         r.SessionID,
			operation:         r.Operation,
			payee:             r.Payee,
			estimatedFee:      r.EstimatedFee,
			actualFee:         r.ActualFee,
			state:             state,
			createdAt:         r.CreatedAt.std(),
			settledAt:         r.SettledAt.std(),
			waitDeadline:      r.WaitDeadline.std(),
			reservedAt:        r.ReservedAt.std(),
			reserveDuration:   r.ReserveDuration.std(),
			reserveDeadline:   r.ReserveDeadline.std(),
			reserveExpiredAt:  r.ReserveExpiredAt.std(),
			decidedAt:         r.DecidedAt.std(),
			approverAccountID: r.ApproverAccountID,
			rejectReason:      r.RejectReason,
		}
		if err := validateRequestTimingAndState(req, policyByID[r.PolicyID], sessionByID[r.SessionID]); err != nil {
			return fmt.Errorf("%w: request %q/%q: %v", ErrBackupInvalid, r.AccountID, r.RequestID, err)
		}
		w.requests[key] = req

		// 累加各账户/策略“按请求推算”的金额，稍后与快照余额核对。
		// 只有当前仍处于已预留的请求才占用账户预留与策略预留总额。
		if state == RequestReserved {
			sum, ok := addInt64(reservedByAccount[req.payerAccountID], req.estimatedFee)
			if !ok {
				return fmt.Errorf("%w: account %q reserved sum overflows int64", ErrBackupInvalid, req.payerAccountID)
			}
			reservedByAccount[req.payerAccountID] = sum
			sum, ok = addInt64(reservedSumByPolicy[req.policyID], req.estimatedFee)
			if !ok {
				return fmt.Errorf("%w: policy %q reserved sum overflows int64", ErrBackupInvalid, req.policyID)
			}
			reservedSumByPolicy[req.policyID] = sum
		}
		if state == RequestSettled {
			sum, ok := addInt64(spentSumByPolicy[req.policyID], req.actualFee)
			if !ok {
				return fmt.Errorf("%w: policy %q spent sum overflows int64", ErrBackupInvalid, req.policyID)
			}
			spentSumByPolicy[req.policyID] = sum
		}
	}

	// ---- 账户预留余额 == 已预留请求预估费用之和 ----
	for id, acc := range w.accounts {
		if got, want := acc.reserved, reservedByAccount[id]; got != want {
			return fmt.Errorf("%w: account %q reserved %d != sum of reserved requests %d", ErrBackupInvalid, id, got, want)
		}
	}

	// ---- 可用余额 + 现存预留不得超过 int64 上限 ----
	// 取消或预留超时会把预留全额退回可用余额：合计一旦超出 MaxInt64，
	// 退回时余额就会越界变负。reservedByAccount 按出资账户汇总该账户在
	// 所有策略下仍处于已预留状态的费用（待审批不冻结费用，已结算、已取消、
	// 已预留超时的费用不再占用预留，故均不计入）。本检查必须在恢复时的
	// 到期自动退回之前完成，使“恢复即超时退款”的备份也不能蒙混过关；
	// 合计恰好等于上限合法（随后退回恰好得到上限金额）。
	for id, acc := range w.accounts {
		if _, ok := addInt64(acc.available, reservedByAccount[id]); !ok {
			return fmt.Errorf("%w: account %q available %d plus reserved %d overflows int64", ErrBackupInvalid, id, acc.available, reservedByAccount[id])
		}
	}

	// ---- 策略预留总额/已花费总额 == 请求求和，且合计不得超过累计上限 ----
	// 现存预留费用与已结算的实际费用共同占用策略的共享累计额度；同一策略的
	// 多个使用账户合计判断，不同策略分别判断。待审批不冻结费用，已取消、被
	// 拒绝、待审批过期、预留超时的请求均不计入，故上方求和本就不含它们。
	// 严格超过累计上限的备份自相矛盾（正常流程在受理时就会拒绝），必须整体
	// 拒绝；合计恰好等于上限合法。本检查针对备份保存的资金占用状态，位于
	// 恢复时的到期自动退回之前：即使某笔已预留请求恢复时已到期、将全额
	// 退回，也不能先退回再让原本超额的备份通过。两项金额各自为 int64、
	// 合计越过 int64 上限时，addInt64 先捕获溢出，不能误判为额度充足。
	for id, p := range policyByID {
		if got, want := p.reservedTotal, reservedSumByPolicy[id]; got != want {
			return fmt.Errorf("%w: policy %q reserved total %d != sum of reserved requests %d", ErrBackupInvalid, id, got, want)
		}
		if got, want := p.spentTotal, spentSumByPolicy[id]; got != want {
			return fmt.Errorf("%w: policy %q spent total %d != sum of settled requests %d", ErrBackupInvalid, id, got, want)
		}
		used, ok := addInt64(reservedSumByPolicy[id], spentSumByPolicy[id])
		if !ok {
			return fmt.Errorf("%w: policy %q reserved %d plus spent %d overflows int64 and exceeds cumulative total limit %d",
				ErrBackupInvalid, id, reservedSumByPolicy[id], spentSumByPolicy[id], p.maxTotal)
		}
		if used > p.maxTotal {
			return fmt.Errorf("%w: policy %q reserved %d plus spent %d exceeds cumulative total limit %d",
				ErrBackupInvalid, id, reservedSumByPolicy[id], spentSumByPolicy[id], p.maxTotal)
		}
	}

	// ---- 账本 ----
	if b.Ledger == nil {
		return fmt.Errorf("%w: missing ledger", ErrBackupInvalid)
	}
	w.ledger = make([]LedgerEntry, 0, len(b.Ledger))
	for i, e := range b.Ledger {
		kind := LedgerKind(e.Kind)
		if kind < LedgerReserve || kind > LedgerReservationExpiration {
			return fmt.Errorf("%w: ledger[%d] has unknown kind %d", ErrBackupInvalid, i, e.Kind)
		}
		if e.Amount < 0 {
			return fmt.Errorf("%w: ledger[%d] amount is negative: %d", ErrBackupInvalid, i, e.Amount)
		}
		// 账本是历史留痕：其账户/请求编号允许为空或指向不存在的实体。
		// 未被受理申请（必填字段缺失、使用账户或会话不存在等）同样会留下
		// 拒绝记录，这些记录必须原样保留，不能因为找不到对应账户或请求而
		// 丢弃或拒绝恢复。资金一致性由账户余额与现存请求的求和核对保证，
		// 不依赖账本文本。PolicyID 若给出则必须存在（仅策略停用记录使用）。
		if e.PolicyID != "" {
			if _, ok := policyByID[e.PolicyID]; !ok {
				return fmt.Errorf("%w: ledger[%d] references unknown policy %q", ErrBackupInvalid, i, e.PolicyID)
			}
		}
		w.ledger = append(w.ledger, LedgerEntry{
			Kind:      kind,
			AccountID: e.AccountID,
			RequestID: e.RequestID,
			PolicyID:  e.PolicyID,
			Amount:    e.Amount,
			Reason:    e.Reason,
			At:        e.At.std(),
		})
	}

	// ---- 恢复时按当前时间结清到期请求（不重新计时，只推进惰性状态） ----
	// 处理顺序与导出保持一致：先释放已到期预留，再把到期的待审批转为过期，
	// 两类记录均按各自截止时刻排序后追加到账本末尾。
	now := w.now()
	w.expireReservationsLocked(now)
	w.expirePendingApprovalsLocked(now)
	return nil
}

// validatePolicyParams 校验策略条件与限额参数，规则与 SavePolicy 一致。
func validatePolicyParams(p policyBackupV1) error {
	if p.Operation == "" || p.Payee == "" {
		return errors.New("operation and payee are required")
	}
	if p.MaxPerRequest <= 0 || p.MaxTotal <= 0 {
		return errors.New("limits must be positive")
	}
	if p.ApprovalThreshold < 0 {
		return errors.New("approval threshold must not be negative")
	}
	if p.ApprovalThreshold > 0 {
		if p.ApprovalThreshold > p.MaxPerRequest {
			return fmt.Errorf("approval threshold %d exceeds per-request limit %d", p.ApprovalThreshold, p.MaxPerRequest)
		}
		if p.ApprovalWait <= 0 {
			return errors.New("approval wait must be positive when approval is enabled")
		}
	}
	if p.ApprovalWait < 0 || p.MaxReserveDuration < 0 {
		return errors.New("durations must not be negative")
	}
	if !p.StartsAt.std().Before(p.EndsAt.std()) {
		return errors.New("starts-at must be before ends-at")
	}
	if p.PayerAccountID == "" || len(p.AllowedAccountIDs) == 0 {
		return errors.New("payer account and at least one allowed account are required")
	}
	return nil
}

// validateRequestRef 校验请求的外键引用：账户、会话、策略均须存在；申请
// 会话必须属于使用账户；出资账户必须与策略一致；申请账户必须被策略允许。
func validateRequestRef(r requestBackupV1, i int, accounts map[string]struct{}, sessions map[string]*session, policies map[string]*policy) error {
	where := fmt.Sprintf("request[%d] %q/%q", i, r.AccountID, r.RequestID)
	if r.RequestID == "" || r.AccountID == "" || r.PolicyID == "" || r.SessionID == "" {
		return fmt.Errorf("%w: %s missing required id (account/request/policy/session)", ErrBackupInvalid, where)
	}
	if _, ok := accounts[r.AccountID]; !ok {
		return fmt.Errorf("%w: %s references unknown usage account %q", ErrBackupInvalid, where, r.AccountID)
	}
	if _, ok := accounts[r.PayerAccountID]; !ok {
		return fmt.Errorf("%w: %s references unknown payer account %q", ErrBackupInvalid, where, r.PayerAccountID)
	}
	sess, ok := sessions[r.SessionID]
	if !ok {
		return fmt.Errorf("%w: %s references unknown session %q", ErrBackupInvalid, where, r.SessionID)
	}
	// 申请会话必须属于使用账户。
	if sess.accountID != r.AccountID {
		return fmt.Errorf("%w: %s session %q belongs to account %q, not usage account %q", ErrBackupInvalid, where, r.SessionID, sess.accountID, r.AccountID)
	}
	p, ok := policies[r.PolicyID]
	if !ok {
		return fmt.Errorf("%w: %s references unknown policy %q", ErrBackupInvalid, where, r.PolicyID)
	}
	// 请求出资账户必须与策略出资账户一致。
	if r.PayerAccountID != p.payerAccountID {
		return fmt.Errorf("%w: %s payer account %q does not match policy %q payer %q", ErrBackupInvalid, where, r.PayerAccountID, r.PolicyID, p.payerAccountID)
	}
	if _, ok := p.allowedAccounts[r.AccountID]; !ok {
		return fmt.Errorf("%w: %s usage account %q is not allowed by policy %q", ErrBackupInvalid, where, r.AccountID, r.PolicyID)
	}
	return nil
}

// validateReserveTiming 校验预留计时三元组自洽：duration 必须非负；
// duration>0 时 reservedAt 必填且 reserveDeadline 必须等于
// reservedAt+duration；duration==0 时 reserveDeadline 必须为零。
// reservedAt 是否必填由各状态自行决定（待审批取消的请求两者都没有）。
func validateReserveTiming(r *request) error {
	if r.reserveDuration < 0 {
		return errors.New("reserve duration must not be negative")
	}
	if r.reserveDuration > 0 {
		if r.reservedAt.IsZero() {
			return errors.New("reserve duration set but reserved_at is missing")
		}
		want := r.reservedAt.Add(r.reserveDuration)
		if !r.reserveDeadline.Equal(want) {
			return fmt.Errorf("reserve deadline %v does not match reserved_at+duration %v", r.reserveDeadline, want)
		}
	} else if !r.reserveDeadline.IsZero() {
		return errors.New("reserve deadline set while reserve duration is zero")
	}
	return nil
}

// validateRequestTimingAndState 校验请求状态与其计时/金额字段自洽，并与
// 不可变的策略条件、申请会话保持一致，防止任意状态搭配任意时间戳的损坏
// 备份。sess 为该请求的申请会话。
func validateRequestTimingAndState(r *request, p *policy, sess *session) error {
	if r.createdAt.IsZero() {
		return errors.New("request missing created_at")
	}
	if r.operation == "" || r.payee == "" {
		return errors.New("operation and payee are required")
	}
	if r.operation != p.operation || r.payee != p.payee {
		return fmt.Errorf("operation/payee do not match policy %q", p.id)
	}
	if r.estimatedFee > p.maxPerRequest {
		return fmt.Errorf("estimated fee %d exceeds policy per-request limit %d", r.estimatedFee, p.maxPerRequest)
	}
	if r.approverAccountID != "" && r.approverAccountID != r.payerAccountID {
		return fmt.Errorf("approver account %q is not the payer %q", r.approverAccountID, r.payerAccountID)
	}
	if r.actualFee > r.estimatedFee {
		return fmt.Errorf("actual fee %d exceeds estimated fee %d", r.actualFee, r.estimatedFee)
	}

	// 申请是否必须走过审批由不可变策略门槛决定：费用严格超过正门槛的请求
	// 只能来自待审批流程；其余请求只能直接预留。
	approvalRequired := p.approvalThreshold > 0 && r.estimatedFee > p.approvalThreshold
	reservedOrigin := !r.reservedAt.IsZero()

	switch r.state {
	case RequestReserved, RequestSettled, RequestReservationExpired:
		// 这三类一定发生过预留。
		if err := validateReservedOrigin(r, p, approvalRequired, reservedOrigin); err != nil {
			return err
		}
	case RequestPendingApproval, RequestRejected, RequestExpired:
		// 这三类一定从未预留，且必须来自开启审批的策略。
		if err := validatePendingOrigin(r, p, approvalRequired, reservedOrigin); err != nil {
			return err
		}
	case RequestCancelled:
		// 取消可能来自待审批（从未预留）或已预留。
		if reservedOrigin {
			if err := validateReservedOrigin(r, p, approvalRequired, true); err != nil {
				return err
			}
		} else {
			if err := validatePendingOrigin(r, p, approvalRequired, false); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown state %d", r.state)
	}

	switch r.state {
	case RequestReserved:
		// 启用超时时 deadline 必须等于 reservedAt + 时长，未启用时为零，
		// 且不得已记录超时。
		if err := validateReserveTiming(r); err != nil {
			return err
		}
		if !r.reserveExpiredAt.IsZero() {
			return errors.New("reserved request must not carry reserve-expired time")
		}
		if r.actualFee != 0 || !r.settledAt.IsZero() {
			return errors.New("reserved request must not be settled")
		}
		if r.rejectReason != "" {
			return errors.New("reserved request must not carry a reject reason")
		}
	case RequestSettled:
		if r.settledAt.IsZero() {
			return errors.New("settled request missing settled_at")
		}
		if !r.reserveExpiredAt.IsZero() {
			return errors.New("settled request must not carry reserve-expired time")
		}
		if r.settledAt.Before(r.reservedAt) {
			return errors.New("settled_at is before reserved_at")
		}
		if err := validateReserveTiming(r); err != nil {
			return err
		}
		if r.rejectReason != "" {
			return errors.New("settled request must not carry a reject reason")
		}
	case RequestCancelled:
		if r.actualFee != 0 || !r.settledAt.IsZero() {
			return errors.New("cancelled request must not be settled")
		}
		if !r.reserveExpiredAt.IsZero() {
			return errors.New("cancelled request must not carry reserve-expired time")
		}
		if r.rejectReason != "" {
			return errors.New("cancelled request must not carry a reject reason")
		}
		if reservedOrigin {
			if err := validateReserveTiming(r); err != nil {
				return err
			}
		} else {
			// 待审批取消：决定时间必填。
			if r.decidedAt.IsZero() {
				return errors.New("cancelled-from-pending request missing decided_at")
			}
		}
	case RequestPendingApproval:
		// 待审批不冻结余额：不得有实际费用，且等待期限必须等于
		// min(提交+等待时长, 策略结束, 申请会话到期)。
		if r.actualFee != 0 || !r.settledAt.IsZero() {
			return errors.New("pending request must not be settled")
		}
		if r.waitDeadline.IsZero() {
			return errors.New("pending request missing wait deadline")
		}
		if r.approverAccountID != "" {
			return errors.New("pending request must not carry an approver")
		}
		if r.rejectReason != "" {
			return errors.New("pending request must not carry a reject reason")
		}
		want := r.createdAt.Add(p.approvalWait)
		if p.endsAt.Before(want) {
			want = p.endsAt
		}
		if sess.expiresAt.Before(want) {
			want = sess.expiresAt
		}
		if !r.waitDeadline.Equal(want) {
			return fmt.Errorf("wait deadline %v does not match min(created+wait, policy end, session expiry) %v", r.waitDeadline, want)
		}
	case RequestRejected:
		if r.decidedAt.IsZero() || r.rejectReason == "" {
			return errors.New("rejected request missing decided_at or reason")
		}
	case RequestExpired:
		if r.decidedAt.IsZero() {
			return errors.New("expired request missing decided_at")
		}
		if r.rejectReason != "" {
			return errors.New("expired request must not carry a reject reason")
		}
		// 惰性/停用过期的决定时刻不可能早于等待截止时刻。
		if r.decidedAt.Before(r.waitDeadline) {
			return errors.New("expired request decided before its wait deadline")
		}
	case RequestReservationExpired:
		if r.reserveDuration <= 0 {
			return errors.New("reservation-expired request must have a positive reserve duration")
		}
		if err := validateReserveTiming(r); err != nil {
			return err
		}
		if !r.reserveExpiredAt.Equal(r.reserveDeadline) {
			return errors.New("reserve-expired-at must equal reserve deadline")
		}
		if r.actualFee != 0 || !r.settledAt.IsZero() {
			return errors.New("reservation-expired request must not be settled")
		}
		if r.rejectReason != "" {
			return errors.New("reservation-expired request must not carry a reject reason")
		}
	}
	return nil
}

// validateReservedOrigin 校验“发生过预留”的请求：预留计时字段必须齐全、
// 预留时长快照必须与策略一致，且审批路径（批准人、决定时间）与费用/门槛
// 相匹配。对所有发生过预留的状态（已预留、已结算、预留超时、已预留后
// 取消）统一适用：超门槛的必须经出资账户在预留时刻批准，未超门槛的直接
// 受理、不得携带批准人或决定时间。
func validateReservedOrigin(r *request, p *policy, approvalRequired, reservedOrigin bool) error {
	if !reservedOrigin {
		return errors.New("request is in a reserved-origin state but missing reserved_at")
	}
	if r.reserveDuration != p.maxReserveDuration {
		return fmt.Errorf("reserve duration %v does not match policy max reserve duration %v", r.reserveDuration, p.maxReserveDuration)
	}
	if approvalRequired {
		if r.approverAccountID != r.payerAccountID {
			return errors.New("above-threshold request was reserved without payer approval")
		}
		if !r.decidedAt.Equal(r.reservedAt) {
			return errors.New("approved request decided_at must equal reserved_at")
		}
	} else {
		if r.approverAccountID != "" {
			return errors.New("below-threshold request must not carry an approver")
		}
		if !r.decidedAt.IsZero() {
			return errors.New("directly reserved request must not carry decided_at")
		}
	}
	return nil
}

// validatePendingOrigin 校验“从未预留”的待审批系请求：不得携带任何预留
// 计时字段，且策略必须开启审批、费用必须严格超过门槛。
func validatePendingOrigin(r *request, p *policy, approvalRequired, reservedOrigin bool) error {
	if reservedOrigin {
		return errors.New("request is in a pending-origin state but carries reserved_at")
	}
	if !approvalRequired {
		return errors.New("request is in an approval state although policy approval is not enabled for its fee")
	}
	if !r.reserveDeadline.IsZero() || !r.reserveExpiredAt.IsZero() || r.reserveDuration != 0 {
		return errors.New("pending-origin request must not carry reserve timing")
	}
	return nil
}
