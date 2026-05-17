package turns

// 文件说明：回合阶段状态机，实现 deployment/execution 的推进、快进与剩余时长计算。

import "time"

// Phase 类型定义用于统一该模块的数据表达。
type Phase string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	PhaseDeployment Phase = "deployment"
	PhaseExecution  Phase = "execution"
)

// Budgets 结构体用于承载该模块的核心数据。
type Budgets struct {
	Strategic      time.Duration `json:"strategic"`
	Deployment     time.Duration `json:"deployment"`
	Execution      time.Duration `json:"execution"`
	FastForwardCap time.Duration `json:"fast_forward_cap"`
}

// DefaultBudgets 返回默认的三阶段时长预算与快进上限。
func DefaultBudgets() Budgets {
	return Budgets{
		Strategic:      time.Minute,
		Deployment:     time.Minute,
		Execution:      150 * time.Second,
		FastForwardCap: 15 * time.Second,
	}
}

// NormalizeBudgets 补齐旧存档或异常输入中的阶段预算，并把部署阶段统一为 1 分钟。
func NormalizeBudgets(budgets Budgets) Budgets {
	defaults := DefaultBudgets()
	if budgets.Strategic <= 0 {
		budgets.Strategic = defaults.Strategic
	}
	if budgets.Deployment != defaults.Deployment {
		budgets.Deployment = defaults.Deployment
	}
	if budgets.Execution <= 0 {
		budgets.Execution = defaults.Execution
	}
	if budgets.FastForwardCap <= 0 {
		budgets.FastForwardCap = defaults.FastForwardCap
	}
	return budgets
}

// State 结构体用于承载该模块的核心数据。
type State struct {
	Turn           int       `json:"turn"`
	Phase          Phase     `json:"phase"`
	PhaseStartedAt time.Time `json:"phase_started_at"`
	PhaseEndsAt    time.Time `json:"phase_ends_at"`
	Budgets        Budgets   `json:"budgets"`
}

// NewState 创建回合状态初始值（T1 + deployment 阶段）。
func NewState(now time.Time, budgets Budgets) State {
	now = now.UTC()
	budgets = NormalizeBudgets(budgets)
	return State{
		Turn:           1,
		Phase:          PhaseDeployment,
		PhaseStartedAt: now,
		PhaseEndsAt:    now.Add(budgets.Deployment),
		Budgets:        budgets,
	}
}

// Advance 将阶段推进到下一个状态；执行阶段结束后会进入下一回合的部署阶段。
func (state *State) Advance(now time.Time) {
	now = now.UTC()
	state.Budgets = NormalizeBudgets(state.Budgets)
	switch state.Phase {
	case PhaseDeployment:
		state.Phase = PhaseExecution
		state.PhaseStartedAt = now
		state.PhaseEndsAt = now.Add(state.Budgets.Execution)
	default:
		state.Turn++
		state.Phase = PhaseDeployment
		state.PhaseStartedAt = now
		state.PhaseEndsAt = now.Add(state.Budgets.Deployment)
	}
}

// Tick 根据当前时间推进阶段，必要时连续跨越多个已超时阶段。
func (state *State) Tick(now time.Time) {
	now = now.UTC()
	for !now.Before(state.PhaseEndsAt) {
		state.Advance(state.PhaseEndsAt)
	}
}

// FastForward 对当前阶段执行“快进”，把剩余时间压到 FastForwardCap 以内。
func (state *State) FastForward(now time.Time) {
	now = now.UTC()
	remaining := state.Remaining(now)
	if remaining > state.Budgets.FastForwardCap {
		state.PhaseEndsAt = now.Add(state.Budgets.FastForwardCap)
	}
}

// Remaining 返回当前阶段距离截止时间的剩余时长。
func (state State) Remaining(now time.Time) time.Duration {
	now = now.UTC()
	if now.After(state.PhaseEndsAt) {
		return 0
	}
	return state.PhaseEndsAt.Sub(now)
}

// PlannedTurnDuration 返回一个完整回合（部署+执行）理论总时长。
func (state State) PlannedTurnDuration() time.Duration {
	return state.Budgets.Deployment + state.Budgets.Execution
}
