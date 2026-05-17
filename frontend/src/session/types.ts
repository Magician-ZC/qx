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
};
