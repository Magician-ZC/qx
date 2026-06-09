package session

// 文件说明：开局候选单位生成与玩家选人草案应用逻辑。

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

const (
	openingCandidateCount    = 10
	openingRosterSize        = 10
	openingRecommendedUnits  = 5
	openingCandidateCacheKey = "latest"
)

// RecommendedOpeningUnitCount 返回前端建议展示的每方单位数。
func RecommendedOpeningUnitCount() int { return openingRecommendedUnits }

// MaxOpeningUnitCount 返回每方单位数上限。
func MaxOpeningUnitCount() int { return openingRosterSize }

// NormalizeOpeningUnitCount 将用户输入的单位数约束到 1-10；空值/非法值使用建议值 5。
func NormalizeOpeningUnitCount(count int) int {
	if count <= 0 {
		return openingRecommendedUnits
	}
	if count > openingRosterSize {
		return openingRosterSize
	}
	return count
}

var candidatePortraitRoles = []string{"warrior", "archer", "scout", "guardian", "healer", "merchant", "scholar", "raider", "wanderer", "hunter", "farmer", "miner", "blacksmith", "bard", "monk", "alchemist", "engineer", "spy", "ranger", "noble", "priest", "cook", "messenger", "beastmaster", "herbalist", "sailor", "cartographer", "dancer", "duelist", "tactician", "nomad", "artisan", "oracle", "warden", "spearman", "crossbow", "cavalier", "scribe", "diplomat", "performer", "apothecary", "fisher", "porter", "innkeeper", "inventor", "sentinel", "pilgrim", "taxer", "weaver", "drummer"}

type pregameCandidateResponse struct {
	Candidates []UnitCandidate `json:"candidates"`
}

type openingCandidateCachePayload struct {
	Seed   int64           `json:"seed"`
	Player []UnitCandidate `json:"player"`
	Enemy  []UnitCandidate `json:"enemy"`
}

func (service *Service) openingCandidatesForDraft(ctx context.Context, seed int64) ([]UnitCandidate, []UnitCandidate) {
	if player, enemy, ok := service.loadOpeningCandidateCache(ctx, seed); ok {
		service.refreshOpeningCandidateCacheAsync(seed)
		return player, enemy
	}
	service.refreshOpeningCandidateCacheAsync(seed)
	return fallbackOpeningUnitCandidates(seed), fallbackOpeningUnitCandidates(seed + 2026)
}

func (service *Service) refreshOpeningCandidateCacheAsync(seed int64) {
	if service == nil || service.llm == nil || service.db == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		var playerCandidates []UnitCandidate
		var enemyCandidates []UnitCandidate
		var playerErr error
		var enemyErr error
		var waitGroup sync.WaitGroup
		waitGroup.Add(2)
		go func() {
			defer waitGroup.Done()
			playerCandidates, playerErr = service.GenerateOpeningUnitCandidates(ctx, seed)
		}()
		go func() {
			defer waitGroup.Done()
			enemyCandidates, enemyErr = service.GenerateOpeningUnitCandidates(ctx, seed+2026)
		}()
		waitGroup.Wait()
		if playerErr != nil || enemyErr != nil || len(playerCandidates) == 0 || len(enemyCandidates) == 0 {
			return
		}
		_ = service.saveOpeningCandidateCache(ctx, seed, playerCandidates, enemyCandidates)
	}()
}

func (service *Service) ensureOpeningCandidateCacheTable(ctx context.Context) error {
	if service == nil || service.db == nil {
		return nil
	}
	query := `CREATE TABLE IF NOT EXISTS opening_candidate_cache (
		cache_key TEXT PRIMARY KEY,
		payload TEXT NOT NULL,
		updated_at_unix INTEGER NOT NULL
	)`
	if dbdialect.IsMySQL(service.db) {
		query = `CREATE TABLE IF NOT EXISTS opening_candidate_cache (
			cache_key VARCHAR(191) PRIMARY KEY,
			payload LONGTEXT NOT NULL,
			updated_at_unix BIGINT NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	}
	_, err := service.db.ExecContext(ctx, query)
	return err
}

func (service *Service) loadOpeningCandidateCache(ctx context.Context, seed int64) ([]UnitCandidate, []UnitCandidate, bool) {
	if service == nil || service.db == nil {
		return nil, nil, false
	}
	if err := service.ensureOpeningCandidateCacheTable(ctx); err != nil {
		return nil, nil, false
	}
	var raw string
	if err := service.db.QueryRowContext(ctx, `SELECT payload FROM opening_candidate_cache WHERE cache_key = ?`, openingCandidateCacheKey).Scan(&raw); err != nil {
		return nil, nil, false
	}
	var payload openingCandidateCachePayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, nil, false
	}
	if len(payload.Player) == 0 || len(payload.Enemy) == 0 {
		return nil, nil, false
	}
	return normalizeOpeningCandidates(payload.Player, seed), normalizeOpeningCandidates(payload.Enemy, seed+2026), true
}

func (service *Service) saveOpeningCandidateCache(ctx context.Context, seed int64, player []UnitCandidate, enemy []UnitCandidate) error {
	if service == nil || service.db == nil {
		return nil
	}
	if err := service.ensureOpeningCandidateCacheTable(ctx); err != nil {
		return err
	}
	payload := openingCandidateCachePayload{
		Seed:   seed,
		Player: normalizeOpeningCandidates(player, seed),
		Enemy:  normalizeOpeningCandidates(enemy, seed+2026),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	query := `INSERT INTO opening_candidate_cache(cache_key, payload, updated_at_unix)
		VALUES(?, ?, ?)
		ON CONFLICT(cache_key) DO UPDATE SET payload = excluded.payload, updated_at_unix = excluded.updated_at_unix`
	if dbdialect.IsMySQL(service.db) {
		query = `INSERT INTO opening_candidate_cache(cache_key, payload, updated_at_unix)
		VALUES(?, ?, ?)
		ON DUPLICATE KEY UPDATE payload = VALUES(payload), updated_at_unix = VALUES(updated_at_unix)`
	}
	_, err = service.db.ExecContext(ctx, query,
		openingCandidateCacheKey,
		string(encoded),
		time.Now().Unix(),
	)
	return err
}

// GenerateOpeningUnitCandidates 生成开局 20 个候选单位；LLM 不可用时使用可复现兜底名单。
func (service *Service) GenerateOpeningUnitCandidates(ctx context.Context, seed int64) ([]UnitCandidate, error) {
	if seed == 0 {
		seed = 1
	}
	fallback := fallbackOpeningUnitCandidates(seed)
	if service == nil || service.llm == nil {
		return fallback, nil
	}

	systemPrompt := "你是一念战棋游戏的角色导演，负责生成开局候选单位。只输出 JSON。"
	userPrompt := fmt.Sprintf("请生成 %d 个中文候选单位。每个单位必须有 id、name、gender(male/female/nonbinary)、portrait_url、age、biography、recruitment_pitch、specialties(1-3项)、stats、skills，以及 personality 中 courage/loyalty/aggression/prudence/sociability/integrity/stability/ambition 八项 0-1 数值。stats.primary 六项范围 6-15，stats.derived 表示基础战斗/探索能力：attack 6-18、defense/evasion 2-12、accuracy 5-18、vision 3-7、carry_weight 4-10；skills 三组数值范围 0-5。姓名/外号、生平、招募词、属性必须共同体现 personality：高 courage/aggression 的名字可更硬、更冲、更锋利；高 prudence/stability 的名字更稳、更克制；高 sociability/charisma 的名字可更市井、更会来事；高 integrity/loyalty 的名字更可靠；高 ambition 的名字更有野心。所有属性必须结合生平和性格：例如猎户/斥候更高 perception/scouting/bow/vision，铁匠/矿工更高 strength/blunt/carry_weight，医徒更高 wisdom/medical/medicine，商贩/说客更高 charisma/trade/negotiation，谨慎者 defense/evasion/medicine 可高，莽勇者 attack/strength 可高但 prudence 不要硬凑。portrait_url 从 /characters/generated_001_male_warrior.svg 到 /characters/generated_100_female_drummer.svg 中挑选，男女可按编号段大致匹配。名字不要死板，可以混入互联网流行语、谐音梗、轻度抽象梗或含梗外号，但必须服务于性格，不要为了搞怪而和 personality 矛盾；每局必须新鲜，不要复用固定名单。生平要一人一句独特经历，并解释为什么形成这些性格，不要套用同一句模板。生平、性格和属性可被玩家再编辑。随机种子：%d", openingCandidateCount, seed)
	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskBackstory,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		SchemaName:     "opening_unit_candidates",
		ResponseSchema: buildOpeningUnitCandidatesSchema(),
		Temperature:    0.85,
		MaxTokens:      4096,
		Timeout:        60 * time.Second,
		Fallback: ai.RuleFallbackFunc(func(context.Context, ai.CompletionRequest, error) (json.RawMessage, error) {
			encoded, marshalErr := json.Marshal(pregameCandidateResponse{Candidates: fallback})
			return encoded, marshalErr
		}),
	})
	if err != nil {
		return fallback, nil
	}

	var payload pregameCandidateResponse
	if err := json.Unmarshal(result.Output, &payload); err != nil || len(payload.Candidates) == 0 {
		return fallback, nil
	}
	return normalizeOpeningCandidates(payload.Candidates, seed), nil
}

func normalizeOpeningCandidates(candidates []UnitCandidate, seed int64) []UnitCandidate {
	fallback := fallbackOpeningUnitCandidates(seed)
	result := make([]UnitCandidate, 0, openingCandidateCount)
	seen := map[string]struct{}{}
	for index, candidate := range candidates {
		if len(result) >= openingCandidateCount {
			break
		}
		candidate.ID = strings.TrimSpace(candidate.ID)
		if candidate.ID == "" {
			candidate.ID = fmt.Sprintf("cand-%02d", index+1)
		}
		if _, ok := seen[candidate.ID]; ok {
			candidate.ID = fmt.Sprintf("%s-%02d", candidate.ID, index+1)
		}
		seen[candidate.ID] = struct{}{}
		if strings.TrimSpace(candidate.Name) == "" {
			candidate.Name = fallback[index%len(fallback)].Name
		}
		candidate.Gender = normalizeGender(candidate.Gender, index)
		candidate.PortraitURL = normalizeCandidatePortraitURL(candidate.PortraitURL, candidate.Gender, index, seed)
		if candidate.Age < 16 || candidate.Age > 60 {
			candidate.Age = fallback[index%len(fallback)].Age
		}
		candidate.Biography = limitTextRunes(firstNonEmptyText(candidate.Biography, fallback[index%len(fallback)].Biography), 120)
		candidate.RecruitmentPitch = limitTextRunes(firstNonEmptyText(candidate.RecruitmentPitch, fallback[index%len(fallback)].RecruitmentPitch), 60)
		candidate.Personality = normalizeCandidatePersonality(candidate.Personality, int64(index)+seed)
		if len(candidate.Specialties) == 0 {
			candidate.Specialties = fallback[index%len(fallback)].Specialties
		}
		candidate.Stats = normalizeCandidateStats(candidate.Stats, candidate.Personality, candidate.Biography, candidate.Specialties, int64(index)+seed)
		candidate.Skills = normalizeCandidateSkills(candidate.Skills, candidate.Biography, candidate.Specialties, int64(index)+seed)
		result = append(result, candidate)
	}
	for index := len(result); index < openingCandidateCount; index++ {
		result = append(result, fallback[index])
	}
	return result
}

func fallbackOpeningUnitCandidates(seed int64) []UnitCandidate {
	names := []string{"雾港小满", "盐井阿澈", "夜市锤妹", "断桥老周", "河滩小六", "纸甲阿梨", "火塘阿岚", "借刀书生", "灰棚阿宁", "北坡狸奴", "炊烟斥候", "半盏铁匠", "雨棚药娘", "石牙跑商", "低保刀客", "碎嘴哨兵", "野井账房", "铜铃猎户", "睡不醒军师", "跑路圣手", "红泥搬山", "旧邮差", "缺德向导", "竹篓医徒", "翻墙先生", "冻梨枪客"}
	bioTemplates := []string{
		"%s曾在荒镇替商队探路，记得每条能绕开伏击的小径。",
		"%s从失火驿站逃出后学会了先救人再算账，嘴硬但手很稳。",
		"%s做过临时守夜人，习惯用笑话压住害怕，也最先发现危险。",
		"%s在饥荒年带着邻里换粮，对补给和人心都有一套笨办法。",
		"%s输过一场不该输的架，此后每次动手前都会多看半步退路。",
	}
	specialties := [][]string{{"scout"}, {"shield"}, {"herbalist"}, {"archer"}, {"builder"}, {"forager"}, {"merchant"}, {"duelist"}, {"watcher"}, {"cook"}}
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	result := make([]UnitCandidate, 0, openingCandidateCount)
	for index := 0; index < openingCandidateCount; index++ {
		name := names[index%len(names)]
		gender := normalizeGender("", index)
		age := 18 + rng.Intn(23)
		bio := fmt.Sprintf(bioTemplates[(index+rng.Intn(len(bioTemplates)))%len(bioTemplates)], name)
		result = append(result, UnitCandidate{
			ID:               fmt.Sprintf("cand-%02d", index+1),
			Name:             name,
			Gender:           gender,
			PortraitURL:      normalizeCandidatePortraitURL("", gender, index, seed),
			Age:              age,
			Biography:        bio,
			Stats:            deriveFallbackCandidateStats(bio, specialties[index%len(specialties)], unit.GeneratePersonality(seed+int64(index)*17), seed+int64(index)*29),
			Skills:           deriveFallbackCandidateSkills(bio, specialties[index%len(specialties)], seed+int64(index)*31),
			Personality:      unit.GeneratePersonality(seed + int64(index)*17),
			RecruitmentPitch: fmt.Sprintf("%s说自己能顶上空缺，但讨厌被当成一次性棋子。", name),
			Specialties:      specialties[index%len(specialties)],
		})
	}
	return result
}

func buildOpeningUnitCandidatesSchema() []byte {
	personalityProps := map[string]any{}
	for _, key := range []string{"courage", "loyalty", "aggression", "prudence", "sociability", "integrity", "stability", "ambition"} {
		personalityProps[key] = map[string]any{"type": "number", "minimum": 0, "maximum": 1}
	}
	primaryProps := map[string]any{}
	for _, key := range []string{"strength", "dexterity", "constitution", "wisdom", "perception", "charisma"} {
		primaryProps[key] = map[string]any{"type": "integer", "minimum": 6, "maximum": 15}
	}
	derivedProps := map[string]any{
		"attack":       map[string]any{"type": "integer", "minimum": 6, "maximum": 18},
		"defense":      map[string]any{"type": "integer", "minimum": 2, "maximum": 12},
		"accuracy":     map[string]any{"type": "integer", "minimum": 5, "maximum": 18},
		"evasion":      map[string]any{"type": "integer", "minimum": 2, "maximum": 12},
		"vision":       map[string]any{"type": "integer", "minimum": 3, "maximum": 7},
		"carry_weight": map[string]any{"type": "integer", "minimum": 4, "maximum": 10},
	}
	weaponSkillProps := map[string]any{}
	for _, key := range []string{"sword", "bow", "blunt", "shield", "medical"} {
		weaponSkillProps[key] = map[string]any{"type": "integer", "minimum": 0, "maximum": 5}
	}
	survivalSkillProps := map[string]any{}
	for _, key := range []string{"scouting", "stealth", "medicine", "gathering"} {
		survivalSkillProps[key] = map[string]any{"type": "integer", "minimum": 0, "maximum": 5}
	}
	socialSkillProps := map[string]any{}
	for _, key := range []string{"negotiation", "intimidation", "charm", "trade"} {
		socialSkillProps[key] = map[string]any{"type": "integer", "minimum": 0, "maximum": 5}
	}
	statsSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"primary": map[string]any{
				"type":                 "object",
				"properties":           primaryProps,
				"required":             []string{"strength", "dexterity", "constitution", "wisdom", "perception", "charisma"},
				"additionalProperties": false,
			},
			"derived": map[string]any{
				"type":                 "object",
				"properties":           derivedProps,
				"required":             []string{"attack", "defense", "accuracy", "evasion", "vision", "carry_weight"},
				"additionalProperties": false,
			},
		},
		"required":             []string{"primary", "derived"},
		"additionalProperties": false,
	}
	skillsSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"weapons": map[string]any{
				"type":                 "object",
				"properties":           weaponSkillProps,
				"required":             []string{"sword", "bow", "blunt", "shield", "medical"},
				"additionalProperties": false,
			},
			"survival": map[string]any{
				"type":                 "object",
				"properties":           survivalSkillProps,
				"required":             []string{"scouting", "stealth", "medicine", "gathering"},
				"additionalProperties": false,
			},
			"social": map[string]any{
				"type":                 "object",
				"properties":           socialSkillProps,
				"required":             []string{"negotiation", "intimidation", "charm", "trade"},
				"additionalProperties": false,
			},
		},
		"required":             []string{"weapons", "survival", "social"},
		"additionalProperties": false,
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"candidates": map[string]any{
				"type":     "array",
				"minItems": openingCandidateCount,
				"maxItems": openingCandidateCount,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":                map[string]any{"type": "string", "minLength": 1, "maxLength": 32},
						"name":              map[string]any{"type": "string", "minLength": 1, "maxLength": 16},
						"gender":            map[string]any{"type": "string", "enum": []string{"male", "female", "nonbinary"}},
						"portrait_url":      map[string]any{"type": "string", "minLength": 1, "maxLength": 96},
						"age":               map[string]any{"type": "integer", "minimum": 16, "maximum": 60},
						"biography":         map[string]any{"type": "string", "minLength": 12, "maxLength": 120},
						"recruitment_pitch": map[string]any{"type": "string", "minLength": 6, "maxLength": 60},
						"stats":             statsSchema,
						"skills":            skillsSchema,
						"specialties": map[string]any{
							"type":     "array",
							"minItems": 1,
							"maxItems": 3,
							"items":    map[string]any{"type": "string", "minLength": 1, "maxLength": 24},
						},
						"personality": map[string]any{
							"type":                 "object",
							"properties":           personalityProps,
							"required":             []string{"courage", "loyalty", "aggression", "prudence", "sociability", "integrity", "stability", "ambition"},
							"additionalProperties": false,
						},
					},
					"required":             []string{"id", "name", "gender", "portrait_url", "age", "biography", "recruitment_pitch", "stats", "skills", "specialties", "personality"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"candidates"},
		"additionalProperties": false,
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return []byte(`{"type":"object","properties":{"candidates":{"type":"array"}},"required":["candidates"],"additionalProperties":false}`)
	}
	return encoded
}

func normalizeCandidatePortraitURL(raw string, gender string, index int, seed int64) string {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "/characters/generated_") && strings.HasSuffix(trimmed, ".svg") {
		var rawNumber int
		if _, err := fmt.Sscanf(trimmed, "/characters/generated_%03d_", &rawNumber); err == nil && rawNumber >= 1 && rawNumber <= 100 {
			return candidatePortraitURLFromNumber(rawNumber)
		}
	}
	base := 1
	span := 50
	if gender == "female" {
		base = 51
	} else if gender == "nonbinary" && (index+int(seed))%2 == 1 {
		base = 51
	}
	number := base + int((seed+int64(index)*7)%int64(span))
	if number < base {
		number = base
	}
	return candidatePortraitURLFromNumber(number)
}

func candidatePortraitURLFromNumber(number int) string {
	if number < 1 {
		number = 1
	}
	if number > 100 {
		number = 100
	}
	base := 1
	if number >= 51 {
		base = 51
	}
	role := candidatePortraitRoles[(number-base)%len(candidatePortraitRoles)]
	label := "male"
	if number >= 51 {
		label = "female"
	}
	return fmt.Sprintf("/characters/generated_%03d_%s_%s.svg", number, label, role)
}

func normalizeCandidatePersonality(personality unit.Personality, seed int64) unit.Personality {
	fallback := unit.GeneratePersonality(seed)
	if personality.Courage == 0 && personality.Loyalty == 0 && personality.Aggression == 0 && personality.Prudence == 0 && personality.Sociability == 0 && personality.Integrity == 0 && personality.Stability == 0 && personality.Ambition == 0 {
		return fallback
	}
	personality.Courage = clamp01WithFallback(personality.Courage, fallback.Courage)
	personality.Loyalty = clamp01WithFallback(personality.Loyalty, fallback.Loyalty)
	personality.Aggression = clamp01WithFallback(personality.Aggression, fallback.Aggression)
	personality.Prudence = clamp01WithFallback(personality.Prudence, fallback.Prudence)
	personality.Sociability = clamp01WithFallback(personality.Sociability, fallback.Sociability)
	personality.Integrity = clamp01WithFallback(personality.Integrity, fallback.Integrity)
	personality.Stability = clamp01WithFallback(personality.Stability, fallback.Stability)
	personality.Ambition = clamp01WithFallback(personality.Ambition, fallback.Ambition)
	return personality
}

func normalizeCandidateStats(stats unit.Stats, personality unit.Personality, biography string, specialties []string, seed int64) unit.Stats {
	fallback := deriveFallbackCandidateStats(biography, specialties, personality, seed)
	stats.Primary.Strength = clampIntWithFallback(stats.Primary.Strength, 6, 15, fallback.Primary.Strength)
	stats.Primary.Dexterity = clampIntWithFallback(stats.Primary.Dexterity, 6, 15, fallback.Primary.Dexterity)
	stats.Primary.Constitution = clampIntWithFallback(stats.Primary.Constitution, 6, 15, fallback.Primary.Constitution)
	stats.Primary.Wisdom = clampIntWithFallback(stats.Primary.Wisdom, 6, 15, fallback.Primary.Wisdom)
	stats.Primary.Perception = clampIntWithFallback(stats.Primary.Perception, 6, 15, fallback.Primary.Perception)
	stats.Primary.Charisma = clampIntWithFallback(stats.Primary.Charisma, 6, 15, fallback.Primary.Charisma)
	stats.Derived.Attack = clampIntWithFallback(stats.Derived.Attack, 6, 18, fallback.Derived.Attack)
	stats.Derived.Defense = clampIntWithFallback(stats.Derived.Defense, 2, 12, fallback.Derived.Defense)
	stats.Derived.Accuracy = clampIntWithFallback(stats.Derived.Accuracy, 5, 18, fallback.Derived.Accuracy)
	stats.Derived.Evasion = clampIntWithFallback(stats.Derived.Evasion, 2, 12, fallback.Derived.Evasion)
	stats.Derived.Vision = clampIntWithFallback(stats.Derived.Vision, 3, 7, fallback.Derived.Vision)
	stats.Derived.CarryWeight = clampIntWithFallback(stats.Derived.CarryWeight, 4, 10, fallback.Derived.CarryWeight)
	stats.Growth.Level = 1
	stats.Growth.Experience = 0
	stats.Growth.SkillPoints = 0
	return stats
}

func normalizeCandidateSkills(skills unit.SkillSet, biography string, specialties []string, seed int64) unit.SkillSet {
	fallback := deriveFallbackCandidateSkills(biography, specialties, seed)
	skills.Weapons.Sword = clampIntWithFallback(skills.Weapons.Sword, 0, 5, fallback.Weapons.Sword)
	skills.Weapons.Bow = clampIntWithFallback(skills.Weapons.Bow, 0, 5, fallback.Weapons.Bow)
	skills.Weapons.Blunt = clampIntWithFallback(skills.Weapons.Blunt, 0, 5, fallback.Weapons.Blunt)
	skills.Weapons.Shield = clampIntWithFallback(skills.Weapons.Shield, 0, 5, fallback.Weapons.Shield)
	skills.Weapons.Medical = clampIntWithFallback(skills.Weapons.Medical, 0, 5, fallback.Weapons.Medical)
	skills.Survival.Scouting = clampIntWithFallback(skills.Survival.Scouting, 0, 5, fallback.Survival.Scouting)
	skills.Survival.Stealth = clampIntWithFallback(skills.Survival.Stealth, 0, 5, fallback.Survival.Stealth)
	skills.Survival.Medicine = clampIntWithFallback(skills.Survival.Medicine, 0, 5, fallback.Survival.Medicine)
	skills.Survival.Gathering = clampIntWithFallback(skills.Survival.Gathering, 0, 5, fallback.Survival.Gathering)
	skills.Social.Negotiation = clampIntWithFallback(skills.Social.Negotiation, 0, 5, fallback.Social.Negotiation)
	skills.Social.Intimidation = clampIntWithFallback(skills.Social.Intimidation, 0, 5, fallback.Social.Intimidation)
	skills.Social.Charm = clampIntWithFallback(skills.Social.Charm, 0, 5, fallback.Social.Charm)
	skills.Social.Trade = clampIntWithFallback(skills.Social.Trade, 0, 5, fallback.Social.Trade)
	if len(specialties) > 0 {
		skills.Specialties = append([]string{}, specialties...)
	} else if len(skills.Specialties) == 0 {
		skills.Specialties = append([]string{}, fallback.Specialties...)
	}
	return skills
}

func deriveFallbackCandidateStats(biography string, specialties []string, personality unit.Personality, seed int64) unit.Stats {
	rng := rand.New(rand.NewSource(seed))
	primary := unit.PrimaryStats{
		Strength:     9 + rng.Intn(3),
		Dexterity:    9 + rng.Intn(3),
		Constitution: 9 + rng.Intn(3),
		Wisdom:       9 + rng.Intn(3),
		Perception:   9 + rng.Intn(3),
		Charisma:     9 + rng.Intn(3),
	}
	text := strings.ToLower(biography + " " + strings.Join(specialties, " "))
	if containsAny(text, "hunter", "scout", "ranger", "斥候", "猎", "探路", "向导", "watcher") {
		primary.Perception += 3
		primary.Dexterity += 1
	}
	if containsAny(text, "miner", "blacksmith", "builder", "铁匠", "矿", "锤", "搬山") {
		primary.Strength += 3
		primary.Constitution += 1
	}
	if containsAny(text, "herbalist", "healer", "医", "药", "救人") {
		primary.Wisdom += 3
		primary.Perception += 1
	}
	if containsAny(text, "merchant", "bard", "diplomat", "商", "账房", "说客", "谈判") {
		primary.Charisma += 3
		primary.Wisdom += 1
	}
	if containsAny(text, "shield", "guardian", "sentinel", "守", "盾", "稳住") {
		primary.Constitution += 2
		primary.Strength += 1
	}
	if personality.Courage > 0.7 {
		primary.Strength++
	}
	if personality.Prudence > 0.7 {
		primary.Perception++
	}
	if personality.Sociability > 0.7 {
		primary.Charisma++
	}
	primary.Strength = clampOpeningInt(primary.Strength, 6, 15)
	primary.Dexterity = clampOpeningInt(primary.Dexterity, 6, 15)
	primary.Constitution = clampOpeningInt(primary.Constitution, 6, 15)
	primary.Wisdom = clampOpeningInt(primary.Wisdom, 6, 15)
	primary.Perception = clampOpeningInt(primary.Perception, 6, 15)
	primary.Charisma = clampOpeningInt(primary.Charisma, 6, 15)
	derived := unit.DerivedStats{
		Attack:      clampOpeningInt(6+primary.Strength/2+int(personality.Aggression*3), 6, 18),
		Defense:     clampOpeningInt(2+primary.Constitution/3+int(personality.Prudence*3), 2, 12),
		Accuracy:    clampOpeningInt(5+primary.Dexterity/3+primary.Perception/3, 5, 18),
		Evasion:     clampOpeningInt(2+primary.Dexterity/3+int(personality.Stability*2), 2, 12),
		Vision:      clampOpeningInt(3+primary.Perception/4, 3, 7),
		CarryWeight: clampOpeningInt(4+primary.Strength/4+primary.Constitution/5, 4, 10),
	}
	return unit.Stats{Primary: primary, Derived: derived, Growth: unit.GrowthStats{Level: 1}}
}

func deriveFallbackCandidateSkills(biography string, specialties []string, seed int64) unit.SkillSet {
	rng := rand.New(rand.NewSource(seed))
	skills := unit.SkillSet{
		Weapons:  unit.WeaponSkills{Sword: 1 + rng.Intn(2), Bow: 1 + rng.Intn(2), Blunt: 1 + rng.Intn(2), Shield: 1 + rng.Intn(2), Medical: 1},
		Survival: unit.SurvivalSkills{Scouting: 1 + rng.Intn(2), Stealth: 1 + rng.Intn(2), Medicine: 1, Gathering: 1 + rng.Intn(2)},
		Social:   unit.SocialSkills{Negotiation: 1 + rng.Intn(2), Intimidation: 1, Charm: 1 + rng.Intn(2), Trade: 1},
	}
	text := strings.ToLower(biography + " " + strings.Join(specialties, " "))
	if containsAny(text, "archer", "hunter", "ranger", "弓", "猎") {
		skills.Weapons.Bow += 3
		skills.Survival.Scouting += 2
	}
	if containsAny(text, "blacksmith", "miner", "锤", "矿", "铁匠") {
		skills.Weapons.Blunt += 3
		skills.Survival.Gathering += 1
	}
	if containsAny(text, "shield", "guardian", "sentinel", "盾", "守") {
		skills.Weapons.Shield += 3
	}
	if containsAny(text, "healer", "herbalist", "医", "药", "救人") {
		skills.Weapons.Medical += 2
		skills.Survival.Medicine += 3
	}
	if containsAny(text, "merchant", "diplomat", "商", "账房", "交易") {
		skills.Social.Trade += 3
		skills.Social.Negotiation += 2
	}
	if containsAny(text, "scout", "spy", "斥候", "探路", "向导") {
		skills.Survival.Scouting += 3
		skills.Survival.Stealth += 2
	}
	skills.Weapons.Sword = clampOpeningInt(skills.Weapons.Sword, 0, 5)
	skills.Weapons.Bow = clampOpeningInt(skills.Weapons.Bow, 0, 5)
	skills.Weapons.Blunt = clampOpeningInt(skills.Weapons.Blunt, 0, 5)
	skills.Weapons.Shield = clampOpeningInt(skills.Weapons.Shield, 0, 5)
	skills.Weapons.Medical = clampOpeningInt(skills.Weapons.Medical, 0, 5)
	skills.Survival.Scouting = clampOpeningInt(skills.Survival.Scouting, 0, 5)
	skills.Survival.Stealth = clampOpeningInt(skills.Survival.Stealth, 0, 5)
	skills.Survival.Medicine = clampOpeningInt(skills.Survival.Medicine, 0, 5)
	skills.Survival.Gathering = clampOpeningInt(skills.Survival.Gathering, 0, 5)
	skills.Social.Negotiation = clampOpeningInt(skills.Social.Negotiation, 0, 5)
	skills.Social.Intimidation = clampOpeningInt(skills.Social.Intimidation, 0, 5)
	skills.Social.Charm = clampOpeningInt(skills.Social.Charm, 0, 5)
	skills.Social.Trade = clampOpeningInt(skills.Social.Trade, 0, 5)
	skills.Specialties = append([]string{}, specialties...)
	return skills
}

func clampIntWithFallback(value int, min int, max int, fallback int) int {
	if value == 0 {
		return clampOpeningInt(fallback, min, max)
	}
	return clampOpeningInt(value, min, max)
}

func clampOpeningInt(value int, min int, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func clamp01WithFallback(value float64, fallback float64) float64 {
	if value <= 0 {
		return fallback
	}
	if value > 1 {
		return 1
	}
	return value
}

func normalizeGender(gender string, index int) string {
	switch strings.ToLower(strings.TrimSpace(gender)) {
	case "male", "男", "m":
		return "male"
	case "female", "女", "f":
		return "female"
	case "nonbinary", "unknown":
		return "nonbinary"
	default:
		if index%2 == 0 {
			return "female"
		}
		return "male"
	}
}

func selectedOpeningRoster(candidates []UnitCandidate, seed int64) []UnitCandidate {
	return selectedOpeningRosterWithLimit(candidates, seed, openingRosterSize)
}

func selectedOpeningRosterWithLimit(candidates []UnitCandidate, seed int64, limit int) []UnitCandidate {
	limit = NormalizeOpeningUnitCount(limit)
	normalized := normalizeOpeningCandidates(candidates, seed)
	if len(candidates) == 0 {
		if len(normalized) > limit {
			return normalized[:limit]
		}
		return normalized
	}
	if len(normalized) > limit {
		normalized = normalized[:limit]
	}
	return normalized
}

func applyCandidateToRecord(record *unit.Record, candidate UnitCandidate) {
	if record == nil {
		return
	}
	record.Identity.Name = strings.TrimSpace(firstNonEmptyText(candidate.Name, record.Identity.Name))
	record.Identity.Gender = normalizeGender(candidate.Gender, 0)
	record.Identity.PortraitURL = normalizeCandidatePortraitURL(candidate.PortraitURL, record.Identity.Gender, 0, int64(len(record.ID)))
	record.Identity.Age = candidate.Age
	if record.Identity.Age <= 0 {
		record.Identity.Age = 24
	}
	record.Identity.Biography = strings.TrimSpace(candidate.Biography)
	record.Identity.RecruitmentPitch = strings.TrimSpace(candidate.RecruitmentPitch)
	record.Personality = normalizeCandidatePersonality(candidate.Personality, int64(len(record.ID)))
	record.Stats = normalizeCandidateStats(candidate.Stats, record.Personality, record.Identity.Biography, candidate.Specialties, int64(len(record.ID)))
	record.Skills = normalizeCandidateSkills(candidate.Skills, record.Identity.Biography, candidate.Specialties, int64(len(record.ID)))
	record.Status.Attack = record.Stats.Derived.Attack
	record.Status.Defense = record.Stats.Derived.Defense
	record.Status.Move = 4
	unit.RecalculateDerivedStats(record)
	if len(candidate.Specialties) > 0 {
		record.Skills.Specialties = append([]string{}, candidate.Specialties...)
	}
	if record.Social.ParentUnitIDs == nil {
		record.Social.ParentUnitIDs = []string{}
	}
	if record.Social.ChildUnitIDs == nil {
		record.Social.ChildUnitIDs = []string{}
	}
}

func candidateFromRecord(record unit.Record) UnitCandidate {
	return UnitCandidate{
		ID:               uuid.NewString(),
		Name:             record.Identity.Name,
		Gender:           record.Identity.Gender,
		PortraitURL:      record.Identity.PortraitURL,
		Age:              record.Identity.Age,
		Biography:        record.Identity.Biography,
		Stats:            record.Stats,
		Skills:           record.Skills,
		Personality:      record.Personality,
		RecruitmentPitch: record.Identity.RecruitmentPitch,
		Specialties:      append([]string{}, record.Skills.Specialties...),
	}
}

func draftRecordsFromCandidates(seed int64, sessionID string, factionID string, candidates []UnitCandidate) []unit.Record {
	coords := draftSpawnCoords(factionID)
	records := make([]unit.Record, 0, len(candidates))
	for index, candidate := range candidates {
		coord := coords[index%len(coords)]
		record, err := bootstrapBattleUnit(seed+int64(index)*31, sessionID, factionID, candidate.Name, coord)
		if err != nil {
			continue
		}
		applyCandidateToRecord(&record, candidate)
		record.Status.PositionQ = coord.Q
		record.Status.PositionR = coord.R
		record.Social.Wildling = factionID == FactionWildling
		record.Social.BornTurn = 1
		records = append(records, record)
	}
	return records
}

func repositionRecordsForMap(records []unit.Record, factionID string, snapshot world.MapSnapshot) {
	coords := spawnCoordsForMap(factionID, snapshot.Width, snapshot.Height, len(records))
	if len(coords) == 0 {
		return
	}
	for index := range records {
		coord := coords[index%len(coords)]
		records[index].Status.PositionQ = coord.Q
		records[index].Status.PositionR = coord.R
	}
}

func spawnCoordsForMap(factionID string, width int, height int, count int) []world.Coord {
	if width <= 0 {
		width = defaultBattlefieldWidth
	}
	if height <= 0 {
		height = defaultBattlefieldHeight
	}
	if count <= 0 {
		count = 10
	}
	leftColumns := []int{1, 2}
	rightColumns := []int{sessionMaxInt(0, width-2), sessionMaxInt(0, width-3)}
	columns := leftColumns
	if factionID == "enemy" {
		columns = rightColumns
	}
	rows := orderedSpawnRows(height)
	coords := make([]world.Coord, 0, count)
	for _, column := range columns {
		for _, row := range rows {
			if column < 0 || column >= width || row < 0 || row >= height {
				continue
			}
			coords = append(coords, world.Coord{Q: column, R: row})
			if len(coords) >= count {
				return coords
			}
		}
	}
	return coords
}

func orderedSpawnRows(height int) []int {
	rows := make([]int, 0, sessionMaxInt(1, height-2))
	center := height / 2
	for offset := 0; len(rows) < sessionMaxInt(1, height-2); offset++ {
		candidates := []int{center - offset, center + offset}
		for _, row := range candidates {
			if row <= 0 || row >= height-1 {
				continue
			}
			already := false
			for _, existing := range rows {
				if existing == row {
					already = true
					break
				}
			}
			if !already {
				rows = append(rows, row)
			}
		}
	}
	return rows
}

func sessionMaxInt(a int, b int) int {
	if a > b {
		return a
	}
	return b
}

func draftSpawnCoords(factionID string) []world.Coord {
	switch factionID {
	case "enemy":
		return []world.Coord{{Q: 5, R: 1}, {Q: 5, R: 2}, {Q: 5, R: 3}, {Q: 5, R: 4}, {Q: 5, R: 5}, {Q: 4, R: 1}, {Q: 4, R: 2}, {Q: 4, R: 3}, {Q: 4, R: 4}, {Q: 4, R: 5}}
	case FactionWildling:
		return []world.Coord{{Q: 3, R: 0}, {Q: 3, R: 6}, {Q: 0, R: 3}, {Q: 6, R: 3}}
	default:
		return []world.Coord{{Q: 1, R: 1}, {Q: 1, R: 2}, {Q: 1, R: 3}, {Q: 1, R: 4}, {Q: 1, R: 5}, {Q: 2, R: 1}, {Q: 2, R: 2}, {Q: 2, R: 3}, {Q: 2, R: 4}, {Q: 2, R: 5}}
	}
}
