package session

// 文件说明：前缀缓存不变量守护（M1.3，设计 沙盘 §11.2）。
// 决策 system prompt 必须对所有调用字节恒等且不含任何单位身份——否则供应商前缀缓存命不中，成本翻倍。

import (
	"strings"
	"testing"
)

func TestUnitDecisionSystemPromptIsStaticAndIdentityFree(t *testing.T) {
	a := unitDecisionSystemPrompt()
	b := unitDecisionSystemPrompt()
	if a != b {
		t.Fatalf("system prompt 必须字节恒等（前缀缓存前提）")
	}
	// 身份/可变标记绝不能进 system prompt（否则缓存前缀被打断）。
	for _, leak := range []string{"ID=", "名称=", "昵称=", "当前回合"} {
		if strings.Contains(a, leak) {
			t.Fatalf("system prompt 不应含单位身份/可变内容 %q（破坏前缀缓存）", leak)
		}
	}
	// 仍须携带完整静态规则（约 2800 token 的可缓存大块）。
	if !strings.Contains(a, "你不是玩家") || len([]rune(a)) < 500 {
		t.Fatalf("system prompt 应含完整静态决策规则，长度=%d", len([]rune(a)))
	}
}
