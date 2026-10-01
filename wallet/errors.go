package wallet

import "errors"

var (
	ErrAccountExists       = errors.New("账户已存在")
	ErrAccountNotFound     = errors.New("账户不存在")
	ErrInvalidAmount       = errors.New("无效金额")
	ErrInvalidFee          = errors.New("费用必须为正")
	ErrDeviceRequired      = errors.New("设备标识不能为空")
	ErrSessionNotFound     = errors.New("会话不存在")
	ErrSessionExpired      = errors.New("会话已过期")
	ErrSessionRevoked      = errors.New("会话已吊销")
	ErrSessionWrongAccount = errors.New("会话与使用账户不符")
	ErrSessionWrongDevice  = errors.New("会话与设备不符")
	ErrPolicyExists        = errors.New("策略已存在")
	ErrPolicyNotFound      = errors.New("策略不存在")
	ErrInvalidPolicy       = errors.New("策略无效")
	ErrUnauthorized        = errors.New("无匹配的代付授权")
	ErrFeeExceedsCap       = errors.New("费用超出策略限额")
	ErrInsufficientFunds   = errors.New("出资账户余额不足")
	ErrRequestNotFound     = errors.New("请求不存在")
	ErrRequestConflict     = errors.New("请求内容冲突")
	ErrInvalidActualFee    = errors.New("实际费用无效")
	ErrRequestNoRequired   = errors.New("请求编号不能为空")
	ErrAlreadySettled      = errors.New("请求已结算")
	ErrAlreadyCancelled    = errors.New("请求已取消")
)
