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

	"qunxiang/backend/internal/engine/arbitration"
	"qunxiang/backend/internal/engine/events"
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

	// 交易门槛：一段「有来有往的生意关系」——正向信任 + 一点好感 + 戒备/竞争都不高（互利而非羁绊或敌意）。
	// 全部**低于**结识门（trust 4 / affection 2.5），故按优先级排在结识之后只接「更浅的互利对」，不改既有结识判定。
	socialTrustTrade      = 3.0 // 交易门槛：actor→target 有正向信任（够做一桩生意）
	socialAffectionTrade  = 1.5 // 交易门槛：有一丝好感（非纯利害）
	socialRivalryTradeMax = 3.0 // 交易门槛上限：竞争不高（高竞争走反目/复仇线，不做生意）
	socialFearTradeMax    = 3.0 // 交易门槛上限：戒备不高（高戒备不会与之交易）

	// 联姻门槛：比结盟更深的羁绊——极高好感 + 高信任（亲事是不可逆重决策，门要更高）。
	// affection 8 高于结盟的 5、trust 6.5 与结盟同级，故按优先级排在结盟之前只接「好感深到谈婚论嫁」的对。
	socialAffectionMarriage = 8.0 // 联姻门槛：极高好感（深于结盟）
	socialTrustMarriage     = 6.5 // 联姻门槛：高信任

	// 开战门槛：比复仇更极端的敌意升级——竞争与戒备都拉满（势力级卷入，落到个人=盟友/家人被卷入）。
	// rivalry 8 / fear 7 均高于复仇的 6/6，故按优先级排在复仇之前只接「敌意烈到要开战」的对。
	socialRivalryWar = 8.0 // 开战门槛：极高竞争（深于复仇）
	socialFearWar    = 7.0 // 开战门槛：极高戒备
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
			// 统一管线：先把交互记进世界总线（append-only 事实源）+ 按后果层经 consent_gate 分级路由本侧关系增量。
			//   - 结识/交易=层1 unilateral 立即应用；结盟/反目=层2 contested；联姻/复仇/开战=层3 requires_consent（挂 pending 待对方角色自治回应）。
			// best-effort：内部已做世界总线记账 + 跨分片安全的关系应用，吞错绝不中断推进。
			res, err := service.RecordSevenInteraction(ctx, worldID, actor.ID, target.ID, interaction, importance)
			if err != nil {
				continue
			}
			// 交易的 arbitration 定价（设计 §2.3「交易：成功 trust+2 / 违约 trust-4·rivalry+3」）：
			// 仅当交易已 unilateral 落地（res.Applied）时叠加确定性成败差量——成功补足到 +2，违约改判为 -4·rivalry+3。
			// 与 arbitration 同philosophy：成败仅由确定性投入(Score)派生、与频率/顺序无关、付费不进 Score。
			if interaction == InteractionTrade && res.Applied {
				service.settleAutonomousTradePricing(ctx, state, &pool[i], &pool[j], row)
			}
			// 开战落到势力级（设计 §2.3「开战：faction 级 rivalry+fear；落到个人=她的盟友/家人被卷入」）：
			// 仅在 actor/target 属不同势力时把本侧 FactionRelations 置 war（同势力则 canonicalFactionPair 拒、安全 no-op）。
			// **只改本侧** state.FactionRelations，绝不直写他人 session——满足跨玩家硬不变量。
			if interaction == InteractionWar {
				service.frameAutonomousWar(state, &pool[i], &pool[j])
			}
			// 黑吃黑/罗生门落地（设计 §2.7）：自治反目（InteractionFallout → worldbus.KindBetrayal）就是一桩背叛——
			// 把那条已写进世界总线、带权威 occurred_at 的 cross_event（res.EventID）喂给 PropagateCrossBetrayal：
			// 受害者收回应卡 + 衍生第三方收 ReasonCrossDerived + cross_event_echoes 罗生门 + blood_feud 同盟绑定。
			// victim = 被反目的一方（target=pool[j]）；betrayer = actor。同一 eventID 只在此处传播一次（避免双触发）。
			// 全程 best-effort（PropagateCrossBetrayal 内部吞错 + flag-gated QUNXIANG_BLOOD_FEUD 默认开），绝不阻断扫描推进。
			if interaction == InteractionFallout && res.EventID != "" {
				_ = service.PropagateCrossBetrayal(ctx, state.ID, worldID, res.EventID, pool[j], actor.ID, target.DisplayName()+" 被本该共担的人反咬了一口。")
			}
			processed++
		}
	}
}

// settleAutonomousTradePricing 为一桩已 unilateral 落地的自治交易做 arbitration 定价的成败叠加（设计 §2.3）。
// RecordSevenInteraction 已按交易模板对本侧记了基础增量（trust+1/affection+0.5，「公平交易基线」）；
// 本函数据**确定性**成败判定在其上叠加差量，使最终落到设计口径：
//   - 成功（履约）：再 +1 信任 → 累计 trust 约 +2；
//   - 违约（毁约）：trust 改判到约 -4（在 +1 基线上 -5）、rivalry+3。
//
// 成败由 tradeHonored 用 arbitration.Resolve 确定性派生（Score=actor 对 target 的信任投入 vs 固定违约基线，
// Key 含 session+turn+pair → 同输入同结果、与频率/顺序无关、**付费不进 Score**，反 P2W）。
// 全程 best-effort：增量经 applyRelationShift（只改本侧 source→target relations，clamp [-10,10]），失败只吞错。
func (service *Service) settleAutonomousTradePricing(ctx context.Context, state *State, actor, target *unit.Record, row relationPromptRow) {
	if service == nil || actor == nil || target == nil {
		return
	}
	var delta relationDelta
	reason := "自治·交易履约"
	if tradeHonored(state, actor.ID, target.ID, row) {
		delta = relationDelta{Trust: 1} // 在 +1 基线上补足 → 约 +2
	} else {
		delta = relationDelta{Trust: -5, Rivalry: 3} // 在 +1 基线上 -5 → 约 -4；并 rivalry+3
		reason = "自治·交易违约"
	}
	// 只改本侧（source=actor → target），best-effort 吞错绝不中断。
	_, _ = service.applyRelationShift(ctx, state, actor, target, delta, reason)
}

// tradeHonored 确定性判定一桩自治交易是否履约（true=成功 / false=违约），与频率/入队顺序无关、付费不进 Score。
// 复用 arbitration.Resolve：把「履约」「违约」当作两名虚拟争夺者，各自 Score 为确定性投入——
//   - 履约 Score = 基线 1 + max(0, actor→target 信任)/2（信任越高越倾向履约）；
//   - 违约 Score = 基线 1 + max(0, actor→target 竞争)/2（竞争越高越倾向毁约）。
//
// Key 含 session+turn+actor+target → 同一对在同一回合的成败可复现。胜者=「履约」即成功。
// 这让「成功/违约」是关系投入的确定性函数（信任高→多半履约、竞争高→多半违约），而非随机或频率驱动。
func tradeHonored(state *State, actorID, targetID string, row relationPromptRow) bool {
	sessionID := ""
	turn := 0
	if state != nil {
		sessionID = state.ID
		turn = state.TurnState.Turn
	}
	honorScore := 1.0
	if row.Trust > 0 {
		honorScore += row.Trust / 2.0
	}
	breachScore := 1.0
	if row.Rivalry > 0 {
		breachScore += row.Rivalry / 2.0
	}
	key := "autotrade:" + sessionID + ":" + actorID + "->" + targetID + ":" + itoa(turn)
	out := arbitration.Resolve(arbitration.Contest{
		Key:      key,
		Resource: "trade_honor",
		Contestants: []arbitration.Contestant{
			{UnitID: "honor", Score: honorScore},
			{UnitID: "breach", Score: breachScore},
		},
	})
	return out.WinnerID == "honor"
}

// itoa 是无依赖的小整数转字符串（用于拼 arbitration Key，避免引入 strconv 仅为一处）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// classifySocialInteraction 据 actor→target 四轴（[-10,10]）保守判定应触发的七种交互之一。
// 优先级（重后果优先，敌意先于善意）：开战 > 复仇 > 反目 ；联姻 > 结盟 > 结识 > 交易。
// 同一对若既有高竞争又有正向，按敌意处理。返回 ok=false 表示未跨任何阈值（不触发）。
//
// 三类新增自治触发（trade/marriage/war）的阈值都「夹」在既有 4 类之外（更深/更浅/更烈），
// 且按优先级排在对应既有类的相邻位，故**既有 4 类的判定结果逐对完全不变**——只是把原先落空的更深/更浅/更烈的对接住：
//   - 开战：rivalry≥8 且 fear≥7（烈于复仇 6/6），排复仇前；
//   - 联姻：affection≥8 且 trust≥6.5（深于结盟 affection 5），排结盟前；
//   - 交易：trust≥3 且 affection≥1.5 且 rivalry<3 且 fear<3（浅于结识 trust 4/affection 2.5、且无敌意），排结识后兜浅互利对。
func classifySocialInteraction(row relationPromptRow) (SevenInteraction, bool) {
	switch {
	// 开战：竞争与戒备都极高（敌意烈到势力级卷入，最重的敌意）——排在复仇前，只接更极端的对。
	case row.Rivalry >= socialRivalryWar && row.Fear >= socialFearWar:
		return InteractionWar, true
	// 复仇：高竞争 且 高戒备（对方既是对手又令我惧惮 → 先发制人的敌意）。
	case row.Rivalry >= socialRivalryHot && row.Fear >= socialFearHot:
		return InteractionVengeance, true
	// 反目：高竞争 或 戒备明显升高（关系已实质破裂，但未到复仇量级）。
	case row.Rivalry >= socialRivalryHot || row.Fear >= socialFearWary:
		return InteractionFallout, true
	// 联姻：极高好感 且 高信任（羁绊深到谈婚论嫁）——排在结盟前，只接更深的对。
	case row.Affection >= socialAffectionMarriage && row.Trust >= socialTrustMarriage:
		return InteractionMarriage, true
	// 结盟：高信任 且 高好感（羁绊已成 → 升级为正式同盟）。
	case row.Trust >= socialTrustAlly && row.Affection >= socialAffectionAlly:
		return InteractionAlliance, true
	// 结识：信任达熟人量级 且 有正向好感（初步善意成型）。
	case row.Trust >= socialTrustWarm && row.Affection >= socialAffectionWarm:
		return InteractionAcquaint, true
	// 交易：浅互利关系（正向信任+一丝好感、且无敌意）——排在结识后，只接更浅的互利对。
	case row.Trust >= socialTrustTrade && row.Affection >= socialAffectionTrade &&
		row.Rivalry < socialRivalryTradeMax && row.Fear < socialFearTradeMax:
		return InteractionTrade, true
	default:
		return "", false
	}
}

// socialImportanceFor 给自治社交交互一个保守的世界总线重要度（联姻/开战/复仇最重，结盟次之，反目/交易/结识更轻）。
func socialImportanceFor(interaction SevenInteraction) int {
	switch interaction {
	case InteractionMarriage, InteractionWar:
		return 6 // 不可逆/势力级——最重
	case InteractionAlliance, InteractionVengeance:
		return 5
	case InteractionFallout:
		return 4
	case InteractionTrade:
		return 3 // 一桩生意——轻于反目，重于结识
	default: // 结识
		return 2
	}
}

// crossReasonForInteraction 把一种自治社交交互映射到其专属跨玩家 reason-code（events.Catalog 已登记，见 reason_codes.go）。
// 仅用于本地审计/叙事的 reason 选取（落库的世界总线事实由 RecordSevenInteraction 经 worldbus.EventKind 记，
// 收件箱 reason 由 SurfaceFateEvent 决定）；此处给「确定性、专属码」让自治触发可被审计区分（trade/marriage/war 各有其码）。
func crossReasonForInteraction(interaction SevenInteraction) events.ReasonCode {
	switch interaction {
	case InteractionAcquaint:
		return events.ReasonCrossEncounter
	case InteractionAlliance:
		return events.ReasonCrossAlliance
	case InteractionTrade:
		return events.ReasonCrossTrade
	case InteractionMarriage:
		return events.ReasonCrossMarriage
	case InteractionFallout:
		return events.ReasonCrossDerived // 反目沿用「殃及/恶化」类码（无专属背叛码登记给自治侧）
	case InteractionVengeance:
		return events.ReasonCrossVendetta
	case InteractionWar:
		return events.ReasonCrossWarDraw
	default:
		return events.ReasonCrossDerived
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
