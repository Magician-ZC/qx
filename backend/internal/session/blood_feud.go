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
	"os"
	"strings"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/engine/status"
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

// mournerBond 是一个哀悼者对死者的关系快照（四轴）+ 传播跳数，用于推导「该继承多少敌意」。
type mournerBond struct {
	MournerID string
	Trust     float64
	Fear      float64
	Affection float64
	Rivalry   float64
	Hop       int // 0=直系（直接对死者有关系记录）；>0=经关系图多跳传播（递减可信度）
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

	bonds := service.loadMournerBonds(ctx, deceased.ID, perpetratorID, bloodFeudMournerLimit)
	if len(bonds) == 0 {
		return
	}

	perp := unit.Record{ID: perpetratorID}
	reason := deceased.DisplayName() + " 之死，血债待偿"
	griefApplied := 0
	for _, b := range bonds {
		rivalryDelta, fearDelta := bloodFeudInheritance(b)
		if rivalryDelta <= 0 && fearDelta <= 0 {
			continue // 不在乎死者 / 纯敌视者：不继承血仇
		}
		// 1) 哀悼者 → 凶手：继承敌意（rivalry+ / fear+），经既有 applyRelationShift（四轴、clamp±10、留痕）。
		mourner := unit.Record{ID: b.MournerID}
		_, _ = service.applyRelationShift(ctx, state, &mourner, &perp, relationDelta{
			Rivalry: rivalryDelta,
			Fear:    fearDelta,
		}, reason)

		// 2) 最亲近的几位哀悼者：哀恸士气下挫（经 status.Mutator，绝不直改 unit.Status）。
		if griefApplied < bloodFeudMaxGriefMourners && careRelevanceForDeceased(b) >= bloodFeudGriefThreshold {
			if service.applyBloodFeudGrief(ctx, state, b.MournerID, deceased, byID) {
				griefApplied++
			}
		}

		// 3) 世界总线留痕 + 命运卡（best-effort，各自吞错）。
		service.surfaceBloodFeud(ctx, state, worldID, b.MournerID, perpetratorID, deceased)
	}
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

// loadMournerBonds 读取「在乎死者」的人对死者的关系四轴（直系 hop=0），按关系强度降序，排除凶手本人与死者本人。
// 当前仅取直系哀悼者（与死者有直接关系记录）；多跳传播的可信度衰减由 bloodFeudInheritance 的 Hop 字段承载，
// 留作后续接入（取关系图二跳邻居）。best-effort：查询失败返回空（调用方据此 no-op）。
func (service *Service) loadMournerBonds(ctx context.Context, deceasedID string, perpetratorID string, limit int) []mournerBond {
	if service == nil || service.db == nil || strings.TrimSpace(deceasedID) == "" {
		return nil
	}
	if limit <= 0 {
		limit = bloodFeudMournerLimit
	}
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT source_unit_id, trust, fear, affection, rivalry FROM relations
		 WHERE target_unit_id = ?
		 ORDER BY (ABS(trust) + ABS(fear) + ABS(affection) + ABS(rivalry)) DESC
		 LIMIT ?`,
		deceasedID, limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	bonds := make([]mournerBond, 0, limit)
	for rows.Next() {
		var b mournerBond
		if scanErr := rows.Scan(&b.MournerID, &b.Trust, &b.Fear, &b.Affection, &b.Rivalry); scanErr != nil {
			continue
		}
		if b.MournerID == "" || b.MournerID == deceasedID || b.MournerID == perpetratorID {
			continue // 死者本人 / 凶手自己不哀悼
		}
		b.Hop = 0 // 直系哀悼者
		bonds = append(bonds, b)
	}
	if rows.Err() != nil {
		return nil
	}
	return bonds
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
