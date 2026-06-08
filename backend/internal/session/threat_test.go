package session

// 文件说明：单人 elite 遭遇全链路集成测试（对真实 SQLite）：撞见→combat_roll 多回合→胜利分赃/失败惩罚→留痕+收件箱卡。

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/engine/status"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

// TestResolveEliteEncounter_OptimisticNoClobbersBattle 验证 PvE-3 乐观并发硬化：离线 elite 遭遇与一个无条件写者
// （模拟战斗，每次 hunger-1）并发改同一单位时，**战斗的每次写都不被遭遇覆盖**。遭遇只写 HP/wallet/morale，其
// applyEliteMutation(乐观) 冲突即重读重试、绝不用 stale 整块回写覆盖战斗的 hunger。不变量：final.Hunger == 起始-aWrites
// （镜像 real-3-0 的「A 战斗写永不被 B 离线写覆盖」）。
func TestResolveEliteEncounter_OptimisticNoClobbersBattle(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(2, "s1", "player", "她")
	actor.Status.HP = 100
	actor.Status.Hunger = 100
	actor.Status.Attack = 20
	actor.Status.Defense = 5
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 弱威胁：角色会赢、会受点伤（HP 写）、有金币掉落（wallet 写）——都是遭遇侧的乐观整块写。
	weak := Threat{ID: "t1", Name: "野狗", Tier: ThreatTierElite, Attack: 15, Defense: 5, HPPool: 40,
		Loot: []encounter.LootItem{{ID: "gold", Rarity: encounter.Common, Quantity: 15}}}
	state := State{ID: "s1"}

	const aWrites = 40
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // A：战斗——无条件 Save，每次 hunger-1（遭遇绝不写 hunger，故 hunger 只由 A 改）。
		defer wg.Done()
		for i := 0; i < aWrites; i++ {
			cur, err := repo.GetByID(ctx, actor.ID)
			if err != nil {
				t.Errorf("A get: %v", err)
				return
			}
			cur.Status.Hunger -= 1
			if err := repo.Save(ctx, cur); err != nil {
				t.Errorf("A save: %v", err)
				return
			}
		}
	}()

	// B：离线 elite 遭遇（乐观并发），与 A 并发。
	if _, err := service.ResolveEliteEncounter(ctx, &state, &actor, weak); err != nil {
		t.Fatalf("resolve elite: %v", err)
	}
	wg.Wait()

	final, err := repo.GetByID(ctx, actor.ID)
	if err != nil {
		t.Fatalf("final get: %v", err)
	}
	if final.Status.Hunger != 100-aWrites {
		t.Fatalf("战斗的 hunger 写不应被离线遭遇覆盖：期望 %d，实际 %d（PvE-3 乐观硬化失效）", 100-aWrites, final.Status.Hunger)
	}
}

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
