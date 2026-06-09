// 文件说明：赛季骨架的 DB 集成测试（创建建世界+落 seasons、收尾回流存活成员+封存世界+幂等）。
package liveops

import (
	"context"
	"errors"
	"sync"
	"testing"

	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/world"
)

// fakeArchiver 记录每个被回流的角色，用于断言「存活成员都回流了名人堂」。
type fakeArchiver struct {
	mu       sync.Mutex
	archived []string
	failFor  map[string]bool // 指定哪些角色回流时报错（验证 best-effort 不阻断）
}

func (f *fakeArchiver) ArchiveCharacterToHall(_ context.Context, _, characterID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFor[characterID] {
		return errors.New("archive boom")
	}
	f.archived = append(f.archived, characterID)
	return nil
}

func TestCreateSeason_CreatesWorldAndRow(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	svc := NewService(db)

	se, err := svc.CreateSeason(ctx, CreateSeasonInput{Name: "开元一季", MaxPopulation: 500, RegionSeed: "seed-9"})
	if err != nil {
		t.Fatalf("创建赛季失败: %v", err)
	}
	if se.ID == "" || se.WorldID == "" || se.Status != SeasonActive {
		t.Fatalf("赛季字段不符: %+v", se)
	}
	// 世界应已建好且 active。
	w, err := world.Get(ctx, db, se.WorldID)
	if err != nil {
		t.Fatalf("取赛季世界失败: %v", err)
	}
	if w.Status != world.StatusActive || w.MaxPopulation != 500 {
		t.Fatalf("赛季世界字段不符: %+v", w)
	}
}

func TestFinalizeSeason_ArchivesSurvivorsAndSeals(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	arch := &fakeArchiver{failFor: map[string]bool{"u3": true}}
	svc := NewService(db).WithArchiver(arch)

	se, err := svc.CreateSeason(ctx, CreateSeasonInput{Name: "落幕季"})
	if err != nil {
		t.Fatalf("创建赛季失败: %v", err)
	}
	// 接入三名成员（u3 回流会失败，验证 best-effort）。
	for _, id := range []string{"u1", "u2", "u3"} {
		if err := world.Join(ctx, db, se.WorldID, id, "", dbdialect.DialectSQLite); err != nil {
			t.Fatalf("接入 %s 失败: %v", id, err)
		}
	}

	res, err := svc.FinalizeSeason(ctx, se.ID)
	if err != nil {
		t.Fatalf("收尾失败: %v", err)
	}
	if res.MembersTotal != 3 {
		t.Fatalf("成员总数应为 3，得到 %d", res.MembersTotal)
	}
	if res.Archived != 2 {
		t.Fatalf("应回流 2 人（u3 失败），得到 %d", res.Archived)
	}
	if len(res.ArchiveErrors) != 1 {
		t.Fatalf("应记 1 条回流失败，得到 %d: %v", len(res.ArchiveErrors), res.ArchiveErrors)
	}
	if !res.Sealed {
		t.Fatalf("世界应已封存")
	}
	// 世界应封存、赛季应 finalized。
	w, _ := world.Get(ctx, db, se.WorldID)
	if w.Status != world.StatusSealed {
		t.Fatalf("世界应 sealed，得到 %s", w.Status)
	}
	got, _ := svc.GetSeason(ctx, se.ID)
	if got.Status != SeasonFinalized {
		t.Fatalf("赛季应 finalized，得到 %s", got.Status)
	}
}

func TestFinalizeSeason_Idempotent(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	arch := &fakeArchiver{}
	svc := NewService(db).WithArchiver(arch)
	se, _ := svc.CreateSeason(ctx, CreateSeasonInput{Name: "幂等季"})
	_ = world.Join(ctx, db, se.WorldID, "u1", "", dbdialect.DialectSQLite)

	if _, err := svc.FinalizeSeason(ctx, se.ID); err != nil {
		t.Fatalf("首次收尾失败: %v", err)
	}
	firstCount := len(arch.archived)
	// 二次收尾应幂等：不重复回流。
	res, err := svc.FinalizeSeason(ctx, se.ID)
	if err != nil {
		t.Fatalf("二次收尾失败: %v", err)
	}
	if len(arch.archived) != firstCount {
		t.Fatalf("二次收尾不应重复回流，回流总数 %d -> %d", firstCount, len(arch.archived))
	}
	if !res.Sealed {
		t.Fatalf("幂等收尾仍应报告已封存")
	}
}

func TestFinalizeSeason_NoArchiverStillSeals(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	svc := NewService(db) // 未注入 archiver
	se, _ := svc.CreateSeason(ctx, CreateSeasonInput{Name: "无归档季"})
	_ = world.Join(ctx, db, se.WorldID, "u1", "", dbdialect.DialectSQLite)

	res, err := svc.FinalizeSeason(ctx, se.ID)
	if err != nil {
		t.Fatalf("收尾失败: %v", err)
	}
	if res.Archived != 0 || !res.Sealed {
		t.Fatalf("无 archiver 应零回流但仍封存: %+v", res)
	}
	w, _ := world.Get(ctx, db, se.WorldID)
	if w.Status != world.StatusSealed {
		t.Fatalf("世界应封存")
	}
}

func TestGetSeason_NotFound(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	svc := NewService(db)
	if _, err := svc.GetSeason(ctx, "nope"); !errors.Is(err, ErrSeasonNotFound) {
		t.Fatalf("不存在赛季应返回 ErrSeasonNotFound，得到 %v", err)
	}
}
