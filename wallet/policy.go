package wallet

import (
	"errors"
	"fmt"
	"time"
)

// policyParams 是策略自身参数规则所需的最小数据视图，保存（PolicySpec）
// 与备份恢复（policyBackupV1）各自映射进来后共用同一套校验，不再在两个
// 入口分别维护限额、时间窗与审批配置的判断。
//
// 这里只包含“策略自身参数”的规则：必填内容、正数限额、时间窗先后、
// 审批开关与等待时长、最长预留时长。账户是否存在、允许账户列表是否重复、
// 策略编号是否重复等入口各自的要求不在此处，仍由 SavePolicy 与 Restore
// 分别按原有规则判断（保存时同一允许账户重复出现按同一个授权账户处理，
// 备份中重复列出授权账户则必须拒绝）。
type policyParams struct {
	operation          string
	payee              string
	maxPerRequest      int64
	maxTotal           int64
	approvalThreshold  int64
	approvalWait       time.Duration
	maxReserveDuration time.Duration
	startsAt           time.Time
	endsAt             time.Time
	payerAccountID     string
	allowedAccountIDs  []string
}

// params 把保存入口的策略规格映射为共用的参数视图。
func (s PolicySpec) params() policyParams {
	return policyParams{
		operation:          s.Operation,
		payee:              s.Payee,
		maxPerRequest:      s.MaxPerRequest,
		maxTotal:           s.MaxTotal,
		approvalThreshold:  s.ApprovalThreshold,
		approvalWait:       s.ApprovalWait,
		maxReserveDuration: s.MaxReserveDuration,
		startsAt:           s.StartsAt,
		endsAt:             s.EndsAt,
		payerAccountID:     s.PayerAccountID,
		allowedAccountIDs:  s.AllowedAccountIDs,
	}
}

// params 把备份中的策略记录映射为共用的参数视图。
func (p policyBackupV1) params() policyParams {
	return policyParams{
		operation:          p.Operation,
		payee:              p.Payee,
		maxPerRequest:      p.MaxPerRequest,
		maxTotal:           p.MaxTotal,
		approvalThreshold:  p.ApprovalThreshold,
		approvalWait:       p.ApprovalWait.std(),
		maxReserveDuration: p.MaxReserveDuration.std(),
		startsAt:           p.StartsAt.std(),
		endsAt:             p.EndsAt.std(),
		payerAccountID:     p.PayerAccountID,
		allowedAccountIDs:  p.AllowedAccountIDs,
	}
}

// 策略参数校验的具体原因以哨兵错误定义，供保存与恢复两个入口共用同一
// 措辞；需要带上参数值的原因（如门槛超过单次上限）在校验处直接构造。
var (
	errPolicyOperationPayeeRequired     = errors.New("operation and payee are required")
	errPolicyLimitsPositive             = errors.New("limits must be positive")
	errPolicyApprovalThresholdNegative  = errors.New("approval threshold must not be negative")
	errPolicyApprovalWaitNonPositive    = errors.New("approval wait must be positive when approval is enabled")
	errPolicyMaxReserveDurationNegative = errors.New("max reserve duration must not be negative")
	errPolicyWindowReversed             = errors.New("starts-at must be before ends-at")
	errPolicyAccountsRequired           = errors.New("payer account and at least one allowed account are required")
)

// validatePolicyParams 校验策略自身的参数规则，保存策略与从备份恢复共用
// 同一份判断；返回不带任何入口错误包装的具体原因，由调用方分别包装成
// ErrPolicyInvalid 或 ErrBackupInvalid。校验只针对参数本身，不要求策略在
// 当前时刻有效：时间窗本身合法（开始严格早于结束）但已经结束或尚未开始
// 的策略同样通过；账户存在性与允许账户列表重复与否由调用方另行判断。
//
// 审批门槛与等待时长的关系在此唯一维护：门槛为零表示关闭审批，此时等待
// 时长不被使用，正、零、负（含可保存、导出、恢复并查询回原值的负值）都
// 接受；门槛为正时等待时长必须为正，且门槛不得超过单次上限——恰好等于
// 单次上限仍合法（没有费用能严格超过它）。最长预留时长是独立设置：零
// 表示关闭预留超时，负值始终无效，不能因为审批关闭就一并忽略。
func validatePolicyParams(p policyParams) error {
	if p.operation == "" || p.payee == "" {
		return errPolicyOperationPayeeRequired
	}
	if p.maxPerRequest <= 0 || p.maxTotal <= 0 {
		return errPolicyLimitsPositive
	}
	// 审批门槛：零表示关闭；不得为负，不得超过单次上限；开启时等待时长必须为正。
	if p.approvalThreshold < 0 {
		return errPolicyApprovalThresholdNegative
	}
	if p.approvalThreshold > 0 {
		if p.approvalThreshold > p.maxPerRequest {
			return fmt.Errorf("approval threshold %d exceeds per-request limit %d", p.approvalThreshold, p.maxPerRequest)
		}
		if p.approvalWait <= 0 {
			return errPolicyApprovalWaitNonPositive
		}
	}
	// 审批关闭（门槛为零）时等待时长不做要求：保存与恢复都保留原值（允许
	// 负值）。最长预留时长独立判断：零表示关闭，负值始终无效。
	if p.maxReserveDuration < 0 {
		return errPolicyMaxReserveDurationNegative
	}
	if !p.startsAt.Before(p.endsAt) {
		return errPolicyWindowReversed
	}
	if p.payerAccountID == "" || len(p.allowedAccountIDs) == 0 {
		return errPolicyAccountsRequired
	}
	return nil
}
