package session

// 文件说明：玩家「在线直接操作角色」的动作（命运开盒的混合模型——离线她自治，上线玩家可直接干预）。
// 这些是玩家即时直驱的动作，与世界自治推进共存：玩家随时可指挥她移动/穿卸装备/吃东西，世界往前走时她仍按自治决策行动。
// 统一纪律（波次 A-2）：
//   ① 互斥：执行阶段进行中（ExecutionInProgress / 进程级异步注册表）一律拒绝，返回哨兵 ErrExecutionBusy——
//      避免玩家直驱与后台执行循环对同一单位双写竞态（router 据 errors.Is 映射 409）。
//   ② 受保护字段（HP/Hunger/Morale/Loyalty/Mood/LivesRemaining）恒经 status.Mutator（如 PlayerUseItem 的饥饿/生命恢复）；
//      位置/背包/装备是非受保护字段，直改 + 持久化。
//   ③ 落痕：每个直驱动作 appendLog 一句中文叙事 + EmitProcessEvent 流程事件（ReasonPlayerTileAction，importance 低调），
//      让直控对角色「真的发生过」，可被回响/归因/命运 feed 引用。落痕本身 best-effort，绝不阻断已完成的动作。
//   ④ 零 LLM、全程校验单位归属本会话，防跨会话越权操作他人角色。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/item"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// ErrExecutionBusy 是「执行阶段进行中，玩家直驱被互斥拒绝」的哨兵错误。
// router 层应以 errors.Is(err, session.ErrExecutionBusy) 识别并映射 HTTP 409 Conflict。
var ErrExecutionBusy = errors.New("她正忙于眼前的事，稍候片刻")

// playerMoveMaxDistance 是单次直驱移动的六边形距离上限（A6 限距：消除「瞬移跨图规避一切风险」）。
const playerMoveMaxDistance = 8

// playerActionLocks 是进程级「玩家直驱动作」per-session 互斥注册表（评审 C1）：
// router 每请求新建 Service 实例，实例内无任何跨请求护栏；isAsyncExecutionRunning 只互斥「玩家 vs 执行循环」，
// 不互斥「玩家请求 vs 玩家请求」——并发双发 poi-encounter/use-item 会各自加载快照完整结算（双倍奖励/一药双喝）。
// 这里用 try-lock 语义：占用中直接返回 ErrExecutionBusy（409），不排队（玩家重试成本低，排队反而堆积写竞态窗口）。
var playerActionLocks = struct {
	sync.Mutex
	busy map[string]struct{}
}{busy: make(map[string]struct{})}

// acquirePlayerActionLock 占用某会话的玩家直驱锁；成功返回释放函数（调用方必须 defer release()），
// 已被占用返回 ErrExecutionBusy。
func acquirePlayerActionLock(sessionID string) (func(), error) {
	sessionID = strings.TrimSpace(sessionID)
	playerActionLocks.Lock()
	defer playerActionLocks.Unlock()
	if _, exists := playerActionLocks.busy[sessionID]; exists {
		return nil, ErrExecutionBusy
	}
	playerActionLocks.busy[sessionID] = struct{}{}
	return func() {
		playerActionLocks.Lock()
		defer playerActionLocks.Unlock()
		delete(playerActionLocks.busy, sessionID)
	}, nil
}

// findUnitInSession 在本会话单位里按 id 找一个可改的记录指针（不属本局返回 nil）。
func findUnitInSession(units []unit.Record, unitID string) *unit.Record {
	for i := range units {
		if units[i].ID == unitID {
			return &units[i]
		}
	}
	return nil
}

// guardPlayerAction 是所有玩家直驱写入口的统一前置（评审 C1/M1 后收口为五道门）：
// per-session 直驱锁（防玩家请求并发双写）→ 执行互斥（A7）→ 终局门 → 归属本局 → 归属玩家方。
// 成功时返回 release 函数，调用方必须 defer release()；任何失败 release 已在内部处理、返回 nil release。
func (service *Service) guardPlayerAction(ctx context.Context, sessionID, unitID string) (State, []unit.Record, *unit.Record, func(), error) {
	release, err := acquirePlayerActionLock(sessionID)
	if err != nil {
		return State{}, nil, nil, nil, err
	}
	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		release()
		return State{}, nil, nil, nil, err
	}
	// 互斥：同步标志或进程级异步注册表任一命中即视为执行中（与 AdvanceFateWorld 同口径）。
	if state.ExecutionInProgress || isAsyncExecutionRunning(sessionID) {
		release()
		return State{}, nil, nil, nil, ErrExecutionBusy
	}
	// 终局门：已落幕的会话不再接受任何直驱动作（评审 minor#5 收口）。
	if state.Outcome != OutcomeOngoing {
		release()
		return State{}, nil, nil, nil, fmt.Errorf("这一局已经落幕")
	}
	rec := findUnitInSession(units, unitID)
	if rec == nil {
		release()
		return State{}, nil, nil, nil, ErrTileUnitNotFound
	}
	// 玩家方校验（评审 M1）：直驱只许差遣自己的人——否则可卸敌方武器/强灌敌方喝药/搬空敌方口粮。
	if !isPlayerSideUnit(state, rec.ID) {
		release()
		return State{}, nil, nil, nil, ErrTileUnitNotYours
	}
	return state, units, rec, release, nil
}

// recordPlayerActionTrace 给一次已成功的玩家直驱动作落叙事痕迹（A8）：
// appendLog 一句中文 + EmitProcessEvent 流程事件（ReasonPlayerTileAction，importance 低调）+ 持久化会话状态。
// 全程 best-effort：落痕失败绝不影响已持久化的动作结果（动作本体在调用前已 Save）。
func (service *Service) recordPlayerActionTrace(ctx context.Context, state *State, actor *unit.Record, logKind, message, actionKind string) {
	defer func() { _ = recover() }() // 落痕是旁路叙事，panic 不外溢
	if state == nil || actor == nil {
		return
	}
	appendLog(state, logKind, message, actor.ID, "")
	if service.db != nil {
		_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
			SessionID:   state.ID,
			OwnerUnitID: actor.ID,
			Code:        events.ReasonPlayerTileAction,
			Category:    events.CategoryLifecycle,
			Payload: map[string]any{
				"narrative":  message,
				"unit_id":    actor.ID,
				"turn":       state.TurnState.Turn,
				"reason":     string(events.ReasonPlayerTileAction),
				"kind":       actionKind,
				"importance": 2, // 低调：玩家日常直驱不该盖过真正的命运事件
			},
			WorldID:  state.WorldID,
			RegionID: state.ID,
			Tick:     state.TurnState.Turn,
		})
	}
	// 状态里新增的日志要持久化才能被前端/重连看到；用合并保存避免覆盖外部写入。
	_ = service.saveSessionMergingExternalEvents(ctx, state)
}

// itemDisplayName 取物品的中文显示名（查不到目录时回退原 ID，叙事不至于开天窗）。
func itemDisplayName(itemID string) string {
	if definition, ok := item.Lookup(itemID); ok && definition.DisplayName != "" {
		return definition.DisplayName
	}
	return itemID
}

// PlayerMoveUnit 玩家直接把某角色移到目标格（在线操作）。
// 校验：执行互斥（A7）→ 目标在地图内 → 非阻挡地形 → 单位归属本会话 → 限距 ≤8 格（A6）。
// 成功后：直改位置并持久化 → 落叙事痕迹（A8）→ best-effort 冒遭遇 beat（A5，绝不影响移动结果）。返回移动后的坐标。
func (service *Service) PlayerMoveUnit(ctx context.Context, sessionID, unitID string, q, r int) (int, int, error) {
	if service == nil || service.units == nil {
		return 0, 0, fmt.Errorf("player move: service unavailable")
	}
	state, units, rec, release, err := service.guardPlayerAction(ctx, sessionID, unitID)
	if err != nil {
		return 0, 0, err
	}
	defer release()
	coord := world.Coord{Q: q, R: r}
	if !inBounds(state.Map, coord) {
		return 0, 0, fmt.Errorf("那里在天地之外，去不得")
	}
	// 阻挡：水域/山地不可直接踏入（与战棋移动同口径，避免她站进河里）。
	switch terrainAt(state.Map, coord) {
	case world.TerrainRiver, world.TerrainMountain:
		return 0, 0, fmt.Errorf("那里过不去（水/山阻路）")
	}
	// 限距（A6）：单次移动六边形距离 ≤8 格，消除「一跳即达任意远格规避一切风险」。
	if unit.HexDistance(rec.Status.PositionQ, rec.Status.PositionR, q, r) > playerMoveMaxDistance {
		return 0, 0, fmt.Errorf("太远了，先让她走近些")
	}
	distance := unit.HexDistance(rec.Status.PositionQ, rec.Status.PositionR, q, r)
	rec.Status.PositionQ = q
	rec.Status.PositionR = r
	if err := service.units.Save(ctx, *rec); err != nil {
		return 0, 0, fmt.Errorf("player move (save): %w", err)
	}
	// 赶路的代价（评审 minor#6）：直驱移动按距离消耗少量饥饿（1~3 点，恒经 Mutator + 既有行军损耗 code），
	// 让「连发 move 横穿地图」至少付出口粮成本，向自治移动的消耗口径靠拢。best-effort：扣不动不回滚移动。
	if distance > 0 && service.mutator != nil {
		marchCost := float64(1 + distance/4)
		_ = service.applyStatusMutation(ctx, &state, rec, status.FieldHunger, -marchCost, events.ReasonSurvivalMarch, "我依嘱咐赶了一段路，耗了些体力。")
	}
	// 落痕（A8）：一句中文叙事 + 流程事件，best-effort。
	service.recordPlayerActionTrace(
		ctx, &state, rec,
		"player_move",
		fmt.Sprintf("依你的指引，%s动身去了 (%d,%d)。", rec.DisplayName(), q, r),
		"player_move",
	)
	// 遭遇判定（A5）：移动落点附近有 POI/野外 NPC 时确定性概率冒一条遭遇命运 beat。
	// best-effort + panic 兜底：遭遇是旁路叙事，任何异常绝不影响已完成的移动。
	func() {
		defer func() { _ = recover() }()
		service.surfaceEncounterBeatBestEffort(ctx, &state, rec, mapRecordsByID(units))
	}()
	return q, r, nil
}

// PlayerEquipItem 玩家给某角色从背包穿上某装备（在线操作）。复用 unit.EquipBackpackItem（含槽位/重算派生攻防）。
// 执行互斥（A7）+ 轻量落痕（A8）。
func (service *Service) PlayerEquipItem(ctx context.Context, sessionID, unitID, itemID string) error {
	if service == nil || service.units == nil {
		return fmt.Errorf("player equip: service unavailable")
	}
	state, _, rec, release, err := service.guardPlayerAction(ctx, sessionID, unitID)
	if err != nil {
		return err
	}
	defer release()
	if err := unit.EquipBackpackItem(rec, itemID); err != nil {
		return err
	}
	if err := service.units.Save(ctx, *rec); err != nil {
		return err
	}
	service.recordPlayerActionTrace(
		ctx, &state, rec,
		"player_equip",
		fmt.Sprintf("依你的安排，%s换上了「%s」。", rec.DisplayName(), itemDisplayName(itemID)),
		"player_equip",
	)
	return nil
}

// PlayerUseItem 玩家让某角色使用背包里的一件消耗品（在线操作，A10）。
// 结算复用既有进食/喝药路径（hunger.go 的 executeEat/executeHealingPotion 同口径）：
//   - ration（口粮）→ 饥饿 +35，恒经 status.Mutator + ReasonSurvivalHunger；
//   - healing_potion（治疗药剂）→ HP +25，同上（满血时拒绝，免浪费）；
//   - 其余消耗品（药草包/解毒药/复活石）暂无直驱用法 → 中文报错；
//   - 非消耗品 → 「这件东西不是用来吃的」。
// 顺序与 executeEat 一致：先扣背包并 Save（让 Mutator 读到的快照已含扣减），再经 Mutator 恢复数值，最后落痕。
func (service *Service) PlayerUseItem(ctx context.Context, sessionID, unitID, itemID string) error {
	if service == nil || service.units == nil || service.mutator == nil {
		return fmt.Errorf("player use item: service unavailable")
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return fmt.Errorf("要用哪件东西，先指明白")
	}
	state, _, rec, release, err := service.guardPlayerAction(ctx, sessionID, unitID)
	if err != nil {
		return err
	}
	defer release()
	if rec.Status.LifeState != unit.LifeStateActive {
		return fmt.Errorf("她现在的状态做不了这件事")
	}
	definition, ok := item.Lookup(itemID)
	if !ok {
		return fmt.Errorf("没有这样的东西")
	}
	if definition.Category != item.CategoryConsumable {
		return fmt.Errorf("这件东西不是用来吃的")
	}
	if !hasBackpackItem(*rec, itemID) {
		return fmt.Errorf("她的行囊里没有这件东西")
	}

	var (
		field      status.Field
		delta      float64
		reasonText string
		logText    string
	)
	switch itemID {
	case "ration":
		// 与 executeEat 同口径：口粮恢复饥饿 +35。
		field, delta = status.FieldHunger, 35
		reasonText = "我依嘱咐吃下一份口粮恢复体力。"
		logText = fmt.Sprintf("依你的嘱咐，%s吃下了一份「%s」。", rec.DisplayName(), definition.DisplayName)
	case "healing_potion":
		// 与 executeHealingPotion 同口径：药剂恢复 HP +25，满血拒绝。
		if rec.Status.HP >= 100 {
			return fmt.Errorf("她状态还满，不必浪费药")
		}
		field, delta = status.FieldHP, 25
		reasonText = "我依嘱咐喝下一瓶药剂恢复生命。"
		logText = fmt.Sprintf("依你的嘱咐，%s喝下了一瓶「%s」。", rec.DisplayName(), definition.DisplayName)
	default:
		// 其余消耗品（herb_bundle/antidote/revive_stone）尚无玩家直驱结算路径，先明确拒绝而非吞掉物品。
		return fmt.Errorf("这东西她还不知如何使用")
	}

	if err := unit.ConsumeBackpackItem(rec, itemID, 1); err != nil {
		return err
	}
	if err := service.units.Save(ctx, *rec); err != nil {
		return err
	}
	// 受保护字段（Hunger/HP）恒经 Mutator：clamp + reason code 留痕 + RecentEventIDs 追加，可审计。
	if err := service.applyStatusMutation(ctx, &state, rec, field, delta, events.ReasonSurvivalHunger, reasonText); err != nil {
		return err
	}
	service.recordPlayerActionTrace(ctx, &state, rec, "player_use_item", logText, "player_use_item")
	return nil
}

// PlayerUnequipItem 玩家让某角色卸下指定槽位的装备放回背包（在线操作，A11）。
// 复用 unit.UnequipBackpackItem（EquipBackpackItem 的逆操作：回包保留强化等级/自定义名 + 重算派生攻防）。
// slot 取装备槽位键：weapon / armor / shoes / accessory。
func (service *Service) PlayerUnequipItem(ctx context.Context, sessionID, unitID, slot string) error {
	if service == nil || service.units == nil {
		return fmt.Errorf("player unequip: service unavailable")
	}
	slot = strings.ToLower(strings.TrimSpace(slot))
	if slot == "" {
		return fmt.Errorf("要卸下哪一处，先指明白")
	}
	state, _, rec, release, err := service.guardPlayerAction(ctx, sessionID, unitID)
	if err != nil {
		return err
	}
	defer release()
	if current, occupied := rec.Inventory.Equipment[slot]; !occupied || current.ItemID == "" {
		return fmt.Errorf("那里本就空无一物")
	}
	stack, err := unit.UnequipBackpackItem(rec, slot)
	if err != nil {
		// 最常见失败：背包满（回包失败时装备保持穿戴原样，绝不凭空消失）。给中文友好提示。
		if strings.Contains(err.Error(), "backpack is full") {
			return fmt.Errorf("她的行囊已满，腾不出地方放这件装备")
		}
		return err
	}
	if err := service.units.Save(ctx, *rec); err != nil {
		return err
	}
	service.recordPlayerActionTrace(
		ctx, &state, rec,
		"player_unequip",
		fmt.Sprintf("依你的安排，%s卸下了「%s」，收进了行囊。", rec.DisplayName(), itemDisplayName(stack.ItemID)),
		"player_unequip",
	)
	return nil
}
