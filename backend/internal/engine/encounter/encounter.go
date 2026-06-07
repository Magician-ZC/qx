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
