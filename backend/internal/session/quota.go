package session

// 文件说明：账户级 LLM 成本配额累加的轻量旁路（设计 docs/产品方案PRD.md §3.1「付费用户成本上限=月卡×40%，用配额封死」）。
// 把单局/单次 LLM 成本累加进 account_llm_quota 表，为「会话级预算护栏（llm_budget.go）升级到账户级」铺路。
//
// 纪律与不变量：
//  1. flag-gated：QUNXIANG_BILLING_ENABLED 关时（默认）整方法 no-op、零行为变化、零 DB 写——对默认链路无成本。
//  2. best-effort：内部吞错（仅记入日志由调用方决定），账户配额累加**绝不**中断或影响模拟主循环（与埋点 analytics 同纪律）。
//  3. 双驱动：经 dbdialect.IsMySQL 分支 ON CONFLICT vs ON DUPLICATE KEY，统一 ? 占位（与 billing.Service.AddSpend 等价语义）。
//  4. 确定性无关：纯累加（spent += micro），不引入随机；periodBucket 由当前 UTC 月份派生（计费周期键，非模拟随机）。
//
// 接线点（由整合方接，本文件仅提供方法 + 说明）：
//   真正的调用应落在「LLM 成本结算处」——即 ai.Service.GenerateJSON 返回后、把单次 LLMInteraction.EstimatedCostUSD 折算成
//   micro_usd 的地方（session/llm*.go / metrics.go 附近）。账户 ID 来自当前会话所属账户（建局时 State 上应已带 account 归属；
//   单机匿名局可传空 → 本方法直接 no-op，安全）。把会话级 llm_budget 护栏与本账户级配额二者取「先到者」即完成升级。
//   cap 缺省由 billing 层 DefaultCapMicroUSD（月卡×40%）兜底——见 internal/billing/service.go 的 CheckQuota。

import (
	"context"
	"os"
	"strings"
	"time"

	"qunxiang/backend/internal/storage/dbdialect"
)

// quotaDefaultCapMicroUSD 是账户级 LLM 成本上限兜底（micro_usd）。来源 PRD §3.1：天命月卡 ¥30 ÷ 7.2 × 40% ≈ $1.6667。
// 与 billing.DefaultCapMicroUSD 同值（刻意不跨包引用，避免 session→billing 的额外依赖；二者须保持一致）。
const quotaDefaultCapMicroUSD int64 = 1_666_667

// billingQuotaEnabled 读 QUNXIANG_BILLING_ENABLED（true/1/yes/on 视为开），默认关 → chargeLLMQuota no-op、零行为变化。
func billingQuotaEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_BILLING_ENABLED"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// quotaPeriodBucket 返回当前计费周期键（UTC 月份 "YYYY-MM"）。基于 wall-clock 的计费周期，非模拟随机，故用 time 不破坏可复现。
func quotaPeriodBucket() string {
	return time.Now().UTC().Format("2006-01")
}

// chargeLLMQuota 把一次 LLM 成本（micro_usd）best-effort 累加进 account_llm_quota，供把会话级预算护栏升级到账户级。
// flag QUNXIANG_BILLING_ENABLED 关时（默认）直接 no-op；accountID 空、micro<=0、db 不可用时同样 no-op；内部吞错（best-effort）。
// 返回 error 仅供调用方可选记日志——返回非 nil 也**不应**中断主循环（与 analytics 埋点同纪律）。
func (s *Service) chargeLLMQuota(ctx context.Context, accountID string, microUSD int64) error {
	if !billingQuotaEnabled() {
		return nil
	}
	if s == nil || s.db == nil {
		return nil
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || microUSD <= 0 {
		return nil
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	bucket := quotaPeriodBucket()
	if dbdialect.IsMySQL(s.db) {
		// 跨计费周期重置 spent：仅当存量 period_bucket 与本次相同才累加，否则置为本次 delta（新周期从 0 起，
		// 否则「每期上限」退化成「终生上限」，评审修复）。spent 赋值在 period_bucket 之前 → IF 内引用的是旧 bucket。
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO account_llm_quota (account_id, period_bucket, spent_micro_usd, cap_micro_usd, updated_at) VALUES (?,?,?,?,?)
			 ON DUPLICATE KEY UPDATE spent_micro_usd = IF(period_bucket = VALUES(period_bucket), spent_micro_usd + VALUES(spent_micro_usd), VALUES(spent_micro_usd)), period_bucket = VALUES(period_bucket), updated_at = VALUES(updated_at)`,
			accountID, bucket, microUSD, quotaDefaultCapMicroUSD, now)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO account_llm_quota (account_id, period_bucket, spent_micro_usd, cap_micro_usd, updated_at) VALUES (?,?,?,?,?)
		 ON CONFLICT(account_id) DO UPDATE SET spent_micro_usd = CASE WHEN account_llm_quota.period_bucket = excluded.period_bucket THEN account_llm_quota.spent_micro_usd + excluded.spent_micro_usd ELSE excluded.spent_micro_usd END, period_bucket = excluded.period_bucket, updated_at = excluded.updated_at`,
		accountID, bucket, microUSD, quotaDefaultCapMicroUSD, now)
	return err
}
