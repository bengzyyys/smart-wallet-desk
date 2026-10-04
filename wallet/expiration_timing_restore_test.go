package wallet

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildExpiredTimingBackup 构造一笔“超门槛待审批、因等待审批到期进入过期终态、
// 从未预留费用”的请求备份，所有计时字段默认合法：提交于 t0、等待截止
// t0+1h（策略窗口至 t0+24h、会话至 t0+24h，均不抢先）、于 t0+2h 才查询并
// 记录过期决定。mutate 在余额核对前调整请求/策略/会话字段，以便构造各类
// 不合法历史。
func buildExpiredTimingBackup(t *testing.T, t0 time.Time, mutate func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)) []byte {
	t.Helper()
	const fee int64 = 20
	r := requestBackupV1{
		PolicyID: "p", RequestID: "big", AccountID: "u1", PayerAccountID: "payer",
		SessionID: "s1", Operation: "charge", Payee: "shop",
		EstimatedFee: fee,
		State:        int(RequestExpired),
		CreatedAt:    timeJSON(t0),
		WaitDeadline: timeJSON(t0.Add(time.Hour)),
		// 钱包在到期很久以后才发现过期：合法决定时刻晚于截止时刻。
		DecidedAt: timeJSON(t0.Add(2 * time.Hour)),
	}
	pol := policyBackupV1{
		ID: "p", PayerAccountID: "payer", AllowedAccountIDs: []string{"u1"},
		Operation: "charge", Payee: "shop",
		StartsAt:           timeJSON(t0.Add(-time.Hour)),
		EndsAt:             timeJSON(t0.Add(24 * time.Hour)),
		MaxPerRequest:      100,
		MaxTotal:           1000,
		ApprovalThreshold:  10,
		ApprovalWait:       durationJSON(time.Hour),
		MaxReserveDuration: durationJSON(2 * time.Hour),
	}
	sess := sessionBackupV1{
		ID: "s1", AccountID: "u1", DeviceID: "d1",
		ExpiresAt: timeJSON(t0.Add(24 * time.Hour)), CreatedAt: timeJSON(t0.Add(-2 * time.Hour)),
	}
	if mutate != nil {
		mutate(&r, &pol, &sess)
	}
	b := backupV1{
		Version:    backupVersion,
		ExportedAt: timeJSON(t0.Add(-time.Minute)),
		Accounts: []accountBackupV1{
			{ID: "payer", Available: 100, Reserved: 0, CreatedAt: timeJSON(t0.Add(-2 * time.Hour))},
			{ID: "u1", Available: 0, Reserved: 0, CreatedAt: timeJSON(t0.Add(-2 * time.Hour))},
		},
		Sessions: []sessionBackupV1{sess},
		Policies: []policyBackupV1{pol},
		Requests: []requestBackupV1{r},
		// 过期终态本就带一条零金额过期留痕；恢复合法终态时不得追加或改写。
		Ledger: []ledgerEntryBackupV1{{
			Kind: int(LedgerExpiration), AccountID: "u1", RequestID: "big",
			Reason: "approval period expired", At: r.DecidedAt,
		}},
	}
	data, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// assertExpiredTimingRejected 恢复必须失败、包装 ErrBackupInvalid，并在错误
// 信息中点名使用账户与请求编号、说明时间矛盾；不得返回部分恢复的钱包。
func assertExpiredTimingRejected(t *testing.T, data []byte, at time.Time) string {
	t.Helper()
	w2, err := restoreAt(data, at)
	if err == nil {
		if w2 != nil {
			t.Fatalf("invalid expiration history restored a wallet: %+v", w2)
		}
		t.Fatal("invalid expiration history restored without error")
	}
	if !errors.Is(err, ErrBackupInvalid) {
		t.Fatalf("err = %v, want ErrBackupInvalid", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, `"u1"`) || !strings.Contains(msg, `"big"`) {
		t.Fatalf("error %q must name usage account and request id", msg)
	}
	return msg
}

// TestRestoreExpiredRequestTimingAccepted 验证合法的待审批过期历史照常恢复：
// 决定恰在截止时刻或晚于截止时刻很久都合法，且决定时刻不被改写成截止时刻或
// 恢复时刻；恢复时会话已到期或被吊销、策略已结束或停用都不影响接受；原提交
// 时间、等待截止、过期决定与既有账本顺序原样保留，不追加过期记录、不产生
// 预留、扣减或退款。
func TestRestoreExpiredRequestTimingAccepted(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("decision long after deadline restores without rewrite", func(t *testing.T) {
		// 截止 t0+1h，t0+2h 才查询记录过期；恢复时已过去两天。
		data := buildExpiredTimingBackup(t, t0, nil)
		w2, err := restoreAt(data, t0.Add(48*time.Hour))
		if err != nil {
			t.Fatalf("legal expired history must restore: %v", err)
		}
		r, err := w2.Request("u1", "big")
		if err != nil {
			t.Fatal(err)
		}
		if r.State != RequestExpired {
			t.Fatalf("state = %v, want expired", r.State)
		}
		if !r.CreatedAt.Equal(t0) || !r.WaitDeadline.Equal(t0.Add(time.Hour)) {
			t.Fatalf("submission/deadline rewritten: created %v deadline %v", r.CreatedAt, r.WaitDeadline)
		}
		// 决定时刻必须原样保留，不得被改成截止时刻或恢复时刻。
		if !r.DecidedAt.Equal(t0.Add(2 * time.Hour)) {
			t.Fatalf("decided_at = %v, want recorded %v", r.DecidedAt, t0.Add(2*time.Hour))
		}
		if r.RejectReason != "" || r.ApproverAccountID != "" {
			t.Fatalf("expired request carries rejection info: reason %q approver %q", r.RejectReason, r.ApproverAccountID)
		}
		if !r.ReservedAt.IsZero() {
			t.Fatalf("never-reserved request carries reserved_at %v", r.ReservedAt)
		}
		// 既有账本保留且不追加；终态不产生任何资金变动。
		entries := w2.Ledger()
		if len(entries) != 1 || entries[0].Kind != LedgerExpiration || entries[0].RequestID != "big" {
			t.Fatalf("ledger = %+v, want the single original expiration entry", entries)
		}
		if !entries[0].At.Equal(t0.Add(2 * time.Hour)) {
			t.Fatalf("expiration ledger time = %v, want recorded decision time", entries[0].At)
		}
		if bal, _ := w2.Balance("payer"); bal != (Balances{Available: 100, Reserved: 0}) {
			t.Fatalf("payer balance = %+v, want untouched", bal)
		}
	})

	t.Run("decision exactly at deadline restores", func(t *testing.T) {
		data := buildExpiredTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.DecidedAt = timeJSON(t0.Add(time.Hour))
		})
		w2, err := restoreAt(data, t0.Add(time.Minute))
		if err != nil {
			t.Fatalf("decision exactly at deadline must be legal: %v", err)
		}
		r, _ := w2.Request("u1", "big")
		if !r.DecidedAt.Equal(t0.Add(time.Hour)) {
			t.Fatalf("decided_at = %v, want deadline instant", r.DecidedAt)
		}
	})

	t.Run("deadline bounded by session expiry with late discovery", func(t *testing.T) {
		// 任务给定场景：最长等待允许到 12:10，但申请会话 12:05 到期；正确截止
		// 为 12:05，12:08 才查询并记录过期决定的历史可以恢复。
		data := buildExpiredTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, s *sessionBackupV1) {
			s.ExpiresAt = timeJSON(t0.Add(5 * time.Minute))
			r.WaitDeadline = timeJSON(t0.Add(5 * time.Minute))
			r.DecidedAt = timeJSON(t0.Add(8 * time.Minute))
		})
		if _, err := restoreAt(data, t0.Add(time.Hour)); err != nil {
			t.Fatalf("session-expiry bounded deadline with late decision: %v", err)
		}
	})

	t.Run("deadline bounded by policy end", func(t *testing.T) {
		data := buildExpiredTimingBackup(t, t0, func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
			p.EndsAt = timeJSON(t0.Add(30 * time.Minute))
			r.WaitDeadline = timeJSON(t0.Add(30 * time.Minute))
			r.DecidedAt = timeJSON(t0.Add(31 * time.Minute))
		})
		if _, err := restoreAt(data, t0.Add(time.Hour)); err != nil {
			t.Fatalf("policy-end bounded deadline: %v", err)
		}
	})

	t.Run("revoked session and ended or deactivated policy do not block restore", func(t *testing.T) {
		// 会话已到期且被吊销、策略已结束且被停用：合法过期历史只看备份记载的
		// 申请与过期时间，仍原样恢复。三者最早值为会话到期 t0+5m。
		data := buildExpiredTimingBackup(t, t0, func(r *requestBackupV1, p *policyBackupV1, s *sessionBackupV1) {
			s.ExpiresAt = timeJSON(t0.Add(5 * time.Minute))
			s.Revoked = true
			p.EndsAt = timeJSON(t0.Add(30 * time.Minute))
			p.Deactivated = true
			p.DeactivatedAt = timeJSON(t0.Add(45 * time.Minute))
			p.DeactivatorAccountID = "payer"
			p.DeactivateReason = "stop it"
			r.WaitDeadline = timeJSON(t0.Add(5 * time.Minute))
			r.DecidedAt = timeJSON(t0.Add(8 * time.Minute))
		})
		if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
			t.Fatalf("expired history must restore despite revoked session and ended/deactivated policy: %v", err)
		}
	})

	t.Run("nanosecond precision and timezone representations are honored", func(t *testing.T) {
		// 截止比整点晚 1 纳秒：策略等待时长也带这 1 纳秒，丢精度的实现会误判。
		data := buildExpiredTimingBackup(t, t0, func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
			p.ApprovalWait = durationJSON(time.Hour + time.Nanosecond)
			deadline := t0.Add(time.Hour + time.Nanosecond)
			r.WaitDeadline = timeJSON(deadline)
			r.DecidedAt = timeJSON(deadline)
		})
		if _, err := restoreAt(data, t0.Add(2*time.Hour)); err != nil {
			t.Fatalf("nanosecond deadline must be preserved and matched: %v", err)
		}
		// 同一时刻用 +02:00 时区写法：结论必须相同。
		data = buildExpiredTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
			r.WaitDeadline = timeJSON(t0.Add(time.Hour).In(time.FixedZone("UTC+2", 2*3600)))
			r.DecidedAt = timeJSON(t0.Add(2 * time.Hour).In(time.FixedZone("UTC+2", 2*3600)))
		})
		if _, err := restoreAt(data, t0.Add(48*time.Hour)); err != nil {
			t.Fatalf("same instant in another zone must restore: %v", err)
		}
	})

	t.Run("legal terminal expiry coexists with pending expiring at restore", func(t *testing.T) {
		// 既有过期终态原样恢复；同一备份中的现存待审批请求在恢复时到期的惰性
		// 处理行为继续保留，只追加该待审批请求自己的一条过期记录。
		data := buildExpiredTimingBackup(t, t0, nil)
		var b backupV1
		if err := json.Unmarshal(data, &b); err != nil {
			t.Fatal(err)
		}
		b.Requests = append(b.Requests, requestBackupV1{
			PolicyID: "p", RequestID: "still-waiting", AccountID: "u1", PayerAccountID: "payer",
			SessionID: "s1", Operation: "charge", Payee: "shop",
			EstimatedFee: 20, State: int(RequestPendingApproval),
			CreatedAt:    timeJSON(t0.Add(90 * time.Minute)),
			WaitDeadline: timeJSON(t0.Add(150 * time.Minute)),
		})
		fixed, err := json.Marshal(&b)
		if err != nil {
			t.Fatal(err)
		}
		// 恢复时刻 t0+4h：still-waiting 截止 t0+150m 已过，按恢复逻辑过期。
		w2, err := restoreAt(fixed, t0.Add(4*time.Hour))
		if err != nil {
			t.Fatalf("mixed backup: %v", err)
		}
		old, _ := w2.Request("u1", "big")
		if old.State != RequestExpired || !old.DecidedAt.Equal(t0.Add(2*time.Hour)) {
			t.Fatalf("terminal expiry rewritten: %+v", old)
		}
		pending, _ := w2.Request("u1", "still-waiting")
		if pending.State != RequestExpired {
			t.Fatalf("due pending request state = %v, want expired at restore", pending.State)
		}
		var kinds []LedgerKind
		for _, e := range w2.Ledger() {
			kinds = append(kinds, e.Kind)
		}
		if len(kinds) != 2 || kinds[0] != LedgerExpiration || kinds[1] != LedgerExpiration {
			t.Fatalf("ledger kinds = %v, want original expiration then lazy one", kinds)
		}
	})
}

// TestRestoreRejectsExpiredTiming 验证已过期请求：等待截止缺失、被改长/改短
// （即使修改后仍早于记载的过期决定）、未随更早的策略结束或会话到期更新，或
// 过期决定缺失、早于截止时刻，都必须让整个备份被拒绝；余额与累计金额自洽
// 也不能放行，且结论与恢复时刻远近无关。
func TestRestoreRejectsExpiredTiming(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		mutate func(*requestBackupV1, *policyBackupV1, *sessionBackupV1)
		reason string
	}{
		{
			name: "missing wait deadline",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.WaitDeadline = timeJSON(time.Time{})
			},
			reason: "missing wait deadline",
		},
		{
			name: "lengthened wait deadline",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				// 正确截止 t0+1h；改到 t0+2h 后，记载的决定 t0+2h 恰好落在伪
				// 截止上，旧的“决定不早于截止”校验会放行，现在必须拒绝。
				r.WaitDeadline = timeJSON(t0.Add(2 * time.Hour))
			},
			reason: "does not match min",
		},
		{
			name: "shortened deadline still earlier than recorded decision",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				// 正确截止 t0+1h，决定 t0+2h；把截止改成 t0+15m 后决定仍晚于
				// 它，旧校验会放行——现在必须拒绝。
				r.WaitDeadline = timeJSON(t0.Add(15 * time.Minute))
			},
			reason: "does not match min",
		},
		{
			name: "deadline moved to 12:06 while session expires 12:05",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, s *sessionBackupV1) {
				// 任务给定反例：最长等待到 12:10、会话 12:05 到期，正确截止
				// 12:05；改成 12:06 后决定 12:08 仍晚于它，也必须失败。
				s.ExpiresAt = timeJSON(t0.Add(5 * time.Minute))
				r.WaitDeadline = timeJSON(t0.Add(6 * time.Minute))
				r.DecidedAt = timeJSON(t0.Add(8 * time.Minute))
			},
			reason: "does not match min",
		},
		{
			name: "deadline ignores earlier policy end",
			mutate: func(r *requestBackupV1, p *policyBackupV1, _ *sessionBackupV1) {
				p.EndsAt = timeJSON(t0.Add(30 * time.Minute))
				// 正确截止应为 t0+30m，备份仍保存 t0+1h，决定 t0+2h 晚于两者。
			},
			reason: "does not match min",
		},
		{
			name: "deadline ignores earlier session expiry",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, s *sessionBackupV1) {
				s.ExpiresAt = timeJSON(t0.Add(20 * time.Minute))
				// 正确截止应为 t0+20m，备份仍保存 t0+1h。
			},
			reason: "does not match min",
		},
		{
			name: "missing decided_at",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(time.Time{})
			},
			reason: "missing decided_at",
		},
		{
			name: "decision one nanosecond before deadline",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				r.DecidedAt = timeJSON(t0.Add(time.Hour - time.Nanosecond))
			},
			reason: "before wait deadline",
		},
		{
			name: "decision well before deadline",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				// 决定 t0+30m：那时请求仍在等待，不可能是过期终态。
				r.DecidedAt = timeJSON(t0.Add(30 * time.Minute))
			},
			reason: "before wait deadline",
		},
		{
			name: "nanosecond mismatch cannot pass",
			mutate: func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
				// 截止比正确值早 1 纳秒：必须保留纳秒精度予以拒绝。
				r.WaitDeadline = timeJSON(t0.Add(time.Hour - time.Nanosecond))
			},
			reason: "does not match min",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := buildExpiredTimingBackup(t, t0, tc.mutate)
			// 恢复时刻远近都不影响结论：只按备份记载的完整绝对时刻判断。
			for _, at := range []time.Time{t0.Add(time.Minute), t0.Add(48 * time.Hour)} {
				msg := assertExpiredTimingRejected(t, data, at)
				if !strings.Contains(msg, tc.reason) {
					t.Fatalf("error %q must explain %q", msg, tc.reason)
				}
			}
		})
	}
}

// TestRestoreExpiredTimingConsistentBalancesCannotPass 验证余额、累计额度与
// 账本完全自洽的备份，只要过期时间矛盾仍必须整体拒绝、不返回部分钱包。
func TestRestoreExpiredTimingConsistentBalancesCannotPass(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// 任务给定反例：截止被改成 12:06（正确 12:05），余额与累计额度完全一致。
	data := buildExpiredTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, s *sessionBackupV1) {
		s.ExpiresAt = timeJSON(t0.Add(5 * time.Minute))
		r.WaitDeadline = timeJSON(t0.Add(6 * time.Minute))
		r.DecidedAt = timeJSON(t0.Add(8 * time.Minute))
	})
	assertExpiredTimingRejected(t, data, t0.Add(time.Hour))
}

// TestRestoreExpiredTimingRejectsBeforeLazyProcessing 验证时间矛盾的终态请求
// 即使在恢复时刻“看起来早已到期”，也在校验阶段（恢复时的到期惰性处理之前）
// 被整体拒绝：不会因为将自动处理到期请求而放行矛盾历史。
func TestRestoreExpiredTimingRejectsBeforeLazyProcessing(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	data := buildExpiredTimingBackup(t, t0, func(r *requestBackupV1, _ *policyBackupV1, _ *sessionBackupV1) {
		// 截止被清空；恢复时刻远在将来也不能放行。
		r.WaitDeadline = timeJSON(time.Time{})
	})
	assertExpiredTimingRejected(t, data, t0.Add(72*time.Hour))
}
