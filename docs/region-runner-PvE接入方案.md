# region-runner PvE 接入方案（elite 遭遇接入执行主循环）

承接已完工的 M7.3-real region-runner（离线单位在战斗之外觅食/休息/社交/反思）。本方案把 **PvE elite 遭遇**
接入 region-runner 主循环——让被唤醒的离线单位**真的会在路上撞见硬茬**，由 `decision.Router` 的关键节点闸
（StrategicFork）触发参与。设计依据：`docs/PvE威胁系统.md` line 169（「待补：威胁刷新调度、接入执行主循环
（decision.Router StrategicFork 触发参与）」）、`大世界沙盘设计方案.md` §8.2（关键节点 gating，单 tick <2% LLM）。

## 现状（已具备的底座）
- `session/threat.go::ResolveEliteEncounter`：单人 elite 全链路（反射护栏先保命 → `combat_roll` 多回合确定性消耗战
  → 胜利 `encounter.AllocateLoot` 分赃 / 失败 `DegradePenalty` 后果分级闸 → 经 `status.Mutator` 留痕 → 祖魂语气
  命运卡），**对 session State 依赖极轻**（仅 `state.ID` + `state.TurnState.Turn`）。
- `session/threat.go::TriggerEliteEncounter(ctx, sessionID, unitID)`：已是离线友好签名（内部 `state := State{ID: sessionID}`），
  可直接作注入式 handler。
- `engine/decision.Router.Route(Situation) Decision`：L1 安全反射（HP<25% → `ActionFlee` 撤退保命，零 LLM）→
  关键节点闸（`StrategicFork` 等 → `NeedsLLM`）→ 日常反射。纯数据、确定性、可测。

## 架构决策
1. **注入式 threat handler，保持 regionrunner 不依赖 session**（与 `execGuard`/`costEstimate` 同模式）。
   `Runner.SetThreatHandler(func(ctx, sessionID, unitID) error)`；main 注入 `session.TriggerEliteEncounter` 的包装。
   regionrunner 只负责「何时该撞威胁」，session 负责「遭遇怎么打」。
2. **威胁刷新 MVP 用简化确定性概率**，而非完整的 `region.threat_level` 累积 + 锚加权选址。
   完整版（`threat_spawn_score = 0.5·threat_level/100 + 0.3·anchor_density + 0.2·freshness` + `arbitration` 选址、
   field_boss/world_boss provenance 链、dungeon 分段）依赖 region 威胁度状态表，**登记为后续**。MVP 先把「elite 单 tick
   遭遇接入主循环」跑通。
3. **只对当前 HOT（正活跃）单位 roll 威胁**：活跃单位才「在路上、在事件里」，沉寂单位不折腾——既符合语义，也天然限频
   （HOT 单位 ~每 TickSeconds 唤醒一次），呼应「关键节点稀疏、单 tick <2%」。
4. **decision.Router 先保命**：roll 命中后过 `Router.Route`，HP 危急（<25%）→ `ActionFlee` **撤退、不应战**（离线单位
   不送死，贡献保留语义留给完整版），计 `threats_fled`；否则关键节点 → 触发遭遇。
5. **flag-gated**：`QUNXIANG_REGION_RUNNER_THREATS` 默认关。

## 接入点（applyAmbientL1）
过完让位检查（`execGuard`/`InCombat`，含改前复查）+ 读 record 之后、选 ambient action **之前**：
```
currentTier := ClassifyTier(tick, lastActive)
if threatsEnabled && currentTier==HOT && rollThreat(sessionID, unitID, tick):
    threats_rolled++
    dec := threatRouter.Route(situationFrom(record))   // StrategicFork=true
    if dec.Intent.Action == ActionFlee:                # HP 危急 → 撤退保命
        threats_fled++; return HOT, reschedule          # 本次唤醒用于规避
    threats_encountered++
    if threatHandler != nil:                            # PvE-2 真触发；PvE-1 shadow=nil 只计数
        if execGuard(sessionID): deferred++; return HOT  # 触发前再查让位收窄并发窗口
        if err := threatHandler(ctx, sessionID, unitID); err != nil:
            encounter_errors++; log.Warn(...)
    return HOT, reschedule                               # 遭遇消耗这次唤醒
# 否则走日常 ambient（forage/rest/socialize/reflect）
```

## 分步
- **PvE-1（shadow）**：威胁 roll + `decision.Router` 闸 + 遥测，`threatHandler==nil` **不真触发**（只计 would-encounter）。
  验证 roll 的确定性/分布、L1 护栏先保命、与 ambient 主链路的衔接。flag 默认关。
- **PvE-2（真触发）**：main 注入 `session.TriggerEliteEncounter` 包装，命中真跑 elite 遭遇（改 HP/钱包、分赃/惩罚、命运卡）。
  触发前再查让位收窄并发窗口；评估/登记 `ResolveEliteEncounter` 改 `ApplyOptimistic`（与 real-3-0 一致）为并发硬化。
  SQLite + 真 MySQL 端到端验证。

## 遥测（/healthz region_runner）
`threats_enabled` / `threats_rolled`（roll 命中次数）/ `threats_encountered`（升级为遭遇）/ `threats_fled`（HP 危急撤退）/
`encounter_errors`（真触发失败）。

## 红线（沿用）
威胁 `power/hp_pool/Threshold` 是配置常量、付费不进（与受保护字段同级）；遭遇 `combat_roll` 确定性、付费全盲；
受保护字段经 `status.Mutator` 留痕；失败 `DegradePenalty` D0-D3 硬锁不可逆（lives 永不归零）。
