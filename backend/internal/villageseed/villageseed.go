// Package villageseed 确定性地生成「出生即 20 人关系网」（设计宪法 §4.5）。
//
// 这是「角色命运开盒」的根：一个角色一出生，身边就有 20 个有名有姓、有秘密、有人生目标、
// 与她有亲疏恩怨的人——「她想要什么 / 她牵挂谁」才有了真实的着落，而不是空壳。
//
// 全程确定性：同一 (worldID, seed) 永远生成同一个村庄（用项目约定的 FNV 哈希取材，不用全局随机），
// 因此可复现、可在任意分片重算、可断线重建。本包是纯逻辑、零 DB / 零 LLM；持久化由 session 层负责。
package villageseed

import (
	"fmt"
	"hash/fnv"
)

// VillageSize 是出生关系网的人数（设计宪法 §4.5：出生即 20 人）。
const VillageSize = 20

// Traits 与 unit.Personality 的 8 轴对齐（[0.05,0.95]），便于持久化时直接映射。
type Traits struct {
	Courage     float64
	Loyalty     float64
	Aggression  float64
	Prudence    float64
	Sociability float64
	Integrity   float64
	Stability   float64
	Ambition    float64
}

// Member 是村庄里的一个人。
type Member struct {
	Index      int
	Name       string
	Gender     string // 男 / 女
	Age        int
	Archetype  string // 出身（猎户/铁匠之女/落魄书生…）
	Traits     Traits
	SeedMemory string // 定调的种子记忆（一句，进她的记忆库）
	Secret     string // 一个不愿示人的秘密
	LifeGoal   string // 她/他真正想要的（欲望）
}

// Bond 是两人之间的一条有向关系（四轴 [-10,10]）。
type Bond struct {
	From      int
	To        int
	Kind      string
	Trust     float64
	Fear      float64
	Affection float64
	Rivalry   float64
}

// Village 是一整张出生关系网。
type Village struct {
	WorldID string
	Seed    int64
	Members []Member
	Bonds   []Bond
}

// ---- 确定性取材 ----

func h64(worldID string, seed int64, salt string) uint64 {
	hh := fnv.New64a()
	_, _ = hh.Write([]byte(fmt.Sprintf("%s|%d|%s", worldID, seed, salt)))
	v := hh.Sum64()
	// splitmix64 收尾，弱化 1 字节差异的雪崩不足（与 arbitration 同款做法）。
	v ^= v >> 30
	v *= 0xbf58476d1ce4e5b9
	v ^= v >> 27
	v *= 0x94d049bb133111eb
	v ^= v >> 31
	return v
}

func pick[T any](pool []T, h uint64) T { return pool[h%uint64(len(pool))] }

func frac(h uint64) float64 { return float64(h%10000) / 10000.0 }

func clamp95(v float64) float64 {
	if v < 0.05 {
		return 0.05
	}
	if v > 0.95 {
		return 0.95
	}
	return v
}

func clamp10(v float64) float64 {
	if v < -10 {
		return -10
	}
	if v > 10 {
		return 10
	}
	return v
}

// ---- 内容池（中文，足够 20 人有辨识度；密度优先而非堆量）----

var surnames = []string{"林", "周", "赵", "沈", "陆", "苏", "顾", "谢", "孟", "韩", "白", "宋", "江", "裴", "温", "卫"}
var givenF = []string{"采薇", "知夏", "晚晴", "听雪", "怜婉", "若离", "云栖", "岚光", "明月", "青禾"}
var givenM = []string{"砚舟", "无咎", "怀瑾", "长歌", "知行", "守拙", "决明", "野", "停云", "故渊"}

var archetypes = []struct {
	Name string
	Lean Traits
}{
	{"边境猎户", Traits{Courage: 0.78, Aggression: 0.55, Prudence: 0.62, Stability: 0.6, Sociability: 0.35, Loyalty: 0.6, Integrity: 0.6, Ambition: 0.45}},
	{"铁匠之女", Traits{Courage: 0.6, Aggression: 0.45, Prudence: 0.55, Stability: 0.72, Sociability: 0.5, Loyalty: 0.7, Integrity: 0.7, Ambition: 0.4}},
	{"落魄书生", Traits{Courage: 0.35, Aggression: 0.3, Prudence: 0.7, Stability: 0.45, Sociability: 0.55, Loyalty: 0.5, Integrity: 0.75, Ambition: 0.65}},
	{"行脚商人", Traits{Courage: 0.5, Aggression: 0.4, Prudence: 0.75, Stability: 0.55, Sociability: 0.8, Loyalty: 0.45, Integrity: 0.4, Ambition: 0.7}},
	{"庙祝巫医", Traits{Courage: 0.45, Aggression: 0.25, Prudence: 0.68, Stability: 0.66, Sociability: 0.6, Loyalty: 0.6, Integrity: 0.72, Ambition: 0.4}},
	{"流亡贵族", Traits{Courage: 0.55, Aggression: 0.5, Prudence: 0.6, Stability: 0.4, Sociability: 0.65, Loyalty: 0.45, Integrity: 0.5, Ambition: 0.85}},
	{"逃兵游勇", Traits{Courage: 0.5, Aggression: 0.7, Prudence: 0.5, Stability: 0.35, Sociability: 0.4, Loyalty: 0.35, Integrity: 0.4, Ambition: 0.5}},
	{"采药孤女", Traits{Courage: 0.58, Aggression: 0.3, Prudence: 0.72, Stability: 0.6, Sociability: 0.4, Loyalty: 0.65, Integrity: 0.7, Ambition: 0.45}},
}

var lifeGoals = []string{
	"替惨死的父母讨回一个公道",
	"挣够钱，把流落在外的妹妹找回来",
	"在乱世里护住身边每一个人，一个都不能少",
	"读完那本没读完的书，考取功名光耀门楣",
	"找到传说中那味能治她娘顽疾的草药",
	"亲手再打一把不会断的刀",
	"离开这座困住她半生的村子，去看看海",
	"洗清当年背在身上的那桩冤屈",
	"等一个人回来，哪怕只是确认他还活着",
	"攒下一片自己的田，再不看人脸色",
	"把师父没传完的手艺学全",
	"在族谱上留下不被人耻笑的一笔",
}

var secrets = []string{
	"其实是从邻县逃出来的通缉犯，改了名姓",
	"暗暗喜欢着村里某个早已订亲的人",
	"藏着一小袋金子，谁也没告诉",
	"亲手放走过本该处死的俘虏，至今没人知道",
	"识字，却装作目不识丁",
	"梦里总梦见一场没能救下的火，惊醒一身冷汗",
	"身上有一道不愿被人看见的旧伤疤",
	"曾经为了活命，出卖过一个信任她的人",
	"其实不是这家的亲生孩子",
	"偷偷给山里的逃户送过粮",
}

var seedMemories = []string{
	"那年大旱，她跟着大人去远处背水，回来时村口的老槐树下少了三个人。",
	"小时候她把唯一的半块饼分给了一只快饿死的狗，被娘骂了，却记了一辈子。",
	"她见过最亮的一次篝火，是父亲战死那夜，全村为他守灵点起来的。",
	"她第一次握刀，是为了护住一个比她还小的孩子，手抖得厉害。",
	"她记得师父咽气前最后那句没说完的话，至今猜不出后半截。",
	"那个冬天太冷，她和青梅竹马挤在一床破被里，约好长大谁也不许先走。",
	"她偷偷看过一次县城的灯火，从此心里就再装不下这座小村。",
	"她替人顶过一次罪，挨的那顿打，让她记住了什么叫人心。",
}

var bondKinds = []struct {
	Kind                            string
	Trust, Fear, Affection, Rivalry float64
	Mutual                          bool
}{
	{"青梅竹马", 7, -1, 8, -2, true},
	{"血亲手足", 8, 0, 9, 0, true},
	{"生死之交", 9, 0, 7, -1, true},
	{"恩师", 6, 1, 5, 1, false},
	{"宿敌", -6, 3, -4, 8, true},
	{"暗恋", 2, 2, 9, 0, false},
	{"债主", -2, 5, -1, 4, false},
	{"猜忌", -4, 4, -2, 5, true},
}

// Generate 确定性生成一张 20 人出生关系网。同一 (worldID, seed) 永远得到同一结果。
func Generate(worldID string, seed int64) Village {
	v := Village{WorldID: worldID, Seed: seed, Members: make([]Member, 0, VillageSize)}

	for i := 0; i < VillageSize; i++ {
		s := func(tag string) uint64 { return h64(worldID, seed, fmt.Sprintf("m%d:%s", i, tag)) }
		gender := "女"
		if s("g")%2 == 0 {
			gender = "男"
		}
		given := givenF
		if gender == "男" {
			given = givenM
		}
		arch := pick(archetypes, s("arch"))
		lean := arch.Lean
		trait := func(base float64, tag string) float64 {
			return clamp95(base + (frac(s(tag))-0.5)*0.4)
		}
		v.Members = append(v.Members, Member{
			Index:     i,
			Name:      pick(surnames, s("sur")) + pick(given, s("giv")),
			Gender:    gender,
			Age:       16 + int(s("age")%35),
			Archetype: arch.Name,
			Traits: Traits{
				Courage:     trait(lean.Courage, "t_cou"),
				Loyalty:     trait(lean.Loyalty, "t_loy"),
				Aggression:  trait(lean.Aggression, "t_agg"),
				Prudence:    trait(lean.Prudence, "t_pru"),
				Sociability: trait(lean.Sociability, "t_soc"),
				Integrity:   trait(lean.Integrity, "t_int"),
				Stability:   trait(lean.Stability, "t_sta"),
				Ambition:    trait(lean.Ambition, "t_amb"),
			},
			SeedMemory: pick(seedMemories, s("mem")),
			Secret:     pick(secrets, s("sec")),
			LifeGoal:   pick(lifeGoals, s("goal")),
		})
	}

	v.Bonds = generateBonds(worldID, seed)
	return v
}

// generateBonds 织一张密度足够、确定性的关系网（每人 1–2 条主关系，部分双向）。
func generateBonds(worldID string, seed int64) []Bond {
	bonds := make([]Bond, 0, VillageSize*2)
	seen := map[string]bool{}
	add := func(from, to int, k struct {
		Kind                            string
		Trust, Fear, Affection, Rivalry float64
		Mutual                          bool
	}) {
		if from == to {
			return
		}
		key := fmt.Sprintf("%d-%d-%s", from, to, k.Kind)
		if seen[key] {
			return
		}
		seen[key] = true
		bonds = append(bonds, Bond{From: from, To: to, Kind: k.Kind,
			Trust: clamp10(k.Trust), Fear: clamp10(k.Fear), Affection: clamp10(k.Affection), Rivalry: clamp10(k.Rivalry)})
	}

	for i := 0; i < VillageSize; i++ {
		s := func(tag string) uint64 { return h64(worldID, seed, fmt.Sprintf("b%d:%s", i, tag)) }
		degree := 1 + int(s("deg")%2) // 1 或 2 条
		for d := 0; d < degree; d++ {
			j := int(s(fmt.Sprintf("to%d", d)) % VillageSize)
			if j == i {
				j = (j + 1) % VillageSize
			}
			k := pick(bondKinds, s(fmt.Sprintf("kind%d", d)))
			add(i, j, k)
			if k.Mutual {
				add(j, i, k)
			}
		}
	}
	return bonds
}
