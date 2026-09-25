package market

import (
	"fmt"
	"math"
	"sort"
)

// DiscountCurve 由贴现因子报价构建的贴现曲线。
// 对 log(DF) 做分段线性插值，等价于相邻期限间远期利率恒定；
// 端点以外采用最近期限的水平外推（不静默发明利率数据）。
type DiscountCurve struct {
	tenors []float64
	logDF  []float64
}

// BuildDiscountCurve 由利率报价构建贴现曲线。
// 期限重复返回 ErrQuoteDuplicate；
// 随期限增长贴现因子上升（期限倒挂）返回 ErrTenorInversion。
func BuildDiscountCurve(quotes []RateQuote) (*DiscountCurve, error) {
	if len(quotes) == 0 {
		return nil, fmt.Errorf("%w: no rate quotes", ErrQuoteMissing)
	}
	sorted := make([]RateQuote, len(quotes))
	copy(sorted, quotes)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Tenor < sorted[j].Tenor })

	tenors := make([]float64, len(sorted))
	logDF := make([]float64, len(sorted))
	prevDF := 1.0 // t=0 时 DF=1
	prevT := 0.0  // 上一个期限
	for i, q := range sorted {
		if i > 0 && q.Tenor == tenors[i-1] {
			return nil, fmt.Errorf("%w: rate quote tenor %v", ErrQuoteDuplicate, q.Tenor)
		}
		if q.DiscountFactor > prevDF+1e-12 {
			return nil, fmt.Errorf("%w: discount factor rose from %.10f at tenor %v to %.10f at tenor %v",
				ErrTenorInversion, prevDF, prevT, q.DiscountFactor, q.Tenor)
		}
		tenors[i] = q.Tenor
		logDF[i] = math.Log(q.DiscountFactor)
		prevDF = q.DiscountFactor
		prevT = q.Tenor
	}
	return &DiscountCurve{tenors: tenors, logDF: logDF}, nil
}

// logDiscountAt 返回 t 处的 log 贴现因子（log 空间分段线性）。
func (c *DiscountCurve) logDiscountAt(t float64) float64 {
	if t <= c.tenors[0] {
		// 0 到最短期限：按 (0,1) 与首点连线，保持 t=0 时 DF=1。
		if t <= 0 {
			return 0
		}
		return c.logDF[0] * (t / c.tenors[0])
	}
	last := len(c.tenors) - 1
	if t >= c.tenors[last] {
		return c.logDF[last]
	}
	i := sort.Search(last, func(i int) bool { return c.tenors[i] >= t })
	// i 满足 tenors[i-1] < t <= tenors[i]
	t0, t1 := c.tenors[i-1], c.tenors[i]
	w := (t - t0) / (t1 - t0)
	return c.logDF[i-1]*(1-w) + c.logDF[i]*w
}

// Discount 返回期限 t（年）的贴现因子。
func (c *DiscountCurve) Discount(t float64) float64 {
	return math.Exp(c.logDiscountAt(t))
}

// ZeroRate 返回期限 t 的连续复利零息利率 r(t) = -log(DF(t))/t。
func (c *DiscountCurve) ZeroRate(t float64) float64 {
	if t <= 0 {
		return 0
	}
	return -c.logDiscountAt(t) / t
}
