package wallet

import "errors"

// 资金与会话相关错误。
var (
	// ErrInvalidAmount 金额非法（负数等）。
	ErrInvalidAmount = errors.New("wallet: invalid amount")
	// ErrAccountExists 账户编号已存在。
	ErrAccountExists = errors.New("wallet: account already exists")
	// ErrAccountNotFound 账户不存在。
	ErrAccountNotFound = errors.New("wallet: account not found")
	// ErrEmptyAccountID 账户编号为空。
	ErrEmptyAccountID = errors.New("wallet: empty account id")
	// ErrInvalidArgument 必填参数缺失。
	ErrInvalidArgument = errors.New("wallet: invalid argument")
	// ErrSessionExists 会话编号已存在。
	ErrSessionExists = errors.New("wallet: session already exists")

	// ErrSessionNotFound 会话不存在。
	ErrSessionNotFound = errors.New("wallet: session not found")
	// ErrSessionAccountMismatch 会话不属于申请时提供的账户。
	ErrSessionAccountMismatch = errors.New("wallet: session belongs to another account")
	// ErrSessionDeviceMismatch 会话绑定的设备与申请设备不符。
	ErrSessionDeviceMismatch = errors.New("wallet: session device mismatch")
	// ErrSessionExpired 会话已到期（含到期时刻本身）。
	ErrSessionExpired = errors.New("wallet: session expired")
	// ErrSessionRevoked 会话已被吊销。
	ErrSessionRevoked = errors.New("wallet: session revoked")

	// ErrPolicyExists 策略编号已存在。
	ErrPolicyExists = errors.New("wallet: policy already exists")
	// ErrPolicyNotFound 策略不存在。
	ErrPolicyNotFound = errors.New("wallet: policy not found")
	// ErrPolicyInvalid 策略定义非法（限额、时间窗、账户等）。
	ErrPolicyInvalid = errors.New("wallet: invalid policy")
	// ErrPolicyDenied 申请不满足策略授权条件（默认拒绝）。
	ErrPolicyDenied = errors.New("wallet: request denied by policy")
	// ErrInsufficientBalance 出资账户可用余额不足。
	ErrInsufficientBalance = errors.New("wallet: insufficient available balance")
	// ErrQuotaExceeded 超出策略单次或累计费用上限。
	ErrQuotaExceeded = errors.New("wallet: policy quota exceeded")

	// ErrRequestNotFound 请求不存在。
	ErrRequestNotFound = errors.New("wallet: request not found")
	// ErrConflict 幂等键相同但申请内容或结算金额不一致。
	ErrConflict = errors.New("wallet: conflicting request")
	// ErrAlreadySettled 请求已结算，不能取消或再次按不同金额结算。
	ErrAlreadySettled = errors.New("wallet: request already settled")
	// ErrAlreadyCancelled 请求已取消，不能结算。
	ErrAlreadyCancelled = errors.New("wallet: request already cancelled")
	// ErrSettleTooLarge 结算实际费用超过预留的预估费用。
	ErrSettleTooLarge = errors.New("wallet: actual fee exceeds estimated fee")
)
