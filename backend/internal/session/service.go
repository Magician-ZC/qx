package session

// 文件说明：service.go，会话系统主服务实现，负责单局初始化、回合推进、AI决策调度与状态持久化。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/analytics"
	combatdomain "qunxiang/backend/internal/combat"
	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	maxDirectiveHistory      = 64
	maxDialogueHistory       = 128
	maxDecisionHistory       = 96
	maxLLMHistory            = 48
	maxRawEventHistory       = 192
	maxFullLLMHistory        = 8
	maxStoredPromptRunes     = 12000
	maxStoredOutputRunes     = 8000
	maxPigeonQueue           = 96
	maxBattleReports         = 48
	battleReportsEnabled     = false
	maxIntelAssets           = 64
	maxIntelReports          = 160
	maxModerationReports     = 256
	baseExecutionAP          = 2
	maxActionsPerUnit        = 6
	asyncExecutionTimeout    = 45 * time.Minute
	asyncPhaseAdvanceTimeout = 3 * time.Minute
	asyncCleanupTimeout      = 30 * time.Second
	defiantMemoryBoost       = 3
	battlefieldRemnantTurns  = 5
	openingDraftWaitDuration = 60 * time.Second
)

var asyncExecutionRegistry = struct {
	sync.Mutex
	running map[string]struct{}
}{running: map[string]struct{}{}}

var asyncUnitNarrativeRegistry = struct {
	sync.Mutex
	running map[string]struct{}
}{running: map[string]struct{}{}}

var asyncHallMemoryRegistry = struct {
	sync.Mutex
	running map[string]struct{}
}{running: map[string]struct{}{}}

type executionActorState struct {
	remainingAP int
	actionIndex int
	started     bool
	completed   bool
}

// Service 结构体用于承载该模块的核心数据。
type Service struct {
	db        *sql.DB
	coldStore *sql.DB
	sessions  *Repository
	units     *unit.Repository
	mutator   *status.Mutator
	llm       completionClient

	asyncExecution   bool
	progressReporter func(reason string, snapshot Snapshot, extra map[string]any)
	broadcaster      Broadcaster
	sessionSaveMu    sync.Mutex

	coldSchemaMu    sync.Mutex
	coldSchemaReady bool

	memoryFTSOnce sync.Once
	memoryFTSErr  error

	memoryRefreshMu   sync.Mutex
	memoryRefreshTurn map[string]int
	memoryRecallMu    sync.Mutex
	memoryRecallTurn  map[string]int

	// 决策归因因果句瞬态缓存（§5.4）：generateUnitDecision 落地后按 unitID 暂存其 LLM 当次决策的 NarrativeZH，
	// 供同一执行流内由该单位造成的命运卡（如 WorldizeDeath）取「当次 LLM 因果句」而非启发式模板。执行流单 goroutine，加锁防御。
	decisionNarrativeMu sync.Mutex
	decisionNarrative   map[string]string

	// 归因校验强制开关（每实例配置）；累计计数为进程级全局，见 attribution_bridge.go。
	attributionEnforced bool

	// 离线自治调度开关（M7.3-real-4b，由 main/router 按 QUNXIANG_REGION_RUNNER_ENABLED 注入，默认关）：
	// 关时建局/组队不写 units 作用域列、不入唤醒队列，使大世界 region-runner 这一 flag-gated 能力对默认链路零成本。
	ambientSchedulingEnabled bool

	// 反射真短路开关（降本，由 main/router 按 QUNXIANG_REFLEX_SHORTCIRCUIT 注入，默认关）：开启后日常安静 tick 的单位决策
	// 由反射层零成本落地、跳过 LLM（见 reflex_shadow.go）。默认关时退化为纯影子统计（reflex_shadow.skip_rate）。
	reflexShortCircuit bool

	// 账户级 LLM 成本记账器（由 main/router 按 QUNXIANG_BILLING_ENABLED 开时注入 billing.Service，默认 nil）：
	// nil 安全——未注入时账户级记账/配额拦截整体 no-op，仅保留会话级预算护栏（llm_budget.go）。结构上由 SpendRecorder 接口约束，
	// 刻意不在 session 包 import billing（避免循环依赖），仅由 Wire agent 注入。
	spend SpendRecorder

	// 账户权益查询器（由 main/router 按 QUNXIANG_BILLING_ENABLED 开时注入 billing.Service 的适配器，默认 nil）：
	// nil 安全——未注入时叙事密度 perk 整体 no-op（narrativeDensityPerk 恒 false）。mirror spend SpendRecorder，刻意不 import billing。
	entitlements EntitlementChecker
}

// SpendRecorder 是账户级 LLM 成本记账器的最小接口（结构上由 billing.Service 满足，见 internal/billing/service.go）。
// 刻意只声明 session 真正用到的两个方法，避免 session→billing 的包依赖：
//   - AddSpend：把一次 LLM 成本（micro_usd）累加进账户当期配额（periodBucket=UTC "YYYY-MM"，跨月重置）。
//   - CheckQuota：判账户当期是否仍在成本上限内（allowed=true 放行；false=超额）。
type SpendRecorder interface {
	AddSpend(ctx context.Context, accountID, periodBucket string, microUSD int64) error
	CheckQuota(ctx context.Context, accountID string) (bool, error)
}

// SetSpendRecorder 注入账户级 LLM 成本记账器（Wire agent 在 QUNXIANG_BILLING_ENABLED 开时调用，传 billing.Service）。
// 传 nil 等价于关闭账户级记账/配额拦截（向后兼容，零行为变化）。
func (service *Service) SetSpendRecorder(r SpendRecorder) {
	if service == nil {
		return
	}
	service.spend = r
}

// recordLLMSpendBestEffort 把一次 LLM 交互的估算成本 best-effort 累加进账户级配额（设计 PRD §3.1 成本封顶）。
// 纪律：①未注入记账器 / 账户为空 / 成本<=0 → 直接 no-op，对默认链路零成本；②吞错，账户记账绝不中断或影响模拟主循环。
// 注意：CacheHit 回放在 buildLLMInteraction 处已把 EstimatedCost 归零，故此处天然不重复计费。
func (service *Service) recordLLMSpendBestEffort(ctx context.Context, state *State, interaction LLMInteraction) {
	if service == nil || service.spend == nil || state == nil {
		return
	}
	accountID := strings.TrimSpace(state.AccountID)
	if accountID == "" {
		return
	}
	micro := int64(interaction.EstimatedCost * 1_000_000)
	if micro <= 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_ = service.spend.AddSpend(ctx, accountID, quotaPeriodBucket(), micro)
}

// appendLLMInteractionWithSpend 是 appendLLMInteraction 的「带账户记账」变体：在同一 choke point 把会话级成本表
// （accumulateLLMMetrics，内嵌于 appendLLMInteraction）与账户级配额（recordLLMSpendBestEffort）同口径累加。
// 用于执行主循环内全部「真实花了钱」的 LLM 站点（combat_shake/reflection/social/romance/intelligence/interaction_actions/
// narrative/pigeon/random_events/command_intent/legacy_hall 等），避免账户级配额相对会话级成本系统性低估（仅主决策入账）。
// 纪律：recordLLMSpendBestEffort 自身 best-effort（未注入记账器/账户空/成本<=0/CacheHit 归零 → no-op），故对默认链路零行为变化。
func (service *Service) appendLLMInteractionWithSpend(ctx context.Context, state *State, interaction LLMInteraction) {
	appendLLMInteraction(state, interaction)
	service.recordLLMSpendBestEffort(ctx, state, interaction)
}

// accountOverQuota 判当前会话所属账户是否已超 LLM 成本配额（true=超额，应阻断后续 LLM 调用）。
// 纪律：未注入记账器 / 账户为空 → 不阻断（false）；CheckQuota 出错按 best-effort 放行（false，不误伤）。
func (service *Service) accountOverQuota(ctx context.Context, state State) bool {
	if service == nil || service.spend == nil {
		return false
	}
	accountID := strings.TrimSpace(state.AccountID)
	if accountID == "" {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	allowed, err := service.spend.CheckQuota(ctx, accountID)
	if err != nil {
		return false
	}
	return !allowed
}

// llmBlocked 组合「会话级预算护栏」与「账户级配额超额」：任一命中即阻断本次 LLM 调用，走兜底路径。
// 用于把 llm.go 各 GenerateJSON 站点的会话级护栏判定升级为「会话级 || 账户级」，复用既有 budgetGuardrail* 兜底，零新增降级逻辑。
func (service *Service) llmBlocked(ctx context.Context, state State) bool {
	return llmBudgetGuardrailActive(state) || service.accountOverQuota(ctx, state)
}

// NewServiceWithColdStore 初始化会话服务，统一挂接状态仓库、单位仓库和状态变更器。
func NewService(db *sql.DB, llm completionClient) *Service {
	return NewServiceWithColdStore(db, llm, nil)
}

// NewServiceWithColdStore 初始化会话服务，并挂接主库/冷存储与核心仓库对象。
func NewServiceWithColdStore(db *sql.DB, llm completionClient, coldStore *sql.DB) *Service {
	units := unit.NewRepository(db)
	return &Service{
		db:                db,
		coldStore:         coldStore,
		sessions:          NewRepository(db),
		units:             units,
		mutator:           status.NewMutator(db, units),
		llm:               llm,
		memoryRefreshTurn: map[string]int{},
		memoryRecallTurn:  map[string]int{},
	}
}

// Broadcaster 向某会话的所有订阅客户端推送实时事件（*ws.Hub 结构上即满足，无需 session 依赖 ws）。
type Broadcaster interface {
	BroadcastSessionEvent(sessionID string, eventType string, payload any) int
}

// SetReflexShortCircuit 开启/关闭反射真短路（降本，main/router 按 QUNXIANG_REFLEX_SHORTCIRCUIT 注入）。
func (service *Service) SetReflexShortCircuit(enabled bool) {
	if service == nil {
		return
	}
	service.reflexShortCircuit = enabled
}

// SetBroadcaster 注册实时广播器（用于命运收件箱/回响的 WS 实时推送）。
func (service *Service) SetBroadcaster(b Broadcaster) {
	if service == nil {
		return
	}
	service.broadcaster = b
}

// pushRealtime 向会话订阅者推送一条实时事件（best-effort，无广播器则静默跳过）。
func (service *Service) pushRealtime(sessionID string, eventType string, payload any) {
	if service == nil || service.broadcaster == nil || sessionID == "" {
		return
	}
	service.broadcaster.BroadcastSessionEvent(sessionID, eventType, payload)
}

// SetProgressReporter 注册进度回调，用于向前端推送增量快照事件。
func (service *Service) SetProgressReporter(
	reporter func(reason string, snapshot Snapshot, extra map[string]any),
) {
	if service == nil {
		return
	}
	service.progressReporter = reporter
}

// SetAsyncExecution 配置执行阶段是否在后台异步运行。
func (service *Service) SetAsyncExecution(enabled bool) {
	if service == nil {
		return
	}
	service.asyncExecution = enabled
}

// CreateSinglePlayer 创建单人对局（默认地图脚本）。
func (service *Service) CreateSinglePlayer(ctx context.Context, seed int64) (Snapshot, error) {
	return service.CreateSinglePlayerWithMapScript(ctx, seed, "")
}

// CreateSinglePlayerWithMapScript 创建单人对局并初始化地图、天气、单位与回合状态。
func (service *Service) CreateSinglePlayerWithMapScript(ctx context.Context, seed int64, mapScriptID string) (Snapshot, error) {
	return service.createSinglePlayerWithMapScript(ctx, seed, mapScriptID, BattlefieldSizeSmall, false, 3, ModeSinglePlayer, false, true, "", false)
}

// CreateSinglePlayerDraftWithMapScript 创建带开局选人阶段的单人对局。
func (service *Service) CreateSinglePlayerDraftWithMapScript(ctx context.Context, seed int64, mapScriptID string) (Snapshot, error) {
	return service.CreateSinglePlayerDraftWithMapScriptAndUnitCount(ctx, seed, mapScriptID, openingRosterSize)
}

// CreateSinglePlayerDraftWithMapScriptAndUnitCount 创建带开局选人阶段的单人对局，并指定每方单位数。
func (service *Service) CreateSinglePlayerDraftWithMapScriptAndUnitCount(ctx context.Context, seed int64, mapScriptID string, unitCount int) (Snapshot, error) {
	return service.CreateSinglePlayerDraftWithMapScriptSizeAndUnitCount(ctx, seed, mapScriptID, BattlefieldSizeSmall, unitCount)
}

// CreateSinglePlayerDraftWithMapScriptSizeAndUnitCount 创建带开局选人阶段的单人对局，并指定地图尺寸和每方单位数。
func (service *Service) CreateSinglePlayerDraftWithMapScriptSizeAndUnitCount(ctx context.Context, seed int64, mapScriptID string, mapSizeID string, unitCount int) (Snapshot, error) {
	return service.CreateSinglePlayerDraftWithMapScriptSizeUnitCountAndFog(ctx, seed, mapScriptID, mapSizeID, unitCount, false)
}

// CreateSinglePlayerDraftWithMapScriptSizeUnitCountAndFog 创建带开局选人阶段的单人对局，并指定迷雾开关。
func (service *Service) CreateSinglePlayerDraftWithMapScriptSizeUnitCountAndFog(ctx context.Context, seed int64, mapScriptID string, mapSizeID string, unitCount int, fogOfWarEnabled bool) (Snapshot, error) {
	return service.CreateSinglePlayerDraftWithMapScriptSizeUnitCountFogAndRandomEvents(ctx, seed, mapScriptID, mapSizeID, unitCount, fogOfWarEnabled, true)
}

// CreateSinglePlayerDraftWithMapScriptSizeUnitCountFogAndRandomEvents 创建带开局选人阶段的单人对局，并指定迷雾与随机事件开关。
// 向后兼容入口：不带账户归属（匿名局），等价于账户级配额对其 no-op。
func (service *Service) CreateSinglePlayerDraftWithMapScriptSizeUnitCountFogAndRandomEvents(ctx context.Context, seed int64, mapScriptID string, mapSizeID string, unitCount int, fogOfWarEnabled bool, randomEventsEnabled bool) (Snapshot, error) {
	return service.CreateSinglePlayerDraftWithMapScriptSizeUnitCountFogRandomEventsAndAccount(ctx, seed, mapScriptID, mapSizeID, unitCount, fogOfWarEnabled, randomEventsEnabled, "", false)
}

// CreateSinglePlayerDraftWithMapScriptSizeUnitCountFogRandomEventsAndAccount 同上，但携带账户归属（accountID 空=匿名局，安全）
// 与未成年模式 minorMode（由 compliance.Gate 裁定、落 State.MinorMode；开启则关闭恋爱·生育、降露骨暴力叙事；匿名/成年局传 false）。
// router 软解析鉴权账户后调用本入口，把 accountID 写入 State.AccountID，供 LLM 成本闭环按账户记账与配额拦截。
func (service *Service) CreateSinglePlayerDraftWithMapScriptSizeUnitCountFogRandomEventsAndAccount(ctx context.Context, seed int64, mapScriptID string, mapSizeID string, unitCount int, fogOfWarEnabled bool, randomEventsEnabled bool, accountID string, minorMode bool) (Snapshot, error) {
	return service.createSinglePlayerWithMapScript(ctx, seed, mapScriptID, mapSizeID, true, unitCount, ModeSinglePlayer, fogOfWarEnabled, randomEventsEnabled, accountID, minorMode)
}

// CreateDuelWithMapScriptAndUnitCount 创建双人对局，并由房主指定每方单位数。
func (service *Service) CreateDuelWithMapScriptAndUnitCount(ctx context.Context, seed int64, mapScriptID string, unitCount int) (Snapshot, error) {
	return service.CreateDuelWithMapScriptSizeAndUnitCount(ctx, seed, mapScriptID, BattlefieldSizeSmall, unitCount)
}

// CreateDuelWithMapScriptSizeAndUnitCount 创建双人对局，并由房主指定地图尺寸和每方单位数。
func (service *Service) CreateDuelWithMapScriptSizeAndUnitCount(ctx context.Context, seed int64, mapScriptID string, mapSizeID string, unitCount int) (Snapshot, error) {
	return service.CreateDuelWithMapScriptSizeUnitCountAndFog(ctx, seed, mapScriptID, mapSizeID, unitCount, false)
}

// CreateDuelWithMapScriptSizeUnitCountAndFog 创建双人对局，并由房主指定地图、人数和迷雾开关。
func (service *Service) CreateDuelWithMapScriptSizeUnitCountAndFog(ctx context.Context, seed int64, mapScriptID string, mapSizeID string, unitCount int, fogOfWarEnabled bool) (Snapshot, error) {
	return service.CreateDuelWithMapScriptSizeUnitCountFogAndRandomEvents(ctx, seed, mapScriptID, mapSizeID, unitCount, fogOfWarEnabled, true)
}

// CreateDuelWithMapScriptSizeUnitCountFogAndRandomEvents 创建双人对局，并由房主指定地图、人数、迷雾和随机事件开关。
func (service *Service) CreateDuelWithMapScriptSizeUnitCountFogAndRandomEvents(ctx context.Context, seed int64, mapScriptID string, mapSizeID string, unitCount int, fogOfWarEnabled bool, randomEventsEnabled bool) (Snapshot, error) {
	return service.CreateDuelWithMapScriptSizeUnitCountFogRandomEventsAndAccount(ctx, seed, mapScriptID, mapSizeID, unitCount, fogOfWarEnabled, randomEventsEnabled, "", false)
}

// CreateDuelWithMapScriptSizeUnitCountFogRandomEventsAndAccount 同上，并把房主账户 ID 贯穿到 State.AccountID，
// 用于双人对局房主侧的 LLM 成本归账 / 配额拦截（与单人账户入口同口径）。accountID 为空 → 匿名局（账户级记账/配额 no-op）。
// minorMode 为房主侧未成年裁定（落 State.MinorMode）。旧同名方法委托本入口传 ""/false 保持向后兼容。
func (service *Service) CreateDuelWithMapScriptSizeUnitCountFogRandomEventsAndAccount(ctx context.Context, seed int64, mapScriptID string, mapSizeID string, unitCount int, fogOfWarEnabled bool, randomEventsEnabled bool, accountID string, minorMode bool) (Snapshot, error) {
	return service.createSinglePlayerWithMapScript(ctx, seed, mapScriptID, mapSizeID, false, unitCount, ModeDuel, fogOfWarEnabled, randomEventsEnabled, accountID, minorMode)
}

func (service *Service) createSinglePlayerWithMapScript(ctx context.Context, seed int64, mapScriptID string, mapSizeID string, draftMode bool, unitCount int, mode string, fogOfWarEnabled bool, randomEventsEnabled bool, accountID string, minorMode bool) (Snapshot, error) {
	if seed == 0 {
		seed = time.Now().UTC().UnixNano()
	}
	if strings.TrimSpace(mode) == "" {
		mode = ModeSinglePlayer
	}
	if draftMode || unitCount != 3 {
		unitCount = NormalizeOpeningUnitCount(unitCount)
	}
	if err := events.SeedReasonCodeCatalog(ctx, service.db); err != nil {
		return Snapshot{}, err
	}

	now := time.Now().UTC()
	sessionID := uuid.NewString()
	selectedMapScriptID := normalizeBattlefieldScriptID(mapScriptID, seed)
	selectedMapScriptName := battlefieldScriptDisplayName(selectedMapScriptID)
	selectedMapSize := battlefieldSizeByID(mapSizeID)
	state := State{
		ID:                   sessionID,
		AccountID:            strings.TrimSpace(accountID), // 本局账户归属（匿名/单机局为空，账户级成本配额对其 no-op）；随 state_json 落库
		MinorMode:            minorMode,                    // 未成年模式（compliance.Gate 裁定）：随 state_json 落库，关闭恋爱·生育、降露骨暴力
		Mode:                 mode,
		RandomSeed:           seed,
		PlayerFactionID:      "player",
		EnemyFactionID:       "enemy",
		SetupPhase:           SetupPhaseReady,
		DraftRequiredPick:    unitCount,
		TurnState:            turns.NewState(now, turns.DefaultBudgets()),
		Outcome:              OutcomeOngoing,
		VictoryPath:          VictoryPathNone,
		Weather:              weatherForTurnBySeed(seed, 1),
		Map:                  generateBattlefieldWithSize(sessionID, seed, selectedMapScriptID, selectedMapSize.ID),
		MapScriptID:          selectedMapScriptID,
		MapScriptName:        selectedMapScriptName,
		MapSizeID:            selectedMapSize.ID,
		MapSizeName:          selectedMapSize.DisplayName,
		FogOfWarEnabled:      fogOfWarEnabled,
		RandomEventsDisabled: !randomEventsEnabled,
		CommandPower:         defaultCommandPower(),
		FactionRelations:     []FactionRelation{},
		Structures:           []Structure{},
		DirectiveHistory:     []Directive{},
		DialogueHistory:      []DialogueMessage{},
		DecisionTraces:       []DecisionTrace{},
		LLMInteractions:      []LLMInteraction{},
		PigeonQueue:          []PigeonDispatch{},
		BattleReports:        []BattleReport{},
		IntelAssets:          []IntelAsset{},
		IntelReports:         []IntelReport{},
		ModerationReports:    []ModerationReport{},
		Metrics:              SessionMetrics{},
		RawEventLog:          []RawEventEntry{},
		Logs:                 []LogEntry{},
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	if draftMode {
		state.SetupPhase = SetupPhaseDrafting
	}
	narrativeRefreshTargets := make([]string, 0, openingRosterSize*2)

	if draftMode {
		playerCandidates, enemyCandidates := service.openingCandidatesForDraft(ctx, seed)
		state.PlayerDraftPool = draftRecordsFromCandidates(seed+1, sessionID, state.PlayerFactionID, playerCandidates)
		state.EnemyDraftPool = draftRecordsFromCandidates(seed+101, sessionID, state.EnemyFactionID, selectedOpeningRosterWithLimit(enemyCandidates, seed+2026, unitCount))
		repositionRecordsForMap(state.PlayerDraftPool, state.PlayerFactionID, state.Map)
		repositionRecordsForMap(state.EnemyDraftPool, state.EnemyFactionID, state.Map)
		state.SetupDeadlineAt = time.Now().Add(openingDraftWaitDuration)
	} else if unitCount != 3 {
		playerCandidates, enemyCandidates := service.openingCandidatesForDraft(ctx, seed)
		playerRecords := draftRecordsFromCandidates(seed+1, sessionID, state.PlayerFactionID, selectedOpeningRosterWithLimit(playerCandidates, seed, unitCount))
		enemyRecords := draftRecordsFromCandidates(seed+101, sessionID, state.EnemyFactionID, selectedOpeningRosterWithLimit(enemyCandidates, seed+2026, unitCount))
		repositionRecordsForMap(playerRecords, state.PlayerFactionID, state.Map)
		repositionRecordsForMap(enemyRecords, state.EnemyFactionID, state.Map)

		// 先在切片元素上原地补给 + 模板兜底叙事，再批量身份叙事补全（一次去重并发），最后逐个落库。
		// 这样落库的单位直接带真叙事，避免「先存模板再异步补」的视觉延迟。批量补全 best-effort、缓存命中即跳过 LLM。
		identityTargets := make([]*unit.Record, 0, len(playerRecords)+len(enemyRecords))
		for index := range playerRecords {
			if err := addOpeningSupply(&playerRecords[index], index); err != nil {
				return Snapshot{}, err
			}
			appendOpeningSupplyLog(&state, playerRecords[index])
			service.primeRecordNarrative(&playerRecords[index])
			identityTargets = append(identityTargets, &playerRecords[index])
		}
		for index := range enemyRecords {
			if err := addOpeningSupply(&enemyRecords[index], index); err != nil {
				return Snapshot{}, err
			}
			appendOpeningSupplyLog(&state, enemyRecords[index])
			service.primeRecordNarrative(&enemyRecords[index])
			identityTargets = append(identityTargets, &enemyRecords[index])
		}
		service.EnrichUnitIdentityNarrativesBestEffort(ctx, &state, identityTargets)
		for index := range playerRecords {
			if err := service.units.Save(ctx, playerRecords[index]); err != nil {
				return Snapshot{}, err
			}
			state.PlayerUnitIDs = append(state.PlayerUnitIDs, playerRecords[index].ID)
		}
		for index := range enemyRecords {
			if err := service.units.Save(ctx, enemyRecords[index]); err != nil {
				return Snapshot{}, err
			}
			state.EnemyUnitIDs = append(state.EnemyUnitIDs, enemyRecords[index].ID)
		}
		narrativeRefreshTargets = append(narrativeRefreshTargets, state.PlayerUnitIDs...)
		narrativeRefreshTargets = append(narrativeRefreshTargets, state.EnemyUnitIDs...)
	} else {
		playerSpawns := []world.Coord{{Q: 1, R: 2}, {Q: 1, R: 4}, {Q: 2, R: 3}}
		playerNames := []string{"惊蛰", "行舟", "折棠"}
		playerSupplies := [][]string{{"ration"}, {"carrier_pigeon"}, {"rope"}}
		playerRecords := make([]unit.Record, 0, len(playerSpawns))
		for index, spawn := range playerSpawns {
			record, err := bootstrapBattleUnit(seed+int64(index)+1, sessionID, state.PlayerFactionID, playerNames[index], spawn)
			if err != nil {
				return Snapshot{}, err
			}
			for _, itemID := range playerSupplies[index] {
				if err := unit.AddBackpackItem(&record, itemID, 1); err != nil {
					return Snapshot{}, err
				}
			}
			appendOpeningSupplyLog(&state, record)
			playerRecords = append(playerRecords, record)
		}

		enemySpawns := []world.Coord{{Q: 5, R: 2}, {Q: 5, R: 4}, {Q: 4, R: 3}}
		enemyNames := []string{"灰狼前锋", "断桥游兵", "黑镰斥候"}
		enemySupplies := [][]string{{"herb_bundle"}, {"ration"}, {"pickaxe"}}
		enemyRecords := make([]unit.Record, 0, len(enemySpawns))
		for index, spawn := range enemySpawns {
			record, err := bootstrapBattleUnit(seed+int64(index)+101, sessionID, state.EnemyFactionID, enemyNames[index], spawn)
			if err != nil {
				return Snapshot{}, err
			}
			for _, itemID := range enemySupplies[index] {
				if err := unit.AddBackpackItem(&record, itemID, 1); err != nil {
					return Snapshot{}, err
				}
			}
			appendOpeningSupplyLog(&state, record)
			enemyRecords = append(enemyRecords, record)
		}

		// 默认 3v3：同样在落库前批量身份叙事补全（best-effort、缓存命中跳过 LLM），让单位直接带真叙事。
		identityTargets := make([]*unit.Record, 0, len(playerRecords)+len(enemyRecords))
		for index := range playerRecords {
			service.primeRecordNarrative(&playerRecords[index])
			identityTargets = append(identityTargets, &playerRecords[index])
		}
		for index := range enemyRecords {
			service.primeRecordNarrative(&enemyRecords[index])
			identityTargets = append(identityTargets, &enemyRecords[index])
		}
		service.EnrichUnitIdentityNarrativesBestEffort(ctx, &state, identityTargets)
		for index := range playerRecords {
			if err := service.units.Save(ctx, playerRecords[index]); err != nil {
				return Snapshot{}, err
			}
			state.PlayerUnitIDs = append(state.PlayerUnitIDs, playerRecords[index].ID)
		}
		for index := range enemyRecords {
			if err := service.units.Save(ctx, enemyRecords[index]); err != nil {
				return Snapshot{}, err
			}
			state.EnemyUnitIDs = append(state.EnemyUnitIDs, enemyRecords[index].ID)
		}
		narrativeRefreshTargets = append(narrativeRefreshTargets, state.PlayerUnitIDs...)
		narrativeRefreshTargets = append(narrativeRefreshTargets, state.EnemyUnitIDs...)
	}

	appendDirective(&state, Directive{
		ID:        uuid.NewString(),
		Turn:      1,
		Phase:     turns.PhaseDeployment,
		Kind:      DirectiveKindDoctrine,
		Text:      "稳住阵型，优先保全队伍，再寻找敌方破绽逐步推进。",
		Priority:  "normal",
		IssuedAt:  now,
		IssuedBy:  "player",
		AppliesTo: state.PlayerFactionID,
	})
	appendLog(
		&state,
		"setup",
		setupLogMessage(draftMode, selectedMapScriptName, unitCount),
		"",
		"",
	)
	appendLog(&state, "weather", fmt.Sprintf("本回合天气：%s。%s", state.Weather.DisplayName, state.Weather.Note), "", "")
	ensureFactionRelations(&state)

	// 把本局接入世界（QUNXIANG_WORLD_BINDING：默认 shared 共享主世界，点亮 worldbus/cross-event/七交互/world-boss 整层；
	// best-effort 不阻断建局）。必须在玩家单位已落库之后、seedAmbientForUnits/seedVillageForSession 之前——使 state.WorldID 在 ambient/村庄锚落库时已置位。
	_ = service.bindSessionWorld(ctx, &state)

	// 把玩家单位 seed 进大世界离线调度（M7.3-real-4b，开关关时 no-op）。best-effort：绝不因调度登记失败拖垮建局。
	// draft 模式此处 PlayerUnitIDs 尚空（单位在 ApplyOpeningDraft 才落库），故对 draft 自然 no-op、由组队完成时再 seed。
	// 传 state.WorldID（未接多世界时恒空 → region_id 回退 sessionID；接入后才升级世界子区分片）。
	_ = service.seedAmbientForUnits(ctx, sessionID, state.WorldID, state.PlayerUnitIDs)

	// 在主战局兑现命运开盒「她身边已有二十个有名有姓的人」承诺（QUNXIANG_MAIN_VILLAGE 关时 no-op，best-effort）。
	// 仅非 draft 路径在此织村庄：draft 模式此处玩家单位尚未落库，由 ApplyOpeningDraft 组队完成时再 seed，避免漏播/重复。
	if !draftMode {
		service.seedVillageForSession(ctx, &state)
	}

	if err := service.syncCombatFlags(ctx, &state, nil); err != nil {
		return Snapshot{}, err
	}
	if err := service.sessions.Save(ctx, &state); err != nil {
		return Snapshot{}, err
	}
	if !draftMode {
		loadedState, units, err := service.loadSession(ctx, sessionID)
		if err != nil {
			return Snapshot{}, err
		}
		state = loadedState
		service.refreshEnemyGlobalDirectiveForDeploymentPhase(ctx, &state, units, "deployment_phase_started")
		if err := service.sessions.Save(ctx, &state); err != nil {
			return Snapshot{}, err
		}
	}
	if err := service.recordPhaseBoundarySnapshot(ctx, &state, nil); err != nil {
		return Snapshot{}, err
	}
	service.refreshHallMemoriesAsync(sessionID, narrativeRefreshTargets)
	service.refreshUnitNarrativesAsync(sessionID, narrativeRefreshTargets)

	return service.GetSnapshot(ctx, sessionID)
}

func setupLogMessage(draftMode bool, selectedMapScriptName string, unitCount int) string {
	if draftMode {
		return fmt.Sprintf("开局组队阶段开始。LLM 已生成候选单位；你有 60 秒选择并改写 %d 人的名字、生平和性格。当前地图剧本：%s。", unitCount, selectedMapScriptName)
	}
	return fmt.Sprintf("第 1 回合部署阶段开始。当前地图剧本：%s。玩家可以发布自然语言方针、点名对话或下达部署任务；吃饭、交易、采集、建造与战斗都由 AI 单位自己判断并执行。", selectedMapScriptName)
}

// GetSnapshot 读取会话最新状态并组装对外快照。
func (service *Service) GetSnapshot(ctx context.Context, sessionID string) (Snapshot, error) {
	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	if service.resumeStaleAsyncExecutionIfNeeded(ctx, &state) {
		return buildSnapshot(state, units), nil
	}
	service.maybeRefreshDraftNarrativesAsync(state)
	return buildSnapshot(state, units), nil
}

// SetGlobalDirective 写入玩家全局方针并更新方针历史。
func (service *Service) SetGlobalDirective(ctx context.Context, sessionID string, text string) (Snapshot, error) {
	return service.SetPlayerDirective(ctx, sessionID, DirectiveKindDoctrine, "", text)
}

// ApplyOpeningDraft 确认开局名单；超时或空选择时自动取候选池前 N 人。
func (service *Service) ApplyOpeningDraft(ctx context.Context, sessionID string, selected []unit.Record) (Snapshot, error) {
	state, _, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	if state.SetupPhase != SetupPhaseDrafting {
		return service.GetSnapshot(ctx, sessionID)
	}
	if len(state.PlayerDraftPool) == 0 {
		return Snapshot{}, fmt.Errorf("opening draft pool is empty")
	}

	poolByID := make(map[string]unit.Record, len(state.PlayerDraftPool))
	for _, record := range state.PlayerDraftPool {
		poolByID[record.ID] = record
	}
	requiredPick := NormalizeOpeningUnitCount(state.DraftRequiredPick)
	picked := make([]unit.Record, 0, requiredPick)
	seen := map[string]struct{}{}
	for _, edited := range selected {
		if len(picked) >= requiredPick {
			break
		}
		base, ok := poolByID[edited.ID]
		if !ok {
			continue
		}
		if _, exists := seen[base.ID]; exists {
			continue
		}
		applyCandidateToRecord(&base, candidateFromRecord(edited))
		picked = append(picked, base)
		seen[base.ID] = struct{}{}
	}
	for _, base := range state.PlayerDraftPool {
		if len(picked) >= requiredPick {
			break
		}
		if _, exists := seen[base.ID]; exists {
			continue
		}
		picked = append(picked, base)
		seen[base.ID] = struct{}{}
	}
	if len(picked) != requiredPick {
		return Snapshot{}, fmt.Errorf("need %d units, got %d", requiredPick, len(picked))
	}

	state.PlayerUnitIDs = []string{}
	state.EnemyUnitIDs = []string{}
	// 捏人确认路径：组装玩家选中 + 敌方阵容的可寻址副本，补给后批量身份叙事补全（best-effort、缓存命中跳过 LLM），
	// 再逐个落库——让玩家亲手捏的角色与敌方一上场即带真传记/招募词，避免「先模板后异步补」的视觉延迟。
	playerSave := make([]unit.Record, 0, len(picked))
	enemySave := make([]unit.Record, 0, requiredPick)
	identityTargets := make([]*unit.Record, 0, len(picked)+requiredPick)
	for index := range picked {
		record := picked[index]
		if err := addOpeningSupply(&record, index); err != nil {
			return Snapshot{}, err
		}
		appendOpeningSupplyLog(&state, record)
		playerSave = append(playerSave, record)
	}
	for index := 0; index < len(state.EnemyDraftPool) && index < requiredPick; index++ {
		record := state.EnemyDraftPool[index]
		if err := addOpeningSupply(&record, index); err != nil {
			return Snapshot{}, err
		}
		appendOpeningSupplyLog(&state, record)
		enemySave = append(enemySave, record)
	}
	for index := range playerSave {
		identityTargets = append(identityTargets, &playerSave[index])
	}
	for index := range enemySave {
		identityTargets = append(identityTargets, &enemySave[index])
	}
	service.EnrichUnitIdentityNarrativesBestEffort(ctx, &state, identityTargets)
	for index := range playerSave {
		if err := service.units.Save(ctx, playerSave[index]); err != nil {
			return Snapshot{}, err
		}
		state.PlayerUnitIDs = append(state.PlayerUnitIDs, playerSave[index].ID)
	}
	for index := range enemySave {
		if err := service.units.Save(ctx, enemySave[index]); err != nil {
			return Snapshot{}, err
		}
		state.EnemyUnitIDs = append(state.EnemyUnitIDs, enemySave[index].ID)
	}
	state.PlayerDraftPool = []unit.Record{}
	state.EnemyDraftPool = []unit.Record{}
	state.SetupPhase = SetupPhaseReady
	state.SetupDeadlineAt = time.Time{}
	appendLog(&state, "setup", fmt.Sprintf("开局组队完成：玩家选择了 %d 名单位。第 1 回合部署阶段开始。", len(state.PlayerUnitIDs)), "", "")
	ensureFactionRelations(&state)
	// 组队完成、玩家单位刚落库 → 先接入世界（默认 shared，点亮跨玩家整层；best-effort），再 seed 离线调度。
	_ = service.bindSessionWorld(ctx, &state)
	// 组队完成、玩家单位刚落库 → seed 进大世界离线调度（M7.3-real-4b，开关关时 no-op；best-effort）。
	// 传 state.WorldID（未接多世界时恒空 → region_id 回退 sessionID；接入后才升级世界子区分片）。
	_ = service.seedAmbientForUnits(ctx, sessionID, state.WorldID, state.PlayerUnitIDs)
	// 同步在主战局兑现命运开盒「她身边已有二十个有名有姓的人」承诺（QUNXIANG_MAIN_VILLAGE 关时 no-op，best-effort）。
	// draft 路径的村庄播种唯一落点——createSinglePlayerWithMapScript 在 draft 模式跳过，此处补齐，避免漏播/重复。
	service.seedVillageForSession(ctx, &state)
	if err := service.syncCombatFlags(ctx, &state, nil); err != nil {
		return Snapshot{}, err
	}
	if err := service.sessions.Save(ctx, &state); err != nil {
		return Snapshot{}, err
	}
	loadedState, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	state = loadedState
	ensureFallbackEnemyGlobalDirectiveForDeploymentPhase(&state, units)
	if err := service.sessions.Save(ctx, &state); err != nil {
		return Snapshot{}, err
	}
	if err := service.recordPhaseBoundarySnapshot(ctx, &state, nil); err != nil {
		return Snapshot{}, err
	}
	targets := append([]string{}, state.PlayerUnitIDs...)
	targets = append(targets, state.EnemyUnitIDs...)
	service.refreshHallMemoriesAsync(sessionID, targets)
	service.refreshUnitNarrativesAsync(sessionID, targets)
	return service.GetSnapshot(ctx, sessionID)
}

// TalkToUnit 处理玩家与单位对话，调用 LLM 生成回复并落日志/记忆。
func (service *Service) TalkToUnit(ctx context.Context, sessionID string, unitID string, message string) (Snapshot, DialogueMessage, error) {
	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return Snapshot{}, DialogueMessage{}, err
	}

	message = strings.TrimSpace(message)
	if message == "" {
		return Snapshot{}, DialogueMessage{}, fmt.Errorf("message is required")
	}
	if state.Outcome != OutcomeOngoing {
		return Snapshot{}, DialogueMessage{}, fmt.Errorf("session is already finished")
	}
	if state.TurnState.Phase == turns.PhaseExecution {
		return Snapshot{}, DialogueMessage{}, fmt.Errorf("unit dialogue is only available during deployment, not execution")
	}
	byID := mapRecordsByID(units)
	record, ok := byID[unitID]
	if !ok {
		return Snapshot{}, DialogueMessage{}, fmt.Errorf("unit %s was not found", unitID)
	}
	if record.Status.LifeState == unit.LifeStateDead {
		return Snapshot{}, DialogueMessage{}, fmt.Errorf("dead units cannot respond")
	}

	playerLine := DialogueMessage{
		ID:         uuid.NewString(),
		UnitID:     unitID,
		Speaker:    "player",
		Message:    message,
		Turn:       state.TurnState.Turn,
		Phase:      state.TurnState.Phase,
		OccurredAt: time.Now().UTC(),
	}
	appendDialogue(&state, playerLine)

	replyPayload, result, interaction, err := service.generateDialogueReply(ctx, state, *record, message, byID)
	service.appendLLMInteractionWithSpend(ctx, &state, interaction)
	if err != nil {
		appendLog(&state, "dialogue_error", "我这回合没接上话。", record.ID, "")
		replyPayload = fallbackDialogueReplyPayload(*record, message)
		result.Provider = firstNonEmptyText(strings.TrimSpace(result.Provider), "local_fallback")
		result.Model = firstNonEmptyText(strings.TrimSpace(result.Model), "dialogue_rule")
		result.UsedFallback = true
	}

	reply := DialogueMessage{
		ID:           uuid.NewString(),
		UnitID:       unitID,
		Speaker:      record.DisplayName(),
		Message:      replyPayload.Reply,
		Turn:         state.TurnState.Turn,
		Phase:        state.TurnState.Phase,
		OccurredAt:   time.Now().UTC(),
		Provider:     result.Provider,
		Model:        result.Model,
		UsedFallback: result.UsedFallback,
	}
	appendDialogue(&state, reply)
	appendLog(&state, "dialogue", fmt.Sprintf("%s：%s", record.DisplayName(), reply.Message), record.ID, "")
	if err := service.rememberUnit(
		ctx,
		record,
		state.TurnState.Turn,
		replyPayload.Memory,
	); err != nil {
		return Snapshot{}, DialogueMessage{}, err
	}

	if err := service.sessions.Save(ctx, &state); err != nil {
		return Snapshot{}, DialogueMessage{}, err
	}

	snapshot := buildSnapshot(state, units)
	return snapshot, reply, nil
}

// fallbackDialogueReplyPayload 在 LLM 不可用时生成单位兜底回复，避免对话只有玩家发言。
func fallbackDialogueReplyPayload(record unit.Record, playerMessage string) dialogueReplyPayload {
	reply := fmt.Sprintf("%s：收到。我会按当前局势自己判断。", record.DisplayName())
	memory := strings.TrimSpace(playerMessage)
	if memory != "" {
		memory = "我记下：" + memory
	}
	memory = limitTextRunes(firstNonEmptyText(memory, "我记下了玩家指令。"), 18)
	return dialogueReplyPayload{
		Reply:  reply,
		Mood:   "steady",
		Intent: "acknowledge",
		Memory: memory,
	}
}

// AdvancePhase 负责单局状态机推进，并在阶段切换时触发对应的 AI 自主流程。
func (service *Service) AdvancePhase(ctx context.Context, sessionID string) (Snapshot, error) {
	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	if state.SetupPhase == SetupPhaseDrafting {
		if _, draftErr := service.ApplyOpeningDraft(ctx, sessionID, nil); draftErr != nil {
			return buildSnapshot(state, units), draftErr
		}
		state, units, err = service.loadSession(ctx, sessionID)
		if err != nil {
			return Snapshot{}, err
		}
	}

	if state.Outcome != OutcomeOngoing && state.TurnState.Phase == turns.PhaseExecution {
		return buildSnapshot(state, units), nil
	}

	now := time.Now().UTC()
	startAsyncExecution := false
	switch state.TurnState.Phase {
	case turns.PhaseDeployment:
		if !service.asyncExecution {
			service.refreshEnemyGlobalDirectiveForDeploymentPhase(ctx, &state, units, "deployment_phase_advanced")
			if err := service.resolvePigeonDispatches(ctx, &state, units); err != nil {
				if saveErr := service.sessions.Save(ctx, &state); saveErr != nil {
					return Snapshot{}, saveErr
				}
				return buildSnapshot(state, units), err
			}
		}
		state.TurnState.Advance(now)
		resetPhaseReady(&state)
		state.ExecutionInProgress = service.asyncExecution
		appendLog(&state, "phase", fmt.Sprintf("第 %d 回合执行阶段开始。所有单位会先消化你的自然语言方针与对话，再由 AI 自主处理进食、调拨、交换、生产、建造与战斗行动。", state.TurnState.Turn), "", "")
		if service.asyncExecution {
			startAsyncExecution = true
		} else if err := service.resolveExecution(ctx, &state, units); err != nil {
			return buildSnapshot(state, units), err
		}
	case turns.PhaseExecution:
		if state.ExecutionInProgress {
			return buildSnapshot(state, units), nil
		}
		if state.Outcome != OutcomeOngoing {
			return buildSnapshot(state, units), nil
		}
		state.TurnState.Advance(now)
		resetPhaseReady(&state)
		state.Weather = weatherForTurnBySeed(state.RandomSeed, state.TurnState.Turn)
		rechargeCommandPower(&state)
		if err := service.resolveDuePregnancies(ctx, &state); err != nil {
			return Snapshot{}, err
		}
		if err := service.resolvePigeonDeliveries(ctx, &state, units); err != nil {
			return Snapshot{}, err
		}
		appendLog(&state, "phase", fmt.Sprintf("第 %d 回合部署阶段开始。你可以更新新的全局方针、点名对话或下达部署任务。", state.TurnState.Turn), "", "")
		appendLog(&state, "weather", fmt.Sprintf("第 %d 回合天气：%s。%s", state.TurnState.Turn, state.Weather.DisplayName, state.Weather.Note), "", "")
		if err := service.applyTurnHungerUpkeep(ctx, &state, units); err != nil {
			return Snapshot{}, err
		}
		if err := service.resolveTurnRandomEvent(ctx, &state, units); err != nil {
			return Snapshot{}, err
		}
		if err := service.refreshSessionMemoryDecay(ctx, &state, units); err != nil {
			return Snapshot{}, err
		}
		// 自治长线结算（best-effort，绝不中断主链路）：在回合边界对每个单位
		//   ① 周期目标重估 reassessGoalIfDue（goal_reassess.go，cadence=24，未到期内部 no-op；无 LLM 时走确定性 fallback 落高显著度记忆）；
		//   ② 自然衰老人格漂移 ApplyPersonalityDrift(aging)（personality_drift.go，单次每维 ≤0.03、单日每维 ≤0.10，确定性 FNV，不直改受保护字段）。
		// 失败只吞错、不污染回合推进；与 hunger/memory-decay 同批，确定性可复现。
		service.settleAutonomyAtDeploymentBoundary(ctx, &state, units)
		service.settleConsentsAtBoundary(ctx, &state)                                          // consent 异步同意：超时兜底 expire + 宪章授权自治同意（best-effort）
		if n, err := service.degradeLayer3ConsentTimeoutsAtBoundary(ctx, &state); err != nil { // §6 层3 consent 超时降级：未应答→COMBAT_MAIMED 残废（绝不阵亡），best-effort
			appendLog(&state, "consent", fmt.Sprintf("层3 consent 超时降级扫描失败：%v", err), "", "")
		} else if n > 0 {
			appendLog(&state, "consent", fmt.Sprintf("%d 名重伤者久未等到你的回应，活了下来，却落下了一辈子的残。", n), "", "")
		}
		service.refreshThreats(ctx, &state, units)                            // 野外威胁刷新（默认 surface-only；QUNXIANG_AUTO_PVE 开时可升级开打，best-effort）
		service.surfaceCrossEventsAtBoundary(ctx, &state, units)              // 跨玩家事件投递（读出侧触发，best-effort，仅 WorldID 非空时生效）
		if n, err := service.ResolveDungeonTimeout(ctx, &state); err != nil { // 副本异步分段 charter 超时兜底（QUNXIANG_DUNGEON 默认关，best-effort；关时零行为）
			appendLog(&state, "dungeon", fmt.Sprintf("副本超时兜底失败：%v", err), "", "")
		} else if n > 0 {
			appendLog(&state, "dungeon", fmt.Sprintf("%d 处副本因你久未归来，依先前叮嘱见好就收了。", n), "", "")
		}
		if _, err := service.ScanAndWorldizeInbound(ctx, &state, units); err != nil { // 双向世界化入向探针扇出（QUNXIANG_WORLDIZE_INBOUND 默认关，best-effort；关时 no-op）
			appendLog(&state, "world", fmt.Sprintf("入向世界化扇出失败：%v", err), "", "")
		}
		if state.WorldID != "" { // 共享世界 Boss 自动刷新（QUNXIANG_WORLD_BOSS_AUTO 默认关，best-effort；函数内已二次 guard）
			if err := service.maybeRefreshWorldBoss(ctx, state.WorldID); err != nil {
				appendLog(&state, "world", fmt.Sprintf("世界Boss自动刷新失败：%v", err), "", "")
			}
		}
		service.scanAndMatch(ctx, &state, units)                    // 撮合自动扫描（QUNXIANG_AUTO_MATCH 默认关，低频确定性触发，best-effort）
		service.scanAndSocialize(ctx, &state, units)                // 社交自治扫描（QUNXIANG_AUTO_SOCIAL 默认开，低频确定性，best-effort，仅本会话单位对、WorldID 非空时生效）
		service.scanExclusiveContestsAtBoundary(ctx, &state, units) // 排他标的零和裁决（QUNXIANG_ZEROSUM_CONTEST 默认开，低频确定性，best-effort，先做联姻冲突）
		service.refreshEnemyGlobalDirectiveForDeploymentPhase(ctx, &state, units, "deployment_phase_started")
		appendSessionMetricsLog(&state)
		units = nil
	}

	if err := service.syncCombatFlags(ctx, &state, units); err != nil {
		return Snapshot{}, err
	}
	if err := service.sessions.Save(ctx, &state); err != nil {
		return Snapshot{}, err
	}
	if err := service.recordPhaseBoundarySnapshot(ctx, &state, units); err != nil {
		return Snapshot{}, err
	}
	if startAsyncExecution {
		service.launchAsyncExecution(state.ID)
	}

	return service.GetSnapshot(ctx, sessionID)
}

// settleAutonomyAtDeploymentBoundary 在 Execution→Deployment 回合边界对每个单位做自治长线结算：
//   - reassessGoalIfDue：周期目标重估（goal_reassess.go），cadence=24，未到期内部自动 no-op。
//   - ApplyPersonalityDrift(aging)：自然衰老人格漂移（personality_drift.go），步长/单日额度封顶，确定性。
//
// 全程 best-effort：单个单位失败只吞错、不影响其余单位与回合推进；与 hunger/memory-decay 同处边界批结算。
// 仅对活着的单位结算（衰老/目标重估对已退场单位无意义）。
func (service *Service) settleAutonomyAtDeploymentBoundary(ctx context.Context, state *State, units []unit.Record) {
	if service == nil || state == nil || len(units) == 0 {
		return
	}
	turn := state.TurnState.Turn
	for index := range units {
		record := &units[index]
		if record.ID == "" || record.Status.LifeState == unit.LifeStateDead {
			continue
		}
		// 周期目标重估（未到期内部 no-op；无 LLM 走确定性 fallback；落「高显著度记忆」）。
		_ = service.reassessGoalIfDue(ctx, state, record, turn)
		// 自然衰老人格漂移（确定性 FNV；步长 ≤0.03/维、单日 ≤0.10/维；经 PERSONALITY_DRIFT 流程事件留痕）。
		_, _ = service.ApplyPersonalityDrift(ctx, state.ID, record.ID, DriftReasonAging, turn)
		// 阵营道德轴漂移（F2：据本回合道德效价信号确定性漂移 MoralAlignment，best-effort）；
		// 漂移先把连击/道德轴算好，再判阵营切换（QUNXIANG_FACTION_SWITCH 默认关，满足隐藏条件才概率切→命运卡）。
		_, _ = service.settleMoralDrift(ctx, state.ID, record, turn)
		_ = service.maybeSwitchFaction(ctx, state.ID, record, turn)
	}
}

// GetUnitCharterForSession 读取某单位的离线宪章（HTTP 读端点用）。返回宪章 + 是否曾登记。
// 归属由调用方（router）按会话/鉴权校验后再调；本方法只按 sessionID 载入 state 后委托 GetUnitCharter。
func (service *Service) GetUnitCharterForSession(ctx context.Context, sessionID, unitID string) (OfflineCharter, bool, error) {
	if service == nil {
		return OfflineCharter{}, false, fmt.Errorf("session service is not available")
	}
	state, _, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return OfflineCharter{}, false, err
	}
	charter, ok := GetUnitCharter(&state, unitID)
	return charter, ok, nil
}

// SetUnitCharterForSession 写入/覆盖某单位的离线宪章（HTTP 写端点用），落库并写 CHARTER_ACTIVATED/CHARTER_UPDATED 留痕。
// charter 写入即经 NormalizeCharter（去空白、补稳定红线 ID），保证持久化往返一致与归因可引用。
// 归属由调用方（router）按会话/鉴权 + 单位归属本会话校验后再调。
func (service *Service) SetUnitCharterForSession(ctx context.Context, sessionID, unitID string, charter OfflineCharter) (OfflineCharter, error) {
	if service == nil {
		return OfflineCharter{}, fmt.Errorf("session service is not available")
	}
	if strings.TrimSpace(unitID) == "" {
		return OfflineCharter{}, fmt.Errorf("unit id is required")
	}
	state, _, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return OfflineCharter{}, err
	}
	_, existed := GetUnitCharter(&state, unitID)
	SetUnitCharter(&state, unitID, charter)
	stored, _ := GetUnitCharter(&state, unitID)
	if err := service.saveSessionMergingExternalEvents(ctx, &state); err != nil {
		return OfflineCharter{}, err
	}
	// 授权变更留痕（流程事件旁路，不改受保护字段）：首次设立=CHARTER_ACTIVATED，更新=CHARTER_UPDATED。best-effort。
	code := events.ReasonCharterActivated
	if existed {
		code = events.ReasonCharterUpdated
	}
	if service.db != nil {
		_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
			SessionID:     sessionID,
			OwnerUnitID:   unitID,
			RelatedUnitID: unitID,
			Code:          code,
			Category:      events.CategoryPlayer,
			Payload: map[string]any{
				"long_term_goals": len(stored.LongTermGoals),
				"redlines":        len(stored.Redlines),
				"social_mandates": len(stored.SocialMandates),
			},
		})
	}
	return stored, nil
}

// ClearUnitCharterForSession 撤销某单位的离线宪章（HTTP DELETE 用），落库并写 CHARTER_UPDATED 留痕（撤销视为更新）。
func (service *Service) ClearUnitCharterForSession(ctx context.Context, sessionID, unitID string) error {
	if service == nil {
		return fmt.Errorf("session service is not available")
	}
	state, _, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if _, existed := GetUnitCharter(&state, unitID); !existed {
		return nil // 本就没有宪章：no-op。
	}
	ClearUnitCharter(&state, unitID)
	if err := service.saveSessionMergingExternalEvents(ctx, &state); err != nil {
		return err
	}
	if service.db != nil {
		_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
			SessionID:     sessionID,
			OwnerUnitID:   unitID,
			RelatedUnitID: unitID,
			Code:          events.ReasonCharterUpdated,
			Category:      events.CategoryPlayer,
			Payload:       map[string]any{"cleared": true},
		})
	}
	return nil
}

// RecordPlayerInterventionWithDrift 在记录一次玩家接管/嘱咐（echo.go 的 RecordPlayerIntervention）之后，
// best-effort 对被接管单位施加一次「intervention」成因的人格漂移（personality_drift.go）——玩家的介入会潜移默化改变她。
// 漂移失败/读不到回合一律吞错，绝不影响接管事件本身的落库（接管是真实动作、漂移是旁路增益）。
// 返回接管事件 ID（与 RecordPlayerIntervention 一致），供前端关联回响。
func (service *Service) RecordPlayerInterventionWithDrift(ctx context.Context, sessionID, unitID, summary string) (string, error) {
	id, err := service.RecordPlayerIntervention(ctx, sessionID, unitID, summary)
	if err != nil {
		return id, err
	}
	// 取会话当前回合喂确定性漂移（载不到则用 0：仍确定性、无随机）。best-effort。
	turn := 0
	if service.sessions != nil {
		if state, getErr := service.sessions.Get(ctx, sessionID); getErr == nil {
			turn = state.TurnState.Turn
		}
	}
	_, _ = service.ApplyPersonalityDrift(ctx, sessionID, unitID, DriftReasonIntervention, turn)
	return id, nil
}

// RequestAdvancePhase 处理玩家“下一阶段”请求：单人局满足当前阶段方针即可推进；多人局先记录准备，双方都准备后推进。
func (service *Service) RequestAdvancePhase(ctx context.Context, sessionID string, commanderFactionID string) (Snapshot, bool, error) {
	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return Snapshot{}, false, err
	}
	if state.SetupPhase == SetupPhaseDrafting || state.TurnState.Phase == turns.PhaseExecution || state.Outcome != OutcomeOngoing {
		snapshot, advanceErr := service.AdvancePhase(ctx, sessionID)
		return snapshot, true, advanceErr
	}
	if state.TurnState.Phase == turns.PhaseDeployment && service.asyncExecution && phaseDeadlineReached(state) {
		snapshot, advanceErr := service.advanceDeploymentToExecutionFastPath(ctx, &state, units)
		return snapshot, true, advanceErr
	}

	commanderFactionID = normalizeCommanderFactionID(state, commanderFactionID)
	if commanderFactionID == "" {
		return Snapshot{}, false, fmt.Errorf("invalid commander faction")
	}
	if !hasFactionDirectiveForCurrentPhase(state, commanderFactionID) {
		return buildSnapshot(state, units), false, fmt.Errorf("请先提交当前阶段方针，再选择下一阶段")
	}
	if state.Mode == ModeSinglePlayer && state.TurnState.Phase == turns.PhaseDeployment && !service.asyncExecution {
		service.refreshEnemyGlobalDirectiveForDeploymentPhase(ctx, &state, units, "single_player_ready_check")
		if !hasFactionDirectiveForCurrentPhase(state, state.EnemyFactionID) {
			if err := service.sessions.Save(ctx, &state); err != nil {
				return Snapshot{}, false, err
			}
			return buildSnapshot(state, units), false, fmt.Errorf("敌方全局方针尚未生成，请稍后再进入执行阶段")
		}
	}

	if state.Mode != ModeDuel {
		if state.TurnState.Phase == turns.PhaseDeployment && service.asyncExecution {
			snapshot, advanceErr := service.advanceDeploymentToExecutionFastPath(ctx, &state, units)
			return snapshot, true, advanceErr
		}
		snapshot, advanceErr := service.AdvancePhase(ctx, sessionID)
		return snapshot, true, advanceErr
	}

	ensurePhaseReady(&state)
	state.PhaseReady[commanderFactionID] = true
	if duelFactionsReady(state) {
		if err := service.sessions.Save(ctx, &state); err != nil {
			return Snapshot{}, false, err
		}
		snapshot, advanceErr := service.AdvancePhase(ctx, sessionID)
		return snapshot, true, advanceErr
	}

	appendLog(&state, "phase_ready", fmt.Sprintf("%s已选择进入下一阶段，等待对方确认。", factionCommanderLabel(state, commanderFactionID)), "", "")
	if err := service.sessions.Save(ctx, &state); err != nil {
		return Snapshot{}, false, err
	}
	return buildSnapshot(state, units), false, nil
}

// advanceDeploymentToExecutionFastPath 只完成 deployment -> execution 的最小状态切换。
// 部署收尾、敌方方针刷新、信鸽派发和单位行动仍在异步执行流开头按原顺序处理，
// 这样 HTTP 请求不会因为 LLM 或阶段收尾超过反向代理超时。
func (service *Service) advanceDeploymentToExecutionFastPath(ctx context.Context, state *State, units []unit.Record) (Snapshot, error) {
	if service == nil || state == nil {
		return Snapshot{}, fmt.Errorf("session service is not available")
	}
	if state.TurnState.Phase != turns.PhaseDeployment || state.Outcome != OutcomeOngoing {
		return buildSnapshot(*state, units), nil
	}

	now := time.Now().UTC()
	state.TurnState.Advance(now)
	resetPhaseReady(state)
	state.ExecutionInProgress = true
	appendLog(state, "phase", fmt.Sprintf("第 %d 回合执行阶段开始。所有单位会先消化你的自然语言方针与对话，再由 AI 自主处理进食、调拨、交换、生产、建造与战斗行动。", state.TurnState.Turn), "", "")

	if err := service.saveSession(ctx, state); err != nil {
		return Snapshot{}, err
	}
	snapshot := buildSnapshot(*state, units)
	service.launchAsyncExecution(state.ID)
	go service.recordExecutionBoundaryBestEffort(state.ID)
	return snapshot, nil
}

func (service *Service) recordExecutionBoundaryBestEffort(sessionID string) {
	if service == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), asyncCleanupTimeout)
	defer cancel()
	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil || state.TurnState.Phase != turns.PhaseExecution {
		return
	}
	_ = service.syncCombatFlags(ctx, &state, units)
	_ = service.recordPhaseBoundarySnapshot(ctx, &state, units)
}

// phaseDeadlineReached 判断当前阶段是否已到倒计时截止，超时后自动推进不受准备状态限制。
func phaseDeadlineReached(state State) bool {
	deadline := state.TurnState.PhaseEndsAt
	return !deadline.IsZero() && !time.Now().UTC().Before(deadline)
}

func ensurePhaseReady(state *State) {
	if state.PhaseReady == nil {
		state.PhaseReady = map[string]bool{}
	}
}

func resetPhaseReady(state *State) {
	state.PhaseReady = map[string]bool{}
}

func duelFactionsReady(state State) bool {
	return state.PhaseReady[state.PlayerFactionID] && state.PhaseReady[state.EnemyFactionID]
}

func hasFactionDirectiveForCurrentPhase(state State, factionID string) bool {
	factionID = normalizeCommanderFactionID(state, factionID)
	if factionID == "" {
		return false
	}
	expectedKind := DirectiveKindDoctrine
	if state.TurnState.Phase != turns.PhaseDeployment {
		return true
	}
	for index := len(state.DirectiveHistory) - 1; index >= 0; index-- {
		directive := state.DirectiveHistory[index]
		if directive.Turn != state.TurnState.Turn || directive.Phase != state.TurnState.Phase {
			continue
		}
		if normalizeDirectiveKind(directive.Kind) != expectedKind {
			continue
		}
		if strings.TrimSpace(directive.IssuedBy) != factionID || strings.TrimSpace(directive.Text) == "" {
			continue
		}
		return true
	}
	return false
}

// resolveExecutionAsync 在后台推进执行阶段，避免 HTTP 请求阻塞整轮 LLM 决策。
func (service *Service) resolveExecutionAsync(sessionID string) {
	if !markAsyncExecutionRunning(sessionID) {
		return
	}
	defer unmarkAsyncExecutionRunning(sessionID)
	defer func() {
		if recovered := recover(); recovered != nil {
			service.markAsyncExecutionInterrupted(sessionID, fmt.Sprintf("执行阶段后台任务异常退出：%v", recovered))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), asyncExecutionTimeout)
	defer cancel()
	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		service.markAsyncExecutionInterrupted(sessionID, fmt.Sprintf("执行阶段后台任务启动失败：%v", err))
		return
	}
	if state.TurnState.Phase != turns.PhaseExecution || !state.ExecutionInProgress {
		return
	}
	if err := service.resolveDeploymentBeforeAsyncExecution(ctx, &state, units); err != nil {
		appendLog(&state, "deployment_error", fmt.Sprintf("部署收尾被中断：%v", err), "", "")
	}
	if err := service.resolveExecution(ctx, &state, units); err != nil {
		appendLog(&state, "execution_error", fmt.Sprintf("执行阶段被中断：%v", err), "", "")
		state.ExecutionInProgress = false
		service.saveAsyncExecutionStateBestEffort(&state)
		if service.progressReporter != nil {
			service.progressReporter("execution_interrupted", buildSnapshot(state, units), map[string]any{
				"turn":  state.TurnState.Turn,
				"phase": state.TurnState.Phase,
				"error": err.Error(),
			})
		}
		return
	}
	advanceCtx, advanceCancel := context.WithTimeout(context.Background(), asyncPhaseAdvanceTimeout)
	defer advanceCancel()
	if err := service.advanceAfterAsyncExecution(advanceCtx, &state, units); err != nil {
		appendLog(&state, "execution_error", fmt.Sprintf("执行阶段收尾被中断：%v", err), "", "")
		state.ExecutionInProgress = false
		service.saveAsyncExecutionStateBestEffort(&state)
		if service.progressReporter != nil {
			service.progressReporter("execution_interrupted", buildSnapshot(state, units), map[string]any{
				"turn":  state.TurnState.Turn,
				"phase": state.TurnState.Phase,
				"error": err.Error(),
			})
		}
		return
	}
	if service.progressReporter != nil {
		service.progressReporter("execution_completed", buildSnapshot(state, units), map[string]any{
			"turn":  state.TurnState.Turn,
			"phase": state.TurnState.Phase,
		})
	}
}

func (service *Service) launchAsyncExecution(sessionID string) {
	if service == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	go service.resolveExecutionAsync(sessionID)
}

func markAsyncExecutionRunning(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	asyncExecutionRegistry.Lock()
	defer asyncExecutionRegistry.Unlock()
	if _, exists := asyncExecutionRegistry.running[sessionID]; exists {
		return false
	}
	asyncExecutionRegistry.running[sessionID] = struct{}{}
	return true
}

func unmarkAsyncExecutionRunning(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	asyncExecutionRegistry.Lock()
	delete(asyncExecutionRegistry.running, sessionID)
	asyncExecutionRegistry.Unlock()
}

// IsExecutionRunning 导出进程级「某会话正在聚焦战斗执行」判定，供 region-runner 让位战斗（不打扰在战单位）。
func IsExecutionRunning(sessionID string) bool {
	return isAsyncExecutionRunning(sessionID)
}

func isAsyncExecutionRunning(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	asyncExecutionRegistry.Lock()
	defer asyncExecutionRegistry.Unlock()
	_, exists := asyncExecutionRegistry.running[sessionID]
	return exists
}

func (service *Service) resumeStaleAsyncExecutionIfNeeded(ctx context.Context, state *State) bool {
	if service == nil || state == nil || !service.asyncExecution {
		return false
	}
	if state.TurnState.Phase != turns.PhaseExecution || !state.ExecutionInProgress || state.Outcome != OutcomeOngoing {
		return false
	}
	if isAsyncExecutionRunning(state.ID) || activeLLMCallForSession(state.ID) {
		return false
	}
	now := time.Now().UTC()
	staleAfter := state.UpdatedAt.Add(2 * time.Minute)
	if !state.TurnState.PhaseEndsAt.IsZero() && state.TurnState.PhaseEndsAt.After(staleAfter) {
		staleAfter = state.TurnState.PhaseEndsAt
	}
	if now.Before(staleAfter) {
		return false
	}
	appendLog(state, "execution_resume", "执行阶段后台任务疑似中断，系统已重新接续执行。", "", "")
	if err := service.sessions.Save(ctx, state); err != nil {
		return false
	}
	service.launchAsyncExecution(state.ID)
	return true
}

func activeLLMCallForSession(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	for _, call := range ai.ActiveCalls() {
		if strings.TrimSpace(call.Metadata["session_id"]) == sessionID {
			return true
		}
	}
	return false
}

func (service *Service) markAsyncExecutionInterrupted(sessionID string, message string) {
	if service == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), asyncCleanupTimeout)
	defer cancel()
	state, _, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return
	}
	if state.TurnState.Phase != turns.PhaseExecution || !state.ExecutionInProgress {
		return
	}
	if strings.TrimSpace(message) == "" {
		message = "执行阶段后台任务异常中断。"
	}
	appendLog(&state, "execution_error", message, "", "")
	state.ExecutionInProgress = false
	_ = service.saveSessionMergingExternalEvents(ctx, &state)
}

// saveAsyncExecutionStateBestEffort 用独立短超时上下文保存异步执行的最终状态。
// 后台执行的主 ctx 可能已因整轮超时被取消，此时继续复用它会导致“清理失败”，前端就会停留在执行中。
func (service *Service) saveAsyncExecutionStateBestEffort(state *State) {
	if service == nil || state == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), asyncCleanupTimeout)
	defer cancel()
	_ = service.saveSessionMergingExternalEvents(cleanupCtx, state)
}

// saveSessionMergingExternalEvents 保存异步执行状态前，合并执行期间由 HTTP 请求写入的指令/日志。
// 单局状态是整块 JSON 覆盖写；如果后台执行协程直接保存旧快照，会覆盖玩家在执行阶段预设的下一回合方针。
func (service *Service) saveSessionMergingExternalEvents(ctx context.Context, state *State) error {
	if service == nil || state == nil || strings.TrimSpace(state.ID) == "" {
		return nil
	}
	service.sessionSaveMu.Lock()
	defer service.sessionSaveMu.Unlock()
	if persisted, err := service.sessions.Get(ctx, state.ID); err == nil {
		mergeMissingDirectives(state, persisted.DirectiveHistory)
		mergeMissingLogs(state, persisted.Logs)
		// 注意：拆 state_json 后 LLM 交互与原始事件日志均已切旁路表、Save 会从 blob 摘除，故裸 Get 的 persisted.LLMInteractions /
		// persisted.RawEventLog 恒空、这两条 merge 已成 no-op——但不丢数据：外部写入方各自的 Save 已 persist 进表，
		// 下次 loadSession 由 hydrate 兜底自愈。保留调用仅为向后兼容尚未切表的现网旧 blob 残留（hydrate 也会回填）。
		mergeMissingLLMInteractions(state, persisted.LLMInteractions)
		mergeMissingRawEvents(state, persisted.RawEventLog)
		refreshGlobalDirectiveFromHistory(state)
	}
	return service.sessions.Save(ctx, state)
}

func (service *Service) saveSession(ctx context.Context, state *State) error {
	if service == nil || state == nil {
		return nil
	}
	service.sessionSaveMu.Lock()
	defer service.sessionSaveMu.Unlock()
	return service.sessions.Save(ctx, state)
}

// compactStateForStorage keeps the hot session row small enough for frequent
// polling and websocket broadcasts. Full prompt bodies are only useful for the
// most recent developer inspection; old entries keep summary, provider/model
// and token/cost metadata.
func compactStateForStorage(state *State) {
	if state == nil {
		return
	}
	if len(state.DecisionTraces) > maxDecisionHistory {
		state.DecisionTraces = state.DecisionTraces[len(state.DecisionTraces)-maxDecisionHistory:]
	}
	compactLLMInteractions(state)
	if len(state.RawEventLog) > maxRawEventHistory {
		state.RawEventLog = state.RawEventLog[len(state.RawEventLog)-maxRawEventHistory:]
	}
	for index := range state.RawEventLog {
		entry := &state.RawEventLog[index]
		if entry.Source == "llm" || entry.Source == "decision" {
			entry.PayloadJSON = ""
		} else {
			entry.PayloadJSON = limitTextRunes(entry.PayloadJSON, 1200)
		}
	}
}

// compactLLMInteractions 把 LLM 交互的内存工作集收敛为「最近 maxLLMHistory 条、其中仅最近 maxFullLLMHistory 条留完整
// prompt」。它既是 Save 前的 blob 瘦身，也是 cutover 后 hydrate 的收敛器——hydrate 从旁路表读全量后调它，保证切表后
// 内存/快照视图与切表前逐字节一致（旁路表仍保有全量完整 prompt 供 ListLLMInteractions 审计，但工作集不膨胀）。幂等。
func compactLLMInteractions(state *State) {
	if state == nil {
		return
	}
	if len(state.LLMInteractions) > maxLLMHistory {
		state.LLMInteractions = state.LLMInteractions[len(state.LLMInteractions)-maxLLMHistory:]
	}
	fullFrom := len(state.LLMInteractions) - maxFullLLMHistory
	if fullFrom < 0 {
		fullFrom = 0
	}
	for index := range state.LLMInteractions {
		interaction := &state.LLMInteractions[index]
		if index < fullFrom {
			interaction.SystemPrompt = ""
			interaction.UserPrompt = ""
			interaction.ParsedOutput = limitTextRunes(interaction.ParsedOutput, 600)
			interaction.RawOutput = ""
			interaction.Attempts = nil
			continue
		}
		interaction.SystemPrompt = limitTextRunes(interaction.SystemPrompt, maxStoredPromptRunes)
		interaction.UserPrompt = limitTextRunes(interaction.UserPrompt, maxStoredPromptRunes)
		interaction.ParsedOutput = limitTextRunes(interaction.ParsedOutput, maxStoredOutputRunes)
		interaction.RawOutput = limitTextRunes(interaction.RawOutput, maxStoredOutputRunes)
	}
}

func mergeMissingDirectives(state *State, external []Directive) {
	if state == nil || len(external) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(state.DirectiveHistory))
	for _, directive := range state.DirectiveHistory {
		if directive.ID != "" {
			seen[directive.ID] = struct{}{}
		}
	}
	for _, directive := range external {
		if directive.ID == "" {
			continue
		}
		if _, ok := seen[directive.ID]; ok {
			continue
		}
		state.DirectiveHistory = append(state.DirectiveHistory, directive)
		seen[directive.ID] = struct{}{}
	}
	if len(state.DirectiveHistory) > maxDirectiveHistory {
		state.DirectiveHistory = state.DirectiveHistory[len(state.DirectiveHistory)-maxDirectiveHistory:]
	}
}

func mergeMissingLogs(state *State, external []LogEntry) {
	if state == nil || len(external) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(state.Logs))
	for _, entry := range state.Logs {
		if entry.ID != "" {
			seen[entry.ID] = struct{}{}
		}
	}
	for _, entry := range external {
		if entry.ID == "" {
			continue
		}
		if _, ok := seen[entry.ID]; ok {
			continue
		}
		state.Logs = append(state.Logs, entry)
		seen[entry.ID] = struct{}{}
	}
	if len(state.Logs) > maxLogEntries {
		state.Logs = state.Logs[len(state.Logs)-maxLogEntries:]
	}
}

func mergeMissingLLMInteractions(state *State, external []LLMInteraction) {
	if state == nil || len(external) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(state.LLMInteractions))
	for _, interaction := range state.LLMInteractions {
		if interaction.ID != "" {
			seen[interaction.ID] = struct{}{}
		}
	}
	for _, interaction := range external {
		if interaction.ID == "" {
			continue
		}
		if _, ok := seen[interaction.ID]; ok {
			continue
		}
		state.LLMInteractions = append(state.LLMInteractions, interaction)
		seen[interaction.ID] = struct{}{}
	}
	if len(state.LLMInteractions) > maxLLMHistory {
		state.LLMInteractions = state.LLMInteractions[len(state.LLMInteractions)-maxLLMHistory:]
	}
}

func mergeMissingRawEvents(state *State, external []RawEventEntry) {
	if state == nil || len(external) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(state.RawEventLog))
	for _, entry := range state.RawEventLog {
		if entry.ID != "" {
			seen[entry.ID] = struct{}{}
		}
	}
	for _, entry := range external {
		if entry.ID == "" {
			continue
		}
		if _, ok := seen[entry.ID]; ok {
			continue
		}
		state.RawEventLog = append(state.RawEventLog, entry)
		seen[entry.ID] = struct{}{}
	}
}

func refreshGlobalDirectiveFromHistory(state *State) {
	if state == nil {
		return
	}
	for index := len(state.DirectiveHistory) - 1; index >= 0; index-- {
		directive := state.DirectiveHistory[index]
		if normalizeDirectiveKind(directive.Kind) != DirectiveKindDoctrine {
			continue
		}
		if directive.Turn > state.TurnState.Turn {
			continue
		}
		if directive.AppliesTo == "" || directive.AppliesTo == state.PlayerFactionID {
			state.GlobalDirective = directive
			return
		}
	}
}

// advanceAfterAsyncExecution 在异步执行完成后执行与手动推进执行阶段相同的回合收尾。
func (service *Service) advanceAfterAsyncExecution(ctx context.Context, state *State, units []unit.Record) error {
	if service == nil || state == nil {
		return nil
	}
	state.ExecutionInProgress = false
	if state.TurnState.Phase == turns.PhaseExecution && state.Outcome == OutcomeOngoing {
		now := time.Now().UTC()
		state.TurnState.Advance(now)
		resetPhaseReady(state)
		state.Weather = weatherForTurnBySeed(state.RandomSeed, state.TurnState.Turn)
		rechargeCommandPower(state)
		if err := service.resolveDuePregnancies(ctx, state); err != nil {
			return err
		}
		if err := service.resolvePigeonDeliveries(ctx, state, units); err != nil {
			return err
		}
		appendLog(state, "phase", fmt.Sprintf("第 %d 回合部署阶段开始。你可以更新新的全局方针、点名对话或下达部署任务。", state.TurnState.Turn), "", "")
		appendLog(state, "weather", fmt.Sprintf("第 %d 回合天气：%s。%s", state.TurnState.Turn, state.Weather.DisplayName, state.Weather.Note), "", "")
		if err := service.applyTurnHungerUpkeep(ctx, state, units); err != nil {
			return err
		}
		if err := service.resolveTurnRandomEvent(ctx, state, units); err != nil {
			return err
		}
		if err := service.refreshSessionMemoryDecay(ctx, state, units); err != nil {
			return err
		}
		service.refreshEnemyGlobalDirectiveForDeploymentPhase(ctx, state, units, "deployment_phase_started")
		appendSessionMetricsLog(state)
	}
	if err := service.syncCombatFlags(ctx, state, units); err != nil {
		return err
	}
	if err := service.saveSessionMergingExternalEvents(ctx, state); err != nil {
		return err
	}
	return service.recordPhaseBoundarySnapshot(ctx, state, units)
}

// resolveDeploymentBeforeAsyncExecution 在异步执行流开头完成部署阶段的 AI 收尾。
// 这样 deployment -> execution 的 HTTP 请求可以立即返回，避免部署交易/信鸽 LLM 调用阻塞前端。
func (service *Service) resolveDeploymentBeforeAsyncExecution(ctx context.Context, state *State, units []unit.Record) error {
	if service == nil || state == nil || state.TurnState.Phase != turns.PhaseExecution {
		return nil
	}
	originalPhase := state.TurnState.Phase
	state.TurnState.Phase = turns.PhaseDeployment
	defer func() {
		state.TurnState.Phase = originalPhase
	}()
	service.refreshEnemyGlobalDirectiveForDeploymentPhase(ctx, state, units, "deployment_phase_advanced")
	if err := service.resolvePigeonDispatches(ctx, state, units); err != nil {
		return err
	}
	return nil
}

// resolveExecution 逐单位执行完整回合：按执行顺序轮转，每个单位每轮只执行一步 AP 动作。
func (service *Service) resolveExecution(ctx context.Context, state *State, units []unit.Record) error {
	byID := mapRecordsByID(units)
	executionOrder, speedBreakdowns := buildExecutionOrderByATB(*state, byID)
	appendLog(state, "executor_loop", describeExecutorLoop(executionOrder, byID, speedBreakdowns), "", "")
	progressTotal := 0
	for _, unitID := range executionOrder {
		record := byID[unitID]
		if record == nil || !isBattleReady(*record) {
			continue
		}
		progressTotal++
	}
	startedUnits := 0
	completedUnits := 0
	executionStates := make(map[string]*executionActorState, len(executionOrder))

	finalizeActor := func(actor *unit.Record, actorState *executionActorState) error {
		if actor == nil || actorState == nil || actorState.completed {
			return nil
		}
		actorState.completed = true
		completedUnits++
		if err := service.saveSessionMergingExternalEvents(ctx, state); err != nil {
			return err
		}
		service.emitExecutionUnitProgress(state, byID, actor, completedUnits, progressTotal)
		return nil
	}

	for {
		actedThisRound := false

		for _, unitID := range executionOrder {
			actor := byID[unitID]
			if actor == nil {
				continue
			}
			actorState, ok := executionStates[unitID]
			if !ok {
				actorState = &executionActorState{}
				executionStates[unitID] = actorState
			}
			if actorState.completed {
				continue
			}
			if !isBattleReady(*actor) {
				if err := finalizeActor(actor, actorState); err != nil {
					return err
				}
				continue
			}

			if !actorState.started {
				startedUnits++
				service.emitExecutionUnitStart(state, byID, actor, startedUnits, progressTotal)
				if err := service.saveSessionMergingExternalEvents(ctx, state); err != nil {
					return err
				}

				if clearExpiredCombatEffects(actor, state.TurnState.Turn) {
					if err := service.units.Save(ctx, *actor); err != nil {
						return err
					}
				}
				actorState.remainingAP = executionActionPoints(*actor)
				actorState.actionIndex = 1
				actorState.started = true
			}
			if actorState.remainingAP <= 0 {
				if err := finalizeActor(actor, actorState); err != nil {
					return err
				}
				continue
			}

			targetIDs := visibleOpposingIDs(*state, byID, actor)

			var (
				decision unitDecisionPayload
				result   ai.CompletionResult
			)
			normalDecision, normalResult, interaction, normalErr := service.generateUnitDecision(
				ctx,
				*state,
				byID,
				actor,
				targetIDs,
				actorState.remainingAP,
				false,
			)
			service.appendLLMInteractionWithSpend(ctx, state, interaction)
			if normalErr != nil {
				appendLog(state, "decision_error", "我这回合没想好下一步。", actor.ID, "")
				if !service.asyncExecution {
					return normalErr
				}
				if err := finalizeActor(actor, actorState); err != nil {
					return err
				}
				continue
			}
			decision = normalDecision
			result = normalResult

			// 高压情绪覆写：仅战斗就绪且触发器命中时，combat_shake 才会改写本次决策。
			// 触发失败（LLM/规则）按未触发处理，best-effort，绝不中断主循环；确定性由内部 FNV 掷骰保证。
			shakeResolution, shakeErr := service.resolveCombatShake(ctx, state, byID, actor)
			if shakeErr != nil {
				appendLog(state, "shake_skip", "我心里一阵翻涌，但还是照原计划行事。", actor.ID, "")
				shakeResolution = combatShakeResolution{
					Modifiers: actionModifiers{MoveMultiplier: 1, AttackMultiplier: 1},
				}
			}
			if shakeResolution.Triggered {
				if shakeResolution.OverrideDecision != nil {
					decision = *shakeResolution.OverrideDecision
				}
				decision = applyCombatShakeOverlay(decision, shakeResolution)
			}

			compliance := resolveDirectiveCompliance(*state, byID, actor, decision)
			logDirectiveCompliance(state, actor, byID, compliance)
			// 忠诚负反馈闭环（§5.7「越按越不听」，best-effort）：强令违心→离心、顺其本心→归心，经 Mutator 落地。
			// 仅在本回合首个 AP 动作结算一次：执行主循环对 AP=N 的单位一回合处理 N 次，若每动作都结算会按 AP 数翻倍漂移，
			// 破坏「速率∝玩家按了几次」语义。actionIndex==1 即本回合首动作（service.go:1708 置 1、随后递增）。
			if actorState.actionIndex == 1 {
				_ = service.settleLoyaltyFromCompliance(ctx, state, actor, compliance)
			}
			defiantAction := isDefiantAction(compliance)
			modifiers := combineActionModifiers(compliance.Modifiers, hungerActionModifiers(*actor), shakeResolution.Modifiers)
			if actor.Status.Hunger < 30 {
				appendLog(state, "hunger_penalty", "我因饥饿而行动效率下降。", actor.ID, "")
			}
			if err := service.actionValidator(*state, byID, actor, targetIDs, compliance.Final, actorState.remainingAP); err != nil {
				reasonText := strings.TrimSpace(firstNonEmptyText(
					compliance.Final.NextAction,
					compliance.Final.Speak,
					compliance.Final.Memory,
					compliance.Final.Reasoning,
				))
				reasonText = strings.TrimSpace(firstNonEmptyText(reasonText, "我这一步先不动。"))
				appendLog(
					state,
					"action_invalid",
					reasonText,
					actor.ID,
					compliance.Final.TargetUnitID,
				)
				compliance.Final = unitDecisionPayload{
					Action:     DecisionActionHold,
					NextAction: compliance.Final.NextAction,
					Speak:      compliance.Final.Speak,
					Memory:     compliance.Final.Memory,
					Knowledge:  compliance.Final.Knowledge,
					Reasoning:  compliance.Final.Reasoning,
				}
			}

			apBefore := actorState.remainingAP
			apCost := executionActionCost(compliance.Final)
			if apCost > actorState.remainingAP {
				apText := strings.TrimSpace(firstNonEmptyText(
					compliance.Final.NextAction,
					compliance.Final.Speak,
					compliance.Final.Memory,
					compliance.Final.Reasoning,
				))
				apText = strings.TrimSpace(firstNonEmptyText(apText, compliance.Final.Reasoning))
				appendLog(
					state,
					"ap",
					apText,
					actor.ID,
					"",
				)
				compliance.Final = unitDecisionPayload{
					Action:     DecisionActionHold,
					NextAction: compliance.Final.NextAction,
					Speak:      compliance.Final.Speak,
					Memory:     compliance.Final.Memory,
					Knowledge:  compliance.Final.Knowledge,
					Reasoning:  compliance.Final.Reasoning,
				}
				apCost = 0
			}
			compliance.Final = service.ensureAIDecisionText(ctx, state, byID, actor, compliance.Final)
			apAfter := actorState.remainingAP - apCost
			if apAfter < 0 {
				apAfter = 0
			}
			memorySource := "unit_self"
			memoryImportanceBoost := 0
			if defiantAction {
				memorySource = "defiant_action"
				memoryImportanceBoost = defiantMemoryBoost
			}

			service.shadowDecisionTrace(ctx, state.ID, appendDecisionTrace(state, DecisionTrace{
				ID:                    uuid.NewString(),
				UnitID:                actor.ID,
				FactionID:             actor.FactionID,
				RequestedAction:       compliance.Requested.Action,
				RequestedActivity:     string(compliance.Requested.Activity),
				RequestedSkillID:      compliance.Requested.SkillID,
				RequestedStructureID:  compliance.Requested.StructureID,
				RequestedStructure:    string(compliance.Requested.StructureType),
				RequestedTargetUnitID: compliance.Requested.TargetUnitID,
				RequestedTradeKind:    string(compliance.Requested.TradeKind),
				RequestedItemID:       compliance.Requested.ItemID,
				RequestedOtherItemID:  compliance.Requested.OtherItemID,
				RequestedPrice:        compliance.Requested.Price,
				RequestedGoldAmount:   compliance.Requested.GoldAmount,
				RequestedTargetQ:      compliance.Requested.TargetQ,
				RequestedTargetR:      compliance.Requested.TargetR,
				RequestedNextAction:   compliance.Requested.NextAction,
				RequestedSpeak:        compliance.Requested.Speak,
				RequestedMemory:       compliance.Requested.Memory,
				RequestedKnowledge:    compliance.Requested.Knowledge,
				RequestedReasoning:    compliance.Requested.Reasoning,
				Action:                compliance.Final.Action,
				Activity:              string(compliance.Final.Activity),
				SkillID:               compliance.Final.SkillID,
				StructureID:           compliance.Final.StructureID,
				StructureType:         string(compliance.Final.StructureType),
				TargetUnitID:          compliance.Final.TargetUnitID,
				TradeKind:             string(compliance.Final.TradeKind),
				ItemID:                compliance.Final.ItemID,
				OtherItemID:           compliance.Final.OtherItemID,
				Price:                 compliance.Final.Price,
				GoldAmount:            compliance.Final.GoldAmount,
				TargetQ:               compliance.Final.TargetQ,
				TargetR:               compliance.Final.TargetR,
				NextAction:            compliance.Final.NextAction,
				Speak:                 compliance.Final.Speak,
				Memory:                compliance.Final.Memory,
				Knowledge:             compliance.Final.Knowledge,
				Reasoning:             compliance.Final.Reasoning,
				ObedienceState:        string(compliance.State),
				ObedienceNote:         compliance.Note,
				RejectProbability:     compliance.RejectProbability,
				RiskScore:             compliance.RiskScore,
				Defiant:               defiantAction,
				MemoryImportanceBoost: memoryImportanceBoost,
				MoveMultiplier:        modifiers.MoveMultiplier,
				AttackMultiplier:      modifiers.AttackMultiplier,
				ActionIndex:           actorState.actionIndex,
				APBefore:              apBefore,
				APCost:                apCost,
				APAfter:               apAfter,
				Turn:                  state.TurnState.Turn,
				Phase:                 state.TurnState.Phase,
				OccurredAt:            time.Now().UTC(),
				Provider:              result.Provider,
				Model:                 result.Model,
				UsedFallback:          result.UsedFallback,
			}))

			if compliance.Final.Speak != "" {
				appendLog(state, "speech", fmt.Sprintf("%s：%s", actor.DisplayName(), compliance.Final.Speak), actor.ID, "")
			}

			actionLogStart := len(state.Logs)
			if err := service.actionExecutor(ctx, state, byID, actor, targetIDs, compliance.Final, modifiers); err != nil {
				return err
			}
			if !service.asyncExecution {
				service.emitActionNarrationBestEffort(ctx, state, byID, actor, compliance.Final, actionLogStart)
			}
			if err := service.rememberUnitWithSource(ctx, actor, state.TurnState.Turn, compliance.Final.Memory, memorySource, memoryImportanceBoost); err != nil {
				return err
			}
			if knowledgeLine, learned := service.rememberWorldKnowledgeBestEffort(ctx, *state, actor, compliance.Final); learned {
				appendLog(
					state,
					"knowledge",
					fmt.Sprintf("%s：%s", actor.DisplayName(), strings.TrimSpace(knowledgeLine)),
					actor.ID,
					"",
				)
			}
			if err := service.saveSessionMergingExternalEvents(ctx, state); err != nil {
				return err
			}
			service.emitExecutionActionProgress(state, byID, actor, completedUnits, progressTotal, actorState.actionIndex)

			actorState.remainingAP = apAfter
			if apCost > 0 {
				apText := strings.TrimSpace(firstNonEmptyText(
					compliance.Final.NextAction,
					compliance.Final.Speak,
					compliance.Final.Memory,
					compliance.Final.Reasoning,
				))
				if apText != "" {
					appendLog(
						state,
						"ap",
						apText,
						actor.ID,
						"",
					)
				}
			}

			actedThisRound = true
			if updateOutcome(state, byID) {
				if err := finalizeActor(actor, actorState); err != nil {
					return err
				}
				break
			}
			if compliance.Final.Action == DecisionActionHold ||
				actorState.remainingAP <= 0 ||
				actorState.actionIndex >= maxActionsPerUnit {
				if err := finalizeActor(actor, actorState); err != nil {
					return err
				}
				continue
			}

			actorState.actionIndex++
		}
		if state.Outcome != OutcomeOngoing || !actedThisRound {
			break
		}
	}

	if state.Outcome == OutcomeOngoing {
		service.processIntelAssets(ctx, state, byID)
	}
	if battleReportsEnabled {
		service.recordBattleReportBestEffort(ctx, state, byID)
	}
	if state.Outcome != OutcomeOngoing {
		if err := service.persistHallOfFame(ctx, state, byID); err != nil {
			return err
		}
	}

	return service.syncCombatFlags(ctx, state, units)
}

// emitExecutionActionProgress 在单位完成一次 AP 行动后广播快照，用于前端立即展示刚发生的行动。
func (service *Service) emitExecutionActionProgress(
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	completedUnits int,
	totalUnits int,
	actionIndex int,
) {
	if service == nil || service.progressReporter == nil || state == nil || actor == nil {
		return
	}
	if totalUnits <= 0 {
		return
	}
	snapshot := buildSnapshot(*state, snapshotUnitsFromByID(*state, byID))
	service.progressReporter("execution_action_completed", snapshot, map[string]any{
		"turn":            state.TurnState.Turn,
		"phase":           state.TurnState.Phase,
		"unit_id":         actor.ID,
		"unit_name":       actor.DisplayName(),
		"completed_units": completedUnits,
		"total_units":     totalUnits,
		"action_index":    actionIndex,
	})
}

// executionActionPoints 根据单位状态计算本回合可用 AP。
func executionActionPoints(actor unit.Record) int {
	points := baseExecutionAP
	if actor.Status.Hunger < 20 {
		points--
	}
	if actor.Status.Fatigue >= 70 {
		points--
	}
	if points < 1 {
		return 1
	}
	return points
}

// executionActionCost 返回动作的基础 AP 消耗。
func executionActionCost(decision unitDecisionPayload) int {
	return decisionCost(decision)
}

// decisionActionCost 根据决策动作枚举计算 AP 消耗。
func decisionActionCost(action DecisionAction) int {
	switch action {
	case DecisionActionCharge, DecisionActionHeavyAttack, DecisionActionGather, DecisionActionBuild, DecisionActionForge, DecisionActionUpgrade:
		return 2
	case DecisionActionAttack, DecisionActionMove, DecisionActionDefend, DecisionActionObserve, DecisionActionAssist, DecisionActionSkill, DecisionActionSay, DecisionActionDialogue, DecisionActionTrade, DecisionActionRomance, DecisionActionFamily, DecisionActionDemolish, DecisionActionEquip, DecisionActionEat, DecisionActionPickup:
		return 1
	default:
		return 0
	}
}

// decisionCost 从完整决策载荷推导最终 AP 成本。
func decisionCost(decision unitDecisionPayload) int {
	if decision.Action == DecisionActionSkill {
		return combatSkillCost(decision.SkillID)
	}
	return decisionActionCost(decision.Action)
}

// actionLabel 生成动作对应的中文标签文案。
func actionLabel(action DecisionAction) string {
	switch action {
	case DecisionActionAttack:
		return "攻击"
	case DecisionActionCharge:
		return "冲锋"
	case DecisionActionHeavyAttack:
		return "重击"
	case DecisionActionSkill:
		return "技能"
	case DecisionActionDefend:
		return "防御"
	case DecisionActionObserve:
		return "观察"
	case DecisionActionAssist:
		return "援助"
	case DecisionActionSay:
		return "发言"
	case DecisionActionDialogue:
		return "交谈"
	case DecisionActionTrade:
		return "交易"
	case DecisionActionRomance:
		return "表白"
	case DecisionActionFamily:
		return "养育"
	case DecisionActionBuild:
		return "建造"
	case DecisionActionDemolish:
		return "拆除"
	case DecisionActionGather:
		return "生产"
	case DecisionActionForge:
		return "锻造"
	case DecisionActionUpgrade:
		return "强化"
	case DecisionActionEquip:
		return "装备"
	case DecisionActionEat:
		return "进食"
	case DecisionActionPickup:
		return "拾取"
	case DecisionActionMove:
		return "移动"
	default:
		return "待命"
	}
}

// emitActionNarrationBestEffort 尝试生成并广播动作叙述，不阻塞主执行流。
func (service *Service) emitActionNarrationBestEffort(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	decision unitDecisionPayload,
	logStart int,
) {
	if service == nil || state == nil || actor == nil {
		return
	}

	eventSummary := actionNarrationEventSummary(*state, actor.ID, decision, logStart)
	eventSummary = enrichActionNarrationEventSummary(*state, byID, actor, decision, eventSummary, actionNarrationStartTime(*state, logStart))
	payload, result, interaction, err := service.generateUnitReflection(
		ctx,
		*state,
		byID,
		*actor,
		eventSummary,
		"action_narration",
	)
	if err != nil {
		appendLog(
			state,
			"action_narration_error",
			"我这回合没补上行动短句。",
			actor.ID,
			"",
		)
		return
	}
	service.appendLLMInteractionWithSpend(ctx, state, interaction)
	appendAIDialogue(state, *actor, payload.Bubble, result)
	service.rememberUnitBestEffort(ctx, actor, state.TurnState.Turn, payload.Memory)
	appendLog(
		state,
		"action_narration",
		fmt.Sprintf("%s：%s", actor.DisplayName(), payload.Bubble),
		actor.ID,
		"",
	)
}

func enrichActionNarrationEventSummary(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	decision unitDecisionPayload,
	eventSummary string,
	since time.Time,
) string {
	parts := make([]string, 0, 5)
	if summary := strings.TrimSpace(eventSummary); summary != "" {
		parts = append(parts, summary)
	}
	if action := actionResultDetail(state, byID, actor, decision, since); action != "" {
		parts = append(parts, "行动细节："+action)
	}
	if surroundings := visibleSurroundingsMemoryDetail(state, byID, actor, 3); surroundings != "" {
		parts = append(parts, "周围发现："+surroundings)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "；")
}

func actionResultDetail(state State, byID map[string]*unit.Record, actor *unit.Record, decision unitDecisionPayload, since time.Time) string {
	if actor == nil {
		return ""
	}
	if damage := latestDamageDetail(state, byID, actor.ID, since); damage != "" {
		return damage
	}
	switch decision.Action {
	case DecisionActionMove:
		return fmt.Sprintf("我前进到%d,%d", actor.Status.PositionQ, actor.Status.PositionR)
	case DecisionActionAttack, DecisionActionCharge, DecisionActionHeavyAttack, DecisionActionSkill:
		target := byID[decision.TargetUnitID]
		if target == nil {
			target = nearestBattleReady(visibleOpposingIDs(state, byID, actor), byID, actor)
		}
		targetName := "目标"
		if target != nil {
			targetName = target.DisplayName()
		}
		weapon := equippedWeaponName(*actor)
		if weapon != "" {
			return fmt.Sprintf("我用%s攻击%s", weapon, targetName)
		}
		return fmt.Sprintf("我攻击%s", targetName)
	case DecisionActionDefend:
		return fmt.Sprintf("我在%d,%d防守", actor.Status.PositionQ, actor.Status.PositionR)
	case DecisionActionObserve:
		return fmt.Sprintf("我在%d,%d观察", actor.Status.PositionQ, actor.Status.PositionR)
	case DecisionActionAssist:
		if target := byID[decision.TargetUnitID]; target != nil {
			return fmt.Sprintf("我支援%s", target.DisplayName())
		}
	case DecisionActionGather:
		return fmt.Sprintf("我在%d,%d执行%s", actor.Status.PositionQ, actor.Status.PositionR, productionActivityDisplayName(decision.Activity))
	case DecisionActionBuild:
		return fmt.Sprintf("我在%d,%d建造%s", actor.Status.PositionQ, actor.Status.PositionR, structureDisplayName(decision.StructureType))
	case DecisionActionEat:
		return fmt.Sprintf("我使用%s", displayItemName(decision.ItemID))
	case DecisionActionEquip:
		return fmt.Sprintf("我装备%s", displayItemName(decision.ItemID))
	case DecisionActionPickup:
		return fmt.Sprintf("我拾取%s", displayItemName(decision.ItemID))
	}
	return ""
}

func latestDamageDetail(state State, byID map[string]*unit.Record, actorUnitID string, since time.Time) string {
	for index := len(state.RawEventLog) - 1; index >= 0; index-- {
		entry := state.RawEventLog[index]
		if !since.IsZero() && entry.OccurredAt.Before(since) {
			continue
		}
		if entry.Kind != "hp" || entry.TargetUnitID != actorUnitID || strings.TrimSpace(entry.PayloadJSON) == "" {
			continue
		}
		var payload struct {
			Delta      float64 `json:"delta"`
			ReasonText string  `json:"reason_text"`
		}
		if err := json.Unmarshal([]byte(entry.PayloadJSON), &payload); err != nil || payload.Delta >= 0 {
			continue
		}
		amount := int(-payload.Delta)
		attackerName := "不明来源"
		if attacker := byID[entry.ActorUnitID]; attacker != nil && attacker.ID != actorUnitID {
			attackerName = attacker.DisplayName()
		}
		weapon := ""
		if attacker := byID[entry.ActorUnitID]; attacker != nil && attacker.ID != actorUnitID {
			weapon = equippedWeaponName(*attacker)
		}
		if weapon != "" {
			return fmt.Sprintf("%s用%s向我造成%d伤害", attackerName, weapon, amount)
		}
		if reason := strings.TrimSpace(payload.ReasonText); reason != "" {
			return fmt.Sprintf("%s向我造成%d伤害（%s）", attackerName, amount, reason)
		}
		return fmt.Sprintf("%s向我造成%d伤害", attackerName, amount)
	}
	return ""
}

func actionNarrationStartTime(state State, logStart int) time.Time {
	if logStart < 0 || logStart >= len(state.Logs) {
		return time.Time{}
	}
	return state.Logs[logStart].OccurredAt
}

func visibleSurroundingsMemoryDetail(state State, byID map[string]*unit.Record, actor *unit.Record, limit int) string {
	if actor == nil || limit <= 0 {
		return ""
	}
	parts := make([]string, 0, limit)
	for _, unitID := range visibleOpposingIDs(state, byID, actor) {
		if len(parts) >= limit {
			break
		}
		target := byID[unitID]
		if target == nil || !isBattleReady(*target) {
			continue
		}
		distance := unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR)
		parts = append(parts, fmt.Sprintf("%s在%d,%d距%d", target.DisplayName(), target.Status.PositionQ, target.Status.PositionR, distance))
	}
	remaining := limit - len(parts)
	if remaining > 0 {
		for _, unitID := range visibleAlliedIDs(state, byID, actor) {
			if len(parts) >= limit {
				break
			}
			ally := byID[unitID]
			if ally == nil || !isBattleReady(*ally) {
				continue
			}
			distance := unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, ally.Status.PositionQ, ally.Status.PositionR)
			parts = append(parts, fmt.Sprintf("友军%s在%d,%d距%d", ally.DisplayName(), ally.Status.PositionQ, ally.Status.PositionR, distance))
		}
	}
	if len(parts) == 0 {
		return "无近身单位"
	}
	return strings.Join(parts, "、")
}

func equippedWeaponName(record unit.Record) string {
	stack, ok := record.Inventory.Equipment["weapon"]
	if !ok || strings.TrimSpace(stack.ItemID) == "" {
		return ""
	}
	if strings.TrimSpace(stack.CustomName) != "" {
		return stack.CustomName
	}
	return displayItemName(stack.ItemID)
}

// actionNarrationEventSummary 组装动作叙述事件摘要，供日志与气泡复用。
func actionNarrationEventSummary(
	state State,
	actorUnitID string,
	decision unitDecisionPayload,
	logStart int,
) string {
	if logStart < 0 {
		logStart = 0
	}
	if logStart > len(state.Logs) {
		logStart = len(state.Logs)
	}

	lines := make([]string, 0, 3)
	for index := logStart; index < len(state.Logs); index++ {
		entry := state.Logs[index]
		if strings.TrimSpace(entry.Message) == "" {
			continue
		}
		if entry.Kind == "speech" || entry.Kind == "action_narration" || entry.Kind == "action_narration_error" {
			continue
		}
		if strings.TrimSpace(entry.ActorUnitID) != "" && entry.ActorUnitID != actorUnitID {
			continue
		}
		lines = append(lines, strings.TrimSpace(entry.Message))
		if len(lines) >= 3 {
			break
		}
	}
	if len(lines) == 0 {
		fallback := strings.TrimSpace(decision.NextAction)
		if fallback == "" {
			fallback = strings.TrimSpace(decision.Speak)
		}
		if fallback == "" {
			fallback = strings.TrimSpace(decision.Reasoning)
		}
		return fallback
	}
	return strings.Join(lines, "；")
}

// isDefiantAction 判断当前动作是否属于“抗命”类型。
func isDefiantAction(compliance obedienceResolution) bool {
	return compliance.State != obedienceSteady
}

// actionValidator 按动作类型返回对应的参数校验函数。
func (service *Service) actionValidator(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	decision unitDecisionPayload,
	remainingAP int,
) error {
	return validateDecision(state, byID, actor, targetIDs, decision, remainingAP)
}

// actionExecutor 按动作类型返回对应的执行函数。
func (service *Service) actionExecutor(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	decision unitDecisionPayload,
	modifiers actionModifiers,
) error {
	return service.executeDecision(ctx, state, byID, actor, targetIDs, decision, modifiers)
}

// executeDecision 执行单个决策，并统一处理失败兜底与日志。
func (service *Service) executeDecision(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	decision unitDecisionPayload,
	modifiers actionModifiers,
) error {
	if isUnitPregnant(*state, actor.ID) && pregnancyBlockedAction(decision.Action) {
		appendLog(state, "pregnancy_hold", "我正在孕期，不能参与战斗或建筑。", actor.ID, "")
		return service.applyActionHungerCost(ctx, state, actor, "孕期休整")
	}
	// Freeze List 拦截（freeze_list.go，flag QUNXIANG_FREEZE_LIST 默认关 → 整段 no-op）：高代价离线处置
	// （卖/赠传家宝、触碰离线宪章红线、违背社交禁令）命中时绝不直接落地，转命运待决策上交玩家定夺，
	// 单位本回合回退安全决策（继续待命）。best-effort：判定/上交失败一律退回正常落地，绝不中断主循环。
	if service.maybeFreezeOfflineAction(ctx, state, actor, decision) {
		return nil
	}
	switch decision.Action {
	case DecisionActionAttack:
		return service.executeEngage(ctx, state, byID, actor, targetIDs, decision, modifiers)
	case DecisionActionCharge:
		return service.executeChargeEngage(ctx, state, byID, actor, targetIDs, decision, modifiers)
	case DecisionActionHeavyAttack:
		return service.executeHeavyEngage(ctx, state, byID, actor, targetIDs, decision, modifiers)
	case DecisionActionSkill:
		return service.executeSkill(ctx, state, byID, actor, decision, modifiers)
	case DecisionActionDefend:
		return service.executeDefend(ctx, state, actor, decision)
	case DecisionActionObserve:
		return service.executeObserve(ctx, state, actor, decision)
	case DecisionActionAssist:
		return service.executeAssist(ctx, state, byID, actor, decision)
	case DecisionActionSay:
		return service.executeSay(ctx, state, byID, actor, decision)
	case DecisionActionDialogue:
		return service.executeDialogue(ctx, state, byID, actor, decision)
	case DecisionActionTrade:
		return service.executeTrade(ctx, state, byID, actor, decision)
	case DecisionActionRomance:
		return service.executeRomance(ctx, state, byID, actor, decision)
	case DecisionActionFamily:
		return service.executeFamily(ctx, state, byID, actor, decision)
	case DecisionActionBuild:
		return service.executeBuild(ctx, state, actor, decision)
	case DecisionActionDemolish:
		return service.executeDemolish(ctx, state, actor, decision)
	case DecisionActionGather:
		return service.executeGather(ctx, state, actor, decision)
	case DecisionActionForge:
		return service.executeForge(ctx, state, actor, decision)
	case DecisionActionUpgrade:
		return service.executeUpgrade(ctx, state, actor, decision)
	case DecisionActionEquip:
		return service.executeEquip(ctx, state, actor, decision)
	case DecisionActionEat:
		return service.executeEat(ctx, state, actor, decision)
	case DecisionActionPickup:
		return service.executePickup(ctx, state, actor, decision)
	case DecisionActionMove:
		return service.executeMove(
			ctx,
			state,
			byID,
			actor,
			world.Coord{Q: decision.TargetQ, R: decision.TargetR},
			decision,
			modifiers,
		)
	default:
		message := decisionLogText(decision)
		if message != "" {
			appendLog(state, "hold", message, actor.ID, "")
		}
		return nil
	}
}

// maybeFreezeOfflineAction 在动作落地前判定是否该把它冻结上交命运待决策（Freeze List，freeze_list.go）。
// 返回 true 表示「已拦截、动作不应落地」（调用方应直接 return nil，单位本回合回退安全决策）。
//
// flag QUNXIANG_FREEZE_LIST 默认关 → 直接返回 false（零行为变化）。仅对「高代价离线处置」类动作构造判定：
//   - 交易/赠予（DecisionActionTrade）：从单位库存解析标的物的 Pinned 标记（传家宝硬门）+ 宪章红线/社交禁令匹配；
//   - 表白/家庭（romance/family）与（未来的）叛变：映射到 decision.GatedAction，由 GateSurprise 决定是否须上交玩家。
//
// best-effort：判定纯函数确定性；上交命运失败（FreezeAndSurrenderToFate 返回 error）一律退回正常落地（返回 false），
// 绝不中断主循环、绝不因拦截链异常吞掉单位的正常行动。
func (service *Service) maybeFreezeOfflineAction(ctx context.Context, state *State, actor *unit.Record, dec unitDecisionPayload) bool {
	if service == nil || state == nil || actor == nil || !freezeListEnabled() {
		return false
	}
	itemID := strings.TrimSpace(dec.ItemID)
	itemPinned := itemID != "" && actorItemPinned(*actor, itemID)
	kind := gatedActionForDecision(dec, itemPinned)
	isDisposalTrade := tradeIsDisposal(dec)
	// 仅「门控转折动作（恋爱/卖传家宝）」或「交出己方资产的处置类交易（卖/赠/付金币）」才进入判定；
	// 普通买入、非交易动作直接放行（修：此前把所有交易无差别标为高代价处置 → 开 flag 后连买入/卖一捆草料都被冻结）。
	if kind == "" && !isDisposalTrade {
		return false
	}
	charter, _ := GetUnitCharter(state, actor.ID)
	action := FreezeAction{
		Kind:            kind,
		ItemID:          itemID,
		ItemName:        strings.TrimSpace(dec.ItemName),
		ItemPinned:      itemPinned,
		TargetFactionID: "",
		Intent:          decisionLogText(dec),
		// IsHighStakesDisposal 不再靠裸「是否交易」判定（会误伤普通买卖）；改由 Pinned 标记 / 宪章红线 /
		// 社交禁令三条确定信号在 shouldFreezeAction 内拦截。裸金额阈值易误伤，故此处恒 false。
		IsHighStakesDisposal: false,
	}
	res := shouldFreezeAction(charter, action)
	if !res.Freeze {
		return false
	}
	if _, err := service.FreezeAndSurrenderToFate(ctx, state.ID, actor, action, res); err != nil {
		// 上交失败：退回正常落地，绝不让单位本回合凭空丢动作。
		appendLog(state, "freeze_skip", "她本想自作主张处置一件大事，但我没拦下来，仍按原计划行事。", actor.ID, "")
		return false
	}
	appendLog(state, "freeze_intercept", fmt.Sprintf("我替你拦下了 %s 正要自作主张的大事，已转交你定夺。", actor.DisplayName()), actor.ID, "")
	return true
}

// gatedActionForDecision 把决策动作映射到 decision.GatedAction（门控转折动作）；非门控返回空串。
// 交易仅在「处置类（卖/赠/付金币）且标的带 Pinned 标记」时才视作 sell_pinned 门控——普通买卖不进门控，
// 否则 GateSurprise 会把每一笔交易当「卖锚物」拦下（修：此前对所有交易恒返回 ActionSellPinned）。
func gatedActionForDecision(dec unitDecisionPayload, itemPinned bool) decision.GatedAction {
	switch dec.Action {
	case DecisionActionRomance:
		return decision.ActionRomance
	case DecisionActionTrade:
		if itemPinned && tradeIsDisposal(dec) {
			return decision.ActionSellPinned
		}
		return ""
	default:
		return ""
	}
}

// tradeIsDisposal 判定一笔交易是否属「交出己方资产/钱财」的处置方向（卖出/赠出/付金币）——
// 买入/换入不算处置。仅处置方向的交易才进入 Freeze List 的红线/禁令/Pinned 判定（普通买卖不冻）。
func tradeIsDisposal(dec unitDecisionPayload) bool {
	if dec.Action != DecisionActionTrade {
		return false
	}
	switch dec.TradeKind {
	case TradeActionKindSell, TradeActionKindGift, TradeActionKindGold:
		return true
	default:
		return false
	}
}

// actorItemPinned 检查单位库存（装备栏 + 背包）中某物品是否被标记为 Pinned（传家宝硬门）。
func actorItemPinned(record unit.Record, itemID string) bool {
	itemID = strings.ToLower(strings.TrimSpace(itemID))
	if itemID == "" {
		return false
	}
	for _, stack := range record.Inventory.Equipment {
		if strings.ToLower(strings.TrimSpace(stack.ItemID)) == itemID && stack.Pinned {
			return true
		}
	}
	for _, stack := range record.Inventory.Backpack {
		if strings.ToLower(strings.TrimSpace(stack.ItemID)) == itemID && stack.Pinned {
			return true
		}
	}
	return false
}

// decisionLogText 按优先级提取可展示的 AI 文本（next_action/speak/memory/reasoning）。
func decisionLogText(decision unitDecisionPayload) string {
	return strings.TrimSpace(firstNonEmptyText(
		decision.NextAction,
		decision.Speak,
		decision.Memory,
		decision.Reasoning,
	))
}

// executeMove 执行移动动作，并处理路径占位、陷阱与饥饿消耗。
func (service *Service) executeMove(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	target world.Coord,
	decision unitDecisionPayload,
	modifiers actionModifiers,
) error {
	aiText := decisionLogText(decision)
	moveMultiplier := modifiers.MoveMultiplier * weatherAdjustedMoveMultiplier(*state, *actor)
	if effectiveMoveRange(actor.Status.Move, moveMultiplier) <= 0 {
		message := aiText
		if message == "" {
			message = fmt.Sprintf("我想去 %d,%d，但这回合机动力不够。", target.Q, target.R)
		}
		appendLog(
			state,
			"move_blocked",
			message,
			actor.ID,
			"",
		)
		return nil
	}

	steps, err := moveActorToward(state.Map, byID, actor, target, 1, 0)
	if err != nil {
		return err
	}
	if steps == 0 {
		message := aiText
		if message == "" {
			message = fmt.Sprintf("我想去 %d,%d，但这回合没走出有效路线。", target.Q, target.R)
		}
		appendLog(
			state,
			"move_blocked",
			message,
			actor.ID,
			"",
		)
		return nil
	}
	if err := service.units.Save(ctx, *actor); err != nil {
		return err
	}
	if err := service.applyActionHungerCost(ctx, state, actor, "行军"); err != nil {
		return err
	}
	if triggered, err := service.triggerTrapAt(ctx, state, actor); err != nil {
		return err
	} else if triggered && !isBattleReady(*actor) {
		return nil
	}

	appendLog(
		state,
		"move",
		strings.TrimSpace(firstNonEmptyText(
			aiText,
			fmt.Sprintf("我移动到 %d,%d。", actor.Status.PositionQ, actor.Status.PositionR),
		)),
		actor.ID,
		"",
	)
	return nil
}

// combatAttackStyle 结构体用于承载该模块的核心数据。
type combatAttackStyle struct {
	Label            string
	DamageMultiplier float64
	MissChance       float64
}

// 变量定义区：集中声明该文件使用的共享配置。
var (
	normalAttackStyle = combatAttackStyle{
		Label:            "攻击",
		DamageMultiplier: 1,
	}
	heavyAttackStyle = combatAttackStyle{
		Label:            "重击",
		DamageMultiplier: 1.5,
		MissChance:       0.2,
	}
)

// executeEngage 执行普通攻击入口。
func (service *Service) executeEngage(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	decision unitDecisionPayload,
	modifiers actionModifiers,
) error {
	return service.executeEngageWithStyle(ctx, state, byID, actor, targetIDs, decision, modifiers, normalAttackStyle, 1)
}

// executeChargeEngage 执行冲锋攻击入口。
func (service *Service) executeChargeEngage(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	decision unitDecisionPayload,
	modifiers actionModifiers,
) error {
	return service.executeEngageWithStyle(ctx, state, byID, actor, targetIDs, decision, modifiers, combatAttackStyle{
		Label:            "冲锋",
		DamageMultiplier: 1.1,
	}, 2)
}

// executeHeavyEngage 执行重击攻击入口。
func (service *Service) executeHeavyEngage(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	decision unitDecisionPayload,
	modifiers actionModifiers,
) error {
	return service.executeEngageWithStyle(ctx, state, byID, actor, targetIDs, decision, modifiers, heavyAttackStyle, 1)
}

// executeEngageWithStyle 统一处理不同攻击风格的命中、伤害与击杀结算。
func (service *Service) executeEngageWithStyle(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	decision unitDecisionPayload,
	modifiers actionModifiers,
	style combatAttackStyle,
	maxAdvanceSteps int,
) error {
	if strings.TrimSpace(decision.StructureID) != "" && strings.TrimSpace(decision.TargetUnitID) == "" {
		return service.executeEngageStructureWithStyle(ctx, state, byID, actor, decision, modifiers, style, maxAdvanceSteps)
	}
	aiText := decisionLogText(decision)
	preferredTargetID := decision.TargetUnitID
	target := resolveTarget(targetIDs, byID, preferredTargetID, actor)
	if target == nil {
		appendLog(
			state,
			"hold",
			strings.TrimSpace(firstNonEmptyText(
				aiText,
				"我暂时没找到可追击目标。",
			)),
			actor.ID,
			"",
		)
		return nil
	}
	reach := attackReachWithWeather(*state, *actor)
	moveLimit := effectiveMoveRange(actor.Status.Move, modifiers.MoveMultiplier*weatherAdjustedMoveMultiplier(*state, *actor))

	before := world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}
	steps := 0
	if maxAdvanceSteps < 0 {
		maxAdvanceSteps = 0
	}
	if moveLimit > 0 && maxAdvanceSteps > 0 {
		advanceSteps := maxAdvanceSteps
		if advanceSteps > moveLimit {
			advanceSteps = moveLimit
		}
		advanceSteps = weatherAdjustedAdvanceSteps(*state, *actor, advanceSteps)
		var err error
		steps, err = moveActorToward(
			state.Map,
			byID,
			actor,
			world.Coord{Q: target.Status.PositionQ, R: target.Status.PositionR},
			advanceSteps,
			reach,
		)
		if err != nil {
			return err
		}
	}
	if steps > 0 {
		if err := service.units.Save(ctx, *actor); err != nil {
			return err
		}
		appendLog(
			state,
			"move",
			strings.TrimSpace(firstNonEmptyText(
				aiText,
				fmt.Sprintf("我从 %d,%d 压向 %d,%d。", before.Q, before.R, actor.Status.PositionQ, actor.Status.PositionR),
			)),
			actor.ID,
			target.ID,
		)
		if err := service.applyActionHungerCost(ctx, state, actor, style.Label+"逼近"); err != nil {
			return err
		}
		if triggered, err := service.triggerTrapAt(ctx, state, actor); err != nil {
			return err
		} else if triggered && !isBattleReady(*actor) {
			return nil
		}
	}

	target = resolveTarget(targetIDs, byID, preferredTargetID, actor)
	if target == nil {
		appendLog(
			state,
			"hold",
			strings.TrimSpace(firstNonEmptyText(
				aiText,
				"我逼近后发现目标已经失去战斗力。",
			)),
			actor.ID,
			"",
		)
		return nil
	}
	reach = attackReachWithWeather(*state, *actor)
	if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR) > reach {
		appendLog(
			state,
			"advance",
			strings.TrimSpace(firstNonEmptyText(
				aiText,
				fmt.Sprintf("我继续朝 %s 逼近。", target.DisplayName()),
			)),
			actor.ID,
			target.ID,
		)
		return nil
	}
	return service.applyAttack(ctx, state, byID, actor, target, modifiers.AttackMultiplier, style, aiText)
}

// executeDefend 执行防御动作并附加临时防御收益。
func (service *Service) executeDefend(
	ctx context.Context,
	state *State,
	actor *unit.Record,
	decision unitDecisionPayload,
) error {
	logText := decisionLogText(decision)
	if grantCombatEffect(actor, combatEffectGuarded, state.TurnState.Turn+1) {
		if err := service.units.Save(ctx, *actor); err != nil {
			return err
		}
	}
	if err := service.applyActionHungerCost(ctx, state, actor, "防御姿态"); err != nil {
		return err
	}
	appendLog(
		state,
		"defend",
		strings.TrimSpace(firstNonEmptyText(
			logText,
			"我进入防御姿态，直到下回合首次受击前伤害降低。",
		)),
		actor.ID,
		"",
	)
	return nil
}

// executeObserve 执行观察动作并生成侦察信息。
func (service *Service) executeObserve(
	ctx context.Context,
	state *State,
	actor *unit.Record,
	decision unitDecisionPayload,
) error {
	logText := decisionLogText(decision)
	if grantCombatEffect(actor, combatEffectFocused, state.TurnState.Turn+1) {
		if err := service.units.Save(ctx, *actor); err != nil {
			return err
		}
	}
	if err := service.applyActionHungerCost(ctx, state, actor, "观察敌情"); err != nil {
		return err
	}
	appendLog(
		state,
		"observe",
		strings.TrimSpace(firstNonEmptyText(
			logText,
			"我先观察并校准攻击节奏，下次攻击更精准。",
		)),
		actor.ID,
		"",
	)
	return nil
}

// executeAssist 执行支援动作（如补给/协防等）。
func (service *Service) executeAssist(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	decision unitDecisionPayload,
) error {
	logText := decisionLogText(decision)
	ally, ok := byID[decision.TargetUnitID]
	if !ok || !isBattleReady(*ally) || ally.FactionID != actor.FactionID || ally.ID == actor.ID {
		appendLog(
			state,
			"hold",
			strings.TrimSpace(firstNonEmptyText(
				logText,
				"我想援助队友，但目标无效。",
			)),
			actor.ID,
			decision.TargetUnitID,
		)
		return nil
	}
	if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, ally.Status.PositionQ, ally.Status.PositionR) > 1 {
		appendLog(
			state,
			"hold",
			strings.TrimSpace(firstNonEmptyText(
				logText,
				fmt.Sprintf("我想援助 %s，但距离过远。", ally.DisplayName()),
			)),
			actor.ID,
			ally.ID,
		)
		return nil
	}

	if grantCombatEffect(ally, combatEffectAssisted, state.TurnState.Turn+1) {
		if err := service.units.Save(ctx, *ally); err != nil {
			return err
		}
	}
	if err := service.applyStatusMutation(
		ctx,
		state,
		ally,
		status.FieldMorale,
		0.04,
		events.ReasonCommandAccepted,
		fmt.Sprintf("我获得了 %s 的临阵支援。", actor.DisplayName()),
	); err != nil {
		return err
	}
	if err := service.applyActionHungerCost(ctx, state, actor, "援助队友"); err != nil {
		return err
	}
	appendLog(
		state,
		"assist",
		strings.TrimSpace(firstNonEmptyText(
			logText,
			fmt.Sprintf("我为 %s 提供掩护与鼓劲。", ally.DisplayName()),
		)),
		actor.ID,
		ally.ID,
	)
	assistDelta := relationDelta{
		Trust:     0.68,
		Fear:      -0.16,
		Affection: 0.44,
		Rivalry:   -0.18,
	}
	helpedDelta := relationDelta{
		Trust:     1.12,
		Fear:      -0.26,
		Affection: 0.78,
		Rivalry:   -0.24,
	}
	service.applyMutualRelationShiftBestEffort(ctx, state, actor, ally, assistDelta, helpedDelta, "临阵援助")
	return nil
}

// applyAttack 应用一次攻击结算，返回伤害与击倒结果。
func (service *Service) applyAttack(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	attacker *unit.Record,
	target *unit.Record,
	attackMultiplier float64,
	style combatAttackStyle,
	aiText string,
) error {
	aiText = strings.TrimSpace(aiText)
	targetHPBefore := target.Status.HP
	effectiveMultiplier := attackMultiplier * style.DamageMultiplier
	if hasCombatEffect(*attacker, combatEffectFocused, state.TurnState.Turn) {
		effectiveMultiplier *= 1.2
	}
	if hasCombatEffect(*attacker, combatEffectRage, state.TurnState.Turn) {
		effectiveMultiplier *= 1.2
	}
	if hasCombatEffect(*target, combatEffectGuarded, state.TurnState.Turn) {
		effectiveMultiplier *= 0.5
	}
	if hasCombatEffect(*target, combatEffectAssisted, state.TurnState.Turn) {
		effectiveMultiplier *= 0.75
	}
	if hasCombatEffect(*target, combatEffectRage, state.TurnState.Turn) {
		effectiveMultiplier *= 1.2
	}
	effectiveMultiplier *= weatherAdjustedAttackMultiplier(*state, *attacker, *target)

	if err := service.applyActionHungerCost(ctx, state, attacker, style.Label); err != nil {
		return err
	}
	if style.MissChance > 0 && combatActionRoll(*state, *attacker, *target, style.Label) < style.MissChance {
		message := strings.TrimSpace(firstNonEmptyText(
			aiText,
			fmt.Sprintf("我对 %s 发起%s，但落空了。", target.DisplayName(), style.Label),
		))
		appendLog(
			state,
			"attack_miss",
			message,
			attacker.ID,
			target.ID,
		)
		return nil
	}

	damage := scaledDamage(calculateDamage(state.Map, state.Structures, *attacker, *target), effectiveMultiplier)
	if damage < 1 {
		damage = 1
	}

	reasonText := fmt.Sprintf("我以%s命中 %s", style.Label, target.DisplayName())
	if style.Label == "攻击" {
		reasonText = fmt.Sprintf("我命中 %s", target.DisplayName())
	}

	reasonCode := events.ReasonCombatHit
	if target.Status.HP-damage <= 0 {
		reasonCode = events.ReasonCombatDown
	}

	result, err := service.mutator.Apply(ctx, status.Mutation{
		UnitID:     target.ID,
		Turn:       state.TurnState.Turn,
		Field:      status.FieldHP,
		Delta:      -float64(damage),
		ReasonCode: reasonCode,
		ReasonText: reasonText,
		Actors:     []string{attacker.ID},
		Location:   fmt.Sprintf("hex_%d_%d", target.Status.PositionQ, target.Status.PositionR),
		// §7.3 events 作用域双写：战斗伤害/倒地是 region 查询/离线 catch-up 最关键的事件源，填世界三键。
		Scope: mutationScopeFromState(state),
	})
	if err != nil {
		return fmt.Errorf("apply attack: %w", err)
	}
	appendRawEvent(state, rawEventSpec{
		source:       "status",
		kind:         string(result.Payload.Field),
		summary:      result.Payload.ReasonText,
		actorUnitID:  attacker.ID,
		targetUnitID: target.ID,
		payload:      result.Payload,
	})
	appendLog(
		state,
		"stat_change",
		fmt.Sprintf(
			"%s 触发 %s 的 %s %.2f (%.2f -> %.2f) [%s]",
			attacker.DisplayName(),
			target.DisplayName(),
			result.Payload.Field,
			result.Payload.Delta,
			result.Payload.Before,
			result.Payload.After,
			result.Payload.ReasonCode,
		),
		attacker.ID,
		target.ID,
	)

	*target = result.Record
	attackerDelta := relationDelta{
		Trust:     -0.34,
		Fear:      0.04,
		Affection: -0.28,
		Rivalry:   1.36,
	}
	targetDelta := relationDelta{
		Trust:     -1.20,
		Fear:      2.20,
		Affection: -0.50,
		Rivalry:   2.80,
	}
	if target.Status.HP == 0 {
		targetDelta.Fear += 0.80
		targetDelta.Rivalry += 0.90
	}
	service.applyMutualRelationShiftBestEffort(ctx, state, attacker, target, attackerDelta, targetDelta, "正面交战")
	if target.Status.HP == 0 && target.Status.LifeState == unit.LifeStateActive {
		if err := unit.ApplyFatalDamage(target); err != nil {
			return err
		}
		if err := service.units.Save(ctx, *target); err != nil {
			return err
		}
		if target.Status.LifeState == unit.LifeStateDead {
			// 灵魂绑定/传家遗物先于普通战利品继承：转移给在乎死者的人（best-effort，绝不影响结算）。
			// 关键顺序：必须在 resolveKillLoot 之前——后者经 LootInheritor 会清空死者背包把普通物分给凶手/地面；
			// 配合 loot_inheritor.go 跳过 SoulBound/IsLegacy 件，使遗物→亲密继承人、普通物→凶手，两路不重叠不丢件。
			_, _ = service.inheritLegacyItems(ctx, state, *target)
			if err := service.resolveKillLoot(ctx, state, attacker, target); err != nil {
				return err
			}
			// 把她的死按相关性路由进「在乎她的人」的命运收件箱（best-effort，绝不影响战斗结算）。
			// 带上凶手本次 LLM 决策的归因因果句（§5.4）：让死亡卡的「为什么」是当次决策因果而非启发式模板（recall 为空则回落）。
			_, _ = service.WorldizeDeath(ctx, state.ID, *target, service.recallDecisionNarrative(attacker.ID))
			// 阵亡产品埋点（best-effort）：留存/牵挂信号，进 product_events 供北极星/漏斗聚合。
			_ = analytics.Emit(ctx, service.db, analytics.Event{
				Stage: analytics.StageRetention, Name: analytics.EventCharacterDied,
				SessionID: state.ID, UnitID: target.ID,
				Props: map[string]any{"turn": state.TurnState.Turn, "killer_id": attacker.ID},
			})
			// 血仇传播（flag-gated 默认开，QUNXIANG_BLOOD_FEUD=false 才关 + best-effort）：在乎死者的人继承对凶手 attacker 的敌意，
			// 最亲近者哀恸、世仇留痕并投「为TA复仇？」命运卡。绝不影响战斗结算。
			// 传入 byID（执行主循环持有的活指针映射），令哀恸 morale 的 Mutator 结果能回写内存态，
			// 避免后续 units.Save(*actor/*ally) 用旧内存态覆盖落库的悲恸（保持内存↔DB 一致）。
			service.propagateBloodFeud(ctx, state, *target, attacker.ID, state.WorldID, byID)
			// 接入世界后：这一击作为跨玩家事件写进不可篡改的世界总线（best-effort，gate 在 WorldID）。
			service.recordWorldizedKill(ctx, state, attacker, target)
			// 编年史关键事件留痕（chronicle.go，best-effort）：一场死亡是双方传记里的「回到那一刻」锚点。
			// 给凶手与逝者各记一笔，吞错不影响结算（listChronicle/anchorMoment 读侧供命运 Copilot/分享传记卡取材）。
			_ = service.recordChronicleEntry(ctx, state.ID, attacker.ID, state.TurnState.Turn, "kill",
				fmt.Sprintf("我在第 %d 回合的交战中击倒了 %s。", state.TurnState.Turn, target.DisplayName()))
			_ = service.recordChronicleEntry(ctx, state.ID, target.ID, state.TurnState.Turn, "death",
				fmt.Sprintf("我在第 %d 回合倒在了 %s 手中。", state.TurnState.Turn, attacker.DisplayName()))
			// 重大经历人格漂移（personality_drift.go，ordeal）：杀人与濒死都会让性情悄然改变。best-effort、确定性、封顶。
			// 逝者已 LifeStateDead，对其漂移仍有意义（落她生前最后一次性情记号供回响/传记），步长极小且经流程事件留痕。
			_, _ = service.ApplyPersonalityDrift(ctx, state.ID, attacker.ID, DriftReasonOrdeal, state.TurnState.Turn)
			_, _ = service.ApplyPersonalityDrift(ctx, state.ID, target.ID, DriftReasonOrdeal, state.TurnState.Turn)
		}

		appendLog(
			state,
			"attack",
			strings.TrimSpace(firstNonEmptyText(
				aiText,
				fmt.Sprintf("我对 %s 发起%s造成 %d 伤害，%s 倒下。", target.DisplayName(), style.Label, damage, target.DisplayName()),
			)),
			attacker.ID,
			target.ID,
		)
		if err := service.applyCombatGroupDynamics(ctx, state, byID, attacker, target, targetHPBefore, target.Status.HP); err != nil {
			return err
		}
		return nil
	}

	appendLog(
		state,
		"attack",
		strings.TrimSpace(firstNonEmptyText(
			aiText,
			fmt.Sprintf("我对 %s 发起%s造成 %d 伤害，目标剩余 %d HP。", target.DisplayName(), style.Label, damage, target.Status.HP),
		)),
		attacker.ID,
		target.ID,
	)
	if err := service.applyCombatGroupDynamics(ctx, state, byID, attacker, target, targetHPBefore, target.Status.HP); err != nil {
		return err
	}
	return nil
}

// resolveKillLoot 在单位被击杀后触发战利品继承；具体保留哪些物资由 LootInheritor 交给 LLM/规则回退决定。
func (service *Service) resolveKillLoot(
	ctx context.Context,
	state *State,
	attacker *unit.Record,
	target *unit.Record,
) error {
	if service == nil || state == nil || attacker == nil || target == nil {
		return nil
	}
	resolver := combatdomain.NewLootInheritor(service.db, service.units, service.llm)
	result, err := resolver.Resolve(ctx, combatdomain.ResolveRequest{
		KillerUnitID: attacker.ID,
		VictimUnitID: target.ID,
		Location:     fmt.Sprintf("hex_%d_%d", target.Status.PositionQ, target.Status.PositionR),
	})
	if err != nil {
		return fmt.Errorf("resolve kill loot: %w", err)
	}
	*attacker = result.Killer
	*target = result.Victim
	now := time.Now().UTC()
	state.GraveMarkers = append(state.GraveMarkers, GraveMarker{
		ID:        uuid.NewString(),
		UnitID:    target.ID,
		UnitName:  target.DisplayName(),
		FactionID: target.FactionID,
		Q:         target.Status.PositionQ,
		R:         target.Status.PositionR,
		Turn:      state.TurnState.Turn,
		CreatedAt: now,
	})
	if len(result.DroppedItems) > 0 {
		state.GroundLootDrops = append(state.GroundLootDrops, GroundLootDrop{
			ID:             firstNonEmptyText(result.DropID, uuid.NewString()),
			Q:              target.Status.PositionQ,
			R:              target.Status.PositionR,
			SourceUnitID:   target.ID,
			SourceUnitName: target.DisplayName(),
			InheritorID:    attacker.ID,
			Items:          result.DroppedItems,
			Turn:           state.TurnState.Turn,
			CreatedAt:      now,
		})
	}

	keptText := formatLootKeptCandidates(result.KeptCandidates)
	if keptText == "" {
		keptText = "没有可带走的物资"
	}
	message := fmt.Sprintf("我检查了 %s 的遗留物，带走了%s。", target.DisplayName(), keptText)
	if len(result.DroppedItems) > 0 {
		message = fmt.Sprintf("%s 其余物资散落在原地。", message)
	}
	appendLog(state, "loot", message, attacker.ID, target.ID)
	appendRawEvent(state, rawEventSpec{
		source:       "combat",
		kind:         "loot",
		summary:      message,
		actorUnitID:  attacker.ID,
		targetUnitID: target.ID,
		payload: map[string]any{
			"kept_candidates": result.KeptCandidates,
			"dropped_items":   result.DroppedItems,
			"drop_id":         result.DropID,
		},
	})
	return nil
}

// formatLootKeptCandidates 把本次继承的候选物资压缩成战报短句。
func formatLootKeptCandidates(candidates []combatdomain.Candidate) string {
	if len(candidates) == 0 {
		return ""
	}
	parts := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		name := strings.TrimSpace(candidate.DisplayName)
		if name == "" {
			name = candidate.ItemID
		}
		if candidate.Quantity > 1 {
			name = fmt.Sprintf("%s×%d", name, candidate.Quantity)
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, "、")
}

// executePickup 拾取当前地块上的地面掉落，背包满时保留无法带走的部分。
func (service *Service) executePickup(ctx context.Context, state *State, actor *unit.Record, decision unitDecisionPayload) error {
	drop := resolveGroundLootAtActor(*state, actor, decision.GroundLootID)
	if drop == nil {
		appendLog(state, "pickup_blocked", "我想拾取脚下物资，但这里已经没有可拿的东西。", actor.ID, "")
		return nil
	}
	kept := make([]unit.ItemStack, 0, len(drop.Items))
	remaining := make([]unit.ItemStack, 0)
	for _, stack := range drop.Items {
		if stack.ItemID == "" || stack.Quantity <= 0 {
			continue
		}
		if err := unit.AddBackpackItem(actor, stack.ItemID, stack.Quantity); err != nil {
			remaining = append(remaining, stack)
			continue
		}
		kept = append(kept, stack)
	}
	if len(kept) == 0 {
		appendLog(state, "pickup_blocked", fmt.Sprintf("我想拾取 %s，但背包已经装不下。", formatItemStacksWithEffects(drop.Items)), actor.ID, "")
		return nil
	}
	if err := service.units.Save(ctx, *actor); err != nil {
		return err
	}
	if err := service.applyActionHungerCost(ctx, state, actor, "拾取物资"); err != nil {
		return err
	}
	for index := range state.GroundLootDrops {
		if state.GroundLootDrops[index].ID != drop.ID {
			continue
		}
		if len(remaining) == 0 {
			state.GroundLootDrops = append(state.GroundLootDrops[:index], state.GroundLootDrops[index+1:]...)
		} else {
			state.GroundLootDrops[index].Items = remaining
		}
		break
	}
	appendLog(state, "pickup", fmt.Sprintf("我从地上拾取了 %s。", formatItemStacksWithEffects(kept)), actor.ID, "")
	return nil
}

func resolveGroundLootAtActor(state State, actor *unit.Record, groundLootID string) *GroundLootDrop {
	if actor == nil {
		return nil
	}
	for index := range state.GroundLootDrops {
		drop := &state.GroundLootDrops[index]
		if groundLootID != "" && drop.ID != groundLootID {
			continue
		}
		if drop.Q == actor.Status.PositionQ && drop.R == actor.Status.PositionR && !groundLootExpired(state, *drop) && len(drop.Items) > 0 {
			return drop
		}
	}
	return nil
}

func groundLootAtCoord(state State, q int, r int) []GroundLootDrop {
	drops := make([]GroundLootDrop, 0)
	for _, drop := range state.GroundLootDrops {
		if drop.Q == q && drop.R == r && !groundLootExpired(state, drop) && len(drop.Items) > 0 {
			drops = append(drops, drop)
		}
	}
	return drops
}

func groundLootExpired(state State, drop GroundLootDrop) bool {
	return state.TurnState.Turn-drop.Turn >= battlefieldRemnantTurns
}

func graveMarkerExpired(state State, marker GraveMarker) bool {
	return state.TurnState.Turn-marker.Turn >= battlefieldRemnantTurns
}

func expireBattlefieldRemnants(state *State) {
	if state == nil {
		return
	}
	drops := state.GroundLootDrops[:0]
	for _, drop := range state.GroundLootDrops {
		if !groundLootExpired(*state, drop) && len(drop.Items) > 0 {
			drops = append(drops, drop)
		}
	}
	state.GroundLootDrops = drops
}

func formatItemStacksBrief(stacks []unit.ItemStack) string {
	if len(stacks) == 0 {
		return "无"
	}
	parts := make([]string, 0, len(stacks))
	for _, stack := range stacks {
		if stack.ItemID == "" || stack.Quantity <= 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s x%d", displayItemName(stack.ItemID), stack.Quantity))
	}
	if len(parts) == 0 {
		return "无"
	}
	return strings.Join(parts, "、")
}

// applyCombatGroupDynamics 根据关系网与阵营状态施加群体战斗修正。
func (service *Service) applyCombatGroupDynamics(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	attacker *unit.Record,
	target *unit.Record,
	targetHPBefore int,
	targetHPAfter int,
) error {
	if attacker == nil || target == nil {
		return nil
	}

	targetDowned := targetHPAfter == 0
	targetCritical := targetHPAfter > 0 && targetHPAfter <= 25
	if !targetDowned && !targetCritical {
		return nil
	}

	downReason := events.ReasonCombatDown
	hitReason := events.ReasonCombatHit
	wasOneShot := targetDowned && targetHPBefore >= 70

	// 聚合「攻击者击倒提振」与「冲击半径内每个友军士气受挫」为一次 ApplyBatch，收敛 DB 往返。
	// 每条 mutation 的额外副作用（morale_shift 日志、挚友暴怒授予）经 after 闭包在该条标准副作用
	// 之后立即执行，保持与逐次 applyStatusMutation 路径完全一致的日志/事件交错顺序。
	shockMutations := make([]pendingStatusMutation, 0)

	if targetDowned {
		attackerRef := attacker
		targetName := target.DisplayName()
		attackerTargetID := target.ID
		shockMutations = append(shockMutations, pendingStatusMutation{
			record:     attacker,
			field:      status.FieldMorale,
			delta:      0.08,
			reasonCode: downReason,
			reasonText: fmt.Sprintf("我击倒了 %s，士气提振。", target.DisplayName()),
			after: func() error {
				appendLog(
					state,
					"morale_shift",
					fmt.Sprintf("我因成功击倒 %s，士气上升。", targetName),
					attackerRef.ID,
					attackerTargetID,
				)
				return nil
			},
		})
	}

	shockRadius := 2
	for _, alliedID := range alliedIDs(*state, target.FactionID) {
		if alliedID == target.ID {
			continue
		}
		ally, ok := byID[alliedID]
		if !ok || !isBattleReady(*ally) {
			continue
		}
		if unit.HexDistance(ally.Status.PositionQ, ally.Status.PositionR, target.Status.PositionQ, target.Status.PositionR) > shockRadius {
			continue
		}

		penalty := 0.0
		reasonText := ""
		reasonCode := hitReason
		bonded := isTrustedCompanion(*ally, *target) || service.hasBondRelation(ctx, ally.ID, target.ID)
		if targetDowned {
			penalty = -0.12
			reasonCode = downReason
			reasonText = fmt.Sprintf("我目睹队友 %s 倒下，士气受挫。", target.DisplayName())
		} else if targetCritical {
			penalty = -0.06
			reasonText = fmt.Sprintf("我看到队友 %s 濒死，出现明显动摇。", target.DisplayName())
		}
		if wasOneShot {
			penalty -= 0.06
			reasonText = fmt.Sprintf("我看到队友 %s 被重击秒倒，士气大幅波动。", target.DisplayName())
		}
		if bonded {
			penalty -= 0.08
			reasonText = fmt.Sprintf("我目睹挚友 %s 受创，情绪剧烈失衡。", target.DisplayName())
		}
		if penalty == 0 {
			continue
		}

		// 闭包捕获当前迭代变量（penalty / bonded / ally 指针），供批应用后回放副作用。
		allyRef := ally
		penaltyVal := penalty
		bondedVal := bonded
		shockTargetID := target.ID
		shockMutations = append(shockMutations, pendingStatusMutation{
			record:     ally,
			field:      status.FieldMorale,
			delta:      penalty,
			reasonCode: reasonCode,
			reasonText: reasonText,
			after: func() error {
				appendLog(
					state,
					"morale_shift",
					fmt.Sprintf("我受到同伴战损冲击，士气变化 %.2f。", penaltyVal),
					allyRef.ID,
					shockTargetID,
				)
				if bondedVal && grantCombatEffect(allyRef, combatEffectRage, state.TurnState.Turn+1) {
					if err := service.units.Save(ctx, *allyRef); err != nil {
						return err
					}
					appendLog(
						state,
						"rage",
						"我因挚友受创陷入暴怒，攻击升高但防守会更冒险。",
						allyRef.ID,
						shockTargetID,
					)
				}
				return nil
			},
		})
	}

	if err := service.applyStatusMutationsBatch(ctx, state, shockMutations); err != nil {
		return err
	}

	if !targetDowned {
		return nil
	}

	targetID := target.ID
	if isFactionLeader(*state, target.ID, target.FactionID) {
		// 队长阵亡：同阵营每个友军 -0.3 士气并各记一条 morale_shift 日志。各友军互相独立、无数据依赖，
		// 聚成一次 ApplyBatch；每条变更紧随的 morale_shift 日志经 after 闭包在该条标准副作用后立即回放，
		// 与逐次路径的日志/事件交错顺序完全一致。
		leaderMutations := make([]pendingStatusMutation, 0)
		for _, alliedID := range alliedIDs(*state, target.FactionID) {
			if alliedID == target.ID {
				continue
			}
			ally, ok := byID[alliedID]
			if !ok || !isBattleReady(*ally) {
				continue
			}
			allyRef := ally
			leaderMutations = append(leaderMutations, pendingStatusMutation{
				record:     ally,
				field:      status.FieldMorale,
				delta:      -0.3,
				reasonCode: downReason,
				reasonText: fmt.Sprintf("我目睹队长 %s 阵亡，士气重挫。", target.DisplayName()),
				after: func() error {
					appendLog(
						state,
						"morale_shift",
						"我因队长阵亡遭受全军冲击。",
						allyRef.ID,
						targetID,
					)
					return nil
				},
			})
		}
		if err := service.applyStatusMutationsBatch(ctx, state, leaderMutations); err != nil {
			return err
		}
	}
	if isFactionStandardBearer(*state, target.ID, target.FactionID) {
		// 旗手倒下：同阵营每个友军 -0.2 士气并各记一条 morale_shift 日志。同上聚成一次 ApplyBatch，
		// 日志经 after 闭包回放，保持与逐次路径完全一致的交错顺序。
		bearerMutations := make([]pendingStatusMutation, 0)
		for _, alliedID := range alliedIDs(*state, target.FactionID) {
			if alliedID == target.ID {
				continue
			}
			ally, ok := byID[alliedID]
			if !ok || !isBattleReady(*ally) {
				continue
			}
			allyRef := ally
			bearerMutations = append(bearerMutations, pendingStatusMutation{
				record:     ally,
				field:      status.FieldMorale,
				delta:      -0.2,
				reasonCode: downReason,
				reasonText: fmt.Sprintf("我目睹旗手 %s 被击溃，军心受到打击。", target.DisplayName()),
				after: func() error {
					appendLog(
						state,
						"morale_shift",
						"我因旗手倒下受到士气冲击。",
						allyRef.ID,
						targetID,
					)
					return nil
				},
			})
		}
		if err := service.applyStatusMutationsBatch(ctx, state, bearerMutations); err != nil {
			return err
		}
	}

	// 击倒方阵营冲击半径内的友军各 +0.05 士气并各记一条 morale_shift 日志。同上聚成一次 ApplyBatch，
	// 日志经 after 闭包回放，保持与逐次路径完全一致的交错顺序。
	attackerAllyMutations := make([]pendingStatusMutation, 0)
	for _, alliedID := range alliedIDs(*state, attacker.FactionID) {
		if alliedID == attacker.ID {
			continue
		}
		ally, ok := byID[alliedID]
		if !ok || !isBattleReady(*ally) {
			continue
		}
		if unit.HexDistance(ally.Status.PositionQ, ally.Status.PositionR, attacker.Status.PositionQ, attacker.Status.PositionR) > shockRadius {
			continue
		}

		allyRef := ally
		attackerAllyMutations = append(attackerAllyMutations, pendingStatusMutation{
			record:     ally,
			field:      status.FieldMorale,
			delta:      0.05,
			reasonCode: downReason,
			reasonText: fmt.Sprintf("我目睹 %s 击倒敌人，士气上扬。", attacker.DisplayName()),
			after: func() error {
				appendLog(
					state,
					"morale_shift",
					"我看到友军击倒敌人，士气提升。",
					allyRef.ID,
					targetID,
				)
				return nil
			},
		})
	}
	if err := service.applyStatusMutationsBatch(ctx, state, attackerAllyMutations); err != nil {
		return err
	}

	return nil
}

// isFactionLeader 判断单位是否属于阵营领袖角色。
func isFactionLeader(state State, unitID string, factionID string) bool {
	if unitID == "" {
		return false
	}
	if factionID == state.PlayerFactionID {
		return len(state.PlayerUnitIDs) > 0 && state.PlayerUnitIDs[0] == unitID
	}
	return len(state.EnemyUnitIDs) > 0 && state.EnemyUnitIDs[0] == unitID
}

// isFactionStandardBearer 判断单位是否属于阵营旗手角色。
func isFactionStandardBearer(state State, unitID string, factionID string) bool {
	if unitID == "" {
		return false
	}
	ids := alliedIDs(state, factionID)
	if len(ids) == 0 {
		return false
	}
	if len(ids) == 1 {
		return ids[0] == unitID
	}
	return ids[1] == unitID
}

// isTrustedCompanion 判断单位是否为可触发协同加成的受信同伴。
func isTrustedCompanion(observer unit.Record, target unit.Record) bool {
	if observer.ID == target.ID || observer.FactionID != target.FactionID {
		return false
	}
	if memoryContainsAny(observer, target.DisplayName(), target.Identity.Nickname, "挚友", "兄弟", "姐妹", "搭档", "救了我") {
		return true
	}
	if observer.Personality.Sociability < 0.72 || observer.Personality.Loyalty < 0.6 {
		return false
	}
	return unit.HexDistance(
		observer.Status.PositionQ,
		observer.Status.PositionR,
		target.Status.PositionQ,
		target.Status.PositionR,
	) <= 1
}

// syncCombatFlags 同步单位的 in_combat 状态标记。
func (service *Service) syncCombatFlags(ctx context.Context, state *State, units []unit.Record) error {
	if units == nil {
		loaded, err := service.units.ListBySession(ctx, state.ID)
		if err != nil {
			return err
		}
		units = loaded
	}

	combatActive := state.Outcome == OutcomeOngoing && state.TurnState.Phase == turns.PhaseExecution
	for index := range units {
		desired := combatActive && isBattleReady(units[index])
		if units[index].Status.InCombat == desired {
			continue
		}
		units[index].Status.InCombat = desired
		if err := service.units.Save(ctx, units[index]); err != nil {
			return err
		}
	}

	return nil
}

// ensureAIDecisionText 确保决策文本字段完整，避免前端展示空文案。
func (service *Service) ensureAIDecisionText(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	decision unitDecisionPayload,
) unitDecisionPayload {
	if state == nil || actor == nil {
		return decision
	}
	if strings.TrimSpace(decision.NextAction) != "" &&
		strings.TrimSpace(decision.Speak) != "" &&
		strings.TrimSpace(decision.Memory) != "" {
		return decision
	}

	payload, _, ok := service.recordUnitReflectionBestEffort(
		ctx,
		state,
		byID,
		actor,
		summarizeDecision(byID, decision),
		"reflection",
	)
	if ok {
		if strings.TrimSpace(decision.Speak) == "" {
			decision.Speak = payload.Bubble
		}
		if strings.TrimSpace(decision.Memory) == "" {
			decision.Memory = payload.Memory
		}
	}

	if strings.TrimSpace(decision.NextAction) == "" {
		switch {
		case strings.TrimSpace(decision.Speak) != "":
			decision.NextAction = limitTextRunes(strings.TrimSpace(decision.Speak), llmNextActionRuneLimit)
		case strings.TrimSpace(decision.Memory) != "":
			decision.NextAction = limitTextRunes(strings.TrimSpace(decision.Memory), llmNextActionRuneLimit)
		case strings.TrimSpace(decision.Reasoning) != "":
			decision.NextAction = limitTextRunes(strings.TrimSpace(decision.Reasoning), llmNextActionRuneLimit)
		}
	}
	if strings.TrimSpace(decision.Speak) == "" && strings.TrimSpace(decision.NextAction) != "" {
		decision.Speak = strings.TrimSpace(decision.NextAction)
	}
	if strings.TrimSpace(decision.Memory) == "" {
		switch {
		case strings.TrimSpace(decision.Speak) != "":
			decision.Memory = strings.TrimSpace(decision.Speak)
		case strings.TrimSpace(decision.NextAction) != "":
			decision.Memory = strings.TrimSpace(decision.NextAction)
		case strings.TrimSpace(decision.Reasoning) != "":
			decision.Memory = strings.TrimSpace(decision.Reasoning)
		}
	}
	return decision
}

// loadSession 加载会话状态以及当前在场单位数据。
func (service *Service) loadSession(ctx context.Context, sessionID string) (State, []unit.Record, error) {
	state, err := service.sessions.Get(ctx, sessionID)
	if err != nil {
		return State{}, nil, err
	}
	// 拆 state_json：决策轨迹 + LLM 交互 + 原始事件日志的权威读源已切到旁路表——回填旧局残留 + 从表 hydrate（须在任何 Save 之前，免旧局数据被瘦身丢掉）。
	service.hydrateDecisionTraces(ctx, &state)
	service.hydrateLLMInteractions(ctx, &state)
	service.hydrateRawEvents(ctx, &state)
	oldBudgets := state.TurnState.Budgets
	state.TurnState.Budgets = turns.NormalizeBudgets(state.TurnState.Budgets)
	if state.TurnState.Phase == turns.PhaseDeployment && oldBudgets.Deployment != state.TurnState.Budgets.Deployment {
		state.TurnState.PhaseEndsAt = state.TurnState.PhaseStartedAt.Add(state.TurnState.Budgets.Deployment)
	}
	if oldBudgets != state.TurnState.Budgets {
		if err := service.sessions.Save(ctx, &state); err != nil {
			return State{}, nil, err
		}
	}

	units, err := service.units.ListBySession(ctx, sessionID)
	if err != nil {
		return State{}, nil, err
	}

	return state, units, nil
}

// bootstrapBattleUnit 根据种子与阵营初始化单位档案与初始属性。
func bootstrapBattleUnit(seed int64, sessionID string, factionID string, name string, coord world.Coord) (unit.Record, error) {
	record := unit.BootstrapRecord(seed, sessionID, factionID, name)
	record.Status.PositionQ = coord.Q
	record.Status.PositionR = coord.R
	record.Status.InCombat = false

	for _, itemID := range []string{"long_sword", "cloth_armor", "cloth_shoes"} {
		if err := unit.AddItem(&record, itemID, 1); err != nil {
			return unit.Record{}, fmt.Errorf("equip %s on %s: %w", itemID, record.DisplayName(), err)
		}
	}

	return record, nil
}

func addOpeningSupply(record *unit.Record, index int) error {
	if record == nil {
		return nil
	}
	supplies := [][]string{{"ration"}, {"carrier_pigeon"}, {"rope"}, {"herb_bundle"}, {"pickaxe"}}
	for _, itemID := range supplies[index%len(supplies)] {
		if err := unit.AddBackpackItem(record, itemID, 1); err != nil {
			return err
		}
	}
	return nil
}

func appendOpeningSupplyLog(state *State, record unit.Record) {
	if state == nil || record.ID == "" || len(record.Inventory.Backpack) == 0 {
		return
	}
	appendLog(
		state,
		"opening_supply",
		fmt.Sprintf("%s 开局携带补给：%s。", record.DisplayName(), formatItemStacksWithEffects(record.Inventory.Backpack)),
		record.ID,
		"",
	)
}

// buildSnapshot 按当前状态构建可下发给前端的权威快照。
func buildSnapshot(state State, units []unit.Record) Snapshot {
	hiddenDeadUnitIDs := map[string]struct{}{}
	for _, marker := range state.GraveMarkers {
		if graveMarkerExpired(state, marker) {
			hiddenDeadUnitIDs[marker.UnitID] = struct{}{}
		}
	}
	expireBattlefieldRemnants(&state)
	byID := make(map[string]unit.Record, len(units))
	for _, record := range units {
		if _, hidden := hiddenDeadUnitIDs[record.ID]; hidden && record.Status.LifeState != unit.LifeStateActive {
			continue
		}
		byID[record.ID] = record
	}

	playerUnits := orderedUnits(state.PlayerUnitIDs, byID)
	enemyUnits := orderedUnits(state.EnemyUnitIDs, byID)
	if state.SetupPhase == SetupPhaseDrafting {
		if len(playerUnits) == 0 {
			limit := state.DraftRequiredPick
			if limit <= 0 || limit > len(state.PlayerDraftPool) {
				limit = len(state.PlayerDraftPool)
			}
			playerUnits = append([]unit.Record{}, state.PlayerDraftPool[:limit]...)
		}
		if len(enemyUnits) == 0 {
			enemyUnits = append([]unit.Record{}, state.EnemyDraftPool...)
		}
	}

	return Snapshot{
		ID:                  state.ID,
		WorldID:             state.WorldID,
		MinorMode:           state.MinorMode,
		Mode:                state.Mode,
		RandomSeed:          state.RandomSeed,
		PlayerFactionID:     state.PlayerFactionID,
		EnemyFactionID:      state.EnemyFactionID,
		SetupPhase:          state.SetupPhase,
		SetupDeadlineAt:     state.SetupDeadlineAt,
		DraftRequiredPick:   state.DraftRequiredPick,
		PlayerDraftPool:     append([]unit.Record{}, state.PlayerDraftPool...),
		EnemyDraftPool:      append([]unit.Record{}, state.EnemyDraftPool...),
		MapScriptID:         state.MapScriptID,
		MapScriptName:       state.MapScriptName,
		MapSizeID:           state.MapSizeID,
		MapSizeName:         state.MapSizeName,
		FogOfWarEnabled:     state.FogOfWarEnabled,
		RandomEventsEnabled: !state.RandomEventsDisabled,
		TurnState:           state.TurnState,
		PhaseReady:          cloneBoolMap(state.PhaseReady),
		ExecutionInProgress: state.ExecutionInProgress,
		Outcome:             state.Outcome,
		WinnerFactionID:     state.WinnerFactionID,
		VictoryPath:         state.VictoryPath,
		Weather:             state.Weather,
		Map:                 state.Map,
		CommandPower:        state.CommandPower,
		FactionRelations:    append([]FactionRelation{}, state.FactionRelations...),
		Structures:          append([]Structure{}, state.Structures...),
		GraveMarkers:        append([]GraveMarker{}, state.GraveMarkers...),
		GroundLootDrops:     append([]GroundLootDrop{}, state.GroundLootDrops...),
		GlobalDirective:     state.GlobalDirective,
		DirectiveHistory:    state.DirectiveHistory,
		DialogueHistory:     state.DialogueHistory,
		DecisionTraces:      state.DecisionTraces,
		LLMInteractions:     state.LLMInteractions,
		ActiveLLMCalls:      activeLLMInteractionsForState(state),
		PigeonQueue:         append([]PigeonDispatch{}, state.PigeonQueue...),
		Pregnancies:         append([]PregnancyState{}, state.Pregnancies...),
		BattleReports:       append([]BattleReport{}, state.BattleReports...),
		HallArchiveEntries:  append([]HallArchiveEntry{}, state.HallArchiveEntries...),
		IntelAssets:         append([]IntelAsset{}, state.IntelAssets...),
		IntelReports:        append([]IntelReport{}, state.IntelReports...),
		ModerationReports:   append([]ModerationReport{}, state.ModerationReports...),
		Metrics:             state.Metrics,
		RawEventLog:         append([]RawEventEntry{}, state.RawEventLog...),
		Logs:                state.Logs,
		PlayerUnits:         playerUnits,
		EnemyUnits:          enemyUnits,
		WildUnits:           orderedUnits(state.WildUnitIDs, byID),
	}
}

// PublicSnapshot returns the default lightweight payload used by normal game
// clients. Developer-only prompt/trace history is returned only when qxdev()
// asks for it explicitly.
func PublicSnapshot(snapshot Snapshot, includeDebug bool) Snapshot {
	if includeDebug {
		return snapshot
	}
	snapshot.LLMInteractions = publicLLMInteractions(snapshot.LLMInteractions)
	snapshot.ActiveLLMCalls = publicActiveLLMInteractions(snapshot.ActiveLLMCalls)
	snapshot.RawEventLog = redactRawEventLog(snapshot.RawEventLog)
	snapshot.ModerationReports = redactModerationReports(snapshot.ModerationReports)
	return snapshot
}

// RedactModerationReportForPublic 抹去单条举报记录里的隐私字段（Reporter 举报人、Detail 举报详情），返回脱敏副本。
// 与 redactModerationReports（脱敏整张快照列表）同口径，供 WS 广播 extra 里携带的 report 在下发前先脱敏——
// 否则 submit/resolve 广播会把举报人身份+详情原样推给该局**所有**订阅客户端，绕过 PublicSnapshot 的快照脱敏闸（隐私泄露）。
// 注意：reporter/detail 的 JSON tag 无 omitempty，置空仍会序列化为 "" 而非省略，但已不含敏感原文。
func RedactModerationReportForPublic(report ModerationReport) ModerationReport {
	report.Reporter = ""
	report.Detail = ""
	return report
}

// redactModerationReports 抹去举报记录里的隐私字段（Reporter 举报人、Detail 举报详情），只对**非调试/非 ops** 客户端生效。
// 这是「举报记录原样下发每个客户端」隐私漏洞的脱敏闸：普通玩家仍能看到「某单位/某类目下有 N 条举报、是否已处理」的事实
// （UI 需要这些做提示），但拿不到是谁举报的、举报了什么细节——后者只在 includeDebug（qxdev/ops）路径保留全量。
func redactModerationReports(reports []ModerationReport) []ModerationReport {
	if len(reports) == 0 {
		return reports
	}
	result := make([]ModerationReport, len(reports))
	copy(result, reports)
	for index := range result {
		result[index].Reporter = "" // 抹去举报人身份（防对线报复）
		result[index].Detail = ""   // 抹去举报详情（可能含被举报方/第三方的敏感原文）
	}
	return result
}

func publicActiveLLMInteractions(interactions []LLMInteraction) []LLMInteraction {
	if len(interactions) == 0 {
		return nil
	}
	return publicLLMInteractions(interactions)
}

func publicLLMInteractions(interactions []LLMInteraction) []LLMInteraction {
	if len(interactions) == 0 {
		return []LLMInteraction{}
	}
	result := make([]LLMInteraction, 0, len(interactions))
	for _, interaction := range interactions {
		interaction.SystemPrompt = ""
		interaction.UserPrompt = ""
		interaction.RawOutput = ""
		interaction.Attempts = nil
		result = append(result, interaction)
	}
	return result
}

func redactRawEventLog(entries []RawEventEntry) []RawEventEntry {
	if len(entries) == 0 {
		return []RawEventEntry{}
	}
	result := make([]RawEventEntry, len(entries))
	copy(result, entries)
	for index := range result {
		if result[index].Source == "llm" || result[index].Source == "decision" {
			result[index].PayloadJSON = ""
		}
	}
	return result
}

func activeLLMInteractionsForState(state State) []LLMInteraction {
	calls := ai.ActiveCalls()
	if len(calls) == 0 {
		return nil
	}
	interactions := make([]LLMInteraction, 0, len(calls))
	for _, call := range calls {
		if strings.TrimSpace(call.Metadata["session_id"]) != state.ID {
			continue
		}
		unitID := strings.TrimSpace(call.Metadata["unit_id"])
		kind := strings.TrimSpace(string(call.Task))
		if kind == "" {
			kind = strings.TrimSpace(call.SchemaName)
		}
		if kind == "" {
			kind = "llm"
		}
		interactions = append(interactions, LLMInteraction{
			ID:           call.ID,
			UnitID:       unitID,
			Kind:         kind,
			Summary:      call.Summary,
			SystemPrompt: call.SystemPrompt,
			UserPrompt:   call.UserPrompt,
			Turn:         state.TurnState.Turn,
			Phase:        state.TurnState.Phase,
			OccurredAt:   call.StartedAt,
			Provider:     call.Provider,
			Model:        call.Model,
			InProgress:   true,
			ElapsedMS:    call.ElapsedMS,
		})
	}
	if len(interactions) == 0 {
		return nil
	}
	return interactions
}

func cloneBoolMap(source map[string]bool) map[string]bool {
	if len(source) == 0 {
		return map[string]bool{}
	}
	clone := make(map[string]bool, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

// orderedUnits 按“玩家方 -> 敌方”稳定顺序拼接单位列表。
func orderedUnits(ids []string, byID map[string]unit.Record) []unit.Record {
	records := make([]unit.Record, 0, len(ids))
	for _, id := range ids {
		record, ok := byID[id]
		if !ok {
			continue
		}
		records = append(records, record)
	}
	return records
}

// mapRecordsByID 建立 unit_id 到记录指针的索引映射。
func mapRecordsByID(units []unit.Record) map[string]*unit.Record {
	byID := make(map[string]*unit.Record, len(units))
	for index := range units {
		byID[units[index].ID] = &units[index]
	}
	return byID
}

// snapshotUnitsFromByID 从索引映射还原可序列化单位切片并保持稳定顺序。
func snapshotUnitsFromByID(state State, byID map[string]*unit.Record) []unit.Record {
	if len(byID) == 0 {
		return nil
	}
	orderedIDs := append([]string{}, state.PlayerUnitIDs...)
	orderedIDs = append(orderedIDs, state.EnemyUnitIDs...)
	orderedIDs = append(orderedIDs, state.WildUnitIDs...)
	records := make([]unit.Record, 0, len(byID))
	seen := make(map[string]struct{}, len(byID))
	for _, id := range orderedIDs {
		record := byID[id]
		if record == nil {
			continue
		}
		records = append(records, *record)
		seen[id] = struct{}{}
	}
	for id, record := range byID {
		if record == nil {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		records = append(records, *record)
	}
	return records
}

// emitExecutionUnitProgress 在单位行动完成后广播快照，用于前端“逐单位完成”即时展示。
func (service *Service) emitExecutionUnitProgress(
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	completedUnits int,
	totalUnits int,
) {
	if service == nil || service.progressReporter == nil || state == nil || actor == nil {
		return
	}
	if totalUnits <= 0 {
		return
	}
	snapshot := buildSnapshot(*state, snapshotUnitsFromByID(*state, byID))
	service.progressReporter("execution_unit_completed", snapshot, map[string]any{
		"turn":            state.TurnState.Turn,
		"phase":           state.TurnState.Phase,
		"unit_id":         actor.ID,
		"unit_name":       actor.DisplayName(),
		"completed_units": completedUnits,
		"total_units":     totalUnits,
	})
}

// emitExecutionUnitStart 在单位开始行动时广播快照，用于前端“思考中”状态展示。
func (service *Service) emitExecutionUnitStart(
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	startedUnits int,
	totalUnits int,
) {
	if service == nil || service.progressReporter == nil || state == nil || actor == nil {
		return
	}
	if totalUnits <= 0 {
		return
	}
	snapshot := buildSnapshot(*state, snapshotUnitsFromByID(*state, byID))
	service.progressReporter("execution_unit_started", snapshot, map[string]any{
		"turn":          state.TurnState.Turn,
		"phase":         state.TurnState.Phase,
		"unit_id":       actor.ID,
		"unit_name":     actor.DisplayName(),
		"started_units": startedUnits,
		"total_units":   totalUnits,
	})
}

// isBattleReady 判断单位是否可参与执行阶段行动。
func isBattleReady(record unit.Record) bool {
	return record.Status.LifeState == unit.LifeStateActive && record.Status.HP > 0
}

// occupiedByAnother 判断目标坐标是否被其他可战斗单位占用。
func occupiedByAnother(byID map[string]*unit.Record, excludedUnitID string, coord world.Coord) bool {
	for unitID, record := range byID {
		if unitID == excludedUnitID || !isBattleReady(*record) {
			continue
		}
		if record.Status.PositionQ == coord.Q && record.Status.PositionR == coord.R {
			return true
		}
	}
	return false
}

// moveActorToward 让单位朝目标坐标推进，并返回本次移动结果。
func moveActorToward(
	snapshot world.MapSnapshot,
	byID map[string]*unit.Record,
	actor *unit.Record,
	target world.Coord,
	maxSteps int,
	stopDistance int,
) (int, error) {
	if maxSteps < 0 {
		return 0, fmt.Errorf("maxSteps must not be negative")
	}
	if stopDistance < 0 {
		return 0, fmt.Errorf("stopDistance must not be negative")
	}

	current := world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}
	steps := 0
	for steps < maxSteps {
		currentDistance := unit.HexDistance(current.Q, current.R, target.Q, target.R)
		if currentDistance <= stopDistance {
			break
		}

		best := current
		bestDistance := currentDistance
		for _, neighbor := range axialNeighbors(current) {
			if !inBounds(snapshot, neighbor) || occupiedByAnother(byID, actor.ID, neighbor) {
				continue
			}

			distance := unit.HexDistance(neighbor.Q, neighbor.R, target.Q, target.R)
			if distance >= bestDistance {
				continue
			}

			best = neighbor
			bestDistance = distance
		}

		if best == current {
			break
		}

		current = best
		steps++
	}

	actor.Status.PositionQ = current.Q
	actor.Status.PositionR = current.R
	return steps, nil
}

// resolveTarget 校验并解析决策中的目标单位引用。
func resolveTarget(targetIDs []string, byID map[string]*unit.Record, preferredTargetID string, actor *unit.Record) *unit.Record {
	if preferredTargetID != "" {
		target, ok := byID[preferredTargetID]
		if ok && isBattleReady(*target) && isTargetCandidate(targetIDs, target.ID) {
			return target
		}
		return nil
	}

	return nearestBattleReady(targetIDs, byID, actor)
}

// nearestBattleReady 查找最近的可战斗单位。
func nearestBattleReady(targetIDs []string, byID map[string]*unit.Record, actor *unit.Record) *unit.Record {
	var chosen *unit.Record
	bestDistance := 1 << 30
	bestHP := 1 << 30

	for _, targetID := range targetIDs {
		target, ok := byID[targetID]
		if !ok || !isBattleReady(*target) {
			continue
		}

		distance := unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR)
		if distance < bestDistance || (distance == bestDistance && target.Status.HP < bestHP) {
			chosen = target
			bestDistance = distance
			bestHP = target.Status.HP
		}
	}

	return chosen
}

// updateOutcome 根据当前战场结果推进胜负状态。
func updateOutcome(state *State, byID map[string]*unit.Record) bool {
	// 阵营开放世界 F1：开放世界局从不配固定敌方阵营（EnemyUnitIDs 始终为空）——此时「敌方全灭」判胜没有意义，
	// 不能因「0 个敌方=enemyAlive==0」就在第一个行动后误判玩家获胜、提前结束这局。故无敌方时短路、保持 ongoing。
	// 正常战棋局的敌方单位即便战死仍留在 EnemyUnitIDs 里（只是 isBattleReady=false），故 len>0 恒成立，行为不变。
	// 战斗对手由 F3 在游历相遇时动态接入 EnemyUnitIDs，届时本短路自然失效、胜负判定恢复。
	if len(state.EnemyUnitIDs) == 0 {
		return false
	}
	playerAlive := countBattleReady(state.PlayerUnitIDs, byID)
	enemyAlive := countBattleReady(state.EnemyUnitIDs, byID)

	switch {
	case playerAlive == 0 && enemyAlive == 0:
		setOutcome(state, OutcomeDraw, "", VictoryPathConquest, "双方单位同时全部阵亡，判定为平局。")
		return true
	case enemyAlive == 0:
		setOutcome(state, OutcomeVictory, state.PlayerFactionID, VictoryPathConquest, "敌方单位已全部阵亡，己方获胜。")
		return true
	case playerAlive == 0:
		setOutcome(state, OutcomeDefeat, state.EnemyFactionID, VictoryPathConquest, "己方单位已全部阵亡，己方失败。")
		return true
	}

	return false
}

// setOutcome 设置会话最终结果、胜者与胜利路径。
func setOutcome(state *State, outcome Outcome, winnerFactionID string, path VictoryPath, message string) {
	state.Outcome = outcome
	state.WinnerFactionID = winnerFactionID
	state.VictoryPath = path
	appendLog(state, "result", message, "", "")
}

// countBattleReady 统计阵营当前可战斗单位数量。
func countBattleReady(unitIDs []string, byID map[string]*unit.Record) int {
	count := 0
	for _, unitID := range unitIDs {
		record, ok := byID[unitID]
		if ok && isBattleReady(*record) {
			count++
		}
	}
	return count
}

// appendDirective 追加方针历史并按上限截断。
func appendDirective(state *State, directive Directive) {
	directive.Kind = normalizeDirectiveKind(directive.Kind)
	if directive.Priority == "" {
		directive.Priority = "normal"
	}
	if directive.Kind == DirectiveKindDoctrine {
		if directive.Turn <= state.TurnState.Turn && (directive.AppliesTo == "" || directive.AppliesTo == state.PlayerFactionID) {
			state.GlobalDirective = directive
		}
	}
	state.DirectiveHistory = append(state.DirectiveHistory, directive)
	appendRawEvent(state, rawEventSpec{
		source:      "directive",
		kind:        "set_" + string(directive.Kind),
		summary:     directive.Text,
		payload:     directive,
		actorUnitID: directive.IssuedBy,
	})
	if len(state.DirectiveHistory) > maxDirectiveHistory {
		state.DirectiveHistory = state.DirectiveHistory[len(state.DirectiveHistory)-maxDirectiveHistory:]
	}
}

// appendDialogue 追加对话历史并按上限截断。
func appendDialogue(state *State, message DialogueMessage) {
	state.DialogueHistory = append(state.DialogueHistory, message)
	appendRawEvent(state, rawEventSpec{
		source:      "dialogue",
		kind:        message.Speaker,
		summary:     message.Message,
		actorUnitID: message.UnitID,
		payload:     message,
	})
	if len(state.DialogueHistory) > maxDialogueHistory {
		state.DialogueHistory = state.DialogueHistory[len(state.DialogueHistory)-maxDialogueHistory:]
	}
}

// appendDecisionTrace 追加决策轨迹并按上限截断。返回所追加的轨迹（供旁路表影子双写）。
func appendDecisionTrace(state *State, trace DecisionTrace) DecisionTrace {
	state.DecisionTraces = append(state.DecisionTraces, trace)
	summary := strings.TrimSpace(trace.NextAction)
	if summary == "" {
		summary = strings.TrimSpace(trace.Speak)
	}
	if summary == "" {
		summary = strings.TrimSpace(trace.Reasoning)
	}
	appendRawEvent(state, rawEventSpec{
		source:       "decision",
		kind:         string(trace.Action),
		summary:      summary,
		actorUnitID:  trace.UnitID,
		targetUnitID: trace.TargetUnitID,
		payload:      trace,
	})
	if len(state.DecisionTraces) > maxDecisionHistory {
		state.DecisionTraces = state.DecisionTraces[len(state.DecisionTraces)-maxDecisionHistory:]
	}
	return trace
}

// appendLLMInteraction 追加 LLM 交互记录并按上限截断。
func appendLLMInteraction(state *State, interaction LLMInteraction) {
	state.LLMInteractions = append(state.LLMInteractions, interaction)
	accumulateLLMMetrics(state, interaction)
	appendRawEvent(state, rawEventSpec{
		source:      "llm",
		kind:        interaction.Kind,
		summary:     interaction.Summary,
		actorUnitID: interaction.UnitID,
		payload:     interaction,
	})
	if len(state.LLMInteractions) > maxLLMHistory {
		state.LLMInteractions = state.LLMInteractions[len(state.LLMInteractions)-maxLLMHistory:]
	}
}

// appendLog 追加战局日志并按上限截断。
func appendLog(state *State, kind string, message string, actorUnitID string, targetUnitID string) {
	entry := LogEntry{
		ID:           uuid.NewString(),
		Turn:         state.TurnState.Turn,
		Phase:        state.TurnState.Phase,
		Kind:         kind,
		Message:      message,
		ActorUnitID:  actorUnitID,
		TargetUnitID: targetUnitID,
		OccurredAt:   time.Now().UTC(),
	}
	state.Logs = append(state.Logs, entry)
	appendRawEvent(state, rawEventSpec{
		source:       "log",
		kind:         kind,
		summary:      message,
		actorUnitID:  actorUnitID,
		targetUnitID: targetUnitID,
		payload:      entry,
	})

	if len(state.Logs) > maxLogEntries {
		state.Logs = state.Logs[len(state.Logs)-maxLogEntries:]
	}
}

// appendSessionMetricsLog 把关键指标写成可读日志项。
func appendSessionMetricsLog(state *State) {
	if state == nil {
		return
	}
	appendLog(
		state,
		"session_metrics",
		fmt.Sprintf(
			"阶段结算：跨势力交互累计 %d 次，LLM 估算成本 $%.4f（tokens=%d）。",
			state.Metrics.CrossFactionInteractions,
			state.Metrics.LLMEstimatedCostUSD,
			state.Metrics.LLMTotalTokens,
		),
		"",
		"",
	)
}

// rawEventSpec 结构体用于承载该模块的核心数据。
type rawEventSpec struct {
	source       string
	kind         string
	summary      string
	actorUnitID  string
	targetUnitID string
	payload      any
}

// appendRawEvent 追加原始事件流并按上限截断。
func appendRawEvent(state *State, spec rawEventSpec) {
	if state == nil {
		return
	}
	payloadJSON := ""
	if spec.payload != nil {
		if encoded, err := json.Marshal(spec.payload); err == nil {
			payloadJSON = string(encoded)
		}
	}
	if spec.source == "llm" || spec.source == "decision" {
		payloadJSON = ""
	} else {
		payloadJSON = limitTextRunes(payloadJSON, 1200)
	}
	state.RawEventLog = append(state.RawEventLog, RawEventEntry{
		ID:           uuid.NewString(),
		Turn:         state.TurnState.Turn,
		Phase:        state.TurnState.Phase,
		Source:       spec.source,
		Kind:         spec.kind,
		Summary:      spec.summary,
		ActorUnitID:  spec.actorUnitID,
		TargetUnitID: spec.targetUnitID,
		PayloadJSON:  payloadJSON,
		OccurredAt:   time.Now().UTC(),
	})
	if len(state.RawEventLog) > maxRawEventHistory {
		state.RawEventLog = state.RawEventLog[len(state.RawEventLog)-maxRawEventHistory:]
	}
}
