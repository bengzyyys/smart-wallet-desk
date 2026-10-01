package wallet

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Wallet 是进程内的智能钱包。
//
// 所有方法都可被多个 goroutine 并发调用；每一次申请、结算或取消都在
// 同一把锁内原子完成，因此余额与共享累计额度不会被超扣，同一请求也
// 只会产生一个终态。状态仅在当前 Wallet 实例的生命周期内保留。
type Wallet struct {
	mu sync.Mutex

	// now 可在同包测试中替换，默认使用 time.Now。
	now func() time.Time

	accounts map[string]*account
	sessions map[string]*session
	policies map[string]*policy
	// requests 以“使用账户编号 + 请求编号”为键，实现按使用账户的幂等。
	requests map[requestKey]*request
	ledger   []LedgerEntry
}

type account struct {
	id        string
	available int64
	reserved  int64
	createdAt time.Time
}

type session struct {
	id        string
	accountID string
	deviceID  string
	expiresAt time.Time
	revoked   bool
	createdAt time.Time
}

type policy struct {
	id                string
	payerAccountID    string
	allowedAccounts   map[string]struct{}
	operation         string
	payee             string
	startsAt          time.Time
	endsAt            time.Time
	maxPerRequest     int64
	maxTotal          int64
	approvalThreshold int64
	approvalWait      time.Duration
	reservedTotal     int64
	spentTotal        int64
}

type request struct {
	policyID       string
	requestID      string
	accountID      string
	payerAccountID string
	operation      string
	payee          string
	sessionID      string
	estimatedFee   int64
	actualFee      int64
	state          RequestState
	createdAt      time.Time
	// waitUntil 为待审批期限；decidedAt 为批准/拒绝/过期的决定时间。
	waitUntil    time.Time
	decidedAt    time.Time
	approver     string
	rejectReason string
	settledAt    time.Time
}

type requestKey struct {
	accountID string
	requestID string
}

// New 创建一个空钱包。
func New() *Wallet {
	return &Wallet{
		now:      time.Now,
		accounts: make(map[string]*account),
		sessions: make(map[string]*session),
		policies: make(map[string]*policy),
		requests: make(map[requestKey]*request),
	}
}

// CreateAccount 创建具有唯一编号和初始代付余额的账户。
//
// 金额为最小货币单位的整数；初始余额为负或编号重复时返回错误，
// 且不会改动任何已有资金记录。
func (w *Wallet) CreateAccount(id string, initialBalance int64) (AccountView, error) {
	if id == "" {
		return AccountView{}, ErrEmptyAccountID
	}
	if initialBalance < 0 {
		return AccountView{}, fmt.Errorf("%w: initial balance must not be negative", ErrInvalidAmount)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if _, ok := w.accounts[id]; ok {
		return AccountView{}, fmt.Errorf("%w: %s", ErrAccountExists, id)
	}
	now := w.now()
	acc := &account{id: id, available: initialBalance, createdAt: now}
	w.accounts[id] = acc
	return acc.view(), nil
}

// Account 查询账户视图；账户不存在时返回 ErrAccountNotFound。
func (w *Wallet) Account(id string) (AccountView, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	acc, ok := w.accounts[id]
	if !ok {
		return AccountView{}, fmt.Errorf("%w: %s", ErrAccountNotFound, id)
	}
	return acc.view(), nil
}

// Balance 查询账户的可用余额与预留余额。
func (w *Wallet) Balance(id string) (Balances, error) {
	view, err := w.Account(id)
	if err != nil {
		return Balances{}, err
	}
	return view.Balances, nil
}

// CreateSession 为账户创建一个绑定设备、带到期时间的会话。
//
// 会话编号全局唯一；账户必须存在。
func (w *Wallet) CreateSession(id, accountID, deviceID string, expiresAt time.Time) (SessionView, error) {
	if id == "" || accountID == "" || deviceID == "" {
		return SessionView{}, fmt.Errorf("%w: session id, account id and device id are required", ErrInvalidArgument)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if _, ok := w.accounts[accountID]; !ok {
		return SessionView{}, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	if _, ok := w.sessions[id]; ok {
		return SessionView{}, fmt.Errorf("%w: %s", ErrSessionExists, id)
	}
	now := w.now()
	sess := &session{
		id:        id,
		accountID: accountID,
		deviceID:  deviceID,
		expiresAt: expiresAt,
		createdAt: now,
	}
	w.sessions[id] = sess
	return sess.view(), nil
}

// RevokeSession 主动吊销会话。会话不存在返回错误；重复吊销视为成功。
// 吊销后不再受理该会话的新申请，已预留费用的请求仍可结算或取消；
// 仍在待审批的请求立即进入拒绝终态并说明会话已吊销。
func (w *Wallet) RevokeSession(id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	sess, ok := w.sessions[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	now := w.now()
	alreadyRevoked := sess.revoked
	sess.revoked = true
	if !alreadyRevoked {
		// 首次吊销：以该会话提交且仍在待审批的请求全部拒绝留痕；
		// 已批准并预留的请求不受影响，仍可按原规则结算或取消。
		reason := fmt.Errorf("%w: application session %s revoked before approval", ErrSessionRevoked, id).Error()
		for _, req := range w.requests {
			if req.sessionID == id && req.state == RequestPendingApproval {
				// 系统流转而非审批人决定，审批账户留空。
				w.transitionToRejectedLocked(req, now, "", reason)
			}
		}
	}
	return nil
}

// Session 查询会话视图。
func (w *Wallet) Session(id string) (SessionView, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	sess, ok := w.sessions[id]
	if !ok {
		return SessionView{}, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	return sess.view(), nil
}

// SavePolicy 保存（创建）一条代付策略。
//
// 出资账户和所有使用账户必须存在，限额必须为正，开始时间必须严格早于
// 结束时间，编号不得重复，否则拒绝保存并返回包装了具体原因的错误。
//
// 大额审批为可选项：ApprovalThreshold 为零表示关闭审批，申请沿用直接
// 预留的原有行为；为正时必须不超过 MaxPerRequest，且 ApprovalWait 必须
// 为正，预估费用严格超过门槛的申请先进入待审批；为负一律拒绝保存。
func (w *Wallet) SavePolicy(spec PolicySpec) error {
	if spec.ID == "" {
		return fmt.Errorf("%w: policy id is required", ErrPolicyInvalid)
	}
	if spec.Operation == "" || spec.Payee == "" {
		return fmt.Errorf("%w: operation and payee are required", ErrPolicyInvalid)
	}
	if spec.MaxPerRequest <= 0 || spec.MaxTotal <= 0 {
		return fmt.Errorf("%w: limits must be positive", ErrPolicyInvalid)
	}
	if !spec.StartsAt.Before(spec.EndsAt) {
		return fmt.Errorf("%w: starts-at must be before ends-at", ErrPolicyInvalid)
	}
	if spec.PayerAccountID == "" || len(spec.AllowedAccountIDs) == 0 {
		return fmt.Errorf("%w: payer account and at least one allowed account are required", ErrPolicyInvalid)
	}
	if spec.ApprovalThreshold < 0 {
		return fmt.Errorf("%w: approval threshold must not be negative", ErrPolicyInvalid)
	}
	if spec.ApprovalThreshold > spec.MaxPerRequest {
		return fmt.Errorf("%w: approval threshold %d must not exceed per-request limit %d", ErrPolicyInvalid, spec.ApprovalThreshold, spec.MaxPerRequest)
	}
	if spec.ApprovalThreshold > 0 && spec.ApprovalWait <= 0 {
		return fmt.Errorf("%w: approval wait must be positive when approval is enabled", ErrPolicyInvalid)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if _, ok := w.policies[spec.ID]; ok {
		return fmt.Errorf("%w: %s", ErrPolicyExists, spec.ID)
	}
	if _, ok := w.accounts[spec.PayerAccountID]; !ok {
		return fmt.Errorf("%w: payer account %q: %w", ErrPolicyInvalid, spec.PayerAccountID, ErrAccountNotFound)
	}
	allowed := make(map[string]struct{}, len(spec.AllowedAccountIDs))
	for _, id := range spec.AllowedAccountIDs {
		if _, ok := w.accounts[id]; !ok {
			return fmt.Errorf("%w: allowed account %q: %w", ErrPolicyInvalid, id, ErrAccountNotFound)
		}
		allowed[id] = struct{}{}
	}

	w.policies[spec.ID] = &policy{
		id:                spec.ID,
		payerAccountID:    spec.PayerAccountID,
		allowedAccounts:   allowed,
		operation:         spec.Operation,
		payee:             spec.Payee,
		startsAt:          spec.StartsAt,
		endsAt:            spec.EndsAt,
		maxPerRequest:     spec.MaxPerRequest,
		maxTotal:          spec.MaxTotal,
		approvalThreshold: spec.ApprovalThreshold,
		approvalWait:      spec.ApprovalWait,
	}
	return nil
}

// Policy 查询策略视图，含当前预留中与已结算的累计金额。
func (w *Wallet) Policy(id string) (PolicyView, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	p, ok := w.policies[id]
	if !ok {
		return PolicyView{}, fmt.Errorf("%w: %s", ErrPolicyNotFound, id)
	}
	return p.view(), nil
}

// Apply 提交一笔代付申请。
//
// 校验顺序为：申请字段 -> 使用账户 -> 会话（存在性、归属、设备、吊销、
// 到期）-> 幂等请求编号 -> 策略授权与额度 -> 出资账户余额。任一环节
// 失败都会在账本中留下带具体原因的拒绝记录，不产生任何扣款或预留。
//
// 同一使用账户以相同请求编号重复申请：策略、操作类型、收款方和预估
// 费用一致时直接返回已有请求（不再预留）；任一不同则返回 ErrConflict。
func (w *Wallet) Apply(in RequestInput) (RequestView, error) {
	if in.EstimatedFee <= 0 {
		return RequestView{}, w.reject(in, fmt.Errorf("%w: estimated fee must be positive", ErrInvalidAmount))
	}
	if in.AccountID == "" || in.SessionID == "" || in.DeviceID == "" || in.RequestID == "" || in.PolicyID == "" {
		return RequestView{}, w.reject(in, fmt.Errorf("%w: account, session, device, policy and request id are required", ErrInvalidArgument))
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.now()

	acc, ok := w.accounts[in.AccountID]
	if !ok {
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("%w: %s", ErrAccountNotFound, in.AccountID))
	}
	_ = acc

	sess, ok := w.sessions[in.SessionID]
	if !ok {
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("%w: %s", ErrSessionNotFound, in.SessionID))
	}
	if sess.accountID != in.AccountID {
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("%w: session %s belongs to account %s", ErrSessionAccountMismatch, sess.id, sess.accountID))
	}
	if sess.deviceID != in.DeviceID {
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("%w: session %s is bound to device %s", ErrSessionDeviceMismatch, sess.id, sess.deviceID))
	}
	if sess.revoked {
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("%w: session %s", ErrSessionRevoked, sess.id))
	}
	// 到期时刻及之后一律视为过期：[零值, ExpiresAt) 才有效。
	if !now.Before(sess.expiresAt) {
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("%w: session %s expired at %s", ErrSessionExpired, sess.id, sess.expiresAt.Format(time.RFC3339)))
	}

	key := requestKey{accountID: in.AccountID, requestID: in.RequestID}
	if existing, ok := w.requests[key]; ok {
		if existing.policyID == in.PolicyID &&
			existing.operation == in.Operation &&
			existing.payee == in.Payee &&
			existing.estimatedFee == in.EstimatedFee {
			// 幂等命中：先把已到期的待审批请求流转为过期，再返回已有
			// 结果；待审批或任何终态请求都不会被重新创建或再次预留。
			w.expireIfDueLocked(existing, now)
			return existing.view(), nil
		}
		return RequestView{}, fmt.Errorf("%w: account %s request %s was already submitted with different content", ErrConflict, in.AccountID, in.RequestID)
	}

	p, ok := w.policies[in.PolicyID]
	if !ok {
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("%w: %s", ErrPolicyNotFound, in.PolicyID))
	}
	if _, ok := p.allowedAccounts[in.AccountID]; !ok {
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("%w: account %s is not allowed by policy %s", ErrPolicyDenied, in.AccountID, p.id))
	}
	if in.Operation != p.operation {
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("%w: operation %q does not match policy %q", ErrPolicyDenied, in.Operation, p.operation))
	}
	if in.Payee != p.payee {
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("%w: payee %q does not match policy %q", ErrPolicyDenied, in.Payee, p.payee))
	}
	// 时间窗含开始、不含结束：[StartsAt, EndsAt)。
	if now.Before(p.startsAt) || !now.Before(p.endsAt) {
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("%w: current time %s is outside policy window [%s, %s)", ErrPolicyDenied,
			now.Format(time.RFC3339), p.startsAt.Format(time.RFC3339), p.endsAt.Format(time.RFC3339)))
	}
	if in.EstimatedFee > p.maxPerRequest {
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("%w: estimated fee %d exceeds per-request limit %d", ErrQuotaExceeded, in.EstimatedFee, p.maxPerRequest))
	}
	// 预留中的费用同样占用共享累计额度。
	if p.reservedTotal+p.spentTotal+in.EstimatedFee > p.maxTotal {
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("%w: estimated fee %d would exceed shared total limit %d (used %d)",
			ErrQuotaExceeded, in.EstimatedFee, p.maxTotal, p.reservedTotal+p.spentTotal))
	}

	payer, ok := w.accounts[p.payerAccountID]
	if !ok {
		// 保存策略时已校验，这里仅作防御。
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("payer account %s: %w", p.payerAccountID, ErrAccountNotFound))
	}
	if payer.available < in.EstimatedFee {
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("%w: payer %s available %d, need %d", ErrInsufficientBalance, payer.id, payer.available, in.EstimatedFee))
	}

	req := &request{
		policyID:       p.id,
		requestID:      in.RequestID,
		accountID:      in.AccountID,
		payerAccountID: payer.id,
		operation:      in.Operation,
		payee:          in.Payee,
		sessionID:      in.SessionID,
		estimatedFee:   in.EstimatedFee,
		createdAt:      now,
	}

	// 通过会话、授权、余额与全部硬限额检查后，按审批门槛分流：
	// 审批关闭或预估费用未严格超过门槛的，直接预留（原有行为）；
	// 其余进入待审批，不冻结余额、不占用共享累计额度。
	if p.approvalThreshold > 0 && in.EstimatedFee > p.approvalThreshold {
		req.state = RequestPendingApproval
		req.waitUntil = earliestTime(now.Add(p.approvalWait), p.endsAt, sess.expiresAt)
		w.requests[key] = req
		w.ledger = append(w.ledger, LedgerEntry{
			Kind:           LedgerPendingApproval,
			UsageAccountID: in.AccountID,
			RequestID:      in.RequestID,
			Reason:         "request pending large-amount approval",
			At:             now,
		})
		return req.view(), nil
	}

	w.reserveLocked(req, p, payer, now, "reserve estimated fee")
	w.requests[key] = req
	return req.view(), nil
}

// reserveLocked 在持锁状态下一次性预留请求的全部预估费用，并写入预留
// 资金记录。直接受理与审批通过后的预留共用本路径。
func (w *Wallet) reserveLocked(req *request, p *policy, payer *account, at time.Time, reason string) {
	payer.available -= req.estimatedFee
	payer.reserved += req.estimatedFee
	p.reservedTotal += req.estimatedFee
	req.state = RequestReserved
	w.ledger = append(w.ledger, LedgerEntry{
		Kind:           LedgerReserve,
		AccountID:      payer.id,
		UsageAccountID: req.accountID,
		RequestID:      req.requestID,
		Amount:         req.estimatedFee,
		Reason:         reason,
		At:             at,
	})
}

// earliestTime 返回若干非零时刻中的最早值。
func earliestTime(ts ...time.Time) time.Time {
	var out time.Time
	for i, t := range ts {
		if i == 0 || t.Before(out) {
			out = t
		}
	}
	return out
}

// Approve 批准一笔待审批的代付请求。
//
// 必须使用策略出资账户绑定当前设备的有效会话（账户匹配、会话存在且归
// 属该账户、设备一致、未吊销且未到期），否则返回 ErrApprovalUnauthorized
// 且不改变请求。批准时会再次检查出资账户可用余额与策略共享剩余额度：
// 二者都足够才一次性预留全部预估费用并转为 RequestReserved；不足时返回
// 包装了 ErrInsufficientBalance 或 ErrQuotaExceeded 的具体原因，请求保留
// 待审批状态，期限内可再次批准。到达等待期限（含时刻本身）不能批准。
//
// 已获批并处于预留或结算状态的请求重复批准只幂等返回当前结果，不再
// 预留；已拒绝、过期或取消的请求不能再批准。
func (w *Wallet) Approve(accountID, requestID string, in ApprovalInput) (RequestView, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	req, p, err := w.lookupRequest(accountID, requestID)
	if err != nil {
		return RequestView{}, err
	}
	now := w.now()

	switch req.state {
	case RequestReserved, RequestSettled:
		// 已获批：只返回当前结果，不得再次预留。
		return req.view(), nil
	case RequestExpired:
		return RequestView{}, fmt.Errorf("%w: request %s expired at %s", ErrApprovalDeadline, requestID, req.waitUntil.Format(time.RFC3339))
	case RequestRejected:
		return RequestView{}, fmt.Errorf("%w: request %s was rejected", ErrRequestNotPending, requestID)
	case RequestCancelled:
		return RequestView{}, fmt.Errorf("%w: request %s was cancelled", ErrRequestNotPending, requestID)
	}

	// 待审批：先看期限，再校验审批身份。
	if !now.Before(req.waitUntil) {
		// lookupRequest 通常已完成流转，这里作边界兜底。
		w.transitionToExpiredLocked(req, now)
		return RequestView{}, fmt.Errorf("%w: request %s expired at %s", ErrApprovalDeadline, requestID, req.waitUntil.Format(time.RFC3339))
	}
	if err := w.checkApproverLocked(req, in, now); err != nil {
		return RequestView{}, err
	}

	payer := w.accounts[req.payerAccountID]
	if payer.available < req.estimatedFee {
		return RequestView{}, fmt.Errorf("%w: payer %s available %d, need %d", ErrInsufficientBalance, payer.id, payer.available, req.estimatedFee)
	}
	if p.reservedTotal+p.spentTotal+req.estimatedFee > p.maxTotal {
		return RequestView{}, fmt.Errorf("%w: fee %d would exceed shared total limit %d (used %d)",
			ErrQuotaExceeded, req.estimatedFee, p.maxTotal, p.reservedTotal+p.spentTotal)
	}

	// 复查通过：批准状态留痕后一次性预留全部预估费用。
	req.decidedAt = now
	req.approver = in.ApproverAccountID
	w.ledger = append(w.ledger, LedgerEntry{
		Kind:           LedgerApproval,
		UsageAccountID: req.accountID,
		RequestID:      req.requestID,
		Reason:         "pending request approved",
		At:             now,
	})
	w.reserveLocked(req, p, payer, now, "reserve estimated fee on approval")
	return req.view(), nil
}

// Reject 拒绝一笔待审批的代付请求。
//
// 审批身份要求与 Approve 相同；reason 不能为空或全为空白，否则返回
// ErrInvalidArgument 且请求保持不变。拒绝不产生退款，请求进入
// RequestRejected 终态并记录原因。
//
// 以相同理由重复拒绝幂等返回、不重复留痕；理由不同返回 ErrConflict。
// 已批准（预留或结算）、已取消的请求不能再拒绝；已过期的请求按过期
// 处理。
func (w *Wallet) Reject(accountID, requestID string, in ApprovalInput) (RequestView, error) {
	if strings.TrimSpace(in.Reason) == "" {
		return RequestView{}, fmt.Errorf("%w: reject reason must not be blank", ErrInvalidArgument)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	req, _, err := w.lookupRequest(accountID, requestID)
	if err != nil {
		return RequestView{}, err
	}
	now := w.now()

	switch req.state {
	case RequestRejected:
		if req.rejectReason == in.Reason {
			return req.view(), nil
		}
		return RequestView{}, fmt.Errorf("%w: request %s was rejected with reason %q", ErrConflict, requestID, req.rejectReason)
	case RequestExpired:
		return RequestView{}, fmt.Errorf("%w: request %s expired at %s", ErrApprovalDeadline, requestID, req.waitUntil.Format(time.RFC3339))
	case RequestReserved, RequestSettled:
		return RequestView{}, fmt.Errorf("%w: request %s was already approved", ErrRequestNotPending, requestID)
	case RequestCancelled:
		return RequestView{}, fmt.Errorf("%w: request %s was cancelled", ErrRequestNotPending, requestID)
	}

	if !now.Before(req.waitUntil) {
		w.transitionToExpiredLocked(req, now)
		return RequestView{}, fmt.Errorf("%w: request %s expired at %s", ErrApprovalDeadline, requestID, req.waitUntil.Format(time.RFC3339))
	}
	if err := w.checkApproverLocked(req, in, now); err != nil {
		return RequestView{}, err
	}

	w.transitionToRejectedLocked(req, now, in.ApproverAccountID, in.Reason)
	return req.view(), nil
}

// checkApproverLocked 校验批准/拒绝身份：必须是出资账户绑定当前设备的
// 有效会话。任何不符都返回 ErrApprovalUnauthorized，不改变请求。
func (w *Wallet) checkApproverLocked(req *request, in ApprovalInput, now time.Time) error {
	if in.ApproverAccountID == "" || in.SessionID == "" || in.DeviceID == "" {
		return fmt.Errorf("%w: approver account, session and device are required", ErrApprovalUnauthorized)
	}
	if in.ApproverAccountID != req.payerAccountID {
		return fmt.Errorf("%w: account %s is not the payer account %s", ErrApprovalUnauthorized, in.ApproverAccountID, req.payerAccountID)
	}
	sess, ok := w.sessions[in.SessionID]
	if !ok {
		return fmt.Errorf("%w: session %s: %w", ErrApprovalUnauthorized, in.SessionID, ErrSessionNotFound)
	}
	if sess.accountID != in.ApproverAccountID {
		return fmt.Errorf("%w: session %s belongs to account %s", ErrApprovalUnauthorized, sess.id, sess.accountID)
	}
	if sess.deviceID != in.DeviceID {
		return fmt.Errorf("%w: session %s is bound to device %s", ErrApprovalUnauthorized, sess.id, sess.deviceID)
	}
	if sess.revoked {
		return fmt.Errorf("%w: session %s revoked", ErrApprovalUnauthorized, sess.id)
	}
	if !now.Before(sess.expiresAt) {
		return fmt.Errorf("%w: session %s expired at %s", ErrApprovalUnauthorized, sess.id, sess.expiresAt.Format(time.RFC3339))
	}
	return nil
}

// expireIfDueLocked 把到达等待期限的待审批请求流转为过期终态并留痕；
// 其他状态不做任何处理。必须在持锁状态下调用。
func (w *Wallet) expireIfDueLocked(req *request, now time.Time) {
	if req.state != RequestPendingApproval {
		return
	}
	if now.Before(req.waitUntil) {
		return
	}
	w.transitionToExpiredLocked(req, now)
}

// transitionToExpiredLocked 必须在持锁状态下调用。
func (w *Wallet) transitionToExpiredLocked(req *request, now time.Time) {
	req.state = RequestExpired
	req.decidedAt = now
	w.ledger = append(w.ledger, LedgerEntry{
		Kind:           LedgerExpired,
		UsageAccountID: req.accountID,
		RequestID:      req.requestID,
		Reason:         "approval deadline passed without decision",
		At:             now,
	})
}

// transitionToRejectedLocked 必须在持锁状态下调用。拒绝不涉及任何资金
// 变动；approver 为空表示由系统流转（如申请会话被吊销）。
func (w *Wallet) transitionToRejectedLocked(req *request, now time.Time, approver, reason string) {
	req.state = RequestRejected
	req.decidedAt = now
	req.approver = approver
	req.rejectReason = reason
	w.ledger = append(w.ledger, LedgerEntry{
		Kind:           LedgerApprovalRejected,
		UsageAccountID: req.accountID,
		RequestID:      req.requestID,
		Reason:         reason,
		At:             now,
	})
}

// Settle 结算一笔已预留的代付请求。
//
// actualFee 允许为零但不得为负、不得超过预留的预估费用。结算时扣除
// 实际费用并把差额退回可用余额。以相同实际费用重复结算幂等返回，不
// 重复记账；不同实际费用返回 ErrConflict。只有已预留的请求可以结算：
// 待审批、已拒绝、过期或已取消的请求均被拒绝。
func (w *Wallet) Settle(accountID, requestID string, actualFee int64) (RequestView, error) {
	if actualFee < 0 {
		return RequestView{}, fmt.Errorf("%w: actual fee must not be negative", ErrInvalidAmount)
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	req, p, err := w.lookupRequest(accountID, requestID)
	if err != nil {
		return RequestView{}, err
	}
	switch req.state {
	case RequestSettled:
		if req.actualFee == actualFee {
			return req.view(), nil
		}
		return RequestView{}, fmt.Errorf("%w: request %s already settled with actual fee %d", ErrConflict, requestID, req.actualFee)
	case RequestCancelled:
		return RequestView{}, fmt.Errorf("%w: request %s", ErrAlreadyCancelled, requestID)
	case RequestReserved:
		// 下方继续结算。
	default:
		return RequestView{}, fmt.Errorf("%w: request %s is %s", ErrRequestNotReserved, requestID, stateName(req.state))
	}

	if actualFee > req.estimatedFee {
		// 拒绝并保留预留。
		return RequestView{}, fmt.Errorf("%w: actual %d > estimated %d for request %s", ErrSettleTooLarge, actualFee, req.estimatedFee, requestID)
	}

	now := w.now()
	payer := w.accounts[req.payerAccountID]
	refund := req.estimatedFee - actualFee

	// 扣除实际费用、退回差额、释放共享额度。
	payer.reserved -= req.estimatedFee
	payer.available += refund
	p.reservedTotal -= req.estimatedFee
	p.spentTotal += actualFee

	req.state = RequestSettled
	req.actualFee = actualFee
	req.settledAt = now

	w.ledger = append(w.ledger, LedgerEntry{
		Kind:           LedgerSettle,
		AccountID:      payer.id,
		UsageAccountID: req.accountID,
		RequestID:      requestID,
		Amount:         actualFee,
		Reason:         "settle actual fee",
		At:             now,
	})
	if refund > 0 {
		w.ledger = append(w.ledger, LedgerEntry{
			Kind:           LedgerRefund,
			AccountID:      payer.id,
			UsageAccountID: req.accountID,
			RequestID:      requestID,
			Amount:         refund,
			Reason:         "refund estimated-minus-actual fee",
			At:             now,
		})
	}
	return req.view(), nil
}

// Cancel 取消一笔尚未终态处理的代付请求。
//
// 取消已预留请求时预留费用全部退回，并同时留下取消状态记录；取消待
// 审批请求不涉及任何资金，直接进入取消终态。已结算的请求不能取消；
// 已拒绝或已过期的请求已是终态、不能改为取消；重复取消幂等返回，不
// 重复记账。
func (w *Wallet) Cancel(accountID, requestID string) (RequestView, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	req, p, err := w.lookupRequest(accountID, requestID)
	if err != nil {
		return RequestView{}, err
	}
	now := w.now()
	switch req.state {
	case RequestCancelled:
		return req.view(), nil
	case RequestSettled:
		return RequestView{}, fmt.Errorf("%w: request %s", ErrAlreadySettled, requestID)
	case RequestRejected, RequestExpired:
		return RequestView{}, fmt.Errorf("%w: request %s is %s", ErrRequestNotReserved, requestID, stateName(req.state))
	case RequestPendingApproval:
		// 待审批取消：无预留可退，只留取消终态，不产生退款。
		req.state = RequestCancelled
		req.decidedAt = now
		w.ledger = append(w.ledger, LedgerEntry{
			Kind:           LedgerCancelled,
			UsageAccountID: req.accountID,
			RequestID:      requestID,
			Reason:         "pending request cancelled before approval",
			At:             now,
		})
		return req.view(), nil
	}

	payer := w.accounts[req.payerAccountID]

	payer.reserved -= req.estimatedFee
	payer.available += req.estimatedFee
	p.reservedTotal -= req.estimatedFee

	req.state = RequestCancelled

	w.ledger = append(w.ledger, LedgerEntry{
		Kind:           LedgerRefund,
		AccountID:      payer.id,
		UsageAccountID: req.accountID,
		RequestID:      requestID,
		Amount:         req.estimatedFee,
		Reason:         "refund reserved fee on cancel",
		At:             now,
	})
	w.ledger = append(w.ledger, LedgerEntry{
		Kind:           LedgerCancelled,
		UsageAccountID: req.accountID,
		RequestID:      requestID,
		Reason:         "reserved request cancelled",
		At:             now,
	})
	return req.view(), nil
}

// stateName 返回请求状态的可读名称，用于错误说明。
func stateName(s RequestState) string {
	switch s {
	case RequestPendingApproval:
		return "pending approval"
	case RequestReserved:
		return "reserved"
	case RequestSettled:
		return "settled"
	case RequestCancelled:
		return "cancelled"
	case RequestRejected:
		return "rejected"
	case RequestExpired:
		return "expired"
	default:
		return "unknown"
	}
}

// Request 查询某使用账户下的代付请求。
// 到达等待期限的待审批请求会在查询时流转为过期终态并据此返回。
func (w *Wallet) Request(accountID, requestID string) (RequestView, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	req, _, err := w.lookupRequest(accountID, requestID)
	if err != nil {
		return RequestView{}, err
	}
	return req.view(), nil
}

// Ledger 返回全部账本记录的副本，按产生顺序排列。
// 申请被拒绝的记录（Kind 为 LedgerRejection）也包含在内并带有原因。
func (w *Wallet) Ledger() []LedgerEntry {
	w.mu.Lock()
	defer w.mu.Unlock()

	out := make([]LedgerEntry, len(w.ledger))
	copy(out, w.ledger)
	return out
}

// AccountLedger 返回与指定出资账户资金变动相关的账本记录副本
// （预留、扣减、退回），不含与资金无关的拒绝记录。
func (w *Wallet) AccountLedger(accountID string) []LedgerEntry {
	w.mu.Lock()
	defer w.mu.Unlock()

	var out []LedgerEntry
	for _, e := range w.ledger {
		if e.AccountID == accountID {
			out = append(out, e)
		}
	}
	return out
}

// lookupRequest 必须在持锁状态下调用。查询时顺带把已到期限的待审批
// 请求流转为过期终态，保证所有后续操作看到的都是最新状态。
func (w *Wallet) lookupRequest(accountID, requestID string) (*request, *policy, error) {
	req, ok := w.requests[requestKey{accountID: accountID, requestID: requestID}]
	if !ok {
		return nil, nil, fmt.Errorf("%w: account %s request %s", ErrRequestNotFound, accountID, requestID)
	}
	w.expireIfDueLocked(req, w.now())
	return req, w.policies[req.policyID], nil
}

// reject 记录无需持锁快速校验阶段的拒绝（当前没有此类调用，保留对称接口）。
func (w *Wallet) reject(in RequestInput, cause error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.appendRejection(in, w.now(), cause)
	return cause
}

// rejectLocked 在持锁状态下记录拒绝原因并原样返回错误。
func (w *Wallet) rejectLocked(in RequestInput, at time.Time, cause error) error {
	w.appendRejection(in, at, cause)
	return cause
}

// appendRejection 必须在持锁状态下调用。拒绝记录不携带资金账户，
// 只记录使用账户、请求编号与具体原因，绝不产生金额变动。
func (w *Wallet) appendRejection(in RequestInput, at time.Time, cause error) {
	w.ledger = append(w.ledger, LedgerEntry{
		Kind:           LedgerRejection,
		UsageAccountID: in.AccountID,
		RequestID:      in.RequestID,
		Reason:         cause.Error(),
		At:             at,
	})
}

func (a *account) view() AccountView {
	return AccountView{
		ID: a.id,
		Balances: Balances{
			Available: a.available,
			Reserved:  a.reserved,
		},
		CreatedAt: a.createdAt,
	}
}

func (s *session) view() SessionView {
	state := SessionActive
	if s.revoked {
		state = SessionRevoked
	}
	return SessionView{
		ID:        s.id,
		AccountID: s.accountID,
		DeviceID:  s.deviceID,
		ExpiresAt: s.expiresAt,
		State:     state,
		CreatedAt: s.createdAt,
	}
}

func (p *policy) view() PolicyView {
	ids := make([]string, 0, len(p.allowedAccounts))
	for id := range p.allowedAccounts {
		ids = append(ids, id)
	}
	return PolicyView{
		PolicySpec: PolicySpec{
			ID:                p.id,
			PayerAccountID:    p.payerAccountID,
			AllowedAccountIDs: ids,
			Operation:         p.operation,
			Payee:             p.payee,
			StartsAt:          p.startsAt,
			EndsAt:            p.endsAt,
			MaxPerRequest:     p.maxPerRequest,
			MaxTotal:          p.maxTotal,
			ApprovalThreshold: p.approvalThreshold,
			ApprovalWait:      p.approvalWait,
		},
		ReservedTotal: p.reservedTotal,
		SpentTotal:    p.spentTotal,
	}
}

func (r *request) view() RequestView {
	return RequestView{
		PolicyID:          r.policyID,
		RequestID:         r.requestID,
		AccountID:         r.accountID,
		PayerAccountID:    r.payerAccountID,
		Operation:         r.operation,
		Payee:             r.payee,
		EstimatedFee:      r.estimatedFee,
		ActualFee:         r.actualFee,
		State:             r.state,
		CreatedAt:         r.createdAt,
		WaitUntil:         r.waitUntil,
		DecidedAt:         r.decidedAt,
		ApproverAccountID: r.approver,
		RejectReason:      r.rejectReason,
		SettledAt:         r.settledAt,
	}
}
