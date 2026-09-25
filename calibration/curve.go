// Package calibration 由行情快照构建贴现曲线与隐含波动率曲面。
//
// 相同输入必须得到相同结果：所有插值与求解均为确定性算法，
// 不依赖 map 迭代顺序、随机数或时钟。
package calibration

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
)

// 曲线构建失败原因。
var (
	ErrCurveNotBuildable = errors.New("calibration: 贴现曲线不可构建")
)

// DiscountCurve 是基于零息利率的线性插值贴现曲线。
//
// 插值规则：零息利率在相邻期限节点间线性插值，
// 端点之外按端点利率平坦外推；贴现因子为 exp(-r(t)*t)。
type DiscountCurve struct {
	tenors []float64 // 严格递增
	rates  []float64
}

// BuildDiscountCurve 由快照中的利率报价构建贴现曲线。
// 快照须先通过 Validate；此处仍做防御性检查以保证可区分失败。
func BuildDiscountCurve(snap *marketdata.Snapshot) (*DiscountCurve, error) {
	if snap == nil || len(snap.Rates) == 0 {
		return nil, fmt.Errorf("%w: 利率报价为空", ErrCurveNotBuildable)
	}
	qs := append([]marketdata.RateQuote(nil), snap.Rates...)
	sort.Slice(qs, func(i, j int) bool { return qs[i].Tenor < qs[j].Tenor })
	tenors := make([]float64, len(qs))
	rates := make([]float64, len(qs))
	for i, q := range qs {
		if math.IsNaN(q.Tenor) || math.IsInf(q.Tenor, 0) || q.Tenor <= 0 ||
			math.IsNaN(q.Rate) || math.IsInf(q.Rate, 0) {
			return nil, fmt.Errorf("%w: 非法利率报价 (tenor=%v, rate=%v)",
				ErrCurveNotBuildable, q.Tenor, q.Rate)
		}
		if i > 0 && q.Tenor == tenors[i-1] {
			return nil, fmt.Errorf("%w: 利率期限 %v 重复", ErrCurveNotBuildable, q.Tenor)
		}
		tenors[i] = q.Tenor
		rates[i] = q.Rate
	}
	return &DiscountCurve{tenors: tenors, rates: rates}, nil
}

// ZeroRate 返回期限 t（年）的连续复利零息利率。
func (c *DiscountCurve) ZeroRate(t float64) float64 {
	n := len(c.tenors)
	if t <= c.tenors[0] {
		return c.rates[0]
	}
	if t >= c.tenors[n-1] {
		return c.rates[n-1]
	}
	// 二分定位插值区间，保证确定性。
	i := sort.SearchFloat64s(c.tenors, t)
	t0, t1 := c.tenors[i-1], c.tenors[i]
	r0, r1 := c.rates[i-1], c.rates[i]
	w := (t - t0) / (t1 - t0)
	return r0 + w*(r1-r0)
}

// Discount 返回期限 t（年）的贴现因子。
func (c *DiscountCurve) Discount(t float64) float64 {
	if t <= 0 {
		return 1
	}
	return math.Exp(-c.ZeroRate(t) * t)
}
