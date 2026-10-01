package wallet

import "time"

// Account 是出资或使用账户，金额均以最小货币单位的整数表示。
type Account struct {
	ID        string
	Available int64 // 可用余额
	Reserved  int64 // 预留余额
	CreatedAt time.Time
}

// Session 是使用账户绑定到具体设备的会话。
type Session struct {
	ID        string
	AccountID string // 会话所属的使用账户
	DeviceID  string // 绑定的设备
	ExpiresAt time.Time
	Revoked   bool
	CreatedAt time.Time
}

// Policy 是一条代付策略。
type Policy struct {
	ID               string
	FundingAccountID string   // 出资账户
	AllowedAccounts  []string // 允许使用的账户
	OpType           string   // 操作类型
	Payee            string   // 收款方
	StartAt          time.Time
	EndAt            time.Time
	PerTxCap         int64 // 单次费用上限
	CumulativeCap    int64 // 累计费用上限

	cumulativeUsed int64 // 已占用的累计额度（预留中的预估费 + 已结算实际费）
}

// RequestStatus 是代付申请的状态。
type RequestStatus string

const (
	StatusPending   RequestStatus = "pending"   // 已受理，费用预留中
	StatusSettled   RequestStatus = "settled"   // 已结算
	StatusCancelled RequestStatus = "cancelled" // 已取消
)

// PaymentRequest 是一笔代付申请。
type PaymentRequest struct {
	ID            string
	RequestNo     string // 请求编号，同一使用账户下唯一
	UserAccountID string
	PolicyID      string
	OpType        string
	Payee         string
	EstimatedFee  int64
	ActualFee     int64 // 结算后的实际费用；未结算为 0
	Status        RequestStatus
	CreatedAt     time.Time
	SettledAt     *time.Time
}

// LedgerEntry 是一条账本记录。Amount 为金额（恒为正），Type 区分方向：
// reserve 表示预留（可用转预留），deduct 表示扣减（实际费用），refund 表示退回。
type LedgerEntry struct {
	ID        int64
	AccountID string
	RequestID string
	Type      string // "reserve" | "deduct" | "refund"
	Amount    int64
	CreatedAt time.Time
}

// Rejection 记录一笔被拒绝的申请及具体原因。
type Rejection struct {
	UserAccountID string
	PolicyID      string
	RequestNo     string
	OpType        string
	Payee         string
	EstimatedFee  int64
	Reason        string
	CreatedAt     time.Time
}
