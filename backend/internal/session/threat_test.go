package session

// 文件说明：单人 elite 遭遇全链路集成测试（对真实 SQLite）：撞见→combat_roll 多回合→胜利分赃/失败惩罚→留痕+收件箱卡。

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/engine/status"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

func newThreatTestService(t *testing.T) (*sql.DB, *unit.Repository, *Service) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "threat.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := unit.NewRepository(db)
	service := &Service{db: db, units: repo, mutator: status.NewMutator(db, repo)}
	return db, repo, service
}

func eventCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("统计 events 失败: %v", err)
	}
	return n
}

func TestResolveEliteEncounter_Victory(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(2, "s1", "player", "阿采")
	actor.Status.Attack = 30
	actor.Status.Defense = 5
	actor.Status.HP = 100
	actor.Status.Wallet = 100
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("保存角色失败: %v", err)
	}

	threat := Threat{
		ID: "t_shanxiao", Name: "山魈", Tier: ThreatTierElite, RegionID: "r1",
		Power: 120, Attack: 5, Defense: 5, HPPool: 60, Severity: 40,
		Loot: []encounter.LootItem{{ID: "gold", Rarity: encounter.Common, Quantity: 20}},
	}
	state := State{ID: "s1"}

	res, err := service.ResolveEliteEncounter(ctx, &state, &actor, threat)
	if err != nil {
		t.Fatalf("遭遇出错: %v", err)
	}
	if res.Outcome != "defeated" {
		t.Fatalf("强角色应击败弱精英，得到 %q（rounds=%d dealt=%d）", res.Outcome, res.Rounds, res.DamageDealt)
	}
	if res.DamageDealt < threat.HPPool {
		t.Fatalf("造成伤害应≥威胁血量，得到 %d", res.DamageDealt)
	}
	if len(res.Awards) == 0 {
		t.Fatalf("胜利应有分赃")
	}
	// 落库：钱包 +20，HP 受了点伤但活着。
	reloaded, _ := repo.GetByID(ctx, actor.ID)
	if reloaded.Status.Wallet != 120 {
		t.Fatalf("钱包应 +20=120，得到 %d", reloaded.Status.Wallet)
	}
	if reloaded.Status.HP <= 0 || reloaded.Status.HP >= 100 {
		t.Fatalf("应受了些伤但活着，HP=%d", reloaded.Status.HP)
	}
	if eventCount(t, db) == 0 {
		t.Fatalf("应有战斗/分赃留痕")
	}
	if res.InboxCard == "" || !contains(res.InboxCard, "山魈") || !contains(res.InboxCard, "铜钱") {
		t.Fatalf("收件箱卡应是祖魂语气并含威胁名与战利品：%q", res.InboxCard)
	}
	// 状态卡应可在 state.Logs 里查到这次遭遇。
	if len(state.Logs) == 0 {
		t.Fatalf("应写入一条遭遇日志")
	}
}

func TestResolveEliteEncounter_Defeat(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(4, "s1", "player", "弱者")
	actor.Status.Attack = 2
	actor.Status.Defense = 5
	actor.Status.HP = 100
	actor.Status.Morale = 0.7
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("保存角色失败: %v", err)
	}

	threat := Threat{
		ID: "t_bear", Name: "巨熊", Tier: ThreatTierElite, RegionID: "r1",
		Power: 600, Attack: 50, Defense: 10, HPPool: 300, Severity: 90,
	}
	state := State{ID: "s1"}

	res, err := service.ResolveEliteEncounter(ctx, &state, &actor, threat)
	if err != nil {
		t.Fatalf("遭遇出错: %v", err)
	}
	if res.Outcome == "defeated" {
		t.Fatalf("弱角色不应击败强精英，得到 %q", res.Outcome)
	}
	// elite 失败候选层=1，经分级闸恒落「可恢复」：士气重挫，绝不阵亡。
	if res.PenaltyLayer != 1 {
		t.Fatalf("elite 失败应落后果分级层1，得到 %d", res.PenaltyLayer)
	}
	reloaded, _ := repo.GetByID(ctx, actor.ID)
	if reloaded.Status.Morale >= 0.7 {
		t.Fatalf("失败应令士气下降，得到 %v", reloaded.Status.Morale)
	}
	if reloaded.Status.LivesRemaining <= 0 {
		t.Fatalf("elite 失败绝不应让角色阵亡（D0-D3 硬锁），lives=%d", reloaded.Status.LivesRemaining)
	}
	if res.InboxCard == "" || !contains(res.InboxCard, "巨熊") {
		t.Fatalf("收件箱卡应是祖魂语气并含威胁名：%q", res.InboxCard)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
