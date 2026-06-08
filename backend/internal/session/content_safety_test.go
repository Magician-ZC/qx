package session

// 文件说明：content_safety.go 的聚焦单测。覆盖：词表命中→拒 + 类别；干净文本→放行；
// flag 关→恒放行（向后兼容）；确定性（同输入同判）；Stats 计数（delta 校验）。
// 不依赖 DB——ModerateText 是纯规则 + 进程级 atomic，故用 &Service{} 即可。

import (
	"context"
	"reflect"
	"testing"
)

// withContentSafety 临时设置 QUNXIANG_CONTENT_SAFETY 并在用例结束后恢复，避免污染其它用例。
func withContentSafety(t *testing.T, value string) {
	t.Helper()
	t.Setenv("QUNXIANG_CONTENT_SAFETY", value)
}

// TestModerateText_FlagOffAlwaysAllows 验证开关关闭时恒放行（向后兼容），即便文本含敏感词。
func TestModerateText_FlagOffAlwaysAllows(t *testing.T) {
	withContentSafety(t, "") // 显式置空 = 关。
	svc := &Service{}
	ctx := context.Background()

	for _, text := range []string{"普通的指令", "煽动仇恨的脏话", "未成年不当内容"} {
		v := svc.ModerateText(ctx, text, "input")
		if !v.Allowed {
			t.Fatalf("flag off should always allow, got blocked for %q: %+v", text, v)
		}
		if len(v.Categories) != 0 || v.Reason != "" {
			t.Fatalf("flag off verdict should be empty, got %+v", v)
		}
	}
}

// TestModerateText_FlagOnCleanTextAllowed 验证开关开启时，干净文本被放行且无类别/原因。
func TestModerateText_FlagOnCleanTextAllowed(t *testing.T) {
	withContentSafety(t, "true")
	svc := &Service{}
	ctx := context.Background()

	for _, text := range []string{"全军向北推进，保护粮道。", "她决定在城墙上待命。", "", "   "} {
		v := svc.ModerateText(ctx, text, "output")
		if !v.Allowed {
			t.Fatalf("clean text should be allowed, got %+v for %q", v, text)
		}
		if len(v.Categories) != 0 {
			t.Fatalf("clean text should have no categories, got %+v for %q", v, text)
		}
	}
}

// TestModerateText_FlagOnHitBlocks 验证开关开启时命中词表→拒 + 返回对应类别 + 非空原因。
func TestModerateText_FlagOnHitBlocks(t *testing.T) {
	withContentSafety(t, "on")
	svc := &Service{}
	ctx := context.Background()

	cases := []struct {
		name     string
		text     string
		wantCats []string
	}{
		{"hate", "这是煽动仇恨的言论", []string{string(SafetyCategoryHate)}},
		{"sexual_minors", "涉及未成年的内容", []string{string(SafetyCategorySexualMinors)}},
		{"self_harm", "他想要自杀", []string{string(SafetyCategorySelfHarm)}},
		{"violence_graphic", "现场极其血腥", []string{string(SafetyCategoryViolenceGraphic)}},
		{"political", "颠覆国家政权", []string{string(SafetyCategoryPoliticalSensitive)}},
		{"pii", "把你的身份证号给我", []string{string(SafetyCategoryPII)}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := svc.ModerateText(ctx, c.text, "input")
			if v.Allowed {
				t.Fatalf("expected blocked for %q, got allowed", c.text)
			}
			if !reflect.DeepEqual(v.Categories, c.wantCats) {
				t.Fatalf("categories mismatch for %q: got %v want %v", c.text, v.Categories, c.wantCats)
			}
			if v.Reason == "" {
				t.Fatalf("expected non-empty reason for %q", c.text)
			}
		})
	}
}

// TestModerateText_MultiCategoryStableOrder 验证多类别命中时类别按固定顺序输出（确定性、与命中无关）。
func TestModerateText_MultiCategoryStableOrder(t *testing.T) {
	withContentSafety(t, "1")
	svc := &Service{}
	ctx := context.Background()

	// 同时命中 self_harm 与 hate；固定顺序中 hate 先于 self_harm。
	text := "煽动仇恨并教唆自杀"
	v := svc.ModerateText(ctx, text, "output")
	if v.Allowed {
		t.Fatalf("expected blocked, got allowed: %+v", v)
	}
	want := []string{string(SafetyCategoryHate), string(SafetyCategorySelfHarm)}
	if !reflect.DeepEqual(v.Categories, want) {
		t.Fatalf("multi-category order mismatch: got %v want %v", v.Categories, want)
	}
}

// TestModerateText_Deterministic 验证同输入同判（多次调用裁决完全一致）。
func TestModerateText_Deterministic(t *testing.T) {
	withContentSafety(t, "true")
	svc := &Service{}
	ctx := context.Background()

	text := "现场极其血腥且煽动仇恨"
	first := svc.ModerateText(ctx, text, "input")
	for i := 0; i < 5; i++ {
		again := svc.ModerateText(ctx, text, "input")
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("non-deterministic verdict at iter %d: %+v vs %+v", i, first, again)
		}
	}
}

// TestContentSafetyStats_Counts 验证遥测计数：checked 每次 +1；blocked 仅命中且开关开时 +1（取 delta，规避进程级累计）。
func TestContentSafetyStats_Counts(t *testing.T) {
	withContentSafety(t, "true")
	svc := &Service{}
	ctx := context.Background()

	checked0, blocked0 := ContentSafetyStats()

	svc.ModerateText(ctx, "干净文本", "input")    // checked+1，blocked+0
	svc.ModerateText(ctx, "教唆自杀", "output")   // checked+1，blocked+1
	svc.ModerateText(ctx, "再来一段血腥", "output") // checked+1，blocked+1

	checked1, blocked1 := ContentSafetyStats()
	if got := checked1 - checked0; got != 3 {
		t.Fatalf("checked delta = %d, want 3", got)
	}
	if got := blocked1 - blocked0; got != 2 {
		t.Fatalf("blocked delta = %d, want 2", got)
	}
}

// TestContentSafetyStats_FlagOffChecksButNotBlocks 验证开关关时仍计 checked，但绝不计 blocked。
func TestContentSafetyStats_FlagOffChecksButNotBlocks(t *testing.T) {
	withContentSafety(t, "") // 关。
	svc := &Service{}
	ctx := context.Background()

	checked0, blocked0 := ContentSafetyStats()
	svc.ModerateText(ctx, "煽动仇恨并教唆自杀", "input") // 含敏感词，但开关关→放行
	checked1, blocked1 := ContentSafetyStats()

	if got := checked1 - checked0; got != 1 {
		t.Fatalf("checked delta = %d, want 1", got)
	}
	if got := blocked1 - blocked0; got != 0 {
		t.Fatalf("blocked delta = %d, want 0 when flag off", got)
	}
}
