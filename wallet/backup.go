package wallet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"time"
)

// backupVersion 为当前支持的备份格式版本号。
const backupVersion = 1

// ErrBackupInvalid 表示备份文本为空、无法解析、缺少必需数据、版本不支持
// 或内容自相矛盾。恢复接口返回包装了具体原因的错误；任何校验失败都不会
// 返回部分恢复的钱包。
var ErrBackupInvalid = errors.New("wallet: invalid backup")

// timeJSON 以 RFC3339Nano（带纳秒与时区）序列化时间，零值时间序列化为
// 空字符串，避免把“尚未发生”的计时信息误恢复成公元元年。
type timeJSON time.Time

func (t timeJSON) MarshalJSON() ([]byte, error) {
	tm := time.Time(t)
	if tm.IsZero() {
		return []byte(`""`), nil
	}
	return json.Marshal(tm.Format(time.RFC3339Nano))
}

func (t *timeJSON) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s == "" {
		*t = timeJSON(time.Time{})
		return nil
	}
	tm, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		// 宽容解析无纳秒的 RFC3339。
		tm, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return err
		}
	}
	*t = timeJSON(tm)
	return nil
}

func (t timeJSON) std() time.Time { return time.Time(t) }

// durationJSON 以 int64 纳秒序列化时长，保持精确往返。
type durationJSON time.Duration

func (d durationJSON) MarshalJSON() ([]byte, error) {
	return json.Marshal(int64(d))
}

func (d *durationJSON) UnmarshalJSON(data []byte) error {
	var n int64
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*d = durationJSON(n)
	return nil
}

func (d durationJSON) std() time.Duration { return time.Duration(d) }

type accountBackupV1 struct {
	ID        string   `json:"id"`
	Available int64    `json:"available"`
	Reserved  int64    `json:"reserved"`
	CreatedAt timeJSON `json:"created_at"`
}

type sessionBackupV1 struct {
	ID        string   `json:"id"`
	AccountID string   `json:"account_id"`
	DeviceID  string   `json:"device_id"`
	ExpiresAt timeJSON `json:"expires_at"`
	Revoked   bool     `json:"revoked"`
	CreatedAt timeJSON `json:"created_at"`
}

type policyBackupV1 struct {
	ID                   string       `json:"id"`
	PayerAccountID       string       `json:"payer_account_id"`
	AllowedAccountIDs    []string     `json:"allowed_account_ids"`
	Operation            string       `json:"operation"`
	Payee                string       `json:"payee"`
	StartsAt             timeJSON     `json:"starts_at"`
	EndsAt               timeJSON     `json:"ends_at"`
	MaxPerRequest        int64        `json:"max_per_request"`
	MaxTotal             int64        `json:"max_total"`
	ApprovalThreshold    int64        `json:"approval_threshold"`
	ApprovalWait         durationJSON `json:"approval_wait_nanos"`
	MaxReserveDuration   durationJSON `json:"max_reserve_duration_nanos"`
	ReservedTotal        int64        `json:"reserved_total"`
	SpentTotal           int64        `json:"spent_total"`
	Deactivated          bool         `json:"deactivated"`
	DeactivatedAt        timeJSON     `json:"deactivated_at"`
	DeactivatorAccountID string       `json:"deactivator_account_id"`
	DeactivateReason     string       `json:"deactivate_reason"`
}

type requestBackupV1 struct {
	PolicyID          string       `json:"policy_id"`
	RequestID         string       `json:"request_id"`
	AccountID         string       `json:"account_id"`
	PayerAccountID    string       `json:"payer_account_id"`
	SessionID         string       `json:"session_id"`
	Operation         string       `json:"operation"`
	Payee             string       `json:"payee"`
	EstimatedFee      int64        `json:"estimated_fee"`
	ActualFee         int64        `json:"actual_fee"`
	State             int          `json:"state"`
	CreatedAt         timeJSON     `json:"created_at"`
	SettledAt         timeJSON     `json:"settled_at"`
	WaitDeadline      timeJSON     `json:"wait_deadline"`
	ReservedAt        timeJSON     `json:"reserved_at"`
	ReserveDuration   durationJSON `json:"reserve_duration_nanos"`
	ReserveDeadline   timeJSON     `json:"reserve_deadline"`
	ReserveExpiredAt  timeJSON     `json:"reserve_expired_at"`
	DecidedAt         timeJSON     `json:"decided_at"`
	ApproverAccountID string       `json:"approver_account_id"`
	RejectReason      string       `json:"reject_reason"`
}

type ledgerEntryBackupV1 struct {
	Kind      int      `json:"kind"`
	AccountID string   `json:"account_id"`
	RequestID string   `json:"request_id"`
	PolicyID  string   `json:"policy_id"`
	Amount    int64    `json:"amount"`
	Reason    string   `json:"reason"`
	At        timeJSON `json:"at"`
}

type backupV1 struct {
	Version    int                   `json:"version"`
	ExportedAt timeJSON              `json:"exported_at"`
	Accounts   []accountBackupV1     `json:"accounts"`
	Sessions   []sessionBackupV1     `json:"sessions"`
	Policies   []policyBackupV1      `json:"policies"`
	Requests   []requestBackupV1     `json:"requests"`
	Ledger     []ledgerEntryBackupV1 `json:"ledger"`
}

// Export 将当前钱包导出为带版本号的 JSON 文本。
//
// 导出在钱包同一把全局锁内完成，对应某一时刻的完整状态：可与申请、
// 批准、拒绝、吊销、停用、结算或取消并发，不会出现只有余额变化却缺少
// 对应请求或账本的半截状态。导出前先按导出时的当前时间处理全部到期
// 请求——待审批到达等待截止时刻后进入过期终态并留痕，已预留到达预留
// 截止时刻后全额退回（释放时间仍为原截止时刻）并留下退款及超时记录；
// 未到期请求继续等待或预留。
//
// 备份保留账户余额、会话的设备绑定与吊销状态、策略条件及停用信息、
// 全部请求的申请内容、状态、审批决定与全部计时信息，以及按原顺序排列
// 的账本（包括未被受理申请的拒绝记录）。空钱包也能导出并恢复；导出
// 本身不新增任何账本记录（到期处理带来的记录除外）。
//
// 备份不能暂停期限，因此导出不改变任何请求的计时基准；导出后的钱包
// 与备份文本、以及从该备份恢复出的钱包之间互不影响。
func (w *Wallet) Export() ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.now()
	// 导出快照前结清全部到期项，保证备份对应一个完整状态：已预留超时的按
	// 原截止时刻全额退回并留两条记录（按截止时刻排序），待审批过期的留
	// 状态记录（按等待截止时刻排序）。
	w.expireReservationsLocked(now)
	w.expirePendingApprovalsLocked(now)

	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(now),
		Accounts:   make([]accountBackupV1, 0, len(w.accounts)),
		Sessions:   make([]sessionBackupV1, 0, len(w.sessions)),
		Policies:   make([]policyBackupV1, 0, len(w.policies)),
		Requests:   make([]requestBackupV1, 0, len(w.requests)),
		Ledger:     make([]ledgerEntryBackupV1, 0, len(w.ledger)),
	}

	// map 迭代顺序不确定；集合类数据按编号排序，使备份可逐字节复现。
	for _, a := range w.accounts {
		b.Accounts = append(b.Accounts, accountBackupV1{
			ID:        a.id,
			Available: a.available,
			Reserved:  a.reserved,
			CreatedAt: timeJSON(a.createdAt),
		})
	}
	sort.Slice(b.Accounts, func(i, j int) bool { return b.Accounts[i].ID < b.Accounts[j].ID })

	for _, s := range w.sessions {
		b.Sessions = append(b.Sessions, sessionBackupV1{
			ID:        s.id,
			AccountID: s.accountID,
			DeviceID:  s.deviceID,
			ExpiresAt: timeJSON(s.expiresAt),
			Revoked:   s.revoked,
			CreatedAt: timeJSON(s.createdAt),
		})
	}
	sort.Slice(b.Sessions, func(i, j int) bool { return b.Sessions[i].ID < b.Sessions[j].ID })

	for _, p := range w.policies {
		allowed := make([]string, 0, len(p.allowedAccounts))
		for id := range p.allowedAccounts {
			allowed = append(allowed, id)
		}
		sort.Strings(allowed)
		b.Policies = append(b.Policies, policyBackupV1{
			ID:                   p.id,
			PayerAccountID:       p.payerAccountID,
			AllowedAccountIDs:    allowed,
			Operation:            p.operation,
			Payee:                p.payee,
			StartsAt:             timeJSON(p.startsAt),
			EndsAt:               timeJSON(p.endsAt),
			MaxPerRequest:        p.maxPerRequest,
			MaxTotal:             p.maxTotal,
			ApprovalThreshold:    p.approvalThreshold,
			ApprovalWait:         durationJSON(p.approvalWait),
			MaxReserveDuration:   durationJSON(p.maxReserveDuration),
			ReservedTotal:        p.reservedTotal,
			SpentTotal:           p.spentTotal,
			Deactivated:          p.deactivated,
			DeactivatedAt:        timeJSON(p.deactivatedAt),
			DeactivatorAccountID: p.deactivatorAccountID,
			DeactivateReason:     p.deactivateReason,
		})
	}
	sort.Slice(b.Policies, func(i, j int) bool { return b.Policies[i].ID < b.Policies[j].ID })

	for _, r := range w.requests {
		b.Requests = append(b.Requests, requestBackupV1{
			PolicyID:          r.policyID,
			RequestID:         r.requestID,
			AccountID:         r.accountID,
			PayerAccountID:    r.payerAccountID,
			SessionID:         r.sessionID,
			Operation:         r.operation,
			Payee:             r.payee,
			EstimatedFee:      r.estimatedFee,
			ActualFee:         r.actualFee,
			State:             int(r.state),
			CreatedAt:         timeJSON(r.createdAt),
			SettledAt:         timeJSON(r.settledAt),
			WaitDeadline:      timeJSON(r.waitDeadline),
			ReservedAt:        timeJSON(r.reservedAt),
			ReserveDuration:   durationJSON(r.reserveDuration),
			ReserveDeadline:   timeJSON(r.reserveDeadline),
			ReserveExpiredAt:  timeJSON(r.reserveExpiredAt),
			DecidedAt:         timeJSON(r.decidedAt),
			ApproverAccountID: r.approverAccountID,
			RejectReason:      r.rejectReason,
		})
	}
	sort.Slice(b.Requests, func(i, j int) bool {
		if b.Requests[i].AccountID != b.Requests[j].AccountID {
			return b.Requests[i].AccountID < b.Requests[j].AccountID
		}
		return b.Requests[i].RequestID < b.Requests[j].RequestID
	})

	// 账本严格按原顺序导出，不重新排序。
	for _, e := range w.ledger {
		b.Ledger = append(b.Ledger, ledgerEntryBackupV1{
			Kind:      int(e.Kind),
			AccountID: e.AccountID,
			RequestID: e.RequestID,
			PolicyID:  e.PolicyID,
			Amount:    e.Amount,
			Reason:    e.Reason,
			At:        timeJSON(e.At),
		})
	}

	out, err := json.Marshal(&b)
	if err != nil {
		return nil, fmt.Errorf("%w: encode: %v", ErrBackupInvalid, err)
	}
	return out, nil
}

// addInt64 在求和时检测 int64 溢出；超出范围返回 false。
func addInt64(sum, x int64) (int64, bool) {
	if x > 0 && sum > math.MaxInt64-x {
		return 0, false
	}
	if x < 0 && sum < math.MinInt64-x {
		return 0, false
	}
	return sum + x, true
}

// Restore 从 Export 产生的 JSON 文本创建一个独立的钱包。
//
// 恢复不重新计时：所有截止时刻与会话到期时间沿用备份中的绝对时刻，
// 恢复前先按恢复时的当前时间处理全部到期请求（待审批过期、已预留超时
// 全额退回，释放时间仍为原截止时刻）。既有终态与历史记录不改写；过期或
// 吊销的会话、停用或结束的策略可以恢复，但不会重新获得授权。恢复后被
// 超时退回的余额可立即用于新申请，旧请求的迟到结算不能再扣走这笔钱。
//
// 空文本、非法 JSON、缺少必需数据、不支持的版本、编号重复、未知状态或
// 账本类型、非法金额或策略参数、悬空引用、申请会话不属于使用账户、
// 请求出资账户与策略不符，或余额/累计金额与请求求和不一致（含求和超出
// int64 范围），都返回包装了具体原因的 ErrBackupInvalid，绝不会返回
// 部分恢复的钱包。每笔已受理请求记载的提交时刻还必须落在关联策略的时间
// 窗内且不晚于申请会话到期：不早于策略开始时刻（恰在开始时刻提交可以
// 接受）、严格早于策略结束时刻、严格早于申请会话到期时刻（恰在策略结束
// 或会话到期时提交必须拒绝）；正常提交时这些时刻的申请本就会被拒绝，
// 不可能成为已受理请求。直接预留与经过审批的申请适用同一规则，后来已
// 结算、取消、拒绝或过期的请求也不豁免；该核对只针对备份记载的提交历史，
// 不以恢复时的当前时间替代，因此申请当时符合条件、之后会话到期或被吊销、
// 策略结束或被停用的请求仍按现有规则恢复。未被受理的申请留下的独立拒绝
// 账本记录不关联请求，不适用本规则，仍原样保留。曾经批准成功并实际预留费用的超门槛请求（恢复时仍为
// 已预留，或后来已结算、已取消、已预留超时）还必须通过审批时限核对：
// 保存的等待截止时刻必须与提交时刻加策略等待时长、策略结束时间、申请
// 会话到期时间三者的最早值一致（缺失、提前、推迟都拒绝），且批准决定
// 必须发生在提交时刻及之后、等待截止时刻之前（恰在提交时刻批准可以
// 接受，恰到截止时刻批准必须拒绝）；该核对只针对备份记载的批准历史，
// 不以恢复时的当前时间替代批准时间，因此期限内已批准的请求即使恢复时
// 等待期限、申请会话或策略时间窗均已结束也照常恢复，不重新计时。
// 已拒绝请求（出资账户主动拒绝、申请会话吊销或策略停用导致，原因与决定
// 账户的区别保留）同样必须通过拒绝时限核对：保存的等待截止时刻必须存在，
// 并与提交时刻加策略最长等待时长、策略结束时刻、申请会话到期时刻三者的
// 最早值一致（缺失、提前、推迟都拒绝），且拒绝决定必须发生在提交时刻及
// 之后、等待截止时刻之前（提交当时立即拒绝可以接受，截止当时才拒绝必须
// 拒绝）；该核对只看备份记载的申请与拒绝历史，与恢复时的当前时间无关，
// 因此期限内完成的合法拒绝即使恢复时会话已到期或被吊销、策略已结束或
// 停用，仍保持已拒绝原样恢复，不追加过期记录、不产生资金变动。余额与
// 策略累计金额核对一致也不能让时间矛盾的备份通过。
// 曾在待审批状态取消、从未预留费用的请求（已预留后再取消的沿用既有规则，
// 不因缺少待审批取消的决定时间被拒绝）同样必须通过取消时限核对：保存的
// 等待截止时刻必须存在，并与提交时刻加策略等待时长、策略结束时间、申请
// 会话到期时间三者的最早值一致（缺失、提前、推迟都拒绝，哪怕保存的取消
// 时间仍落在修改后的期限内），且取消决定必须发生在提交时刻及之后、等待
// 截止时刻之前（提交当时就取消可以接受，恰到截止时刻或更晚取消必须拒绝；
// 策略结束或会话到期先发生时按最早的那个时刻判断，不能只用最长等待时长）。
// 该核对只看备份记载的申请与取消历史，与恢复时的当前时间无关，因此期限内
// 完成的合法取消即使很久以后才恢复、申请会话已过期或吊销、策略已结束或
// 停用，仍保持原取消状态、决定时间与账本顺序，不追加过期或退款记录，不
// 冻结余额、不占用额度；矛盾的已取消记录不会被自动改成过期来接受备份。
// 此外，任一仍处于待审批状态的请求都不得关联已吊销的申请会话：主动吊销
// 申请会话时，吊销发生在等待截止时刻之前的待审批请求会立即进入拒绝终态，
// 恰到或超过截止时刻的进入过期终态，二者皆终态，不可能继续等待出资账户
// 批准。因此备份保存的“申请会话已吊销 + 请求仍待审批”自相矛盾，只要存在
// 一笔即整体拒绝恢复（错误指出使用账户、请求编号与申请会话编号，并说明
// 已吊销会话不能保留待审批请求），哪怕该请求的账户、会话归属、策略、费用、
// 等待期限符合其他全部校验、账户与策略金额核对一致，即使同一备份的其他
// 会话与请求均合法也不能放行。该判断只针对备份保存的会话吊销标记与请求
// 状态、不随恢复时刻改变：等待期限尚未到达、恰好到达或早已过去都得到相同
// 的备份无效结果；恢复时不会先把该请求按当前时间自动变成过期再接受备份，
// 也不替调用方补写吊销拒绝决定、猜测吊销时间、清除吊销标记或删除请求来
// 消除矛盾。判断按提交申请所用的会话进行：出资账户用于审批的会话有效也
// 不能使矛盾备份恢复成功。已吊销会话本身及其全部合法历史仍可恢复：吊销前
// 已预留的请求继续按原规则结算、取消或等待预留超时；吊销产生的拒绝（期限
// 内吊销）或过期（恰到或超过期限吊销）请求，以及已经结算、取消的历史，
// 保留原状态、金额、决定信息与既有账本；未吊销会话下的合法待审批请求仍可
// 恢复并继续审批，到期时按原规则过期。独立的申请拒绝账本记录不关联请求，
// 不受此规则影响。
// 此外，任一仍处于待审批状态的请求都不得关联已停用策略：正常停用时该策略
// 下未到等待期限的待审批请求会立即进入拒绝终态、已到（含恰到）或超过期限
// 的进入过期终态，二者皆终态，不可能继续等待出资账户批准。因此备份保存的
// “已停用策略 + 待审批请求”自相矛盾，只要存在一笔即整体拒绝恢复（错误指出
// 使用账户、请求编号与策略编号，并说明已停用策略不能保留待审批请求），哪怕
// 该请求的账户与会话引用、审批门槛、等待截止时刻、账户与策略金额都符合其他
// 全部校验，即使同一备份的其他策略与请求均合法也不能放行。该判断只针对备份
// 保存的状态、不随恢复时刻改变：等待期限尚未到达、恰好到达或早已过去都得到
// 相同的备份无效结果；恢复时不会先把该请求自动变成过期再接受备份，也不替
// 调用方补写停用拒绝记录、清除停用标志或改写请求来消除矛盾。已停用策略本身
// 及其全部合法历史仍可恢复：停用前已预留的请求继续按原规则结算、取消或等待
// 预留超时；已结算、已取消、已拒绝或已过期的请求保留原状态与决定信息；策略
// 的首次停用时间、执行账户、理由、余额与已有账本顺序原样保留；未停用策略下
// 的合法待审批请求仍可恢复并继续审批，到期时按原规则过期。
// 此外，每条标为已停用的策略都必须保存非空的执行停用账户，且与该策略的
// 出资账户完全一致：正常停用只允许策略的出资账户凭有效会话操作，停用成功后
// 查询保留的也是这个出资账户，因此恢复已停用策略时执行账户留空（字段缺失与
// 空字符串均按缺少执行账户处理），或换成钱包里另一个已存在账户——即使该账户
// 确实存在、在本策略允许使用的账户列表中，或是另一条策略的出资账户——都是正常
// 停用不可能产生的记录，只要存在一条即整体拒绝恢复（错误指出策略编号与具体
// 身份问题；账户不符时同时给出保存的执行账户与本应执行停用的出资账户；填入
// 不存在账户编号的拒绝沿用原有校验），哪怕停用时间、理由合法，余额与请求金额
// 核对完全一致，即使同一备份的其他账户、策略与请求均合法也不能略过出错策略、
// 补填出资账户或清除停用标记后继续恢复。该核对只针对备份保存的停用历史，不
// 要求出资账户在恢复时仍持有有效会话：这是在核对已保存的停用历史，原会话已经
// 到期或被吊销也不否定此前合法的停用；尚未开始或已经结束的策略原本都允许停用，
// 其合法停用记录同样接受。合法的已停用策略仍按原停用状态、首次停用时间、执行
// 账户、理由与已有账本原样恢复；未停用策略继续沿用现有的停用信息校验。
// 此外，关联已停用策略的每笔已受理请求，其保存的提交时刻不得严格晚于该
// 策略的首次停用时刻：停用后策略不再受理新申请，停用后才提交的申请只会
// 留下不关联请求的拒绝账本记录，不可能成为已受理请求；超过审批门槛、经
// 批准才预留费用的请求，其保存的批准时刻同样不得严格晚于首次停用时刻
// （直接受理的请求不携带审批信息，不适用批准时刻核对）。只要存在一笔
// 矛盾即整体拒绝恢复（错误指出使用账户、请求编号与策略编号，并说明是
// 停用后提交还是停用后批准），哪怕该请求的等待期限、余额与额度核对完全
// 一致。该核对对请求后来的状态一律适用：后来已结算、取消、拒绝、等待
// 审批过期或预留超时的历史也不能绕过——后续处理时刻不是新的提交或批准
// 时刻。提交或批准与首次停用时刻完全相同时可以接受：先完成申请或批准、
// 再停用，二者可能共享同一个时间戳，只有严格更晚才矛盾。时间按备份保存
// 的完整绝对时刻比较，纳秒精度保留，不同时区表示的同一时刻判定相同；
// 该核对只针对备份记载的历史时刻，不以恢复时的当前时间替代，也不能先把
// 相关预留按当前时间退回再接受本不合法的备份。停用前合法提交并完成预留
// 的请求继续沿用已有行为：停用后才结算或取消的历史正常恢复，保留原状态、
// 金额、审批信息、预留期限与已有账本，不因本核对补写停用、拒绝或退款
// 记录。未停用策略下的请求不受此限。
// 从未预留费用、因等待审批到期进入已过期终态的请求（已预留费用的预留超时
// 不适用）同样必须通过过期时限核对：保存的等待截止时刻必须存在，并与提交
// 时刻加策略最长等待时长、策略结束时间、申请会话到期时间三者的最早值一致
// （缺失、提前、推迟都拒绝，即使修改后的截止时刻仍早于记载的过期决定；
// 策略或会话更早到期时按最早的那个时刻判断，不能只按最长等待时长），且
// 过期决定时刻必须存在、不早于该截止时刻（恰到截止时刻发现过期可以接受，
// 早于它必须拒绝）。钱包可能在到期很久后才发现请求过期，合法决定时刻不必
// 等于截止时刻，恢复时不改写为截止时刻或当前时间。该核对只看备份记载的
// 申请与过期历史，与恢复时的当前时间无关，因此合法过期即使恢复时会话已
// 到期或被吊销、策略已结束或停用，仍保持原过期状态、全部时刻与账本顺序，
// 不追加过期记录，不产生预留、扣减或退款。
// 已拒绝（出资账户主动拒绝、申请会话吊销、策略停用三条路径一致）与等待
// 审批过期这两类未结算终态从未预留过费用，也没有执行结算，因此保存的实际
// 费用必须为零、结算时间必须为空：正数实际费用即使不超过预估费用也不能接受，
// 实际费用为零但结算时间非空同样无效，两项只要有一项矛盾即整体拒绝恢复
// （错误指出使用账户、请求编号，并说清是未结算终态携带了实际费用还是结算
// 时间），哪怕账户预留余额与策略已花费总额均为零、其余引用与审批时刻均合法，
// 即使同一备份还包含其他合法账户、策略与请求也不能只跳过该笔继续恢复，或把
// 费用清零、清空结算时间后接受。该核对只看备份保存的请求状态与结算信息，与
// 恢复时距离审批期限过去多久无关：恢复不会先按当前时间改变状态再接受备份。
// 合法的拒绝与过期历史保留原状态、预估费用、提交时间、等待截止时间与决定时间
// （决定时间不是结算时间，不因存在决定时间而被拒绝）及已有账本顺序；拒绝原因
// 与审批账户信息照常保留。实际完成结算且实际费用为零的请求仍是合法的已结算
// 历史（RequestSettled），保留结算时间与原有退款结果；已预留费用的预留超时
// （RequestReservationExpired）继续按现有规则处理，均不适用本规则。
// 曾经完成结算的请求（直接受理与批准后预留两条路径一致）还必须通过结算
// 时刻核对：启用预留超时时，结算时刻必须不早于实际预留时刻且严格早于预留
// 截止时刻（恰在预留完成时结算可以接受，恰到截止时刻及之后必须拒绝，实际
// 费用为零也不例外）；时间按备份保存的完整绝对时刻比较，纳秒与时区表示均
// 不改变结论，关闭预留超时的请求不设此项期限。该核对同样只看备份记载的
// 预留与结算时刻，与恢复时的当前时间无关：期限内完成的结算即使很久以后恢复
// 仍保持已结算，余额、策略已花费总额与既有账本原样保留，不追加退款或超时
// 记录。
// 未经审批直接受理的请求（策略关闭审批，或预估费用未严格超过审批门槛）
// 只要曾经预留过费用（恢复时仍为已预留，或后来已结算、已取消、已预留超时），
// 实际预留时刻还必须与提交时刻是同一瞬间：正常申请无需审批时，提交成功即
// 同时完成费用预留。预留起点提前或推迟均拒绝，即使只差一纳秒；两种带不同
// 时区的时间写法若表示同一绝对时刻则正常接受。关闭预留超时的策略同样适用，
// 不允许预留起点与提交时刻脱离。该核对只针对备份记载的提交与预留时刻，与
// 恢复时的当前时间无关：即使被篡改的预留截止时刻在恢复时已经过去，也必须
// 整体拒绝，不能先按超时退款、再把矛盾历史当成合法备份。经大额审批后才
// 预留的请求不受此限，其实际预留时刻为批准成功时刻、允许晚于提交时刻，
// 由上方的审批时限核对覆盖。
// 此外，备份保存的每笔已受理请求，其预估费用都不得严格超过关联策略保存的
// 完整累计上限：正常受理时（直接预留与进入待审批两条路径一致）都先做共享
// 累计额度判断，预估费用自身大于完整累计上限的申请在受理当时就被拒绝，只
// 留下不关联请求的拒绝账本记录，不可能成为请求，更不可能进入待审批。因此
// 累计上限 10、单次上限 100、审批门槛 10 时预估费用 30 的新申请会因超出
// 累计上限被拒绝；备份中若保存了这样一笔请求（哪怕仍为待审批，且引用、
// 时间与余额等信息符合其他全部规则）就是正常流程无法产生的记录，只要存在
// 一笔即整体拒绝恢复（错误指出使用账户、请求编号与策略编号，并说明保存的
// 预估费用与完整累计上限），且不返回钱包。该核对对请求后来的状态一律适用：
// 已经保存为请求的申请不因现在不占用额度而豁免，仍待审批、后来被拒绝、
// （待审批）取消或待审批过期、已预留后取消，以及曾经预留后按较低实际费用
// 结算的历史，都遵循同一规则；不能跳过该请求、降低预估费用或改大策略上限
// 后继续恢复，同一备份中的合法请求也不能被部分恢复。比较的是该策略保存的
// 完整累计上限，不是恢复时的剩余额度，也不累计其他请求的历史预估费用：
// 累计上限 50 的策略，一笔历史请求预估 40、最终扣减 10，另有一笔现存预留
// 40 使当前占用恰为 50 时仍可恢复，不能再把历史预估 40 加上而误判超限；
// 预估费用恰等于累计上限时，只要其余校验全部通过就接受；同一策略下已经
// 退回的历史预估费用合计超过上限也不仅因此拒绝。合法的待审批请求也可能因
// 其他请求后来占用了额度而暂时无法批准，这只影响批准、不构成备份无效。
// 该核对与恢复时的当前时间无关，位于恢复时的到期自动处理之前：即使超限
// 请求的等待或预留期限已过，也不能先自动处理到期再接受矛盾备份；现存预留
// 与已花费总额的核对继续独立保留，未被受理申请的独立拒绝账本记录不属于
// 保存的请求，仍按现有规则保留。
// 任一策略现存预留费用与已结算实际费用的合计严格超过其
// 共享累计上限时同样拒绝（同一策略的多个使用账户合并计算，不同策略分别
// 判断；待审批、已取消、被拒绝、已过期及预留超时的请求不占用额度），
// 该检查针对备份保存的资金占用状态、在恢复时的到期自动退回之前完成：
// 合计恰好等于上限合法，上限恰为 MaxInt64 时也不缩小可接受的金额范围。
// 任一账户的可用余额加上该账户在所有策略下仍处于已预留
// 状态的费用之和超出 int64 上限时同样拒绝（即使各请求、各策略金额分别
// 合法）：否则取消或预留超时的退回会使余额越界；校验在恢复时的到期
// 自动退回之前完成，预留尚未到期也当场拒绝。
// 每条账本记录保存的金额还必须与其记录类型相符（只排除负金额不够）：
// 预留与退款记录实际记载一笔资金变动，金额必须严格大于零，金额为零也必须
// 拒绝；扣减记录允许零金额，实际费用为零也是合法结算，零金额扣减记录必须
// 原样保留且请求仍为已结算，不能把它当成没有发生结算；预留、扣减、退款之外
// 的全部现有类型（待审批、批准、拒绝、待审批取消、待审批过期、策略停用、
// 预留超时状态）都是不发生资金变化的状态留痕，金额必须为零。预留超时本就
// 另有一条正数全额退款和一条零金额状态记录，退款金额不得再次写进状态记录；
// 待审批取消从未冻结费用，其取消留痕也不能携带资金金额。任何类型仍不接受
// 负金额。该核对逐条针对记录自身的类型与金额，不看其关联对象是否存在：只要
// 备份中有一条违反，整个恢复即失败并返回 ErrBackupInvalid，错误指出该记录
// 在原账本中的位置、记录类型、保存的金额及违反的金额要求，且不返回钱包；
// 不能跳过该记录、把金额改成零或更换类型后继续恢复，同一备份的账户余额、
// 策略累计金额与请求求和完全一致也不能掩盖它。未被受理申请的零金额拒绝
// 记录即使账户或请求编号缺失、关联对象不存在，仍按原规则恢复，本核对不因此
// 增加账户或请求必须存在的要求；正常备份的账本顺序、时间、原因与关联编号
// 原样保留，不增添记录或改动余额；空账本同样可以恢复。恢复出的钱包与原钱包、同一
// 备份恢复出的其他钱包互不影响。
func Restore(data []byte) (*Wallet, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%w: empty backup", ErrBackupInvalid)
	}

	var b backupV1
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("%w: malformed json: %v", ErrBackupInvalid, err)
	}
	// 不允许备份对象之后还有多余的 JSON 值；忽略空白。
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: unexpected trailing data after backup object", ErrBackupInvalid)
	}
	if b.Version != backupVersion {
		return nil, fmt.Errorf("%w: unsupported version %d (supported: %d)", ErrBackupInvalid, b.Version, backupVersion)
	}

	w := New()
	if err := w.restoreLocked(&b); err != nil {
		return nil, err
	}
	return w, nil
}

// restoreAt 仅供同包测试使用：与 Restore 相同，但显式指定恢复时刻，
// 便于确定性地验证恢复时的到期处理。
func restoreAt(data []byte, now time.Time) (*Wallet, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%w: empty backup", ErrBackupInvalid)
	}
	var b backupV1
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("%w: malformed json: %v", ErrBackupInvalid, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: unexpected trailing data after backup object", ErrBackupInvalid)
	}
	if b.Version != backupVersion {
		return nil, fmt.Errorf("%w: unsupported version %d (supported: %d)", ErrBackupInvalid, b.Version, backupVersion)
	}
	w := New()
	w.now = func() time.Time { return now }
	if err := w.restoreLocked(&b); err != nil {
		return nil, err
	}
	return w, nil
}

// restoreLocked 逐段构建并校验新钱包；任何失败都返回错误，调用方因此
// 绝不会拿到部分恢复的钱包。
func (w *Wallet) restoreLocked(b *backupV1) error {
	// ---- 账户 ----
	if b.Accounts == nil {
		return fmt.Errorf("%w: missing accounts", ErrBackupInvalid)
	}
	accountIDs := make(map[string]struct{}, len(b.Accounts))
	for i, a := range b.Accounts {
		if a.ID == "" {
			return fmt.Errorf("%w: account[%d] has empty id", ErrBackupInvalid, i)
		}
		if _, dup := accountIDs[a.ID]; dup {
			return fmt.Errorf("%w: duplicate account id %q", ErrBackupInvalid, a.ID)
		}
		if a.Available < 0 {
			return fmt.Errorf("%w: account %q available balance is negative: %d", ErrBackupInvalid, a.ID, a.Available)
		}
		if a.Reserved < 0 {
			return fmt.Errorf("%w: account %q reserved balance is negative: %d", ErrBackupInvalid, a.ID, a.Reserved)
		}
		accountIDs[a.ID] = struct{}{}
		w.accounts[a.ID] = &account{
			id:        a.ID,
			available: a.Available,
			reserved:  a.Reserved,
			createdAt: a.CreatedAt.std(),
		}
	}

	// ---- 会话 ----
	if b.Sessions == nil {
		return fmt.Errorf("%w: missing sessions", ErrBackupInvalid)
	}
	sessionByID := make(map[string]*session, len(b.Sessions))
	for i, s := range b.Sessions {
		if s.ID == "" {
			return fmt.Errorf("%w: session[%d] has empty id", ErrBackupInvalid, i)
		}
		if _, dup := sessionByID[s.ID]; dup {
			return fmt.Errorf("%w: duplicate session id %q", ErrBackupInvalid, s.ID)
		}
		if s.AccountID == "" || s.DeviceID == "" {
			return fmt.Errorf("%w: session %q missing account or device id", ErrBackupInvalid, s.ID)
		}
		if _, ok := accountIDs[s.AccountID]; !ok {
			return fmt.Errorf("%w: session %q references unknown account %q", ErrBackupInvalid, s.ID, s.AccountID)
		}
		sess := &session{
			id:        s.ID,
			accountID: s.AccountID,
			deviceID:  s.DeviceID,
			expiresAt: s.ExpiresAt.std(),
			revoked:   s.Revoked,
			createdAt: s.CreatedAt.std(),
		}
		sessionByID[s.ID] = sess
		w.sessions[s.ID] = sess
	}

	// ---- 策略 ----
	if b.Policies == nil {
		return fmt.Errorf("%w: missing policies", ErrBackupInvalid)
	}
	policyByID := make(map[string]*policy, len(b.Policies))
	for i, p := range b.Policies {
		if p.ID == "" {
			return fmt.Errorf("%w: policy[%d] has empty id", ErrBackupInvalid, i)
		}
		if _, dup := policyByID[p.ID]; dup {
			return fmt.Errorf("%w: duplicate policy id %q", ErrBackupInvalid, p.ID)
		}
		if err := validatePolicyParams(p); err != nil {
			return fmt.Errorf("%w: policy %q: %v", ErrBackupInvalid, p.ID, err)
		}
		if _, ok := accountIDs[p.PayerAccountID]; !ok {
			return fmt.Errorf("%w: policy %q references unknown payer account %q", ErrBackupInvalid, p.ID, p.PayerAccountID)
		}
		allowed := make(map[string]struct{}, len(p.AllowedAccountIDs))
		for j, id := range p.AllowedAccountIDs {
			if id == "" {
				return fmt.Errorf("%w: policy %q allowed account[%d] is empty", ErrBackupInvalid, p.ID, j)
			}
			if _, ok := accountIDs[id]; !ok {
				return fmt.Errorf("%w: policy %q references unknown allowed account %q", ErrBackupInvalid, p.ID, id)
			}
			if _, dup := allowed[id]; dup {
				return fmt.Errorf("%w: policy %q lists allowed account %q more than once", ErrBackupInvalid, p.ID, id)
			}
			allowed[id] = struct{}{}
		}
		if p.ReservedTotal < 0 || p.SpentTotal < 0 {
			return fmt.Errorf("%w: policy %q totals must not be negative (reserved %d spent %d)", ErrBackupInvalid, p.ID, p.ReservedTotal, p.SpentTotal)
		}
		po := &policy{
			id:                   p.ID,
			payerAccountID:       p.PayerAccountID,
			allowedAccounts:      allowed,
			operation:            p.Operation,
			payee:                p.Payee,
			startsAt:             p.StartsAt.std(),
			endsAt:               p.EndsAt.std(),
			maxPerRequest:        p.MaxPerRequest,
			maxTotal:             p.MaxTotal,
			approvalThreshold:    p.ApprovalThreshold,
			approvalWait:         p.ApprovalWait.std(),
			maxReserveDuration:   p.MaxReserveDuration.std(),
			reservedTotal:        p.ReservedTotal,
			spentTotal:           p.SpentTotal,
			deactivated:          p.Deactivated,
			deactivatedAt:        p.DeactivatedAt.std(),
			deactivatorAccountID: p.DeactivatorAccountID,
			deactivateReason:     p.DeactivateReason,
		}
		// 停用信息自洽：停用标志必须与停用时间/理由同时出现；执行停用的账户
		// 必须非空、存在且与本策略的出资账户完全一致；未停用不得携带任何停用
		// 记录。
		if po.deactivated {
			if po.deactivatedAt.IsZero() || po.deactivateReason == "" {
				return fmt.Errorf("%w: policy %q marked deactivated but missing deactivation time or reason", ErrBackupInvalid, p.ID)
			}
			// 正常停用只允许策略的出资账户凭有效会话操作（DeactivatePolicy 直接
			// 把执行账户记为出资账户），因此已停用策略保存的执行账户必须与出资
			// 账户完全一致：字段缺失或空字符串按缺少执行账户处理；填入其他账户
			// 编号——即使该账户确实存在、在本策略允许使用的账户列表中，或是另一
			// 条策略的出资账户——都按执行账户与出资账户不符处理。对不存在账户的
			// 拒绝仍由 accountIDs 查找先行保留。合法的停用时间、理由、余额与请求
			// 金额核对一致都不能使这条矛盾记录通过恢复。
			if po.deactivatorAccountID == "" {
				return fmt.Errorf("%w: policy %q marked deactivated but missing deactivator account: only its payer account %q can deactivate a policy", ErrBackupInvalid, p.ID, po.payerAccountID)
			}
			if _, ok := accountIDs[po.deactivatorAccountID]; !ok {
				return fmt.Errorf("%w: policy %q deactivator references unknown account %q", ErrBackupInvalid, p.ID, po.deactivatorAccountID)
			}
			if po.deactivatorAccountID != po.payerAccountID {
				return fmt.Errorf("%w: policy %q saved deactivator account %q does not match its payer account %q: only the payer account that owns the policy can deactivate it", ErrBackupInvalid, p.ID, po.deactivatorAccountID, po.payerAccountID)
			}
		} else if !po.deactivatedAt.IsZero() || po.deactivatorAccountID != "" || po.deactivateReason != "" {
			return fmt.Errorf("%w: policy %q not deactivated but carries deactivation info", ErrBackupInvalid, p.ID)
		}
		policyByID[p.ID] = po
		w.policies[p.ID] = po
	}

	// ---- 请求 ----
	if b.Requests == nil {
		return fmt.Errorf("%w: missing requests", ErrBackupInvalid)
	}
	reqKeys := make(map[requestKey]struct{}, len(b.Requests))
	// 金额归属表：每笔请求按备份保存的状态在这里唯一地归入账户预留、
	// 策略预留或策略已花费，随后的金额核对统一读这张表，不再与钱包重建
	// 及到期处理交织。
	amounts := newRestoreAmounts()
	for i, r := range b.Requests {
		if err := validateRequestRef(r, i, accountIDs, sessionByID, policyByID); err != nil {
			return err
		}
		key := requestKey{accountID: r.AccountID, requestID: r.RequestID}
		if _, dup := reqKeys[key]; dup {
			return fmt.Errorf("%w: duplicate request id %q under account %q", ErrBackupInvalid, r.RequestID, r.AccountID)
		}
		reqKeys[key] = struct{}{}

		state := RequestState(r.State)
		if state < RequestReserved || state > RequestReservationExpired {
			return fmt.Errorf("%w: request %q/%q has unknown state %d", ErrBackupInvalid, r.AccountID, r.RequestID, r.State)
		}
		if r.EstimatedFee <= 0 {
			return fmt.Errorf("%w: request %q/%q estimated fee must be positive: %d", ErrBackupInvalid, r.AccountID, r.RequestID, r.EstimatedFee)
		}
		if r.ActualFee < 0 {
			return fmt.Errorf("%w: request %q/%q actual fee is negative: %d", ErrBackupInvalid, r.AccountID, r.RequestID, r.ActualFee)
		}
		if r.ReserveDuration.std() < 0 {
			return fmt.Errorf("%w: request %q/%q reserve duration is negative", ErrBackupInvalid, r.AccountID, r.RequestID)
		}

		req := &request{
			policyID:          r.PolicyID,
			requestID:         r.RequestID,
			accountID:         r.AccountID,
			payerAccountID:    r.PayerAccountID,
			sessionID:         r.SessionID,
			operation:         r.Operation,
			payee:             r.Payee,
			estimatedFee:      r.EstimatedFee,
			actualFee:         r.ActualFee,
			state:             state,
			createdAt:         r.CreatedAt.std(),
			settledAt:         r.SettledAt.std(),
			waitDeadline:      r.WaitDeadline.std(),
			reservedAt:        r.ReservedAt.std(),
			reserveDuration:   r.ReserveDuration.std(),
			reserveDeadline:   r.ReserveDeadline.std(),
			reserveExpiredAt:  r.ReserveExpiredAt.std(),
			decidedAt:         r.DecidedAt.std(),
			approverAccountID: r.ApproverAccountID,
			rejectReason:      r.RejectReason,
		}
		if err := validateRequestTimingAndState(req, policyByID[r.PolicyID], sessionByID[r.SessionID]); err != nil {
			return fmt.Errorf("%w: request %q/%q: %v", ErrBackupInvalid, r.AccountID, r.RequestID, err)
		}
		w.requests[key] = req

		// 按备份保存的请求状态把这笔费用归入唯一对应的总额；归属规则
		// （含求和溢出判断）集中在 restoreAmounts，账户与策略不再分别
		// 累加同一笔已预留请求。
		if err := amounts.addRequest(req); err != nil {
			return err
		}
	}

	// ---- 金额核对：账户预留余额、账户退款上限与策略共享累计额度 ----
	// 全部判断只针对备份保存的状态，必须在下方恢复时的到期自动退回之前
	// 完成；规则与错误口径见 restoreAmounts.reconcile。
	if err := amounts.reconcile(w.accounts, policyByID); err != nil {
		return err
	}

	// ---- 账本 ----
	if b.Ledger == nil {
		return fmt.Errorf("%w: missing ledger", ErrBackupInvalid)
	}
	w.ledger = make([]LedgerEntry, 0, len(b.Ledger))
	for i, e := range b.Ledger {
		kind := LedgerKind(e.Kind)
		if kind < LedgerReserve || kind > LedgerReservationExpiration {
			return fmt.Errorf("%w: ledger[%d] has unknown kind %d", ErrBackupInvalid, i, e.Kind)
		}
		// 每条账本记录自身的金额必须与其记录类型相符，不能只排除负金额：
		// 预留与退款只在实际发生资金变动时记账，金额必须严格为正；扣减允许
		// 为零，实际费用为零也是一次合法结算；其余类型全部是不发生资金变化
		// 的状态留痕（待审批、批准、拒绝、待审批取消、待审批过期、策略停用、
		// 预留超时状态），金额必须为零——预留超时的全额退款另由同笔的
		// LedgerRefund 记载，待审批取消从未冻结费用，二者都不能再带一笔资金。
		if err := validateLedgerAmount(kind, e.Amount); err != nil {
			return fmt.Errorf("%w: ledger[%d] (kind %s/%d): %v", ErrBackupInvalid, i, ledgerKindName(kind), e.Kind, err)
		}
		// 账本是历史留痕：其账户/请求编号允许为空或指向不存在的实体。
		// 未被受理申请（必填字段缺失、使用账户或会话不存在等）同样会留下
		// 拒绝记录，这些记录必须原样保留，不能因为找不到对应账户或请求而
		// 丢弃或拒绝恢复。资金一致性由账户余额与现存请求的求和核对保证，
		// 不依赖账本文本。PolicyID 若给出则必须存在（仅策略停用记录使用）。
		if e.PolicyID != "" {
			if _, ok := policyByID[e.PolicyID]; !ok {
				return fmt.Errorf("%w: ledger[%d] references unknown policy %q", ErrBackupInvalid, i, e.PolicyID)
			}
		}
		w.ledger = append(w.ledger, LedgerEntry{
			Kind:      kind,
			AccountID: e.AccountID,
			RequestID: e.RequestID,
			PolicyID:  e.PolicyID,
			Amount:    e.Amount,
			Reason:    e.Reason,
			At:        e.At.std(),
		})
	}

	// ---- 恢复时按当前时间结清到期请求（不重新计时，只推进惰性状态） ----
	// 处理顺序与导出保持一致：先释放已到期预留，再把到期的待审批转为过期，
	// 两类记录均按各自截止时刻排序后追加到账本末尾。
	now := w.now()
	w.expireReservationsLocked(now)
	w.expirePendingApprovalsLocked(now)
	return nil
}

// ledgerKindName 返回账本记录类型的稳定名称，供错误信息定位问题记录；
// 未知类型（正常流程中已被恢复校验先行拦截）返回数字编号。
func ledgerKindName(kind LedgerKind) string {
	switch kind {
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
		return "pending-approval-expiration"
	case LedgerPolicyDeactivation:
		return "policy-deactivation"
	case LedgerReservationExpiration:
		return "reservation-expiration"
	default:
		return fmt.Sprintf("unknown(%d)", int(kind))
	}
}

// validateLedgerAmount 校验单条账本记录保存的金额与其记录类型相符。账本只
// 按原样留痕，恢复时不依据账本重算余额，因此一条类型与金额不符的记录不会被
// 余额/累计金额核对发现——它会出现在恢复后的账本中，账户余额却没有对应变化，
// 调用方无法据此核对费用。各类规则：
//   - 预留（LedgerReserve）：只在实际冻结费用时记账，金额必须严格大于零；
//   - 退款（LedgerRefund）：结算差额、已预留取消或预留超时的全额退回都实际
//     增加可用余额，金额必须严格大于零；零费用结算的全额退回是 settle 差额
//     等于预留全额的正数退款，不是零金额退款；
//   - 扣减（LedgerSettle）：金额必须非负，允许为零——实际费用为零也是一次
//     合法结算，零金额扣减记录必须保留，请求仍为已结算，不能当成未结算；
//   - 其余类型（拒绝、待审批、批准、待审批取消、待审批过期、策略停用、预留
//     超时状态）均为不发生资金变化的状态留痕，金额必须为零。预留超时已另有
//     一条正数全额退款，超时状态记录不能再写进退款金额；待审批取消从未冻结
//     费用，其取消留痕同样不能携带资金金额。
//
// 任何类型都不接受负金额。该核对只看每条记录自身保存的类型与金额，不依赖其
// 账户、请求或策略是否存在（未被受理申请的零金额拒绝记录允许悬空引用），也
// 不能被账户余额、策略累计金额与请求求和一致所掩盖：违反即由调用方整体拒绝
// 恢复，不跳过、不改写金额或类型后继续。
func validateLedgerAmount(kind LedgerKind, amount int64) error {
	switch kind {
	case LedgerReserve, LedgerRefund:
		if amount < 0 {
			return fmt.Errorf("%s entry carries a negative amount %d, but its amount must be positive", ledgerKindName(kind), amount)
		}
		if amount == 0 {
			return fmt.Errorf("%s entry carries amount 0, but its amount must be positive: it records an actual fund movement", ledgerKindName(kind))
		}
		return nil
	case LedgerSettle:
		if amount < 0 {
			return fmt.Errorf("settle entry carries a negative amount %d, but its amount must be zero or positive", amount)
		}
		return nil
	default:
		if amount != 0 {
			return fmt.Errorf("%s status entry carries amount %d, but it records no fund movement and its amount must be zero", ledgerKindName(kind), amount)
		}
		return nil
	}
}

// validatePolicyParams 校验备份中策略的条件与限额参数，与 SavePolicy 共用
// validatePolicySpecParams 的同一套判断，恢复时不再单独维护一份规则。
// 备份特有的编号、编号重复、账户存在性与授权账户重复列出检查仍由
// restoreLocked 按恢复口径另行处理（备份中同一策略重复列出授权账户必须
// 拒绝，不能像保存入口那样按同一账户处理）。
//
// 关闭审批（门槛为零）时等待时长不被使用，备份保存的原值（正、零、负
// 均可）必须原样接受，不能因为未使用的等待设置拒绝整个钱包；最长预留
// 时长是独立设置，即使关闭审批也不得为负——这些关系由共用的参数判断
// 保证，与保存入口一致。
func validatePolicyParams(p policyBackupV1) error {
	return validatePolicySpecParams(PolicySpec{
		PayerAccountID:     p.PayerAccountID,
		AllowedAccountIDs:  p.AllowedAccountIDs,
		Operation:          p.Operation,
		Payee:              p.Payee,
		StartsAt:           p.StartsAt.std(),
		EndsAt:             p.EndsAt.std(),
		MaxPerRequest:      p.MaxPerRequest,
		MaxTotal:           p.MaxTotal,
		ApprovalThreshold:  p.ApprovalThreshold,
		ApprovalWait:       p.ApprovalWait.std(),
		MaxReserveDuration: p.MaxReserveDuration.std(),
	})
}

// validateRequestRef 校验请求的外键引用：账户、会话、策略均须存在；申请
// 会话必须属于使用账户；出资账户必须与策略一致；申请账户必须被策略允许。
func validateRequestRef(r requestBackupV1, i int, accounts map[string]struct{}, sessions map[string]*session, policies map[string]*policy) error {
	where := fmt.Sprintf("request[%d] %q/%q", i, r.AccountID, r.RequestID)
	if r.RequestID == "" || r.AccountID == "" || r.PolicyID == "" || r.SessionID == "" {
		return fmt.Errorf("%w: %s missing required id (account/request/policy/session)", ErrBackupInvalid, where)
	}
	if _, ok := accounts[r.AccountID]; !ok {
		return fmt.Errorf("%w: %s references unknown usage account %q", ErrBackupInvalid, where, r.AccountID)
	}
	if _, ok := accounts[r.PayerAccountID]; !ok {
		return fmt.Errorf("%w: %s references unknown payer account %q", ErrBackupInvalid, where, r.PayerAccountID)
	}
	sess, ok := sessions[r.SessionID]
	if !ok {
		return fmt.Errorf("%w: %s references unknown session %q", ErrBackupInvalid, where, r.SessionID)
	}
	// 申请会话必须属于使用账户。
	if sess.accountID != r.AccountID {
		return fmt.Errorf("%w: %s session %q belongs to account %q, not usage account %q", ErrBackupInvalid, where, r.SessionID, sess.accountID, r.AccountID)
	}
	p, ok := policies[r.PolicyID]
	if !ok {
		return fmt.Errorf("%w: %s references unknown policy %q", ErrBackupInvalid, where, r.PolicyID)
	}
	// 请求出资账户必须与策略出资账户一致。
	if r.PayerAccountID != p.payerAccountID {
		return fmt.Errorf("%w: %s payer account %q does not match policy %q payer %q", ErrBackupInvalid, where, r.PayerAccountID, r.PolicyID, p.payerAccountID)
	}
	if _, ok := p.allowedAccounts[r.AccountID]; !ok {
		return fmt.Errorf("%w: %s usage account %q is not allowed by policy %q", ErrBackupInvalid, where, r.AccountID, r.PolicyID)
	}
	return nil
}

// validateReserveTiming 校验预留计时三元组自洽：duration 必须非负；
// duration>0 时 reservedAt 必填且 reserveDeadline 必须等于
// reservedAt+duration；duration==0 时 reserveDeadline 必须为零。
// reservedAt 是否必填由各状态自行决定（待审批取消的请求两者都没有）。
func validateReserveTiming(r *request) error {
	if r.reserveDuration < 0 {
		return errors.New("reserve duration must not be negative")
	}
	if r.reserveDuration > 0 {
		if r.reservedAt.IsZero() {
			return errors.New("reserve duration set but reserved_at is missing")
		}
		want := r.reservedAt.Add(r.reserveDuration)
		if !r.reserveDeadline.Equal(want) {
			return fmt.Errorf("reserve deadline %v does not match reserved_at+duration %v", r.reserveDeadline, want)
		}
	} else if !r.reserveDeadline.IsZero() {
		return errors.New("reserve deadline set while reserve duration is zero")
	}
	return nil
}

// validateSettlementTiming 校验已结算请求的结算时刻必须是正常结算能够产生的
// 历史：不早于实际预留时刻（恰在预留完成时结算可以接受）；当请求启用了预留
// 超时（reserveDuration>0，此时 reserveDeadline 已由 validateReserveTiming
// 确认为 reservedAt+duration）时，还必须严格早于预留截止时刻——恰到截止时刻
// 及之后预留已按超时规则全额退回，任何结算（含实际费用为零）都不可能成功。
//
// 时间一律按保存的完整时刻以 time.Time 的绝对瞬间比较（Equal/Before）：
// 纳秒精度保留，截止前不足一秒的合法结算不会被当成超时；不同时区表示的同一
// 时刻判定相同。关闭预留超时（reserveDuration==0）时没有截止时刻，沿用原有
// 结算规则、不人为增加期限。该校验只针对备份记载的预留与结算时刻，与恢复时
// 的当前时间无关：期限内完成的结算在很久以后恢复仍是已结算。
func validateSettlementTiming(r *request) error {
	if r.settledAt.Before(r.reservedAt) {
		return fmt.Errorf("settled_at %v is before reserved_at %v", r.settledAt, r.reservedAt)
	}
	if r.reserveDuration > 0 && !r.settledAt.Before(r.reserveDeadline) {
		return fmt.Errorf("settled_at %v is not within the valid reserve window ending strictly before reserve deadline %v", r.settledAt, r.reserveDeadline)
	}
	return nil
}

// validateUnsettledTerminalSettlement 校验“未结算终态”（已拒绝
// RequestRejected、等待审批过期 RequestExpired）不得携带任何结算信息：这两类
// 请求自始至终处于待审批、从未预留过费用，被拒绝或到期时既没有扣减也没有结算，
// 因此正常流程保存的实际费用必为零、结算时间必为空。备份若带有正数实际费用
// （即使不超过预估费用）或非空结算时间，就是正常流程不可能产生的矛盾状态——
// 实际费用会在请求查询中显示为一笔没有对应扣减、也不占用任何余额的费用。
//
// 两项只要有一项矛盾即拒绝：实际费用为零但结算时间非空同样无效。拒绝或过期的
// 决定时刻保存在独立的 decided_at 字段、与 settled_at 无关，本核对不限制决定
// 时刻的存在。已完成结算（含实际费用为零的结算）属于 RequestSettled，不适用
// 本规则；已预留费用的预留超时（RequestReservationExpired）由各自的既有规则
// 覆盖。时间与金额均只看备份保存的内容，与恢复时的当前时间无关：恢复时距离
// 审批期限过去多久都不改变结论，也不替调用方把费用清零或清空结算时间后接受。
func validateUnsettledTerminalSettlement(r *request, stateNoun string) error {
	if r.actualFee != 0 {
		return fmt.Errorf("usage account %q request %q is in the %s terminal state but carries an actual fee %d: a %s request never reserves or settles funds, so its actual fee must be zero",
			r.accountID, r.requestID, stateNoun, r.actualFee, stateNoun)
	}
	if !r.settledAt.IsZero() {
		return fmt.Errorf("usage account %q request %q is in the %s terminal state but carries a settled-at time %v: a %s request never settles, so its settled_at must be empty",
			r.accountID, r.requestID, stateNoun, r.settledAt, stateNoun)
	}
	return nil
}

// validateSubmissionTiming 校验请求记载的提交时刻必须是正常申请能够产生的
// 历史：正常提交时，策略尚未开始、已经结束或申请会话已经到期的申请都会被
// 拒绝（只留下不关联请求的拒绝账本记录），不可能进入已受理请求序列。因此
// 备份中每笔已受理请求的提交时刻必须：
//   - 不早于关联策略的开始时刻（恰在策略开始时提交可以接受）；
//   - 严格早于策略结束时刻（恰在策略结束时提交必须拒绝）；
//   - 严格早于申请会话的到期时刻（恰在会话到期时提交必须拒绝）。
//
// 直接预留与经过审批的申请适用同一规则；后来已经结算、取消、拒绝或过期
// （含预留超时）的请求也不豁免。时间一律按保存的完整时刻以 time.Time 的
// 绝对瞬间比较（Before）：纳秒精度保留，不同时区表示的同一时刻判定相同。
// 该校验只针对备份记载的提交历史，与恢复时的当前时间无关：申请当时符合
// 条件、之后会话到期或被吊销、策略结束或被停用的请求仍按现有规则恢复。
func validateSubmissionTiming(r *request, p *policy, sess *session) error {
	if r.createdAt.Before(p.startsAt) {
		return fmt.Errorf("created_at %v is before policy %q window starts_at %v", r.createdAt, p.id, p.startsAt)
	}
	if !r.createdAt.Before(p.endsAt) {
		return fmt.Errorf("created_at %v is not strictly before policy %q window ends_at %v", r.createdAt, p.id, p.endsAt)
	}
	if !r.createdAt.Before(sess.expiresAt) {
		return fmt.Errorf("created_at %v is not strictly before session %q expiry %v", r.createdAt, sess.id, sess.expiresAt)
	}
	return nil
}

// validateDeactivationTiming 校验关联已停用策略的请求，其时间线必须是正常
// 流程能够产生的历史：策略首次停用后不再受理新申请、也不再批准待审批请求
// （停用瞬间未到等待期限的待审批请求立即被拒绝，已到期限的进入过期终态），
// 因此备份中关联已停用策略的每笔已受理请求：
//   - 提交时刻不得严格晚于策略的首次停用时刻（停用后才提交的申请只会留下
//     不关联请求的拒绝账本记录，不可能成为已受理请求）；
//   - 超过审批门槛、经批准才预留费用的请求，其批准时刻同样不得严格晚于
//     首次停用时刻（停用后出资账户已无法再批准）。直接受理的请求不携带
//     审批信息，不适用批准时刻核对。
//
// 两项核对对请求后来的状态一律适用：已结算、已取消、被拒绝、待审批过期或
// 预留超时的历史也不豁免——后续处理时刻不是新的提交或批准时刻。提交或批准
// 与首次停用时刻完全相同时可以接受：先完成申请或批准、再停用，二者可能共享
// 同一个时间戳，只有严格更晚才矛盾。时间一律按保存的完整时刻以 time.Time
// 的绝对瞬间比较（After）：纳秒精度保留，不同时区表示的同一时刻判定相同。
// 仍处于待审批状态的请求不适用提交时刻核对：它们已被“已停用策略不得保留
// 待审批请求”的专项核对覆盖，由该核对给出对应的矛盾说明。
// 该校验只针对备份记载的历史时刻，与恢复时的当前时间无关：不能先把相关预留
// 按当前时间退回，再接受本不合法的备份。未停用策略下的请求不受此限。
func validateDeactivationTiming(r *request, p *policy, approvalRequired bool) error {
	if !p.deactivated {
		return nil
	}
	if r.state != RequestPendingApproval && r.createdAt.After(p.deactivatedAt) {
		return fmt.Errorf("usage account %q request %q was submitted at %v, after policy %q was first deactivated at %v: a deactivated policy cannot accept new submissions",
			r.accountID, r.requestID, r.createdAt, p.id, p.deactivatedAt)
	}
	if approvalRequired && !r.reservedAt.IsZero() && r.decidedAt.After(p.deactivatedAt) {
		return fmt.Errorf("usage account %q request %q was approved at %v, after policy %q was first deactivated at %v: a deactivated policy cannot approve requests",
			r.accountID, r.requestID, r.decidedAt, p.id, p.deactivatedAt)
	}
	return nil
}

// validateEstimatedFeeWithinTotal 校验单笔已保存请求的预估费用不得严格超过
// 其关联策略保存的完整累计上限（MaxTotal）。正常流程中，无论申请是直接
// 预留还是进入待审批，受理前都必须先通过共享累计额度判断：计入本笔预估
// 费用后“现存预留 + 已结算实际费用”严格超过完整累计上限的申请直接被
// 拒绝，只留下不关联请求的拒绝账本记录，不可能成为请求。因此一笔预估
// 费用自身就大于完整累计上限的请求（例如累计上限 10、单次上限 100、审批
// 门槛 10 时预估费用 30 的申请会因超出累计上限被拒，不可能进入待审批）是
// 正常流程不可能产生的记录，备份中只要存在一笔即整体无效。
//
// 该规则对请求保存时的全部状态一律适用，不因其现在不占用额度而豁免：仍在
// 待审批、后来被拒绝、待审批取消、待审批过期或已预留后取消，以及曾经预留
// 后按较低实际费用结算的历史，预估费用都不得严格超过累计上限——它们保存
// 的申请内容，在受理当时本就不可能通过额度校验。预估费用恰等于累计上限
// 时可以接受（其余校验全部通过为前提）：合计恰好等于上限本就合法。
//
// 这里比较的是策略保存的完整累计上限，不是恢复时的剩余额度，也不把其他
// 请求的历史预估费用累加进来：例如累计上限 50 的策略，一笔历史请求预估
// 40、最终只扣减 10，另有一笔现存预留 40 使当前占用恰为 50，两笔各自的
// 预估费用都不超过完整上限，备份仍合法，不能把历史的预估 40 再加到当前
// 占用上误判超限；同一策略下已经退回的历史预估费用合计超过上限，同样不
// 仅因此拒绝。现存预留与已花费总额的核对（restoreLocked 中的
// reservedTotal/spentTotal 求和）继续独立保留。合法的待审批请求也可能因
// 其他请求后来占用了额度而在恢复时暂时无法批准，那只影响批准、不构成备份
// 无效。该校验只看备份保存的请求预估费用与策略上限，与恢复时的当前时间
// 无关：即使该请求的等待或预留期限已过，也必须先拒绝整个备份，不能先在
// 恢复时自动处理到期再接受矛盾数据。
func validateEstimatedFeeWithinTotal(r *request, p *policy) error {
	if r.estimatedFee > p.maxTotal {
		return fmt.Errorf("usage account %q request %q policy %q saved estimated fee %d exceeds the policy's full cumulative total limit %d: an application that large is rejected against the cumulative limit before acceptance and can never become a request",
			r.accountID, r.requestID, p.id, r.estimatedFee, p.maxTotal)
	}
	return nil
}

// validateRequestTimingAndState 校验请求状态与其计时/金额字段自洽，并与
// 不可变的策略条件、申请会话保持一致，防止任意状态搭配任意时间戳的损坏
// 备份。sess 为该请求的申请会话。
func validateRequestTimingAndState(r *request, p *policy, sess *session) error {
	if r.createdAt.IsZero() {
		return errors.New("request missing created_at")
	}
	// 提交时刻必须是正常申请能够产生的历史：落在策略时间窗内且申请会话
	// 尚未到期。对直接预留与经过审批的申请、以及所有后续状态统一适用。
	if err := validateSubmissionTiming(r, p, sess); err != nil {
		return err
	}
	if r.operation == "" || r.payee == "" {
		return errors.New("operation and payee are required")
	}
	if r.operation != p.operation || r.payee != p.payee {
		return fmt.Errorf("operation/payee do not match policy %q", p.id)
	}
	if r.estimatedFee > p.maxPerRequest {
		return fmt.Errorf("estimated fee %d exceeds policy per-request limit %d", r.estimatedFee, p.maxPerRequest)
	}
	// 预估费用还不得严格超过策略保存的完整累计上限：正常受理时（无论直接
	// 预留还是进入待审批）都必须先通过共享累计额度校验，预估费用本身大于
	// 完整上限的申请只会留下不关联请求的拒绝账本记录，不可能成为请求——
	// 更不可能进入待审批。该核对逐笔针对保存的预估费用本身，与请求后来的
	// 状态无关，也不看恢复时的剩余额度，故在 validateRequestTimingAndState
	// 的统一入口处拦截，具体适用范围见 validateEstimatedFeeWithinTotal。
	if err := validateEstimatedFeeWithinTotal(r, p); err != nil {
		return err
	}
	if r.approverAccountID != "" && r.approverAccountID != r.payerAccountID {
		return fmt.Errorf("approver account %q is not the payer %q", r.approverAccountID, r.payerAccountID)
	}
	if r.actualFee > r.estimatedFee {
		return fmt.Errorf("actual fee %d exceeds estimated fee %d", r.actualFee, r.estimatedFee)
	}

	// 申请是否必须走过审批由不可变策略门槛决定：费用严格超过正门槛的请求
	// 只能来自待审批流程；其余请求只能直接预留。
	approvalRequired := p.approvalThreshold > 0 && r.estimatedFee > p.approvalThreshold
	reservedOrigin := !r.reservedAt.IsZero()

	// 已停用策略下的请求，其提交（及经审批请求的批准）不得严格晚于策略的
	// 首次停用时刻；对全部后续状态统一适用。
	if err := validateDeactivationTiming(r, p, approvalRequired); err != nil {
		return err
	}

	switch r.state {
	case RequestReserved, RequestSettled, RequestReservationExpired:
		// 这三类一定发生过预留。
		if err := validateReservedOrigin(r, p, sess, approvalRequired, reservedOrigin); err != nil {
			return err
		}
	case RequestPendingApproval, RequestRejected, RequestExpired:
		// 这三类一定从未预留，且必须来自开启审批的策略。
		if err := validatePendingOrigin(r, p, approvalRequired, reservedOrigin); err != nil {
			return err
		}
	case RequestCancelled:
		// 取消可能来自待审批（从未预留）或已预留。
		if reservedOrigin {
			if err := validateReservedOrigin(r, p, sess, approvalRequired, true); err != nil {
				return err
			}
		} else {
			if err := validatePendingOrigin(r, p, approvalRequired, false); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown state %d", r.state)
	}

	switch r.state {
	case RequestReserved:
		// 启用超时时 deadline 必须等于 reservedAt + 时长，未启用时为零，
		// 且不得已记录超时。
		if err := validateReserveTiming(r); err != nil {
			return err
		}
		if !r.reserveExpiredAt.IsZero() {
			return errors.New("reserved request must not carry reserve-expired time")
		}
		if r.actualFee != 0 || !r.settledAt.IsZero() {
			return errors.New("reserved request must not be settled")
		}
		if r.rejectReason != "" {
			return errors.New("reserved request must not carry a reject reason")
		}
	case RequestSettled:
		if r.settledAt.IsZero() {
			return errors.New("settled request missing settled_at")
		}
		if !r.reserveExpiredAt.IsZero() {
			return errors.New("settled request must not carry reserve-expired time")
		}
		if err := validateReserveTiming(r); err != nil {
			return err
		}
		// 结算时刻必须落在有效预留期内：启用预留超时时严格早于截止时刻。
		if err := validateSettlementTiming(r); err != nil {
			return err
		}
		if r.rejectReason != "" {
			return errors.New("settled request must not carry a reject reason")
		}
	case RequestCancelled:
		if r.actualFee != 0 || !r.settledAt.IsZero() {
			return errors.New("cancelled request must not be settled")
		}
		if !r.reserveExpiredAt.IsZero() {
			return errors.New("cancelled request must not carry reserve-expired time")
		}
		if r.rejectReason != "" {
			return errors.New("cancelled request must not carry a reject reason")
		}
		if reservedOrigin {
			if err := validateReserveTiming(r); err != nil {
				return err
			}
		} else {
			// 待审批取消：决定时间必填，且等待期限与取消决定时刻必须是
			// 正常取消能够产生的历史。
			if r.decidedAt.IsZero() {
				return errors.New("cancelled-from-pending request missing decided_at")
			}
			if err := validateWaitDeadline(r, p, sess, "cancelled-from-pending request"); err != nil {
				return err
			}
			if err := validateWaitDecisionTiming(r, "cancellation"); err != nil {
				return err
			}
		}
	case RequestPendingApproval:
		// 待审批不冻结余额：不得有实际费用，且等待期限必须等于
		// min(提交+等待时长, 策略结束, 申请会话到期)。
		if r.actualFee != 0 || !r.settledAt.IsZero() {
			return errors.New("pending request must not be settled")
		}
		if r.waitDeadline.IsZero() {
			return errors.New("pending request missing wait deadline")
		}
		if r.approverAccountID != "" {
			return errors.New("pending request must not carry an approver")
		}
		if r.rejectReason != "" {
			return errors.New("pending request must not carry a reject reason")
		}
		// 已停用策略下不得残留待审批请求：正常停用时，未到等待期限的待审批
		// 请求会立即随停用进入拒绝终态，已到（含恰到）或超过期限的进入过期
		// 终态，二者皆终态、不再等待批准。备份保存的“已停用策略 + 待审批
		// 请求”是正常流程不可能产生的矛盾状态，必须整体拒绝。该校验只看备份
		// 保存的状态，与恢复时的当前时间无关：等待期限尚未到达、恰好到达或
		// 早已过去结果相同；不能先在恢复时把该请求自动过期、再把矛盾备份当作
		// 合法数据，也不替调用方补写停用拒绝记录、清除停用标志或改写请求。
		// 停用前已进入其他状态（已预留、已结算、已取消、被拒绝、已过期、预留
		// 超时）的历史请求不受此限，已停用策略本身及其全部合法历史仍可恢复。
		if p.deactivated {
			return fmt.Errorf("usage account %q request %q is still pending approval under deactivated policy %q: a deactivated policy cannot retain pending-approval requests", r.accountID, r.requestID, p.id)
		}
		// 已吊销申请会话下不得残留待审批请求：主动吊销会话时，未到等待期限的
		// 待审批请求会立即随吊销进入拒绝终态，已到（含恰到）或超过期限的进入
		// 过期终态，二者皆终态、不再等待批准。备份保存的“申请会话已吊销 +
		// 待审批请求”是正常流程不可能产生的矛盾状态，必须整体拒绝。该校验只看
		// 备份保存的吊销标记与请求状态，与恢复时的当前时间无关：等待期限尚未
		// 到达、恰好到达或早已过去结果相同；不能先在恢复时把该请求按当前时间
		// 自动过期、再把矛盾备份当作合法数据，也不替调用方补写吊销拒绝决定、
		// 猜测吊销时间、清除吊销标记或删除请求来消除矛盾。这里按提交申请所用的
		// 会话判断：即使出资账户用于审批的会话仍然有效，也不能让矛盾备份恢复
		// 成功。吊销前已进入其他状态（已预留、已结算、已取消、被拒绝、已过期、
		// 预留超时）的历史请求不受此限，已吊销会话本身及其全部合法历史仍可恢复。
		if sess.revoked {
			return fmt.Errorf("usage account %q request %q is still pending approval under revoked application session %q: a revoked session cannot retain pending-approval requests", r.accountID, r.requestID, sess.id)
		}
		// 待审批没有决定时刻，只需核对保存的等待截止时刻。
		if err := validateWaitDeadline(r, p, sess, "pending request"); err != nil {
			return err
		}
	case RequestRejected:
		if r.decidedAt.IsZero() || r.rejectReason == "" {
			return errors.New("rejected request missing decided_at or reason")
		}
		// 已拒绝终态从未预留或结算费用：实际费用必须为零、结算时间必须为空，
		// 正数实际费用（即使不超过预估费用）或非空结算时间都使备份自相矛盾。
		// 拒绝决定时刻保存在 decided_at，与结算时间无关，不受此限。
		if err := validateUnsettledTerminalSettlement(r, "rejected"); err != nil {
			return err
		}
		// 等待截止时刻与拒绝决定时刻必须是正常审批流程能够产生的历史：
		// 出资账户主动拒绝、申请会话吊销、策略停用三条路径统一适用。
		if err := validateWaitDeadline(r, p, sess, "rejected request"); err != nil {
			return err
		}
		if err := validateWaitDecisionTiming(r, "rejection"); err != nil {
			return err
		}
	case RequestExpired:
		if r.decidedAt.IsZero() {
			return errors.New("expired request missing decided_at")
		}
		if r.rejectReason != "" {
			return errors.New("expired request must not carry a reject reason")
		}
		// 等待审批过期终态从未预留或结算费用：实际费用必须为零、结算时间必须
		// 为空；与已预留费用的预留超时（RequestReservationExpired）不同。
		if err := validateUnsettledTerminalSettlement(r, "pending-approval-expired"); err != nil {
			return err
		}
		// 等待截止时刻与过期决定时刻必须是正常到期处理能够产生的历史：
		// 截止时刻必须等于提交时确定的期限，决定不得早于该截止时刻。
		if err := validateExpirationTiming(r, p, sess); err != nil {
			return err
		}
	case RequestReservationExpired:
		if r.reserveDuration <= 0 {
			return errors.New("reservation-expired request must have a positive reserve duration")
		}
		if err := validateReserveTiming(r); err != nil {
			return err
		}
		if !r.reserveExpiredAt.Equal(r.reserveDeadline) {
			return errors.New("reserve-expired-at must equal reserve deadline")
		}
		if r.actualFee != 0 || !r.settledAt.IsZero() {
			return errors.New("reservation-expired request must not be settled")
		}
		if r.rejectReason != "" {
			return errors.New("reservation-expired request must not carry a reject reason")
		}
	}
	return nil
}

// expectedWaitDeadline 计算审批路径请求的等待截止时刻：提交时刻 + 策略
// 等待时长、策略结束时间、申请会话到期时间三者中的最早值。所有走过审批
// 流程的请求（待审批、批准、拒绝、待审批取消、待审批过期）保存的等待
// 截止时刻都必须与该值一致。
func expectedWaitDeadline(r *request, p *policy, sess *session) time.Time {
	want := r.createdAt.Add(p.approvalWait)
	if p.endsAt.Before(want) {
		want = p.endsAt
	}
	if sess.expiresAt.Before(want) {
		want = sess.expiresAt
	}
	return want
}

// validateWaitDeadline 核对审批路径请求保存的等待截止时刻自洽：不得缺失，
// 且必须等于提交时刻+策略等待时长、策略结束时间、申请会话到期时间三者的
// 最早值；提前或推迟都拒绝，即使决定时刻仍落在修改后的期限内。
//
// whatNoun 为请求所处的审批环节名称（如 "approved request"、"rejected
// request"、"cancelled-from-pending request"、"expired request"），用于保留
// 各环节原有的错误措辞。待审批、批准、拒绝、待审批取消与待审批过期共用这
// 同一条业务规则，不再分别维护。
//
// 时间一律按保存的完整时刻以 time.Time 的绝对瞬间比较（Equal）：纳秒精度
// 保留，不同时区表示的同一时刻判定相同。该校验只针对备份记载的历史时刻，
// 与恢复时的当前时间无关。
func validateWaitDeadline(r *request, p *policy, sess *session, whatNoun string) error {
	if r.waitDeadline.IsZero() {
		return fmt.Errorf("%s missing wait deadline", whatNoun)
	}
	if want := expectedWaitDeadline(r, p, sess); !r.waitDeadline.Equal(want) {
		return fmt.Errorf("wait deadline %v does not match min(created+wait, policy end, session expiry) %v", r.waitDeadline, want)
	}
	return nil
}

// validateWaitDecisionTiming 核对“期限内决定”（批准、拒绝、待审批取消）
// 共用的决定时刻规则：决定必须不早于提交时刻（恰在提交时刻决定可以接受），
// 且严格早于等待截止时刻——决定早于提交、恰到截止时刻或晚于截止时刻都不
// 可能在正常审批流程中产生（截止时刻及之后请求只能进入待审批过期终态）。
//
// actionNoun 为操作名称（"approval"、"rejection"、"cancellation"），用于保留
// 各操作原有的错误措辞。调用方须先通过 validateWaitDeadline 确认截止时刻
// 存在且正确，本函数不再重复该核对。时间一律按保存的完整时刻以 time.Time
// 的绝对瞬间比较（Before）：纳秒精度保留，不同时区表示的同一时刻判定相同。
// 该校验只针对备份记载的提交与决定历史，与恢复时的当前时间无关：期限内的
// 合法决定即使很久以后恢复、申请会话已到期或吊销、策略已结束或停用，仍保持
// 原状态、金额、决定信息与账本顺序，不追加退款或过期记录。
func validateWaitDecisionTiming(r *request, actionNoun string) error {
	if r.decidedAt.Before(r.createdAt) {
		return fmt.Errorf("%s decided_at %v is before created_at %v", actionNoun, r.decidedAt, r.createdAt)
	}
	if !r.decidedAt.Before(r.waitDeadline) {
		return fmt.Errorf("%s decided_at %v is not strictly before wait deadline %v", actionNoun, r.decidedAt, r.waitDeadline)
	}
	return nil
}

// validateExpirationTiming 校验“待审批过期”（从未预留费用、因等待审批到期
// 进入已过期终态）的请求保存的等待期限与过期决定时刻自洽，防止备份中出现
// 正常流程不可能产生的过期历史：
//   - 等待截止时刻核对与批准、拒绝、待审批取消共用同一规则（见
//     validateWaitDeadline）：缺失、提前、推迟都拒绝，即使修改后的截止时刻
//     仍早于记载的过期决定；策略结束或会话到期更早时按最早的那个时刻判断，
//     不能只按最长等待时长；
//   - 过期决定时刻必须存在，且不早于等待截止时刻：恰到截止时刻发现过期可以
//     接受，早于它必须拒绝。钱包可能在到期很久之后才（因查询、新申请或导出）
//     发现请求过期，因此合法决定时刻可以远晚于截止时刻，恢复时不得把它改写
//     为截止时刻或当前时间。
//
// 时间一律按保存的完整时刻以 time.Time 的绝对瞬间比较（Equal/Before）：
// 纳秒精度保留，不同时区表示的同一时刻判定相同。该校验只针对备份记载的
// 申请与过期历史，与恢复时的当前时间无关：合法过期即使恢复时申请会话已
// 到期或被吊销、策略已结束或停用，仍保持原过期状态、提交时刻、等待截止
// 与决定时刻及既有账本顺序，不追加过期记录，不产生预留、扣减或退款。
// 已预留费用的预留超时（RequestReservationExpired）不适用本规则。
func validateExpirationTiming(r *request, p *policy, sess *session) error {
	if err := validateWaitDeadline(r, p, sess, "expired request"); err != nil {
		return err
	}
	if r.decidedAt.Before(r.waitDeadline) {
		return fmt.Errorf("expiration decided_at %v is before wait deadline %v", r.decidedAt, r.waitDeadline)
	}
	return nil
}

// validateReservedOrigin 校验“发生过预留”的请求：预留计时字段必须齐全、
// 预留时长快照必须与策略一致，且审批路径（批准人、决定时间、等待期限）与
// 费用/门槛相匹配。对所有发生过预留的状态（已预留、已结算、预留超时、
// 已预留后取消）统一适用：超门槛的必须经出资账户在等待期限内于预留时刻
// 批准；未超门槛（或策略关闭审批）的直接受理请求不得携带批准人或决定时间，
// 且实际预留时刻必须与提交时刻为同一瞬间——正常申请无需审批时，提交成功
// 即同时完成费用预留，预留起点不可能早于或晚于提交时刻。
func validateReservedOrigin(r *request, p *policy, sess *session, approvalRequired, reservedOrigin bool) error {
	if !reservedOrigin {
		return errors.New("request is in a reserved-origin state but missing reserved_at")
	}
	if r.reserveDuration != p.maxReserveDuration {
		return fmt.Errorf("reserve duration %v does not match policy max reserve duration %v", r.reserveDuration, p.maxReserveDuration)
	}
	if approvalRequired {
		if r.approverAccountID != r.payerAccountID {
			return errors.New("above-threshold request was reserved without payer approval")
		}
		if !r.decidedAt.Equal(r.reservedAt) {
			return errors.New("approved request decided_at must equal reserved_at")
		}
		// 等待截止时刻与批准时刻必须是正常审批流程能够产生的历史：即使该
		// 预留后来已经结算、取消或超时退回，也不放宽这一要求。
		if err := validateWaitDeadline(r, p, sess, "approved request"); err != nil {
			return err
		}
		if err := validateWaitDecisionTiming(r, "approval"); err != nil {
			return err
		}
	} else {
		if r.approverAccountID != "" {
			return errors.New("below-threshold request must not carry an approver")
		}
		if !r.decidedAt.IsZero() {
			return errors.New("directly reserved request must not carry decided_at")
		}
		// 直接受理的请求在提交成功的同一时刻完成费用预留：实际预留时刻必须
		// 与提交时刻是同一瞬间。预留起点提前或推迟均拒绝，即使只差一纳秒——
		// 否则被篡改的预留起点会带着与之“自洽”的预留截止一起，把本应从受理
		// 时刻起算的预留推迟释放。时间按保存的完整绝对时刻比较（Equal）：
		// 不同时区表示的同一时刻判定相同。关闭预留超时（时长为零）的请求同样
		// 适用。该核对只针对备份记载的提交与预留时刻，与恢复时的当前时间无关：
		// 即使被篡改的预留截止在恢复时已经过去，也必须拒绝，不能先按超时退款
		// 再接受矛盾历史。
		if !r.reservedAt.Equal(r.createdAt) {
			return fmt.Errorf("directly reserved request reserved_at %v does not match created_at %v", r.reservedAt, r.createdAt)
		}
	}
	return nil
}

// validatePendingOrigin 校验“从未预留”的待审批系请求：不得携带任何预留
// 计时字段，且策略必须开启审批、费用必须严格超过门槛。
func validatePendingOrigin(r *request, p *policy, approvalRequired, reservedOrigin bool) error {
	if reservedOrigin {
		return errors.New("request is in a pending-origin state but carries reserved_at")
	}
	if !approvalRequired {
		return errors.New("request is in an approval state although policy approval is not enabled for its fee")
	}
	if !r.reserveDeadline.IsZero() || !r.reserveExpiredAt.IsZero() || r.reserveDuration != 0 {
		return errors.New("pending-origin request must not carry reserve timing")
	}
	return nil
}
