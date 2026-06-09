// 文件说明：live-ops 维度补全的测试（赛季切片 NorthStarBySeason + 牵挂回访 EmitReturnVisit/ReturnVisitsByActor）。
package analytics

import (
	"context"
	"strings"
	"testing"
)

func TestNorthStarBySeason_Slices(t *testing.T) {
	ctx, db := newDB(t)
	// 赛季 A 一次付费、一次新建会话；赛季 B 一次付费；无赛季一次付费（不应进任一赛季切片）。
	mustEmitDim(t, ctx, db, Event{Stage: StageRevenue, Name: EventPurchase, SessionID: "s1", SeasonID: "A"})
	mustEmitDim(t, ctx, db, Event{Stage: StageAcquisition, Name: EventSessionCreated, SessionID: "s1", SeasonID: "A"})
	mustEmitDim(t, ctx, db, Event{Stage: StageRevenue, Name: EventPurchase, SessionID: "s2", SeasonID: "B"})
	mustEmitDim(t, ctx, db, Event{Stage: StageRevenue, Name: EventPurchase, SessionID: "s3"}) // 无赛季

	repA, err := NorthStarBySeason(ctx, db, "A", 0)
	if err != nil {
		t.Fatalf("赛季 A 北极星失败: %v", err)
	}
	if repA.Purchases != 1 || repA.SessionsCreated != 1 {
		t.Fatalf("赛季 A 切片不符: purchases=%d sessions=%d", repA.Purchases, repA.SessionsCreated)
	}
	repB, _ := NorthStarBySeason(ctx, db, "B", 0)
	if repB.Purchases != 1 || repB.SessionsCreated != 0 {
		t.Fatalf("赛季 B 切片不符: purchases=%d sessions=%d", repB.Purchases, repB.SessionsCreated)
	}
	// 全量（NorthStar）应见到全部 3 次付费。
	all, _ := NorthStar(ctx, db, 0)
	if all.Purchases != 3 {
		t.Fatalf("全量应见 3 次付费，得到 %d", all.Purchases)
	}
}

func TestNorthStarBySeason_EmptySeasonEqualsAll(t *testing.T) {
	ctx, db := newDB(t)
	mustEmitDim(t, ctx, db, Event{Stage: StageRevenue, Name: EventPurchase, SessionID: "s1", SeasonID: "A"})
	mustEmitDim(t, ctx, db, Event{Stage: StageRevenue, Name: EventPurchase, SessionID: "s2"})
	rep, err := NorthStarBySeason(ctx, db, "", 0)
	if err != nil {
		t.Fatalf("空赛季北极星失败: %v", err)
	}
	if rep.Purchases != 2 {
		t.Fatalf("空赛季应退化为全量（2 次付费），得到 %d", rep.Purchases)
	}
}

func TestEmitReturnVisitAndCount(t *testing.T) {
	ctx, db := newDB(t)
	// alice 回访 3 次、bob 1 次。
	for i := 0; i < 3; i++ {
		if err := EmitReturnVisit(ctx, db, "alice", "s1", "user-1"); err != nil {
			t.Fatalf("alice 回访埋点失败: %v", err)
		}
	}
	if err := EmitReturnVisit(ctx, db, "bob", "s2", ""); err != nil {
		t.Fatalf("bob 回访埋点失败: %v", err)
	}

	n, err := ReturnVisitsByActor(ctx, db, "alice")
	if err != nil {
		t.Fatalf("读 alice 回访失败: %v", err)
	}
	if n != 3 {
		t.Fatalf("alice 回访应为 3，得到 %d", n)
	}
	nb, _ := ReturnVisitsByActor(ctx, db, "bob")
	if nb != 1 {
		t.Fatalf("bob 回访应为 1，得到 %d", nb)
	}
	// 无回访的角色应为 0（替代旧的硬编码 0 的真 COUNT）。
	nc, _ := ReturnVisitsByActor(ctx, db, "carol")
	if nc != 0 {
		t.Fatalf("carol 无回访应为 0，得到 %d", nc)
	}
	// 空 actor 安全返回 0。
	nz, _ := ReturnVisitsByActor(ctx, db, "")
	if nz != 0 {
		t.Fatalf("空 actor 应返回 0，得到 %d", nz)
	}
}

func TestEmitReturnVisit_EmptyActorRejected(t *testing.T) {
	ctx, db := newDB(t)
	if err := EmitReturnVisit(ctx, db, "", "s1", "u1"); err == nil {
		t.Fatalf("空 actor 回访埋点应报错")
	}
}

func TestMergeDimensionProps_PreservesAndInjects(t *testing.T) {
	// 原 map props + season/actor 注入：原键保留，维度键注入。
	got := mergeDimensionProps(map[string]any{"foo": "bar"}, "S1", "A1")
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("应返回 map，得到 %T", got)
	}
	if m["foo"] != "bar" || m[seasonPropKey] != "S1" || m[actorPropKey] != "A1" {
		t.Fatalf("合并结果不符: %+v", m)
	}
	// 无维度时原样返回（不强制转 map）。
	pass := mergeDimensionProps(nil, "", "")
	if pmap, ok := pass.(map[string]any); !ok || len(pmap) != 0 {
		t.Fatalf("无维度无 props 应返回空 map，得到 %#v", pass)
	}
	// 非 map props + 维度：原 props 落 "props" 子键，不丢数据。
	got2 := mergeDimensionProps([]string{"x"}, "S2", "")
	m2 := got2.(map[string]any)
	if m2[seasonPropKey] != "S2" || m2["props"] == nil {
		t.Fatalf("非 map props 合并不符: %+v", m2)
	}
}

func TestSeasonAndActorDimensionsLandInJSON(t *testing.T) {
	ctx, db := newDB(t)
	mustEmitDim(t, ctx, db, Event{Stage: StageRetention, Name: EventReturnVisit, SessionID: "s1", SeasonID: "S9", ActorID: "A9"})
	var props string
	if err := db.QueryRowContext(ctx, `SELECT properties_json FROM product_events LIMIT 1`).Scan(&props); err != nil {
		t.Fatalf("读 properties_json 失败: %v", err)
	}
	if !strings.Contains(props, `"season_id":"S9"`) || !strings.Contains(props, `"actor_id":"A9"`) {
		t.Fatalf("维度未落 properties_json: %s", props)
	}
}

func mustEmitDim(t *testing.T, ctx context.Context, db Execer, ev Event) {
	t.Helper()
	if err := Emit(ctx, db, ev); err != nil {
		t.Fatalf("emit 失败: %v", err)
	}
}
