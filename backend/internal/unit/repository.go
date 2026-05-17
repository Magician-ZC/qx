package unit

// 文件说明：单位仓储实现，负责记录初始化、序列化持久化、按 ID 读取与会话内列表查询。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"

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
	}
}

// Save 写入单位记录（不存在则插入，存在则更新）。
func (repository *Repository) Save(ctx context.Context, record Record) error {
	return repository.saveWithExecer(ctx, repository.db, record)
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

// saveWithExecer 使用指定执行器持久化单位，便于复用事务上下文。
func (repository *Repository) saveWithExecer(ctx context.Context, execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, record Record) error {
	record = repository.mergePersistentSocialState(ctx, record)

	encodedProfile, err := json.Marshal(profileDocument{
		Identity: record.Identity,
		Stats:    record.Stats,
		Skills:   record.Skills,
		Social:   record.Social,
		Memory:   record.Memory,
	})
	if err != nil {
		return fmt.Errorf("marshal unit profile: %w", err)
	}

	encodedPersonality, err := json.Marshal(record.Personality)
	if err != nil {
		return fmt.Errorf("marshal unit personality: %w", err)
	}

	encodedStatus, err := json.Marshal(record.Status)
	if err != nil {
		return fmt.Errorf("marshal unit status: %w", err)
	}

	encodedInventory, err := json.Marshal(record.Inventory)
	if err != nil {
		return fmt.Errorf("marshal unit inventory: %w", err)
	}

	query := `
		INSERT INTO units (
			id,
			session_id,
			faction_id,
			display_name,
			profile_json,
			personality_json,
			status_json,
			inventory_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			session_id = excluded.session_id,
			faction_id = excluded.faction_id,
			display_name = excluded.display_name,
			profile_json = excluded.profile_json,
			personality_json = excluded.personality_json,
			status_json = excluded.status_json,
			inventory_json = excluded.inventory_json,
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
			inventory_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			session_id = VALUES(session_id),
			faction_id = VALUES(faction_id),
			display_name = VALUES(display_name),
			profile_json = VALUES(profile_json),
			personality_json = VALUES(personality_json),
			status_json = VALUES(status_json),
			inventory_json = VALUES(inventory_json),
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
		string(encodedProfile),
		string(encodedPersonality),
		string(encodedStatus),
		string(encodedInventory),
	); err != nil {
		return fmt.Errorf("save unit %s: %w", record.ID, err)
	}

	return nil
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
			inventory_json
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
