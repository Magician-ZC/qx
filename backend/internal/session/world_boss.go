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
	"hash/fnv"
	"log/slog"
	"os"
	"strings"

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
	// SoloLocked 标记本次出手被「单人物理锁」挡下（severity>soloCap 且发起方为单人，未真打）。
	// 此时不扣血、不记账本，HPRemaining=Boss 当前血、SoloCard 给出「这不是一个人能撼动的」祖魂语气卡。
	SoloLocked bool
	SoloCard   string // 单人物理锁触发时的高光卡（祖魂语气），其余情况为空
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
//
// 这是**协作出手**入口：世界Boss 是共享血池的异步协作机制，不同玩家的角色在不同时间各自出手共同消耗血池——
// 这本身就是「组队/协力」语义，故协作出手不受单人物理锁约束（共享血池的每一击都是这场群体围猎的一份子）。
// 「单人独自撼动世界Boss」的物理锁见 StrikeWorldBossParty（显式声明参战 party，party 单人且 severity>cap → 锁）。
func (service *Service) StrikeWorldBoss(ctx context.Context, worldID string, bossID string, attacker *unit.Record) (WorldBossStrikeResult, error) {
	if attacker == nil {
		return WorldBossStrikeResult{}, fmt.Errorf("strike world boss: missing dependencies")
	}
	// 协作入口：把本击并入共享血池的群体围猎——party 取「账本既有不同出手者 ∪ 本人」，恒视作协作（不触发单人锁）。
	return service.StrikeWorldBossParty(ctx, worldID, bossID, attacker, nil)
}

// StrikeWorldBossParty 是带**显式 party 声明**的世界Boss出手入口（设计 §7 单人物理锁）。
// partyUnitIDs 是本次参战队伍的全部成员（含 attacker）；为空/仅含 attacker 即「单人独闯」。
//   - ① 单人物理锁：world_boss 的 severity 恒 >soloCap（存亡级威胁），若声明的 party 实为单人——物理上撼不动它，
//     **根本不让 strike**（不扣血、不记账本），返回「这不是一个人能撼动的」高光卡。组队（≥2 不同成员）才解锁。
//   - 解锁后落入与共享血池协作完全一致的原子扣血 + 记账本 + 闩锁结算链路。
//
// 确定性、付费不进：是否锁只看 hp_max（战力派生 severity）+ party 成员数，绝不含 wallet/billing/出手频率。
func (service *Service) StrikeWorldBossParty(ctx context.Context, worldID string, bossID string, attacker *unit.Record, partyUnitIDs []string) (WorldBossStrikeResult, error) {
	if service == nil || service.db == nil || attacker == nil {
		return WorldBossStrikeResult{}, fmt.Errorf("strike world boss: missing dependencies")
	}
	result := WorldBossStrikeResult{BossID: bossID, AttackerID: attacker.ID}

	// ① 单人物理锁（设计 §7）：仅当**显式声明了 party 且其为单人**时才判锁；party 为空表示「协作共享血池出手」（不锁）。
	// 组队（party 含 ≥2 不同成员，或账本里已沉淀别的出手者）才解锁。world_boss severity 恒 >cap，故单人必锁。
	if soloParty := isSoloParty(attacker.ID, partyUnitIDs); soloParty {
		locked, hpNow, bossName, err := service.worldBossSoloLocked(ctx, worldID, bossID, attacker.ID)
		if err != nil {
			return result, err
		}
		if locked {
			result.SoloLocked = true
			result.HPRemaining = hpNow
			result.SoloCard = service.surfaceWorldBossSoloLock(ctx, worldID, attacker, bossID, bossName)
			return result, nil
		}
	}

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

// ---- ① 单人物理锁（设计 §7：world_boss severity>cap → 单人物理不解锁，必须组队）----

// isSoloParty 判断一次出手声明的 party 是否实为「单人独闯」（设计 §7 单人门的输入侧）。
//   - partyUnitIDs 为空（nil/[]）→ 表示「协作共享血池出手」语义（StrikeWorldBoss 走此路），**不视作单人**（返回 false，不锁）。
//   - 非空时：去重后只剩 attacker 自己（或就是单元素 {attacker}）→ 单人独闯（返回 true）；含 ≥1 个别的成员 → 组队（false）。
//
// 确定性、纯函数：只看成员集合基数，不含任何付费/频率信号。
func isSoloParty(attackerID string, partyUnitIDs []string) bool {
	if len(partyUnitIDs) == 0 {
		return false // 协作共享血池出手：不在「显式 party」语义内，不判单人锁
	}
	distinct := map[string]bool{attackerID: true}
	for _, id := range partyUnitIDs {
		if id != "" {
			distinct[id] = true
		}
	}
	return len(distinct) <= 1
}

// worldBossSeverityCap 是世界Boss单人解锁门的 severity 上限：超过此值物理上单人不解锁（必须组队）。
// 与 encounter.SoloAllowed 的 soloCap 同义；定为 60——elite(40)/dungeon 可单人，world_boss 的 severity 恒在其上。
const worldBossSeverityCap = 60.0

// worldBossSeverity 把世界Boss的血上限（hp_max，来自战力派生、非付费）映射为确定性 severity（设计 §1 四档表：
// world_boss 是「存亡级威胁」）。world_boss 的 severity **恒 > worldBossSeverityCap**——任何被当作 world_boss 投放的目标
// 都视为单人物理不解锁。血量越高 severity 越高（在 [cap+10, 100) 单调），但下限钉死在 cap+10，确保单人门恒触发。
// 纯函数、确定性、不含 wallet/billing。
func worldBossSeverity(hpMax int) float64 {
	floor := worldBossSeverityCap + 10 // world_boss 恒视作存亡级：severity 下限即 >cap，单人物理不解锁
	if hpMax <= 0 {
		return floor // 异常血量也按存亡级处理（保守锁单人）
	}
	// 血量在 (0, maxWorldBossHP] 上单调映射到 [0,100)，再抬到下限 floor 之上（保证恒 >cap、且随血量单调不降）。
	scaled := 100.0 * float64(hpMax) / float64(maxWorldBossHP+1)
	if scaled < floor {
		return floor
	}
	return scaled
}

// worldBossSoloLocked 判定一次出手是否应被单人物理锁挡下（设计 §7）。返回 (是否锁住, Boss 当前血, Boss 名)。
//   - 读 Boss 的 hp_max/hp_remaining/name/status（确定性、不含付费）；非 active 不在此判（交由 strikeTx 报 ErrWorldBossInactive）。
//   - severity = worldBossSeverity(hp_max)；若 encounter.SoloAllowed(severity, cap) 为真（低威胁可 solo）→ 不锁。
//   - 否则数贡献账本里**不同**的既往出手者：本次 attacker 之外**已有 ≥1 名**别的出手者 → 视作组队，解锁；
//     否则（本次出手者是唯一/首位 → 单人）→ 锁住，返回「这不是一个人能撼动的」高光卡。
//
// 反 P2W / 确定性：判定只用 hp_max（战力派生）+ 账本里的不同 actor 集合，绝不含 wallet/billing/出手频率。
// best-effort 读：DB 故障按「不锁」放行（绝不因读失败把本可出手的玩家挡在门外——锁是保护、非门禁），由 strikeTx 兜底校验 active。
func (service *Service) worldBossSoloLocked(ctx context.Context, worldID, bossID, attackerID string) (bool, int, string, error) {
	var hpMax, hpRemaining int
	var bossName, st string
	err := service.db.QueryRowContext(ctx, `
		SELECT hp_max, hp_remaining, name, status FROM world_bosses WHERE id = ? AND world_id = ?`,
		bossID, worldID).Scan(&hpMax, &hpRemaining, &bossName, &st)
	if errors.Is(err, sql.ErrNoRows) || st != "active" {
		return false, hpRemaining, bossName, nil // 不存在/非 active：不在此判，交由 strikeTx 报 ErrWorldBossInactive
	}
	if err != nil {
		return false, 0, "", fmt.Errorf("read world boss for solo gate: %w", err)
	}

	severity := worldBossSeverity(hpMax)
	if encounter.SoloAllowed(severity, worldBossSeverityCap) {
		return false, hpRemaining, bossName, nil // 低威胁档：单人可挑战，不锁
	}

	// 数账本里**不同**的既往出手者（本次 attacker 之外有别人 → 组队 → 解锁）。
	busEvents, err := worldbus.ListByWorldKind(ctx, service.db, worldID, worldbus.KindWorldBossStrike)
	if err != nil {
		// best-effort：读账本失败时不锁（锁是保护非门禁，宁可放行也不误挡），strikeTx 仍会原子兜底。
		return false, hpRemaining, bossName, nil
	}
	others := map[string]bool{}
	for _, ev := range busEvents {
		var p struct {
			BossID string `json:"boss_id"`
		}
		if raw, ok := ev.Payload.(json.RawMessage); ok {
			_ = json.Unmarshal(raw, &p)
		}
		if p.BossID != bossID || ev.ActorID == "" || ev.ActorID == attackerID {
			continue
		}
		others[ev.ActorID] = true
	}
	if len(others) >= 1 {
		return false, hpRemaining, bossName, nil // 已有别的角色出手过 → 组队协力，解锁
	}
	return true, hpRemaining, bossName, nil // 唯一/首位出手者 = 单人 → 物理锁
}

// surfaceWorldBossSoloLock 在单人物理锁触发时，把「这不是一个人能撼动的」祖魂语气高光卡路由进发起者命运收件箱。
// best-effort：SurfaceFateEvent 失败只吞错（绝不让「投卡」失败影响「不打」这个确定性结果）；恒返回卡文案供调用方回传。
func (service *Service) surfaceWorldBossSoloLock(ctx context.Context, worldID string, attacker *unit.Record, bossID, bossName string) string {
	card := fmt.Sprintf("她一个人站在那头%s的影子里，忽然懂了：这不是一个人能撼动的。", bossName)
	if attacker == nil {
		return card
	}
	// 经命运层投一张高光卡（她自己的事，重要度高、情绪低落但非创伤）。worldID 透传供来源标注；失败不阻断。
	_, _ = service.SurfaceFateEvent(ctx, "", attacker, FateEvent{
		ActorID:        attacker.ID,
		TargetID:       attacker.ID,
		ReasonCode:     events.ReasonThreatEmerged, // 威胁浮现：她撞见了一头撼不动的存亡级威胁（流程留痕，非状态变更）
		Importance:     7,
		EmotionWeight:  -0.45,
		Summary:        card,
		SourceRegionID: worldID,
	})
	return card
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

	// H2：唯一遗物(world_boss_relic)经 arbitration 决出归属后，给胜/败方各留可审计零和争夺事件（CROSS_CONTEST_WIN/LOSE）。
	// Scope.WorldID=worldID、Scope.Tick=当前世界时钟（否则被审计 world_id/tick 过滤掉）。仅 ≥2 争夺者时发；反 P2W 只记 score。
	if result.Participants >= 2 {
		tick := 0
		if w, err := world.Get(ctx, service.db, worldID); err == nil {
			tick = w.Tick
		}
		scoreByUnit := make(map[string]float64, len(participants))
		for _, p := range participants {
			scoreByUnit[p.UnitID] = p.Score
		}
		service.recordExclusiveContestOutcomes(ctx, "", worldID, tick, awards, scoreByUnit)
	}

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

// 自动刷新世界Boss：让共享世界里持续有可协作讨伐的 Boss，无需 ops 手动 spawn。
//
// 为何**默认关**（QUNXIANG_WORLD_BOSS_AUTO，仅 true/1/yes/on 视为开）：
//   maybeRefreshWorldBoss 会真实写入 world_bosses（生成一头会改变世界状态的 PvE 目标，全服可见），
//   不是「读出侧投卡」那种零状态变化的自治；与 QUNXIANG_AUTO_PVE/AUTO_MATCH 同向取保守默认——
//   未设即整方法 no-op、零 DB 写、零行为变化，可灰度按 world 逐步开启。

// 自动生成的世界Boss名表（确定性按 FNV(worldID) 选名，与手动 spawn 的 name 解耦）。
var autoWorldBossNames = []string{
	"赤鳞古龙", "万岁幽冥", "九首蚀月蟒", "焚天魔猿", "永夜冰魄", "裂地巨像", "噬星鲲鹏", "血河罗刹王",
}

// 自动生成世界Boss的血量梯度（确定性按 FNV(worldID) 取一档；均在 maxWorldBossHP 内、足够多人协作消耗）。
var autoWorldBossHPTiers = []int{120_000, 200_000, 360_000, 600_000}

// worldBossAutoEnabled 读 QUNXIANG_WORLD_BOSS_AUTO（true/1/yes/on 视为开，大小写不敏感、忽略首尾空白），
// 默认关 → maybeRefreshWorldBoss 整方法 no-op、零行为变化、零 DB 写。
func worldBossAutoEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_WORLD_BOSS_AUTO"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// worldBossNameHash 确定性哈希（项目约定的 FNV）：把 worldID + salt 映射到稳定的 uint64，用于选名/定血。
func worldBossNameHash(worldID, salt string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("worldboss-auto:" + worldID + ":" + salt))
	return h.Sum64()
}

// maybeRefreshWorldBoss 在部署边界 best-effort 保证共享世界里始终有一头可讨伐的世界Boss。
//   - flag QUNXIANG_WORLD_BOSS_AUTO 默认关 → 直接 return（这是会改变世界状态的自治生成，保守默认、可灰度）。
//   - worldID 为空（未接入共享世界）→ 直接 return：世界Boss是跨玩家共享机制，单局无意义。
//   - 该 world 已有 status='active' 的世界Boss → return（不重复刷，单世界同时至多一头自治 Boss）。
//   - 否则确定性生成一头：名按 FNV(worldID) 取名表、血按 FNV(worldID) 取梯度、regionID 空（全世界共享、不绑区域）。
//
// 并发正确性（修审计§8 已知低危 TOCTOU 竞态）：早先实现是「COUNT active → 无则 SpawnWorldBoss INSERT」两步，
// COUNT 与 INSERT 间存在 TOCTOU 窗口——高并发多请求都见 0 → 都 INSERT → 生成多头 Boss（违反单世界至多一头）。
// 现改为**单条原子 INSERT ... SELECT ... WHERE NOT EXISTS**：是否已有 active 的判定与插入在 DB 一条语句内原子完成，
// 任何瞬间至多一头 active 自治 Boss。RowsAffected==0 表示已有 active（被 WHERE NOT EXISTS 挡下）→ 正常兜底 return nil；
// ==1 表示本请求成功插入。手动投放路径 SpawnWorldBoss 不变（手动单次无竞态）。
//
// best-effort：任一步出错只返回该错（由调用方在边界吞错记日志），绝不中断回合推进。
func (service *Service) maybeRefreshWorldBoss(ctx context.Context, worldID string) error {
	if service == nil || service.db == nil {
		return nil
	}
	if worldID == "" || !worldBossAutoEnabled() {
		return nil
	}
	// 世界必须已存在：与 SpawnWorldBoss 同口径——否则首次出手 AdvanceTick 才报错，那时血已被改、状态已脏（finding robustness）。
	if _, err := world.Get(ctx, service.db, worldID); err != nil {
		return fmt.Errorf("auto spawn world boss: %w", err)
	}

	// ② 威胁度链升级 provenance（设计 §1：world_boss 仅当 region 威胁度触顶且已沉淀 ≥1 个未解决 field_boss 时升级，不凭空刷）。
	// 当前无 threats 持久表，用既有可查信号近似 provenance 门：该 world 是否已沉淀 ≥1 个 field_boss/elite「痕迹」
	// （events 表 THREAT_EMERGED 计数）。无任何痕迹 → 威胁度尚未升级到该出 world_boss 的程度 → 不凭空 spawn（return nil）。
	// 有痕迹才升级——保证自动刷新有「威胁度链」前因，而非脱钩威胁度的凭空 FNV spawn。
	emergedTraces, err := service.countThreatEmergedTraces(ctx, worldID)
	if err != nil {
		return fmt.Errorf("auto spawn world boss: read threat provenance: %w", err)
	}
	if emergedTraces < 1 {
		return nil // 无 provenance 信号（该 region 尚无沉淀的 field_boss/elite 痕迹）→ 不凭空刷，正常 no-op
	}

	// 名/血都由 FNV(worldID) 派生：同一世界稳定可复现，不同世界各异，不用全局 rand。
	name := autoWorldBossNames[worldBossNameHash(worldID, "name")%uint64(len(autoWorldBossNames))]
	hp := autoWorldBossHPTiers[worldBossNameHash(worldID, "hp")%uint64(len(autoWorldBossHPTiers))]
	id := "wboss_" + uuid.NewString()

	// 原子条件 INSERT：仅当该 world 当前**无** active Boss 时才插入；判定与插入在一条 SQL 内原子完成，杜绝 COUNT→INSERT 的 TOCTOU。
	// created_at 沿用列默认值（与 SpawnWorldBoss 一致）：SQLite 用 CURRENT_TIMESTAMP、MySQL 用空串默认，不在此列显式赋值。
	var query string
	if dbdialect.IsMySQL(service.db) {
		// MySQL 不允许 INSERT ... SELECT 直接省略 FROM，需 FROM DUAL 才能挂 WHERE NOT EXISTS。
		query = `
			INSERT INTO world_bosses (id, world_id, name, hp_max, hp_remaining, status, region_id)
			SELECT ?, ?, ?, ?, ?, 'active', ''
			FROM DUAL
			WHERE NOT EXISTS (SELECT 1 FROM world_bosses WHERE world_id = ? AND status = 'active')`
	} else {
		query = `
			INSERT INTO world_bosses (id, world_id, name, hp_max, hp_remaining, status, region_id)
			SELECT ?, ?, ?, ?, ?, 'active', ''
			WHERE NOT EXISTS (SELECT 1 FROM world_bosses WHERE world_id = ? AND status = 'active')`
	}
	res, err := service.db.ExecContext(ctx, query, id, worldID, name, hp, hp, worldID)
	if err != nil {
		// L4 唯一兜底：WHERE NOT EXISTS 是主护栏，partial unique index uq_world_boss_active 是硬兜底。
		// 两个并发 INSERT 的 NOT EXISTS 子查询可能都见 0（彼此尚未提交）→ 都尝试插入 → 后者触发 UNIQUE 约束冲突。
		// 这等价于「已有 active Boss」——视为正常兜底（类 INSERT IGNORE 语义），返回 nil、不外抛、不中断回合推进。
		// MySQL 的 gap-lock 理论竞态属 documented residual（flag 默认关）。
		if isDupKeyErr(err) {
			return nil
		}
		return fmt.Errorf("auto spawn world boss: %w", err)
	}
	// 无论 RowsAffected 为 0（WHERE NOT EXISTS 挡下：已有 active Boss——手动投放或另一并发请求刚插入，正常兜底）
	// 还是 1（本请求成功插入一头自治 Boss），都满足「单世界至多一头 active」不变量，皆为正常结果。
	// 仅当本请求真正插入了一头（RowsAffected==1）才记 provenance（避免 no-op 兜底重复留痕）。
	if affected, _ := res.RowsAffected(); affected == 1 {
		// 记明本次升级的威胁度链依据（沉淀的 field_boss/elite 痕迹数 + FNV 派生的名/血档），append-only 留痕、不改任何状态字段。
		// best-effort：留痕失败只吞错（Boss 已成功落库，绝不因「记依据失败」回滚或外抛）。
		service.recordWorldBossProvenance(ctx, worldID, id, name, hp, emergedTraces)
	}
	return nil
}

// countThreatEmergedTraces 数某世界已沉淀的 field_boss/elite「威胁浮现」痕迹（events 表 reason_code=THREAT_EMERGED、world_id 命中）。
// 这是 world_boss 自动升级的 provenance 门近似信号（无 threats 持久表时的可查替代）：≥1 即视作「该 region 威胁度已累积、
// 沉淀过未解决的 field_boss/elite」，才允许把威胁度链升级到 world_boss。纯读、确定性、不含付费。
func (service *Service) countThreatEmergedTraces(ctx context.Context, worldID string) (int, error) {
	var n int
	if err := service.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM events WHERE world_id = ? AND reason_code = ?`,
		worldID, string(events.ReasonThreatEmerged)).Scan(&n); err != nil {
		return 0, fmt.Errorf("count threat emerged traces: %w", err)
	}
	return n, nil
}

// worldBossSpawnProvenanceKind 是世界Boss自动升级 provenance 在世界总线上的事件类型（append-only、owner-less、不进贡献账本）。
// 它**不是** KindWorldBossStrike（settleWorldBoss 只按 strike 类读账本，故 provenance 不会污染分赃），也不是 DOWN 广播；
// 是一条独立的「升级依据」留痕，供审计/复算「凭什么把威胁度升级到了 world_boss」。
const worldBossSpawnProvenanceKind worldbus.EventKind = "WORLD_BOSS_SPAWN"

// recordWorldBossProvenance 把一次世界Boss自动升级的「威胁度链依据」追加进世界总线（append-only、非状态变更、不经 Mutator）。
// payload 记明 boss_id/名/血档 + 触发升级的 field_boss/elite 痕迹数（provenance：凭什么把威胁度升级到了 world_boss）。
// 走世界总线而非 events 表：provenance 是**世界级、无具体 owner**的事件，events.actor_unit_id 有 units FK（空串过不了），
// 世界总线 actor 可空（nullable）——与 DOWN 广播同口径。best-effort：吞错只 log，绝不阻断（Boss 已落库，留痕失败不回滚）。
func (service *Service) recordWorldBossProvenance(ctx context.Context, worldID, bossID, name string, hp, emergedTraces int) {
	if _, err := service.RecordCrossInteraction(ctx, worldID, "", bossID,
		worldBossSpawnProvenanceKind, 5,
		map[string]any{
			"boss_id":      bossID,
			"name":         name,
			"hp_max":       hp,
			"tier":         "world_boss",
			"provenance":   "threat_escalation", // 升级依据：威胁度链（非凭空 FNV spawn）
			"field_traces": emergedTraces,       // 触发升级时沉淀的 field_boss/elite 痕迹数
			"auto":         true,                // 自动刷新（QUNXIANG_WORLD_BOSS_AUTO）
		}); err != nil {
		// best-effort：留痕失败不影响已落库的 Boss（绝不回滚、不外抛、不中断回合推进）。
		slog.Warn("record world boss provenance failed (best-effort)", "world", worldID, "boss", bossID, "err", err)
	}
}

// isDupKeyErr 判断一个 DB 错误是否为唯一约束冲突（partial unique index uq_world_boss_active 触发）。
// SQLite 报 "UNIQUE constraint failed"、MySQL 报 "Duplicate entry ... for key"——大小写不敏感按错误串匹配
// （跨方言无统一错误码，串匹配是 modernc/驱动无关的稳妥判法）。命中即视为「已有 active Boss」正常兜底。
func isDupKeyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "constraint") || strings.Contains(msg, "duplicate")
}
