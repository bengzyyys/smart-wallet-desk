package wallet

import "time"

// Balances 表示账户当前的资金状态，单位均为最小货币单位的整数。
type Balances struct {
	// Available 为可用于新预留的余额（初始余额减去已预留和已扣减）。
	Available int64
	// Reserved 为已受理但尚未结算或取消的请求所预留的费用总额。
	Reserved int64
}

// AccountView 是账户的只读视图。
type AccountView struct {
	ID        string
	Balances  Balances
	CreatedAt time.Time
}

// SessionState 描述会话状态。
type SessionState int

const (
	// SessionActive 有效会话。
	SessionActive SessionState = iota
	// SessionRevoked 已被主动吊销。
	SessionRevoked
)

// SessionView 是会话的只读视图。
type SessionView struct {
	ID        string
	AccountID string
	DeviceID  string
	ExpiresAt time.Time
	State     SessionState
	CreatedAt time.Time
}

// PolicySpec 定义一条代付策略。
//
// 多个使用账户共享同一累计额度。时间窗为 [StartsAt, EndsAt)。
type PolicySpec struct {
	// ID 为策略唯一编号，留空时拒绝保存。
	ID string
	// PayerAccountID 为出资账户。
	PayerAccountID string
	// AllowedAccountIDs 为允许发起代付的使用账户列表，不能为空。
	AllowedAccountIDs []string
	// Operation 为允许的操作类型，不能为空。
	Operation string
	// Payee 为允许的收款方，不能为空。
	Payee string
	// StartsAt 为授权时间窗起点（含）。
	StartsAt time.Time
	// EndsAt 为授权时间窗终点（不含）。
	EndsAt time.Time
	// MaxPerRequest 为单次费用上限，必须为正。
	MaxPerRequest int64
	// MaxTotal 为所有使用账户共享的累计费用上限，必须为正。
	MaxTotal int64
}

// PolicyView 是策略的只读视图。
type PolicyView struct {
	PolicySpec
	// ReservedTotal 为当前处于预留中的费用总额（占用累计额度）。
	ReservedTotal int64
	// SpentTotal 为已结算扣除的累计费用。
	SpentTotal int64
}

// RequestState 描述代付请求的生命周期状态。
type RequestState int

const (
	// RequestReserved 已受理并完成费用预留，等待结算或取消。
	RequestReserved RequestState = iota
	// RequestSettled 已按实际费用结算。
	RequestSettled
	// RequestCancelled 未结算即取消，预留已全部退回。
	RequestCancelled
)

// RequestInput 是代付申请内容。
type RequestInput struct {
	// PolicyID 指定适用策略。
	PolicyID string
	// RequestID 为使用账户维度的幂等请求编号。
	RequestID string
	// AccountID 为发起申请的使用账户。
	AccountID string
	// SessionID 为该账户绑定设备的有效会话。
	SessionID string
	// DeviceID 为当前设备，必须与会话绑定设备一致。
	DeviceID string
	// Operation 为操作类型，必须与策略一致。
	Operation string
	// Payee 为收款方，必须与策略一致。
	Payee string
	// EstimatedFee 为预估费用，必须为正，受理后按此金额预留。
	EstimatedFee int64
}

// RequestView 是代付请求的只读视图。
type RequestView struct {
	PolicyID       string
	RequestID      string
	AccountID      string
	PayerAccountID string
	Operation      string
	Payee          string
	EstimatedFee   int64
	ActualFee      int64
	State          RequestState
	CreatedAt      time.Time
	SettledAt      time.Time
}

// LedgerKind 标识账本记录类型。
type LedgerKind int

const (
	// LedgerReserve 预留：冻结预估费用。
	LedgerReserve LedgerKind = iota
	// LedgerSettle 扣减：结算时实际扣除的费用。
	LedgerSettle
	// LedgerRefund 退回：结算差额或取消时退回的预留。
	LedgerRefund
	// LedgerRejection 申请被拒绝的留痕，不涉及任何金额变动。
	LedgerRejection
)

// LedgerEntry 是一条账本记录。
type LedgerEntry struct {
	// Kind 为记录类型。
	Kind LedgerKind
	// AccountID 为资金发生变动的出资账户；拒绝记录中为空。
	AccountID string
	// RequestID 为关联的代付请求编号；拒绝记录也会尽量记录。
	RequestID string
	// Amount 为金额（最小货币单位，非负）；拒绝记录为 0。
	Amount int64
	// Reason 为拒绝原因或补充说明。
	Reason string
	At     time.Time
}
