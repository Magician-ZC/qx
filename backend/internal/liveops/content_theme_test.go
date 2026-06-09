// 文件说明：赛季内容母题库 CRUD + 默认播种 + 赛季列表的 DB 集成测试。
package liveops

import (
	"testing"
)

func TestContentThemeCRUD(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	svc := NewService(db)

	// 默认播种幂等：两次播种不翻倍。
	if err := svc.SeedDefaultContentThemes(ctx); err != nil {
		t.Fatalf("seed1: %v", err)
	}
	if err := svc.SeedDefaultContentThemes(ctx); err != nil {
		t.Fatalf("seed2: %v", err)
	}
	themes, err := svc.ListContentThemes(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(themes) != 3 {
		t.Fatalf("默认母题应为 3（幂等不翻倍），得到 %d", len(themes))
	}

	// 新增一个母题（id 留空自动生成），数组字段往返一致。
	id, err := svc.UpsertContentTheme(ctx, ContentTheme{
		SeasonID:         "s1",
		DecisiveEventIDs: []string{"e1", "e2"},
		TitleIDs:         []string{"称号甲"},
		LandmarkNames:    []string{"地标乙"},
	})
	if err != nil || id == "" {
		t.Fatalf("upsert new: id=%q err=%v", id, err)
	}
	themes, _ = svc.ListContentThemes(ctx)
	var got *ContentTheme
	for i := range themes {
		if themes[i].ID == id {
			got = &themes[i]
		}
	}
	if got == nil {
		t.Fatalf("新增母题未列出")
	}
	if len(got.DecisiveEventIDs) != 2 || got.TitleIDs[0] != "称号甲" || got.LandmarkNames[0] != "地标乙" {
		t.Fatalf("数组字段往返不一致: %+v", got)
	}

	// 改（同 id upsert）：更新字段、不新增行。
	if _, err := svc.UpsertContentTheme(ctx, ContentTheme{ID: id, TitleIDs: []string{"称号丙"}}); err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	themes, _ = svc.ListContentThemes(ctx)
	if len(themes) != 4 {
		t.Fatalf("改不应新增行，期望 4（3默认+1新增），得到 %d", len(themes))
	}

	// 删。
	if err := svc.DeleteContentTheme(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	themes, _ = svc.ListContentThemes(ctx)
	if len(themes) != 3 {
		t.Fatalf("删后应剩 3，得到 %d", len(themes))
	}
}

func TestListSeasons(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	svc := NewService(db)

	if _, err := svc.CreateSeason(ctx, CreateSeasonInput{Name: "一季", MaxPopulation: 100}); err != nil {
		t.Fatalf("create1: %v", err)
	}
	if _, err := svc.CreateSeason(ctx, CreateSeasonInput{Name: "二季", MaxPopulation: 100}); err != nil {
		t.Fatalf("create2: %v", err)
	}
	seasons, err := svc.ListSeasons(ctx, 0)
	if err != nil {
		t.Fatalf("list seasons: %v", err)
	}
	if len(seasons) != 2 {
		t.Fatalf("应列出 2 个赛季，得到 %d", len(seasons))
	}
}
