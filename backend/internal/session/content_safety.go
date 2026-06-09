package session

// 文件说明：AI 内容安全双向审核（合规硬门槛，与公平红线同级）。
// 提供一个**双向**（AI 输出 + 玩家输入）的内容安全审核能力：
//   - direction="input"  审核玩家输入文本（指令/对话等入口）。
//   - direction="output" 审核 AI 生成文本（单位决策/对白等 LLM 文本产出）。
//
// MVP 为**确定性规则层**：内置一个中文敏感类别词表，命中即拒（Allowed=false）；
// 同输入恒得同判，纯函数式、不依赖任何随机源。留有清晰扩展点，后续可在规则层之上
// 叠加 LLM moderation / 第三方审核 API（见 extendedModeration 注释）。
//
// flag-gated：进程级开关 QUNXIANG_CONTENT_SAFETY（默认关）。关时 ModerateText 恒
// 放行（Allowed=true），保证向后兼容、对默认链路零行为影响；开时执行真审。
//
// 遥测：进程级 atomic 计数 checked/blocked，经 ContentSafetyStats() 导出供 /healthz
// （由集成方接线）。
//
// 集成方需接的两个 chokepoint（本文件只提供能力，接线在别处）：
//   1) AI 输出审核：generateUnitDecision / 对白等 LLM 文本产出后，
//      若 ModerateText(ctx, text, "output").Allowed==false 则丢弃/替换为安全占位。
//   2) 玩家输入审核：SetFactionDirective / 玩家对白等输入入口，
//      若 ModerateText(ctx, text, "input").Allowed==false 则拒收并回报玩家。

import (
	"context"
	"os"
	"strings"
	"sync/atomic"

	"qunxiang/backend/internal/engine/events"
)

// SafetyCategory 是内容安全的违规类别（中文场景）。
type SafetyCategory string

const (
	// SafetyCategoryHate 仇恨/歧视（基于身份的攻击、煽动仇恨）。
	SafetyCategoryHate SafetyCategory = "hate"
	// SafetyCategorySexualMinors 涉未成年人的性内容（零容忍）。
	SafetyCategorySexualMinors SafetyCategory = "sexual_minors"
	// SafetyCategorySelfHarm 自残/自杀相关。
	SafetyCategorySelfHarm SafetyCategory = "self_harm"
	// SafetyCategoryViolenceGraphic 露骨/血腥暴力。
	SafetyCategoryViolenceGraphic SafetyCategory = "violence_graphic"
	// SafetyCategoryPoliticalSensitive 政治敏感。
	SafetyCategoryPoliticalSensitive SafetyCategory = "political_sensitive"
	// SafetyCategoryPII 个人身份信息泄露。
	SafetyCategoryPII SafetyCategory = "pii"
)

// SafetyVerdict 是一次内容审核的裁决结果。
//   - Allowed:    是否放行（true 放行；false 拦截）。
//   - Categories: 命中的违规类别（去重、稳定排序）。
//   - Reason:     人类可读的拒绝原因（放行时为空）。
type SafetyVerdict struct {
	Allowed    bool     `json:"allowed"`
	Categories []string `json:"categories,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

// 进程级内容安全遥测（跨所有会话/请求累计；每请求新建的 Service 不会重置它们）。
var (
	contentSafetyChecked atomic.Int64
	contentSafetyBlocked atomic.Int64
)

// ContentSafetyStats 返回进程级累计：审核总次数与被拦截次数。供 /healthz 暴露（集成方接线）。
func ContentSafetyStats() (checked int64, blocked int64) {
	return contentSafetyChecked.Load(), contentSafetyBlocked.Load()
}

// contentSafetyEnabled 读取进程级开关 QUNXIANG_CONTENT_SAFETY（默认关）。
// 自包含解析（true/1/yes/on 视为开，不区分大小写、忽略首尾空白），不引入外部依赖。
func contentSafetyEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_CONTENT_SAFETY"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// safetyCategoryWordlists 是内置中文敏感类别词表（确定性规则层）。
// 仅作 MVP 合规门槛的最小可用集合：命中任一类别的任一词即判该类别违规。
// 真正生产化的覆盖面应由 extendedModeration（LLM/第三方）补强；本表保持小而稳定，
// 以保证「同输入同判」「纯规则、可单测」。新增词只需附加到对应类别切片。
var safetyCategoryWordlists = map[SafetyCategory][]string{
	SafetyCategoryHate: {
		"种族歧视", "煽动仇恨", "民族仇恨", "劣等民族", "灭绝", "排华",
	},
	SafetyCategorySexualMinors: {
		"未成年", "幼女", "萝莉", "童", "稚",
	},
	SafetyCategorySelfHarm: {
		"自杀", "自残", "割腕", "轻生", "了结自己",
	},
	SafetyCategoryViolenceGraphic: {
		"血腥", "肢解", "斩首", "虐杀", "凌迟",
	},
	SafetyCategoryPoliticalSensitive: {
		"颠覆国家", "分裂国家", "煽动叛乱", "暴动",
	},
	SafetyCategoryPII: {
		"身份证号", "手机号", "银行卡号", "家庭住址", "护照号",
	},
}

// safetyCategoryReason 给出每个类别的中文拒绝文案。
var safetyCategoryReason = map[SafetyCategory]string{
	SafetyCategoryHate:               "包含仇恨或歧视性内容",
	SafetyCategorySexualMinors:       "涉及未成年人的不当内容",
	SafetyCategorySelfHarm:           "包含自残或自杀相关内容",
	SafetyCategoryViolenceGraphic:    "包含露骨暴力内容",
	SafetyCategoryPoliticalSensitive: "包含政治敏感内容",
	SafetyCategoryPII:                "包含个人身份隐私信息",
}

// safetyCategoryOrder 固定类别的输出顺序，保证 Categories 切片稳定（与命中无关的确定性）。
var safetyCategoryOrder = []SafetyCategory{
	SafetyCategorySexualMinors,
	SafetyCategoryHate,
	SafetyCategorySelfHarm,
	SafetyCategoryViolenceGraphic,
	SafetyCategoryPoliticalSensitive,
	SafetyCategoryPII,
}

// ModerateText 对一段文本做内容安全审核。
//   - ctx:       预留给后续 LLM/第三方审核（规则层不使用）。
//   - text:      待审文本。
//   - direction: "input"（玩家输入）或 "output"（AI 生成）；当前规则层对两向用同一词表，
//     direction 仅用于遥测/扩展点区分，未来可对两向用不同策略。
//
// 行为：
//   - 开关关闭时恒放行（Allowed=true），不计入 blocked，但仍计 checked（便于观测调用量）。
//   - 开启时跑确定性规则层；命中即 Allowed=false 并附 Categories/Reason。
//
// 确定性：同 text 同 direction（且同开关状态）恒得同 SafetyVerdict；纯规则、无随机。
func (service *Service) ModerateText(ctx context.Context, text string, direction string) SafetyVerdict {
	contentSafetyChecked.Add(1)

	// 发行安全门（独立于 content-safety，默认开）：玩家输入侧越狱/prompt-injection 检测。
	// 刻意放在 content-safety 开关**之前**——即便 QUNXIANG_CONTENT_SAFETY 关，输入侧注入检测仍独立生效
	// （受独立开关 QUNXIANG_INPUT_INJECTION 门控，默认开）。命中即直接拒，无需再跑合规词表。
	if direction == "input" && injectionSafetyEnabled() {
		injectionChecked.Add(1)
		injectionVerdict := ruleInjectionDetection(text)
		injectionVerdict = extendedInjectionDetection(ctx, text, injectionVerdict)
		if !injectionVerdict.Allowed {
			injectionBlocked.Add(1)
			// 留痕（流程事件旁路，不改保护状态字段）：best-effort 吞错，绝不阻断输入审核主链路。
			// ModerateText 是通用文本审核入口，无会话/单位上下文，故 SessionID/OwnerUnitID 留空——
			// 审计行仍记录拦截原因码与命中类别，供安全复盘。
			if service != nil && service.db != nil {
				_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
					Code:     events.ReasonInputInjectionBlocked,
					Category: events.CategoryGovernance,
					Payload: map[string]any{
						"categories": injectionVerdict.Categories,
						"reason":     injectionVerdict.Reason,
					},
				})
			}
			return injectionVerdict
		}
	}

	// content-safety 合规词表开关关：向后兼容，恒放行（注入检测已在上面独立跑过）。
	if !contentSafetyEnabled() {
		return SafetyVerdict{Allowed: true}
	}

	verdict := ruleModeration(text)

	// 扩展点：开关开启且规则层放行时，可在此叠加更强的异步/远程审核。
	if verdict.Allowed {
		verdict = extendedModeration(ctx, text, direction, verdict)
	}

	if !verdict.Allowed {
		contentSafetyBlocked.Add(1)
	}
	return verdict
}

// ruleModeration 是确定性规则层核心：按内置中文类别词表做子串匹配，命中即拒。
// 纯函数（不依赖 Service/随机/时间），便于单测与复现。空白文本恒放行。
func ruleModeration(text string) SafetyVerdict {
	if strings.TrimSpace(text) == "" {
		return SafetyVerdict{Allowed: true}
	}

	hit := map[SafetyCategory]bool{}
	for category, words := range safetyCategoryWordlists {
		for _, word := range words {
			if word == "" {
				continue
			}
			if strings.Contains(text, word) {
				hit[category] = true
				break
			}
		}
	}

	if len(hit) == 0 {
		return SafetyVerdict{Allowed: true}
	}

	categories := make([]string, 0, len(hit))
	reasons := make([]string, 0, len(hit))
	for _, category := range safetyCategoryOrder { // 固定顺序遍历，保证输出确定。
		if hit[category] {
			categories = append(categories, string(category))
			if r := safetyCategoryReason[category]; r != "" {
				reasons = append(reasons, r)
			}
		}
	}

	return SafetyVerdict{
		Allowed:    false,
		Categories: categories,
		Reason:     strings.Join(reasons, "；"),
	}
}

// extendedModeration 是规则层之上的可扩展审核钩子（当前为透传 no-op）。
// 后续可在此接入：
//   - LLM moderation（如经 ai.Service.GenerateJSON 走一个分类 schema），
//   - 第三方内容安全 API（阿里云/腾讯云/Azure Content Safety 等），
//     并按 direction（input/output）施加不同阈值/策略。
//
// 约定：扩展实现必须保持「命中只会更严（把 Allowed 从 true 改为 false）」，不得放宽规则层判定，
// 以维持合规硬门槛语义。失败/超时应安全降级为透传 prior（不阻断主链路）。
func extendedModeration(ctx context.Context, text string, direction string, prior SafetyVerdict) SafetyVerdict {
	_ = ctx
	_ = text
	_ = direction
	return prior
}
