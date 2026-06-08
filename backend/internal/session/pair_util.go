package session

// 文件说明：单位配对判定的通用小工具，供关系/对话等模块复用。

// samePair 判断两组 actor/target 是否表示同一对单位（忽略先后顺序）。
func samePair(actorA string, actorB string, leftID string, rightID string) bool {
	return (actorA == leftID && actorB == rightID) || (actorA == rightID && actorB == leftID)
}
