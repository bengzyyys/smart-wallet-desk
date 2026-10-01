// Package wallet 提供进程内的智能钱包与代付策略能力。
//
// 它管理账户代付费用的预留、结算与退回，不执行真实转账；所有状态只在
// 同一个 Wallet 实例的生命周期内保留。
package wallet

// Ready 表示基线可以运行。
func Ready() bool { return true }
