package session

// 文件说明：分区大世界阶段3 · 任务系统（设计 docs/分区大世界设计方案-2026-06-10.md §5）。
// 魔兽式核心：去不同区域做任务。本文件承载「数据结构 + 服务端口（接取/进行中/交付）+ 进度结算 hook + 解锁」。
//   - 任务骨架（确定性护栏）在 quest_catalog.go；剧情按角色画像动态生成（LLM + fallback）在 quest_gen.go。
//   - 数据：Quest/Objective/QuestReward；state 加 ActiveQuests/CompletedQuestIDs/UnlockedZones（均 omitempty 兼容旧档）。
//   - 端口：AvailableQuests / AcceptQuest / ActiveQuests / TurnInQuest，全走 guardPlayerAction 五道门。
//   - 进度：advanceQuestObjectives 由世界事件驱动（击败 boss / 采集 / 到达新区）递增匹配 objective，全满 → completed。
//   - 解锁：交付时 UnlockZone 非空 → append UnlockedZones → zonePortalUnlocked 据此放开 portal 传送门。
//
// 硬约束：发奖的钱包经 status.Mutator(ReasonEconomyReward) 留痕、物品经 unit.AddBackpackItem、经验直增 Growth（非受保护）；
// 任务 ID 用 FNV 确定性派生（quest_catalog/quest_gen）；接取/达成的流程痕迹经 EmitProcessEvent 旁路。

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/unit"
)

const (
	// maxActiveQuests 是主角同时进行中任务的上限（设计 §4「上限如 5」）——避免任务列表无限堆积 + 自治上下文被任务淹没。
	maxActiveQuests = 5
)

// QuestType 是任务类型（决定结算口径）。阶段3 落 slay/collect/explore 三类（escort/story 后续阶段）。
type QuestType string

const (
	QuestTypeSlay    QuestType = "slay"    // 讨伐：击败某区 boss
	QuestTypeCollect QuestType = "collect" // 收集：采集 N 个某材料
	QuestTypeExplore QuestType = "explore" // 探索：到达某区域
)

// QuestState 是任务生命周期状态。
type QuestState string

const (
	QuestStateAvailable QuestState = "available" // 可接取（城镇发布，尚未加入 ActiveQuests）
	QuestStateActive    QuestState = "active"    // 进行中（在 ActiveQuests，目标未全达成）
	QuestStateCompleted QuestState = "completed" // 目标已全达成、待回城交付
	QuestStateTurnedIn  QuestState = "turned_in" // 已交付发奖（入 CompletedQuestIDs，移出 Active）
)

// objective.Kind 取值（结算驱动锚点）。
const (
	ObjectiveDefeatBoss  = "defeat_boss"  // 击败 Target=zoneId 的区域 boss（Required 通常 1）
	ObjectiveCollectItem = "collect_item" // 采集 Target=itemId 累计 Required 个
	ObjectiveReachZone   = "reach_zone"   // 到达 Target=zoneId 区域（Required 通常 1）
)

// Quest 是一桩任务（设计 §5.1）。Title/NarrativeZH 由 LLM 按角色画像动态生成（quest_gen.go），
// objective 类型/区域/数值与 Rewards 倾向由 quest_catalog.go 的骨架护栏锁定（LLM 只填叙事不改机制）。
type Quest struct {
	ID          string      `json:"id"`            // 确定性 id：FNV(sessionID+zoneId+skeletonKey)
	Title       string      `json:"title"`         // 「肃清晨曦平原的野狼」（LLM 动态生成 / fallback 模板）
	NarrativeZH string      `json:"narrative_zh"`  // 一段贴合角色画像的任务切入叙事（LLM / fallback）
	GiverUnitID string      `json:"giver_unit_id"` // 发布任务的功能性 NPC（可空——城镇泛发布）
	ZoneID      string      `json:"zone_id"`       // 任务所在/指向区域 id
	Type        QuestType   `json:"type"`          // slay/collect/explore
	Objectives  []Objective `json:"objectives"`    // 目标（含进度）
	Rewards     QuestReward `json:"rewards"`       // 奖励
	State       QuestState  `json:"state"`         // available/active/completed/turned_in
}

// Objective 是任务的一个目标（含进度）。Kind 决定结算锚点，Target 决定匹配对象。
type Objective struct {
	Kind     string `json:"kind"`     // defeat_boss / collect_item / reach_zone
	Target   string `json:"target"`   // zoneId / itemId / zoneId
	Required int    `json:"required"` // 需要的数量（defeat_boss/reach_zone 通常 1）
	Current  int    `json:"current"`  // 当前进度
}

// done 判定单条目标是否已达成。
func (o Objective) done() bool { return o.Current >= o.Required }

// QuestReward 是任务奖励（设计 §5.1）。Wallet 经 Mutator、ItemGrants 经背包、Exp 直增 Growth；
// UnlockZone 非空时交付后解锁该区 portal 传送门（append state.UnlockedZones）。
type QuestReward struct {
	Wallet     int      `json:"wallet"`                // 金币（经 status.Mutator(ReasonEconomyReward) 落 Wallet）
	ItemGrants []string `json:"item_grants,omitempty"` // 物品 id 列表（各 +1，经 unit.AddBackpackItem）
	Exp        int      `json:"exp"`                   // 经验（直增 Growth.Experience，非受保护字段）
	UnlockZone string   `json:"unlock_zone,omitempty"` // 完成解锁的 portal 目标区 id（空=不解锁）
}

// allObjectivesDone 判定任务的全部目标是否达成（空目标视为未达成，防空任务被秒交付刷奖）。
func allObjectivesDone(q Quest) bool {
	if len(q.Objectives) == 0 {
		return false
	}
	for _, o := range q.Objectives {
		if !o.done() {
			return false
		}
	}
	return true
}

// questActiveIndex 在 state.ActiveQuests 里按 id 找下标（找不到返回 -1）。
func questActiveIndex(state *State, questID string) int {
	if state == nil {
		return -1
	}
	for i := range state.ActiveQuests {
		if state.ActiveQuests[i].ID == questID {
			return i
		}
	}
	return -1
}

// questCompleted 判定某任务 id 是否已交付（在 CompletedQuestIDs 集合里）。
func questCompleted(state *State, questID string) bool {
	if state == nil {
		return false
	}
	for _, id := range state.CompletedQuestIDs {
		if id == questID {
			return true
		}
	}
	return false
}

// zoneUnlocked 判定某区域 id 是否已被任务解锁（在 UnlockedZones 集合里）。
func zoneUnlocked(state *State, zoneID string) bool {
	if state == nil || zoneID == "" {
		return false
	}
	for _, id := range state.UnlockedZones {
		if id == zoneID {
			return true
		}
	}
	return false
}

// appendUnlockedZone 幂等地把 zoneID 记入 state.UnlockedZones（已在则 no-op）。
func appendUnlockedZone(state *State, zoneID string) {
	if state == nil || zoneID == "" || zoneUnlocked(state, zoneID) {
		return
	}
	state.UnlockedZones = append(state.UnlockedZones, zoneID)
}

// appendCompletedQuest 幂等地把 questID 记入 state.CompletedQuestIDs（已在则 no-op）。
func appendCompletedQuest(state *State, questID string) {
	if state == nil || questID == "" || questCompleted(state, questID) {
		return
	}
	state.CompletedQuestIDs = append(state.CompletedQuestIDs, questID)
}

// activeQuestContextForUnit 返回某 unit 进行中任务的「她当前的目标」上下文串（喂自治决策 prompt，设计 §5.3 步骤 5）。
// 任务挂在主角身上（state.ActiveQuests 是主角的），故仅对玩家方单位（主角）surface；非玩家方单位/无任务返回空串。
// 仅取未交付的任务（available/active/completed），每桩一行「她当前的目标：[title]——[一句目标概要]」。
func activeQuestContextForUnit(state *State, unitID string) string {
	if state == nil || unitID == "" || len(state.ActiveQuests) == 0 {
		return ""
	}
	if !isPlayerSideUnit(*state, unitID) {
		return "" // 任务是主角的目标，不喂给 NPC/敌方的自治上下文
	}
	lines := make([]string, 0, len(state.ActiveQuests))
	for _, q := range state.ActiveQuests {
		if q.State == QuestStateTurnedIn {
			continue
		}
		lines = append(lines, fmt.Sprintf("她当前的目标：%s——%s", q.Title, questObjectiveHintZH(state, q)))
	}
	return strings.Join(lines, "\n")
}

// questObjectiveHintZH 把一桩任务的首个未达成目标翻成一句中文行动提示（喂自治上下文，引导她朝目标行动）。
// 全达成（待交付）则提示回城交付。纯函数、确定性。
func questObjectiveHintZH(state *State, q Quest) string {
	for _, o := range q.Objectives {
		if o.done() {
			continue
		}
		switch o.Kind {
		case ObjectiveDefeatBoss:
			name := questZoneBossName(state, o.Target)
			return fmt.Sprintf("去讨平%s", name)
		case ObjectiveCollectItem:
			return fmt.Sprintf("再采集 %d 份%s（已 %d/%d）", o.Required-o.Current, itemDisplayName(o.Target), o.Current, o.Required)
		case ObjectiveReachZone:
			return fmt.Sprintf("前往「%s」", questZoneName(state, o.Target))
		}
	}
	return "回城向发布者交差"
}

// questZoneName 取区域中文名（找不到回退区 id）。
func questZoneName(state *State, zoneID string) string {
	if idx := findZoneIndex(state, zoneID); idx >= 0 {
		if name := strings.TrimSpace(state.Zones[idx].Name); name != "" {
			return name
		}
	}
	return zoneID
}

// questZoneBossName 取某区 boss 名（找不到回退通用名）。
func questZoneBossName(state *State, zoneID string) string {
	if idx := findZoneIndex(state, zoneID); idx >= 0 {
		if name := strings.TrimSpace(state.Zones[idx].BossName); name != "" {
			return name
		}
	}
	return "那片天地的霸主"
}

// AvailableQuests 返回主角当前所在区域城镇可接取的任务（设计 §5.3 步骤 1）。
// 流程：guardPlayerAction → 取当前区 → questSkeletonsForZone 列骨架 → 每骨架 generateQuestForSkeleton 出剧情
// （LLM best-effort，失败走 fallback 模板）→ 过滤掉已接取/已交付的任务。读路径（不写 state），但持锁取一致快照。
func (service *Service) AvailableQuests(ctx context.Context, sessionID, unitID, zoneID string) ([]Quest, error) {
	if service == nil || service.units == nil {
		return nil, fmt.Errorf("available quests: service unavailable")
	}
	state, _, rec, release, err := service.guardPlayerAction(ctx, sessionID, unitID)
	if err != nil {
		return nil, err
	}
	defer release()

	// 默认看主角当前区；显式传 zone 且世界有分区则以请求为准（前端切区预览）。
	targetZoneID := strings.TrimSpace(zoneID)
	if targetZoneID == "" {
		targetZoneID = state.CurrentZoneID
	}
	if len(state.Zones) == 0 {
		return []Quest{}, nil // 单区/旧档：无任务系统（前端退回纯单图）
	}
	zoneIdx := findZoneIndex(&state, targetZoneID)
	if zoneIdx < 0 {
		return []Quest{}, nil // 未知区：无任务（不报错，前端空列表）
	}
	zone := state.Zones[zoneIdx]
	charter, _ := GetUnitCharter(&state, rec.ID)

	skeletons := questSkeletonsForZone(zone)
	out := make([]Quest, 0, len(skeletons))
	for _, sk := range skeletons {
		quest, interaction := service.generateQuestForSkeleton(ctx, state, rec, charter, zone, sk)
		// 记账：LLM 交互（含 fallback 路径，便于遥测）。best-effort，不阻断列举。
		if strings.TrimSpace(interaction.Kind) != "" {
			service.appendLLMInteractionWithSpend(ctx, &state, interaction)
		}
		// 过滤：已交付（不再出现）/ 进行中（已在 ActiveQuests，不重复发布）的不列入「可接取」。
		if questCompleted(&state, quest.ID) || questActiveIndex(&state, quest.ID) >= 0 {
			continue
		}
		quest.State = QuestStateAvailable
		out = append(out, quest)
	}
	return out, nil
}

// AcceptQuest 接取一桩任务（设计 §5.3 步骤 2）：把它加入 ActiveQuests（State=active）。
// 流程：guardPlayerAction → 校验未超上限/未重复接取/未已交付 → 据 questId 重新生成该骨架的任务（确定性 id 命中）→
// 入 ActiveQuests → 落痕（QUEST_ACCEPTED 流程事件）→ Save。questId 须为该区某骨架确定性 id（防伪造任意任务接取刷奖）。
func (service *Service) AcceptQuest(ctx context.Context, sessionID, unitID, questID string) (Quest, error) {
	if service == nil || service.units == nil {
		return Quest{}, fmt.Errorf("accept quest: service unavailable")
	}
	questID = strings.TrimSpace(questID)
	if questID == "" {
		return Quest{}, fmt.Errorf("要接哪桩差事，先说清楚")
	}
	state, _, rec, release, err := service.guardPlayerAction(ctx, sessionID, unitID)
	if err != nil {
		return Quest{}, err
	}
	defer release()

	if len(state.ActiveQuests) >= maxActiveQuests {
		return Quest{}, fmt.Errorf("她手上的差事已经够多了（上限 %d 桩），先了结几桩再说", maxActiveQuests)
	}
	if questActiveIndex(&state, questID) >= 0 {
		return Quest{}, fmt.Errorf("这桩差事她已经接下了")
	}
	if questCompleted(&state, questID) {
		return Quest{}, fmt.Errorf("这桩差事早已了结")
	}
	// 据 questId 找出对应骨架并重生成（确定性 id：同会话同区同骨架恒同 id，故能命中）。
	quest, interaction, ok := service.resolveQuestByID(ctx, &state, rec, questID)
	if strings.TrimSpace(interaction.Kind) != "" {
		service.appendLLMInteractionWithSpend(ctx, &state, interaction)
	}
	if !ok {
		return Quest{}, fmt.Errorf("找不到这桩差事——它或许不在此地发布")
	}
	quest.State = QuestStateActive
	state.ActiveQuests = append(state.ActiveQuests, quest)

	narrative := fmt.Sprintf("%s接下了「%s」。", rec.DisplayName(), quest.Title)
	appendLog(&state, "quest_accept", narrative, rec.ID, "")
	service.emitQuestProcessEvent(ctx, &state, rec.ID, events.ReasonQuestAccepted, quest, narrative)
	if err := service.saveSessionMergingExternalEvents(ctx, &state); err != nil {
		return Quest{}, fmt.Errorf("accept quest (save session): %w", err)
	}
	return quest, nil
}

// ListActiveQuests 返回主角进行中的任务 + 进度（设计 §5.3 步骤 3）。读路径，不写 state。
func (service *Service) ListActiveQuests(ctx context.Context, sessionID, unitID string) ([]Quest, error) {
	if service == nil || service.units == nil {
		return nil, fmt.Errorf("active quests: service unavailable")
	}
	state, _, _, release, err := service.guardPlayerAction(ctx, sessionID, unitID)
	if err != nil {
		return nil, err
	}
	defer release()
	out := append([]Quest(nil), state.ActiveQuests...) // 拷贝出读视图，避免外泄内部切片
	return out, nil
}

// TurnInQuest 交付一桩任务并发奖（设计 §5.3 步骤 4）。
// 流程：guardPlayerAction → 在 ActiveQuests 找到 → 校验 objective 全达成 → 发奖（钱包经 Mutator / 物品经背包 / 经验直增 Growth）
// → UnlockZone 非空则 append UnlockedZones（解锁 portal）→ 移出 ActiveQuests + 入 CompletedQuestIDs → 落痕 → Save。
// 未达成则拒绝（State 仍 active/completed，可继续推进）。发奖经 units.Save（背包/经验）+ session Save（任务态）双库落地。
func (service *Service) TurnInQuest(ctx context.Context, sessionID, unitID, questID string) (Quest, error) {
	if service == nil || service.units == nil {
		return Quest{}, fmt.Errorf("turn in quest: service unavailable")
	}
	questID = strings.TrimSpace(questID)
	if questID == "" {
		return Quest{}, fmt.Errorf("要交付哪桩差事，先说清楚")
	}
	state, _, rec, release, err := service.guardPlayerAction(ctx, sessionID, unitID)
	if err != nil {
		return Quest{}, err
	}
	defer release()

	idx := questActiveIndex(&state, questID)
	if idx < 0 {
		return Quest{}, fmt.Errorf("她手上没有这桩差事")
	}
	quest := state.ActiveQuests[idx]
	if !allObjectivesDone(quest) {
		return Quest{}, fmt.Errorf("「%s」的事还没办妥，不能交差", quest.Title)
	}

	// ── 先标记后发奖（幂等闸，关闭跨库刷奖窗口）──
	// 历史缺陷：发奖（钱包经 Mutator / 物品+经验经 units.Save）先落 unit 库，而「移出 ActiveQuests + 入 CompletedQuestIDs」
	// 只随 session 库的最后一次 Save 落地。若 session save 失败或进程在两次落库之间崩溃，玩家重试时 questActiveIndex≥0 仍命中、
	// 进度仍全满、无「已结算」标记可查 → 钱包/物品/经验被**再次**发放。
	// 修法（设计宪法「结算用代码」+ 与 travel.go 的自愈同口径）：把「交付标记 + CompletedQuestIDs」作为发奖前置门——
	// 先把 quest 移出 Active、入 CompletedQuestIDs 并成功落 session 库，再发奖。重试时 questCompleted 命中即被上方 line 286
	// 的「这桩差事早已了结」门拒绝（accept 路径）、本函数 questActiveIndex<0 即「她手上没有这桩差事」拒绝（turn-in 路径），
	// 双路均不再二次发奖。session save 成功后即便发奖失败，至多漏发（不可重领、无刷奖），不构成经济风险。
	//
	// ① 解锁传送：UnlockZone 非空 → 解锁该区 portal（zonePortalUnlocked 据 UnlockedZones 放开）。随 session 库一并落地。
	if zid := strings.TrimSpace(quest.Rewards.UnlockZone); zid != "" {
		appendUnlockedZone(&state, zid)
	}
	// ② 收尾标记：移出 ActiveQuests + 入 CompletedQuestIDs（state.ActiveQuests 是切片删除）。
	quest.State = QuestStateTurnedIn
	state.ActiveQuests = append(state.ActiveQuests[:idx], state.ActiveQuests[idx+1:]...)
	appendCompletedQuest(&state, quest.ID)

	narrative := fmt.Sprintf("%s了结了「%s」，得了应有的酬劳。", rec.DisplayName(), quest.Title)
	appendLog(&state, "quest_turn_in", narrative, rec.ID, "")
	service.emitQuestProcessEvent(ctx, &state, rec.ID, events.ReasonQuestCompleted, quest, narrative)
	// ③ 先落 session 库（交付标记成为发奖前置门）。失败即返回，未发任何奖 → 玩家重试可正常再交付，无任何刷奖窗口。
	if err := service.saveSessionMergingExternalEvents(ctx, &state); err != nil {
		return Quest{}, fmt.Errorf("turn in quest (save session): %w", err)
	}

	// ── 发奖（交付标记已落库，此处恒幂等：重试不可能再走到这里）──
	// ④ 钱包：经 status.Mutator(ReasonEconomyReward) 留痕（wallet 域），actor 同步回写。best-effort：失败仅记日志、不回滚
	// 标记（标记已是终态，回滚会重开刷奖窗口）——至多漏发钱包，玩家不可重领。
	if quest.Rewards.Wallet > 0 {
		res, mErr := service.applyEliteMutation(ctx, status.Mutation{
			UnitID:     rec.ID,
			Turn:       state.TurnState.Turn,
			Field:      status.FieldWallet,
			Delta:      float64(quest.Rewards.Wallet),
			ReasonCode: events.ReasonEconomyReward,
			ReasonText: fmt.Sprintf("交付「%s」得来的酬劳", quest.Title),
			Actors:     []string{rec.ID},
		})
		if mErr != nil {
			slog.Warn("turn in quest: wallet reward failed after turn-in committed",
				"session", state.ID, "unit", rec.ID, "quest", quest.ID, "error", mErr)
		} else {
			*rec = res.Record
		}
	}
	// ⑤ 物品：经 unit.AddBackpackItem（未知物品/满包 best-effort 跳过，不阻断交付）。
	for _, itemID := range quest.Rewards.ItemGrants {
		itemID = strings.TrimSpace(itemID)
		if itemID == "" {
			continue
		}
		_ = unit.AddBackpackItem(rec, itemID, 1)
	}
	// ⑥ 经验：直增 Growth.Experience（非受保护字段，无需 Mutator；不在此做升级换算，留派生重算路径）。
	if quest.Rewards.Exp > 0 {
		rec.Stats.Growth.Experience += quest.Rewards.Exp
	}
	// 背包/经验改动落库（钱包已由 Mutator 落库；这里再 Save 一次确保物品/经验持久）。best-effort：失败仅记日志，
	// 交付已是终态（不可重领），至多漏发物品/经验。
	if err := service.units.Save(ctx, *rec); err != nil {
		slog.Warn("turn in quest: item/exp reward save failed after turn-in committed",
			"session", state.ID, "unit", rec.ID, "quest", quest.ID, "error", err)
	}
	return quest, nil
}

// emitQuestProcessEvent 为任务接取/交付落一条流程事件（EmitProcessEvent 旁路，不改保护字段）+ 推实时 feed。
// best-effort：吞错（含 panic）绝不阻断任务主链路。
func (service *Service) emitQuestProcessEvent(ctx context.Context, state *State, unitID string, code events.ReasonCode, quest Quest, narrative string) {
	defer func() { _ = recover() }()
	if service == nil || state == nil {
		return
	}
	if service.db != nil {
		_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
			SessionID:   state.ID,
			OwnerUnitID: unitID,
			Code:        code,
			Category:    events.CategoryLifecycle,
			Payload: map[string]any{
				"narrative":  narrative,
				"unit_id":    unitID,
				"turn":       state.TurnState.Turn,
				"quest_id":   quest.ID,
				"quest_type": string(quest.Type),
				"importance": 4,
			},
			WorldID:  state.WorldID,
			RegionID: state.ID,
			Tick:     state.TurnState.Turn,
		})
	}
	service.pushRealtime(state.ID, "fate_life_beat", map[string]any{
		"unit_id":   unitID,
		"narrative": narrative,
		"turn":      state.TurnState.Turn,
	})
}

// advanceCollectObjectivesForGrants 把一次采集实际入袋的材料推进 collect_item 任务目标（gather hook 专用）。
// rewards 是本次产出、discarded 是满包丢弃部分——只计真正落袋的（rewards 减去 discarded 同 itemId 的量）。
// best-effort、纯逻辑（改 state.ActiveQuests，随既有 Save 落库）。
func advanceCollectObjectivesForGrants(state *State, rewards, discarded []itemGrant) {
	if state == nil || len(rewards) == 0 || len(state.ActiveQuests) == 0 {
		return
	}
	// 满包丢弃量按 itemId 汇总，从产出里扣除（只计真正入袋的）。
	dropped := map[string]int{}
	for _, d := range discarded {
		dropped[d.ItemID] += d.Quantity
	}
	for _, r := range rewards {
		landed := r.Quantity - dropped[r.ItemID]
		if landed > 0 {
			advanceQuestObjectives(state, ObjectiveCollectItem, r.ItemID, landed)
		}
	}
}

// advanceQuestObjectives 是任务进度结算 hook（设计 §5.3 步骤 3，best-effort，不破坏既有结算）。
// 由世界事件驱动：击败 boss(kind=ObjectiveDefeatBoss,target=zoneId) / 采集(kind=ObjectiveCollectItem,target=itemId,累计 amount)
// / 到达新区(kind=ObjectiveReachZone,target=zoneId) 时，递增匹配的 active quest objective。某 quest 目标全满 → State=completed（待交付）。
//
// 纯逻辑（只改传入的 *state.ActiveQuests，不落库——由调用方在既有 Save 路径一并持久化），确定性，无 LLM/无 I/O。
// amount 是本次事件的增量（采集量；击败/到达通常 1）。amount<=0 视为 1（容错）。
func advanceQuestObjectives(state *State, kind, target string, amount int) {
	if state == nil || kind == "" || target == "" {
		return
	}
	if amount <= 0 {
		amount = 1
	}
	for qi := range state.ActiveQuests {
		quest := &state.ActiveQuests[qi]
		if quest.State == QuestStateTurnedIn {
			continue
		}
		changed := false
		for oi := range quest.Objectives {
			obj := &quest.Objectives[oi]
			if obj.Kind != kind || obj.Target != target || obj.done() {
				continue
			}
			obj.Current += amount
			if obj.Current > obj.Required {
				obj.Current = obj.Required // clamp（采集溢出不超额记，避免进度条越界）
			}
			changed = true
		}
		// 全目标达成 → 标 completed（待回城交付）。仅在本 quest 真有进度变化时复检（省遍历）。
		if changed && allObjectivesDone(*quest) {
			quest.State = QuestStateCompleted
		}
	}
}
