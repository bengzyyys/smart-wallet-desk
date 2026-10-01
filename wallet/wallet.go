package wallet

import (
	"fmt"
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
	id              string
	payerAccountID  string
	allowedAccounts map[string]struct{}
	operation       string
	payee           string
	startsAt        time.Time
	endsAt          time.Time
	maxPerRequest   int64
	maxTotal        int64
	reservedTotal   int64
	spentTotal      int64
}

type request struct {
	policyID       string
	requestID      string
	accountID      string
	payerAccountID string
	operation      string
	payee          string
	estimatedFee   int64
	actualFee      int64
	state          RequestState
	createdAt      time.Time
	settledAt      time.Time
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
// 吊销后不再受理该会话的新申请，但已预留费用的请求仍可结算或取消。
func (w *Wallet) RevokeSession(id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	sess, ok := w.sessions[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	sess.revoked = true
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
		id:              spec.ID,
		payerAccountID:  spec.PayerAccountID,
		allowedAccounts: allowed,
		operation:       spec.Operation,
		payee:           spec.Payee,
		startsAt:        spec.StartsAt,
		endsAt:          spec.EndsAt,
		maxPerRequest:   spec.MaxPerRequest,
		maxTotal:        spec.MaxTotal,
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
			// 幂等命中：直接返回已有结果，不再预留、不记账。
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

	// 受理：从出资账户可用余额中预留预估费用。
	payer.available -= in.EstimatedFee
	payer.reserved += in.EstimatedFee
	p.reservedTotal += in.EstimatedFee

	req := &request{
		policyID:       p.id,
		requestID:      in.RequestID,
		accountID:      in.AccountID,
		payerAccountID: payer.id,
		operation:      in.Operation,
		payee:          in.Payee,
		estimatedFee:   in.EstimatedFee,
		state:          RequestReserved,
		createdAt:      now,
	}
	w.requests[key] = req
	w.ledger = append(w.ledger, LedgerEntry{
		Kind:      LedgerReserve,
		AccountID: payer.id,
		RequestID: in.RequestID,
		Amount:    in.EstimatedFee,
		Reason:    "reserve estimated fee",
		At:        now,
	})
	return req.view(), nil
}

// Settle 结算一笔已预留的代付请求。
//
// actualFee 允许为零但不得为负、不得超过预留的预估费用。结算时扣除
// 实际费用并把差额退回可用余额。以相同实际费用重复结算幂等返回，不
// 重复记账；不同实际费用返回 ErrConflict。已取消的请求不能结算。
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
		Kind:      LedgerSettle,
		AccountID: payer.id,
		RequestID: requestID,
		Amount:    actualFee,
		Reason:    "settle actual fee",
		At:        now,
	})
	if refund > 0 {
		w.ledger = append(w.ledger, LedgerEntry{
			Kind:      LedgerRefund,
			AccountID: payer.id,
			RequestID: requestID,
			Amount:    refund,
			Reason:    "refund estimated-minus-actual fee",
			At:        now,
		})
	}
	return req.view(), nil
}

// Cancel 取消一笔尚未结算的代付请求，预留费用全部退回。
//
// 已结算的请求不能取消；重复取消幂等返回，不重复记账。
func (w *Wallet) Cancel(accountID, requestID string) (RequestView, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	req, p, err := w.lookupRequest(accountID, requestID)
	if err != nil {
		return RequestView{}, err
	}
	switch req.state {
	case RequestCancelled:
		return req.view(), nil
	case RequestSettled:
		return RequestView{}, fmt.Errorf("%w: request %s", ErrAlreadySettled, requestID)
	}

	now := w.now()
	payer := w.accounts[req.payerAccountID]

	payer.reserved -= req.estimatedFee
	payer.available += req.estimatedFee
	p.reservedTotal -= req.estimatedFee

	req.state = RequestCancelled

	w.ledger = append(w.ledger, LedgerEntry{
		Kind:      LedgerRefund,
		AccountID: payer.id,
		RequestID: requestID,
		Amount:    req.estimatedFee,
		Reason:    "refund reserved fee on cancel",
		At:        now,
	})
	return req.view(), nil
}

// Request 查询某使用账户下的代付请求。
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

// lookupRequest 必须在持锁状态下调用。
func (w *Wallet) lookupRequest(accountID, requestID string) (*request, *policy, error) {
	req, ok := w.requests[requestKey{accountID: accountID, requestID: requestID}]
	if !ok {
		return nil, nil, fmt.Errorf("%w: account %s request %s", ErrRequestNotFound, accountID, requestID)
	}
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
// 只记录请求编号与具体原因，绝不产生金额变动。
func (w *Wallet) appendRejection(in RequestInput, at time.Time, cause error) {
	w.ledger = append(w.ledger, LedgerEntry{
		Kind:      LedgerRejection,
		RequestID: in.RequestID,
		Reason:    cause.Error(),
		At:        at,
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
		},
		ReservedTotal: p.reservedTotal,
		SpentTotal:    p.spentTotal,
	}
}

func (r *request) view() RequestView {
	return RequestView{
		PolicyID:       r.policyID,
		RequestID:      r.requestID,
		AccountID:      r.accountID,
		PayerAccountID: r.payerAccountID,
		Operation:      r.operation,
		Payee:          r.payee,
		EstimatedFee:   r.estimatedFee,
		ActualFee:      r.actualFee,
		State:          r.state,
		CreatedAt:      r.createdAt,
		SettledAt:      r.settledAt,
	}
}
