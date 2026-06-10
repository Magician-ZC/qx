package session

// 文件说明：共享世界 Phase 5「统一世界推进」的集成测试（对真实 SQLite）。
// 死守两条红线：
//   ① region-runner 接管共享世界主角的离线自治推进——共享主角的唤醒队列 region_id 与 units 列对齐到复合
//      region_id=worldID#zoneID，使中心化 region-runner 按真·地理子区把共享主角纳入唤醒（离线也活、与同区别玩家 co-tick）。
//   ② 停双推进者——region-runner 启用 + 共享世界局时，AdvanceFateWorld（fate autotick 的逐 session 推进入口）对共享
//      session 直接早返回（不推），保证同一共享主角绝不被两个驱动各推一拍。私有档（world_default）/ flag 关时仍由
//      fate autotick 推进，零影响。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/agentqueue"
)

// wakeRegionOf 读某单位在唤醒队列（agent_wake_queue）里的 region_id（测试用，验证 wake 是否对齐到复合 region）。
// 返回空串表示该单位无唤醒排期。
func wakeRegionOf(t *testing.T, ctx context.Context, service *Service, regionID string, unitID string) (string, bool) {
	t.Helper()
	// ListDueWakes 按 region_id 等值 + wake_at_tick<=cur 拉，用极大 currentTick 保证到点；只看是否含该单位。
	due, err := agentqueue.ListDueWakes(ctx, service.db, "", regionID, 1<<62, 1024)
	if err != nil {
		t.Fatalf("列举 region %q 唤醒队列失败: %v", regionID, err)
	}
	for _, w := range due {
		if w.UnitID == unitID {
			return w.RegionID, true
		}
	}
	return "", false
}

// TestSharedWorldPhase5_ProtagonistWakeAlignedToSharedRegion 验证 ①（session 侧）：region-runner 启用 + 共享世界降生后，
// 共享主角的 wake 与 units 列都对齐到复合 region_id（worldID#zoneID），故 region-runner 的调度（DistinctWakeRegions→
// ListDueWakes(复合 region)）能在真·地理子区下把共享主角纳入唤醒（与同区别玩家 co-tick）。
// region-runner 真把它唤醒、跑一拍自治的端到端验证在 regionrunner 包（TestSharedWorldProtagonistWokenAndAlive），
// 此处只验 session 侧的 wake 对齐事实（region-runner 的拉取口径就是按 wake.region_id）。
func TestSharedWorldPhase5_ProtagonistWakeAlignedToSharedRegion(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "1")
	_, service := newMainWorldTestService(t)
	service.SetAmbientSchedulingEnabled(true) // 模拟 main 按 QUNXIANG_REGION_RUNNER_ENABLED 注入：region-runner 启用
	ctx := context.Background()

	view, err := service.CreateMainWorldCharacter(ctx, "p5-acc-A", MainWorldCharacterInput{Name: "甲玩家"})
	if err != nil {
		t.Fatalf("降生失败: %v", err)
	}
	wantRegion := sharedRegionID(sharedWorldID, "zone_neutral_start")

	// units 列与唤醒队列 region_id 都应是复合 region（不是 sessionID）——这是统一推进的地理事实源。
	if got := service.regionOf(ctx, view.UnitID); got != wantRegion {
		t.Fatalf("共享主角 units.region_id 应是复合 %q，得到 %q", wantRegion, got)
	}
	// 共享主角的 wake 应已对齐到复合 region（requeueSharedWorldWakeBestEffort 的核心）。
	gotWakeRegion, ok := wakeRegionOf(t, ctx, service, wantRegion, view.UnitID)
	if !ok {
		t.Fatalf("共享主角应在复合 region %q 的唤醒队列里（否则 region-runner 永不在地理子区下调度它）", wantRegion)
	}
	if gotWakeRegion != wantRegion {
		t.Fatalf("共享主角 wake.region_id 应对齐复合 %q，得到 %q", wantRegion, gotWakeRegion)
	}
	// region-runner 的 region 枚举源（DistinctWakeRegions）应能发现这个复合 region——否则它根本不会调度该区。
	regions, err := agentqueue.DistinctWakeRegions(ctx, service.db)
	if err != nil {
		t.Fatalf("DistinctWakeRegions 失败: %v", err)
	}
	foundRegion := false
	for _, r := range regions {
		if r.RegionID == wantRegion {
			foundRegion = true
			break
		}
	}
	if !foundRegion {
		t.Fatalf("region-runner 的 region 枚举源应含复合 region %q（共享主角的唤醒区）", wantRegion)
	}
	// 反向断言：它绝不应仍停在 sessionID 桶的唤醒队列里（那会被调度在「每会话自成一区」、永不与别玩家 co-tick）。
	if _, staleInSession := wakeRegionOf(t, ctx, service, view.SessionID, view.UnitID); staleInSession {
		t.Fatalf("共享主角不应再停在 sessionID=%q 的唤醒队列（wake 未对齐复合 region → 双区分裂）", view.SessionID)
	}
}

// TestSharedWorldPhase5_NoDoubleDrive 验证 ②：region-runner 启用 + 共享世界局时，AdvanceFateWorld 对共享 session
// 直接早返回（不推、advancing=false）——共享主角绝不被 fate autotick 与 region-runner 双推。
func TestSharedWorldPhase5_NoDoubleDrive(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "1")
	_, service := newMainWorldTestService(t)
	service.SetAmbientSchedulingEnabled(true) // region-runner 启用（接管共享世界推进）
	service.SetAsyncExecution(true)            // 与生产一致：异步执行
	ctx := context.Background()

	view, err := service.CreateMainWorldCharacter(ctx, "p5-acc-nodd", MainWorldCharacterInput{Name: "乙玩家"})
	if err != nil {
		t.Fatalf("降生失败: %v", err)
	}

	// 前置自检：确属共享世界局（守门判定 sharedWorldDrivenByRegionRunner 的输入成立）。
	state, _, err := service.loadSession(ctx, view.SessionID)
	if err != nil {
		t.Fatalf("载入 state 失败: %v", err)
	}
	if !service.sharedWorldDrivenByRegionRunner(&state) {
		t.Fatalf("region-runner 启用 + 共享世界局 → 应判定为「由 region-runner 推进」")
	}

	// 核心断言：fate autotick 的逐 session 推进入口对共享 session 让位——不推（advancing=false、无 error）。
	advancing, err := service.AdvanceFateWorld(ctx, view.SessionID)
	if err != nil {
		t.Fatalf("AdvanceFateWorld 共享局不应报错（best-effort 早返回），得到 %v", err)
	}
	if advancing {
		t.Fatalf("region-runner 接管时 AdvanceFateWorld 绝不应推进共享 session（双推红线），得到 advancing=true")
	}
	// 防御纵深断言：fate autotick 的扫描源只扫 world_default，绝不含共享世代 session。
	worldID, err := service.EnsureDefaultWorld(ctx)
	if err != nil {
		t.Fatalf("EnsureDefaultWorld 失败: %v", err)
	}
	ids, err := service.sessions.ListMainWorldSessionIDs(ctx, worldID)
	if err != nil {
		t.Fatalf("ListMainWorldSessionIDs 失败: %v", err)
	}
	if containsString(ids, view.SessionID) {
		t.Fatalf("fate autotick 扫描源（world_default）绝不应含共享世代 session %q", view.SessionID)
	}
	// 执行未被启动（双推会触发执行）：共享局 state 不应进入执行阶段/置 ExecutionInProgress。
	after, _, err := service.loadSession(ctx, view.SessionID)
	if err != nil {
		t.Fatalf("重载 state 失败: %v", err)
	}
	if after.ExecutionInProgress {
		t.Fatalf("让位后共享局绝不应被 AdvanceFateWorld 推进入执行（双推/45min 长锁红线）")
	}
}

// TestSharedWorldPhase5_AutotickUnarmedStillNoDoubleDrive 修 major-1 的测试偏差：上面的用例在被测 service 上手动调
// SetAmbientSchedulingEnabled(true) 才使入口守门 sharedWorldDrivenByRegionRunner 生效，**不反映** main.go 的真实接线——
// 修复前 fateSvc 从未武装该 flag，故在「唯一会循环喂 session 给 AdvanceFateWorld 的生产路径=runFateAutoTickPass」上
// 入口守门恒为 false、永不让位。本用例显式模拟「fateSvc 未武装 flag」（service.ambientSchedulingEnabled=false），
// 断言：① 入口守门此时确实不生效（sharedWorldDrivenByRegionRunner=false，即修复前的真实状态——纵深名存实亡）；
// ② 但共享主角仍**绝不**被 autotick 双推——因为 runFateAutoTickPass 的扫描源 ListMainWorldSessionIDs(world_default)
// 天然排除共享世代 session（这层 scan-exclusion 才是真正 load-bearing 的红线兜底）。
// 这样测试与生产接线不再有「全靠手动武装 flag 才绿」的偏差：纵深守门由 main.go 通电（武装态另由上面用例验证），
// scan-exclusion 由本用例独立钉死，二者各为一层。
func TestSharedWorldPhase5_AutotickUnarmedStillNoDoubleDrive(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "1")
	t.Setenv("QUNXIANG_FATE_AUTOTICK", "1") // 让 runFateAutoTickPass 真正跑扫描（非零行为早返回）
	_, service := newMainWorldTestService(t)
	// 关键：刻意**不**调 SetAmbientSchedulingEnabled——复刻修复前 main.go 里 fateSvc 的真实（未武装）状态。
	service.SetAsyncExecution(true)
	ctx := context.Background()

	view, err := service.CreateMainWorldCharacter(ctx, "p5-acc-unarmed", MainWorldCharacterInput{Name: "丁玩家"})
	if err != nil {
		t.Fatalf("降生失败: %v", err)
	}
	state, _, err := service.loadSession(ctx, view.SessionID)
	if err != nil {
		t.Fatalf("载入 state 失败: %v", err)
	}
	// ① 未武装 flag → 入口守门此时不生效（这正是修复前 autotick 生产路径的真实状态，纵深名存实亡）。
	if service.sharedWorldDrivenByRegionRunner(&state) {
		t.Fatalf("未武装 ambientSchedulingEnabled 时入口守门不应命中（本用例刻意复刻未武装态）")
	}
	// ② scan-exclusion 兜底：autotick 扫描源（world_default）绝不含共享世代 session → 即便入口守门失效也不双推。
	worldID, err := service.EnsureDefaultWorld(ctx)
	if err != nil {
		t.Fatalf("EnsureDefaultWorld 失败: %v", err)
	}
	ids, err := service.sessions.ListMainWorldSessionIDs(ctx, worldID)
	if err != nil {
		t.Fatalf("ListMainWorldSessionIDs 失败: %v", err)
	}
	if containsString(ids, view.SessionID) {
		t.Fatalf("autotick 扫描源（world_default）绝不应含共享世代 session %q（scan-exclusion 是红线真正兜底层）", view.SessionID)
	}
	// 端到端：跑一整遍 runFateAutoTickPass（flag 开 → 真扫描 + 对每个命中 session 调 AdvanceFateWorld），
	// 共享局绝不应被它推进入执行（scan-exclusion 使共享 session 根本不在被推列表里）。
	service.runFateAutoTickPass(ctx)
	after, _, err := service.loadSession(ctx, view.SessionID)
	if err != nil {
		t.Fatalf("重载 state 失败: %v", err)
	}
	if after.ExecutionInProgress {
		t.Fatalf("未武装态下共享局仍绝不应被 autotick 推进入执行（scan-exclusion 兜底失效=红线破）")
	}
}

// TestSharedWorldPhase5_PrivateStillFateAutoTicked 验证 ③：flag 关（私有档 world_default）时，
// region-runner 启用也不改变私有档行为——sharedWorldDrivenByRegionRunner 不命中，AdvanceFateWorld 仍正常推进私有档，零影响。
func TestSharedWorldPhase5_PrivateStillFateAutoTicked(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "0") // 私有档世代（world_default）
	_, service := newMainWorldTestService(t)
	service.SetAmbientSchedulingEnabled(true) // region-runner 启用：仍不应接管私有档
	service.SetAsyncExecution(true)
	ctx := context.Background()

	view, err := service.CreateMainWorldCharacter(ctx, "p5-acc-priv", MainWorldCharacterInput{Name: "丙玩家"})
	if err != nil {
		t.Fatalf("降生失败: %v", err)
	}
	if view.WorldID != defaultWorldID {
		t.Fatalf("flag 关时应绑私有世界 %q，得到 %q", defaultWorldID, view.WorldID)
	}

	state, _, err := service.loadSession(ctx, view.SessionID)
	if err != nil {
		t.Fatalf("载入 state 失败: %v", err)
	}
	// 私有档绝不被判为「由 region-runner 推进」（inSharedWorld 不成立）——双驱动让位门对它不命中。
	if service.sharedWorldDrivenByRegionRunner(&state) {
		t.Fatalf("私有档（flag 关）绝不应被判定为由 region-runner 推进")
	}

	// AdvanceFateWorld 仍正常推进私有档（advancing=true，启动一拍自治）——零影响。
	advancing, err := service.AdvanceFateWorld(ctx, view.SessionID)
	if err != nil {
		t.Fatalf("私有档 AdvanceFateWorld 失败: %v", err)
	}
	if !advancing {
		t.Fatalf("私有档应仍由 fate autotick 正常推进（advancing=true），得到 false")
	}
	// 私有档的 wake 维持 sessionID 口径（绝不被复合 region 污染）——共享世界改造对私有档零影响。
	if _, ok := wakeRegionOf(t, ctx, service, view.SessionID, view.UnitID); !ok {
		t.Fatalf("私有档主角的 wake 应仍在 sessionID=%q 口径的唤醒队列里", view.SessionID)
	}
	wantSharedRegion := sharedRegionID(sharedWorldID, "zone_neutral_start")
	if _, polluted := wakeRegionOf(t, ctx, service, wantSharedRegion, view.UnitID); polluted {
		t.Fatalf("私有档主角的 wake 绝不应被赋复合 region %q（共享改造串味）", wantSharedRegion)
	}
}
