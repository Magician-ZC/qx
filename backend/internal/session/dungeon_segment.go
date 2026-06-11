package session

// 文件说明：副本(Dungeon)异步分段推进（设计 docs/PvE威胁系统.md §3-5）。
// 把 dungeon.go 的同步核心切成「逐段(每段=1 floor)可中断、跨回合续跑」的异步推进态：
//   - DungeonSegment 持久化到 dungeon_segments 表（队员HP/贡献快照 + boss_hp_remaining/floor_round 断点续跑 +
//     awards_accumulated_json 累计已清层 + left_at 玩家离场计时）。
//   - RunDungeonSegment 推进当前层一段 combat_roll，返回 NextAction 枚举（继续下层/末层boss首触暂停/濒死撤退抉择暂停/
//     通关/撤退/团灭）。关键节点经 events.EmitProcessEvent 写 DUNGEON_SEGMENT_PAUSE + SurfaceFateEvent 路由命运卡。
//   - ResumePausedDungeonSegment 玩家回来后据选择(继续/撤退)续跑；ResolveDungeonTimeout 扫 left_at 超时段→宪章兜底
//     见好就收（保留已得 loot、轻惩 morale，绝不触碰 lives），写 DUNGEON_CHARTER_TIMEOUT。
//   - propagateDungeonEvent 每段完成/濒死按 relevance.Score 找在乎她的人投收件箱（复用 loadThreatBystanders 模式）。
// 复用 dungeon.go 的 scaledDungeonFloor/runDungeonFloorCore/settleDungeon（同口径、确定性）。
// flag：复用 QUNXIANG_DUNGEON（默认关）；关时所有入口零行为。best-effort 副路（命运/传播）吞错只记日志，绝不阻断结算。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
)

// DungeonNextAction 是推进一段副本后的下一步指示（驱动主控/前端决定是续跑、暂停待玩家、还是终局）。
type DungeonNextAction string

const (
	// DungeonContinueNextFloor：本层已清，可推进下一层（普通层之间无需打扰玩家，可自动 continue）。
	DungeonContinueNextFloor DungeonNextAction = "continue_next_floor"
	// DungeonPauseFirstContact：抵达末层 boss 首次接触，暂停进收件箱（FirstContact 关键节点，§3）。
	DungeonPauseFirstContact DungeonNextAction = "pause_first_contact"
	// DungeonPausePlayerDecision：有队员濒死（HP 反射阈值 <0.25），暂停问玩家「要不要撤」（§3/§4 濒死保命）。
	DungeonPausePlayerDecision DungeonNextAction = "pause_player_decision"
	// DungeonCompletedCleared：跑满全部层、通关。
	DungeonCompletedCleared DungeonNextAction = "completed_cleared"
	// DungeonCompletedFled：撤退（保留已得 loot）。
	DungeonCompletedFled DungeonNextAction = "completed_fled"
	// DungeonCompletedWiped：团灭（不分赃、各自分级惩罚）。
	DungeonCompletedWiped DungeonNextAction = "completed_wiped"
)

// dungeon_segments 的 state 列取值。
const (
	dungeonSegStateInProgress = "in_progress" // 推进中（一段刚跑完待续/可自动续下层）
	dungeonSegStatePaused     = "paused"      // 暂停在关键节点（待玩家决策；pause_reason 记暂停类型）
	dungeonSegStateCompleted  = "completed"   // 终局（cleared/fled/wiped，已 settle 落库）
)

// pause_reason 列取值（暂停在收件箱的关键节点类型）。
const (
	dungeonPauseReasonFirstContact = "first_contact"   // 末层 boss 首次接触
	dungeonPauseReasonPlayerDying  = "player_decision" // 濒死撤退抉择
)

// DungeonCharterTimeout 是玩家离场后自动兜底的超时阈值（确定性比较，绝不依赖 wall-clock 随机）。
// 设计 §3「超时按 charter 兜底续命」：玩家没回来，照着早先的叮嘱见好就收。12h 与设计回响卡口径一致。
const DungeonCharterTimeout = 12 * time.Hour

// dungeonSegPlayerChoice 是 ResumePausedDungeonSegment 的玩家选择枚举。
const (
	DungeonChoiceContinue = "continue" // 继续深入
	DungeonChoiceRetreat  = "retreat"  // 见好就收，撤离（保留已得 loot）
)

// dungeonMemberSnapshot 是一名队员的跨段持久化快照（members_state_json 的元素）。
// 存 HP/贡献四分量/状态，使断点续跑时能精确还原战斗态（不回库重取，避免与并发战斗写打架）。
type dungeonMemberSnapshot struct {
	UnitID string  `json:"unit_id"`
	HP     int     `json:"hp"`
	Status string  `json:"status"` // contributed / fled / down
	Damage float64 `json:"damage"` // 累计输出贡献
	Tank   float64 `json:"tank"`   // 累计承伤贡献
	Role   float64 `json:"role,omitempty"`
	Risk   float64 `json:"risk,omitempty"`
	Clutch float64 `json:"clutch,omitempty"`
	Dealt  int     `json:"dealt"`
	Taken  int     `json:"taken"`
}

// dungeonAwardSnapshot 是累计已清层战利品的一条快照（awards_accumulated_json 的元素）。
// 注意：这里只累计「已清层的 loot 规格」（ID+稀有度+数量），分赃归属在终局一次性 settle 时由 AllocateLoot 仲裁，
// 与同步核心同口径（确定性、付费不进）——绝不在分段中途把排他件提前判给谁。
type dungeonAwardSnapshot struct {
	Floor    int    `json:"floor"`
	IsBoss   bool   `json:"is_boss"`
	ItemID   string `json:"item_id"`
	Rarity   string `json:"rarity"`
	Quantity int    `json:"quantity"`
}

// DungeonSegment 是一次副本异步推进的持久化态（落 dungeon_segments 表）。
type DungeonSegment struct {
	ID           string
	DungeonRunID string
	SessionID    string
	UnitIDs      []string
	Floors       int                     // 计划总层数
	Floor        int                     // 当前推进到的层（1..Floors）
	EnteredTurn  int                     // 踏入副本时的回合（L1：钉死 combat_roll 骰序，使 resume 骰序与玩家何时回来无关）
	State        string                  // in_progress / paused / completed
	Members      []dungeonMemberSnapshot // 队员 HP/贡献快照
	BossHPRemain int                     // 当前层 boss 剩余血（断点续跑用，0=本层未开打或已清）
	FloorRound   int                     // 当前层已进行到的回合数（断点续跑用）
	Awards       []dungeonAwardSnapshot  // 累计已清层战利品规格
	PauseReason  string                  // paused 时的暂停类型（已剥离失败计数后缀，对消费方呈现裸 kind）
	StartedAt    string
	LeftAt       sql.NullString // 玩家离场时刻（charter 超时计时；NULL=玩家在场）
	UpdatedAt    string
	// TimeoutFailures 是该段超时兜底连续失败次数（M1 防卡死重试），从 pause_reason 列的 failed:<n> 后缀反解，仅内存。
	TimeoutFailures int
	// Outcome 仅 completed 时有意义（cleared/fled/wiped），从 state/最后一段推导，不落独立列（复用 pause_reason 语义边界）。
	Outcome string
}

// DungeonSegmentResult 是推进/恢复一段副本的返回（NextAction + 该段战报 + 暂停时的祖魂卡）。
type DungeonSegmentResult struct {
	SegmentID   string
	NextAction  DungeonNextAction
	Floor       int
	FloorResult *DungeonFloorResult // 本段跑的那一层结果（暂停在 first_contact 且未开打时可为 nil）
	PauseCard   string              // 暂停时投进收件箱的祖魂语气卡（continue/终局时为空）
	Outcome     string              // 终局时的 cleared/fled/wiped
}

// StartDungeonAsync 创建一次副本异步推进的首段并落库（不立即推进任何战斗）。
// flag 关时返回 ErrDungeonDisabled、零行为。成功返回首段（state=in_progress，floor=1，全员满血快照）。
func (service *Service) StartDungeonAsync(ctx context.Context, sessionID string, unitIDs []string, floors int) (*DungeonSegment, error) {
	if !dungeonEnabled() {
		return nil, ErrDungeonDisabled
	}
	if service == nil || service.db == nil || service.units == nil {
		return nil, fmt.Errorf("start dungeon async: missing dependencies")
	}
	if len(unitIDs) == 0 {
		return nil, fmt.Errorf("start dungeon async: empty party")
	}
	if floors < dungeonMinFloors {
		floors = dungeonMinFloors
	}
	if floors > dungeonMaxFloors {
		floors = dungeonMaxFloors
	}

	members := make([]dungeonMemberSnapshot, 0, len(unitIDs))
	for _, id := range unitIDs {
		rec, err := service.units.GetByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("load party member %s: %w", id, err)
		}
		members = append(members, dungeonMemberSnapshot{
			UnitID: rec.ID,
			HP:     rec.Status.HP,
			Status: "contributed",
		})
	}

	// L1：踏入回合钉死——用建段时刻的 live Turn 作为整段副本 combat_roll 的回合 salt，
	// 使后续 resume/兜底续跑的骰序与玩家何时回来（live Turn 可能已漂移）无关，确定性可复现。
	enteredTurn := 0
	worldID := ""
	if state := service.loadStateForFate(ctx, sessionID); state != nil {
		enteredTurn = state.TurnState.Turn
		worldID = state.WorldID
	}

	// 进入闸（模块3：每日次数/冷却）：异步副本与同步同口径，在落首段（=真正踏入）前校验并消费一次进入名额。
	// 队伍以队长=首位单位计名额；flag 关时恒放行、零行为；DB 故障 best-effort 放行（仅记 warn）。
	if leaderID := unitIDs[0]; leaderID != "" {
		allowed, _, lockErr := service.checkAndConsumeDungeonEntry(ctx, worldID, leaderID, dungeonLockoutKey)
		if lockErr != nil {
			slog.WarnContext(ctx, "dungeon async lockout gate error; allowing entry", "session_id", sessionID, "unit_id", leaderID, "error", lockErr)
		}
		if !allowed {
			return nil, ErrDungeonDailyCapReached
		}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	seg := &DungeonSegment{
		ID:           "dgseg_" + uuid.NewString(),
		DungeonRunID: "dungeon_" + uuid.NewString(),
		SessionID:    sessionID,
		UnitIDs:      append([]string(nil), unitIDs...),
		Floors:       floors,
		Floor:        1,
		EnteredTurn:  enteredTurn,
		State:        dungeonSegStateInProgress,
		Members:      members,
		BossHPRemain: 0,
		FloorRound:   0,
		Awards:       []dungeonAwardSnapshot{},
		StartedAt:    now,
		UpdatedAt:    now,
	}
	if err := service.saveDungeonSegment(ctx, seg); err != nil {
		return nil, err
	}
	return seg, nil
}

// RunDungeonSegment 推进当前层一段 combat_roll（满血则从第 1 回合开打，否则从 boss_hp_remaining/floor_round 续跑）。
// 返回 NextAction：continue_next_floor / pause_first_contact / pause_player_decision / completed_*。每段都落库。
// 关键节点暂停（first_contact / player_decision）经 events.EmitProcessEvent 写 DUNGEON_SEGMENT_PAUSE + SurfaceFateEvent 投命运卡。
func (service *Service) RunDungeonSegment(ctx context.Context, seg *DungeonSegment) (DungeonSegmentResult, error) {
	return service.runDungeonSegment(ctx, seg, false)
}

// runDungeonSegment 是 RunDungeonSegment 的内部体。skipDyingCheck=true 时跳过「濒死撤退抉择」暂停（玩家刚选了 continue，
// 不应在同一段又因同一个濒死队员立刻再暂停——交给 runDungeonFloorCore 的反射护栏自动让其退场）。
func (service *Service) runDungeonSegment(ctx context.Context, seg *DungeonSegment, skipDyingCheck bool) (DungeonSegmentResult, error) {
	res := DungeonSegmentResult{}
	if !dungeonEnabled() {
		return res, ErrDungeonDisabled
	}
	if service == nil || service.db == nil || service.mutator == nil || seg == nil {
		return res, fmt.Errorf("run dungeon segment: missing dependencies")
	}
	res.SegmentID = seg.ID
	res.Floor = seg.Floor
	if seg.State == dungeonSegStateCompleted {
		// 已终局：幂等返回（不重复结算）。
		res.NextAction = dungeonOutcomeToAction(seg.Outcome)
		res.Outcome = seg.Outcome
		return res, nil
	}

	state := service.dungeonSegmentState(ctx, seg)
	run, err := service.rebuildDungeonRun(ctx, seg, state)
	if err != nil {
		return res, err
	}

	floor := seg.Floor
	isBoss := floor == seg.Floors
	threat := scaledDungeonFloor(run.state, run.members, floor, run.floors)

	// 末层 boss 首次接触（本层从未开打：boss_hp_remaining==0 且 floor_round==0）→ 暂停问玩家（FirstContact §3），不打第一枪。
	if isBoss && seg.BossHPRemain == 0 && seg.FloorRound == 0 {
		card := dungeonFirstContactCard(floor, threat.Name)
		res.NextAction = DungeonPauseFirstContact
		res.PauseCard = card
		seg.State = dungeonSegStatePaused
		seg.PauseReason = dungeonPauseReasonFirstContact
		// 标记 boss 已就绪（boss_hp_remaining 写满血、floor_round=0），玩家 resume(continue) 时从满血第 1 回合开打。
		seg.BossHPRemain = threat.HPPool
		if err := service.saveDungeonSegment(ctx, seg); err != nil {
			return res, err
		}
		service.emitDungeonPause(ctx, run.state, seg, card, dungeonPauseReasonFirstContact)
		return res, nil
	}

	// 濒死撤退抉择（§3/§4）：本层开打前若有仍在战、HP 已跌破反射阈值的队员 → 暂停问玩家「要不要整队撤」，
	// 不径直把她推进下一回合送死。玩家刚选 continue（skipDyingCheck）则不再为同一濒死队员重复暂停。
	if !skipDyingCheck && dungeonHasDyingMember(run) {
		card := dungeonDyingCard(floor)
		res.NextAction = DungeonPausePlayerDecision
		res.PauseCard = card
		seg.State = dungeonSegStatePaused
		seg.PauseReason = dungeonPauseReasonPlayerDying
		if err := service.saveDungeonSegment(ctx, seg); err != nil {
			return res, err
		}
		service.emitDungeonPause(ctx, run.state, seg, card, dungeonPauseReasonPlayerDying)
		return res, nil
	}

	// 续跑起点：满血(==threat.HPPool 或首开)从第 1 回合；否则从留库的 boss_hp_remaining/floor_round+1 续。
	mobHP := seg.BossHPRemain
	startRound := seg.FloorRound + 1
	if mobHP <= 0 {
		mobHP = threat.HPPool
		startRound = 1
	}

	floorRes, terminal, err := service.runDungeonFloorCore(ctx, run, floor, threat, mobHP, startRound)
	if err != nil {
		return res, err
	}
	res.FloorResult = &floorRes

	// 把战后的队员战斗态/层结果同步回 segment 快照。
	syncMembersToSnapshot(seg, run)

	switch {
	case floorRes.Outcome == "cleared":
		// 本层清了：累计本层 loot，boss 血/回合清零。
		appendClearedLoot(seg, floor, isBoss)
		seg.BossHPRemain = 0
		seg.FloorRound = 0
		// 旁观传播（best-effort）：清层是一桩值得在乎她的人知晓的小高光。
		service.bestEffortPropagateDungeon(ctx, run.state, seg, floor, false)
		if floor >= seg.Floors {
			// 跑满全部层 → 通关终局。
			return service.completeDungeonSegment(ctx, seg, run, "cleared", &res)
		}
		// 推进下一层（普通层之间自动 continue，不打扰玩家）。
		seg.Floor = floor + 1
		seg.State = dungeonSegStateInProgress
		res.NextAction = DungeonContinueNextFloor
		if err := service.saveDungeonSegment(ctx, seg); err != nil {
			return res, err
		}
		return res, nil

	case terminal == "fled":
		// 有人濒死先反射撤了，本层未清 → 撤退终局（保留已清层 loot）。
		// 但若是「单步濒死」语义（仍有可战之人、只是某人触阈值退场），暂停问玩家是否整队撤——这里区分：
		// runDungeonFloorCore 的 fled 表示「全队已无可战之人」，是确定终局；个体濒死的暂停在下方反射阈值检查里。
		service.bestEffortPropagateDungeon(ctx, run.state, seg, floor, true)
		return service.completeDungeonSegment(ctx, seg, run, "fled", &res)

	case terminal == "wiped":
		service.bestEffortPropagateDungeon(ctx, run.state, seg, floor, true)
		return service.completeDungeonSegment(ctx, seg, run, "wiped", &res)

	default:
		// runDungeonFloorCore 只会在 cleared/fled/wiped 返回；其它视为异常终局保护（不应到达）。
		return service.completeDungeonSegment(ctx, seg, run, floorRes.Outcome, &res)
	}
}

// ResumePausedDungeonSegment 玩家回来后据选择续跑一段暂停的副本（设计 §3 恢复入口）。
// playerChoice=continue → 从断点续跑当前层；retreat → 见好就收撤离（保留已得 loot、轻惩，绝不触碰 lives）。
// flag 关时返回 ErrDungeonDisabled。
func (service *Service) ResumePausedDungeonSegment(ctx context.Context, state *State, segmentID string, playerChoice string) (DungeonSegmentResult, error) {
	res := DungeonSegmentResult{SegmentID: segmentID}
	if !dungeonEnabled() {
		return res, ErrDungeonDisabled
	}
	if service == nil || service.db == nil {
		return res, fmt.Errorf("resume dungeon segment: missing db")
	}
	seg, err := service.loadDungeonSegment(ctx, segmentID)
	if err != nil {
		return res, err
	}
	// M5：授权——段必须属于该会话（state.ID==sessionID），否则跨会话越权恢复。归一为「未找到」错（让路由映 404）。
	if seg == nil || (state != nil && state.ID != "" && seg.SessionID != state.ID) {
		return res, fmt.Errorf("resume dungeon segment: segment %s not found", segmentID)
	}
	if seg.State == dungeonSegStateCompleted {
		res.NextAction = dungeonOutcomeToAction(seg.Outcome)
		res.Outcome = seg.Outcome
		return res, nil
	}
	// 玩家回来了：清掉离场计时（left_at→NULL），不再被 charter 超时兜底。
	seg.LeftAt = sql.NullString{}

	choice := strings.ToLower(strings.TrimSpace(playerChoice))
	if choice == DungeonChoiceRetreat {
		// 见好就收：当前层不再续打，按撤退终局结算（保留已清层 loot），轻惩 morale（经 Mutator，绝不触 lives）。
		run, err := service.rebuildDungeonRun(ctx, seg, service.dungeonSegmentState(ctx, seg))
		if err != nil {
			return res, err
		}
		return service.completeDungeonSegment(ctx, seg, run, "fled", &res)
	}

	// 继续：从暂停态续跑当前层。把 paused→in_progress，交回推进。若刚是「濒死撤退抉择」暂停而玩家选了继续，
	// 跳过同段的濒死再暂停（交给反射护栏自动让濒死者退场），避免对同一濒死队员死循环暂停。
	wasDying := seg.PauseReason == dungeonPauseReasonPlayerDying
	seg.State = dungeonSegStateInProgress
	seg.PauseReason = ""
	if err := service.saveDungeonSegment(ctx, seg); err != nil {
		return res, err
	}
	return service.runDungeonSegment(ctx, seg, wasDying)
}

// RunDungeonSegmentByID 是 /run 路由的薄导出包装：按 segmentID 加载段后推进一段（内部 loadDungeonSegment + RunDungeonSegment）。
// 段不存在返回错误（路由把它当 404/400）。flag 关时 RunDungeonSegment 已返回 ErrDungeonDisabled。
func (service *Service) RunDungeonSegmentByID(ctx context.Context, sessionID, segmentID string) (DungeonSegmentResult, error) {
	if !dungeonEnabled() {
		return DungeonSegmentResult{SegmentID: segmentID}, ErrDungeonDisabled
	}
	if service == nil || service.db == nil {
		return DungeonSegmentResult{SegmentID: segmentID}, fmt.Errorf("run dungeon segment by id: missing db")
	}
	seg, err := service.loadDungeonSegment(ctx, segmentID)
	if err != nil {
		return DungeonSegmentResult{SegmentID: segmentID}, err
	}
	// M5：授权——段必须属于该会话，否则跨会话越权访问。归一为「未找到」错（让路由映 404，不泄露段是否存在）。
	if seg == nil || seg.SessionID != sessionID {
		return DungeonSegmentResult{SegmentID: segmentID}, fmt.Errorf("run dungeon segment by id: segment %s not found", segmentID)
	}
	return service.RunDungeonSegment(ctx, seg)
}

// ResumeDungeonSegmentByID 是 /resume 路由的薄导出包装：按 sessionID 载 state 后据 choice 续跑一段暂停的副本
// （内部 loadStateForFate + ResumePausedDungeonSegment）。flag 关时 ResumePausedDungeonSegment 已返回 ErrDungeonDisabled。
func (service *Service) ResumeDungeonSegmentByID(ctx context.Context, sessionID, segmentID, choice string) (DungeonSegmentResult, error) {
	if !dungeonEnabled() {
		return DungeonSegmentResult{SegmentID: segmentID}, ErrDungeonDisabled
	}
	if service == nil || service.db == nil {
		return DungeonSegmentResult{SegmentID: segmentID}, fmt.Errorf("resume dungeon segment by id: missing db")
	}
	state := service.loadStateForFate(ctx, sessionID)
	if state == nil {
		state = &State{ID: sessionID}
	}
	return service.ResumePausedDungeonSegment(ctx, state, segmentID, choice)
}

// dungeonHasDyingMember 判断是否有「仍在战、HP 已跌破反射撤退阈值」的队员（濒死撤退抉择的触发条件）。
// 与 runDungeonFloorCore 的反射护栏同阈值（eliteFleeHP，≈HP 上限的 0.25）；确定性、纯函数。
func dungeonHasDyingMember(run *dungeonRun) bool {
	for _, ms := range run.members {
		if ms.status == "contributed" && ms.rec.Status.HP < eliteFleeHP {
			return true
		}
	}
	return false
}

// dungeonDyingCard 渲染濒死撤退抉择的祖魂语气暂停卡（绝不裸露 HP 数值）。
func dungeonDyingCard(floor int) string {
	return fmt.Sprintf("第%d层的恶战里，她已经撑不住了——再打下去，怕是要把命搭进去。要不要让她退？这一下，得你拿主意。", floor)
}

// ResolveDungeonTimeout 是 Execution→Deployment 边界钩子：扫该会话所有「玩家已离场且超时」的暂停段，按宪章兜底
// 自动见好就收/撤离（保留已得 loot、轻惩 morale，绝不触碰 lives），写 DUNGEON_CHARTER_TIMEOUT。返回被兜底的段数。
// 全程 best-effort：任一段出错只记日志、跳过，绝不阻断阶段切换。flag 关时零行为。
func (service *Service) ResolveDungeonTimeout(ctx context.Context, state *State) (int, error) {
	if !dungeonEnabled() {
		return 0, nil
	}
	if service == nil || service.db == nil || state == nil {
		return 0, nil
	}
	segs, err := service.loadTimedOutDungeonSegments(ctx, state.ID, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	reclaimed := 0
	for _, seg := range segs {
		actor := dungeonTimeoutActorID(seg)
		run, err := service.rebuildDungeonRun(ctx, seg, state)
		if err != nil {
			// M1：不再静默 continue——留痕该段重建失败并累计失败计数（连续超上限后该段被过滤、不再无限重试）。
			appendLog(state, "dungeon_charter_timeout_failed",
				fmt.Sprintf("副本超时兜底重建失败（第%d层），稍后重试：%v", seg.Floor, err), actor, seg.ID)
			service.bumpDungeonTimeoutFailureLogged(ctx, state, seg, actor)
			continue
		}
		var res DungeonSegmentResult
		if _, err := service.completeDungeonSegment(ctx, seg, run, "fled", &res); err != nil {
			// M1：结算失败同样留痕并累计失败计数（H1 的 CAS 保证已 completed 的段不会被重复结算/重复发赃）。
			appendLog(state, "dungeon_charter_timeout_failed",
				fmt.Sprintf("副本超时兜底结算失败（第%d层），稍后重试：%v", seg.Floor, err), actor, seg.ID)
			service.bumpDungeonTimeoutFailureLogged(ctx, state, seg, actor)
			continue
		}
		// 兜底留痕：DUNGEON_CHARTER_TIMEOUT 流程事件 + 回响命运卡（祖魂语气，绝不裸露数值）。
		card := dungeonCharterTimeoutCard(seg.Floor)
		service.emitDungeonTimeout(ctx, state, seg, card)
		reclaimed++
	}
	return reclaimed, nil
}

// dungeonTimeoutActorID 取超时兜底失败留痕的 actor（队首队员 ID；空队退化为段 ID，保证 log 行有锚）。
func dungeonTimeoutActorID(seg *DungeonSegment) string {
	if seg != nil && len(seg.UnitIDs) > 0 {
		return seg.UnitIDs[0]
	}
	return seg.ID
}

// bumpDungeonTimeoutFailureLogged 累计一段超时兜底失败计数；写入本身失败时再留一条痕（best-effort，绝不阻断阶段切换）。
func (service *Service) bumpDungeonTimeoutFailureLogged(ctx context.Context, state *State, seg *DungeonSegment, actor string) {
	if err := service.bumpDungeonTimeoutFailure(ctx, seg); err != nil {
		appendLog(state, "dungeon_charter_timeout_failed",
			fmt.Sprintf("副本超时兜底失败计数写入失败（第%d层）：%v", seg.Floor, err), actor, seg.ID)
	}
}

// MarkDungeonSegmentLeft 标记玩家离场时刻（left_at=now），开始 charter 超时计时。供主控在玩家断线/挂起时调用。
// M5：收 sessionID 校验段归属——UPDATE 加 AND session_id=?，跨会话越权标离场不会命中任何行（静默零作用，不泄露段存在性）。
func (service *Service) MarkDungeonSegmentLeft(ctx context.Context, sessionID, segmentID string) error {
	if service == nil || service.db == nil {
		return fmt.Errorf("mark dungeon segment left: missing db")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := service.db.ExecContext(ctx,
		`UPDATE dungeon_segments SET left_at = ?, updated_at = ? WHERE id = ? AND session_id = ? AND state = ?`,
		now, now, segmentID, sessionID, dungeonSegStatePaused)
	if err != nil {
		return fmt.Errorf("mark dungeon segment left: %w", err)
	}
	return nil
}

// ---- 终局结算（复用 dungeon.go 的 settleDungeon 同口径，确定性、付费不进）----

// completeDungeonSegment 把一段副本推到终局并一次性结算（分赃/惩罚/留痕/收件箱），落库 state=completed。
// 复用 settleDungeon：用 segment 快照重建 dungeonRun + DungeonResult（FloorResults 据已清层 loot 重建），
// 由 AllocateLoot 仲裁分赃、DegradePenalty 分级惩罚——与同步核心逐字节同口径。
//
// H1（并发双花防护）：load→settle→save 无原子性会让并发 run/resume 两个 racer 都 settle 完才落库，
// 导致 wallet 双发、遗物双判、收件箱卡双张。修法是「抢占在前、结算在后」——先用条件 UPDATE 原子抢占 completed 状态
// （WHERE state!='completed'），只有抢到（RowsAffected==1）的 racer 才有权 settleDungeon；抢不到（==0）说明已被
// 并发结算，立即重读该段、返回其既有 outcome 的幂等结果，绝不再跑 settleDungeon（不再 applyEliteMutation 落 wallet/遗物）。
func (service *Service) completeDungeonSegment(ctx context.Context, seg *DungeonSegment, run *dungeonRun, outcome string, res *DungeonSegmentResult) (DungeonSegmentResult, error) {
	// 抢占结算权：原子地把 state 翻成 completed（仅当当前 state 尚非 completed）。
	claimed, err := service.claimDungeonCompletion(ctx, seg, outcome)
	if err != nil {
		return *res, err
	}
	if !claimed {
		// 已被并发 racer 结算：重读该段，幂等返回其既有 outcome，绝不再 settle（不再发 wallet/遗物/收件箱）。
		existing, lerr := service.loadDungeonSegment(ctx, seg.ID)
		if lerr != nil {
			return *res, lerr
		}
		if existing != nil {
			*seg = *existing
		}
		res.NextAction = dungeonOutcomeToAction(seg.Outcome)
		res.Outcome = seg.Outcome
		return *res, nil
	}

	// 抢到结算权：内存态同步到 completed（claim 已落库 state/pause_reason/left_at），再跑唯一一次结算。
	seg.State = dungeonSegStateCompleted
	seg.Outcome = outcome
	// dungeon_segments 无独立 outcome 列：completed 段把终局 outcome 复用 pause_reason 列持久化（completed:<outcome>），
	// scanDungeonSegment 反解还原 seg.Outcome——保证重载/状态端点能读出 cleared/fled/wiped。
	seg.PauseReason = dungeonCompletedPauseReason(outcome)
	seg.LeftAt = sql.NullString{}

	result := &DungeonResult{
		DungeonID:    seg.DungeonRunID,
		Floors:       seg.Floors,
		Outcome:      outcome,
		Contribution: map[string]float64{},
		PenaltyLayer: map[string]int{},
		InboxCards:   map[string]string{},
	}
	// 据累计已清层 loot 重建 FloorResults（settleDungeon 只看 cleared 层的 lootSpec()）。
	clearedFloors := dungeonClearedFloorsFromSnapshot(seg)
	for _, cf := range clearedFloors {
		result.FloorResults = append(result.FloorResults, DungeonFloorResult{
			Floor: cf.floor, IsBoss: cf.isBoss, Outcome: "cleared",
		})
	}
	result.FloorsClear = len(clearedFloors)

	if err := service.settleDungeon(ctx, run, result); err != nil {
		return *res, err
	}

	// 持久化完整终局快照（members/awards/floor 等；state/pause_reason 已被 claim 落库，这里二次 upsert 仅补全其余列、语义一致）。
	if err := service.saveDungeonSegment(ctx, seg); err != nil {
		return *res, err
	}

	res.NextAction = dungeonOutcomeToAction(outcome)
	res.Outcome = outcome
	return *res, nil
}

// claimDungeonCompletion 用条件 UPDATE 原子抢占一段副本的结算权（H1 防并发双花）：
// 仅当 state 当前**非** completed 时，把它翻成 completed 并把终局 outcome 编进 pause_reason 列（completed:<outcome>）、清 left_at。
// 返回 (true,nil)=抢到结算权（RowsAffected==1）；(false,nil)=已被并发 racer 抢占（RowsAffected==0，调用方应走幂等返回）。
func (service *Service) claimDungeonCompletion(ctx context.Context, seg *DungeonSegment, outcome string) (bool, error) {
	if service == nil || service.db == nil || seg == nil {
		return false, fmt.Errorf("claim dungeon completion: missing db")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	r, err := service.db.ExecContext(ctx,
		`UPDATE dungeon_segments SET state = ?, pause_reason = ?, left_at = NULL, updated_at = ?
		 WHERE id = ? AND state != ?`,
		dungeonSegStateCompleted, dungeonCompletedPauseReason(outcome), now,
		seg.ID, dungeonSegStateCompleted)
	if err != nil {
		return false, fmt.Errorf("claim dungeon completion: %w", err)
	}
	affected, err := r.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim dungeon completion rows: %w", err)
	}
	return affected == 1, nil
}

// dungeonClearedFloor 是从快照重建的一条已清层（floor + 是否 boss）。
type dungeonClearedFloor struct {
	floor  int
	isBoss bool
}

// dungeonClearedFloorsFromSnapshot 从 awards 累计快照恢复「哪些层已清」（按 floor 去重，保留 isBoss 标记）。
func dungeonClearedFloorsFromSnapshot(seg *DungeonSegment) []dungeonClearedFloor {
	seen := map[int]bool{}
	out := make([]dungeonClearedFloor, 0)
	for _, a := range seg.Awards {
		if seen[a.Floor] {
			continue
		}
		seen[a.Floor] = true
		out = append(out, dungeonClearedFloor{floor: a.Floor, isBoss: a.IsBoss})
	}
	return out
}

// ---- 重建战斗态 / 同步快照 ----

// rebuildDungeonRun 从 segment 快照 + DB 重建一个 dungeonRun（队员 HP/贡献从快照还原，不回库覆盖并发战斗写）。
// L1：combat_roll 的回合 salt 用 seg.EnteredTurn（踏入副本时钉死的回合）覆盖 live Turn——
// 复制一份 state 并改其 TurnState.Turn（不污染调用方共享的 state），使 resume/兜底续跑的骰序与玩家何时回来无关。
func (service *Service) rebuildDungeonRun(ctx context.Context, seg *DungeonSegment, state *State) (*dungeonRun, error) {
	members := make([]*memberCombatState, 0, len(seg.Members))
	for _, snap := range seg.Members {
		rec, err := service.units.GetByID(ctx, snap.UnitID)
		if err != nil {
			return nil, fmt.Errorf("load dungeon member %s: %w", snap.UnitID, err)
		}
		member := rec
		ms := &memberCombatState{
			rec:    &member,
			status: snap.Status,
			dealt:  snap.Dealt,
			taken:  snap.Taken,
		}
		ms.contrib.Damage = snap.Damage
		ms.contrib.Tank = snap.Tank
		ms.contrib.Role = snap.Role
		ms.contrib.Risk = snap.Risk
		ms.contrib.Clutch = snap.Clutch
		if ms.status == "" {
			ms.status = "contributed"
		}
		members = append(members, ms)
	}
	// 用踏入回合钉死骰序：浅复制 state，仅覆盖 TurnState.Turn，避免改动调用方持有的共享 state。
	runState := state
	if state != nil {
		pinned := *state
		pinned.TurnState.Turn = seg.EnteredTurn
		runState = &pinned
	}
	return &dungeonRun{id: seg.DungeonRunID, floors: seg.Floors, members: members, state: runState}, nil
}

// syncMembersToSnapshot 把战后 dungeonRun 的队员战斗态写回 segment 快照（HP/贡献/状态）。
func syncMembersToSnapshot(seg *DungeonSegment, run *dungeonRun) {
	byID := map[string]*memberCombatState{}
	for _, ms := range run.members {
		byID[ms.rec.ID] = ms
	}
	for i := range seg.Members {
		ms := byID[seg.Members[i].UnitID]
		if ms == nil {
			continue
		}
		seg.Members[i].HP = ms.rec.Status.HP
		seg.Members[i].Status = ms.status
		seg.Members[i].Damage = ms.contrib.Damage
		seg.Members[i].Tank = ms.contrib.Tank
		seg.Members[i].Role = ms.contrib.Role
		seg.Members[i].Risk = ms.contrib.Risk
		seg.Members[i].Clutch = ms.contrib.Clutch
		seg.Members[i].Dealt = ms.dealt
		seg.Members[i].Taken = ms.taken
	}
}

// appendClearedLoot 把刚清的一层的 loot 规格累计进 segment（与 dungeonFloorLoot 同口径，确定性）。
func appendClearedLoot(seg *DungeonSegment, floor int, isBoss bool) {
	for _, item := range dungeonFloorLoot(floor, isBoss) {
		seg.Awards = append(seg.Awards, dungeonAwardSnapshot{
			Floor:    floor,
			IsBoss:   isBoss,
			ItemID:   item.ID,
			Rarity:   string(item.Rarity),
			Quantity: item.Quantity,
		})
	}
}

// dungeonSegmentState best-effort 载入该 segment 所属会话的 state（供确定性/世界化/红线锚）；失败退化为最小 State。
func (service *Service) dungeonSegmentState(ctx context.Context, seg *DungeonSegment) *State {
	if state := service.loadStateForFate(ctx, seg.SessionID); state != nil {
		return state
	}
	return &State{ID: seg.SessionID}
}

// ---- 暂停 / 兜底 留痕（best-effort）----

// emitDungeonPause 把一次关键节点暂停写 DUNGEON_SEGMENT_PAUSE 流程事件 + 经 SurfaceFateEvent 路由命运卡（祖魂语气）。
// best-effort：吞错只记日志，绝不阻断推进结算。
func (service *Service) emitDungeonPause(ctx context.Context, state *State, seg *DungeonSegment, card string, pauseKind string) {
	owner := service.firstDungeonMember(ctx, seg)
	if owner == nil {
		return
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   seg.SessionID,
		OwnerUnitID: owner.ID,
		Code:        events.ReasonDungeonSegmentPause,
		Category:    events.CategoryLifecycle,
		Payload: map[string]any{
			"segment_id":   seg.ID,
			"floor":        seg.Floor,
			"pause_reason": pauseKind,
			"narrative":    card,
		},
	}); err != nil {
		appendLog(state, "dungeon_segment_pause", card, owner.ID, seg.ID)
		return
	}
	// 路由命运卡：濒死=强相关待决策，首触=高光。importance/emotion 据暂停类型，绝不裸露数值。
	importance, emotion := dungeonPauseWeight(pauseKind)
	if _, err := service.SurfaceFateEvent(ctx, seg.SessionID, owner, FateEvent{
		ActorID:       owner.ID,
		TargetID:      owner.ID,
		ReasonCode:    events.ReasonDungeonSegmentPause,
		Importance:    importance,
		EmotionWeight: emotion,
		Summary:       card,
	}); err != nil {
		// best-effort：路由失败不阻断。
		appendLog(state, "dungeon_segment_pause", card, owner.ID, seg.ID)
	}
}

// emitDungeonTimeout 写 DUNGEON_CHARTER_TIMEOUT 流程事件 + 经 SurfaceFateEvent 投回响命运卡（祖魂语气）。best-effort。
func (service *Service) emitDungeonTimeout(ctx context.Context, state *State, seg *DungeonSegment, card string) {
	owner := service.firstDungeonMember(ctx, seg)
	if owner == nil {
		return
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   seg.SessionID,
		OwnerUnitID: owner.ID,
		Code:        events.ReasonDungeonCharterTimeout,
		Category:    events.CategoryLifecycle,
		Payload: map[string]any{
			"segment_id": seg.ID,
			"floor":      seg.Floor,
			"narrative":  card,
		},
	}); err != nil {
		appendLog(state, "dungeon_charter_timeout", card, owner.ID, seg.ID)
		return
	}
	if _, err := service.SurfaceFateEvent(ctx, seg.SessionID, owner, FateEvent{
		ActorID:       owner.ID,
		TargetID:      owner.ID,
		ReasonCode:    events.ReasonDungeonCharterTimeout,
		Importance:    6,
		EmotionWeight: -0.2,
		Summary:       card,
	}); err != nil {
		appendLog(state, "dungeon_charter_timeout", card, owner.ID, seg.ID)
	}
}

// firstDungeonMember 取队伍第一名队员（作为该次副本暂停/兜底命运卡的 owner）。best-effort 载入。
func (service *Service) firstDungeonMember(ctx context.Context, seg *DungeonSegment) *unit.Record {
	if len(seg.UnitIDs) == 0 {
		return nil
	}
	rec, err := service.units.GetByID(ctx, seg.UnitIDs[0])
	if err != nil {
		return &unit.Record{ID: seg.UnitIDs[0]}
	}
	return &rec
}

// ---- 旁观传播（复用 loadThreatBystanders / relevance 模式，新函数不碰 threat_propagation.go）----

const (
	// dungeonPropagationFanout 是副本事件向旁观者传播的扇出上限（与 threat 失败传播同量级、略收敛）。
	dungeonPropagationFanout = 32
	// 副本旁观者基线（清层=小高光偏正；濒死/挫败偏负）。
	dungeonClearBaseImportance   = 4
	dungeonClearBaseEmotion      = 0.25
	dungeonSetbackBaseImportance = 6
	dungeonSetbackBaseEmotion    = -0.4
)

// bestEffortPropagateDungeon 包裹 propagateDungeonEvent，吞错只记日志（绝不阻断副本结算主链路）。
func (service *Service) bestEffortPropagateDungeon(ctx context.Context, state *State, seg *DungeonSegment, floor int, setback bool) {
	if _, err := service.propagateDungeonEvent(ctx, state, seg, floor, setback); err != nil {
		// best-effort：传播失败不阻断结算。
		_ = err
	}
}

// propagateDungeonEvent 把一段副本的清层(小高光)/濒死或挫败(负向)，按 relevance 找在乎队员的人投收件箱，一人一版。
// 复用 threat_propagation.go 的 loadThreatBystanders/relevance 模式（但在本文件写新函数，不编辑 threat_propagation.go）。
// 返回被实际惊动（非 RouteAutonomous）的旁观者人数。确定性、best-effort。
func (service *Service) propagateDungeonEvent(ctx context.Context, state *State, seg *DungeonSegment, floor int, setback bool) (int, error) {
	if service == nil || service.db == nil || state == nil {
		return 0, fmt.Errorf("propagate dungeon event: missing dependencies")
	}
	baseImportance := dungeonClearBaseImportance
	baseEmotion := dungeonClearBaseEmotion
	reasonCode := events.ReasonDungeonFloorClear
	if setback {
		baseImportance = dungeonSetbackBaseImportance
		baseEmotion = dungeonSetbackBaseEmotion
		reasonCode = events.ReasonEmotionTrauma
	}

	surfaced := 0
	var firstErr error
	for _, snap := range seg.Members {
		victim, err := service.units.GetByID(ctx, snap.UnitID)
		if err != nil {
			continue
		}
		bystanders, err := service.loadThreatBystanders(ctx, victim.ID, dungeonPropagationFanout)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		victimName := victim.DisplayName()
		for _, b := range bystanders {
			// 关系距离 → 措辞/强度衰减（与 threat 失败传播同口径：HopFidelity）。
			fidelity := relevance.HopFidelity(relationHopForIntensity(b.intensity))
			importance := scaleImportance(baseImportance, fidelity)
			emotion := baseEmotion * fidelity
			summary := dungeonBystanderSummary(victimName, floor, setback, fidelity)

			owner := unit.Record{ID: b.sourceID}
			routing, err := service.SurfaceFateEvent(ctx, state.ID, &owner, FateEvent{
				ActorID:       victim.ID,
				TargetID:      victim.ID,
				ReasonCode:    reasonCode,
				Importance:    importance,
				EmotionWeight: emotion,
				Summary:       summary,
			})
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if routing.Route != relevance.RouteAutonomous {
				surfaced++
			}
		}
	}
	return surfaced, firstErr
}

// dungeonBystanderSummary 渲染「一人一版」的副本旁观者叙事（清层/挫败 × 关系距离三档）。确定性、纯函数、无 LLM。
func dungeonBystanderSummary(victimName string, floor int, setback bool, fidelity float64) string {
	if setback {
		switch {
		case fidelity >= 1.0:
			return fmt.Sprintf("你挂念的 %s 在那处秘境第%d层吃了亏，伤痕累累退了下来——但人还在。", victimName, floor)
		case fidelity >= relevance.HopDecay:
			return fmt.Sprintf("有消息辗转传来：%s 在秘境里栽了跟头，没能再往深处走。", victimName)
		default:
			return fmt.Sprintf("远处隐约传来风声，说 %s 进了一处险地，出来时带着伤。", victimName)
		}
	}
	switch {
	case fidelity >= 1.0:
		return fmt.Sprintf("你挂念的 %s 又把那处秘境往深处推进了一层（第%d层）。", victimName, floor)
	case fidelity >= relevance.HopDecay:
		return fmt.Sprintf("有消息辗转传来：%s 在秘境里又闯过了一关。", victimName)
	default:
		return fmt.Sprintf("远处隐约传来风声，说 %s 正一步步往那处幽深之地的深处走。", victimName)
	}
}

// ---- 祖魂语气卡 / 路由权重（绝不裸露数值）----

// dungeonFirstContactCard 渲染末层 boss 首次接触的祖魂语气暂停卡。
func dungeonFirstContactCard(floor int, threatName string) string {
	return fmt.Sprintf("她一路打到了第%d层。最深处守着的，是这座秘境的主人——%s。她在门口顿住了脚步。这一仗要不要打，得你给个准信。", floor, threatName)
}

// dungeonCharterTimeoutCard 渲染 charter 超时兜底的回响卡（设计 §3 回响卡口径）。
func dungeonCharterTimeoutCard(floor int) string {
	return fmt.Sprintf("你没回来。于是在第%d层的岔口，她照着你早先的叮嘱，见好就收了——带着已经到手的东西和一身的疲惫，从那处幽深之地爬了出来。", floor)
}

// dungeonPauseWeight 把暂停类型映射为命运重要度/情绪强度（驱动收件箱三档路由）。濒死=强相关待决策。
func dungeonPauseWeight(pauseKind string) (int, float64) {
	switch pauseKind {
	case dungeonPauseReasonPlayerDying:
		return 8, -0.6 // 濒死：强相关，应升级待决策
	default: // first_contact
		return 6, 0.1 // boss 首触：高光，略带紧张
	}
}

// dungeonCompletedPrefix 是 completed 段把终局 outcome 编入 pause_reason 列的前缀（dungeon_segments 无独立 outcome 列）。
const dungeonCompletedPrefix = "completed:"

// dungeonCompletedPauseReason 把终局 outcome 编成 pause_reason 列值（completed:<outcome>）。
func dungeonCompletedPauseReason(outcome string) string {
	return dungeonCompletedPrefix + outcome
}

// dungeonOutcomeFromPauseReason 从 pause_reason 列反解 completed 段的终局 outcome；非 completed 编码返回 ("",false)。
func dungeonOutcomeFromPauseReason(pauseReason string) (string, bool) {
	if strings.HasPrefix(pauseReason, dungeonCompletedPrefix) {
		return strings.TrimPrefix(pauseReason, dungeonCompletedPrefix), true
	}
	return "", false
}

// ---- M1：超时兜底连续失败计数 sentinel（防卡死段无限重试每边界重复刷错）----

// dungeonTimeoutFailPrefix 把超时兜底的连续失败计数编进 pause_reason 列的后缀（与原 pause kind 用 '|' 分隔，
// 形如 "first_contact|failed:2"）。dungeon_segments 无独立失败计数列，复用 pause_reason 列承载——不污染 pause kind 语义。
const dungeonTimeoutFailPrefix = "|failed:"

// dungeonMaxTimeoutFailures 是单段超时兜底允许的连续失败上限：超过即被 loadTimedOutDungeonSegments 过滤、
// 不再每边界重复重试（避免一个永久损坏的段把每个阶段切换都拖着刷错/空耗）。
const dungeonMaxTimeoutFailures = 3

// dungeonPauseKindAndFailures 从 pause_reason 列拆出「原 pause kind」与「已累计的超时失败次数」。
// 形如 "player_decision|failed:2" → ("player_decision", 2)；无后缀 → (原值, 0)。
func dungeonPauseKindAndFailures(pauseReason string) (string, int) {
	idx := strings.Index(pauseReason, dungeonTimeoutFailPrefix)
	if idx < 0 {
		return pauseReason, 0
	}
	kind := pauseReason[:idx]
	nStr := pauseReason[idx+len(dungeonTimeoutFailPrefix):]
	n := 0
	for _, ch := range nStr {
		if ch < '0' || ch > '9' {
			n = 0
			break
		}
		n = n*10 + int(ch-'0')
	}
	return kind, n
}

// dungeonPauseReasonWithFailures 把「原 pause kind + 失败计数」重编回 pause_reason 列值（count<=0 时只留 kind）。
func dungeonPauseReasonWithFailures(kind string, count int) string {
	if count <= 0 {
		return kind
	}
	return fmt.Sprintf("%s%s%d", kind, dungeonTimeoutFailPrefix, count)
}

// bumpDungeonTimeoutFailure 把一段超时兜底失败的计数 +1 持久化进 pause_reason 列（best-effort，吞错只返回；不阻断阶段切换）。
// 超过 dungeonMaxTimeoutFailures 的段会被 loadTimedOutDungeonSegments 过滤，不再无限重试。
func (service *Service) bumpDungeonTimeoutFailure(ctx context.Context, seg *DungeonSegment) error {
	if service == nil || service.db == nil || seg == nil {
		return fmt.Errorf("bump dungeon timeout failure: missing db")
	}
	// seg.PauseReason 经 scan 已剥成裸 kind、失败次数在 seg.TimeoutFailures——用后者累计（不可从已剥离的 PauseReason 反解）。
	next := seg.TimeoutFailures + 1
	encoded := dungeonPauseReasonWithFailures(seg.PauseReason, next)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// 仅对仍 paused 的段累计（避免与并发 claim 的 completed 落库打架）。
	if _, err := service.db.ExecContext(ctx,
		`UPDATE dungeon_segments SET pause_reason = ?, updated_at = ? WHERE id = ? AND state = ?`,
		encoded, now, seg.ID, dungeonSegStatePaused); err != nil {
		return fmt.Errorf("bump dungeon timeout failure: %w", err)
	}
	// 同步内存态（PauseReason 仍存裸 kind，失败计数进 TimeoutFailures，与 scan 后的呈现一致）。
	seg.TimeoutFailures = next
	return nil
}

// dungeonOutcomeToAction 把终局 outcome 映射为 NextAction 枚举。
func dungeonOutcomeToAction(outcome string) DungeonNextAction {
	switch outcome {
	case "cleared":
		return DungeonCompletedCleared
	case "fled":
		return DungeonCompletedFled
	case "wiped":
		return DungeonCompletedWiped
	default:
		return DungeonCompletedFled
	}
}

// ---- 持久化 CRUD（dungeon_segments 表，dbdialect 处理 ON CONFLICT vs ON DUPLICATE）----

// saveDungeonSegment upsert 一段副本态（主键 id）。
func (service *Service) saveDungeonSegment(ctx context.Context, seg *DungeonSegment) error {
	if service == nil || service.db == nil || seg == nil {
		return fmt.Errorf("save dungeon segment: missing db")
	}
	unitIDsJSON, err := json.Marshal(seg.UnitIDs)
	if err != nil {
		return fmt.Errorf("marshal unit ids: %w", err)
	}
	membersJSON, err := json.Marshal(seg.Members)
	if err != nil {
		return fmt.Errorf("marshal members: %w", err)
	}
	awardsJSON, err := json.Marshal(seg.Awards)
	if err != nil {
		return fmt.Errorf("marshal awards: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	seg.UpdatedAt = now
	if seg.StartedAt == "" {
		seg.StartedAt = now
	}
	// state=completed/in_progress 时清掉 left_at；paused 由 MarkDungeonSegmentLeft 单独写。这里以 seg.LeftAt 为准。
	var leftAt any
	if seg.LeftAt.Valid {
		leftAt = seg.LeftAt.String
	} else {
		leftAt = nil
	}

	query := `
		INSERT INTO dungeon_segments (
			id, dungeon_run_id, session_id, unit_ids_json, floors, floor, entered_turn, state,
			members_state_json, boss_hp_remaining, floor_round, awards_accumulated_json,
			pause_reason, started_at, left_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			floor = excluded.floor, entered_turn = excluded.entered_turn, state = excluded.state,
			members_state_json = excluded.members_state_json,
			boss_hp_remaining = excluded.boss_hp_remaining, floor_round = excluded.floor_round,
			awards_accumulated_json = excluded.awards_accumulated_json,
			pause_reason = excluded.pause_reason, left_at = excluded.left_at, updated_at = excluded.updated_at`
	if dbdialect.IsMySQL(service.db) {
		query = `
			INSERT INTO dungeon_segments (
				id, dungeon_run_id, session_id, unit_ids_json, floors, floor, entered_turn, state,
				members_state_json, boss_hp_remaining, floor_round, awards_accumulated_json,
				pause_reason, started_at, left_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				floor = VALUES(floor), entered_turn = VALUES(entered_turn), state = VALUES(state),
				members_state_json = VALUES(members_state_json),
				boss_hp_remaining = VALUES(boss_hp_remaining), floor_round = VALUES(floor_round),
				awards_accumulated_json = VALUES(awards_accumulated_json),
				pause_reason = VALUES(pause_reason), left_at = VALUES(left_at), updated_at = VALUES(updated_at)`
	}
	if _, err := service.db.ExecContext(ctx, query,
		seg.ID, seg.DungeonRunID, seg.SessionID, string(unitIDsJSON), seg.Floors, seg.Floor, seg.EnteredTurn, seg.State,
		string(membersJSON), seg.BossHPRemain, seg.FloorRound, string(awardsJSON),
		seg.PauseReason, seg.StartedAt, leftAt, seg.UpdatedAt,
	); err != nil {
		return fmt.Errorf("save dungeon segment: %w", err)
	}
	return nil
}

// loadDungeonSegment 按 id 读一段副本态；不存在返回 (nil, nil)。
func (service *Service) loadDungeonSegment(ctx context.Context, segmentID string) (*DungeonSegment, error) {
	if service == nil || service.db == nil {
		return nil, fmt.Errorf("load dungeon segment: missing db")
	}
	row := service.db.QueryRowContext(ctx, `
		SELECT id, dungeon_run_id, session_id, unit_ids_json, floors, floor, entered_turn, state,
			members_state_json, boss_hp_remaining, floor_round, awards_accumulated_json,
			pause_reason, started_at, left_at, updated_at
		FROM dungeon_segments WHERE id = ?`, segmentID)
	seg, err := scanDungeonSegment(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return seg, nil
}

// loadTimedOutDungeonSegments 读某会话所有「玩家已离场(left_at 非空)且离场已超 DungeonCharterTimeout」的暂停段。
// 确定性比较：left_at 解析为时间，now-left_at >= 超时阈值即超时。解析失败的段跳过（保守不兜底）。
// M1：连续兜底失败已超 dungeonMaxTimeoutFailures 的段也跳过——避免一个永久损坏的段把每个阶段边界都拖着无限重试/刷错。
func (service *Service) loadTimedOutDungeonSegments(ctx context.Context, sessionID string, now time.Time) ([]*DungeonSegment, error) {
	rows, err := service.db.QueryContext(ctx, `
		SELECT id, dungeon_run_id, session_id, unit_ids_json, floors, floor, entered_turn, state,
			members_state_json, boss_hp_remaining, floor_round, awards_accumulated_json,
			pause_reason, started_at, left_at, updated_at
		FROM dungeon_segments
		WHERE session_id = ? AND state = ? AND left_at IS NOT NULL`,
		sessionID, dungeonSegStatePaused)
	if err != nil {
		return nil, fmt.Errorf("query timed-out dungeon segments: %w", err)
	}
	defer rows.Close()
	out := make([]*DungeonSegment, 0)
	for rows.Next() {
		seg, err := scanDungeonSegment(rows)
		if err != nil {
			return nil, err
		}
		if !seg.LeftAt.Valid {
			continue
		}
		if seg.TimeoutFailures >= dungeonMaxTimeoutFailures {
			continue // 连续兜底失败超上限：放弃重试，避免无限刷错
		}
		left, ok := parseDungeonTime(seg.LeftAt.String)
		if !ok {
			continue // 解析失败：保守不兜底
		}
		if now.Sub(left) >= DungeonCharterTimeout {
			out = append(out, seg)
		}
	}
	return out, rows.Err()
}

// dungeonRowScanner 抽象 *sql.Row 与 *sql.Rows 的 Scan（共用扫描逻辑）。
type dungeonRowScanner interface {
	Scan(dest ...any) error
}

// scanDungeonSegment 把一行 dungeon_segments 扫成 DungeonSegment。
func scanDungeonSegment(scanner dungeonRowScanner) (*DungeonSegment, error) {
	var seg DungeonSegment
	var unitIDsJSON, membersJSON, awardsJSON string
	var leftAt sql.NullString
	if err := scanner.Scan(
		&seg.ID, &seg.DungeonRunID, &seg.SessionID, &unitIDsJSON, &seg.Floors, &seg.Floor, &seg.EnteredTurn, &seg.State,
		&membersJSON, &seg.BossHPRemain, &seg.FloorRound, &awardsJSON,
		&seg.PauseReason, &seg.StartedAt, &leftAt, &seg.UpdatedAt,
	); err != nil {
		return nil, err
	}
	seg.LeftAt = leftAt
	// M1：paused 段的 pause_reason 可能带 failed:<n> 后缀（超时兜底连续失败计数）——先剥离，把裸 kind 还给消费方
	// （resume/status/emit 据裸 kind 判定），失败次数存入 seg.TimeoutFailures。completed 段的 completed:<outcome> 不含此后缀。
	if kind, n := dungeonPauseKindAndFailures(seg.PauseReason); n > 0 {
		seg.PauseReason = kind
		seg.TimeoutFailures = n
	}
	// completed 段把终局 outcome 编进 pause_reason 列（completed:<outcome>）：还原 seg.Outcome 并把 PauseReason 归零
	// （completed 段语义上无暂停原因）。非 completed 段的 pause_reason 原样保留。
	if outcome, ok := dungeonOutcomeFromPauseReason(seg.PauseReason); ok {
		seg.Outcome = outcome
		seg.PauseReason = ""
	}
	_ = json.Unmarshal([]byte(unitIDsJSON), &seg.UnitIDs)
	_ = json.Unmarshal([]byte(membersJSON), &seg.Members)
	_ = json.Unmarshal([]byte(awardsJSON), &seg.Awards)
	if seg.UnitIDs == nil {
		seg.UnitIDs = []string{}
	}
	if seg.Members == nil {
		seg.Members = []dungeonMemberSnapshot{}
	}
	if seg.Awards == nil {
		seg.Awards = []dungeonAwardSnapshot{}
	}
	return &seg, nil
}

// parseDungeonTime 解析 left_at（RFC3339Nano，兼容 RFC3339）。失败返回 (zero,false)。
func parseDungeonTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	// 兼容 "2006-01-02 15:04:05"（部分 upsert 路径可能写 DB 本地格式）。
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// DungeonSegmentStatus 是回给前端/主控的一段副本状态视图（绝不裸露数值血量，只透层/进度/暂停语义）。
type DungeonSegmentStatus struct {
	SegmentID   string `json:"segment_id"`
	State       string `json:"state"`
	Floor       int    `json:"floor"`
	Floors      int    `json:"floors"`
	PauseReason string `json:"pause_reason,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
	AtBoss      bool   `json:"at_boss"`
}

// DungeonSegmentStatusView 读一段副本的可展示状态（供 GET status 端点）；不存在或不属于该会话返回 (nil,nil)。
// M5：收 sessionID 校验段归属——跨会话越权查询归一为「未找到」（返回 nil，让路由映 404，不泄露段是否存在）。
func (service *Service) DungeonSegmentStatusView(ctx context.Context, sessionID, segmentID string) (*DungeonSegmentStatus, error) {
	seg, err := service.loadDungeonSegment(ctx, segmentID)
	if err != nil {
		return nil, err
	}
	if seg == nil || seg.SessionID != sessionID {
		return nil, nil
	}
	return &DungeonSegmentStatus{
		SegmentID:   seg.ID,
		State:       seg.State,
		Floor:       seg.Floor,
		Floors:      seg.Floors,
		PauseReason: seg.PauseReason,
		Outcome:     seg.Outcome,
		AtBoss:      seg.Floor == seg.Floors,
	}, nil
}
