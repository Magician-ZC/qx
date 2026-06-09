package session

// 文件说明：血仇（blood feud）沿关系图的确定性传播（P2，设计 docs/事件耦合与跨玩家关联.md 的「传播/结算」）。
// 当一个角色死于另一角色之手（凶手 perpetrator），那些**在乎死者**的人——对死者好感/信任高、或与之有强关系羁绊的人——
// 会按关系亲密度「继承」对凶手的敌意：经 applyRelationShift 给「哀悼者→凶手」加 rivalry/fear，强度用
// engine/relevance 的 Score + HopFidelity 按关系跳数衰减（直系哀悼者最强、远系递减）。最亲近的哀悼者另受一记
// 哀恸的士气下挫（经 status.Mutator，reason=BLOOD_FEUD_GRIEF）。世仇留痕进世界总线，并经既有命运卡路径投
// 「为TA复仇？」。
//
// 全程 flag-gated（QUNXIANG_BLOOD_FEUD，**默认开** → 仅显式置 false/0/no/off 才整段 no-op、零行为变化）+
// best-effort（任何失败吞错跳过，绝不影响战斗结算/阶段推进）。确定性：仅用关系四轴 + 跳数派生强度，无随机抖动、不读 time.Now 作判定。

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/socialobject"
	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/worldbus"
)

const (
	// bloodFeudMournerLimit 单次死亡最多处理的哀悼者数（按关系强度降序截断，控算量、防全图洪泛）。
	bloodFeudMournerLimit = 32

	// bloodFeudGriefThreshold 仅「真正在乎」死者（对死者的牵挂相关度 ≥ 此阈）的哀悼者才施加哀恸士气下挫，
	// 与世仇敌意继承（更宽松、按强度连续衰减）区分——避免给路人也强加悲恸。
	bloodFeudGriefThreshold = 0.45

	// bloodFeudMaxGriefMourners 一次死亡最多施加哀恸的人数（只动最亲近的几位，避免一死全军士气崩）。
	bloodFeudMaxGriefMourners = 3

	// 世仇敌意继承的基准幅度（乘以 relevance 强度后落到 rivalry/fear 上；clamp 仍由 applyRelationShift 兜底 ±10）。
	bloodFeudRivalryBase = 3.2 // 直系（hop=0）强锚满命中时的 rivalry 增量上限基准
	bloodFeudFearBase    = 1.4 // 同上的 fear 增量基准（敌意里带几分忌惮）

	// bloodFeudGriefMorale 最亲近哀悼者的哀恸士气下挫幅度（负向，经 Mutator clamp 落地）。
	bloodFeudGriefMorale = -0.06

	// bloodFeudMaxHop 血仇沿关系图传播的最大跳数（对齐 relevance.MaxHops=2，防全图洪泛）。
	// hop=0 直系哀悼者；hop=1「在乎死者的人」的至亲；hop=2「在乎『在乎死者的人』的人」（二手消息）。
	bloodFeudMaxHop = relevance.MaxHops

	// bloodFeudHopFanout 每一跳从一个已知哀悼者出发，最多再扩展的邻居数（控指数爆炸）。
	bloodFeudHopFanout = 8
)

// bloodFeudEnabled 读 QUNXIANG_BLOOD_FEUD，**默认开**（未设/非法值 → 视为开，玩家默认即可感知血仇传播）。
// 仅当显式置 false/0/no/off 时才关 → propagateBloodFeud 整段 no-op、零行为变化、零 DB 写（用于回退/对照）。
func bloodFeudEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(featureflags.EnvOrOverride("QUNXIANG_BLOOD_FEUD"))) {
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
}

// mournerBond 是一个哀悼者对「上一跳节点」的关系快照（四轴）+ 传播跳数，用于推导「该继承多少敌意」。
// hop=0 时四轴是哀悼者对**死者**的关系；hop>0 时是哀悼者对**上一跳哀悼者**（FromUnit）的关系——
// 即「在乎『在乎死者的人』」，二手消息按 HopFidelity 失真衰减。
type mournerBond struct {
	MournerID string
	FromUnit  string // 这一跳消息从谁传来：hop=0 为死者本人；hop>0 为上一跳哀悼者（propagation_log 的 from_unit）
	Trust     float64
	Fear      float64
	Affection float64
	Rivalry   float64
	Hop       int // 0=直系（直接对死者有关系记录）；>0=经关系图多跳传播（递减可信度）
}

// bloodFeudNullableUnit 把空 unit id 映射为 SQL NULL（propagation_log.from_unit 可空：源头跳无上游）。
func bloodFeudNullableUnit(id string) any {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	return id
}

// careRelevanceForDeceased 把哀悼者对死者的关系四轴，经 engine/relevance 翻译成「牵挂相关度」∈ [0,1]。
// 用 relation 锚（weight=关系强度归一）+ HopFidelity(hop) 的 noisy-OR 评分：关系越强、跳数越浅 → 相关度越高。
// 纯函数、确定性、零 DB——是「谁继承多少敌意」的可单测核心。
func careRelevanceForDeceased(b mournerBond) float64 {
	weight := relationIntensity(b.Trust, b.Fear, b.Affection, b.Rivalry) / relationIntensityNorm
	if weight <= 0 {
		return 0
	}
	if weight > 1 {
		weight = 1
	}
	hits := []relevance.Hit{{
		Anchor: relevance.Anchor{Kind: relevance.Relation, Ref: "deceased", Weight: weight},
	}}
	return relevance.Score(hits, relevance.HopFidelity(b.Hop))
}

// bloodFeudInheritance 是世仇继承的纯函数核心：输入哀悼者对死者的关系四轴 + 传播跳数，
// 输出该哀悼者「应继承的、对凶手的敌意增量」（rivalry+ / fear+）。
//
// 语义与单调性（可单测）：
//   - 只有「在乎死者」（好感/信任为正、净亲密度>0）的人才继承敌意；纯粹敌视死者的人（affection/trust 负）继承为 0
//     （死了你的仇人，你不会去恨杀他的人）。
//   - 继承强度 ∝ careRelevance（关系越亲密越强），并按 HopFidelity 跳数衰减 → **直系哀悼者敌意 > 远系**。
//   - fear 增量恒 ≤ rivalry 增量（敌意以「仇」为主、带几分忌惮）。
//
// 返回 (rivalryDelta, fearDelta)，均 ≥0。强度满命中（直系强羁绊）≈ bloodFeudRivalryBase。
func bloodFeudInheritance(b mournerBond) (rivalryDelta float64, fearDelta float64) {
	// 净亲密度：只有正向牵挂（爱/信任压过仇/惧）的人才会为死者继承血仇。
	closeness := b.Affection*0.6 + b.Trust*0.4 - b.Rivalry*0.3 - b.Fear*0.2
	if closeness <= 0 {
		return 0, 0
	}
	rel := careRelevanceForDeceased(b) // [0,1]，已含 HopFidelity 跳数衰减
	if rel <= 0 {
		return 0, 0
	}
	rivalryDelta = bloodFeudRivalryBase * rel
	fearDelta = bloodFeudFearBase * rel
	return rivalryDelta, fearDelta
}

// propagateBloodFeud 把「死者死于凶手之手」沿关系图传播成世仇：在乎死者的人继承对凶手的敌意。
// best-effort + flag-gated：flag 关 / 缺依赖 / 凶手或死者 ID 空 / 凶手即死者 → 直接 no-op。任何子步失败只吞错跳过。
// 绝不影响战斗结算（调用方以 _ 忽略返回；这里也不返回 error）。
//
// worldID 可空（未接多世界时跳过世界总线留痕，仍做本库关系继承/哀恸/命运卡）。
// byID 可空：执行主循环持有的「unit_id → 活指针」映射。哀悼者若在其中（多为死者同场存活的同伴，
// 即主循环会再次 Save 的活单位），哀恸 morale 的 Mutator 结果须回写其内存态，否则后续
// units.Save(*actor/*ally) 会用缺了悲恸的旧内存态整列覆盖落库值（内存↔DB 失同步、悲恸丢失）。
func (service *Service) propagateBloodFeud(
	ctx context.Context,
	state *State,
	deceased unit.Record,
	perpetratorID string,
	worldID string,
	byID map[string]*unit.Record,
) {
	if !bloodFeudEnabled() {
		return // flag 关：零行为变化
	}
	if service == nil || service.db == nil || state == nil {
		return
	}
	perpetratorID = strings.TrimSpace(perpetratorID)
	if perpetratorID == "" || deceased.ID == "" || perpetratorID == deceased.ID {
		return // 无凶手（自然死/环境死）或自戕 → 无世仇可传
	}

	// 多跳关系图传播：hop=0 直系哀悼者 → hop=1「在乎死者的人」的至亲 → hop=2「在乎『在乎死者的人』的人」。
	// 每跳带 from_unit（消息从谁传来）+ HopFidelity(hop)=0.6^hop 的二手失真，强度随跳衰减。
	bonds := service.loadMournerBonds(ctx, deceased.ID, perpetratorID, bloodFeudMournerLimit)
	if len(bonds) == 0 {
		return
	}

	sessionID := ""
	if state != nil {
		sessionID = state.ID
	}
	// origin_event_id：以「死者+凶手」派生确定性源 id，使同一桩死亡的多跳传播留痕可串联回溯（无随机、可复现）。
	originEventID := bloodFeudOriginEventID(sessionID, deceased.ID, perpetratorID)

	perp := unit.Record{ID: perpetratorID}
	reason := deceased.DisplayName() + " 之死，血债待偿"
	griefApplied := 0
	for _, b := range bonds {
		rivalryDelta, fearDelta := bloodFeudInheritance(b)
		if rivalryDelta <= 0 && fearDelta <= 0 {
			continue // 不在乎死者 / 纯敌视者：不继承血仇
		}
		// 0) 传播留痕：这一跳的边（from_unit→该哀悼者）+ 跳数 + HopFidelity 失真，进 propagation_log（best-effort）。
		service.logBloodFeudPropagation(ctx, sessionID, originEventID, b.FromUnit, b.MournerID, b.Hop)

		// 1) 哀悼者 → 凶手：继承敌意（rivalry+ / fear+），经既有 applyRelationShift（四轴、clamp±10、留痕）。
		mourner := unit.Record{ID: b.MournerID}
		_, _ = service.applyRelationShift(ctx, state, &mourner, &perp, relationDelta{
			Rivalry: rivalryDelta,
			Fear:    fearDelta,
		}, reason)

		// 2) 最亲近的几位哀悼者：哀恸士气下挫（经 status.Mutator，绝不直改 unit.Status）。
		//    仅直系（hop=0）才施加哀恸——二手消息不致深切悲恸。
		if b.Hop == 0 && griefApplied < bloodFeudMaxGriefMourners && careRelevanceForDeceased(b) >= bloodFeudGriefThreshold {
			if service.applyBloodFeudGrief(ctx, state, b.MournerID, deceased, byID) {
				griefApplied++
			}
		}

		// 3) 世界总线留痕 + 命运卡（best-effort，各自吞错）。
		service.surfaceBloodFeud(ctx, state, worldID, b.MournerID, perpetratorID, deceased)
	}
}

// bloodFeudOriginEventID 由 (session|deceased|perpetrator) 派生确定性传播源 id，
// 使同一桩死亡引发的多跳传播在 propagation_log 中共享 origin_event_id，可回溯串联（无随机、可复现）。
func bloodFeudOriginEventID(sessionID, deceasedID, perpetratorID string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte("blood_feud_origin:" + sessionID + ":" + deceasedID + ":" + perpetratorID))
	return fmt.Sprintf("bf_%016x", h.Sum64())
}

// logBloodFeudPropagation 把血仇传播的一跳（from→to，hop，fidelity=0.6^hop）追加进 propagation_log（append-only 留痕）。
// best-effort：缺 db / to 空 → 跳过；写失败吞错，绝不影响传播继续。确定性行 id 由 (origin|from|to|hop) 派生（幂等去重）。
func (service *Service) logBloodFeudPropagation(ctx context.Context, sessionID, originEventID, fromUnit, toUnit string, hop int) {
	if service == nil || service.db == nil || strings.TrimSpace(toUnit) == "" {
		return
	}
	fidelity := relevance.HopFidelity(hop)
	h := fnv.New64a()
	_, _ = h.Write([]byte("blood_feud_prop:" + originEventID + ":" + fromUnit + ":" + toUnit + ":" + strconv.Itoa(hop)))
	rowID := fmt.Sprintf("pl_%016x", h.Sum64())
	// 定宽 UTC 时间串（字典序==时间序，双驱动 ORDER BY created_at 一致；MySQL 列默认空串故须显式写）。
	createdAt := time.Now().UTC().Format("2006-01-02 15:04:05")
	if dbdialect.IsMySQL(service.db) {
		_, _ = service.db.ExecContext(ctx,
			`INSERT INTO propagation_log (id, session_id, origin_event_id, from_unit, to_unit, hop, fidelity, created_at)
			 VALUES (?,?,?,?,?,?,?,?)
			 ON DUPLICATE KEY UPDATE fidelity=VALUES(fidelity)`,
			rowID, sessionID, originEventID, bloodFeudNullableUnit(fromUnit), toUnit, hop, fidelity, createdAt)
		return
	}
	_, _ = service.db.ExecContext(ctx,
		`INSERT INTO propagation_log (id, session_id, origin_event_id, from_unit, to_unit, hop, fidelity, created_at)
		 VALUES (?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET fidelity=excluded.fidelity`,
		rowID, sessionID, originEventID, bloodFeudNullableUnit(fromUnit), toUnit, hop, fidelity, createdAt)
}

// applyBloodFeudGrief 给一位哀悼者施加哀恸士气下挫（经 status.Mutator，reason=BLOOD_FEUD_GRIEF）。
// 返回是否成功落地（用于计数最亲近哀悼者）。best-effort：读不到单位 / Mutator 报错 → 返回 false、不影响其它哀悼者。
//
// byID 可空：若哀悼者在执行主循环的活指针映射中（多为死者同场存活的同伴，主循环会再次 Save 它们），
// 须把 Mutator 结果回写其内存态（含 Status.Morale 与 RecentEventIDs），否则后续 units.Save(*actor/*ally)
// 会用缺了悲恸的旧内存态整列覆盖落库值，导致哀恸 morale 被静默回滚（内存↔DB 失同步）。
// 不在 byID 中的哀悼者（跨会话/离线）仅靠这次 DB 写即正确持久，不受影响。
func (service *Service) applyBloodFeudGrief(ctx context.Context, state *State, mournerID string, deceased unit.Record, byID map[string]*unit.Record) bool {
	if service == nil || service.mutator == nil || strings.TrimSpace(mournerID) == "" {
		return false
	}
	turn := 0
	if state != nil {
		turn = state.TurnState.Turn
	}
	result, err := service.mutator.Apply(ctx, status.Mutation{
		UnitID:     mournerID,
		Turn:       turn,
		Field:      status.FieldMorale,
		Delta:      bloodFeudGriefMorale,
		ReasonCode: events.ReasonBloodFeudGrief,
		ReasonText: deceased.DisplayName() + " 的死讯传来，她久久无法平静",
		Actors:     []string{deceased.ID},
		// §7.3 events 作用域双写：哀恸事件随会话世界三键留痕（mutationScopeFromState 对 nil state 安全返零值）。
		Scope: mutationScopeFromState(state),
	})
	if err != nil {
		return false
	}
	// 回写内存态：保持执行主循环持有的活指针与落库值一致，避免后续 units.Save 用旧态覆盖悲恸。
	if byID != nil {
		if m := byID[mournerID]; m != nil {
			*m = result.Record
		}
	}
	return true
}

// surfaceBloodFeud 把一桩世仇留痕进世界总线（CROSS_VENGEANCE，gate 在 worldID）并经既有命运路径投「为TA复仇？」卡。
// 全程 best-effort：worldID 空只跳过总线、仍投命运卡；任何步失败吞错。复用现有收件箱机制，不新造。
func (service *Service) surfaceBloodFeud(ctx context.Context, state *State, worldID string, mournerID string, perpetratorID string, deceased unit.Record) {
	if service == nil || service.db == nil {
		return
	}
	// 1) 世界总线留痕（接入多世界时）：哀悼者 vs 凶手的世仇成为不可篡改事实源。
	if strings.TrimSpace(worldID) != "" {
		_, _ = service.RecordCrossInteraction(ctx, worldID, mournerID, perpetratorID,
			worldbus.KindVengeance, 8, map[string]any{
				"blood_feud": true,
				"deceased":   deceased.ID,
			})
	}
	// 2) 命运卡：把这桩血仇按相关性路由进哀悼者的命运收件箱（「为TA复仇？」），复用 SurfaceFateEvent。
	//    owner=哀悼者；ActorID=凶手（牵动她的对象），TargetID=哀悼者本人（走自相关路径、且 FK 落本库存在的她）。
	owner := unit.Record{ID: mournerID}
	sessionID := ""
	if state != nil {
		sessionID = state.ID
	}
	_, _ = service.SurfaceFateEvent(ctx, sessionID, &owner, FateEvent{
		ActorID:       perpetratorID,
		TargetID:      mournerID,
		ReasonCode:    events.ReasonVengeanceFulfilled,
		Importance:    8,
		EmotionWeight: -0.7,
		Summary:       deceased.DisplayName() + " 死在了那个人手里。你要为TA讨回这笔血债吗？",
	})
}

// loadMournerBonds 沿关系图做**多跳**哀悼者发现（确定性 BFS，hop 0→bloodFeudMaxHop）：
//   - hop=0：直接在乎死者的人（relations.target=死者），FromUnit=死者；
//   - hop=1：在乎「直系哀悼者」的人（relations.target=某直系哀悼者），即「在乎『在乎死者的人』的人」，FromUnit=该直系哀悼者；
//   - hop=2：再外扩一圈（二手再二手）。
//
// 二手消息失真由 mournerBond.Hop 承载——careRelevanceForDeceased/bloodFeudInheritance 经 relevance.HopFidelity(hop)=0.6^hop
// 让 hop 越远的哀悼者继承的敌意越弱。每跳从一个已知哀悼者扇出 bloodFeudHopFanout 个最强邻居（控指数爆炸），
// 全图去重（一个人只按其**最浅**跳数记一次，先到先得）。返回按 (hop 升序, 关系强度降序) 排列，hop=0 在前。
//
// best-effort：任一跳查询失败只截断该跳、返回已发现的；凶手本人/死者本人/已访问者跳过。确定性：仅靠关系强度排序，无随机。
func (service *Service) loadMournerBonds(ctx context.Context, deceasedID string, perpetratorID string, limit int) []mournerBond {
	if service == nil || service.db == nil || strings.TrimSpace(deceasedID) == "" {
		return nil
	}
	if limit <= 0 {
		limit = bloodFeudMournerLimit
	}

	bonds := make([]mournerBond, 0, limit)
	visited := map[string]bool{deceasedID: true, perpetratorID: true} // 死者/凶手不作哀悼者
	// frontier：当前跳已确定的「上一跳节点」集合（hop=0 的前沿是死者本人）。
	frontier := []string{deceasedID}

	for hop := 0; hop <= bloodFeudMaxHop; hop++ {
		if len(bonds) >= limit || len(frontier) == 0 {
			break
		}
		// 每跳从 frontier 出发收集邻居（在乎 frontier 节点的人）作为本跳哀悼者，并作为下一跳的前沿。
		nextFrontier := make([]string, 0, len(frontier)*bloodFeudHopFanout)
		// hop=0 直系哀悼者用整 limit；hop>0 每个上游节点限 bloodFeudHopFanout 扇出。
		perNodeLimit := bloodFeudHopFanout
		if hop == 0 {
			perNodeLimit = limit
		}
		for _, fromUnit := range frontier {
			if len(bonds) >= limit {
				break
			}
			neighbors := service.loadCarersOf(ctx, fromUnit, perNodeLimit)
			for _, nb := range neighbors {
				if len(bonds) >= limit {
					break
				}
				if nb.MournerID == "" || visited[nb.MournerID] {
					continue // 全图去重：只按最浅跳数记一次
				}
				visited[nb.MournerID] = true
				nb.Hop = hop
				nb.FromUnit = fromUnit
				bonds = append(bonds, nb)
				nextFrontier = append(nextFrontier, nb.MournerID)
			}
		}
		frontier = nextFrontier
	}
	return bonds
}

// loadCarersOf 读「在乎 targetID 的人」对 targetID 的关系四轴（relations.target=targetID），按关系强度降序取前 limit。
// 多跳 BFS 的一跳扩展原语：hop=0 时 targetID=死者→得直系哀悼者；hop>0 时 targetID=上一跳哀悼者→得二手哀悼者。
// best-effort：查询失败返回空（调用方据此截断该跳）。Hop/FromUnit 由调用方填充（本函数只取关系快照）。
func (service *Service) loadCarersOf(ctx context.Context, targetID string, limit int) []mournerBond {
	if service == nil || service.db == nil || strings.TrimSpace(targetID) == "" {
		return nil
	}
	if limit <= 0 {
		limit = bloodFeudHopFanout
	}
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT source_unit_id, trust, fear, affection, rivalry FROM relations
		 WHERE target_unit_id = ?
		 ORDER BY (ABS(trust) + ABS(fear) + ABS(affection) + ABS(rivalry)) DESC, source_unit_id ASC
		 LIMIT ?`,
		targetID, limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]mournerBond, 0, limit)
	for rows.Next() {
		var b mournerBond
		if scanErr := rows.Scan(&b.MournerID, &b.Trust, &b.Fear, &b.Affection, &b.Rivalry); scanErr != nil {
			continue
		}
		if b.MournerID == "" || b.MournerID == targetID {
			continue
		}
		out = append(out, b)
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// BloodFeudEntry 是某角色的一条世仇关系（她对某人怀有的、以 rivalry/fear 为主的强敌意）。
type BloodFeudEntry struct {
	TargetUnitID string  `json:"target_unit_id"`
	TargetName   string  `json:"target_name,omitempty"`
	Rivalry      float64 `json:"rivalry"`
	Fear         float64 `json:"fear"`
	Trust        float64 `json:"trust"`
	Affection    float64 `json:"affection"`
}

// bloodFeudRivalryGate 列入世仇清单的最低 rivalry 阈（够「成仇」才算，避免把普通竞争都列进来）。
const bloodFeudRivalryGate = 4.0

// ListBloodFeuds 列出某角色当前怀有的世仇关系（rivalry ≥ bloodFeudRivalryGate 的对外关系），按敌意降序。
// 纯读、不 flag-gate（读历史无副作用）；供前端/调试查看血仇网络。limit<=0 取默认 32。
func (service *Service) ListBloodFeuds(ctx context.Context, unitID string, limit int) ([]BloodFeudEntry, error) {
	if service == nil || service.db == nil {
		return nil, nil
	}
	unitID = strings.TrimSpace(unitID)
	if unitID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = bloodFeudMournerLimit
	}
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT r.target_unit_id, COALESCE(u.display_name, ''), r.rivalry, r.fear, r.trust, r.affection
		 FROM relations r
		 LEFT JOIN units u ON u.id = r.target_unit_id
		 WHERE r.source_unit_id = ? AND r.rivalry >= ?
		 ORDER BY r.rivalry DESC, r.fear DESC
		 LIMIT ?`,
		unitID, bloodFeudRivalryGate, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]BloodFeudEntry, 0, limit)
	for rows.Next() {
		var e BloodFeudEntry
		if scanErr := rows.Scan(&e.TargetUnitID, &e.TargetName, &e.Rivalry, &e.Fear, &e.Trust, &e.Affection); scanErr != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ── 血仇衍生撮合：让世仇成为撮合候选来源（设计 docs/事件耦合与跨玩家关联.md §2.2 的「钩子驱动撮合」）──
//
// 直觉：两个对**同一个人**怀有血仇的角色，天然有「共同的敌人」这条强叙事钩子——可被撮合进一个
// common_enemy / 复仇同盟类社会客体（「敌人的敌人是朋友」）。本段产出这类候选，交给既有
// MatchIntoSocialObject 打分→过门→arbitration 确定性择人。flag-gated（QUNXIANG_BLOOD_FEUD 关→空）、
// 确定性（仅靠关系强度 + sessionID 派生的钩子，无随机）、best-effort（查询失败返回空）。

const (
	// bloodFeudMatchKind 血仇衍生社会客体的类型（共敌同盟）。
	bloodFeudMatchKind = "common_enemy"
	// bloodFeudMatchLabelPrefix 标签前缀（后接共同仇敌的 id，使不同仇敌产出不同确定性社会客体）。
	bloodFeudMatchLabelPrefix = "共敌·"
	// bloodFeudMatchMinAllies 共敌同盟最少需要的成员数（一个人不成「同盟」）。
	bloodFeudMatchMinAllies = 2
)

// BloodFeudMatchGroup 是一组「对同一仇敌怀有血仇」的候选 + 其撮合元数据，供 Wire 阶段喂给 MatchIntoSocialObject。
type BloodFeudMatchGroup struct {
	EnemyID    string           // 共同仇敌的 unit id（社会客体的撮合锚）
	Kind       string           // 社会客体类型（common_enemy）
	Label      string           // 社会客体标签（共敌·<enemyID>）
	Candidates []MatchCandidate // 对该仇敌怀有血仇的角色们（四因子已填，可直接撮合）
}

// BloodFeudMatchCandidates 扫描候选单位的世仇关系，按「共同仇敌」聚类，产出 common_enemy 撮合候选组。
// 对每个被≥bloodFeudMatchMinAllies 名候选共同怀恨的仇敌，组一个候选组；组内每名角色的四因子：
//   - RelationIntersect = 其对该共敌的世仇强度归一（恨得越深越该入伙）；
//   - HookFit = sessionID+enemy+unit 派生的确定性「复仇钩子」契合度（无全局 rand，可复现）；
//   - GeoNear/DensityAdj 由 Wire 阶段按情境补全（这里不读地理/锚密度，保持本文件自洽与确定性）。
//
// flag 关 / 无 db / 候选不足 → 返回 nil（调用方据此 no-op）。确定性：仇敌按 id 升序、组内候选按 id 升序。best-effort。
func (service *Service) BloodFeudMatchCandidates(ctx context.Context, sessionID string, candidateUnits []unit.Record) []BloodFeudMatchGroup {
	if !bloodFeudEnabled() {
		return nil // flag 关：零行为变化
	}
	if service == nil || service.db == nil || len(candidateUnits) < bloodFeudMatchMinAllies {
		return nil
	}

	// 共敌 → 怀恨者列表（每项含怀恨者 id 与其对该共敌的世仇强度）。
	// 候选池即 candidateUnits（只在传入的角色之间撮合共敌同盟；仇敌本身无须在池内）。
	type hater struct {
		unitID  string
		rivalry float64
	}
	byEnemy := make(map[string][]hater)
	for i := range candidateUnits {
		uid := candidateUnits[i].ID
		if uid == "" {
			continue
		}
		feuds, err := service.ListBloodFeuds(ctx, uid, bloodFeudMournerLimit)
		if err != nil {
			continue // best-effort：该角色查询失败只跳过她
		}
		for _, f := range feuds {
			if f.TargetUnitID == "" || f.TargetUnitID == uid {
				continue
			}
			byEnemy[f.TargetUnitID] = append(byEnemy[f.TargetUnitID], hater{unitID: uid, rivalry: f.Rivalry})
		}
	}
	if len(byEnemy) == 0 {
		return nil
	}

	// 仇敌按 id 升序遍历（确定性），对每个被≥min 名候选共恨的仇敌组一个候选组。
	enemyIDs := make([]string, 0, len(byEnemy))
	for enemyID := range byEnemy {
		enemyIDs = append(enemyIDs, enemyID)
	}
	sortStrings(enemyIDs)

	groups := make([]BloodFeudMatchGroup, 0, len(enemyIDs))
	for _, enemyID := range enemyIDs {
		haters := byEnemy[enemyID]
		if len(haters) < bloodFeudMatchMinAllies {
			continue // 只有一个人恨他，不成共敌同盟
		}
		// 组内按怀恨者 id 升序（确定性，插入排序）。
		for i := 1; i < len(haters); i++ {
			for j := i; j > 0 && haters[j-1].unitID > haters[j].unitID; j-- {
				haters[j-1], haters[j] = haters[j], haters[j-1]
			}
		}
		cands := make([]MatchCandidate, 0, len(haters))
		for _, h := range haters {
			cands = append(cands, MatchCandidate{
				UnitID:            h.unitID,
				RelationIntersect: bloodFeudHatredNorm(h.rivalry), // 恨得越深越该入伙
				HookFit:           bloodFeudRevengeHook(sessionID, enemyID, h.unitID),
				// GeoNear / DensityAdj 留给 Wire 阶段按情境补（本文件不读地理/锚密度）。
			})
		}
		groups = append(groups, BloodFeudMatchGroup{
			EnemyID:    enemyID,
			Kind:       bloodFeudMatchKind,
			Label:      bloodFeudMatchLabelPrefix + enemyID,
			Candidates: cands,
		})
	}
	return groups
}

// bloodFeudHatredNorm 把对共敌的 rivalry（[-10,10] 量级）归一为 [0,1] 的「入伙意愿」（仅取正向，rivalry≤0 即 0）。
func bloodFeudHatredNorm(rivalry float64) float64 {
	if rivalry <= 0 {
		return 0
	}
	v := rivalry / 10.0
	if v > 1 {
		return 1
	}
	return v
}

// bloodFeudRevengeHook 用 sessionID+enemy+unit 的 FNV 派生稳定的 [0,1]「复仇钩子」契合度（无全局 rand，可复现）。
func bloodFeudRevengeHook(sessionID, enemyID, unitID string) float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("blood_feud_revenge_hook:" + sessionID + ":" + enemyID + ":" + unitID))
	return float64(h.Sum64()%10000) / 10000.0
}

// sortStrings 对字符串切片做升序原地排序（确定性遍历用；不引 sort 包以保持本文件依赖最小）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// ════════════════════════════════════════════════════════════════════════════
// 黑吃黑（CROSS_BETRAYAL）+ 罗生门 echo + 衍生传播 + blood_feud 社会客体
//   设计 docs/事件耦合与跨玩家关联.md §2.7（共享历史与可仲裁证据链）+ PvE §7（黑吃黑）。
//
// 缺口与落地：一条 cross_event（背叛/偷袭）发生后——
//   ① 沿受害者两侧关系图传播（复用既有 relations BFS）→ 对第三方（她信任的人的密友）按关系强度收
//      衰减后的 ReasonCrossDerived「你信任的人被人偷袭了」（不再用 VENGEANCE_FULFILLED 代偿）；
//   ② 为每个收到的 owner 写一条 cross_event_echo（视角化叙事 narrative_zh + relevance + route + hop）——
//      同一 cross_event_id 多 owner 各一条 echo（罗生门），但事实唯一：争议回退 cross_events.occurred_at 仲裁；
//   ③ 黑吃黑给受害者经 SurfaceFateEvent 投回应卡（追讨/认栽记仇/求和），受害者侧 Mutator 只改本侧
//      （A 对 B trust- / rivalry+ + 生成 debt_grudge_love 锚）——永不直写背叛者一侧；
//   ④ 撮合系统据 ReasonCrossDerived 生成 blood_feud 社会客体，把受害者 + 其密友绑成对抗背叛者的同盟。
//
// 全程 flag-gated（复用 QUNXIANG_BLOOD_FEUD，默认开 → 仅 false/0/no/off 整段 no-op）+ best-effort
// （任何子步失败吞错跳过，绝不阻断主结算）+ 确定性（FNV 派生，无随机）+ 反 P2W（无 wallet/billing 入裁决）。
//
// 跨玩家硬不变量：只产 append-only cross_event；echo 仅视角叙事层、事实唯一；各自 Mutator/applyRelationShift
// 只改本侧 relations，**永不直写背叛者/第三方他人的 units/relations**（衍生侧只继承「对背叛者的敌意」，是本侧主动方向）。
// ════════════════════════════════════════════════════════════════════════════

const (
	// crossDerivedHopLimit 黑吃黑衍生传播沿受害者关系图的最大跳数（对齐 relevance.MaxHops，防全图洪泛）。
	crossDerivedHopLimit = relevance.MaxHops
	// crossDerivedFanout 每个上游节点最多扇出的密友数（控指数爆炸）。
	crossDerivedFanout = 8
	// crossDerivedMournerLimit 一桩黑吃黑最多衍生到的第三方人数（按关系强度降序截断）。
	crossDerivedMournerLimit = 24
	// crossDerivedImportanceBase 衍生卡的基准重要度（hop=0 直系密友最高，按 HopFidelity 衰减）。
	crossDerivedImportanceBase = 7
	// crossDerivedValence 衍生事件的情绪效价（负向：你信任的人被偷袭，是坏消息）。
	crossDerivedValence = -0.5
	// bloodFeudSocialObjectKind 黑吃黑衍生社会客体类型（受害者+密友 vs 背叛者的对抗同盟）。
	bloodFeudSocialObjectKind = "blood_feud"
)

// PropagateCrossBetrayal 是黑吃黑（CROSS_BETRAYAL）的完整落地编排：受害者回应卡 + 衍生传播 + 罗生门 echo + blood_feud 同盟。
// crossEventID 是那条已写进世界总线、append-only、带权威 occurred_at 的背叛事件 id（事实唯一源）；victim/betrayer 是双方角色 id；
// betrayalSummary 是这桩黑吃黑的一句话（用于受害者卡）。worldID 可空（无世界则跳过同盟绑定，仍投受害者卡/写本库 echo）。
//
// 全程 best-effort + flag-gated：flag 关 / 缺依赖 / id 空 / victim==betrayer → no-op。任一子步失败只吞错跳过，绝不阻断主结算。
// 返回被实际衍生（写了 echo）的第三方人数，供调用方遥测/测试（错误从不上抛，跨玩家旁路不阻断主流程）。
func (service *Service) PropagateCrossBetrayal(
	ctx context.Context,
	sessionID string,
	worldID string,
	crossEventID string,
	victim unit.Record,
	betrayerID string,
	betrayalSummary string,
) int {
	if !bloodFeudEnabled() {
		return 0 // flag 关：零行为变化
	}
	if service == nil || service.db == nil {
		return 0
	}
	crossEventID = strings.TrimSpace(crossEventID)
	betrayerID = strings.TrimSpace(betrayerID)
	if crossEventID == "" || victim.ID == "" || betrayerID == "" || victim.ID == betrayerID {
		return 0
	}

	// LOW① 当日冷却（防回声室洪泛）：同 (betrayer,victim,CROSS_BETRAYAL) 每自然日 ≤1 次全量传播。
	// 缺这道闸时，自治反目（social_scan）对同一对每约 18 turn 触发一次（pairCycleSlot），每次都全量铺开
	// 受害者卡 + 衍生第三方 + echo + 同盟绑定 → 同一桩反目被反复放大。复用 worldize_inbound.go 的
	// inboundCooldownActive 范式：按 (actor=betrayer, target=victim, code=CROSS_BETRAYAL) + UTC 日窗查 events 去重。
	// 冷却内 return 0、不重复传播（与 flag 关同样零行为）。冷却是软抑制：查错保守放行（见 crossBetrayalCooldownActive）。
	if service.crossBetrayalCooldownActive(ctx, betrayerID, victim.ID) {
		return 0
	}
	// 先写冷却锚（best-effort），使本日后续同 (betrayer,victim,CROSS_BETRAYAL) 传播被上面这道闸挡下。
	// 写在实际传播之前：即便后续子步部分失败，当日也不再重复全量铺开（去重优先于完整度，反洪泛硬要求）。
	service.markCrossBetrayalCooldown(ctx, sessionID, betrayerID, victim.ID)

	// ③ 黑吃黑受害者回应卡（追讨/认栽记仇/求和）+ 受害者侧本侧关系增量 + debt_grudge_love 锚。
	service.surfaceBetrayalVictimCard(ctx, sessionID, victim, betrayerID, crossEventID, betrayalSummary)

	// ①② 沿受害者关系图衍生传播到「她信任的人的密友」，每人一条 ReasonCrossDerived + 一条罗生门 echo。
	derivedOwners := service.propagateCrossDerived(ctx, sessionID, worldID, crossEventID, victim, betrayerID)

	// ④ 撮合系统据 ReasonCrossDerived 生成 blood_feud 社会客体：受害者 + 衍生密友绑成对抗背叛者的同盟。
	service.bindBloodFeudAlliance(ctx, worldID, crossEventID, victim.ID, betrayerID, derivedOwners)

	return len(derivedOwners)
}

// crossBetrayalCooldownMarker 是当日黑吃黑传播冷却锚的 payload 判别键（与 worldize_inbound 的 WORLDIZE_OUTBOUND
// 锚区分：后者无 kind 键、reason 取入向白名单码；本锚有此 kind 且 reason=CROSS_BETRAYAL，两者 payload 永不互相误命中）。
const crossBetrayalCooldownMarker = "cross_betrayal_cooldown"

// crossBetrayalCooldownActive 判定同 (betrayer,victim,CROSS_BETRAYAL) 当天是否已传播过（已写过冷却锚）。
// 复用 worldize_inbound.go 的 inboundCooldownActive 范式：按 actor=betrayer 的 WORLDIZE_OUTBOUND 锚在 UTC 日窗内查，
// 再用 payload 精确比对 kind+target+reason（payload 是确定性 JSON，含这三键）。冷却是软抑制：查错保守返回 false
// （放行，宁可多传一次也不静默吞掉应有的血仇牵连——与 inboundCooldownActive 同向）。
func (service *Service) crossBetrayalCooldownActive(ctx context.Context, betrayerID, victimID string) bool {
	if service == nil || service.db == nil || strings.TrimSpace(betrayerID) == "" {
		return false
	}
	dayLo := dayStartUTC().Format(time.RFC3339Nano)
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT payload_json FROM events
		 WHERE actor_unit_id = ? AND reason_code = ? AND occurred_at >= ?`,
		betrayerID, string(events.ReasonWorldizeOutbound), dayLo,
	)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var payloadJSON string
		if scanErr := rows.Scan(&payloadJSON); scanErr != nil {
			return false
		}
		// 精确比对 kind（区分自 worldize 锚）+ target（victim）+ reason（CROSS_BETRAYAL）。
		if strings.Contains(payloadJSON, `"kind":"`+crossBetrayalCooldownMarker+`"`) &&
			strings.Contains(payloadJSON, `"reason":"`+string(events.ReasonCrossBetrayal)+`"`) &&
			strings.Contains(payloadJSON, `"target":"`+victimID+`"`) {
			return true
		}
	}
	return false
}

// markCrossBetrayalCooldown 写一条当日黑吃黑传播冷却锚（WORLDIZE_OUTBOUND，payload 带 kind 判别键 + betrayer/victim/code）。
// best-effort：缺 db / betrayer 空 → 跳过；写失败吞错（冷却是软抑制，写不成只是本日可能多传一次，不阻断主结算）。
// OwnerUnitID=betrayer（恒真实单位），RelatedUnitID 故意留空回落为 owner——victim 完整写进 payload.target 供冷却比对
// （与 worldize_inbound 同款：target 写 payload 而非 target_unit_id，避免非单位引用触发 events 表 FK，本处亦保持一致）。
func (service *Service) markCrossBetrayalCooldown(ctx context.Context, sessionID, betrayerID, victimID string) {
	if service == nil || service.db == nil || strings.TrimSpace(betrayerID) == "" {
		return
	}
	_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   sessionID,
		OwnerUnitID: betrayerID,
		Code:        events.ReasonWorldizeOutbound,
		Category:    events.CategoryFate,
		Payload: map[string]any{
			"kind":   crossBetrayalCooldownMarker,
			"actor":  betrayerID,
			"target": victimID,
			"reason": string(events.ReasonCrossBetrayal),
		},
	})
}

// surfaceBetrayalVictimCard 给黑吃黑受害者投一张回应卡（追讨/认栽记仇/求和，经 SurfaceFateEvent 走自相关待决策路径），
// 并在受害者**本侧**落关系恶化（A 对 B trust- / rivalry+，经既有 applyRelationShift、clamp±10、留痕）+ 一根 debt_grudge_love 锚
// （这桩血债结成了一根久驻心头的弦）。**只改受害者本侧**——永不直写背叛者一侧（跨玩家硬不变量）。
// best-effort：任一步失败吞错，绝不影响衍生传播/同盟绑定。回应卡的三选项由 SurfaceFateEvent 的情境化 Copilot 按 reason-code 生成。
func (service *Service) surfaceBetrayalVictimCard(ctx context.Context, sessionID string, victim unit.Record, betrayerID string, crossEventID string, betrayalSummary string) {
	if service == nil || service.db == nil || victim.ID == "" {
		return
	}
	// 1) 受害者本侧关系恶化：对背叛者 trust- / rivalry+（黑吃黑是实质的反目，比一般反目更重）。
	//    state 传 nil（黑吃黑在跨玩家旁路触发、可能在回合循环外）；applyRelationShift 内部 clamp±10 + 写 notes 留痕。
	betrayer := unit.Record{ID: betrayerID}
	v := victim
	_, _ = service.applyRelationShift(ctx, nil, &v, &betrayer, relationDelta{
		Trust:   -4,
		Rivalry: 4,
		Fear:    1,
	}, "黑吃黑·被信任的人反咬")

	// 2) debt_grudge_love 锚：这笔血债从此是她心上久驻的一根弦（半衰走关系锚半衰，比泛泛关系更久）。best-effort。
	_ = service.UpsertDebtAnchor(ctx, sessionID, victim.ID, betrayerID, bloodFeudHatredNorm(4), "血债·"+victim.DisplayName())

	// 3) 受害者回应卡：经 SurfaceFateEvent 走自相关待决策路径（owner=受害者，TargetID=受害者本人 → FK 落本库存在的她；
	//    ActorID=背叛者，仅入 payload 非 FK）。reason=ReasonCrossBetrayal 让情境化 Copilot 产出贴合的（追讨/认栽记仇/求和）选项。
	summary := strings.TrimSpace(betrayalSummary)
	if summary == "" {
		summary = "本该与她共担的人，临头却反咬了她一口。这笔账，她要怎么算？"
	}
	owner := victim
	_, _ = service.SurfaceFateEvent(ctx, sessionID, &owner, FateEvent{
		ActorID:       betrayerID,
		TargetID:      victim.ID,
		ReasonCode:    events.ReasonCrossBetrayal,
		Importance:    8,
		EmotionWeight: -0.7,
		Summary:       summary,
	})
	_ = crossEventID // crossEventID 是受害者侧事实锚，已由世界总线持有；此处自相关卡无需再绑（echo 才需）。
}

// propagateCrossDerived 把一桩黑吃黑沿受害者关系图衍生到「她信任的人的密友」（确定性 BFS，复用 loadCarersOf 的多跳发现）：
//   - hop=0：直接在乎受害者的人（她信任的人）；hop=1：在乎「她信任的人」的密友（你信任的人的密友）；hop≤crossDerivedHopLimit。
//
// 对每个第三方 owner：① 经 SurfaceFateEvent 投一条 ReasonCrossDerived 牵挂卡（强度按 HopFidelity=0.6^hop 衰减——
// 直系密友最强、远系递减）；② 写一条 cross_event_echo（视角化叙事 + relevance + route + hop，罗生门：同一 cross_event_id
// 各 owner 一条）。返回成功衍生（写了 echo）的第三方 unitID 列表（供 blood_feud 同盟绑定）。
//
// best-effort：任一第三方失败只吞错跳过。确定性：仅靠关系强度排序 + FNV 派生 echo id，无随机。
func (service *Service) propagateCrossDerived(ctx context.Context, sessionID string, worldID string, crossEventID string, victim unit.Record, betrayerID string) []string {
	if service == nil || service.db == nil || victim.ID == "" {
		return nil
	}
	// 复用既有多跳哀悼者发现：从受害者出发沿 relations 图找「在乎她的人」（及其密友），排除背叛者本人。
	bonds := service.loadMournerBonds(ctx, victim.ID, betrayerID, crossDerivedMournerLimit)
	if len(bonds) == 0 {
		return nil
	}
	owners := make([]string, 0, len(bonds))
	for _, b := range bonds {
		if b.MournerID == "" || b.MournerID == victim.ID || b.MournerID == betrayerID {
			continue
		}
		// 衍生强度：只有真正在乎受害者的人（净亲密度>0）才被「你信任的人被偷袭了」牵动；纯敌视者跳过。
		rel := careRelevanceForDeceased(b) // [0,1]，已含 HopFidelity 跳数衰减
		closeness := b.Affection*0.6 + b.Trust*0.4 - b.Rivalry*0.3 - b.Fear*0.2
		if rel <= 0 || closeness <= 0 {
			continue
		}
		// ① 衍生牵挂卡：ReasonCrossDerived「你信任的人被人偷袭了」。owner=第三方；TargetID=第三方本人（自相关、FK 落本库存在的她）；
		//    ActorID=背叛者（牵动她的对象，仅入 payload）。重要度按 HopFidelity 衰减 → 直系密友更可能进前台、远系沉为背景噪声。
		importance := crossDerivedImportance(b.Hop)
		ownerRec := unit.Record{ID: b.MournerID}
		routing, err := service.SurfaceFateEvent(ctx, sessionID, &ownerRec, FateEvent{
			ActorID:       betrayerID,
			TargetID:      b.MournerID,
			ReasonCode:    events.ReasonCrossDerived,
			Importance:    importance,
			EmotionWeight: crossDerivedValence * relevance.HopFidelity(b.Hop),
			Summary:       crossDerivedSummary(victim, b.Hop),
		})
		route := relevance.RouteAutonomous
		fateScore := 0.0
		if err == nil {
			route = routing.Route
			fateScore = routing.Relevance
		}
		// ② 罗生门 echo：同一 cross_event_id 各 owner 视角化一条，事实唯一（争议回退 cross_events.occurred_at）。
		service.writeCrossEventEcho(ctx, sessionID, b.MournerID, crossEventID, fateScore, fateScore,
			string(route), crossDerivedSummary(victim, b.Hop), crossDerivedValence*relevance.HopFidelity(b.Hop), b.Hop)

		owners = append(owners, b.MournerID)
		_ = worldID // worldID 衍生侧仅 blood_feud 绑定用，传播本身只写本库 echo（不再产新 cross_event，避免传播放大事实源）。
	}
	return owners
}

// crossDerivedImportance 把跳数映射成衍生牵挂卡的重要度：hop=0 取基准，按 HopFidelity 衰减（直系密友最强、远系递减）。
// 纯函数、确定性、可测。下限 1（再远的传闻也至少留痕一档，但多半被 FateScore 路由为自治不打扰）。
func crossDerivedImportance(hop int) int {
	v := int(float64(crossDerivedImportanceBase)*relevance.HopFidelity(hop) + 0.5)
	if v < 1 {
		return 1
	}
	return v
}

// crossDerivedSummary 从第三方视角措辞「你信任的人被人偷袭了」（hop 越远越像辗转传闻，对齐不可靠叙事）。
func crossDerivedSummary(victim unit.Record, hop int) string {
	name := victim.DisplayName()
	if name == "" {
		name = "她信任的一个人"
	}
	switch {
	case hop <= 0:
		return name + "被本该共担的人在背后捅了一刀——你信任的人，遭了暗算。"
	case hop == 1:
		return "听说" + name + "那边出了事，被信过的人反咬了一口。"
	default:
		return "辗转传来一桩走了样的旧事：" + name + "好像被人黑吃黑了。"
	}
}

// writeCrossEventEcho 写一条 cross_event_echo（视角化叙事层，事件耦合 §2.7「echo 仅视角叙事，事实唯一回退 cross_events」）。
// 同一 cross_event_id 在多个 owner/session 各有一条 echo（罗生门），但**事实唯一**——争议恒回退 cross_events 原表 occurred_at 仲裁。
// 行 id 由 (cross_event_id|session|owner) 派生确定性（幂等去重：同一桩事对同一 owner 只一条 echo，重复编排不堆叠）。
// best-effort：缺 db / owner 空 → 跳过；写失败吞错，绝不影响传播继续。双驱动（SQLite ON CONFLICT / MySQL ON DUPLICATE）。
func (service *Service) writeCrossEventEcho(ctx context.Context, sessionID, ownerUnitID, crossEventID string, relevanceScore, fateScore float64, route, narrativeZH string, valence float64, hop int) {
	if service == nil || service.db == nil || strings.TrimSpace(ownerUnitID) == "" || strings.TrimSpace(crossEventID) == "" {
		return
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte("cross_echo:" + crossEventID + ":" + sessionID + ":" + ownerUnitID))
	rowID := fmt.Sprintf("ce_%016x", h.Sum64())
	createdAt := time.Now().UTC().Format("2006-01-02 15:04:05")
	if dbdialect.IsMySQL(service.db) {
		_, _ = service.db.ExecContext(ctx,
			`INSERT INTO cross_event_echoes (id, session_id, owner_unit_id, cross_event_id, relevance, fate_score, route, narrative_zh, valence, hop, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?)
			 ON DUPLICATE KEY UPDATE relevance=VALUES(relevance), fate_score=VALUES(fate_score), route=VALUES(route), narrative_zh=VALUES(narrative_zh), valence=VALUES(valence), hop=VALUES(hop)`,
			rowID, sessionID, ownerUnitID, crossEventID, relevanceScore, fateScore, route, narrativeZH, valence, hop, createdAt)
		return
	}
	_, _ = service.db.ExecContext(ctx,
		`INSERT INTO cross_event_echoes (id, session_id, owner_unit_id, cross_event_id, relevance, fate_score, route, narrative_zh, valence, hop, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET relevance=excluded.relevance, fate_score=excluded.fate_score, route=excluded.route, narrative_zh=excluded.narrative_zh, valence=excluded.valence, hop=excluded.hop`,
		rowID, sessionID, ownerUnitID, crossEventID, relevanceScore, fateScore, route, narrativeZH, valence, hop, createdAt)
}

// CrossEventEcho 是一条视角化 echo（供前端/测试读取罗生门视角集）。事实唯一性由 cross_event_id 串回 cross_events 保证。
type CrossEventEcho struct {
	OwnerUnitID  string  `json:"owner_unit_id"`
	CrossEventID string  `json:"cross_event_id"`
	Relevance    float64 `json:"relevance"`
	FateScore    float64 `json:"fate_score"`
	Route        string  `json:"route"`
	NarrativeZH  string  `json:"narrative_zh"`
	Valence      float64 `json:"valence"`
	Hop          int     `json:"hop"`
}

// ListCrossEventEchoes 列出同一 cross_event_id 的全部视角化 echo（罗生门视角集），按 (hop 升序, owner 升序) 确定性排列。
// 纯读、不 flag-gate（读历史无副作用）。供前端展示「三个玩家都记得的那次背叛」的各自视角，及测试验证多视角同 cross_event_id。
func (service *Service) ListCrossEventEchoes(ctx context.Context, crossEventID string) ([]CrossEventEcho, error) {
	if service == nil || service.db == nil {
		return nil, nil
	}
	crossEventID = strings.TrimSpace(crossEventID)
	if crossEventID == "" {
		return nil, nil
	}
	rows, err := service.db.QueryContext(ctx,
		`SELECT owner_unit_id, cross_event_id, relevance, fate_score, route, narrative_zh, valence, hop
		 FROM cross_event_echoes WHERE cross_event_id = ?
		 ORDER BY hop ASC, owner_unit_id ASC`,
		crossEventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CrossEventEcho, 0)
	for rows.Next() {
		var e CrossEventEcho
		if scanErr := rows.Scan(&e.OwnerUnitID, &e.CrossEventID, &e.Relevance, &e.FateScore, &e.Route, &e.NarrativeZH, &e.Valence, &e.Hop); scanErr != nil {
			continue
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// bindBloodFeudAlliance 据黑吃黑的衍生网络生成一个 blood_feud 社会客体：把受害者 + 其密友绑成对抗背叛者的同盟
// （设计 §2.7「撮合系统据 CROSS_DERIVED 生成 blood_feud 社会客体把 A、C 绑成对抗 B 的同盟」）。
// 直接在本文件内经既有 socialobject helper 落客体 + 绑成员（受害者+密友），不走 arbitration（同盟是「共担血债」的天然结盟，
// 非稀缺名额争夺，无需择人；反 P2W 不涉及——成员资格只由「是否被这桩血债牵动」决定，无 wallet/billing）。
//
// label 绑 (crossEventID|betrayer)：同一桩背叛幂等复用同一社会客体 id（衍生网络扩大时增量加成员、不重复建客体）。
// worldID 空 / 无密友 → no-op（社会客体需世界域；只有受害者一人不成「同盟」）。best-effort：失败吞错，绝不阻断主结算。
func (service *Service) bindBloodFeudAlliance(ctx context.Context, worldID string, crossEventID string, victimID string, betrayerID string, friends []string) {
	if service == nil || service.db == nil {
		return
	}
	worldID = strings.TrimSpace(worldID)
	if worldID == "" || len(friends) == 0 || victimID == "" {
		return // 无世界域 / 只受害者一人 → 不成同盟
	}
	// 成员集合 = 受害者 + 衍生密友（去重，排除背叛者本人——背叛者不会入对抗自己的同盟）。
	members := make([]string, 0, len(friends)+1)
	seen := map[string]bool{}
	for _, id := range append([]string{victimID}, friends...) {
		id = strings.TrimSpace(id)
		if id == "" || id == betrayerID || seen[id] {
			continue
		}
		seen[id] = true
		members = append(members, id)
	}
	if len(members) < bloodFeudMatchMinAllies {
		return // 不足两人不成同盟
	}
	sortStrings(members) // 确定性成员顺序

	// label 绑 (crossEventID|betrayer)：同一桩黑吃黑复用同一确定性社会客体 id（幂等：衍生网络扩大时增量加成员）。
	label := "血仇·" + betrayerID + "·" + crossEventID
	key := worldID + "|" + bloodFeudSocialObjectKind + "|" + label
	objID, err := socialobject.Create(ctx, service.db, socialobject.SocialObject{
		ID: socialObjectID(key), WorldID: worldID, Kind: bloodFeudSocialObjectKind, Label: label,
	})
	if err != nil {
		return // best-effort：建客体失败即放弃同盟（衍生卡/echo 已落，主结算不受影响）
	}
	for _, uid := range members {
		// Score=「被这桩血债牵动的强度」：受害者满分；密友按其对受害者的关系强度归一（恨得越深/爱得越切越铁）。
		score := 1.0
		if uid != victimID {
			score = service.bloodFeudAllyScore(ctx, uid, victimID)
		}
		if err := socialobject.AddMember(ctx, service.db, socialobject.Member{ObjectID: objID, UnitID: uid, Score: score}); err != nil {
			continue // best-effort：单个成员绑定失败只跳过，不影响其余成员
		}
		// 留痕（流程事件，非状态变更）：social_objects 非单位，RelatedUnitID 留空（回退 owner），object_id 入 payload。
		_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
			SessionID: worldID, OwnerUnitID: uid,
			Code: events.ReasonSocialObjectBind, Category: events.CategoryFate,
			Payload: map[string]any{"object_id": objID, "kind": bloodFeudSocialObjectKind, "label": label, "score": score, "against": betrayerID},
			WorldID: worldID,
		})
	}
}

// bloodFeudAllyScore 把一名密友对受害者的关系强度归一为 [0,1] 的「入伙血仇同盟意愿」（关系越强越铁）。
// best-effort：读不到关系行返回保守 0.5（既然已被衍生发现，至少有基本牵动）。确定性、纯读。
func (service *Service) bloodFeudAllyScore(ctx context.Context, allyID, victimID string) float64 {
	if service == nil || service.db == nil {
		return 0.5
	}
	var trust, fear, affection, rivalry float64
	err := service.db.QueryRowContext(ctx,
		`SELECT trust, fear, affection, rivalry FROM relations WHERE source_unit_id = ? AND target_unit_id = ?`,
		allyID, victimID,
	).Scan(&trust, &fear, &affection, &rivalry)
	if err != nil {
		return 0.5
	}
	v := relationIntensity(trust, fear, affection, rivalry) / relationIntensityNorm
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
