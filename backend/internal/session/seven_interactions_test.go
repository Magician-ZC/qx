package session

// 文件说明：七种交互 + consent_gate 测试（对真实 SQLite）：单方立即应用、高后果待同意、accept 应用/reject 不应用、
// 重复 resolve 守门、pending 列出、expire 兜底、未知交互拒绝。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

func relAxis(t *testing.T, service *Service, axis, src, tgt string) float64 {
	t.Helper()
	var v float64
	_ = service.db.QueryRow(`SELECT `+axis+` FROM relations WHERE source_unit_id=? AND target_unit_id=?`, src, tgt).Scan(&v)
	return v
}

func TestSevenInteractionsAndConsent(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	if _, err := world.Create(ctx, db, world.World{ID: "w1", Name: "测试世界"}); err != nil {
		t.Fatalf("create world: %v", err)
	}
	for _, id := range []string{"a", "b"} {
		rec := unit.BootstrapRecord(1, "w1", "player", "x")
		rec.ID = id
		if err := repo.Save(ctx, rec); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}

	// 单方交互（结识，层1 unilateral）：立即应用关系（a→b trust+）。
	res, err := service.RecordSevenInteraction(ctx, "w1", "a", "b", InteractionAcquaint, 5)
	if err != nil {
		t.Fatalf("acquaint: %v", err)
	}
	if !res.Applied || res.Tier != "unilateral" || res.ConsentRequestID != "" {
		t.Fatalf("结识应单方立即成立，得 %+v", res)
	}
	if relAxis(t, service, "trust", "a", "b") <= 0 {
		t.Fatalf("结识应增 a→b 信任")
	}

	// 高后果交互（联姻，层3 requires_consent）：建 pending，关系暂不变。
	// 跨玩家硬不变量：accept 写的是**接受方 b 自己 owner 一侧**出边 b→a（不写发起方 a 的 a→b），故这里追踪 b→a。
	affBefore := relAxis(t, service, "affection", "b", "a")
	// 发起方 a→b 出边的 affection 基线（结识只增 a→b trust，affection 仍 0）——accept 后它绝不应被改（红线）。
	affAB0 := relAxis(t, service, "affection", "a", "b")
	res2, err := service.RecordSevenInteraction(ctx, "w1", "a", "b", InteractionMarriage, 8)
	if err != nil {
		t.Fatalf("marriage: %v", err)
	}
	if res2.Applied || res2.Tier != "requires_consent" || res2.ConsentRequestID == "" {
		t.Fatalf("联姻应需同意、暂不应用，得 %+v", res2)
	}
	if relAxis(t, service, "affection", "b", "a") != affBefore {
		t.Fatalf("待同意期间关系不应变")
	}

	// 目标方列出待决请求。
	pend, err := service.ListPendingConsents(ctx, "b")
	if err != nil || len(pend) != 1 || pend[0].ID != res2.ConsentRequestID {
		t.Fatalf("b 应有 1 条待决同意，得 %v err=%v", pend, err)
	}

	// accept → 应用接受方本侧出边 b→a 的 affection（联姻 +5）。
	req, err := service.ResolveConsentRequest(ctx, res2.ConsentRequestID, true)
	if err != nil || req.Status != "accepted" {
		t.Fatalf("accept 应置 accepted，得 %+v err=%v", req, err)
	}
	if relAxis(t, service, "affection", "b", "a") <= affBefore {
		t.Fatalf("accept 后应增接受方本侧 b→a 好感")
	}
	// 红线：绝不替发起方 a 写其出边 a→b 的 affection（a→b 由结识既存，但 affection 必须仍为基线、未被 accept 改）。
	if relAxis(t, service, "affection", "a", "b") != affAB0 {
		t.Fatalf("跨玩家硬不变量被破坏：accept 不应改发起方 a 的出边 a→b（affection 期望 %v 得 %v）", affAB0, relAxis(t, service, "affection", "a", "b"))
	}
	// 重复 resolve 守门。
	if _, err := service.ResolveConsentRequest(ctx, res2.ConsentRequestID, true); err == nil {
		t.Fatalf("已处理的同意请求应拒绝重复 resolve")
	}

	// reject 路径（复仇，层3）：拒绝 → 不应用（b→a affection 不变）。
	affBABefore := relAxis(t, service, "affection", "b", "a")
	res3, err := service.RecordSevenInteraction(ctx, "w1", "b", "a", InteractionVengeance, 8)
	if err != nil || res3.ConsentRequestID == "" {
		t.Fatalf("vengeance 应建待决请求，得 %+v err=%v", res3, err)
	}
	if _, err := service.ResolveConsentRequest(ctx, res3.ConsentRequestID, false); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if relAxis(t, service, "affection", "b", "a") != affBABefore {
		t.Fatalf("reject 后关系不应变")
	}

	// expire 兜底：建一条 pending（开战），用未来 cutoff 全部置 expired。
	if _, err := service.RecordSevenInteraction(ctx, "w1", "a", "b", InteractionWar, 6); err != nil {
		t.Fatalf("war: %v", err)
	}
	n, err := service.ExpireStaleConsents(ctx, "9999-12-31 00:00:00")
	if err != nil || n < 1 {
		t.Fatalf("应至少 expire 1 条 pending，得 n=%d err=%v", n, err)
	}

	// 未知交互拒绝。
	if _, err := service.RecordSevenInteraction(ctx, "w1", "a", "b", SevenInteraction("bogus"), 1); err == nil {
		t.Fatalf("未知交互应报错")
	}

	// 跨分片 target（不在本库）：unilateral 交互仍成功（总线记事实），关系效果 best-effort 跳过、不整体崩坏（评审 load-bearing）。
	res4, err := service.RecordSevenInteraction(ctx, "w1", "a", "remote-x", InteractionAcquaint, 5)
	if err != nil {
		t.Fatalf("跨分片 target 不应使交互失败: %v", err)
	}
	if !res4.Applied {
		t.Fatalf("unilateral 交互应标记 applied（关系效果对远端 best-effort 跳过）")
	}
	if relAxis(t, service, "trust", "a", "remote-x") != 0 {
		t.Fatalf("远端 target 不应有本地关系行")
	}
}
