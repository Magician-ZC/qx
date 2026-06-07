package narration

import (
	"strings"
	"testing"
)

func TestBeatDeterministic(t *testing.T) {
	a := Beat("EMOTION_TRAUMA", -0.5, false, "她中了一箭", 0)
	b := Beat("EMOTION_TRAUMA", -0.5, false, "她中了一箭", 0)
	if a != b {
		t.Fatalf("同输入应同输出：%q vs %q", a, b)
	}
	if !strings.Contains(a, "她中了一箭") {
		t.Fatalf("beat 应包含事实摘要：%q", a)
	}
}

func TestValenceDrivesTone(t *testing.T) {
	cases := []struct {
		code    string
		valence float64
		want    tone
	}{
		{"INBOX_HIGHLIGHT", 0.4, toneWarm},      // 效价为正 → 暖
		{"INBOX_HIGHLIGHT", -0.4, toneGrave},    // 效价为负 → 沉
		{"INBOX_HIGHLIGHT", 0.0, toneConnective}, // 中性效价 → 按 reason-code 归牵连
		{"ECONOMY_LOOT", 0.0, toneWarm},
		{"COMBAT_DOWN", 0.0, toneGrave},
		{"UNKNOWN_CODE", 0.0, toneNeutral},
	}
	for _, c := range cases {
		if got := classify(c.code, c.valence); got != c.want {
			t.Fatalf("classify(%q,%.2f)=%d，期望 %d", c.code, c.valence, got, c.want)
		}
	}
}

func TestPendingWraps(t *testing.T) {
	const summary = "她的旧友被困在峡谷里"
	pending := Beat("PENDING_DECISION", 0, true, summary, 7)
	plain := Beat("PENDING_DECISION", 0, false, summary, 7)
	if !strings.Contains(pending, summary) {
		t.Fatalf("应仍含事实摘要：%q", pending)
	}
	if pending == plain || len(pending) <= len(plain) {
		t.Fatalf("待决策应在普通卡外再套 pending 外层：pending=%q plain=%q", pending, plain)
	}
}

func TestEmptySummaryFallback(t *testing.T) {
	out := Beat("RELEVANCE_MATCH", 0, false, "", 0)
	if !strings.Contains(out, "她在乎的人那边") {
		t.Fatalf("空摘要应用兜底：%q", out)
	}
}

func TestVarietyAcrossEvents(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range []string{"事一", "事二", "事三", "事四", "事五", "事六", "事七", "事八"} {
		seen[Beat("RELEVANCE_MATCH", 0, false, s, 0)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("不同事件应打散出多种变体，仅 %d 种", len(seen))
	}
}

func TestExplicitSeedCoversBank(t *testing.T) {
	seen := map[string]bool{}
	for s := uint64(1); s <= 12; s++ {
		seen[Beat("EMOTION_REWARD", 0.5, false, "X", s)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("不同种子应覆盖多个变体，仅 %d 种", len(seen))
	}
}

// TestRenderSamples 打印若干渲染样例（go test -v 可见），便于人工核对祖魂语气是否自然。
func TestRenderSamples(t *testing.T) {
	samples := []struct {
		code    string
		valence float64
		pending bool
		summary string
	}{
		{"EMOTION_TRAUMA", -0.7, false, "她在峡谷口中了一记闷棍，倒了下去。"},
		{"ECONOMY_LOOT", 0.4, false, "她砍翻了那头山魈，腰间多了 15 枚铜钱。"},
		{"RELEVANCE_MATCH", 0.0, false, "她的旧友在北境打了场胜仗。"},
		{"PENDING_DECISION", -0.5, true, "她从前的恩人，如今落了难。"},
	}
	for _, s := range samples {
		out := Beat(s.code, s.valence, s.pending, s.summary, 0)
		if !strings.Contains(out, "。") && !strings.Contains(out, s.summary) {
			t.Fatalf("渲染异常：%q", out)
		}
		t.Logf("[%s] %s", s.code, out)
	}
}
