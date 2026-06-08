package session

// 文件说明：社交自治扫描（设计 docs/事件耦合与跨玩家关联.md §2.3 + 审计·跨玩家 B）。
// 把「七种交互」从「仅 ops-HTTP 手动入口」扶正为部署边界的低频自治触发：玩家不在场时，
// 本局单位据其 actor→target 四轴关系（trust/fear/affection/rivalry，clamp [-10,10]）自动发生
// 结识/结盟/反目/复仇——命中阈值即调 service.RecordSevenInteraction，让世界总线真有自治记账方。
// 纪律对齐 auto_match.go：flag-gated（QUNXIANG_AUTO_SOCIAL **默认开**，仅 false/0/no/off 关）、
// 低频确定性触发（turn 取模 + FNV(sessionID+turn+pair) 限每回合处理少量对）、best-effort（吞错绝不中断推进）。
// 候选**仅限本会话单位**两两组合（避免共享世界下跨会话社交爆炸），确定性可复现（无全局 rand）。

import (
	"context"
	"hash/fnv"
	"os"
	"strings"

	"qunxiang/backend/internal/unit"
)

const (
	// socialScanEveryNTurns 社交扫描的部署回合周期：每 N 个部署回合扫一次（与撮合错频，低频不刷屏）。
	socialScanEveryNTurns = 3
	// socialScanMaxPairs 单次扫描最多实际处理（命中并记账）的对数上限——硬顶每回合社交音量，避免一回合刷屏。
	socialScanMaxPairs = 2
	// socialScanMaxUnits 单次扫描纳入两两组合的单位数上限（控算量：两两组合是 O(n²)，按确定性顺序截断）。
	socialScanMaxUnits = 16
	// socialDedupCycle 防重周期：同一对仅在「pairHash 与本次扫描序号在该周期内对齐」的扫描才有资格触发，
	// 使每对在每个周期内至多获得一个触发槽，配合阈值与处理上限自然收敛，避免每回合重复对同一对 acquaint。
	// 注意键用「扫描序号」(turn/socialScanEveryNTurns) 而非裸 turn——因扫描本就低频（仅 turn%EveryN==0 触发），
	// 用裸 turn 取模会让槽位只覆盖 {0, EveryN, 2*EveryN, …}%Cycle 的子集，错配掉大部分 pairHash 槽。
	socialDedupCycle = 6
)

// 社交触发阈值（量纲 [-10,10]，全部取保守值，确保只在关系明确成型/破裂时触发，宁缺毋滥）。
const (
	socialTrustWarm     = 4.0 // 结识门槛：actor→target 信任达「熟人」量级
	socialAffectionWarm = 2.5 // 结识门槛：有正向好感
	socialTrustAlly     = 6.5 // 结盟门槛：高信任
	socialAffectionAlly = 5.0 // 结盟门槛：高好感
	socialRivalryHot    = 6.0 // 反目/复仇门槛：高竞争
	socialFearHot       = 6.0 // 复仇门槛：高戒备（与高竞争同时满足才升级到复仇）
	socialFearWary      = 5.0 // 反目门槛：戒备升高（单独高竞争或高戒备即反目）
)

// autoSocialEnabled 读 QUNXIANG_AUTO_SOCIAL，**默认开**：未设/非法值视为开，仅 false/0/no/off 显式关。
// 默认开理由：社交自治是「玩家不在场时世界仍自行演化」的核心乐趣（设计 §2.3），且全程 best-effort + 低频 +
// 仅在本会话单位间撮合 + 阈值保守，行为受控；与 auto_match（默认关，会绑社会客体/牵涉 arbitration 名额）相比，
// 本扫描只往世界总线记一条关系交互、不抢稀缺资源，风险低，故默认开以让世界「活」起来。
func autoSocialEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_AUTO_SOCIAL"))) {
	case "false", "0", "no", "off":
		return false
	default:
		return true // 未设/非法/其余值 → 开
	}
}

// scanAndSocialize 在部署边界低频自治撮合社交：遍历本局存活单位两两组合，读 actor→target 四轴，
// 命中保守阈值即调 RecordSevenInteraction 记一次七种交互（结识/结盟/反目/复仇）。
// 守卫：nil 依赖 / flag 关 / WorldID 空（无世界不记跨玩家事件）/ 候选不足两人 / 未到周期 → no-op。
// 全程 best-effort：任何错误只吞掉（RecordSevenInteraction 内部已含跨分片安全），绝不影响阶段推进。
func (service *Service) scanAndSocialize(ctx context.Context, state *State, units []unit.Record) {
	if service == nil || service.db == nil || state == nil {
		return
	}
	if !autoSocialEnabled() {
		return
	}
	worldID := state.WorldID
	if worldID == "" {
		return // 无世界域：不记跨玩家事件
	}
	turn := state.TurnState.Turn
	// 低频触发：每 socialScanEveryNTurns 个部署回合扫一次（确定性 turn 取模）。
	if socialScanEveryNTurns <= 0 || turn%socialScanEveryNTurns != 0 {
		return
	}
	// 扫描序号：本次是第几次社交扫描（用作防重周期键，使槽位覆盖完整 [0,Cycle) 而非裸 turn 的稀疏子集）。
	scanIndex := turn / socialScanEveryNTurns

	// 候选池：本局玩家阵营、存活的角色（仅本会话单位 → 避免共享世界下跨会话社交爆炸）。
	pool := make([]unit.Record, 0, len(units))
	for i := range units {
		u := units[i]
		if state.PlayerFactionID != "" && u.FactionID != state.PlayerFactionID {
			continue
		}
		if u.Status.LifeState == unit.LifeStateDead || u.Status.LivesRemaining <= 0 {
			continue
		}
		pool = append(pool, u)
		if len(pool) >= socialScanMaxUnits {
			break // 控算量：两两组合 O(n²)，按确定性遍历顺序截断
		}
	}
	if len(pool) < 2 {
		return
	}

	processed := 0
	// 两两有向组合：对每个 actor，读其对外关系一次（map），再看其对池内其余成员的四轴是否跨阈。
	for i := range pool {
		if processed >= socialScanMaxPairs {
			break // 每回合处理上限：硬顶社交音量
		}
		actor := pool[i]
		relations := service.loadOutgoingRelationMap(ctx, actor.ID)
		if len(relations) == 0 {
			continue
		}
		for j := range pool {
			if processed >= socialScanMaxPairs {
				break
			}
			if i == j {
				continue
			}
			target := pool[j]
			row, ok := relations[target.ID]
			if !ok {
				continue // 尚无关系行 → 无四轴可判，跳过（结识需先有正向积累）
			}
			// 防重：仅当该有向对的稳定哈希在本周期内与「扫描序号」对齐才有资格触发，
			// 使同一对每个周期至多一个触发槽，配合阈值/上限自然收敛、不每回合重复同一交互。
			if socialDedupCycle > 0 && pairCycleSlot(state.ID, actor.ID, target.ID) != scanIndex%socialDedupCycle {
				continue
			}
			interaction, ok := classifySocialInteraction(row)
			if !ok {
				continue // 未跨任何阈值：关系未明确成型/破裂，宁缺毋滥
			}
			importance := socialImportanceFor(interaction)
			// best-effort：内部已做世界总线记账 + 跨分片安全的关系应用，吞错绝不中断推进。
			if _, err := service.RecordSevenInteraction(ctx, worldID, actor.ID, target.ID, interaction, importance); err != nil {
				continue
			}
			processed++
		}
	}
}

// classifySocialInteraction 据 actor→target 四轴（[-10,10]）保守判定应触发的七种交互之一。
// 优先级：敌意（复仇 > 反目）先于善意（结盟 > 结识），重后果优先——同一对若既有高竞争又有正向，按敌意处理。
// 返回 ok=false 表示未跨任何阈值（不触发）。trade/marriage/war 不在自治范围（联姻/开战属重决策，留给玩家/LLM）。
func classifySocialInteraction(row relationPromptRow) (SevenInteraction, bool) {
	switch {
	// 复仇：高竞争 且 高戒备（最重，对方既是对手又令我惧惮 → 先发制人的敌意）。
	case row.Rivalry >= socialRivalryHot && row.Fear >= socialFearHot:
		return InteractionVengeance, true
	// 反目：高竞争 或 戒备明显升高（关系已实质破裂，但未到复仇量级）。
	case row.Rivalry >= socialRivalryHot || row.Fear >= socialFearWary:
		return InteractionFallout, true
	// 结盟：高信任 且 高好感（羁绊已成 → 升级为正式同盟）。
	case row.Trust >= socialTrustAlly && row.Affection >= socialAffectionAlly:
		return InteractionAlliance, true
	// 结识：信任达熟人量级 且 有正向好感（初步善意成型）。
	case row.Trust >= socialTrustWarm && row.Affection >= socialAffectionWarm:
		return InteractionAcquaint, true
	default:
		return "", false
	}
}

// socialImportanceFor 给自治社交交互一个保守的世界总线重要度（结盟/复仇是大事，结识/反目其次）。
func socialImportanceFor(interaction SevenInteraction) int {
	switch interaction {
	case InteractionAlliance, InteractionVengeance:
		return 5
	case InteractionFallout:
		return 4
	default: // 结识
		return 2
	}
}

// pairCycleSlot 为有向对 (actor→target) 派生一个稳定的 [0, socialDedupCycle) 触发槽（确定性 FNV，无全局 rand）。
// 仅当 turn%socialDedupCycle 等于该槽位时，本对才有资格触发，从而每个周期内每对至多一个触发窗口（防每回合重复）。
func pairCycleSlot(sessionID, actorID, targetID string) int {
	if socialDedupCycle <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte("social_pair:" + sessionID + ":" + actorID + "->" + targetID))
	return int(h.Sum64() % uint64(socialDedupCycle))
}
