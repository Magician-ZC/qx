package session

// 文件说明：大世界页游入口——「账号登录 → 捏人 → 降生进共享主世界 world_default 的账号绑定持久角色」。
//
// 关键洞察（无需新表）：session 已有 State.AccountID + State.WorldID，且 repository.go 把二者去规范化为
// single_player_sessions 的可查询列（(account_id, world_id) 复合索引）。所以「账号在主世界的角色」=
// 查 `WHERE account_id=? AND world_id=world_default` 的那一局玩家角色。
//
//   - ResumeMainWorldCharacter：登录后 resume，幂等持久——同账号任何设备登录拿到同一角色。
//   - CreateMainWorldCharacter：捏人降生，创建 1 个玩家角色（非 5 人选秀）+ 20 人村庄网 + 绑 world_default +
//     离线宪章落 desire/wound/redline + 保留敌方 NPC 阵营（战棋接管战需要对手）。**幂等**：若该账号已有
//     world_default 角色，返回既有的、绝不重复降生（防多设备/重复点击重复造人）。
//
// 与旧「单机选秀建局」（POST /api/sessions/single-player 的 draft 分支）的关系：那条路径不再是默认入口，
// 玩家入口改为命运开盒；战棋对局/战斗结算机制（battlefield/combat_roll/terrain_combat/executor_loop）全保留，
// 供「命运角色遇战 → 手动接管打战棋 → 打完回命运」的关键战接管使用。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/faction"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// MainWorldCharacterInput 是捏人降生的输入：玩家给主角定的「出身/夙愿/创伤/红线」。
// 这是命运开盒的角色契约——dream/wound 写进角色传记（决策 prompt 会读到）；desire/wound/redline 经离线宪章
// 落成长期目标 + 红线（喂归因校验 snap.Redlines），让 LLM 自治时朝夙愿努力、撞红线被硬门拦截。
type MainWorldCharacterInput struct {
	Name    string // 角色名（空→确定性占位名「无名」，由玩家后续改）
	Origin  string // 出身（写进 Identity.Lineage 与传记开头）
	Desire  string // 夙愿（长期目标，写进传记 + 离线宪章 LongTermGoals）
	Wound   string // 创伤（写进传记，作为性格底色）
	Redline string // 红线（绝对禁区，写进离线宪章 Redlines + 归因校验锚）
	// Faction 是玩家选择的阵营（freedom/order/chaos，亦容中文别名「自由/秩序/混乱」）。
	// 空/非法 → 据出身/夙愿启发选（resolveBirthFaction），最终恒落三阵营之一（默认 freedom）。
	// 决定玩家角色的 Faction + MoralAlignment（=该阵营道德基准）、出生据点 region、出生点公共 NPC 阵营。
	Faction string
}

// MainWorldCharacter 是「账号在主世界的角色」对外视图（resume / 降生都返回它）。
type MainWorldCharacter struct {
	HasCharacter   bool                   `json:"has_character"`             // 该账号在 world_default 是否已有角色
	SessionID      string                 `json:"session_id,omitempty"`      // 角色所在 session（命运/战棋推进都用它）
	UnitID         string                 `json:"unit_id,omitempty"`         // 玩家主角单位 ID
	Name           string                 `json:"name,omitempty"`            // 角色名
	WorldID        string                 `json:"world_id,omitempty"`        // 所属世界（恒 world_default）
	Origin         string                 `json:"origin,omitempty"`          // 出身
	Faction        string                 `json:"faction,omitempty"`         // 所属阵营（freedom/order/chaos），阵营开放世界 F1 引入
	SpawnRegion    string                 `json:"spawn_region,omitempty"`    // 出生据点 region（据 faction+seed 确定性落点）
	MoralAlignment faction.MoralAlignment `json:"moral_alignment,omitempty"` // 3 维数值道德轴（=阵营道德基准），供前端/F2 读
	Created        bool                   `json:"created,omitempty"`         // 本次调用是否新降生（幂等命中既有时为 false）
}

// ResumeMainWorldCharacter 查某账号在共享主世界 world_default 的持久角色（GET /api/me/character 的后端）。
// 幂等持久：同账号任何设备登录拿到同一角色。无角色返回 {HasCharacter:false}（前端据此进捏人）。
// 全程只读、零 LLM、零写：先解析 world_default，再走 (account_id, world_id) 索引查 session ID，载入取主角单位。
func (service *Service) ResumeMainWorldCharacter(ctx context.Context, accountID string) (MainWorldCharacter, error) {
	accountID = strings.TrimSpace(accountID)
	if service == nil || service.db == nil {
		return MainWorldCharacter{}, fmt.Errorf("resume main-world character: missing db")
	}
	if accountID == "" {
		return MainWorldCharacter{}, fmt.Errorf("resume main-world character: empty account")
	}
	worldID, err := service.EnsureDefaultWorld(ctx)
	if err != nil {
		return MainWorldCharacter{}, err
	}
	sessionID, found, err := service.sessions.FindMainWorldSessionID(ctx, accountID, worldID)
	if err != nil {
		return MainWorldCharacter{}, err
	}
	if !found {
		return MainWorldCharacter{HasCharacter: false}, nil
	}
	return service.mainWorldCharacterView(ctx, sessionID, worldID, false)
}

// mainWorldCharacterView 从一局已存在的主世界 session 组装对外角色视图（取首个玩家单位作主角）。
// created 标记本次是否新降生（resume 命中既有时传 false，降生时传 true）。
func (service *Service) mainWorldCharacterView(ctx context.Context, sessionID string, worldID string, created bool) (MainWorldCharacter, error) {
	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return MainWorldCharacter{}, err
	}
	view := MainWorldCharacter{
		HasCharacter: true,
		SessionID:    sessionID,
		WorldID:      worldID,
		Created:      created,
	}
	if len(state.PlayerUnitIDs) > 0 {
		view.UnitID = state.PlayerUnitIDs[0]
	}
	byID := make(map[string]unit.Record, len(units))
	for _, rec := range units {
		byID[rec.ID] = rec
	}
	if hero, ok := byID[view.UnitID]; ok {
		view.Name = hero.Identity.Name
		view.Origin = hero.Identity.Lineage
		view.Faction = hero.Faction
		view.MoralAlignment = hero.MoralAlignment
		view.SpawnRegion = faction.PickSpawnPoint(hero.Faction, state.RandomSeed)
	}
	return view, nil
}

// CreateMainWorldCharacter 捏人降生：在共享主世界 world_default 为某账号创建 1 个玩家角色 +
// 20 人出生关系网 + 保留敌方 NPC 阵营（战棋接管战需要对手）+ 离线宪章落 desire/wound/redline。
//
// 幂等（核心不变量，防重复降生）：起始处查该账号在 world_default 是否已有角色——有则直接返回既有的、
// 绝不重复建局/造人（多设备同时登录、重复点击「降生」都安全）。
//
// 单玩家角色而非 5 人选秀：玩家入口是命运开盒，主角只有 1 个。敌方 NPC 阵营仍生成（server-authoritative，
// 敌方全局策略由 LLM 在执行期生成，与旧局一致），供「命运角色遇关键战 → 手动接管打战棋」时有对手。
func (service *Service) CreateMainWorldCharacter(ctx context.Context, accountID string, in MainWorldCharacterInput) (MainWorldCharacter, error) {
	accountID = strings.TrimSpace(accountID)
	if service == nil || service.db == nil {
		return MainWorldCharacter{}, fmt.Errorf("create main-world character: missing db")
	}
	if accountID == "" {
		return MainWorldCharacter{}, fmt.Errorf("create main-world character: empty account")
	}

	// world_default 必须先于幂等查询解析（resume 与降生共用同一世界锚）。
	worldID, err := service.EnsureDefaultWorld(ctx)
	if err != nil {
		return MainWorldCharacter{}, err
	}
	// 幂等守卫：该账号在 world_default 已有角色 → 返回既有，绝不重复降生。
	if sessionID, found, findErr := service.sessions.FindMainWorldSessionID(ctx, accountID, worldID); findErr != nil {
		return MainWorldCharacter{}, findErr
	} else if found {
		return service.mainWorldCharacterView(ctx, sessionID, worldID, false)
	}

	if err := events.SeedReasonCodeCatalog(ctx, service.db); err != nil {
		return MainWorldCharacter{}, err
	}

	now := time.Now().UTC()
	seed := now.UnixNano()
	sessionID := uuid.NewString()
	selectedMapScriptID := normalizeBattlefieldScriptID("", seed)
	selectedMapScriptName := battlefieldScriptDisplayName(selectedMapScriptID)
	selectedMapSize := battlefieldSizeByID(BattlefieldSizeSmall)

	state := State{
		ID:                   sessionID,
		AccountID:            accountID, // 账号绑定（随 state_json + account_id 列落库），resume 的 (account_id, world_id) 查询键
		Mode:                 ModeSinglePlayer,
		RandomSeed:           seed,
		PlayerFactionID:      "player",
		EnemyFactionID:       "enemy",
		SetupPhase:           SetupPhaseReady,
		DraftRequiredPick:    1, // 命运入口主角 1 人（非选秀）
		TurnState:            turns.NewState(now, turns.DefaultBudgets()),
		Outcome:              OutcomeOngoing,
		VictoryPath:          VictoryPathNone,
		Weather:              weatherForTurnBySeed(seed, 1),
		Map:                  generateBattlefieldWithSize(sessionID, seed, selectedMapScriptID, selectedMapSize.ID),
		MapScriptID:          selectedMapScriptID,
		MapScriptName:        selectedMapScriptName,
		MapSizeID:            selectedMapSize.ID,
		MapSizeName:          selectedMapSize.DisplayName,
		FogOfWarEnabled:      false,
		RandomEventsDisabled: false,
		CommandPower:         defaultCommandPower(),
		FactionRelations:     []FactionRelation{},
		Structures:           []Structure{},
		DirectiveHistory:     []Directive{},
		DialogueHistory:      []DialogueMessage{},
		DecisionTraces:       []DecisionTrace{},
		LLMInteractions:      []LLMInteraction{},
		PigeonQueue:          []PigeonDispatch{},
		BattleReports:        []BattleReport{},
		IntelAssets:          []IntelAsset{},
		IntelReports:         []IntelReport{},
		ModerationReports:    []ModerationReport{},
		Metrics:              SessionMetrics{},
		RawEventLog:          []RawEventEntry{},
		Logs:                 []LogEntry{},
		CreatedAt:            now,
		UpdatedAt:            now,
	}

	// 阵营开放世界 F1：解析玩家阵营（空/非法 → 据出身/夙愿启发选，最终恒落三阵营之一），
	// 并据 faction + seed 确定性选出生据点 region。
	chosenFaction := resolveBirthFaction(in)
	spawnRegion := faction.PickSpawnPoint(chosenFaction, seed)

	// 玩家主角（1 人）：复用 bootstrapBattleUnit（与建局同一造人底座），再覆写捏人字段（名字/出身/传记）。
	heroName := strings.TrimSpace(in.Name)
	if heroName == "" {
		heroName = "无名"
	}
	hero, err := bootstrapBattleUnit(seed+1, sessionID, state.PlayerFactionID, heroName, world.Coord{Q: 1, R: 3})
	if err != nil {
		return MainWorldCharacter{}, err
	}
	applyMainWorldPersona(&hero, in)
	// 玩家角色的阵营 + 道德轴（=该阵营道德基准）：非保护字段，直接写（不走 Mutator，仿 Ambition）。
	hero.Faction = chosenFaction
	hero.MoralAlignment = faction.BaselineFor(chosenFaction)
	if err := service.units.Save(ctx, hero); err != nil {
		return MainWorldCharacter{}, err
	}
	state.PlayerUnitIDs = append(state.PlayerUnitIDs, hero.ID)

	// 阵营开放世界 F1：不再播种固定敌方 NPC（EnemyUnitIDs 留空）。开放世界没有「固定对手阵营」，
	// 战斗对手（PvE 威胁/跨阵营遭遇）由 F3 在游历相遇时动态接入；当前留空对 session 安全
	// （updateOutcome 对「从未配过敌方」的开放世界局短路、不误判胜负——见 updateOutcome 注释）。
	state.EnemyUnitIDs = []string{}

	appendDirective(&state, Directive{
		ID:        uuid.NewString(),
		Turn:      1,
		Phase:     turns.PhaseDeployment,
		Kind:      DirectiveKindDoctrine,
		Text:      "顺着本心活下去，护住在乎的人，再图谋夙愿。",
		Priority:  "normal",
		IssuedAt:  now,
		IssuedBy:  "player",
		AppliesTo: state.PlayerFactionID,
	})
	appendLog(&state, "setup", fmt.Sprintf("%s 降生于主世界。命运开盒就此展开。", heroName), hero.ID, "")
	appendLog(&state, "weather", fmt.Sprintf("本回合天气：%s。%s", state.Weather.DisplayName, state.Weather.Note), "", "")
	ensureFactionRelations(&state)

	// 离线宪章：把捏人的 desire/wound/redline 落成长期目标 + 红线（喂归因校验 snap.Redlines、驱动 LLM 自治）。
	applyMainWorldCharter(&state, hero.ID, in)

	// 接入共享主世界（必须在玩家单位已落库之后、村庄/ambient 锚落库之前——使 state.WorldID 置位，
	// 跨玩家锚带正确世界域）。best-effort：吞错不阻断降生。注意：bindSessionWorld 受 QUNXIANG_WORLD_BINDING
	// 控制，默认 shared → world_default；若被设为 off/per_session，state.WorldID 可能非 world_default，
	// 下方做一次最终校正，保证主世界角色恒绑 world_default（否则 resume 查不到）。
	_ = service.bindSessionWorld(ctx, &state)
	if strings.TrimSpace(state.WorldID) != worldID {
		// QUNXIANG_WORLD_BINDING=off/per_session 时强制把主世界角色锚回 world_default（页游入口的硬契约）。
		if err := service.AssignSessionToWorld(ctx, &state, worldID); err != nil {
			return MainWorldCharacter{}, err
		}
	}

	// 离线调度 seed（开关关时 no-op，best-effort）。
	_ = service.seedAmbientForUnits(ctx, sessionID, state.WorldID, state.PlayerUnitIDs)
	// 阵营开放世界 F1：在出生据点播种 8–12 个公共同阵营 NPC（替换原 20 人私人村庄网）。
	// 公共而非私人——不建玩家↔NPC 的 relations 行，关系靠后天游历相遇结成。best-effort、幂等。
	service.SeedFactionSpawnBestEffort(ctx, sessionID, chosenFaction, spawnRegion, seed+1)

	if err := service.syncCombatFlags(ctx, &state, nil); err != nil {
		return MainWorldCharacter{}, err
	}
	// 并发降生 TOCTOU 硬兜底（H2）：query-first 幂等守卫（上方 FindMainWorldSessionID）与此处终写之间窗口很宽
	// （夹 EnsureDefaultWorld / 4 单位 / 20 村民 / bind world 等数十秒）；两个并发请求可能各自越过守卫、各插一行
	// 不同 uuid id 的 session，致同账号在 world_default 出现两个角色。spine 已加唯一索引
	// uniq_single_player_sessions_account_world(account_id, world_id)，故并发竞态中的「输家」此处 Save 必触唯一冲突——
	// 这等价于「另一个并发请求已为本账号降生」，回退查既有角色返回（与 world_boss dup-key、world.Join 撞唯一键再 Get
	// 的兜底同模式）。输家先前 Save 的孤儿单位行（hero/enemy）因无对应 session 行、永不被 FindMainWorldSessionID 触达，
	// 属无害残留（不进任何账本/不被 resume）。
	if err := service.sessions.Save(ctx, &state); err != nil {
		if isDupKeyErr(err) {
			if existingID, found, findErr := service.sessions.FindMainWorldSessionID(ctx, accountID, worldID); findErr == nil && found {
				return service.mainWorldCharacterView(ctx, existingID, worldID, false)
			}
		}
		return MainWorldCharacter{}, err
	}

	// 阵营开放世界 F1：开放世界局无固定敌方阵营（EnemyUnitIDs 留空），跳过敌方全局策略首刷
	// （否则会为不存在的对手白白发一次 LLM 调用）。仅当本局确有敌方单位时才刷（战斗对手 F3 动态接入后此分支自然恢复）。
	loadedState, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return MainWorldCharacter{}, err
	}
	state = loadedState
	if len(state.EnemyUnitIDs) > 0 {
		service.refreshEnemyGlobalDirectiveForDeploymentPhase(ctx, &state, units, "deployment_phase_started")
		if err := service.sessions.Save(ctx, &state); err != nil {
			return MainWorldCharacter{}, err
		}
	}
	if err := service.recordPhaseBoundarySnapshot(ctx, &state, nil); err != nil {
		return MainWorldCharacter{}, err
	}

	return service.mainWorldCharacterView(ctx, sessionID, worldID, true)
}

// resolveBirthFaction 解析玩家选择的阵营，确保最终恒落三阵营之一：
//   - 显式给了合法阵营（freedom/order/chaos，或中文别名「自由/秩序/混乱」）→ 直接用。
//   - 空/非法 → 据出身/夙愿做关键词启发式选阵营（自由/秩序/混乱各有触发词）；无命中默认 freedom。
//
// 确定性、纯字符串匹配（无 LLM、无随机）：同一输入永远解析同一阵营，可复现。
func resolveBirthFaction(in MainWorldCharacterInput) string {
	if fid := faction.Normalize(in.Faction); fid != "" {
		return fid
	}
	hay := strings.ToLower(strings.TrimSpace(in.Origin + " " + in.Desire + " " + in.Wound))
	if hay != "" {
		// 秩序触发词：律法/规矩/守护/官府等。
		for _, kw := range []string{"律", "法", "规", "守", "护", "官", "府", "秩序", "教谕", "卫"} {
			if strings.Contains(hay, kw) {
				return faction.IDOrder
			}
		}
		// 混乱触发词：复仇/破坏/废墟/亡命等。
		for _, kw := range []string{"仇", "恨", "复仇", "破", "废", "乱", "逃", "盗", "黑市", "亡命"} {
			if strings.Contains(hay, kw) {
				return faction.IDChaos
			}
		}
		// 自由触发词：游侠/流浪/自由/野等（命中即归自由；其余落默认自由）。
		for _, kw := range []string{"侠", "游", "浪", "自由", "野", "牧", "漂"} {
			if strings.Contains(hay, kw) {
				return faction.IDFreedom
			}
		}
	}
	return faction.IDFreedom
}

// applyMainWorldPersona 把捏人的出身/夙愿/创伤覆写进角色身份与传记（决策 prompt 会读 Biography）。
// 确定性、纯内存改写：出身写进 Lineage。**注意**：这会让主角 Lineage 命中 village.go isSeededVillagerRecord 的
// 村民 Lineage 指纹（如「边境猎户」），与村民无法靠指纹区分；因此村庄幂等守卫 sessionAlreadyHasVillage **必须靠
// state.PlayerUnitIDs 显式剔除玩家主角**后再判，不能依赖「中文 Gender 主指纹会自动避开主角」（主角 Gender 是英文
// male/female 确实不命中 Gender 指纹，但会命中 Lineage 指纹——曾因此把带出身的主角误判为村民，致 20 人村庄被整体跳过，
// 即 H1）。
func applyMainWorldPersona(record *unit.Record, in MainWorldCharacterInput) {
	if record == nil {
		return
	}
	origin := strings.TrimSpace(in.Origin)
	if origin != "" {
		record.Identity.Lineage = origin
	}
	var b strings.Builder
	if origin != "" {
		b.WriteString(origin)
		b.WriteString("出身。")
	}
	if desire := strings.TrimSpace(in.Desire); desire != "" {
		b.WriteString("她心里一直惦记着一件事：")
		b.WriteString(desire)
		b.WriteString("。")
	}
	if wound := strings.TrimSpace(in.Wound); wound != "" {
		b.WriteString("她身上有一道难以愈合的伤：")
		b.WriteString(wound)
		b.WriteString("。")
	}
	if bio := strings.TrimSpace(b.String()); bio != "" {
		record.Identity.Biography = bio
	}
}

// applyMainWorldCharter 把捏人的 desire/wound/redline 落成主角的离线宪章（玩家不在场时单位据此自治）。
//   - Desire → LongTermGoals：日常自治朝夙愿努力（charterContextForUnit 拼进决策 prompt）。
//   - Wound  → LongTermGoals 的一条背景目标（「带着这道伤活下去」），让创伤参与目标重估。
//   - Redline → Redlines：绝对禁区，喂归因校验 snap.Redlines + buildRelevanceAnchors 的红线锚（硬门拦截）。
//
// 三段皆空时不写宪章（CharterIsEmpty），保持轻量。
func applyMainWorldCharter(state *State, unitID string, in MainWorldCharacterInput) {
	if state == nil || strings.TrimSpace(unitID) == "" {
		return
	}
	charter := OfflineCharter{}
	if desire := strings.TrimSpace(in.Desire); desire != "" {
		charter.LongTermGoals = append(charter.LongTermGoals, desire)
	}
	if wound := strings.TrimSpace(in.Wound); wound != "" {
		charter.LongTermGoals = append(charter.LongTermGoals, "带着这道伤活下去："+wound)
	}
	if redline := strings.TrimSpace(in.Redline); redline != "" {
		charter.Redlines = append(charter.Redlines, CharterRedline{Text: redline, Severity: "hard"})
	}
	if CharterIsEmpty(charter) {
		return
	}
	SetUnitCharter(state, unitID, charter)
}
