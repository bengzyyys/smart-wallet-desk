package wallet

import "time"

// CreateAccount 创建一个具有唯一编号和初始代付余额的账户。
// 初始余额为负、编号重复时返回明确错误，且不改变任何已有记录。
func (w *Wallet) CreateAccount(id string, initialBalance int64) (*Account, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if initialBalance < 0 {
		return nil, ErrInvalidAmount
	}
	if id == "" {
		return nil, ErrInvalidAmount
	}
	if _, ok := w.accounts[id]; ok {
		return nil, ErrAccountExists
	}
	acc := &Account{
		ID:        id,
		Available: initialBalance,
		Reserved:  0,
		CreatedAt: w.now(),
	}
	w.accounts[id] = acc
	return cloneAccount(acc), nil
}

// GetAccount 返回账户的可用与预留余额。
func (w *Wallet) GetAccount(id string) (*Account, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	acc, ok := w.accounts[id]
	if !ok {
		return nil, ErrAccountNotFound
	}
	return cloneAccount(acc), nil
}

func cloneAccount(a *Account) *Account {
	cp := *a
	return &cp
}

// appendLedgerLocked 在持锁状态下追加一条账本记录。
func (w *Wallet) appendLedgerLocked(accountID, requestID, typ string, amount int64, now time.Time) {
	w.seq++
	w.ledger = append(w.ledger, &LedgerEntry{
		ID:        w.seq,
		AccountID: accountID,
		RequestID: requestID,
		Type:      typ,
		Amount:    amount,
		CreatedAt: now,
	})
}

// Ledger 返回指定账户的全部账本记录（预留、扣减、退回），按发生顺序排列。
func (w *Wallet) Ledger(accountID string) ([]LedgerEntry, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, ok := w.accounts[accountID]; !ok {
		return nil, ErrAccountNotFound
	}
	out := make([]LedgerEntry, 0)
	for _, e := range w.ledger {
		if e.AccountID == accountID {
			out = append(out, *e)
		}
	}
	return out, nil
}
