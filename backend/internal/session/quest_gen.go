package session

// 文件说明：剧情动态生成（设计 docs/分区大世界设计方案-2026-06-10.md §5「关键设计」）。
// **所有任务的剧情/动机/语气按玩家角色画像（年龄 + 性别 + 出身 + 阵营 + desire/wound/redline）由 LLM 动态生成**——
// 同一桩骨架，对「少年游侠」与「中年遗孀」呈现不同的剧情切入与语气。机制（objective 类型/区域/数值/奖励）由
// quest_catalog.go 的骨架护栏锁定，LLM **只填叙事不改机制**（Title + NarrativeZH），确定性 fallback 兜底，绝不阻断。
//
// 硬约束：
//   - 任务 ID 用 FNV(sessionID + zoneId + skeletonKey) 确定性派生——同会话同区同骨架恒同 id（accept/turn-in 据 id 命中、防重复）。
//   - LLM 不可用/超时/预算护栏/解析失败 → fallback 模板拼确定性任务（"讨平[区]的[boss名]" 等），best-effort。
//   - LLM 只回 {title, narrative}，objective/reward 一律用骨架的（防 LLM 篡改机制刷奖）。

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

const (
	// questTitleMaxRunes / questNarrativeMaxRunes 是 LLM 文案上限（字）——标题一行、叙事一小段，控密度。
	questTitleMaxRunes     = 24
	questNarrativeMaxRunes = 120
)

// questGenPayload 是 LLM 任务剧情生成的结构化产物（被 questGenSchema 强校验）。只含叙事，不含机制。
type questGenPayload struct {
	Title     string `json:"title"`     // 任务标题（贴角色画像，≤24 字）
	Narrative string `json:"narrative"` // 任务剧情切入/动机（第二/三人称叙事，≤120 字）
}

// questGenSchema 强约束 LLM 只返回 {title, narrative}，长度受限——绝不让它回 objective/reward（机制锁在骨架）。
var questGenSchema = []byte(`{
  "type":"object",
  "properties":{
    "title":{"type":"string","minLength":1,"maxLength":24},
    "narrative":{"type":"string","minLength":1,"maxLength":120}
  },
  "required":["title","narrative"],
  "additionalProperties":false
}`)

// questDeterministicID 用 FNV(sessionID + zoneId + skeletonKey) 派生稳定任务 id（同会话同区同骨架恒同 id）。
// 让 accept/turn-in 据 id 命中、防重复接取/重复发奖；与 combat_roll/zoneBoss 的 FNV 确定性口径一致（禁全局 rand）。
func questDeterministicID(sessionID, zoneID, skeletonKey string) string {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(sessionID))
	_, _ = hasher.Write([]byte("|"))
	_, _ = hasher.Write([]byte(zoneID))
	_, _ = hasher.Write([]byte("|"))
	_, _ = hasher.Write([]byte(skeletonKey))
	return fmt.Sprintf("quest_%016x", hasher.Sum64())
}

// generateQuestForSkeleton 据骨架 + 角色画像生成一桩 Quest（设计 §5「关键设计」）。
// 机制（id/zone/type/objectives/rewards）一律取骨架护栏；Title/NarrativeZH 由 LLM 动态填（fallback 模板兜底）。
// 返回 Quest 与一条 LLMInteraction（含 fallback 路径，供 appendLLMInteractionWithSpend 记账/遥测）。
func (service *Service) generateQuestForSkeleton(
	ctx context.Context, state State, actor *unit.Record, charter OfflineCharter, zone world.Zone, sk questSkeleton,
) (Quest, LLMInteraction) {
	if ctx == nil {
		ctx = context.Background()
	}
	// 机制骨架先就位（无论 LLM 是否成功，机制恒由护栏锁定）。Objectives 深拷贝（Current 归 0，防共享底层数组）。
	quest := Quest{
		ID:         questDeterministicID(state.ID, zone.ID, sk.Key),
		ZoneID:     zone.ID,
		Type:       sk.Type,
		Objectives: cloneObjectives(sk.Objectives),
		Rewards:    sk.Reward,
		State:      QuestStateAvailable,
	}

	title, narrative := questFallbackText(zone, sk) // 确定性兜底文案（LLM 失败/不可用时用）
	systemPrompt := questGenSystemPrompt(actor)
	userPrompt := questGenUserPrompt(actor, charter, zone, sk)

	// LLM 不可用 / 预算护栏 / 账户超额 → 直接用 fallback 文案（带 interaction 记账）。
	if service.llm == nil || service.llmBlocked(ctx, state) {
		result := budgetGuardrailResult(state)
		result.UsedFallback = true
		quest.Title, quest.NarrativeZH = title, narrative
		return quest, buildLLMInteraction(state, actor.ID, "quest_gen", title, systemPrompt, userPrompt, result, "")
	}

	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskBackstory,
		SchemaName:     "session_quest_gen",
		ResponseSchema: questGenSchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.7, // 剧情要有变化（不同角色不同切入），略高温
		MaxTokens:      220,
		Timeout:        llmRequestTimeout,
		// Cacheable：同角色同区同骨架的剧情在 prompt 缓存 TTL（5min）内复用、记 $0。两层收益：
		//   ① 成本——AvailableQuests 对每桩骨架各调一次 LLM、前端反复开/刷面板（含接取/交付后 refresh）都会重触发，
		//      开启后 TTL 内命中缓存即跳过真实调用（CacheHit 成本归零，service.go 已处理）。
		//   ② 一致性——cacheKey 由 task+schema+system+user prompt 派生（service.go cacheKey），而 system 含角色名、user 含
		//      完整确定性画像+骨架机制，故同角色同区同骨架 cacheKey 稳定。Temperature 0.7 仅在缓存未命中时生效一次；命中后
		//      AvailableQuests（展示）与 AcceptQuest→resolveQuestByID（落库）拿到**同一套** Title/NarrativeZH，
		//      杜绝「列表里看到讨平赤毛狼王、接取后变成肃清晨曦兽群」的展示/接取不一致与面板刷新文案漂移。
		// 缓存全程 flag-gated（QUNXIANG_PROMPT_CACHE，prompt_cache.go）；关闭时行为同未标 Cacheable（不阻断、走 fallback 路径不变）。
		Cacheable: true,
	})
	if err != nil {
		result.UsedFallback = true
		quest.Title, quest.NarrativeZH = title, narrative
		return quest, buildLLMInteraction(state, actor.ID, "quest_gen", title, systemPrompt, userPrompt, result, err.Error())
	}

	var payload questGenPayload
	if jsonErr := json.Unmarshal(result.Output, &payload); jsonErr != nil ||
		strings.TrimSpace(payload.Title) == "" || strings.TrimSpace(payload.Narrative) == "" {
		result.UsedFallback = true
		cause := "quest gen output empty"
		if jsonErr != nil {
			cause = jsonErr.Error()
		}
		quest.Title, quest.NarrativeZH = title, narrative
		return quest, buildLLMInteraction(state, actor.ID, "quest_gen", title, systemPrompt, userPrompt, result, cause)
	}

	quest.Title = limitTextRunes(strings.TrimSpace(payload.Title), questTitleMaxRunes)
	quest.NarrativeZH = limitTextRunes(strings.TrimSpace(payload.Narrative), questNarrativeMaxRunes)
	return quest, buildLLMInteraction(state, actor.ID, "quest_gen", quest.Title, systemPrompt, userPrompt, result, "")
}

// resolveQuestByID 据 questId 找出对应骨架并重生成该任务（accept 用：确定性 id 命中即重建机制+剧情）。
// 遍历主角当前区 + 任务所属区的骨架（id 由 FNV(sessionID+zoneId+key) 决定，故只需在「能产生这个 id 的区」里找）。
// 返回 (quest, interaction, true)；找不到返回 (zero, zero, false)。
func (service *Service) resolveQuestByID(ctx context.Context, state *State, actor *unit.Record, questID string) (Quest, LLMInteraction, bool) {
	if state == nil || actor == nil {
		return Quest{}, LLMInteraction{}, false
	}
	charter, _ := GetUnitCharter(state, actor.ID)
	// 遍历全部区域的骨架找命中 id 的那桩——id 由 FNV 唯一确定，任一区命中即是它（避免只认当前区导致跨区任务接不了）。
	for zi := range state.Zones {
		zone := state.Zones[zi]
		for _, sk := range questSkeletonsForZone(zone) {
			if questDeterministicID(state.ID, zone.ID, sk.Key) != questID {
				continue
			}
			quest, interaction := service.generateQuestForSkeleton(ctx, *state, actor, charter, zone, sk)
			return quest, interaction, true
		}
	}
	return Quest{}, LLMInteraction{}, false
}

// cloneObjectives 深拷贝目标骨架并把 Current 归 0（防共享底层数组 + 防残留进度）。
func cloneObjectives(src []Objective) []Objective {
	out := make([]Objective, len(src))
	for i, o := range src {
		o.Current = 0
		out[i] = o
	}
	return out
}

// questGenSystemPrompt 是任务剧情生成的系统提示：强调「为这个角色量身定制剧情切入与语气」+ 只回 JSON、不改机制。
func questGenSystemPrompt(actor *unit.Record) string {
	return fmt.Sprintf(
		"你是《一念》分区大世界的任务说书人。请为角色「%s」量身定制一桩任务的标题与剧情切入——"+
			"剧情的语气、动机、切入点必须贴合她的年龄、性别、出身与阵营（少年与中年、游侠与遗孀，所历所感各不相同）。"+
			"你只负责叙事文案，**绝不更改任务的目标与奖励**（那些已由系统锁定）。只能返回 JSON：{title, narrative}。",
		actor.DisplayName(),
	)
}

// questGenUserPrompt 拼装任务剧情生成的用户提示：角色画像（年龄/性别/出身/阵营/夙愿/创伤/红线）+ 任务机制概要（让 LLM 据机制写贴合的叙事）。
func questGenUserPrompt(actor *unit.Record, charter OfflineCharter, zone world.Zone, sk questSkeleton) string {
	var b strings.Builder
	// ── 角色画像（设计 §5：年龄 + 性别 + 初始身份 是动态生成的核心输入）──
	fmt.Fprintf(&b, "【角色画像】\n姓名: %s\n", actor.DisplayName())
	fmt.Fprintf(&b, "年龄: %d 岁\n", actor.Identity.Age)
	fmt.Fprintf(&b, "性别: %s\n", genderDisplayZH(actor.Identity.Gender))
	if origin := strings.TrimSpace(actor.Identity.Lineage); origin != "" {
		fmt.Fprintf(&b, "出身: %s\n", origin)
	}
	if faction := strings.TrimSpace(actor.Faction); faction != "" {
		fmt.Fprintf(&b, "阵营: %s\n", factionDisplayZH(faction))
	}
	fmt.Fprintf(&b, "性格: %s\n", summarizeActorPersonality(*actor))
	// 夙愿/创伤/红线（来自离线宪章——让剧情切入呼应她的长远图景与底色）。
	if goals := trimNonEmpty(charter.LongTermGoals); len(goals) > 0 {
		fmt.Fprintf(&b, "夙愿与长期目标: %s\n", strings.Join(goals, "；"))
	}
	if redlines := charterRedlineTexts(charter); len(redlines) > 0 {
		fmt.Fprintf(&b, "红线（绝不可触碰）: %s\n", strings.Join(redlines, "；"))
	}

	// ── 任务机制概要（已锁定，仅供 LLM 据此写贴合的叙事，不可更改）──
	fmt.Fprintf(&b, "\n【任务机制（已锁定，据此写叙事即可，不要改动）】\n所在区域: %s（%s，等级带 %d-%d）\n",
		zone.Name, zoneKindDisplayZH(zone.Kind), zone.LevelMin, zone.LevelMax)
	fmt.Fprintf(&b, "任务类型: %s\n目标: %s\n", questTypeDisplayZH(sk.Type), objectivesSummaryZH(sk.Objectives, zone))

	fmt.Fprintf(&b, "\n请返回 JSON：title（≤%d字，一句话任务名）、narrative（≤%d字，第二/三人称，写出这桩差事为何落到她头上、她为何会去做——贴合她的年龄/性别/出身/阵营）。不要写系统旁白、不要列目标数值。",
		questTitleMaxRunes, questNarrativeMaxRunes)
	return b.String()
}

// questFallbackText 是确定性兜底文案（LLM 不可用/失败时用）：据任务类型 + 区域信息拼一句标题 + 一段叙事。
// 同输入恒同输出（确定性），绝不阻断任务发布。
func questFallbackText(zone world.Zone, sk questSkeleton) (title, narrative string) {
	switch sk.Type {
	case QuestTypeSlay:
		bossName := strings.TrimSpace(zone.BossName)
		if bossName == "" {
			bossName = "盘踞此地的霸主"
		}
		title = fmt.Sprintf("讨平%s", bossName)
		narrative = fmt.Sprintf("「%s」一带的%s作乱已久，城里悬出了赏格——若能将它讨平，这片天地便能太平些。", zone.Name, bossName)
	case QuestTypeCollect:
		required := objectiveRequired(sk.Objectives)
		title = fmt.Sprintf("为「%s」备办物资", zone.Name)
		narrative = fmt.Sprintf("「%s」的人手紧缺，托她在野外采办 %d 份口粮，以应一时之需。", zone.Name, required)
	case QuestTypeExplore:
		title = fmt.Sprintf("探往「%s」之外", zone.Name)
		narrative = fmt.Sprintf("有人想知道「%s」之外的光景，托她走上一遭，去那相邻的天地看个究竟。", zone.Name)
	default:
		title = fmt.Sprintf("「%s」的一桩差事", zone.Name)
		narrative = fmt.Sprintf("「%s」有一桩差事等人去办。", zone.Name)
	}
	return limitTextRunes(title, questTitleMaxRunes), limitTextRunes(narrative, questNarrativeMaxRunes)
}

// ── 展示辅助（纯函数，中文化，供 prompt 与 fallback 共用）──

// objectiveRequired 取首个目标的 Required（fallback 文案用）；空返回 1。
func objectiveRequired(objs []Objective) int {
	if len(objs) == 0 {
		return 1
	}
	if objs[0].Required < 1 {
		return 1
	}
	return objs[0].Required
}

// objectivesSummaryZH 把目标骨架翻成给 prompt 的中文概要（让 LLM 知道她要做什么，据此写动机）。
func objectivesSummaryZH(objs []Objective, zone world.Zone) string {
	parts := make([]string, 0, len(objs))
	for _, o := range objs {
		switch o.Kind {
		case ObjectiveDefeatBoss:
			name := strings.TrimSpace(zone.BossName)
			if name == "" {
				name = "区域霸主"
			}
			parts = append(parts, fmt.Sprintf("讨平%s", name))
		case ObjectiveCollectItem:
			parts = append(parts, fmt.Sprintf("采集 %d 份%s", o.Required, itemDisplayName(o.Target)))
		case ObjectiveReachZone:
			parts = append(parts, "前往一片相邻的天地")
		}
	}
	if len(parts) == 0 {
		return "完成一桩委托"
	}
	return strings.Join(parts, "；")
}

// genderDisplayZH 把 male/female 翻成中文。
func genderDisplayZH(g string) string {
	switch strings.ToLower(strings.TrimSpace(g)) {
	case "female":
		return "女"
	case "male":
		return "男"
	default:
		return "不详"
	}
}

// zoneKindDisplayZH 把区域 kind 翻成中文。
func zoneKindDisplayZH(k world.ZoneKind) string {
	switch k {
	case world.ZoneCapital:
		return "阵营主城区"
	case world.ZoneWild:
		return "阵营野外区"
	case world.ZoneStarter:
		return "中立新手区"
	default:
		return "未知区域"
	}
}

// questTypeDisplayZH 把任务类型翻成中文。
func questTypeDisplayZH(t QuestType) string {
	switch t {
	case QuestTypeSlay:
		return "讨伐"
	case QuestTypeCollect:
		return "收集"
	case QuestTypeExplore:
		return "探索"
	default:
		return "委托"
	}
}

// charterRedlineTexts 取宪章红线原文列表（去空白）。
func charterRedlineTexts(charter OfflineCharter) []string {
	out := make([]string, 0, len(charter.Redlines))
	for _, r := range charter.Redlines {
		if t := strings.TrimSpace(r.Text); t != "" {
			out = append(out, t)
		}
	}
	return out
}
