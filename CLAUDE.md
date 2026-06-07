# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目概述

**群像战棋** 是一款 LLM 驱动的六边形回合制战棋游戏。后端是服务端权威（server-authoritative）的 Go 服务，负责完整的世界模拟；玩家不直接操控单位，而是下达**自然语言指令**，由 LLM 为每个单位生成具体行动。前端是 React + Pixi.js 的渲染与指令客户端，仅发送输入、接收快照（见 `router.go:1127` 的注释 “client only sends input, server remains the authoritative state owner”）。

- `backend/` — Go 1.26，模块名 `qunxiang/backend`，所有业务逻辑在 `internal/` 下。
- `frontend/` — Vite + React 18 + Pixi.js v7（TypeScript）。
- 代码注释一律中文（每个文件开头有 `// 文件说明：…`），标识符用英文。

## 常用命令

### 后端（在 `backend/` 目录下）

```bash
go run ./cmd/server          # 启动 HTTP + WebSocket 服务，默认监听 :8080
go build ./...               # 编译全部包
go test ./...                # 运行测试（目前仅 internal/engine/... 有用例，其余包暂无）
go vet ./...                 # 标准静态检查（注意：当前存在 trade.go:68 unreachable code 的历史告警）
go run ./cmd/statuslint ./...  # 运行自定义状态字段静态分析器（详见下方“关键不变量”）
```

- **测试现状**：测试集中在 `internal/engine/`（`decision` 决策层路由、`arbitration` 零和仲裁、`status` 批量状态写入），是仓库第一套测试；`session` 等其余包暂无 `*_test.go`。测试文件被 `statuslint` 白名单豁免（可直接改状态字段）。`status` 包的批写测试会用 `internal/storage/sqlite` 起一个临时 SQLite（modernc 纯 Go，无需 CGO）。
- `statuslint` 是**独立**分析器，`go build` / `go vet` **不会**自动运行它，必须单独执行。它当前会报告 3 处历史违例（`session/hunger.go:176-177`、`session/romance.go:999`）并以非零码退出——这是既有状态，不是你引入的。

### 前端（在 `frontend/` 目录下）

```bash
npm install
npm run dev       # Vite 开发服务器，绑定 0.0.0.0:5173
npm run build     # 先 tsc --noEmit 类型检查，再 vite build 产出 dist/
npm run lint      # eslint 检查 src/**/*.{ts,tsx} 与 vite.config.ts
npm run preview   # 预览生产构建
```

前端默认通过 `VITE_API_BASE_URL`（开发态默认 `http://127.0.0.1:8080`，生产态用 `window.location.origin`）连后端 REST，并把 `http→ws / https→wss` 协议替换后连 `/ws`。`vite.config.ts` **没有配置代理**，跨源时会有 CORS 问题（后端已设 `Access-Control-Allow-Origin: *`）。

## 配置与密钥

后端配置经三层加载（`internal/config/config.go` 的 `Load()`），优先级：**环境变量 > 本地 JSON 文件 > 默认值**。

- 本地 JSON 默认读 `backend/config.local.json`（已被 gitignore，**勿提交真实 key**），可用 `QUNXIANG_CONFIG_FILE` 指向别处；模板见 `backend/config.example.json`。JSON 里的值仅在对应环境变量缺失时注入（`setEnvIfMissing`）。
- 关键环境变量（均可加 `QUNXIANG_` 前缀，多数也接受裸 `OPENAI_*` 形式）：
  - `QUNXIANG_HTTP_ADDR`（默认 `:8080`）、`QUNXIANG_DB_DRIVER`（`sqlite`/`mysql`，默认 sqlite）、`QUNXIANG_SQLITE_PATH`、`QUNXIANG_MYSQL_DSN`、`QUNXIANG_POSTGRES_DSN`。
  - LLM 端点：`OPENAI_BASE_URL` / `OPENAI_API_KEY` / `OPENAI_MODEL` / `OPENAI_WIRE_API`（`chat_completions` 或 `responses`）/ `OPENAI_REASONING_EFFORT`，以及 `DEEPSEEK_*` 与最多 8 级 `*_FALLBACK_*` 后备端点。
  - OpenRouter base_url 时会把 `OPENROUTER_API_KEYS`（逗号/换行分隔的多 key）并入轮询。
- LLM 超时被夹在 [60s, 180s]（`parseDurationSeconds`）。

## 架构总览

### 回合生命周期（这是理解整个系统的主线）

状态机由 `internal/engine/turns/state.go` 的 `turns.State` 驱动，单局推进两个阶段交替：

1. **PhaseDeployment（部署）**：玩家下发指令（doctrine/task/order），消耗指令力（command power）。
2. **PhaseExecution（执行）**：按 **ATB（Active Time Battle）** 速度顺序逐个唤醒单位，由 LLM 为每个单位生成决策并执行，直到行动点耗尽或回合结束。

`session.Service.AdvancePhase`（`session/service.go:620`）是状态机驱动入口。Execution→Deployment 切换时统一结算怀孕生育、信鸽投递、饥饿、记忆衰减，并刷新敌方指令。ATB 速度公式与“同阵营连续行动 -15% 势头惩罚”在 `session/executor_loop.go`。

执行可同步或**异步**（`SetAsyncExecution(true)`，HTTP 路由默认开启）。异步执行在后台 goroutine 跑（带 panic 恢复、45 分钟超时），通过 `SetProgressReporter` 回调把进度推到 WebSocket。`ExecutionInProgress=true` 期间 `AdvancePhase` 直接早返回。

### 指令 → LLM → 单位决策（核心数据流）

```
玩家自然语言指令
  → SetFactionDirective (session/command.go)
  → parseDirectiveIntent 用 LLM 归一化文本/优先级/目标 (command_intent.go，有关键词 fallback)
  → 写入 DirectiveHistory
  → 执行阶段聚合进单位决策上下文 (directive_context.go)
  → generateUnitDecision 调 LLM 产出 unitDecisionPayload (session/llm.go)
  → resolveDirectiveCompliance 按性格/记忆判定是否服从 (command_policy.go / obedience.go)
  → 行动落地，状态变更经 Mutator
```

- 三类指令（`types.go`）：**Doctrine**（阵营全局策略）、**Task**（单位组任务）、**Order**（执行期即时单令，消耗指令力）。指令力默认 3/3，每部署回合 +2，order/严格 task 各扣 1。
- 单人模式下，敌方阵营的全局策略也由 LLM 生成（`generateEnemyGlobalDirective`），失败时走启发式 fallback。

### 引擎核心包（`internal/engine/`，含大世界升级的新构件）

`turns`（回合状态机）、`events`（reason-code 目录）、`status`（Mutator）是原有核心。以下三个是为「大世界沙盘」演进新增的、**纯逻辑可测试**构件，体现「决策用 LLM、结算用代码」原则（设计见 `docs/游戏开发方案GDD.md` §7、`docs/大世界沙盘设计方案.md` §1.5/§11.3）：

- `engine/decision`：**决策层路由** `Router.Route(Situation) Decision`——安全反射(L1 护栏)优先 → 关键节点才升级 LLM → 日常零 LLM。把「<2% 上 LLM」从降级模式扶正为常态。
- `engine/arbitration`：**零和仲裁原语** `Resolve(Contest) Outcome`——胜负仅由 `Score` + 确定性掷骰（FNV+splitmix64+A-Res 加权抽样），**与行动频率/入队顺序无关**，胜率∝Score。这是反 P2W 的机制保证。
- `engine/decision` 的 `attribution.go`：**「意外但合理」的代码强制**（设计宪法 §5）。`ValidateAttribution(attr, snap)` 要求每个自治选择都带可解析的前因（人格/记忆/红线/关系/压力/回响），无源戏剧性意外判 OOC；`GateSurprise(action, in)` 把突然恋爱/卖传家宝/叛变硬绑前因。**已接入 `session.generateUnitDecision` 且线上默认开启强制**（`httpapi/router.go` 调 `SetAttributionEnforcement(true)`）：决策前 `prepareAttribution`（`session/attribution_bridge.go`）构造快照——人格(persona_trait)+压力(pressure)+实时记忆(memory，可引用 ID 暴露进 prompt)+对外关系四轴(relation，归一 [-1,1])，校验不过的归因优雅回退安全决策（继续待命）。遥测 `Service.AttributionStats()`；OOC 率过高可 `SetAttributionEnforcement(false)` 回影子模式。`redline/order_echo` 待离线宪章/回响落地后接入。
- `status.Mutator.ApplyBatch`：**批量状态写入**——把「每决策 ~15 次 DB 往返」收敛为「按单位读一次/写一次 + 单事务批量插事件」，与逐次 `Apply` 语义等价。
- 这些包都有测试（`go test ./internal/engine/...`）。它们是新增能力，尚未全面接入 `session` 执行主链路（属渐进式升级，整合见 GDD §7.3 的引擎升级技术规格）。

### AI/LLM 子系统（`internal/ai/`）

- 所有 LLM 调用统一走 `ai.Service.GenerateJSON()`，**没有旁路**。请求是 `CompletionRequest`（带 `ResponseSchema` 的 JSON Schema 字节），结果是被 `gojsonschema` **强校验**后的 JSON——校验不通过即拒绝，不接受部分结果。
- 多 provider 故障转移：Primary（OpenAI）→ Secondary（DeepSeek）→ Tertiary（fallback 端点）；同签名端点用原子序号轮询多 key。全部失败则调用规则 fallback（若有）。
- `batch.go` 按指纹（task+schema+prompts+metadata 的 SHA256）去重并发请求，受 `MaxConcurrency` 限制。
- 每次交互记为 `LLMInteraction`（含 prompt、token、估算成本、是否 fallback），存进 `State.LLMInteractions`。存储前会压缩（仅保留最近若干条的完整 prompt）。LLM 累计成本触发预算护栏后，该局后续 LLM 调用全部降级为 fallback（`llm_budget.go`）。

### 关键不变量：状态变更必须经 StatusMutator

这是本仓库最重要的约定，由静态分析器强制：

- **受保护字段**（`unit.Status` 上的 `HP / Hunger / Morale / Loyalty / LivesRemaining / Mood`）**禁止直接赋值或自增**。所有变更必须经 `internal/engine/status` 的 `Mutator.Apply(Mutation{...})`，它会读取 reason code 定义、做字段级 clamp、生成标准化事件行并追加到单位 `RecentEventIDs`，从而让每次状态变化都可审计。
- `internal/infra/statuslint`（经 `cmd/statuslint` 运行）在 AST 层拦截白名单外的直接改写，报错 `direct mutation of protected unit status is forbidden; use StatusMutator`。白名单仅限：`*_test.go`、`/internal/engine/status/`、`/internal/unit/repository.go`、`/internal/unit/lives.go`。
- 配套的 **reason code 目录**在 `internal/engine/events/reason_codes.go`：每个 code 映射到类别（combat_damage / survival_consumption / emotion_event / …）、默认文案与影响域。`Mutator.Apply` 严格查表，未知 code 直接报错。新增状态变化原因须先在此登记。

### 持久化（`internal/storage/`）

- 主存储支持 **SQLite（默认）/ MySQL**，经 `QUNXIANG_DB_DRIVER` 选择，承载 session/unit/world/event/account 全部数据。
- `dbdialect` 抽象按连接注册方言，处理 `ON CONFLICT`（SQLite）vs `ON DUPLICATE KEY`（MySQL）差异——开新连接务必 `Register()`，否则 `For()` 默认回退 SQLite 方言导致 SQL 语法错误。
- **PostgreSQL 仅作冷存储（cold storage）**：配置 `QUNXIANG_POSTGRES_DSN` 时才启用，只归档 `hall_of_fame_entries`（阵亡/退役单位的传记）。注意：一旦配了 postgres，账户服务会改用 postgres（`cmd/server/main.go:69`）。
- session 状态以整块 JSON 存 `single_player_sessions`；unit 以多个 JSON blob 列存储。阶段边界快照另存 `session_phase_snapshots` 供断线重连（`reconnect.go`）。`State`（服务端权威全量）与 `Snapshot`/`PublicSnapshot`（客户端可见、剔除审计字段）是两个不同视图。

### session 包按主题分布（~50 个文件）

`internal/session/` 是模拟内核，按主题定位：

- **核心循环**：`service.go`、`types.go`、`executor_loop.go`、`command*.go`、`llm*.go`、`directive_context.go`、`repository.go`、`reconnect.go`、`pregame.go`。
- **战斗**：`battlefield*.go`（地图生成/脚本）、`combat_roll.go`（确定性掷骰）、`combat_effects.go`、`combat_shake.go`（高压情绪覆盖）、`terrain_combat.go`、`skills.go`、`reaction_queue.go`。
- **社交/关系**：`relation.go`（四轴 trust/fear/affection/rivalry，clamp 到 [-10,10]）、`romance.go`、`diplomacy.go`、`social.go`、`obedience.go`、`intelligence.go`。
- **生存/经济**：`hunger.go`、`weather.go`（确定性天气）、`production.go`、`trade.go`、`interaction_actions.go`、`structure_actions.go`、`random_events.go`。
- **记忆/叙事**：`memory*.go`、`knowledge_memory.go`、`reflection.go`、`narrative.go`、`legacy_hall.go`、`cold_storage.go`、`pigeon.go`、`ai_auxiliary.go`。
- **治理/安全**：`moderation.go`（举报）、`privacy.go`（不可逆数据擦除）。

### 实时通道与前端

- WebSocket Hub（`internal/ws/hub.go`）维护 `sessionID → clients` 订阅；客户端连 `/ws` 后发 `session_subscribe`{session_id} 注册。后端经 `BroadcastSessionEvent` 推送三类事件：`session_snapshot`、`session_log`、`llm_interaction`。心跳 60s ping，>120s 无 pong 断开；前端 1200ms 重连。
- 前端 `src/session/api.ts` 是 REST + WS 客户端；`src/game/PixiBoard.tsx` + `bootstrapPixi.ts` 负责 Pixi 渲染。渲染分层 worldLayer（地形，按 `worldRenderKey` 缓存）/ unitLayer / hudLayer，以 `SessionSnapshot` 为唯一数据源；快照不可变，每次 diff 全量重渲。App ticker 停用，仅数据变化时手动 `renderFrame()`。
- 战争迷雾在前端按指挥阵营从友军视野 BFS 计算。前端正在向 Unciv 风格 UI 改造（见 `frontend/UI_PLAN.md`，地形资源在 `public/unciv/`）。

## 其它约定

- **确定性随机**：模拟逻辑用 `sessionID + turn + actor` 的 FNV-32a/64a 哈希取随机，不用全局 `rand`，以保证可复现。
- LLM 决策点几乎都有规则 fallback（`fallback*`、`confusedUnitDecision` 等），LLM 不可用/超时/解析失败/预算护栏触发时不会中断主循环。
- 记忆按类别有容量上限并指数衰减（tau≈120 回合），关键事件靠 importance 加成与闪回保留。
