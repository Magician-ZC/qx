package session

// 文件说明：生成单位身份叙事与战报文本，维护叙事 schema、插图资源映射及批量降级策略。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// unitIdentityNarrativePayload 结构体用于承载该模块的核心数据。
type unitIdentityNarrativePayload struct {
	Biography        string `json:"biography"`
	RecruitmentPitch string `json:"recruitment_pitch"`
}

// battleReportPayload 结构体用于承载该模块的核心数据。
type battleReportPayload struct {
	Title              string `json:"title,omitempty"`
	Report             string `json:"report"`
	Memory             string `json:"memory,omitempty"`
	IllustrationPrompt string `json:"illustration_prompt,omitempty"`
}

var unitIdentityNarrativeSchema = []byte(`{
  "type":"object",
  "properties":{
    "biography":{"type":"string","minLength":1},
    "recruitment_pitch":{"type":"string","minLength":1}
  },
  "required":["biography","recruitment_pitch"],
  "additionalProperties":false
}`)

var battleReportSchema = []byte(`{
  "type":"object",
  "properties":{
    "title":{"type":"string"},
    "report":{"type":"string","minLength":80},
    "memory":{"type":"string"},
    "illustration_prompt":{"type":"string"}
  },
  "required":["report"],
  "additionalProperties":false
}`)

var battleReportIllustrationAssets = map[world.TerrainID]string{
	world.TerrainPlains:      "/unciv/terrain/plains.png",
	world.TerrainForest:      "/unciv/terrain/forest.png",
	world.TerrainMountain:    "/unciv/terrain/mountain.png",
	world.TerrainRiver:       "/unciv/terrain/river.png",
	world.TerrainRiverValley: "/unciv/terrain/river_valley.png",
	world.TerrainGrassland:   "/unciv/terrain/grassland.png",
	world.TerrainDesert:      "/unciv/terrain/desert.png",
	world.TerrainSwamp:       "/unciv/terrain/swamp.png",
	world.TerrainRuins:       "/unciv/terrain/ruins.png",
	world.TerrainVillage:     "/unciv/terrain/village.png",
	world.TerrainCity:        "/unciv/terrain/city.png",
	world.TerrainSnowfield:   "/unciv/terrain/snowfield.png",
	world.TerrainRoad:        "/unciv/terrain/road.png",
}

// enrichUnitIdentityNarrativeBestEffort 为单个单位补全传记与招募词。
// 若模型失败则回退到本地模板，不中断主流程。
func (service *Service) enrichUnitIdentityNarrativeBestEffort(
	ctx context.Context,
	state *State,
	record *unit.Record,
) {
	_ = state
	if service == nil || record == nil {
		return
	}
	if strings.TrimSpace(record.Identity.Biography) != "" && strings.TrimSpace(record.Identity.RecruitmentPitch) != "" {
		return
	}

	payload, _, _, err := service.generateUnitIdentityNarrative(ctx, record)
	if err != nil {
		record.Identity.Biography = fallbackUnitBiography(*record)
		record.Identity.RecruitmentPitch = fallbackRecruitmentPitch(*record)
		return
	}
	record.Identity.Biography = payload.Biography
	record.Identity.RecruitmentPitch = payload.RecruitmentPitch
}

// enrichUnitIdentityNarrativesBatchBestEffort 批量补全单位身份叙事。
// 优先走批量接口，降级时逐个请求并应用本地回退。
func (service *Service) enrichUnitIdentityNarrativesBatchBestEffort(
	ctx context.Context,
	records []*unit.Record,
) {
	if service == nil || len(records) == 0 {
		return
	}

	type unitIdentityPlan struct {
		key          string
		record       *unit.Record
		systemPrompt string
		userPrompt   string
		request      ai.CompletionRequest
	}

	plans := make([]unitIdentityPlan, 0, len(records))
	for index, record := range records {
		if record == nil {
			continue
		}
		if strings.TrimSpace(record.Identity.Biography) != "" && strings.TrimSpace(record.Identity.RecruitmentPitch) != "" {
			continue
		}
		systemPrompt, userPrompt, request := buildUnitIdentityNarrativeRequest(*record)
		key := strings.TrimSpace(record.ID)
		if key == "" {
			key = fmt.Sprintf("record-%d", index)
		}
		plans = append(plans, unitIdentityPlan{
			key:          key,
			record:       record,
			systemPrompt: systemPrompt,
			userPrompt:   userPrompt,
			request:      request,
		})
	}
	if len(plans) == 0 {
		return
	}

	batcher, ok := service.llm.(batchCompletionClient)
	if !ok || batcher == nil || len(plans) == 1 {
		for _, plan := range plans {
			payload, _, _, err := service.generateUnitIdentityNarrative(ctx, plan.record)
			if err != nil {
				plan.record.Identity.Biography = fallbackUnitBiography(*plan.record)
				plan.record.Identity.RecruitmentPitch = fallbackRecruitmentPitch(*plan.record)
				continue
			}
			plan.record.Identity.Biography = payload.Biography
			plan.record.Identity.RecruitmentPitch = payload.RecruitmentPitch
		}
		return
	}

	planByKey := make(map[string]unitIdentityPlan, len(plans))
	batchRequests := make([]ai.BatchRequest, 0, len(plans))
	for _, plan := range plans {
		key := plan.key
		if _, exists := planByKey[key]; exists {
			key = fmt.Sprintf("%s#%d", key, len(planByKey))
		}
		plan.key = key
		planByKey[key] = plan
		batchRequests = append(batchRequests, ai.BatchRequest{
			Key:     key,
			Request: plan.request,
		})
	}

	results := batcher.GenerateJSONBatch(ctx, batchRequests, ai.BatchOptions{
		MaxConcurrency: 6,
	})
	handled := make(map[string]struct{}, len(results))
	for _, batchResult := range results {
		plan, found := planByKey[batchResult.Key]
		if !found || plan.record == nil {
			continue
		}
		handled[batchResult.Key] = struct{}{}
		if batchResult.Err != nil {
			plan.record.Identity.Biography = fallbackUnitBiography(*plan.record)
			plan.record.Identity.RecruitmentPitch = fallbackRecruitmentPitch(*plan.record)
			continue
		}
		payload, err := parseUnitIdentityNarrativePayload(batchResult.Result)
		if err != nil {
			plan.record.Identity.Biography = fallbackUnitBiography(*plan.record)
			plan.record.Identity.RecruitmentPitch = fallbackRecruitmentPitch(*plan.record)
			continue
		}
		plan.record.Identity.Biography = payload.Biography
		plan.record.Identity.RecruitmentPitch = payload.RecruitmentPitch
	}
	for key, plan := range planByKey {
		if _, ok := handled[key]; ok || plan.record == nil {
			continue
		}
		plan.record.Identity.Biography = fallbackUnitBiography(*plan.record)
		plan.record.Identity.RecruitmentPitch = fallbackRecruitmentPitch(*plan.record)
	}
}

// generateUnitIdentityNarrative 调用 LLM 生成单位 biography + recruitment_pitch。
// 同时返回交互审计信息，便于后续落盘。
func (service *Service) generateUnitIdentityNarrative(
	ctx context.Context,
	record *unit.Record,
) (unitIdentityNarrativePayload, ai.CompletionResult, LLMInteraction, error) {
	if record == nil {
		return unitIdentityNarrativePayload{}, ai.CompletionResult{}, LLMInteraction{}, fmt.Errorf("record is nil")
	}
	state := State{
		TurnState: turnsFallbackState(),
	}
	systemPrompt, userPrompt, request := buildUnitIdentityNarrativeRequest(*record)

	if service.llm == nil {
		err := fmt.Errorf("llm client is disabled")
		result := ai.CompletionResult{
			Debug: ai.CompletionDebug{FallbackCause: err.Error()},
		}
		return unitIdentityNarrativePayload{}, result, buildLLMInteraction(state, record.ID, "unit_profile", "", systemPrompt, userPrompt, result, err.Error()), err
	}

	result, err := service.llm.GenerateJSON(ctx, request)
	if err != nil {
		return unitIdentityNarrativePayload{}, result, buildLLMInteraction(state, record.ID, "unit_profile", "", systemPrompt, userPrompt, result, err.Error()), err
	}

	payload, err := parseUnitIdentityNarrativePayload(result)
	if err != nil {
		cause := err.Error()
		return unitIdentityNarrativePayload{}, result, buildLLMInteraction(state, record.ID, "unit_profile", "", systemPrompt, userPrompt, result, cause), err
	}

	return payload, result, buildLLMInteraction(state, record.ID, "unit_profile", payload.RecruitmentPitch, systemPrompt, userPrompt, result, ""), nil
}

// buildUnitIdentityNarrativeRequest 组装单位身份叙事请求（三元组：system、user、request）。
func buildUnitIdentityNarrativeRequest(record unit.Record) (string, string, ai.CompletionRequest) {
	systemPrompt := fmt.Sprintf(
		"你是《群像》的单位设定生成器。请为单位 %s 生成两段文本：1) biography：约 200 字第三人称人物传记；2) recruitment_pitch：单位第一人称招募词。姓名/外号、生平、招募词必须和性格向量互相解释：勇敢或激进的人说话更冲，谨慎或稳定的人更克制，社交高的人更会来事，正直或忠诚高的人更可靠，野心高的人更想证明自己。不要把单位写成玩家遥控器。只返回 JSON。",
		record.DisplayName(),
	)
	userPrompt := buildUnitIdentityNarrativePrompt(record)
	request := ai.CompletionRequest{
		Task:           ai.TaskBackstory,
		SchemaName:     "session_unit_identity_narrative",
		ResponseSchema: unitIdentityNarrativeSchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.72,
		MaxTokens:      340,
	}
	return systemPrompt, userPrompt, request
}

// parseUnitIdentityNarrativePayload 解析并校验单位身份叙事结果。
// 会做字段裁剪并保证 biography 与 recruitment_pitch 均非空。
func parseUnitIdentityNarrativePayload(result ai.CompletionResult) (unitIdentityNarrativePayload, error) {
	var payload unitIdentityNarrativePayload
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		return unitIdentityNarrativePayload{}, fmt.Errorf("decode unit identity narrative payload: %w", err)
	}
	payload.Biography = limitTextRunes(strings.TrimSpace(payload.Biography), 200)
	payload.RecruitmentPitch = limitTextRunes(strings.TrimSpace(payload.RecruitmentPitch), 64)
	if payload.Biography == "" || payload.RecruitmentPitch == "" {
		return unitIdentityNarrativePayload{}, fmt.Errorf("unit identity narrative is incomplete")
	}
	return payload, nil
}

// buildUnitIdentityNarrativePrompt 构建单位身份叙事的用户提示词。
func buildUnitIdentityNarrativePrompt(record unit.Record) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "单位资料: %s\n", describeUnit(record, nil))
	fmt.Fprintf(&builder, "性格向量: %s\n", summarizeActorPersonality(record))
	fmt.Fprintf(&builder, "背包摘要: %s\n", summarizeInventoryBrief(record))
	fmt.Fprintln(&builder, "输出要求:")
	fmt.Fprintln(&builder, "1. biography 为 160-220 字，偏写实，不要空洞形容词堆砌。")
	fmt.Fprintln(&builder, "2. recruitment_pitch 为单位第一人称，18-48 字。")
	fmt.Fprintln(&builder, "3. 两段都要体现：该单位会基于环境、性格、记忆自主决策。")
	fmt.Fprintln(&builder, "4. 生平必须解释姓名/外号与 personality 的关系，例如为何谨慎、为何激进、为何可靠或为何爱冒险；招募词也要符合这些性格，不要和性格向量矛盾。")
	return builder.String()
}

// summarizeInventoryBrief 生成单位背包简摘要，用于叙事提示词上下文。
func summarizeInventoryBrief(record unit.Record) string {
	items := make([]string, 0, len(record.Inventory.Backpack))
	for _, stack := range record.Inventory.Backpack {
		if stack.ItemID == "" || stack.Quantity <= 0 {
			continue
		}
		items = append(items, fmt.Sprintf("%s x%d（%s）", displayItemName(stack.ItemID), stack.Quantity, formatItemStackEffect(stack)))
	}
	if len(items) == 0 {
		return "无"
	}
	if len(items) > 4 {
		items = items[:4]
	}
	return strings.Join(items, " / ")
}

// fallbackUnitBiography 在模型不可用时生成默认人物传记。
func fallbackUnitBiography(record unit.Record) string {
	temperament := "谨慎稳健"
	switch {
	case record.Personality.Aggression > 0.72:
		temperament = "锋线激进"
	case record.Personality.Prudence > 0.72:
		temperament = "稳守审慎"
	case record.Personality.Sociability > 0.72:
		temperament = "善于协作"
	}
	summary := fmt.Sprintf(
		"%s 出身边地行伍，长期在补给紧张的战线磨炼。%s 的作风偏 %s，遇事先看地形与队友状态，再结合自己的记忆做判断。无论是进食、调拨、交易还是交战，他都倾向于先评估风险再执行，不靠玩家逐项遥控。",
		record.DisplayName(),
		record.DisplayName(),
		temperament,
	)
	return limitTextRunes(summary, 200)
}

// fallbackRecruitmentPitch 在模型不可用时生成默认第一人称招募词。
func fallbackRecruitmentPitch(record unit.Record) string {
	line := fmt.Sprintf("我叫%s，把局势看清再出手，交给我就行。", record.DisplayName())
	return limitTextRunes(line, 64)
}

// recordBattleReportBestEffort 记录本回合战报。
// 包括旁白人选择、模型生成/回退、入库、日志摘要和参与者记忆写入。
func (service *Service) recordBattleReportBestEffort(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
) {
	if service == nil || state == nil || len(byID) == 0 {
		return
	}
	if hasBattleReportForTurn(*state, state.TurnState.Turn) {
		return
	}

	narrator := selectBattleReportNarrator(*state, byID)
	if narrator == nil {
		return
	}
	payload, result, interaction, err := service.generateBattleReport(ctx, *state, byID, *narrator)
	appendLLMInteraction(state, interaction)
	if err != nil {
		payload = fallbackBattleReportPayload(*state, byID, *narrator)
	}

	report := normalizeBattleReportPayload(payload, *state, byID, *narrator)
	entry := BattleReport{
		ID:                 uuid.NewString(),
		Turn:               state.TurnState.Turn,
		Phase:              state.TurnState.Phase,
		NarratorUnitID:     narrator.ID,
		Narrator:           narrator.DisplayName(),
		Title:              report.Title,
		Content:            report.Report,
		IllustrationPrompt: report.IllustrationPrompt,
		IllustrationURL:    resolveBattleReportIllustrationURL(*state, *narrator, report),
		Memory:             report.Memory,
		CreatedAt:          time.Now().UTC(),
		Provider:           result.Provider,
		Model:              result.Model,
		UsedFallback:       result.UsedFallback || err != nil,
	}
	state.BattleReports = append(state.BattleReports, entry)
	if len(state.BattleReports) > maxBattleReports {
		state.BattleReports = state.BattleReports[len(state.BattleReports)-maxBattleReports:]
	}

	headline := entry.Title
	if strings.TrimSpace(headline) == "" {
		headline = fmt.Sprintf("T%d 战场纪事", state.TurnState.Turn)
	}
	appendLog(
		state,
		"battle_report",
		fmt.Sprintf("%s（%s视角）: %s", headline, narrator.DisplayName(), limitTextRunes(entry.Content, 72)),
		narrator.ID,
		"",
	)

	participants := collectTurnParticipantIDs(*state, byID)
	if len(participants) == 0 {
		participants = []string{narrator.ID}
	}
	for _, unitID := range participants {
		record := byID[unitID]
		if record == nil {
			continue
		}
		memory := fmt.Sprintf("我记下第%d回合战况。", state.TurnState.Turn)
		if unitID == narrator.ID && strings.TrimSpace(entry.Memory) != "" {
			memory = entry.Memory
		}
		service.rememberUnitBestEffort(ctx, record, state.TurnState.Turn, memory)
	}
}

// generateBattleReport 调用 LLM 生成“单位第一人称回合战报”。
// 当预算护栏触发时直接返回成本可控的本地回退内容。
func (service *Service) generateBattleReport(
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	narrator unit.Record,
) (battleReportPayload, ai.CompletionResult, LLMInteraction, error) {
	systemPrompt := fmt.Sprintf(
		"你是《群像》里的单位 %s。请用第一人称写一段本回合战报，长度 150-300 字，强调你如何依据环境、性格和记忆自主行动。只返回 JSON。",
		narrator.DisplayName(),
	)
	userPrompt := buildBattleReportPrompt(service, ctx, state, byID, narrator)
	if llmBudgetGuardrailActive(state) {
		payload := fallbackBattleReportPayload(state, byID, narrator)
		result := budgetGuardrailResult(state)
		summary := payload.Title
		if strings.TrimSpace(summary) == "" {
			summary = limitTextRunes(payload.Report, 24)
		}
		return payload, result, buildLLMInteraction(state, narrator.ID, "battle_report", summary, systemPrompt, userPrompt, result, ""), nil
	}

	if service.llm == nil {
		err := fmt.Errorf("llm client is disabled")
		result := ai.CompletionResult{
			Debug: ai.CompletionDebug{FallbackCause: err.Error()},
		}
		return battleReportPayload{}, result, buildLLMInteraction(state, narrator.ID, "battle_report", "", systemPrompt, userPrompt, result, err.Error()), err
	}

	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskBattleReport,
		SchemaName:     "session_turn_battle_report",
		ResponseSchema: battleReportSchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.72,
		MaxTokens:      460,
		Timeout:        60 * time.Second,
	})
	if err != nil {
		return battleReportPayload{}, result, buildLLMInteraction(state, narrator.ID, "battle_report", "", systemPrompt, userPrompt, result, err.Error()), err
	}

	var payload battleReportPayload
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		cause := fmt.Sprintf("decode battle report payload: %v", err)
		return battleReportPayload{}, result, buildLLMInteraction(state, narrator.ID, "battle_report", "", systemPrompt, userPrompt, result, cause), fmt.Errorf("%s", cause)
	}
	normalized := normalizeBattleReportPayload(payload, state, byID, narrator)
	if strings.TrimSpace(normalized.Report) == "" {
		cause := "battle report is empty"
		return battleReportPayload{}, result, buildLLMInteraction(state, narrator.ID, "battle_report", "", systemPrompt, userPrompt, result, cause), fmt.Errorf("%s", cause)
	}
	summary := normalized.Title
	if strings.TrimSpace(summary) == "" {
		summary = limitTextRunes(normalized.Report, 24)
	}
	return normalized, result, buildLLMInteraction(state, narrator.ID, "battle_report", summary, systemPrompt, userPrompt, result, ""), nil
}

// buildBattleReportPrompt 组装战报提示词，汇入人格、环境、关系、记忆与回合行为摘要。
func buildBattleReportPrompt(
	service *Service,
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	narrator unit.Record,
) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "当前回合: %d\n", state.TurnState.Turn)
	fmt.Fprintf(&builder, "阶段: %s\n", state.TurnState.Phase)
	fmt.Fprintf(&builder, "全局方针: %s\n", directiveForUnit(state, narrator.ID, narrator.FactionID))
	fmt.Fprintf(&builder, "你的资料: %s\n", describeUnit(narrator, nil))
	fmt.Fprintf(&builder, "你的性格: %s\n", summarizeActorPersonality(narrator))
	fmt.Fprintf(&builder, "你的环境: %s\n", summarizeImmediateEnvironment(state, byID, &narrator))
	fmt.Fprintf(&builder, "你的关系网:\n%s\n", service.relationSummaryForPrompt(ctx, byID, narrator, 4))
	fmt.Fprintf(&builder, "你的记忆:\n%s\n", service.memorySummaryForPrompt(ctx, state, byID, narrator, 6))
	fmt.Fprintf(&builder, "本回合你的行动:\n%s\n", summarizeTurnDecisions(state, narrator.ID, byID, 6))
	fmt.Fprintf(&builder, "本回合关键事件:\n%s\n", summarizeTurnLogs(state, 10))
	fmt.Fprintln(&builder, "输出要求:")
	fmt.Fprintln(&builder, "1. report 为第一人称 150-300 字。")
	fmt.Fprintln(&builder, "2. title 为可选短标题（<=20字）。")
	fmt.Fprintln(&builder, "3. memory 为可选第一人称短记忆（<=18字）。")
	fmt.Fprintln(&builder, "4. illustration_prompt 可选，用于后续配图（<=80字）。")
	return builder.String()
}

// normalizeBattleReportPayload 清洗战报输出并限制标题/正文/记忆/配图提示长度。
func normalizeBattleReportPayload(
	payload battleReportPayload,
	state State,
	byID map[string]*unit.Record,
	narrator unit.Record,
) battleReportPayload {
	payload.Title = limitTextRunes(strings.TrimSpace(payload.Title), 20)
	payload.Report = normalizeBattleReportLength(strings.TrimSpace(payload.Report), fallbackBattleReportPayload(state, byID, narrator).Report)
	payload.Memory = limitTextRunes(strings.TrimSpace(payload.Memory), 18)
	payload.IllustrationPrompt = limitTextRunes(strings.TrimSpace(payload.IllustrationPrompt), 80)
	if payload.Memory == "" {
		payload.Memory = limitTextRunes(fmt.Sprintf("我记下了第%d回合。", state.TurnState.Turn), 18)
	}
	return payload
}

// resolveBattleReportIllustrationURL 为战报选择配图素材 URL。
// 依据旁白位置地形、天气和文本信号推断最终地形主题。
func resolveBattleReportIllustrationURL(
	state State,
	narrator unit.Record,
	payload battleReportPayload,
) string {
	baseTerrain := terrainAt(state.Map, world.Coord{
		Q: narrator.Status.PositionQ,
		R: narrator.Status.PositionR,
	})
	terrain := chooseBattleReportIllustrationTerrain(
		state.Weather.Type,
		baseTerrain,
		payload.IllustrationPrompt,
		payload.Report,
	)
	if url, ok := battleReportIllustrationAssets[terrain]; ok {
		return url
	}
	return battleReportIllustrationAssets[world.TerrainPlains]
}

// chooseBattleReportIllustrationTerrain 根据天气与文本语义选择最贴合的战报配图地形。
func chooseBattleReportIllustrationTerrain(
	weather WeatherType,
	base world.TerrainID,
	illustrationPrompt string,
	report string,
) world.TerrainID {
	if base == "" {
		base = world.TerrainPlains
	}

	textSignal := battleReportTerrainFromText(illustrationPrompt + "\n" + report)
	if textSignal != "" {
		base = textSignal
	}

	switch weather {
	case WeatherRainy:
		switch base {
		case world.TerrainPlains, world.TerrainGrassland, world.TerrainRoad, world.TerrainDesert:
			base = world.TerrainRiverValley
		case world.TerrainMountain:
			base = world.TerrainRiver
		}
	case WeatherFoggy:
		switch base {
		case world.TerrainPlains, world.TerrainGrassland, world.TerrainRoad, world.TerrainDesert:
			base = world.TerrainForest
		}
	case WeatherWindy:
		switch base {
		case world.TerrainPlains, world.TerrainRoad, world.TerrainRiverValley:
			base = world.TerrainGrassland
		}
	}

	if _, ok := battleReportIllustrationAssets[base]; ok {
		return base
	}
	return world.TerrainPlains
}

// battleReportTerrainFromText 从自由文本里抽取地形信号词。
func battleReportTerrainFromText(text string) world.TerrainID {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return ""
	}

	switch {
	case containsAnyText(text, "河谷", "谷地", "river_valley", "river valley", "valley"):
		return world.TerrainRiverValley
	case containsAnyText(text, "雪原", "雪地", "冰原", "snowfield", "snow"):
		return world.TerrainSnowfield
	case containsAnyText(text, "废墟", "遗迹", "ruins", "ruin"):
		return world.TerrainRuins
	case containsAnyText(text, "村庄", "村落", "village"):
		return world.TerrainVillage
	case containsAnyText(text, "城池", "城市", "city"):
		return world.TerrainCity
	case containsAnyText(text, "沼泽", "泥潭", "swamp", "bog"):
		return world.TerrainSwamp
	case containsAnyText(text, "沙漠", "沙地", "desert", "dune"):
		return world.TerrainDesert
	case containsAnyText(text, "河道", "河流", "溪流", "river", "stream"):
		return world.TerrainRiver
	case containsAnyText(text, "山地", "山岭", "高地", "mountain", "ridge"):
		return world.TerrainMountain
	case containsAnyText(text, "森林", "林地", "树林", "forest", "woods"):
		return world.TerrainForest
	case containsAnyText(text, "草原", "草地", "grassland", "prairie"):
		return world.TerrainGrassland
	case containsAnyText(text, "道路", "路口", "路面", "road", "trail"):
		return world.TerrainRoad
	case containsAnyText(text, "平原", "平地", "plains"):
		return world.TerrainPlains
	default:
		return ""
	}
}

// containsAnyText 判断文本是否包含任意候选术语。
func containsAnyText(text string, terms ...string) bool {
	for _, term := range terms {
		if strings.Contains(text, term) {
			return true
		}
	}
	return false
}

// normalizeBattleReportLength 约束战报正文长度在 150-300 字区间，并在过短时补全尾句。
func normalizeBattleReportLength(content string, fallback string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		content = strings.TrimSpace(fallback)
	}
	runes := []rune(content)
	if len(runes) < 150 {
		appendix := "我知道下一回合仍要靠我们自己判断补给、位置和节奏，不会把命运交给一句口号。"
		content = strings.TrimSpace(content + appendix)
		runes = []rune(content)
	}
	if len(runes) > 300 {
		content = string(runes[:300])
	}
	return strings.TrimSpace(content)
}

// fallbackBattleReportPayload 生成无需模型即可使用的默认战报内容。
func fallbackBattleReportPayload(state State, byID map[string]*unit.Record, narrator unit.Record) battleReportPayload {
	report := fmt.Sprintf(
		"我是%s。第%d回合执行阶段里，我先盯住地形和队伍状态，再决定推进还是收缩。敌人一压上来，我没有等玩家逐项指挥，而是按记忆里最危险的方向先做应对：该守就守、该援就援、该换位就换位。回头看这一轮，我们靠的是彼此之间的默契和临场判断，不是硬拼蛮冲。我把这些写下来，是提醒自己下回合依旧要先看环境、再看人心，最后才是出手时机。",
		narrator.DisplayName(),
		state.TurnState.Turn,
	)
	return battleReportPayload{
		Title:              fmt.Sprintf("第%d回合纪事", state.TurnState.Turn),
		Report:             normalizeBattleReportLength(report, report),
		Memory:             limitTextRunes(fmt.Sprintf("我记下第%d回合战况。", state.TurnState.Turn), 18),
		IllustrationPrompt: limitTextRunes(fmt.Sprintf("六边格战场，%s 第一人称回望第%d回合战斗。", narrator.DisplayName(), state.TurnState.Turn), 80),
	}
}

// selectBattleReportNarrator 选择本回合战报叙述者。
// 优先行动贡献高且可战斗单位，其次回退到双方首个可战斗单位。
func selectBattleReportNarrator(state State, byID map[string]*unit.Record) *unit.Record {
	actionCount := map[string]int{}
	for _, trace := range state.DecisionTraces {
		if trace.Turn != state.TurnState.Turn {
			continue
		}
		actionCount[trace.UnitID]++
	}

	type candidate struct {
		record *unit.Record
		score  int
	}
	candidates := make([]candidate, 0, len(actionCount))
	for unitID, score := range actionCount {
		record := byID[unitID]
		if record == nil {
			continue
		}
		if record.FactionID == state.PlayerFactionID {
			score += 2
		}
		if isBattleReady(*record) {
			score += 1
		}
		candidates = append(candidates, candidate{record: record, score: score})
	}
	sort.Slice(candidates, func(i int, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].record.ID < candidates[j].record.ID
		}
		return candidates[i].score > candidates[j].score
	})
	if len(candidates) > 0 {
		return candidates[0].record
	}

	for _, unitID := range state.PlayerUnitIDs {
		record := byID[unitID]
		if record != nil && isBattleReady(*record) {
			return record
		}
	}
	for _, unitID := range state.EnemyUnitIDs {
		record := byID[unitID]
		if record != nil && isBattleReady(*record) {
			return record
		}
	}
	return nil
}

// collectTurnParticipantIDs 收集本回合有决策轨迹的参与单位 ID 列表。
func collectTurnParticipantIDs(state State, byID map[string]*unit.Record) []string {
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(byID))
	for _, trace := range state.DecisionTraces {
		if trace.Turn != state.TurnState.Turn {
			continue
		}
		if _, ok := byID[trace.UnitID]; !ok {
			continue
		}
		if _, ok := seen[trace.UnitID]; ok {
			continue
		}
		seen[trace.UnitID] = struct{}{}
		ids = append(ids, trace.UnitID)
	}
	return ids
}

// hasBattleReportForTurn 判断指定回合是否已生成过战报，避免重复写入。
func hasBattleReportForTurn(state State, turn int) bool {
	for i := len(state.BattleReports) - 1; i >= 0; i-- {
		if state.BattleReports[i].Turn == turn {
			return true
		}
		if state.BattleReports[i].Turn < turn {
			break
		}
	}
	return false
}

// summarizeTurnDecisions 汇总某单位本回合决策轨迹，生成按时间顺序的文本列表。
func summarizeTurnDecisions(state State, unitID string, byID map[string]*unit.Record, limit int) string {
	if limit <= 0 {
		limit = 6
	}
	lines := make([]string, 0, limit)
	for i := len(state.DecisionTraces) - 1; i >= 0 && len(lines) < limit; i-- {
		trace := state.DecisionTraces[i]
		if trace.Turn != state.TurnState.Turn || trace.UnitID != unitID {
			continue
		}
		lines = append(lines, formatNarrativeDecisionLine(trace, byID))
	}
	if len(lines) == 0 {
		return "无"
	}
	for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
		lines[left], lines[right] = lines[right], lines[left]
	}
	return strings.Join(lines, "\n")
}

// summarizeTurnLogs 汇总本回合关键日志，供战报与提示词消费。
func summarizeTurnLogs(state State, limit int) string {
	if limit <= 0 {
		limit = 8
	}
	lines := make([]string, 0, limit)
	for i := len(state.Logs) - 1; i >= 0 && len(lines) < limit; i-- {
		entry := state.Logs[i]
		if entry.Turn != state.TurnState.Turn {
			continue
		}
		lines = append(lines, fmt.Sprintf("[%s] %s", entry.Kind, entry.Message))
	}
	if len(lines) == 0 {
		return "无"
	}
	for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
		lines[left], lines[right] = lines[right], lines[left]
	}
	return strings.Join(lines, "\n")
}

// formatNarrativeDecisionLine 把一条决策轨迹格式化为可读叙事行。
func formatNarrativeDecisionLine(trace DecisionTrace, byID map[string]*unit.Record) string {
	switch trace.Action {
	case DecisionActionAttack, DecisionActionCharge, DecisionActionHeavyAttack, DecisionActionAssist:
		targetName := trace.TargetUnitID
		if target, ok := byID[trace.TargetUnitID]; ok && target != nil {
			targetName = target.DisplayName()
		} else if trace.StructureType != "" {
			targetName = structureDisplayName(StructureType(trace.StructureType))
		}
		return fmt.Sprintf("%s -> %s (%s)", trace.Action, targetName, trace.Reasoning)
	case DecisionActionDialogue:
		targetName := trace.TargetUnitID
		if target, ok := byID[trace.TargetUnitID]; ok && target != nil {
			targetName = target.DisplayName()
		}
		return fmt.Sprintf("dialogue -> %s (%s)", targetName, trace.Reasoning)
	case DecisionActionTrade:
		targetName := trace.TargetUnitID
		if target, ok := byID[trace.TargetUnitID]; ok && target != nil {
			targetName = target.DisplayName()
		}
		switch trace.TradeKind {
		case string(TradeActionKindGift):
			return fmt.Sprintf("trade gift %s -> %s (%s)", trace.ItemID, targetName, trace.Reasoning)
		case string(TradeActionKindGold):
			return fmt.Sprintf("trade gold %d -> %s (%s)", trace.GoldAmount, targetName, trace.Reasoning)
		case string(TradeActionKindSell):
			return fmt.Sprintf("trade sell %s for %d -> %s (%s)", trace.ItemID, trace.Price, targetName, trace.Reasoning)
		default:
			return fmt.Sprintf("trade -> %s (%s)", targetName, trace.Reasoning)
		}
	case DecisionActionRomance, DecisionActionFamily:
		targetName := trace.TargetUnitID
		if target, ok := byID[trace.TargetUnitID]; ok && target != nil {
			targetName = target.DisplayName()
		}
		return fmt.Sprintf("%s -> %s (%s)", trace.Action, targetName, trace.Reasoning)
	case DecisionActionMove:
		return fmt.Sprintf("move -> %d,%d (%s)", trace.TargetQ, trace.TargetR, trace.Reasoning)
	case DecisionActionSkill:
		return fmt.Sprintf("skill %s (%s)", trace.SkillID, trace.Reasoning)
	case DecisionActionGather:
		return fmt.Sprintf("gather %s (%s)", trace.Activity, trace.Reasoning)
	case DecisionActionBuild:
		return fmt.Sprintf("build %s (%s)", trace.StructureType, trace.Reasoning)
	case DecisionActionDemolish:
		return fmt.Sprintf("demolish %s (%s)", trace.StructureType, trace.Reasoning)
	default:
		return fmt.Sprintf("%s (%s)", trace.Action, trace.Reasoning)
	}
}

// turnsFallbackState 返回叙事生成流程使用的默认回合状态。
func turnsFallbackState() turns.State {
	return turns.State{
		Turn:  1,
		Phase: turns.PhaseDeployment,
	}
}
