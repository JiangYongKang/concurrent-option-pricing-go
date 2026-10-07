package scenario

import (
	"context"
	"errors"

	"github.com/highcumontoa/concurrent-option-pricing-go/calibration"
	"github.com/highcumontoa/concurrent-option-pricing-go/engine"
	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
	"github.com/highcumontoa/concurrent-option-pricing-go/pricing"
)

// Outcome 是单个标的在单个市场状态下的判定分类，三者互斥：
//
//   - OutcomePriced：算出了可交付的结果（可能来自降级路径）；
//   - OutcomeRejected：被明确拒绝，没有任何价格结果；
//   - OutcomeDegraded：首选方法未成功，按策略改用备选方法后算出结果。
type Outcome int

const (
	OutcomePriced Outcome = iota
	OutcomeRejected
	OutcomeDegraded
)

// String 返回判定的中文名称，便于日志与报告展示。
func (o Outcome) String() string {
	switch o {
	case OutcomePriced:
		return "priced"
	case OutcomeRejected:
		return "rejected"
	case OutcomeDegraded:
		return "degraded"
	default:
		return "unknown"
	}
}

// Reason 是拒绝或降级原因的分门别类枚举。
type Reason int

const (
	ReasonNone Reason = iota
	// ReasonDataUnavailable：市场状态数据本身缺失/非法（绝不默认补齐）。
	ReasonDataUnavailable
	// ReasonNotCalibratable：数据合法但曲线/曲面无法校准。
	ReasonNotCalibratable
	// ReasonMethodNotApplicable：方法不适用于该工具（如美式用解析解/终端 MC）。
	ReasonMethodNotApplicable
	// ReasonNotConverged：采样预算用尽后标准误仍超容差。
	ReasonNotConverged
	// ReasonParameterOutOfRange：行权价/期限/波动率等参数越界。
	ReasonParameterOutOfRange
	// ReasonBudgetExceeded：整批共享的路径预算用尽，按序拒绝。
	ReasonBudgetExceeded
	// ReasonCanceled：该状态所属的执行上下文被取消。
	ReasonCanceled
	// ReasonTimeout：该状态自身的超时触发。
	ReasonTimeout
	// ReasonUnknown：未归入上述类别的内部意外错误。
	ReasonUnknown
)

// String 返回原因的稳定短名，用于报告与日志。
func (r Reason) String() string {
	switch r {
	case ReasonNone:
		return "none"
	case ReasonDataUnavailable:
		return "data-unavailable"
	case ReasonNotCalibratable:
		return "not-calibratable"
	case ReasonMethodNotApplicable:
		return "method-not-applicable"
	case ReasonNotConverged:
		return "not-converged"
	case ReasonParameterOutOfRange:
		return "parameter-out-of-range"
	case ReasonBudgetExceeded:
		return "budget-exceeded"
	case ReasonCanceled:
		return "canceled"
	case ReasonTimeout:
		return "timeout"
	default:
		return "unknown"
	}
}

// classifyError 把底层错误映射为互斥的 (原因, 是否降级可恢复)。
//
// 判定依据全部通过 errors.Is 对哨兵错误做归类，上下文取消与超时
// 优先于一切业务错误（被取消的结果不允许伪装成业务拒绝）。
func classifyError(err error) Reason {
	switch {
	case err == nil:
		return ReasonNone
	case errors.Is(err, context.DeadlineExceeded):
		return ReasonTimeout
	case errors.Is(err, context.Canceled):
		return ReasonCanceled
	case errors.Is(err, ErrStateDataUnavailable):
		return ReasonDataUnavailable
	case errors.Is(err, ErrStateNotCalibratable):
		return ReasonNotCalibratable
	case errors.Is(err, engine.ErrBudgetExceeded):
		return ReasonBudgetExceeded
	case errors.Is(err, pricing.ErrNotConverged):
		return ReasonNotConverged
	case errors.Is(err, pricing.ErrMethodNotApplicable):
		return ReasonMethodNotApplicable
	case errors.Is(err, pricing.ErrParameterOutOfRange):
		return ReasonParameterOutOfRange
	case errors.Is(err, calibration.ErrVolNotSolvable),
		errors.Is(err, calibration.ErrCalendarArbitrage),
		errors.Is(err, calibration.ErrCurveNotBuildable):
		return ReasonNotCalibratable
	case errors.Is(err, marketdata.ErrMissingQuote),
		errors.Is(err, marketdata.ErrDuplicateQuote),
		errors.Is(err, marketdata.ErrInvertedMaturity),
		errors.Is(err, marketdata.ErrInvalidValue):
		return ReasonDataUnavailable
	default:
		return ReasonUnknown
	}
}

// degradable 报告某原因是否允许按降级策略改走备选方法。
//
// 只有“首选方法自身”的问题才降级；数据问题、预算问题、取消/超时、
// 参数越界都不允许靠换方法蒙混过去。
func (r Reason) degradable() bool {
	return r == ReasonNotConverged || r == ReasonMethodNotApplicable
}
