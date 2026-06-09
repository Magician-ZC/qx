package session

// 文件说明：编年史「读侧导出 API」（chronicle.go 的 ChronicleFeed / ChronicleMomentByID / listChroniclePaged）的
// DB 集成测试，对真实 SQLite 临时库跑通（与 chronicle_test.go 同范式）。覆盖三件读侧关切：
//   1. 倒序分页：ChronicleFeed 按 created_at DESC 倒序、offset 翻页、HasMore/NextOffset 游标正确，且不跨页重复/漏。
//   2. 「回到那一刻」上下文：ChronicleFeed/ChronicleMomentByID 逐条补的 MomentAnchor 命中同 turn 同主角的事件。
//   3. 空集安全：空局 / 不存在的 chronicle_id / 非法 offset / nil service 都返回安全降级（不 panic、不误报错）。
// 这些是「读侧死管线」的回归护栏：router/snapshot 唯一消费口若退化，这里先红。

import (
	"testing"

	"qunxiang/backend/internal/engine/events"
)

// 倒序分页：ChronicleFeed 倒序 + offset 翻页 + HasMore/NextOffset 游标正确，分页拼接无重复无遗漏。
func TestChronicleFeedReverseOrderPaging(t *testing.T) {
	service, ctx := newChronicleService(t)
	u1 := seedChronicleUnit(t, service, ctx, 2, "s1", "甲")

	// 顺序落 5 条（turn 1..5）；created_at 定宽布局严格递增 → 倒序读应为 turn 5,4,3,2,1。
	const total = 5
	for turn := 1; turn <= total; turn++ {
		if id := service.recordChronicleEntry(ctx, "s1", u1, turn, "battle", chronicleText(turn)); id == "" {
			t.Fatalf("落第 %d 条编年史失败", turn)
		}
	}

	// 第一页：limit=2 → 应拿 turn 5,4，HasMore=true，NextOffset=2。
	page1, err := service.ChronicleFeed(ctx, "s1", "", 2, 0)
	if err != nil {
		t.Fatalf("ChronicleFeed 第一页出错: %v", err)
	}
	if len(page1.Views) != 2 {
		t.Fatalf("第一页期望 2 条，得到 %d", len(page1.Views))
	}
	if page1.Views[0].Entry.Turn != 5 || page1.Views[1].Entry.Turn != 4 {
		t.Fatalf("第一页倒序口径错: %d, %d", page1.Views[0].Entry.Turn, page1.Views[1].Entry.Turn)
	}
	if !page1.HasMore || page1.NextOffset != 2 {
		t.Fatalf("第一页游标错: HasMore=%v NextOffset=%d", page1.HasMore, page1.NextOffset)
	}
	if page1.Limit != 2 || page1.Offset != 0 {
		t.Fatalf("第一页 limit/offset 回填错: %+v", page1)
	}

	// 第二页：从 NextOffset 续读 → turn 3,2，仍 HasMore（还剩 turn 1）。
	page2, err := service.ChronicleFeed(ctx, "s1", "", 2, page1.NextOffset)
	if err != nil {
		t.Fatalf("ChronicleFeed 第二页出错: %v", err)
	}
	if len(page2.Views) != 2 || page2.Views[0].Entry.Turn != 3 || page2.Views[1].Entry.Turn != 2 {
		t.Fatalf("第二页倒序/续读错: %+v", turnsOf(page2.Views))
	}
	if !page2.HasMore || page2.NextOffset != 4 {
		t.Fatalf("第二页游标错: HasMore=%v NextOffset=%d", page2.HasMore, page2.NextOffset)
	}

	// 第三页（末页）：只剩 turn 1，HasMore=false，NextOffset 无意义（应为 0）。
	page3, err := service.ChronicleFeed(ctx, "s1", "", 2, page2.NextOffset)
	if err != nil {
		t.Fatalf("ChronicleFeed 末页出错: %v", err)
	}
	if len(page3.Views) != 1 || page3.Views[0].Entry.Turn != 1 {
		t.Fatalf("末页期望仅 turn1: %+v", turnsOf(page3.Views))
	}
	if page3.HasMore || page3.NextOffset != 0 {
		t.Fatalf("末页游标应收口: HasMore=%v NextOffset=%d", page3.HasMore, page3.NextOffset)
	}

	// 三页拼接应恰好覆盖全部 5 条、无重复无遗漏（turn 集合 = {1..5}）。
	seen := map[int]bool{}
	for _, pg := range [][]ChronicleView{page1.Views, page2.Views, page3.Views} {
		for _, v := range pg {
			if seen[v.Entry.Turn] {
				t.Fatalf("分页拼接出现重复 turn=%d", v.Entry.Turn)
			}
			seen[v.Entry.Turn] = true
		}
	}
	if len(seen) != total {
		t.Fatalf("分页拼接应覆盖 %d 条，实际 %d 条: %v", total, len(seen), seen)
	}
}

// 按单位过滤：ChronicleFeed 传 unitID 只装配该单位的视图，且回填 UnitID。
func TestChronicleFeedUnitFilter(t *testing.T) {
	service, ctx := newChronicleService(t)
	u1 := seedChronicleUnit(t, service, ctx, 2, "s1", "甲")
	u2 := seedChronicleUnit(t, service, ctx, 4, "s1", "乙")

	service.recordChronicleEntry(ctx, "s1", u1, 1, "birth", "u1 一笔")
	service.recordChronicleEntry(ctx, "s1", u1, 2, "battle", "u1 二笔")
	service.recordChronicleEntry(ctx, "s1", u2, 3, "birth", "u2 一笔")

	feed, err := service.ChronicleFeed(ctx, "s1", u1, 0, 0)
	if err != nil {
		t.Fatalf("ChronicleFeed 过滤出错: %v", err)
	}
	if feed.UnitID != u1 {
		t.Fatalf("Feed.UnitID 未回填: %q", feed.UnitID)
	}
	if len(feed.Views) != 2 {
		t.Fatalf("u1 过滤期望 2 条，得到 %d", len(feed.Views))
	}
	for _, v := range feed.Views {
		if v.Entry.UnitID != u1 {
			t.Fatalf("过滤后混入非 u1: %+v", v.Entry)
		}
	}
}

// 「回到那一刻」上下文：ChronicleFeed 逐条补的锚点命中同 turn 同主角事件；ChronicleMomentByID 单条反查一致。
func TestChronicleFeedAndMomentAnchorContext(t *testing.T) {
	service, ctx := newChronicleService(t)
	u1 := seedChronicleUnit(t, service, ctx, 2, "s1", "甲")
	u2 := seedChronicleUnit(t, service, ctx, 4, "s1", "乙")

	// turn=8 给 u1 落一条编年史（自带旁路 1 条 turn8/u1 事件），再手工旁路 1 条 turn8/u1、1 条 turn8/u2（不命中）。
	id := service.recordChronicleEntry(ctx, "s1", u1, 8, "vengeance", "她了结旧怨")
	if id == "" {
		t.Fatalf("落编年史失败")
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID: "s1", OwnerUnitID: u1, Code: events.ReasonChronicleRecord, Tick: 8,
	}); err != nil {
		t.Fatalf("旁路 turn8/u1 事件失败: %v", err)
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID: "s1", OwnerUnitID: u2, Code: events.ReasonChronicleRecord, Tick: 8,
	}); err != nil {
		t.Fatalf("旁路 turn8/u2 事件失败: %v", err)
	}

	// 经 Feed 拿到的视图应自带锚点：turn=8、主角 u1、命中 2 条事件（record 自带 + 手工那条），不含 u2。
	feed, err := service.ChronicleFeed(ctx, "s1", u1, 0, 0)
	if err != nil {
		t.Fatalf("ChronicleFeed 出错: %v", err)
	}
	if len(feed.Views) != 1 {
		t.Fatalf("期望 1 条视图，得到 %d", len(feed.Views))
	}
	anchor := feed.Views[0].Anchor
	if anchor.ChronicleID != id || anchor.Turn != 8 || anchor.UnitID != u1 {
		t.Fatalf("Feed 视图锚点头信息不符: %+v", anchor)
	}
	if len(anchor.EventIDs) != 2 {
		t.Fatalf("期望命中 2 条 turn8/u1 事件，得到 %d (%v)", len(anchor.EventIDs), anchor.EventIDs)
	}

	// ChronicleMomentByID 单条反查应与 Feed 内嵌锚点一致（同一条目、同一组事件）。
	view, found, err := service.ChronicleMomentByID(ctx, "s1", id)
	if err != nil {
		t.Fatalf("ChronicleMomentByID 出错: %v", err)
	}
	if !found {
		t.Fatalf("应能反查到刚落的编年史条目")
	}
	if view.Entry.ID != id || view.Entry.Turn != 8 || view.Entry.Kind != "vengeance" {
		t.Fatalf("反查条目字段不符: %+v", view.Entry)
	}
	if len(view.Anchor.EventIDs) != 2 {
		t.Fatalf("单条反查锚点应命中 2 条，得到 %d", len(view.Anchor.EventIDs))
	}
}

// 空集安全：空局 Feed / 不存在的 chronicle_id / 非法 offset / nil service 都安全降级。
func TestChronicleReadEmptyAndDefensive(t *testing.T) {
	service, ctx := newChronicleService(t)

	// 空局：Feed.Views 为非 nil 空切片，HasMore=false，无错。
	empty, err := service.ChronicleFeed(ctx, "s1", "", 0, 0)
	if err != nil {
		t.Fatalf("空局 ChronicleFeed 不应报错: %v", err)
	}
	if empty.Views == nil || len(empty.Views) != 0 || empty.HasMore {
		t.Fatalf("空局应返回非 nil 空 Views、HasMore=false: %+v", empty)
	}

	// 落一条后用不存在的 chronicle_id 反查：found=false、无错。
	u1 := seedChronicleUnit(t, service, ctx, 2, "s1", "甲")
	service.recordChronicleEntry(ctx, "s1", u1, 1, "birth", "唯一一笔")
	_, found, err := service.ChronicleMomentByID(ctx, "s1", "no-such-id")
	if err != nil {
		t.Fatalf("不存在 id 反查不应报错: %v", err)
	}
	if found {
		t.Fatalf("不存在的 chronicle_id 应 found=false")
	}

	// 非法 offset（负数）应归零，仍读回该条。
	feed, err := service.ChronicleFeed(ctx, "s1", "", 10, -5)
	if err != nil {
		t.Fatalf("负 offset ChronicleFeed 不应报错: %v", err)
	}
	if feed.Offset != 0 || len(feed.Views) != 1 {
		t.Fatalf("负 offset 应归零并读回 1 条: offset=%d, views=%d", feed.Offset, len(feed.Views))
	}

	// offset 越界（超过总条数）：返回空 Views、不报错、HasMore=false。
	overshoot, err := service.ChronicleFeed(ctx, "s1", "", 10, 999)
	if err != nil {
		t.Fatalf("越界 offset 不应报错: %v", err)
	}
	if len(overshoot.Views) != 0 || overshoot.HasMore {
		t.Fatalf("越界 offset 应空 Views、HasMore=false: %+v", overshoot)
	}

	// nil service / 空 sessionID 防御：返回错误但不 panic。
	var nilSvc *Service
	if _, err := nilSvc.ChronicleFeed(ctx, "s1", "", 0, 0); err == nil {
		t.Fatalf("nil service ChronicleFeed 应返回错误")
	}
	if _, _, err := nilSvc.ChronicleMomentByID(ctx, "s1", "x"); err == nil {
		t.Fatalf("nil service ChronicleMomentByID 应返回错误")
	}
	if _, err := service.ChronicleFeed(ctx, "", "", 0, 0); err == nil {
		t.Fatalf("空 sessionID ChronicleFeed 应返回错误")
	}
}

// chronicleText 给分页测试造可区分的条目文案（itoaSmall 复用 combat_roll_det_test.go 里的同名小工具）。
func chronicleText(turn int) string {
	return "第 " + itoaSmall(turn) + " 回合的一笔"
}

// turnsOf 抽取视图列表的 turn 序列，供失败信息可读。
func turnsOf(views []ChronicleView) []int {
	out := make([]int, 0, len(views))
	for _, v := range views {
		out = append(out, v.Entry.Turn)
	}
	return out
}
