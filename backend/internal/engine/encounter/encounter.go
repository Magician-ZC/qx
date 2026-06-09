package encounter

// 文件说明：威胁遭遇的「结算」原语（设计文档 docs/PvE威胁系统.md 的确定性核心）。
// 把「贡献评分 / 战利品按贡献分配 / 失败惩罚分级闸 / 单人解锁」做成纯函数、可测试，
// 复用 engine/arbitration 做排他件的确定性、频率无关、付费不进 score 的裁决。
// 战利品分配是零和分赃——「史诗装该归谁」由此可仲裁、可复算，且付费买不到。

import (
	"math"
	"sort"

	"qunxiang/backend/internal/engine/arbitration"
)

// Contribution 是一名参与者在威胁事件中的投入分项（全部来自 combat_roll/status.Mutator 留痕，付费不产生）。
type Contribution struct {
	Damage float64 // 对威胁造成的伤害
	Tank   float64 // 承伤（吸引火力/掩护）
	Role   float64 // 治疗/救援/补给/鼓舞等非伤害扮演折算
	Risk   float64 // 到场风险溢价（弱者敢上加分、纯蹭场极小）
	Clutch float64 // 关键救场（残血终结/救濒死队友）
}

// ContributionWeights 贡献评分权重（设计文档默认值，[待测试/可调]）。
var ContributionWeights = struct{ Damage, Tank, Role, Risk, Clutch float64 }{
	Damage: 1.0,
	Tank:   0.8,
	Role:   0.6,
	Risk:   0.5,
	Clutch: 1.2,
}

// ContributionScore 把投入分项聚合为贡献分。确定性、可从事件流水复算、付费恒不进。
func ContributionScore(c Contribution) float64 {
	w := ContributionWeights
	return w.Damage*c.Damage + w.Tank*c.Tank + w.Role*c.Role + w.Risk*c.Risk + w.Clutch*c.Clutch
}

// --- 关键救场（Clutch）标记原语（设计文档 §5「关键救场」1.2 权重的数据来源）---
//
// 问题：Contribution.Clutch 字段在战斗循环里从不被填——它需要一个「事件→分值」的录入口，
// 让「濒死反杀 / 救援濒死队友 / 终结一击」这类关键时刻真正进 Score（而非永远为 0）。
// 这里提供纯函数 API：战斗循环（session/threat.go、dungeon_segment.go）在识别到对应事件时累加进 Clutch 分项。
// 全部确定性、与付费无关（事件由 combat_roll 确定性判定，钱包/计费绝不产生 Clutch）。

// ClutchKind 是一类关键救场事件（每类有固定折算分值，可叠加）。
type ClutchKind string

const (
	// ClutchFinalBlow 终结一击：亲手把威胁打到 HP≤0 的那一下（最靠近它心脏的人）。
	ClutchFinalBlow ClutchKind = "final_blow"
	// ClutchRescueDown 救援濒死队友：在队友倒下/濒死时把其拉回安全（治疗/掩护/拽离）。
	ClutchRescueDown ClutchKind = "rescue_down"
	// ClutchNearDeathReversal 濒死反杀：自身 HP 已跌破撤退线却仍站住并扭转战局（不退反进的孤勇）。
	ClutchNearDeathReversal ClutchKind = "near_death_reversal"
)

// ClutchValues 各类关键救场的折算分值（累加进 Contribution.Clutch，再乘 ContributionWeights.Clutch=1.2）。
// 这些是「事件计数→分项」的折算基准：一次终结一击≈1 个标准救场单位。设计默认值，可调。
var ClutchValues = struct{ FinalBlow, RescueDown, NearDeathReversal float64 }{
	FinalBlow:         1.0,
	RescueDown:        1.0,
	NearDeathReversal: 1.2, // 濒死还能反杀比常规救场更险，折算略高
}

// ClutchValue 返回某类关键救场的折算分值（未知类型返回 0，安全降级）。
func ClutchValue(kind ClutchKind) float64 {
	switch kind {
	case ClutchFinalBlow:
		return ClutchValues.FinalBlow
	case ClutchRescueDown:
		return ClutchValues.RescueDown
	case ClutchNearDeathReversal:
		return ClutchValues.NearDeathReversal
	default:
		return 0
	}
}

// MarkClutch 把一次关键救场事件累加进贡献分项的 Clutch 通道，返回更新后的 Contribution（纯函数、值进值出）。
// 战斗循环每识别到一次 final_blow / rescue_down / near_death_reversal 即调用一次。
// 多次救场自然叠加（一场仗里既终结一击又救了队友 → Clutch 累计），确定性、付费不进。
func MarkClutch(c Contribution, kind ClutchKind) Contribution {
	c.Clutch += ClutchValue(kind)
	return c
}

// HadClutch 判断该贡献是否含任何关键救场（供 session 侧判「是否触发传家物升级待决策卡」的门，见 §5 闭环）。
func HadClutch(c Contribution) bool {
	return c.Clutch > 0
}

// IsNearDeathReversal 判断「当前这一下是否构成濒死反杀」的纯判定：
// 自身归一化 HP 已低于撤退线（fleeFraction，如 0.25）却仍打出了有效输出（dealt>0）且未当场倒下（still alive）。
// 战斗循环在角色攻击命中后用本判定决定是否 MarkClutch(ClutchNearDeathReversal)。确定性、无随机。
func IsNearDeathReversal(selfHPFraction, fleeFraction float64, dealtDamage int, stillAlive bool) bool {
	return stillAlive && dealtDamage > 0 && selfHPFraction < fleeFraction
}

// IsFinalBlow 判断「这一击是否构成终结一击」：本次伤害把威胁血量从 >0 打到 ≤0。
// 战斗循环在扣完威胁 HP 后用本判定决定是否 MarkClutch(ClutchFinalBlow)。确定性、无随机。
func IsFinalBlow(threatHPBefore, threatHPAfter int) bool {
	return threatHPBefore > 0 && threatHPAfter <= 0
}

// Rarity 战利品稀有度。
type Rarity string

const (
	Epic   Rarity = "epic"   // 唯一排他大件
	Rare   Rarity = "rare"   // 少量 N 件
	Common Rarity = "common" // 可分割材料/货币
)

// LootItem 一项掉落。Common 的 Quantity 是可分割总量；Rare 的 Quantity 是件数；Epic 恒视作 1。
type LootItem struct {
	ID       string
	Rarity   Rarity
	Quantity int
}

// Participant 参与者及其贡献分。
type Participant struct {
	UnitID string
	Score  float64
}

// AwardReason 分配原因。
type AwardReason string

const (
	AwardWon         AwardReason = "won"         // 排他件胜者
	AwardShare       AwardReason = "share"       // 可分割材料按比例分得
	AwardConsolation AwardReason = "consolation" // 败者补偿（差一名 → 合成碎片）
)

// Award 一条分配结果。
type Award struct {
	ItemID   string
	UnitID   string
	Quantity int
	Reason   AwardReason
}

// AllocateLoot 按贡献分确定性分配战利品（设计文档「装备分配」）。
//   - 排他件(epic/rare)走 arbitration.Resolve：胜率∝Score、与频率/入队顺序无关、付费不进 Score；
//     未中签的入排名者各得一份「败者补偿」碎片（差一名≠零收获）。
//   - 可分割件(common)按 Score 比例确定性瓜分（floor + 余数按名次补，无浮点歧义）。
//   - minMeaningfulScore：贡献低于此值的「蹭场者」不进排他件排名（反白嫖），仍可分 common 材料。
//
// 同一 (key, items, participants) 必然得到同一结果，可复算验证（仲裁友好）。
func AllocateLoot(key string, items []LootItem, participants []Participant, minMeaningfulScore float64) []Award {
	meaningful := make([]Participant, 0, len(participants))
	for _, p := range participants {
		if p.Score > 0 && p.Score >= minMeaningfulScore {
			meaningful = append(meaningful, p)
		}
	}

	awards := make([]Award, 0, len(items))
	for _, item := range items {
		if item.Rarity == Common {
			split := SplitProportional(item.Quantity, participants)
			for _, p := range sortByScoreDescThenID(participants) {
				if q := split[p.UnitID]; q > 0 {
					awards = append(awards, Award{ItemID: item.ID, UnitID: p.UnitID, Quantity: q, Reason: AwardShare})
				}
			}
			continue
		}

		if len(meaningful) == 0 {
			continue
		}
		n := 1
		if item.Rarity == Rare && item.Quantity > 1 {
			n = item.Quantity
		}
		outcome := arbitration.Resolve(arbitration.Contest{Key: key + "|" + item.ID, Contestants: toContestants(meaningful)})
		won := make(map[string]bool, n)
		for i, uid := range outcome.Ranking {
			if i < n {
				awards = append(awards, Award{ItemID: item.ID, UnitID: uid, Quantity: 1, Reason: AwardWon})
				won[uid] = true
			}
		}
		for _, uid := range outcome.Ranking {
			if !won[uid] {
				awards = append(awards, Award{ItemID: item.ID, UnitID: uid, Quantity: 1, Reason: AwardConsolation})
			}
		}
	}
	return awards
}

// SplitProportional 把 total 份可分割资源按 Score 比例确定性瓜分。
// 用 floor 分配 + 余数按 (Score 降序, UnitID 升序) 顺序逐一补发，保证 Σ=total、无浮点丢失、可复现。
func SplitProportional(total int, participants []Participant) map[string]int {
	result := map[string]int{}
	if total <= 0 || len(participants) == 0 {
		return result
	}
	ordered := sortByScoreDescThenID(participants)

	sum := 0.0
	for _, p := range ordered {
		if p.Score > 0 {
			sum += p.Score
		}
	}

	if sum <= 0 {
		// 全零分：按字典序尽量均分。
		base := total / len(ordered)
		rem := total % len(ordered)
		for i, p := range ordered {
			result[p.UnitID] = base
			if i < rem {
				result[p.UnitID]++
			}
		}
		return result
	}

	assigned := 0
	for _, p := range ordered {
		score := p.Score
		if score < 0 {
			score = 0
		}
		share := int(math.Floor(float64(total) * score / sum))
		result[p.UnitID] = share
		assigned += share
	}
	// 余数（< 参与人数）按名次补 1。
	for i := 0; i < total-assigned && i < len(ordered); i++ {
		result[ordered[i].UnitID]++
	}
	return result
}

// PenaltyCap 返回某角色当前可承受的最重后果层（设计文档后果分级闸：牵挂×在世天数硬锁）。
//   - 层1 可恢复：始终可达。
//   - 层2 高代价：care≥40 或 在世≥3 天。
//   - 层3 不可逆：care≥70 且 在世≥7 天（三条 AND 中的两条，第三条「已发生≥1次层2」由调用方追加）。
func PenaltyCap(care float64, daysAlive int) int {
	if care >= 70 && daysAlive >= 7 {
		return 3
	}
	if care >= 40 || daysAlive >= 3 {
		return 2
	}
	return 1
}

// DegradePenalty 把候选惩罚层降级到该角色当前允许的最重层（绝不一刀切毁心血；返回 min(候选, cap)）。
func DegradePenalty(candidateLayer int, care float64, daysAlive int) int {
	cap := PenaltyCap(care, daysAlive)
	if candidateLayer < cap {
		return candidateLayer
	}
	return cap
}

// SoloAllowed 判断某威胁是否允许单人挑战（severity 超过 soloCap 时物理上要求组队）。
func SoloAllowed(severity float64, soloCap float64) bool {
	return severity <= soloCap
}

// --- 传家物升级判定（设计文档 §5「用某装备完成 Clutch/跨越关键命运节点 → 待决策卡刻成传家物」）---
//
// 当前缺口：IsLegacy 只在「角色阵亡时传承」路径被设；没有「角色在世时主动把陪她出生入死的装备升级为传家物」的入口。
// 本判定回答「这件正在使用的装备是否够资格触发『要不要刻成传家物』的待决策卡」——
// 真正落标记（IsLegacy/SoulBound）由 item.LegacyUpgrade 在玩家确认后执行（纯函数、本包不碰物品结构以免与 unit 包形成 import 环）。

// LegacyUpgradeTrigger 是触发传家物升级评估的关键时刻类别。
type LegacyUpgradeTrigger string

const (
	// TriggerClutch 用这件装备完成了一次关键救场（终结一击/救濒死队友/濒死反杀）。
	TriggerClutch LegacyUpgradeTrigger = "clutch"
	// TriggerFateNode 用这件装备跨越了一个关键命运节点（如挺过 Boss 决战/为羁绊之人挡下致命一击）。
	TriggerFateNode LegacyUpgradeTrigger = "fate_node"
)

// LegacyUpgradeQuery 是「是否该弹出刻为传家物的待决策卡」的判定入参（纯前因，无随机、无钱包）。
type LegacyUpgradeQuery struct {
	Trigger       LegacyUpgradeTrigger
	AlreadyLegacy bool // 已是传家物则无需再升级（避免重复弹卡）
	HasOwner      bool // 必须有在世持有者（无主装备不进升级流）
	BondTurns     int  // 该装备陪伴当前持有者的回合数（羁绊时长，越久越够格）
	ClutchCount   int  // 该装备累计完成的关键救场次数（Clutch 事件计数）
}

// MinLegacyBondTurns 是触发传家物升级待决策卡的最小羁绊回合数（防一捡到手就因偶然 clutch 弹卡）。
const MinLegacyBondTurns = 5

// QualifiesForLegacyUpgrade 判定是否应弹出「要不要把它刻成传家物」的待决策卡（确定性、纯前因）。
//   - 必须有在世持有者、尚非传家物；
//   - Clutch 触发：至少 1 次关键救场 + 羁绊≥MinLegacyBondTurns（陪你出生入死过、且不是萍水相逢）；
//   - 命运节点触发：跨越关键命运节点本身即够格（这类时刻天然稀有、有叙事重量），仍要求最小羁绊。
//
// 注意：本函数只决定「是否弹卡」；最终是否刻成传家物由玩家在待决策卡里确认，再由 item.LegacyUpgrade 落标记。
func QualifiesForLegacyUpgrade(q LegacyUpgradeQuery) bool {
	if q.AlreadyLegacy || !q.HasOwner {
		return false
	}
	if q.BondTurns < MinLegacyBondTurns {
		return false
	}
	switch q.Trigger {
	case TriggerClutch:
		return q.ClutchCount >= 1
	case TriggerFateNode:
		return true
	default:
		return false
	}
}

func toContestants(ps []Participant) []arbitration.Contestant {
	cs := make([]arbitration.Contestant, len(ps))
	for i, p := range ps {
		cs[i] = arbitration.Contestant{UnitID: p.UnitID, Score: p.Score}
	}
	return cs
}

func sortByScoreDescThenID(ps []Participant) []Participant {
	out := make([]Participant, len(ps))
	copy(out, ps)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].UnitID < out[j].UnitID
	})
	return out
}
