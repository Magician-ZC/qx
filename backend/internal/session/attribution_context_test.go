package session

// 文件说明：归因上下文构造的 DB 集成测试——验证关系四轴被归一化填入快照、缺数据时优雅降级。

import (
	"context"
	"path/filepath"
	"testing"

	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

func TestBuildDecisionAttributionContext_Relations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attr.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repo := unit.NewRepository(db)
	ctx := context.Background()
	source := unit.BootstrapRecord(2, "s1", "player", "甲")
	source.Personality.Aggression = 0.9
	target := unit.BootstrapRecord(4, "s1", "player", "乙")
	if err := repo.Save(ctx, source); err != nil {
		t.Fatalf("保存 source 失败: %v", err)
	}
	if err := repo.Save(ctx, target); err != nil {
		t.Fatalf("保存 target 失败: %v", err)
	}
	// 关系四轴在 [-10,10] 量级，构造一条强关系。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry) VALUES (?, ?, ?, ?, ?, ?)`,
		source.ID, target.ID, 3.0, 1.0, 8.0, 0.0,
	); err != nil {
		t.Fatalf("插入关系失败: %v", err)
	}

	service := &Service{db: db}
	snap, _ := service.buildDecisionAttributionContext(ctx, State{}, &source)

	axes, ok := snap.Relations[target.ID]
	if !ok {
		t.Fatalf("关系应被载入快照")
	}
	// 归一化：/10。
	if axes.Affection != 0.8 || axes.Trust != 0.3 || axes.Fear != 0.1 {
		t.Fatalf("关系四轴归一化错误：%+v", axes)
	}
	// 人格与压力仍在。
	if snap.Traits["aggression"] != 0.9 {
		t.Fatalf("人格维应保留，得到 %v", snap.Traits["aggression"])
	}
}

func TestBuildDecisionAttributionContext_GracefulEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attr2.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	service := &Service{db: db}
	actor := unit.BootstrapRecord(6, "s1", "player", "丙")
	snap, block := service.buildDecisionAttributionContext(context.Background(), State{}, &actor)

	if block != "" {
		t.Fatalf("无记忆时不应有可引用 ID 块，得到 %q", block)
	}
	if len(snap.Relations) != 0 {
		t.Fatalf("无关系时 Relations 应为空，得到 %d", len(snap.Relations))
	}
	if len(snap.Traits) != 8 {
		t.Fatalf("人格维应始终填充，得到 %d", len(snap.Traits))
	}
}
