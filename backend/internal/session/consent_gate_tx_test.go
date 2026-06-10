package session

// 文件说明：consent accept 事务原子性测试（修复 2）。验证：
//   (1) 正常 accept 路径——事务提交、status=accepted、四轴关系增量生效（与翻转同生共死）；
//   (2) 关系效果失败时——整个事务回滚、status 仍 pending、可重试（修复前先 flip 再应用会卡死无法重试）；
//   (3) 跨分片/远端 target——关系写 best-effort 跳过、accept 仍成功提交（不整体崩坏）。
// 走真实 SQLite（newThreatTestService）。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// seedConsentWorldAndUnits 建一个世界 + 给定单位，便于各用例复用。
func seedConsentWorldAndUnits(t *testing.T, ctx context.Context, service *Service, repo *unit.Repository, worldID string, ids ...string) {
	t.Helper()
	if _, err := world.Create(ctx, service.db, world.World{ID: worldID, Name: "事务测试世界"}); err != nil {
		t.Fatalf("create world: %v", err)
	}
	for _, id := range ids {
		rec := unit.BootstrapRecord(1, worldID, "player", "x")
		rec.ID = id
		if err := repo.Save(ctx, rec); err != nil {
			t.Fatalf("save unit %s: %v", id, err)
		}
	}
}

func consentStatus(t *testing.T, service *Service, reqID string) string {
	t.Helper()
	var s string
	if err := service.db.QueryRow(`SELECT status FROM consent_requests WHERE id=?`, reqID).Scan(&s); err != nil {
		t.Fatalf("read consent status: %v", err)
	}
	return s
}

// TestResolveConsent_AcceptCommitsAtomically 断言正常 accept 路径：事务提交、status=accepted、关系增量生效。
func TestResolveConsent_AcceptCommitsAtomically(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	seedConsentWorldAndUnits(t, ctx, service, repo, "w1", "a", "b")

	// 联姻（层3 requires_consent）建 pending 请求：affection +5、trust +3 待 accept 应用。
	res, err := service.RecordSevenInteraction(ctx, "w1", "a", "b", InteractionMarriage, 8)
	if err != nil {
		t.Fatalf("marriage: %v", err)
	}
	if res.ConsentRequestID == "" || res.Applied {
		t.Fatalf("联姻应建待决请求且暂不应用，得 %+v", res)
	}
	// 待同意期间关系不应有任何行（接受方 b 的本侧出边 b→a 也尚未写）。
	if relAxis(t, service, "affection", "b", "a") != 0 {
		t.Fatalf("待同意期间不应有关系增量")
	}

	req, err := service.ResolveConsentRequest(ctx, res.ConsentRequestID, true)
	if err != nil {
		t.Fatalf("accept 不应报错: %v", err)
	}
	if req.Status != "accepted" {
		t.Fatalf("accept 应置 accepted，得 %q", req.Status)
	}
	// 事务已提交 → DB 里 status=accepted 且关系增量生效（与翻转同一事务）。
	if got := consentStatus(t, service, res.ConsentRequestID); got != "accepted" {
		t.Fatalf("DB status 应为 accepted，得 %q", got)
	}
	// 跨玩家硬不变量：accept 只写**接受方 b 自己 owner 一侧**出边 b→a（绝不替发起方 a 写 a→b）。
	if aff := relAxis(t, service, "affection", "b", "a"); aff != 5 {
		t.Fatalf("accept 后接受方本侧 b→a affection 应为联姻增量 5，得 %v", aff)
	}
	if tr := relAxis(t, service, "trust", "b", "a"); tr != 3 {
		t.Fatalf("accept 后接受方本侧 b→a trust 应为联姻增量 3，得 %v", tr)
	}
	// 红线回归：发起方 a 的出边 a→b 绝不被接受方结算写入。
	if relationRowCount(t, service, "a", "b") != 0 {
		t.Fatalf("跨玩家硬不变量被破坏：接受方结算不应替发起方写 a→b 出边")
	}

	// 重复 resolve 守门：已 accepted 不能再处理。
	if _, err := service.ResolveConsentRequest(ctx, res.ConsentRequestID, true); err == nil {
		t.Fatalf("已处理的同意请求应拒绝重复 resolve")
	}
}

// TestResolveConsent_RelationFailureRollsBackAndRetriable 断言关系效果失败时整个事务回滚、status 仍 pending、可重试。
// 构造失败：在 accept 前 DROP relations 表 → 事务内 UPSERT 必然报错 → applyRelationShiftTx 返错 → ResolveConsentRequest 回滚。
// 修复前（先 flip 再在 service.db 上应用）此时 status 已是 accepted、无法重试；修复后状态回到 pending。
func TestResolveConsent_RelationFailureRollsBackAndRetriable(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	seedConsentWorldAndUnits(t, ctx, service, repo, "w1", "a", "b")

	res, err := service.RecordSevenInteraction(ctx, "w1", "a", "b", InteractionMarriage, 8)
	if err != nil {
		t.Fatalf("marriage: %v", err)
	}
	if res.ConsentRequestID == "" {
		t.Fatalf("联姻应建待决请求，得 %+v", res)
	}

	// 让事务内关系 UPSERT 失败：删掉 relations 表（units 存在性检查仍能过，写时才炸）。
	if _, err := service.db.ExecContext(ctx, `DROP TABLE relations`); err != nil {
		t.Fatalf("drop relations: %v", err)
	}

	// accept：关系效果失败 → 应返错且整体回滚。
	if _, err := service.ResolveConsentRequest(ctx, res.ConsentRequestID, true); err == nil {
		t.Fatalf("关系效果失败时 accept 应返错（触发回滚）")
	}
	// 回滚后 status 仍 pending（未被 flip 留住）→ 可重试。
	if got := consentStatus(t, service, res.ConsentRequestID); got != "pending" {
		t.Fatalf("关系效果失败回滚后 status 应仍为 pending（可重试），得 %q", got)
	}

	// 恢复 relations 表后重试 accept：这次应成功提交、关系生效——证明回滚保留了可重试性。
	if _, err := service.db.ExecContext(ctx, `CREATE TABLE relations (
		source_unit_id TEXT NOT NULL,
		target_unit_id TEXT NOT NULL,
		trust REAL NOT NULL DEFAULT 0,
		fear REAL NOT NULL DEFAULT 0,
		affection REAL NOT NULL DEFAULT 0,
		rivalry REAL NOT NULL DEFAULT 0,
		notes_json TEXT NOT NULL DEFAULT '{}',
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (source_unit_id, target_unit_id)
	)`); err != nil {
		t.Fatalf("recreate relations: %v", err)
	}
	req, err := service.ResolveConsentRequest(ctx, res.ConsentRequestID, true)
	if err != nil {
		t.Fatalf("恢复后重试 accept 应成功: %v", err)
	}
	if req.Status != "accepted" {
		t.Fatalf("重试 accept 应置 accepted，得 %q", req.Status)
	}
	if got := consentStatus(t, service, res.ConsentRequestID); got != "accepted" {
		t.Fatalf("重试后 DB status 应为 accepted，得 %q", got)
	}
	if aff := relAxis(t, service, "affection", "b", "a"); aff != 5 {
		t.Fatalf("重试 accept 后接受方本侧关系增量应生效（b→a affection=5），得 %v", aff)
	}
}

// TestResolveConsent_RemoteTargetBestEffortCommits 断言跨分片/远端 target（不在本库）时，
// accept 关系写 best-effort 跳过但事务仍提交、status=accepted（不因 FK 缺失整体崩坏）。
func TestResolveConsent_RemoteTargetBestEffortCommits(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	seedConsentWorldAndUnits(t, ctx, service, repo, "w1", "a") // 只建 a，不建 remote-x

	res, err := service.RecordSevenInteraction(ctx, "w1", "a", "remote-x", InteractionMarriage, 8)
	if err != nil {
		t.Fatalf("marriage to remote: %v", err)
	}
	if res.ConsentRequestID == "" {
		t.Fatalf("联姻应建待决请求，得 %+v", res)
	}

	req, err := service.ResolveConsentRequest(ctx, res.ConsentRequestID, true)
	if err != nil {
		t.Fatalf("远端 target accept 不应报错（关系写 best-effort 跳过）: %v", err)
	}
	if req.Status != "accepted" {
		t.Fatalf("远端 target accept 仍应置 accepted，得 %q", req.Status)
	}
	if got := consentStatus(t, service, res.ConsentRequestID); got != "accepted" {
		t.Fatalf("远端 target accept 后 DB status 应为 accepted，得 %q", got)
	}
	// 远端 target 不存在 → 无本地关系行。
	if aff := relAxis(t, service, "affection", "a", "remote-x"); aff != 0 {
		t.Fatalf("远端 target 不应写本地关系，得 affection=%v", aff)
	}
}
