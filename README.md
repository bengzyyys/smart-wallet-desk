# 智能钱包与代付策略

这是一个在本机运行的内存态智能钱包与代付策略模块，供本机程序调用。所有状态只在同一次运行内保留，金额一律使用最小货币单位的整数。

## 能力

- **账户**：创建带唯一编号和初始代付余额的账户，查询可用/预留余额与账本。
- **会话**：账户可创建绑定设备、指定到期时间的会话，可随时吊销；到期时刻及之后视为过期。吊销后新申请不再受理，已预留费用的请求仍可结算或取消。
- **代付策略**：明确出资账户、允许使用的账户、操作类型、收款方、起止时间、单次费用上限和累计费用上限。时间窗口含开始、不含结束，无匹配授权默认拒绝。
- **申请**：校验会话、策略授权、时间窗口、费用与累计额度，全部满足且出资账户余额足够时，从可用余额预留预估费用。拒绝记录具体原因，不产生扣款。
- **结算**：实际费用允许为零但不得超过预估；完成后扣除实际费用并退回差额。超预估或负数拒绝并保留预留。
- **取消**：未结算请求可全额退回。
- **幂等**：同一使用账户重复提交已受理的请求编号，策略/操作类型/收款方/预估费用一致时返回已有结果；任一内容不同报冲突。相同实际费用的重复结算、重复取消不重复记账。
- **并发安全**：所有方法加锁，多个请求同时申请、结算或取消时余额与共享额度不会超扣，同一请求只有一个终态。

## 使用

```bash
go test ./...
```

示例：

```go
w := wallet.New()
w.CreateAccount("fund", 1000)
w.CreateAccount("u1", 0)
sess, _ := w.CreateSession("u1", "device-1", time.Now().Add(time.Hour))
w.CreatePolicy(wallet.PolicySpec{
    ID: "p1", FundingAccountID: "fund", AllowedAccounts: []string{"u1"},
    OpType: "pay", Payee: "shop",
    StartAt: time.Now().Add(-time.Hour), EndAt: time.Now().Add(24 * time.Hour),
    PerTxCap: 100, CumulativeCap: 1000,
})
req, _ := w.Apply(wallet.ApplyRequest{
    UserAccountID: "u1", SessionID: sess.ID, DeviceID: "device-1",
    PolicyID: "p1", RequestNo: "r-001", OpType: "pay", Payee: "shop",
    EstimatedFee: 30,
})
w.Settle("u1", "r-001", 20) // 扣 20，退 10
```
