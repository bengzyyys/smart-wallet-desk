package wallet

import (
	"fmt"
	"time"
)

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
	// ApprovalThreshold 为大额审批门槛：预估费用严格超过此值的申请进入
	// 待审批，由出资账户审批通过后才预留。零表示关闭审批，申请直接预留；
	// 不得为负，且不得超过单次费用上限。
	ApprovalThreshold int64
	// ApprovalWait 为待审批请求的最长等待时长（自提交时刻起算）。
	// 审批开启（ApprovalThreshold 为正）时必须为正；关闭时忽略。
	ApprovalWait time.Duration
	// MaxReserveDuration 为已预留请求的最长预留时长：自直接受理时刻，或
	// 待审批请求被批准成功的时刻起算（等待审批的时间不计入）。零表示关闭
	// 预留超时，已预留费用只在结算或取消时释放；不得为负。
	MaxReserveDuration time.Duration
}

// PolicyView 是策略的只读视图。
type PolicyView struct {
	PolicySpec
	// ReservedTotal 为当前处于预留中的费用总额（占用累计额度）。
	ReservedTotal int64
	// SpentTotal 为已结算扣除的累计费用。
	SpentTotal int64
	// Deactivated 表示策略是否已被出资账户主动停用；新保存的策略为 false。
	// 停用不可撤销，停用后策略不再受理新申请，编号也不能重新保存为新策略。
	Deactivated bool
	// DeactivatedAt 为首次停用时间；未停用时为零值。
	DeactivatedAt time.Time
	// DeactivatorAccountID 为执行停用的出资账户；未停用时为空。
	DeactivatorAccountID string
	// DeactivateReason 为去掉首尾空白后的停用理由；未停用时为空。
	DeactivateReason string
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
	// RequestPendingApproval 预估费用超过审批门槛，等待出资账户审批；
	// 此状态不冻结余额、不占用共享累计额度。
	RequestPendingApproval
	// RequestRejected 已被拒绝（审批拒绝、申请会话在等待期限内被吊销或
	// 策略被停用），终态。已到等待截止时刻后才吊销会话的请求不进入此态，
	// 而是 RequestExpired。
	RequestRejected
	// RequestExpired 待审批超过等待期限未获批准，终态。
	RequestExpired
	// RequestReservationExpired 已预留请求超过策略的最长预留时长仍未结算或
	// 取消，终态。预留已在截止时刻全额自动退回，与待审批等待过期
	// （RequestExpired）区分。
	RequestReservationExpired
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
	// WaitDeadline 为待审批期限（提交时刻 + 等待时长、策略结束时间、
	// 申请会话到期时间三者中的最早值）；所有走过审批流程的请求（待审批、
	// 被拒、过期、待审批取消及超门槛经批准后预留）保留该时刻，未超门槛
	// 直接预留的请求为零值。
	WaitDeadline time.Time
	// ReservedAt 为费用实际预留完成的时刻：直接受理的请求为受理时刻，
	// 批准后预留的请求为批准成功时刻。从未预留（待审批、被拒、待审批过期、
	// 待审批取消）时为零值；预留超时后仍保留该时刻。
	ReservedAt time.Time
	// ReserveDuration 为受理/批准时适用的策略最长预留时长；策略未启用预留
	// 超时（时长为零）或请求从未预留时为零。
	ReserveDuration time.Duration
	// ReserveDeadline 为启用预留超时时的截止时刻（预留时刻 + 最长预留时长）；
	// 未启用或请求从未预留时为零值。结算或取消不会改变该时刻。
	ReserveDeadline time.Time
	// ReserveExpiredAt 为预留因超时自动释放的时刻；记录的是截止时刻本身，
	// 而非稍后实际处理（查询或新申请）的时刻。尚未超时或未启用时为零值。
	ReserveExpiredAt time.Time
	// DecidedAt 为审批决定时间（批准、拒绝或过期的时刻）；未决定时为零值。
	DecidedAt time.Time
	// ApproverAccountID 为作出批准或拒绝决定的出资账户；未决定时为空。
	ApproverAccountID string
	// RejectReason 为拒绝原因（审批拒绝、会话吊销或策略停用）；未拒绝时为空。
	RejectReason string
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
	// LedgerRejection 申请被拒绝或待审批被拒的留痕，不涉及任何金额变动。
	LedgerRejection
	// LedgerPendingApproval 申请进入待审批的状态留痕，无金额变动。
	LedgerPendingApproval
	// LedgerApproval 待审批被批准的状态留痕，无金额变动；批准带来的
	// 预留另记 LedgerReserve。
	LedgerApproval
	// LedgerCancellation 待审批请求被取消的状态留痕，无金额变动
	// （已预留请求的取消仍记 LedgerRefund）。
	LedgerCancellation
	// LedgerExpiration 待审批超过期限未获批准的状态留痕，无金额变动。
	LedgerExpiration
	// LedgerPolicyDeactivation 策略被出资账户主动停用的留痕，无金额变动；
	// AccountID 为出资账户，PolicyID 为被停用的策略，RequestID 为空。
	LedgerPolicyDeactivation
	// LedgerReservationExpiration 已预留请求超过最长预留时长被自动退回的
	// 状态留痕，无金额变动；AccountID 为发起申请的使用账户，关联请求编号。
	// 全额退款另记一条 LedgerRefund（AccountID 为出资账户，金额为预留全额）。
	LedgerReservationExpiration
)

// String 返回账本记录类型的固定英文名称，供备份恢复等错误信息引用，使
// 调用方无需按底层整数编号辨认记录类型。
func (k LedgerKind) String() string {
	switch k {
	case LedgerReserve:
		return "reserve"
	case LedgerSettle:
		return "settle"
	case LedgerRefund:
		return "refund"
	case LedgerRejection:
		return "rejection"
	case LedgerPendingApproval:
		return "pending-approval"
	case LedgerApproval:
		return "approval"
	case LedgerCancellation:
		return "cancellation"
	case LedgerExpiration:
		return "expiration"
	case LedgerPolicyDeactivation:
		return "policy-deactivation"
	case LedgerReservationExpiration:
		return "reservation-expiration"
	default:
		return fmt.Sprintf("unknown-ledger-kind-%d", int(k))
	}
}

// LedgerEntry 是一条账本记录。
type LedgerEntry struct {
	// Kind 为记录类型。
	Kind LedgerKind
	// AccountID 为资金发生变动的出资账户（预留、扣减、退回）；
	// 无金额变动的状态记录（待审批、批准、拒绝、取消、过期）中
	// 为发起申请的使用账户，用于关联使用账户。
	AccountID string
	// RequestID 为关联的代付请求编号。
	RequestID string
	// PolicyID 为关联的策略编号；仅策略停用等不针对具体请求的记录使用，
	// 其余记录为空（请求记录可用请求上的策略编号关联）。
	PolicyID string
	// Amount 为金额（最小货币单位，非负）；状态记录为 0。
	Amount int64
	// Reason 为拒绝原因或补充说明。
	Reason string
	At     time.Time
}
