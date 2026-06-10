package unit

// 文件说明：单位仓储实现，负责记录初始化、序列化持久化、按 ID 读取与会话内列表查询。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/google/uuid"

	"qunxiang/backend/internal/storage/dbdialect"
)

// Repository 提供单位记录的持久化访问能力。
type Repository struct {
	db *sql.DB
}

// NewRepository 创建单位仓库实例。
func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// BootstrapRecord 生成一个带默认属性的初始单位记录。

var avatarFiles = []string{
	"generated_001_male_warrior.svg", "generated_002_male_archer.svg", "generated_003_male_scout.svg", "generated_004_male_guardian.svg", "generated_005_male_healer.svg", "generated_006_male_merchant.svg", "generated_007_male_scholar.svg", "generated_008_male_raider.svg", "generated_009_male_wanderer.svg", "generated_010_male_hunter.svg", "generated_011_male_farmer.svg", "generated_012_male_miner.svg", "generated_013_male_blacksmith.svg", "generated_014_male_bard.svg", "generated_015_male_monk.svg", "generated_016_male_alchemist.svg", "generated_017_male_engineer.svg", "generated_018_male_spy.svg", "generated_019_male_ranger.svg", "generated_020_male_noble.svg", "generated_021_male_priest.svg", "generated_022_male_cook.svg", "generated_023_male_messenger.svg", "generated_024_male_beastmaster.svg", "generated_025_male_herbalist.svg", "generated_026_male_sailor.svg", "generated_027_male_cartographer.svg", "generated_028_male_dancer.svg", "generated_029_male_duelist.svg", "generated_030_male_tactician.svg", "generated_031_male_nomad.svg", "generated_032_male_artisan.svg", "generated_033_male_oracle.svg", "generated_034_male_warden.svg", "generated_035_male_spearman.svg", "generated_036_male_crossbow.svg", "generated_037_male_cavalier.svg", "generated_038_male_scribe.svg", "generated_039_male_diplomat.svg", "generated_040_male_performer.svg", "generated_041_male_apothecary.svg", "generated_042_male_fisher.svg", "generated_043_male_porter.svg", "generated_044_male_innkeeper.svg", "generated_045_male_inventor.svg", "generated_046_male_sentinel.svg", "generated_047_male_pilgrim.svg", "generated_048_male_taxer.svg", "generated_049_male_weaver.svg", "generated_050_male_drummer.svg", "generated_051_female_warrior.svg", "generated_052_female_archer.svg", "generated_053_female_scout.svg", "generated_054_female_guardian.svg", "generated_055_female_healer.svg", "generated_056_female_merchant.svg", "generated_057_female_scholar.svg", "generated_058_female_raider.svg", "generated_059_female_wanderer.svg", "generated_060_female_hunter.svg", "generated_061_female_farmer.svg", "generated_062_female_miner.svg", "generated_063_female_blacksmith.svg", "generated_064_female_bard.svg", "generated_065_female_monk.svg", "generated_066_female_alchemist.svg", "generated_067_female_engineer.svg", "generated_068_female_spy.svg", "generated_069_female_ranger.svg", "generated_070_female_noble.svg", "generated_071_female_priest.svg", "generated_072_female_cook.svg", "generated_073_female_messenger.svg", "generated_074_female_beastmaster.svg", "generated_075_female_herbalist.svg", "generated_076_female_sailor.svg", "generated_077_female_cartographer.svg", "generated_078_female_dancer.svg", "generated_079_female_duelist.svg", "generated_080_female_tactician.svg", "generated_081_female_nomad.svg", "generated_082_female_artisan.svg", "generated_083_female_oracle.svg", "generated_084_female_warden.svg", "generated_085_female_spearman.svg", "generated_086_female_crossbow.svg", "generated_087_female_cavalier.svg", "generated_088_female_scribe.svg", "generated_089_female_diplomat.svg", "generated_090_female_performer.svg", "generated_091_female_apothecary.svg", "generated_092_female_fisher.svg", "generated_093_female_porter.svg", "generated_094_female_innkeeper.svg", "generated_095_female_inventor.svg", "generated_096_female_sentinel.svg", "generated_097_female_pilgrim.svg", "generated_098_female_taxer.svg", "generated_099_female_weaver.svg", "generated_100_female_drummer.svg",
}

func getAvatarURL(id string, gender string) string {
	h := fnv.New32a()
	h.Write([]byte(id))
	hash := int(h.Sum32())
	if hash < 0 {
		hash = -hash
	}

	pool := avatarFiles[:50]
	if gender == "female" {
		pool = avatarFiles[50:]
	}

	filename := pool[hash%len(pool)]
	return "/characters/" + filename
}

func BootstrapRecord(seed int64, sessionID string, factionID string, name string) Record {
	id := uuid.NewString()
	gender := "male" // Default or let it be assigned by logic
	if seed%2 == 0 {
		gender = "female"
	}
	return Record{
		ID:        id,
		SessionID: sessionID,
		FactionID: factionID,
		Identity: Identity{
			Name:             name,
			Nickname:         "",
			PortraitURL:      getAvatarURL(id, gender),
			Gender:           gender,
			Lineage:          "wanderer",
			Age:              24,
			Biography:        "",
			RecruitmentPitch: "",
		},
		Stats: Stats{
			Primary: PrimaryStats{
				Strength:     10,
				Dexterity:    10,
				Constitution: 10,
				Wisdom:       10,
				Perception:   10,
				Charisma:     10,
			},
			Derived: DerivedStats{
				Attack:      10,
				Defense:     5,
				Accuracy:    10,
				Evasion:     5,
				Vision:      5,
				CarryWeight: 6,
			},
			Growth: GrowthStats{
				Level:       1,
				Experience:  0,
				SkillPoints: 0,
			},
		},
		Skills: SkillSet{
			Weapons: WeaponSkills{
				Sword:   1,
				Bow:     1,
				Blunt:   1,
				Shield:  1,
				Medical: 1,
			},
			Survival: SurvivalSkills{
				Scouting:  1,
				Stealth:   1,
				Medicine:  1,
				Gathering: 1,
			},
			Social: SocialSkills{
				Negotiation:  1,
				Intimidation: 1,
				Charm:        1,
				Trade:        1,
			},
			Specialties: []string{"field_adaptable"},
		},
		Personality: GeneratePersonality(seed),
		Social: SocialState{
			ParentUnitIDs: []string{},
			ChildUnitIDs:  []string{},
		},
		Status: Status{
			HP:              100,
			MP:              0,
			LivesRemaining:  3,
			LivesMax:        3,
			LifeState:       LifeStateActive,
			RecoveryTurns:   0,
			Attack:          10,
			Defense:         5,
			Move:            4,
			Hunger:          100,
			StarvationTurns: 0,
			Fatigue:         0,
			Mood:            "calm",
			Morale:          0.7,
			Loyalty:         0.7,
			Wallet:          100,
			PositionQ:       0,
			PositionR:       0,
			InCombat:        false,
			Injuries:        []string{},
			Debuffs:         []string{},
		},
		Memory: MemoryProfile{
			RecentEventIDs: []string{},
			Highlights:     []string{},
		},
		Inventory: Inventory{
			Equipment: map[string]ItemStack{},
			Backpack:  []ItemStack{},
		},
		// 六维野心向量确定性派生（unit.Ambition 在全代码库的唯一写入源，覆盖所有建人路径）。
		Ambition: DeriveAmbition(seed, "wanderer", ""),
	}
}

// Save 写入单位记录（不存在则插入，存在则更新）。
func (repository *Repository) Save(ctx context.Context, record Record) error {
	return repository.saveWithExecer(ctx, repository.db, record)
}

// ErrCrossSessionWrite 表示一次写入越过了「跨玩家硬不变量」：试图改写不属于操作方 session 的他人单位
// （设计宪法红线：B 永远只能写一条 cross_event，改不了 A 的 units）。SaveOwnedBy 命中此情形即返回本错误、拒绝写入。
var ErrCrossSessionWrite = errors.New("cross-session unit write forbidden: a player may never directly write another player's unit (use cross_event)")

// SaveOwnedBy 是带「本侧归属断言」的条件写：仅当目标单位的**已落库 session_id == ownerSessionID** 时才落盘，
// 否则拒绝写入并返回 ErrCrossSessionWrite（共享世界 Phase 3 跨玩家硬不变量的 **storage 层强制**——
// 设计文档 §2.1/§5：「跨玩家只产 append-only cross_event，各自 Mutator 只改本侧；storage 断言禁 cross-session UPDATE」）。
//
// 守门规则（先读 session_id 决定是否写）：
//   - 行不存在（新插入，跨分片远端角色本就无本地行）→ 拒绝（SaveOwnedBy 只用于「改写已存在的本侧单位」，
//     新建走 Save；这样误把他人 id 当新行插入也会被挡）。
//   - 行存在且 session_id == ownerSessionID（本侧单位）→ 放行，走与 Save 等价的整记录 upsert。
//   - 行存在但 session_id != ownerSessionID（他人单位）→ 拒绝写、返回 ErrCrossSessionWrite。
//
// ⚠️ **不开显式事务跨整段**（避免 SQLite 单连接 `SetMaxOpenConns(1)` 自死锁）：先用 db 读一次 session_id（连接随即释放），
// 校验通过再走普通 Save（其内部各自取/还连接）。session_id 是单位创建时即固定、此后写入也**不改**它的不可变锚——
// 故「先读后写」无实质 TOCTOU 风险（两步间 session_id 不会变），且并发丢更新由调用层的乐观并发（version/SaveOptimistic）兜底。
//
// ownerSessionID 为空时退化为不设归属约束（等价 Save）——仅供「无 session 上下文的合法系统写」用；
// 跨玩家结算路径必须传**操作方自己的 sessionID**，让越界直写在 storage 层被挡。
func (repository *Repository) SaveOwnedBy(ctx context.Context, record Record, ownerSessionID string) error {
	if repository == nil || repository.db == nil {
		return fmt.Errorf("save owned-by: nil repository or db")
	}
	if strings.TrimSpace(ownerSessionID) == "" {
		return repository.Save(ctx, record) // 无归属上下文：退化为普通 Save（不施加跨玩家约束）。
	}
	var persistedSession string
	err := repository.db.QueryRowContext(ctx, `SELECT session_id FROM units WHERE id = ?`, record.ID).Scan(&persistedSession)
	if err == sql.ErrNoRows {
		// 不存在本侧行：SaveOwnedBy 不负责新建（新建走 Save），且这样能挡住「把他人 id 误当新行插入」。
		return fmt.Errorf("save owned-by %s: %w (no local row to update)", record.ID, ErrCrossSessionWrite)
	}
	if err != nil {
		return fmt.Errorf("save owned-by lookup %s: %w", record.ID, err)
	}
	if strings.TrimSpace(persistedSession) != strings.TrimSpace(ownerSessionID) {
		// 他人单位：storage 层硬拒（跨玩家硬不变量第一道防线）。
		return fmt.Errorf("save owned-by %s (persisted session=%q, owner=%q): %w",
			record.ID, persistedSession, ownerSessionID, ErrCrossSessionWrite)
	}
	// 本侧单位：走普通 Save（整记录 upsert，含 mergePersistentSocialState）——每步各自取/还连接，无单连接自死锁。
	return repository.Save(ctx, record)
}

// mergePersistentSocialState 避免旧快照保存时把已成立的家庭关系清空。
func (repository *Repository) mergePersistentSocialState(ctx context.Context, record Record) Record {
	if repository == nil || repository.db == nil || record.ID == "" {
		return record
	}
	current, err := repository.GetByID(ctx, record.ID)
	if err != nil {
		return record
	}
	record.Social = mergeSocialState(record.Social, current.Social)
	return record
}

func mergeSocialState(next SocialState, current SocialState) SocialState {
	if next.LoverUnitID == "" {
		next.LoverUnitID = current.LoverUnitID
	}
	if next.BornTurn == 0 {
		next.BornTurn = current.BornTurn
	}
	if next.LastRomanceTurn == 0 {
		next.LastRomanceTurn = current.LastRomanceTurn
	}
	next.ParentUnitIDs = mergeStringSet(next.ParentUnitIDs, current.ParentUnitIDs)
	next.ChildUnitIDs = mergeStringSet(next.ChildUnitIDs, current.ChildUnitIDs)
	return next
}

func mergeStringSet(next []string, current []string) []string {
	if len(current) == 0 {
		return next
	}
	seen := make(map[string]struct{}, len(next)+len(current))
	merged := make([]string, 0, len(next)+len(current))
	for _, value := range next {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		merged = append(merged, value)
	}
	for _, value := range current {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		merged = append(merged, value)
	}
	return merged
}

// marshalUnitBlobs 把 Record 的四个 JSON blob 序列化出来（Save / SaveOptimistic 共用）。
func marshalUnitBlobs(record Record) (profile, personality, statusJSON, inventory string, err error) {
	encodedProfile, err := json.Marshal(profileDocument{
		Identity:         record.Identity,
		Stats:            record.Stats,
		Skills:           record.Skills,
		Social:           record.Social,
		Memory:           record.Memory,
		Ambition:         record.Ambition,
		Faction:          record.Faction,
		MoralAlignment:   record.MoralAlignment,
		MoralDriftStreak: record.MoralDriftStreak,
		Pinned:           record.Pinned,
	})
	if err != nil {
		return "", "", "", "", fmt.Errorf("marshal unit profile: %w", err)
	}
	encodedPersonality, err := json.Marshal(record.Personality)
	if err != nil {
		return "", "", "", "", fmt.Errorf("marshal unit personality: %w", err)
	}
	encodedStatus, err := json.Marshal(record.Status)
	if err != nil {
		return "", "", "", "", fmt.Errorf("marshal unit status: %w", err)
	}
	encodedInventory, err := json.Marshal(record.Inventory)
	if err != nil {
		return "", "", "", "", fmt.Errorf("marshal unit inventory: %w", err)
	}
	return string(encodedProfile), string(encodedPersonality), string(encodedStatus), string(encodedInventory), nil
}

// saveWithExecer 使用指定执行器持久化单位，便于复用事务上下文。
func (repository *Repository) saveWithExecer(ctx context.Context, execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, record Record) error {
	record = repository.mergePersistentSocialState(ctx, record)

	encodedProfile, encodedPersonality, encodedStatus, encodedInventory, err := marshalUnitBlobs(record)
	if err != nil {
		return err
	}

	// life_state 双写灰度（沙盘 §8.7）：把 status_json.LifeState 去规范化到可查询的 life_state 列（每次 Save 同步）。
	// version 每次更新单调 +1（乐观并发版本号，real-3-0）——所有写者透明递增，供 SaveOptimistic 检测并发修改。
	// world_id/region_id/last_active_tick 由调度层方法赋值（SetUnitScope/TouchLastActiveTick），故**不**在此写，
	// 避免被每次 Save 覆盖回默认值——新插入时取列默认、更新时保留。
	query := `
		INSERT INTO units (
			id,
			session_id,
			faction_id,
			display_name,
			profile_json,
			personality_json,
			status_json,
			inventory_json,
			life_state
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			session_id = excluded.session_id,
			faction_id = excluded.faction_id,
			display_name = excluded.display_name,
			profile_json = excluded.profile_json,
			personality_json = excluded.personality_json,
			status_json = excluded.status_json,
			inventory_json = excluded.inventory_json,
			life_state = excluded.life_state,
			version = version + 1,
			updated_at = CURRENT_TIMESTAMP
		`
	if dbdialect.IsMySQL(repository.db) {
		query = `
		INSERT INTO units (
			id,
			session_id,
			faction_id,
			display_name,
			profile_json,
			personality_json,
			status_json,
			inventory_json,
			life_state
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			session_id = VALUES(session_id),
			faction_id = VALUES(faction_id),
			display_name = VALUES(display_name),
			profile_json = VALUES(profile_json),
			personality_json = VALUES(personality_json),
			status_json = VALUES(status_json),
			inventory_json = VALUES(inventory_json),
			life_state = VALUES(life_state),
			version = version + 1,
			updated_at = CURRENT_TIMESTAMP
		`
	}
	if _, err := execer.ExecContext(
		ctx,
		query,
		record.ID,
		record.SessionID,
		record.FactionID,
		record.DisplayName(),
		encodedProfile,
		encodedPersonality,
		encodedStatus,
		encodedInventory,
		normalizedLifeState(record.Status.LifeState),
	); err != nil {
		return fmt.Errorf("save unit %s: %w", record.ID, err)
	}

	return nil
}

// SaveOptimistic 条件写：仅当 units.version 仍等于 record.Version（读时版本）才更新（同语句 version+1），返回是否真的写入。
// applied=false 表示自读取以来该单位被其它写者改过（战斗/HTTP，它们经 Save 必 version+1）——调用方应退避而非覆盖。
// 是 region-runner 离线写让位战斗/HTTP、防丢更新的护栏（real-3-0）。
//
// 写入字段：status_json + **profile_json（整块，含 Social 与 Memory.RecentEventIDs）** + life_state + version；
// 刻意不写 personality_json/inventory_json（缩小冲突面），且**不**调 mergePersistentSocialState。
// ⚠️ 不丢 Social/profile 子字段的保证**完全来自 version 守护**（version 匹配 ⟺ 读后无人改过该行 ⟺ 写回的 Social 即当前值、
// 为幂等 no-op），**而非「没写 Social」**——切勿放宽 version 守护（如改用 updated_at 秒级比较或加宽窗口），否则立即引入
// profile/Social 丢更新。前置不变量：record 必须是同一调用内 GetByID 刚读出的快照（Version 与 Social/profile 同源）。
// 另：applied 判定依赖 SET 内的 version=version+1 使匹配行必「变更」（兼容 go-sql-driver/mysql 的 changed-rows 语义），勿移除。
func (repository *Repository) SaveOptimistic(ctx context.Context, record Record) (bool, error) {
	encodedStatus, err := json.Marshal(record.Status)
	if err != nil {
		return false, fmt.Errorf("marshal unit status: %w", err)
	}
	encodedProfile, _, _, _, err := marshalUnitBlobs(record)
	if err != nil {
		return false, err
	}
	res, err := repository.db.ExecContext(
		ctx,
		`UPDATE units SET status_json = ?, profile_json = ?, life_state = ?, version = version + 1, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ? AND version = ?`,
		string(encodedStatus), encodedProfile, normalizedLifeState(record.Status.LifeState), record.ID, record.Version,
	)
	if err != nil {
		return false, fmt.Errorf("save unit optimistic %s: %w", record.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("save unit optimistic %s rows affected: %w", record.ID, err)
	}
	return affected > 0, nil
}

// normalizedLifeState 把空生命态归一为 active（与 BootstrapRecord 默认一致），保证 life_state 列恒为合法值。
func normalizedLifeState(state string) string {
	if state == "" {
		return LifeStateActive
	}
	return state
}

// GetByID 按单位 ID 加载完整记录。
func (repository *Repository) GetByID(ctx context.Context, unitID string) (Record, error) {
	var record Record
	var displayName string
	var encodedProfile string
	var encodedPersonality string
	var encodedStatus string
	var encodedInventory string

	if err := repository.db.QueryRowContext(
		ctx,
		`
		SELECT
			id,
			session_id,
			faction_id,
			display_name,
			profile_json,
			personality_json,
			status_json,
			inventory_json,
			version
		FROM units
		WHERE id = ?
		`,
		unitID,
	).Scan(
		&record.ID,
		&record.SessionID,
		&record.FactionID,
		&displayName,
		&encodedProfile,
		&encodedPersonality,
		&encodedStatus,
		&encodedInventory,
		&record.Version,
	); err != nil {
		return Record{}, fmt.Errorf("get unit %s: %w", unitID, err)
	}

	var profile profileDocument
	if err := json.Unmarshal([]byte(encodedProfile), &profile); err != nil {
		return Record{}, fmt.Errorf("decode unit profile: %w", err)
	}
	if err := json.Unmarshal([]byte(encodedPersonality), &record.Personality); err != nil {
		return Record{}, fmt.Errorf("decode unit personality: %w", err)
	}
	if err := json.Unmarshal([]byte(encodedStatus), &record.Status); err != nil {
		return Record{}, fmt.Errorf("decode unit status: %w", err)
	}
	if err := json.Unmarshal([]byte(encodedInventory), &record.Inventory); err != nil {
		return Record{}, fmt.Errorf("decode unit inventory: %w", err)
	}

	record.Identity = profile.Identity
	record.Stats = profile.Stats
	record.Skills = profile.Skills
	record.Social = profile.Social
	if record.Social.ParentUnitIDs == nil {
		record.Social.ParentUnitIDs = []string{}
	}
	if record.Social.ChildUnitIDs == nil {
		record.Social.ChildUnitIDs = []string{}
	}
	record.Memory = profile.Memory
	record.Ambition = profile.Ambition
	record.Faction = profile.Faction
	record.MoralAlignment = profile.MoralAlignment
	record.MoralDriftStreak = profile.MoralDriftStreak
	record.Pinned = profile.Pinned
	if record.Identity.Name == "" {
		record.Identity.Name = displayName
	}

	return record, nil
}

// ListBySession 列出某会话下的全部单位记录。
func (repository *Repository) ListBySession(ctx context.Context, sessionID string) ([]Record, error) {
	orderBy := "rowid"
	if dbdialect.IsMySQL(repository.db) {
		orderBy = "created_at, id"
	}
	rows, err := repository.db.QueryContext(
		ctx,
		`
		SELECT
			id,
			session_id,
			faction_id,
			display_name,
			profile_json,
			personality_json,
			status_json,
			inventory_json
		FROM units
		WHERE session_id = ?
		ORDER BY `+orderBy,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list units for session %s: %w", sessionID, err)
	}
	defer rows.Close()

	records := make([]Record, 0)
	for rows.Next() {
		var record Record
		var displayName string
		var encodedProfile string
		var encodedPersonality string
		var encodedStatus string
		var encodedInventory string

		if err := rows.Scan(
			&record.ID,
			&record.SessionID,
			&record.FactionID,
			&displayName,
			&encodedProfile,
			&encodedPersonality,
			&encodedStatus,
			&encodedInventory,
		); err != nil {
			return nil, fmt.Errorf("scan unit for session %s: %w", sessionID, err)
		}

		var profile profileDocument
		if err := json.Unmarshal([]byte(encodedProfile), &profile); err != nil {
			return nil, fmt.Errorf("decode unit profile: %w", err)
		}
		if err := json.Unmarshal([]byte(encodedPersonality), &record.Personality); err != nil {
			return nil, fmt.Errorf("decode unit personality: %w", err)
		}
		if err := json.Unmarshal([]byte(encodedStatus), &record.Status); err != nil {
			return nil, fmt.Errorf("decode unit status: %w", err)
		}
		if err := json.Unmarshal([]byte(encodedInventory), &record.Inventory); err != nil {
			return nil, fmt.Errorf("decode unit inventory: %w", err)
		}

		record.Identity = profile.Identity
		record.Stats = profile.Stats
		record.Skills = profile.Skills
		record.Memory = profile.Memory
		record.Ambition = profile.Ambition
		record.Faction = profile.Faction
		record.MoralAlignment = profile.MoralAlignment
		record.MoralDriftStreak = profile.MoralDriftStreak
		record.Pinned = profile.Pinned
		if record.Identity.Name == "" {
			record.Identity.Name = displayName
		}

		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate units for session %s: %w", sessionID, err)
	}

	return records, nil
}

// DeleteBySession 删除某会话下全部单位并返回影响行数。
func (repository *Repository) DeleteBySession(ctx context.Context, sessionID string) (int64, error) {
	result, err := repository.db.ExecContext(
		ctx,
		`DELETE FROM units WHERE session_id = ?`,
		sessionID,
	)
	if err != nil {
		return 0, fmt.Errorf("delete units for session %s: %w", sessionID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected deleting units for session %s: %w", sessionID, err)
	}
	return affected, nil
}
