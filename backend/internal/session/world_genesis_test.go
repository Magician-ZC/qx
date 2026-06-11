package session

// 文件说明：创世序章（模块1）入史的集成测试——flag 开时写一条卷首楔子（含世界名、三阵营、舞台让给后人）、
// 幂等只写一条（重复调用不重复写）、flag 关时 no-op、空 worldID no-op；以及世界名表的确定性。
// 复用 threat_test.go 的 newThreatTestService(t) 临时 SQLite Service helper。

import (
	"context"
	"strings"
	"testing"

	"qunxiang/backend/internal/world"
)

// withWorldGenesisFlag 在测试期临时开启 QUNXIANG_WORLD_GENESIS（t.Setenv 自动在用例结束还原）。
func withWorldGenesisFlag(t *testing.T, on bool) {
	t.Helper()
	if on {
		t.Setenv("QUNXIANG_WORLD_GENESIS", "on")
	} else {
		t.Setenv("QUNXIANG_WORLD_GENESIS", "false") // 显式关（默认已开）
	}
}

func TestEnsureWorldGenesis_WritesOnceWithWorldName(t *testing.T) {
	withWorldGenesisFlag(t, true)
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	wid := "w_genesis"

	id := service.ensureWorldGenesis(ctx, wid)
	if id == "" {
		t.Fatal("flag 开时创世序章应写入一条并返回 id")
	}

	feed, err := service.WorldChronicleFeedByWorldID(ctx, wid, 0)
	if err != nil {
		t.Fatalf("读世界编年史失败: %v", err)
	}
	if len(feed.Entries) != 1 {
		t.Fatalf("应有 1 条序章，得 %d", len(feed.Entries))
	}
	e := feed.Entries[0]
	if e.Category != WorldChronicleGenesis {
		t.Fatalf("category 应为 genesis，得 %q", e.Category)
	}
	if e.Era != worldGenesisEra {
		t.Fatalf("era 应为 %q，得 %q", worldGenesisEra, e.Era)
	}
	if e.WorldTick != worldGenesisWorldTick {
		t.Fatalf("world_tick 应为 %d，得 %d", worldGenesisWorldTick, e.WorldTick)
	}
	if e.Importance != worldGenesisImportance {
		t.Fatalf("importance 应为 %d，得 %d", worldGenesisImportance, e.Importance)
	}
	// 文案须含确定性世界名 + 三阵营关键词 + 末句把舞台让给后人。
	name := world.WorldDisplayName(wid)
	if !strings.Contains(e.NarrativeZH, name) {
		t.Fatalf("序章应含世界名 %q，得 %q", name, e.NarrativeZH)
	}
	for _, kw := range []string{"自由", "秩序", "混乱", "后人亲历"} {
		if !strings.Contains(e.NarrativeZH, kw) {
			t.Fatalf("序章应含关键词 %q，得 %q", kw, e.NarrativeZH)
		}
	}
}

func TestEnsureWorldGenesis_Idempotent(t *testing.T) {
	withWorldGenesisFlag(t, true)
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	wid := "w_genesis_idem"

	if id := service.ensureWorldGenesis(ctx, wid); id == "" {
		t.Fatal("首次应写入序章")
	}
	// 重复调用：已有 genesis → 不重复写、返回空串。
	if id := service.ensureWorldGenesis(ctx, wid); id != "" {
		t.Fatalf("重复调用应幂等 no-op 返回空 id，得 %q", id)
	}
	if id := service.ensureWorldGenesis(ctx, wid); id != "" {
		t.Fatalf("第三次调用仍应幂等 no-op，得 %q", id)
	}

	feed, err := service.WorldChronicleFeedByWorldID(ctx, wid, 0)
	if err != nil {
		t.Fatalf("读世界编年史失败: %v", err)
	}
	if len(feed.Entries) != 1 {
		t.Fatalf("幂等：多次调用后仍应只有 1 条序章，得 %d", len(feed.Entries))
	}
}

func TestEnsureWorldGenesis_FlagOffNoOp(t *testing.T) {
	withWorldGenesisFlag(t, false)
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	wid := "w_genesis_off"

	if id := service.ensureWorldGenesis(ctx, wid); id != "" {
		t.Fatalf("flag 关时应 no-op 返回空 id，得 %q", id)
	}
	feed, err := service.WorldChronicleFeedByWorldID(ctx, wid, 0)
	if err != nil {
		t.Fatalf("读世界编年史失败: %v", err)
	}
	if len(feed.Entries) != 0 {
		t.Fatalf("flag 关时不应写任何条目，得 %d", len(feed.Entries))
	}
}

func TestEnsureWorldGenesis_EmptyWorldIDNoOp(t *testing.T) {
	withWorldGenesisFlag(t, true)
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	// 空 worldID（旧单图档/未接多世界）：即便 flag 开也应 no-op。
	if id := service.ensureWorldGenesis(ctx, ""); id != "" {
		t.Fatalf("空 worldID 应 no-op 返回空 id，得 %q", id)
	}
}

func TestWorldDisplayName_Deterministic(t *testing.T) {
	// 空 worldID 回落「无名之境」。
	if got := world.WorldDisplayName(""); got != "无名之境" {
		t.Fatalf("空 worldID 应回落「无名之境」，得 %q", got)
	}
	// 非空 worldID：同 ID 恒得同名（确定性、跨调用稳定）。
	a := world.WorldDisplayName("world_shared_v1")
	b := world.WorldDisplayName("world_shared_v1")
	if a != b {
		t.Fatalf("同 worldID 应得同名（确定性），得 %q vs %q", a, b)
	}
	if strings.TrimSpace(a) == "" {
		t.Fatal("非空 worldID 应得非空世界名")
	}
}
