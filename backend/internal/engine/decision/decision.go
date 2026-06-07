package decision

// 文件说明：三层执行模型的「决策层」路由。反射层(纯代码,零 LLM)处理绝大多数日常 tick；
// 只有命中「关键决策节点」时才升级到决断层(调用 LLM 产出意图)。结算层不在此包，由
// status.Mutator / combat 等确定性代码完成。本包是设计方案 §1.5「决策用 LLM、结算用代码」
// 的工程落地：把昂贵的 LLM 调用从「每步」收敛到「每个有意义的决策节点一次」，使
// "<2% 上 LLM" 从「预算耗尽时的降级」变成「设计的常态」。

import (
	"hash/fnv"
	"strconv"
)

// Tier 表示一次决策由哪一层产生。
type Tier string

const (
	// TierReflex 反射层：纯代码规则直接得出意图，零 LLM。
	TierReflex Tier = "reflex"
	// TierDeliberate 决断层：需要 LLM 在模糊情境下产出意图。
	TierDeliberate Tier = "deliberate"
)

// Action 是单位的意图动作类型（轻量枚举，与 session 层决策动作对齐的子集）。
type Action string

const (
	ActionHold     Action = "hold"     // 原地待命
	ActionContinue Action = "continue" // 继续既有目标
	ActionEat      Action = "eat"      // 进食
	ActionFlee     Action = "flee"     // 撤退保命
	ActionMove     Action = "move"     // 移动
	ActionGather   Action = "gather"   // 采集/谋生
	ActionEngage   Action = "engage"   // 交战
)

// Intent 是一次决策的结构化结果——「想做什么」，不含任何数值结算。
type Intent struct {
	Action   Action `json:"action"`
	TargetID string `json:"target_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Tier     Tier   `json:"tier"`
}

// Situation 是决策路由的输入快照：纯数据，不依赖 DB / LLM，便于确定性与测试。
// Hunger 取值 0..100，与 unit.Status 一致（100=饱足，低于阈值=饥饿）。
type Situation struct {
	UnitID   string
	WorldID  string
	RegionID string
	Tick     int

	HP    int
	HPMax int

	Hunger    int
	HasRation bool

	EnemyInSight   bool
	EnemyAdjacent  bool
	FirstContact   bool // 首次遭遇敌对
	HasNewOrder    bool // 玩家刚下达新 order
	SocialOffer    bool // 收到社交/交易/恋爱/结盟邀约
	StrategicFork  bool // 战略岔路（势力态势剧变等）
	PlayerWatching bool // 玩家在场可见范围内

	CurrentGoal Action // 既有目标，反射层默认沿用
}

// Decision 是路由结果。NeedsLLM=true 表示这是关键节点、需要决断层调用 LLM 产出意图；
// 此时 Intent 给出一个「LLM 失败时」可直接落地的安全兜底意图。
type Decision struct {
	Intent   Intent
	NeedsLLM bool
}

// Router 编排反射规则与升级闸门。零值 Router 即可使用（采用默认阈值与默认闸门）。
type Router struct {
	HPFleeRatio        float64             // HP/HPMax 低于此值触发反射撤退，默认 0.25
	HungerEatThreshold int                 // Hunger 低于此值且有口粮时触发反射进食，默认 30
	Gate               func(Situation) bool // 关键节点判定，默认 DefaultEscalationGate
}

// DefaultRouter 返回采用默认阈值与默认闸门的路由器。
func DefaultRouter() Router {
	return Router{HPFleeRatio: 0.25, HungerEatThreshold: 30, Gate: DefaultEscalationGate}
}

// DefaultEscalationGate 关键节点判定：只有这些情形才值得花一次 LLM。
// 其余绝大多数日常 tick 都由反射层零成本处理。
func DefaultEscalationGate(s Situation) bool {
	switch {
	case s.FirstContact, s.HasNewOrder, s.SocialOffer, s.StrategicFork:
		return true
	case s.EnemyInSight && s.PlayerWatching:
		// 玩家在场目睹的战斗时点是高光节点，值得上 LLM。
		return true
	default:
		return false
	}
}

// Route 决定这一步意图的来源：反射 or 决断。
//
//	1) 安全反射(L1 生理护栏)：即使是关键节点也先保命，零 LLM。
//	2) 关键节点：升级决断层(NeedsLLM=true)，并给一个安全兜底意图。
//	3) 日常反射：继续既有目标或原地待命，零 LLM。
func (r Router) Route(s Situation) Decision {
	// 1) 安全反射
	if s.HPMax > 0 && float64(s.HP)/float64(s.HPMax) < r.fleeRatio() {
		return Decision{Intent: Intent{Action: ActionFlee, Reason: "HP 危急，撤退保命", Tier: TierReflex}}
	}
	if s.Hunger < r.eatThreshold() && s.HasRation {
		return Decision{Intent: Intent{Action: ActionEat, Reason: "饥饿，就地进食", Tier: TierReflex}}
	}

	// 2) 关键节点 → 决断层
	if r.gate()(s) {
		return Decision{
			NeedsLLM: true,
			Intent:   Intent{Action: r.safeFallback(s), Reason: "关键决策节点，交由决断层产出意图", Tier: TierDeliberate},
		}
	}

	// 3) 日常反射
	return Decision{Intent: Intent{Action: r.safeFallback(s), Reason: "日常推进，反射层处理", Tier: TierReflex}}
}

// safeFallback 给出一个保守、可直接落地的兜底动作。
func (r Router) safeFallback(s Situation) Action {
	if s.CurrentGoal != "" {
		return s.CurrentGoal
	}
	return ActionHold
}

func (r Router) fleeRatio() float64 {
	if r.HPFleeRatio > 0 {
		return r.HPFleeRatio
	}
	return 0.25
}

func (r Router) eatThreshold() int {
	if r.HungerEatThreshold > 0 {
		return r.HungerEatThreshold
	}
	return 30
}

func (r Router) gate() func(Situation) bool {
	if r.Gate != nil {
		return r.Gate
	}
	return DefaultEscalationGate
}

// Seed 生成确定性随机种子，用于反射层的平局/朝向等抉择。
// 复用项目约定：FNV(worldID+regionID+unitID+tick+salt)，保证可复现、可审计。
func Seed(s Situation, salt string) uint32 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(s.WorldID))
	_, _ = hasher.Write([]byte(s.RegionID))
	_, _ = hasher.Write([]byte(s.UnitID))
	_, _ = hasher.Write([]byte(strconv.Itoa(s.Tick)))
	_, _ = hasher.Write([]byte(salt))
	return hasher.Sum32()
}

// Roll 返回 [0,1) 的确定性随机值，供反射层做带概率的小抉择。
func Roll(s Situation, salt string) float64 {
	return float64(Seed(s, salt)%10000) / 10000
}
