package session

// 文件说明：维护战场脚本目录并按脚本 ID/种子生成可复现的地形模板与地标分布规则。

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"strings"

	"qunxiang/backend/internal/world"
)

const defaultBattlefieldScriptID = "crossroads_ruins"

// BattlefieldScript 结构体用于承载该模块的核心数据。
type BattlefieldScript struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Summary     string `json:"summary"`
}

var battlefieldScriptCatalog = []BattlefieldScript{
	{ID: "crossroads_ruins", DisplayName: "十字废墟", Summary: "中央废墟 + 十字道路，适合正面拉扯。"},
	{ID: "mountain_pass", DisplayName: "山隘关口", Summary: "双侧山脊夹路，强调关口争夺。"},
	{ID: "twin_cities", DisplayName: "双城对峙", Summary: "两端城市互相牵制，中线道路贯通。"},
	{ID: "swamp_delta", DisplayName: "沼泽三角洲", Summary: "河道与沼泽交错，机动受限。"},
	{ID: "desert_outpost", DisplayName: "荒漠哨站", Summary: "沙地与绿洲并存，补给点集中。"},
	{ID: "forest_maze", DisplayName: "林海迷径", Summary: "森林密布，适合伏击与绕后。"},
	{ID: "frozen_front", DisplayName: "霜原前线", Summary: "雪原重压机动，道路价值更高。"},
	{ID: "river_fork", DisplayName: "双岔河口", Summary: "河道分叉压缩战线，桥口即命门。"},
	{ID: "iron_ridge", DisplayName: "铁脊矿岭", Summary: "山地与矿脉地形，围绕高地博弈。"},
	{ID: "grassland_charge", DisplayName: "草原突击", Summary: "开阔草原，强调冲锋与长机动。"},
	{ID: "village_belt", DisplayName: "村镇带", Summary: "村镇节点串联，适合据点拉扯。"},
	{ID: "ruins_ring", DisplayName: "环形遗迹", Summary: "外围遗迹环绕，中心道路会战。"},
}

// BattlefieldScripts 返回战场脚本目录的副本，避免调用方直接修改全局目录。
func BattlefieldScripts() []BattlefieldScript {
	result := make([]BattlefieldScript, 0, len(battlefieldScriptCatalog))
	result = append(result, battlefieldScriptCatalog...)
	return result
}

// IsBattlefieldScriptID 判断给定脚本 ID 是否存在于目录中。
func IsBattlefieldScriptID(scriptID string) bool {
	scriptID = strings.TrimSpace(scriptID)
	if scriptID == "" {
		return false
	}
	for _, script := range battlefieldScriptCatalog {
		if script.ID == scriptID {
			return true
		}
	}
	return false
}

// normalizeBattlefieldScriptID 规范化战场脚本 ID。
// 优先使用请求值；无效时依据种子稳定挑选，最终回退默认脚本。
func normalizeBattlefieldScriptID(requestedID string, seed int64) string {
	requestedID = strings.TrimSpace(requestedID)
	if requestedID != "" && IsBattlefieldScriptID(requestedID) {
		return requestedID
	}
	if len(battlefieldScriptCatalog) == 0 {
		return defaultBattlefieldScriptID
	}
	if seed == 0 {
		return defaultBattlefieldScriptID
	}

	index := int(deterministicBattlefieldScriptRoll(seed) * float64(len(battlefieldScriptCatalog)))
	if index < 0 {
		index = 0
	}
	if index >= len(battlefieldScriptCatalog) {
		index = len(battlefieldScriptCatalog) - 1
	}
	return battlefieldScriptCatalog[index].ID
}

// battlefieldScriptDisplayName 返回脚本展示名，未知脚本回退默认脚本名。
func battlefieldScriptDisplayName(scriptID string) string {
	for _, script := range battlefieldScriptCatalog {
		if script.ID == scriptID {
			return script.DisplayName
		}
	}
	for _, script := range battlefieldScriptCatalog {
		if script.ID == defaultBattlefieldScriptID {
			return script.DisplayName
		}
	}
	return defaultBattlefieldScriptID
}

// deterministicBattlefieldScriptRoll 用种子生成可复现的脚本选择随机值。
func deterministicBattlefieldScriptRoll(seed int64) float64 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(fmt.Sprintf("battlefield-script:%d", seed)))
	return float64(hasher.Sum32()%10000) / 10000
}

// battleTerrainForScript 根据脚本模板与坐标返回地形类型。
// 每个脚本代表一套固定布局规则，少量分支可引入轻随机变化。
func battleTerrainForScript(scriptID string, rng *rand.Rand, q int, r int, width int, height int) world.TerrainID {
	battlefieldWidth := width
	battlefieldHeight := height
	switch scriptID {
	case "mountain_pass":
		switch {
		case r == battlefieldHeight/2:
			return world.TerrainRoad
		case (q == 2 || q == 4) && r != battlefieldHeight/2:
			return world.TerrainMountain
		case q == 0 || q == battlefieldWidth-1:
			return world.TerrainGrassland
		case q == battlefieldWidth/2 && (r == 1 || r == battlefieldHeight-2):
			return world.TerrainRuins
		default:
			if (q+r)%3 == 0 {
				return world.TerrainForest
			}
			return world.TerrainPlains
		}
	case "twin_cities":
		switch {
		case (q == 1 && r == battlefieldHeight/2) || (q == battlefieldWidth-2 && r == battlefieldHeight/2):
			return world.TerrainCity
		case r == battlefieldHeight/2:
			return world.TerrainRoad
		case q == battlefieldWidth/2 && (r == 1 || r == battlefieldHeight-2):
			return world.TerrainVillage
		case q == 0 || q == battlefieldWidth-1:
			return world.TerrainRiverValley
		default:
			if (q+r)%2 == 0 {
				return world.TerrainPlains
			}
			return world.TerrainGrassland
		}
	case "swamp_delta":
		switch {
		case q == battlefieldWidth/2 || r == battlefieldHeight/2:
			return world.TerrainRiver
		case q+r >= 8 && q+r <= 10:
			return world.TerrainSwamp
		case q == 1 && r == 1:
			return world.TerrainVillage
		case q == battlefieldWidth-2 && r == battlefieldHeight-2:
			return world.TerrainRuins
		default:
			if (q+r)%3 == 0 {
				return world.TerrainRiverValley
			}
			return world.TerrainGrassland
		}
	case "desert_outpost":
		switch {
		case q == battlefieldWidth/2 && r == battlefieldHeight/2:
			return world.TerrainVillage
		case q == battlefieldWidth/2 || r == battlefieldHeight/2:
			return world.TerrainRiverValley
		case q == 1 && r == 1:
			return world.TerrainRuins
		case q == battlefieldWidth-2 && r == battlefieldHeight-2:
			return world.TerrainCity
		default:
			if (q+r)%2 == 0 {
				return world.TerrainDesert
			}
			return world.TerrainGrassland
		}
	case "forest_maze":
		switch {
		case q == battlefieldWidth/2 && r == battlefieldHeight/2:
			return world.TerrainRuins
		case q == battlefieldWidth/2 || r == battlefieldHeight/2:
			return world.TerrainRoad
		case q == 0 || r == 0 || q == battlefieldWidth-1 || r == battlefieldHeight-1:
			return world.TerrainForest
		default:
			if (q+r)%2 == 0 {
				return world.TerrainForest
			}
			return world.TerrainSwamp
		}
	case "frozen_front":
		switch {
		case q == battlefieldWidth/2 || r == battlefieldHeight/2:
			return world.TerrainRoad
		case q == 1 && r == battlefieldHeight/2:
			return world.TerrainVillage
		case q == battlefieldWidth-2 && r == battlefieldHeight/2:
			return world.TerrainRuins
		default:
			if (q+r)%3 == 0 {
				return world.TerrainMountain
			}
			return world.TerrainSnowfield
		}
	case "river_fork":
		switch {
		case q == battlefieldWidth/2 || r == battlefieldHeight/2 || (q+r == battlefieldWidth-1):
			return world.TerrainRiver
		case q == 1 && r == 1:
			return world.TerrainVillage
		case q == battlefieldWidth-2 && r == 1:
			return world.TerrainVillage
		case q == battlefieldWidth/2 && r == battlefieldHeight-2:
			return world.TerrainCity
		default:
			if (q+r)%2 == 0 {
				return world.TerrainRiverValley
			}
			return world.TerrainGrassland
		}
	case "iron_ridge":
		switch {
		case (q == 2 || q == 4) && (r >= 1 && r <= battlefieldHeight-2):
			return world.TerrainMountain
		case q == battlefieldWidth/2 || r == battlefieldHeight/2:
			return world.TerrainRoad
		case q == battlefieldWidth/2 && r == battlefieldHeight/2:
			return world.TerrainRuins
		case (q == 1 && r == battlefieldHeight-2) || (q == battlefieldWidth-2 && r == 1):
			return world.TerrainVillage
		default:
			if (q+r)%2 == 0 {
				return world.TerrainPlains
			}
			return world.TerrainForest
		}
	case "grassland_charge":
		switch {
		case q == battlefieldWidth/2 || r == battlefieldHeight/2:
			return world.TerrainRoad
		case (q == 1 && r == 1) || (q == battlefieldWidth-2 && r == battlefieldHeight-2):
			return world.TerrainVillage
		case q == battlefieldWidth/2 && r == battlefieldHeight/2:
			return world.TerrainRuins
		default:
			if (q+r)%4 == 0 {
				return world.TerrainPlains
			}
			return world.TerrainGrassland
		}
	case "village_belt":
		switch {
		case q == battlefieldWidth/2 && r == battlefieldHeight/2:
			return world.TerrainCity
		case q == battlefieldWidth/2 || r == battlefieldHeight/2:
			return world.TerrainRoad
		case (q == 1 && r == 1) || (q == battlefieldWidth-2 && r == 1) || (q == 1 && r == battlefieldHeight-2) || (q == battlefieldWidth-2 && r == battlefieldHeight-2):
			return world.TerrainVillage
		default:
			if (q+r)%2 == 0 {
				return world.TerrainPlains
			}
			return world.TerrainRiverValley
		}
	case "ruins_ring":
		switch {
		case q == battlefieldWidth/2 && r == battlefieldHeight/2:
			return world.TerrainRoad
		case q == 1 || q == battlefieldWidth-2 || r == 1 || r == battlefieldHeight-2:
			return world.TerrainRuins
		case q == battlefieldWidth/2 || r == battlefieldHeight/2:
			return world.TerrainRoad
		default:
			if (q+r)%2 == 0 {
				return world.TerrainForest
			}
			return world.TerrainPlains
		}
	default:
		return battleTerrainForDefault(rng, q, r, width, height)
	}
}

// battleTerrainForDefault 生成默认战场脚本地形布局。
func battleTerrainForDefault(rng *rand.Rand, q int, r int, width int, height int) world.TerrainID {
	battlefieldWidth := width
	battlefieldHeight := height
	switch {
	case q == battlefieldWidth/2 && r == battlefieldHeight/2:
		return world.TerrainRuins
	case q == battlefieldWidth/2 || r == battlefieldHeight/2:
		return world.TerrainRoad
	case (q == 2 && r == 2) || (q == 4 && r == 4) || (q == 2 && r == 4) || (q == 4 && r == 2):
		return world.TerrainForest
	case q == 0 || r == 0 || q == battlefieldWidth-1 || r == battlefieldHeight-1:
		if (q+r)%2 == 0 {
			return world.TerrainGrassland
		}
		return world.TerrainRiverValley
	default:
		switch rng.Intn(4) {
		case 0:
			return world.TerrainPlains
		case 1:
			return world.TerrainForest
		default:
			return world.TerrainRoad
		}
	}
}

// battlefieldLandmarkFor 为关键地块附加地标标签，供前端渲染和脚本逻辑识别。
func battlefieldLandmarkFor(scriptID string, q int, r int, terrain world.TerrainID, width int, height int) string {
	battlefieldWidth := width
	battlefieldHeight := height
	switch terrain {
	case world.TerrainCity:
		return "city"
	case world.TerrainVillage:
		return "village"
	case world.TerrainRuins:
		if q == battlefieldWidth/2 && r == battlefieldHeight/2 {
			return "central_ruin"
		}
		return "ruins"
	}

	if scriptID == defaultBattlefieldScriptID && q == battlefieldWidth/2 && r == battlefieldHeight/2 {
		return "central_ruin"
	}
	return ""
}
