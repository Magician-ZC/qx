package session

// 文件说明：成本/单位经济仪表盘聚合测试（对真实 SQLite）。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/unit"
)

func TestCostDashboard_Aggregates(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	ins := func(sessionID, provider string, cost float64, tokens int, fallback bool) {
		it := LLMInteraction{ID: uuid.NewString(), Provider: provider, EstimatedCost: cost, TotalTokens: tokens, UsedFallback: fallback}
		raw, _ := json.Marshal(it)
		if _, err := db.ExecContext(ctx, `INSERT INTO llm_interactions (id, session_id, interaction_json) VALUES (?,?,?)`, it.ID, sessionID, string(raw)); err != nil {
			t.Fatalf("insert interaction: %v", err)
		}
	}
	// s1：openai 两次（真调用），s2：deepseek 一次 + rules fallback 一次。
	ins("s1", "openai", 0.002, 100, false)
	ins("s1", "openai", 0.003, 150, false)
	ins("s2", "deepseek", 0.001, 80, false)
	ins("s2", "rules", 0.0, 0, true)

	// 单位经济：两 active + 一 dead。
	for i, ls := range []string{unit.LifeStateActive, unit.LifeStateActive, unit.LifeStateDead} {
		rec := unit.BootstrapRecord(int64(i)+1, "s1", "player", "u")
		rec.Status.LifeState = ls
		if err := repo.Save(ctx, rec); err != nil {
			t.Fatalf("save unit: %v", err)
		}
	}

	d, err := service.CostDashboard(ctx, 0, time.Now()) // 0=全量
	if err != nil {
		t.Fatalf("CostDashboard: %v", err)
	}
	if d.TotalInteractions != 4 {
		t.Fatalf("总交互应 4，得 %d", d.TotalInteractions)
	}
	if want := 0.002 + 0.003 + 0.001; d.TotalCostUSD < want-1e-9 || d.TotalCostUSD > want+1e-9 {
		t.Fatalf("总成本应 %.4f，得 %.4f", want, d.TotalCostUSD)
	}
	if d.TotalTokens != 330 {
		t.Fatalf("总 token 应 330，得 %d", d.TotalTokens)
	}
	if d.DistinctSessions != 2 {
		t.Fatalf("活跃会话（MAU 代理）应 2，得 %d", d.DistinctSessions)
	}
	if d.FallbackCount != 1 {
		t.Fatalf("fallback 应 1，得 %d", d.FallbackCount)
	}
	if d.ByProvider["openai"].Calls != 2 || d.ByProvider["openai"].CostUSD < 0.005-1e-9 {
		t.Fatalf("openai 应 2 次/0.005，得 %+v", d.ByProvider["openai"])
	}
	if d.UnitsTotal != 3 || d.UnitsByLifeState["active"] != 2 || d.UnitsByLifeState["dead"] != 1 {
		t.Fatalf("单位经济应 total=3 active=2 dead=1，得 total=%d %+v", d.UnitsTotal, d.UnitsByLifeState)
	}
}
