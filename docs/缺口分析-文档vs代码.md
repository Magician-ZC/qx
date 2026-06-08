# 文档 vs 代码 缺口分析（2026-06-08，7 份核心设计/产品文档逐项核实）

> 由 7 个 Opus agent 各通读一份文档 + grep/Read 核实代码生成。状态：缺失=完全没有；部分=有原语未接主链路。〔已登记〕指开发进度.md 路线图已列。

I now have enough evidence. `ResolveFateDecision` only writes a `DECISION_RESOLVED` marker — it does NOT apply any actual consequence to the unit (no Mutator effect, no echo-able outcome), and the 3 generic resolve types (`let_her`/`urge`/`acknowledge`) don't map to the doc's Copilot 2 contextual options with consequence outcomes. I have comprehensively verified all the key items. Let me write the final report.

## 设计宪法-角色命运开盒.md 缺口报告

总体结论：命运开盒的**确定性原语层**（relevance 路由、attribution 校验、consequence 分级闸、villageseed、attachment、arbitration 反 P2W）基本齐全且有测试，但**前台核心闭环大面积断裂**——FateScore 公式从未实现（只用裸 relevance 路由）、20 人关系网/秘密/红线宪章/门控意外/抗命溯源卡/Copilot 决策后果/血脉接管/灵魂共乘等关键体验要么是「原语未接主链路」要么完全缺失，且 onboarding UI 向玩家承诺了并不存在的内容（「身边已有二十个人」）。

### P0 阻塞核心体验/红线

- [状态: 缺失] **出生即 20 人关系网（§4.5）** — 文档要求出生确定性生成 20 人 NPC（家人/朋友/债主/暗恋/仇人）+ 关系网；代码现状 `session/village.go:27 SeedVillage` 已实现并能落库村民+人格+四轴关系+锚，但 **全仓非测试代码无任何调用点**（grep `SeedVillage` 仅命中定义与测试），真实造人走 `bootstrapCharacter`→ `service.go:184` 只建 1 个玩家单位；缺口=「密度而非面积」的地基存在但从未接入任何 onboarding 路径，玩家角色身边实际是空的；〔未登记——开发进度.md 未列 SeedVillage 接入为 TODO〕

- [状态: 缺失] **onboarding 虚假承诺 20 人** — 文档 §4.5 要求关系网真实存在；代码现状 `frontend/src/fate/FateApp.tsx:113` 预览页硬编码文案「她身边，已有二十个有名有姓、有恩有怨的人」，但 `create()`（FateApp.tsx:47-86）只调 `bootstrapCharacter` 建单角色、不触发任何村庄生成；缺口=UI 明确向玩家撒谎，体验与宪法第一支柱（「这是我的人 / 她想要什么」需有具体关系答案）直接冲突；〔未登记〕

- [状态: 部分] **FateScore 准入门槛（§4.2，原则 10）** — 文档要求 `FateScore = 不可逆度 × 牵挂相关度 × 情绪强度` 三因子相乘后三档路由；代码现状 `session/fate.go:132` 直接用 `relevance.RouteFor(rel)`，而 `rel` 仅是 `Importance/10` 或关系锚 relevance（fate.go:119-131），**「不可逆度」「牵挂相关度」两个因子从未进入打分**（grep `FateScore`/`不可逆度`/`牵挂相关度` 全仓 0 命中）；缺口=路由维度退化为单一相关性，宪法明确的「准入门槛=三因子」未落地；〔未登记〕

- [状态: 缺失] **触红线/不可逆事件强制进待决策 + 每日 ≤3 预算（§4.2 硬规则）** — 文档要求触红线或不可逆后果一律强制 PENDING，且每自然日待决策 ≤3、溢出降级高光卡；代码现状 `session/fate.go:132-151` 纯按分数路由，无红线强制升档、无 `dailyBudget`/`maxPendingPerDay`（grep 0 命中）；缺口=防轰炸与红线保护两条硬规则均无实现；〔未登记〕

- [状态: 缺失] **门控意外硬绑前因（§5.3，原则 4 红线）** — 文档要求 romance/sell_pinned/defect 即使归因「看似合理」也必须命中专属前因否则连选项都不生成；代码现状 `engine/decision/attribution.go:242 GateSurprise` 已实现全部三类谓词，但 **session 主链路无任何调用**（grep `GateSurprise`/`GateCheck` 在 `internal/session/` 0 命中）；缺口=「突然恋爱/卖传家宝/叛变」的硬前因门控是纯原语、未接 `generateUnitDecision`，与 §8「在 resolveDirectiveCompliance 前调用」的承诺不符；〔未登记〕

- [状态: 缺失] **抗命溯源卡 + 成长旁白（§3.3）** — 文档要求任何 refused/兜底 UI 强制附「她为什么没听你」溯源卡，归因落到五来源之一，末尾固定旁白「她第一次没有照你说的做。她在变成她自己。」并写入编年史人物弧；代码现状 `session/obedience.go:386 refusedDecision` 只把 reason 塞进 `Reasoning` 字段转 hold，**无溯源卡、无固定成长旁白、无编年史人物弧节点**（grep 旁白原文 0 命中）；缺口=祖魂身份最核心的叙事化（抗命=成长而非 bug）完全缺失；〔未登记〕

- [状态: 缺失] **OOC_REJECTED 审计事件（§5.2 回退要求）** — 文档要求归因判 OOC 时写 `OOC_REJECTED` 审计事件并改走 safeFallback；代码现状 `attribution_bridge.go:259 oocFallbackDecision` 返回 hold 决策，但 **从不 emit OOC_REJECTED**（grep `OOC_REJECTED` 在非测试代码 0 命中，reason code 虽登记但无发射点）；缺口=OOC 回退不留可审计痕迹，遥测只有进程级计数、无事件流证据；〔未登记〕

- [状态: 部分] **待决策=Copilot 2 选项 + 后果分级标签 + 实际后果落地（§4.6 槽 C / §6 Q6）** — 文档要求待决策给「情境 + 倒计时 + Copilot 2 选项（标后果分级）+ 不处理会怎样」，处理后产生真实下游后果；代码现状 `FateView.tsx:159-169` 是 3 个写死的通用按钮（由她去/疾呼拦住/默默看着），无倒计时、无后果分级标签、无兜底预告；`session/fate.go:281 ResolveFateDecision` 只写 `DECISION_RESOLVED` 标记，**不经 Mutator 施加任何实际后果、不生成 echo-able 结果事件**；缺口=「待决策」是空壳——选了等于没选，宪法的「真正伤筋动骨的命运节点」无实质；〔未登记〕

### P1 重要

- [状态: 缺失] **过期兜底 + 「你没回来，于是她自己选了」回响卡（§4.2 修正 §11.6）** — 文档要求倒计时到按宪章兜底，且结果与玩家倾向相反时事后给一张回响卡（修「安全网杀死损失厌恶」）；代码现状 fate 收件箱无倒计时/过期机制（仅 `consent_gate.go:118 ExpireStaleConsents` 用于七交互同意、与命运待决策不同路径），grep「你没回来」「于是她自己选了」0 命中；缺口=损失厌恶修正完全未实现；〔未登记〕

- [状态: 缺失] **把归因因果句暴露给玩家（§5.4 + §4.6 槽渲染）** — 文档要求高光卡/待决策渲染 `NarrativeZH` 因果句「{角色}{动作}，因为{primary.snippet}」、supporting 折叠「为什么?」、memory 给「回到那一刻」链接；代码现状 attribution `NarrativeZH` 在 `attribution_bridge.go` 被校验/存储，但 `OpenFateFeed`/`fateCard`（fate.go:392）渲染的是祖魂换皮文案，**不含 attribution 因果句**；`FateView.tsx` 卡片只显示 `narrative` 单串、无「为什么?」折叠、无溯源链接；缺口=「意外但合理」的「因」对玩家不可见，归因校验沦为纯后台；〔未登记〕

- [状态: 缺失] **轻交互 surprise 校准回写（§5.4 + §8 GDD §8 接口）** — 文档要求高光卡轻交互〔意料之中/有点意外但合理/太离谱〕回写 surprise 校准作惊喜命中率采集口；代码现状 `attribution.go:35 SurpriseLevel` 仅 LLM 单向产出，无玩家回写端点/UI（grep `意料之中`/`太离谱`/`surpriseFeedback` 0 命中）；缺口=惊喜命中率采集闭环缺失；〔未登记〕

- [状态: 缺失] **离线宪章红线作归因前因（§4.1 红线 0.28 + §5.1 redline cause）** — 文档红线占 relevance 权重 0.28、是 6 类 cause 之一；代码现状 `relevance.go:28 Redline:0.28` 常量在，但 `attribution_bridge.go:151 Redlines: map[string]string{}` **恒为空**、`buildRelevanceAnchors` 从不 upsert Redline 锚（village.go 只 upsert DebtGrudgeLove/Goal）；FateApp 的红线只作 free-text `player_intervention` 发出、不解析成结构化红线条目；缺口=红线既不进相关性也不进归因，宪法多处依赖的红线维度全线断裂；〔已登记——开发进度.md §三「归因 redline 类前因：等离线宪章落地后接入」〕

- [状态: 缺失] **后果分级闸第三条 AND「已发生 ≥1 次层2」（§4.3）** — 文档层3 解锁需牵挂≥70 且在世≥7 天 且 已发生≥1 次层2（三条 AND）；代码现状 `encounter.go:179 PenaltyCap` 注释明言第三条「由调用方追加」，但唯一调用方 `threat.go:614 DegradePenalty(candidate, care, state.TurnState.Turn)` **从不追加第三条**；缺口=层3 实际只靠「elite 候选层=1」物理挡住，分级闸的第三条 AND 形同虚设（一旦有层3 候选即可能误开）；〔未登记〕

- [状态: 缺失] **后果分级闸 provenance 可追溯断言（§4.3）** — 文档要求任何层2/层3 后果落地前必须携带 `provenance` 链（指向人格/记忆/红线/关系/压力之一），否则降级层1或丢弃；代码现状 `threat.go:609 applyDefeatPenalty` 直接按 layer 施加士气挫伤、无 provenance 校验（grep `provenance` 0 命中）；缺口=「她叛变必须答得出为什么」的代码强制未落地；〔未登记〕

- [状态: 缺失] **死亡→血脉接管入口（§6 Q1 + §3 传承）** — 文档要求 Q1 已死时给「死亡传记卡 + 血脉接管入口」，祖魂随血脉延续接管后代；代码现状 `frontend/src/fate/` 无血脉/接管 UI，后端无 lineage-takeover 流程（grep `血脉接管`/`heir`/`bloodline` 0 命中）；缺口=祖魂身份赖以成立的「角色死亡接管后代不破设定」未实现，角色死=玩家人生终结；〔未登记〕

### P2 增强

- [状态: 缺失] **身份三句锚 + 第一次抗命预埋卡（§3.1）** — 文档要求开场 10 秒第一屏写死「我是什么/能做什么/不能做什么」三句，快照末尾强制弹「第一次抗命预埋卡」+ 黄字「这是她，不是你。」；代码现状 `FateApp.tsx` onboarding 只有捏人表单 + 预览页，无三句身份锚、无抗命预埋卡（grep「这是她，不是你」「第一次抗命」0 命中）；缺口=D0 第一屏未把「她会不听」设为已知规则；〔未登记〕

- [状态: 缺失] **灵魂共乘（直控特例，§3 D1 + §7 养成奖励）** — 文档要求直控降为「罕见、有代价的灵魂共乘」作为非 P2W 养成奖励；代码现状无直控/灵魂共乘机制（grep `灵魂共乘`/`soulRide`/`directControl` 0 命中），玩家干预仅 `recordPlayerIntervention` 托梦文本；缺口=祖魂身份的「直控特例」维度缺失；〔未登记〕

- [状态: 缺失] **translation_templates 数据驱动翻译层（§4.1）** — 文档要求 data-driven `translation_templates`（战争·征召/贸易·欠债/势力·投靠 三类模板把系统事实改写成对她的关系/压力变化 + 加 worry 压力位 + hook）；代码现状 `fate.go:394 fateCard` 走 `narration.BeatWithAnchor` 程序化换皮，无 `translation_templates` 表、不新增压力位/hook（grep `translation_template` 0 命中）；缺口=翻译层只换语气、不产生真实关系/压力变化；〔已登记——开发进度.md §三「翻译模板矩阵 translation_templates」〕

- [状态: 部分] **相关性五维只接通 3 维（§4.1）** — 文档 relevance=熟识0.32+红线0.28+目标0.18+债仇爱0.14+地理0.08；代码现状权重常量齐全（relevance.go:27-32），但 `buildRelevanceAnchors`（fate.go:53-85）实际只产 Relation 锚 + village.go（未接入）的 DebtGrudgeLove/Goal 锚，**Redline 与 Geo 锚从无 upsert 点**；缺口=红线/地理两维相关性恒为 0，相关性评分系统性偏低；〔部分登记——redline 已登记，geo 未登记〕

- [状态: 缺失] **Secret 持久化（§4.5）** — 文档要求每段关系 ≥1 个 Secret（玩家知道、村里多数人不知道）；代码现状 `villageseed.go:211` 生成 Secret 字段，但 `village.go` 落库时**只写 Biography/锚，Secret 既不入任何 unit 字段也不入表**（village.go 无 Secret 引用）；缺口=秘密信息维度丢失，「玩家知道而村里不知道」的信息不对称玩法无数据支撑；〔未登记〕

- [状态: 部分] **SeedMemory 未进记忆库（§4.5 强制写入）** — 文档要求每段关系强制写入 ≥1 条 SeedMemory 进她的记忆库；代码现状 `village.go:50` 只把 SeedMemory 拼进 Biography 文本，**不写入 memory_store 成结构化 memoryRow**；缺口=SeedMemory 无 Importance/EmotionWeight/salience，无法被 §5.1 attribution 的 `kind=memory`（需 `Importance≥6 且 salience>0.15`）引用；〔未登记〕

- [状态: 缺失] **回响 Echo 完整双向（§6.2 负向 + valence）** — 文档要求 EchoCard 带 valence、负向回响（「上次你强迫她杀俘虏，这次她第一次犹豫了」）；代码现状 `session/echo.go` 已实现 order_echo 引用与正向回响卡，但 grep 未见负向 valence 分支与「犹豫」类负向模板，FateView 回响带只渲染 narrative 无 valence 色；缺口=回响只有正向、缺负向情绪色；〔已登记——M3 回响 Echo 标记完成，但负向/valence 渲染未单列〕

========================================

I now have comprehensive evidence across all the key checkpoints. The world tick is per-world (in `world.AdvanceTick`), not per-region as the design specifies — but region-runner derives tick from real Unix seconds / TickSeconds, a third model. Template stitching exists for the opening candidate generation (pregame.go) but there's no `CharacterTemplate`/archetype/ambition-vector system as designed. Let me compose the report.

## 大世界沙盘设计方案.md 缺口报告

总体结论：地基（确定性结算原语、worldbus/world 注册表、冷热分层调度原语、region-runner 离线自治循环、events 双键、state_json 拆表、prompt 缓存）已扎实落地，但**§3 模版系统、§4 三层动机栈的 L2/野心向量/人格漂移、§5 离线宪章/冻结清单/四种接管模式/胆量曲线、§6 编年史物化视图、§7 天命点算力账户**等「前台玩法与世界发动机的中间层」几乎全部未实现——当前 region-runner 只跑 {觅食/休息/社交/反思} 四动作反射循环，与文档设想的「动机栈驱动的自由谋生/结盟/复仇」相距甚远；§8.7 新表清单约半数未建。

### P0 阻塞核心体验/红线

- [状态: 缺失] **三层动机栈 L2「当前目标」(reassessGoal)** — 文档 §4 要求每 24 tick 或重大事件触发轻量 LLM `reassessGoal` 产出≤60字短目标、存为高显著度知识记忆，是「自由发展」涌现的中枢；代码现状 `engine/decision/decision.go:67` 的 `CurrentGoal` 只是反射层「沿用既有目标」的 passthrough，全仓无任何 `reassessGoal`/目标重估调用（grep 仅命中 relevance 的 `Goal` AnchorKind）；缺口 角色没有自发漂移的中期目标，region-runner 的离线动作空间是写死的 {觅食/休息/社交/反思}，"vengeance 高→漂向接近仇人"这类涌现无从发生；〔未登记〕

- [状态: 缺失] **野心向量 Ambition（6维）** — 文档 §3/§4 L3 要求 `Profile.Ambition={power,vengeance,wealth,lineage,mastery,freedom}` 六维 ∈[0,1]、出生固化、终生漂移≤0.15，作为一切自发行为的引力源；代码现状 `unit/personality.go:19` 仅有**单标量** `Ambition float64`（8 维人格之一），无六维向量、无固化/漂移约束、不进决策 prompt 作引力；缺口 L3 长期动机层缺失，角色行为没有「北极星」；〔未登记〕

- [状态: 缺失] **离线宪章 Offline Charter（Directive.Scope）** — 文档 §5.1 要求 Directive 加 `scope="offline_charter"`，含长期目标/红线禁令/社交授权三段，离线自治据此续命；代码现状 `session/types.go:189` 的 `Directive` struct 无 `Scope` 字段（仅 ID/Turn/Phase/Kind/Text/Priority/Target/AppliesTo），§8.7 列的「directive 加 scope」未做；缺口 离线自治没有玩家可设的章程，region-runner 反射循环不读任何 charter；〔未登记（"离线宪章落地"在进度.md仅作为归因 redline 的前置被提及，未作为独立条目排期）〕

- [状态: 缺失] **冻结清单 Freeze List（高代价动作上交收件箱）** — 文档 §5.3 要求离线决策产出落在冻结集（死战单挑/迁出区域/卖 pinned/生育/结盟背叛/触红线）的 action 不落地、改发 `PENDING_DECISION` 入收件箱、本 tick 走 Hold；代码现状 region-runner（`regionrunner.go`）只在四个安全动作间选，从不产出高代价动作、无冻结判定；`PENDING_DECISION` 仅由 `session/fate.go:148` 的命运相关性路由发出（"她在乎的事"），**不是**离线自治冒出危险动作时的拦截；缺口 离线"灾难性后果"防护（§9 风险登记册首要缓解项）未实现；〔未登记〕

- [状态: 缺失] **inventory item `pinned` 字段** — 文档 §5.3/§7.2 红线要求 pinned（传家宝）物品永不自动卖、卖赠须上交玩家；代码现状 `engine/decision/attribution.go:204-259` 的 `GateSurprise` 有 `ActionSellPinned` 门控逻辑（返回 `PINNED_PERMANENT`/`SELL_PINNED_NEEDS_PLAYER`），但**物品数据结构无 `Pinned` 字段**（grep 无 InventoryItem.Pinned），§8.7「inventory item 加 pinned」未做；缺口 门控原语存在但无数据可判，红线"pinned 永不自动卖"无法落地；〔未登记〕

- [状态: 部分] **付费频率/反应类杠杆的「仲裁 tick」解耦（P2W 根红线）** — 文档 §7.1/§11.3/§9 反复强调：离线 tick 频率 + 反应速度插队在零和节点 = P2W（红队测纯频率差胜率 ~83%），上线前**必须**做"零和结果统一节奏仲裁 tick 裁决 + 插队账号级封顶"；代码现状 `engine/arbitration/arbitration.go` 提供了胜率∝Score/频率无关的零和裁决原语（贸易抢单/伏击/求偶等竞速场景尚未接入它），且**天命点付费档骨架本身未实现**（见下），故红线暂未被违反；缺口 一旦上线频率类付费而竞速节点未走 arbitration，即触 P2W 根红线——属"原语就绪、竞速主链路未接 + 账号级插队封顶未做"；〔部分登记（arbitration 原语已登记，竞速节点接入与封顶未登记）〕

### P1 重要

- [状态: 缺失] **角色创建模版系统（§3 CharacterTemplate / 6–8 指定原型 / 随机模版自洽约束矩阵）** — 文档 §3 要求统一 `CharacterTemplate{ArchetypeID,Origin,Motive,Wound,TalentTags,PersonalityBias,SeedMemories,Ambition...}` + `ApplyTemplate(record,tmpl,seed)`，6–8 手调原型（游侠/贵族/商人/刺客…）+ 随机模版（出身×动机×伤痕×天赋抽签 + 兼容性权重矩阵 0.2/1.0/3.0 + LLM 缝合 160–220字）；代码现状 无 `CharacterTemplate`/`ApplyTemplate`/原型档案；`villageseed/villageseed.go:107` 有 7 个 `archetypes`（猎户/铁匠之女…）做"出生即20人关系网"，但那是村庄种子、非玩家可选模版；`pregame.go:181` 用 `TaskBackstory` 生成开局候选，无伤痕/动机/兼容性约束矩阵；缺口 玩家"选模版创建角色"这一核心循环入口（§0 第一步）缺失；〔未登记〕

- [状态: 缺失] **人格漂移机制（PERSONALITY_DRIFT 应用）** — 文档 §5.7/§4.2/§9 要求接管经带 `PERSONALITY_DRIFT` reason-code 的小步调节器调 Personality（每次≤0.03、单日单维≤0.10），衰老/经历也漂移野心；代码现状 reason-code 已登记（`events/reason_codes.go:77,124`）但**全仓无任何地方发射或应用它**（grep `PERSONALITY_DRIFT` 除 reason_codes.go 外零命中），无 ≤0.03/≤0.10 步长约束的调节器；缺口 "你怎么对她塑造她变成谁"的性格闭环（设计宪法核心情感）未接通；〔未登记〕

- [状态: 缺失] **生命循环非战斗死亡：衰老 / 饥荒累积死亡 / 自然死亡掷骰** — 文档 §4.2 要求 `Identity.Age` 随 tick 增长（每240 tick +0.2岁）、过高龄阈值后每 tick 递增 FNV 确定性自然死亡掷骰，`StarvationTurns`/`Injuries` 累积触发死亡；代码现状 `hunger.go:204` 有"背包有口粮却饿死"的硬兜底但**无衰老/年龄随 tick 增长逻辑**（`Identity.Age` 仅在 BootstrapRecord/造子时设值，从不递增），无高龄死亡掷骰；`CHARACTER_DIED` reason-code 已登记（`reason_codes.go:74`）但只接战斗致死；缺口 "死亡=留存钩子+血脉传承"的非战斗触发路径缺失，世界无新陈代谢；〔未登记〕

- [状态: 缺失] **四种接管模式之 ②附身/直控 ③Copilot共同决策 ④复盘** — 文档 §5.5 要求四模式；代码现状 仅 ①指令模式（`SetFactionDirective`）+ 部分接管留痕（`echo.go:46` `RecordPlayerIntervention` 写可引用事件，HTTP `/intervene`）落地；**②直控**（`forcedOrder`/`activeImmediateOrderForUnit` 放行路径在 `obedience.go:84` 存在，但无"局部时停 + 逐步指定 action + 指挥专注/每日免费时长上限"的接管 UI/逻辑）、**③Copilot**（LLM 生成 2–3 候选+理由+风险分供选）、**④复盘**（编年史时间线追认/纠偏/补救）均未实现；缺口 §10 MVP 明列"①指令+④复盘最便宜两个先做"，④复盘缺失；〔未登记〕

- [状态: 缺失] **自治胆量曲线：广度 Autonomy(t)↑ + 风险 offlineCaution↓** — 文档 §5.2 要求两条曲线：可自决领域随离线时长解锁（0-6h/6-24h/>24h 三档动作集），`offlineCaution=1+0.4·log2(1+offlineHours)` 乘进 `rejectProbability`；代码现状 `obedience.go:100` 的 `rejectProbability` 公式无 offlineCaution 因子、无 offlineHours 概念、无广度解锁档；缺口 "越久越自主但越保守"的离线张力机制缺失；〔未登记〕

- [状态: 缺失] **区域聚合器（个体↔宏观闭环）** — 文档 §4.1 要求每 Hot Region 每 24 tick 跑一次（规则+1次廉价 LLM 摘要）：势力自组织聚类→人口/财富/战损统计→写 `memoryCategorySpatial`"天下大势"→触发宏观事件（`FACTION_COLLAPSE`）下放为 L2 目标；代码现状 `FACTION_COLLAPSE` reason-code 已登记（`reason_codes.go:76`）但无聚合器、无势力聚类、无宏观→个体下放，全仓无"区域聚合"实现；缺口 "本势力衰退该跳船→叛变涌现"的宏观闭环缺失；〔未登记〕

- [状态: 缺失] **编年史物化视图 `chronicle_entries` / 多视角不可靠叙事 / 高光卡管线** — 文档 §6/§11.4 要求 `chronicle_entries`（带 `source_event_ids` 证据链 + `perspective_unit_id` 多视角）+ 四层叙事聚合 + 幻觉回扫 NER + §11.4 高光卡（顶层覆盖99%、3秒读完）；代码现状 无 `chronicle_entries`/`chronicle_cache` 表（schema.sql 中无）、无多视角渲染、无高光卡管线；§8.7 列的 chronicle 表全缺；fate 收件箱有「高光卡」reason-code（`ReasonInboxHighlight`）但那是命运卡、非编年史物化视图；缺口 §11.4 标 P1"编年史去长文化（高光卡为主）"，整套叙事物化层缺失；〔未登记〕

### P2 增强

- [状态: 缺失] **§8.7 新表清单约半数未建** — 文档 §8.7 列 16+ 新表；代码现状（schema.sql）已建 worlds/world_members/cross_events/decision_traces/llm_interactions/raw_event_log/agent_wake_queue/agent_decision_jobs/social_objects/consent_requests/product_events/relevance_anchors；**未建** `regions`、`world_ticks`、`directives`、`dialogue_messages`、`battle_reports`、`moderation_reports`、`chronicle_entries`/`chronicle_cache`/`chronicle_archive`、`llm_budget_accounts`、`llm_charges`；缺口 region 作用域至今 ==session（`agentqueue.go:22` "RegionID 必填 MVP==sessionID"），无独立 regions 表/per-region tick；〔部分登记（events 双键 M6.1 已做，regions/chronicle/budget 表未登记）〕

- [状态: 部分] **天命点 Mana of Fate / 每角色算力账户 / 分 tier 模型路由** — 文档 §7 要求把 `llm_budget.go` 护栏升级为玩家可见"天命点"（1天命=$0.001）、`llm_budget_accounts`+`llm_charges` 流水、`BudgetGate` 按 character→user→region→world 逐级扣、`ConfiguredTaskProfilesByTier` 免费 DeepSeek/付费 OpenAI 分档；代码现状 有进程级/会话级预算护栏（`llm_budget.go`）+ region-runner 离线成本闸（`ambient_llm.go` micro-USD atomic + `QUNXIANG_REGION_LLM_BUDGET_USD` latch）+ 成本基准 `cmd/costbench`，但**无玩家可见天命点、无 per-account 表、无 BudgetGate 多级扣减、无 ConfiguredTaskProfilesByTier 付费分档**；缺口 §7 商业化骨架（MVP 第8项"天命点+免费/付费档骨架+月卡"）未落地；〔未登记〕

- [状态: 部分] **<2% 上 LLM 成本控制：从影子转真短路、关键节点 gating** — 文档 §1.5/§8.3 要求真实 LLM <2%、关键节点 gating（HP<30%/新order/首次遭遇/贸易恋爱生育/玩家在场）才上 LLM；代码现状 `decision.Router`（三层模型）+ 反射影子（`reflex_shadow.go`）+ 真短路 flag `QUNXIANG_REFLEX_SHORTCIRCUIT`（`llm.go:208`，默认关，仅短路 hold/continue 安静 tick）+ region-runner HOT-LLM 离线决策（`ambient_llm.go`）均已落地；但短路只覆盖"安静 tick"，**完整的关键节点 gating 白名单（首次遭遇/贸易/恋爱/生育判据）未在 session 执行主循环成体系实现**，且短路默认关；缺口 <2% 目标的 gating 维度尚未在主链路完整接入、默认未开；〔已登记（reflex_shortcircuit 已登记并标 done，但完整 gating 白名单未登记）〕

- [状态: 部分] **世界 tick 模型与文档不一致（per-region 逻辑时钟 vs 三套并存）** — 文档 §2.2 要求 per-region 逻辑 tick（1 tick=真实6分钟、Hot 5-10s/Warm 600s/Cold 不tick），ATB gauge 累积到阈值才唤醒；代码现状 存在**三套互不对齐**的 tick 概念：`world.AdvanceTick`（`world.go:128`，per-world 单调发号器，给 cross_events 排序用）、region-runner `currentTick=真实Unix秒/TickSeconds`（`regionrunner.go:9`，真实时钟派生）、`turns.State.Tick`（对局内回合）；唤醒用 `scheduler.ClassifyTier`（空闲 tick 数分层，`regionrunner.go:357`）而**非** ATB gauge；缺口 文档设想的"per-region 逻辑时钟 + ATB gauge 唤醒 + 6分钟固定映射"未统一实现，现为多模型拼接；〔部分登记（region-runner 调度已登记，tick 模型统一未作为债务登记）〕

- [状态: 部分] **血脉传承（全资产+记忆继承+无缝接管后代）** — 文档 §4.2 要求死亡触发遗产+未竟 `LifeGoal` 传继承人、"父辈遗志"作最高 importance 永久记忆注入、玩家化身死亡可无缝接管血脉后代；代码现状 `service.go:2696` `resolveKillLoot` + `combatdomain.NewLootInheritor` 做**战利品**继承（`InheritorID`），但无 LifeGoal 传承、无"父辈遗志"永久记忆注入、无玩家无缝接管后代流程；缺口 §0 核心循环"血脉传承←死亡/衰老"这一留存钩子仅做了物资层；〔未登记〕

- [状态: 缺失] **低耗叙事模式（超长离线 summary-LLM 压缩）** — 文档 §5.4 要求离线>24h 或预算触发时改"每叙事窗口（1世界日）调一次低成本 summary-LLM 把 N tick 压成一段"，token 降一个量级；代码现状 region-runner 离线决策按 HOT/反射逐 tick 走（`ambient_llm.go`），无"叙事窗口聚合压缩"模式（`memory_store.go` 的 `generateMemory2Compaction` 是记忆压缩、非离线 tick 聚合）；缺口 超长离线硬控成本的降级档缺失；〔未登记〕

========================================

I've confirmed that `applyRelationShift` does NOT upsert anchors — relation anchors are derived live in `buildRelevanceAnchors`, but the doc's §1.1 spec ("每次 status.Mutator 落事件就 upsert 锚 weight + 刷新 updated_turn") is not the live mechanism; non-relation anchors are only seeded at village generation, never refreshed on goal/redline/debt changes. This is a partial gap.

I now have enough evidence to write the report.

## 事件耦合与跨玩家关联.md 缺口报告

**总体结论**：问题 A（世界事件→角色命运）的相关性评分原语与命运三档路由已完整落地并接入主链路；问题 B（跨玩家）的总线/撮合/七种交互/consent_gate 也已写入闭环。但**全文最有特色的三块——关系图 N 跳传播(§1.4/§2.7)、双向 Worldize 出向探针(§1.3)、(reason_code×anchor_kind) 翻译模板矩阵(§1.2)——基本未实现**；锚体系只活在「实时关系锚 + 建村一次性种子」，非关系锚不随事件刷新；证据链 echo 表与跨玩家零和裁决也缺。下面只列真实缺口。

### P0 阻塞核心体验/红线

- [状态: 缺失] **关系图 N 跳传播 `Propagate`（§1.4 + §2.7 共享历史证据链）** — 文档要求事件命中锚后沿 `relations` 图 BFS 向「关心他的人」传播、每跳 `hop_fidelity ×= 0.6`、落 `propagation_log`(hop/fidelity/source_event_id) 使「越传越失真」成可仲裁数据，并据此生成 `CROSS_DERIVED`/`blood_feud`；代码现状：`relevance.go:97 HopFidelity`/`:152 StopPropagation` 仅为纯原语，全仓**无任何调用方**（grep `Propagate`/`propagation_log`/`CROSS_DERIVED`/`PROPAGATION_RELAYED` 均「未找到」），`WorldizeDeath`(fate.go:332) 只对死者的直接 mourners 做 1 跳、无 fidelity 衰减、不递归、不留传播痕迹；缺口：多跳传播、可信度失真叙事、共享历史证据链全缺。〔已登记：进度.md L101「关系图传播 + 共享历史证据链」未勾选〕

- [状态: 缺失] **双向世界化出向 `Worldize`（§1.3）** — 文档要求 `status.Mutator` 落事件后挂钩子：当 actor 是玩家角色且 `reason_code ∈ worldizing_codes`(背叛/救援/劫掠/倒地/社交/债务)，反查「谁的锚会被点亮」给他们落 `PROPAGATION_INBOUND` 探针并驱动 NPC 自治；代码现状：仅 `WorldizeDeath`(fate.go:332) 这一条出向路径（且只覆盖死亡），`engine/status` 的 Mutator **无任何 worldize 钩子**（grep status 包内 worldize=0 命中），`worldizing_codes` 表/常量「未找到」，`PROPAGATION_INBOUND`/`WORLDIZE_OUTBOUND` reason-code 未登记；缺口：救援/背叛/劫掠/社交/债务五类出向探针、入向自治驱动全缺，双向耦合只剩「死亡」一根。〔未登记：进度.md 仅记 WorldizeDeath 已接入，其余出向码无 TODO〕

- [状态: 部分] **翻译模板矩阵 `translation_templates`（§1.2 + 风险登记「模板覆盖不全」）** — 文档要求每个 `(reason_code × anchor_kind)` 配 data-driven 模板把系统事实改写成命运 beat（`COMBAT_DOWN×relation`→「老吴在北岭倒下…force_pending」等），缺模板回退 `DefaultReasonText` 并计入遥测排期补；代码现状：`fate.go:394 fateCard` 调 `narration.BeatWithAnchor(reason, anchorKind, …)` 是**硬编码 switch**（narration.go:144 仅对少数 reason 分支），无 `translation_templates` 表（grep「未找到」）、无 (code×kind) 全覆盖矩阵、无 `force_pending` 专属模板标记、无缺模板遥测；缺口：data-driven 模板表与全覆盖矩阵缺，现为有限硬编码。〔已登记：进度.md L68「翻译模板矩阵(translation_templates)：把 reason-code×anchor-kind 渲染成祖魂语气 beat」未勾选〕

### P1 重要

- [状态: 部分] **锚随事件 upsert/刷新（§1.1「锚不是静态」）** — 文档要求每次 `status.Mutator` 落一条改变关系/目标/债务的事件就 upsert 对应锚 weight 并刷新 `updated_turn`；代码现状：`anchors.go:40 UpsertAnchor` 只在 `village.go:76/82` 建村时一次性写 `debt_grudge_love`/`goal` 种子，关系变更路径 `relation.go:60 applyRelationShift` **不调 UpsertAnchor**（关系锚改为 `fate.go:65` 每次实时从 relations 派生），目标/红线/债务变更后非关系锚**永不刷新**；缺口：非关系锚（goal/redline/debt/legacy）的事件驱动 upsert 缺，`updated_turn`/事件溯源(source_event_id)列未在锚表(schema.sql:253 无此列)。〔未登记〕

- [状态: 缺失] **geo / redline / legacy 三类锚来源（§1.1 表 + §1.6）** — 文档列六类锚，`geo`=当前 RegionID(half-life~3d)、`redline`=离线宪章红线 ID、`legacy`=pinned 物/血脉项；代码现状：`relevance.go:13-20` 定义了六个 AnchorKind 常量，但实际写入只有 `DebtGrudgeLove`/`Goal`(village.go) + 实时 `Relation`(fate.go)——`Geo`/`Redline`/`Legacy` 锚**无任何写入点**(grep `relevance.Geo`/`relevance.Redline`/`relevance.Legacy` 在非 relevance 包=0)；缺口：地理/红线/血脉三类锚从未被点亮，「她所在 region 被劫」「触红线」类命运 beat 无法触发。〔部分登记：进度.md L67「归因 redline 类前因：等离线宪章落地后接入」涵盖 redline，geo/legacy 未登记〕

- [状态: 缺失] **`relevance_links` 证据链表（§1.6 + §3「为什么进收件箱」）** — 文档要求落 `relevance_links(event_id, player_unit_id, anchor_kind, anchor_ref, factor_weight, hop_count, hop_fidelity, relevance, …)` 与 `RELEVANCE_MATCH/INBOX_ENQUEUED` 留痕，使「点开看：因为他是你的密友(0.71)+断了你商路(0.4)」可追溯到具体锚命中；代码现状：表「未找到」，`fate.go:138-151` 入箱 payload 只存 `relevance`/`source_actor`/`reason` 标量，**不存命中的逐锚明细**，`ReasonRelevanceMatch` 已登记(reason_codes.go:57)但 `INBOX_ENQUEUED`/`ANCHOR_LIT` 等未登记；缺口：逐锚命中证据链不可追溯，前端无法渲染「凭哪几根锚」。〔未登记〕

- [状态: 缺失] **跨玩家零和裁决 `CROSS_CONTEST`（§2.6）** — 文档要求两玩家争排他结果(同一联姻对象/继承席位/战利品)走 `arbitration.Contest(Key=worldID+SO.id+tick, Score=各自投入)`、付费不进 Score、裁决 tick 统一结算、离线宪章兜底投入、蒙特卡洛红线 ±2%；代码现状：`arbitration.Resolve` 仅被 `social_match.go:55`（撮合择人）和 PvE 分赃调用，**无跨玩家排他争夺的 Contest**（grep `CROSS_CONTEST`/跨玩家 contest=0），`CROSS_CONTEST_WIN/LOSE` reason-code 未登记，无离线宪章自动投入；缺口：跨玩家零和争夺与其反 P2W 红线测试整块缺。〔未登记〕

- [状态: 缺失] **共享历史 echo 表 + 罗生门视角层（§2.7 + §3 `cross_event_echoes`）** — 文档要求双方 session 内只存 cross_event 的**翻译产物 echo**(`cross_event_echoes(session_id, owner_unit_id, cross_event_id, relevance, route, narrative_zh, valence, …)`)、三方 echo 都指向同一 `cross_event_id`、争议回退原表按 `occurred_at` 仲裁；代码现状：`cross_link.go:71 SurfaceCrossEventsForCharacter` 把跨事件直接翻成 `events` 表里的命运卡，**不写 echo 表、不保留 cross_event_id 反指针**(`cross_event_echoes` 表「未找到」)，cross_events 表(schema.sql:179) 也**缺 `prev_cross_event_id` 复仇/证据链反指针列**；缺口：「三个玩家都记得的那次背叛」的可仲裁证据链未落地。〔已登记：进度.md L101 同传播一并未勾选〕

- [状态: 部分] **跨玩家事件→收件箱的定时桥接/上线拉取（§2.5 + 落地阶段4）** — 文档预期 A 上线后收件箱里出现 B 造成的事件（祖魂语气自治回应）；代码现状：`SurfaceCrossEventsForCharacter`(cross_link.go:61) 已实现但**无任何生产调用方**（grep 仅测试与定义，httpapi/session/cmd 无调用），即跨事件不会自动流进对方收件箱，须外部手动触发；缺口：缺定时拉取 / 登录时桥接的调度接线。〔已登记：进度.md L80「剩余：…定时拉取桥接」〕

### P2 增强

- [状态: 缺失] **NPC「有意义事件」锚加权预算 + 破圈预算（§1.5）** — 文档要求后台 NPC 事件按「玩家锚密度」加权掷事件、单角色每日入向探针 ≤12 截断、每日 ≥1 件零锚来源低权事件强制进高光卡作新锚种子；代码现状：`anchors.go:24 AnchorDensity` 已实现并喂 region-runner **威胁刷新**(PvE-4)，但用于**世界事件生成预算**与「每日探针 ≤12 配额」「破圈 ≥1 零锚」均「未找到」；缺口：事件源头的锚加权调度与反信息茧房破圈预算缺。〔未登记（PvE-4 只覆盖威胁 spawn，非事件预算）〕

- [状态: 缺失] **撮合的 NPC 兜底 + 密度调节实算 + 每日 ≤2 冷却（§2.2 + §2.5）** — 文档要求撮不到时由后台 NPC 社会客体兜底、`MatchScore` 含密度调节项(活跃跨玩家关系越少越高)、每角色每日新绑定 ≤2；代码现状：`social_match.go:29 MatchIntoSocialObject` 接受**调用方算好的四因子**（注释明言「四因子由调用方按情境算」），`relevance.MatchScore`(relevance.go:187) 是纯公式，仓内**无密度调节的真实计算、无每日 ≤2 冷却、无 NPC 兜底产社会客体**(grep 0)；`social_objects` 表(schema.sql:353) 还**缺 `severity`/`expires_at`/`member_unit_ids`** 设计列；缺口：撮合的密度防垄断/冷却/兜底三项治理机制与表结构缺。〔未登记〕

- [状态: 部分] **七种交互的关系增量与文档表不一致 + 缺血脉/势力级效果（§2.3 表）** — 文档表规定「交易 arbitration 定价；违约 trust-4·rivalry+3」「联姻 affection+4/trust+3 + 血脉绑定 hook」「复仇被复仇方进收件箱」「开战 faction 级 rivalry+fear」；代码现状：`seven_interactions.go:37-45 sevenTemplates` 是**固定四轴增量**——交易无 arbitration 定价/违约分支、联姻无血脉绑定 hook(无 `CROSS_MARRIAGE` 血脉传承接线)、开战只改个人四轴非 faction 级、复仇未单独把被复仇方推进收件箱；缺口：交易动态定价、联姻血脉 hook、开战 faction 扩散为部分简化实现。〔未登记〕

- [状态: 缺失] **consent 超时分档兜底差异（§2.4）** — 文档要求超时按档区分：`REQUIRES_CONSENT` 自动失效 + 给 B 回响卡「她的命没有等到回应」，`CONTESTED` 按 A 离线宪章兜底自治回应；代码现状：`consent_gate.go:119 ExpireStaleConsents` 对所有 pending **一律置 expired、不应用、无回响卡、无离线宪章兜底回应**，不分档；缺口：超时的分档差异化兜底（回响卡 / 离线宪章自治回应）缺。〔未登记〕

补充说明（非缺口，仅记差异）：设计 §3 的 `fate_inbox` 独立表未建，命运收件箱改由 `events` 表 + `EmitProcessEvent` 流程事件旁路实现（fate.go:152、182），属等价实现选择，不计为缺口。`relevance` 原语、命运三档 `RouteFor`、`ConsentTierFor`、World Bus(`worldbus`/`world`)、社会客体存储、七种交互+consent 闭环、`AnchorDensity` 均已落地。

证据文件（绝对路径）：
- `/Users/zc/Desktop/py_project/qx/backend/internal/engine/relevance/relevance.go`（原语，HopFidelity/StopPropagation/MatchScore 无生产调用方）
- `/Users/zc/Desktop/py_project/qx/backend/internal/session/fate.go`（SurfaceFateEvent/WorldizeDeath/fateCard 硬编码）
- `/Users/zc/Desktop/py_project/qx/backend/internal/session/anchors.go` + `village.go:76,82`（锚仅建村种子）
- `/Users/zc/Desktop/py_project/qx/backend/internal/session/cross_link.go`（无 echo 表/无 prev 指针/SurfaceCrossEventsForCharacter 无调用方）
- `/Users/zc/Desktop/py_project/qx/backend/internal/session/seven_interactions.go` + `consent_gate.go` + `social_match.go`
- `/Users/zc/Desktop/py_project/qx/backend/internal/storage/sqlite/schema.sql:179,253,353,374`（cross_events/relevance_anchors/social_objects/consent_requests；缺 relevance_links/propagation_log/cross_event_echoes/translation_templates/prev_cross_event_id/severity/expires_at）
- `/Users/zc/Desktop/py_project/qx/backend/internal/engine/events/reason_codes.go`（21 个 spec 码仅 RELEVANCE_MATCH/INBOX_HIGHLIGHT/PENDING_DECISION/DECISION_RESOLVED/SOCIAL_OBJECT_BIND/ECHO_LINK/PLAYER_INTERVENTION 登记；PROPAGATION_*/WORLDIZE_OUTBOUND/ANCHOR_*/CROSS_CONTEST_*/CROSS_CONSENT_*/CROSS_DERIVED/INBOX_ENQUEUED 等未登记）

========================================

I now have a comprehensive picture. The grep for "传家" in relevance/decision are unrelated (anchor-kind matrix, not loot bequeath). No dungeon, no legacy bequeath, no consent/matchmaking/backfill on threats, no epic item materialized into inventory, no DB tables, no PvE reason-codes. Let me write the report.

## PvE威胁系统.md 缺口报告

总体结论：结算原语（贡献/分赃/惩罚闸/单人门）已完整落地并测试，单人 elite、组队 field_boss、异步世界 boss 三条主链路也跑通了"撞见→消耗战→分赃/惩罚→祖魂语气收件箱卡"；但**整套 ThreatEvent 生命周期/调度/数据持久化、参与意愿评估(§2)、dungeon(§3)、战利品的物品级落地（SoulBound/Legacy/传承/败者补偿入库）、撮合/补位/黑吃黑(§7)、provenance 链、和 §9 全部 PvE 专属 reason-codes / 数据表均缺失**——现有实现是"用通用 reason-code 跑确定性数学 + 投一张叙事卡"，物品/威胁状态几乎不入库。

### P0 阻塞核心体验/红线

- [状态: 缺失] **参与意愿评估 + 归因校验（§2）** — 文档要求威胁显形后对每个可达角色跑 `join_intent = w_amb·野心+w_rel·护短+w_goal·目标契合−w_fear·惧战−w_risk·后果层`，且加入决策必走 `ValidateAttribution`（cause∈persona/relation/goal/pressure），无源不许"她突然冲上去送死"，祖魂 advice 只作偏置项；代码现状：`session/threat.go`、`regionrunner/threat.go` 均**无 join_intent 计算、无 advice 偏置、遭遇加入完全无 `ValidateAttribution`**（grep 全仓 join_intent/joinIntent 零命中；`ValidateAttribution` 仅在 `attribution_bridge.go` 的常规决策链，未接威胁参与）；region-runner 只有"HP<25%→反射撤退，否则 StrategicFork→必遭遇"二分（`regionrunner/threat.go:139`），相当于强制参战。缺口：宪法 §5「意外但合理」红线在威胁参与路径上完全未守，怕死角色靠 HP 闸物理挡住但护短/野心/目标动机维度全无。〔未登记〕

- [状态: 缺失] **后果分级闸的层3 RequiresConsent + 超时降级（§6 红线/§11 异步致死）** — 文档要求层3（本人战死/pinned 丢失/血脉后果）必过 `ConsentTierFor(3)=RequiresConsent`：先冻结为待决策卡、需同意才落地、**超时未应答自动降级为残废而非阵亡**，且层3 须 care≥70 且在世≥7 且已发生≥1次层2（三条 AND）；代码现状：`encounter.PenaltyCap`（encounter.go:179）只校验前两条 AND，注释明说"第三条由调用方追加"，而**唯一调用方 `applyDefeatPenalty`（threat.go:610）未追加第三条、也未对层3 接 consent_gate/超时降级**，且实际惩罚永远只写 `FieldMorale`（threat.go:619），层2/层3 的致残/结仇/欠债/阵亡内容**一条都没实现**。缺口：异步致死防护链（最致命风险）只剩"HP<0.25 反射撤退 + elite 候选层硬钉=1"，老角色不可逆死亡的 consent + 超时降级安全网完全缺。〔部分登记：开发进度把 D0-D3 列为已守，但仅指 elite 层=1，层3 consent 路径未登记〕

- [状态: 缺失] **付费红线的代码层强制（§8）** — 文档要求"任何把付费接入 Score/掉落/惩罚的代码路径由 statuslint 同款 lint + arbitration『Score 来源白名单』在 CI 拦截"；代码现状：`arbitration`/`encounter` 设计上确实付费不进 Score（贡献来自 combat_roll 留痕），但**没有任何『Score 来源白名单』lint 或 CI 闸**（grep Score 来源/白名单/付费红线零命中；statuslint 只守受保护状态字段）。缺口：红线靠当前调用方自觉而非机制强制，运营悄悄把付费接 ContributionScore 无静态拦截。〔未登记〕

### P1 重要

- [状态: 缺失] **威胁数据模型全部 7 张表（§9）** — 文档要求 `threats / threat_participants / threat_encounter_rounds / dungeon_runs / loot_distributions / party_state / party_members`，其中 `loot_distributions` 明确"可仲裁审计、任何人用(参与集合,各自Score,Key)复算验证"；代码现状：schema.sql 里**只有 `world_bosses` 一张**（sqlite/schema.sql:224），其余六张**完全不存在**（grep 全部零命中）。`Threat` 是纯内存态（threat.go:54 注释"单人 elite 先用内存态，不落新表"），elite/field_boss 遭遇结束后**威胁、参与、每回合留痕、分赃结果全不持久化**，事后无法仲裁复算。缺口：可仲裁分赃的审计基础（loot_distributions + score_snapshot）缺失，违背 §5「任何人复算验证史诗归属」。〔部分登记：开发进度 §1 末"完整 ThreatEvent 容器/调度/副本推进属后续"〕

- [状态: 缺失] **§9 全部 PvE 专属 reason-codes** — 文档要求接入 `events.Catalog` 的 `THREAT_EMERGED/JOIN_AUTONOMOUS/JOIN_ADVISED/JOIN_DECLINED/HIT/PHASE/DEFEATED/WIPE/ALLY_DOWN`、`DUNGEON_ENTER/FLOOR_CLEAR/EXIT`、`ECONOMY_LOOT_ARBITRATED/CONSOLATION_MATERIAL/REGION_RAVAGED/GEAR_DAMAGED`、`COMBAT_MAIMED/FELL_IN_DEFEAT`、`LEGACY_BEQUEATHED`、`CROSS_PARTY_JOIN/LEAVE/WIPE`、`CROSS_CONTEST_WIN/LOSE`、`CROSS_BETRAYAL`；代码现状：reason_codes.go 里这些**一个都没有**（逐个 grep 零命中，唯一存在的 `CROSS_BETRAYAL` 是 worldbus EventKind 不是 events reason-code）。威胁链路复用通用码：受击=`COMBAT_HIT`、分赃=`ECONOMY_LOOT`、失败=`EMOTION_TRAUMA`、卡片=`INBOX_HIGHLIGHT`。缺口：威胁事件无法按 PvE 语义在事件流水里区分（"被精英打的伤"和"普通战斗伤"同码），破坏可审计/可归因粒度。〔未登记〕

- [状态: 缺失] **战利品物品级落地：SoulBound/Legacy/Durability/传承/败者补偿入库（§5/§9）** — 文档要求 `ItemStack` 扩展 `SoulBound/Durability/DurabilityMax/IsLegacy`，史诗装 `SoulBound=true`、Clutch 后升级 Legacy 锚 + `GateSurprise(sell_pinned)` Reject、角色阵亡 Legacy 沿最高 relation 锚找继承人 + 入名人堂 + 注入下一代记忆，败者补偿发"史诗合成碎片"；代码现状：`unit.ItemStack`（profile.go:125）**只有 ItemID/Quantity/CustomName/Level，无 SoulBound/Durability/IsLegacy**；epic 件（`boss_relic`/`world_boss_relic`）**从不写进任何单位 Inventory**，AllocateLoot 的 epic Award 只用于渲染叙事卡（threat.go:547），实际只有 gold 落 Wallet；`AwardConsolation` 在 encounter.go 算出但 session 层**从不消费**（grep Consolation 零命中于 session）；`LEGACY_BEQUEATHED`/bequeath 全仓不存在。缺口：胜利除了铜钱什么都拿不到，史诗装"认了她"纯叙事、败者补偿碎片完全没发、传承线（死亡→继承人→名人堂）未实现。〔部分登记：开发进度 §3 把"死亡传承/名人堂"列为剩余〕

- [状态: 缺失] **dungeon 全档（§1/§3）** — 文档要求副本=异步可推进的"一段专注遭遇"，切 N 个 segment 按 tick 离线 lazy catch-up，多数段反射自动结算、关键节点（floor boss FirstContact / 濒死撤退 / 岔路抉择）才进收件箱并暂停，超时按 charter 兜底；代码现状：`ThreatTierDungeon` **仅是一个 tier 常量**（threat.go:50）且仅在 `candidateDefeatLayer`（threat.go:138）出现，**无任何 segment/floor/dungeon_runs/lazy catch-up 推进逻辑**（grep dungeon/segment/floor 在 session/regionrunner 业务零命中）。缺口：四档里整一档完全没实现。〔部分登记：开发进度 §1 末"副本推进属后续"〕

- [状态: 部分] **威胁刷新：threat_level 累积 + freshness + arbitration 选址 + 破圈预算（§1）** — 文档要求 `region.threat_level` 随后台世界事件累积上涨、跨阈值时 `threat_spawn_score=0.5·threat_level/100+0.3·anchor_density+0.2·freshness` 用 `arbitration.Resolve` 取确定性首位、每日 ≥1 个零锚破圈、world_boss 仅当 region 触顶且已沉淀 ≥1 未解决 field_boss 才升级；代码现状：`regionrunner/threat.go` 实现了**锚加权（anchor_density 项 + 破圈下限 floor）**这一半（PvE-4），但 `threatBaseLevel=50` 是 MVP 常量、**没有 threat_level 累积、没有 freshness 项、没有 arbitration 区域级选址、没有"每日≥1零锚来源"的破圈预算配额、world_boss 升级无 provenance 前置（SpawnWorldBoss 是 HTTP 直接投放，world_boss.go:62）**。缺口：威胁选址只做了"扎堆她在乎处"，缺反扎堆(freshness)/累积上涨/破圈配额，违背 §11"锚加权让威胁永远扎堆她村子"的缓解项。〔已登记：开发进度"完整威胁刷新剩余"〕

- [状态: 缺失] **provenance 链（§1/§6/§9）** — 文档要求威胁 `provenance_json`、world_boss"不凭空刷"靠 provenance 链、每条层2/层3 惩罚必带 provenance（指向威胁战力/她的选择/她的伤势）否则降级；代码现状：**全仓 provenance 零命中**（threats 表不存在，惩罚 `applyDefeatPenalty` 不带任何前因证据）。缺口：威胁/惩罚的"为什么会发生在她身上"证据链完全缺，与归因红线断裂。〔未登记〕

- [状态: 缺失] **接入执行/region-runner 主循环触发参与（§2/落地阶段2）** — 文档与落地路线要求 elite 经 `decision.Router` StrategicFork 在主循环触发；代码现状：`regionrunner/threat.go` 的 PvE-1~PvE-4 已把 elite roll 接进 region-runner（但全 flag-gated 默认关，需 `ENABLED+APPLY+THREATS+THREATS_APPLY` 四开），而 **field_boss/world_boss/dungeon 完全不接主循环**，只能 HTTP 手动触发（router.go:1345/1443/1466）。缺口：除了离线 elite，威胁不会在世界自然发生，全靠外部 POST。〔已登记：开发进度 PvE-1~4 + "field_boss/dungeon 分段异步推进"剩余〕

### P2 增强

- [状态: 缺失] **组队=party social_object + 撮合/异步补位/黑吃黑（§7）** — 文档要求威胁产 party social_object，从"地理近+钩子契合+关系交集"候选用 `relevance.MatchScore(≥0.45)+arbitration` 择人、`backfill_npc` 兜底凑齐 world_boss、过多离线自动降速而非团灭、黑吃黑走 `CROSS_BETRAYAL` 进受害者 Contested 回应卡；代码现状：`ResolveFieldBoss`/`StrikeWorldBoss` 的 party 是**调用方传进来的 unitIDs 现成名单**（threat.go:299），**无撮合、无 backfill、无降速、无黑吃黑**（grep backfill/降速 在威胁路径零命中；`MatchIntoSocialObject` 存在于 social_match.go 但威胁链路不调用它建 party）。缺口：组队是"传一组 ID 进来打"，世界 boss"永远能凑齐一队、分不出真人/NPC"的核心承诺未实现。〔部分登记：开发进度"party social_object 撮合、黑吃黑 CROSS_BETRAYAL"列为剩余〕

- [状态: 缺失] **SoloAllowed 物理锁未接（§7）** — 文档要求 world_boss `severity>cap` 时物理上 solo 不解锁、单人靠近只触发"这不是一个人能撼动的"高光卡；代码现状：`encounter.SoloAllowed`（encounter.go:199）原语已实现并测试，但 **session 层零调用**（grep SoloAllowed 在 session 零命中），`StrikeWorldBoss`（world_boss.go:88）允许任意单个 attacker 出手，**无 solo 门禁**。缺口：原语未接主链路，单人理论上可独刷世界 boss。〔未登记〕

- [状态: 缺失] **失败后果经 relevance 传播成"一人一版"命运（§6）** — 文档要求 Defeat 写 `region_ravaged` cross_event → 对每个角色跑 `relevance.Score`（geo/relation/redline 锚）过 0.35 阈进收件箱，不在场但家乡被屠者经关系图传播（HopFidelity 衰减 + StopPropagation）也被点亮；代码现状：威胁失败只对**当事人自己**投一张 `SurfaceFateEvent`（threat.go:245、528），**不写 region_ravaged cross_event、不跑 relevance 传播给在乎她的旁人**（`WorldizeDeath` 那套双向耦合未接威胁失败路径）。缺口：失败仍是"一人一卡"而非经关系图扩散的"她不在那座城但她母亲在"。〔未登记〕

- [状态: 部分] **邀请同意按后果层定 ConsentTier（§7）** — 文档要求加入按"对她最坏后果层"定 Unilateral/Contested/RequiresConsent；代码现状：`consent_gate.go`/`ConsentTierFor` 通用原语已落地并用于 seven_interactions，但**威胁加入路径完全不调用**（world_boss/threat.go 无 ConsentTier 引用）。缺口：原语在，威胁参与未接。〔未登记〕

注：祖魂语气叙事卡（§8 不暴露原始数值）在现有三条链路中**已正确落地**（InboxCard 全是第二人称叙事、不写"+15金/-50士气"），`ContributionScore`/`AllocateLoot`/`DegradePenalty`/`SoloAllowed`/`arbitration` 付费不进 Score 这些**结算原语本身已完整实现并测试**，故未列为缺口。

========================================

All confirmed. Key findings:
- `decision.Router` IS wired into session (reflex_shadow/short-circuit) — that engine claim is genuinely landed.
- `arbitration.Resolve` is only used in `social_match.go`, NOT in combat (`combat_roll.go` has its own determinism). The §7.2 GDD claim "付费改不了已开打的战斗结果" via arbitration is real-via-combat_roll but the arbitration primitive isn't the combat resolver.
- 一致性旋钮 (§5.1) NOT implemented.
- 情绪色系统 / 可分享传记卡 (§9 VS, the病毒核心) NOT implemented — only a `StageReferral` analytics stage label exists.

I have comprehensive evidence. Writing the gap report.

## 游戏开发方案GDD.md 缺口报告

总体结论：传统战棋内核（回合/指令/战斗/技能/地形/天气/生存经济/社交/记忆/血脉）与 §7.2 声称的引擎升级（`decision.Router`、`status.ApplyBatch`，已接入 session）基本完整；但 GDD 反复强调的「core toy = 人格炼蛊开盒循环」的产品层（§3 moment-to-moment、§4 即时人格快照、§5 共鸣度/直控解锁、§8 乐趣度量埋点、§9 VS 病毒物、§11 内容安全双向审核）几乎全部缺失或仅占位——即「玩的那一下好不好玩」恰恰是最大缺口。`arbitration.Resolve` 也未做战斗结算器（仅用于撮合）。

### P0 阻塞核心体验/红线
- [状态: 部分] **§4 即时人格快照（Instant Soul Preview，O2 最高 ROI 啊哈）** — 文档要求创角后用 1 次 LLM 做「15 秒压缩快进」，产出一张「符合人格但玩家没指定」的微选择高光卡（如「绕开人群、对受伤的鸟说第一句话」）作为 D0 首次啊哈；代码现状 `frontend/src/fate/FateApp.tsx:71-76` 的 preview 仅回显 `identity.biography` 或拼接「出身+欲望」静态文案，无任何快进/微选择 LLM 调用；缺口：缺少 core toy 的第一次开盒证据（「我捏的种子真的长出了行为」），把 D0 情感账户起步全押回了 D2；〔未登记〕（进度.md 仅记 FateApp「即时快照 onboarding」为已完成，实为 biography 回显，名实不符）
- [状态: 缺失] **§3+§8 高光卡轻交互〔意料之中/有点意外但合理/太离谱〕** — 文档要求每张高光卡带三档轻交互以同时采集惊喜命中率/OOC 率；代码现状 `frontend/src/fate/FateView.tsx:178-182` 高光卡为纯展示 `<div>{c.narrative}</div>`，仅待决策卡有 let_her/urge/acknowledge 三键；缺口：玩家无法标注卡片，§8 看板的两个核心乐趣指标无数据来源；〔未登记〕
- [状态: 缺失] **§8 乐趣度量埋点（惊喜命中率 / OOC 率 / 乐趣自评）** — 文档要求三指标进 MVP 看板、与留存并列、作为负体验看门狗与一致性旋钮触发器；代码现状 `internal/analytics/analytics.go:27-34` 事件名仅 session_created/character_created/decision_pending/decision_resolved/player_intervention/return_visit，无 surprise/ooc 任何枚举，全仓 grep「惊喜命中/意外但合理/OOC率」零业务命中；缺口：整个「证好玩而非只证回来」的度量闭环不存在；〔未登记〕
- [状态: 缺失] **§11 AI 内容安全双向审核管线（与公平红线同级）** — 文档要求输入侧 prompt-injection/越狱检测 + 输出侧 NSFW/暴力/仇恨分类器（命中回退规则模板）+ 年龄分级/未成年人模式 + 「角色行为下限契约」；代码现状 `internal/session/moderation.go` 仅 `SubmitModerationReport` 玩家举报落库，`internal/ai/` 全 grep 无 injection/nsfw/分类器/未成年；缺口：LLM 直出文本无任何前置/后置安全闸，发行前 gate 缺位；〔未登记〕

### P1 重要
- [状态: 缺失] **§3 收件箱 Copilot（2 选项 + 角色倾向 + 后果预测）** — 文档要求处理悬念决策时 Copilot 给 2 选项、标注角色倾向、预测后果，「选/改写」后给处理回执；代码现状 `FateView.tsx:160-168` 待决策为固定三键（由她去/疾呼拦住/默默看着），后端 `internal/session/fate.go` `ResolveFateDecision` 无 option/倾向/后果预测字段，全仓无 copilot/后果预测命中；缺口：缺「介入命运 + 掌控可感」的核心交互，决策退化为通用三态；〔未登记〕
- [状态: 缺失] **§3 会话收尾卡（结局感 + 影响累积 + 下次 D2 钩子）** — 文档要求会话末给「这次你为它做了 3 个决定、因为你它[守住断刃]、下次面对[预告]」；代码现状 全仓 grep「收尾卡/会话 summary/预告」零命中（service.go 的「收尾」均指回合后台结算），FateView 无该 section；缺口：缺会话闭环与下次回归钩子；〔未登记〕
- [状态: 缺失] **§5.1 共鸣度 Resonance（园丁式掌控，玩家可见进度指标）** — 文档要求一个玩家可感的「近期决策与角色自发选择契合度滑动窗口」聚合视图，正面化解自治 vs 掌控张力；代码现状 `engine/attachment` 仅有内部 `Inputs.Resonance float64`（牵挂计算的一个输入），无窗口式契合度计算、无可见指标、无「追认/讲价值观/选一致项提升共鸣」机制，前端零展示；缺口：差异化命门「我把它养成懂我的人」的成就感玩法不存在；〔未登记〕
- [状态: 缺失] **§5.2 直控解锁（随共鸣度/牵挂解锁的高光共同演出）** — 文档要求直控时长/权限随共鸣度/牵挂解锁、只在关键节点接管几回合并特别标进编年史；代码现状 旧战棋客户端 `App.tsx` 有手操但无解锁门控，新命运客户端 `FateView` 仅「托梦」一句嘱咐（`recordPlayerIntervention`），无附身/接管几回合、无 attachment 门控、无「我亲手打的那一仗」编年史标记；缺口：GDD 称为「唯一有手感的那一下」的养成奖励出口缺失；〔未登记〕
- [状态: 缺失] **§9 可分享死亡/高光传记卡（变现 + 病毒核心物）** — 文档列为 VS 必含与「变现 + 病毒核心」；代码现状 `internal/session/legacy_hall.go` 生成名人堂传记文本，但无 Spotify-Wrapped 式竖图渲染、无分享端点，`analytics.go:23` 仅有 `StageReferral`「分享传记卡」阶段标签占位、无对应事件；缺口：最强分享触发点（死亡传记卡）无可分享产物；〔未登记〕

### P2 增强
- [状态: 缺失] **§10 决策事件母题库（上线 ≥40 情境母题）** — 文档要求 ≥40 个含触发条件+两难结构+Copilot 选项骨架+过期兜底文案的母题（战书/求婚/复仇/夺权…）作为持续内容产能；代码现状 `internal/session/random_events.go` 有 ~46 个随机事件，但它们是回合内即时 branch（pay_toll/ration/torch 等资源/生存小事件），非 GDD 所指的「两难情境母题 + Copilot 骨架」，无 motif/母题抽象、无两难结构字段、无过期兜底进收件箱；缺口：内容生产管线的骨架资产形态与命运决策不匹配；〔未登记〕
- [状态: 缺失] **§6 赛季制（每季神话层+burn-in 重开、归档名人堂）** — 文档列为同时解决公平起跑/运营节奏/技术降负/冷启动密度的机制；代码现状 仅有 `legacy_hall.go` 名人堂归档与 `world.Seal`，无赛季重开/神话层/burn-in 活史/冷启动密度任何实现，全仓 grep「赛季/season/神话层/burn-in」零业务命中；缺口：季更运营节奏与冷启动方案未落地；〔未登记〕
- [状态: 部分] **§7.2 `arbitration.Resolve` 作为战斗公平结算** — 文档 §7.2 表把 `arbitration.Resolve`（胜率∝Score、频率/顺序无关）列为「付费改不了已开打的战斗结果」的反 P2W 保证；代码现状 `arbitration.Resolve` 仅被 `internal/session/social_match.go:55` 与分赃（field_boss/world_boss）调用，战斗主结算走独立的 `combat_roll.go`（自有 FNV 确定性，未调 arbitration）；缺口：仲裁原语未作为战斗结算器接入，二者反 P2W 各自实现、未统一（combat 确定性成立，但与 §7.2 表述不符）；〔已登记（进度.md 一/二将 arbitration 仅记于撮合/分赃）〕
- [状态: 部分] **§6 生命循环之「衰老」** — 文档要求衰老/死亡/血脉传承；代码现状 死亡（`recordWorldizedKill`/`WorldizeDeath`）、血脉传承（`romance.go` 怀孕生育 + `Identity.Lineage="child"` + `InheritorID` 战利品继承）已实现，但无年龄/衰老机制（grep「衰老/aging」仅文档注释，无字段无衰减）；缺口：生命循环三要素缺「衰老」一环；〔未登记〕
- [状态: 缺失] **§5.1 / §12 一致性旋钮（自治强度可调，新玩家「不是我的角色」缓解）** — 文档（§5.1 引、§12 风险表）要求一致性旋钮配合共鸣度收紧/放松角色我行我素程度；代码现状 全仓 grep「一致性旋钮/consistency knob」零命中（attribution 有自动降级护栏，但无玩家可调旋钮）；缺口：自治太强导致的新手疏离感无调节手段；〔未登记〕

补充说明（非缺口，供 orientation）：§7.2 的 `status.Mutator.ApplyBatch` 与 `decision.Router`（经 `reflex_shadow.go`/`reflex_shortcircuit` 接入 `llm.go:210`）确为已落地且接入主链路；回合生命周期（`turns/state.go`）、指令三类（doctrine/task/order，`command*.go`）、战斗（`combat_roll.go`/`combat_effects.go`/`reaction_queue.go`）、技能（`skills.go` 含 battle_cry/suppressive_strike/fire_assault/field_treatment 等编目）、地形/天气修正（`terrain_combat.go`/`weather.go` 确定性、已接战斗）、生存经济（`hunger.go`/`production.go`/`trade.go`）、社交四轴（`relation.go`）、记忆衰减（`memory_store.go` tau=120）均完整，故未列入缺口。

========================================

I have all the evidence I need. Now I'll compile the gap report. The PRD is primarily a business/product strategy document — most of it (market analysis, P&L, GTM, team, fundraising, roadmap timeline) is non-code. I'll focus on the items that map to code/product/UI deliverables the task highlighted: north star/core loop/retention/monetization/account/governance(reporting+privacy)/MVP scope/launch gates, plus AI safety which PRD §9 makes "公平红线同级".

## 产品方案PRD.md 缺口报告

PRD 主体是商业/产品战略文档（市场竞品、单位经济 P&L、GTM、团队人月、融资、合规路径——这些天然不是代码，不计为「缺口」）。落到工程/产品可交付面：**账号体系、举报、隐私擦除、成本仪表盘、假门留资、AARRR 埋点骨架、命运核心循环 UI 均已落地**；真正的缺口集中在三处——**变现链路(天命配额/订阅/计费)完全无代码、AI 内容安全与合规门(NSFW/越狱/年龄分级/实名)零实现、北极星留存漏斗只埋了一半且无聚合报表**。

### P0 阻塞核心体验/红线

- [状态: 缺失] **天命配额 + 订阅/计费变现链路** — 文档要求(§3.1/§3.2/§3.7)月卡 ¥30→3000 天命配额，「付费用户成本上限=月卡价×40%」用配额硬封成本，订阅/季卡/叙事增强包 SKU；代码现状：全仓 grep `quota|天命配额|月卡|subscription|订阅|billing|payment|首充|复购|arppu` 在 `internal/`/`cmd/` 无任何业务实现（`router.go:196` 的 `session_subscription_count` 是 WS 订阅计数、与付费无关；`unit/trade.go` 的 `PurchaseItem` 是游戏内金币交易），无配额表、无配额扣减、无成本封顶逻辑、无任何支付/IAP 端点；缺口：PRD 把「配额封死付费成本」列为唯一防「越多人付费越亏」的机制(§3.2 结论)，当前完全不存在——直接破坏 §3 商业模型与 §10「卖月卡即亏」风险对策；〔未登记〕（开发进度.md 无任何变现条目）

- [状态: 缺失] **AI 内容安全双向审核管线** — 文档要求(§9，明示「与公平红线同级」)输入侧 prompt-injection/越狱检测+红线词、输出侧 NSFW/暴力/仇恨分类器、命中回退规则模板；代码现状：grep `nsfw|prompt.inject|越狱|jailbreak|敏感词|moderation api|red.?team` 无实现；`ai.Service.GenerateJSON` 仅做 `gojsonschema` 结构校验（`ai/validator.go`），无内容安全过滤；`session/moderation.go` 是**事后玩家举报**（`SubmitModerationReport`）而非**生成时审核**，二者不可互替；缺口：高压叙事(恋爱/复仇/背叛)生成内容无任何护栏，PRD §10 列为「中×致命」的发行前置门，红队 gate 亦无；〔未登记〕

- [状态: 缺失] **年龄分级 + 未成年人模式 + 强制实名/防沉迷** — 文档要求(§5 内容安全行 / §9)高压叙事做可分级开关、强制实名+年龄门、未成年模式(关闭恋爱生育/降暴力)；代码现状：前后端 grep `age.?gate|realname|实名|未成年|防沉迷|分级|content.rating` 均无（`bootstrapPixi.ts:2120` 的「伤害分级」是 UI 颜色，无关）；`session/romance.go`/`pregame.go` 恋爱生育无年龄门开关；缺口：PRD §5/§10 标为「踩 AI×恋爱/复仇×未成年四高压线」的致命合规门，零实现；〔未登记〕

### P1 重要

- [状态: 部分] **北极星留存漏斗埋点（D2 回访 / AARRR）** — 文档要求(§12「换北极星=organic 无推送 D2 回访率」、开发进度 M1.2)AARRR 五阶段全埋点；代码现状：`analytics/analytics.go` 定义了 6 个事件常量，但只有 3 个被实际 emit——`EventDecisionPending`/`EventDecisionResolved`(`fate.go:164,294`)、`EventIntervention`(`echo.go:64`)；**`EventSessionCreated`/`EventCharacterCreated`/`EventReturnVisit` 定义后从无 emit 点**（grep 确认零调用；`router.go:38` 的 `broadcastSessionSnapshot("session_created",…)` 是 WS 事件名、非 `analytics.Emit`）；缺口：北极星正是 D2 **回访率**，而 `return_visit` 从不落库 → 无法算分母；Acquisition/Activation 两阶段(session/character created)亦空 → AARRR 漏斗顶部断裂；〔部分登记：开发进度把 M1.2 标 ✅，但回访埋点缺失未登记〕

- [状态: 缺失] **product_events 漏斗聚合报表** — 文档要求(§4.3/§12 双指标门、留存×成本×变现三门需可观测漏斗)；代码现状：`product_events` 表有写入层(`analytics.Emit`)，但 grep `product_events` 在 `internal/httpapi/` 无任何读取/聚合端点；`/api/ops/*` 仅 `cost-dashboard`(LLM 成本+单位经济)与 `leads-funnel`(假门留资，读 `fake_door_leads` 表)，二者都**不读 `product_events`**；缺口：AARRR 漏斗数据写进去了却无任何运营可视化/查询出口，无法支撑 PRD「双/三指标门」决策；〔未登记〕

- [状态: 缺失] **死亡传记卡分享/导出（referral 触发点 + 自发分享率 KPI）** — 文档要求(§3.4/§11.4「死亡传记卡=最强分享触发点」被赋硬 KPI：自发分享率每 +1pp 等效降 CAC X；§4.2 把世界做成可剪辑社交素材)；代码现状：`legacy_hall.go` 生成名人堂传记(`hallBiographyPayload`/`biography_summary` 入 `hall_of_fame_entries`)，但**无任何分享/导出端点或前端分享 UI**（grep `share|分享|referral` 仅命中游戏内 `AwardShare` 分赃、口粮分发、DeepSeek 端点 URL，全不相关；`analytics.StageReferral` 常量定义但零 emit）；缺口：PRD §3.4 把「自发分享压 CAC」列为唯一活路(LTV/CAC=0.08–0.14、买量必死)，而 referral 链路从触发到埋点全空；〔未登记〕

### P2 增强

- [状态: 部分] **账号体系的合规要件（OAuth/第三方登录、实名绑定）** — 文档要求(§5 强制实名、§4.2 邀请制波次、出海 COPPA/GDPR)；代码现状：`account/service.go` 已实现用户名/密码注册+登录+bearer token+过期清理(`accounts_users`/`accounts_sessions`，bcrypt，双驱动)，但仅本地账密——无第三方/OAuth、无实名字段、无邀请码/波次开服机制(grep `邀请制|invite|波次` 无)；缺口：满足基础登录但缺 PRD §4.2 冷启动「邀请制波次」与 §5 实名要件，属增强；〔未登记（邀请制/实名）〕

- [状态: 部分] **单 MAU LLM 成本监控（§3.5/§11 盈亏卡点指标）** — 文档要求(§3.5「免费用户单位成本能否压到 $0.02/月」是唯一需死磕的数字、§11 provider 涨价敏感性)；代码现状：`session/cost_dashboard.go`(`/api/ops/cost-dashboard`)已聚合真实 LLM 成本(`TotalCostUSD`/`ByProvider`/`FallbackRate`/`CostPerSessionUSD`)+ `DistinctSessions`(MAU 代理)+ 按生命态单位计数；但 MAU 用「窗口内有 LLM 交互的 session 数」代理(非账号 MAU)、无「每免费用户每月 $/月」口径、无 provider 涨价敏感性建模；缺口：PRD 真正要盯的「单免费用户 $/月 vs $0.02 门」未直接计算，需账号维度聚合；〔登记为后续：cost_dashboard.go 注明「扁平成本列/钱包 SUM 是规模化后续」〕

- [状态: 部分] **假门预实验运营闭环（W0/§12）** — 文档要求(§12 W0 近零成本预实验：成本真跑+概念访谈+微 WoZ；§4.3 假门估真实 CPI)；代码现状：`httpapi/leads.go`(`POST /api/leads` + `GET /api/ops/leads-funnel`)+ `landing/index.html`(3 变体 A/B/C + 22 处 track 调用/sendBeacon 埋点 + 问卷)已就绪，`cmd/costbench` 成本基准工具存在；缺口：开发进度自标「剩余：人肉后台运营 + 真实投放跑实验」——即「真实 CPI 估算/概念访谈/WoZ」是运营动作非代码，工程支撑已足；〔已登记：开发进度 W0 标 [~]〕

—

证据补充：
- 已完整实现、**不计缺口**：账号注册/登录/登出(`account/service.go`)、玩家举报(`session/moderation.go` + `POST /api/sessions/:id/reports` + `GET /api/sessions/:id/audit`)、不可逆隐私擦除(`session/privacy.go` `EraseSessionPrivateData` + `PurgeExpiredSessionData`，覆盖 blob/三旁路表/相位快照/记忆 FTS，并处理异步执行竞态)、命运核心循环 UI(`frontend/src/fate/FateApp.tsx`+`FateView.tsx`：捏人→离线家训/红线→四槽开盒→托梦/疾呼接管，WS 实时)。
- PRD 主体的 §2 市场竞品、§3 P&L 建模、§4 GTM、§5 合规路径决策、§6 里程碑甘特、§7 团队人月预算、§8 Live-Ops、§10 风险登记、§11 provider 对冲、§12 验证升级——属投资/战略叙述，无代码可核（除上列已抽出的工程化要件），不列为缺口。

========================================

The FateApp UI has charter/redline/嘱咐 onboarding (single redline field, doc says ≤2 redlines + 3 quick嘱咐presets保守/激进/推荐). It records the charter as a player intervention. No countdown/紧急 surfaced, no LOYALTY_GAIN/STRAIN differentiated receipt copy ("更信你一点"/"记下了这一次"). The redline isn't stored as a structured relevance anchor for the attribution `redline` precursor (already a known open TODO in 开发进度).

I have enough evidence to write the report.

## 验证实验设计.md 缺口报告

**总体结论**：W0 假门测试（步骤①）的后端+落地页支撑已基本闭环（landing 3 变体/Q1-Q5 问卷/26 处埋点 + `/api/leads` + `/api/ops/leads-funnel`），成本基准工具 `costbench` 与 `cost-dashboard` 也在。但步骤③ MVP 的**埋点漏斗（§5.2/5.3）几乎全空**：除 decision_pending/resolved/intervention 三个服务端埋点外，文档要求的 ~20 个漏斗事件无一落地，前端零埋点；**北极星「D2 收件箱处理率」无法计算**（缺倒计时/不可逆/过期兜底/was_against_will 等关键字段与读侧聚合）；§5.4 牵挂指数、§5.5 分层留存、§5.6 成本告警阈值、§5.7 16 张看板卡全部缺失。本文档的产品验证体系仍停在「数据采集骨架」，未到「可判 gate」。

### P0 阻塞核心体验/红线

- [状态: 缺失] **北极星 D2 收件箱处理率不可计算** — 文档 §5.3 要求北极星=「D2 窗口处理≥1条 / D2 确有待决策可处理者，去重以 decision_resolved 为准、排除 expired_fallback」，阈值 >70% 强成立；代码现状 `product_events` 仅有 `decision_pending`/`decision_resolved` 两条裸埋点（`fate.go:164,294`），无任何读侧聚合（仅 `analytics_test.go` 查表，无生产查询），分母所需的「expired_fallback」「day_n」「source」字段均不存在；缺口=核心 gate 指标完全算不出来；〔已登记，开发进度 §三标 `[~] W0`「剩余：人肉后台运营 + 真实投放」，但埋点漏斗未单列〕
- [状态: 缺失] **待决策四要素之倒计时/不可逆/过期兜底** — 文档 §4.2/§5.8 把「紧急+不可逆+倒计时(6–24h)+过期兜底（玩家不来角色按宪章自选）」列为 D2 钩子四要素必备，§5.2 要求 `PENDING_DECISION` payload 含 `urgency/irreversible/expires_at/default_fallback`；代码现状 `PENDING_DECISION` 实际 payload 只有 `narrative/relevance/source_actor/source_target/reason/decision_id`（`fate.go:140-151`），无 expires_at/倒计时字段、无过期自动兜底机制（`ResolveFateDecision` 仅人工处理，无定时 expired_fallback 写入路径）；前端 `FateView.tsx` 也不渲染倒计时/紧急度；缺口=损失厌恶钩子的核心载体未实现，钩子退化为「无时限待办」；〔未登记〕
- [状态: 缺失] **漏斗事件全链路（§5.2/§5.3 S1–S11）** — 文档要求 11 步主漏斗事件 `account_registered/character_created/charter_completed/session_first_offline/d2_push_scheduled|sent|opened/app_session_started/status_card_viewed/inbox_opened/...chronicle_viewed/share_initiated` 等约 20 个【P】事件；代码现状 `analytics.go:27-34` 只定义了 6 个常量，其中 `EventCharacterCreated/EventSessionCreated/EventReturnVisit` 三个常量**定义但全代码零调用**（grep 无命中），其余漏斗事件名连常量都没有；前端 `frontend/src/fate/` 零客户端埋点（grep `track/analytics/product_event` 全空）；缺口=漏斗 S1–S8、S10、S11 全部无数据，主漏斗只剩 S9 一个点；〔未登记〕

### P1 重要

- [状态: 缺失] **DECISION_RESOLVED / 违背意志代理字段** — 文档 §5.2 要求 `DECISION_RESOLVED` payload 含 `resolve_type(acknowledge/correct/remedy/direct_control/expired_fallback)/via_copilot/resolve_latency_sec/was_against_will`，并要 `direct_control_against_will`【P】专喂 H2；代码现状 resolved payload 仅 `decision_id/resolve_type`（`fate.go:290,297`），无 `was_against_will/resolve_latency_sec/via_copilot`，无 `direct_control_against_will` 事件；缺口=H2「违背意志反应分布」看门狗（§5.4⑥、§5.7⑩、gate 表 A「愤怒/弃坑>50%」）无数据来源；〔未登记〕
- [状态: 缺失] **成本×留存联合监控阈值与口径（§5.6/gate 表 B）** — 文档要求「单活跃**角色**日均 LLM 成本」按 source 拆（offline_tick/dialogue/copilot/reflection/chronicle）、黄 `>$0.18/活跃日`/红 `>$0.30`、单角色单日 `>$0.5` 异常、周环比 +25% 告警、对 ARPU×0.5；代码现状 `cost_dashboard.go` 只算 `CostPerSessionUSD`（按 session 不按活跃角色/天）、无 source 拆分、无任何阈值/告警/ARPU 比较字段（grep `0.18/0.30/0.05/ARPU` 在成本路径无命中）；缺口=双指标 gate 表 B（红线 `>$0.05/活跃日`、`≤0.5×ARPU`）无法判定，「循环成立但商业不成立」的否决机制不可执行；〔已登记「扁平成本列/钱包 SUM 是规模化后续」，但按角色/天口径与告警阈值未登记〕
- [状态: 缺失] **牵挂指数 attachment_score 与 6 项代理（§5.4）** — 文档定义 `attachment_score=0.30·无推送回访+0.20·回访间隔倒数+0.20·第一人称回顾+0.15·分享+0.15·在世天数` 分层用；代码现状无任何 product_events 读侧聚合、无 `firstperson_recap_requested/share_initiated/在世天数` 埋点（grep 全空），`engine/attachment` 包是另一套「角色牵挂等级」纯函数（共鸣+在世+回访+共创，开发进度 §一），与本文档的「玩家牵挂代理指数」不是一回事、未接 product_events；缺口=§5.4 整张表与 §5.7 牵挂区卡⑥⑦⑧⑨无实现；〔未登记〕
- [状态: 缺失] **留存分层 T1/T2/T3 与 D1/D2/D7 矩阵（§5.5）** — 文档要求三个二值标签（展开读者/第一人称读者/分享者）× 四档 L0–L3 × {D1,D2,D7} 出表，避 survivorship；代码现状无 `chronicle_entry_expanded/firstperson_recap_requested(button)/share` 埋点、无任何 cohort/D-day 留存计算（grep `retention/cohort/tier` 在 product 路径无命中）；缺口=「分层看不看平均」铁律①无实现，gate 表 A「高投入层 D7 留存」无数据；〔未登记〕

### P2 增强

- [状态: 缺失] **核心看板 16 张卡（§5.7）** — 文档列 16 张运营卡（北极星双口径/分层热力图/留存-成本象限/埋点健康度等）；代码现状仅 2 个只读 JSON 端点（`/api/ops/leads-funnel` 假门漏斗 + `/api/ops/cost-dashboard` 成本），无可视化看板、无埋点健康度卡⑯（decision_id 配对完整率/AB 分桶均衡/关键事件丢失率）；缺口=运营无法据 gate 判读；〔未登记〕
- [状态: 部分] **MVP onboarding 文案与处理回执（§5.8）** — 文档要求 3 个快捷嘱咐预设（保守/激进/推荐）+ 红线≤2条 + 差异化处理回执「追认→更信你一点(LOYALTY_GAIN)/纠偏→记下了这一次(LOYALTY_STRAIN)」；代码现状 `FateApp.tsx:149` 仅单条红线输入（非≤2 + 无三预设），`FateView.tsx:107` 回执是统一「她记下了」（未按 LOYALTY_GAIN/STRAIN 分文案，`LOYALTY_GAIN/LOYALTY_STRAIN` reason-code 已在 `reason_codes.go` 但前端未触发分支）；缺口=「你怎么对它决定它多听你」第一分钟可感性弱化；〔未登记〕
- [状态: 部分] **session_first_offline 服务端兜底补埋（§5.2/风险登记）** — 文档要求「后端最后心跳+宽限兜底补埋」会话边界事件，看板卡⑯监控丢失率>5%报警；代码现状无 `session_first_offline` 事件、无心跳兜底补埋逻辑（grep 无命中）；缺口=会话边界事件易漏埋的已知风险无缓解；〔未登记〕
- [状态: 部分] **W0 人肉后台（步骤②）运营支撑** — 文档 §4 要求人肉后台 Runbook（角色台账=人肉版 events 表 / D2 召回 5 变体 / 违背脚本）；代码现状无对应支撑（属纯运营，可不入代码），但开发进度自标「剩余：人肉后台运营 + 真实投放」；缺口=②阶段无任何工具化沉淀，全靠 Notion/CronCreate 外部工具；〔已登记〕

> 说明：`engine/events/reason_codes.go` 已登记 `PENDING_DECISION/DECISION_RESOLVED/PLAYER_INTERVENTION`（含 §5.2 要求的 `player_decision` 语义，虽归在 CategoryFate/Lifecycle 而非新建 `player_decision` 类目），`product_events`/`fake_door_leads` 表与 landing 假门已就绪——这些是已完整实现部分，未列入缺口。证据文件：`backend/internal/session/fate.go`、`backend/internal/analytics/analytics.go`、`backend/internal/session/cost_dashboard.go`、`backend/internal/httpapi/leads.go`、`landing/index.html`、`frontend/src/fate/FateApp.tsx|FateView.tsx`。