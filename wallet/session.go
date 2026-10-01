package wallet

import "time"

// CreateSession 为账户创建一个绑定指定设备的会话，并指定到期时间。
func (w *Wallet) CreateSession(accountID, deviceID string, expiresAt time.Time) (*Session, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, ok := w.accounts[accountID]; !ok {
		return nil, ErrAccountNotFound
	}
	if deviceID == "" {
		return nil, ErrDeviceRequired
	}
	s := &Session{
		ID:        w.nextID("sess"),
		AccountID: accountID,
		DeviceID:  deviceID,
		ExpiresAt: expiresAt,
		CreatedAt: w.now(),
	}
	w.sessions[s.ID] = s
	return cloneSession(s), nil
}

// RevokeSession 吊销会话。吊销后该会话不能再受理新申请；
// 已预留费用的请求仍可结算或取消。重复吊销幂等。
func (w *Wallet) RevokeSession(sessionID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	s, ok := w.sessions[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	s.Revoked = true
	return nil
}

// GetSession 返回会话信息。
func (w *Wallet) GetSession(sessionID string) (*Session, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	s, ok := w.sessions[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}
	return cloneSession(s), nil
}

// checkSessionLocked 校验会话是否可用于代付申请。
// 不存在、所属账户不符、设备不符、已过期（到期时刻及之后）或已吊销都要拒绝。
func (w *Wallet) checkSessionLocked(sessionID, accountID, deviceID string, now time.Time) error {
	sess, ok := w.sessions[sessionID]
	if !ok {
		return ErrSessionNotFound
	}
	if sess.AccountID != accountID {
		return ErrSessionWrongAccount
	}
	if sess.DeviceID != deviceID {
		return ErrSessionWrongDevice
	}
	if sess.Revoked {
		return ErrSessionRevoked
	}
	if !now.Before(sess.ExpiresAt) {
		return ErrSessionExpired
	}
	return nil
}

func cloneSession(s *Session) *Session {
	cp := *s
	return &cp
}
