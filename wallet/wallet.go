// Package wallet 是智能钱包与代付策略的本地基线。
package wallet

import (
	"fmt"
	"sync"
	"time"
)

// Wallet 是一个内存态的智能钱包：账户、会话、策略、申请与账本都只在同一次运行内保留。
// 所有方法并发安全；金额一律使用最小货币单位的整数。
type Wallet struct {
	mu         sync.Mutex
	now        func() time.Time
	seq        int64
	accounts   map[string]*Account
	sessions   map[string]*Session
	policies   map[string]*Policy
	requests   map[string]*PaymentRequest // key: userAccountID + "\x00" + requestNo
	ledger     []*LedgerEntry
	rejections []*Rejection
}

// New 创建一个空钱包。
func New() *Wallet {
	return &Wallet{
		now:      time.Now,
		accounts: make(map[string]*Account),
		sessions: make(map[string]*Session),
		policies: make(map[string]*Policy),
		requests: make(map[string]*PaymentRequest),
	}
}

func (w *Wallet) nextID(prefix string) string {
	w.seq++
	return fmt.Sprintf("%s-%d", prefix, w.seq)
}

// requestKey 返回使用账户与请求编号共同确定的幂等键。
func requestKey(userAccountID, requestNo string) string {
	return userAccountID + "\x00" + requestNo
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
