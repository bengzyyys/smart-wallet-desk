package wallet

import (
	"fmt"
	"sort"
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
	// maxReserveDuration 为最长预留时长；零表示关闭预留超时。
	maxReserveDuration time.Duration
	reservedTotal      int64
	spentTotal         int64

	deactivated          bool
	deactivatedAt        time.Time
	deactivatorAccountID string
	deactivateReason     string
}

type request struct {
	policyID       string
	requestID      string
	accountID      string
	payerAccountID string
	sessionID      string
	operation      string
	payee          string
	estimatedFee   int64
	actualFee      int64
	state          RequestState
	createdAt      time.Time
	settledAt      time.Time
	waitDeadline   time.Time
	// reservedAt 为费用实际预留完成的时刻（直接受理或批准成功的时刻）。
	reservedAt time.Time
	// reserveDuration 为预留时适用的最长预留时长快照；零表示未启用超时。
	reserveDuration time.Duration
	// reserveDeadline 为预留截止时刻（reservedAt + reserveDuration）。
	reserveDeadline time.Time
	// reserveExpiredAt 为超时释放时间；惰性处理时记录的仍是截止时刻本身。
	reserveExpiredAt  time.Time
	decidedAt         time.Time
	approverAccountID string
	rejectReason      string
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
// 查询时已到期的预留会先被自动释放，因此视图反映最新的可用/预留余额。
func (w *Wallet) Account(id string) (AccountView, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	acc, ok := w.accounts[id]
	if !ok {
		return AccountView{}, fmt.Errorf("%w: %s", ErrAccountNotFound, id)
	}
	w.expireReservationsLocked(w.now())
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

// RevokeSession 主动吊销会话。会话不存在返回 ErrSessionNotFound；已经到期
// 的会话也允许吊销；重复吊销视为成功且不重复留痕。吊销后不再受理该会话
// 的新申请。
//
// 吊销瞬间按各请求自身的等待截止时间分别结清该会话仍在待审批的请求：
// 当前时刻严格早于截止时间的进入拒绝终态并说明申请会话已被吊销，决定
// 时间为吊销时刻；当前时刻等于或晚于截止时间（等待时长耗尽、策略结束
// 或申请会话到期先发生）的进入过期终态，不带吊销拒绝原因、不填写审批
// 账户，提交时间与等待截止时间保持不变。终态结果只取决于申请自身的
// 期限，不因吊销前是否被查询过而不同；先前已进入终态的请求保留原决定
// 信息与历史记录。
//
// 两类处理都不冻结费用：不改变出资余额、策略预留总额和已花费总额，也不
// 生成退款、扣减或预留记录。已预留请求继续按原规则结算、取消或等待自身
// 预留超时，吊销不提前释放费用；其他会话的申请不受影响。
func (w *Wallet) RevokeSession(id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	sess, ok := w.sessions[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	if sess.revoked {
		return nil
	}
	sess.revoked = true
	now := w.now()
	reason := "application session revoked before approval"

	pending := make([]*request, 0)
	for _, req := range w.requests {
		if req.state == RequestPendingApproval && req.sessionID == sess.id {
			pending = append(pending, req)
		}
	}
	w.settlePendingLocked(pending, now, func(req *request) {
		// 未到等待期限：随会话吊销进入拒绝终态，不填写审批账户。
		req.state = RequestRejected
		req.rejectReason = reason
		req.decidedAt = now
		w.ledger = append(w.ledger, LedgerEntry{
			Kind:      LedgerRejection,
			AccountID: req.accountID,
			RequestID: req.requestID,
			Reason:    reason,
			At:        now,
		})
	})
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
	// 审批门槛：零表示关闭；不得为负，不得超过单次上限；开启时等待时长必须为正。
	if spec.ApprovalThreshold < 0 {
		return fmt.Errorf("%w: approval threshold must not be negative", ErrPolicyInvalid)
	}
	if spec.ApprovalThreshold > 0 {
		if spec.ApprovalThreshold > spec.MaxPerRequest {
			return fmt.Errorf("%w: approval threshold %d exceeds per-request limit %d", ErrPolicyInvalid, spec.ApprovalThreshold, spec.MaxPerRequest)
		}
		if spec.ApprovalWait <= 0 {
			return fmt.Errorf("%w: approval wait must be positive when approval is enabled", ErrPolicyInvalid)
		}
	}
	// 最长预留时长：零表示关闭预留超时（保持既有行为）；不得为负。
	if spec.MaxReserveDuration < 0 {
		return fmt.Errorf("%w: max reserve duration must not be negative", ErrPolicyInvalid)
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
		id:                 spec.ID,
		payerAccountID:     spec.PayerAccountID,
		allowedAccounts:    allowed,
		operation:          spec.Operation,
		payee:              spec.Payee,
		startsAt:           spec.StartsAt,
		endsAt:             spec.EndsAt,
		maxPerRequest:      spec.MaxPerRequest,
		maxTotal:           spec.MaxTotal,
		approvalThreshold:  spec.ApprovalThreshold,
		approvalWait:       spec.ApprovalWait,
		maxReserveDuration: spec.MaxReserveDuration,
	}
	return nil
}

// Policy 查询策略视图，含当前预留中与已结算的累计金额。查询时已到期的
// 预留会先被自动释放，ReservedTotal 因而反映释放后的结果。
func (w *Wallet) Policy(id string) (PolicyView, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	p, ok := w.policies[id]
	if !ok {
		return PolicyView{}, fmt.Errorf("%w: %s", ErrPolicyNotFound, id)
	}
	w.expireReservationsLocked(w.now())
	return p.view(), nil
}

// DeactivatePolicy 由出资账户主动停用一条代付策略，立即停止该策略继续
// 受理代付。停用不可撤销。
//
// 必须使用该策略出资账户绑定当前设备的有效会话：会话不存在、不属于出资
// 账户、设备不符、已吊销或已到期时返回 ErrNotPolicyOwner，不改变策略、
// 请求或账本。策略编号、会话编号、设备缺失，或理由去掉首尾空白后为空，
// 返回 ErrInvalidArgument。策略不存在返回 ErrPolicyNotFound。保存与比较
// 理由均使用去掉首尾空白后的内容。
//
// 再次停用仍须通过会话校验：理由与首次相同时幂等返回首次结果，理由不同
// 返回 ErrConflict；两者都不改写首次停用信息，也不增加账本记录。尚未
// 开始或已经结束的策略也允许停用；停用后该编号不能再保存成新策略。
//
// 停用成功时，该策略下未到等待期限的待审批请求立即转为拒绝终态，拒绝
// 信息说明策略被停用并包含停用理由，决定时间为停用时间；已到（含恰到）
// 或超过等待期限的待审批请求进入过期终态。两类处理都不冻结余额、不占用
// 额度，也不产生退款；其他状态的请求与其他策略不受影响。
func (w *Wallet) DeactivatePolicy(policyID, sessionID, deviceID, reason string) (PolicyView, error) {
	reason = strings.TrimSpace(reason)
	if policyID == "" || sessionID == "" || deviceID == "" || reason == "" {
		return PolicyView{}, fmt.Errorf("%w: policy id, session id, device id and reason are required", ErrInvalidArgument)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	p, ok := w.policies[policyID]
	if !ok {
		return PolicyView{}, fmt.Errorf("%w: %s", ErrPolicyNotFound, policyID)
	}
	now := w.now()
	if err := w.checkPolicyOwnerLocked(p, sessionID, deviceID, now); err != nil {
		return PolicyView{}, err
	}
	// 鉴权通过后先释放已到期预留（停用不缩短任何未到期请求的截止时间）。
	w.expireReservationsLocked(now)

	if p.deactivated {
		// 停用不可撤销：理由相同返回首次结果，理由不同返回冲突，
		// 均不改写首次信息、不增加账本记录。
		if p.deactivateReason == reason {
			return p.view(), nil
		}
		return PolicyView{}, fmt.Errorf("%w: policy %s was already deactivated with a different reason", ErrConflict, policyID)
	}

	p.deactivated = true
	p.deactivatedAt = now
	p.deactivatorAccountID = p.payerAccountID
	p.deactivateReason = reason

	w.ledger = append(w.ledger, LedgerEntry{
		Kind:      LedgerPolicyDeactivation,
		AccountID: p.payerAccountID,
		PolicyID:  p.id,
		Reason:    reason,
		At:        now,
	})

	// 停用瞬间结清该策略全部待审批请求：未到等待期限的随停用立即拒绝，
	// 已到或超过期限的进入过期终态（不能因未被查询过而变成停用拒绝）。
	pending := make([]*request, 0)
	for _, req := range w.requests {
		if req.policyID == p.id && req.state == RequestPendingApproval {
			pending = append(pending, req)
		}
	}
	w.settlePendingLocked(pending, now, func(req *request) {
		msg := fmt.Sprintf("policy %s deactivated by payer: %s", p.id, reason)
		req.state = RequestRejected
		req.rejectReason = msg
		req.decidedAt = now
		req.approverAccountID = p.payerAccountID
		w.ledger = append(w.ledger, LedgerEntry{
			Kind:      LedgerRejection,
			AccountID: req.accountID,
			RequestID: req.requestID,
			Reason:    msg,
			At:        now,
		})
	})
	return p.view(), nil
}

// checkPolicyOwnerLocked 在持锁状态下校验停用会话：必须存在、归属策略的
// 出资账户、绑定当前设备、未吊销且未到期。任一不满足都返回
// ErrNotPolicyOwner，且不改变策略、请求或账本。
func (w *Wallet) checkPolicyOwnerLocked(p *policy, sessionID, deviceID string, now time.Time) error {
	sess, fail := w.checkSessionLocked(sessionID, p.payerAccountID, deviceID, now)
	if fail != sessionCheckOK {
		return authorizedSessionError(ErrNotPolicyOwner, sessionID, p.payerAccountID, deviceID, sess, fail)
	}
	return nil
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

	sess, fail := w.checkSessionLocked(in.SessionID, in.AccountID, in.DeviceID, now)
	if fail != sessionCheckOK {
		return RequestView{}, w.rejectLocked(in, now, applySessionError(in, sess, fail))
	}

	key := requestKey{accountID: in.AccountID, requestID: in.RequestID}
	if existing, ok := w.requests[key]; ok {
		// 待审批到期即过期、已预留到期即自动退回，重复申请看到的应是最新终态。
		w.refreshPendingLocked(existing, now)
		w.refreshReservedLocked(existing, now)
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
	// 停用后的策略不再受理新申请；历史请求的幂等查询在上方已先行处理。
	if p.deactivated {
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("%w: policy %s deactivated: %s", ErrPolicyDeactivated, p.id, p.deactivateReason))
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
	// 资金与额度检查前释放所有已到期预留（含同一出资账户在其他策略下的
	// 预留），因此先查余额还是先申请看到的可用金额都一致。
	w.expireReservationsLocked(now)
	// 预留中的费用同样占用共享累计额度。
	if used, exceed := quotaWouldExceedLocked(p, in.EstimatedFee); exceed {
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("%w: estimated fee %d would exceed shared total limit %d (used %d)",
			ErrQuotaExceeded, in.EstimatedFee, p.maxTotal, used))
	}

	payer, ok := w.accounts[p.payerAccountID]
	if !ok {
		// 保存策略时已校验，这里仅作防御。
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("payer account %s: %w", p.payerAccountID, ErrAccountNotFound))
	}
	if payer.available < in.EstimatedFee {
		return RequestView{}, w.rejectLocked(in, now, fmt.Errorf("%w: payer %s available %d, need %d", ErrInsufficientBalance, payer.id, payer.available, in.EstimatedFee))
	}

	// 所有硬性检查通过：预估费用严格超过审批门槛的进入待审批，其余直接预留。
	if p.approvalThreshold > 0 && in.EstimatedFee > p.approvalThreshold {
		// 待审批不冻结余额、不占用共享累计额度。期限取提交时刻 + 等待时长、
		// 策略结束时间、申请会话到期时间三者中的最早值。
		deadline := now.Add(p.approvalWait)
		if p.endsAt.Before(deadline) {
			deadline = p.endsAt
		}
		if sess.expiresAt.Before(deadline) {
			deadline = sess.expiresAt
		}
		req := &request{
			policyID:       p.id,
			requestID:      in.RequestID,
			accountID:      in.AccountID,
			payerAccountID: payer.id,
			sessionID:      sess.id,
			operation:      in.Operation,
			payee:          in.Payee,
			estimatedFee:   in.EstimatedFee,
			state:          RequestPendingApproval,
			createdAt:      now,
			waitDeadline:   deadline,
		}
		w.requests[key] = req
		w.ledger = append(w.ledger, LedgerEntry{
			Kind:      LedgerPendingApproval,
			AccountID: in.AccountID,
			RequestID: in.RequestID,
			Reason:    "pending approval for large amount",
			At:        now,
		})
		return req.view(), nil
	}

	// 所有硬性检查通过且无需审批：直接受理。以提交时刻作为实际预留时刻，
	// 不填写任何审批决定信息。
	req := &request{
		policyID:       p.id,
		requestID:      in.RequestID,
		accountID:      in.AccountID,
		payerAccountID: payer.id,
		sessionID:      sess.id,
		operation:      in.Operation,
		payee:          in.Payee,
		estimatedFee:   in.EstimatedFee,
		createdAt:      now,
	}
	w.reserveFeeLocked(p, payer, req, now, "reserve estimated fee")
	w.requests[key] = req
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
	now := w.now()
	w.refreshPendingLocked(req, now)
	// 已到最长预留时长的预留先自动退回：此后任何结算都不能再扣款。
	w.refreshReservedLocked(req, now)
	switch req.state {
	case RequestSettled:
		if req.actualFee == actualFee {
			return req.view(), nil
		}
		return RequestView{}, fmt.Errorf("%w: request %s already settled with actual fee %d", ErrConflict, requestID, req.actualFee)
	case RequestCancelled:
		return RequestView{}, fmt.Errorf("%w: request %s", ErrAlreadyCancelled, requestID)
	case RequestReservationExpired:
		// 预留已超时全额退回：传入非负实际费用也返回明确的超时错误，不能扣款。
		return RequestView{}, fmt.Errorf("%w: request %s reservation expired at %s", ErrReservationExpired, requestID, req.reserveExpiredAt.Format(time.RFC3339))
	case RequestPendingApproval, RequestRejected, RequestExpired:
		// 结算只允许处理已预留请求。
		return RequestView{}, fmt.Errorf("%w: request %s is %v, only reserved requests can be settled", ErrRequestNotReserved, requestID, req.state)
	}

	if actualFee > req.estimatedFee {
		// 拒绝并保留预留。
		return RequestView{}, fmt.Errorf("%w: actual %d > estimated %d for request %s", ErrSettleTooLarge, actualFee, req.estimatedFee, requestID)
	}

	payer := w.accounts[req.payerAccountID]
	refund := req.estimatedFee - actualFee

	// 统一释放预留：出资账户预留余额与策略预留总额都按预估全额减少，实际
	// 费用计入策略已花费总额，差额退回该出资账户可用余额；实际费用等于预估
	// 时差额为零，只扣实际费用、不产生退款。
	w.releaseReservationLocked(p, payer, req, refund, actualFee)

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

// Cancel 取消一笔尚未结算的代付请求。
//
// 待审批请求取消后直接进入取消终态，不产生退款（本就没有冻结余额）；
// 已预留请求取消后全部预留退回。已结算的请求不能取消；重复取消幂等
// 返回，不重复记账。已拒绝或已过期的请求不能取消。
func (w *Wallet) Cancel(accountID, requestID string) (RequestView, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	req, p, err := w.lookupRequest(accountID, requestID)
	if err != nil {
		return RequestView{}, err
	}
	now := w.now()
	w.refreshPendingLocked(req, now)
	// 已到最长预留时长的预留先自动退回。
	w.refreshReservedLocked(req, now)
	switch req.state {
	case RequestCancelled:
		return req.view(), nil
	case RequestSettled:
		return RequestView{}, fmt.Errorf("%w: request %s", ErrAlreadySettled, requestID)
	case RequestReservationExpired:
		// 预留已超时自动全额退回：取消幂等返回已有超时结果，不重复退回、不留痕。
		return req.view(), nil
	case RequestPendingApproval:
		// 待审批取消：无资金冻结，直接留取消终态。
		req.state = RequestCancelled
		req.decidedAt = now
		w.ledger = append(w.ledger, LedgerEntry{
			Kind:      LedgerCancellation,
			AccountID: req.accountID,
			RequestID: req.requestID,
			Reason:    "cancel pending-approval request",
			At:        now,
		})
		return req.view(), nil
	case RequestRejected, RequestExpired:
		return RequestView{}, fmt.Errorf("%w: request %s is %v, cannot cancel", ErrRequestNotReserved, requestID, req.state)
	}

	// 已预留：取消不产生实际费用，预估费用全额退回出资账户可用余额。
	payer := w.accounts[req.payerAccountID]
	w.releaseReservationLocked(p, payer, req, req.estimatedFee, 0)

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

// Approve 批准一笔待审批的大额代付请求。
//
// 审批必须使用该策略出资账户绑定当前设备的有效会话；其他账户或无效
// 会话返回 ErrNotApprover 无权审批，且不改变请求。批准时再次检查出资
// 余额与共享剩余额度，足够才一次性预留全部预估费用并转为已预留；不足
// 时返回具体原因，保留待审批状态，期限内可再次批准。
//
// 已批准并处于预留或结算状态的请求重复批准只返回当前结果，不重复预留；
// 已拒绝、过期或取消的请求不能批准。截止时刻及之后不能批准。
func (w *Wallet) Approve(accountID, requestID, sessionID, deviceID string) (RequestView, error) {
	if accountID == "" || requestID == "" || sessionID == "" || deviceID == "" {
		return RequestView{}, fmt.Errorf("%w: account, request, session and device ids are required", ErrInvalidArgument)
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.now()
	req, p, err := w.lookupRequest(accountID, requestID)
	if err != nil {
		return RequestView{}, err
	}
	if err := w.checkApproverLocked(req, sessionID, deviceID, now); err != nil {
		return RequestView{}, err
	}
	// 鉴权通过后释放已到期预留（含同一出资账户在其他策略下的预留），使
	// 批准的余额与额度检查计入退回；因余额或额度不足而失败的批准不会开始
	// 预留计时。
	w.expireReservationsLocked(now)
	w.refreshPendingLocked(req, now)
	if req.state == RequestExpired {
		return RequestView{}, fmt.Errorf("%w: approval deadline %s passed", ErrApprovalExpired, req.waitDeadline.Format(time.RFC3339))
	}

	switch req.state {
	case RequestPendingApproval:
		payer := w.accounts[req.payerAccountID]
		if payer.available < req.estimatedFee {
			return RequestView{}, fmt.Errorf("%w: payer %s available %d, need %d", ErrInsufficientBalance, payer.id, payer.available, req.estimatedFee)
		}
		if used, exceed := quotaWouldExceedLocked(p, req.estimatedFee); exceed {
			return RequestView{}, fmt.Errorf("%w: estimated fee %d would exceed shared total limit %d (used %d)",
				ErrQuotaExceeded, req.estimatedFee, p.maxTotal, used)
		}
		// 一次性完成全部预估费用的预留（资金划转、转已预留、预留记录与从批准
		// 成功时刻起算的预留计时）；余额或额度不足时已在上方返回，不会走到
		// 这里，因此失败的批准不会开始计时。
		w.reserveFeeLocked(p, payer, req, now, "reserve estimated fee on approval")
		// 批准成功保留原提交时刻与等待截止时刻；决定时间为批准时刻，并记录
		// 出资账户的批准决定。随后追加使用账户的零金额批准状态记录。
		req.decidedAt = now
		req.approverAccountID = payer.id
		w.ledger = append(w.ledger, LedgerEntry{
			Kind:      LedgerApproval,
			AccountID: req.accountID,
			RequestID: req.requestID,
			Reason:    "approve large-amount request",
			At:        now,
		})
		return req.view(), nil
	case RequestReserved, RequestSettled:
		// 已批准：重复批准幂等返回，不重复预留。
		return req.view(), nil
	default:
		return RequestView{}, fmt.Errorf("%w: request %s is %v, cannot approve", ErrRequestNotPending, requestID, req.state)
	}
}

// Reject 拒绝一笔待审批的大额代付请求。
//
// 拒绝必须使用该策略出资账户绑定当前设备的有效会话；其他账户或无效
// 会话返回 ErrNotApprover 无权审批，且不改变请求。拒绝理由为空或全为
// 空白时返回 ErrInvalidArgument 参数错误，请求保持不变。
//
// 重复拒绝且理由一致时幂等返回，不重复留痕；理由不同返回 ErrConflict。
// 已批准（含预留、结算）、已过期或已取消的请求不能拒绝。
func (w *Wallet) Reject(accountID, requestID, sessionID, deviceID, reason string) (RequestView, error) {
	if strings.TrimSpace(reason) == "" {
		return RequestView{}, fmt.Errorf("%w: reject reason must not be empty or blank", ErrInvalidArgument)
	}
	if accountID == "" || requestID == "" || sessionID == "" || deviceID == "" {
		return RequestView{}, fmt.Errorf("%w: account, request, session and device ids are required", ErrInvalidArgument)
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.now()
	req, _, err := w.lookupRequest(accountID, requestID)
	if err != nil {
		return RequestView{}, err
	}
	if err := w.checkApproverLocked(req, sessionID, deviceID, now); err != nil {
		return RequestView{}, err
	}
	w.refreshPendingLocked(req, now)

	switch req.state {
	case RequestPendingApproval:
		req.state = RequestRejected
		req.rejectReason = reason
		req.decidedAt = now
		req.approverAccountID = req.payerAccountID

		w.ledger = append(w.ledger, LedgerEntry{
			Kind:      LedgerRejection,
			AccountID: req.accountID,
			RequestID: req.requestID,
			Reason:    reason,
			At:        now,
		})
		return req.view(), nil
	case RequestRejected:
		if req.rejectReason == reason {
			return req.view(), nil
		}
		return RequestView{}, fmt.Errorf("%w: request %s already rejected with a different reason", ErrConflict, requestID)
	default:
		return RequestView{}, fmt.Errorf("%w: request %s is %v, cannot reject", ErrRequestNotPending, requestID, req.state)
	}
}

// checkApproverLocked 在持锁状态下校验审批会话：必须存在、归属出资账户、
// 绑定当前设备、未吊销且未过期。任一不满足都返回 ErrNotApprover，且不
// 改变请求。
func (w *Wallet) checkApproverLocked(req *request, sessionID, deviceID string, now time.Time) error {
	sess, fail := w.checkSessionLocked(sessionID, req.payerAccountID, deviceID, now)
	if fail != sessionCheckOK {
		return authorizedSessionError(ErrNotApprover, sessionID, req.payerAccountID, deviceID, sess, fail)
	}
	return nil
}

// sessionCheckFailure 描述会话有效性校验未通过的具体条件，供提交申请、
// 出资账户审批与策略停用三个入口共用同一套判定，再各自翻译成对外错误：
// 申请区分为五类公开错误，审批与停用统一为无权错误但保留具体说明。
type sessionCheckFailure int

const (
	sessionCheckOK sessionCheckFailure = iota
	// sessionCheckMissing 会话不存在。
	sessionCheckMissing
	// sessionCheckAccount 会话存在但不属于要求的账户（申请时为使用账户，
	// 审批与停用时为出资账户）。
	sessionCheckAccount
	// sessionCheckDevice 会话归属账户正确，但绑定设备与提交设备不符。
	sessionCheckDevice
	// sessionCheckRevoked 会话已被吊销；与到期同时成立时优先报告吊销。
	sessionCheckRevoked
	// sessionCheckExpired 会话已到期；到期时刻本身即无效（[零值, ExpiresAt)
	// 才有效）。
	sessionCheckExpired
)

// checkSessionLocked 在持锁状态下按固定顺序校验会话：存在性、账户归属、
// 设备绑定、吊销、到期。校验只代表身份通过，不涉及任何策略、余额、额度
// 或请求状态判断。返回的 failure 为 sessionCheckOK 时 sess 即有效会话；
// 否则 sess 可能为 nil（会话不存在）或为找到的会话（用于错误说明），
// 本函数不改变任何状态。必须在持锁状态下调用。
func (w *Wallet) checkSessionLocked(sessionID, wantAccountID, deviceID string, now time.Time) (sess *session, failure sessionCheckFailure) {
	sess, ok := w.sessions[sessionID]
	if !ok {
		return nil, sessionCheckMissing
	}
	if sess.accountID != wantAccountID {
		return sess, sessionCheckAccount
	}
	// 设备仍按提交值与绑定值比较，不能因账户正确就跳过。
	if sess.deviceID != deviceID {
		return sess, sessionCheckDevice
	}
	if sess.revoked {
		return sess, sessionCheckRevoked
	}
	if !now.Before(sess.expiresAt) {
		return sess, sessionCheckExpired
	}
	return sess, sessionCheckOK
}

// applySessionError 把申请路径的会话校验失败翻译成对应的公开错误，错误
// 类别与说明沿用申请入口原有口径（不存在、账户不符、设备不符、吊销、
// 到期）。会话不存在时以提交的会话编号说明，其余以找到的会话说明。
func applySessionError(in RequestInput, sess *session, failure sessionCheckFailure) error {
	switch failure {
	case sessionCheckMissing:
		return fmt.Errorf("%w: %s", ErrSessionNotFound, in.SessionID)
	case sessionCheckAccount:
		return fmt.Errorf("%w: session %s belongs to account %s", ErrSessionAccountMismatch, sess.id, sess.accountID)
	case sessionCheckDevice:
		return fmt.Errorf("%w: session %s is bound to device %s", ErrSessionDeviceMismatch, sess.id, sess.deviceID)
	case sessionCheckRevoked:
		return fmt.Errorf("%w: session %s", ErrSessionRevoked, sess.id)
	case sessionCheckExpired:
		return fmt.Errorf("%w: session %s expired at %s", ErrSessionExpired, sess.id, sess.expiresAt.Format(time.RFC3339))
	default:
		return nil
	}
}

// authorizedSessionError 把审批与停用路径的会话校验失败统一包装成出资账户
// 无权错误（ErrNotApprover 或 ErrNotPolicyOwner），说明中保留导致失败的
// 具体会话条件，调用方仍可按无权错误类别识别。会话不存在时以提交的会话
// 编号说明，其余以找到的会话说明。
func authorizedSessionError(authErr error, sessionID, wantAccountID, deviceID string, sess *session, failure sessionCheckFailure) error {
	switch failure {
	case sessionCheckMissing:
		return fmt.Errorf("%w: session %s", authErr, sessionID)
	case sessionCheckAccount:
		return fmt.Errorf("%w: session %s belongs to account %s, not payer %s", authErr, sess.id, sess.accountID, wantAccountID)
	case sessionCheckDevice:
		return fmt.Errorf("%w: session %s is bound to device %s, not %s", authErr, sess.id, sess.deviceID, deviceID)
	case sessionCheckRevoked:
		return fmt.Errorf("%w: session %s revoked", authErr, sess.id)
	case sessionCheckExpired:
		return fmt.Errorf("%w: session %s expired at %s", authErr, sess.id, sess.expiresAt.Format(time.RFC3339))
	default:
		return nil
	}
}

// refreshPendingLocked 在持锁状态下检查待审批请求是否已到期限；到期
// （含到期时刻）则转为过期终态并记账。必须在持锁状态下调用。
func (w *Wallet) refreshPendingLocked(req *request, now time.Time) {
	if req.state != RequestPendingApproval {
		return
	}
	if now.Before(req.waitDeadline) {
		return
	}
	w.expirePendingLocked(req, now)
}

// expirePendingLocked 将一笔待审批请求转为过期终态：决定时间记录本次处理
// 时刻，拒绝理由与审批账户保持为空，账本只追加一条关联使用账户与请求编号
// 的零金额过期记录。不冻结或退回费用，不改变任何余额与额度。必须在持锁
// 状态下调用。
func (w *Wallet) expirePendingLocked(req *request, now time.Time) {
	req.state = RequestExpired
	req.decidedAt = now
	w.ledger = append(w.ledger, LedgerEntry{
		Kind:      LedgerExpiration,
		AccountID: req.accountID,
		RequestID: req.requestID,
		Reason:    "approval period expired",
		At:        now,
	})
}

// settlePendingLocked 按等待截止时刻顺序（同截止时刻按使用账户、请求编号
// 排序）分别结清 reqs 中仍在待审批的请求，保证账本追加顺序确定。每笔请求
// 只按自己的等待截止时间判断：now 严格早于截止时间且 reject 非空时调用
// reject 进入拒绝终态（拒绝理由与审批账户由调用方决定），恰到或超过截止
// 时间的进入过期终态；已到期的与仍在等待的请求互不影响，不因共用会话或
// 策略而混同。两类处理都不冻结费用、不改变余额与额度。必须在持锁状态下
// 调用。
func (w *Wallet) settlePendingLocked(reqs []*request, now time.Time, reject func(req *request)) {
	sortRequestsByDeadline(reqs, func(req *request) time.Time { return req.waitDeadline })
	for _, req := range reqs {
		if req.state != RequestPendingApproval {
			continue
		}
		if now.Before(req.waitDeadline) {
			if reject != nil {
				reject(req)
			}
			continue
		}
		w.expirePendingLocked(req, now)
	}
}

// sortRequestsByDeadline 按 deadline 给出的截止时刻排序（同截止时刻按使用
// 账户、请求编号排序），供各类批量到期处理共用，保证账本追加顺序确定。
func sortRequestsByDeadline(reqs []*request, deadline func(req *request) time.Time) {
	sort.SliceStable(reqs, func(i, j int) bool {
		if di, dj := deadline(reqs[i]), deadline(reqs[j]); !di.Equal(dj) {
			return di.Before(dj)
		}
		if reqs[i].accountID != reqs[j].accountID {
			return reqs[i].accountID < reqs[j].accountID
		}
		return reqs[i].requestID < reqs[j].requestID
	})
}

// refreshReservedLocked 在持锁状态下检查单笔已预留请求是否已到最长预留
// 时长；截止时刻及之后转为“预留超时”终态并退回全额预留。必须在持锁状态
// 下调用。
func (w *Wallet) refreshReservedLocked(req *request, now time.Time) {
	if req.state != RequestReserved || req.reserveDuration <= 0 {
		return
	}
	if now.Before(req.reserveDeadline) {
		return
	}
	w.expireReservationLocked(req, req.reserveDeadline)
}

// expirePendingApprovalsLocked 惰性结清当前全部已到等待期限（含到期时刻）
// 的待审批请求，使它们进入过期终态并留下状态记录。必须在持锁状态下调用。
func (w *Wallet) expirePendingApprovalsLocked(now time.Time) {
	pending := make([]*request, 0)
	for _, req := range w.requests {
		if req.state == RequestPendingApproval {
			pending = append(pending, req)
		}
	}
	w.settlePendingLocked(pending, now, nil)
}

// expireReservationsLocked 惰性结清当前全部已到期（含到期时刻）的预留。
// 所有账户/余额/策略/请求/账本查询以及新申请、批准的资金检查入口都先调用
// 它，因此调用者无须逐笔取消即可看到已到期预留的释放结果。释放时间一律
// 记录为各请求自己的截止时刻，而不是处理时刻；账本按截止时刻顺序追加。
// 必须在持锁状态下调用。
func (w *Wallet) expireReservationsLocked(now time.Time) {
	due := make([]*request, 0)
	for _, req := range w.requests {
		if req.state == RequestReserved && req.reserveDuration > 0 && !now.Before(req.reserveDeadline) {
			due = append(due, req)
		}
	}
	sortRequestsByDeadline(due, func(req *request) time.Time { return req.reserveDeadline })
	for _, req := range due {
		w.expireReservationLocked(req, req.reserveDeadline)
	}
}

// expireReservationLocked 将一笔已预留请求转为 RequestReservationExpired
// 终态：全额退回预估费用到出资账户可用余额，减少出资账户预留余额与该
// 策略的预留总额，不增加实际费用或已花费总额。账本增加一条出资账户的
// 全额退款记录和一条使用账户的零金额超时状态记录，均关联请求编号。
// 必须在持锁状态下调用。
func (w *Wallet) expireReservationLocked(req *request, deadline time.Time) {
	payer := w.accounts[req.payerAccountID]
	// 超时不产生实际费用：预估费用全额退回出资账户可用余额，不计入策略
	// 已花费总额；释放时刻与预留截止时刻均为 deadline，与取消区分。
	w.releaseReservationLocked(w.policies[req.policyID], payer, req, req.estimatedFee, 0)
	req.state = RequestReservationExpired
	req.reserveExpiredAt = deadline

	w.ledger = append(w.ledger,
		LedgerEntry{
			Kind:      LedgerRefund,
			AccountID: payer.id,
			RequestID: req.requestID,
			Amount:    req.estimatedFee,
			Reason:    "refund reserved fee on reservation timeout",
			At:        deadline,
		},
		LedgerEntry{
			Kind:      LedgerReservationExpiration,
			AccountID: req.accountID,
			RequestID: req.requestID,
			Reason:    "reservation timed out before settlement",
			At:        deadline,
		},
	)
}

// reserveFeeLocked 一次性完成一笔费用的实际预留，供“普通申请直接受理”与
// “待审批申请被出资账户批准”两条入口共用，避免资金、请求状态与账本分别
// 维护：
//   - 全部预估费用从出资账户可用余额转入预留余额，并计入策略的预留总额；
//     策略已花费总额不变，费用始终由策略指定的出资账户承担；
//   - 请求转为已预留，并以 reservedAt 作为实际预留时刻写入预留计时
//     （启用最长预留时长时截止时刻为 reservedAt+时长，等待审批的时间不计
//     入；未启用时截止时刻保持零值）；
//   - 追加一条出资账户的预留记录，金额为预估费用全额，原因由调用方给出
//     （直接受理与批准的说明不同）。
//
// 资金变化、请求转为已预留与预留记录在同一次持锁调用中完成，不会观察到
// 只完成一部分的结果。审批路径的审批决定信息（决定时间、出资账户）与
// 使用账户零金额批准记录由 Approve 在调用本函数后另行追加，直接受理则
// 两者都不产生。必须在持锁状态下调用，且调用前已完成余额与额度检查。
func (w *Wallet) reserveFeeLocked(p *policy, payer *account, req *request, reservedAt time.Time, reason string) {
	fee := req.estimatedFee
	payer.available -= fee
	payer.reserved += fee
	p.reservedTotal += fee

	req.state = RequestReserved
	req.reservedAt = reservedAt
	req.reserveDuration = p.maxReserveDuration
	if p.maxReserveDuration > 0 {
		req.reserveDeadline = reservedAt.Add(p.maxReserveDuration)
	}

	w.ledger = append(w.ledger, LedgerEntry{
		Kind:      LedgerReserve,
		AccountID: payer.id,
		RequestID: req.requestID,
		Amount:    fee,
		Reason:    reason,
		At:        reservedAt,
	})
}

// releaseReservationLocked 一次性释放一笔已预留费用，供“结算实际费用”、
// “取消已预留请求”与“预留超时全额退回”三个入口共用，使三种操作的资金
// 核算遵循同一规则，避免分别维护出资账户余额与策略预留总额：
//   - 无论结算、取消还是超时，出资账户的预留余额与该策略的预留总额都按
//     本请求的预估费用全额减少；其他请求占用的金额保持不变；
//   - refund 退回该出资账户的可用余额（资金始终归策略指定的出资账户，不
//     因申请账户不同而改变），spent 计入策略已花费总额：
//     结算时 refund=预估-实际、spent=实际（实际为零时全额退回，实际等于
//     预估时 refund 为零）；取消与预留超时 refund=预估全额、spent=0；
//   - 本函数只统一资金核算，不改变请求状态、终态时间或账本：状态、终态
//     （已结算/已取消/独立的预留超时）与各自的账本记录（实际扣减、正数
//     差额才追加的退款、超时状态记录）仍由各调用方按原内容与顺序追加。
//
// 账户余额、策略金额与调用方随后写入的请求状态、账本在同一把锁内连续
// 完成，不会观察到只释放一部分的结果。p 为该请求所属策略；与超时路径
// 原有的防御一致，策略缺失时只释放账户余额、不触碰策略总额。必须在持锁
// 状态下调用。
func (w *Wallet) releaseReservationLocked(p *policy, payer *account, req *request, refund, spent int64) {
	fee := req.estimatedFee
	payer.reserved -= fee
	payer.available += refund
	if p != nil {
		p.reservedTotal -= fee
		p.spentTotal += spent
	}
}

// quotaWouldExceedLocked 判断在策略 p 上再计入 fee 后，共享累计合计是否
// 严格超过累计上限，并返回计入前“现存预留 + 已结算实际费用”的合计（用于
// 错误信息；该合计本身越过 int64 范围时返回 0）。现存预留、已结算实际费用
// 与本次费用各自合法、但合计越过 int64 范围时 exceed 同样为 true：不能让
// 求和回绕成小（或负）数后被当成额度充足。累计上限与单次上限仍允许取
// int64 最大值，这里不缩小金额范围。必须在持锁状态下调用。
func quotaWouldExceedLocked(p *policy, fee int64) (used int64, exceed bool) {
	used, ok := addInt64(p.reservedTotal, p.spentTotal)
	if !ok {
		return 0, true
	}
	total, ok := addInt64(used, fee)
	if !ok {
		return used, true
	}
	return used, total > p.maxTotal
}

// Request 查询某使用账户下的代付请求。待审批请求到期即转为过期终态，
// 已预留请求到最长预留时长即转为预留超时终态，查询结果反映最新状态；
// 超时释放时间记录的是截止时刻本身，而不是本次查询时刻。
func (w *Wallet) Request(accountID, requestID string) (RequestView, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	req, _, err := w.lookupRequest(accountID, requestID)
	if err != nil {
		return RequestView{}, err
	}
	now := w.now()
	w.refreshPendingLocked(req, now)
	w.refreshReservedLocked(req, now)
	return req.view(), nil
}

// Ledger 返回全部账本记录的副本，按产生顺序排列。
// 申请被拒绝的记录（Kind 为 LedgerRejection）也包含在内并带有原因。
// 查询时已到期预留的自动退款与超时留痕也会在返回前补齐。
func (w *Wallet) Ledger() []LedgerEntry {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.expireReservationsLocked(w.now())
	out := make([]LedgerEntry, len(w.ledger))
	copy(out, w.ledger)
	return out
}

// AccountLedger 返回与指定账户相关的账本记录副本。
//
// 对出资账户返回预留、扣减、退回等资金变动记录；对待审批、批准、拒绝、
// 取消、过期等状态记录（无金额变动），其 AccountID 为发起申请的使用
// 账户，因此按使用账户查询可看到该请求的完整状态留痕。
func (w *Wallet) AccountLedger(accountID string) []LedgerEntry {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.expireReservationsLocked(w.now())
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

// appendRejection 必须在持锁状态下调用。拒绝记录不涉及金额变动，
// 只记录使用账户、请求编号与具体原因。
func (w *Wallet) appendRejection(in RequestInput, at time.Time, cause error) {
	w.ledger = append(w.ledger, LedgerEntry{
		Kind:      LedgerRejection,
		AccountID: in.AccountID,
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
			ID:                 p.id,
			PayerAccountID:     p.payerAccountID,
			AllowedAccountIDs:  ids,
			Operation:          p.operation,
			Payee:              p.payee,
			StartsAt:           p.startsAt,
			EndsAt:             p.endsAt,
			MaxPerRequest:      p.maxPerRequest,
			MaxTotal:           p.maxTotal,
			ApprovalThreshold:  p.approvalThreshold,
			ApprovalWait:       p.approvalWait,
			MaxReserveDuration: p.maxReserveDuration,
		},
		ReservedTotal:        p.reservedTotal,
		SpentTotal:           p.spentTotal,
		Deactivated:          p.deactivated,
		DeactivatedAt:        p.deactivatedAt,
		DeactivatorAccountID: p.deactivatorAccountID,
		DeactivateReason:     p.deactivateReason,
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
		SettledAt:         r.settledAt,
		WaitDeadline:      r.waitDeadline,
		ReservedAt:        r.reservedAt,
		ReserveDuration:   r.reserveDuration,
		ReserveDeadline:   r.reserveDeadline,
		ReserveExpiredAt:  r.reserveExpiredAt,
		DecidedAt:         r.decidedAt,
		ApproverAccountID: r.approverAccountID,
		RejectReason:      r.rejectReason,
	}
}
