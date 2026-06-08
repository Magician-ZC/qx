package session

// 文件说明：守护「prompt 缓存命中不计成本」不变量（§11.2 降本）。
// buildLLMInteraction 对 result.CacheHit=true 的回放必须把 EstimatedCost 归零，
// token 仍保留供遥测；否则幻影成本会重复计入会话预算护栏与成本仪表盘。

import (
	"testing"

	"qunxiang/backend/internal/ai"
)

// cacheHitCostFixtureResult 构造一笔“真有 token 用量”的结果——非命中时它必产生正成本，
// 从而保证命中归零断言不是因为本就零成本而平凡通过。
func cacheHitCostFixtureResult(cacheHit bool) ai.CompletionResult {
	return ai.CompletionResult{
		Provider: "openai",
		Model:    "gpt-5.3-codex",
		Output:   []byte(`{"ok":true}`),
		Usage: ai.Usage{
			PromptTokens:     2000,
			CompletionTokens: 500,
			TotalTokens:      2500,
		},
		CacheHit: cacheHit,
	}
}

func TestBuildLLMInteraction_CacheHitZeroesCost(t *testing.T) {
	state := State{}

	// 基线：非命中、同样的 token 用量必须产生正成本（否则下面的归零断言无意义）。
	miss := buildLLMInteraction(state, "u1", "decision", "s", "sys", "usr",
		cacheHitCostFixtureResult(false), "")
	if miss.EstimatedCost <= 0 {
		t.Fatalf("非缓存命中、有 token 用量应产生正成本，得到 %.8f", miss.EstimatedCost)
	}

	// 命中：成本必须归零。
	hit := buildLLMInteraction(state, "u1", "decision", "s", "sys", "usr",
		cacheHitCostFixtureResult(true), "")
	if hit.EstimatedCost != 0 {
		t.Fatalf("缓存命中应记 $0 成本，得到 %.8f", hit.EstimatedCost)
	}

	// token 仍保留供遥测（仅成本计 0，不是把用量也抹掉）。
	if hit.PromptTokens != 2000 || hit.OutputTokens != 500 || hit.TotalTokens != 2500 {
		t.Fatalf("缓存命中应保留 token 用量供遥测，得到 prompt=%d output=%d total=%d",
			hit.PromptTokens, hit.OutputTokens, hit.TotalTokens)
	}

	// 原始 Provider 也应保留（命中不再伪装成 prompt_cache provider）。
	if hit.Provider != "openai" {
		t.Fatalf("缓存命中应保留原始 Provider 供遥测，得到 %q", hit.Provider)
	}
}
