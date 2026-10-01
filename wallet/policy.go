package wallet

import "time"

// PolicySpec 是保存代付策略时的参数。
type PolicySpec struct {
	ID               string
	FundingAccountID string   // 出资账户，必须存在
	AllowedAccounts  []string // 允许使用的账户，必须存在
	OpType           string   // 操作类型
	Payee            string   // 收款方
	StartAt          time.Time
	EndAt            time.Time
	PerTxCap         int64 // 单次费用上限，必须为正
	CumulativeCap    int64 // 累计费用上限，必须为正
}

// CreatePolicy 保存一条代付策略。
// 出资或使用账户不存在、限额非正、开始时间不早于结束时间、编号重复时拒绝保存，
// 且不改变任何已有记录。
func (w *Wallet) CreatePolicy(spec PolicySpec) (*Policy, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if spec.ID == "" || spec.FundingAccountID == "" || spec.OpType == "" || spec.Payee == "" {
		return nil, ErrInvalidPolicy
	}
	if spec.PerTxCap <= 0 || spec.CumulativeCap <= 0 {
		return nil, ErrInvalidPolicy
	}
	if !spec.StartAt.Before(spec.EndAt) {
		return nil, ErrInvalidPolicy
	}
	if _, ok := w.policies[spec.ID]; ok {
		return nil, ErrPolicyExists
	}
	if _, ok := w.accounts[spec.FundingAccountID]; !ok {
		return nil, ErrInvalidPolicy
	}
	if len(spec.AllowedAccounts) == 0 {
		return nil, ErrInvalidPolicy
	}
	allowed := make([]string, 0, len(spec.AllowedAccounts))
	seen := make(map[string]bool, len(spec.AllowedAccounts))
	for _, id := range spec.AllowedAccounts {
		if id == "" {
			return nil, ErrInvalidPolicy
		}
		if _, ok := w.accounts[id]; !ok {
			return nil, ErrInvalidPolicy
		}
		if !seen[id] {
			seen[id] = true
			allowed = append(allowed, id)
		}
	}

	p := &Policy{
		ID:               spec.ID,
		FundingAccountID: spec.FundingAccountID,
		AllowedAccounts:  allowed,
		OpType:           spec.OpType,
		Payee:            spec.Payee,
		StartAt:          spec.StartAt,
		EndAt:            spec.EndAt,
		PerTxCap:         spec.PerTxCap,
		CumulativeCap:    spec.CumulativeCap,
	}
	w.policies[p.ID] = p
	return clonePolicy(p), nil
}

// GetPolicy 返回策略信息。
func (w *Wallet) GetPolicy(id string) (*Policy, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	p, ok := w.policies[id]
	if !ok {
		return nil, ErrPolicyNotFound
	}
	return clonePolicy(p), nil
}

func clonePolicy(p *Policy) *Policy {
	cp := *p
	cp.AllowedAccounts = append([]string(nil), p.AllowedAccounts...)
	return &cp
}
