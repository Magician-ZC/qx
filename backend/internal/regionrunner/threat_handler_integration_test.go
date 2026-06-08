package regionrunner

// 文件说明：PvE-2 真触发端到端集成测试。在 regionrunner 包内（可用私有 firstTick/withClock/processOne 确定性定位命中 tick），
// 注入**真** session.Service.TriggerEliteEncounter 作 threatHandler，验证 region-runner 唤醒 HOT 单位 → 命中威胁 →
// 真跑 elite 遭遇（多回合 combat_roll + Mutator 改 HP，全 LLM-free）。regionrunner 不被 session import，故此 test 引 session 无环。

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"qunxiang/backend/internal/agentqueue"
	"qunxiang/backend/internal/session"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

func TestThreatHandlerRunsRealEliteEncounter(t *testing.T) {
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "pve2.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	// 真 session.Service（nil llm 可——elite 遭遇 LLM-free）。
	svc := session.NewService(db, nil)

	hit := firstTick("s1", "u1", true)
	r := New(db, Config{TickSeconds: 1, Apply: true, Threats: true}, nil).withClock(func() time.Time { return time.Unix(hit, 0).UTC() })
	// 注入真 elite 遭遇结算（main 同款包装）。
	r.SetThreatHandler(func(ctx context.Context, sessionID, unitID string) error {
		_, err := svc.TriggerEliteEncounter(ctx, sessionID, unitID)
		return err
	})
	tick := r.currentTick()

	// 一名有战斗力的 HOT 玩家单位。
	repo := unit.NewRepository(db)
	rec := unit.BootstrapRecord(7, "s1", "player", "她")
	rec.ID = "u1"
	rec.Status.HP = 100
	rec.Status.Attack = 25 // 能一战但会受伤
	rec.Status.Defense = 5
	rec.Status.LifeState = unit.LifeStateActive
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("保存单位失败: %v", err)
	}
	if err := repo.TouchLastActiveTick(ctx, "u1", tick); err != nil { // 设为 HOT
		t.Fatalf("touch: %v", err)
	}
	if _, err := agentqueue.EnqueueJob(ctx, db, agentqueue.DecisionJob{UnitID: "u1", SessionID: "s1", RegionID: "s1", Tick: tick}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if _, err := r.processOne(ctx); err != nil {
		t.Fatalf("processOne: %v", err)
	}

	// 真遭遇执行：encountered=1 且 handler 无错。
	if r.Stats()["threats_encountered"].(int64) != 1 {
		t.Fatalf("应真触发一次 elite 遭遇，得到 encountered=%v", r.Stats()["threats_encountered"])
	}
	if r.Stats()["encounter_errors"].(int64) != 0 {
		t.Fatalf("真遭遇不应报错，得到 encounter_errors=%v", r.Stats()["encounter_errors"])
	}
	// 真遭遇有真效果：多回合消耗战让单位受伤（HP<100）。
	final, err := repo.GetByID(ctx, "u1")
	if err != nil {
		t.Fatalf("读单位失败: %v", err)
	}
	if final.Status.HP >= 100 {
		t.Fatalf("真 elite 遭遇应让单位受伤 HP<100，得到 %d（疑似 handler 未真打）", final.Status.HP)
	}
}
