package session

// 文件说明：共享世界 Phase4「共享进度」——把区域 boss 从「per-session 单人 elite」升级为「world 级共享实例」。
// 设计 docs/共享世界改造方案-方向B-2026-06-10.md §4 Phase4 + docs/PvE威胁系统.md（贡献分赃/反 P2W/共享血池）。
//
// 核心：同一共享世界（world_shared_v1）里某玩家打掉/打伤一个 zone 的 boss，对**全区玩家生效**——
//   - 共享血池：zone boss 落 world_bosses 表（region_id=复合 worldID#zoneID），多玩家谁打都扣**同一池血**（StrikeWorldBossParty
//     范式的 strikeSharedBossCore：原子扣血 + 记贡献账本 worldbus + 血池清零闩锁结算）。
//   - 共享「已讨平」：从 state.DefeatedBosses（per-session 私有）改为查 world_bosses status='defeated'（world 级共享）——
//     A 打掉 → B 的挑战看到已讨平被拒。
//   - 战利品按贡献分赃：settleWorldBoss 复用，排他遗物 zone_boss_relic 走 arbitration 胜率∝贡献（频率无关/付费不进），
//     可分货币按贡献 SplitProportional，钱包经 Mutator 留痕。
//
// 与 world_boss 的差异（为何不直接当 world_boss）：zone boss 是 field_boss 档，**可单人 chip 共享血池**（不过单人物理锁——
// 那是存亡级 world_boss 专属）；多区 boss 可并存（约束键 (world_id, region_id)，见 schema uq_world_boss_active）。
//
// 私有档/flag 关零影响：仅 inSharedWorld(state)（QUNXIANG_SHARED_WORLD 开 + world_id==共享世代）走此共享路径；
// 否则 ChallengeZoneBoss 维持原单人 ResolveEliteEncounter + state.DefeatedBosses（Phase2 前行为，逐字节不变）。
//
// 跨玩家硬红线：共享 boss 扣血/分赃**只写**共享 world_bosses 行 + worldbus 账本 + 各自本侧钱包（经 Mutator）；
// 绝不直写他人 units（settleWorldBoss 只对本库 GetByID 命中的参战者本侧发钱，跨分片角色只记账）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// zoneBossEpicRelicID 是区域共享 boss 的排他遗物 ID（走 arbitration 胜率∝贡献归属；与 world_boss_relic 区分掉落表）。
const zoneBossEpicRelicID = "zone_boss_relic"

// sharedZoneBossHPScale 把区域 boss 等级换算为共享血池上限：base 200 + level·40。
// 比单人 elite 的 zoneBossThreat HP（60+level·12）显著厚——共享 boss 由多玩家协力消耗，单人一击只 chip 一点，需更大池子
// 才能体现「全区共享的世界级威胁」。仍在 maxWorldBossHP 内、可单人多次 chip 也可多人协力。确定性、不含付费。
func sharedZoneBossHP(bossLevel int) int {
	if bossLevel < 1 {
		bossLevel = 1
	}
	hp := 200 + bossLevel*40
	if hp > maxWorldBossHP {
		hp = maxWorldBossHP
	}
	return hp
}

// zoneBossLootSpec 区域共享 boss 的掉落规格：货币按血量规模缩放（共享血池厚，按 1/10 给避免金币爆量但仍随等级递增），
// 排他遗物 zone_boss_relic。与 worldBossLootSpec 同构，供 strikeSharedBossCore 复用 settle 分赃链路。
func zoneBossLootSpec(hpMax int) bossLootSpec {
	gold := hpMax / 10
	if gold < 1 {
		gold = 1
	}
	return bossLootSpec{RelicID: zoneBossEpicRelicID, GoldQty: gold, KeyTag: "zoneboss"}
}

// ensureSharedZoneBoss get-or-create 某 zone 在共享世界里的共享 boss（幂等，按 (world_id, region_id) 单 active）。
//
// region_id 用复合键 worldID#zoneID（sharedRegionID）：让同一共享世界里**每个 zone 各自一头** active 共享 boss、多区并存
// （约束键 uq_world_boss_active=(world_id, region_id)）。返回 (bossID, hpMax, alreadyDefeated, err)：
//   - 已有 status='active' 行 → 返回既有（幂等，不重复 spawn）。
//   - 已有 status='defeated' 行 → alreadyDefeated=true（world 级「已讨平」，调用方据此拒绝再战）。
//   - 都没有 → 原子条件 INSERT 一头 active（WHERE NOT EXISTS 防并发双插；唯一约束 uq_world_boss_active 硬兜底，
//     SQLite=partial unique index / MySQL=STORED 生成列 active_region_key + 唯一键，见 dbmigrate.EnsureWorldBossActiveUnique）。
//
// 并发：与 maybeRefreshWorldBoss 同模式（原子条件 INSERT + dup-key 收敛为「已有 active」再查一次）。确定性、不含付费。
func (service *Service) ensureSharedZoneBoss(ctx context.Context, worldID, regionID, bossName string, bossLevel int) (string, int, bool, error) {
	if service == nil || service.db == nil {
		return "", 0, false, fmt.Errorf("ensure shared zone boss: missing db")
	}
	if worldID == "" || regionID == "" {
		return "", 0, false, fmt.Errorf("ensure shared zone boss: empty world_id or region_id")
	}

	// 先查该 (world_id, region_id) 当前有无 active / defeated 的共享 boss。
	if id, hpMax, defeated, ok, err := service.lookupZoneBoss(ctx, worldID, regionID); err != nil {
		return "", 0, false, err
	} else if ok {
		return id, hpMax, defeated, nil
	}

	// 无任何行 → 原子条件 INSERT 一头 active（仅当该 (world_id, region_id) 当前无 active 时）。
	hp := sharedZoneBossHP(bossLevel)
	name := bossName
	if name == "" {
		name = "盘踞此地的霸主"
	}
	if _, err := world.Get(ctx, service.db, worldID); err != nil {
		return "", 0, false, fmt.Errorf("ensure shared zone boss: %w", err)
	}
	id := "zboss_" + uuid.NewString()
	res, err := service.db.ExecContext(ctx, `
		INSERT INTO world_bosses (id, world_id, name, hp_max, hp_remaining, status, region_id)
		SELECT ?, ?, ?, ?, ?, 'active', ?
		WHERE NOT EXISTS (SELECT 1 FROM world_bosses WHERE world_id = ? AND region_id = ? AND status = 'active')`,
		id, worldID, name, hp, hp, regionID, worldID, regionID)
	if err != nil {
		// 并发双插：唯一约束冲突 → 等价「已有 active」，再查一次拿既有行（与 maybeRefreshWorldBoss 同收敛）。
		if isDupKeyErr(err) {
			if eid, ehp, edef, ok, lerr := service.lookupZoneBoss(ctx, worldID, regionID); lerr == nil && ok {
				return eid, ehp, edef, nil
			}
		}
		return "", 0, false, fmt.Errorf("insert shared zone boss: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 1 {
		return id, hp, false, nil // 本请求成功插入一头 active
	}
	// RowsAffected==0（WHERE NOT EXISTS 挡下：并发请求刚插了一头 active）→ 再查一次拿既有行。
	if eid, ehp, edef, ok, err := service.lookupZoneBoss(ctx, worldID, regionID); err == nil && ok {
		return eid, ehp, edef, nil
	}
	return "", 0, false, fmt.Errorf("ensure shared zone boss: lost spawn race but no row found")
}

// lookupZoneBoss 查某 (world_id, region_id) 当前的共享 boss 行：优先取 active，其次取 defeated。
// 返回 (bossID, hpMax, defeated, found, err)。同 region 可能历史上有多条（旧 active 已 defeated + 一条新 active）——
// 取最新一条为准（按 created_at 降序）：active 优先（再战），无 active 但有 defeated 则报「已讨平」。
func (service *Service) lookupZoneBoss(ctx context.Context, worldID, regionID string) (string, int, bool, bool, error) {
	// active 优先：有 active 就返回它（可继续 chip）。
	var id string
	var hpMax int
	err := service.db.QueryRowContext(ctx, `
		SELECT id, hp_max FROM world_bosses
		WHERE world_id = ? AND region_id = ? AND status = 'active'
		ORDER BY created_at DESC LIMIT 1`, worldID, regionID).Scan(&id, &hpMax)
	if err == nil {
		return id, hpMax, false, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, false, fmt.Errorf("lookup active zone boss: %w", err)
	}
	// 无 active → 看是否已 defeated（world 级「已讨平」共享事实）。
	err = service.db.QueryRowContext(ctx, `
		SELECT id, hp_max FROM world_bosses
		WHERE world_id = ? AND region_id = ? AND status = 'defeated'
		ORDER BY created_at DESC LIMIT 1`, worldID, regionID).Scan(&id, &hpMax)
	if err == nil {
		return id, hpMax, true, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, false, nil // 该区从未投放过共享 boss
	}
	return "", 0, false, false, fmt.Errorf("lookup defeated zone boss: %w", err)
}

// sharedZoneBossDefeated 判某 zone 的共享 boss 是否已被 world 级讨平（status='defeated'，全区玩家共享此事实）。
// 这取代了 per-session 私有的 zoneBossDefeated(state, zoneID)：A 打掉 → B 这里看到已讨平。
func (service *Service) sharedZoneBossDefeated(ctx context.Context, worldID, regionID string) (bool, error) {
	_, _, defeated, found, err := service.lookupZoneBoss(ctx, worldID, regionID)
	if err != nil {
		return false, err
	}
	return found && defeated, nil
}

// sharedDefeatedRegionIDs 一次性取某共享世界里所有「已 world 级讨平」的复合 region_id 集合（评审 #4）。
// 供 ZonesOverview 在共享局里给每个 zone 批量判「已讨平」用——一次 SELECT 取全 world 的 defeated region，
// 避免逐 zone 各发一次 lookupZoneBoss（N 次往返）。无 active 行覆盖即视作该区已讨平（active 优先语义见 lookupZoneBoss）：
// 故须排除「同区仍有 active」的 region（A 打掉旧 boss 后又刷了新 boss 的边角场景），以「该区当前无 active 但有 defeated」为准。
// 返回 region_id → true 集合（仅含确已讨平、当前无 active 的区）。纯读、确定性、不含付费。
func (service *Service) sharedDefeatedRegionIDs(ctx context.Context, worldID string) (map[string]bool, error) {
	if service == nil || service.db == nil || strings.TrimSpace(worldID) == "" {
		return map[string]bool{}, nil
	}
	// 取该 world 当前仍有 active boss 的 region（这些区不算「已讨平」，哪怕历史上有 defeated 行）。
	activeRegions := map[string]bool{}
	rows, err := service.db.QueryContext(ctx,
		`SELECT DISTINCT region_id FROM world_bosses WHERE world_id = ? AND status = 'active'`, worldID)
	if err != nil {
		return nil, fmt.Errorf("list active shared boss regions: %w", err)
	}
	for rows.Next() {
		var region string
		if err := rows.Scan(&region); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan active shared boss region: %w", err)
		}
		activeRegions[region] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate active shared boss regions: %w", err)
	}
	rows.Close()

	// 取该 world 有 defeated boss 的 region，剔除仍有 active 的区 → 余下即「当前已讨平、无 active」的区。
	defeated := map[string]bool{}
	drows, err := service.db.QueryContext(ctx,
		`SELECT DISTINCT region_id FROM world_bosses WHERE world_id = ? AND status = 'defeated'`, worldID)
	if err != nil {
		return nil, fmt.Errorf("list defeated shared boss regions: %w", err)
	}
	defer drows.Close()
	for drows.Next() {
		var region string
		if err := drows.Scan(&region); err != nil {
			return nil, fmt.Errorf("scan defeated shared boss region: %w", err)
		}
		if !activeRegions[region] {
			defeated[region] = true
		}
	}
	if err := drows.Err(); err != nil {
		return nil, fmt.Errorf("iterate defeated shared boss regions: %w", err)
	}
	return defeated, nil
}

// challengeSharedZoneBoss 是 ChallengeZoneBoss 的共享路径（inSharedWorld 时走）：玩家一次攻击 = 对该 zone 的共享 boss
// 原子扣血 + 记贡献账本（worldbus），血池清零则闩锁结算按贡献分赃。多玩家共享同一 boss HP，谁打都扣同一池。
//
// 与 world_boss 复用 strikeSharedBossCore（原子扣血/闩锁/分赃），但**不过单人物理锁**（zone boss 是 field_boss 档、可单人 chip）。
// 掉落规格用 zoneBossLootSpec（zone_boss_relic + 缩放货币）。
//
// 返回 EliteEncounterResult（与单人路径同形，HTTP 路由 {"encounter": result} 不变）：把一次共享出手翻译成那一形，
// Outcome 取 defeated（这一击打死了 boss）/ ongoing（打伤了、boss 仍活）。InboxCard 给祖魂语气进度卡。
//
// 入参 rec 是已经过 guardPlayerAction 五道门 + 站位/等级护栏校验的主角；state 是其会话态（inSharedWorld 已确认）。
func (service *Service) challengeSharedZoneBoss(ctx context.Context, state *State, rec *unit.Record, zone world.Zone) (EliteEncounterResult, error) {
	result := EliteEncounterResult{Outcome: "ongoing"}
	if service == nil || service.db == nil || state == nil || rec == nil {
		return result, fmt.Errorf("challenge shared zone boss: missing dependencies")
	}
	worldID := strings.TrimSpace(state.WorldID)
	regionID := sharedRegionID(worldID, zone.ID)
	if regionID == "" {
		return result, fmt.Errorf("challenge shared zone boss: empty composite region")
	}

	// get-or-create 该 zone 的共享 boss；已 world 级讨平 → 拒绝再战（共享「已讨平」语义）。
	bossID, _, defeated, err := service.ensureSharedZoneBoss(ctx, worldID, regionID, zone.BossName, zone.BossLevel)
	if err != nil {
		return result, fmt.Errorf("challenge shared zone boss (ensure): %w", err)
	}
	if defeated {
		return result, fmt.Errorf("「%s」已被讨平，这片天地暂时太平了", zone.BossName)
	}
	result.ThreatID = bossID

	// 一次攻击 = 对共享血池原子扣血 + 记贡献账本；清零则闩锁结算分赃（不过单人锁，field_boss 档可单人 chip）。
	strike, err := service.strikeSharedBossCore(ctx, worldID, bossID, rec, WorldBossStrikeResult{BossID: bossID, AttackerID: rec.ID}, zoneBossLootSpec)
	if err != nil {
		// boss 在本次攻击前的一刻被别人打死（并发）→ strikeTx 报 ErrWorldBossInactive：等价「已被讨平」。
		if errors.Is(err, ErrWorldBossInactive) {
			return result, fmt.Errorf("「%s」刚被讨平，这片天地暂时太平了", zone.BossName)
		}
		return result, fmt.Errorf("challenge shared zone boss (strike): %w", err)
	}
	result.DamageDealt = strike.Damage

	// 把共享出手翻译成 EliteEncounterResult 形 + 祖魂语气进度卡。
	if strike.Defeated {
		result.Outcome = "defeated"
		result.Awards = strike.Awards // 仅结算者（最后一击）填充；非结算者 Awards 为空但 Outcome=defeated
		if strike.SettledByMe {
			result.InboxCard = fmt.Sprintf("众人合力，终于讨平了盘踞「%s」的%s——这片天地，太平了。", zone.Name, zone.BossName)
		} else {
			result.InboxCard = fmt.Sprintf("她补上的这一击，正赶上%s倒下的那一刻——「%s」之围，解了。", zone.BossName, zone.Name)
		}
		// 入世界编年史（共享：boss_slain 进群像史，best-effort，绝不影响结算）。
		service.chronicleBossSlain(ctx, worldID, state.TurnState.Turn, rec.ID, rec.DisplayName(), zone.BossName, zone.Name)

		// 评审 #3：任务进度 hook（与私有路径 zone_boss.go:102 同口径）——讨平本区 boss → 推进本玩家匹配的
		// defeat_boss objective（target=zone.ID）。共享路径此前完全没接此 hook，致共享世界里 defeat_boss 任务变死任务、
		// 永不 completed。任务进度是 per-session 私有（正确保留为个人进度，未误 world 共享）；语义=「每人在自己落下致命
		// 一击讨平时推进自己的 defeat_boss 目标」（落最后一击者在自己 challengeSharedZoneBoss 返回 defeated 时推进）。
		// best-effort、纯改 state.ActiveQuests；共享路径此前不写 state，故须显式 saveSessionMergingExternalEvents 落库
		// （否则任务进度只在内存、随请求结束丢失）。落库失败只记日志，绝不影响共享 boss 结算（已落 world_bosses/钱包）。
		// 仅当 sessions 仓可用时才落库（生产恒有；纯结算单测的极简 service 无 sessions 仓 → 只在内存推进、不落库，不 panic）。
		advanceQuestObjectives(state, ObjectiveDefeatBoss, zone.ID, 1)
		if service.sessions != nil {
			if err := service.saveSessionMergingExternalEvents(ctx, state); err != nil {
				slog.Warn("shared zone boss: persist quest progress best-effort failed",
					"session", state.ID, "zone", zone.ID, "unit", rec.ID, "err", err)
			}
		}
	} else {
		result.InboxCard = fmt.Sprintf("她朝盘踞「%s」的%s狠狠劈了一刀（还余 %d 口气）——这头东西，得众人合力才撼得动。", zone.Name, zone.BossName, strike.HPRemaining)
	}

	// 命运收件箱进度卡（她自己的事，重要度据是否讨平）：与单人 elite 收件箱同口径（祖魂语气、不暴露原始数值）。
	importance := 5
	emotion := 0.3
	if strike.Defeated {
		importance, emotion = 7, 0.5
	}
	if _, err := service.SurfaceFateEvent(ctx, state.ID, rec, FateEvent{
		ActorID:        rec.ID,
		TargetID:       rec.ID,
		ReasonCode:     events.ReasonInboxHighlight,
		Importance:     importance,
		EmotionWeight:  emotion,
		Summary:        result.InboxCard,
		SourceRegionID: regionID,
	}); err != nil {
		return result, err
	}
	return result, nil
}
