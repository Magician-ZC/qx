package session

// 文件说明：命运开盒「世界推进」——让降生主世界后停在 Turn1/部署阶段的 session 真正转起来。
//
// 问题（本文件解决的）：主世界角色降生即 Turn1/PhaseDeployment（mainworld.go），从无人推进它跨阶段；而所有命运
// 事件、角色自治「生活」全挂在执行阶段 + 阶段边界（只有 AdvancePhase 触达）。结果指引只记日志（执行期永不跑故永不消费）、
// 角色永不自治、命运卡永不冒出来、feed 永远「还很平静」。
//
// 本文件提供：
//   - AdvanceFateWorld：best-effort 推主世界 session 一拍（部署→异步执行一轮自治+边界结算）。复用于指引/端点/ticker。
//   - surfaceLifeBeatBestEffort：主世界玩家角色执行期自治一拍后，把她这拍的经历低调 surface 成一条命运 feed 生活 beat
//     （LIFE_BEAT 流程事件，每拍至多 1 条、仅玩家角色），让「她近来经历的」始终有内容。
//   - RunFateAutoTickLoop：后台低频 ticker（flag QUNXIANG_FATE_AUTOTICK 默认关），开启时扫 world_default 下活跃主世界
//     session 各推一拍，让世界自己往前走。默认关零行为。

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// AdvanceFateWorld best-effort 推主世界 session 一拍：
//   - 若 PhaseDeployment 且 !ExecutionInProgress → 启动异步执行一轮（角色自治决策 + 边界结算 + 生活 beat），返回 advancing=true。
//   - 若已在执行中（ExecutionInProgress / 异步运行中）→ 不重复推，返回 advancing=true（一拍正进行中）。
//   - 其它（非部署、已结束、载不到）→ advancing=false。
//
// 关键（与 RequestAdvancePhase 的区别）：命运世界的自治推进**不要求玩家每回合新提交方针**。RequestAdvancePhase 有
// 「请先提交当前阶段方针」的门（hasFactionDirectiveForCurrentPhase 要求本回合本阶段有玩家 doctrine）——降生只在 Turn1 种了一条
// doctrine，第二拍起就再无新方针，那条门会把自治世界永久卡死在部署阶段。故 AdvanceFateWorld 绕过该门、直接走
// advanceDeploymentToExecutionFastPath（切执行阶段 + 置 ExecutionInProgress + launchAsyncExecution 起后台一轮）：
// 角色据**长期生效**的离线宪章/出生 doctrine 自治，无需玩家每回合下令。
//
// 全程吞错不崩（best-effort）：推进失败绝不阻断调用它的指引/端点/ticker。复用于指引触发、"让世界往前走"端点、后台 ticker。
func (service *Service) AdvanceFateWorld(ctx context.Context, sessionID string) (advancing bool, err error) {
	if service == nil || service.sessions == nil {
		return false, fmt.Errorf("advance fate world: missing dependencies")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false, fmt.Errorf("advance fate world: empty session id")
	}
	state, units, loadErr := service.loadSession(ctx, sessionID)
	if loadErr != nil {
		return false, loadErr
	}
	// 已在执行中（同步标志或进程级异步注册表）→ 一拍正进行中，不重复推。
	if state.ExecutionInProgress || isAsyncExecutionRunning(sessionID) {
		return true, nil
	}
	// 仅在「部署阶段 + 局未结束」时启动一拍；执行阶段交由既有异步收尾（advanceAfterAsyncExecution）跑完。
	if state.TurnState.Phase != turns.PhaseDeployment || state.Outcome != OutcomeOngoing {
		return false, nil
	}
	// 异步执行未启用（理论上生产恒异步；防御性兜底）→ 走同步 AdvancePhase 推一拍，部署→执行→边界一次跑完。
	if !service.asyncExecution {
		if _, advErr := service.AdvancePhase(ctx, sessionID); advErr != nil {
			return false, advErr
		}
		return true, nil
	}
	// 生产路径：直接走 fast-path 切执行阶段、置 ExecutionInProgress、起后台异步执行一轮（绕过「每回合提交方针」门）。
	if _, advErr := service.advanceDeploymentToExecutionFastPath(ctx, &state, units); advErr != nil {
		return false, advErr
	}
	return true, nil
}

// surfaceLifeBeatBestEffort 把主世界玩家角色执行期自治走过的一拍经历（她做了什么/去哪/遇见谁/心情）
// 低调 surface 成一条命运 feed「生活 beat」（LIFE_BEAT 流程事件，CategoryLifecycle）。
//
// 关键（让「她近来经历的」有内容）：每推进一拍，feed 多一条她的经历。约束：
//   - 仅主世界玩家角色（world_default 绑定 + 在 state.PlayerUnitIDs 里），非 NPC——避免给一堆村民/敌方刷屏。
//   - 每拍至多 1 条（由调用点 actionIndex==1 即本回合首动作守门）。
//   - 全程 best-effort：吞错不阻断执行主循环（生活 beat 是旁路叙事，绝不影响结算）。
//
// beat 文本取自该拍决策的叙事字段（next_action / speak / reasoning / memory，按可读性优先），冠以她的名字。
func (service *Service) surfaceLifeBeatBestEffort(
	ctx context.Context,
	state *State,
	actor *unit.Record,
	decision unitDecisionPayload,
) {
	if service == nil || service.db == nil || state == nil || actor == nil {
		return
	}
	// 仅主世界玩家角色：world_default 绑定且该单位是玩家单位（非 NPC/村民/敌方）。
	if !isMainWorldPlayerUnit(*state, actor.ID) {
		return
	}
	beat := composeLifeBeatText(*actor, decision)
	if strings.TrimSpace(beat) == "" {
		return
	}
	payload := map[string]any{
		"narrative": beat,
		"unit_id":   actor.ID,
		"turn":      state.TurnState.Turn,
		"reason":    string(events.ReasonLifeBeat),
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   state.ID,
		OwnerUnitID: actor.ID,
		Code:        events.ReasonLifeBeat,
		Category:    events.CategoryLifecycle,
		Payload:     payload,
		WorldID:     state.WorldID,
	}); err != nil {
		return // best-effort：写库失败不阻断主循环
	}
	// 实时推送（best-effort）：让前端命运 feed 无需轮询即可即时看到这拍生活 beat。
	service.pushRealtime(state.ID, "fate_life_beat", map[string]any{
		"unit_id":   actor.ID,
		"narrative": beat,
		"turn":      state.TurnState.Turn,
	})
}

// surfaceEncounterBeatBestEffort 在她移动后判断「当前格/相邻格有 POI 或野外 NPC」时，额外冒一条遭遇命运 beat
// （如「她在古迹遗物前驻足」「她在路上撞见一个似有求助之事的人」）。复用 ReasonLifeBeat（feed 零改即可显示）。
// 全确定性（encounterRoll 含坐标+turn 的 FNV，禁 time.Now/rand）；每拍 ≤1 条（调用点已在 actionIndex==1 守门）+ 概率闸防刷屏。
func (service *Service) surfaceEncounterBeatBestEffort(
	ctx context.Context,
	state *State,
	actor *unit.Record,
	byID map[string]*unit.Record,
) {
	if service == nil || service.db == nil || state == nil || actor == nil {
		return
	}
	if !isMainWorldPlayerUnit(*state, actor.ID) {
		return
	}
	aq, ar := actor.Status.PositionQ, actor.Status.PositionR

	// 候选遭遇源：① 相邻(含同格)的野外 NPC（带身上的事件类型）② 当前格的特殊资源 POI。
	var beat string
	// ① 野外 NPC：取最近的一个。
	for _, id := range state.WildUnitIDs {
		w := byID[id]
		if w == nil {
			continue
		}
		// 分区过滤（与 zoneVisibleUnits 同口径）：只对主角当前区的野外 NPC 判遭遇，避免挂在别区的散人
		// 因坐标数值相邻冒假遭遇。CurrentZoneID 为空（旧单图档）不过滤；空 ZoneID 视作当前区（兜旧迁移残留）。
		if state.CurrentZoneID != "" && w.Status.ZoneID != "" && w.Status.ZoneID != state.CurrentZoneID {
			continue
		}
		if unit.HexDistance(aq, ar, w.Status.PositionQ, w.Status.PositionR) > 1 {
			continue
		}
		coord := world.Coord{Q: w.Status.PositionQ, R: w.Status.PositionR}
		eventType := npcEventTypeFor(state.ID, coord, w.ID)
		beat = fmt.Sprintf("%s：在路上撞见了%s，似有%s之事。", actor.DisplayName(), w.DisplayName(), eventType)
		break
	}
	// ② 没撞见人，则看当前格有无特殊资源 POI（与 computeMapResourcePOIs 同口径的稀疏判定）。
	if beat == "" {
		coord := world.Coord{Q: aq, R: ar}
		terrain := terrainAt(state.Map, coord)
		if res := resourceForTerrain(terrain); res != "" {
			threshold := 0.12
			if terrain == world.TerrainRuins {
				threshold = 0.85
			}
			if poiRoll(state.ID, coord, "resource") < threshold {
				beat = fmt.Sprintf("%s：在一处%s前驻足，似有所得。", actor.DisplayName(), res)
			}
		}
	}
	if beat == "" {
		return // 绝大多数拍：无遭遇源
	}
	// 概率闸（含 turn，使同一处不同拍掷骰不同）：<阈值才冒，避免每拍都刷。
	if encounterRoll(*state, *actor) >= 0.45 {
		return
	}
	payload := map[string]any{
		"narrative": beat,
		"unit_id":   actor.ID,
		"turn":      state.TurnState.Turn,
		"reason":    string(events.ReasonLifeBeat),
		"kind":      "encounter",
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   state.ID,
		OwnerUnitID: actor.ID,
		Code:        events.ReasonLifeBeat,
		Category:    events.CategoryLifecycle,
		Payload:     payload,
		WorldID:     state.WorldID,
	}); err != nil {
		return
	}
	service.pushRealtime(state.ID, "fate_life_beat", map[string]any{
		"unit_id":   actor.ID,
		"narrative": beat,
		"turn":      state.TurnState.Turn,
	})
}

// encounterRoll 是遭遇触发的确定性掷骰（含 turn：同一处不同拍可掷出不同结果）。[0,1)。
func encounterRoll(state State, actor unit.Record) float64 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(state.ID))
	_, _ = hasher.Write([]byte(actor.ID))
	_, _ = hasher.Write([]byte("fate_encounter"))
	_, _ = hasher.Write([]byte(fmt.Sprintf("%d,%d,%d", actor.Status.PositionQ, actor.Status.PositionR, state.TurnState.Turn)))
	return float64(hasher.Sum32()%10000) / 10000
}

// composeLifeBeatText 从一拍决策的叙事字段组装一句生活 beat（冠以角色名）。
// 优先取最像「她经历了什么」的字段：next_action（她接下来要做的）→ speak（她说的话）→ reasoning（她的盘算）→ memory（她记下的）。
// 全空则返回空串（调用方据此跳过，绝不写空 beat）。纯函数、确定性。
func composeLifeBeatText(actor unit.Record, decision unitDecisionPayload) string {
	gist := strings.TrimSpace(firstNonEmptyText(
		decision.NextAction,
		decision.Speak,
		decision.Reasoning,
		decision.Memory,
	))
	if gist == "" {
		return ""
	}
	// 剥掉 gist 开头冗余的自名：当 gist 取自她的对白（decision.Speak 常以自称名起头，如「丛仔，我先稳住」），
	// 直接 fmt 会拼成「丛仔：丛仔，我先稳住」自己喊自己名字。先去掉开头那一截「名字+分隔符」，避免重复。
	gist = stripLeadingSelfName(gist, actor.DisplayName())
	if gist == "" {
		return ""
	}
	return fmt.Sprintf("%s：%s", actor.DisplayName(), gist)
}

// stripLeadingSelfName 去掉文本开头冗余的「自名 + 常见分隔符（，,：: 、 空格）」前缀（可重复一次），
// 使生活 beat 不出现「名字：名字，…」的自我称名。纯函数、确定性；名字为空或无前缀则原样返回。
func stripLeadingSelfName(text, name string) string {
	out := strings.TrimSpace(text)
	name = strings.TrimSpace(name)
	if name == "" {
		return out
	}
	if strings.HasPrefix(out, name) {
		rest := strings.TrimLeft(strings.TrimPrefix(out, name), "，,：:、 \t")
		// 仅当剥掉名字后仍有实质内容才采用，避免把「丛仔」这种纯名字 beat 清空。
		if strings.TrimSpace(rest) != "" {
			out = strings.TrimSpace(rest)
		}
	}
	return out
}

// isMainWorldPlayerUnit 判断某单位是否为「主世界玩家角色」：本局绑定 world_default（页游主世界）
// 且该单位在 state.PlayerUnitIDs 里（即玩家亲自降生的角色，非 NPC/村民/敌方）。确定性、纯函数。
func isMainWorldPlayerUnit(state State, unitID string) bool {
	if strings.TrimSpace(unitID) == "" {
		return false
	}
	if strings.TrimSpace(state.WorldID) != defaultWorldID {
		return false
	}
	for _, id := range state.PlayerUnitIDs {
		if id == unitID {
			return true
		}
	}
	return false
}

// ===== ④ 后台自动 tick（flag QUNXIANG_FATE_AUTOTICK 默认关）：让她自己活，世界自己往前走 =====

// fateAutoTickEnabled 读 QUNXIANG_FATE_AUTOTICK（true/1/yes/on 视为开），默认关 → ticker 零行为
//（与既有 flag 风格一致；默认玩家手动指引/按钮即可推进，开启=世界自己往前走）。
func fateAutoTickEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(featureflags.EnvOrOverride("QUNXIANG_FATE_AUTOTICK"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// RunFateAutoTickLoop 是后台低频 ticker：每 interval 唤醒一次，flag QUNXIANG_FATE_AUTOTICK 开启时扫
// world_default 下的活跃主世界 session，各推一拍（AdvanceFateWorld）。默认关时零行为（每次唤醒只查一次 flag 即 return）。
//
// 成本：每拍 1 次 LLM 自治决策（每个被推进的 session 一拍）；低频（interval 默认 60s）+ best-effort + flag 默认关 控成本。
// 随 ctx 取消优雅退出（与 region-runner 同模式，main.go 启动并在关停信号时等其退出）。
func (service *Service) RunFateAutoTickLoop(ctx context.Context, interval time.Duration) {
	if service == nil {
		return
	}
	if interval <= 0 {
		interval = 60 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			service.runFateAutoTickPass(ctx)
		}
	}
}

// runFateAutoTickPass 跑一遍 ticker：flag 关时立即 return（零行为）；开时扫活跃主世界 session 各推一拍。
// 全程 best-effort：单 session 推进失败不影响其余；panic 兜底防一个坏 session 拖垮整个 ticker。
func (service *Service) runFateAutoTickPass(ctx context.Context) {
	if !fateAutoTickEnabled() {
		return // 默认关：零行为。
	}
	defer func() { _ = recover() }() // best-effort：异常不拖垮 ticker
	if service.sessions == nil {
		return
	}
	worldID, err := service.EnsureDefaultWorld(ctx)
	if err != nil {
		return
	}
	sessionIDs, err := service.sessions.ListMainWorldSessionIDs(ctx, worldID)
	if err != nil {
		return
	}
	for _, sessionID := range sessionIDs {
		// 各自吞错：一个 session 推不动不阻断其余（AdvanceFateWorld 内部已 best-effort）。
		_, _ = service.AdvanceFateWorld(ctx, sessionID)
	}
}
