CREATE TABLE IF NOT EXISTS units (
  id VARCHAR(191) PRIMARY KEY,
  session_id VARCHAR(191) NOT NULL,
  faction_id VARCHAR(191) NOT NULL,
  display_name VARCHAR(191) NOT NULL,
  profile_json LONGTEXT NOT NULL,
  personality_json LONGTEXT NOT NULL,
  status_json LONGTEXT NOT NULL,
  inventory_json LONGTEXT NOT NULL,
  -- 大世界单位作用域 + 生命态调度列（沙盘 §8.7，双写灰度）。现有库经 dbmigrate 幂等补列。
  world_id VARCHAR(191) NULL,
  region_id VARCHAR(191) NULL,
  life_state VARCHAR(32) NOT NULL DEFAULT 'active',
  last_active_tick BIGINT NOT NULL DEFAULT 0,
  version BIGINT NOT NULL DEFAULT 0,
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  updated_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_units_session_id (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS memories (
  id VARCHAR(191) PRIMARY KEY,
  unit_id VARCHAR(191) NOT NULL,
  category VARCHAR(191) NOT NULL,
  summary TEXT NOT NULL,
  emotion_weight DOUBLE NOT NULL DEFAULT 1.0,
  salience DOUBLE NOT NULL DEFAULT 0.0,
  recall_count INTEGER NOT NULL DEFAULT 0,
  metadata_json LONGTEXT NOT NULL,
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  last_recalled_at VARCHAR(64),
  INDEX idx_memories_unit_id (unit_id),
  INDEX idx_memories_salience (salience),
  INDEX idx_memories_unit_sort (unit_id, salience, recall_count, created_at),
  INDEX idx_memories_unit_category_sort (unit_id, category, salience, created_at),
  CONSTRAINT fk_memories_unit FOREIGN KEY (unit_id) REFERENCES units(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS memories_fts (
  memory_id VARCHAR(191) PRIMARY KEY,
  unit_id VARCHAR(191) NOT NULL,
  summary TEXT NOT NULL,
  INDEX idx_memories_fts_unit_id (unit_id),
  FULLTEXT INDEX idx_memories_fts_summary (summary)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS events (
  id VARCHAR(191) PRIMARY KEY,
  session_id VARCHAR(191) NOT NULL,
  actor_unit_id VARCHAR(191),
  target_unit_id VARCHAR(191),
  event_type VARCHAR(191) NOT NULL,
  reason_code VARCHAR(191) NOT NULL,
  payload_json LONGTEXT NOT NULL,
  occurred_at VARCHAR(64) NOT NULL DEFAULT '',
  world_id VARCHAR(191) NULL,
  region_id VARCHAR(191) NULL,
  tick BIGINT NOT NULL DEFAULT 0,
  INDEX idx_events_session_id (session_id),
  INDEX idx_events_actor_unit_id (actor_unit_id),
  INDEX idx_events_target_unit_id (target_unit_id),
  INDEX idx_events_reason_code (reason_code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS relations (
  source_unit_id VARCHAR(191) NOT NULL,
  target_unit_id VARCHAR(191) NOT NULL,
  trust DOUBLE NOT NULL DEFAULT 0,
  fear DOUBLE NOT NULL DEFAULT 0,
  affection DOUBLE NOT NULL DEFAULT 0,
  rivalry DOUBLE NOT NULL DEFAULT 0,
  notes_json LONGTEXT NOT NULL,
  updated_at VARCHAR(64) NOT NULL DEFAULT '',
  PRIMARY KEY (source_unit_id, target_unit_id),
  INDEX idx_relations_target_unit_id (target_unit_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS event_reason_codes (
  code VARCHAR(191) PRIMARY KEY,
  category VARCHAR(191) NOT NULL,
  display_name VARCHAR(191) NOT NULL,
  default_reason_text TEXT NOT NULL,
  stat_domains_json LONGTEXT NOT NULL,
  importance_min INTEGER NOT NULL,
  importance_max INTEGER NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS terrain_types (
  id VARCHAR(191) PRIMARY KEY,
  display_name VARCHAR(191) NOT NULL,
  move_cost DOUBLE NOT NULL,
  vision_range INTEGER NOT NULL,
  combat_rules_json LONGTEXT NOT NULL,
  activities_json LONGTEXT NOT NULL,
  resources_json LONGTEXT NOT NULL,
  special_rules_json LONGTEXT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS world_maps (
  id VARCHAR(191) PRIMARY KEY,
  seed BIGINT NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  generated_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_world_maps_generated_at (generated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS world_tiles (
  map_id VARCHAR(191) NOT NULL,
  q INTEGER NOT NULL,
  r INTEGER NOT NULL,
  terrain_id VARCHAR(191) NOT NULL,
  region_id VARCHAR(191) NOT NULL DEFAULT '',
  landmark VARCHAR(191) NOT NULL DEFAULT '',
  PRIMARY KEY (map_id, q, r),
  INDEX idx_world_tiles_terrain_id (terrain_id),
  CONSTRAINT fk_world_tiles_map FOREIGN KEY (map_id) REFERENCES world_maps(id) ON DELETE CASCADE,
  CONSTRAINT fk_world_tiles_terrain FOREIGN KEY (terrain_id) REFERENCES terrain_types(id) ON DELETE RESTRICT
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS ground_loot_drops (
  id VARCHAR(191) PRIMARY KEY,
  location VARCHAR(191) NOT NULL,
  source_unit_id VARCHAR(191) NOT NULL,
  inheritor_unit_id VARCHAR(191) NOT NULL,
  items_json LONGTEXT NOT NULL,
  created_at VARCHAR(64) NOT NULL DEFAULT ''
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS single_player_sessions (
  id VARCHAR(191) PRIMARY KEY,
  state_json LONGTEXT NOT NULL,
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  updated_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_single_player_sessions_updated_at (updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS session_phase_snapshots (
  id VARCHAR(191) PRIMARY KEY,
  session_id VARCHAR(191) NOT NULL,
  turn INTEGER NOT NULL,
  phase VARCHAR(64) NOT NULL,
  snapshot_json LONGTEXT NOT NULL,
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  UNIQUE KEY uq_session_phase (session_id, turn, phase),
  INDEX idx_session_phase_snapshots_session_created_at (session_id, created_at),
  CONSTRAINT fk_phase_session FOREIGN KEY (session_id) REFERENCES single_player_sessions(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS hall_of_fame_entries (
  id VARCHAR(191) PRIMARY KEY,
  source_session_id VARCHAR(191) NOT NULL,
  source_unit_id VARCHAR(191) NOT NULL,
  unit_name VARCHAR(191) NOT NULL,
  unit_faction_id VARCHAR(191) NOT NULL,
  outcome VARCHAR(64) NOT NULL,
  biography_summary TEXT NOT NULL,
  top_events_json LONGTEXT NOT NULL,
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  UNIQUE KEY uq_hall_source (source_session_id, source_unit_id),
  INDEX idx_hall_of_fame_entries_unit_name (unit_name, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS opening_candidate_cache (
  cache_key VARCHAR(191) PRIMARY KEY,
  payload LONGTEXT NOT NULL,
  updated_at_unix BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS cross_events (
  id VARCHAR(191) PRIMARY KEY,
  world_id VARCHAR(191) NOT NULL,
  actor_unit_id VARCHAR(191) NULL,
  target_unit_id VARCHAR(191) NULL,
  event_kind VARCHAR(64) NOT NULL,
  region_id VARCHAR(191) NULL,
  importance INT NOT NULL DEFAULT 0,
  world_tick BIGINT NOT NULL DEFAULT 0,
  payload_json LONGTEXT NOT NULL,
  occurred_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_cross_events_world (world_id, world_tick, occurred_at),
  INDEX idx_cross_events_actor (actor_unit_id),
  INDEX idx_cross_events_target (target_unit_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS worlds (
  id VARCHAR(191) PRIMARY KEY,
  name VARCHAR(191) NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'active',
  tick BIGINT NOT NULL DEFAULT 0,
  max_population INT NOT NULL DEFAULT 0,
  region_seed VARCHAR(191) NOT NULL DEFAULT '',
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_worlds_status (status, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS world_members (
  world_id VARCHAR(191) NOT NULL,
  character_unit_id VARCHAR(191) NOT NULL,
  role VARCHAR(64) NOT NULL DEFAULT 'inhabitant',
  joined_at VARCHAR(64) NOT NULL DEFAULT '',
  PRIMARY KEY (world_id, character_unit_id),
  INDEX idx_world_members_character (character_unit_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS world_bosses (
  id VARCHAR(191) PRIMARY KEY,
  world_id VARCHAR(191) NOT NULL,
  name VARCHAR(191) NOT NULL,
  hp_max INT NOT NULL,
  hp_remaining INT NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'active',
  region_id VARCHAR(191) NOT NULL DEFAULT '',
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_world_bosses_world (world_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS product_events (
  id VARCHAR(191) PRIMARY KEY,
  stage VARCHAR(32) NOT NULL,
  event_name VARCHAR(64) NOT NULL,
  session_id VARCHAR(191) NULL,
  unit_id VARCHAR(191) NULL,
  properties_json LONGTEXT NOT NULL,
  occurred_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_product_events_name (event_name, occurred_at),
  INDEX idx_product_events_session (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS relevance_anchors (
  character_unit_id VARCHAR(191) NOT NULL,
  anchor_kind VARCHAR(32) NOT NULL,
  anchor_ref VARCHAR(191) NOT NULL,
  weight DOUBLE NOT NULL DEFAULT 0,
  label VARCHAR(255) NOT NULL DEFAULT '',
  half_life_days DOUBLE NOT NULL DEFAULT 14,
  updated_at VARCHAR(64) NOT NULL DEFAULT '',
  PRIMARY KEY (character_unit_id, anchor_kind, anchor_ref),
  INDEX idx_relevance_anchors_char (character_unit_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS decision_traces (
  id VARCHAR(191) PRIMARY KEY,
  session_id VARCHAR(191) NOT NULL,
  unit_id VARCHAR(191) NULL,
  trace_json LONGTEXT NOT NULL,
  occurred_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_decision_traces_session (session_id, occurred_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS llm_interactions (
  id VARCHAR(191) PRIMARY KEY,
  session_id VARCHAR(191) NOT NULL,
  unit_id VARCHAR(191) NULL,
  interaction_json LONGTEXT NOT NULL,
  occurred_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_llm_interactions_session (session_id, occurred_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS raw_event_log (
  id VARCHAR(191) PRIMARY KEY,
  session_id VARCHAR(191) NOT NULL,
  unit_id VARCHAR(191) NULL,
  event_json LONGTEXT NOT NULL,
  occurred_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_raw_event_log_session (session_id, occurred_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- region-runner 调度地基（沙盘 §8.2 / §9，M7.3）：唤醒队列 + 决策作业队列（worker 池原子认领）。shadow/additive。
CREATE TABLE IF NOT EXISTS agent_wake_queue (
  unit_id VARCHAR(191) PRIMARY KEY,
  session_id VARCHAR(191) NULL,
  world_id VARCHAR(191) NULL,
  region_id VARCHAR(191) NULL,
  wake_at_tick BIGINT NOT NULL DEFAULT 0,
  tier VARCHAR(16) NOT NULL DEFAULT 'hot',
  enqueued_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_agent_wake_region_due (region_id, wake_at_tick)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS agent_decision_jobs (
  id VARCHAR(191) PRIMARY KEY,
  unit_id VARCHAR(191) NOT NULL,
  session_id VARCHAR(191) NULL,
  world_id VARCHAR(191) NULL,
  region_id VARCHAR(191) NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'pending',
  tick BIGINT NOT NULL DEFAULT 0,
  attempt INT NOT NULL DEFAULT 0,
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  claimed_at VARCHAR(64) NULL,
  completed_at VARCHAR(64) NULL,
  INDEX idx_agent_jobs_status (status, created_at),
  INDEX idx_agent_jobs_claimed (status, claimed_at),
  -- region 维度认领（ClaimNextJobInRegion）的覆盖索引：让多实例分片的 FOR UPDATE 只锁本区 pending 段、
  -- 不跨区过锁，兑现 per-region 并行吞吐（§11.2 单区单写者串行、跨区并行）。
  INDEX idx_agent_jobs_region (region_id, status, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS fake_door_leads (
  id VARCHAR(191) PRIMARY KEY,
  kind VARCHAR(32) NOT NULL DEFAULT 'lead',
  vid VARCHAR(191) NULL,
  email VARCHAR(255) NULL,
  source VARCHAR(191) NULL,
  payload_json LONGTEXT NOT NULL,
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_fake_door_leads_kind (kind, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS social_objects (
  id VARCHAR(191) PRIMARY KEY,
  world_id VARCHAR(191) NOT NULL,
  kind VARCHAR(64) NOT NULL,
  label VARCHAR(255) NOT NULL DEFAULT '',
  status VARCHAR(32) NOT NULL DEFAULT 'active',
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_social_objects_world (world_id, kind)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS social_object_members (
  object_id VARCHAR(191) NOT NULL,
  unit_id VARCHAR(191) NOT NULL,
  score DOUBLE NOT NULL DEFAULT 0,
  joined_at VARCHAR(64) NOT NULL DEFAULT '',
  PRIMARY KEY (object_id, unit_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS consent_requests (
  id VARCHAR(191) PRIMARY KEY,
  world_id VARCHAR(191) NOT NULL,
  actor_unit_id VARCHAR(191) NOT NULL,
  target_unit_id VARCHAR(191) NOT NULL,
  interaction VARCHAR(32) NOT NULL,
  tier VARCHAR(32) NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'pending',
  event_id VARCHAR(191) NULL,
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  resolved_at VARCHAR(64) NULL,
  INDEX idx_consent_requests_target (target_unit_id, status),
  INDEX idx_consent_requests_status (status, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS fate_decision_resolutions (
  decision_id VARCHAR(191) PRIMARY KEY,
  unit_id VARCHAR(191) NOT NULL,
  resolve_type VARCHAR(32) NOT NULL,
  resolved_at VARCHAR(64) NOT NULL DEFAULT ''
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 商业化/合规/配额/region 租约（P2，flag-gated）。无 account/units 外键（跨分片）；金额一律最小货币单位（cents/micro_usd）。

-- 售卖项目（SKU）目录：kind 区分订阅/一次性/消耗品，price_cents 最小货币单位，active 软上下架。
CREATE TABLE IF NOT EXISTS billing_skus (
  id VARCHAR(191) PRIMARY KEY,
  kind VARCHAR(64) NOT NULL DEFAULT '',
  name VARCHAR(191) NOT NULL DEFAULT '',
  price_cents BIGINT NOT NULL DEFAULT 0,
  period VARCHAR(32) NOT NULL DEFAULT '',
  active TINYINT NOT NULL DEFAULT 1,
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_billing_skus_active (active)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 账户权益：账户对 SKU 的当前权益态，复合主键保证一账户一 SKU 一条。
CREATE TABLE IF NOT EXISTS account_entitlements (
  account_id VARCHAR(191) NOT NULL,
  sku_id VARCHAR(191) NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT '',
  granted_at VARCHAR(64) NOT NULL DEFAULT '',
  expires_at VARCHAR(64) NULL,
  PRIMARY KEY (account_id, sku_id),
  INDEX idx_account_entitlements_account (account_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 计费流水：每次购买/扣款一条，append-only 审计；amount_cents 最小货币单位。
CREATE TABLE IF NOT EXISTS billing_charges (
  id VARCHAR(191) PRIMARY KEY,
  account_id VARCHAR(191) NOT NULL,
  sku_id VARCHAR(191) NOT NULL,
  amount_cents BIGINT NOT NULL DEFAULT 0,
  provider VARCHAR(64) NOT NULL DEFAULT '',
  receipt_ref VARCHAR(191) NOT NULL DEFAULT '',
  status VARCHAR(32) NOT NULL DEFAULT '',
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_billing_charges_account (account_id),
  INDEX idx_billing_charges_sku (sku_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- IAP 收据：Apple/Google 原始收据存证，verified 校验闩；receipt_blob 留原文供复核/补验。
CREATE TABLE IF NOT EXISTS iap_receipts (
  id VARCHAR(191) PRIMARY KEY,
  account_id VARCHAR(191) NOT NULL,
  platform VARCHAR(32) NOT NULL DEFAULT '',
  receipt_blob LONGTEXT NOT NULL,
  verified TINYINT NOT NULL DEFAULT 0,
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_iap_receipts_account (account_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 账户 LLM 配额：按 period_bucket 累计已花 micro_usd 与上限，一账户一条；CheckQuota 读它判放行。
CREATE TABLE IF NOT EXISTS account_llm_quota (
  account_id VARCHAR(191) PRIMARY KEY,
  period_bucket VARCHAR(32) NOT NULL DEFAULT '',
  spent_micro_usd BIGINT NOT NULL DEFAULT 0,
  cap_micro_usd BIGINT NOT NULL DEFAULT 0,
  updated_at VARCHAR(64) NOT NULL DEFAULT ''
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 账户合规态：实名/未成年模式/防沉迷（day_bucket 当日累计在线秒数）；compliance.Gate 读它判宵禁/时长。
CREATE TABLE IF NOT EXISTS account_compliance (
  account_id VARCHAR(191) PRIMARY KEY,
  birth_date VARCHAR(32) NOT NULL DEFAULT '',
  realname_verified TINYINT NOT NULL DEFAULT 0,
  minor_mode TINYINT NOT NULL DEFAULT 0,
  day_bucket VARCHAR(32) NOT NULL DEFAULT '',
  daily_play_seconds BIGINT NOT NULL DEFAULT 0,
  updated_at VARCHAR(64) NOT NULL DEFAULT ''
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- region 租约：holder 持有某 region 至 expires_at（一 region 一条），region-runner 据此分片独占调度。
CREATE TABLE IF NOT EXISTS region_leases (
  region_id VARCHAR(191) PRIMARY KEY,
  holder VARCHAR(191) NOT NULL DEFAULT '',
  expires_at VARCHAR(64) NULL,
  updated_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_region_leases_expires (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- region 注册表（多世界模型 region 实体地基，设计 docs/大世界沙盘设计方案.md §8.1）：把 region 从
-- 「region_id==sessionID」隐式约定扶正为 worlds 下一等子实体，承载区级活跃度（HOT/WARM/COLD）、
-- threat_level（威胁累积，供 PvE「天然扎堆」结算）、last_tick（最近推进的逻辑时钟值）。纯新增。
CREATE TABLE IF NOT EXISTS regions (
  id VARCHAR(191) PRIMARY KEY,
  world_id VARCHAR(191) NOT NULL DEFAULT '',
  activity_tier VARCHAR(16) NOT NULL DEFAULT 'cold',
  threat_level BIGINT NOT NULL DEFAULT 0,
  last_tick BIGINT NOT NULL DEFAULT 0,
  updated_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_regions_world_tier (world_id, activity_tier)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- per-region 逻辑时钟（设计 §8.1）：worlds.tick 是世界级全局序，本表是每个 region 自己的单调发号器。
-- AdvanceRegionTick 原子 +1（双驱动，对齐 world.AdvanceTick：MySQL 走 SELECT…FOR UPDATE + UPDATE）。
CREATE TABLE IF NOT EXISTS world_ticks (
  world_id VARCHAR(191) NOT NULL,
  region_id VARCHAR(191) NOT NULL,
  tick BIGINT NOT NULL DEFAULT 0,
  updated_at VARCHAR(64) NOT NULL DEFAULT '',
  PRIMARY KEY (world_id, region_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
