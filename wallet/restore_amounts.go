package wallet

import "fmt"

// restoreAmounts 是恢复时的“金额归属表”：按备份保存的请求状态，把每笔
// 请求的费用一次性归入它唯一应当贡献的总额，恢复流程中的余额核对、退款
// 上限核对与策略累计额度核对都只读本表，不再各自遍历请求、各自维护一份
// “哪种状态计入哪个总额”的规则。
//
// 归属规则完全由备份保存的请求状态决定：
//   - 已预留（RequestReserved）：按预估费用同时计入其出资账户的预留余额
//     （reservedByAccount；一个出资账户在多条策略下的预留合并到同一项）与
//     所属策略的预留总额（reservedByPolicy）；
//   - 已结算（RequestSettled）：只按实际费用计入所属策略的已花费总额
//     （spentByPolicy），结算时已经退回的预估差额不再占用任何总额；
//   - 待审批、已取消、已拒绝、等待审批过期、预留超时：不贡献任何总额。
//
// 同一笔已预留请求对账户与策略的计入在 addRequest 一处成对完成，调用方
// 不会再把同一笔请求在账户、策略两处分别累加、分别维护。全部金额只反映
// 备份原有状态：归属汇总与核对必须在恢复时的到期自动处理（预留超时退回、
// 待审批过期）之前完成——某笔预留即使已经到了退款时间，也不能先退回它来
// 掩盖备份原本的超额或矛盾。
type restoreAmounts struct {
	reservedByAccount map[string]int64
	reservedByPolicy  map[string]int64
	spentByPolicy     map[string]int64
}

func newRestoreAmounts() *restoreAmounts {
	return &restoreAmounts{
		reservedByAccount: make(map[string]int64),
		reservedByPolicy:  make(map[string]int64),
		spentByPolicy:     make(map[string]int64),
	}
}

// addRequest 按请求在备份中保存的状态把费用归入对应总额；一笔请求至多
// 贡献一处账户金额与一处策略金额，其他状态不贡献任何总额。任何一次求和
// 越过 int64 范围都返回包装 ErrBackupInvalid 的错误，并指出对应的账户或
// 策略（求和溢出类别），由调用方整体拒绝恢复：各笔金额分别合法而合计超出
// int64 范围的备份同样不能通过。
func (a *restoreAmounts) addRequest(r *request) error {
	switch r.state {
	case RequestReserved:
		// 已预留：预估费用同时占用出资账户预留余额与所属策略预留总额，
		// 两处计入在同一分支成对完成，避免同一笔请求被重复维护。
		sum, ok := addInt64(a.reservedByAccount[r.payerAccountID], r.estimatedFee)
		if !ok {
			return fmt.Errorf("%w: account %q reserved sum overflows int64", ErrBackupInvalid, r.payerAccountID)
		}
		a.reservedByAccount[r.payerAccountID] = sum
		sum, ok = addInt64(a.reservedByPolicy[r.policyID], r.estimatedFee)
		if !ok {
			return fmt.Errorf("%w: policy %q reserved sum overflows int64", ErrBackupInvalid, r.policyID)
		}
		a.reservedByPolicy[r.policyID] = sum
	case RequestSettled:
		// 已结算：只有实际费用继续占用策略累计额度；预估超出实际的差额
		// 结算时已经退回，不能再算作占用。
		sum, ok := addInt64(a.spentByPolicy[r.policyID], r.actualFee)
		if !ok {
			return fmt.Errorf("%w: policy %q spent sum overflows int64", ErrBackupInvalid, r.policyID)
		}
		a.spentByPolicy[r.policyID] = sum
	}
	// 待审批、已取消、已拒绝、等待审批过期、预留超时均不贡献总额。
	return nil
}

// reconcile 针对备份保存的账户余额、策略金额与归属表执行全部金额核对：
//  1. 每个出资账户保存的预留余额必须等于其名下（可跨多条策略）全部已预留
//     请求预估费用之和；
//  2. 每个账户的可用余额加上现存预留不得越过 int64 上限——取消或预留超时
//     会把预留全额退回可用余额，合计一旦越过 MaxInt64，退回时余额就会
//     越界；合计恰好等于上限合法；
//  3. 每条策略保存的预留总额、已花费总额必须分别等于请求求和，且现存预留
//     加已花费不得严格超过共享累计上限：同一策略的多个使用账户合并核对，
//     不同策略分别核对；合计恰好等于上限合法；两项各自为 int64 而合计越过
//     int64 范围时按求和溢出拒绝，不能误判为额度充足。
//
// 三个阶段保持恢复原有的错误优先顺序：先核对全部金额一致性，再核对账户
// 退款上限，最后逐条核对策略累计上限。任何一项失败都返回指出问题账户或
// 策略的 ErrBackupInvalid，并区分金额不一致、累计超限与求和溢出；调用方
// 因此绝不会拿到部分恢复的钱包。本核对只看备份保存的资金占用状态，位于
// 恢复时到期自动退回之前：即使某笔已预留请求恢复时已到期、将全额退回，
// 也不能先退回再让原本超额或矛盾的备份通过。
func (a *restoreAmounts) reconcile(accounts map[string]*account, policies map[string]*policy) error {
	// ---- 账户预留余额 == 名下已预留请求预估费用之和（跨多条策略合并） ----
	for id, acc := range accounts {
		if got, want := acc.reserved, a.reservedByAccount[id]; got != want {
			return fmt.Errorf("%w: account %q reserved %d != sum of reserved requests %d", ErrBackupInvalid, id, got, want)
		}
	}

	// ---- 可用余额 + 现存预留不得越过 int64 上限 ----
	// reservedByAccount 已按出资账户汇总该账户在所有策略下仍处于已预留
	// 状态的费用（待审批不冻结费用，已结算、已取消、已预留超时的费用不再
	// 占用预留，故均不计入）。合计恰好等于上限合法（随后退回恰好得到上限
	// 金额）。
	for id, acc := range accounts {
		if _, ok := addInt64(acc.available, a.reservedByAccount[id]); !ok {
			return fmt.Errorf("%w: account %q available %d plus reserved %d overflows int64", ErrBackupInvalid, id, acc.available, a.reservedByAccount[id])
		}
	}

	// ---- 策略预留/已花费总额 == 请求求和，且合计不得超过累计上限 ----
	// 现存预留费用与已结算的实际费用共同占用策略的共享累计额度；待审批、
	// 已取消、被拒绝、待审批过期、预留超时的请求均不计入归属表。严格超过
	// 累计上限的备份自相矛盾（正常流程在受理时就会拒绝），必须整体拒绝；
	// 合计恰好等于上限合法，上限恰为 MaxInt64 时也不缩小可接受的金额范围。
	for id, p := range policies {
		reserved := a.reservedByPolicy[id]
		spent := a.spentByPolicy[id]
		if got, want := p.reservedTotal, reserved; got != want {
			return fmt.Errorf("%w: policy %q reserved total %d != sum of reserved requests %d", ErrBackupInvalid, id, got, want)
		}
		if got, want := p.spentTotal, spent; got != want {
			return fmt.Errorf("%w: policy %q spent total %d != sum of settled requests %d", ErrBackupInvalid, id, got, want)
		}
		used, ok := addInt64(reserved, spent)
		if !ok {
			return fmt.Errorf("%w: policy %q reserved %d plus spent %d overflows int64 and exceeds cumulative total limit %d",
				ErrBackupInvalid, id, reserved, spent, p.maxTotal)
		}
		if used > p.maxTotal {
			return fmt.Errorf("%w: policy %q reserved %d plus spent %d exceeds cumulative total limit %d",
				ErrBackupInvalid, id, reserved, spent, p.maxTotal)
		}
	}
	return nil
}
