// Command costbench 用真实 LLM 端点测一次单位决策的 token 与成本，估算「每角色每离线日」的 LLM 成本。
// 这是验证实验 W0 预实验 part① 唯一真正可编码的一步（设计 docs/验证实验设计.md / 方案评测 O4）。
// 需要真实 API key 与少量花费；无 key 时给出明确提示并退出。
//
// 用法（在 backend/ 下）：
//
//	go run ./cmd/costbench -n 20 -decisions-per-day 20
//
// 配置走与服务端相同的三层加载（环境变量 > config.local.json > 默认值），见 internal/config。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/config"
	"qunxiang/backend/internal/session"
)

func main() {
	n := flag.Int("n", 20, "调用次数（预热前缀缓存 + 测均值）")
	perDay := flag.Int("decisions-per-day", 20, "每角色每离线日的决策次数（用于折算每角色每日成本）")
	flag.Parse()

	cfg := config.Load()
	svc := ai.NewService(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	req := session.BenchDecisionRequest()
	ctx := context.Background()

	fmt.Printf("=== 一念 LLM 决策成本基准 ===\n调用次数 n=%d，折算用 decisions-per-day=%d\n\n", *n, *perDay)

	var (
		okCalls                       int
		sumPrompt, sumOutput, sumCost float64
		coldPromptTokens              int
		fallbackCalls                 int
	)
	for i := 0; i < *n; i++ {
		start := time.Now()
		res, err := svc.GenerateJSON(ctx, req)
		elapsed := time.Since(start)
		if err != nil {
			fmt.Printf("[%02d] 调用失败: %v\n", i+1, err)
			if i == 0 {
				fmt.Println("\n提示：请确认已配置真实 LLM 端点（OPENAI_BASE_URL/OPENAI_API_KEY/OPENAI_MODEL，或 config.local.json）。无 key 无法测成本。")
				os.Exit(1)
			}
			continue
		}
		if res.UsedFallback {
			fallbackCalls++
		}
		cost := session.EstimateLLMCostUSD(res.Provider, res.Model, res.Usage.PromptTokens, res.Usage.CompletionTokens)
		okCalls++
		sumPrompt += float64(res.Usage.PromptTokens)
		sumOutput += float64(res.Usage.CompletionTokens)
		sumCost += cost
		if i == 0 {
			coldPromptTokens = res.Usage.PromptTokens
		}
		fmt.Printf("[%02d] provider=%s model=%s prompt=%d output=%d cost=$%.6f %v\n",
			i+1, res.Provider, res.Model, res.Usage.PromptTokens, res.Usage.CompletionTokens, cost, elapsed.Round(time.Millisecond))
	}

	if okCalls == 0 {
		fmt.Println("\n无成功调用，无法给出基准。")
		os.Exit(1)
	}

	avgPrompt := sumPrompt / float64(okCalls)
	avgOutput := sumOutput / float64(okCalls)
	avgCost := sumCost / float64(okCalls)
	fmt.Printf("\n=== 结果（%d 次成功，%d 次 fallback）===\n", okCalls, fallbackCalls)
	fmt.Printf("平均 prompt tokens: %.0f（首次冷调用 %d）\n", avgPrompt, coldPromptTokens)
	fmt.Printf("平均 output tokens: %.0f\n", avgOutput)
	fmt.Printf("平均每次决策成本: $%.6f\n", avgCost)
	fmt.Printf("折算每角色每离线日成本（×%d 次决策）: $%.4f\n", *perDay, avgCost*float64(*perDay))
	fmt.Printf("折算每角色每月成本（×30 日）: $%.4f\n", avgCost*float64(*perDay)*30)
	if coldPromptTokens > 0 && avgPrompt < float64(coldPromptTokens) {
		fmt.Printf("提示：平均 prompt tokens 低于冷调用，前缀缓存疑似生效（计费输入下降）。\n")
	}
	fmt.Println("\n注：这是单决策成本基准；真实 MAU 成本还取决于活跃角色数、每日决策次数与缓存命中率。")
}
