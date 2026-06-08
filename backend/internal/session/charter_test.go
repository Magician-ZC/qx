package session

// 文件说明：离线宪章（OfflineCharter）读写/规范化/红线展平辅助的单元测试（零 DB、纯逻辑、确定性）。
// 守护：缺失安全返回零值、写入懒初始化 map、规范化去空白补稳定 ID、charterRedlinesAsMap 对齐 snap.Redlines、
// state==nil/unitID 空时各路 no-op 不 panic、规范化确定性（同输入同输出）。

import (
	"reflect"
	"testing"
)

func TestGetUnitCharter_MissingAndNilSafe(t *testing.T) {
	// nil state 不 panic，返回零值 + false。
	if c, ok := GetUnitCharter(nil, "u1"); ok || !CharterIsEmpty(c) {
		t.Fatalf("nil state 应返回空宪章+false，得到 %+v ok=%v", c, ok)
	}
	state := &State{ID: "s1"}
	// UnitCharters 为 nil 时缺失返回 false。
	if c, ok := GetUnitCharter(state, "u1"); ok || !CharterIsEmpty(c) {
		t.Fatalf("未设置宪章应返回空+false，得到 %+v ok=%v", c, ok)
	}
	// 空 unitID 安全返回。
	if _, ok := GetUnitCharter(state, ""); ok {
		t.Fatalf("空 unitID 应返回 false")
	}
}

func TestSetUnitCharter_LazyInitAndRoundTrip(t *testing.T) {
	state := &State{ID: "s1"} // UnitCharters 故意为 nil
	in := OfflineCharter{
		LongTermGoals:  []string{"光复故土", "  ", "护住幼妹"},
		SocialMandates: []string{"可代我结盟北境", ""},
		Redlines: []CharterRedline{
			{ID: "rl_betray", Text: "绝不背叛血亲", Severity: "hard"},
			{Text: "勿伤平民"}, // 缺 ID，应补稳定 ID
			{Text: "   "},      // 空白原文，应被剔除
		},
	}
	SetUnitCharter(state, "u1", in)

	if state.UnitCharters == nil {
		t.Fatalf("SetUnitCharter 应懒初始化 UnitCharters map")
	}
	got, ok := GetUnitCharter(state, "u1")
	if !ok {
		t.Fatalf("写入后应能读回")
	}
	// 长期目标去空白后剩 2 条。
	if !reflect.DeepEqual(got.LongTermGoals, []string{"光复故土", "护住幼妹"}) {
		t.Fatalf("长期目标规范化不符，得到 %v", got.LongTermGoals)
	}
	// 社交授权去空白后剩 1 条。
	if !reflect.DeepEqual(got.SocialMandates, []string{"可代我结盟北境"}) {
		t.Fatalf("社交授权规范化不符，得到 %v", got.SocialMandates)
	}
	// 红线：空白原文被剔除，剩 2 条，缺 ID 的被补稳定 ID。
	if len(got.Redlines) != 2 {
		t.Fatalf("红线应剩 2 条（剔除空白原文），得到 %d", len(got.Redlines))
	}
	if got.Redlines[0].ID != "rl_betray" {
		t.Fatalf("显式 ID 应保留，得到 %q", got.Redlines[0].ID)
	}
	if got.Redlines[1].ID == "" {
		t.Fatalf("缺 ID 的红线应补稳定 ID")
	}
}

func TestSetUnitCharter_NoOpGuards(t *testing.T) {
	// nil state / 空 unitID 均安全 no-op，不 panic。
	SetUnitCharter(nil, "u1", OfflineCharter{LongTermGoals: []string{"x"}})
	state := &State{ID: "s1"}
	SetUnitCharter(state, "", OfflineCharter{LongTermGoals: []string{"x"}})
	if state.UnitCharters != nil {
		t.Fatalf("空 unitID 不应初始化 map")
	}
}

func TestClearUnitCharter(t *testing.T) {
	state := &State{ID: "s1"}
	SetUnitCharter(state, "u1", OfflineCharter{LongTermGoals: []string{"goal"}})
	ClearUnitCharter(state, "u1")
	if _, ok := GetUnitCharter(state, "u1"); ok {
		t.Fatalf("清除后不应再读到宪章")
	}
	// nil state / 空 id 安全。
	ClearUnitCharter(nil, "u1")
	ClearUnitCharter(state, "")
}

func TestCharterRedlinesAsMap_AlignsWithSnapshot(t *testing.T) {
	state := &State{ID: "s1"}
	// 未设置宪章：返回非 nil 空 map（便于调用方直接赋值给 snap.Redlines）。
	m := charterRedlinesAsMap(state, "u1")
	if m == nil || len(m) != 0 {
		t.Fatalf("无宪章应返回非 nil 空 map，得到 %v", m)
	}

	SetUnitCharter(state, "u1", OfflineCharter{
		Redlines: []CharterRedline{
			{ID: "rl_a", Text: "不卖传家剑"},
			{ID: "rl_b", Text: "不向旧主开火"},
			{ID: "rl_c", Text: "  "}, // 空白原文剔除
		},
	})
	m = charterRedlinesAsMap(state, "u1")
	if len(m) != 2 {
		t.Fatalf("红线 map 应有 2 条，得到 %d", len(m))
	}
	if m["rl_a"] != "不卖传家剑" || m["rl_b"] != "不向旧主开火" {
		t.Fatalf("红线 map 内容不符: %v", m)
	}
}

func TestNormalizeCharter_Deterministic(t *testing.T) {
	in := OfflineCharter{
		LongTermGoals: []string{" a ", "b"},
		Redlines:      []CharterRedline{{Text: "x"}, {Text: "y"}},
	}
	first := NormalizeCharter("u1", in)
	second := NormalizeCharter("u1", in)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("规范化应确定性（同输入同输出）：\n%+v\n%+v", first, second)
	}
	// 缺 ID 的红线在固定 unitID+索引下得到稳定 ID。
	if first.Redlines[0].ID != "rl_u1_0" || first.Redlines[1].ID != "rl_u1_1" {
		t.Fatalf("派生红线 ID 不符: %q %q", first.Redlines[0].ID, first.Redlines[1].ID)
	}
	// 全空宪章规范化后仍判空。
	if !CharterIsEmpty(NormalizeCharter("u1", OfflineCharter{LongTermGoals: []string{"  "}})) {
		t.Fatalf("全空白宪章规范化后应判空")
	}
}

func TestItoaNonNeg(t *testing.T) {
	cases := map[int]string{0: "0", 7: "7", 42: "42", 100: "100"}
	for in, want := range cases {
		if got := itoaNonNeg(in); got != want {
			t.Fatalf("itoaNonNeg(%d)=%q 期望 %q", in, got, want)
		}
	}
}
