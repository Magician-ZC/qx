package arbitration

// 文件说明：零和竞争的确定性仲裁原语（设计方案 §11.3「仲裁 tick」的工程落地）。
// 把「先到先得 / 谁反应快」的零和结果，改为在统一节奏上、仅由参与者的数值竞争力(Score)
// 加确定性掷骰来裁决——与「行动频率 / 入队次数 / 谁先到」完全无关。这从机制上斩断了
// 「付费更高频 → 先到 → 赢」这条 P2W 链路：付费只能买更高的 Score(投入/技能/属性)，
// 买不到「保证赢」。胜负对参与者集合 + 各自 Score 是确定的，可复现、可审计、可仲裁。
//
// 算法：Efraimidis–Spirakis 加权随机（A-Res）。给每个参与者一个确定性键
// key_i = -ln(u_i) / score_i，u_i 来自 FNV(Key|UnitID) 的确定性均匀数。按 key 升序排名，
// 最小者胜。该方法保证「排第一的概率 = score_i / Σ score」，即胜率与竞争力成正比、
// 且对入队顺序与重复入队不变。

import (
	"hash/fnv"
	"math"
	"sort"
)

// minScore 防止 Score 为 0/负导致除零；零分参与者之间仍按 u_i 确定性排序。
const minScore = 1e-9

// Contestant 是一名竞争者。Score 由数值属性（出价/技能/距离/投入等）算出，与 tick 频率无关。
type Contestant struct {
	UnitID string  `json:"unit_id"`
	Score  float64 `json:"score"`
}

// Contest 是一次零和竞争。Key 是该竞争事件的确定性标识（应包含 region+tick，保证可复现）。
type Contest struct {
	Key         string       `json:"key"`
	Resource    string       `json:"resource,omitempty"`
	Contestants []Contestant `json:"contestants"`
}

// Outcome 是仲裁结果：胜者 + 全序排名（便于次名补偿/瓜分）。
type Outcome struct {
	WinnerID string   `json:"winner_id"`
	Ranking  []string `json:"ranking"`
}

// Resolve 对一次零和竞争做确定性裁决。
// 性质：① 确定性(同 Contest → 同 Outcome)；② 与入队顺序无关；③ 与重复入队(频率)无关；
// ④ 胜率与 Score 成正比。
func Resolve(c Contest) Outcome {
	cs := dedupMaxScore(c.Contestants)
	if len(cs) == 0 {
		return Outcome{}
	}

	type keyed struct {
		id  string
		key float64
	}
	ranked := make([]keyed, 0, len(cs))
	for _, x := range cs {
		score := x.Score
		if score < minScore {
			score = minScore
		}
		u := uniform(c.Key, x.UnitID)
		// key = -ln(u)/score；score 越大 → key 越小 → 越靠前。
		ranked = append(ranked, keyed{id: x.UnitID, key: -math.Log(u) / score})
	}
	// 按 key 升序；key 相等时按 UnitID 兜底，确保全序确定。
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].key != ranked[j].key {
			return ranked[i].key < ranked[j].key
		}
		return ranked[i].id < ranked[j].id
	})

	ids := make([]string, len(ranked))
	for i, r := range ranked {
		ids[i] = r.id
	}
	return Outcome{WinnerID: ids[0], Ranking: ids}
}

// dedupMaxScore 按 UnitID 去重，保留最高 Score；与输入顺序无关。
// 这是「频率无关」的关键：同一参与者无论入队多少次，只算一次。
func dedupMaxScore(in []Contestant) []Contestant {
	best := make(map[string]float64, len(in))
	for _, c := range in {
		if c.UnitID == "" {
			continue
		}
		if s, ok := best[c.UnitID]; !ok || c.Score > s {
			best[c.UnitID] = c.Score
		}
	}
	out := make([]Contestant, 0, len(best))
	for id, score := range best {
		out = append(out, Contestant{UnitID: id, Score: score})
	}
	// 规范化顺序，消除 map 遍历的不确定性。
	sort.Slice(out, func(i, j int) bool { return out[i].UnitID < out[j].UnitID })
	return out
}

// uniform 由 FNV(UnitID|Key) 生成 (0,1) 区间的确定性均匀随机数。
// 关键：FNV-1a 对相邻输入(如 unitID 仅差一字节 a/b)雪崩很弱，会让不同参与者抽到几乎相同的 u，
// 从而让高 Score 一方近乎必胜——破坏「胜率与 Score 成正比」。因此把 UnitID 放在前面先扩散，
// 再用 splitmix64 finalizer 强化雪崩，确保即便输入仅差一字节，输出也彼此独立、均匀分布。
func uniform(key string, unitID string) float64 {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(unitID))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(key))
	x := mix64(hasher.Sum64())
	// 取高 53 位映射到 (0,1)：分子 +1、分母 +2，避免取到 0 或 1。
	const denom = 1 << 53
	return (float64(x>>11) + 1) / (float64(denom) + 2)
}

// mix64 是 splitmix64 的 finalizer，对 64 位整数做强雪崩混合。
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}
