package session

// 文件说明：types.go，会话域核心类型定义，集中声明回合、指令、天气、设施与快照数据结构。

import (
	"time"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

const ModeSinglePlayer = "single_player"
const ModeDuel = "duel"

// SetupPhase 类型定义用于统一开局组队阶段的数据表达。
type SetupPhase string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	SetupPhaseReady    SetupPhase = "ready"
	SetupPhaseDrafting SetupPhase = "drafting"
)
const FactionWildling = "wildling"

// UnitCandidate 是开局等待阶段展示给玩家的候选单位草案。
type UnitCandidate struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	Gender           string           `json:"gender"`
	PortraitURL      string           `json:"portrait_url,omitempty"`
	Age              int              `json:"age"`
	Biography        string           `json:"biography"`
	Stats            unit.Stats       `json:"stats,omitempty"`
	Skills           unit.SkillSet    `json:"skills,omitempty"`
	Personality      unit.Personality `json:"personality"`
	RecruitmentPitch string           `json:"recruitment_pitch"`
	Specialties      []string         `json:"specialties,omitempty"`
}

// Outcome 类型定义用于统一该模块的数据表达。
type Outcome string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	OutcomeOngoing Outcome = "ongoing"
	OutcomeVictory Outcome = "victory"
	OutcomeDefeat  Outcome = "defeat"
	OutcomeDraw    Outcome = "draw"
)

// VictoryPath 类型定义用于统一该模块的数据表达。
type VictoryPath string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	VictoryPathNone     VictoryPath = ""
	VictoryPathConquest VictoryPath = "conquest"
)

// DecisionAction 类型定义用于统一该模块的数据表达。
type DecisionAction string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	DecisionActionAttack      DecisionAction = "attack"
	DecisionActionCharge      DecisionAction = "charge"
	DecisionActionHeavyAttack DecisionAction = "heavy_attack"
	DecisionActionSkill       DecisionAction = "skill"
	DecisionActionDefend      DecisionAction = "defend"
	DecisionActionObserve     DecisionAction = "observe"
	DecisionActionAssist      DecisionAction = "assist"
	DecisionActionSay         DecisionAction = "say"
	DecisionActionDialogue    DecisionAction = "dialogue"
	DecisionActionTrade       DecisionAction = "trade"
	DecisionActionRomance     DecisionAction = "romance"
	DecisionActionFamily      DecisionAction = "family"
	DecisionActionBuild       DecisionAction = "build"
	DecisionActionDemolish    DecisionAction = "demolish"
	DecisionActionGather      DecisionAction = "gather"
	DecisionActionForge       DecisionAction = "forge"
	DecisionActionUpgrade     DecisionAction = "upgrade"
	DecisionActionEquip       DecisionAction = "equip"
	DecisionActionEat         DecisionAction = "eat"
	DecisionActionPickup      DecisionAction = "pickup"
	DecisionActionMove        DecisionAction = "move"
	DecisionActionHold        DecisionAction = "hold"
)

// TradeActionKind 类型定义用于统一该模块的数据表达。
type TradeActionKind string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	TradeActionKindGift TradeActionKind = "gift"
	TradeActionKindGold TradeActionKind = "gold"
	TradeActionKindSell TradeActionKind = "sell"
)

// ProductionActivity 类型定义用于统一该模块的数据表达。
type ProductionActivity string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	ProductionActivityFarm   ProductionActivity = "farm"
	ProductionActivityFish   ProductionActivity = "fish"
	ProductionActivityForage ProductionActivity = "forage"
	ProductionActivityHunt   ProductionActivity = "hunt"
	ProductionActivityMine   ProductionActivity = "mine"
)

// StructureType 类型定义用于统一该模块的数据表达。
type StructureType string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	StructureTypeFarmland   StructureType = "farmland"
	StructureTypeForge      StructureType = "forge"
	StructureTypeTrap       StructureType = "trap"
	StructureTypeTurret     StructureType = "turret"
	StructureTypeWatchtower StructureType = "watchtower"
)

// WeatherType 类型定义用于统一该模块的数据表达。
type WeatherType string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	WeatherClear WeatherType = "clear"
	WeatherWindy WeatherType = "windy"
	WeatherRainy WeatherType = "rainy"
	WeatherFoggy WeatherType = "foggy"
)

// WeatherState 结构体用于承载该模块的核心数据。
type WeatherState struct {
	Type        WeatherType `json:"type"`
	DisplayName string      `json:"display_name"`
	Note        string      `json:"note,omitempty"`
	Turn        int         `json:"turn"`
}

// Structure 结构体用于承载该模块的核心数据。
type Structure struct {
	ID               string        `json:"id"`
	Type             StructureType `json:"type"`
	FactionID        string        `json:"faction_id"`
	BuilderUnitID    string        `json:"builder_unit_id,omitempty"`
	Q                int           `json:"q"`
	R                int           `json:"r"`
	BuildProgress    int           `json:"build_progress"`
	BuildRequired    int           `json:"build_required"`
	Completed        bool          `json:"completed"`
	StartedTurn      int           `json:"started_turn"`
	CompletedTurn    int           `json:"completed_turn,omitempty"`
	HarvestReadyTurn int           `json:"harvest_ready_turn,omitempty"`
	Charges          int           `json:"charges,omitempty"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
}

// GraveMarker 标记一名单位的阵亡地点；超过保留回合后不再下发。
type GraveMarker struct {
	ID        string    `json:"id"`
	UnitID    string    `json:"unit_id"`
	UnitName  string    `json:"unit_name"`
	FactionID string    `json:"faction_id"`
	Q         int       `json:"q"`
	R         int       `json:"r"`
	Turn      int       `json:"turn"`
	CreatedAt time.Time `json:"created_at"`
}

// GroundLootDrop 标记地面掉落物；超过保留回合后自动消失。
type GroundLootDrop struct {
	ID             string           `json:"id"`
	Q              int              `json:"q"`
	R              int              `json:"r"`
	SourceUnitID   string           `json:"source_unit_id,omitempty"`
	SourceUnitName string           `json:"source_unit_name,omitempty"`
	InheritorID    string           `json:"inheritor_unit_id,omitempty"`
	Items          []unit.ItemStack `json:"items"`
	Turn           int              `json:"turn"`
	CreatedAt      time.Time        `json:"created_at"`
}

// Directive 结构体用于承载该模块的核心数据。
type Directive struct {
	ID           string         `json:"id"`
	Turn         int            `json:"turn"`
	Phase        turns.Phase    `json:"phase"`
	Kind         DirectiveKind  `json:"kind,omitempty"`
	Scope        DirectiveScope `json:"scope,omitempty"` // 指令作用域：空=即时回合指令（默认）；offline_charter=离线宪章长效授权
	Text         string         `json:"text"`
	Priority     string         `json:"priority,omitempty"`
	TargetUnitID string         `json:"target_unit_id,omitempty"`
	IssuedAt     time.Time      `json:"issued_at"`
	IssuedBy     string         `json:"issued_by"`
	AppliesTo    string         `json:"applies_to"`
}

// DirectiveScope 标记一条指令的作用域/时效域。
// 默认空值代表「即时回合指令」（沿用原有 Doctrine/Task/Order 当回合生效语义，向后兼容）；
// offline_charter 代表「离线宪章」——玩家不在场时单位据此自治（长效授权，写进 OfflineCharter 而非随回合刷新）。
type DirectiveScope string

// 常量定义区：集中声明指令作用域取值。
const (
	DirectiveScopeImmediate      DirectiveScope = ""                // 即时回合指令（默认零值，向后兼容旧存档）
	DirectiveScopeOfflineCharter DirectiveScope = "offline_charter" // 离线宪章：玩家不在场时的长效自治授权
)

// CharterRedline 是离线宪章里的一条红线（绝对禁区/底线），供归因校验器作锚（snap.Redlines）与硬门拦截。
type CharterRedline struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Severity string `json:"severity,omitempty"` // 严重度：soft/hard 等（空=默认普通），由上层逻辑解释
}

// OfflineCharter 是单个单位的「离线宪章」——玩家不在场时单位据此自治的三段长效授权。
// 三段语义：LongTermGoals 长期目标（驱动目标重估/记忆写入）、Redlines 红线（绝对禁区，喂归因校验 snap.Redlines）、
// SocialMandates 社交授权（允许/鼓励的社会行为，如「可代我结盟」「勿与某派结仇」）。
// per-unit 归属，挂进 State.UnitCharters[unitID]；整块随 state_json 持久化（全 omitempty 保旧存档反序列化）。
type OfflineCharter struct {
	LongTermGoals  []string         `json:"long_term_goals,omitempty"`
	Redlines       []CharterRedline `json:"redlines,omitempty"`
	SocialMandates []string         `json:"social_mandates,omitempty"`
}

// DirectiveKind 类型定义用于统一该模块的数据表达。
type DirectiveKind string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	DirectiveKindDoctrine DirectiveKind = "doctrine"
	DirectiveKindTask     DirectiveKind = "task"
	DirectiveKindOrder    DirectiveKind = "order"
)

// CommandPowerState 结构体用于承载该模块的核心数据。
type CommandPowerState struct {
	Current   int `json:"current"`
	Max       int `json:"max"`
	Regen     int `json:"regen"`
	OrderCost int `json:"order_cost"`
}

// FactionRelationState 类型定义用于统一该模块的数据表达。
type FactionRelationState string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	FactionRelationWar     FactionRelationState = "war"
	FactionRelationNeutral FactionRelationState = "neutral"
	FactionRelationAllied  FactionRelationState = "allied"
)

// FactionRelation 结构体用于承载该模块的核心数据。
type FactionRelation struct {
	LeftFactionID  string               `json:"left_faction_id"`
	RightFactionID string               `json:"right_faction_id"`
	State          FactionRelationState `json:"state"`
	UpdatedAt      time.Time            `json:"updated_at,omitempty"`
	Reason         string               `json:"reason,omitempty"`
}

// DialogueMessage 结构体用于承载该模块的核心数据。
type DialogueMessage struct {
	ID           string      `json:"id"`
	UnitID       string      `json:"unit_id"`
	Speaker      string      `json:"speaker"`
	Message      string      `json:"message"`
	Turn         int         `json:"turn"`
	Phase        turns.Phase `json:"phase"`
	OccurredAt   time.Time   `json:"occurred_at"`
	Provider     string      `json:"provider,omitempty"`
	Model        string      `json:"model,omitempty"`
	UsedFallback bool        `json:"used_fallback,omitempty"`
}

// DecisionTrace 结构体用于承载该模块的核心数据。
type DecisionTrace struct {
	ID                    string         `json:"id"`
	UnitID                string         `json:"unit_id"`
	FactionID             string         `json:"faction_id"`
	RequestedAction       DecisionAction `json:"requested_action,omitempty"`
	RequestedActivity     string         `json:"requested_activity,omitempty"`
	RequestedSkillID      string         `json:"requested_skill_id,omitempty"`
	RequestedStructureID  string         `json:"requested_structure_id,omitempty"`
	RequestedStructure    string         `json:"requested_structure_type,omitempty"`
	RequestedTargetUnitID string         `json:"requested_target_unit_id,omitempty"`
	RequestedTradeKind    string         `json:"requested_trade_kind,omitempty"`
	RequestedItemID       string         `json:"requested_item_id,omitempty"`
	RequestedOtherItemID  string         `json:"requested_other_item_id,omitempty"`
	RequestedPrice        int            `json:"requested_price,omitempty"`
	RequestedGoldAmount   int            `json:"requested_gold_amount,omitempty"`
	RequestedTargetQ      int            `json:"requested_target_q,omitempty"`
	RequestedTargetR      int            `json:"requested_target_r,omitempty"`
	RequestedNextAction   string         `json:"requested_next_action,omitempty"`
	RequestedSpeak        string         `json:"requested_speak,omitempty"`
	RequestedMemory       string         `json:"requested_memory,omitempty"`
	RequestedKnowledge    string         `json:"requested_knowledge,omitempty"`
	RequestedReasoning    string         `json:"requested_reasoning,omitempty"`
	Action                DecisionAction `json:"action"`
	Activity              string         `json:"activity,omitempty"`
	SkillID               string         `json:"skill_id,omitempty"`
	StructureID           string         `json:"structure_id,omitempty"`
	StructureType         string         `json:"structure_type,omitempty"`
	TargetUnitID          string         `json:"target_unit_id,omitempty"`
	TradeKind             string         `json:"trade_kind,omitempty"`
	ItemID                string         `json:"item_id,omitempty"`
	OtherItemID           string         `json:"other_item_id,omitempty"`
	Price                 int            `json:"price,omitempty"`
	GoldAmount            int            `json:"gold_amount,omitempty"`
	TargetQ               int            `json:"target_q,omitempty"`
	TargetR               int            `json:"target_r,omitempty"`
	NextAction            string         `json:"next_action,omitempty"`
	Speak                 string         `json:"speak,omitempty"`
	Memory                string         `json:"memory,omitempty"`
	Knowledge             string         `json:"knowledge,omitempty"`
	Reasoning             string         `json:"reasoning"`
	ObedienceState        string         `json:"obedience_state,omitempty"`
	ObedienceNote         string         `json:"obedience_note,omitempty"`
	RejectProbability     float64        `json:"reject_probability,omitempty"`
	RiskScore             float64        `json:"risk_score,omitempty"`
	Defiant               bool           `json:"defiant,omitempty"`
	MemoryImportanceBoost int            `json:"memory_importance_boost,omitempty"`
	MoveMultiplier        float64        `json:"move_multiplier,omitempty"`
	AttackMultiplier      float64        `json:"attack_multiplier,omitempty"`
	ActionIndex           int            `json:"action_index,omitempty"`
	APBefore              int            `json:"ap_before,omitempty"`
	APCost                int            `json:"ap_cost,omitempty"`
	APAfter               int            `json:"ap_after,omitempty"`
	Turn                  int            `json:"turn"`
	Phase                 turns.Phase    `json:"phase"`
	OccurredAt            time.Time      `json:"occurred_at"`
	Provider              string         `json:"provider,omitempty"`
	Model                 string         `json:"model,omitempty"`
	UsedFallback          bool           `json:"used_fallback,omitempty"`
}

// LLMInteraction 结构体用于承载该模块的核心数据。
type LLMInteraction struct {
	ID            string                 `json:"id"`
	UnitID        string                 `json:"unit_id"`
	Kind          string                 `json:"kind"`
	Summary       string                 `json:"summary"`
	SystemPrompt  string                 `json:"system_prompt"`
	UserPrompt    string                 `json:"user_prompt"`
	ParsedOutput  string                 `json:"parsed_output,omitempty"`
	RawOutput     string                 `json:"raw_output,omitempty"`
	ErrorMessage  string                 `json:"error_message,omitempty"`
	FallbackCause string                 `json:"fallback_cause,omitempty"`
	Turn          int                    `json:"turn"`
	Phase         turns.Phase            `json:"phase"`
	OccurredAt    time.Time              `json:"occurred_at"`
	Provider      string                 `json:"provider,omitempty"`
	Model         string                 `json:"model,omitempty"`
	UsedFallback  bool                   `json:"used_fallback,omitempty"`
	PromptTokens  int                    `json:"prompt_tokens,omitempty"`
	OutputTokens  int                    `json:"output_tokens,omitempty"`
	TotalTokens   int                    `json:"total_tokens,omitempty"`
	EstimatedCost float64                `json:"estimated_cost_usd,omitempty"`
	Attempts      []ai.CompletionAttempt `json:"attempts,omitempty"`
	InProgress    bool                   `json:"in_progress,omitempty"`
	ElapsedMS     int64                  `json:"elapsed_ms,omitempty"`
}

// RawEventEntry 结构体用于承载该模块的核心数据。
type RawEventEntry struct {
	ID           string      `json:"id"`
	Turn         int         `json:"turn"`
	Phase        turns.Phase `json:"phase"`
	Source       string      `json:"source"`
	Kind         string      `json:"kind"`
	Summary      string      `json:"summary"`
	ActorUnitID  string      `json:"actor_unit_id,omitempty"`
	TargetUnitID string      `json:"target_unit_id,omitempty"`
	PayloadJSON  string      `json:"payload_json,omitempty"`
	OccurredAt   time.Time   `json:"occurred_at"`
}

// LogEntry 结构体用于承载该模块的核心数据。
type LogEntry struct {
	ID           string      `json:"id"`
	Turn         int         `json:"turn"`
	Phase        turns.Phase `json:"phase"`
	Kind         string      `json:"kind"`
	Message      string      `json:"message"`
	ActorUnitID  string      `json:"actor_unit_id,omitempty"`
	TargetUnitID string      `json:"target_unit_id,omitempty"`
	OccurredAt   time.Time   `json:"occurred_at"`
}

// PigeonDispatch 结构体用于承载该模块的核心数据。
type PigeonDispatch struct {
	ID                 string      `json:"id"`
	Turn               int         `json:"turn"`
	Phase              turns.Phase `json:"phase"`
	SenderUnitID       string      `json:"sender_unit_id"`
	ReceiverUnitID     string      `json:"receiver_unit_id"`
	Message            string      `json:"message"`
	AttachedItemID     string      `json:"attached_item_id,omitempty"`
	DeliverTurn        int         `json:"deliver_turn"`
	InterceptChance    float64     `json:"intercept_chance"`
	Intercepted        bool        `json:"intercepted,omitempty"`
	InterceptorFaction string      `json:"interceptor_faction,omitempty"`
	SentAt             time.Time   `json:"sent_at"`
	ResolvedAt         time.Time   `json:"resolved_at,omitempty"`
}

// BattleReport 结构体用于承载该模块的核心数据。
type BattleReport struct {
	ID                 string      `json:"id"`
	Turn               int         `json:"turn"`
	Phase              turns.Phase `json:"phase"`
	NarratorUnitID     string      `json:"narrator_unit_id"`
	Narrator           string      `json:"narrator"`
	Title              string      `json:"title,omitempty"`
	Content            string      `json:"content"`
	IllustrationPrompt string      `json:"illustration_prompt,omitempty"`
	IllustrationURL    string      `json:"illustration_url,omitempty"`
	Memory             string      `json:"memory,omitempty"`
	CreatedAt          time.Time   `json:"created_at"`
	Provider           string      `json:"provider,omitempty"`
	Model              string      `json:"model,omitempty"`
	UsedFallback       bool        `json:"used_fallback,omitempty"`
}

// HallArchiveEntry 表示对局结束后下发给前端展示的战后档案官条目。
type HallArchiveEntry struct {
	ID           string    `json:"id"`
	UnitID       string    `json:"unit_id"`
	UnitName     string    `json:"unit_name"`
	FactionID    string    `json:"faction_id"`
	Outcome      Outcome   `json:"outcome"`
	Biography    string    `json:"biography"`
	TopEvents    []string  `json:"top_events,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	Provider     string    `json:"provider,omitempty"`
	Model        string    `json:"model,omitempty"`
	UsedFallback bool      `json:"used_fallback,omitempty"`
}

// IntelAsset 结构体用于承载该模块的核心数据。
type IntelAsset struct {
	ID               string    `json:"id"`
	UnitID           string    `json:"unit_id"`
	HomeFactionID    string    `json:"home_faction_id"`
	HandlerFactionID string    `json:"handler_faction_id"`
	Mode             string    `json:"mode"`
	Motivation       string    `json:"motivation,omitempty"`
	Risk             float64   `json:"risk,omitempty"`
	SinceTurn        int       `json:"since_turn"`
	LastReportTurn   int       `json:"last_report_turn,omitempty"`
	Exposed          bool      `json:"exposed,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// IntelReport 结构体用于承载该模块的核心数据。
type IntelReport struct {
	ID              string      `json:"id"`
	Turn            int         `json:"turn"`
	Phase           turns.Phase `json:"phase"`
	Kind            string      `json:"kind"`
	UnitID          string      `json:"unit_id"`
	SourceFactionID string      `json:"source_faction_id"`
	TargetFactionID string      `json:"target_faction_id"`
	Summary         string      `json:"summary"`
	CreatedAt       time.Time   `json:"created_at"`
}

// ModerationReport 结构体用于承载该模块的核心数据。
type ModerationReport struct {
	ID         string      `json:"id"`
	SessionID  string      `json:"session_id"`
	Turn       int         `json:"turn"`
	Phase      turns.Phase `json:"phase"`
	Reporter   string      `json:"reporter"`
	UnitID     string      `json:"unit_id,omitempty"`
	Category   string      `json:"category"`
	Detail     string      `json:"detail"`
	CreatedAt  time.Time   `json:"created_at"`
	Resolved   bool        `json:"resolved,omitempty"`
	ResolvedAt time.Time   `json:"resolved_at,omitempty"`
}

// AuditBundle 结构体用于承载该模块的核心数据。
type AuditBundle struct {
	SessionID       string             `json:"session_id"`
	Reports         []ModerationReport `json:"reports"`
	DialogueHistory []DialogueMessage  `json:"dialogue_history"`
	LLMInteractions []LLMInteraction   `json:"llm_interactions"`
	Logs            []LogEntry         `json:"logs"`
	RawEventLog     []RawEventEntry    `json:"raw_event_log"`
}

// PrivacyEraseOptions 结构体用于承载该模块的核心数据。
type PrivacyEraseOptions struct {
	EraseDialogue   bool `json:"erase_dialogue"`
	EraseLLMDetails bool `json:"erase_llm_details"`
	EraseAuditTrail bool `json:"erase_audit_trail"`
	EraseMemories   bool `json:"erase_memories"`
	EraseReports    bool `json:"erase_reports"`
}

// PrivacyEraseResult 结构体用于承载该模块的核心数据。
type PrivacyEraseResult struct {
	SessionID                 string `json:"session_id"`
	DialogueEntriesErased     int    `json:"dialogue_entries_erased"`
	LLMInteractionsRedacted   int    `json:"llm_interactions_redacted"`
	DecisionTracesErased      int64  `json:"decision_traces_erased"`
	AuditLogsErased           int    `json:"audit_logs_erased"`
	RawEventsErased           int    `json:"raw_events_erased"`
	ReportsErased             int    `json:"reports_erased"`
	UnitHighlightsErased      int    `json:"unit_highlights_erased"`
	MemoryRowsErased          int64  `json:"memory_rows_erased"`
	MemoryFTSRowsErased       int64  `json:"memory_fts_rows_erased"`
	PhaseSnapshotsRegenerated bool   `json:"phase_snapshots_regenerated"`
}

// PrivacyPurgeResult 结构体用于承载该模块的核心数据。
type PrivacyPurgeResult struct {
	RetentionDays          int   `json:"retention_days"`
	CutoffUnix             int64 `json:"cutoff_unix"`
	SessionsDeleted        int64 `json:"sessions_deleted"`
	UnitsDeleted           int64 `json:"units_deleted"`
	EventsDeleted          int64 `json:"events_deleted"`
	HallEntriesDeleted     int64 `json:"hall_entries_deleted"`
	PhaseSnapsDeleted      int64 `json:"phase_snapshots_deleted"`
	MemoriesFTSDeleted     int64 `json:"memories_fts_deleted"`
	LLMInteractionsDeleted int64 `json:"llm_interactions_deleted"`
	DecisionTracesDeleted  int64 `json:"decision_traces_deleted"`
	RawEventsDeleted       int64 `json:"raw_events_deleted"`
	WakeQueueDeleted       int64 `json:"wake_queue_deleted"`
	DecisionJobsDeleted    int64 `json:"decision_jobs_deleted"`
}

// SessionMetrics 结构体用于承载该模块的核心数据。
type SessionMetrics struct {
	CrossFactionInteractions int     `json:"cross_faction_interactions"`
	LLMPromptTokens          int     `json:"llm_prompt_tokens"`
	LLMOutputTokens          int     `json:"llm_output_tokens"`
	LLMTotalTokens           int     `json:"llm_total_tokens"`
	LLMEstimatedCostUSD      float64 `json:"llm_estimated_cost_usd"`
}

// PregnancyState 记录已同意共同养育后的孕期进度。
type PregnancyState struct {
	ID             string   `json:"id"`
	ParentUnitIDs  []string `json:"parent_unit_ids"`
	PregnantUnitID string   `json:"pregnant_unit_id"`
	StartedTurn    int      `json:"started_turn"`
	DueTurn        int      `json:"due_turn"`
}

// State 是服务端权威状态，包含完整可回放信息（含内部审计字段）。
type State struct {
	ID                    string            `json:"id"`
	AccountID             string            `json:"account_id,omitempty"`              // 本局所属账户（空=匿名/单机局，账户级成本配额对其 no-op）；随 state_json 持久化，供 LLM 成本闭环按账户记账与配额拦截
	MinorMode             bool              `json:"minor_mode,omitempty"`              // 本局是否未成年模式（建局时由 compliance.Gate 裁定落库）：开启则全程关闭恋爱·生育、降露骨暴力叙事；随 state_json 持久化，断线重连/推进自动带回，匿名/成年局默认 false 零影响
	CrossSurfaceWatermark map[string]int    `json:"cross_surface_watermark,omitempty"` // 每角色已浮现到的跨玩家事件 world_tick 水位线：部署边界仅浮现 tick 大于水位线的新跨玩家事件，防同一事件每回合重复刷命运卡（共享世界默认开后必需的去重）
	WorldID               string            `json:"world_id,omitempty"`                // 本局所属世界（空=未接入多世界；接入后实战交互自动写世界总线）
	Mode                  string            `json:"mode"`
	RandomSeed            int64             `json:"random_seed"`
	PlayerFactionID       string            `json:"player_faction_id"`
	EnemyFactionID        string            `json:"enemy_faction_id"`
	SetupPhase            SetupPhase        `json:"setup_phase,omitempty"`
	SetupDeadlineAt       time.Time         `json:"setup_deadline_at,omitempty"`
	DraftRequiredPick     int               `json:"draft_required_pick,omitempty"`
	PlayerDraftPool       []unit.Record     `json:"player_draft_pool,omitempty"`
	EnemyDraftPool        []unit.Record     `json:"enemy_draft_pool,omitempty"`
	MapScriptID           string            `json:"map_script_id,omitempty"`
	MapScriptName         string            `json:"map_script_name,omitempty"`
	MapSizeID             string            `json:"map_size_id,omitempty"`
	MapSizeName           string            `json:"map_size_name,omitempty"`
	FogOfWarEnabled       bool              `json:"fog_of_war_enabled"`
	RandomEventsDisabled  bool              `json:"random_events_disabled,omitempty"`
	TurnState             turns.State       `json:"turn_state"`
	PhaseReady            map[string]bool   `json:"phase_ready,omitempty"`
	ExecutionInProgress   bool              `json:"execution_in_progress,omitempty"`
	Outcome               Outcome           `json:"outcome"`
	WinnerFactionID       string            `json:"winner_faction_id,omitempty"`
	VictoryPath           VictoryPath       `json:"victory_path,omitempty"`
	Weather               WeatherState      `json:"weather"`
	Map                   world.MapSnapshot `json:"map"`
	// Zones 是分区大世界的全部区域（设计 docs/分区大世界设计方案-2026-06-10.md §1）。
	// 空 = 旧单图存档（向后兼容，Map 即唯一区域）；非空时 Map 恒是 CurrentZoneID 区域的投影拷贝
	// （旧代码读 state.Map 照常工作，作用于「当前区域」）。整块随 state_json 持久化。
	Zones []world.Zone `json:"zones,omitempty"`
	// CurrentZoneID 是主角当前所在区域 id（空 = 单区/兼容旧档）；travel 切区时改它并把 Map 重投影为该区地图。
	CurrentZoneID string `json:"current_zone_id,omitempty"`
	// SeededZoneIDs 是「已 lazy 播种过公共 NPC 的区域 id 集合」（阶段2 §1）。建局时出生区已播种 → 初始化含出生区 id；
	// travel 进入某区后若不在此集合则 ensureZoneSeededBestEffort 播种 + 标 NPC.ZoneID + append。omitempty 保旧档兼容。
	SeededZoneIDs []string `json:"seeded_zone_ids,omitempty"`
	// DefeatedBosses 是「本世界周期内已被讨平的区域 id 集合」（阶段2 §3 防反复刷）。ChallengeZoneBoss 胜利后 append
	// 对应 zoneID，已在集合内则拒绝再战。失败不入集合（可重试）。omitempty 保旧档兼容。
	DefeatedBosses   []string          `json:"defeated_bosses,omitempty"`
	CommandPower     CommandPowerState `json:"command_power"`
	FactionRelations []FactionRelation `json:"faction_relations"`
	PlayerUnitIDs    []string          `json:"player_unit_ids"`
	EnemyUnitIDs     []string          `json:"enemy_unit_ids"`
	WildUnitIDs      []string          `json:"wild_unit_ids,omitempty"`
	// AmbientUnitIDs 是阵营开放世界出生点的「公共同阵营 NPC」单位 ID（faction_spawn.go SeedFactionSpawn 落库）。
	// 这批 NPC **静态上图可见**（进 AmbientUnits 快照、有 hex 坐标），但**绝不自治、零 LLM**——
	// 故**绝不进 PlayerUnitIDs/EnemyUnitIDs/WildUnitIDs**（那三者会进 buildExecutionOrderByATB 被唤醒决策，
	// 每拍 +8~12 次 LLM 成本爆炸）。仅做轻量确定性微游走（settleExecutionToDeploymentBoundary，flag 门控）。
	// 随 state_json 持久化；omitempty 保旧存档反序列化兼容。
	AmbientUnitIDs   []string         `json:"ambient_unit_ids,omitempty"`
	Structures       []Structure      `json:"structures"`
	GraveMarkers     []GraveMarker    `json:"grave_markers,omitempty"`
	GroundLootDrops  []GroundLootDrop `json:"ground_loot_drops,omitempty"`
	GlobalDirective  Directive        `json:"global_directive"`
	DirectiveHistory []Directive      `json:"directive_history"`
	// UnitCharters 是每单位的离线宪章（unitID → OfflineCharter），玩家不在场时单位据此自治。
	// 长效授权（区别于随回合刷新的 Directive），整块随 state_json 持久化；omitempty 保旧存档反序列化。
	UnitCharters map[string]OfflineCharter `json:"unit_charters,omitempty"`
	// ConsumedPOIs 是已被「采完/探完」的地图兴趣点（"q,r" → 消耗时回合数）。POI 本体仍由 map_pois.go
	// 哈希确定性派生（不落库），这里只记消耗态：下发时标 consumed 供前端徽标变淡，结算侧作幂等防重放闸。
	// 整块随 state_json 持久化；omitempty 保旧存档反序列化（helper 见 poi_state.go）。
	ConsumedPOIs       map[string]int     `json:"consumed_pois,omitempty"`
	DialogueHistory    []DialogueMessage  `json:"dialogue_history"`
	DecisionTraces     []DecisionTrace    `json:"decision_traces"`
	LLMInteractions    []LLMInteraction   `json:"llm_interactions"`
	PigeonQueue        []PigeonDispatch   `json:"pigeon_queue"`
	Pregnancies        []PregnancyState   `json:"pregnancies,omitempty"`
	BattleReports      []BattleReport     `json:"battle_reports"`
	HallArchiveEntries []HallArchiveEntry `json:"hall_archive_entries,omitempty"`
	IntelAssets        []IntelAsset       `json:"intel_assets"`
	IntelReports       []IntelReport      `json:"intel_reports"`
	ModerationReports  []ModerationReport `json:"moderation_reports"`
	Metrics            SessionMetrics     `json:"metrics"`
	RawEventLog        []RawEventEntry    `json:"raw_event_log"`
	Logs               []LogEntry         `json:"logs"`
	CreatedAt          time.Time          `json:"created_at"`
	UpdatedAt          time.Time          `json:"updated_at"`
}

// Snapshot 是下发给前端/客户端的会话视图，不含仅服务端内部使用字段。
type Snapshot struct {
	ID                  string             `json:"id"`
	WorldID             string             `json:"world_id,omitempty"`   // 本局所属世界（空=未接入多世界）；暴露给前端供世界 Boss 等跨玩家面板定位
	MinorMode           bool               `json:"minor_mode,omitempty"` // 本局未成年模式（前端据此可隐藏恋爱/生育入口、提示分级）
	Mode                string             `json:"mode"`
	RandomSeed          int64              `json:"random_seed"`
	PlayerFactionID     string             `json:"player_faction_id"`
	EnemyFactionID      string             `json:"enemy_faction_id"`
	SetupPhase          SetupPhase         `json:"setup_phase,omitempty"`
	SetupDeadlineAt     time.Time          `json:"setup_deadline_at,omitempty"`
	DraftRequiredPick   int                `json:"draft_required_pick,omitempty"`
	PlayerDraftPool     []unit.Record      `json:"player_draft_pool,omitempty"`
	EnemyDraftPool      []unit.Record      `json:"enemy_draft_pool,omitempty"`
	MapScriptID         string             `json:"map_script_id,omitempty"`
	MapScriptName       string             `json:"map_script_name,omitempty"`
	MapSizeID           string             `json:"map_size_id,omitempty"`
	MapSizeName         string             `json:"map_size_name,omitempty"`
	FogOfWarEnabled     bool               `json:"fog_of_war_enabled"`
	RandomEventsEnabled bool               `json:"random_events_enabled"`
	TurnState           turns.State        `json:"turn_state"`
	PhaseReady          map[string]bool    `json:"phase_ready,omitempty"`
	ExecutionInProgress bool               `json:"execution_in_progress,omitempty"`
	Outcome             Outcome            `json:"outcome"`
	WinnerFactionID     string             `json:"winner_faction_id,omitempty"`
	VictoryPath         VictoryPath        `json:"victory_path,omitempty"`
	Weather             WeatherState       `json:"weather"`
	Map                 world.MapSnapshot  `json:"map"`
	CommandPower        CommandPowerState  `json:"command_power"`
	FactionRelations    []FactionRelation  `json:"faction_relations"`
	Structures          []Structure        `json:"structures"`
	GraveMarkers        []GraveMarker      `json:"grave_markers,omitempty"`
	GroundLootDrops     []GroundLootDrop   `json:"ground_loot_drops,omitempty"`
	GlobalDirective     Directive          `json:"global_directive"`
	DirectiveHistory    []Directive        `json:"directive_history"`
	DialogueHistory     []DialogueMessage  `json:"dialogue_history"`
	DecisionTraces      []DecisionTrace    `json:"decision_traces"`
	LLMInteractions     []LLMInteraction   `json:"llm_interactions"`
	ActiveLLMCalls      []LLMInteraction   `json:"active_llm_calls,omitempty"`
	PigeonQueue         []PigeonDispatch   `json:"pigeon_queue"`
	Pregnancies         []PregnancyState   `json:"pregnancies,omitempty"`
	BattleReports       []BattleReport     `json:"battle_reports"`
	HallArchiveEntries  []HallArchiveEntry `json:"hall_archive_entries,omitempty"`
	IntelAssets         []IntelAsset       `json:"intel_assets"`
	IntelReports        []IntelReport      `json:"intel_reports"`
	ModerationReports   []ModerationReport `json:"moderation_reports"`
	Metrics             SessionMetrics     `json:"metrics"`
	RawEventLog         []RawEventEntry    `json:"raw_event_log"`
	Logs                []LogEntry         `json:"logs"`
	PlayerUnits         []unit.Record      `json:"player_units"`
	EnemyUnits          []unit.Record      `json:"enemy_units"`
	WildUnits           []unit.Record      `json:"wild_units,omitempty"`
	// AmbientUnits 是出生点公共同阵营 NPC 的快照视图（静态上图可见，有 hex 坐标）。前端据此把 NPC 画上命运地图。
	// **观战性质**：这批 NPC 不自治、零 LLM、不进执行 order；前端按位置渲染即可（契约：SessionSnapshot.ambient_units）。
	AmbientUnits []unit.Record `json:"ambient_units,omitempty"`
}

// PhaseBoundarySnapshotMeta 结构体用于承载该模块的核心数据。
type PhaseBoundarySnapshotMeta struct {
	ID        string      `json:"id"`
	SessionID string      `json:"session_id"`
	Turn      int         `json:"turn"`
	Phase     turns.Phase `json:"phase"`
	CreatedAt time.Time   `json:"created_at"`
}

// ReconnectSnapshot 结构体用于承载该模块的核心数据。
type ReconnectSnapshot struct {
	Session          Snapshot                    `json:"session"`
	Boundary         PhaseBoundarySnapshotMeta   `json:"boundary"`
	BoundarySession  Snapshot                    `json:"boundary_session"`
	RecentBoundaries []PhaseBoundarySnapshotMeta `json:"recent_boundaries"`
}
