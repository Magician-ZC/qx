package session

// 文件说明：共享世界 Phase 3「七交互玩法内自动跨玩家发起」（设计 docs/事件耦合与跨玩家关联.md §2.3 + 共享世界方案 §4 Phase3）。
//
// social_scan.go 的 scanAndSocialize 只在**本会话单位**两两间自治触发七交互；本文件补的是**跨玩家**那一半：
// 共享世界里，同区相遇的真人 A、B 的角色按关系/概率**确定性**自动发起合适的七交互——
//   - 初遇（彼此尚无关系）→ 结识（acquaint，层1 unilateral 立即成立）；
//   - 已成敌对（高竞争/戒备）→ 反目（fallout，层2 contested，对方上线得回应卡）；甚至复仇（层3 requires_consent）。
// 全程经统一管线 RecordSevenInteraction → consent_gate 三档异步同意：A 发起对（可能离线的）B，B 的角色据离线宪章/归因
// 自治回应（consent_gate 已有三档；跨 session 投卡用 owner 自己 session=Phase0 seam 已修，见 consent_gate.surfaceConsentCardToTarget）。
//
// 跨玩家硬不变量（必守）：发起方只写**本侧**（applyRelationShift 源=本侧 actor→target 的本侧关系行）+ 一条 cross_event；
// **绝不直写他人 units/relations**（target=B 的本侧关系/收件箱由 B 自己 session 经 consent accept / SurfaceFateEvent 落，
// 用 owner 自己 sessionID）。本扫描只读同区跨 session 单位（ListActiveByRegion）、只调 RecordSevenInteraction（内部已守红线）。
//
// 纪律：flag-gated（QUNXIANG_AUTO_SOCIAL_CROSS **默认关**——跨玩家自动发起更敏感，默认关使私有档/MVP 行为零变化）、
// 仅 inSharedWorld 生效、低频确定性触发（turn 取模 + FNV 概率门，每扫描限处理少量对）、best-effort（吞错绝不中断推进）。
// 确定性 FNV 无全局 rand，可复现。

import (
	"context"
	"hash/fnv"
	"strconv"
	"strings"

	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/unit"
)

const (
	// crossSocialEveryNTurns 跨玩家社交扫描的部署回合周期：每 N 个部署回合扫一次（与本会话社交/撮合/裁决错频，低频不刷屏）。
	crossSocialEveryNTurns = 4
	// crossSocialMaxInitiations 单次扫描最多实际发起的跨玩家交互数——硬顶每回合跨玩家社交音量，避免一回合对一片人刷屏。
	crossSocialMaxInitiations = 2
	// crossSocialMaxPeers 单次扫描纳入配对的跨 session 同区 peer 数上限（控算量：actor×peer 是 O(n·m)，按确定性顺序截断）。
	crossSocialMaxPeers = 16
	// crossSocialInitiateProbBuckets / crossSocialInitiateProbHits 概率门：FNV(pair,epoch)%Buckets < Hits 才发起——
	// 使「初遇就发起」不必然、随对/周期确定性稀疏化（约 Hits/Buckets 的对会在某周期发起一次），自然收敛、不每周期对同一对刷。
	crossSocialInitiateProbBuckets = 5
	crossSocialInitiateProbHits    = 2 // 约 40% 的「有资格」对在其周期发起（确定性，非随机）

	// 跨玩家敌对升级阈值（量纲 [-10,10]，取与 social_scan 同源的保守值）：高于此即按敌意发起反目/复仇而非结识。
	crossSocialRivalryHot = 6.0 // 反目/复仇门：高竞争
	crossSocialFearHot    = 6.0 // 复仇门：高戒备（与高竞争同时满足才升级到复仇，否则反目）
)

// autoSocialCrossEnabled 读 QUNXIANG_AUTO_SOCIAL_CROSS，**默认关**：仅显式 1/true/yes/on 才开。
// 默认关：私有档 / 未开此 flag 的共享世界局，跨玩家自动发起整段 no-op、零行为变化、零 DB 写。
func autoSocialCrossEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(featureflags.EnvOrOverride("QUNXIANG_AUTO_SOCIAL_CROSS"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// scanAndSocializeCrossPlayer 在部署边界让共享世界同区相遇的真人角色**玩法内自动发起**七交互（Phase 3）。
// actor = 本 session 的存活 protagonist（玩家角色）；target = 同区跨 session 的存活 protagonist（别玩家角色）。
// 按关系/概率确定性挑一种合适交互（初遇→结识、敌对→反目/复仇），走 RecordSevenInteraction（含 consent_gate 三档）。
//
// 守卫（任一不满足即 no-op）：nil 依赖 / flag 关 / 非共享世界局 / WorldID 空 / CurrentZoneID 空 / 未到扫描周期 / 同区无别玩家。
// 全程 best-effort：单对失败只吞错跳过，绝不影响阶段推进。
func (service *Service) scanAndSocializeCrossPlayer(ctx context.Context, state *State, units []unit.Record) {
	if service == nil || service.db == nil || service.units == nil || state == nil {
		return
	}
	if !autoSocialCrossEnabled() {
		return // 默认关：零行为变化
	}
	if !inSharedWorld(state) {
		return // 非共享世界局（私有档/flag 关）：跨玩家自动发起不触发，零影响
	}
	worldID := strings.TrimSpace(state.WorldID)
	zoneID := strings.TrimSpace(state.CurrentZoneID)
	if worldID == "" || zoneID == "" {
		return
	}
	turn := state.TurnState.Turn
	if crossSocialEveryNTurns <= 0 || turn%crossSocialEveryNTurns != 0 {
		return // 低频确定性触发
	}
	epoch := turn / crossSocialEveryNTurns

	// actor 池：本 session 存活 protagonist（玩家角色）。只有「我的角色」能作为本侧发起方（绝不替别玩家发起）。
	actors := make([]unit.Record, 0, len(units))
	for i := range units {
		u := units[i]
		if u.Status.LifeState == unit.LifeStateDead || u.Status.LivesRemaining <= 0 {
			continue
		}
		if !isProtagonistUnit(state, &u) {
			continue // 仅玩家角色作发起方（背景 NPC 的跨玩家社交不在 Phase 3 范围）
		}
		actors = append(actors, u)
	}
	if len(actors) == 0 {
		return
	}

	// peer 池：同区跨 session 的存活 protagonist（别玩家角色），只读载入、确定性排序截断。
	peers := service.sameZoneCrossSessionProtagonists(ctx, state, zoneID)
	if len(peers) == 0 {
		return
	}

	initiated := 0
	for ai := range actors {
		if initiated >= crossSocialMaxInitiations {
			break
		}
		actor := actors[ai]
		relations := service.loadOutgoingRelationMap(ctx, actor.ID)
		for pi := range peers {
			if initiated >= crossSocialMaxInitiations {
				break
			}
			peer := peers[pi]
			if peer.ID == actor.ID {
				continue
			}
			// 概率门：FNV(world+pair+epoch)%Buckets < Hits 才有资格本周期发起（确定性稀疏化，非每周期对同一对刷）。
			if !crossSocialShouldInitiate(worldID, actor.ID, peer.ID, epoch) {
				continue
			}
			interaction := classifyCrossInteraction(relations[peer.ID])
			importance := socialImportanceFor(interaction)
			// 统一管线：RecordSevenInteraction 先 append cross_event（事实源），再按后果层 consent_gate 路由——
			// 层1(结识) 立即只改**本侧** actor→peer 关系；层2/3(反目/复仇) 建 pending、给（可能离线的）peer 投回应卡
			// （用 peer 自己 session，Phase0 seam）。本侧绝不直写 peer 的 units/relations。
			if _, err := service.RecordSevenInteraction(ctx, worldID, actor.ID, peer.ID, interaction, importance); err != nil {
				continue // best-effort：内部已含跨分片安全，吞错绝不中断
			}
			appendLog(state, "cross_social",
				crossSocialLogText(actor.DisplayName(), peer.DisplayName(), interaction), actor.ID, peer.ID)
			initiated++
		}
	}
}

// sameZoneCrossSessionProtagonists 只读拉同区（worldID#zoneID）跨 session 的存活 protagonist（别玩家角色）。
// 复用 ListActiveByRegion（Phase 2 已把同区单位锚到复合 region）。过滤：剔除本 session 单位、非 protagonist、死亡/无命。
// 按 unitID 确定性排序 + 截断（view-independent、可复现）。**只读**，绝不写他人 units。
func (service *Service) sameZoneCrossSessionProtagonists(ctx context.Context, state *State, zoneID string) []unit.Record {
	regionID := sharedRegionID(state.WorldID, zoneID)
	if regionID == "" {
		return nil
	}
	all, err := service.units.ListActiveByRegion(ctx, regionID)
	if err != nil || len(all) == 0 {
		return nil
	}
	out := make([]unit.Record, 0, len(all))
	for i := range all {
		p := all[i]
		if strings.TrimSpace(p.SessionID) == strings.TrimSpace(state.ID) {
			continue // 本 session 单位绝不当跨玩家 target
		}
		if p.Status.LifeState == unit.LifeStateDead || p.Status.LivesRemaining <= 0 {
			continue
		}
		if p.Identity.LifecycleClass != unit.LifecycleProtagonist {
			continue // Phase 3 只让别玩家的 protagonist 作 target（共享 NPC 是 Phase 4）
		}
		out = append(out, p)
		if len(out) >= crossSocialMaxPeers {
			break // ListActiveByRegion 已按 (last_active_tick, id) 排序，截断确定性
		}
	}
	return out
}

// classifyCrossInteraction 据 actor→peer 的现有关系（可能为零值=初遇）确定性挑一种跨玩家交互：
//   - 高竞争且高戒备 → 复仇（vengeance，层3）；
//   - 高竞争或高戒备（未到复仇量级）→ 反目（fallout，层2）；
//   - 其余（含初遇、彼此尚无明显敌意）→ 结识（acquaint，层1）。
//
// 设计：初遇默认走「结识」（先建立善意），让真人相遇的第一拍是友好的；敌对才升级——与 social_scan 的优先级口径一致（敌意先于善意）。
func classifyCrossInteraction(row relationPromptRow) SevenInteraction {
	switch {
	case row.Rivalry >= crossSocialRivalryHot && row.Fear >= crossSocialFearHot:
		return InteractionVengeance
	case row.Rivalry >= crossSocialRivalryHot || row.Fear >= crossSocialFearHot:
		return InteractionFallout
	default:
		return InteractionAcquaint
	}
}

// crossSocialShouldInitiate 概率门：用 FNV(world+actor->peer+epoch) 取确定性 [0,Buckets) 桶，落 [0,Hits) 才发起。
// 使「有资格的对」在某周期约 Hits/Buckets 概率发起一次——确定性稀疏化（无全局 rand、可复现），不每周期对同一对刷。
func crossSocialShouldInitiate(worldID, actorID, peerID string, epoch int) bool {
	if crossSocialInitiateProbBuckets <= 0 {
		return true
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte("cross_social:" + worldID + ":" + actorID + "->" + peerID + ":" + strconv.Itoa(epoch)))
	return int(h.Sum64()%uint64(crossSocialInitiateProbBuckets)) < crossSocialInitiateProbHits
}

// crossSocialLogText 给一次跨玩家自动发起生成本侧可读日志（祖魂语气，克制；只描述本侧 actor 的动作）。
func crossSocialLogText(actorName, peerName string, interaction SevenInteraction) string {
	switch interaction {
	case InteractionVengeance:
		return actorName + " 朝 " + peerName + " 那缕命途，递出了一桩了断的意。"
	case InteractionFallout:
		return actorName + " 与 " + peerName + " 之间，生了嫌隙。"
	default: // 结识
		return actorName + " 在这片地方，与 " + peerName + " 结识了。"
	}
}
