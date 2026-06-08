package session

// 文件说明：社会客体撮合测试（对真实 SQLite）：四因子过门、arbitration 确定性择 slots 人、绑成员、留痕、幂等。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/socialobject"
	"qunxiang/backend/internal/unit"
)

func TestMatchIntoSocialObject(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	// 候选单位先落库（留痕 EmitProcessEvent 的 events.actor_unit_id 有 FK，需真单位）。
	for i, id := range []string{"u1", "u2", "u3"} {
		rec := unit.BootstrapRecord(int64(i)+1, "w1", "player", "她")
		rec.ID = id
		if err := repo.Save(ctx, rec); err != nil {
			t.Fatalf("save unit %s: %v", id, err)
		}
	}

	// 三个高分候选（四因子接近满）+ 一个低分（过不了门）。
	cands := []MatchCandidate{
		{UnitID: "u1", GeoNear: 0.9, HookFit: 0.9, RelationIntersect: 0.8, DensityAdj: 0.7},
		{UnitID: "u2", GeoNear: 0.8, HookFit: 0.85, RelationIntersect: 0.9, DensityAdj: 0.6},
		{UnitID: "u3", GeoNear: 0.95, HookFit: 0.8, RelationIntersect: 0.7, DensityAdj: 0.9},
		{UnitID: "u-low", GeoNear: 0.0, HookFit: 0.0, RelationIntersect: 0.0, DensityAdj: 0.0}, // 过不了门
	}
	objID, chosen, err := service.MatchIntoSocialObject(ctx, "w1", "party", "boss-team", cands, 2)
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if objID == "" || len(chosen) != 2 {
		t.Fatalf("应建客体且择 2 人，得 obj=%q chosen=%v", objID, chosen)
	}
	for _, uid := range chosen {
		if uid == "u-low" {
			t.Fatalf("低分候选不应被选中（未过撮合门）")
		}
	}

	// 成员落库 = chosen。
	members, err := socialobject.ListMembers(ctx, db, objID)
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("应绑 2 名成员，得 %d", len(members))
	}

	// 确定性：同输入再撮合 → 同客体 id + 同选择。
	objID2, chosen2, err := service.MatchIntoSocialObject(ctx, "w1", "party", "boss-team", cands, 2)
	if err != nil {
		t.Fatalf("match2: %v", err)
	}
	if objID2 != objID {
		t.Fatalf("同上下文应复用同客体 id：%q vs %q", objID2, objID)
	}
	if len(chosen2) != 2 || chosen2[0] != chosen[0] || chosen2[1] != chosen[1] {
		t.Fatalf("arbitration 应确定性：%v vs %v", chosen2, chosen)
	}

	// 留痕：SOCIAL_OBJECT_BIND 流程事件落库（每名成员一条）。
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE reason_code = ?`, "SOCIAL_OBJECT_BIND").Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n < 2 {
		t.Fatalf("应有 ≥2 条 SOCIAL_OBJECT_BIND 留痕，得 %d", n)
	}

	// 无人达标 → 不建客体。
	if id, ch, _ := service.MatchIntoSocialObject(ctx, "w1", "party", "empty", []MatchCandidate{{UnitID: "x", GeoNear: 0, HookFit: 0, RelationIntersect: 0, DensityAdj: 0}}, 3); id != "" || len(ch) != 0 {
		t.Fatalf("无人过门不应建客体，得 id=%q chosen=%v", id, ch)
	}
}
