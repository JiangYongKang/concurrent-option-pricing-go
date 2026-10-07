package scenario

import (
	"context"
	"errors"

	"github.com/highcumontoa/concurrent-option-pricing-go/calibration"
	"github.com/highcumontoa/concurrent-option-pricing-go/engine"
	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
	"github.com/highcumontoa/concurrent-option-pricing-go/pricing"
)

// Outcome 是单个标的在单个市场状态下的判定结论。
type Outcome int

const (
	OutcomePriced   Outcome = iota // 算出了结果
	OutcomeDegraded                // 按策略降级（如 MC 未收敛/超预算回退二叉树）
	OutcomeRejected                // 被明确拒绝
)

func (o Outcome) String() string {
	switch o {
	case OutcomePriced:
		return "已定价"
	case OutcomeDegraded:
		return "已降级"
	case OutcomeRejected:
		return "已拒绝"
	}
	return "未知结论"
}

// Reason 是拒绝或降级的原因分类，互相可区分。
type Reason int

const (
	ReasonNone Reason = iota
	ReasonDataUnavailable     // 市场状态数据缺失/非法/不可校准
	ReasonScenarioUnknown     // 指定的市场状态不存在
	ReasonMethodNotApplicable // 方法不适用
	ReasonNotConverged        // 采样未收敛
	ReasonParameterOutOfRange // 参数越界
	ReasonBudgetExceeded      // 超出共享路径预算
	ReasonCanceled            // 该状态被取消
	ReasonTimeout             // 该状态超时
	ReasonOther               // 其他未分类失败
)

func (r Reason) String() string {
	switch r {
	case ReasonNone:
		return "无"
	case ReasonDataUnavailable:
		return "数据不可用"
	case ReasonScenarioUnknown:
		return "市场状态不存在"
	case ReasonMethodNotApplicable:
		return "方法不适用"
	case ReasonNotConverged:
		return "采样未收敛"
	case ReasonParameterOutOfRange:
		return "参数越界"
	case ReasonBudgetExceeded:
		return "超出路径预算"
	case ReasonCanceled:
		return "已取消"
	case ReasonTimeout:
		return "已超时"
	}
	return "其他失败"
}

// classify 把错误归类为可区分的原因。
// 数据缺失/非法（marketdata 校验、校准失败）一律归为数据不可用，
// 绝不允许被默认值蒙混成成功。
func classify(err error) Reason {
	switch {
	case err == nil:
		return ReasonNone
	case errors.Is(err, ErrUnknownScenario):
		return ReasonScenarioUnknown
	case errors.Is(err, context.Canceled):
		return ReasonCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return ReasonTimeout
	case errors.Is(err, engine.ErrBudgetExceeded):
		return ReasonBudgetExceeded
	case errors.Is(err, pricing.ErrMethodNotApplicable):
		return ReasonMethodNotApplicable
	case errors.Is(err, pricing.ErrNotConverged):
		return ReasonNotConverged
	case errors.Is(err, pricing.ErrParameterOutOfRange):
		return ReasonParameterOutOfRange
	case errors.Is(err, marketdata.ErrMissingQuote),
		errors.Is(err, marketdata.ErrDuplicateQuote),
		errors.Is(err, marketdata.ErrInvertedMaturity),
		errors.Is(err, marketdata.ErrInvalidValue),
		errors.Is(err, calibration.ErrCurveNotBuildable),
		errors.Is(err, calibration.ErrVolNotSolvable),
		errors.Is(err, calibration.ErrCalendarArbitrage):
		return ReasonDataUnavailable
	default:
		return ReasonOther
	}
}

// ItemResult 是单个标的在单个市场状态下的结果。
type ItemResult struct {
	ID      string
	Outcome Outcome
	Price   float64  // 仅 OutcomePriced / OutcomeDegraded 时有效
	Method  string   // 实际使用的方法（降级时为回退方法）
	Reason  Reason   // OutcomePriced 时为 ReasonNone
	Detail  string   // 拒绝/降级的具体原因（原始错误信息）
}

// ScenarioResult 是一个市场状态的整批结果。
// Err 非空表示状态级失败（数据不可用/未知状态），此时 Items 为空，
// 对比报告中该状态下所有标的按同一原因记为已拒绝。
type ScenarioResult struct {
	Name  string
	Items []ItemResult
	Err   error
}

// ScenarioValue 是某标的在指定状态下的取值（用于跨状态对比）。
type ScenarioValue struct {
	Scenario string
	Item     ItemResult
}

// Comparison 是单个标的的跨市场状态对比行。
type Comparison struct {
	ID     string
	Values []ScenarioValue // 顺序与运行时给定的状态顺序一致
}

// Report 是一次多市场状态评估的完整结果。
type Report struct {
	Scenarios  []ScenarioResult
	RequestIDs []string // 请求顺序，用于对比行的确定性输出
}

// Compare 按标的汇总跨市场状态对比结果。
// 行的顺序与请求顺序一致；每行内状态的顺序与运行时给定顺序一致。
// 状态级失败（Err 非空）时，该状态下每个标的都记为同一原因的已拒绝。
func (r *Report) Compare() []Comparison {
	rows := make([]Comparison, 0, len(r.RequestIDs))
	for _, id := range r.RequestIDs {
		row := Comparison{ID: id, Values: make([]ScenarioValue, 0, len(r.Scenarios))}
		for _, sc := range r.Scenarios {
			item := ItemResult{ID: id, Outcome: OutcomeRejected, Reason: ReasonOther}
			if sc.Err != nil {
				item.Reason = classify(sc.Err)
				item.Detail = sc.Err.Error()
			} else {
				found := false
				for _, it := range sc.Items {
					if it.ID == id {
						item = it
						found = true
						break
					}
				}
				if !found {
					item.Detail = "该状态下无此标的结果"
				}
			}
			row.Values = append(row.Values, ScenarioValue{Scenario: sc.Name, Item: item})
		}
		rows = append(rows, row)
	}
	return rows
}
