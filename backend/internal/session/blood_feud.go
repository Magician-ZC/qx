package session

// 文件说明：血仇（blood feud）沿关系图的确定性传播（P2，设计 docs/事件耦合与跨玩家关联.md 的「传播/结算」）。
// 当一个角色死于另一角色之手（凶手 perpetrator），那些**在乎死者**的人——对死者好感/信任高、或与之有强关系羁绊的人——
// 会按关系亲密度「继承」对凶手的敌意：经 applyRelationShift 给「哀悼者→凶手」加 rivalry/fear，强度用
// engine/relevance 的 Score + HopFidelity 按关系跳数衰减（直系哀悼者最强、远系递减）。最亲近的哀悼者另受一记
// 哀恸的士气下挫（经 status.Mutator，reason=BLOOD_FEUD_GRIEF）。世仇留痕进世界总线，并经既有命运卡路径投
// 「为TA复仇？」。
//
// 全程 flag-gated（QUNXIANG_BLOOD_FEUD，默认关 → 整段 no-op、零行为变化）+ best-effort（任何失败吞错跳过，
// 绝不影响战斗结算/阶段推进）。确定性：仅用关系四轴 + 跳数派生强度，无随机抖动、不读 time.Now 作判定。

import (
	"context"
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
	"strings"
	"time"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/engine/status"
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

// bloodFeudEnabled 读 QUNXIANG_BLOOD_FEUD（true/1/yes/on 视为开），默认关 → propagateBloodFeud 整段 no-op、零行为变化、零 DB 写。
func bloodFeudEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_BLOOD_FEUD"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
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
