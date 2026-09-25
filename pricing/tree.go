package pricing

import (
	"fmt"
	"math"

	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
)

// DefaultTreeSteps 是二叉树的默认步数。
const DefaultTreeSteps = 512

// BinomialTree 用 CRR 二叉树估值（支持欧式与美式）。
//
// 步数固定时结果完全确定；步数越大越接近连续时间极限，
// 与解析解/蒙特卡洛的差异应在可解释容差内。
func BinomialTree(spec Spec, env MarketEnv, steps int) (float64, error) {
	if err := validateSpecEnv(spec, env); err != nil {
		return 0, err
	}
	if steps <= 0 {
		steps = DefaultTreeSteps
	}
	if steps > 1<<20 {
		return 0, fmt.Errorf("%w: 树步数过大 %d", ErrParameterOutOfRange, steps)
	}
	dt := spec.Expiry / float64(steps)
	u := math.Exp(env.Vol * math.Sqrt(dt))
	d := 1 / u
	disc := math.Exp(-env.Rate * dt)
	p := (math.Exp(env.Rate*dt) - d) / (u - d)
	if p < 0 || p > 1 {
		return 0, fmt.Errorf("%w: 风险中性概率越界 p=%v（波动率或步长非法）",
			ErrParameterOutOfRange, p)
	}
	// 终端 payoff。
	payoff := func(s float64) float64 {
		if spec.Right == marketdata.Call {
			return math.Max(0, s-spec.Strike)
		}
		return math.Max(0, spec.Strike-s)
	}
	n := steps + 1
	vals := make([]float64, n)
	s := env.Spot * math.Pow(d, float64(steps))
	for j := 0; j < n; j++ {
		vals[j] = payoff(s)
		s *= u * u // d^{-1}*u = u^2 的递推等价形式：S_j = S_0 * u^j * d^{steps-j}
	}
	// 回溯折现；美式在每一节点比较继续持有与立即行权。
	for i := steps - 1; i >= 0; i-- {
		s = env.Spot * math.Pow(d, float64(i))
		for j := 0; j <= i; j++ {
			hold := disc * (p*vals[j+1] + (1-p)*vals[j])
			if spec.Style == American {
				if ex := payoff(s); ex > hold {
					hold = ex
				}
			}
			vals[j] = hold
			s *= u * u
		}
	}
	return vals[0], nil
}
