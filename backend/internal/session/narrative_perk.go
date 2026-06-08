package session

// 文件说明：narrative_perk.go，叙事密度付费档（设计 PRD §3.6「权益→玩法 perk」）。
// 把 entitlement 从「死数据」接进玩法：持有「叙事密度/会员」权益的账户，其战报与身世叙事会更丰富细腻。
//
// 反 P2W 宪法（与 billing.SeedDefaultSKUs / arbitration / encounter.AllocateLoot 同纪律）：
// perk 只增「叙事密度 / 陪伴感」（多一两句环境与心理描写），绝不增益任何战斗 Score、命运仲裁胜率、
// 掉落排他权或任何影响胜负/资源的量。本文件产出的唯一作用是往叙事 prompt 追加一句「写得更细」的风格指令，
// 不改任何受保护字段、不改 schema、不改 MaxTokens 之外的玩法量纲——纯叙事详略，玩家间结果分布不变。
//
// nil 安全 / 默认无 perk = 今日行为：mirror service.go 的 SpendRecorder 注入范式——
// 未注入 EntitlementChecker（service.entitlements==nil）或账户为空时，narrativeDensityPerk 恒 false、
// narrativeDensityHint 恒空串，对默认链路（匿名局 / 未开 billing flag）零行为变化。

import (
	"context"
	"strings"
)

// EntitlementChecker 是账户权益查询器的最小接口（由 main/router 用 billing.Service 适配后注入）。
// 刻意只返回基元 []string（账户当前 active 权益的 SKUID 列表），使注入方适配器极简、且避免 session→billing 包依赖
// （与 SpendRecorder 同纪律）：适配器只需把 billing.ListEntitlements 过滤 Status=active 后取 SKUID 拼成 []string。
type EntitlementChecker interface {
	ActiveEntitlementSKUs(ctx context.Context, accountID string) ([]string, error)
}

// SetEntitlementChecker 注入账户权益查询器（Wire agent 在 QUNXIANG_BILLING_ENABLED 开时调用，传 billing.Service 的适配器）。
// 传 nil 等价于关闭叙事密度 perk（向后兼容，零行为变化）。mirror service.go 的 SetSpendRecorder。
func (service *Service) SetEntitlementChecker(c EntitlementChecker) {
	if service == nil {
		return
	}
	service.entitlements = c
}

// narrativeDensityPerk 判某账户是否持有「叙事密度 / 会员」权益（设计 PRD §3.6）。
//
// 默认无 perk = 今日行为：
//   - service==nil / service.entitlements==nil（未注入）/ accountID 空（匿名局）→ false；
//   - best-effort 吞错：ListEntitlements 报错一律按「无 perk」处理，绝不阻断/影响叙事主链路。
//
// kind 口径（与 billing.SeedDefaultSKUs 的稳定 SKU id 对齐，service.go:305-310）：
//   - 会员订阅：SKUID 含 "member" 或 "subscription"（sku_member_monthly/quarterly/yearly）；
//   - 叙事密度包：SKUID 含 "narrative_density"（sku_narrative_density_pack）；
//   - 仅 Status=="active" 的权益生效（过期/退款的不给 perk）。
//
// 注：billing.Entitlement 的 ExpiresAt 是否到期由 billing 侧续期/状态机维护；本处只看 Status=active，
// 不做时间比对（保守且零量纲风险——即便误判持有，影响也仅是多一句叙事描写，不触碰任何胜负量）。
func (service *Service) narrativeDensityPerk(ctx context.Context, accountID string) bool {
	if service == nil || service.entitlements == nil {
		return false
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	skus, err := service.entitlements.ActiveEntitlementSKUs(ctx, accountID)
	if err != nil {
		return false // best-effort 吞错 → 默认无 perk。
	}
	for _, raw := range skus {
		sku := strings.ToLower(strings.TrimSpace(raw))
		if sku == "" {
			continue
		}
		if strings.Contains(sku, "narrative_density") ||
			strings.Contains(sku, "member") ||
			strings.Contains(sku, "subscription") {
			return true
		}
	}
	return false
}

// narrativeDensityHint 把 perk 布尔转成可直接拼进叙事 prompt 的风格指令。
// perk=true → 返回「更丰富细腻」的描写要求（多一两句环境与心理细节）；false → 空串（默认详略，今日行为）。
//
// 反 P2W：本句只影响叙事「写多写细」，不暗示任何结果倾向（不写「你会赢 / 你更强 / 你掉落更好」之类）。
// 调用方在拼接时若拿到空串应原样跳过（不加多余空行）。
func (service *Service) narrativeDensityHint(perk bool) string {
	if !perk {
		return ""
	}
	return "叙事密度档（会员陪伴感）：在不改变事实与胜负结果的前提下，描写可以更丰富细腻一些——" +
		"多一两句环境氛围与人物心理细节，让画面与情绪更有沉浸感。仅增叙事详略，不得暗示或改变任何战斗、命运或掉落的结果。"
}
