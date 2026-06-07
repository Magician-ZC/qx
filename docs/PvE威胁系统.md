# 《群像》PvE 威胁系统 — 世界Boss / 副本 / 野外Boss / 精英怪

> 把 Boss/副本/精英 + 战利品分配 + 失败惩罚 + 组队/单人，装进「角色命运开盒」架构（[`设计宪法-角色命运开盒.md`](设计宪法-角色命运开盒.md)），建立在 `engine/arbitration`（裁决）、`engine/relevance`（相关性/传播）、`engine/encounter`（本系统的结算原语）、后果分级闸、`cross_events`（World Bus，[`事件耦合与跨玩家关联.md`](事件耦合与跨玩家关联.md)）之上。

---

## 0. 核心定调

**在一个"角色多数时间自治、玩家是祖魂、玩法异步"的游戏里，传统 MMO 的实时团本世界 Boss 不成立。** 威胁必须重构为：

- **不是一场实时团本，而是一类有生命周期的「世界级威胁事件(ThreatEvent)」**：在 region 里随威胁度升级而显形，经 relevance 锚耦合进「我的角色的命运」。
- **参与是自治或祖魂引导**：角色按离线宪章/人格决定要不要去（怕死的不去、护短的去），祖魂只能叮嘱不能强迫。
- **一次"遭遇"=一段被压缩、离线也能推进的确定性多回合消耗战**（`combat_roll`，0 次 LLM），只在关键节点进命运收件箱。
- **战利品=零和分赃**→直接走 `arbitration` 按贡献裁决，**付费不进 Score**（氪佬抢不到史诗装）。
- **全员失败的惩罚必须过后果分级闸**——D0-D3 物理上毁不了玩家心血。
- **一切落到"她的命运"而非"一场副本战报"**，祖魂语气。
- **四档威胁只是同一内容模型的参数化实例**（规模/参与人数/缩放/consent 档不同），不是四套系统。

---

## 1. 统一 Threat 内容模型（四档参数化）

| 档 | 定位 | power | hp_pool | 窗口 | 缩放 | consent | 单人 |
|---|---|---|---|---|---|---|---|
| **精英怪 elite** | 路上的硬茬，单角色顺手或绕开 | 80–150 | ~200 | 单 tick 遭遇 | 单角色战力 | 无需 | ✅ |
| **野外Boss field_boss** | 一片地界的威慑源，几个角色可凑 | 300–600 | ~1500 | region 威胁度≥60 后 6–24h | N 角色战力和 | Contested | 缩放后更险 |
| **副本 dungeon** | 她个人的一段险境 | 阶梯式（每层递增） | 按层 | "踏入即开一段专注遭遇" | 可单可双 | — | ✅ |
| **世界Boss world_boss** | 一个 region 的存亡级威胁 | 1500–4000 | 20000+ | region 威胁度满 100 触发，24–72h | 跨玩家多角色 | Contested/Consent | ❌ 物理锁，必须组队 |

（数值全部 `[待测试]`，公式化不硬编码；上线前蒙特卡洛模拟各档单人/N 人胜率找边界，先验目标：单人 elite ~75%、单人 field_boss ~45%、3 人 field_boss ~70%。）

**生命周期**：`forming → active → resolving → defeated/wiped/faded`。结算在窗口 `resolves_at` 的确定性 tick：`Σ参与角色有效贡献 ≥ Threshold → Victory`，否则 Defeat。威胁是 `cross_events` 上的一个 `social_object`（type=threat），不可篡改、`occurred_at`=世界首次触发时刻。`power/hp_pool/Threshold` 是配置常量，**不可付费改**（与受保护字段同级红线）。

**刷新 = region 威胁升级 + 锚加权（非固定刷新点）**：威胁不在固定坐标定时刷，而是 `region.threat_level` 随后台世界事件累积上涨；跨阈值时**按玩家锚密度加权选址**（`threat_spawn_score = 0.5·threat_level/100 + 0.3·anchor_density + 0.2·freshness`，用 `arbitration.Resolve` 取确定性首位）——威胁天然落在她在乎的地方。**world_boss 仅当某 region 威胁度触顶且已沉淀 ≥1 个未解决 field_boss 时升级**（provenance 链，不凭空刷）。

---

## 2. 参与：自治 vs 祖魂引导

威胁显形 → 对每个在场/可达角色跑「参与意愿评估」，扩展 `decision.Router`：

1. **先过反射护栏**（零 LLM）：`HP/HPMax < 0.25` 或 离线宪章红线含「避战」 → 反射层直接 `Flee/绕开`——**怕死的物理上不去**。
2. **意愿评分**（确定性、可审计）：`join_intent = w_amb·野心 + w_rel·护短(在乎的人已在场) + w_goal·目标契合(威胁卡了她的商路) − w_fear·惧战 − w_risk·后果层`。≥阈值才自治加入，**归因必走 `ValidateAttribution`**（cause∈{persona_trait,relation,goal,pressure}），无源不许"她突然冲上去送死"（OOC→不参与）。
3. **祖魂引导 = 偏置不是覆盖**：玩家在收件箱出「叮嘱去/别去」→ 写一条 advice，进 `join_intent` 作**偏置项**而非覆盖——她仍按 `rejectProbability` 概率采纳（护短角色采纳「去」、惜命角色可能抗命）。**advice 绝不直接改 participant_ids**，对齐 D1「能听见不能强迫」。

> 〔高光卡·精英怪自治〕「她在去镇上的路上撞见一头独行的山魈——挡了她的道。她握紧了柴刀。」〔随她去〕〔屏息看着〕〔太险了，劝她绕〕

---

## 3. 副本 = 异步可推进的"一段专注遭遇"

副本**不要求玩家在线**。进副本 = 把她锁进一段专注遭遇态，切成 N 个 segment（每段=1 floor/房间，一次 `combat_roll` 遭遇），**按 tick 节奏离线自动推进**（lazy catch-up 补算）。多数段由反射层自动结算（清杂兵/拾物）；只有关键节点才进收件箱并暂停：① floor boss（FirstContact）；② 她濒死要不要撤（HP 反射阈值）；③ 岔路抉择（深入/见好就收）。超时按 charter 兜底续命。

> 〔祖魂语气·踏入〕「她跟着乡里几个后生进了那座废弃的矿井。越往深处走，风里的味道越不对。你听得见她的脚步，却照不亮她前面的路——这一程，得她自己走。」
> 〔回响卡·离线兜底〕「你没回来。于是在第三层的岔口，她照着你早先的叮嘱，见好就收了——带着半箱子矿石和一道新添的伤，从矿井里爬了出来。」

---

## 4. 确定性多回合消耗战

威胁结算是**多回合确定性消耗战，不是实时 DPS**。每回合对每个 participant 调 `combatActionRoll(salt='threat:'+id+':round:'+r)` → 命中/伤害确定性，扣 `hp_remaining`；威胁反扑同样确定性选目标+伤害，经 `status.Mutator(FieldHP, COMBAT_HIT/COMBAT_DOWN)` 落到参与角色（受保护字段经 Mutator+reason-code 留痕）。每回合判定：`hp_remaining≤0`→defeated；全员 lives 耗尽/全 flee→wiped；个体 HP 反射阈值→该角色自动 flee 退出（贡献保留）。**整场 0 次 LLM**（意图在决策层已定），**付费对 `combat_roll` 全盲**。world_boss = 跨多 tick 的长消耗战，玩家"回来的一分钟"看到的是一个进度切片。

> 〔高光卡·世界Boss进度〕「北岭那场围猎进入第三日了。她还在阵中，胳膊上缠着布。乡亲们都在等那头东西倒下的消息。」

> ⚠️ **异步致死防护**（最致命风险）：个体 HP 反射阈值(<0.25) 每回合后自动 flee，**离线角色永远先保命再续战**；threat 对单个 participant 的致命后果必过后果分级闸（层3锁死）；world_boss wipe 最坏只到层2。

---

## 5. 胜利战利品分配（按贡献，付费不进 Score）

**贡献分**（`engine/encounter.ContributionScore`，全部来自 combat_roll/Mutator 留痕，付费不产生）：
`Score = 1.0·伤害 + 0.8·承伤 + 0.6·扮演(治疗/救援/补给/鼓舞) + 0.5·到场风险 + 1.2·关键救场`。
**反蹭场闸**：`Score < Threshold·0.5%` 不进排他件排名（只记到场、可分材料）；`Risk` 用 `baseAttendance·(Boss战力/自身战力)` 鼓励弱者敢上、压制纯到场。

**分配**（`AllocateLoot`，每件独立走 `arbitration.Resolve`，`Key=ThreatID|Region|Tick|ItemSlot`）：
- **史诗(唯一)** → `Ranking[0]`（确定性胜者，**胜率∝Score、与频率/入队顺序无关、付费不进**）。这就是"史诗装该归谁"的可仲裁裁决——任何人用 `(参与集合, 各自Score, Key)` 复算验证。
- **稀有(N件)** → `Ranking[0..N-1]`。
- **材料/货币** → 按 Score 比例 `SplitProportional`（floor + 余数按名次，无浮点歧义）。
- **败者补偿** → 进排名但没拿到的角色各得一份「史诗合成碎片」（差一名≠零收获，善用损失厌恶）。
- **绑定/传承**：史诗装 `SoulBound=true`（不可交易，防 RMT）；用某装备完成 Clutch/跨越关键命运节点 → 待决策卡「要不要把它刻成传家物？」→ 升级为 `Legacy` 锚 + `ItemIsPermanentAnchor`，此后 `GateSurprise(sell_pinned)` 直接 Reject（**LLM 自治也卖不掉**）；角色阵亡时 Legacy 装备不进分赃，沿最高 relation 锚找继承人 + 入名人堂 + 注入下一代记忆（把"失去"转成"延续"）。

> 〔胜利·史诗归属〕「窟窿山的那头东西终于不动了。她浑身是血地站在最后，比谁都更靠近它的心脏——所以那柄从它脊骨里拆出来的断刃，认了她，没认别人。」
> 〔败者补偿〕「她差一点……但她蹲下身，从尸骸的碎甲里抠出了几片寒铁，仔细包好。她说：下一次，我自己锻一柄。」
> 〔死亡传承〕「他终究没能从那场仗里走出来。但在断气前，他把那柄你陪他用了一辈子的刀，塞进了她手里——那个他一直当女儿看的姑娘。」

---

## 6. 全员失败惩罚（后果分级闸硬锁）

Defeat → `ApplyDefeatPenalties`，**每条惩罚先过后果分级闸**（`encounter.DegradePenalty(candidate, care, daysAlive)`），按角色分级降级，**绝不一刀切毁心血**：

| 层 | 内容 | 解锁条件 |
|---|---|---|
| **层1 可恢复**（始终） | 重伤可治、士气重挫、装备耐久损/掉非 pinned 物、被迫撤离、region 反噬（商路暂断=goal 锚可达性↓） | — |
| **层2 高代价** | 重伤致残（attack/defense 永降一档）、结仇、还不起的大债、一个队友阵亡（relation→debt_grudge_love） | care≥40 或 在世≥3（离乡/失盟需≥50） |
| **层3 不可逆** | 她本人战死、pinned 永久丢失、血脉后果 | care≥70 且 在世≥7 且 已发生≥1次层2（三条 AND） |

**D0-D3 把层3锁死**——物理上 wipe 不会杀死玩家刚养的角色，最坏只到「重伤致残+欠债+撤退」。**层3 还须过 `ConsentTierFor(3)=RequiresConsent`**：先冻结为待决策卡，需玩家/角色同意才落地；**超时未应答→自动降级为残废而非阵亡**（异步安全网）。每条层2/层3 必带 provenance（指向威胁战力/她的选择/她的伤势），否则降级。

**失败后果经 relevance 传播成"她的命运"**：Defeat 写一条 `region_ravaged` cross_event → 对每个角色跑 `relevance.Score`（geo/relation/redline 锚被点亮），过 0.35 阈进收件箱；不在场但家乡被屠的角色经关系图传播（HopFidelity 衰减、StopPropagation 防洪泛）也被点亮——"她不在那座城，但她母亲在"。失败不是全服公告，而是**一人一版**。

> 〔失败·新角色硬锁·层1〕「他们没能拦住它。它趟过了她出生的那条河，把河边的磨坊和半季的粮都碾了……但她还在，村子也还在，只是要饿一阵子。」
> 〔失败·老角色·先过 RequiresConsent〕待决策卡：「你寄宿了她整整二十天……她受了致命的伤，还在往人群里挡。她可能回不来了。——你听见了吗？要不要让她退？（不回应：她会活下来，但永远瘸了。）」

---

## 7. 组队 vs 单人

**组队 = 临时 `party` social_object**（World Bus 上长出来的一根"命运绳"），不是团本房间。硬不变量沿用 World Bus：绑定/退出/分赃全部只产 append-only cross_event，各自 Mutator 只改本侧，**永不直写队友的 units**。

- **撮合**：世界先产出 threat social_object，再从「地理近 + 钩子契合 + 关系网交集」候选里用 `relevance.MatchScore`（≥0.45 入池）+ `arbitration.Resolve` 确定性择人（付费不进 Score）。撮不到人由 NPC `backfill_npc` 兜底——**世界Boss 永远能凑齐一队，且玩家分不出队友是真人还是 NPC**。
- **邀请同意**：按「加入对她的最坏后果层」定 `ConsentTier`——普通=Unilateral 事后知情、世界Boss=Contested 上线得回应卡、生死契=RequiresConsent+牵挂硬锁。
- **异步补位**：离线队员的角色按反射层+离线宪章自治（HP<25%先保命、有威胁 Engage、按红线决定投入），**按真实战力贡献、不吃白食**；过多离线导致有效战力不足 → 威胁推进**自动降速/暂停而非团灭**（避免"你没上线所以全队被你拖死"）。
- **共享池分赃**：队内每件走 `arbitration`（按各自 contribution，付费不进）；可分资源按比例切。
- **黑吃黑 = `CROSS_BETRAYAL`**：有人贪掉落 → cross_event，`occurred_at` 钉死谁先动手，进受害者收件箱（Contested 回应卡：追讨/认栽记仇/求和），各自 Mutator 只改本侧（A 对 B trust−/rivalry+ + 生成 debt_grudge_love 锚为 blood_feud 撮合埋点）。**后果分级闸硬锁**：D0-D3 新角色的 pinned/血脉物（层3）抢不走，只能抢普通掉落。
- **单人 vs 组队 = 确定性解锁门**：`SoloAllowed(severity, cap)`——低威胁可 solo（缩放更险、掉落独享、无需分赃）；世界Boss `severity > cap` → **物理上 solo 不解锁**（单人靠近只触发"这不是一个人能撼动的"高光卡）。组队提胜率但摊薄分赃份额，是**确定性取舍而非付费墙**。
- **打完**：bond 高 → party 退化为 alliance（trust+3，未来优先撮合"再并肩"）；否则各奔东西（留一条共享记忆）。全员失败 → 解散 + 共同 debt_grudge_love 锚（下次更易组复仇队）。是否结盟由角色自治决定，祖魂只能劝。

> 〔待决策·黑吃黑〕「刀本该是她的。可她转身时，那个并肩流过血的人已经把它别在了腰上——是另一缕命，先动了手。」〔追讨·结仇〕〔咽下·记一笔〕〔让她自己选〕
> 〔solo 撞世界Boss〕「她一个人站在那东西的影子里，忽然懂了：这不是一个人能撼动的。」

---

## 8. 祖魂语气 + 付费红线
所有奖惩经 narrative 层渲染成祖魂第二人称语气落进收件箱，**绝不暴露原始数值**（不写"+1史诗剑/-50金"）。**付费红线硬编码**：付费 SKU 只能映射 `narrative_density`（更长更细的命运叙事/追溯）与 `cosmetic`（外观/传家物纹饰）——这两类**绝不产生 status.Mutator 事件、不进 ContributionScore、不改 consequence-gate、不改掉率/胜率**。任何把付费接入 Score/掉落/惩罚的代码路径由 statuslint 同款 lint + arbitration「Score 来源白名单」在 CI 拦截。

> 付费玩家与零氪玩家打同一个世界Boss、贡献分相同 → `arbitration` 给出**完全相同**的史诗装归属与名次。差别仅在：付费玩家的收件箱叙事更长更细、传家物上多一道金纹。胜负、掉率、惩罚分毫不变。

---

## 9. 数据模型 + reason-codes
- `threats(id, world_id, region_id, tier, threat_level, power_rating, hp_pool, hp_remaining, phase, encounter_window_*, loot_pool_id, social_object_id, status, occurred_at, provenance_json)`。
- `threat_participants(threat_id, unit_id, session_id, join_mode{autonomous/advised/reflex_declined}, join_intent_score, contribution_score, damage_dealt, damage_taken, rescues, lives_lost, joined_at)`（付费字段绝不进此表）。
- `threat_encounter_rounds(...)`（每回合确定性留痕，salt 含 threat.id+round 可复放）；`dungeon_runs(...)`（异步推进态）。
- `loot_distributions(threat_id, item_id, rarity, contest_key, winner_unit_id, ranking_json, score_snapshot_json, soulbound)`（可仲裁审计）。
- `party_state` / `party_members`（沿用 social_objects，type='party'，零新表面）。
- `ItemStack` 扩展：`SoulBound`、`Durability/DurabilityMax`、`IsLegacy`。
- **新 reason-codes**（接入 `events.Catalog`，全经 status.Mutator）：`THREAT_EMERGED/JOIN_AUTONOMOUS/JOIN_ADVISED/JOIN_DECLINED/HIT/PHASE/DEFEATED/WIPE/ALLY_DOWN`、`DUNGEON_ENTER/FLOOR_CLEAR/EXIT`、`ECONOMY_LOOT_ARBITRATED/CONSOLATION_MATERIAL/REGION_RAVAGED/GEAR_DAMAGED`、`COMBAT_MAIMED/FELL_IN_DEFEAT`、`LEGACY_BEQUEATHED`、`CROSS_PARTY_JOIN/LEAVE/WIPE`、`CROSS_CONTEST_WIN/LOSE`、`CROSS_BETRAYAL`。

---

## 10. 已落地：`engine/encounter` 结算原语
本系统最「结算」的一块（纯确定性数学）已实现并测试（`go test ./internal/engine/encounter`，7 用例）：
- `ContributionScore(parts)`：贡献评分（确定性、付费不进）。
- `AllocateLoot(key, items, participants, minMeaningful)`：战利品按贡献分配——排他件走 `arbitration`（胜率∝Score、频率无关、付费不进、含 pay-blind 回归测试）、可分件 `SplitProportional`、败者补偿、反蹭场过滤。
- `PenaltyCap` / `DegradePenalty`：后果分级闸（D0-D3 硬锁，有 gate 表测试）。
- `SoloAllowed`：单人解锁门。
它与 `arbitration`（裁决）、`relevance`（相关性/传播）、`decision`（归因/反射）一起，构成结算层确定性的几块基石。完整的 ThreatEvent 容器/调度/副本推进属 session 层后续实现（建立在这些原语之上）。

---

## 11. 风险登记（节选）
| 风险 | 缓解 |
|---|---|
| **异步致死**：世界Boss多回合消耗战变成"她离线时被反复掷骰打死" | 个体 HP<0.25 每回合自动 flee 先保命；致命后果过 consequence-gate（层3锁死）；wipe 最坏到层2；"她死"需 care≥70+在世≥7+已发生层2 三条 AND + RequiresConsent + 超时降级为残废 |
| **付费绕红线**（运营悄悄加掉率/减惩罚） | 付费 SKU 代码层只能映射 narrative/cosmetic，绝不产生 Mutator 事件；CI lint + arbitration「Score 来源白名单」拦截 |
| 刷分/蹭场白嫖史诗装 | Score 来自确定性事件聚合非动作次数 + arbitration 频率无关 + minMeaningful 反蹭场闸 + Clutch 不可伪造 |
| 惩罚降级让失败无关痛痒 | 失败重量来自**叙事 + 可恢复代价**（region 蹂躏/结仇/装备损坏 + 个人化叙事），不来自删号；Threshold/窗口让世界Boss失败是真实可达的群体事件 |
| 老角色不可逆阵亡致心碎流失 | layer3 必过 RequiresConsent 可拒绝 + 超时降级 + 死亡传承线（传家物交付继承人 + 入名人堂 + 下一代记忆）把"失去"转成"延续" |
| 锚加权刷新让威胁永远扎堆她的村子 | `freshness` 项 + 破圈预算（每日 ≥1 个零锚来源威胁）+ region 威胁度上限/冷却 |
| 组队稀释成 MMO 团本战报 | 前台永不出现组队 UI/血条/DPS 表，只出祖魂语气的"她遇到一群共担之人"叙事；战斗细节留后台 events |
| 撮不齐真人队致世界Boss打不了 | NPC backfill 兜底 + hookFit 优先推给真正在乎/地理近的角色 |
| 四档数值/缩放/惩罚分级魔法数字平衡崩 | 全部 `[待测试]`、公式化不硬编码；上线前蒙特卡洛模拟各档单人/N 人胜率找边界 |

---

## 12. 落地阶段
1. **engine 原语**（已落地 `engine/encounter`：贡献/分赃/惩罚闸/单人门）。
2. **单人 elite/dungeon**（session 内即可：ThreatEvent 容器 + 自治参与 + combat_roll 多回合 + AllocateLoot + 后果分级闸 + relevance 耦合 + 祖魂语气）——**不依赖 World Bus，先落地见效**。
3. **field_boss + 锚加权刷新**。
4. **world_boss + 组队**（依赖 World Bus / cross_events / social_objects，与大世界 region 化演进同步）：撮合 + 异步补位 + 共享池分赃 + 黑吃黑。
