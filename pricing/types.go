// Package pricing 提供解析解、树模型与蒙特卡洛三类估值路径。
package pricing

import (
	"errors"

	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
)

// 可区分的估值失败原因。
var (
	ErrMethodNotApplicable = errors.New("pricing: 该方法不适用于此工具")
	ErrNotConverged        = errors.New("pricing: 采样不足未收敛")
	ErrParameterOutOfRange = errors.New("pricing: 参数越界")
)

// Style 表示行权方式。
type Style int

const (
	European Style = iota
	American
)

// Method 表示估值方法。
type Method int

const (
	Analytic Method = iota
	Binomial
	MonteCarlo
)

// Spec 描述一个期权工具。
type Spec struct {
	Right  marketdata.Right
	Style  Style
	Strike float64
	Expiry float64
}

// MarketEnv 是估值所需的市场环境。
type MarketEnv struct {
	Spot float64
	Rate float64 // 连续复利零息利率（平利率）
	Vol  float64
}

// MCParams 控制蒙特卡洛估值。
type MCParams struct {
	Paths     int
	Seed      uint64
	Tolerance float64 // 标准误相对价格的可接受上限
}

// Price 按指定方法估值。
func Price(spec Spec, env MarketEnv, method Method, mc MCParams) (float64, error) {
	switch method {
	case Analytic:
		return BlackScholes(spec, env)
	case Binomial:
		return BinomialTree(spec, env, DefaultTreeSteps)
	case MonteCarlo:
		return MonteCarloPrice(spec, env, mc)
	default:
		return 0, ErrMethodNotApplicable
	}
}
