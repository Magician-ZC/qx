package session

// 文件说明：账户级 LLM 成本配额的计费周期键派生（设计 docs/产品方案PRD.md §3.1「付费用户成本上限=月卡×40%，用配额封死」）。
//
// 历史背景：本文件原含 chargeLLMQuota（把单次 LLM 成本累加进 account_llm_quota 的旁路）+ 仅服务它的 billingQuotaEnabled
// 与 quotaDefaultCapMicroUSD。账户级成本记账已统一由注入的 SpendRecorder（=billing.Service，见 service.go 的
// recordLLMSpendBestEffort → spend.AddSpend）承接，与会话级成本表同口径覆盖全部带真实成本的 LLM 站点；那条旁路彻底成为
// 死代码（连 QUNXIANG_BILLING_ENABLED 开启分支也零调用），已删除（审计 B-3）。
//
// 仅 quotaPeriodBucket 仍活：service.go 的 recordLLMSpendBestEffort 用它给 billing.Service.AddSpend 派生计费周期键。

import "time"

// quotaPeriodBucket 返回当前计费周期键（UTC 月份 "YYYY-MM"）。基于 wall-clock 的计费周期，非模拟随机，故用 time 不破坏可复现。
// 口径须与 billing.Service.CheckQuota / AddSpend 一致（同表 account_llm_quota 的 period_bucket）。
func quotaPeriodBucket() string {
	return time.Now().UTC().Format("2006-01")
}
