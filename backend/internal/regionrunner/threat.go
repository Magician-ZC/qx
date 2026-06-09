package regionrunner

// 文件说明：region-runner PvE 接入（沙盘 §8.2 / docs/PvE威胁系统.md / docs/region-runner-PvE接入方案.md）——
// 让被唤醒的离线 **HOT（正活跃）** 单位会在路上撞见 elite 硬茬。决策走 engine/decision.Router 的关键节点闸：
// HP 危急先反射撤退保命（零 LLM），否则 StrategicFork 升级为遭遇。整块 flag-gated（QUNXIANG_REGION_RUNNER_THREATS，默认关）。
//
// **威胁刷新（本切片：region.threat_level 真累积 + freshness 反扎堆）**：威胁概率的「region 威胁度项」此前用硬编码常量
// threatBaseLevel；现改为读 region 注册表的真实 threat_level 累积值（命中威胁经 bumpRegionThreat→BumpThreatLevel 持续 +1，
// 危险区天然越来越危险=「威胁扎堆」§11.3）。再叠一个 **freshness 反扎堆项**：同区刚出过威胁则短期内压低再次触发概率
// （进程内 per-region 最近命中 tick，refractory 窗口内乘性衰减），避免威胁在同一区一窝蜂连刷。**破圈下限 threatFloorPerMille
// 始终保留**（draw<floor 必命中、不受 base/freshness 影响——世界仍处处有危险，不全扎堆活跃区）。
// 整条「读真 threat_level + freshness」**仅在分片开（QUNXIANG_REGION_SHARDING）+ registry 已注入**时生效；分片关 / registry==nil /
// region 未登记 → 回退 threatBaseLevel 常量基线，与本切片前**逐结果等价、零行为变化**（既有 elite 触发链不受影响）。确定性：
// roll 仍是 FNV-64a(sessionID:unitID:tick)，freshness 仅按 per-region 最近命中 tick 做确定性 refractory，不引入全局 rand。
//
// 真遭遇结算经**注入式 threatHandler**（main 注入 session.TriggerEliteEncounter，保持 regionrunner 不依赖 session）；
// 未注入（PvE-1 shadow）则只计遥测、不改单位。
//
// 并发硬化（PvE-2 + PvE-3，与 real-2→real-3-0 同款）：触发 handler 前 maybeEncounterThreat 已查一次让位（execGuard=
// IsExecutionRunning），故已在异步战斗执行中的会话不会触发遭遇。**PvE-3 已落地**：session.ResolveEliteEncounter 的每回合
// HP/钱包/士气写已改用 status.Mutator.ApplyOptimistic + 冲突重试（applyEliteMutation），覆盖**所有并发写者**。

import (
	"context"
	"hash/fnv"
	"sync/atomic"

	"qunxiang/backend/internal/agentqueue"
	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/scheduler"
	"qunxiang/backend/internal/unit"
)

const (
	// 锚加权威胁概率参数（PvE-4，‰=千分比）。每次 HOT 唤醒的威胁概率 = region 威胁度项 + 锚密度项，夹 [破圈下限, 上限]。
	// HOT 单位 ~每 TickSeconds 才唤醒一次，故真实遭遇频率远低于这些 per-tick ‰。
	threatBaseLevel            = 50  // region 基线威胁度 [0,100]（**回退常量**：分片关/registry==nil/region 未登记时用；分片开则被 region.threat_level 真累积值替换）
	threatLevelMax             = 100 // region 威胁度归一上限（threat_level 被夹到 [0,threatLevelMax] 喂概率项，超量不再加成）
	threatLevelPerMilleAtFull  = 20  // threat_level=100 时贡献概率(‰)
	threatAnchorPerMilleAtFull = 60  // anchor_density=1 时贡献概率(‰)——越在乎，威胁越易找上她
	threatFloorPerMille        = 5   // 破圈下限(‰)：零锚单位也有的最低威胁概率（世界仍有危险，不全扎堆）；freshness/base 再低也不破此线
	threatMaxPerMille          = 80  // 概率上限(‰)
	// hpMaxForThreat 是 HP clamp 上限（mutator FieldHP clampInt(0,100)），喂 Router 的 HP/HPMax 危急护栏。
	hpMaxForThreat = 100

	// freshness 反扎堆（§11.3）：同区刚出过威胁 → 短期内压低再次触发概率，避免一窝蜂连刷同一区。
	// 衰减只作用于「base+anchor」项；破圈下限 threatFloorPerMille 不被衰减（世界处处有危险）。仅分片开 + registry 时按
	// per-region 最近命中 tick 生效；refractory 窗口外或冷启动（无最近命中记录）→ 衰减因子=1（与无 freshness 等价）。
	threatFreshnessWindowTicks = 8   // refractory 窗口（tick）：上次命中后多少 tick 内仍施加衰减
	threatFreshnessMinPerMille = 100 // 紧贴上次命中（Δtick=0）时保留的「base+anchor 概率」千分比下限（=10%，即压到 1/10）；窗口内线性回升到 1000(=100%)

	// NPC 锚加权预算（设计 §1.5：从源头不产噪音）。世界事件预算按「该 NPC 的玩家锚密度」加权：
	//   baseWeight = anchorBudgetFloor + (1-anchorBudgetFloor)·anchorDensity ∈ [floor,1]
	// 一个 NPC 被越多角色当作锚指向（density→1）→ baseWeight→1，威胁/事件聚焦她在乎的人和地方；
	// 零锚 NPC（density=0）→ baseWeight=floor，事件被压到地板（噪声世界自消化，但破圈下限仍保留=世界处处有危险）。
	// anchorBudgetFloor=0.25 即设计宪法「0.25 + 0.75·密度」的地板项；纯密度驱动、付费无关、确定性。
	anchorBudgetFloorPerMille = 250 // 锚加权预算地板(‰)：零锚 NPC 仍保 25% 事件预算（与破圈下限协同，世界不全扎堆活跃区）
)

// SetThreatHandler 注入「真遭遇结算」回调（main 注入 session.TriggerEliteEncounter 包装）。
// 不注入（PvE-1 shadow）则威胁只计遥测、不改单位。nil-safe。
func (r *Runner) SetThreatHandler(handler func(ctx context.Context, sessionID string, unitID string) error) {
	if r == nil {
		return
	}
	r.threatHandler = handler
}

// SetAnchorDensityProvider 注入「某单位在乎程度」的密度查询（main 接 session.AnchorDensity），用于锚加权威胁刷新（PvE-4）。
// 不注入则密度恒 0 → 威胁只按 region 基线 + 破圈下限（不按锚加权）。保持 regionrunner 不依赖 session。nil-safe。
func (r *Runner) SetAnchorDensityProvider(provider func(ctx context.Context, unitID string) float64) {
	if r == nil {
		return
	}
	r.anchorDensity = provider
}

// threatRoll1000 是确定性均匀抽样 [0,1000)（FNV-64a(sessionID:unitID:tick) mod 1000）。
// 不依赖全局 rand、可复现（与 session 模拟逻辑一致）；与阈值 threatSpawnPerMille 比较决定是否撞威胁。
func threatRoll1000(sessionID string, unitID string, tick int64) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(sessionID))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(unitID))
	_, _ = h.Write([]byte{':'})
	var tb [8]byte
	for i := 0; i < 8; i++ {
		tb[i] = byte(tick >> (8 * i))
	}
	_, _ = h.Write(tb[:])
	return int(h.Sum64() % 1000)
}

// clampThreatLevel 把任意 threat_level 累积值夹到 [0,threatLevelMax]，喂概率项（负值视 0、超量封顶）。
func clampThreatLevel(level int64) int64 {
	if level < 0 {
		return 0
	}
	if level > threatLevelMax {
		return threatLevelMax
	}
	return level
}

// spawnPerMilleAtLevel 把 region 威胁度(base, [0,threatLevelMax]) + 锚密度 + freshness 千分比(freshnessPerMille, [0,1000])
// 算成本次唤醒的威胁概率(‰)。组成：
//   - base 项 = threatLevelPerMilleAtFull · base/threatLevelMax（region 越危险越易撞——「威胁扎堆」）；
//   - anchor 项 = threatAnchorPerMilleAtFull · anchorDensity（越在乎威胁越易找上她）；
//   - 两项之和先按 freshness 千分比乘性衰减（同区刚出过威胁则压低），再夹 [破圈下限, 上限]。
//
// **破圈下限恒保留**：哪怕 base=0 且 freshness 压到 0，仍返回 ≥threatFloorPerMille（世界处处有危险，不全扎堆活跃区）。
func spawnPerMilleAtLevel(base int64, anchorDensity float64, freshnessPerMille int) int {
	base = clampThreatLevel(base)
	if anchorDensity < 0 {
		anchorDensity = 0
	} else if anchorDensity > 1 {
		anchorDensity = 1
	}
	if freshnessPerMille < 0 {
		freshnessPerMille = 0
	} else if freshnessPerMille > 1000 {
		freshnessPerMille = 1000
	}
	baseTerm := threatLevelPerMilleAtFull * int(base) / threatLevelMax
	anchorTerm := int(float64(threatAnchorPerMilleAtFull)*anchorDensity + 0.5)
	// freshness 只衰减「会聚集到活跃区/在乎处」的 base+anchor 项；破圈下限不被衰减。
	weighted := (baseTerm + anchorTerm) * freshnessPerMille / 1000
	if weighted < threatFloorPerMille {
		weighted = threatFloorPerMille
	}
	if weighted > threatMaxPerMille {
		weighted = threatMaxPerMille
	}
	return weighted
}

// threatSpawnPerMille 是回退基线版（分片关/registry 不可用时用）：region 威胁度取硬编码 threatBaseLevel 常量、无 freshness 衰减。
// 与本切片前**逐结果等价**（保证既有 elite 触发链 + 既有测试零行为变化）。新路径见 maybeEncounterThreat 内的 spawnPerMilleAtLevel。
func threatSpawnPerMille(anchorDensity float64) int {
	return spawnPerMilleAtLevel(threatBaseLevel, anchorDensity, 1000)
}

// anchorWeightedBudgetPerMille 把某 NPC 的锚密度算成「有意义事件预算」千分比 ∈ [anchorBudgetFloorPerMille,1000]
// （设计 §1.5：baseWeight = 0.25 + 0.75·density）。语义：一个 NPC 被越多角色当作锚指向（density→1）→ 预算→1000，
// 重大事件越该聚焦到她在乎的人和地方；零锚 NPC（density=0）→ 预算=地板(250‰)，事件被压低（噪声世界自消化）。
// 纯密度驱动、付费无关、确定性、纯函数。density 越界自夹 [0,1]。
func anchorWeightedBudgetPerMille(density float64) int {
	if density < 0 {
		density = 0
	} else if density > 1 {
		density = 1
	}
	span := 1000 - anchorBudgetFloorPerMille
	return anchorBudgetFloorPerMille + int(float64(span)*density+0.5)
}

// isHighAnchorDensity 判定某 NPC 的锚密度是否「高」（被够多角色在乎，事件值得留 ReasonAnchorWeightedEvent 痕）。
// 阈值取锚加权预算超过中点（density>0.5 → 预算>625‰）。纯函数、确定性。
func isHighAnchorDensity(density float64) bool {
	return density > 0.5
}

// emitAnchorWeightedEvent 在「事件确实聚焦到高锚 NPC」时写一条 ReasonAnchorWeightedEvent 流程事件留痕
// （设计 §1.5「祸福偏要落在她最在意的人和地方」）。best-effort：db 缺失/写失败只记 Debug、绝不影响遭遇结算。
func (r *Runner) emitAnchorWeightedEvent(ctx context.Context, sessionID, unitID, regionID string, density float64, tick int64) {
	if r == nil || r.db == nil || unitID == "" {
		return
	}
	if _, err := events.EmitProcessEvent(ctx, r.db, events.ProcessEvent{
		SessionID:   sessionID,
		OwnerUnitID: unitID,
		RegionID:    regionID,
		Tick:        int(tick),
		Code:        events.ReasonAnchorWeightedEvent,
		Category:    events.CategoryFate,
		Payload: map[string]any{
			"unit_id":         unitID,
			"region_id":       regionID,
			"anchor_density":  density,
			"budget_permille": anchorWeightedBudgetPerMille(density),
		},
	}); err != nil {
		r.log.Debug("region-runner emit anchor-weighted event", "unit", unitID, "error", err)
	}
}

// regionThreatBaseLevel 读 region 注册表的真实 threat_level 累积值作为本区威胁度基线（替换硬编码 threatBaseLevel）。
// best-effort + flag-gated：分片关 / registry==nil / regionID 空 / region 未登记 / 读失败 → 返回 (threatBaseLevel, false)
// 回退常量基线（与本切片前等价）。返回 ok=true 表示用的是真实累积值。
func (r *Runner) regionThreatBaseLevel(ctx context.Context, regionID string) (int64, bool) {
	if !shardingEnabled() || r.registry == nil || regionID == "" {
		return threatBaseLevel, false
	}
	reg, err := r.registry.GetRegion(ctx, regionID)
	if err != nil {
		// region 未登记是常态（DistinctWakeRegions 兜底来的区可能没登记）→ Debug 即可，回退常量基线。
		r.log.Debug("region-runner read region threat level", "region", regionID, "error", err)
		return threatBaseLevel, false
	}
	return clampThreatLevel(reg.ThreatLevel), true
}

// freshnessPerMilleFor 计算同区 freshness 反扎堆千分比 [threatFreshnessMinPerMille,1000]：
// 距上次命中越近越小（更压低 base+anchor 项），refractory 窗口外 / 无最近命中记录 → 1000（不衰减）。确定性（只看 tick 差）。
//
//	Δtick=0 → threatFreshnessMinPerMille（最强压制）；Δtick≥窗口 → 1000（不压制）；窗口内线性回升。
func freshnessPerMilleFor(lastHitTick int64, tick int64, recorded bool) int {
	if !recorded {
		return 1000 // 无最近命中 → 不衰减
	}
	elapsed := tick - lastHitTick
	if elapsed < 0 {
		elapsed = 0 // 时钟回拨/乱序保护：视为刚命中
	}
	if elapsed >= threatFreshnessWindowTicks {
		return 1000 // 窗口外 → 完全恢复
	}
	// 窗口内线性回升：从 min（Δ=0）到 1000（Δ=窗口）。
	span := 1000 - threatFreshnessMinPerMille
	return threatFreshnessMinPerMille + int(int64(span)*elapsed/threatFreshnessWindowTicks)
}

// regionFreshnessPerMille 读本区进程内最近命中 tick，算出 freshness 千分比。分片关 / registry==nil / regionID 空 → 1000（不衰减）。
// 进程内态（threatRecency sync.Map：regionID→最近命中 tick），随进程重启清空——freshness 是短期 refractory 软抑制，
// 重启即恢复无害（最坏多触发一两次，破圈下限/上限仍夹紧）。零值 sync.Map 可直接用，故无需在 New 里初始化。
func (r *Runner) regionFreshnessPerMille(regionID string, tick int64) int {
	if !shardingEnabled() || r.registry == nil || regionID == "" {
		return 1000
	}
	v, ok := r.threatRecency.Load(regionID)
	if !ok {
		return freshnessPerMilleFor(0, tick, false)
	}
	return freshnessPerMilleFor(v.(int64), tick, true)
}

// recordThreatHit 记录本区在 tick 命中威胁（用于 freshness 反扎堆）。单调取最大 tick，避免乱序/并发写把记录拨回。
// 仅在分片开 + registry 时写（与读路径同门控，flag 关时不污染 map、零行为变化）。
func (r *Runner) recordThreatHit(regionID string, tick int64) {
	if !shardingEnabled() || r.registry == nil || regionID == "" {
		return
	}
	for {
		prev, loaded := r.threatRecency.Load(regionID)
		if loaded && prev.(int64) >= tick {
			return // 已有更新或相等的记录，无需回拨
		}
		if loaded {
			if r.threatRecency.CompareAndSwap(regionID, prev, tick) {
				return
			}
			continue // CAS 失败（并发改过）→ 重读重试
		}
		if _, dup := r.threatRecency.LoadOrStore(regionID, tick); !dup {
			return
		}
		// LoadOrStore 发现已被并发写入 → 回到循环按单调比较。
	}
}

// bumpRegionThreat 把一次威胁命中累计到 region.threat_level（威胁扎堆，§11.3）。best-effort + flag-gated：
// 分片关 / registry==nil / regionID 空 / region 未登记（ErrNotFound）时静默跳过——威胁累计是辅助遥测，绝不拖垮遭遇。
func (r *Runner) bumpRegionThreat(ctx context.Context, regionID string) {
	if !shardingEnabled() || r.registry == nil || regionID == "" {
		return
	}
	if _, err := r.registry.BumpThreatLevel(ctx, regionID, 1); err != nil {
		// region 未登记是常态（按 tier 列举的区才一定登记，DistinctWakeRegions 兜底来的可能没登记）→ Debug 即可。
		r.log.Debug("region-runner bump region threat", "region", regionID, "error", err)
	}
}

// situationFromRecord 把单位状态填成 decision.Situation：StrategicFork=true（撞威胁=战略岔路，触发关键节点闸），
// HP/Hunger 喂 Router 的 L1 护栏（HP<25% 危急即撤退）。HasRation 留 false——饥饿由 ambient 层处理，威胁时只关心保命。
func situationFromRecord(record unit.Record, tick int64) decision.Situation {
	return decision.Situation{
		UnitID:        record.ID,
		Tick:          int(tick),
		HP:            record.Status.HP,
		HPMax:         hpMaxForThreat,
		Hunger:        record.Status.Hunger,
		EnemyInSight:  true,
		FirstContact:  true,
		StrategicFork: true,
	}
}

// resolveSpawnThreshold 算本次唤醒的威胁阈值(‰)：分片开 + registry 时用 region.threat_level 真累积值 + freshness 反扎堆 + 锚密度；
// 否则回退 threatBaseLevel 常量基线 + 锚密度（与本切片前等价）。anchorDensity 由调用方按需查（命中区间才查，省 DB）。
func (r *Runner) resolveSpawnThreshold(ctx context.Context, regionID string, anchorDensity float64, tick int64) int {
	base, real := r.regionThreatBaseLevel(ctx, regionID)
	if !real {
		// 回退路径：与 threatSpawnPerMille(anchorDensity) 逐结果一致（base=threatBaseLevel、无 freshness 衰减）。
		return spawnPerMilleAtLevel(base, anchorDensity, 1000)
	}
	fresh := r.regionFreshnessPerMille(regionID, tick)
	return spawnPerMilleAtLevel(base, anchorDensity, fresh)
}

// maybeEncounterThreat：HOT 单位确定性 roll 威胁；命中则过 Router——HP 危急撤退保命 / 否则关键节点遭遇。
// 返回 handled=true 表示本次唤醒被威胁消耗（调用方据返回 tier 重排、不再走日常 ambient）。
// flag 关 / 非 HOT / 未命中 → handled=false（继续日常 ambient）。真遭遇仅在注入了 handler 时发生（PvE-2）；
// shadow（handler==nil）只计遥测。触发 handler 前再查一次让位，收窄「读 record 后会话刚进战斗」的并发窗口。
func (r *Runner) maybeEncounterThreat(ctx context.Context, job *agentqueue.DecisionJob, record unit.Record, currentTier scheduler.Tier, tick int64) (bool, scheduler.Tier) {
	if !r.threatsEnabled || currentTier != scheduler.TierHot {
		return false, scheduler.TierCold
	}
	// 锚加权（PvE-4）+ region.threat_level 真累积（本切片）：先抽样、仅当 draw 落在「可变阈值带」[floor,max) 才查
	// region threat_level / freshness / 锚密度——draw≥max 必不命中、draw<floor 必命中（破圈），都无需查，省掉约 92% DB 查询。
	// 关键不变量：base/freshness/anchor 调高调低只会把阈值落在 [floor,max] 内，故 draw≥max 短路必不命中、draw<floor 必命中
	// 对任意 base/freshness 都成立（破圈下限恒保留），与「直接 draw < resolveSpawnThreshold(...)」逐结果等价。
	draw := threatRoll1000(job.SessionID, job.UnitID, tick)
	if draw >= threatMaxPerMille {
		return false, scheduler.TierCold // 超过任何可能阈值 → 必不命中
	}
	// density 仅在「draw 落在可变阈值带」时才查（省 ~92% DB）；queried 标记是否已查，供命中后留痕复用、不重复查。
	density := 0.0
	queried := false
	if draw >= threatFloorPerMille { // 破圈下限以上：按 region 威胁度+freshness+锚密度阈值判定（以下则必命中）
		if r.anchorDensity != nil {
			density = r.anchorDensity(ctx, job.UnitID)
		}
		queried = true
		if draw >= r.resolveSpawnThreshold(ctx, job.RegionID, density, tick) {
			return false, scheduler.TierCold
		}
	}
	atomic.AddInt64(&r.st.threatsRolled, 1)
	// NPC 锚加权预算（§1.5）：命中的若是高锚 NPC，写 ReasonAnchorWeightedEvent 留痕「祸福偏要落在她最在意的人和地方」。
	// **只在密度已查过时留痕**（queried，即 draw 落在可变阈值带）——不为留痕额外查 DB，保住「省 ~92% DB」不变量。
	// 破圈命中（draw<floor，未查密度）天然是「零锚/低锚噪声」，本就不该被当作「聚焦到她在乎处」，跳过留痕正合语义。
	if queried && isHighAnchorDensity(density) {
		r.emitAnchorWeightedEvent(ctx, job.SessionID, job.UnitID, job.RegionID, density, tick)
	}
	// 威胁扎堆（§11.3）：命中威胁 → ① region.threat_level +1 累计（让威胁在高活跃区天然扎堆，下次刷新读到更高基线）；
	// ② 记本区最近命中 tick（freshness 反扎堆：短期内压低同区再次触发，避免一窝蜂连刷）。两者一升一抑，长期扎堆+短期错峰。
	// best-effort + flag-gated：分片关 / registry==nil / region 未登记时静默跳过，绝不影响遭遇结算。
	r.bumpRegionThreat(ctx, job.RegionID)
	r.recordThreatHit(job.RegionID, tick)

	dec := r.threatRouter.Route(situationFromRecord(record, tick))
	if dec.Intent.Action == decision.ActionFlee {
		// L1 护栏：HP 危急 → 撤退保命、不应战。本次唤醒用于规避，HOT 重排（仍紧张）。
		atomic.AddInt64(&r.st.threatsFled, 1)
		return true, scheduler.TierHot
	}

	// 关键节点（StrategicFork）→ 遭遇。
	atomic.AddInt64(&r.st.threatsEncountered, 1)
	if r.threatHandler != nil {
		if r.execGuard(job.SessionID) {
			atomic.AddInt64(&r.st.deferred, 1)
			return true, scheduler.TierHot
		}
		if err := r.threatHandler(ctx, job.SessionID, job.UnitID); err != nil {
			atomic.AddInt64(&r.st.encounterErrors, 1)
			r.log.Warn("region-runner threat encounter", "unit", job.UnitID, "error", err)
		}
	}
	return true, scheduler.TierHot
}
