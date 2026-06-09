package session

// 文件说明：命运开盒「世界推进」的 DB 集成测试——证明降生后停在 Turn1/部署的 session 真能被推起来转圈：
//   - AdvanceFateWorld 把部署 session 推进执行（ExecutionInProgress 置位）。
//   - LIFE_BEAT 经 surfaceLifeBeatBestEffort 进命运 feed（仅主世界玩家角色、非 NPC）。
//   - 0 敌方主世界角色 → AdvanceFateWorld → 等执行完 → OpenFateFeed 应有 ≥1 条 LIFE_BEAT（命运循环真转起来）。
//   - flag QUNXIANG_FATE_AUTOTICK 关时 ticker 零行为（不推任何 session）。

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

// fateWorldStubLLM 是命运世界推进集成测试的 stub completionClient：对单位决策 schema 回一条「待命 + 叙事」的固定合法 JSON
// （绕开真实网络/gojsonschema——注入 stub 后强校验不在链路上，generateUnitDecision 直接 Unmarshal stub.Output）。
// 其它 schema 的调用回同一条（不报错），让执行期辅助 LLM（叙述/反思等）也走得通、不打断主循环。
type fateWorldStubLLM struct{ decisionJSON string }

func (s *fateWorldStubLLM) GenerateJSON(_ context.Context, req ai.CompletionRequest) (ai.CompletionResult, error) {
	out := s.decisionJSON
	// 非单位决策 schema（叙述/反思/敌方方针等）回一个含通用字段的对象，足以让旁路 best-effort 调用解码不炸。
	if req.SchemaName != "session_unit_decision" {
		out = `{"action":"hold","reasoning":"心绪平和","bubble":"她望了望远方","reply":"嗯","mood":"calm","intent":"observe","should_act":false}`
	}
	return ai.CompletionResult{Provider: "stub", Model: "stub-fate", Output: []byte(out)}, nil
}

// newFateWorldTestService 起一个临时 SQLite + 完整 Service（含 sessions/units/mutator），注入 stub LLM 并开异步执行
//（与生产 router 一致：AdvanceFateWorld 才会经 fast-path 起后台执行一轮）。强制 world binding=shared 让降生绑 world_default。
func newFateWorldTestService(t *testing.T, llm completionClient) (*sql.DB, *Service) {
	t.Helper()
	t.Setenv("QUNXIANG_WORLD_BINDING", "shared")
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "fate_world.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := unit.NewRepository(db)
	service := &Service{
		db:                db,
		sessions:          NewRepository(db),
		units:             repo,
		mutator:           status.NewMutator(db, repo),
		llm:               llm,
		memoryRefreshTurn: map[string]int{},
		memoryRecallTurn:  map[string]int{},
	}
	service.SetAsyncExecution(true)
	return db, service
}

// waitForDeploymentSettled 轮询等某 session 的后台异步执行跑完回到部署阶段（ExecutionInProgress=false）。
// 有界等待，超时即失败——证明这拍确实在合理时间内跑完。
func waitForDeploymentSettled(t *testing.T, service *Service, sessionID string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		state, err := service.sessions.Get(ctx, sessionID)
		if err == nil && !state.ExecutionInProgress && !isAsyncExecutionRunning(sessionID) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 session %s 异步执行跑完超时（ExecutionInProgress 未回落）", sessionID)
}

// TestAdvanceFateWorld_StartsExecutionFromDeployment 验证 ①：降生后停在 Turn1/部署的主世界角色，
// AdvanceFateWorld 能把它推进执行（advancing=true、ExecutionInProgress 置位），等执行完回到下一回合部署。
func TestAdvanceFateWorld_StartsExecutionFromDeployment(t *testing.T) {
	stub := &fateWorldStubLLM{decisionJSON: `{"action":"hold","next_action":"她想去河边走走","speak":"今天天气真好","reasoning":"先安顿下来再说"}`}
	_, service := newFateWorldTestService(t, stub)
	ctx := context.Background()

	view, err := service.CreateMainWorldCharacter(ctx, "acc-advance", MainWorldCharacterInput{Name: "林晚", Origin: "江湖游侠", Desire: "寻一处安身之所"})
	if err != nil {
		t.Fatalf("降生失败: %v", err)
	}

	// 降生即 Turn1/部署。
	before, _ := service.sessions.Get(ctx, view.SessionID)
	if before.TurnState.Turn != 1 || before.ExecutionInProgress {
		t.Fatalf("降生应停在 Turn1/部署且未执行，得到 turn=%d execInProgress=%v", before.TurnState.Turn, before.ExecutionInProgress)
	}

	advancing, err := service.AdvanceFateWorld(ctx, view.SessionID)
	if err != nil {
		t.Fatalf("AdvanceFateWorld 失败: %v", err)
	}
	if !advancing {
		t.Fatalf("部署 session 应被推进（advancing=true）")
	}

	waitForDeploymentSettled(t, service, view.SessionID)
	after, _ := service.sessions.Get(ctx, view.SessionID)
	if after.TurnState.Turn <= before.TurnState.Turn {
		t.Fatalf("推进一拍后回合应前进，得到 before=%d after=%d", before.TurnState.Turn, after.TurnState.Turn)
	}
}

// TestSurfaceLifeBeat_EntersFeed 验证 ③：主世界玩家角色的生活 beat 经 surfaceLifeBeatBestEffort 进命运 feed（kind=life_beat）；
// 且非主世界单位（无 world_default 绑定 / 不在 PlayerUnitIDs）不进 feed（不刷屏）。
func TestSurfaceLifeBeat_EntersFeed(t *testing.T) {
	db, service := newFateWorldTestService(t, &fateWorldStubLLM{})
	ctx := context.Background()
	if err := events.SeedReasonCodeCatalog(ctx, db); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}

	hero := unit.BootstrapRecord(2, "s-life", "player", "苏璃")
	if err := service.units.Save(ctx, hero); err != nil {
		t.Fatalf("save hero: %v", err)
	}
	npc := unit.BootstrapRecord(4, "s-life", "player", "路人甲")
	if err := service.units.Save(ctx, npc); err != nil {
		t.Fatalf("save npc: %v", err)
	}
	state := State{ID: "s-life", WorldID: defaultWorldID, PlayerUnitIDs: []string{hero.ID}}
	state.TurnState.Turn = 3

	// 玩家主角的生活 beat → 进 feed。
	service.surfaceLifeBeatBestEffort(ctx, &state, &hero, unitDecisionPayload{Action: DecisionActionHold, NextAction: "她在集市上买了些干粮", Reasoning: "得备点路上的吃食"})
	// 非玩家单位（不在 PlayerUnitIDs）→ 不进 feed。
	service.surfaceLifeBeatBestEffort(ctx, &state, &npc, unitDecisionPayload{Action: DecisionActionHold, NextAction: "路人甲打了个哈欠"})

	feed, err := service.OpenFateFeed(ctx, hero.ID, 30)
	if err != nil {
		t.Fatalf("open feed: %v", err)
	}
	lifeBeats := 0
	for _, item := range feed {
		if item.Kind == "life_beat" {
			lifeBeats++
			if !contains(item.Narrative, "苏璃") || !contains(item.Narrative, "干粮") {
				t.Fatalf("生活 beat 文本应含角色名与经历，得到 %q", item.Narrative)
			}
		}
	}
	if lifeBeats != 1 {
		t.Fatalf("玩家主角应有 1 条 life_beat，得到 %d", lifeBeats)
	}

	// 非玩家单位的 feed 应为空（生活 beat 没写给 NPC）。
	npcFeed, _ := service.OpenFateFeed(ctx, npc.ID, 30)
	for _, item := range npcFeed {
		if item.Kind == "life_beat" {
			t.Fatalf("NPC 不应有生活 beat（避免刷屏），却得到 %q", item.Narrative)
		}
	}
}

// TestAdvanceFateWorld_ZeroEnemyEndToEnd 是核心证明（⑥ + 整链路）：降生 0 敌方主世界角色 → AdvanceFateWorld →
// 等执行完 → OpenFateFeed 应有 ≥1 条 LIFE_BEAT。即「她确实经历了一拍、feed 真有内容」——命运循环真转起来。
func TestAdvanceFateWorld_ZeroEnemyEndToEnd(t *testing.T) {
	stub := &fateWorldStubLLM{decisionJSON: `{"action":"hold","next_action":"她沿着河岸往北走，想看看那座据点","speak":"总算能喘口气了","reasoning":"先摸清周遭再图后路"}`}
	_, service := newFateWorldTestService(t, stub)
	ctx := context.Background()

	view, err := service.CreateMainWorldCharacter(ctx, "acc-loop", MainWorldCharacterInput{Name: "顾筝", Origin: "边城孤女", Desire: "查清当年灭门真相"})
	if err != nil {
		t.Fatalf("降生失败: %v", err)
	}
	// 确认 0 敌方（开放世界主世界局 EnemyUnitIDs 留空）。
	state0, _ := service.sessions.Get(ctx, view.SessionID)
	if len(state0.EnemyUnitIDs) != 0 {
		t.Fatalf("主世界局应 0 敌方，得到 %d", len(state0.EnemyUnitIDs))
	}

	advancing, err := service.AdvanceFateWorld(ctx, view.SessionID)
	if err != nil {
		t.Fatalf("AdvanceFateWorld 失败: %v", err)
	}
	if !advancing {
		t.Fatalf("0 敌方下也应推进（她自己的生活拍），advancing=true")
	}
	waitForDeploymentSettled(t, service, view.SessionID)

	// 命运 feed 应有 ≥1 条 LIFE_BEAT——证明 0 敌方下她确实跑了一拍自治决策、并 surface 成生活 beat。
	feed, err := service.OpenFateFeed(ctx, view.UnitID, 30)
	if err != nil {
		t.Fatalf("open feed: %v", err)
	}
	lifeBeats := 0
	for _, item := range feed {
		if item.Kind == "life_beat" {
			lifeBeats++
		}
	}
	if lifeBeats < 1 {
		t.Fatalf("推进一拍后命运 feed 应有 ≥1 条 LIFE_BEAT（命运循环未转起来），得到 %d 条 feed", len(feed))
	}
}

// TestFateAutoTick_FlagOffNoBehavior 验证 ④：QUNXIANG_FATE_AUTOTICK 关时 runFateAutoTickPass 零行为——
// 不推任何 session（部署中的主世界角色保持 Turn1/部署、ExecutionInProgress=false）。
func TestFateAutoTick_FlagOffNoBehavior(t *testing.T) {
	t.Setenv("QUNXIANG_FATE_AUTOTICK", "") // 显式关（默认即关）
	_, service := newFateWorldTestService(t, &fateWorldStubLLM{decisionJSON: `{"action":"hold","reasoning":"x"}`})
	ctx := context.Background()

	view, err := service.CreateMainWorldCharacter(ctx, "acc-tick", MainWorldCharacterInput{Name: "白露", Origin: "游方郎中"})
	if err != nil {
		t.Fatalf("降生失败: %v", err)
	}

	service.runFateAutoTickPass(ctx) // flag 关 → 应直接 return、零行为

	state, _ := service.sessions.Get(ctx, view.SessionID)
	if state.TurnState.Turn != 1 || state.ExecutionInProgress {
		t.Fatalf("flag 关时 ticker 不应推进任何 session，得到 turn=%d execInProgress=%v", state.TurnState.Turn, state.ExecutionInProgress)
	}
}

// TestFateAutoTick_FlagOnAdvances 验证 ④ 反面：QUNXIANG_FATE_AUTOTICK 开时 runFateAutoTickPass 推 world_default 活跃主世界角色一拍。
func TestFateAutoTick_FlagOnAdvances(t *testing.T) {
	t.Setenv("QUNXIANG_FATE_AUTOTICK", "true")
	stub := &fateWorldStubLLM{decisionJSON: `{"action":"hold","next_action":"她生起一堆火","reasoning":"夜里得取暖"}`}
	_, service := newFateWorldTestService(t, stub)
	ctx := context.Background()

	view, err := service.CreateMainWorldCharacter(ctx, "acc-tick-on", MainWorldCharacterInput{Name: "沈砚", Origin: "落魄书生"})
	if err != nil {
		t.Fatalf("降生失败: %v", err)
	}
	before, _ := service.sessions.Get(ctx, view.SessionID)

	service.runFateAutoTickPass(ctx) // flag 开 → 应扫到该主世界 session 并推一拍
	waitForDeploymentSettled(t, service, view.SessionID)

	after, _ := service.sessions.Get(ctx, view.SessionID)
	if after.TurnState.Turn <= before.TurnState.Turn {
		t.Fatalf("flag 开时 ticker 应推进主世界角色一拍，得到 before=%d after=%d", before.TurnState.Turn, after.TurnState.Turn)
	}
}
