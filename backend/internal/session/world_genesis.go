package session

// 文件说明：创世序章（genesis preamble）入史规则（模块1）——给每个共享世界写一条卷首楔子，
// 史官口吻交代「史前无名、三道（自由/秩序/混乱）并立」，末句把舞台让给后人亲历，呼应
// 「真历史由玩家涌现」的设计宪法。序章只作叙事露出（前端世界编年史面板置顶卷首渲染），
// 不进任何账本、不碰保护状态字段、不走 status.Mutator——经 recordWorldChronicleBestEffort
// 的旁路流程事件 + world_chronicle 表落地，全 best-effort 吞错。
//
// 幂等/每世界一次：ensureWorldGenesis 先查该 world 是否已有 genesis 条目，无则写一条。
// 多玩家降生同一共享世界时不应重复写序章（查重在先；并发下偶尔重复一条可接受，但优先查重收敛）。
//
// flag QUNXIANG_WORLD_GENESIS 默认关 → 整方法 no-op 返空串、零 DB 写、零行为变化（可灰度按世界开启）。

import (
	"context"
	"fmt"
	"strings"

	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/world"
)

// 创世序章的固定盖戳：era 用「混沌之初」（先于任何纪元的史前），world_tick=0（开天辟地之刻），
// importance=10（卷首至高，永不被容量裁剪挤出）。
const (
	worldGenesisEra        = "混沌之初"
	worldGenesisWorldTick  = 0
	worldGenesisImportance = 10
)

// worldGenesisEnabled 读 QUNXIANG_WORLD_GENESIS，默认开（显式 false/0/no/off 可关 →
// ensureWorldGenesis 整方法 no-op、零行为变化、零 DB 写）。
func worldGenesisEnabled() bool {
	return featureflags.EnabledWithDefault("QUNXIANG_WORLD_GENESIS", true)
}

// worldGenesisNarrative 构造创世序章文案（史官楔子口吻）：含 WorldDisplayName(worldID)、三阵营分立，
// 末句把舞台让给后人亲历（呼应「真历史由玩家涌现」）。纯规则模板、确定性、无 LLM。
func worldGenesisNarrative(worldID string) string {
	name := world.WorldDisplayName(worldID)
	return fmt.Sprintf(
		"%s——史前无名，三道并立：自由不羁、秩序为纲、混乱破立。此卷之后，再无神谕预言；凡有所载，皆后人亲历亲见，血火亲书。",
		name,
	)
}

// ensureWorldGenesis 幂等地为某世界写入创世序章（每世界一条）。
//   - flag QUNXIANG_WORLD_GENESIS 默认关 → 整方法 no-op、返回空串。
//   - worldID 为空（旧单图档/未接多世界）→ no-op、返回空串（序章必挂世界）。
//   - 该 world 已有 genesis 条目 → 不重复写、返回空串（幂等：先查后写）。
//   - 否则写一条 category=genesis、era=混沌之初、world_tick=0、importance=10 的序章并返回其 id。
//
// 全 best-effort：查重/写入任一步出错都吞掉、返回空串、绝不阻断降生主流程（接入点 mainworld.go 忽略返回值）。
func (service *Service) ensureWorldGenesis(ctx context.Context, worldID string) string {
	if service == nil || service.db == nil {
		return ""
	}
	worldID = strings.TrimSpace(worldID)
	if worldID == "" || !worldGenesisEnabled() {
		return ""
	}
	// 幂等查重：该 world 已有 genesis 条目则直接返回（避免多玩家同共享世界重复写卷首）。
	// best-effort：查重 SQL 出错时为安全计不写（避免错误地重复写），返回空串。
	var existing int
	if err := service.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM world_chronicle WHERE world_id = ? AND category = ?`,
		worldID, WorldChronicleGenesis,
	).Scan(&existing); err != nil {
		return ""
	}
	if existing > 0 {
		return ""
	}
	// 序章直接经 world.RecordWorldChronicle 落库（不复用 recordWorldChronicleBestEffort：
	// 那条会按累计重大事件算 era，而序章 era 须固定「混沌之初」、world_tick 须固定 0）。
	// 旁路一条流程事件由 recordWorldChronicleBestEffort 负责的世界级锚点对序章非必要——
	// 序章是卷首派生物，落进 world_chronicle 表即足以让前端置顶渲染。
	id, err := world.RecordWorldChronicle(ctx, service.db, dbdialect.IsMySQL(service.db), world.WorldChronicleEntry{
		WorldID:     worldID,
		WorldTick:   worldGenesisWorldTick,
		Era:         worldGenesisEra,
		Category:    WorldChronicleGenesis,
		TitleZH:     "创世序章",
		NarrativeZH: worldGenesisNarrative(worldID),
		Importance:  worldGenesisImportance,
	})
	if err != nil {
		return "" // 吞错：写表失败不影响降生
	}
	return id
}
