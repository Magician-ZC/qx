package faction

// 文件说明：三阵营开放世界的阵营定义与数值道德轴（MoralAlignment）纯逻辑核心（阵营开放世界 F1 地基）。
//
// 设计：把旧「20 人私人村庄网 + 固定 NPC 敌方」改为「三阵营开放世界」。三阵营各有道德基准：
//   - freedom 自由：各凭本心、最恨强加于人。
//   - order  秩序：律法至上、以规矩束众。
//   - chaos  混乱：破而后立、视秩序为枷锁。
//
// 数值道德轴 MoralAlignment 是 3 维亲和度 {Freedom, Order, Chaos}，各 [0,100]，是 F2 阵营切换的输入、
// 左右自治决策的偏置（仿 unit.Ambition 的非保护字段——直接读写、不走 StatusMutator）。
//
// 本包是纯逻辑（无 DB、无 LLM），全部确定性（出生据点用 FNV 哈希据 seed 落点），可独立单测。

import (
	"hash/fnv"
	"math"
	"strings"
)

// 阵营 ID 常量（freedom/order/chaos）——全代码库以这三个稳定字符串标识阵营。
const (
	IDFreedom = "freedom" // 自由
	IDOrder   = "order"   // 秩序
	IDChaos   = "chaos"   // 混乱
)

// MoralAlignment 是单位的 3 维数值道德轴，各分量 [0,100]：刻画其对三种道德取向的亲和度。
// 是 F2 阵营切换的输入、左右自治决策的偏置。仿 unit.Ambition 的非保护字段语义（直接读写、不走 Mutator）。
// 全 omitempty——旧存档无此字段反序列化为零值（全 0=无明显道德倾向），向后兼容。
type MoralAlignment struct {
	Freedom float64 `json:"freedom,omitempty"`
	Order   float64 `json:"order,omitempty"`
	Chaos   float64 `json:"chaos,omitempty"`
}

// IsZero 判定道德轴是否为全零（既有单位/旧存档默认态，DominantFaction 对其返回空阵营）。
func (m MoralAlignment) IsZero() bool {
	return m.Freedom == 0 && m.Order == 0 && m.Chaos == 0
}

// Clamped 把三维分量各夹到 [0,100]，返回夹钳后的新值（写入前规范化用，确保恒在合法域）。
func (m MoralAlignment) Clamped() MoralAlignment {
	return MoralAlignment{
		Freedom: clamp01to100(m.Freedom),
		Order:   clamp01to100(m.Order),
		Chaos:   clamp01to100(m.Chaos),
	}
}

func clamp01to100(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// Definition 是一个阵营的完整定义：标识、中文名、道德基准、出生据点集合、道德信条。
type Definition struct {
	ID          string         // 阵营 ID（freedom/order/chaos）
	NameZH      string         // 中文名（自由/秩序/混乱）
	Baseline    MoralAlignment // 道德基准（该阵营角色/NPC 降生初值）
	SpawnPoints []string       // 出生据点 region ID 集合（每阵营 3 个，角色据 seed 确定性落其一）
	MoralCreed  string         // 道德信条（供 NPC/角色 prompt 用，定调该阵营行事底色）
}

// definitions 是三阵营的静态定义表（确定性、不可变）。
var definitions = []Definition{
	{
		ID:       IDFreedom,
		NameZH:   "自由",
		Baseline: MoralAlignment{Freedom: 70, Order: 15, Chaos: 15},
		// 出生据点：自由阵营的三处边野栖居地。
		SpawnPoints: []string{"liberty_reach", "wanderers_rest", "open_steppe"},
		MoralCreed:  "不受束缚、各凭本心、最恨强加于人。",
	},
	{
		ID:       IDOrder,
		NameZH:   "秩序",
		Baseline: MoralAlignment{Order: 70, Freedom: 15, Chaos: 15},
		// 出生据点：秩序阵营的三座律法重镇。
		SpawnPoints: []string{"lawkeep_citadel", "order_hold", "iron_assembly"},
		MoralCreed:  "律法至上、各守其分、以规矩护众生。",
	},
	{
		ID:       IDChaos,
		NameZH:   "混乱",
		Baseline: MoralAlignment{Chaos: 70, Freedom: 15, Order: 15},
		// 出生据点：混乱阵营的三处废墟集市。
		SpawnPoints: []string{"ash_warrens", "riot_market", "broken_span"},
		MoralCreed:  "破而后立、视秩序为枷锁、于乱中夺生机。",
	},
}

// All 返回三阵营定义的只读副本切片（顺序稳定：freedom/order/chaos）。
func All() []Definition {
	out := make([]Definition, len(definitions))
	copy(out, definitions)
	return out
}

// Get 按阵营 ID 取定义；归一化（去空白/小写）后查表，未知阵营返回 (零值, false)。
func Get(id string) (Definition, bool) {
	norm := Normalize(id)
	for _, def := range definitions {
		if def.ID == norm {
			return def, true
		}
	}
	return Definition{}, false
}

// Normalize 把阵营 ID 归一化为三个稳定常量之一（去空白、转小写、容中文别名）；无法识别返回空串。
// 容错：接受「自由/秩序/混乱」中文别名与英文 ID，便于入口启发式选阵营。
func Normalize(id string) string {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case IDFreedom, "自由":
		return IDFreedom
	case IDOrder, "秩序":
		return IDOrder
	case IDChaos, "混乱":
		return IDChaos
	default:
		return ""
	}
}

// IsValid 判定阵营 ID 是否为三阵营之一。
func IsValid(id string) bool {
	return Normalize(id) != ""
}

// BaselineFor 返回某阵营的道德基准；未知阵营返回零值道德轴。
func BaselineFor(id string) MoralAlignment {
	if def, ok := Get(id); ok {
		return def.Baseline
	}
	return MoralAlignment{}
}

// DominantFaction 据数值道德轴用 argmax 取主导阵营 ID（哪一维最高即归该阵营）。
// 全零（无明显倾向）返回空串；平手按 freedom>order>chaos 的稳定顺序裁定（确定性，不依赖随机）。
func DominantFaction(m MoralAlignment) string {
	if m.IsZero() {
		return ""
	}
	dominant := IDFreedom
	best := m.Freedom
	if m.Order > best {
		best, dominant = m.Order, IDOrder
	}
	if m.Chaos > best {
		dominant = IDChaos
	}
	return dominant
}

// PickSpawnPoint 据 (faction, seed) 在该阵营的出生据点集合里确定性选一个 region ID。
// 用 FNV-64a 哈希（项目约定的确定性随机口径），同一 (faction, seed) 永远落同一据点。
// 未知阵营或无据点返回空串（调用方应回退处理）。
func PickSpawnPoint(faction string, seed int64) string {
	def, ok := Get(faction)
	if !ok || len(def.SpawnPoints) == 0 {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(faction))
	_, _ = h.Write([]byte("|spawn|"))
	var buf [8]byte
	u := uint64(seed)
	for i := 0; i < 8; i++ {
		buf[i] = byte(u >> (8 * i))
	}
	_, _ = h.Write(buf[:])
	idx := int(h.Sum64() % uint64(len(def.SpawnPoints)))
	return def.SpawnPoints[idx]
}

// FactionForSpawnPoint 反查某出生据点 region 属于哪个阵营（供据点判属/NPC 播种用）；未知据点返回空串。
func FactionForSpawnPoint(regionID string) string {
	norm := strings.TrimSpace(regionID)
	for _, def := range definitions {
		for _, sp := range def.SpawnPoints {
			if sp == norm {
				return def.ID
			}
		}
	}
	return ""
}

// PerturbBaseline 据 (faction, seed, salt) 在基准道德轴上叠一个确定性小扰动（±maxDelta 内），
// 让同阵营 NPC 道德轴≈baseline 但不全相同（有个体差异、可复现）。结果各维夹回 [0,100]。
// maxDelta<=0 时直接返回夹钳后的基准（无扰动）。
func PerturbBaseline(faction string, seed int64, salt string, maxDelta float64) MoralAlignment {
	base := BaselineFor(faction)
	if maxDelta <= 0 {
		return base.Clamped()
	}
	jitter := func(tag string) float64 {
		h := fnv.New64a()
		_, _ = h.Write([]byte(faction + "|" + salt + "|" + tag))
		var buf [8]byte
		u := uint64(seed)
		for i := 0; i < 8; i++ {
			buf[i] = byte(u >> (8 * i))
		}
		_, _ = h.Write(buf[:])
		// 映射 [0,1) → [-maxDelta, +maxDelta)
		frac := float64(h.Sum64()%1000) / 1000.0
		return (frac - 0.5) * 2 * maxDelta
	}
	out := MoralAlignment{
		Freedom: base.Freedom + jitter("f"),
		Order:   base.Order + jitter("o"),
		Chaos:   base.Chaos + jitter("c"),
	}
	return out.Clamped()
}

// AlignmentDistance 返回两个道德轴的欧氏距离（供 F2 阵营切换/漂移判定参考），恒非负。
func AlignmentDistance(a, b MoralAlignment) float64 {
	df := a.Freedom - b.Freedom
	do := a.Order - b.Order
	dc := a.Chaos - b.Chaos
	return math.Sqrt(df*df + do*do + dc*dc)
}
