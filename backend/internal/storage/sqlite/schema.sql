-- 文件说明：SQLite 主 schema，定义单位、记忆、事件、关系、地图、会话快照与审计相关表结构。

PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS units (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  faction_id TEXT NOT NULL,
  display_name TEXT NOT NULL,
  profile_json TEXT NOT NULL DEFAULT '{}',
  personality_json TEXT NOT NULL DEFAULT '{}',
  status_json TEXT NOT NULL DEFAULT '{}',
  inventory_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_units_session_id ON units(session_id);

CREATE TABLE IF NOT EXISTS memories (
  id TEXT PRIMARY KEY,
  unit_id TEXT NOT NULL,
  category TEXT NOT NULL,
  summary TEXT NOT NULL,
  emotion_weight REAL NOT NULL DEFAULT 1.0,
  salience REAL NOT NULL DEFAULT 0.0,
  recall_count INTEGER NOT NULL DEFAULT 0,
  metadata_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  last_recalled_at TEXT,
  FOREIGN KEY(unit_id) REFERENCES units(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_memories_unit_id ON memories(unit_id);
CREATE INDEX IF NOT EXISTS idx_memories_salience ON memories(salience DESC);
CREATE INDEX IF NOT EXISTS idx_memories_unit_sort ON memories(unit_id, salience DESC, recall_count DESC, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_memories_unit_category_sort ON memories(unit_id, category, salience DESC, created_at DESC);

CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  actor_unit_id TEXT,
  target_unit_id TEXT,
  event_type TEXT NOT NULL,
  reason_code TEXT NOT NULL,
  payload_json TEXT NOT NULL DEFAULT '{}',
  occurred_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  world_id TEXT,
  region_id TEXT,
  tick INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY(actor_unit_id) REFERENCES units(id) ON DELETE SET NULL,
  FOREIGN KEY(target_unit_id) REFERENCES units(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_events_session_id ON events(session_id);
CREATE INDEX IF NOT EXISTS idx_events_actor_unit_id ON events(actor_unit_id);
CREATE INDEX IF NOT EXISTS idx_events_target_unit_id ON events(target_unit_id);
CREATE INDEX IF NOT EXISTS idx_events_reason_code ON events(reason_code);

CREATE TABLE IF NOT EXISTS relations (
  source_unit_id TEXT NOT NULL,
  target_unit_id TEXT NOT NULL,
  trust REAL NOT NULL DEFAULT 0,
  fear REAL NOT NULL DEFAULT 0,
  affection REAL NOT NULL DEFAULT 0,
  rivalry REAL NOT NULL DEFAULT 0,
  notes_json TEXT NOT NULL DEFAULT '{}',
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (source_unit_id, target_unit_id),
  FOREIGN KEY(source_unit_id) REFERENCES units(id) ON DELETE CASCADE,
  FOREIGN KEY(target_unit_id) REFERENCES units(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_relations_target_unit_id ON relations(target_unit_id);

CREATE TABLE IF NOT EXISTS event_reason_codes (
  code TEXT PRIMARY KEY,
  category TEXT NOT NULL,
  display_name TEXT NOT NULL,
  default_reason_text TEXT NOT NULL,
  stat_domains_json TEXT NOT NULL DEFAULT '[]',
  importance_min INTEGER NOT NULL,
  importance_max INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS terrain_types (
  id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  move_cost REAL NOT NULL,
  vision_range INTEGER NOT NULL,
  combat_rules_json TEXT NOT NULL DEFAULT '[]',
  activities_json TEXT NOT NULL DEFAULT '[]',
  resources_json TEXT NOT NULL DEFAULT '[]',
  special_rules_json TEXT NOT NULL DEFAULT '[]'
);

CREATE TABLE IF NOT EXISTS world_maps (
  id TEXT PRIMARY KEY,
  seed INTEGER NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  generated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_world_maps_generated_at ON world_maps(generated_at DESC);

CREATE TABLE IF NOT EXISTS world_tiles (
  map_id TEXT NOT NULL,
  q INTEGER NOT NULL,
  r INTEGER NOT NULL,
  terrain_id TEXT NOT NULL,
  region_id TEXT NOT NULL DEFAULT '',
  landmark TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (map_id, q, r),
  FOREIGN KEY(map_id) REFERENCES world_maps(id) ON DELETE CASCADE,
  FOREIGN KEY(terrain_id) REFERENCES terrain_types(id) ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_world_tiles_terrain_id ON world_tiles(terrain_id);

CREATE TABLE IF NOT EXISTS ground_loot_drops (
  id TEXT PRIMARY KEY,
  location TEXT NOT NULL,
  source_unit_id TEXT NOT NULL,
  inheritor_unit_id TEXT NOT NULL,
  items_json TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS single_player_sessions (
  id TEXT PRIMARY KEY,
  state_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_single_player_sessions_updated_at ON single_player_sessions(updated_at DESC);

CREATE TABLE IF NOT EXISTS session_phase_snapshots (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  turn INTEGER NOT NULL,
  phase TEXT NOT NULL,
  snapshot_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(session_id, turn, phase),
  FOREIGN KEY(session_id) REFERENCES single_player_sessions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_session_phase_snapshots_session_created_at ON session_phase_snapshots(session_id, created_at DESC);

CREATE TABLE IF NOT EXISTS hall_of_fame_entries (
  id TEXT PRIMARY KEY,
  source_session_id TEXT NOT NULL,
  source_unit_id TEXT NOT NULL,
  unit_name TEXT NOT NULL,
  unit_faction_id TEXT NOT NULL,
  outcome TEXT NOT NULL,
  biography_summary TEXT NOT NULL,
  top_events_json TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE(source_session_id, source_unit_id)
);

CREATE INDEX IF NOT EXISTS idx_hall_of_fame_entries_unit_name ON hall_of_fame_entries(unit_name, created_at DESC);

-- World Bus：跨玩家不可篡改的唯一事实源（设计文档 docs/事件耦合与跨玩家关联.md）。
-- append-only，永不 UPDATE/DELETE；权威排序键 = (world_tick, occurred_at, id)，即「谁先动手」。
-- 刻意不设 units 外键：actor/target 可能是别的玩家、别的分片、甚至已离线的角色，跨界引用不能被 FK 卡住。
CREATE TABLE IF NOT EXISTS cross_events (
  id TEXT PRIMARY KEY,
  world_id TEXT NOT NULL,
  actor_unit_id TEXT,
  target_unit_id TEXT,
  event_kind TEXT NOT NULL,
  region_id TEXT,
  importance INTEGER NOT NULL DEFAULT 0,
  world_tick INTEGER NOT NULL DEFAULT 0,
  payload_json TEXT NOT NULL DEFAULT '{}',
  occurred_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_cross_events_world ON cross_events(world_id, world_tick, occurred_at);
CREATE INDEX IF NOT EXISTS idx_cross_events_actor ON cross_events(actor_unit_id);
CREATE INDEX IF NOT EXISTS idx_cross_events_target ON cross_events(target_unit_id);

-- 世界注册表（多世界模型的根，设计文档 docs/大世界沙盘设计方案.md §8）。
-- tick 是该世界的权威时钟：「世界会等你，但不会假装暂停」——它单调推进，是 cross_events.world_tick 的来源。
CREATE TABLE IF NOT EXISTS worlds (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',
  tick INTEGER NOT NULL DEFAULT 0,
  max_population INTEGER NOT NULL DEFAULT 0,
  region_seed TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_worlds_status ON worlds(status, created_at DESC);

-- 角色→世界归属。刻意不设 units 外键：成员可能是跨分片角色，归属完整性由业务层负责。
CREATE TABLE IF NOT EXISTS world_members (
  world_id TEXT NOT NULL,
  character_unit_id TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'inhabitant',
  joined_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (world_id, character_unit_id)
);

CREATE INDEX IF NOT EXISTS idx_world_members_character ON world_members(character_unit_id);

-- 世界Boss：全世界共享一个血池的协作目标（设计文档 docs/PvE威胁系统.md 世界Boss）。
-- 异步参战——不同玩家的角色在不同时间各自出手，每次出手记进世界总线(WORLD_BOSS_STRIKE)，
-- 总线即贡献账本；血池清零时按账本全员分赃并广播 WORLD_BOSS_DEFEATED。hp_remaining 的原子递减 + 单次结算闩锁防双结算。
CREATE TABLE IF NOT EXISTS world_bosses (
  id TEXT PRIMARY KEY,
  world_id TEXT NOT NULL,
  name TEXT NOT NULL,
  hp_max INTEGER NOT NULL,
  hp_remaining INTEGER NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',
  region_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_world_bosses_world ON world_bosses(world_id, status);

-- 产品分析埋点（AARRR 漏斗，append-only，无 FK，与游戏状态解耦；设计 docs/验证实验设计.md §5.2）。
CREATE TABLE IF NOT EXISTS product_events (
  id TEXT PRIMARY KEY,
  stage TEXT NOT NULL,
  event_name TEXT NOT NULL,
  session_id TEXT,
  unit_id TEXT,
  properties_json TEXT NOT NULL DEFAULT '{}',
  occurred_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_product_events_name ON product_events(event_name, occurred_at);
CREATE INDEX IF NOT EXISTS idx_product_events_session ON product_events(session_id);

-- 相关性锚（每角色「她在乎什么」的持久集合：关系/红线/目标/债仇爱/所在地/血脉；设计 耦合 §1.1）。
-- 在关系/目标/红线变更时 upsert 权重，喂 engine/relevance.Score。非关系锚（目标/红线/传家物）只有这张表能存。
CREATE TABLE IF NOT EXISTS relevance_anchors (
  character_unit_id TEXT NOT NULL,
  anchor_kind TEXT NOT NULL,
  anchor_ref TEXT NOT NULL,
  weight REAL NOT NULL DEFAULT 0,
  label TEXT NOT NULL DEFAULT '',
  half_life_days REAL NOT NULL DEFAULT 14,
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (character_unit_id, anchor_kind, anchor_ref)
);

CREATE INDEX IF NOT EXISTS idx_relevance_anchors_char ON relevance_anchors(character_unit_id);

-- 决策轨迹旁路表（拆 state_json 第一片，沙盘 §11.2）。影子双写：决策轨迹 append 时同时写这里，
-- 留全量历史（blob 仍按上限裁剪、仍为权威读源——本表零风险，仅旁路留痕，后续验证后再移出 blob）。
CREATE TABLE IF NOT EXISTS decision_traces (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  unit_id TEXT,
  trace_json TEXT NOT NULL,
  occurred_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_decision_traces_session ON decision_traces(session_id, occurred_at);

-- LLM 交互旁路表（拆 state_json 第二片，沙盘 §11.2）。影子双写：Save 时把当回合 state.LLMInteractions
-- 在 blob 压缩（裁剪条数 + 抹除旧 prompt）之前持久化到本表，留全量、含完整 prompt 的可查历史。
-- 执行循环每个 actor 行动后即 Save，故 INSERT OR IGNORE 跨 Save 累积出全量；blob 仍裁剪仍为权威读源——
-- 本表零风险，仅旁路留痕，后续验证后再移出 blob。隐私擦除/保留期清理须同步清本表（见 privacy.go）。
CREATE TABLE IF NOT EXISTS llm_interactions (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  unit_id TEXT,
  interaction_json TEXT NOT NULL,
  occurred_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_llm_interactions_session ON llm_interactions(session_id, occurred_at);

-- 原始事件日志旁路表（拆 state_json 第三片，沙盘 §11.2）。读路径已 cutover：Save 把 state.RawEventLog 持久化到本表、
-- 确认写表成功才从 blob 摘除；load 时 hydrateRawEvents 从表读回。RawEventLog 在 appendRawEvent 即限/裁 payload，
-- 故本表与 blob 同口径（无 LLM 的有损压缩问题），仅 cap maxRawEventHistory。隐私擦除/保留期清理须同步清本表。
CREATE TABLE IF NOT EXISTS raw_event_log (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  unit_id TEXT,
  event_json TEXT NOT NULL,
  occurred_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_raw_event_log_session ON raw_event_log(session_id, occurred_at);
