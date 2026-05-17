package world

// 文件说明：视野计算模块，基于地形惩罚与可见范围扩散求解单位可见格集合。

import (
	"container/heap"
	"fmt"
)

// ComputeVisibleTiles 计算单位在当前地形下的可见坐标集合。
func ComputeVisibleTiles(snapshot MapSnapshot, origin Coord, baseRange int) ([]Coord, error) {
	if !snapshot.inBounds(origin.Q, origin.R) {
		return nil, fmt.Errorf("origin %d,%d out of bounds", origin.Q, origin.R)
	}

	if baseRange <= 0 {
		baseRange = 5
	}

	originTile := snapshot.tile(origin.Q, origin.R)
	effectiveRange := baseRange + (terrainVisionRange(originTile.Terrain) - 5)
	if effectiveRange < 1 {
		effectiveRange = 1
	}

	distances := map[Coord]int{origin: 0}
	queue := &priorityQueue{{coord: origin, cost: 0}}
	heap.Init(queue)

	for queue.Len() > 0 {
		current := heap.Pop(queue).(node)
		if current.cost > effectiveRange {
			continue
		}

		for _, neighbor := range axialNeighbors(current.coord) {
			if !snapshot.inBounds(neighbor.Q, neighbor.R) {
				continue
			}

			stepCost := current.cost + 1 + terrainVisionPenalty(snapshot.tile(neighbor.Q, neighbor.R).Terrain)
			best, seen := distances[neighbor]
			if seen && stepCost >= best {
				continue
			}

			if stepCost > effectiveRange {
				continue
			}

			distances[neighbor] = stepCost
			heap.Push(queue, node{coord: neighbor, cost: stepCost})
		}
	}

	visible := make([]Coord, 0, len(distances))
	for coord := range distances {
		visible = append(visible, coord)
	}

	return visible, nil
}

// terrainVisionRange 返回地形提供的基础视野半径。
func terrainVisionRange(terrain TerrainID) int {
	for _, definition := range TerrainCatalog() {
		if definition.ID == terrain {
			return definition.VisionRange
		}
	}
	return 5
}

// terrainVisionPenalty 返回地形对视野传播的额外代价。
func terrainVisionPenalty(terrain TerrainID) int {
	switch terrain {
	case TerrainForest, TerrainSwamp, TerrainRiver, TerrainRuins, TerrainVillage, TerrainCity, TerrainSnowfield:
		return 1
	default:
		return 0
	}
}

// node 结构体用于承载该模块的核心数据。
type node struct {
	coord Coord
	cost  int
}

// priorityQueue 类型定义用于统一该模块的数据表达。
type priorityQueue []node

// Len 返回优先队列长度。
func (queue priorityQueue) Len() int {
	return len(queue)
}

// Less 定义优先队列排序规则（cost 越小优先级越高）。
func (queue priorityQueue) Less(i int, j int) bool {
	return queue[i].cost < queue[j].cost
}

// Swap 交换队列中两个节点位置。
func (queue priorityQueue) Swap(i int, j int) {
	queue[i], queue[j] = queue[j], queue[i]
}

// Push 向优先队列压入一个节点。
func (queue *priorityQueue) Push(value any) {
	*queue = append(*queue, value.(node))
}

// Pop 弹出优先队列末尾元素（由 heap 包控制顺序）。
func (queue *priorityQueue) Pop() any {
	old := *queue
	last := old[len(old)-1]
	*queue = old[:len(old)-1]
	return last
}
