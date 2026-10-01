package wallet

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// backupVersion 是备份格式的版本号。Restore 只接受当前版本；
// 不支持的版本号会被拒绝，返回具体原因。
const backupVersion = 1

// backupFile 是钱包导出的顶层 JSON 结构。带版本号，调用方可将其保存为
// 文本，并在下次运行时通过 Restore 恢复成独立的钱包。
type backupFile struct {
	Version  int             `json:"version"`
	Accounts []backupAccount `json:"accounts"`
	Sessions []backupSession `json:"sessions"`
	Policies []backupPolicy  `json:"policies"`
	Requests []backupRequest `json:"requests"`
	Ledger   []backupLedger  `json:"ledger"`
}

type backupAccount struct {
	ID        string    `json:"id"`
	Available int64     `json:"available"`
	Reserved  int64     `json:"reserved"`
	CreatedAt time.Time `json:"created_at"`
}

type backupSession struct {
	ID        string    `json:"id"`
	AccountID string    `json:"account_id"`
	DeviceID  string    `json:"device_id"`
	ExpiresAt time.Time `json:"expires_at"`
	Revoked   bool      `json:"revoked"`
	CreatedAt time.Time `json:"created_at"`
}

type backupPolicy struct {
	ID                   string    `json:"id"`
	PayerAccountID       string    `json:"payer_account_id"`
	AllowedAccountIDs    []string  `json:"allowed_account_ids"`
	Operation            string    `json:"operation"`
	Payee                string    `json:"payee"`
	StartsAt             time.Time `json:"starts_at"`
	EndsAt               time.Time `json:"ends_at"`
	MaxPerRequest        int64     `json:"max_per_request"`
	MaxTotal             int64     `json:"max_total"`
	ApprovalThreshold    int64     `json:"approval_threshold"`
	ApprovalWaitNS       int64     `json:"approval_wait_ns"`
	MaxReserveDurNS      int64     `json:"max_reserve_duration_ns"`
	ReservedTotal        int64     `json:"reserved_total"`
	SpentTotal           int64     `json:"spent_total"`
	Deactivated          bool      `json:"deactivated"`
	DeactivatedAt        time.Time `json:"deactivated_at"`
	DeactivatorAccountID string    `json:"deactivator_account_id"`
	DeactivateReason     string    `json:"deactivate_reason"`
}

type backupRequest struct {
	PolicyID          string       `json:"policy_id"`
	RequestID         string       `json:"request_id"`
	AccountID         string       `json:"account_id"`
	PayerAccountID    string       `json:"payer_account_id"`
	SessionID         string       `json:"session_id"`
	Operation         string       `json:"operation"`
	Payee             string       `json:"payee"`
	EstimatedFee      int64        `json:"estimated_fee"`
	ActualFee         int64        `json:"actual_fee"`
	State             RequestState `json:"state"`
	CreatedAt         time.Time    `json:"created_at"`
	SettledAt         time.Time    `json:"settled_at"`
	WaitDeadline       time.Time    `json:"wait_deadline"`
	ReservedAt        time.Time    `json:"reserved_at"`
	ReserveDurationNS int64        `json:"reserve_duration_ns"`
	ReserveDeadline   time.Time    `json:"reserve_deadline"`
	ReserveExpiredAt  time.Time    `json:"reserve_expired_at"`
	DecidedAt         time.Time    `json:"decided_at"`
	ApproverAccountID string       `json:"approver_account_id"`
	RejectReason      string       `json:"reject_reason"`
}

type backupLedger struct {
	Kind      LedgerKind `json:"kind"`
	AccountID string     `json:"account_id"`
	RequestID string     `json:"request_id"`
	PolicyID  string     `json:"policy_id"`
	Amount    int64      `json:"amount"`
	Reason    string     `json:"reason"`
	At        time.Time  `json:"at"`
}

// Export 将当前钱包导出为带版本号的 JSON 文本。
//
// 导出与申请、审批、吊销、停用、结算或取消并发发生时，备份对应一个完整
// 状态：在锁内先按导出时刻处理到期请求（待审批过期、已预留超时退回），
// 再快照全部账户、会话、策略、请求与账本，因此不会出现只有余额变化却
// 缺少对应请求或账本的情况。空钱包也能导出，导出不新增备份账本记录。
func (w *Wallet) Export() (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.exportLocked()
}

func (w *Wallet) exportLocked() (string, error) {
	now := w.now()
	// 先按导出时刻处理到期请求，使备份反映完整状态。
	w.expireReservationsLocked(now)
	for _, req := range w.requests {
		w.refreshPendingLocked(req, now)
	}

	bf := backupFile{Version: backupVersion}

	accts := make([]*account, 0, len(w.accounts))
	for _, a := range w.accounts {
		accts = append(accts, a)
	}
	sort.Slice(accts, func(i, j int) bool { return accts[i].id < accts[j].id })
	for _, a := range accts {
		bf.Accounts = append(bf.Accounts, backupAccount{
			ID: a.id, Available: a.available, Reserved: a.reserved, CreatedAt: a.createdAt,
		})
	}

	sessList := make([]*session, 0, len(w.sessions))
	for _, s := range w.sessions {
		sessList = append(sessList, s)
	}
	sort.Slice(sessList, func(i, j int) bool { return sessList[i].id < sessList[j].id })
	for _, s := range sessList {
		bf.Sessions = append(bf.Sessions, backupSession{
			ID: s.id, AccountID: s.accountID, DeviceID: s.deviceID,
			ExpiresAt: s.expiresAt, Revoked: s.revoked, CreatedAt: s.createdAt,
		})
	}

	polList := make([]*policy, 0, len(w.policies))
	for _, p := range w.policies {
		polList = append(polList, p)
	}
	sort.Slice(polList, func(i, j int) bool { return polList[i].id < polList[j].id })
	for _, p := range polList {
		ids := make([]string, 0, len(p.allowedAccounts))
		for id := range p.allowedAccounts {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		bf.Policies = append(bf.Policies, backupPolicy{
			ID: p.id, PayerAccountID: p.payerAccountID, AllowedAccountIDs: ids,
			Operation: p.operation, Payee: p.payee, StartsAt: p.startsAt, EndsAt: p.endsAt,
			MaxPerRequest: p.maxPerRequest, MaxTotal: p.maxTotal,
			ApprovalThreshold: p.approvalThreshold, ApprovalWaitNS: int64(p.approvalWait),
			MaxReserveDurNS: int64(p.maxReserveDuration),
			ReservedTotal: p.reservedTotal, SpentTotal: p.spentTotal,
			Deactivated: p.deactivated, DeactivatedAt: p.deactivatedAt,
			DeactivatorAccountID: p.deactivatorAccountID, DeactivateReason: p.deactivateReason,
		})
	}

	reqList := make([]*request, 0, len(w.requests))
	for _, r := range w.requests {
		reqList = append(reqList, r)
	}
	sort.Slice(reqList, func(i, j int) bool {
		if reqList[i].accountID != reqList[j].accountID {
			return reqList[i].accountID < reqList[j].accountID
		}
		return reqList[i].requestID < reqList[j].requestID
	})
	for _, r := range reqList {
		bf.Requests = append(bf.Requests, backupRequest{
			PolicyID: r.policyID, RequestID: r.requestID, AccountID: r.accountID,
			PayerAccountID: r.payerAccountID, SessionID: r.sessionID,
			Operation: r.operation, Payee: r.payee, EstimatedFee: r.estimatedFee,
			ActualFee: r.actualFee, State: r.state, CreatedAt: r.createdAt,
			SettledAt: r.settledAt, WaitDeadline: r.waitDeadline,
			ReservedAt: r.reservedAt, ReserveDurationNS: int64(r.reserveDuration),
			ReserveDeadline: r.reserveDeadline, ReserveExpiredAt: r.reserveExpiredAt,
			DecidedAt: r.decidedAt, ApproverAccountID: r.approverAccountID,
			RejectReason: r.rejectReason,
		})
	}

	// 账本按原顺序排列，不排序。
	for _, e := range w.ledger {
		bf.Ledger = append(bf.Ledger, backupLedger{
			Kind: e.Kind, AccountID: e.AccountID, RequestID: e.RequestID,
			PolicyID: e.PolicyID, Amount: e.Amount, Reason: e.Reason, At: e.At,
		})
	}

	data, err := json.Marshal(bf)
	if err != nil {
		return "", fmt.Errorf("wallet: failed to marshal backup: %w", err)
	}
	return string(data), nil
}

// Restore 从 Export 生成的 JSON 文本恢复出独立的钱包。
//
// 恢复会先校验备份的完整性与一致性：空文本、非法 JSON、缺少必需数据、
// 不支持的版本、编号重复、未知状态或账本类型、非法金额或策略参数、
// 引用不存在的账户/会话/策略、会话归属不符、请求出资账户与策略不符、
// 以及余额或额度合计与请求不一致（含求和超出 int64 范围）都会被拒绝，
// 不返回部分恢复的钱包。
//
// 校验通过后按恢复时刻处理到期请求（不重新计时）：待审批过期、已预留
// 超时退回，释放时间仍是原截止时刻。未到期请求继续等待或预留。恢复的
// 钱包与原钱包、从同一备份恢复的其他钱包互不影响。
func Restore(data string) (*Wallet, error) {
	return restore(data, time.Now)
}

// restore 是 Restore 的内部实现，允许指定时钟（同包测试使用）。
func restore(data string, nowFunc func() time.Time) (*Wallet, error) {
	if strings.TrimSpace(data) == "" {
		return nil, errors.New("wallet: empty backup data")
	}
	var bf backupFile
	if err := json.Unmarshal([]byte(data), &bf); err != nil {
		return nil, fmt.Errorf("wallet: invalid backup json: %w", err)
	}
	if bf.Version != backupVersion {
		return nil, fmt.Errorf("wallet: unsupported backup version %d, want %d", bf.Version, backupVersion)
	}

	w := New()
	w.now = nowFunc

	// 账户：编号唯一，金额非负。
	accountIDs := make(map[string]struct{}, len(bf.Accounts))
	for _, a := range bf.Accounts {
		if a.ID == "" {
			return nil, errors.New("wallet: account with empty id")
		}
		if _, ok := accountIDs[a.ID]; ok {
			return nil, fmt.Errorf("wallet: duplicate account id %s", a.ID)
		}
		if a.Available < 0 || a.Reserved < 0 {
			return nil, fmt.Errorf("wallet: account %s has negative balance", a.ID)
		}
		accountIDs[a.ID] = struct{}{}
		w.accounts[a.ID] = &account{
			id: a.ID, available: a.Available, reserved: a.Reserved, createdAt: a.CreatedAt,
		}
	}

	// 会话：编号唯一，引用的账户存在，设备编号非空。
	sessionIDs := make(map[string]struct{}, len(bf.Sessions))
	for _, s := range bf.Sessions {
		if s.ID == "" {
			return nil, errors.New("wallet: session with empty id")
		}
		if _, ok := sessionIDs[s.ID]; ok {
			return nil, fmt.Errorf("wallet: duplicate session id %s", s.ID)
		}
		if _, ok := accountIDs[s.AccountID]; !ok {
			return nil, fmt.Errorf("wallet: session %s references missing account %s", s.ID, s.AccountID)
		}
		if s.DeviceID == "" {
			return nil, fmt.Errorf("wallet: session %s has empty device id", s.ID)
		}
		sessionIDs[s.ID] = struct{}{}
		w.sessions[s.ID] = &session{
			id: s.ID, accountID: s.AccountID, deviceID: s.DeviceID,
			expiresAt: s.ExpiresAt, revoked: s.Revoked, createdAt: s.CreatedAt,
		}
	}

	// 策略：编号唯一，参数合法，引用的账户存在。
	policyIDs := make(map[string]struct{}, len(bf.Policies))
	for _, p := range bf.Policies {
		if err := validateBackupPolicy(p, accountIDs, policyIDs); err != nil {
			return nil, err
		}
		policyIDs[p.ID] = struct{}{}
		allowed := make(map[string]struct{}, len(p.AllowedAccountIDs))
		for _, id := range p.AllowedAccountIDs {
			allowed[id] = struct{}{}
		}
		w.policies[p.ID] = &policy{
			id: p.ID, payerAccountID: p.PayerAccountID, allowedAccounts: allowed,
			operation: p.Operation, payee: p.Payee, startsAt: p.StartsAt, endsAt: p.EndsAt,
			maxPerRequest: p.MaxPerRequest, maxTotal: p.MaxTotal,
			approvalThreshold: p.ApprovalThreshold,
			approvalWait:      time.Duration(p.ApprovalWaitNS),
			maxReserveDuration: time.Duration(p.MaxReserveDurNS),
			reservedTotal:     p.ReservedTotal, spentTotal: p.SpentTotal,
			deactivated: p.Deactivated, deactivatedAt: p.DeactivatedAt,
			deactivatorAccountID: p.DeactivatorAccountID, deactivateReason: p.DeactivateReason,
		}
	}

	// 请求：同一使用账户下编号唯一，引用的账户/会话/策略存在，
	// 会话归属使用账户，出资账户与策略一致，金额与状态合法。
	requestKeys := make(map[requestKey]struct{}, len(bf.Requests))
	for _, r := range bf.Requests {
		if err := validateBackupRequest(r, w, requestKeys); err != nil {
			return nil, err
		}
		requestKeys[requestKey{accountID: r.AccountID, requestID: r.RequestID}] = struct{}{}
		w.requests[requestKey{accountID: r.AccountID, requestID: r.RequestID}] = &request{
			policyID: r.PolicyID, requestID: r.RequestID, accountID: r.AccountID,
			payerAccountID: r.PayerAccountID, sessionID: r.SessionID,
			operation: r.Operation, payee: r.Payee, estimatedFee: r.EstimatedFee,
			actualFee: r.ActualFee, state: r.State, createdAt: r.CreatedAt,
			settledAt: r.SettledAt, waitDeadline: r.WaitDeadline,
			reservedAt: r.ReservedAt, reserveDuration: time.Duration(r.ReserveDurationNS),
			reserveDeadline: r.ReserveDeadline, reserveExpiredAt: r.ReserveExpiredAt,
			decidedAt: r.DecidedAt, approverAccountID: r.ApproverAccountID,
			rejectReason: r.RejectReason,
		}
	}

	// 账本：类型已知，金额非负，必填字段齐全，引用的账户存在。
	for _, e := range bf.Ledger {
		if err := validateBackupLedgerEntry(e, accountIDs); err != nil {
			return nil, err
		}
		w.ledger = append(w.ledger, LedgerEntry{
			Kind: e.Kind, AccountID: e.AccountID, RequestID: e.RequestID,
			PolicyID: e.PolicyID, Amount: e.Amount, Reason: e.Reason, At: e.At,
		})
	}

	// 一致性：账户预留余额、策略预留/已花费总额与请求逐笔核对。
	if err := validateBackupConsistency(w); err != nil {
		return nil, err
	}

	// 按恢复时刻处理到期请求（不重新计时）：待审批过期、已预留超时退回。
	now := nowFunc()
	w.expireReservationsLocked(now)
	for _, req := range w.requests {
		w.refreshPendingLocked(req, now)
	}

	return w, nil
}

// validateBackupPolicy 校验备份中的一条策略：编号唯一、参数合法、
// 引用的出资与使用账户均存在。
func validateBackupPolicy(p backupPolicy, accountIDs, policyIDs map[string]struct{}) error {
	if p.ID == "" {
		return errors.New("wallet: policy with empty id")
	}
	if _, ok := policyIDs[p.ID]; ok {
		return fmt.Errorf("wallet: duplicate policy id %s", p.ID)
	}
	if p.Operation == "" || p.Payee == "" {
		return fmt.Errorf("wallet: policy %s missing operation or payee", p.ID)
	}
	if p.MaxPerRequest <= 0 || p.MaxTotal <= 0 {
		return fmt.Errorf("wallet: policy %s limits must be positive", p.ID)
	}
	if p.ApprovalThreshold < 0 {
		return fmt.Errorf("wallet: policy %s approval threshold must not be negative", p.ID)
	}
	if p.ApprovalThreshold > 0 {
		if p.ApprovalThreshold > p.MaxPerRequest {
			return fmt.Errorf("wallet: policy %s approval threshold %d exceeds per-request limit %d", p.ID, p.ApprovalThreshold, p.MaxPerRequest)
		}
		if p.ApprovalWaitNS <= 0 {
			return fmt.Errorf("wallet: policy %s approval wait must be positive when approval is enabled", p.ID)
		}
	}
	if p.MaxReserveDurNS < 0 {
		return fmt.Errorf("wallet: policy %s max reserve duration must not be negative", p.ID)
	}
	if !p.StartsAt.Before(p.EndsAt) {
		return fmt.Errorf("wallet: policy %s starts-at must be before ends-at", p.ID)
	}
	if p.PayerAccountID == "" || len(p.AllowedAccountIDs) == 0 {
		return fmt.Errorf("wallet: policy %s missing payer account or allowed accounts", p.ID)
	}
	if _, ok := accountIDs[p.PayerAccountID]; !ok {
		return fmt.Errorf("wallet: policy %s references missing payer account %s", p.ID, p.PayerAccountID)
	}
	for _, id := range p.AllowedAccountIDs {
		if id == "" {
			return fmt.Errorf("wallet: policy %s has empty allowed account id", p.ID)
		}
		if _, ok := accountIDs[id]; !ok {
			return fmt.Errorf("wallet: policy %s references missing allowed account %s", p.ID, id)
		}
	}
	if p.Deactivated && p.DeactivatedAt.IsZero() {
		return fmt.Errorf("wallet: policy %s is deactivated but missing deactivated-at", p.ID)
	}
	if p.ReservedTotal < 0 || p.SpentTotal < 0 {
		return fmt.Errorf("wallet: policy %s has negative totals", p.ID)
	}
	return nil
}

// validateBackupRequest 校验备份中的一笔请求：必填字段齐全、同一使用账户下
// 编号唯一、引用的账户/会话/策略存在、会话归属使用账户、出资账户与策略
// 一致、金额与状态合法。
func validateBackupRequest(r backupRequest, w *Wallet, requestKeys map[requestKey]struct{}) error {
	if r.PolicyID == "" || r.RequestID == "" || r.AccountID == "" || r.SessionID == "" ||
		r.Operation == "" || r.Payee == "" {
		return errors.New("wallet: request missing required fields")
	}
	key := requestKey{accountID: r.AccountID, requestID: r.RequestID}
	if _, ok := requestKeys[key]; ok {
		return fmt.Errorf("wallet: duplicate request %s for account %s", r.RequestID, r.AccountID)
	}
	if _, ok := w.accounts[r.AccountID]; !ok {
		return fmt.Errorf("wallet: request %s references missing account %s", r.RequestID, r.AccountID)
	}
	sess, ok := w.sessions[r.SessionID]
	if !ok {
		return fmt.Errorf("wallet: request %s references missing session %s", r.RequestID, r.SessionID)
	}
	if sess.accountID != r.AccountID {
		return fmt.Errorf("wallet: request %s session %s belongs to account %s, not %s",
			r.RequestID, r.SessionID, sess.accountID, r.AccountID)
	}
	pol, ok := w.policies[r.PolicyID]
	if !ok {
		return fmt.Errorf("wallet: request %s references missing policy %s", r.RequestID, r.PolicyID)
	}
	if r.PayerAccountID != pol.payerAccountID {
		return fmt.Errorf("wallet: request %s payer account %s does not match policy payer %s",
			r.RequestID, r.PayerAccountID, pol.payerAccountID)
	}
	if r.EstimatedFee <= 0 {
		return fmt.Errorf("wallet: request %s estimated fee must be positive", r.RequestID)
	}
	if r.ActualFee < 0 {
		return fmt.Errorf("wallet: request %s actual fee must not be negative", r.RequestID)
	}
	if r.ReserveDurationNS < 0 {
		return fmt.Errorf("wallet: request %s reserve duration must not be negative", r.RequestID)
	}
	if !isValidRequestState(r.State) {
		return fmt.Errorf("wallet: request %s has unknown state %d", r.RequestID, r.State)
	}
	return nil
}

// isValidRequestState 判断请求状态是否为已知的合法状态。
func isValidRequestState(s RequestState) bool {
	switch s {
	case RequestReserved, RequestSettled, RequestCancelled, RequestPendingApproval,
		RequestRejected, RequestExpired, RequestReservationExpired:
		return true
	}
	return false
}

// validateBackupLedgerEntry 校验备份中的一条账本记录：类型已知、金额非负、
// 必填字段齐全、引用的账户存在。
func validateBackupLedgerEntry(e backupLedger, accountIDs map[string]struct{}) error {
	if !isValidLedgerKind(e.Kind) {
		return fmt.Errorf("wallet: unknown ledger kind %d", e.Kind)
	}
	if e.AccountID == "" {
		return errors.New("wallet: ledger entry missing account id")
	}
	if _, ok := accountIDs[e.AccountID]; !ok {
		return fmt.Errorf("wallet: ledger entry references missing account %s", e.AccountID)
	}
	if e.Amount < 0 {
		return fmt.Errorf("wallet: ledger entry has negative amount %d", e.Amount)
	}
	switch e.Kind {
	case LedgerPolicyDeactivation:
		if e.PolicyID == "" {
			return errors.New("wallet: policy deactivation ledger entry missing policy id")
		}
		if e.RequestID != "" {
			return errors.New("wallet: policy deactivation ledger entry must not have request id")
		}
	default:
		if e.RequestID == "" {
			return errors.New("wallet: ledger entry missing request id")
		}
	}
	return nil
}

// isValidLedgerKind 判断账本类型是否为已知的合法类型。
func isValidLedgerKind(k LedgerKind) bool {
	switch k {
	case LedgerReserve, LedgerSettle, LedgerRefund, LedgerRejection, LedgerPendingApproval,
		LedgerApproval, LedgerCancellation, LedgerExpiration, LedgerPolicyDeactivation,
		LedgerReservationExpiration:
		return true
	}
	return false
}

// validateBackupConsistency 校验恢复后钱包的内部一致性：
//   - 账户预留余额等于其作为出资账户的已预留请求预估费用之和；
//   - 策略预留总额等于该策略已预留请求的预估费用之和；
//   - 策略已花费总额等于该策略已结算请求的实际费用之和。
//
// 任一不一致或求和超出 int64 范围都返回错误，不通过改写金额掩盖损坏。
func validateBackupConsistency(w *Wallet) error {
	for _, a := range w.accounts {
		var sum int64
		for _, req := range w.requests {
			if req.state == RequestReserved && req.payerAccountID == a.id {
				if sum > math.MaxInt64-req.estimatedFee {
					return fmt.Errorf("wallet: account %s reserved balance sum overflows int64", a.id)
				}
				sum += req.estimatedFee
			}
		}
		if sum != a.reserved {
			return fmt.Errorf("wallet: account %s reserved balance %d does not match sum of reserved requests %d",
				a.id, a.reserved, sum)
		}
	}
	for _, p := range w.policies {
		var reservedSum, spentSum int64
		for _, req := range w.requests {
			if req.policyID != p.id {
				continue
			}
			if req.state == RequestReserved {
				if reservedSum > math.MaxInt64-req.estimatedFee {
					return fmt.Errorf("wallet: policy %s reserved total sum overflows int64", p.id)
				}
				reservedSum += req.estimatedFee
			}
			if req.state == RequestSettled {
				if spentSum > math.MaxInt64-req.actualFee {
					return fmt.Errorf("wallet: policy %s spent total sum overflows int64", p.id)
				}
				spentSum += req.actualFee
			}
		}
		if reservedSum != p.reservedTotal {
			return fmt.Errorf("wallet: policy %s reserved total %d does not match sum of reserved requests %d",
				p.id, p.reservedTotal, reservedSum)
		}
		if spentSum != p.spentTotal {
			return fmt.Errorf("wallet: policy %s spent total %d does not match sum of settled requests %d",
				p.id, p.spentTotal, spentSum)
		}
	}
	return nil
}
