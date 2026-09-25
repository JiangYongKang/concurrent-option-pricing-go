package pricing

import (
	"fmt"
	"math"
	"math/rand/v2"
)

// defaultMCBatch 默认每批路径数。
const defaultMCBatch = 1024

// PriceMonteCarlo 用蒙特卡洛定价欧式期权（GBM 终点采样，对欧式为精确动态）。
//   - 相同种子下结果可复现：随机流只由 cfg.Seed 决定，与外部并发无关；
//   - 每批结束后检查标准误，达到 cfg.Tolerance 即提前收敛；
//   - 路径预算耗尽仍未收敛时返回 ErrNotConverged（不返回零值冒充成功）；
//   - 美式期权不适用，返回 ErrMethodNotApplicable。
func PriceMonteCarlo(o *Option, cfg MCConfig) (Result, error) {
	if o.IsAmerican {
		return Result{}, fmt.Errorf("%w: monte carlo here does not support American exercise (option %q)",
			ErrMethodNotApplicable, o.ID)
	}
	if err := o.Validate(); err != nil {
		return Result{}, err
	}
	if cfg.MaxPaths <= 0 {
		return Result{}, fmt.Errorf("%w: MaxPaths must be positive, got %d", ErrParamOutOfRange, cfg.MaxPaths)
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultMCBatch
	}
	if cfg.Tolerance < 0 {
		return Result{}, fmt.Errorf("%w: Tolerance must be >= 0, got %v", ErrParamOutOfRange, cfg.Tolerance)
	}

	// 确定性随机流：只依赖种子。
	rng := rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9E3779B97F4A7C15))
	drift := (o.Rate - 0.5*o.Vol*o.Vol) * o.Expiry
	diff := o.Vol * math.Sqrt(o.Expiry)
	df := math.Exp(-o.Rate * o.Expiry)

	var sum, sumSq float64
	done := 0
	for done < cfg.MaxPaths {
		n := cfg.BatchSize
		if rem := cfg.MaxPaths - done; n > rem {
			n = rem
		}
		for i := 0; i < n; i++ {
			z := rng.NormFloat64()
			st := o.Spot * math.Exp(drift+diff*z)
			var payoff float64
			if o.IsCall {
				payoff = math.Max(0, st-o.Strike)
			} else {
				payoff = math.Max(0, o.Strike-st)
			}
			payoff *= df
			sum += payoff
			sumSq += payoff * payoff
		}
		done += n
		if cfg.Tolerance > 0 {
			stdErr := mcStdErr(sum, sumSq, done)
			if stdErr <= cfg.Tolerance {
				return Result{OptionID: o.ID, Method: MethodMonteCarlo,
					Price: sum / float64(done), StdErr: stdErr, Paths: done}, nil
			}
		}
	}
	stdErr := mcStdErr(sum, sumSq, done)
	if cfg.Tolerance > 0 {
		return Result{}, fmt.Errorf("%w: option %q stderr %.6g > tolerance %.6g after %d paths",
			ErrNotConverged, o.ID, stdErr, cfg.Tolerance, done)
	}
	// Tolerance == 0：不强制收敛，跑满预算并报告标准误。
	return Result{OptionID: o.ID, Method: MethodMonteCarlo,
		Price: sum / float64(done), StdErr: stdErr, Paths: done}, nil
}

// mcStdErr 由累计和与平方和估计样本均值的标准误。
func mcStdErr(sum, sumSq float64, n int) float64 {
	if n < 2 {
		return math.Inf(1)
	}
	mean := sum / float64(n)
	variance := sumSq/float64(n) - mean*mean
	if variance < 0 {
		variance = 0 // 浮点误差兜底，不改变统计含义
	}
	return math.Sqrt(variance / float64(n))
}
