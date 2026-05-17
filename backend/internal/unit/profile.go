package unit

// 文件说明：单位档案核心数据结构定义，覆盖身份、属性、技能、状态、记忆与库存序列化模型。

type Identity struct {
	Name             string `json:"name"`
	Nickname         string `json:"nickname"`
	PortraitURL      string `json:"portrait_url"`
	Gender           string `json:"gender"`
	Lineage          string `json:"lineage"`
	Age              int    `json:"age"`
	Biography        string `json:"biography"`
	RecruitmentPitch string `json:"recruitment_pitch"`
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
	InCombat        bool     `json:"in_combat"`
	Injuries        []string `json:"injuries"`
	Debuffs         []string `json:"debuffs"`
}

// MemoryProfile 结构体用于承载该模块的核心数据。
type MemoryProfile struct {
	RecentEventIDs []string `json:"recent_event_ids"`
	Highlights     []string `json:"highlights"`
}

// ItemStack 结构体用于承载该模块的核心数据。
type ItemStack struct {
	ItemID     string `json:"item_id"`
	Quantity   int    `json:"quantity"`
	CustomName string `json:"custom_name,omitempty"`
	Level      int    `json:"level,omitempty"`
}

// Inventory 结构体用于承载该模块的核心数据。
type Inventory struct {
	Equipment map[string]ItemStack `json:"equipment"`
	Backpack  []ItemStack          `json:"backpack"`
}

// Profile 结构体用于承载该模块的核心数据。
type Profile struct {
	Identity    Identity      `json:"identity"`
	Stats       Stats         `json:"stats"`
	Skills      SkillSet      `json:"skills"`
	Personality Personality   `json:"personality"`
	Social      SocialState   `json:"social"`
	Status      Status        `json:"status"`
	Memory      MemoryProfile `json:"memory"`
}

// Record 结构体用于承载该模块的核心数据。
type Record struct {
	ID          string        `json:"id"`
	SessionID   string        `json:"session_id"`
	FactionID   string        `json:"faction_id"`
	Identity    Identity      `json:"identity"`
	Stats       Stats         `json:"stats"`
	Skills      SkillSet      `json:"skills"`
	Personality Personality   `json:"personality"`
	Social      SocialState   `json:"social"`
	Status      Status        `json:"status"`
	Memory      MemoryProfile `json:"memory"`
	Inventory   Inventory     `json:"inventory"`
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
		Identity:    record.Identity,
		Stats:       record.Stats,
		Skills:      record.Skills,
		Personality: record.Personality,
		Social:      record.Social,
		Status:      record.Status,
		Memory:      record.Memory,
	}
}

// profileDocument 结构体用于承载该模块的核心数据。
type profileDocument struct {
	Identity Identity      `json:"identity"`
	Stats    Stats         `json:"stats"`
	Skills   SkillSet      `json:"skills"`
	Social   SocialState   `json:"social"`
	Memory   MemoryProfile `json:"memory"`
}
