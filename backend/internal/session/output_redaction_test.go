package session

// 文件说明：AI 输出侧内容安全的脱敏逻辑测试（评审 load-bearing 修复）。
// redactCompletionResultOutput 须把违规原文从 Output/RawOutput 彻底剥离，杜绝经 LLMInteraction 广播/审计外泄；
// 且为值拷贝、不改动原 result。

import (
	"context"
	"strings"
	"testing"

	"qunxiang/backend/internal/ai"
)

func TestRedactCompletionResultOutput_StripsRawText(t *testing.T) {
	const raw = "违规血腥原文"
	result := ai.CompletionResult{
		Output: []byte(`{"reply":"` + raw + `","mood":"x"}`),
	}
	result.Debug.RawOutput = raw + " raw"

	redacted := redactCompletionResultOutput(result, unsafeDialoguePlaceholder)

	if strings.Contains(string(redacted.Output), raw) {
		t.Fatalf("脱敏后 Output 不应残留原文：%s", redacted.Output)
	}
	if redacted.Debug.RawOutput != "" {
		t.Fatalf("脱敏后 RawOutput 应清空，得到 %q", redacted.Debug.RawOutput)
	}
	if !strings.Contains(string(redacted.Output), unsafeDialoguePlaceholder) {
		t.Fatalf("脱敏后 Output 应含安全占位：%s", redacted.Output)
	}
	// 值拷贝：原 result 绝不应被改动（避免污染调用方持有的同一 result）。
	if !strings.Contains(string(result.Output), raw) || result.Debug.RawOutput == "" {
		t.Fatalf("原 result 不应被脱敏改动：Output=%s RawOutput=%q", result.Output, result.Debug.RawOutput)
	}
}

// TestModerateText_OutputDirectionCatchesTrigger 验证开关开启时输出侧能拦截触发词、关闭时恒放行。
// 这间接覆盖 generateUnitDecision/generateDialogueReply 输出侧接线所依赖的判定语义。
func TestModerateText_OutputDirectionCatchesTrigger(t *testing.T) {
	service := &Service{}
	ctx := context.Background()

	// 关：恒放行（向后兼容）。
	t.Setenv("QUNXIANG_CONTENT_SAFETY", "")
	if v := service.ModerateText(ctx, "她举刀斩首了敌兵", "output"); !v.Allowed {
		t.Fatalf("开关关闭时应恒放行，得到 %+v", v)
	}

	// 开：命中露骨暴力词应拦截。
	t.Setenv("QUNXIANG_CONTENT_SAFETY", "on")
	if v := service.ModerateText(ctx, "她举刀斩首了敌兵", "output"); v.Allowed {
		t.Fatalf("开关开启且命中违规词时应拦截，得到 %+v", v)
	}
	// 干净文本仍放行。
	if v := service.ModerateText(ctx, "她向东侧高地移动占据有利地形", "output"); !v.Allowed {
		t.Fatalf("干净文本应放行，得到 %+v", v)
	}
}
