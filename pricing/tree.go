package pricing

import (
	"fmt"
	"math"
)

// DefaultTreeSteps 二叉树默认步数。
const DefaultTreeSteps = 400

// PriceTree 用 CRR 二叉树定价（支持欧式与美式）。
// steps <= 0 时使用 DefaultTreeSteps；steps < 2 视为参数越界。
func PriceTree(o *Option, steps int) (Result, error) {
	if err := o.Validate(); err != nil {
		return Result{}, err
	}
	if steps <= 0 {
		steps = DefaultTreeSteps
	}
	if steps < 2 {
		return Result{}, fmt.Errorf("%w: tree steps must be >= 2, got %d", ErrParamOutOfRange, steps)
	}
	dt := o.Expiry / float64(steps)
	u := math.Exp(o.Vol * math.Sqrt(dt))
	d := 1 / u
	disc := math.Exp(-o.Rate * dt)
	p := (math.Exp(o.Rate*dt) - d) / (u - d)
	if p < 0 || p > 1 {
		return Result{}, fmt.Errorf("%w: CRR risk-neutral probability %.6f outside [0,1] (vol=%v, dt=%v)",
			ErrParamOutOfRange, p, o.Vol, dt)
	}
	// 终端节点价值。
	values := make([]float64, steps+1)
	for j := 0; j <= steps; j++ {
		s := o.Spot * math.Pow(u, float64(j)) * math.Pow(d, float64(steps-j))
		if o.IsCall {
			values[j] = math.Max(0, s-o.Strike)
		} else {
			values[j] = math.Max(0, o.Strike-s)
		}
	}
	// 回溯折现；美式在每个节点比较立即行权价值。
	for n := steps - 1; n >= 0; n-- {
		for j := 0; j <= n; j++ {
			cont := disc * (p*values[j+1] + (1-p)*values[j])
			if o.IsAmerican {
				s := o.Spot * math.Pow(u, float64(j)) * math.Pow(d, float64(n-j))
				var exer float64
				if o.IsCall {
					exer = math.Max(0, s-o.Strike)
				} else {
					exer = math.Max(0, o.Strike-s)
				}
				if exer > cont {
					cont = exer
				}
			}
			values[j] = cont
		}
	}
	return Result{OptionID: o.ID, Method: MethodTree, Price: values[0]}, nil
}
