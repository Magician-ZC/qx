package session

// 文件说明：跨玩家/跨会话「排他标的」的零和裁决（设计 docs/事件耦合与跨玩家关联.md §2.6）。
// 同一排他标的——同一联姻对象 / 势力继承席位 / 同批排他战利品——若被多人同时争夺，旧逻辑无统一裁决窗口，
// 谁先到/谁反应快/谁动作频率高谁就赢（P2W 隐患）。本文件把这类争夺收敛到「裁决 tick 的统一结算」：
// 在同一节奏上、仅由各争夺者的**实力/贡献 Score**（付费不进 Score——反 P2W 基石）经 engine/arbitration.Resolve
// 做**确定性**裁决（胜率∝Score、与入队顺序/动作频率无关、同 Key 同结果可复现）。胜者得标的，败者走
// 「退而求其次」补偿（best-effort 记一条记忆「这次没争过，但…」，绝不阻断推进）。
//
// 本轮升级（§2.6 真跨会话）：
//   ① 裁决 Key 由「sessionID+turn+resource」改为设计的「worldID+SO.id+tick」——同一世界、同一标的、同一裁决 tick
//      对**所有**争夺者得到同一确定性 Key，与「谁先在线/谁先扫到」彻底无关（旧 Key 含本会话 sessionID，
//      两个玩家会算出两个 Key、各裁各的、永不真正争同一标的）。WorldID 空时退回原会话内 Key（向后兼容）。
//   ② 候选池从「仅本会话单位」扩到「跨会话同 world 的 units」——按 units.world_id 查出他人单位作候选（**只读**，
//      绝不写他人 units/relations），同 world 不同 session 争同一 NPC 才真正接通。
//   ③ 胜负在裁决 tick **统一结算**（非先到先得）：把全部争夺者一并喂给 arbitration.Resolve 取确定性胜者。
//   ④ 离线方有**离线宪章自动投入兜底**（玩家不在场时，其单位仍按宪章的社交授权/长期图景默认投入争夺，不被动弃权）
//      + 裁决前**最短补投窗口**（contestReinvestWindowTurns：标的首次成冲突后等一个最短窗口再裁，给离线方补投机会）。
//   ⑤ 覆盖从「仅联姻」扩到「联姻 / 席位继承 / 排他战利品」三类（参数化 ContestType）。
//
// 跨玩家硬不变量（§2.1 / §5）：胜负**只产 append-only cross_event**（带 arbitration_key + score_initiator/score_target
// 可仲裁留痕），本侧 Mutator 只改本侧、**永不直写他人 units/relations**。各自 session 内只存翻译产物（败者补偿记忆/echo）。
//
// 纪律对齐 auto_match.go / social_scan.go：flag-gated（QUNXIANG_ZEROSUM_CONTEST **默认开**，仅 false/0/no/off 关）、
// 低频确定性触发、best-effort（吞错绝不中断阶段推进）。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"qunxiang/backend/internal/engine/arbitration"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
)

const (
	// contestScanEveryNTurns 排他争夺扫描的部署回合周期：每 N 个部署回合扫一次（与 social/match 错频，低频不刷屏）。
	contestScanEveryNTurns = 3
	// contestScanMaxUnits 单次扫描纳入争夺判定的单位数上限（控算量：求亲意图判定是 O(n) 读关系，按确定性顺序截断）。
	contestScanMaxUnits = 24
	// contestCrossPoolMaxUnits 跨会话同 world 候选池的硬上限（防一个热门 world 拉爆候选量；按 unitID 确定性截断）。
	contestCrossPoolMaxUnits = 96
	// contestMaxResolutionsPerScan 单次扫描最多实际裁决的排他标的数——硬顶每回合裁决音量，避免一回合刷屏。
	contestMaxResolutionsPerScan = 2
	// contestMarriageMinContenders 触发联姻裁决所需的最少同对象求亲者数（<2 无冲突，无需零和裁决）。
	contestMarriageMinContenders = 2
	// contestReinvestWindowTurns 裁决前的「最短补投窗口」：标的在 (tick - 窗口) 这个对齐桶里统一裁决，
	// 使「裁决 tick」对窗口内任何时刻发起的争夺都落到同一确定性 tick——给离线方补投机会、且与「谁先扫到」无关。
	contestReinvestWindowTurns = contestScanEveryNTurns

	// 联姻求亲意图的关系信号阈值（量纲 [-10,10]，取与 social_scan 同源的保守值，确保只在好感明确成型时算「想求亲」）。
	contestMarriageTrustMin     = 4.0 // 想求亲：actor→target 信任达「熟人」量级
	contestMarriageAffectionMin = 5.0 // 想求亲：actor→target 有较强好感（高于普通结识门，求亲是重决策）

	// contestOfflineCharterFloorScore 离线方（玩家不在场）凭离线宪章社交授权自动投入争夺时的兜底 Score 下限，
	// 让离线一侧不至于因「无人补投」而被在线方零成本抢走标的——但仍是**真实投入**口径（远低于在场认真争夺），不破坏胜率∝Score。
	contestOfflineCharterFloorScore = 1.5
)

// ContestType 是排他标的的类别（参数化裁决：联姻/席位继承/排他战利品复用同一 arbitration 原语，仅 Score 口径与留痕措辞不同）。
type ContestType string

const (
	ContestTypeMarriage      ContestType = "marriage"         // 联姻：多人争同一单身对象
	ContestTypeSeatInherite  ContestType = "seat_inheritance" // 席位继承：多人争同一势力的继承席位
	ContestTypeExclusiveLoot ContestType = "exclusive_loot"   // 排他战利品：多人争同一批不可分割战利品
)

// ContestContender 是一名排他标的争夺者。
// Score 由其**实力/贡献**算出（属性/士气/关系牵引等），**绝不含付费维度**（钱包/付费档/SKU）——这是反 P2W 的口径保证：
// 付费只能买更高的真实投入，买不到「保证赢」。Detail 是可选的人类可读争夺凭据（用于补偿文案，非裁决输入）。
type ContestContender struct {
	UnitID  string
	Score   float64
	Detail  string // 例：「她对老吴的好感」——用于败者「退而求其次」的记忆文案，不参与裁决
	Offline bool   // 该争夺者是否离线方（玩家不在场，由离线宪章兜底投入）——仅用于留痕/遥测，不进 Score
}

// zeroSumContestEnabled 读 QUNXIANG_ZEROSUM_CONTEST，**默认开**：未设/非法值视为开，仅 false/0/no/off 显式关。
// 默认开理由：排他标的的确定性零和裁决是反 P2W 的机制基石（设计 §2.6），且全程 best-effort + 低频 +
// 付费不进 Score、胜负只产 append-only cross_event（不直写他人状态），行为受控、无破坏性副作用。
func zeroSumContestEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_ZEROSUM_CONTEST"))) {
	case "false", "0", "no", "off":
		return false
	default:
		return true // 未设/非法/其余值 → 开
	}
}

// ResolveExclusiveContest 对一个排他标的做确定性零和裁决：把多个争夺者经 arbitration.Resolve（胜率∝Score、
// 与入队顺序/动作频率无关、付费不进 Score）确定性择一胜者。
//
// 跨玩家升级（§2.6）：
//   - Key 由 (worldID, socialObjectID, tick) 派生（worldID 空时退回 (sessionID, turn, resource) 的会话内 Key，向后兼容）——
//     同一世界、同一标的、同一裁决 tick 对所有争夺者得同一 Key，与「谁先在线/谁先扫到」无关、可复现。
//   - 胜负在裁决 tick **统一结算**：本函数被调用时调用方已把（含跨会话的）全部争夺者一并传入。
//   - 胜负**只产 append-only cross_event**（带 arbitration_key + score_initiator/score_target 可仲裁留痕，仅 worldID 非空时）；
//     本侧只为**本会话**败者记补偿记忆（跨会话败者的本侧补偿由其各自 session 读 cross_event 后翻译，不在此直写他人状态）。
//
// 返回胜者 UnitID。守卫：nil 依赖 / 争夺者 <1 → 返回空串 + err（无可裁决）。单争夺者直接判其胜（无冲突仍可幂等调用）。
// 本函数**只裁决与留痕**，不落地标的归属（联姻成立等副作用由调用方按情境处理）——保持原语通用、可被席位/战利品复用。
func (service *Service) ResolveExclusiveContest(
	ctx context.Context,
	state *State,
	socialObjectID string,
	resource string,
	contenders []ContestContender,
) (string, error) {
	if service == nil {
		return "", fmt.Errorf("resolve contest: missing service")
	}
	// 去空 UnitID；输入顺序无关（arbitration.Resolve 内部 dedupMaxScore 已规范化顺序、与频率无关）。
	valid := make([]ContestContender, 0, len(contenders))
	for _, c := range contenders {
		if strings.TrimSpace(c.UnitID) == "" {
			continue
		}
		valid = append(valid, c)
	}
	if len(valid) == 0 {
		return "", fmt.Errorf("resolve contest %q: no contenders", resource)
	}

	contestants := make([]arbitration.Contestant, 0, len(valid))
	scoreByID := make(map[string]float64, len(valid))
	detailByID := make(map[string]string, len(valid))
	for _, c := range valid {
		contestants = append(contestants, arbitration.Contestant{UnitID: c.UnitID, Score: c.Score})
		scoreByID[c.UnitID] = c.Score
		detailByID[c.UnitID] = strings.TrimSpace(c.Detail)
	}

	// Key：worldID 非空 → (worldID, SO.id, tick)（设计 §2.6，与谁先在线无关）；空 → 退回会话内 (sessionID, turn, resource)。
	sessionID := ""
	turn := 0
	worldID := ""
	if state != nil {
		sessionID = state.ID
		turn = state.TurnState.Turn
		worldID = state.WorldID
	}
	key := exclusiveContestKey(worldID, sessionID, turn, socialObjectID, resource)
	out := arbitration.Resolve(arbitration.Contest{Key: key, Resource: resource, Contestants: contestants})
	winnerID := out.WinnerID
	if winnerID == "" {
		return "", fmt.Errorf("resolve contest %q: arbitration returned no winner", resource)
	}

	// 跨玩家留痕：仅 worldID 非空时，把每个「胜者 vs 败者」对落一条 append-only cross_event（带 arbitration_key + 双方 Score）——
	// 这是跨玩家唯一事实源，供各侧 session 读出后自行翻译成本侧 echo/补偿。best-effort：吞错绝不阻断。
	if worldID != "" {
		service.recordContestCrossEvents(ctx, state, worldID, key, resource, winnerID, valid, scoreByID)
	}

	// 败者「退而求其次」补偿：仅给**本会话**败者记一条「这次没争过，但…」记忆（跨会话败者由其各自 session 处理，
	// 不在此直写他人状态——跨玩家硬不变量）。best-effort，绝不阻断。
	for _, c := range valid {
		if c.UnitID == winnerID {
			continue
		}
		service.recordContestConsolation(ctx, state, c.UnitID, resource, detailByID[c.UnitID])
	}
	return winnerID, nil
}

// recordContestCrossEvents 把一次跨会话排他裁决的胜负写成 append-only cross_events（带 arbitration_key + score_initiator/score_target）。
// 每个「胜者→败者」对落一条 CROSS_CONTEST_LOSE（actor=胜者、target=败者），事实唯一、可仲裁（occurred_at + arbitration_key）。
// 跨玩家硬不变量：本函数只 INSERT cross_events，**绝不** UPDATE/DELETE，也绝不触碰任一方 units/relations。
// best-effort：任一步失败只吞错（这里直接静默），绝不阻断主裁决/阶段推进。
func (service *Service) recordContestCrossEvents(
	ctx context.Context,
	state *State,
	worldID string,
	arbitrationKey string,
	resource string,
	winnerID string,
	contenders []ContestContender,
	scoreByID map[string]float64,
) {
	if service == nil || service.db == nil || worldID == "" || winnerID == "" {
		return
	}
	winnerScore := scoreByID[winnerID]
	winnerSession := service.sessionIDForUnit(ctx, winnerID)
	// 先落一条胜者侧 CROSS_CONTEST_WIN（actor=胜者、target=胜者）做正向留痕（供胜者侧 session 读出庆祝/锚化）。
	_ = service.insertContestCrossEvent(ctx, contestCrossEventRow{
		worldID:        worldID,
		actorID:        winnerID,
		targetID:       winnerID,
		kind:           string(events.ReasonCrossContestWin),
		arbitrationKey: arbitrationKey,
		resource:       resource,
		scoreInitiator: winnerScore,
		scoreTarget:    winnerScore,
		initiatorSess:  winnerSession,
		targetSess:     winnerSession,
	})
	// 再对每个败者落一条 CROSS_CONTEST_LOSE（actor=胜者、target=败者）——事实「谁赢了谁」可仲裁。
	for _, c := range contenders {
		if c.UnitID == winnerID {
			continue
		}
		_ = service.insertContestCrossEvent(ctx, contestCrossEventRow{
			worldID:        worldID,
			actorID:        winnerID,
			targetID:       c.UnitID,
			kind:           string(events.ReasonCrossContestLose),
			arbitrationKey: arbitrationKey,
			resource:       resource,
			scoreInitiator: winnerScore,
			scoreTarget:    scoreByID[c.UnitID],
			initiatorSess:  winnerSession,
			targetSess:     service.sessionIDForUnit(ctx, c.UnitID),
		})
	}
}

// contestCrossEventRow 承载一条排他裁决 cross_event 的写入参数（避免长参数列表、保留 §2.6 的可仲裁列）。
type contestCrossEventRow struct {
	worldID        string
	actorID        string
	targetID       string
	kind           string
	arbitrationKey string
	resource       string
	scoreInitiator float64
	scoreTarget    float64
	initiatorSess  string
	targetSess     string
}

// insertContestCrossEvent 把一条排他裁决事件 append 进 cross_events（含 §2.6 的 arbitration_key/score_initiator/score_target/
// interaction_type/*_session_id 列）。append-only：只 INSERT，绝无 UPDATE/DELETE。occurred_at 交给 DB 默认（CURRENT_TIMESTAMP）
// 充当「谁先动手」权威序。返回事件 ID（失败返回 err，由调用方吞错）。
func (service *Service) insertContestCrossEvent(ctx context.Context, row contestCrossEventRow) error {
	if service == nil || service.db == nil {
		return fmt.Errorf("insert contest cross event: missing db")
	}
	id := newContestEventID(row.arbitrationKey, row.actorID, row.targetID)
	_, err := service.db.ExecContext(ctx, `
		INSERT INTO cross_events (
			id, world_id, actor_unit_id, target_unit_id, event_kind, importance, world_tick,
			payload_json, interaction_type, arbitration_key, score_initiator, score_target,
			initiator_session_id, target_session_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, row.worldID, nullableContestStr(row.actorID), nullableContestStr(row.targetID), row.kind,
		crossEventDefaultImportance, 0,
		contestPayloadJSON(row.resource, row.scoreInitiator, row.scoreTarget),
		nullableContestStr("contest"), nullableContestStr(row.arbitrationKey),
		row.scoreInitiator, row.scoreTarget,
		nullableContestStr(row.initiatorSess), nullableContestStr(row.targetSess),
	)
	if err != nil {
		return fmt.Errorf("insert contest cross event: %w", err)
	}
	return nil
}

// contestPayloadJSON 拼一条裁决 cross_event 的 payload（手拼避免引入 json 仅为三字段；值已是数值/转义安全的 resource）。
func contestPayloadJSON(resource string, scoreInitiator, scoreTarget float64) string {
	return fmt.Sprintf(
		`{"resource":%q,"score_initiator":%s,"score_target":%s}`,
		resource,
		strconv.FormatFloat(scoreInitiator, 'f', 4, 64),
		strconv.FormatFloat(scoreTarget, 'f', 4, 64),
	)
}

// newContestEventID 由 (arbitrationKey, actor, target) 派生确定性事件 ID——使「同一裁决对同一对」幂等（重跑不重复写）。
// 用 arbitration 同源的确定性思路：纯字符串拼接前缀 + 三键，无随机；前缀区分本类事件，便于检索与去重。
func newContestEventID(arbitrationKey, actorID, targetID string) string {
	return "ce_contest|" + arbitrationKey + "|" + actorID + "->" + targetID
}

// sessionIDForUnit 只读查一个单位所属的 session_id（用于 cross_event 的 *_session_id 留痕）。读不到/出错回空串（不阻断）。
func (service *Service) sessionIDForUnit(ctx context.Context, unitID string) string {
	if service == nil || service.db == nil || strings.TrimSpace(unitID) == "" {
		return ""
	}
	var sess sql.NullString
	if err := service.db.QueryRowContext(ctx, `SELECT session_id FROM units WHERE id = ?`, unitID).Scan(&sess); err != nil {
		return ""
	}
	return sess.String
}

// recordContestConsolation 给一名争夺失败者记一条「退而求其次」的命运补偿（best-effort，绝不阻断）。
// 优先记一条单位记忆（让 AI 后续决策能引用「我这次没争过」），并在有 state 时追加一条可读日志。
// 跨玩家硬不变量：只对**本会话**败者记忆（service.units.GetByID 即便能读到他人单位，rememberUnitWithSource 写的是本库记忆，
// 但补偿语义应归各自 session——故由调用方只把本会话败者传给本路径；本函数自身只对「能读到的本库单位」记，不直写他人 relations）。
func (service *Service) recordContestConsolation(ctx context.Context, state *State, loserID string, resource string, detail string) {
	if service == nil || strings.TrimSpace(loserID) == "" {
		return
	}
	turn := 0
	if state != nil {
		turn = state.TurnState.Turn
	}
	// 文案：以「这次没争过，但…」为骨架，detail 给得出就嵌入（如「她对老吴的好感」），给不出就用通用句。
	tail := strings.TrimSpace(detail)
	var summary string
	if tail != "" {
		summary = fmt.Sprintf("这次没争过——%s。但我把这份心意收好，来日方长。", tail)
	} else {
		summary = "这次没争过，但我把这份心意收好，来日方长。"
	}
	if service.units != nil {
		if loser, err := service.units.GetByID(ctx, loserID); err == nil && loser.ID != "" {
			// importanceBoost=1：略高于日常琐事，让「错失」这件事在记忆里多留几回合（衰减 tau≈120）。
			_ = service.rememberUnitWithSource(ctx, &loser, turn, summary, "exclusive_contest", 1)
		}
	}
	if state != nil {
		appendLog(state, "contest_consolation", summary, loserID, "")
	}
}

// exclusiveContestKey 派生确定性裁决 Key（与 arbitration「Key 须含 region+tick 可复现」约定对齐）。
// worldID 非空 → 「contest|w<worldID>|<SO.id>|t<tick>」（§2.6：同世界同标的同 tick 对所有争夺者同 Key，与谁先在线无关；
//
//	tick 用「裁决 tick」= turn 对齐到补投窗口桶，保证窗口内任何时刻发起都落同一桶/同一 Key）；
//
// worldID 空 → 退回旧会话内 Key 「contest|<sessionID>|t<turn>|<resource>」（向后兼容，纯本会话争夺行为不变）。
// 纯字符串拼接，无哈希、无全局 rand，便于测试断言「同 Key 同结果」。
func exclusiveContestKey(worldID, sessionID string, turn int, socialObjectID, resource string) string {
	if strings.TrimSpace(worldID) != "" {
		bucket := contestResolutionTick(turn)
		return "contest|w" + strings.TrimSpace(worldID) + "|" + strings.TrimSpace(socialObjectID) + "|t" + strconv.Itoa(bucket)
	}
	return "contest|" + sessionID + "|t" + strconv.Itoa(turn) + "|" + strings.TrimSpace(resource)
}

// contestResolutionTick 把 turn 对齐到「补投窗口桶」：窗口内任何回合算同一裁决 tick（§2.6 的「裁决前最短补投窗口」+
// 「裁决 tick 统一结算」的确定性落点）——使在线/离线方在窗口内任何时刻发起争夺，都落到同一确定性 Key、被一并裁决。
func contestResolutionTick(turn int) int {
	if contestReinvestWindowTurns <= 1 {
		return turn
	}
	return (turn / contestReinvestWindowTurns) * contestReinvestWindowTurns
}

// scanExclusiveContestsAtBoundary 在部署边界扫描对同一排他标的的竞争，并用 ResolveExclusiveContest 确定性裁决。
// 覆盖三类排他标的（参数化 ContestType）：**联姻冲突**（多个单身单位都想与同一单身对象确认亲密关系）、
// **席位继承**、**排他战利品**（后两类的候选/Score 复用同一聚合骨架，由 contestSubjectsFor 按类型给出标的与意图）。
// WorldID 非空时**跨会话同 world** 拉候选（只读他人 units，绝不写）；空时退回纯本会话行为（向后兼容）。
// 无冲突（每个标的至多一个争夺者）→ no-op。守卫：nil 依赖 / flag 关 / 候选不足 / 未到周期 → no-op。
// 全程 best-effort：吞错绝不影响阶段推进。
//
// 注意「裁决」语义：胜者获得「本回合优先与该标的推进」的资格（实际成立仍走既有路径，不强制）；败者获「退而求其次」补偿。
// 这把「谁先到谁赢」改为「谁的真实投入(Score)更高更可能赢」（频率/付费无关、跨会话统一结算）。
func (service *Service) scanExclusiveContestsAtBoundary(ctx context.Context, state *State, units []unit.Record) {
	if service == nil || service.db == nil || state == nil {
		return
	}
	if !zeroSumContestEnabled() {
		return
	}
	turn := state.TurnState.Turn
	// 低频触发：每 contestScanEveryNTurns 个部署回合扫一次（确定性 turn 取模）。
	if contestScanEveryNTurns <= 0 || turn%contestScanEveryNTurns != 0 {
		return
	}

	// 候选池：本局玩家阵营 + （WorldID 非空时）跨会话同 world 的其他单位。按确定性顺序截断控算量。
	pool := service.buildContestPool(ctx, state, units)
	if len(pool) < contestMarriageMinContenders+1 { // 至少 2 个争夺者 + 1 个标的才可能成冲突
		return
	}

	// 当前覆盖的三类标的，逐类聚合争夺者并裁决（席位/战利品复用同一裁决原语，仅意图判定与措辞不同）。
	resolved := 0
	for _, ctype := range []ContestType{ContestTypeMarriage, ContestTypeSeatInherite, ContestTypeExclusiveLoot} {
		if resolved >= contestMaxResolutionsPerScan {
			break
		}
		resolved += service.resolveContestsOfType(ctx, state, units, pool, ctype, contestMaxResolutionsPerScan-resolved)
	}
}

// contestPoolUnit 是候选池里的一个争夺者快照（自身 + 是否本会话 + 是否离线方）。
type contestPoolUnit struct {
	rec      unit.Record
	ownLocal bool // 是否本会话单位（本会话=玩家在场认真争夺；非本会话=他人单位，可能离线）
	offline  bool // 是否离线方（玩家不在场，按离线宪章兜底投入）
}

// buildContestPool 组装候选池：本会话玩家阵营存活单位 + （WorldID 非空时）跨会话同 world 的存活单位。
// 跨会话单位**只读**载入（service.units.GetByID 按 ID 读，不写）。按 unitID 确定性排序 + 截断，保证与「谁先在线」无关。
func (service *Service) buildContestPool(ctx context.Context, state *State, units []unit.Record) []contestPoolUnit {
	pool := make([]contestPoolUnit, 0, len(units))
	seen := make(map[string]bool, len(units))

	// ① 本会话单位（玩家在场）。
	for i := range units {
		u := units[i]
		if state.PlayerFactionID != "" && u.FactionID != state.PlayerFactionID {
			continue
		}
		if !contestUnitEligible(u) {
			continue
		}
		if seen[u.ID] {
			continue
		}
		seen[u.ID] = true
		pool = append(pool, contestPoolUnit{rec: u, ownLocal: true, offline: false})
		if len(pool) >= contestScanMaxUnits {
			break
		}
	}

	// ② 跨会话同 world 的其他单位（只读）。WorldID 空 → 跳过（退回纯本会话行为，向后兼容）。
	if state.WorldID != "" {
		crossIDs := service.crossWorldContestCandidateIDs(ctx, state.WorldID, state.ID)
		for _, cid := range crossIDs {
			if seen[cid] {
				continue
			}
			if len(pool) >= contestScanMaxUnits+contestCrossPoolMaxUnits {
				break
			}
			rec, err := service.units.GetByID(ctx, cid)
			if err != nil || rec.ID == "" {
				continue
			}
			if !contestUnitEligible(rec) {
				continue
			}
			seen[cid] = true
			// 离线判定：跨会话单位是否「玩家不在场」由其所属 session 的活跃态决定。此处保守地把所有跨会话单位视为
			// 「可能离线」，统一走离线宪章兜底投入下限——既不被在线方零成本抢走标的，又不假定其满额认真投入（反 P2W 中庸）。
			pool = append(pool, contestPoolUnit{rec: rec, ownLocal: false, offline: true})
		}
	}
	return pool
}

// contestUnitEligible 判定一个单位是否可纳入争夺候选（存活、可行动）。
func contestUnitEligible(u unit.Record) bool {
	if !isBattleReady(u) {
		return false
	}
	if u.Status.LifeState == unit.LifeStateDead || u.Status.LivesRemaining <= 0 {
		return false
	}
	return true
}

// crossWorldContestCandidateIDs 只读查同 world、非本 session 的存活单位 ID（按 id 确定性排序 + 截断）。
// **只 SELECT，绝不写**——跨玩家硬不变量（永不直写他人 units）。world_id 空时返回空（调用方已保证非空时才进）。
func (service *Service) crossWorldContestCandidateIDs(ctx context.Context, worldID, selfSessionID string) []string {
	if service == nil || service.db == nil || strings.TrimSpace(worldID) == "" {
		return nil
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT id FROM units
		WHERE world_id = ? AND session_id <> ? AND life_state <> ?
		ORDER BY id
		LIMIT ?`,
		worldID, selfSessionID, unit.LifeStateDead, contestCrossPoolMaxUnits,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	ids := make([]string, 0, contestCrossPoolMaxUnits)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return ids
		}
		ids = append(ids, id)
	}
	return ids
}

// resolveContestsOfType 聚合某一类排他标的的争夺者并逐标的裁决，返回本类实际裁决的标的数（受 maxResolutions 限制）。
// 三类标的复用同一骨架：① 算「单身集合」语境（仅联姻需要）；② 对每个 (争夺者, 标的) 判意图 + 算 Score；
// ③ 聚合到 targetID → []ContestContender；④ 仅对 ≥2 争夺者的标的统一裁决并留痕。
func (service *Service) resolveContestsOfType(
	ctx context.Context,
	state *State,
	localUnits []unit.Record,
	pool []contestPoolUnit,
	ctype ContestType,
	maxResolutions int,
) int {
	if maxResolutions <= 0 {
		return 0
	}
	contendersByTarget, targetName := service.aggregateContenders(ctx, pool, ctype)
	if len(contendersByTarget) == 0 {
		return 0
	}

	// 确定性遍历标的（map 顺序不确定 → 按 targetID 排序），仅对「≥2 争夺者」的标的裁决（无冲突 no-op）。
	targets := make([]string, 0, len(contendersByTarget))
	for tid := range contendersByTarget {
		targets = append(targets, tid)
	}
	sort.Strings(targets)

	resolved := 0
	for _, tid := range targets {
		if resolved >= maxResolutions {
			break
		}
		cs := contendersByTarget[tid]
		if len(cs) < contestMarriageMinContenders {
			continue // 该标的至多一个争夺者 → 无排他冲突，no-op
		}
		resource := string(ctype) + ":" + tid
		// 跨会话败者的本侧补偿不在此直写（硬不变量）；只把**本会话**败者传给 ResolveExclusiveContest 的补偿路径，
		// 跨会话败者仅经 cross_event 留痕、由其各自 session 读出翻译。
		localContenders := service.filterLocalContenders(cs, pool)
		winnerID, err := service.ResolveExclusiveContest(ctx, state, tid, resource, localContenders)
		if err != nil || winnerID == "" {
			continue // best-effort：裁决失败只吞错
		}
		// 胜者获「本回合优先推进该标的」的资格——留痕一条可读日志（实际成立仍走既有路径）。
		appendLog(
			state,
			contestLogKind(ctype),
			contestWinnerLogText(ctype, displayNameOf(targetName, tid), contenderDisplayName(localUnits, winnerID)),
			winnerID,
			tid,
		)
		// 世界总线流程留痕（非状态变更）：仅 WorldID 非空时；best-effort，绝不阻断。
		if state.WorldID != "" {
			_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
				SessionID:   state.ID,
				OwnerUnitID: winnerID,
				Code:        events.ReasonSocialObjectBind,
				Category:    events.CategoryFate,
				Payload:     map[string]any{"resource": resource, "winner": winnerID, "target": tid, "contenders": len(cs), "type": string(ctype)},
				WorldID:     state.WorldID,
			})
		}
		resolved++
	}
	return resolved
}

// filterLocalContenders 从全体争夺者里挑出**本会话**的，作为「需在本侧记补偿记忆」的对象（跨会话败者由各自 session 处理）。
// 跨会话争夺者仍已参与上游裁决（其 Score 已进 arbitration），此处只是把「本侧写记忆」的范围收敛到本库单位。
func (service *Service) filterLocalContenders(all []ContestContender, pool []contestPoolUnit) []ContestContender {
	local := make(map[string]bool, len(pool))
	for i := range pool {
		if pool[i].ownLocal {
			local[pool[i].rec.ID] = true
		}
	}
	out := make([]ContestContender, 0, len(all))
	for _, c := range all {
		// 本会话争夺者照常传入（参与裁决 + 本侧补偿）；跨会话争夺者也传入裁决，但标记 Offline，
		// recordContestConsolation 内部对读不到的他库单位天然 no-op（GetByID 仍能读到，但其记忆归本库——
		// 为严守不变量，这里把非本会话者从「本侧补偿」名单剔除，仅保留其 Score 经裁决的影响）。
		if local[c.UnitID] {
			out = append(out, c)
			continue
		}
		// 跨会话争夺者：保留进裁决但不进本侧补偿名单（Detail 清空避免误记），仍带 Offline 标记供 cross_event 留痕。
		out = append(out, ContestContender{UnitID: c.UnitID, Score: c.Score, Offline: c.Offline})
	}
	return out
}

// aggregateContenders 按标的聚合争夺者：对每个 (争夺者, 标的) 判「想争夺」意图并算零和 Score（付费不进）。
// 三类标的的意图判定与 Score 口径不同，但聚合骨架一致。返回 targetID→争夺者列表 与 targetID→展示名。
func (service *Service) aggregateContenders(ctx context.Context, pool []contestPoolUnit, ctype ContestType) (map[string][]ContestContender, map[string]string) {
	contendersByTarget := make(map[string][]ContestContender)
	targetName := make(map[string]string)

	// 联姻需要「单身集合」语境（争夺者与对象都须单身）；其余类型不需要，置空表示不约束。
	single := map[string]bool{}
	if ctype == ContestTypeMarriage {
		for i := range pool {
			if strings.TrimSpace(pool[i].rec.Social.LoverUnitID) == "" {
				single[pool[i].rec.ID] = true
			}
		}
	}

	for i := range pool {
		actor := pool[i]
		if ctype == ContestTypeMarriage && !single[actor.rec.ID] {
			continue // 已有恋人 → 不发起求亲
		}
		relations := service.loadOutgoingRelationMap(ctx, actor.rec.ID)

		for j := range pool {
			if i == j {
				continue
			}
			target := pool[j]
			wants, score, detail := contestIntentAndScore(ctype, actor, target, relations, single)
			if !wants {
				continue
			}
			// 离线方（跨会话/玩家不在场）凭离线宪章社交授权自动投入兜底：Score 不低于离线下限——
			// 让离线一侧不被在线方零成本抢走标的，但仍是真实投入口径（远低于在场认真争夺）。
			if actor.offline && score < contestOfflineCharterFloorScore {
				score = contestOfflineCharterFloorScore
			}
			contendersByTarget[target.rec.ID] = append(contendersByTarget[target.rec.ID], ContestContender{
				UnitID:  actor.rec.ID,
				Score:   score,
				Detail:  detail,
				Offline: actor.offline,
			})
			targetName[target.rec.ID] = target.rec.DisplayName()
		}
	}
	return contendersByTarget, targetName
}

// contestIntentAndScore 按标的类型判定 actor 是否「想争夺」target，并算其零和 Score（付费不进）+ 补偿文案。
// 三类标的：① 联姻——四轴好感/信任达求亲阈（标的须单身）；② 席位继承——血脉/从属关系牵引（actor 是 target 的子嗣/旧部）；
// ③ 排他战利品——对该批战利品的贡献/争夺意愿（用 actor 对 target 的竞争轴 rivalry 近似「都盯着同一批东西」）。
func contestIntentAndScore(
	ctype ContestType,
	actor, target contestPoolUnit,
	relations map[string]relationPromptRow,
	single map[string]bool,
) (bool, float64, string) {
	row, ok := relations[target.rec.ID]
	switch ctype {
	case ContestTypeMarriage:
		if !single[target.rec.ID] {
			return false, 0, "" // 对象已有恋人 → 标的已被占，非排他可争
		}
		if !ok || !marriageContenderWants(row) {
			return false, 0, ""
		}
		return true, marriageContenderScore(actor.rec, row), fmt.Sprintf("我对 %s 的心意", target.rec.DisplayName())
	case ContestTypeSeatInherite:
		// 席位继承：actor 把 target 视作「权位来源」（高信任 + 不低的竞争心=想接班）。标的是「席位」（用 target 标识其势力锚）。
		if !ok || !seatContenderWants(row) {
			return false, 0, ""
		}
		return true, seatContenderScore(actor.rec, row), fmt.Sprintf("我对 %s 那一脉权位的执念", target.rec.DisplayName())
	case ContestTypeExclusiveLoot:
		// 排他战利品：actor 与 target 在同一批战利品上较劲（明显竞争心=都盯着同一批东西）。标的用 target 标识其把守的那批。
		if !ok || !lootContenderWants(row) {
			return false, 0, ""
		}
		return true, lootContenderScore(actor.rec, row), fmt.Sprintf("我对 %s 那批东西的志在必得", target.rec.DisplayName())
	default:
		return false, 0, ""
	}
}

// marriageContenderWants 据 actor→target 四轴判定 actor 本回合是否「想与 target 求亲」（保守阈值，宁缺毋滥）。
func marriageContenderWants(row relationPromptRow) bool {
	return row.Trust >= contestMarriageTrustMin && row.Affection >= contestMarriageAffectionMin
}

// seatContenderWants 据 actor→target 四轴判定 actor 是否「想继承 target 那一脉的席位」：高信任（认其为权位正统）+ 一定竞争心（想接班）。
func seatContenderWants(row relationPromptRow) bool {
	return row.Trust >= 5.0 && row.Rivalry >= 2.0
}

// lootContenderWants 据 actor→target 四轴判定 actor 是否「与 target 争同一批排他战利品」：明显竞争心 + 不被恐惧压垮（敢上）。
func lootContenderWants(row relationPromptRow) bool {
	return row.Rivalry >= 4.0 && row.Fear < 6.0
}

// marriageContenderScore 算一名联姻争夺者的零和 Score（实力/贡献，**付费不进**）。
// 三块构成（均非付费维度）：① 对该对象的关系亲和（好感为主、信任加成）；② 自身魅力/社交属性；③ 士气状态。
// 钱包(Wallet)/付费档/SKU **绝不**进入——这是反 P2W 的口径保证。结果 clamp 到正区间（arbitration 要求 Score>0 才有意义）。
func marriageContenderScore(actor unit.Record, row relationPromptRow) float64 {
	// ① 关系亲和：好感是主驱动（×0.6），信任为辅（×0.3），戒备/竞争轻微拖累。量纲 [-10,10] → 取正贡献为主。
	affinity := row.Affection*0.6 + row.Trust*0.3 - row.Fear*0.1 - row.Rivalry*0.1
	if affinity < 0 {
		affinity = 0
	}
	// ② 自身魅力/社交：用 PrimaryStats.Charisma（社交吸引力，与战斗付费无关）做主因子，缺省给小基线。
	charisma := float64(actor.Stats.Primary.Charisma)
	if charisma <= 0 {
		charisma = 1
	}
	// ③ 士气：受保护字段只读，不改；高士气者更敢于主动表露。Morale 量纲为 [0,1]（BootstrapRecord 默认 0.7），
	// 取正值做小加成；负值（异常存档）夹到 0。仅作微调，主驱动仍是关系亲和与魅力。
	moraleAdj := actor.Status.Morale
	if moraleAdj < 0 {
		moraleAdj = 0
	}

	score := affinity*1.0 + charisma*0.4 + moraleAdj*2.0
	if score < arbitrationMinContestScore {
		score = arbitrationMinContestScore // 兜底正分：确保 arbitration 仍按 u_i 确定性排序、不退化
	}
	return score
}

// seatContenderScore 算一名席位继承争夺者的零和 Score（付费不进）：① 对权位来源的信任/认同（正统性牵引）；
// ② 自身统御/魅力（带得动一脉的硬实力）；③ 士气。钱包/付费档绝不进入。
func seatContenderScore(actor unit.Record, row relationPromptRow) float64 {
	legitimacy := row.Trust*0.5 + row.Rivalry*0.2 // 既要被认正统（trust），也要有接班的进取心（rivalry）
	if legitimacy < 0 {
		legitimacy = 0
	}
	// 统御力：用魅力近似「服众/带一脉」的能力（无独立 leadership 字段时的合理代理，与战斗付费无关）。
	command := float64(actor.Stats.Primary.Charisma)
	if command <= 0 {
		command = 1
	}
	moraleAdj := actor.Status.Morale
	if moraleAdj < 0 {
		moraleAdj = 0
	}
	score := legitimacy*1.0 + command*0.5 + moraleAdj*2.0
	if score < arbitrationMinContestScore {
		score = arbitrationMinContestScore
	}
	return score
}

// lootContenderScore 算一名排他战利品争夺者的零和 Score（付费不进）：① 争夺意愿（rivalry）；② 自身硬实力（攻防，体现「打得过守家者」的真实投入）；③ 士气。
// 战利品归属用「贡献/实力」而非钱包定夺——付费买不到「保证分到那件排他物」。
func lootContenderScore(actor unit.Record, row relationPromptRow) float64 {
	drive := row.Rivalry*0.4 - row.Fear*0.1
	if drive < 0 {
		drive = 0
	}
	// 硬实力：攻防是「敢争且争得到」的真实投入（受保护字段以外的战斗属性，非付费维度）。
	might := float64(actor.Status.Attack)*0.3 + float64(actor.Status.Defense)*0.2
	if might < 0 {
		might = 0
	}
	moraleAdj := actor.Status.Morale
	if moraleAdj < 0 {
		moraleAdj = 0
	}
	score := drive*1.0 + might + moraleAdj*2.0
	if score < arbitrationMinContestScore {
		score = arbitrationMinContestScore
	}
	return score
}

// arbitrationMinContestScore 是争夺 Score 的下限正值（避免全 0 致 arbitration 退化为纯 u_i 排序时语义不清）。
const arbitrationMinContestScore = 0.01

// contestLogKind 按标的类型选裁决日志的 kind（前端/审计可据此区分三类争夺）。
func contestLogKind(ctype ContestType) string {
	switch ctype {
	case ContestTypeSeatInherite:
		return "contest_seat"
	case ContestTypeExclusiveLoot:
		return "contest_loot"
	default:
		return "contest_marriage"
	}
}

// contestWinnerLogText 按标的类型生成胜者裁决日志文案（祖魂语气、克制）。
func contestWinnerLogText(ctype ContestType, targetDisplay, winnerDisplay string) string {
	switch ctype {
	case ContestTypeSeatInherite:
		return fmt.Sprintf("围绕 %s 那一脉的权位，%s 这一轮赢得了优先。", targetDisplay, winnerDisplay)
	case ContestTypeExclusiveLoot:
		return fmt.Sprintf("围绕 %s 把守的那批东西，%s 这一轮赢得了优先。", targetDisplay, winnerDisplay)
	default:
		return fmt.Sprintf("围绕 %s 的求亲，%s 这一轮赢得了优先。", targetDisplay, winnerDisplay)
	}
}

// displayNameOf 从 name map 取展示名，缺省回落 id。
func displayNameOf(names map[string]string, id string) string {
	if n := strings.TrimSpace(names[id]); n != "" {
		return n
	}
	return id
}

// contenderDisplayName 从单位切片里按 id 找展示名，缺省回落 id（用于裁决日志，避免再读一次 DB）。
func contenderDisplayName(units []unit.Record, id string) string {
	for i := range units {
		if units[i].ID == id {
			return units[i].DisplayName()
		}
	}
	return id
}

// nullableContestStr 把空串映射为 SQL NULL（与 cross_events 可空列语义一致）。
func nullableContestStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
