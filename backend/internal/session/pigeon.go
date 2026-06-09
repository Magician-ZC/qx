package session

// 文件说明：实现信鸽通信系统（发送决策、在途队列、拦截判定、送达结算及关系/记忆联动）。

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/item"
	"qunxiang/backend/internal/unit"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	basePigeonInterceptChance = 0.05
)

// pigeonChoicePayload 描述信鸽发送决策的结构化输出。
type pigeonChoicePayload struct {
	ShouldSend   bool   `json:"should_send"`
	TargetUnitID string `json:"target_unit_id,omitempty"`
	Message      string `json:"message,omitempty"`
	Bubble       string `json:"bubble,omitempty"`
	Memory       string `json:"memory,omitempty"`
	AttachItemID string `json:"attach_item_id,omitempty"`
	Reasoning    string `json:"reasoning"`
}

// pigeonTarget 描述一个可投递的信鸽目标单位。
type pigeonTarget struct {
	ID       string
	Name     string
	Distance int
}

var pigeonChoiceSchema = []byte(`{
  "type":"object",
  "properties":{
    "should_send":{"type":"boolean"},
    "target_unit_id":{"type":"string"},
    "message":{"type":"string"},
    "bubble":{"type":"string"},
    "memory":{"type":"string"},
    "attach_item_id":{"type":"string"},
    "reasoning":{"type":"string","minLength":1}
  },
  "required":["should_send","reasoning"],
  "additionalProperties":false
}`)

// resolvePigeonDispatches 在部署阶段评估并入队新的信鸽投递任务。
func (service *Service) resolvePigeonDispatches(ctx context.Context, state *State, units []unit.Record) error {
	if service == nil || state == nil || state.TurnState.Phase != "deployment" || state.Outcome != OutcomeOngoing {
		return nil
	}
	byID := mapRecordsByID(units)
	allIDs := append([]string{}, state.PlayerUnitIDs...)
	allIDs = append(allIDs, state.EnemyUnitIDs...)

	for _, senderID := range allIDs {
		sender := byID[senderID]
		if sender == nil || !isTradeReadyInState(*state, byID, *sender) || !hasBackpackItem(*sender, "carrier_pigeon") {
			continue
		}
		if !directiveSuggestsPigeon(*state, *sender) {
			continue
		}

		targets := candidatePigeonTargets(*state, byID, sender)
		if len(targets) == 0 {
			continue
		}
		attachable := candidatePigeonAttachItems(*sender)
		choice, result, interaction, err := service.generatePigeonChoice(ctx, *state, byID, *sender, targets, attachable)
		service.appendLLMInteractionWithSpend(ctx, state, interaction)
		if err != nil {
			appendLog(state, "pigeon_error", "我这回合先不放信鸽。", sender.ID, "")
			continue
		}
		if !choice.ShouldSend {
			continue
		}
		target := byID[choice.TargetUnitID]
		if target == nil || target.FactionID != sender.FactionID || !isBattleReady(*target) {
			appendLog(state, "pigeon_blocked", "我这回合没找到合适收件人。", sender.ID, choice.TargetUnitID)
			continue
		}
		if unit.HexDistance(sender.Status.PositionQ, sender.Status.PositionR, target.Status.PositionQ, target.Status.PositionR) <= 1 {
			appendLog(state, "pigeon_blocked", fmt.Sprintf("我和 %s 挨着，直接当面说。", target.DisplayName()), sender.ID, target.ID)
			continue
		}

		if err := unit.ConsumeBackpackItem(sender, "carrier_pigeon", 1); err != nil {
			appendLog(state, "pigeon_blocked", "我手里已经没信鸽了。", sender.ID, "")
			continue
		}

		attachItemID := ""
		if choice.AttachItemID != "" && choice.AttachItemID != "none" && choice.AttachItemID != "carrier_pigeon" && hasBackpackQuantity(*sender, choice.AttachItemID, 1) {
			if err := unit.ConsumeBackpackItem(sender, choice.AttachItemID, 1); err == nil {
				attachItemID = choice.AttachItemID
			}
		}
		if err := service.units.Save(ctx, *sender); err != nil {
			return err
		}

		dispatch := PigeonDispatch{
			ID:              uuid.NewString(),
			Turn:            state.TurnState.Turn,
			Phase:           state.TurnState.Phase,
			SenderUnitID:    sender.ID,
			ReceiverUnitID:  target.ID,
			Message:         choice.Message,
			AttachedItemID:  attachItemID,
			DeliverTurn:     state.TurnState.Turn + 1,
			InterceptChance: basePigeonInterceptChance,
			SentAt:          time.Now().UTC(),
		}
		state.PigeonQueue = append(state.PigeonQueue, dispatch)
		if len(state.PigeonQueue) > maxPigeonQueue {
			state.PigeonQueue = state.PigeonQueue[len(state.PigeonQueue)-maxPigeonQueue:]
		}

		appendAIDialogue(state, *sender, choice.Bubble, result)
		service.rememberUnitBestEffort(ctx, sender, state.TurnState.Turn, choice.Memory)
		message := strings.TrimSpace(firstNonEmptyText(choice.Bubble, choice.Reasoning, choice.Message, choice.Memory))
		if message == "" {
			message = fmt.Sprintf("我给 %s 放了封信。", target.DisplayName())
		}
		if attachItemID != "" {
			message = fmt.Sprintf("%s（随信带了%s）", message, displayItemName(attachItemID))
		}
		appendLog(state, "pigeon_send", message, sender.ID, target.ID)
	}

	return nil
}

// resolvePigeonDeliveries 结算到期信鸽：送达、拦截、附件与关系变化。
func (service *Service) resolvePigeonDeliveries(ctx context.Context, state *State, units []unit.Record) error {
	if service == nil || state == nil || len(state.PigeonQueue) == 0 {
		return nil
	}
	if units == nil {
		loaded, err := service.units.ListBySession(ctx, state.ID)
		if err != nil {
			return err
		}
		units = loaded
	}
	byID := mapRecordsByID(units)
	pending := make([]PigeonDispatch, 0, len(state.PigeonQueue))

	for _, dispatch := range state.PigeonQueue {
		if dispatch.DeliverTurn > state.TurnState.Turn {
			pending = append(pending, dispatch)
			continue
		}

		sender := byID[dispatch.SenderUnitID]
		receiver := byID[dispatch.ReceiverUnitID]
		if sender == nil || receiver == nil || !isBattleReady(*receiver) {
			appendLog(state, "pigeon_lost", "这封信鸽在途中失联了。", dispatch.SenderUnitID, dispatch.ReceiverUnitID)
			continue
		}

		if pigeonInterceptRoll(*state, dispatch) < dispatch.InterceptChance {
			interceptorFaction := opposingFaction(*state, sender.FactionID)
			appendLog(
				state,
				"pigeon_intercept",
				fmt.Sprintf("我的信鸽被%s拦下，没能送到 %s。", factionDisplayName(*state, interceptorFaction), receiver.DisplayName()),
				sender.ID,
				receiver.ID,
			)
			service.rememberUnitBestEffort(ctx, sender, state.TurnState.Turn, "我的信鸽被拦截了。")
			service.rememberUnitBestEffort(ctx, receiver, state.TurnState.Turn, fmt.Sprintf("我没等到%s的信。", sender.DisplayName()))
			senderDelta := relationDelta{
				Trust:     -0.10,
				Fear:      0.08,
				Affection: -0.04,
				Rivalry:   0.02,
			}
			receiverDelta := relationDelta{
				Trust:     -0.06,
				Fear:      0.04,
				Affection: -0.02,
				Rivalry:   0.00,
			}
			service.applyMutualRelationShiftBestEffort(ctx, state, sender, receiver, senderDelta, receiverDelta, "信鸽被拦截")
			continue
		}

		appendDialogue(state, DialogueMessage{
			ID:         uuid.NewString(),
			UnitID:     receiver.ID,
			Speaker:    sender.DisplayName(),
			Message:    dispatch.Message,
			Turn:       state.TurnState.Turn,
			Phase:      state.TurnState.Phase,
			OccurredAt: time.Now().UTC(),
		})
		appendLog(state, "pigeon_deliver", fmt.Sprintf("我收到 %s 的信：%s", sender.DisplayName(), dispatch.Message), sender.ID, receiver.ID)
		service.rememberUnitBestEffort(ctx, receiver, state.TurnState.Turn, fmt.Sprintf("我收到%s来信：%s", sender.DisplayName(), dispatch.Message))
		senderDelta := relationDelta{
			Trust:     0.34,
			Fear:      -0.06,
			Affection: 0.20,
			Rivalry:   -0.06,
		}
		receiverDelta := relationDelta{
			Trust:     0.52,
			Fear:      -0.12,
			Affection: 0.34,
			Rivalry:   -0.08,
		}
		service.applyMutualRelationShiftBestEffort(ctx, state, sender, receiver, senderDelta, receiverDelta, "远程传信成功")

		if dispatch.AttachedItemID != "" {
			if err := unit.AddBackpackItem(receiver, dispatch.AttachedItemID, 1); err != nil {
				appendLog(state, "pigeon_attachment_lost", fmt.Sprintf("我随信带的 %s 在路上丢了。", displayItemName(dispatch.AttachedItemID)), sender.ID, receiver.ID)
			} else {
				if err := service.units.Save(ctx, *receiver); err != nil {
					return err
				}
				appendLog(state, "pigeon_attachment", fmt.Sprintf("我还收到了 %s。", displayItemName(dispatch.AttachedItemID)), sender.ID, receiver.ID)
			}
		}
	}

	state.PigeonQueue = pending
	return nil
}

// generatePigeonChoice 调用 LLM 生成是否放飞信鸽及其内容决策。
func (service *Service) generatePigeonChoice(
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	sender unit.Record,
	targets []pigeonTarget,
	attachable []string,
) (pigeonChoicePayload, ai.CompletionResult, LLMInteraction, error) {
	systemPrompt := fmt.Sprintf(
		"你是《一念》中的单位 %s。你可以决定是否放飞信鸽给同阵营远处队友。玩家只给自然语言意图，真正发送与文本都由你自己决定。只能返回 JSON。",
		sender.DisplayName(),
	)
	promptTargets, targetAliasToID := aliasPigeonTargets(targets)
	userPrompt := buildPigeonPrompt(service, ctx, state, byID, sender, promptTargets, attachable)

	if service.llm == nil {
		err := fmt.Errorf("llm client is disabled")
		result := ai.CompletionResult{Debug: ai.CompletionDebug{FallbackCause: err.Error()}}
		return pigeonChoicePayload{}, result, buildLLMInteraction(state, sender.ID, "pigeon", "", systemPrompt, userPrompt, result, err.Error()), err
	}

	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskDowntime,
		Importance:     ai.ImportanceCheap, // 闲时信鸽=低 stakes（分tier路由 flag 开时走廉价档/短超时；默认关零影响）
		SchemaName:     "session_pigeon_choice",
		ResponseSchema: pigeonChoiceSchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.45,
		MaxTokens:      180,
		Timeout:        60 * time.Second,
	})
	if err != nil {
		return pigeonChoicePayload{}, result, buildLLMInteraction(state, sender.ID, "pigeon", "", systemPrompt, userPrompt, result, err.Error()), err
	}

	var payload pigeonChoicePayload
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		cause := fmt.Sprintf("decode pigeon choice payload: %v", err)
		return pigeonChoicePayload{}, result, buildLLMInteraction(state, sender.ID, "pigeon", "", systemPrompt, userPrompt, result, cause), fmt.Errorf("%s", cause)
	}
	payload = normalizePigeonChoice(payload)
	if realID, ok := targetAliasToID[payload.TargetUnitID]; ok {
		payload.TargetUnitID = realID
	}
	if payload.Reasoning == "" {
		cause := "pigeon choice reasoning must not be empty"
		return pigeonChoicePayload{}, result, buildLLMInteraction(state, sender.ID, "pigeon", "", systemPrompt, userPrompt, result, cause), fmt.Errorf("%s", cause)
	}
	if payload.ShouldSend && payload.TargetUnitID == "" {
		cause := "pigeon target unit_id is required when should_send=true"
		return pigeonChoicePayload{}, result, buildLLMInteraction(state, sender.ID, "pigeon", "", systemPrompt, userPrompt, result, cause), fmt.Errorf("%s", cause)
	}
	if payload.ShouldSend && payload.Message == "" {
		payload.Message = limitTextRunes(payload.Reasoning, 32)
	}

	summary := payload.Reasoning
	if payload.Bubble != "" {
		summary = payload.Bubble
	}
	return payload, result, buildLLMInteraction(state, sender.ID, "pigeon", summary, systemPrompt, userPrompt, result, ""), nil
}

// aliasPigeonTargets 为信鸽提示词生成不含 UUID 的目标编号。
func aliasPigeonTargets(targets []pigeonTarget) ([]pigeonTarget, map[string]string) {
	aliased := make([]pigeonTarget, 0, len(targets))
	mapping := make(map[string]string, len(targets))
	for index, target := range targets {
		alias := fmt.Sprintf("target_%02d", index+1)
		mapping[alias] = target.ID
		target.ID = alias
		aliased = append(aliased, target)
	}
	return aliased, mapping
}

// buildPigeonPrompt 构建信鸽决策任务提示词。
func buildPigeonPrompt(
	service *Service,
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	sender unit.Record,
	targets []pigeonTarget,
	attachable []string,
) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "当前回合: %d\n", state.TurnState.Turn)
	fmt.Fprintf(&builder, "当前阶段: %s\n", state.TurnState.Phase)
	fmt.Fprintf(&builder, "玩家自然语言指令上下文: %s\n", directiveForUnit(state, sender.ID, sender.FactionID))
	fmt.Fprintf(&builder, "你的资料: %s\n", describeUnit(sender, nil))
	fmt.Fprintf(&builder, "你的性格: %s\n", summarizeActorPersonality(sender))
	fmt.Fprintf(&builder, "你的环境摘要: %s\n", summarizeImmediateEnvironment(state, byID, &sender))
	fmt.Fprintf(&builder, "你的关系网:\n%s\n", service.relationSummaryForPrompt(ctx, byID, sender, 4))
	fmt.Fprintf(&builder, "你的记忆:\n%s\n", service.memorySummaryForPrompt(ctx, state, byID, sender, 6))
	fmt.Fprintln(&builder, "可发送目标(同阵营且非相邻):")
	for _, target := range targets {
		fmt.Fprintf(&builder, "- 编号=%s 名称=%s 距离=%d\n", target.ID, target.Name, target.Distance)
	}
	fmt.Fprintf(&builder, "可随信附带物品(可为空): %s\n", strings.Join(append([]string{"none"}, attachable...), ", "))
	fmt.Fprintln(&builder, "输出规则:")
	fmt.Fprintln(&builder, "1. should_send=false 时，可留空 target_unit_id/message/attach_item_id。")
	fmt.Fprintln(&builder, "2. should_send=true 时，target_unit_id 必须来自上面的编号；message 是一句第一人称短句(<=32字)。")
	fmt.Fprintln(&builder, "3. bubble <=16字，memory <=18字，且都要像你本人。")
	fmt.Fprintln(&builder, "4. attach_item_id 只能填 none 或可附带物品中的一个。")
	return builder.String()
}

// normalizePigeonChoice 规范化信鸽决策文本与字段兜底。
func normalizePigeonChoice(choice pigeonChoicePayload) pigeonChoicePayload {
	choice.TargetUnitID = strings.TrimSpace(choice.TargetUnitID)
	choice.Message = limitTextRunes(strings.TrimSpace(choice.Message), 32)
	choice.Bubble = limitTextRunes(strings.TrimSpace(choice.Bubble), 16)
	choice.Memory = limitTextRunes(strings.TrimSpace(choice.Memory), 18)
	choice.AttachItemID = strings.TrimSpace(choice.AttachItemID)
	if strings.EqualFold(choice.AttachItemID, "none") {
		choice.AttachItemID = ""
	}
	choice.Reasoning = strings.TrimSpace(choice.Reasoning)
	if choice.Memory == "" {
		switch {
		case choice.Bubble != "":
			choice.Memory = limitTextRunes(choice.Bubble, 18)
		case choice.Message != "":
			choice.Memory = limitTextRunes(choice.Message, 18)
		default:
			choice.Memory = limitTextRunes(choice.Reasoning, 18)
		}
	}
	if choice.Bubble == "" && choice.ShouldSend {
		choice.Bubble = limitTextRunes(choice.Message, 16)
	}
	return choice
}

// candidatePigeonTargets 列出同阵营且非相邻的可投递目标。
func candidatePigeonTargets(state State, byID map[string]*unit.Record, sender *unit.Record) []pigeonTarget {
	if sender == nil {
		return nil
	}
	targets := make([]pigeonTarget, 0, 4)
	for _, target := range byID {
		if target == nil || target.ID == sender.ID || target.FactionID != sender.FactionID || !isBattleReady(*target) {
			continue
		}
		distance := unit.HexDistance(sender.Status.PositionQ, sender.Status.PositionR, target.Status.PositionQ, target.Status.PositionR)
		if distance <= 1 {
			continue
		}
		targets = append(targets, pigeonTarget{
			ID:       target.ID,
			Name:     target.DisplayName(),
			Distance: distance,
		})
	}
	sort.Slice(targets, func(i int, j int) bool {
		if targets[i].Distance == targets[j].Distance {
			return targets[i].ID < targets[j].ID
		}
		return targets[i].Distance < targets[j].Distance
	})
	return targets
}

// candidatePigeonAttachItems 列出可随信附带的背包物品候选。
func candidatePigeonAttachItems(sender unit.Record) []string {
	seen := map[string]struct{}{}
	items := make([]string, 0, 4)
	for _, stack := range sender.Inventory.Backpack {
		if stack.ItemID == "" || stack.Quantity <= 0 || stack.ItemID == "carrier_pigeon" {
			continue
		}
		if _, ok := seen[stack.ItemID]; ok {
			continue
		}
		seen[stack.ItemID] = struct{}{}
		items = append(items, stack.ItemID)
	}
	sort.Strings(items)
	if len(items) > 6 {
		items = items[:6]
	}
	return items
}

// directiveSuggestsPigeon 判断当前方针是否建议触发远程传信。
func directiveSuggestsPigeon(state State, sender unit.Record) bool {
	context := strings.ToLower(strings.TrimSpace(directiveForUnit(state, sender.ID, sender.FactionID)))
	if containsAny(context, "信鸽", "传信", "通信", "联络", "远程", "通报", "汇报") {
		return true
	}
	return sender.Personality.Sociability >= 0.72 && sender.Personality.Prudence >= 0.55
}

// pigeonInterceptRoll 生成信鸽拦截结算的稳定随机值。
func pigeonInterceptRoll(state State, dispatch PigeonDispatch) float64 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(state.ID))
	_, _ = hasher.Write([]byte(fmt.Sprintf("%d", dispatch.DeliverTurn)))
	_, _ = hasher.Write([]byte(dispatch.SenderUnitID))
	_, _ = hasher.Write([]byte(dispatch.ReceiverUnitID))
	_, _ = hasher.Write([]byte(dispatch.Message))
	return float64(hasher.Sum32()%10000) / 10000
}

// opposingFaction 返回给定阵营的对立阵营 ID。
func opposingFaction(state State, factionID string) string {
	if factionID == state.PlayerFactionID {
		return state.EnemyFactionID
	}
	return state.PlayerFactionID
}

// factionDisplayName 返回阵营 ID 对应的展示名称。
func factionDisplayName(state State, factionID string) string {
	if factionID == state.PlayerFactionID {
		return "玩家阵营"
	}
	if factionID == state.EnemyFactionID {
		return "敌方阵营"
	}
	return factionID
}

// displayAttachItemName 返回附件物品显示名。
func displayAttachItemName(itemID string) string {
	definition, found := item.Lookup(itemID)
	if !found {
		return itemID
	}
	return definition.DisplayName
}
