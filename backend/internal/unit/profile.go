package unit

// 文件说明：单位档案核心数据结构定义，覆盖身份、属性、技能、状态、记忆与库存序列化模型。

import "qunxiang/backend/internal/faction"

type Identity struct {
	Name             string `json:"name"`
	Nickname         string `json:"nickname"`
	PortraitURL      string `json:"portrait_url"`
	Gender           string `json:"gender"`
	Lineage          string `json:"lineage"`
	Age              int    `json:"age"`
	Biography        string `json:"biography"`
	RecruitmentPitch string `json:"recruitment_pitch"`
	// LifecycleClass 是单位的生命周期分级（分区大世界阶段4 §6，用户明确分三类）：
	//   protagonist=玩家主角（Age 冻结、永不衰老死亡，命运可玩性根基）；
	//   functional=固定功能性 NPC（商人/传送/任务发布/铁匠，Age 冻结、永不死亡，世界服务恒定）；
	//   mortal=普通 NPC（路人/村民/野外散人/子嗣，Age 随世界 tick 增长、高龄确定性自然死亡，世界新陈代谢）。
	// 非受保护字段（仿 Faction/Ambition，直接读写、不走 status.Mutator），随 profile blob（profileDocument.Identity）整块持久化。
	// omitempty：旧档/空 class 反序列化为空串——衰老结算里**保守视为 mortal**（有新陈代谢），但主角恒按 PlayerUnitIDs 双保险护住、绝不死。
	LifecycleClass LifecycleClass `json:"lifecycle_class,omitempty"`
}

// LifecycleClass 是单位生命周期分级（分区大世界阶段4 §6）。
type LifecycleClass string

const (
	// LifecycleProtagonist 玩家控制角色——Age 完全冻结、永不衰老死亡（穿越时代的恒定主角）。
	LifecycleProtagonist LifecycleClass = "protagonist"
	// LifecycleFunctional 固定功能性 NPC（商人/传送管理员/任务发布/铁匠）——Age 冻结、永不死亡（世界服务恒定可用）。
	LifecycleFunctional LifecycleClass = "functional"
	// LifecycleMortal 普通 NPC（路人/村民/野外散人/子嗣）——Age 随世界 tick 增长、高龄按确定性掷骰自然死亡（世界新陈代谢）。
	LifecycleMortal LifecycleClass = "mortal"
)

// IsMortal 判定该分级是否会衰老死亡。空串（旧档/未标记）**保守视为 mortal**——有新陈代谢，
// 但主角的不死由调用方按 LifecycleClass!=mortal && 不在 PlayerUnitIDs 双保险另外护住（见 session/aging.go）。
func (c LifecycleClass) IsMortal() bool {
	return c != LifecycleProtagonist && c != LifecycleFunctional
}

// SocialState 记录单位在人际关系系统中的轻量状态。
type SocialState struct {
	LoverUnitID     string   `json:"lover_unit_id,omitempty"`
	ParentUnitIDs   []string `json:"parent_unit_ids,omitempty"`
	ChildUnitIDs    []string `json:"child_unit_ids,omitempty"`
	BornTurn        int      `json:"born_turn,omitempty"`
	LastRomanceTurn int      `json:"last_romance_turn,omitempty"`
	Wildling        bool     `json:"wildling,omitempty"`
}

// PrimaryStats 结构体用于承载该模块的核心数据。
type PrimaryStats struct {
	Strength     int `json:"strength"`
	Dexterity    int `json:"dexterity"`
	Constitution int `json:"constitution"`
	Wisdom       int `json:"wisdom"`
	Perception   int `json:"perception"`
	Charisma     int `json:"charisma"`
}

// DerivedStats 结构体用于承载该模块的核心数据。
type DerivedStats struct {
	Attack      int `json:"attack"`
	Defense     int `json:"defense"`
	Accuracy    int `json:"accuracy"`
	Evasion     int `json:"evasion"`
	Vision      int `json:"vision"`
	CarryWeight int `json:"carry_weight"`
}

// GrowthStats 结构体用于承载该模块的核心数据。
type GrowthStats struct {
	Level       int `json:"level"`
	Experience  int `json:"experience"`
	SkillPoints int `json:"skill_points"`
}

// Stats 结构体用于承载该模块的核心数据。
type Stats struct {
	Primary PrimaryStats `json:"primary"`
	Derived DerivedStats `json:"derived"`
	Growth  GrowthStats  `json:"growth"`
}

// WeaponSkills 结构体用于承载该模块的核心数据。
type WeaponSkills struct {
	Sword   int `json:"sword"`
	Bow     int `json:"bow"`
	Blunt   int `json:"blunt"`
	Shield  int `json:"shield"`
	Medical int `json:"medical"`
}

// SurvivalSkills 结构体用于承载该模块的核心数据。
type SurvivalSkills struct {
	Scouting  int `json:"scouting"`
	Stealth   int `json:"stealth"`
	Medicine  int `json:"medicine"`
	Gathering int `json:"gathering"`
}

// SocialSkills 结构体用于承载该模块的核心数据。
type SocialSkills struct {
	Negotiation  int `json:"negotiation"`
	Intimidation int `json:"intimidation"`
	Charm        int `json:"charm"`
	Trade        int `json:"trade"`
}

// SkillSet 结构体用于承载该模块的核心数据。
type SkillSet struct {
	Weapons     WeaponSkills   `json:"weapons"`
	Survival    SurvivalSkills `json:"survival"`
	Social      SocialSkills   `json:"social"`
	Specialties []string       `json:"specialties"`
}

// Status 结构体用于承载该模块的核心数据。
type Status struct {
	HP              int      `json:"hp"`
	MP              int      `json:"mp"`
	LivesRemaining  int      `json:"lives_remaining"`
	LivesMax        int      `json:"lives_max"`
	LifeState       string   `json:"life_state"`
	RecoveryTurns   int      `json:"recovery_turns"`
	Attack          int      `json:"attack"`
	Defense         int      `json:"defense"`
	Move            int      `json:"move"`
	Hunger          int      `json:"hunger"`
	StarvationTurns int      `json:"starvation_turns"`
	Fatigue         int      `json:"fatigue"`
	Mood            string   `json:"mood"`
	Morale          float64  `json:"morale"`
	Loyalty         float64  `json:"loyalty"`
	Wallet          int      `json:"wallet"`
	PositionQ       int      `json:"position_q"`
	PositionR       int      `json:"position_r"`
	ZoneID          string   `json:"zone_id,omitempty"` // 分区大世界：该单位所在区域 id（空=出生区/兼容旧档）；快照按主角当前区过滤 NPC 显示
	InCombat        bool     `json:"in_combat"`
	Injuries        []string `json:"injuries"`
	Debuffs         []string `json:"debuffs"`
}

// MemoryProfile 结构体用于承载该模块的核心数据。
type MemoryProfile struct {
	RecentEventIDs []string `json:"recent_event_ids"`
	Highlights     []string `json:"highlights"`
}

// Ambition 是单位的六维野心向量，各分量 [0,1]，刻画其内在驱动力，可被人格漂移调节、供离线自治目标重估参考。
// power 权势、vengeance 复仇、wealth 财富、lineage 血脉、mastery 精进、freedom 自由。
// 全 omitempty——旧存档无此字段反序列化为零值（全 0=无明显野心），向后兼容。
type Ambition struct {
	Power     float64 `json:"power,omitempty"`
	Vengeance float64 `json:"vengeance,omitempty"`
	Wealth    float64 `json:"wealth,omitempty"`
	Lineage   float64 `json:"lineage,omitempty"`
	Mastery   float64 `json:"mastery,omitempty"`
	Freedom   float64 `json:"freedom,omitempty"`
}

// ItemStack 结构体用于承载该模块的核心数据。
type ItemStack struct {
	ItemID     string `json:"item_id"`
	Quantity   int    `json:"quantity"`
	CustomName string `json:"custom_name,omitempty"`
	Level      int    `json:"level,omitempty"`
	// Pinned 标记「传家宝」——绝不被离线自治逻辑自动卖/赠（喂归因 GateSurprise 的 ActionSellPinned 硬门）。
	// omitempty：旧存档无此字段反序列化为 false（默认可正常交易），向后兼容。
	Pinned bool `json:"pinned,omitempty"`
	// SoulBound 标记「灵魂绑定」——比 Pinned 更硬：不仅离线自治不卖/赠，连玩家手动交易/赠予也禁止。
	// 用于 epic/boss_relic 等不可交易掉落落库（设计 docs/PvE威胁系统.md 战利品传承）。
	// omitempty：旧存档无此字段反序列化为 false（默认可正常交易），向后兼容。
	SoulBound bool `json:"soul_bound,omitempty"`
	// IsLegacy 标记「传家遗物」——角色死亡时可被传承逻辑转移给在乎死者的继承人（session/legacy_inheritance.go）。
	// omitempty：旧存档无此字段反序列化为 false（默认非遗物、不参与传承），向后兼容。
	IsLegacy bool `json:"is_legacy,omitempty"`
	// Durability 耐久值——0 表示不衰减/无限耐久（多数物品默认）；>0 表示有限耐久，随使用递减、归 0 即损毁。
	// omitempty：旧存档无此字段反序列化为 0（默认无限耐久），向后兼容。
	Durability int `json:"durability,omitempty"`
}

// Inventory 结构体用于承载该模块的核心数据。
type Inventory struct {
	Equipment map[string]ItemStack `json:"equipment"`
	Backpack  []ItemStack          `json:"backpack"`
}

// Profile 结构体用于承载该模块的核心数据。
type Profile struct {
	Identity         Identity               `json:"identity"`
	Stats            Stats                  `json:"stats"`
	Skills           SkillSet               `json:"skills"`
	Personality      Personality            `json:"personality"`
	Ambition         Ambition               `json:"ambition,omitempty"`           // 六维野心向量；可被人格漂移调节，供离线自治目标重估参考
	Faction          string                 `json:"faction,omitempty"`            // 所属阵营（freedom/order/chaos），阵营开放世界 F1 引入
	MoralAlignment   faction.MoralAlignment `json:"moral_alignment,omitempty"`    // 3 维数值道德轴；F2 阵营切换输入、自治偏置
	MoralDriftStreak int                    `json:"moral_drift_streak,omitempty"` // F2 阵营切换隐藏条件②：主导阵营持续背离当前阵营的连击计数
	Social           SocialState            `json:"social"`
	Status           Status                 `json:"status"`
	Memory           MemoryProfile          `json:"memory"`
}

// Record 结构体用于承载该模块的核心数据。
type Record struct {
	ID          string      `json:"id"`
	SessionID   string      `json:"session_id"`
	FactionID   string      `json:"faction_id"`
	Identity    Identity    `json:"identity"`
	Stats       Stats       `json:"stats"`
	Skills      SkillSet    `json:"skills"`
	Personality Personality `json:"personality"`
	Ambition    Ambition    `json:"ambition,omitempty"` // 六维野心向量（与 Profile.Ambition 对齐），可被人格漂移调节
	// Faction 是单位当前所属阵营（freedom/order/chaos）——阵营开放世界 F1 引入的非保护字段（仿 Ambition，
	// 直接读写、不走 StatusMutator），随 profile blob 持久化。omitempty：旧存档/既有单位无此字段反序列化为空阵营，向后兼容。
	Faction string `json:"faction,omitempty"`
	// MoralAlignment 是单位的 3 维数值道德轴 {Freedom,Order,Chaos}（各 [0,100]）——F2 阵营切换的输入、自治决策偏置。
	// 非保护字段（仿 Ambition，直接读写、不走 StatusMutator），随 profile blob 持久化。
	// omitempty + 内嵌零值省略：旧存档/既有单位无此字段反序列化为零值道德轴（无明显倾向），向后兼容、零影响。
	MoralAlignment faction.MoralAlignment `json:"moral_alignment,omitempty"`
	// MoralDriftStreak 是「漂移后主导阵营持续背离当前阵营的连击计数」——F2 阵营切换隐藏条件②的持久化锚
	// （需持续 ≥ switchStreak 回合背离才有资格切换）。每个回合边界结算：主导阵营 != 当前阵营则 +1，否则归 0。
	// 非保护字段（仿 Ambition/MoralAlignment，直接读写、不走 StatusMutator），随 profile blob 持久化。
	// omitempty：旧存档/既有单位无此字段反序列化为 0（无背离连击），向后兼容、零影响。
	MoralDriftStreak int           `json:"moral_drift_streak,omitempty"`
	Social           SocialState   `json:"social"`
	Status           Status        `json:"status"`
	Memory           MemoryProfile `json:"memory"`
	Inventory        Inventory     `json:"inventory"`
	// Pinned 标记「不可自动处置」的角色（如传家血脉/受保护单位）——离线自治逻辑绝不自动卖/弃/送走。
	// omitempty：旧存档无此字段反序列化为 false，向后兼容。
	Pinned bool `json:"pinned,omitempty"`
	// Version 是乐观并发版本号（M7.3-real-3-0），仅由 GetByID 从 version 列填充、供 SaveOptimistic 做条件写；
	// 非 blob 字段、不参与序列化语义（json 仅为完整性）。其它读路径（ListBySession 等）不填，留 0。
	Version int64 `json:"version,omitempty"`
}

// DisplayName 返回单位显示名；缺失时回落到单位 ID。
func (record Record) DisplayName() string {
	if record.Identity.Name != "" {
		return record.Identity.Name
	}

	return record.ID
}

// Profile 导出单位可序列化的档案视图。
func (record Record) Profile() Profile {
	return Profile{
		Identity:         record.Identity,
		Stats:            record.Stats,
		Skills:           record.Skills,
		Personality:      record.Personality,
		Ambition:         record.Ambition,
		Faction:          record.Faction,
		MoralAlignment:   record.MoralAlignment,
		MoralDriftStreak: record.MoralDriftStreak,
		Social:           record.Social,
		Status:           record.Status,
		Memory:           record.Memory,
	}
}

// profileDocument 结构体用于承载该模块的核心数据。
type profileDocument struct {
	Identity         Identity               `json:"identity"`
	Stats            Stats                  `json:"stats"`
	Skills           SkillSet               `json:"skills"`
	Social           SocialState            `json:"social"`
	Memory           MemoryProfile          `json:"memory"`
	Ambition         Ambition               `json:"ambition,omitempty"`           // 六维野心向量（自发行为引力源），随 profile blob 持久化
	Faction          string                 `json:"faction,omitempty"`            // 所属阵营（freedom/order/chaos），阵营开放世界 F1 引入
	MoralAlignment   faction.MoralAlignment `json:"moral_alignment,omitempty"`    // 3 维数值道德轴；F2 阵营切换输入、自治偏置
	MoralDriftStreak int                    `json:"moral_drift_streak,omitempty"` // F2 阵营切换隐藏条件②：主导阵营持续背离当前阵营的连击计数
	Pinned           bool                   `json:"pinned,omitempty"`             // 角色级「不可自动处置」标记（离线自治绝不自动卖/弃/送走）
}
