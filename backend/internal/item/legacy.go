package item

// 文件说明：装备耐久衰减（GEAR_DAMAGED，PvE 威胁系统 §6 层1）与传家物升级（Legacy，§5 闭环）的纯原语。
// 这些函数刻意只在「原始字段」（int 耐久 / bool 标记）上做值进值出的纯计算，**不引用 unit.ItemStack**——
// 因为 unit 包已 import 本 item 包（inventory.go），反向 import 会形成 import 环。
// 调用方（session 层）持有 unit.ItemStack，把对应字段读出传入、再把结果写回字段即可（见各函数 crossFileNeeds 说明）。
// 全部确定性、无随机、无钱包/计费输入（反 P2W）；受保护状态字段一概不碰（耐久非受保护字段，留痕走 events.ReasonGearDamaged）。

// DurabilityResult 是一次耐久衰减的结果（值类型，便于调用方决定是否落 ReasonGearDamaged 留痕）。
type DurabilityResult struct {
	Durability int  // 衰减后的耐久值
	Changed    bool // 本次是否实际发生衰减（无变化则调用方不必落留痕）
	Floored    bool // 是否触及「不归零硬锁」下限（耐久降到 1 即止，永不破坏）
}

// DurabilityFloor 是耐久衰减的硬下限——苦战只损耐久、永不把装备打到 0（设计 §6「掉非 pinned 物/耐久损」但不一刀切毁装备）。
// 归零=损毁是不可逆后果，按层3口径处理（须过后果分级闸/同意），不由本层 1 的日常衰减触发。
const DurabilityFloor = 1

// DegradeDurability 对一件装备的耐久做确定性衰减（§6 层1：失败/苦战后兵器折损）。
//
// 入参：
//   - current：当前耐久值（0 表示无限耐久/不衰减——多数物品默认，直接 no-op）；
//   - amount：本次拟扣减的耐久（≤0 则 no-op）；
//   - pinned：是否为传家宝（Pinned）。**pinned 物绝不被耐久衰减**（设计「掉非 pinned 物」——传家宝不在损耗之列）。
//
// 不变量：
//   - 无限耐久（current<=0）恒不衰减；
//   - pinned 恒不衰减（保护玩家心血）；
//   - 衰减结果硬锁在 DurabilityFloor=1，**不归零即不破坏**（永不由本路径损毁装备）。
//
// 返回 DurabilityResult；Changed=true 时调用方应在 session 侧落一条 events.ReasonGearDamaged 流程留痕（耐久非受保护字段）。
func DegradeDurability(current, amount int, pinned bool) DurabilityResult {
	// 无限耐久 / pinned 传家宝 / 非正扣减 → 一律不动。
	if current <= 0 || pinned || amount <= 0 {
		return DurabilityResult{Durability: current, Changed: false, Floored: false}
	}
	next := current - amount
	floored := false
	if next < DurabilityFloor {
		next = DurabilityFloor
		floored = true
	}
	return DurabilityResult{Durability: next, Changed: next != current, Floored: floored}
}

// DefeatDurabilityLoss 给出「一场失败/苦战」后某件装备的确定性耐久扣减量（设计 §6 层1）。
// 确定性：扣减量只由「是否落败」与该装备承受的回合数派生（非随机、非钱包）；调用方再喂给 DegradeDurability。
//   - lost=true（战斗落败/撤退）：基础折损更重；
//   - roundsEngaged：参战回合数越多、磨损越多（每回合 1 点，封顶 maxDefeatLoss 防一场仗打废）。
func DefeatDurabilityLoss(lost bool, roundsEngaged int) int {
	if roundsEngaged <= 0 {
		return 0
	}
	loss := roundsEngaged
	if loss > maxDefeatLoss {
		loss = maxDefeatLoss
	}
	if lost {
		loss += defeatExtraLoss // 落败额外折损（苦战未果，家伙折得更狠）
	}
	return loss
}

// 耐久折损参数（确定性常量）。
const (
	maxDefeatLoss   = 5 // 单场战斗的回合磨损封顶
	defeatExtraLoss = 3 // 落败的额外折损
)

// --- 传家物升级（§5：用某装备完成 Clutch/跨关键命运节点 → 玩家确认后刻成传家物）---

// LegacyFlags 是一件装备的传承相关标记快照（值进值出，避免引用 unit.ItemStack）。
// 对应 unit.ItemStack 的 IsLegacy / SoulBound / Pinned 三个 bool 字段。
type LegacyFlags struct {
	IsLegacy  bool // 传家遗物：阵亡时可被继承（session/legacy_inheritance.go）
	SoulBound bool // 灵魂绑定：玩家手动交易/赠予也禁止（防 RMT）
	Pinned    bool // 传家宝：离线自治绝不自动卖/赠（喂 GateSurprise 的 sell_pinned 硬门）
}

// LegacyUpgrade 把一件装备刻为传家物（设计 §5 闭环的「落标记」步骤，玩家在待决策卡确认后调用）。
// 纯函数、幂等：置 IsLegacy=true + SoulBound=true + Pinned=true（永久锚的三重保险）。
//   - IsLegacy → 阵亡时进传承（legacy_inheritance 转给在乎她的继承人）；
//   - SoulBound → 连玩家手动交易/赠予都禁止（不可 RMT）；
//   - Pinned → 离线自治逻辑绝不自动卖/赠；且让 session 侧把它识别为「父辈遗志」永久锚，
//     此后 decision.GateSurprise(sell_pinned) 直接 Reject（LLM 自治也卖不掉，见 crossFileNeeds）。
//
// 返回升级后的标记；调用方写回 unit.ItemStack 对应字段，并在 session 侧落 events.ReasonLegacyBequeathed 留痕。
func LegacyUpgrade(flags LegacyFlags) LegacyFlags {
	flags.IsLegacy = true
	flags.SoulBound = true
	flags.Pinned = true
	return flags
}

// IsPermanentAnchor 判断标记是否已构成「永久锚」（父辈遗志级）——三重标记齐备即不可变卖。
// session 侧把「目录外具名独有遗物 OR 本标记永久锚」喂给 GateSurprise 的 ItemIsPermanentAnchor，落实 sell_pinned→Reject。
func IsPermanentAnchor(flags LegacyFlags) bool {
	return flags.IsLegacy && flags.SoulBound && flags.Pinned
}
