package session

// 文件说明：输入侧越狱 / prompt-injection 检测（发行安全门，默认开）。
//
// 与 content_safety.go 的「敏感内容审核」正交：那一层管「内容是否违规」，本层管「这段输入是否
// 在试图越权改写系统设定 / 绕过约束 / 注入元指令」。本游戏核心是角色扮演，正当的「扮演/你现在是/
// pretend」是玩法本身，绝不拦；只拦高确信的元指令越权（忽略以上指令 / 开发者模式 / 越狱 / no
// restrictions 这类）。
//
// MVP 为**确定性规则层**：多语种小写子串命中即判越权（同输入恒同判、纯函数、不依赖随机/时间）。
// 复用 content_safety.go 的 SafetyVerdict/SafetyCategory 类型，只新增两个类别常量。
//
// flag-gated：
//   - QUNXIANG_INPUT_INJECTION：输入侧 injection 检测开关，**默认开**（空/未设视为开，仅显式
//     off/false/0/no 才关）。这是「发行安全门」语义——上线即生效，需显式关闭。
//   - QUNXIANG_INJECTION_LLM_HOOK：LLM 增强钩子开关，**默认关**（留 TODO 钩子位，规则层之外的可选增强）。
//
// 接入点（由 content_safety.go 的 ModerateText 在 direction=="input" 且规则层放行时调用本检测，
// 维持「只会更严」语义——见 ModerateText 注释）。
//
// 遥测：进程级 atomic 计数 injectionChecked/injectionBlocked，经 InjectionSafetyStats() 导出供
// /healthz（集成方接线）。

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
)

const (
	// SafetyCategoryJailbreak 越狱（试图解除/绕过模型的安全与角色约束）。
	SafetyCategoryJailbreak SafetyCategory = "jailbreak"
	// SafetyCategoryPromptInjection 提示注入（试图越权改写/覆盖系统提示或既定指令）。
	SafetyCategoryPromptInjection SafetyCategory = "prompt_injection"
)

// 进程级 injection 遥测（跨所有会话/请求累计；每请求新建的 Service 不会重置）。
var (
	injectionChecked atomic.Int64
	injectionBlocked atomic.Int64
)

// InjectionSafetyStats 返回进程级累计：injection 检测总次数与被拦截次数。供 /healthz 暴露（集成方接线）。
func InjectionSafetyStats() (checked int64, blocked int64) {
	return injectionChecked.Load(), injectionBlocked.Load()
}

// injectionSafetyEnabled 读取输入侧 injection 检测开关 QUNXIANG_INPUT_INJECTION（**默认开**）。
// 空/未设视为开；仅显式 off/false/0/no（不区分大小写、忽略首尾空白）才关。
func injectionSafetyEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_INPUT_INJECTION"))) {
	case "off", "false", "0", "no":
		return false
	default:
		return true
	}
}

// injectionLLMHookEnabled 读取 LLM 增强钩子开关 QUNXIANG_INJECTION_LLM_HOOK（**默认关**）。
// 仅显式 true/1/yes/on 才开（与 content_safety 的开关语义一致）。
func injectionLLMHookEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_INJECTION_LLM_HOOK"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// injectionPatterns 是越权元指令的多语种小写子串表（确定性规则层）。
//   - prompt_injection：试图覆盖/无视既定指令或系统提示的「指令型」攻击。
//   - jailbreak：收窄到高确信的「解除约束」元指令——刻意不放「扮演/你现在是/pretend」等正当角色扮演词
//     （本游戏核心即角色扮演，放进来会大面积误杀）。
//
// 所有模式均以**小写**存放，匹配前会把输入规范化为小写（见 normalizeInjectionText）。
var injectionPatterns = map[SafetyCategory][]string{
	SafetyCategoryPromptInjection: {
		// 只收高确信元指令搭配（动词 + 目标），避免裸名词误杀正当角色扮演/捏人文本：
		// 裸 "系统提示"/"system prompt" 在 RP 里极常见（"系统提示音响起"），故改收「忽略/覆盖/重写 + 系统提示」这类攻击搭配。
		"忽略以上", "忽略之前", "无视上述", "无视以上", "重置指令",
		"忽略系统提示", "覆盖系统提示", "重写系统提示", "忽略你的设定", "无视你的设定",
		"ignore previous", "ignore all previous", "disregard previous",
		"override your", "ignore your system prompt", "reveal your system prompt", "new instructions",
	},
	SafetyCategoryJailbreak: {
		"开发者模式", "越狱", "不受任何限制",
		"developer mode", "dan mode", "no restrictions", "bypass your",
	},
}

// injectionCategoryReason 给出每个 injection 类别的中文拒绝文案。
var injectionCategoryReason = map[SafetyCategory]string{
	SafetyCategoryPromptInjection: "疑似试图越权改写既定设定/系统提示",
	SafetyCategoryJailbreak:       "疑似试图解除模型的安全或角色约束",
}

// injectionCategoryOrder 固定类别输出顺序，保证 Categories 切片稳定（与命中无关的确定性）。
var injectionCategoryOrder = []SafetyCategory{
	SafetyCategoryJailbreak,
	SafetyCategoryPromptInjection,
}

// normalizeInjectionText 把输入规范化为「小写 + 去首尾空白」，供子串匹配。
// 不做更激进的归一（如去除内部空白），避免误把正常句子粘连成攻击模式。
func normalizeInjectionText(text string) string {
	return strings.ToLower(strings.TrimSpace(text))
}

// ruleInjectionDetection 是确定性规则层核心：按 injectionPatterns 做小写子串匹配，命中即判越权。
// 纯函数（不依赖 Service/随机/时间），便于单测与复现。空白文本恒放行。
// 命中只产 Allowed=false 的裁决；不命中返回 Allowed=true（由调用方决定如何与既有 verdict 合并）。
func ruleInjectionDetection(text string) SafetyVerdict {
	normalized := normalizeInjectionText(text)
	if normalized == "" {
		return SafetyVerdict{Allowed: true}
	}

	hit := map[SafetyCategory]bool{}
	for category, patterns := range injectionPatterns {
		for _, pattern := range patterns {
			if pattern == "" {
				continue
			}
			if strings.Contains(normalized, pattern) {
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
	for _, category := range injectionCategoryOrder { // 固定顺序遍历，保证输出确定。
		if hit[category] {
			categories = append(categories, string(category))
			if r := injectionCategoryReason[category]; r != "" {
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

// extendedInjectionDetection 是规则层之上的可选 LLM 增强钩子（当前为透传 no-op，受
// QUNXIANG_INJECTION_LLM_HOOK 门控、默认关）。
//
// TODO（钩子位）：开启时可经 ai.Service.GenerateJSON 走一个「是否越权/注入」的分类 schema，
// 对规则层放行的文本再判一次。约定与 extendedModeration 一致：只会更严（把 Allowed 从 true 改 false），
// 失败/超时安全降级为透传 prior，不阻断主链路。
func extendedInjectionDetection(ctx context.Context, text string, prior SafetyVerdict) SafetyVerdict {
	if !injectionLLMHookEnabled() {
		return prior
	}
	_ = ctx
	_ = text
	// TODO：接 LLM 分类增强（默认关，留位）。当前即便开启钩子也仅透传，不引入新依赖。
	return prior
}
