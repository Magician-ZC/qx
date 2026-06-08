package events

// 文件说明：维护状态变更原因码目录，提供内置定义查询与数据库种子写入/读取。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/storage/dbdialect"
)

// Category 类型定义用于统一该模块的数据表达。
type Category string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	CategoryCombat     Category = "combat_damage"
	CategorySurvival   Category = "survival_consumption"
	CategoryEmotion    Category = "emotion_event"
	CategoryEconomy    Category = "economy_material"
	CategoryRelation   Category = "relation_change"
	CategoryCommand    Category = "command_response"
	CategoryFate       Category = "fate_event"      // 命运流程事件（相关性命中/待决策入队等，非状态变更）
	CategoryPlayer     Category = "player_action"   // 玩家动作（接管/嘱咐等，可被 order_echo 回响引用）
	CategoryLifecycle  Category = "lifecycle_event" // 大世界生命周期（出生/死亡/复仇得偿/势力崩塌/人格漂移，沙盘 §8.7）
	CategoryGovernance Category = "governance"      // 治理处置（举报闭环：警告/封禁对被举报单位的状态后果，经 status.Mutator 落地）
)

// ReasonCode 类型定义用于统一该模块的数据表达。
type ReasonCode string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	ReasonCombatHit        ReasonCode = "COMBAT_HIT"
	ReasonCombatDown       ReasonCode = "COMBAT_DOWN"
	ReasonSurvivalMarch    ReasonCode = "SURVIVAL_MARCH_EXHAUST"
	ReasonSurvivalHunger   ReasonCode = "SURVIVAL_HUNGER"
	ReasonAmbientForage    ReasonCode = "AMBIENT_FORAGE" // 大世界离线自治：饿了在野外觅食补口粮（region-runner L1，§8.2）
	ReasonAmbientRest      ReasonCode = "AMBIENT_REST"   // 大世界离线自治：日常起居的缓慢口粮消耗（中性，区别于"补给不足"的 SURVIVAL_HUNGER）
	ReasonEmotionTrauma    ReasonCode = "EMOTION_TRAUMA"
	ReasonEmotionReward    ReasonCode = "EMOTION_REWARD"
	ReasonAmbientSocialize ReasonCode = "AMBIENT_SOCIALIZE" // 大世界离线自治：与人攀谈交往，士气舒展（region-runner L3，§8.2）
	ReasonAmbientReflect   ReasonCode = "AMBIENT_REFLECT"   // 大世界离线自治：独自思忖沉淀，心绪渐定（region-runner L3）
	ReasonBloodFeudGrief   ReasonCode = "BLOOD_FEUD_GRIEF"  // 血仇传播：在乎死者的哀悼者闻死讯哀恸，士气小幅下挫（blood_feud.go，耦合文档「传播」）
	ReasonEconomyPurchase  ReasonCode = "ECONOMY_PURCHASE"
	ReasonEconomyReward    ReasonCode = "ECONOMY_REWARD"
	ReasonEconomyLoot      ReasonCode = "ECONOMY_LOOT"
	ReasonRelationRescue   ReasonCode = "RELATION_RESCUED"
	ReasonRelationBetray   ReasonCode = "RELATION_BETRAYAL"
	ReasonCommandForced    ReasonCode = "COMMAND_FORCED_ORDER"
	ReasonCommandAccepted  ReasonCode = "COMMAND_ACCEPTED_ADVICE"

	// 命运流程事件（经 EmitProcessEvent，非状态变更，不经 Mutator）。
	ReasonRelevanceMatch   ReasonCode = "RELEVANCE_MATCH"   // 世界事件命中某角色的锚
	ReasonInboxHighlight   ReasonCode = "INBOX_HIGHLIGHT"   // 进高光卡（可一瞥）
	ReasonPendingDecision  ReasonCode = "PENDING_DECISION"  // 升级待决策，入命运收件箱
	ReasonDecisionResolved ReasonCode = "DECISION_RESOLVED" // 待决策被处理（玩家或过期兜底）

	// 玩家动作事件（经 EmitProcessEvent，可被归因校验器的 order_echo 引用——回响 Echo 的锚点）。
	ReasonPlayerIntervention ReasonCode = "PLAYER_INTERVENTION" // 玩家直接接管/嘱咐了一次（验证 §5.2 埋点 + M3 回响锚）

	// 回响：本次自治选择被归因到一条真实的过往玩家动作（「因为你上次…，所以这次…」，宪法 §6.2）。
	ReasonEchoLink ReasonCode = "ECHO_LINK"

	// 社会客体撮合：某角色被 MatchScore+arbitration 确定性撮合进一个社会客体（组队/结盟/市集…，§2.2）。流程事件，非状态变更。
	ReasonSocialObjectBind ReasonCode = "SOCIAL_OBJECT_BIND"

	// 大世界生命周期（沙盘 §8.7）。CHARACTER_DIED / LOYALTY_GAIN / LOYALTY_STRAIN 改保护字段、经 status.Mutator；
	// 其余（出生/复仇得偿/势力崩塌/人格漂移）是流程事件，经 EmitProcessEvent 留痕（人格漂移非保护状态字段，不走 Mutator）。
	ReasonCharacterBorn      ReasonCode = "CHARACTER_BORN"
	ReasonCharacterDied      ReasonCode = "CHARACTER_DIED"
	ReasonVengeanceFulfilled ReasonCode = "VENGEANCE_FULFILLED"
	ReasonFactionCollapse    ReasonCode = "FACTION_COLLAPSE"
	ReasonPersonalityDrift   ReasonCode = "PERSONALITY_DRIFT"
	ReasonLoyaltyGain        ReasonCode = "LOYALTY_GAIN"
	ReasonLoyaltyStrain      ReasonCode = "LOYALTY_STRAIN"

	// 治理处置（举报闭环 ResolveModerationReport，经 status.Mutator 改保护字段并留痕）。
	// MODERATION_WARNING：警告——对被举报单位小幅下调士气示警；MODERATION_BAN：封禁——重罚士气与忠诚。
	ReasonModerationWarning ReasonCode = "MODERATION_WARNING"
	ReasonModerationBan     ReasonCode = "MODERATION_BAN"

	// 本波（offline_charter + 编年史 + 传播 + 自治）新增流程事件码——除特别注明外均经 EmitProcessEvent
	// 留痕，**不改保护状态字段、不走 status.Mutator**（流程事件旁路）。
	// GOAL_REASSESS：单位据离线宪章长期目标做目标重估，把结论写进记忆（流程+写记忆）。
	ReasonGoalReassess ReasonCode = "GOAL_REASSESS"
	// CHRONICLE_RECORD：一条事件被物化进编年史（chronicle_entries），供传记/分享卡取材。
	ReasonChronicleRecord ReasonCode = "CHRONICLE_RECORD"
	// BLOOD_FEUD_PROPAGATE：血仇/敌意沿关系图传播的一跳（propagation_log 留痕；区别于落士气的 BLOOD_FEUD_GRIEF）。
	ReasonBloodFeudPropagate ReasonCode = "BLOOD_FEUD_PROPAGATE"
	// FREEZE_INTERCEPT：离线自治选择触碰宪章红线/Pinned 硬门被冻结拦截（如卖传家宝/叛变），回退安全决策。
	ReasonFreezeIntercept ReasonCode = "FREEZE_INTERCEPT"
	// CHARTER_ACTIVATED：玩家离场，单位据离线宪章进入自治授权态（长效授权生效留痕）。
	ReasonCharterActivated ReasonCode = "CHARTER_ACTIVATED"
	// CHARTER_UPDATED：玩家更新/撤销了某单位的离线宪章（授权变更留痕）。
	ReasonCharterUpdated ReasonCode = "CHARTER_UPDATED"
	// AMBITION_SHIFT：六维野心向量随经历/人格漂移发生迁移（流程留痕，野心非保护状态字段）。
	ReasonAmbitionShift ReasonCode = "AMBITION_SHIFT"
	// REDLINE_TRIP：某行为触发了宪章红线（被归因校验/硬门判为越线），用于回响与复盘留痕。
	ReasonRedlineTrip ReasonCode = "REDLINE_TRIP"
)

// ReasonCodeDefinition 结构体用于承载该模块的核心数据。
type ReasonCodeDefinition struct {
	Code              ReasonCode `json:"code"`
	Category          Category   `json:"category"`
	DisplayName       string     `json:"display_name"`
	DefaultReasonText string     `json:"default_reason_text"`
	StatDomains       []string   `json:"stat_domains"`
	ImportanceMin     int        `json:"importance_min"`
	ImportanceMax     int        `json:"importance_max"`
}

// Catalog 返回内置事件原因码目录，用于状态变更与事件落盘标准化。
func Catalog() []ReasonCodeDefinition {
	return []ReasonCodeDefinition{
		{Code: ReasonCombatHit, Category: CategoryCombat, DisplayName: "战斗受伤", DefaultReasonText: "在战斗中受到伤害", StatDomains: []string{"hp"}, ImportanceMin: 5, ImportanceMax: 8},
		{Code: ReasonCombatDown, Category: CategoryCombat, DisplayName: "倒地濒死", DefaultReasonText: "在战斗中被打至倒地", StatDomains: []string{"hp", "lives_remaining"}, ImportanceMin: 8, ImportanceMax: 10},
		{Code: ReasonSurvivalMarch, Category: CategorySurvival, DisplayName: "行军透支", DefaultReasonText: "连续行军导致疲劳上升", StatDomains: []string{"fatigue"}, ImportanceMin: 2, ImportanceMax: 4},
		{Code: ReasonSurvivalHunger, Category: CategorySurvival, DisplayName: "饥饿消耗", DefaultReasonText: "补给不足导致饥饿加深", StatDomains: []string{"hunger"}, ImportanceMin: 2, ImportanceMax: 4},
		{Code: ReasonAmbientForage, Category: CategorySurvival, DisplayName: "野外觅食", DefaultReasonText: "她在战斗之外觅食，补充了口粮", StatDomains: []string{"hunger"}, ImportanceMin: 2, ImportanceMax: 4},
		{Code: ReasonAmbientRest, Category: CategorySurvival, DisplayName: "日常消耗", DefaultReasonText: "她在战斗之外的日常起居里消耗了些口粮", StatDomains: []string{"hunger"}, ImportanceMin: 1, ImportanceMax: 2},
		{Code: ReasonAmbientSocialize, Category: CategoryEmotion, DisplayName: "日常交往", DefaultReasonText: "她在战斗之外与人攀谈交往，心情舒展了些", StatDomains: []string{"morale"}, ImportanceMin: 2, ImportanceMax: 4},
		{Code: ReasonAmbientReflect, Category: CategoryEmotion, DisplayName: "独自沉淀", DefaultReasonText: "她独自思忖沉淀，心绪渐定", StatDomains: []string{"morale"}, ImportanceMin: 1, ImportanceMax: 3},
		{Code: ReasonBloodFeudGrief, Category: CategoryEmotion, DisplayName: "闻丧哀恸", DefaultReasonText: "她在乎的人死于他人之手，悲恸难抑、士气受挫", StatDomains: []string{"morale"}, ImportanceMin: 5, ImportanceMax: 8},
		{Code: ReasonEmotionTrauma, Category: CategoryEmotion, DisplayName: "创伤事件", DefaultReasonText: "目睹惨烈事件后情绪受挫", StatDomains: []string{"morale"}, ImportanceMin: 6, ImportanceMax: 9},
		{Code: ReasonEmotionReward, Category: CategoryEmotion, DisplayName: "荣誉奖励", DefaultReasonText: "获得奖励后士气提升", StatDomains: []string{"morale"}, ImportanceMin: 6, ImportanceMax: 8},
		{Code: ReasonEconomyPurchase, Category: CategoryEconomy, DisplayName: "物资购买", DefaultReasonText: "花费金币购入物资", StatDomains: []string{"wallet"}, ImportanceMin: 2, ImportanceMax: 5},
		{Code: ReasonEconomyReward, Category: CategoryEconomy, DisplayName: "任务奖励", DefaultReasonText: "完成任务后获得奖励", StatDomains: []string{"wallet"}, ImportanceMin: 3, ImportanceMax: 6},
		{Code: ReasonEconomyLoot, Category: CategoryEconomy, DisplayName: "战利品继承", DefaultReasonText: "继承敌方资产", StatDomains: []string{"wallet"}, ImportanceMin: 4, ImportanceMax: 7},
		{Code: ReasonRelationRescue, Category: CategoryRelation, DisplayName: "被队友救援", DefaultReasonText: "被同伴救下一命", StatDomains: []string{"loyalty"}, ImportanceMin: 5, ImportanceMax: 8},
		{Code: ReasonRelationBetray, Category: CategoryRelation, DisplayName: "遭遇背叛", DefaultReasonText: "对同伴的信任受到打击", StatDomains: []string{"loyalty"}, ImportanceMin: 5, ImportanceMax: 8},
		{Code: ReasonCommandForced, Category: CategoryCommand, DisplayName: "强制命令", DefaultReasonText: "被强令执行高风险命令", StatDomains: []string{"loyalty", "morale"}, ImportanceMin: 3, ImportanceMax: 7},
		{Code: ReasonCommandAccepted, Category: CategoryCommand, DisplayName: "建议被采纳", DefaultReasonText: "自己的建议被采纳", StatDomains: []string{"loyalty", "morale"}, ImportanceMin: 3, ImportanceMax: 6},
		{Code: ReasonRelevanceMatch, Category: CategoryFate, DisplayName: "命运相关", DefaultReasonText: "一件事触到了她在乎的人或物", StatDomains: []string{}, ImportanceMin: 3, ImportanceMax: 7},
		{Code: ReasonInboxHighlight, Category: CategoryFate, DisplayName: "高光时刻", DefaultReasonText: "她经历了一段值得一看的事", StatDomains: []string{}, ImportanceMin: 4, ImportanceMax: 7},
		{Code: ReasonPendingDecision, Category: CategoryFate, DisplayName: "待决策", DefaultReasonText: "一件关乎她命运的事在等你拿主意", StatDomains: []string{}, ImportanceMin: 7, ImportanceMax: 10},
		{Code: ReasonDecisionResolved, Category: CategoryFate, DisplayName: "决断已下", DefaultReasonText: "一件待决策的事有了着落", StatDomains: []string{}, ImportanceMin: 5, ImportanceMax: 8},
		{Code: ReasonPlayerIntervention, Category: CategoryPlayer, DisplayName: "玩家接管", DefaultReasonText: "你直接为她拿了一次主意", StatDomains: []string{}, ImportanceMin: 5, ImportanceMax: 9},
		{Code: ReasonEchoLink, Category: CategoryPlayer, DisplayName: "回响", DefaultReasonText: "因为你上次的选择，这次她做了不一样的事", StatDomains: []string{}, ImportanceMin: 5, ImportanceMax: 9},
		{Code: ReasonSocialObjectBind, Category: CategoryFate, DisplayName: "撮合入局", DefaultReasonText: "命运把她和另几个人牵到了一处", StatDomains: []string{}, ImportanceMin: 4, ImportanceMax: 7},
		{Code: ReasonCharacterBorn, Category: CategoryLifecycle, DisplayName: "新生", DefaultReasonText: "一个新生命降临到这个世界", StatDomains: []string{}, ImportanceMin: 4, ImportanceMax: 7},
		{Code: ReasonCharacterDied, Category: CategoryLifecycle, DisplayName: "陨落", DefaultReasonText: "一个角色的生命走到了尽头", StatDomains: []string{"lives_remaining"}, ImportanceMin: 8, ImportanceMax: 10},
		{Code: ReasonVengeanceFulfilled, Category: CategoryLifecycle, DisplayName: "夙愿得偿", DefaultReasonText: "她了结了一桩萦绕已久的恩怨", StatDomains: []string{}, ImportanceMin: 7, ImportanceMax: 10},
		{Code: ReasonFactionCollapse, Category: CategoryLifecycle, DisplayName: "势力崩塌", DefaultReasonText: "一方势力土崩瓦解", StatDomains: []string{}, ImportanceMin: 7, ImportanceMax: 10},
		{Code: ReasonPersonalityDrift, Category: CategoryLifecycle, DisplayName: "性情流转", DefaultReasonText: "经历沉淀，她的性情悄然变了一些", StatDomains: []string{}, ImportanceMin: 3, ImportanceMax: 6},
		{Code: ReasonLoyaltyGain, Category: CategoryRelation, DisplayName: "归心", DefaultReasonText: "因为某些经历，她更认同你了", StatDomains: []string{"loyalty"}, ImportanceMin: 3, ImportanceMax: 6},
		{Code: ReasonLoyaltyStrain, Category: CategoryRelation, DisplayName: "离心", DefaultReasonText: "某些经历让她对你生了疏离", StatDomains: []string{"loyalty"}, ImportanceMin: 3, ImportanceMax: 6},
		{Code: ReasonModerationWarning, Category: CategoryGovernance, DisplayName: "治理警告", DefaultReasonText: "因一桩举报被裁定示警，她的士气受了些影响", StatDomains: []string{"morale"}, ImportanceMin: 4, ImportanceMax: 7},
		{Code: ReasonModerationBan, Category: CategoryGovernance, DisplayName: "治理封禁", DefaultReasonText: "因一桩举报被裁定封禁，她的士气与归属感重挫", StatDomains: []string{"morale", "loyalty"}, ImportanceMin: 7, ImportanceMax: 10},
		{Code: ReasonGoalReassess, Category: CategoryLifecycle, DisplayName: "目标重估", DefaultReasonText: "她对照心中的长远图景，重新掂量了眼下该做的事", StatDomains: []string{}, ImportanceMin: 3, ImportanceMax: 6},
		{Code: ReasonChronicleRecord, Category: CategoryLifecycle, DisplayName: "编年记述", DefaultReasonText: "这一笔被记进了编年史", StatDomains: []string{}, ImportanceMin: 3, ImportanceMax: 6},
		{Code: ReasonBloodFeudPropagate, Category: CategoryRelation, DisplayName: "血仇蔓延", DefaultReasonText: "一桩血仇沿着人心传到了她这里", StatDomains: []string{}, ImportanceMin: 4, ImportanceMax: 8},
		{Code: ReasonFreezeIntercept, Category: CategoryLifecycle, DisplayName: "红线拦截", DefaultReasonText: "她正要做的事触到了底线，被拦了下来", StatDomains: []string{}, ImportanceMin: 5, ImportanceMax: 8},
		{Code: ReasonCharterActivated, Category: CategoryPlayer, DisplayName: "宪章生效", DefaultReasonText: "你不在的时候，她将依你立下的章程自行其是", StatDomains: []string{}, ImportanceMin: 3, ImportanceMax: 6},
		{Code: ReasonCharterUpdated, Category: CategoryPlayer, DisplayName: "宪章变更", DefaultReasonText: "你重新拟定了她离场后的行事章程", StatDomains: []string{}, ImportanceMin: 3, ImportanceMax: 6},
		{Code: ReasonAmbitionShift, Category: CategoryLifecycle, DisplayName: "野心流转", DefaultReasonText: "经历沉淀，她内心所求悄然偏移了几分", StatDomains: []string{}, ImportanceMin: 3, ImportanceMax: 6},
		{Code: ReasonRedlineTrip, Category: CategoryLifecycle, DisplayName: "触碰红线", DefaultReasonText: "一桩行为越过了她（或你）立下的红线", StatDomains: []string{}, ImportanceMin: 5, ImportanceMax: 8},
	}
}

// ProcessEvent 是一条命运流程事件（非状态变更，不经 status.Mutator）。
type ProcessEvent struct {
	SessionID     string
	OwnerUnitID   string // 这条事件属于谁（actor）
	RelatedUnitID string // 关联对象（target，可空则用 owner）
	Code          ReasonCode
	Category      Category
	Payload       any // 序列化进 payload_json
	// 世界作用域双键（沙盘 §8.7，可空=未接入多世界；接入后用于 region 分片/跨世界检索）。
	WorldID  string
	RegionID string
	Tick     int
}

// EmitProcessEvent 把一条命运流程事件直接写入 events 表（append-only 留痕），不改任何状态字段。
// 用于命运收件箱/相关性命中这类「事件本身」而非「状态变更」的留痕（设计文档「纯流程事件旁路」）。
func EmitProcessEvent(ctx context.Context, execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, event ProcessEvent) (string, error) {
	related := event.RelatedUnitID
	if related == "" {
		related = event.OwnerUnitID
	}
	category := event.Category
	if category == "" {
		category = CategoryFate
	}
	payload := event.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal process event payload: %w", err)
	}
	id := uuid.NewString()
	if _, err := execer.ExecContext(
		ctx,
		`
		INSERT INTO events (
			id, session_id, actor_unit_id, target_unit_id, event_type, reason_code, payload_json, occurred_at, world_id, region_id, tick
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
		id,
		event.SessionID,
		event.OwnerUnitID,
		related,
		string(category),
		string(event.Code),
		string(encoded),
		time.Now().UTC().Format(time.RFC3339Nano),
		nullableText(event.WorldID),
		nullableText(event.RegionID),
		event.Tick,
	); err != nil {
		return "", fmt.Errorf("insert process event: %w", err)
	}
	return id, nil
}

// nullableText 把空字符串映射为 SQL NULL（world_id/region_id 可空）。
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Lookup 按原因码查找定义。
func Lookup(code ReasonCode) (ReasonCodeDefinition, bool) {
	for _, definition := range Catalog() {
		if definition.Code == code {
			return definition, true
		}
	}
	return ReasonCodeDefinition{}, false
}

// SeedReasonCodeCatalog 把内置原因码目录写入数据库（存在则更新）。
func SeedReasonCodeCatalog(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin reason code transaction: %w", err)
	}
	defer tx.Rollback()

	for _, definition := range Catalog() {
		domains, err := json.Marshal(definition.StatDomains)
		if err != nil {
			return fmt.Errorf("marshal stat domains for %s: %w", definition.Code, err)
		}

		query := `
			INSERT INTO event_reason_codes (
				code,
				category,
				display_name,
				default_reason_text,
				stat_domains_json,
				importance_min,
				importance_max
			) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(code) DO UPDATE SET
				category = excluded.category,
				display_name = excluded.display_name,
				default_reason_text = excluded.default_reason_text,
				stat_domains_json = excluded.stat_domains_json,
				importance_min = excluded.importance_min,
				importance_max = excluded.importance_max
			`
		if dbdialect.IsMySQL(db) {
			query = `
			INSERT INTO event_reason_codes (
				code,
				category,
				display_name,
				default_reason_text,
				stat_domains_json,
				importance_min,
				importance_max
			) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				category = VALUES(category),
				display_name = VALUES(display_name),
				default_reason_text = VALUES(default_reason_text),
				stat_domains_json = VALUES(stat_domains_json),
				importance_min = VALUES(importance_min),
				importance_max = VALUES(importance_max)
			`
		}
		if _, err := tx.ExecContext(
			ctx,
			query,
			string(definition.Code),
			string(definition.Category),
			definition.DisplayName,
			definition.DefaultReasonText,
			string(domains),
			definition.ImportanceMin,
			definition.ImportanceMax,
		); err != nil {
			return fmt.Errorf("upsert reason code %s: %w", definition.Code, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reason code transaction: %w", err)
	}

	return nil
}

// LoadReasonCodeCatalog 从数据库读取原因码目录并反序列化 stat_domains 字段。
func LoadReasonCodeCatalog(ctx context.Context, db *sql.DB) ([]ReasonCodeDefinition, error) {
	orderBy := "rowid"
	if dbdialect.IsMySQL(db) {
		orderBy = "code"
	}
	rows, err := db.QueryContext(
		ctx,
		`
		SELECT code, category, display_name, default_reason_text, stat_domains_json, importance_min, importance_max
		FROM event_reason_codes
		ORDER BY `+orderBy,
	)
	if err != nil {
		return nil, fmt.Errorf("query reason codes: %w", err)
	}
	defer rows.Close()

	definitions := make([]ReasonCodeDefinition, 0, len(Catalog()))
	for rows.Next() {
		var definition ReasonCodeDefinition
		var domains string
		if err := rows.Scan(
			&definition.Code,
			&definition.Category,
			&definition.DisplayName,
			&definition.DefaultReasonText,
			&domains,
			&definition.ImportanceMin,
			&definition.ImportanceMax,
		); err != nil {
			return nil, fmt.Errorf("scan reason code: %w", err)
		}

		if err := json.Unmarshal([]byte(domains), &definition.StatDomains); err != nil {
			return nil, fmt.Errorf("decode stat domains for %s: %w", definition.Code, err)
		}

		definitions = append(definitions, definition)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reason codes: %w", err)
	}

	return definitions, nil
}
