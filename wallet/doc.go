// Package wallet 提供进程内的智能钱包与代付策略能力。
//
// 它管理账户代付费用的预留、结算与退回，不执行真实转账；所有状态默认只在
// 同一个 Wallet 实例的生命周期内保留。调用方可通过 (*Wallet).Export 导出
// 带版本号的 JSON 备份，并用包级函数 Restore 在另一个 Wallet 中恢复，从而
// 跨运行继续处理原有代付请求。
package wallet

// Ready 表示基线可以运行。
func Ready() bool { return true }
