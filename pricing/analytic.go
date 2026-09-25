package pricing

import (
	"fmt"
	"math"

	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
)

// normCDF 标准正态累积分布函数（基于 math.Erf，完全确定性）。
func normCDF(x float64) float64 {
	return 0.5 * (1 + math.Erf(x/math.Sqrt2))
}

func validateSpecEnv(spec Spec, env MarketEnv) error {
	if spec.Strike <= 0 || spec.Expiry <= 0 ||
		math.IsNaN(spec.Strike) || math.IsNaN(spec.Expiry) {
		return fmt.Errorf("%w: 行权价与期限必须为正 (K=%v, T=%v)",
			ErrParameterOutOfRange, spec.Strike, spec.Expiry)
	}
	if env.Spot <= 0 || env.Vol <= 0 ||
		math.IsNaN(env.Spot) || math.IsNaN(env.Vol) || math.IsNaN(env.Rate) {
		return fmt.Errorf("%w: 现价与波动率必须为正、利率必须有限 (S=%v, sigma=%v, r=%v)",
			ErrParameterOutOfRange, env.Spot, env.Vol, env.Rate)
	}
	return nil
}

// BlackScholes 计算欧式期权解析解（Black-Scholes-Merton，无股息）。
func BlackScholes(spec Spec, env MarketEnv) (float64, error) {
	if spec.Style != European {
		return 0, fmt.Errorf("%w: 解析解仅支持欧式期权", ErrMethodNotApplicable)
	}
	if err := validateSpecEnv(spec, env); err != nil {
		return 0, err
	}
	sqrtT := math.Sqrt(spec.Expiry)
	d1 := (math.Log(env.Spot/spec.Strike) + (env.Rate+0.5*env.Vol*env.Vol)*spec.Expiry) / (env.Vol * sqrtT)
	d2 := d1 - env.Vol*sqrtT
	disc := math.Exp(-env.Rate * spec.Expiry)
	switch spec.Right {
	case marketdata.Call:
		return env.Spot*normCDF(d1) - spec.Strike*disc*normCDF(d2), nil
	default:
		return spec.Strike*disc*normCDF(-d2) - env.Spot*normCDF(-d1), nil
	}
}
