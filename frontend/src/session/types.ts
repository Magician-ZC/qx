/* 文件说明：前端会话域类型定义，声明快照、单位、日志、决策轨迹与指令等数据结构。 */

export type Phase = "deployment" | "execution";
export type Outcome = "ongoing" | "victory" | "defeat" | "draw";
export type VictoryPath = "conquest";
export type DecisionAction =
  | "attack"
  | "charge"
  | "heavy_attack"
  | "skill"
  | "defend"
  | "observe"
  | "assist"
  | "say"
  | "dialogue"
  | "trade"
  | "romance"
  | "family"
  | "build"
  | "demolish"
  | "gather"
  | "forge"
  | "upgrade"
  | "equip"
  | "eat"
  | "pickup"
  | "move"
  | "hold";
export type ProductionActivity = "farm" | "fish" | "forage" | "hunt" | "mine";
export type StructureType = "farmland" | "forge" | "trap" | "turret" | "watchtower";

export type Coord = {
  q: number;
  r: number;
};

export type TurnState = {
  turn: number;
  phase: Phase;
  phase_started_at: string;
  phase_ends_at: string;
  budgets: {
    deployment: number;
    execution: number;
    fast_forward_cap: number;
  };
};

export type TerrainTile = {
  coord: Coord;
  terrain: string;
  region_id: string;
  landmark?: string;
};

export type BattleMap = {
  id: string;
  seed: number;
  width: number;
  height: number;
  generated_at: string;
  tiles: TerrainTile[];
  counts: Record<string, number>;
};

export type TerrainDefinition = {
  id: string;
  display_name: string;
  move_cost: number;
  vision_range: number;
  combat_rules: string[];
  activities: string[];
  resources: string[];
  special_rules: string[];
};

export type SessionWeather = {
  type: "clear" | "windy" | "rainy" | "foggy";
  display_name: string;
  note?: string;
  turn: number;
};

export type PregnancyState = {
  id: string;
  parent_unit_ids: string[];
  pregnant_unit_id: string;
  started_turn: number;
  due_turn: number;
};

export type InventoryItem = {
  item_id: string;
  quantity: number;
  custom_name?: string;
  level?: number;
};

export type BattleUnit = {
  id: string;
  faction_id: string;
  identity: {
    name: string;
    nickname: string;
    portrait_url?: string;
    gender?: string;
    lineage?: string;
    age?: number;
    biography: string;
    recruitment_pitch: string;
  };
  memory: {
    recent_event_ids: string[];
    highlights: string[];
  };
  stats?: {
    primary?: {
      strength?: number;
      dexterity?: number;
      constitution?: number;
      wisdom?: number;
      perception?: number;
      charisma?: number;
    };
    derived?: {
      attack?: number;
      defense?: number;
      accuracy?: number;
      evasion?: number;
      vision?: number;
      carry_weight?: number;
    };
  };
  skills?: {
    weapons?: {
      sword?: number;
      bow?: number;
      blunt?: number;
      shield?: number;
      medical?: number;
    };
    survival?: {
      scouting?: number;
      stealth?: number;
      medicine?: number;
      gathering?: number;
    };
    social?: {
      negotiation?: number;
      intimidation?: number;
      charm?: number;
      trade?: number;
    };
    specialties?: string[];
  };
  personality: {
    courage: number;
    loyalty: number;
    aggression: number;
    prudence: number;
    sociability: number;
    integrity: number;
    stability: number;
    ambition: number;
  };
  social?: {
    lover_unit_id?: string;
    parent_unit_ids?: string[];
    child_unit_ids?: string[];
    born_turn?: number;
    last_romance_turn?: number;
    wildling?: boolean;
  };
  status: {
    hp: number;
    attack: number;
    defense: number;
    move: number;
    hunger: number;
    starvation_turns?: number;
    wallet: number;
    lives_remaining: number;
    life_state: string;
    recovery_turns: number;
    position_q: number;
    position_r: number;
    in_combat: boolean;
  };
  inventory: {
    equipment: Record<string, InventoryItem>;
    backpack: InventoryItem[];
  };
};

// Directive 是玩家自然语言输入后沉淀的结构化指令记录，单位会参考但不被硬脚本控制。
export type Directive = {
  id: string;
  turn: number;
  phase: Phase;
  kind?: "doctrine" | "task" | "order";
  text: string;
  priority?: "low" | "normal" | "high" | "urgent";
  target_unit_id?: string;
  issued_at: string;
  issued_by: string;
  applies_to: string;
};

export type CommandPower = {
  current: number;
  max: number;
  regen: number;
  order_cost: number;
};

export type DialogueMessage = {
  id: string;
  unit_id: string;
  speaker: string;
  message: string;
  turn: number;
  phase: Phase;
  occurred_at: string;
  provider?: string;
  model?: string;
  used_fallback?: boolean;
};

// DecisionTrace 记录“请求动作 -> 最终执行动作”的完整链路，用于回放与调试。
export type DecisionTrace = {
  id: string;
  unit_id: string;
  faction_id: string;
  requested_action?: DecisionAction;
  requested_activity?: ProductionActivity;
  requested_skill_id?: string;
  requested_structure_id?: string;
  requested_structure_type?: StructureType;
  requested_target_unit_id?: string;
  requested_target_q?: number;
  requested_target_r?: number;
  requested_next_action?: string;
  requested_speak?: string;
  requested_memory?: string;
  requested_knowledge?: string;
  requested_reasoning?: string;
  action: DecisionAction;
  activity?: ProductionActivity;
  skill_id?: string;
  memory?: string;
  structure_id?: string;
  structure_type?: StructureType;
  target_unit_id?: string;
  target_q?: number;
  target_r?: number;
  next_action?: string;
  speak?: string;
  reasoning: string;
  knowledge?: string;
  obedience_state?: string;
  obedience_note?: string;
  reject_probability?: number;
  risk_score?: number;
  move_multiplier?: number;
  attack_multiplier?: number;
  action_index?: number;
  ap_before?: number;
  ap_cost?: number;
  ap_after?: number;
  turn: number;
  phase: Phase;
  occurred_at: string;
  provider?: string;
  model?: string;
  used_fallback?: boolean;
};

export type CompletionAttempt = {
  provider: string;
  endpoint: string;
  base_url?: string;
  wire_api?: string;
  model?: string;
  started_at?: string;
  duration_ms?: number;
  status_code?: number;
  succeeded: boolean;
  error?: string;
};

export type LLMInteraction = {
  id: string;
  unit_id: string;
  kind: string;
  summary: string;
  system_prompt: string;
  user_prompt: string;
  parsed_output?: string;
  raw_output?: string;
  error_message?: string;
  fallback_cause?: string;
  turn: number;
  phase: Phase;
  occurred_at: string;
  provider?: string;
  model?: string;
  used_fallback?: boolean;
  prompt_tokens?: number;
  output_tokens?: number;
  total_tokens?: number;
  estimated_cost_usd?: number;
  attempts?: CompletionAttempt[];
  in_progress?: boolean;
  elapsed_ms?: number;
};

export type SessionLog = {
  id: string;
  turn: number;
  phase: Phase;
  kind: string;
  message: string;
  actor_unit_id?: string;
  target_unit_id?: string;
  occurred_at: string;
};

export type RawEventEntry = {
  id: string;
  turn: number;
  phase: Phase;
  source: string;
  kind: string;
  summary: string;
  actor_unit_id?: string;
  target_unit_id?: string;
  payload_json?: string;
  occurred_at: string;
};

export type SessionStructure = {
  id: string;
  type: StructureType;
  faction_id: string;
  builder_unit_id?: string;
  q: number;
  r: number;
  build_progress: number;
  build_required: number;
  completed: boolean;
  started_turn: number;
  completed_turn?: number;
  harvest_ready_turn?: number;
  charges?: number;
  created_at: string;
  updated_at: string;
};

export type GraveMarker = {
  id: string;
  unit_id: string;
  unit_name: string;
  faction_id: string;
  q: number;
  r: number;
  turn: number;
  created_at: string;
};

export type GroundLootDrop = {
  id: string;
  q: number;
  r: number;
  source_unit_id?: string;
  source_unit_name?: string;
  inheritor_unit_id?: string;
  items: InventoryItem[];
  turn: number;
  created_at: string;
};

export type BattleReport = {
  id: string;
  turn: number;
  phase: Phase;
  narrator_unit_id: string;
  narrator: string;
  title?: string;
  content: string;
  illustration_prompt?: string;
  illustration_url?: string;
  memory?: string;
  created_at: string;
  provider?: string;
  model?: string;
  used_fallback?: boolean;
};

export type HallArchiveEntry = {
  id: string;
  unit_id: string;
  unit_name: string;
  faction_id: string;
  outcome: Outcome;
  biography: string;
  top_events?: string[];
  created_at: string;
  provider?: string;
  model?: string;
  used_fallback?: boolean;
};

export type SessionMetrics = {
  cross_faction_interactions: number;
  llm_prompt_tokens: number;
  llm_output_tokens: number;
  llm_total_tokens: number;
  llm_estimated_cost_usd: number;
};

export type ModerationReport = {
  id: string;
  session_id: string;
  turn: number;
  phase: Phase;
  reporter: string;
  unit_id?: string;
  category: string;
  detail: string;
  created_at: string;
  resolved?: boolean;
  resolved_at?: string;
};

export type AuditBundle = {
  session_id: string;
  reports: ModerationReport[];
  dialogue_history: DialogueMessage[];
  llm_interactions: LLMInteraction[];
  logs: SessionLog[];
  raw_event_log: RawEventEntry[];
};

export type PrivacyEraseOptions = {
  erase_dialogue?: boolean;
  erase_llm_details?: boolean;
  erase_audit_trail?: boolean;
  erase_memories?: boolean;
  erase_reports?: boolean;
};

export type PrivacyEraseResult = {
  session_id: string;
  dialogue_entries_erased: number;
  llm_interactions_redacted: number;
  audit_logs_erased: number;
  raw_events_erased: number;
  reports_erased: number;
  unit_highlights_erased: number;
  memory_rows_erased: number;
  memory_fts_rows_erased: number;
  phase_snapshots_regenerated: boolean;
};

export type PrivacyPurgeResult = {
  retention_days: number;
  cutoff_unix: number;
  sessions_deleted: number;
  units_deleted: number;
  events_deleted: number;
  hall_entries_deleted: number;
  phase_snapshots_deleted: number;
  memories_fts_deleted: number;
  // 与后端补齐的删除计数（详见 backend privacy purge 结果）。
  llm_interactions_deleted?: number;
  decision_traces_deleted?: number;
  raw_events_deleted?: number;
  wake_queue_deleted?: number;
  decision_jobs_deleted?: number;
};

// ---- 商业化 / 合规 / 跨玩家 / PvE 组队 / 漏斗埋点（P2 端点的客户端类型）----

// EncounterAward 是一次 elite/PvE 遭遇分到的一件战利品（与后端 encounter.Award 对齐，Go 默认大写键名）。
export type EncounterAward = {
  ItemID: string;
  UnitID: string;
  Quantity: number;
  Reason: string;
};

// BillingSKU 是一个售卖项目，与后端 billing.SKU 对齐（json tag 蛇形）。
export type BillingSKU = {
  id: string;
  kind: string; // subscription / one_time / consumable
  name: string;
  price_cents: number; // 最小货币单位（分）
  period: string;
  active: boolean;
  created_at: string;
};

// BillingCharge 是一条计费流水，与后端 billing.Charge 对齐（purchase 返回）。
export type BillingCharge = {
  id: string;
  account_id: string;
  sku_id: string;
  amount_cents: number;
  provider: string;
  receipt_ref: string;
  status: string;
  created_at: string;
};

// Entitlement 是某账户对某 SKU 的当前权益态，与后端 billing.Entitlement 对齐。
export type Entitlement = {
  account_id: string;
  sku_id: string;
  status: string; // active / expired / ...
  granted_at: string;
  expires_at: string;
};

// BillingQuota 是账户级 LLM 成本配额闸结果（true=未超额）。
export type BillingQuota = {
  allowed: boolean;
};

// ComplianceGate 是出海合规前置门裁决（未实名/宵禁/防沉迷）。
export type ComplianceGate = {
  allowed: boolean;
  minor_mode: boolean;
  reason: string;
};

// ConsentRequest 是一条跨玩家异步同意请求，与后端 session.ConsentRequest 对齐。
export type ConsentRequest = {
  id: string;
  world_id: string;
  actor_unit_id: string;
  target_unit_id: string;
  interaction: string;
  tier: string;
  status: string; // pending / accepted / rejected / expired
  event_id: string;
  created_at: string;
  resolved_at?: string;
};

// FieldBossMemberOutcome 某队员在野外 Boss 组队遭遇中的结算。
// 注意：后端 FieldBossMemberOutcome 无 json tag，键名为 Go 大写字段名。
export type FieldBossMemberOutcome = {
  UnitID: string;
  Outcome: string; // contributed / fled / down
  DamageDealt: number;
  DamageTaken: number;
  Contribution: number;
  Awards: EncounterAward[] | null;
  PenaltyLayer: number; // 失败时经分级闸落地的后果层，0=未触发
  InboxCard: string; // 祖魂语气命运卡
};

// FieldBossResult 野外 Boss/组队遭遇的整体结算（后端 Go 大写键名，无 json tag）。
export type FieldBossResult = {
  ThreatID: string;
  Victory: boolean;
  Rounds: number;
  Members: FieldBossMemberOutcome[] | null;
};

// LeadEvent 是漏斗埋点的提交载荷（POST /api/leads，无鉴权）。
export type LeadEvent = {
  kind: string;
  vid: string;
  email?: string;
  source?: string;
};

// LeadsFunnelData 是假门转化漏斗（GET /api/ops/leads-funnel，运营态 X-Ops-Token）。
// 后端裸返回 {total, by_kind, unique_visitors}（leads.go），by_kind 按 lead kind 计数。
export type LeadsFunnelData = {
  total: number;
  by_kind: Record<string, number>;
  unique_visitors: number;
};

// ProviderCost 是单个 LLM provider 的成本/调用聚合（与后端 session.ProviderCost 对齐，json tag 蛇形）。
export type ProviderCost = {
  provider: string;
  calls: number;
  cost_usd: number;
  total_tokens: number;
  fallback_hits: number;
};

// CostDashboardData 是运营成本/单位经济仪表盘聚合结果（GET /api/ops/cost-dashboard，运营态 X-Ops-Token）。
// 严格对齐后端 session.CostDashboardData 的 json tag（蛇形）。distinct_sessions 为 MAU 代理（窗口内有 LLM 交互的会话数）。
export type CostDashboardData = {
  since_days: number;
  total_interactions: number;
  total_cost_usd: number;
  total_tokens: number;
  fallback_count: number;
  fallback_rate: number;
  distinct_sessions: number; // MAU 代理：窗口内有 LLM 交互的会话数
  cost_per_session_usd: number;
  by_provider: Record<string, ProviderCost>;
  units_total: number;
  units_by_life_state: Record<string, number>;
  generated_at: string;
};

// BloodFeudEntry 是某角色当前怀有的一条世仇关系（rivalry 主导的强敌意），与后端 session.BloodFeudEntry 对齐。
// 后端 json tag：target_name 带 omitempty（无名时可缺）；其余四轴恒在。GET …/feuds 返回 {feuds: BloodFeudEntry[]}。
export type BloodFeudEntry = {
  target_unit_id: string;
  target_name?: string;
  rivalry: number;
  fear: number;
  trust: number;
  affection: number;
};

// WorldBossStrikeResult 是一次对世界 Boss 出手的结果（POST …/strike 取 {strike}）。
// 注意：后端 session.WorldBossStrikeResult 无 json tag，键名为 Go 大写字段名。
// Participants/Awards/BroadcastCard 仅当本请求抢到结算闩锁（Defeated && SettledByMe）时有意义。
export type WorldBossStrikeResult = {
  BossID: string;
  AttackerID: string;
  Damage: number;
  HPRemaining: number;
  Defeated: boolean; // 这一击是否打死了 Boss
  SettledByMe: boolean; // 这一击是否由本请求执行了结算（抢到闩锁）
  Participants: number; // 结算时的参战人数（仅 Defeated&&SettledByMe 时有意义）
  Awards: EncounterAward[] | null; // 全员分赃结果（仅结算者填充）
  BroadcastCard: string; // 讨平广播的祖魂语气卡（仅结算者填充）
};

// DungeonFloorResult 是一次副本中单层的结算（与后端 session.DungeonFloorResult 对齐，无 json tag，键名为 Go 大写字段名）。
export type DungeonFloorResult = {
  Floor: number;
  IsBoss: boolean; // 末层 boss
  ThreatName: string;
  Outcome: string; // cleared / fled / wiped
  Rounds: number;
  DamageDealt: number;
  DamageTaken: number;
};

// DungeonResult 是一次多层副本的整体结算（与后端 session.DungeonResult 对齐，无 json tag，键名为 Go 大写字段名）。
// FloorResults/Awards 为切片（可空 null）；Contribution/PenaltyLayer/InboxCards 为 map（按单位 ID 索引，可空 null）。
export type DungeonResult = {
  DungeonID: string;
  Floors: number; // 计划层数
  FloorsClear: number; // 实际通关层数
  Outcome: string; // cleared / fled / wiped
  FloorResults: DungeonFloorResult[] | null; // 逐层结果
  Awards: EncounterAward[] | null; // 总分赃（仅通关）
  Contribution: Record<string, number> | null; // 各参战单位累计贡献分
  PenaltyLayer: Record<string, number> | null; // 败北时各单位经分级闸落地的后果层
  InboxCards: Record<string, string> | null; // 各参战单位的祖魂语气命运卡
};

// ProductFunnelReport 是 AARRR 漏斗的窗口聚合（GET /api/ops/product-funnel，运营态 X-Ops-Token）。
// 严格对齐后端 analytics.FunnelReport 的 json tag（蛇形）。since_days<=0=全量。
export type ProductFunnelReport = {
  since_days: number;
  by_event: Record<string, number>; // 事件名 -> 计数
  by_stage: Record<string, number>; // 漏斗阶段 -> 计数
  distinct_sessions: number; // 窗口内去重 session 数
  generated_at: string;
};

// NorthStarReport 是北极星指标的窗口聚合（GET /api/ops/north-star，运营态 X-Ops-Token）。
// 严格对齐后端 analytics.NorthStarReport 的 json tag（蛇形）。处理率/惊喜率分母为 0 时后端返回 0。
export type NorthStarReport = {
  since_days: number;
  sessions_created: number;
  characters_created: number;
  decision_pending: number;
  decision_resolved: number;
  inbox_process_rate: number; // resolved/pending；分母 0 -> 0
  share_initiated: number;
  purchases: number;
  return_visits: number;
  fate_react_expected: number; // 意料之中
  fate_react_surprise: number; // 有点意外但合理 = 命中惊喜
  fate_react_ooc: number; // 太离谱 = 疑似失格
  surprise_hit_rate: number; // surprise/(expected+surprise+ooc)；分母 0 -> 0
  ooc_rate: number; // ooc/(expected+surprise+ooc)；分母 0 -> 0
  generated_at: string;
};

// ExperimentFunnelReport 是某实验按 ab_bucket 拆分的漏斗（GET /api/ops/experiment，运营态 X-Ops-Token）。
// 严格对齐后端 analytics.ExperimentReport 的 json tag（蛇形）：experiment 回显查询入参；
// by_bucket 为 ab_bucket -> (event_name -> 计数) 的二维 map。
export type ExperimentFunnelReport = {
  experiment: string; // 实验标识（=查询入参，回显便于核对）
  since_days: number;
  by_bucket: Record<string, Record<string, number>>; // ab_bucket -> (event_name -> 计数)
  generated_at: string;
};

export type AccountUser = {
  id: string;
  username: string;
  display_name: string;
  created_at: string;
  updated_at: string;
};

export type AccountLoginResult = {
  user: AccountUser;
  token: string;
  expires_at: string;
  token_type: string;
  provider: string;
};

export type DuelRoomStatus = {
  room_code: string;
  player_joined: boolean;
  enemy_joined: boolean;
};

// SessionSnapshot 是前端唯一可信输入，界面只渲染该快照，不本地推演规则。
export type SessionSnapshot = {
  id: string;
  world_id?: string; // 本局所属世界（空=未接入多世界）；世界 Boss 等跨玩家面板据此定位
  minor_mode?: boolean; // 本局未成年模式（前端据此可提示分级 / 隐藏恋爱·生育入口）
  mode: string;
  random_seed: number;
  player_faction_id: string;
  enemy_faction_id: string;
  setup_phase?: "ready" | "drafting";
  setup_deadline_at?: string;
  draft_required_pick?: number;
  player_draft_pool?: BattleUnit[];
  enemy_draft_pool?: BattleUnit[];
  map_script_id?: string;
  map_script_name?: string;
  map_size_id?: "small" | "medium" | "large" | string;
  map_size_name?: string;
  fog_of_war_enabled: boolean;
  random_events_enabled: boolean;
  turn_state: TurnState;
  phase_ready?: Record<string, boolean>;
  execution_in_progress?: boolean;
  outcome: Outcome;
  winner_faction_id?: string;
  victory_path?: VictoryPath;
  weather: SessionWeather;
  map: BattleMap;
  command_power: CommandPower;
  structures: SessionStructure[];
  grave_markers?: GraveMarker[];
  ground_loot_drops?: GroundLootDrop[];
  global_directive: Directive;
  directive_history: Directive[];
  dialogue_history: DialogueMessage[];
  decision_traces: DecisionTrace[];
  llm_interactions: LLMInteraction[];
  active_llm_calls?: LLMInteraction[];
  pregnancies?: PregnancyState[];
  battle_reports: BattleReport[];
  hall_archive_entries?: HallArchiveEntry[];
  intel_assets: {
    id: string;
    unit_id: string;
    home_faction_id: string;
    handler_faction_id: string;
    mode: string;
    motivation?: string;
    risk?: number;
    since_turn: number;
    last_report_turn?: number;
    exposed?: boolean;
    updated_at: string;
  }[];
  intel_reports: {
    id: string;
    turn: number;
    phase: Phase;
    kind: string;
    unit_id: string;
    source_faction_id: string;
    target_faction_id: string;
    summary: string;
    created_at: string;
  }[];
  moderation_reports?: ModerationReport[];
  metrics: SessionMetrics;
  raw_event_log: RawEventEntry[];
  logs: SessionLog[];
  player_units: BattleUnit[];
  enemy_units: BattleUnit[];
  wild_units?: BattleUnit[];
  // ambient_units：阵营据点的公共 NPC（faction_spawn 播种、命运主世界静态可见）。
  // 契约（crossFileNeeds）：后端 State.AmbientUnitIDs → Snapshot.AmbientUnits（json:"ambient_units"）。
  // 这些 NPC **只作静态可见**（在格子上有坐标、可被她在地图上看到），**绝不进 WildUnitIDs/执行 order**，
  // 否则每拍会多触发 8~12 次 LLM 决策、成本爆炸。前端只读渲染：区别色 + 小一号 token。
  ambient_units?: BattleUnit[];
};
