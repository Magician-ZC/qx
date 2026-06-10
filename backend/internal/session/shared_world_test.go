package session

// 文件说明：共享世界 Phase 1（共享几何=共享种子）的集成测试（对真实 SQLite）。
// 覆盖核心不变量：
//   - flag 开（QUNXIANG_SHARED_WORLD=1）：两个**不同账号**降生进同一共享世界 world_shared_v1，
//     拿到**逐格相同**的 Zones 地图（确定性：同 RegionSeed 派生同种子 → GenerateWorld 逐格一致）。
//   - flag 关（默认）：两个账号各自独立私有世界（world_default），Zones 几何**不同**（回归：旧行为不变）。
//   - 旧档隔离：共享世界用 world_shared_v1 世代，与 world_default 物理隔离（不同 world_id）。
//   - EnsureSharedWorld 幂等：RegionSeed 非空且固定。
//   - deriveSharedSeed 确定性：同 RegionSeed 同种子。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/world"
)

// zoneTerrainFingerprint 把一组 Zones 投影成「逐格地形指纹」字符串：仅取确定性字段
// （ZoneID + 每格 Coord/Terrain/RegionID/Landmark），**刻意剔除** MapSnapshot.ID / GeneratedAt
// （二者含 time.Now() 墙钟，同种子也不同，不代表世界几何）。两份指纹相等 ⇔ 世界逐格相同。
func zoneTerrainFingerprint(zones []world.Zone) string {
	var b []byte
	for _, z := range zones {
		b = append(b, z.ID...)
		b = append(b, '|')
		b = append(b, z.FactionID...)
		b = append(b, '|')
		for _, tile := range z.Map.Tiles {
			b = append(b, byte(tile.Coord.Q), byte(tile.Coord.R))
			b = append(b, tile.Terrain...)
			b = append(b, '#')
			b = append(b, tile.RegionID...)
			b = append(b, '#')
			b = append(b, tile.Landmark...)
			b = append(b, ';')
		}
		b = append(b, '\n')
	}
	return string(b)
}

// loadZoneFingerprintForAccount 降生一个账号、载入其 session 的 Zones，返回 (worldID, 逐格指纹)。
func loadZoneFingerprintForAccount(ctx context.Context, t *testing.T, service *Service, accountID, name string) (string, string) {
	t.Helper()
	view, err := service.CreateMainWorldCharacter(ctx, accountID, MainWorldCharacterInput{Name: name})
	if err != nil {
		t.Fatalf("降生失败 (account=%s): %v", accountID, err)
	}
	state, _, err := service.loadSession(ctx, view.SessionID)
	if err != nil {
		t.Fatalf("载入 session 失败 (account=%s): %v", accountID, err)
	}
	if len(state.Zones) == 0 {
		t.Fatalf("session 应有 Zones 投影 (account=%s)", accountID)
	}
	return view.WorldID, zoneTerrainFingerprint(state.Zones)
}

// TestSharedWorld_SameSeedSameGeometry 验证 flag 开时：两个不同账号降生拿到逐格相同的共享世界。
func TestSharedWorld_SameSeedSameGeometry(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "1")
	// 关掉村庄/出生 NPC 等 best-effort 副作用对 Zones 几何无影响，无需特意关；只验 Zones 本身。
	_, service := newMainWorldTestService(t)
	ctx := context.Background()

	worldA, fpA := loadZoneFingerprintForAccount(ctx, t, service, "shared-acc-A", "甲")
	worldB, fpB := loadZoneFingerprintForAccount(ctx, t, service, "shared-acc-B", "乙")

	// 两账号都落进共享世界世代 world_shared_v1（与旧 world_default 物理隔离）。
	if worldA != sharedWorldID || worldB != sharedWorldID {
		t.Fatalf("flag 开时两账号应都绑共享世界 %q，得到 A=%q B=%q", sharedWorldID, worldA, worldB)
	}
	if worldA == defaultWorldID || worldB == defaultWorldID {
		t.Fatalf("共享世界角色绝不应落进旧私有世代 world_default（旧档隔离）")
	}
	// 核心不变量：同 RegionSeed 派生同种子 → 世界逐格相同。
	if fpA != fpB {
		t.Fatalf("flag 开时两账号应拿到逐格相同的共享世界，但地形指纹不同\nA=%d 字节\nB=%d 字节", len(fpA), len(fpB))
	}
}

// TestSharedWorld_OffKeepsPrivatePerPlayerWorlds 验证 flag 关（默认）时：回归到各账号独立私有世界。
func TestSharedWorld_OffKeepsPrivatePerPlayerWorlds(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "0")
	_, service := newMainWorldTestService(t)
	ctx := context.Background()

	worldA, fpA := loadZoneFingerprintForAccount(ctx, t, service, "priv-acc-A", "丙")
	worldB, fpB := loadZoneFingerprintForAccount(ctx, t, service, "priv-acc-B", "丁")

	// 旧行为：两账号都绑旧私有世代 world_default。
	if worldA != defaultWorldID || worldB != defaultWorldID {
		t.Fatalf("flag 关时两账号应都绑旧私有世界 %q，得到 A=%q B=%q", defaultWorldID, worldA, worldB)
	}
	// 各自独立世界：种子=各自 now.UnixNano()，世界几何**不同**（回归保证，绝无意外共享）。
	if fpA == fpB {
		t.Fatalf("flag 关时两账号应各自独立世界（不同种子），但地形指纹相同——私有世界被意外共享了")
	}
}

// TestEnsureSharedWorld_IdempotentFixedSeed 验证 EnsureSharedWorld 幂等 + RegionSeed 非空且固定。
func TestEnsureSharedWorld_IdempotentFixedSeed(t *testing.T) {
	_, service := newMainWorldTestService(t)
	ctx := context.Background()

	id1, seed1, err := service.EnsureSharedWorld(ctx)
	if err != nil {
		t.Fatalf("首次 EnsureSharedWorld 失败: %v", err)
	}
	if id1 != sharedWorldID {
		t.Fatalf("EnsureSharedWorld 应返回 %q，得到 %q", sharedWorldID, id1)
	}
	if seed1 == "" {
		t.Fatalf("共享世界 RegionSeed 不应为空（共享几何的种子根）")
	}
	// 幂等：二次调用返回同 ID + 同 RegionSeed（不会重建、不会改种子）。
	id2, seed2, err := service.EnsureSharedWorld(ctx)
	if err != nil {
		t.Fatalf("二次 EnsureSharedWorld 失败: %v", err)
	}
	if id2 != id1 || seed2 != seed1 {
		t.Fatalf("EnsureSharedWorld 应幂等，得到 (%q,%q) vs (%q,%q)", id1, seed1, id2, seed2)
	}
	// DB 行确实持久化了 RegionSeed（落库，重启后存活）。
	w, err := world.Get(ctx, service.db, sharedWorldID)
	if err != nil {
		t.Fatalf("Get 共享世界失败: %v", err)
	}
	if w.RegionSeed != seed1 {
		t.Fatalf("共享世界行的 RegionSeed 应=%q，得到 %q", seed1, w.RegionSeed)
	}
}

// TestDeriveSharedSeed_Deterministic 验证 deriveSharedSeed 确定性：同 RegionSeed → 同 int64 种子，
// 且不同 RegionSeed 通常派生不同种子（确定性随机的基本性质）。
func TestDeriveSharedSeed_Deterministic(t *testing.T) {
	if deriveSharedSeed(sharedWorldGenesisSeed) != deriveSharedSeed(sharedWorldGenesisSeed) {
		t.Fatalf("deriveSharedSeed 应确定性：同输入同输出")
	}
	if deriveSharedSeed("seed-a") == deriveSharedSeed("seed-b") {
		t.Fatalf("不同 RegionSeed 应派生不同种子（FNV-64a）")
	}
	// 同种子 → GenerateWorld 逐格相同（共享几何的根保证）。
	s := deriveSharedSeed(sharedWorldGenesisSeed)
	if zoneTerrainFingerprint(world.GenerateWorld(s)) != zoneTerrainFingerprint(world.GenerateWorld(s)) {
		t.Fatalf("同种子 GenerateWorld 应逐格相同")
	}
}
