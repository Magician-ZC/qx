package session

// 文件说明：PvE 威胁失败的「向旁观者传播（一人一版）」（设计 docs/PvE威胁系统.md §5/§6）。
// 当一个角色在威胁遭遇里败下阵来（fled/down 等非胜，或家乡被劫 region_ravaged）时，把这桩挫败
// 按相关性扇出给「在乎她的人 / 家乡有牵挂的人」——每人一版叙事，经 SurfaceFateEvent 三档路由进各自的命运收件箱。
// 这是把 fate.go:WorldizeDeath 已对「死亡」做的扇出，泛化到「威胁失败」这类未致死但覆水难收程度较轻的挫败：
//   - 同样从 relations 表查「在乎 victim 的人」（出边指向 victim、按四轴强度排序）；
//   - 同样逐个 SurfaceFateEvent，由 owner 自己的锚集翻译相关性、三档路由；
//   - 区别：措辞强度随关系距离衰减（relevance.HopFidelity 思路）——越亲近的旁观者，事件重要度/情绪强度越强，
//     遥远的弱关系只得到一缕淡淡的牵动（甚至 RouteAutonomous 不打扰），实现「一人一版」。
// 全程 best-effort、确定性（不用全局 rand）：副系统吞错绝不中断威胁结算主链路。

import (
	"context"
	"fmt"
	"strings"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/unit"
)

const (
	// threatFailurePropagationFanout 是单次失败传播的旁观者扇出上限（防一头怪牵动全图，
	// 与 WorldizeDeath 的 LIMIT 64 同量级、略收敛——威胁失败比死亡轻，扇出面应更窄）。
	threatFailurePropagationFanout = 48

	// 旁观者基线重要度/情绪强度（victim 与该旁观者「最亲近」时的满值），再随关系距离衰减。
	// 比 WorldizeDeath 的死亡基线（importance 8 / emotion -0.6）低一档：威胁失败未致死，旁观者的牵动也应更轻。
	threatFailureBaseImportance = 6
	threatFailureBaseEmotion    = -0.45

	// region_ravaged（家乡被劫）的旁观者基线：比单纯「她败了」更重一档——家园遭难是更广的牵挂源。
	regionRavagedBaseImportance = 7
	regionRavagedBaseEmotion    = -0.55
)

// propagateThreatFailure 把一个角色（victim）的威胁失败，按相关性扇出给「在乎她的人」，每人一版叙事。
// 返回被实际惊动（进高光卡/待决策，即非 RouteAutonomous）的旁观者人数。
//
// penaltyLayer 是 victim 本次失败实际落地的后果层（threat.go:applyDefeatPenalty 返回的 D0-D3 层）：
// 层越高（伤得越重/越不可逆），传播给旁观者的措辞越重（importance/emotion 抬一档）。
//
// best-effort：仅当能查到「在乎 victim 的人」时才有意义（与 WorldizeDeath 同口径，用 relations 表）。
// state.WorldID 非空时把世界作用域随事件下沉（供 region 分片/跨世界检索），为空亦可（同会话内的牵挂者照样扇出）。
// 任何一步出错都记下并跳过该旁观者，绝不向上抛错中断威胁结算。
func (service *Service) propagateThreatFailure(ctx context.Context, state *State, victim unit.Record, threatName string, penaltyLayer int) (int, error) {
	if service == nil || service.db == nil || state == nil || strings.TrimSpace(victim.ID) == "" {
		return 0, fmt.Errorf("propagate threat failure: missing dependencies")
	}
	return service.propagateThreatSetback(ctx, state, victim, threatName, penaltyLayer, false)
}

// propagateRegionRavaged 是 propagateThreatFailure 的「家乡被劫」变体（region_ravaged）：victim 此处是「家乡的守望者/
// 代表」，把家园遭难扇出给「对这片家乡有牵挂的人」（当前以 victim 的关系网为代理：在乎守望者的人，多半也牵挂这片家乡）。
// 措辞强度基线更高（家园之难比一场败仗更广），其余复用 propagateThreatSetback。
func (service *Service) propagateRegionRavaged(ctx context.Context, state *State, victim unit.Record, regionName string, penaltyLayer int) (int, error) {
	if service == nil || service.db == nil || state == nil || strings.TrimSpace(victim.ID) == "" {
		return 0, fmt.Errorf("propagate region ravaged: missing dependencies")
	}
	return service.propagateThreatSetback(ctx, state, victim, regionName, penaltyLayer, true)
}

// propagateThreatSetback 是失败传播的共用核心：查「在乎 victim 的人」→ 逐个按关系距离衰减措辞 → SurfaceFateEvent 路由。
// regionRavaged=true 走家乡被劫口径（基线更重、措辞为「家园遭难」）；否则走「她败下阵来」口径。
func (service *Service) propagateThreatSetback(ctx context.Context, state *State, victim unit.Record, threatOrRegion string, penaltyLayer int, regionRavaged bool) (int, error) {
	// 1) 查「在乎 victim 的人」：出边指向 victim、按四轴强度降序（与 WorldizeDeath 同一查询口径）。
	//    一并取回该旁观者→victim 的关系强度，用于按关系距离衰减措辞（一人一版）。
	bystanders, err := service.loadThreatBystanders(ctx, victim.ID, threatFailurePropagationFanout)
	if err != nil {
		return 0, fmt.Errorf("query threat bystanders: %w", err)
	}
	if len(bystanders) == 0 {
		return 0, nil
	}

	baseImportance := threatFailureBaseImportance
	baseEmotion := threatFailureBaseEmotion
	if regionRavaged {
		baseImportance = regionRavagedBaseImportance
		baseEmotion = regionRavagedBaseEmotion
	}

	victimName := victim.DisplayName()
	surfaced := 0
	var firstErr error
	for _, b := range bystanders {
		// 2) 关系距离 → 措辞衰减：把旁观者→victim 的关系强度映射成「跳数」，再用 relevance.HopFidelity 取衰减系数。
		//    亲密（强关系）≈ hop 0（不衰减）；泛泛之交 ≈ hop 1/2（措辞渐弱）。确定性、纯函数。
		fidelity := relevance.HopFidelity(relationHopForIntensity(b.intensity))
		importance := scaleImportance(baseImportance, fidelity)
		emotion := baseEmotion * fidelity

		summary := threatSetbackSummary(victimName, threatOrRegion, penaltyLayer, regionRavaged, fidelity)

		owner := unit.Record{ID: b.sourceID}
		routing, err := service.SurfaceFateEvent(ctx, state.ID, &owner, FateEvent{
			ActorID:  victim.ID,
			TargetID: victim.ID,
			// reason-code 是 SurfaceFateEvent 的「分类器」（不经 Mutator、不改状态）：用 EMOTION_TRAUMA
			//（「目睹惨烈事件后情绪受挫」）标记「目睹/闻知一场惨烈挫败」。它**不在** fateReasonIsIrreversibleClass
			// 里——威胁失败未致死，旁观者不应被升级成「不可逆」重档。
			// 待 PvE reason-codes agent 落地专用的 region_ravaged / 目睹挫败码后，此处可直接替换为该码（见 notes）。
			ReasonCode:    events.ReasonEmotionTrauma,
			Importance:    importance,
			EmotionWeight: emotion,
			Summary:       summary,
		})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue // best-effort：某旁观者路由失败不影响其余人
		}
		if routing.Route != relevance.RouteAutonomous {
			surfaced++
		}
	}
	return surfaced, firstErr
}

// threatBystander 是一个对 victim 有牵挂的旁观者及其关系强度（强度用于按距离衰减措辞）。
type threatBystander struct {
	sourceID  string
	intensity float64
}

// loadThreatBystanders 查「在乎 victim 的人」：relations 表里出边指向 victim 的源单位，按四轴强度降序、取前 limit。
// 与 WorldizeDeath 的哀悼者查询同口径（ABS(trust)+ABS(fear)+ABS(affection)+ABS(rivalry) 排序），额外取回强度供衰减。
// 确定性：排序键里追加 source_unit_id 升序兜底（同强度时稳定有序，不依赖 DB 默认顺序）。
func (service *Service) loadThreatBystanders(ctx context.Context, victimID string, limit int) ([]threatBystander, error) {
	if limit <= 0 {
		limit = threatFailurePropagationFanout
	}
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT source_unit_id, trust, fear, affection, rivalry FROM relations
		 WHERE target_unit_id = ?
		 ORDER BY (ABS(trust) + ABS(fear) + ABS(affection) + ABS(rivalry)) DESC, source_unit_id ASC
		 LIMIT ?`,
		victimID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]threatBystander, 0, limit)
	for rows.Next() {
		var source string
		var trust, fear, affection, rivalry float64
		if err := rows.Scan(&source, &trust, &fear, &affection, &rivalry); err != nil {
			return nil, err
		}
		if source == "" || source == victimID {
			continue
		}
		out = append(out, threatBystander{
			sourceID:  source,
			intensity: relationIntensity(trust, fear, affection, rivalry),
		})
	}
	return out, rows.Err()
}

// relationHopForIntensity 把旁观者→victim 的关系强度（四轴绝对值之和，[0,~40] 量级）映射成「关系距离跳数」。
// 强关系=近（hop 0，措辞不衰减）；中等=hop 1；弱/泛泛=hop 2（措辞最淡）。阈值与 relationIntensityNorm（=20）同源。
// 确定性、纯函数：越亲近越「就在身边」，越疏远越「隔着人传来的一缕风声」。
func relationHopForIntensity(intensity float64) int {
	switch {
	case intensity >= relationIntensityNorm*0.5: // ≥10：亲密/强敌，如在身边
		return 0
	case intensity >= relationIntensityNorm*0.2: // ≥4：有来往
		return 1
	default: // <4：泛泛之交
		return 2
	}
}

// scaleImportance 按衰减系数 fidelity∈(0,1] 缩放基线重要度，向下取整夹到 [1, base]（至少 1，绝不抹平为 0）。
func scaleImportance(base int, fidelity float64) int {
	v := int(float64(base) * fidelity)
	if v < 1 {
		v = 1
	}
	if v > base {
		v = base
	}
	return v
}

// threatSetbackSummary 渲染「一人一版」的旁观者叙事一句话。措辞随 fidelity（关系距离）分三档强弱，
// 并随 penaltyLayer（victim 伤势/不可逆程度）加重；regionRavaged=true 走「家园遭难」口径。确定性、纯函数、无 LLM。
func threatSetbackSummary(victimName, threatOrRegion string, penaltyLayer int, regionRavaged bool, fidelity float64) string {
	if regionRavaged {
		return regionRavagedSummary(victimName, threatOrRegion, penaltyLayer, fidelity)
	}
	consequence := threatConsequenceClause(penaltyLayer)
	switch {
	case fidelity >= 1.0: // 近：仿佛亲见
		return fmt.Sprintf("你挂念的 %s 在与%s的搏斗中败下阵来，%s", victimName, threatOrRegion, consequence)
	case fidelity >= relevance.HopDecay: // 中：辗转听闻
		return fmt.Sprintf("有消息辗转传来：%s 没能拦住那头%s，%s", victimName, threatOrRegion, consequence)
	default: // 远：隐约风声
		return fmt.Sprintf("远处隐约传来风声，说 %s 吃了%s的亏。", victimName, threatOrRegion)
	}
}

// regionRavagedSummary 渲染「家乡被劫」口径的旁观者叙事一句话（victimName 此处是家乡的守望者/代表）。
func regionRavagedSummary(victimName, regionName string, penaltyLayer int, fidelity float64) string {
	consequence := threatConsequenceClause(penaltyLayer)
	switch {
	case fidelity >= 1.0:
		return fmt.Sprintf("你牵挂的家园%s遭了劫难，守在那里的 %s %s", regionName, victimName, consequence)
	case fidelity >= relevance.HopDecay:
		return fmt.Sprintf("有消息辗转传来：%s 一带遭了劫，%s 也未能幸免，%s", regionName, victimName, consequence)
	default:
		return fmt.Sprintf("远处隐约传来风声，说 %s 那边出了乱子。", regionName)
	}
}

// threatConsequenceClause 把 victim 实际落地的后果层（D0-D3）翻成一句后果措辞（层越高越重）。确定性、纯函数。
func threatConsequenceClause(penaltyLayer int) string {
	switch {
	case penaltyLayer >= 3:
		return "伤得极重，几乎没能挺过来。"
	case penaltyLayer == 2:
		return "伤得不轻，心气也低了一截。"
	default:
		return "受了些挫，但人还在。"
	}
}
