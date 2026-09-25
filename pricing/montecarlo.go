package pricing

import (
	"fmt"
	"math"
	"math/rand/v2"

	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
)

// MonteCarloPrice 用 GBM 蒙特卡洛估值（仅欧式）。
//
// 可复现性：随机源为以 MCParams.Seed 为种子的 PCG，
// 与并发度、调用时序无关；相同种子必得相同结果。
// 收敛判定：使用对偶变量（antithetic）缩减方差，跑满 Paths 条路径后，
// 若标准误相对价格仍超过 Tolerance，则返回 ErrNotConverged，
// 绝不返回未收敛的数值冒充成功。
func MonteCarloPrice(spec Spec, env MarketEnv, mc MCParams) (float64, error) {
	if spec.Style != European {
		return 0, fmt.Errorf("%w: 蒙特卡洛（终端抽样）仅支持欧式期权", ErrMethodNotApplicable)
	}
	if err := validateSpecEnv(spec, env); err != nil {
		return 0, err
	}
	if mc.Paths <= 0 {
		return 0, fmt.Errorf("%w: 路径数必须为正, got %d", ErrParameterOutOfRange, mc.Paths)
	}
	if mc.Paths%2 == 1 {
		mc.Paths++ // 对偶变量需要偶数条路径
	}
	if mc.Tolerance <= 0 {
		mc.Tolerance = 1e-2 // 默认：标准误不超过价格的 1%
	}
	rng := rand.New(rand.NewPCG(mc.Seed, mc.Seed^0x9E3779B97F4A7C15))
	drift := (env.Rate - 0.5*env.Vol*env.Vol) * spec.Expiry
	diff := env.Vol * math.Sqrt(spec.Expiry)
	disc := math.Exp(-env.Rate * spec.Expiry)
	payoff := func(s float64) float64 {
		if spec.Right == marketdata.Call {
			return math.Max(0, s-spec.Strike)
		}
		return math.Max(0, spec.Strike-s)
	}
	half := mc.Paths / 2
	sum, sumSq := 0.0, 0.0
	for i := 0; i < half; i++ {
		z := rng.NormFloat64()
		// 对偶路径成对计入，保证样本数恰为 Paths。
		for _, zz := range [2]float64{z, -z} {
			pv := disc * payoff(env.Spot*math.Exp(drift+diff*zz))
			sum += pv
			sumSq += pv * pv
		}
	}
	n := float64(mc.Paths)
	mean := sum / n
	variance := sumSq/n - mean*mean
	if variance < 0 {
		variance = 0
	}
	stderr := math.Sqrt(variance / n)
	// 深虚值时价格可能接近 0，此时用绝对标准误判定。
	scale := math.Max(math.Abs(mean), 1e-8)
	if stderr/scale > mc.Tolerance {
		return 0, fmt.Errorf("%w: %d 条路径后标准误 %v 仍超过容差 %v（价格 %v）",
			ErrNotConverged, mc.Paths, stderr, mc.Tolerance, mean)
	}
	return mean, nil
}
