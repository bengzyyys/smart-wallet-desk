package wallet

import (
	"sort"
	"time"
)

// ApplyRequest 是代付申请参数。
type ApplyRequest struct {
	UserAccountID string
	SessionID     string
	DeviceID      string
	PolicyID      string
	RequestNo     string
	OpType        string
	Payee         string
	EstimatedFee  int64
}

// Apply 受理一笔代付申请。
// 依次校验会话、策略授权、时间窗口、费用与额度，全部满足且出资账户余额足够时，
// 从出资账户可用余额中预留预估费用。被拒绝时返回明确错误并记录具体原因，不产生扣款。
//
// 会话检查通过后，同一使用账户重复提交已受理的请求编号：策略、操作类型、收款方、
// 预估费用一致时返回已有结果，不再预留；任一内容不同则报冲突。
func (w *Wallet) Apply(req ApplyRequest) (*PaymentRequest, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()

	if req.RequestNo == "" {
		return nil, w.rejectLocked(req, ErrRequestNoRequired, "请求编号不能为空", now)
	}
	if req.EstimatedFee <= 0 {
		return nil, w.rejectLocked(req, ErrInvalidFee, "预估费用必须为正", now)
	}

	// 会话检查：不存在、所属账户不符、设备不符、已过期或已吊销都要拒绝。
	if err := w.checkSessionLocked(req.SessionID, req.UserAccountID, req.DeviceID, now); err != nil {
		return nil, w.rejectLocked(req, err, sessionReason(err), now)
	}

	// 幂等：会话检查通过后，同一使用账户的同一请求编号。
	key := requestKey(req.UserAccountID, req.RequestNo)
	if existing, exists := w.requests[key]; exists {
		if existing.PolicyID == req.PolicyID &&
			existing.OpType == req.OpType &&
			existing.Payee == req.Payee &&
			existing.EstimatedFee == req.EstimatedFee {
			return cloneRequest(existing), nil
		}
		return nil, w.rejectLocked(req, ErrRequestConflict, "请求内容与已受理请求冲突", now)
	}

	// 策略与授权检查。
	pol, ok := w.policies[req.PolicyID]
	if !ok {
		return nil, w.rejectLocked(req, ErrPolicyNotFound, "策略不存在", now)
	}
	if !containsString(pol.AllowedAccounts, req.UserAccountID) {
		return nil, w.rejectLocked(req, ErrUnauthorized, "使用账户不在策略允许列表中", now)
	}
	if pol.OpType != req.OpType {
		return nil, w.rejectLocked(req, ErrUnauthorized, "操作类型与策略不符", now)
	}
	if pol.Payee != req.Payee {
		return nil, w.rejectLocked(req, ErrUnauthorized, "收款方与策略不符", now)
	}
	// 时间窗口含开始、不含结束。
	if now.Before(pol.StartAt) || !now.Before(pol.EndAt) {
		return nil, w.rejectLocked(req, ErrUnauthorized, "不在策略有效期内", now)
	}
	if req.EstimatedFee > pol.PerTxCap {
		return nil, w.rejectLocked(req, ErrFeeExceedsCap, "预估费用超过单次费用上限", now)
	}

	// 累计额度：多个使用账户共享，预留中的费用也占用额度。
	if pol.cumulativeUsed+req.EstimatedFee > pol.CumulativeCap {
		return nil, w.rejectLocked(req, ErrFeeExceedsCap, "累计费用超出策略累计上限", now)
	}

	funding := w.accounts[pol.FundingAccountID]
	if funding.Available < req.EstimatedFee {
		return nil, w.rejectLocked(req, ErrInsufficientFunds, "出资账户可用余额不足", now)
	}

	// 受理：从可用余额预留预估费用。
	funding.Available -= req.EstimatedFee
	funding.Reserved += req.EstimatedFee
	pol.cumulativeUsed += req.EstimatedFee

	pr := &PaymentRequest{
		ID:            w.nextID("req"),
		RequestNo:     req.RequestNo,
		UserAccountID: req.UserAccountID,
		PolicyID:      req.PolicyID,
		OpType:        req.OpType,
		Payee:         req.Payee,
		EstimatedFee:  req.EstimatedFee,
		Status:        StatusPending,
		CreatedAt:     now,
	}
	w.requests[key] = pr
	w.appendLedgerLocked(pol.FundingAccountID, pr.ID, "reserve", req.EstimatedFee, now)
	return cloneRequest(pr), nil
}

// Settle 结算一笔已受理的请求。实际费用允许为零但不得超过预估值；
// 完成后扣除实际费用并退回差额。超出预估值或负数的结算被拒绝并保留预留。
// 相同实际费用的重复结算幂等，不同实际费用的重复结算报冲突；已取消不能结算。
func (w *Wallet) Settle(userAccountID, requestNo string, actualFee int64) (*PaymentRequest, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()

	pr, ok := w.requests[requestKey(userAccountID, requestNo)]
	if !ok {
		return nil, ErrRequestNotFound
	}
	if actualFee < 0 {
		return nil, ErrInvalidActualFee
	}
	if pr.Status == StatusSettled {
		if pr.ActualFee == actualFee {
			return cloneRequest(pr), nil
		}
		return nil, ErrRequestConflict
	}
	if pr.Status == StatusCancelled {
		return nil, ErrAlreadyCancelled
	}
	if actualFee > pr.EstimatedFee {
		return nil, ErrInvalidActualFee
	}

	pol := w.policies[pr.PolicyID]
	funding := w.accounts[pol.FundingAccountID]

	// 预留解冻，扣除实际费用，退回差额。
	funding.Reserved -= pr.EstimatedFee
	funding.Available += pr.EstimatedFee - actualFee
	pol.cumulativeUsed -= pr.EstimatedFee
	pol.cumulativeUsed += actualFee

	pr.Status = StatusSettled
	pr.ActualFee = actualFee
	pr.SettledAt = &now

	w.appendLedgerLocked(pol.FundingAccountID, pr.ID, "deduct", actualFee, now)
	if refund := pr.EstimatedFee - actualFee; refund > 0 {
		w.appendLedgerLocked(pol.FundingAccountID, pr.ID, "refund", refund, now)
	}
	return cloneRequest(pr), nil
}

// Cancel 取消一笔未结算请求，预留费用全部退回。
// 重复取消幂等；已结算不能取消。
func (w *Wallet) Cancel(userAccountID, requestNo string) (*PaymentRequest, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()

	pr, ok := w.requests[requestKey(userAccountID, requestNo)]
	if !ok {
		return nil, ErrRequestNotFound
	}
	if pr.Status == StatusCancelled {
		return cloneRequest(pr), nil
	}
	if pr.Status == StatusSettled {
		return nil, ErrAlreadySettled
	}

	pol := w.policies[pr.PolicyID]
	funding := w.accounts[pol.FundingAccountID]

	funding.Reserved -= pr.EstimatedFee
	funding.Available += pr.EstimatedFee
	pol.cumulativeUsed -= pr.EstimatedFee

	pr.Status = StatusCancelled
	w.appendLedgerLocked(pol.FundingAccountID, pr.ID, "refund", pr.EstimatedFee, now)
	return cloneRequest(pr), nil
}

// GetRequest 返回指定使用账户下的请求。
func (w *Wallet) GetRequest(userAccountID, requestNo string) (*PaymentRequest, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	pr, ok := w.requests[requestKey(userAccountID, requestNo)]
	if !ok {
		return nil, ErrRequestNotFound
	}
	return cloneRequest(pr), nil
}

// ListRequests 返回指定使用账户的全部请求，按创建时间与编号排序。
func (w *Wallet) ListRequests(userAccountID string) []PaymentRequest {
	w.mu.Lock()
	defer w.mu.Unlock()

	out := make([]PaymentRequest, 0)
	for _, pr := range w.requests {
		if pr.UserAccountID == userAccountID {
			out = append(out, *pr)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].RequestNo < out[j].RequestNo
	})
	return out
}

// Rejections 返回指定使用账户（或全部，userAccountID 为空时）的拒绝记录，按时间排序。
func (w *Wallet) Rejections(userAccountID string) []Rejection {
	w.mu.Lock()
	defer w.mu.Unlock()

	out := make([]Rejection, 0, len(w.rejections))
	for _, r := range w.rejections {
		if userAccountID == "" || r.UserAccountID == userAccountID {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].RequestNo < out[j].RequestNo
	})
	return out
}

// rejectLocked 记录一笔拒绝申请及具体原因，并返回对应的错误。
func (w *Wallet) rejectLocked(req ApplyRequest, err error, reason string, now time.Time) error {
	w.rejections = append(w.rejections, &Rejection{
		UserAccountID: req.UserAccountID,
		PolicyID:      req.PolicyID,
		RequestNo:     req.RequestNo,
		OpType:        req.OpType,
		Payee:         req.Payee,
		EstimatedFee:  req.EstimatedFee,
		Reason:        reason,
		CreatedAt:     now,
	})
	return err
}

func sessionReason(err error) string {
	switch err {
	case ErrSessionNotFound:
		return "会话不存在"
	case ErrSessionWrongAccount:
		return "会话与使用账户不符"
	case ErrSessionWrongDevice:
		return "会话与设备不符"
	case ErrSessionRevoked:
		return "会话已吊销"
	case ErrSessionExpired:
		return "会话已过期"
	default:
		return "会话校验失败"
	}
}

func cloneRequest(r *PaymentRequest) *PaymentRequest {
	cp := *r
	return &cp
}
