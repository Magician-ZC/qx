package session

// 文件说明：世界Boss——全世界共享血池的异步协作 PvE（设计文档 docs/PvE威胁系统.md 世界Boss）。
// 核心机制：不同玩家的角色在不同时间各自对同一头 Boss 出手，每次出手把伤害记进世界总线(WORLD_BOSS_STRIKE)；
// 世界总线就是不可篡改的贡献账本。血池清零时——由「砍出最后一击、抢到结算闩锁」的那个请求——读账本全员分赃
// （排他件走 arbitration 胜率∝贡献/频率无关/付费不进，可分件按贡献瓜分），并广播 WORLD_BOSS_DEFEATED。
//
// 三条硬保证：
//  1. 血池原子递减（UPDATE ... RETURNING，WHERE status='active'），并发出手不会把血扣成负、不会复活已死 Boss。
//  2. 扣血与「记进贡献账本」在**同一事务**内提交——任一步失败则回滚，绝不留下「血扣了但账本没记/Boss 卡死在 0 血」的不一致。
//  3. 单次结算闩锁（UPDATE status='defeated' WHERE status='active' 的 RowsAffected==1 才结算），并发最后一击只有一人结算。
//
// 贡献口径（反 P2W / 频率无关）：结算分赃用每个出手者的**单次最高伤害**（≈其角色真实战力），不是出手次数之和——
// 这样反复刷同一头 Boss 不会刷高分赃份额，频率不进 Score（设计宪法反 P2W）。血池消耗仍按每击累计（协作击杀）。
//
// 已知残留（需后续系统支撑，非本切片可独立修复，见 docs/开发进度.md）：
//   - 结算（闩锁之后）的发钱+广播未与闩锁同事务（status.Mutator 尚不支持外部事务）：极小概率 DB 故障可致已死 Boss 仅部分分赃。
//   - strike 端点无鉴权/幂等：贡献伪造与频率刷需上层行动力/鉴权层（finding antip2w/robustness）。
//   - 跨分片参战者的金币只记 awards、不在本库发放（跨分片结算是后续课题）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
	"qunxiang/backend/internal/worldbus"
)

// ErrWorldBossInactive 表示 Boss 不存在或已被讨平/失效（不能再出手）。
var ErrWorldBossInactive = errors.New("world boss not active")

// WorldBossStrikeResult 是一次对世界Boss出手的结果。
type WorldBossStrikeResult struct {
	BossID        string
	AttackerID    string
	Damage        int
	HPRemaining   int
	Defeated      bool              // 这一击是否打死了 Boss
	SettledByMe   bool              // 这一击是否由本请求执行了结算（抢到闩锁）
	Participants  int               // 结算时的参战人数（仅 Defeated&&SettledByMe 时有意义）
	Awards        []encounter.Award // 全员分赃结果（仅结算者填充）
	BroadcastCard string            // 讨平广播的祖魂语气卡（仅结算者填充）
}

// 世界Boss默认掉落规模系数（货币按血量给、外加一件唯一遗物）。
const worldBossEpicRelicID = "world_boss_relic"

// maxWorldBossHP 给血量设上限：gold 掉落按 hpMax 给，过大会被钱包上限静默截断而丢钱。
const maxWorldBossHP = 999_999

// SpawnWorldBoss 在某世界投放一头世界Boss。world 必须已注册、hp 须为正且不超上限。返回 boss ID。
func (service *Service) SpawnWorldBoss(ctx context.Context, worldID string, name string, hp int, regionID string) (string, error) {
	if service == nil || service.db == nil {
		return "", fmt.Errorf("spawn world boss: missing db")
	}
	if worldID == "" || name == "" {
		return "", fmt.Errorf("spawn world boss: empty world_id or name")
	}
	if hp <= 0 || hp > maxWorldBossHP {
		return "", fmt.Errorf("spawn world boss: hp must be in (0, %d]", maxWorldBossHP)
	}
	// 世界必须已存在：否则首次出手时 AdvanceTick 才报错，那时血已被改、状态已脏（finding robustness）。
	if _, err := world.Get(ctx, service.db, worldID); err != nil {
		return "", fmt.Errorf("spawn world boss: %w", err)
	}
	id := "wboss_" + uuid.NewString()
	if _, err := service.db.ExecContext(ctx, `
		INSERT INTO world_bosses (id, world_id, name, hp_max, hp_remaining, status, region_id)
		VALUES (?, ?, ?, ?, ?, 'active', ?)`,
		id, worldID, name, hp, hp, regionID); err != nil {
		return "", fmt.Errorf("insert world boss: %w", err)
	}
	return id, nil
}

// StrikeWorldBoss 对一头世界Boss出手一次：原子扣血 + 把伤害记进世界总线（贡献账本）；
// 若这一击清零血池，则由抢到结算闩锁的请求读账本全员分赃并广播。
func (service *Service) StrikeWorldBoss(ctx context.Context, worldID string, bossID string, attacker *unit.Record) (WorldBossStrikeResult, error) {
	if service == nil || service.db == nil || attacker == nil {
		return WorldBossStrikeResult{}, fmt.Errorf("strike world boss: missing dependencies")
	}
	result := WorldBossStrikeResult{BossID: bossID, AttackerID: attacker.ID}

	damage := attacker.Status.Attack
	if damage < 1 {
		damage = 1 // 伤害来自角色已练就的数值，非付费——付费不进贡献
	}
	result.Damage = damage

	// 扣血 + 记账本在同一事务内：任一步失败则回滚（血不会被改、账本不会半写），杜绝「血扣了但没记账/Boss 卡死在 0 血」。
	hpRemaining, bossName, hpMax, err := service.strikeTx(ctx, worldID, bossID, attacker.ID, damage)
	if err != nil {
		return result, err
	}
	result.HPRemaining = hpRemaining

	if hpRemaining > 0 {
		return result, nil
	}

	// 3) 这一击清零了血池 → 抢结算闩锁：只有把 active 翻成 defeated 成功的那个请求才结算（防并发双结算）。
	result.Defeated = true
	res, err := service.db.ExecContext(ctx, `
		UPDATE world_bosses SET status = 'defeated' WHERE id = ? AND status = 'active'`, bossID)
	if err != nil {
		return result, fmt.Errorf("latch world boss defeat: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected != 1 {
		// 别的并发请求已抢到结算——本请求只报告「打死了」，不重复分赃。
		return result, nil
	}
	result.SettledByMe = true

	if err := service.settleWorldBoss(ctx, worldID, bossID, bossName, hpMax, &result); err != nil {
		return result, err
	}
	return result, nil
}

// strikeTx 在单个事务内原子完成「扣血 + 记进贡献账本（世界时钟发号 + 总线追加）」。
// 返回扣后剩余血、Boss 名、血上限。Boss 非 active 时返回 ErrWorldBossInactive（事务回滚，无副作用）。
func (service *Service) strikeTx(ctx context.Context, worldID, bossID, attackerID string, damage int) (int, string, int, error) {
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", 0, fmt.Errorf("begin world boss strike tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // Commit 后为 no-op
	dialect := dbdialect.For(service.db)

	// 1) 原子扣血：仅对 active 的 Boss 生效，血不扣成负；返回扣后剩余血。
	var hpRemaining, hpMax int
	var bossName string
	if dialect == dbdialect.DialectMySQL {
		// MySQL 无 RETURNING：事务内 SELECT...FOR UPDATE 锁行 + 检查 active + UPDATE（避免 RowsAffected 歧义）。
		var status string
		if err := tx.QueryRowContext(ctx, `
			SELECT hp_remaining, name, hp_max, status FROM world_bosses
			WHERE id = ? AND world_id = ? FOR UPDATE`, bossID, worldID).Scan(&hpRemaining, &bossName, &hpMax, &status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, "", 0, ErrWorldBossInactive
			}
			return 0, "", 0, fmt.Errorf("lock world boss: %w", err)
		}
		if status != "active" {
			return 0, "", 0, ErrWorldBossInactive
		}
		hpRemaining -= damage
		if hpRemaining < 0 {
			hpRemaining = 0
		}
		if _, err := tx.ExecContext(ctx, `UPDATE world_bosses SET hp_remaining = ? WHERE id = ?`, hpRemaining, bossID); err != nil {
			return 0, "", 0, fmt.Errorf("decrement world boss hp: %w", err)
		}
	} else {
		row := tx.QueryRowContext(ctx, `
			UPDATE world_bosses SET hp_remaining = MAX(0, hp_remaining - ?)
			WHERE id = ? AND world_id = ? AND status = 'active'
			RETURNING hp_remaining, name, hp_max`, damage, bossID, worldID)
		if err := row.Scan(&hpRemaining, &bossName, &hpMax); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, "", 0, ErrWorldBossInactive
			}
			return 0, "", 0, fmt.Errorf("decrement world boss hp: %w", err)
		}
	}

	// 2) 同事务内把这一击记进世界总线（世界时钟发号 + append-only 账本）。
	tick, err := world.AdvanceTick(ctx, tx, worldID, dialect)
	if err != nil {
		return 0, "", 0, fmt.Errorf("advance world tick: %w", err)
	}
	if _, err := worldbus.Append(ctx, tx, worldbus.CrossEvent{
		WorldID: worldID, ActorID: attackerID, TargetID: bossID,
		Kind: worldbus.KindWorldBossStrike, Importance: 4, WorldTick: tick,
		Payload: map[string]any{"damage": damage, "boss_id": bossID},
	}); err != nil {
		return 0, "", 0, fmt.Errorf("record world boss strike: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, "", 0, fmt.Errorf("commit world boss strike: %w", err)
	}
	return hpRemaining, bossName, hpMax, nil
}

// settleWorldBoss 读世界总线的贡献账本，按贡献全员分赃，并广播讨平事件。
func (service *Service) settleWorldBoss(ctx context.Context, worldID string, bossID string, bossName string, hpMax int, result *WorldBossStrikeResult) error {
	// 读**完整**贡献账本（不设 LIMIT，否则成熟世界里早期出手会被截断而少分赃）。
	busEvents, err := worldbus.ListByWorldKind(ctx, service.db, worldID, worldbus.KindWorldBossStrike)
	if err != nil {
		return fmt.Errorf("read contribution ledger: %w", err)
	}
	// 贡献 = 每个出手者的**单次最高伤害**（≈角色真实战力），频率无关：反复刷不会刷高份额（反 P2W）。
	bestByActor := map[string]int{}
	order := make([]string, 0)
	for _, ev := range busEvents {
		var p struct {
			Damage int    `json:"damage"`
			BossID string `json:"boss_id"`
		}
		if raw, ok := ev.Payload.(json.RawMessage); ok {
			_ = json.Unmarshal(raw, &p)
		}
		if p.BossID != bossID || ev.ActorID == "" {
			continue
		}
		if _, seen := bestByActor[ev.ActorID]; !seen {
			order = append(order, ev.ActorID)
		}
		if p.Damage > bestByActor[ev.ActorID] {
			bestByActor[ev.ActorID] = p.Damage
		}
	}

	participants := make([]encounter.Participant, 0, len(order))
	for _, actorID := range order {
		participants = append(participants, encounter.Participant{UnitID: actorID, Score: float64(bestByActor[actorID])})
	}
	result.Participants = len(participants)

	// 掉落：货币按血量规模给（可分割，按贡献瓜分）+ 一件唯一遗物（排他，走 arbitration 胜率∝贡献）。
	loot := []encounter.LootItem{
		{ID: "gold", Rarity: encounter.Common, Quantity: hpMax},
		{ID: worldBossEpicRelicID, Rarity: encounter.Epic, Quantity: 1},
	}
	key := fmt.Sprintf("%s|worldboss|%s", worldID, bossID)
	awards := encounter.AllocateLoot(key, loot, participants, fieldBossMinMeaningful)
	result.Awards = awards

	// 货币落本库真实存在的参战角色钱包（跨分片角色只记账、不在本库发放）。
	goldByUnit := map[string]int{}
	for _, a := range awards {
		if a.ItemID == "gold" {
			goldByUnit[a.UnitID] += a.Quantity
		}
	}
	for unitID, gold := range goldByUnit {
		if gold <= 0 {
			continue
		}
		actor, err := service.units.GetByID(ctx, unitID)
		if err != nil {
			continue // 跨分片/非本库角色：只在账本与 awards 里留痕，不在本库发钱
		}
		if _, err := service.mutator.Apply(ctx, status.Mutation{
			UnitID:     unitID,
			Field:      status.FieldWallet,
			Delta:      float64(gold),
			ReasonCode: events.ReasonEconomyLoot,
			ReasonText: fmt.Sprintf("讨平%s的全服分成", bossName),
			Actors:     []string{unitID},
		}); err != nil {
			return fmt.Errorf("grant world boss gold: %w", err)
		}
		_ = actor
	}

	// 广播讨平事件进世界总线（全服可见的不可篡改事实）。
	result.BroadcastCard = fmt.Sprintf("举世皆惊——那头%s，终于被众人合力讨平了。", bossName)
	if _, err := service.RecordCrossInteraction(ctx, worldID, "", bossID,
		worldbus.KindWorldBossDown, 9,
		map[string]any{"boss_id": bossID, "participants": result.Participants}); err != nil {
		return fmt.Errorf("broadcast world boss defeat: %w", err)
	}
	return nil
}
