package pricing

import (
	"fmt"
	"math"
)

// normCDF 标准正态分布函数。
func normCDF(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }

// PriceAnalytic 用 Black-Scholes 解析解定价欧式期权。
// 美式或路径依赖工具不适用，返回 ErrMethodNotApplicable。
func PriceAnalytic(o *Option) (Result, error) {
	if o.IsAmerican {
		return Result{}, fmt.Errorf("%w: analytic Black-Scholes does not support American exercise (option %q)",
			ErrMethodNotApplicable, o.ID)
	}
	if err := o.Validate(); err != nil {
		return Result{}, err
	}
	sqrtT := math.Sqrt(o.Expiry)
	d1 := (math.Log(o.Spot/o.Strike) + (o.Rate+0.5*o.Vol*o.Vol)*o.Expiry) / (o.Vol * sqrtT)
	d2 := d1 - o.Vol*sqrtT
	df := math.Exp(-o.Rate * o.Expiry)
	var price float64
	if o.IsCall {
		price = o.Spot*normCDF(d1) - o.Strike*df*normCDF(d2)
	} else {
		price = o.Strike*df*normCDF(-d2) - o.Spot*normCDF(-d1)
	}
	return Result{OptionID: o.ID, Method: MethodAnalytic, Price: price}, nil
}
