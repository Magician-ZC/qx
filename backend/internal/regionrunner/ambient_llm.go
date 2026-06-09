package regionrunner

// 文件说明：region-runner 的 HOT 单位 LLM 决策（M7.3-real-3，大世界沙盘 §7「决策用 LLM、结算用代码」）。
// 仅**当前 HOT**（正活跃）的单位才升级到 LLM 在 {觅食/休息/社交/反思} 间选动作；非 HOT / 未启用 / 预算耗尽 /
// LLM 任何失败 → 一律回退零成本反射 decideAmbientReflex（确定性兜底，绝不中断循环）。整块 flag-gated：
// 默认 r.llm==nil → 全程走反射（与 real-4a 行为一致）；main 按 QUNXIANG_REGION_RUNNER_LLM 注入客户端才启用。
//
// 与 session 的差异（runner 是离线、跨会话、无 session State）：
//   - 预算闸：session 的 llmBudgetGuardrailActive 绑定单局 State.Metrics，runner 无 State → 改用**进程级**累计成本闸
//     （注入式成本估算函数，沿用 session.EstimateLLMCostUSD 的同一套单价表；超 ceiling 即 latch、此后全转反射）。
//   - 归因校验：session 的 prepareAttribution 绑定 State（人格/压力/记忆/关系快照）且用于拦「突然恋爱/叛变」等**戏剧性**
//     选择；离线 ambient 动作空间（觅食/休息/社交/反思）本就是平淡的日常生存，无 OOC 风险，故 real-3 **不接归因**。
//     ⚠️ 若将来 ambient 动作空间扩到戏剧性选择，必须补离线归因。
//   - 留痕：动作的**结果**仍经 status.Mutator + AMBIENT_* reason-code 落事件审计；LLM 的 prompt/response 本身不持久化
//     （仅 Stats 聚合遥测 llm_calls/llm_cost），属 MVP 取舍——平淡决策的逐条 prompt 审计价值低。

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/engine/scheduler"
	"qunxiang/backend/internal/unit"
)

// ambientLLM 是离线决策所需的最小 LLM 能力（与 session.completionClient 同形；*ai.Service 直接满足）。注入式以免 regionrunner 依赖 session。
type ambientLLM interface {
	GenerateJSON(ctx context.Context, req ai.CompletionRequest) (ai.CompletionResult, error)
}

// costEstimator 把一次调用的用量估成 USD（注入 session.EstimateLLMCostUSD，沿用同一单价表，避免 regionrunner 复制定价/依赖 session）。
type costEstimator func(provider string, model string, promptTokens int, outputTokens int) float64

// ambientLLMTimeout 是**整个离线决策**的硬上限（经派生 ctx 强制，跨 provider/endpoint 故障链也不超过它）。
// ai.Service.GenerateJSON 会遍历 Primary→Secondary→Tertiary 多 provider、每 provider 多 endpoint，每段单次 HTTP 各拿一份
// req.Timeout——若不套整体 deadline，一次决策可在长链上累积到数分钟、长占 worker（默认 4 worker 全卡死则吞吐崩溃）。
// ambientPerAttemptTimeout 是单次 HTTP 尝试的超时（小于整体上限，留出链内有限重试空间）。
const (
	ambientLLMTimeout        = 45 * time.Second
	ambientPerAttemptTimeout = 20 * time.Second
	defaultAmbientBudgetUSD  = 1.0 // SetLLMClient 收到 <=0 预算时夹到此安全正值（money guardrail 失败安全，绝不退化成无上限）
)

const ambientDecisionSystemPrompt = "你是《群像》大世界里一个角色在战斗之外的日常自主意识。结合她当下的主导渴望、最近触发的记忆与候选日常的倾向分，只在 forage、rest、socialize、reflect 四个日常动作中选一个最贴合的，并用一句话说明前因（reasoning），返回 JSON。"

// ambientDecisionSchema 约束 LLM 只能产出四个已注册动作之一（gojsonschema 强校验，越界即被 ai.Service 拒绝）。
// reasoning 字段（必填、≤60 字）让 LLM 把选择前因显式化（动机栈消费：选择须有可解释依据，呼应归因「意外但合理」原则的离线轻量版）；
// 当前仅作遥测/可解释性，不持久化、不拦截（离线日常动作空间平淡、无 OOC 风险，见本文件顶部说明）。
var ambientDecisionSchema = []byte(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["action", "reasoning"],
  "properties": {
    "action": {"type": "string", "enum": ["forage", "rest", "socialize", "reflect"]},
    "reasoning": {"type": "string", "maxLength": 60}
  }
}`)

// SetLLMClient 由 main 在 QUNXIANG_REGION_RUNNER_LLM 开启时注入 LLM 客户端、成本估算函数与进程级预算上限（USD）。
// 不调用此方法（默认）则 r.llm==nil → HOT 单位也走反射，行为同 real-4a。budgetUSD<=0 表示不设上限（不建议）。
func (r *Runner) SetLLMClient(llm ambientLLM, cost costEstimator, budgetUSD float64) {
	if r == nil || llm == nil {
		return
	}
	// money guardrail 失败安全：<=0 的预算（含配置笔误 0/负数/极小值）一律夹到安全正默认，绝不退化成「无上限烧钱」。
	if budgetUSD <= 0 {
		r.log.Warn("region-runner LLM budget non-positive; clamping to default", "got", budgetUSD, "default", defaultAmbientBudgetUSD)
		budgetUSD = defaultAmbientBudgetUSD
	}
	r.llm = llm
	r.costEstimate = cost
	r.llmBudgetMicroUSD = int64(budgetUSD * 1e6)
}

// llmBudgetAllows 判断离线 LLM 预算是否仍有余额（latch 后恒 false——一旦耗尽不再恢复，避免抖动）。
func (r *Runner) llmBudgetAllows() bool {
	if atomic.LoadInt32(&r.llmLatched) == 1 {
		return false
	}
	if r.llmBudgetMicroUSD <= 0 {
		return false // fail-safe：无有效上限即不花钱（正常路径 SetLLMClient 已夹为正值，此分支理论不可达）
	}
	if atomic.LoadInt64(&r.llmSpentMicroUSD) >= r.llmBudgetMicroUSD {
		atomic.StoreInt32(&r.llmLatched, 1)
		return false
	}
	return true
}

// addLLMCost 把一次调用的成本累加进进程级预算（调用即计费，无论成败）；超上限即 latch。
func (r *Runner) addLLMCost(result ai.CompletionResult) {
	if r.costEstimate == nil {
		return
	}
	usd := r.costEstimate(result.Provider, result.Model, result.Usage.PromptTokens, result.Usage.CompletionTokens)
	micro := int64(usd * 1e6)
	if micro <= 0 {
		return
	}
	spent := atomic.AddInt64(&r.llmSpentMicroUSD, micro)
	if r.llmBudgetMicroUSD > 0 && spent >= r.llmBudgetMicroUSD {
		atomic.StoreInt32(&r.llmLatched, 1)
	}
}

// chooseAmbientAction 选离线动作（动机栈消费版）：
//  1. 先跑 decideAmbientContextual 生成确定性打分候选（L1 护栏 → 需求强度 → L3 记忆偏置 → 野心乘权 → 降序）——**始终**算，
//     既给 HOT-LLM 路径当 prompt 上下文，又当所有非 LLM/失败路径的 fallback（首选候选即「最强渴望」）。
//  2. 仅 HOT 单位、LLM 已注入且预算未耗尽 → 把候选作上下文喂 LLM 决；否则直接返回候选首选（**非固定反射**，已含野心/记忆偏置）。
//  3. LLM 任何失败（错误/解析/越界）→ 回退候选首选（同样优于固定反射）。绝不中断循环。
//
// flag 关时（野心乘权 flag QUNXIANG_AMBITION_SCORING 默认关）候选排序退化为纯需求强度序，首选与扩链前 decideAmbientReflex
// 的语义一致——不破坏既有行为。返回的动作随后照常过饱和短路 + applyAction。
func (r *Runner) chooseAmbientAction(ctx context.Context, record unit.Record, tier scheduler.Tier) ambientAction {
	candidates := decideAmbientContextual(record)
	fallback := topAmbientAction(candidates)
	if r.llm == nil || tier != scheduler.TierHot || !r.llmBudgetAllows() {
		return fallback
	}
	act, err := r.ambientLLMDecide(ctx, record, candidates)
	if err != nil {
		atomic.AddInt64(&r.st.llmFallbacks, 1)
		return fallback // 失败回退候选首选（含野心/记忆偏置），优于固定反射
	}
	atomic.AddInt64(&r.st.llmCalls, 1)
	return act
}

// ambientLLMDecide 调一次 LLM 选动作并校验落在四个已注册动作内；调用即计费。candidates 是动机栈打分候选（注入 prompt 作上下文）。
func (r *Runner) ambientLLMDecide(ctx context.Context, record unit.Record, candidates []ambientCandidate) (ambientAction, error) {
	// 整个决策的硬上限：派生 ctx 把跨 provider/endpoint 故障链的总耗时夹在 ambientLLMTimeout 内（单次 HTTP 另用更小的
	// req.Timeout）；否则一次决策可在多 provider×多 endpoint 链上累积到数分钟、长占 worker。Run 取消时此 ctx 立即生效、worker 即时退出。
	callCtx, cancel := context.WithTimeout(ctx, ambientLLMTimeout)
	defer cancel()
	result, err := r.llm.GenerateJSON(callCtx, ai.CompletionRequest{
		Task:           ai.TaskDowntime,
		SchemaName:     "region_ambient_decision",
		ResponseSchema: ambientDecisionSchema,
		SystemPrompt:   ambientDecisionSystemPrompt,
		UserPrompt:     buildAmbientPrompt(record, candidates),
		Temperature:    0.4,
		MaxTokens:      120, // 增量上限：动机栈上下文 + reasoning 字段，控在 ≤120
		Timeout:        ambientPerAttemptTimeout,
		Metadata:       map[string]string{"unit_id": record.ID, "source": "region_runner_ambient"},
	})
	r.addLLMCost(result)
	if err != nil {
		return "", err
	}
	return parseAmbientAction(result.Output)
}

// parseAmbientAction 解析 LLM 输出的动作并校验已注册（schema 已限 enum，此处二次兜底防漂移）。
func parseAmbientAction(output json.RawMessage) (ambientAction, error) {
	var payload struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(output, &payload); err != nil {
		return "", fmt.Errorf("decode ambient decision: %w", err)
	}
	act := ambientAction(payload.Action)
	if _, ok := ambientEffects[act]; !ok {
		return "", fmt.Errorf("ambient decision returned unknown action %q", payload.Action)
	}
	return act, nil
}

// buildAmbientPrompt 用单位当下状态 + 动机栈上下文拼提示词（控成本：MaxTokens 120，增量 ≤120 token）。
// 在原极简状态行之外注入三段动机栈信号（设计：M7.3-real 动机栈消费 / 大世界沙盘 §4）：
//   - 当前最强渴望（候选首选的野心标签 Dominant）：让 LLM 知道这角色的内在主导驱动（如「敛财」「复仇」），措辞与机械打分一致。
//   - 最近触发的记忆（top1，截断 30 字）：让选择带可解释前因（呼应「意外但合理」的离线轻量版）。
//   - 候选日常与倾向分（finalScore 降序，至多 3 条）：把确定性打分结果摆给 LLM 作锚，避免它漂到与机械偏置相悖的随意选择。
//
// candidates 为空时退化为原极简 prompt（失败安全）。所有注入段都从已读 record 派生，零额外 DB/LLM。
func buildAmbientPrompt(record unit.Record, candidates []ambientCandidate) string {
	name := record.Identity.Name
	if name == "" {
		name = "她"
	}
	mood := record.Status.Mood
	if mood == "" {
		mood = "平静"
	}
	prompt := fmt.Sprintf(
		"角色：%s。此刻在战斗之外。饥饿 %d/100（越低越饿），士气 %.2f（0-1，越高越振奋），心情「%s」。",
		name, record.Status.Hunger, record.Status.Morale, mood,
	)

	// 当前最强渴望（野心主导标签）：从六维野心取引力最高的行为标签，告诉 LLM 这角色的内在主导驱动。
	if dominant := dominantDesireLabel(record); dominant != "" {
		prompt += "她的主导渴望：" + dominant + "。"
	}

	// 最近触发的记忆（top1，≤30 字）：给选择一个可解释的前因锚。
	if mem := topMemoryTrigger(record); mem != "" {
		prompt += "最近触发的记忆：" + mem + "。"
	}

	// 候选日常与倾向分（已降序）：把确定性打分摆给 LLM 当锚。
	if line := candidateTendencyLine(candidates); line != "" {
		prompt += "候选日常与倾向分（越高越倾向）：" + line + "。"
	}

	prompt += "在 forage(觅食)/rest(休息)/socialize(社交)/reflect(独处反思) 里，她最想做哪件日常事？并用一句话说明前因。"
	return prompt
}

// dominantDesireLabel 把单位野心引力最高的行为标签翻成一个中文渴望词；无明显野心 / flag 关时返回 ""（不注入）。
// 标签语义对齐 ambient_decision.go 的 ambitionDimensionTags（镜像 session）；纯查表、确定性。
func dominantDesireLabel(record unit.Record) string {
	tag := dominantAmbitionTag(record)
	switch tag {
	case "conquer":
		return "攻伐扩张"
	case "revenge":
		return "复仇雪耻"
	case "hoard":
		return "敛财囤积"
	case "nurture":
		return "养育血脉"
	case "train":
		return "精进技艺"
	case "explore":
		return "闯荡求索"
	case "bond":
		return "经营人脉"
	default:
		return "" // 无明显野心（全 0）→ 不注入主导渴望，保持 prompt 精简
	}
}

// topMemoryTrigger 取最近一条记忆 highlight 并截断到 ≤30 字（rune 计），控 token；无记忆返回 ""。
func topMemoryTrigger(record unit.Record) string {
	highlights := unit.RecentHighlights(record, 1)
	if len(highlights) == 0 {
		return ""
	}
	return truncateRunes(highlights[len(highlights)-1], 30)
}

// candidateTendencyLine 把候选（已降序）拼成「socialize:0.91 / rest:0.60 / forage:0.50」一行，至多取前 3 条控 token。
func candidateTendencyLine(candidates []ambientCandidate) string {
	if len(candidates) == 0 {
		return ""
	}
	limit := len(candidates)
	if limit > 3 {
		limit = 3
	}
	line := ""
	for i := 0; i < limit; i++ {
		if i > 0 {
			line += " / "
		}
		line += fmt.Sprintf("%s:%.2f", candidates[i].action, candidates[i].finalScore)
	}
	return line
}

// truncateRunes 按 rune 截断到至多 n 个字符（避免在多字节 UTF-8 中间切断）；超长加省略号。
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
