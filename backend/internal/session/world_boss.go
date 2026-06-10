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
//   - 跨分片参战者的金币只记 awards、不在本库发放（跨分片结算是后续课题）。单库部署下 GetByID 必命中、本残留不触发
//     （实发=应发）；多分片前须落地 pending_cross_shard_awards 待结算表，否则 awards 是分配账面、跨分片部分未必已入钱包。
//     现已在发钱循环对未命中的 award 落结构化 warn（evt #2），让对账能发现「分配了未发放」的金额，而非静默 continue。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/featureflags"
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
// 若这一击清零血池，则由抢到结算闩锁的请求读账本全员分赃并广播。这是唯一的 HTTP 出手入口（不传 party）。
//
// 世界Boss 是共享血池的异步协作机制，不同玩家的角色在不同时间各自出手共同消耗血池。是否真有协作，**以账本的
// distinct-actor 集合为事实来源**（≥2 名不同出手者 = 已成群体围猎、解锁），而非凭「是否显式传 party」。
// 故协作入口（party=nil）仍会评估单人物理锁：存亡级 Boss 的**首位单人出手者**会被锁（不开打、返回高光卡），
// 待账本沉淀 ≥1 名别的出手者后单人迟到者即解锁。这堵住了「单人反复 POST /strike 凿穿世界Boss」的缺口（finding antip2w）。
// 「单人独自撼动世界Boss」的物理锁逻辑由 StrikeWorldBossParty 统一承载（含 nil/协作入口）。
func (service *Service) StrikeWorldBoss(ctx context.Context, worldID string, bossID string, attacker *unit.Record) (WorldBossStrikeResult, error) {
	if attacker == nil {
		return WorldBossStrikeResult{}, fmt.Errorf("strike world boss: missing dependencies")
	}
	// 协作入口：party=nil；StrikeWorldBossParty 以账本 distinct-actor 集合判是否真有协作（≥2 不同出手者才解锁单人锁）。
	return service.StrikeWorldBossParty(ctx, worldID, bossID, attacker, nil)
}

// StrikeWorldBossParty 是世界Boss出手入口（设计 §7 单人物理锁），承载 nil（协作）与显式 party 两种调用。
// partyUnitIDs 是本次参战队伍的全部成员（含 attacker）；为空/仅含 attacker 即「单人独闯」。
//   - ① 单人物理锁：world_boss 的 severity 恒 >soloCap（存亡级威胁）。**是否真有协作以账本 distinct-actor 集合为事实来源**：
//     显式声明 ≥2 成员的组队（isSoloParty=false）直接放行；否则（显式单人 **或** party=nil 协作入口）评估 worldBossSoloLocked——
//     账本里本次 attacker 之外已有 ≥1 名别的出手者（≥2 distinct）= 已成群体围猎、解锁；否则（首位/唯一出手者=真·单人）
//     **根本不让 strike**（不扣血、不记账本），返回「这不是一个人能撼动的」高光卡。
//   - 解锁后落入与共享血池协作完全一致的原子扣血 + 记账本 + 闩锁结算链路。
//
// 确定性、付费不进：是否锁只看 hp_max（战力派生 severity）+ party 成员数 + 账本 distinct-actor 集合，绝不含 wallet/billing/出手频率。
func (service *Service) StrikeWorldBossParty(ctx context.Context, worldID string, bossID string, attacker *unit.Record, partyUnitIDs []string) (WorldBossStrikeResult, error) {
	if service == nil || service.db == nil || attacker == nil {
		return WorldBossStrikeResult{}, fmt.Errorf("strike world boss: missing dependencies")
	}
	result := WorldBossStrikeResult{BossID: bossID, AttackerID: attacker.ID}

	// ① 单人物理锁（设计 §7）：是否真有协作以**账本 distinct-actor 集合**为事实来源，不依赖显式 party 声明。
	// 旧实现仅在「显式声明了单人 party」时判锁——但唯一 HTTP 出手入口 StrikeWorldBoss 恒传 nil party，
	// 致 isSoloParty(id,nil)=false 短路、整套单人锁生产不可达，单人反复 POST /strike 可凿穿存亡级 Boss（finding antip2w）。
	// 修：显式声明单人 party **或** 未声明（nil/协作入口）时一律评估 worldBossSoloLocked——它内部数账本里
	// **不同**出手者：本次 attacker 之外**已有 ≥1 名**别的出手者（≥2 distinct）→ 已成协作围猎、解锁；否则（本次是
	// 唯一/首位出手者=真·单人）→ 物理锁、不开打。显式声明含 ≥2 成员的组队仍由 isSoloParty=false 直接放行（已有协作意图）。
	// 确定性、付费不进：判定只看 hp_max（战力派生 severity）+ 账本 distinct-actor 集合，绝不含 wallet/billing/出手频率。
	if isSoloParty(attacker.ID, partyUnitIDs) || len(partyUnitIDs) == 0 {
		locked, hpNow, bossName, err := service.worldBossSoloLocked(ctx, worldID, bossID, attacker.ID)
		if err != nil {
			return result, err
		}
		if locked {
			result.SoloLocked = true
			result.HPRemaining = hpNow
			// 记一条单人撞门 attempt（append-only、不扣血、不进贡献账本）：让第二名**不同** actor 撞门时能看到前者→双方解锁自举。
			// best-effort：留痕失败只吞错（绝不让「记 attempt」失败改变「被锁、不打」这个确定性结果）。
			service.recordWorldBossSoloAttempt(ctx, worldID, bossID, attacker.ID)
			result.SoloCard = service.surfaceWorldBossSoloLock(ctx, worldID, attacker, bossID, bossName)
			return result, nil
		}
	}

	// 解锁后落入共享扣血核心（与区域共享 boss Phase4 完全复用同一套原子扣血 + 闩锁 + 分赃）；
	// world_boss 的掉落规格用 worldBossLootSpec（货币=hp_max、遗物=world_boss_relic）。
	return service.strikeSharedBossCore(ctx, worldID, bossID, attacker, result, func(hpMax int) bossLootSpec {
		return worldBossLootSpec(hpMax)
	})
}

// strikeSharedBossCore 是「共享血池 boss 一次出手」的**结算核心**（设计 §5/§7）：原子扣血 + 记贡献账本（strikeTx）→
// 血池清零抢单次结算闩锁 → settleWorldBoss 按贡献全员分赃（排他遗物走 arbitration 胜率∝贡献、可分件按贡献瓜分、钱包经 Mutator）。
// 它被两个入口复用，**不含任何单人物理锁判定**（锁是入口职责）：
//   - 全世界 world_boss（StrikeWorldBossParty，先过单人锁再入此核心）；
//   - 区域共享 zone boss（Phase4 challengeSharedZoneBoss，field_boss 档可单人 chip，不过单人锁，直接入此核心）。
//
// lootFor 把 boss 的 hp_max 映射为掉落规格（world_boss 用货币=hp_max+world_boss_relic；zone boss 用缩放货币+zone_boss_relic）。
// 并发安全（与 world_boss 原版逐字节同源）：strikeTx 的原子扣血（UPDATE...WHERE status='active'）防把血扣成负/复活已死 boss；
// 血池清零后 UPDATE status='defeated' WHERE status='active' 的 RowsAffected==1 单次结算闩锁防并发双结算。
func (service *Service) strikeSharedBossCore(ctx context.Context, worldID, bossID string, attacker *unit.Record, result WorldBossStrikeResult, lootFor func(hpMax int) bossLootSpec) (WorldBossStrikeResult, error) {
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

	if err := service.settleWorldBoss(ctx, worldID, bossID, bossName, lootFor(hpMax), &result); err != nil {
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

// worldBossSoloAttemptKind 是「单人撞门被锁」的世界总线留痕类型（append-only、owner=出手者、不进贡献账本/不扣血）。
// 作用：让协作能从「各自单人撞门」自举——首位单人被锁时记一条 attempt，第二名**不同** actor 撞门即在账本里看到前者
// （≥2 distinct）→ 视作群体围猎、解锁，双方随后真打。单个玩家反复撞门只会累积同一 actor 的 attempt（distinct 仍 1）→
// 恒锁、永不扣血（堵住「单人反复 POST /strike 凿穿世界Boss」的反 P2W 缺口）。它**不是** KindWorldBossStrike，
// 故 settleWorldBoss 只按 strike 读账本时绝不会把「只撞门没真打」的人算进分赃。
const worldBossSoloAttemptKind worldbus.EventKind = "WORLD_BOSS_SOLO_ATTEMPT"

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
//   - 否则数账本里**不同**的既往参与者（真出手 KindWorldBossStrike **或** 撞门 attempt KindWorldBossSoloAttempt）：
//     本次 attacker 之外**已有 ≥1 名**别的不同 actor → 视作群体围猎，解锁；否则（本次出手者是唯一/首位 → 真·单人）→ 锁住。
//     算上 attempt 是为让协作能从「各自单人撞门」自举：首位被锁记 attempt，第二名不同 actor 撞门即见前者→双方解锁真打；
//     单个玩家反复撞门只累积同一 actor 的 attempt（distinct 仍 1）→ 恒锁、永不扣血（反 P2W）。
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

	// 数账本里**不同**的既往参与者（真出手 + 撞门 attempt 都算），本次 attacker 之外有别人 → 群体围猎 → 解锁。
	others := map[string]bool{}
	for _, kind := range []worldbus.EventKind{worldbus.KindWorldBossStrike, worldBossSoloAttemptKind} {
		busEvents, err := worldbus.ListByWorldKind(ctx, service.db, worldID, kind)
		if err != nil {
			// best-effort：读账本失败时不锁（锁是保护非门禁，宁可放行也不误挡），strikeTx 仍会原子兜底。
			return false, hpRemaining, bossName, nil
		}
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
	}
	if len(others) >= 1 {
		return false, hpRemaining, bossName, nil // 已有别的角色出手/撞门过 → 群体围猎，解锁
	}
	return true, hpRemaining, bossName, nil // 唯一/首位参与者 = 单人 → 物理锁
}

// recordWorldBossSoloAttempt 把一次「单人撞门被锁」追加进世界总线（append-only、actor=出手者、kind=SOLO_ATTEMPT）。
// 它不扣血、不进贡献账本（settleWorldBoss 只读 KindWorldBossStrike），仅供 worldBossSoloLocked 的 distinct-actor 自举判定。
// best-effort：留痕失败只 log，绝不改变「被锁、不打」的确定性结果，也绝不外抛中断。
// payload 记 boss_id（与 strike 同口径，供 worldBossSoloLocked 按 boss 过滤）+ 标记，确定性、不含 wallet/billing/频率。
func (service *Service) recordWorldBossSoloAttempt(ctx context.Context, worldID, bossID, attackerID string) {
	if service == nil || service.db == nil || worldID == "" || attackerID == "" {
		return
	}
	if _, err := service.RecordCrossInteraction(ctx, worldID, attackerID, bossID,
		worldBossSoloAttemptKind, 2,
		map[string]any{"boss_id": bossID, "solo_locked": true}); err != nil {
		slog.Warn("record world boss solo attempt failed (best-effort)", "world", worldID, "boss", bossID, "actor", attackerID, "err", err)
	}
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

// bossLootSpec 描述一头共享 boss 讨平时的掉落规格（排他遗物 + 货币池 + loot key 标签），让 settleWorldBoss
// 在「全世界 world_boss」与「区域共享 zone boss」（Phase4 共享进度）之间复用同一套贡献账本读取 + 分赃 + 仲裁 + 发钱链路，
// 仅掉落物/key 不同。反 P2W：货币池来自 hp_max（战力派生），绝不含 wallet/billing。
type bossLootSpec struct {
	RelicID  string // 排他遗物 ID（走 arbitration 胜率∝贡献归属）
	GoldQty  int    // 可分割货币总量（按贡献 SplitProportional 瓜分）
	KeyTag   string // loot key 里的档位标签（worldboss / zoneboss），保证不同档 boss 的仲裁 key 不撞
}

// worldBossLootSpec 是全世界 world_boss 的默认掉落规格（与历史行为逐字节一致）。
func worldBossLootSpec(hpMax int) bossLootSpec {
	return bossLootSpec{RelicID: worldBossEpicRelicID, GoldQty: hpMax, KeyTag: "worldboss"}
}

// settleWorldBoss 读世界总线的贡献账本，按贡献全员分赃，并广播讨平事件。掉落规格由 loot 给（world_boss / zone boss 复用）。
func (service *Service) settleWorldBoss(ctx context.Context, worldID string, bossID string, bossName string, loot bossLootSpec, result *WorldBossStrikeResult) error {
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
	lootItems := []encounter.LootItem{
		{ID: "gold", Rarity: encounter.Common, Quantity: loot.GoldQty},
		{ID: loot.RelicID, Rarity: encounter.Epic, Quantity: 1},
	}
	key := fmt.Sprintf("%s|%s|%s", worldID, loot.KeyTag, bossID)
	awards := encounter.AllocateLoot(key, lootItems, participants, fieldBossMinMeaningful)
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
			// 跨分片/非本库角色：award 已写进 result.Awards（玩家/审计可见的分配账面），但本库无此 unit → 无法经 Mutator 发钱。
			// 评审 #2：单库部署下 GetByID 按 unitID 全表查、无 shard 过滤，所有出手者必命中、本分支不触发（实发=应发）；
			// 一旦演进到真·物理多分片，跨分片贡献者金币会静默蒸发而 awards 仍显示「分到了」→ 发放总额<分配总额。
			// 故此处不再静默 continue，而是落一条结构化 warn（unitID + 应发金额 + boss），让对账能发现「分配了未发放」的金额。
			// 中期落地跨分片发放（pending_cross_shard_awards 待结算表，目标分片异步认领经其本侧 Mutator 入账）见 docs/PvE威胁系统.md。
			slog.Warn("world boss gold award not granted: unit not in local store (cross-shard residual)",
				"world", worldID, "boss", bossID, "unit", unitID, "gold", gold)
			continue
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
	switch strings.ToLower(strings.TrimSpace(featureflags.EnvOrOverride("QUNXIANG_WORLD_BOSS_AUTO"))) {
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
	// 全世界自治 boss 占 region_id='' 槽：NOT EXISTS 须按 (world_id, region_id='') 判，否则会被某个 zone boss
	// （region_id=worldID#zoneID 非空）误挡（Phase4 多区 boss 并存后，单看 world_id 的「已有 active」恒为真）。
	var query string
	if dbdialect.IsMySQL(service.db) {
		// MySQL 不允许 INSERT ... SELECT 直接省略 FROM，需 FROM DUAL 才能挂 WHERE NOT EXISTS。
		query = `
			INSERT INTO world_bosses (id, world_id, name, hp_max, hp_remaining, status, region_id)
			SELECT ?, ?, ?, ?, ?, 'active', ''
			FROM DUAL
			WHERE NOT EXISTS (SELECT 1 FROM world_bosses WHERE world_id = ? AND region_id = '' AND status = 'active')`
	} else {
		query = `
			INSERT INTO world_bosses (id, world_id, name, hp_max, hp_remaining, status, region_id)
			SELECT ?, ?, ?, ?, ?, 'active', ''
			WHERE NOT EXISTS (SELECT 1 FROM world_bosses WHERE world_id = ? AND region_id = '' AND status = 'active')`
	}
	res, err := service.db.ExecContext(ctx, query, id, worldID, name, hp, hp, worldID)
	if err != nil {
		// L4 唯一兜底：WHERE NOT EXISTS 是主护栏，唯一约束 uq_world_boss_active 是硬兜底（两驱动均有）。
		// 两个并发 INSERT 的 NOT EXISTS 子查询可能都见 0（彼此尚未提交）→ 都尝试插入 → 后者触发 UNIQUE 约束冲突。
		// 这等价于「已有 active Boss」——视为正常兜底（类 INSERT IGNORE 语义），返回 nil、不外抛、不中断回合推进。
		// SQLite 走 partial unique index、MySQL 走 STORED 生成列 active_region_key + 唯一键（Phase4 评审 #1 补齐，
		// 见 dbmigrate.EnsureWorldBossActiveUnique）——MySQL gap-lock 双插不再是 documented residual。
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
