package session

// 文件说明：共享世界 Phase 0「跨玩家投递 seam」回归测试。
// 验证 WorldizeDeath 把死讯投给**与逝者不在同一 session**的哀悼者时，用的是**哀悼者自己所在 session**
// （units.session_id 反查），而非逝者的 session——否则 SurfaceFateEvent 会用逝者那局的 state 取哀悼者的
// 离线宪章红线锚/在世天数/provenance（致用 A 的红线判 B 的事件、OOC 误判/归因门误拒）。
//
// 断言落点：哀悼者的命运卡（events.actor_unit_id=哀悼者）其 events.session_id 必须=哀悼者自己的 session。

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

// newSeamTestService 起一个带 sessions 仓库的 Service（loadStateForFate 需要它才能取哀悼者宪章红线锚）。
func newSeamTestService(t *testing.T) (*sql.DB, *unit.Repository, *Service) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := unit.NewRepository(db)
	service := &Service{
		db:                db,
		sessions:          NewRepository(db),
		units:             repo,
		mutator:           status.NewMutator(db, repo),
		memoryRefreshTurn: map[string]int{},
		memoryRecallTurn:  map[string]int{},
	}
	return db, repo, service
}

// sessionIDOfMournerCard 查某哀悼者的命运卡（高光/待决策）落库时记的 session_id。
func sessionIDOfMournerCard(ctx context.Context, t *testing.T, db *sql.DB, mournerID string) string {
	t.Helper()
	var sess sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT session_id FROM events
		 WHERE actor_unit_id = ? AND reason_code IN (?, ?)
		 ORDER BY occurred_at DESC LIMIT 1`,
		mournerID, string(events.ReasonInboxHighlight), string(events.ReasonPendingDecision),
	).Scan(&sess)
	if err != nil {
		t.Fatalf("查哀悼者命运卡 session_id 失败: %v", err)
	}
	return sess.String
}

// TestWorldizeDeath_CrossSessionMournerUsesOwnSession 是 Phase 0 seam 的核心回归：
// 逝者在 sDead、哀悼者在 sMourner（不同 session）；死讯投给哀悼者的命运卡应落在 **sMourner**，不是 sDead。
func TestWorldizeDeath_CrossSessionMournerUsesOwnSession(t *testing.T) {
	db, repo, service := newSeamTestService(t)
	ctx := context.Background()

	// 逝者属 sDead；哀悼者属 sMourner（跨 session，模拟共享世界里两个玩家的角色成边）。
	fallen := unit.BootstrapRecord(4, "sDead", "player", "老吴")
	mourner := unit.BootstrapRecord(2, "sMourner", "player", "阿采")
	for _, r := range []unit.Record{fallen, mourner} {
		if err := repo.Save(ctx, r); err != nil {
			t.Fatalf("save unit: %v", err)
		}
	}
	// 阿采深爱老吴（强羁绊 → 死讯必进高光/待决策，非自治）。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry) VALUES (?, ?, ?, ?, ?, ?)`,
		mourner.ID, fallen.ID, 8.0, 0.0, 9.0, 0.0,
	); err != nil {
		t.Fatalf("insert relation: %v", err)
	}
	// 哀悼者所在 session 落库（带其离线宪章红线——loadStateForFate(sMourner) 据此取哀悼者自己的红线锚）。
	mournerState := State{ID: "sMourner", PlayerUnitIDs: []string{mourner.ID}}
	SetUnitCharter(&mournerState, mourner.ID, OfflineCharter{
		Redlines: []CharterRedline{{Text: "绝不放下故人", Severity: "hard"}},
	})
	if err := service.sessions.Save(ctx, &mournerState); err != nil {
		t.Fatalf("save mourner session: %v", err)
	}

	// 用逝者的 session 调 WorldizeDeath（生产里就是逝者那局触发死讯）。
	surfaced, err := service.WorldizeDeath(ctx, "sDead", fallen, "")
	if err != nil {
		t.Fatalf("worldize death: %v", err)
	}
	if surfaced != 1 {
		t.Fatalf("应惊动 1 个跨 session 哀悼者，得到 %d", surfaced)
	}

	// 核心断言：哀悼者的命运卡落在**哀悼者自己的 session**（sMourner），而非逝者的 sDead。
	if got := sessionIDOfMournerCard(ctx, t, db, mourner.ID); got != "sMourner" {
		t.Fatalf("跨 session 哀悼者的命运卡应落在其自己 session sMourner，得到 %q（seam 未修则会是 sDead）", got)
	}

	// 哀悼者能在自己收件箱看到死讯。
	inbox, _ := service.OpenFateInbox(ctx, mourner.ID)
	if len(inbox) != 1 || !contains(inbox[0].Narrative, "老吴") {
		t.Fatalf("跨 session 哀悼者收件箱应有老吴之死，得到 %+v", inbox)
	}
}
