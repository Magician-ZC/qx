// Package attachment 计算「牵挂等级」（设计宪法 §4.4）：Attachment = 100·sigmoid(共鸣+在世+回访+共创)。
//
// 牵挂是后果分级闸的钥匙：只有对一个你**深深牵挂、且陪伴已久**的角色，才可能发生不可逆的后果（层3）；
// 萍水相逢的角色被硬锁在「可恢复」（保护新手心血，宪法 §4.7 / encounter.PenaltyCap）。
//
// 铁律：**不可付费购买**——本函数的输入只有「共鸣/在世/回访/共创」四类挣来的信号，没有任何金钱项。
// 纯函数、确定性、可复算。
package attachment

import "math"

// Inputs 是牵挂的四类来源信号。
type Inputs struct {
	Resonance    float64 // 共鸣 [0,1]：角色与你的契合（忠诚/关系契合度）
	DaysAlive    int     // 在世天数（陪伴时长）
	ReturnVisits int     // 你回访看她的次数
	CoCreations  int     // 你塑造她的次数（接管/处理她的待决策）
}

// 权重（和为 1，[可调]）：共创与共鸣最重，回访次之，单纯在世最轻——牵挂靠互动养成，不是挂机攒出来的。
const (
	wResonance = 0.30
	wDaysAlive = 0.20
	wVisits    = 0.22
	wCoCreate  = 0.28

	daysHalfLife  = 7.0 // 在世约 7 天达到 ~0.63 的饱和
	visitsSoftCap = 3.0 // 回访软上限
	coCreateSoft  = 3.0 // 共创软上限
	sigmoidK      = 6.0 // sigmoid 陡度
)

// Compute 返回牵挂等级 [0,100]。对每个输入单调不减；零输入→低牵挂，满输入→接近 100。
func Compute(in Inputs) float64 {
	res := clamp01(in.Resonance)
	days := saturate(float64(maxInt(0, in.DaysAlive)), daysHalfLife) // 1 - exp(-d/τ) ∈ [0,1)
	visits := ratio(float64(maxInt(0, in.ReturnVisits)), visitsSoftCap)
	coc := ratio(float64(maxInt(0, in.CoCreations)), coCreateSoft)

	raw := wResonance*res + wDaysAlive*days + wVisits*visits + wCoCreate*coc // ∈ [0,1]
	att := 100.0 / (1.0 + math.Exp(-sigmoidK*(raw-0.5)))
	return math.Round(att*100) / 100
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func saturate(x, halfLife float64) float64 {
	if x <= 0 || halfLife <= 0 {
		return 0
	}
	return 1 - math.Exp(-x/halfLife)
}

func ratio(x, soft float64) float64 {
	if x <= 0 {
		return 0
	}
	return x / (x + soft)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
