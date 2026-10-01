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
	// ApprovalThreshold 为大额审批门槛，必须不超过 MaxPerRequest。
	// 为零表示关闭审批，申请通过全部检查后直接预留；为正则预估费用
	// 严格超过该门槛的申请先进入待审批，由出资账户会话批准或拒绝；
	// 不允许为负。
	ApprovalThreshold int64
	// ApprovalWait 为审批开启时待审批请求的最长等待时长，必须为正；
	// 审批关闭（ApprovalThreshold 为零）时该字段被忽略。
	ApprovalWait time.Duration
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
	// RequestPendingApproval 已通过会话、授权、余额与全部硬限额检查，
	// 但预估费用超过审批门槛，等待出资账户批准；此状态不冻结余额、
	// 不占用共享累计额度。
	RequestPendingApproval RequestState = iota
	// RequestReserved 已受理并完成费用预留，等待结算或取消。
	RequestReserved
	// RequestSettled 已按实际费用结算。
	RequestSettled
	// RequestCancelled 未结算即取消（已预留的退回全部预留；待审批
	// 的不涉及任何资金）。
	RequestCancelled
	// RequestRejected 待审批请求被出资账户明确拒绝。
	RequestRejected
	// RequestExpired 待审批请求在等待期限内未获批准，到达期限即过期。
	RequestExpired
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

// ApprovalInput 是批准或拒绝一笔待审批请求所需的身份与理由信息。
//
// 批准与拒绝都必须使用策略出资账户绑定当前设备的有效会话：
// ApproverAccountID 必须是出资账户，SessionID 必须属于该账户且绑定
// DeviceID，会话未吊销、未到期。Reason 仅拒绝时使用，不能为空或全空白。
type ApprovalInput struct {
	ApproverAccountID string
	SessionID         string
	DeviceID          string
	// Reason 为拒绝原因；批准时忽略，拒绝时不能为空或全为空白。
	Reason string
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
	// WaitUntil 为待审批请求的等待期限（提交时刻加等待时长、策略结束
	// 时间与申请会话到期时间三者的最早值）；非待审批产生的请求为零值。
	WaitUntil time.Time
	// DecidedAt 为批准或拒绝的决定时间；过期时为流转到过期终态的时间。
	DecidedAt time.Time
	// ApproverAccountID 为作出批准/拒绝决定的出资账户。
	ApproverAccountID string
	// RejectReason 为拒绝原因；仅拒绝状态下非空。
	RejectReason string
	SettledAt    time.Time
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
	// LedgerPendingApproval 申请进入待审批的留痕，不改变任何金额，
	// 也不冻结余额或占用共享累计额度。
	LedgerPendingApproval
	// LedgerApproval 待审批请求获批准并完成一次性预留的留痕；金额为 0，
	// 对应的资金变动另记一条 LedgerReserve。
	LedgerApproval
	// LedgerApprovalRejected 待审批请求被出资账户拒绝的留痕，不涉及金额。
	LedgerApprovalRejected
	// LedgerCancelled 请求被取消的状态留痕，不涉及金额（已预留请求的
	// 退款另记一条 LedgerRefund）。
	LedgerCancelled
	// LedgerExpired 待审批请求到达等待期限未获批准的留痕，不涉及金额。
	LedgerExpired
)

// LedgerEntry 是一条账本记录。
type LedgerEntry struct {
	// Kind 为记录类型。
	Kind LedgerKind
	// AccountID 为资金发生变动的出资账户；状态留痕与申请拒绝记录中为空。
	AccountID string
	// UsageAccountID 为发起代付申请的使用账户；所有与具体请求关联的
	// 记录都会填写，便于按使用账户追溯。
	UsageAccountID string
	// RequestID 为关联的代付请求编号；拒绝记录也会尽量记录。
	RequestID string
	// Amount 为金额（最小货币单位，非负）；不涉及资金的记录为 0。
	Amount int64
	// Reason 为拒绝原因或补充说明。
	Reason string
	At     time.Time
}
