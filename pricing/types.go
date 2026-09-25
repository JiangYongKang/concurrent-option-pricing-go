// Package pricing 提供解析解、二叉树与蒙特卡洛三类估值路径。
package pricing

import (
	"errors"
	"fmt"
	"math"
)

// 可区分的估值失败原因。
var (
	// ErrMethodNotApplicable 所选方法不适用于该工具
	// （如解析解不支持美式/路径依赖工具）。
	ErrMethodNotApplicable = errors.New("pricing: method not applicable")
	// ErrNotConverged 蒙特卡洛在给定路径预算内未收敛到目标精度。
	ErrNotConverged = errors.New("pricing: monte carlo not converged within path budget")
	// ErrParamOutOfRange 参数越界（非正现货/行权价/波动率、负期限等）。
	ErrParamOutOfRange = errors.New("pricing: parameter out of range")
)

// Method 估值方法。
type Method int

const (
	MethodAnalytic   Method = iota // Black-Scholes 解析解
	MethodTree                     // CRR 二叉树
	MethodMonteCarlo               // 蒙特卡洛模拟
)

func (m Method) String() string {
	switch m {
	case MethodAnalytic:
		return "analytic"
	case MethodTree:
		return "tree"
	case MethodMonteCarlo:
		return "montecarlo"
	}
	return "unknown"
}

// Option 一个待估值的期权工具。
type Option struct {
	ID         string  // 工具标识
	IsCall     bool    // true=看涨 false=看跌
	Spot       float64 // 标的出现价
	Strike     float64 // 行权价
	Expiry     float64 // 到期（年）
	Vol        float64 // 波动率
	Rate       float64 // 连续复利无风险利率
	IsAmerican bool    // true=美式（仅树方法适用）
}

// Validate 校验参数范围，越界返回 ErrParamOutOfRange。
func (o *Option) Validate() error {
	if !(o.Spot > 0) || math.IsInf(o.Spot, 0) {
		return fmt.Errorf("%w: spot must be positive finite, got %v", ErrParamOutOfRange, o.Spot)
	}
	if !(o.Strike > 0) || math.IsInf(o.Strike, 0) {
		return fmt.Errorf("%w: strike must be positive finite, got %v", ErrParamOutOfRange, o.Strike)
	}
	if !(o.Expiry > 0) || math.IsInf(o.Expiry, 0) {
		return fmt.Errorf("%w: expiry must be positive finite, got %v", ErrParamOutOfRange, o.Expiry)
	}
	if !(o.Vol > 0) || o.Vol > 5 || math.IsInf(o.Vol, 0) {
		return fmt.Errorf("%w: vol must be in (0, 5], got %v", ErrParamOutOfRange, o.Vol)
	}
	if math.IsNaN(o.Rate) || math.IsInf(o.Rate, 0) || o.Rate < -1 || o.Rate > 1 {
		return fmt.Errorf("%w: rate must be finite in [-1, 1], got %v", ErrParamOutOfRange, o.Rate)
	}
	return nil
}

// MCConfig 蒙特卡洛配置。
type MCConfig struct {
	Seed      uint64  // 随机种子（相同种子结果可复现）
	MaxPaths  int     // 路径预算上限
	BatchSize int     // 每批路径数（用于收敛检查）
	Tolerance float64 // 收敛容差（标准误）
}

// Result 一次估值的结果。
type Result struct {
	OptionID string
	Method   Method
	Price    float64
	StdErr   float64 // 仅蒙特卡洛有意义
	Paths    int     // 仅蒙特卡洛有意义
}
