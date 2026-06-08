package session

// 文件说明：跨玩家/会话内「排他标的」的零和裁决（设计 docs/事件耦合与跨玩家关联.md §2.6）。
// 同一排他标的——同一联姻对象 / 势力继承席位 / 同批排他战利品——若被多人同时争夺，旧逻辑无统一裁决窗口，
// 谁先到/谁反应快/谁动作频率高谁就赢（P2W 隐患）。本文件把这类争夺收敛到「部署边界的统一 tick」：
// 在同一节奏上、仅由各争夺者的**实力/贡献 Score**（付费不进 Score——反 P2W 基石）经 engine/arbitration.Resolve
// 做**确定性**裁决（胜率∝Score、与入队顺序/动作频率无关、同 Key 同结果可复现）。胜者得标的，败者走
// 「退而求其次」补偿（best-effort 记一条记忆「这次没争过，但…」，绝不阻断推进）。
//
// 纪律对齐 auto_match.go / social_scan.go：flag-gated（QUNXIANG_ZEROSUM_CONTEST **默认开**，仅 false/0/no/off 关）、
// 低频确定性触发（纯本会话单位、按四轴信号近似争夺意图）、best-effort（吞错绝不中断阶段推进）。
// 当前覆盖最可判定的一类排他标的：**联姻冲突**（多个单身单位本回合都想与同一单身对象确认亲密关系）。
// 席位继承 / 排他战利品复用同一 ResolveExclusiveContest 原语（调用方按情境算 Score 即可），后续接入。

import (
	"context"
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
	// contestMaxResolutionsPerScan 单次扫描最多实际裁决的排他标的数——硬顶每回合裁决音量，避免一回合刷屏。
	contestMaxResolutionsPerScan = 2
	// contestMarriageMinContenders 触发联姻裁决所需的最少同对象求亲者数（<2 无冲突，无需零和裁决）。
	contestMarriageMinContenders = 2

	// 联姻求亲意图的关系信号阈值（量纲 [-10,10]，取与 social_scan 同源的保守值，确保只在好感明确成型时算「想求亲」）。
	contestMarriageTrustMin     = 4.0 // 想求亲：actor→target 信任达「熟人」量级
	contestMarriageAffectionMin = 5.0 // 想求亲：actor→target 有较强好感（高于普通结识门，求亲是重决策）
)

// ContestContender 是一名排他标的争夺者。
// Score 由其**实力/贡献**算出（属性/士气/关系牵引等），**绝不含付费维度**（钱包/付费档/SKU）——这是反 P2W 的口径保证：
// 付费只能买更高的真实投入，买不到「保证赢」。Detail 是可选的人类可读争夺凭据（用于补偿文案，非裁决输入）。
type ContestContender struct {
	UnitID string
	Score  float64
	Detail string // 例：「她对老吴的好感」——用于败者「退而求其次」的记忆文案，不参与裁决
}

// zeroSumContestEnabled 读 QUNXIANG_ZEROSUM_CONTEST，**默认开**：未设/非法值视为开，仅 false/0/no/off 显式关。
// 默认开理由：排他标的的确定性零和裁决是反 P2W 的机制基石（设计 §2.6），且全程 best-effort + 低频 +
// 仅在本会话单位间、付费不进 Score，行为受控、无破坏性副作用（胜者本就会成立的关系成立、败者只多一条记忆）。
func zeroSumContestEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_ZEROSUM_CONTEST"))) {
	case "false", "0", "no", "off":
		return false
	default:
		return true // 未设/非法/其余值 → 开
	}
}

// ResolveExclusiveContest 对一个排他标的做确定性零和裁决：把多个争夺者经 arbitration.Resolve（胜率∝Score、
// 与入队顺序/动作频率无关、付费不进 Score）确定性择一胜者；Key 含 sessionID+turn+resource 保可复现。
// 胜者得标的；败者走「退而求其次」补偿（best-effort 记一条记忆「这次没争过，但…」，失败只吞错）。
// 返回胜者 UnitID。守卫：nil 依赖 / 争夺者 <1 → 返回空串 + err（无可裁决）。单争夺者直接判其胜（无冲突仍可幂等调用）。
// 本函数**只裁决与留痕**，不落地标的归属（联姻成立等副作用由调用方按情境处理）——保持原语通用、可被席位/战利品复用。
func (service *Service) ResolveExclusiveContest(
	ctx context.Context,
	state *State,
	contestKey string,
	resource string,
	contenders []ContestContender,
) (string, error) {
	if service == nil {
		return "", fmt.Errorf("resolve contest: missing service")
	}
	// 去空 UnitID；保留输入顺序无关（arbitration.Resolve 内部 dedupMaxScore 已规范化顺序、与频率无关）。
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
	detailByID := make(map[string]string, len(valid))
	for _, c := range valid {
		contestants = append(contestants, arbitration.Contestant{UnitID: c.UnitID, Score: c.Score})
		detailByID[c.UnitID] = strings.TrimSpace(c.Detail)
	}

	// Key 含 sessionID+turn+resource，保「同一会话、同一回合、同一标的」的裁决可复现（与 arbitration 约定一致）。
	sessionID := ""
	turn := 0
	if state != nil {
		sessionID = state.ID
		turn = state.TurnState.Turn
	}
	key := exclusiveContestKey(sessionID, turn, contestKey)
	out := arbitration.Resolve(arbitration.Contest{Key: key, Resource: resource, Contestants: contestants})
	winnerID := out.WinnerID
	if winnerID == "" {
		return "", fmt.Errorf("resolve contest %q: arbitration returned no winner", resource)
	}

	// 败者「退而求其次」补偿：best-effort 给每个非胜者记一条「这次没争过，但…」记忆，绝不阻断。
	for _, c := range valid {
		if c.UnitID == winnerID {
			continue
		}
		service.recordContestConsolation(ctx, state, c.UnitID, resource, detailByID[c.UnitID])
	}
	return winnerID, nil
}

// recordContestConsolation 给一名争夺失败者记一条「退而求其次」的命运补偿（best-effort，绝不阻断）。
// 优先记一条单位记忆（让 AI 后续决策能引用「我这次没争过」），并在有 state 时追加一条可读日志。
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

// exclusiveContestKey 由 (sessionID, turn, resource) 派生确定性裁决 Key（与 arbitration「Key 须含 region+tick 可复现」约定对齐：
// 本会话内 sessionID 充当 region 域、turn 充当 tick）。纯字符串拼接，无哈希、无全局 rand，便于测试断言「同 Key 同结果」。
func exclusiveContestKey(sessionID string, turn int, resource string) string {
	return "contest|" + sessionID + "|t" + strconv.Itoa(turn) + "|" + strings.TrimSpace(resource)
}

// scanExclusiveContestsAtBoundary 在部署边界扫描本会话内对同一排他标的的竞争，并用 ResolveExclusiveContest 确定性裁决。
// 当前覆盖**联姻冲突**：多个单身单位本回合都「想与同一单身对象确认亲密关系」（从 romance/relation 信号近似）。
// 无冲突（每个对象至多一个求亲者）→ no-op，不写任何东西。
// 守卫：nil 依赖 / flag 关 / 候选不足 / 未到周期 → no-op。全程 best-effort：吞错绝不影响阶段推进。
//
// 注意「裁决」语义：胜者获得「本回合优先与该对象推进联姻」的资格（实际成立仍走既有 romance 双方同意路径，不强制成婚）；
// 败者获得「退而求其次」补偿记忆。这把「谁先到谁赢」改为「谁的真实投入(Score)更高更可能赢」（频率/付费无关）。
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

	// 候选池：本局玩家阵营、存活、可参与（已有恋人者不再求亲）。按确定性顺序截断控算量。
	pool := make([]unit.Record, 0, len(units))
	for i := range units {
		u := units[i]
		if state.PlayerFactionID != "" && u.FactionID != state.PlayerFactionID {
			continue
		}
		if !isBattleReady(u) {
			continue
		}
		if u.Status.LifeState == unit.LifeStateDead || u.Status.LivesRemaining <= 0 {
			continue
		}
		pool = append(pool, u)
		if len(pool) >= contestScanMaxUnits {
			break
		}
	}
	if len(pool) < contestMarriageMinContenders+1 { // 至少 2 个求亲者 + 1 个对象才可能成冲突
		return
	}

	// 「单身集合」：本回合可作为求亲对象 / 求亲者的单位（无现存恋人）。
	single := make(map[string]bool, len(pool))
	for i := range pool {
		if strings.TrimSpace(pool[i].Social.LoverUnitID) == "" {
			single[pool[i].ID] = true
		}
	}

	// 聚合「想求亲同一对象」的争夺者：targetID -> []ContestContender。
	// 求亲意图近似：求亲者单身、对象单身、求亲者对对象的四轴跨「想求亲」阈值（信任达熟人量级 且 较强好感）。
	contendersByTarget := make(map[string][]ContestContender)
	targetName := make(map[string]string)
	for i := range pool {
		actor := pool[i]
		if !single[actor.ID] {
			continue // 已有恋人 → 不发起求亲
		}
		relations := service.loadOutgoingRelationMap(ctx, actor.ID)
		if len(relations) == 0 {
			continue
		}
		for j := range pool {
			if i == j {
				continue
			}
			target := pool[j]
			if !single[target.ID] {
				continue // 对象已有恋人 → 非排他可争（标的已被占）
			}
			row, ok := relations[target.ID]
			if !ok {
				continue
			}
			if !marriageContenderWants(row) {
				continue
			}
			// Score=对该对象的「真实投入/牵引」（关系亲和 + 自身实力/士气），**付费不进**（不读 Wallet/付费档）。
			score := marriageContenderScore(actor, row)
			contendersByTarget[target.ID] = append(contendersByTarget[target.ID], ContestContender{
				UnitID: actor.ID,
				Score:  score,
				Detail: fmt.Sprintf("我对 %s 的心意", target.DisplayName()),
			})
			targetName[target.ID] = target.DisplayName()
		}
	}
	if len(contendersByTarget) == 0 {
		return
	}

	// 确定性遍历目标（map 顺序不确定 → 按 targetID 排序），仅对「≥2 争夺者」的标的裁决（无冲突 no-op）。
	targets := make([]string, 0, len(contendersByTarget))
	for tid := range contendersByTarget {
		targets = append(targets, tid)
	}
	sort.Strings(targets)

	resolved := 0
	for _, tid := range targets {
		if resolved >= contestMaxResolutionsPerScan {
			break // 每回合裁决上限：硬顶音量
		}
		cs := contendersByTarget[tid]
		if len(cs) < contestMarriageMinContenders {
			continue // 该对象至多一个求亲者 → 无排他冲突，no-op
		}
		resource := "marriage:" + tid
		winnerID, err := service.ResolveExclusiveContest(ctx, state, resource, resource, cs)
		if err != nil || winnerID == "" {
			continue // best-effort：裁决失败只吞错
		}
		// 胜者获「本回合优先推进与该对象联姻」的资格——留痕一条可读日志（实际成立仍走 romance 双方同意路径）。
		appendLog(
			state,
			"contest_marriage",
			fmt.Sprintf("围绕 %s 的求亲，%s 这一轮赢得了优先。", displayNameOf(targetName, tid), contenderDisplayName(units, winnerID)),
			winnerID,
			tid,
		)
		// 世界总线留痕（流程事件，非状态变更）：仅 WorldID 非空时；best-effort，绝不阻断。
		if state.WorldID != "" {
			_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
				SessionID:   state.ID,
				OwnerUnitID: winnerID,
				Code:        events.ReasonSocialObjectBind,
				Category:    events.CategoryFate,
				Payload:     map[string]any{"resource": resource, "winner": winnerID, "target": tid, "contenders": len(cs)},
				WorldID:     state.WorldID,
			})
		}
		resolved++
	}
}

// marriageContenderWants 据 actor→target 四轴判定 actor 本回合是否「想与 target 求亲」（保守阈值，宁缺毋滥）。
func marriageContenderWants(row relationPromptRow) bool {
	return row.Trust >= contestMarriageTrustMin && row.Affection >= contestMarriageAffectionMin
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

// arbitrationMinContestScore 是争夺 Score 的下限正值（避免全 0 致 arbitration 退化为纯 u_i 排序时语义不清）。
const arbitrationMinContestScore = 0.01

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
