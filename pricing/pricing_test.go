package pricing_test

import (
	"errors"
	"math"
	"testing"

	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
	"github.com/highcumontoa/concurrent-option-pricing-go/pricing"
)

var testEnv = pricing.MarketEnv{Spot: 100, Rate: 0.035, Vol: 0.22}

func TestMethodConsistencyEuropean(t *testing.T) {
	specs := []pricing.Spec{
		{Right: marketdata.Call, Style: pricing.European, Strike: 100, Expiry: 1.0},
		{Right: marketdata.Put, Style: pricing.European, Strike: 90, Expiry: 0.5},
		{Right: marketdata.Call, Style: pricing.European, Strike: 120, Expiry: 2.0},
	}
	for _, spec := range specs {
		an, err := pricing.Price(spec, testEnv, pricing.Analytic, pricing.MCParams{})
		if err != nil {
			t.Fatalf("解析解失败: %v", err)
		}
		bn, err := pricing.Price(spec, testEnv, pricing.Binomial, pricing.MCParams{})
		if err != nil {
			t.Fatalf("二叉树失败: %v", err)
		}
		mc, err := pricing.Price(spec, testEnv, pricing.MonteCarlo,
			pricing.MCParams{Paths: 200000, Seed: 42})
		if err != nil {
			t.Fatalf("蒙特卡洛失败: %v", err)
		}
		t.Logf("spec=%+v 解析=%.6f 树=%.6f MC=%.6f", spec, an, bn, mc)
		// 树与解析解：512 步 CRR 应收敛到 1e-2 量级以内。
		if math.Abs(bn-an) > 0.02 {
			t.Fatalf("树与解析解差异过大: tree=%v analytic=%v", bn, an)
		}
		// MC 与解析解：20 万条对偶路径标准误约 1e-3，容差取 0.5% 相对+0.02 绝对。
		tol := 0.005*math.Abs(an) + 0.02
		if math.Abs(mc-an) > tol {
			t.Fatalf("MC 与解析解差异超出可解释容差: mc=%v analytic=%v tol=%v", mc, an, tol)
		}
	}
}

func TestAmericanPutPremium(t *testing.T) {
	spec := pricing.Spec{Right: marketdata.Put, Style: pricing.American, Strike: 110, Expiry: 1.0}
	am, err := pricing.Price(spec, testEnv, pricing.Binomial, pricing.MCParams{})
	if err != nil {
		t.Fatalf("美式树定价失败: %v", err)
	}
	euSpec := spec
	euSpec.Style = pricing.European
	eu, err := pricing.Price(euSpec, testEnv, pricing.Binomial, pricing.MCParams{})
	if err != nil {
		t.Fatalf("欧式树定价失败: %v", err)
	}
	t.Logf("美式put=%.6f 欧式put=%.6f 提前行权溢价=%.6f", am, eu, am-eu)
	if am < eu-1e-12 {
		t.Fatalf("美式期权不应低于欧式: am=%v eu=%v", am, eu)
	}
}

func TestMethodNotApplicable(t *testing.T) {
	spec := pricing.Spec{Right: marketdata.Put, Style: pricing.American, Strike: 100, Expiry: 1.0}
	_, err := pricing.Price(spec, testEnv, pricing.MonteCarlo, pricing.MCParams{Paths: 10000, Seed: 1})
	t.Logf("美式期权用MC 判定依据: err=%v", err)
	if !errors.Is(err, pricing.ErrMethodNotApplicable) {
		t.Fatalf("期望 ErrMethodNotApplicable, 实际 %v", err)
	}
	_, err = pricing.Price(spec, testEnv, pricing.Analytic, pricing.MCParams{})
	if !errors.Is(err, pricing.ErrMethodNotApplicable) {
		t.Fatalf("解析解用于美式应报 ErrMethodNotApplicable, 实际 %v", err)
	}
}

func TestNotConverged(t *testing.T) {
	spec := pricing.Spec{Right: marketdata.Call, Style: pricing.European, Strike: 100, Expiry: 1.0}
	_, err := pricing.Price(spec, testEnv, pricing.MonteCarlo,
		pricing.MCParams{Paths: 4, Seed: 7, Tolerance: 1e-4})
	t.Logf("仅4条路径 判定依据: err=%v", err)
	if !errors.Is(err, pricing.ErrNotConverged) {
		t.Fatalf("期望 ErrNotConverged, 实际 %v", err)
	}
}

func TestParameterOutOfRange(t *testing.T) {
	spec := pricing.Spec{Right: marketdata.Call, Style: pricing.European, Strike: -1, Expiry: 1.0}
	for _, m := range []pricing.Method{pricing.Analytic, pricing.Binomial, pricing.MonteCarlo} {
		_, err := pricing.Price(spec, testEnv, m, pricing.MCParams{Paths: 1000, Seed: 1})
		t.Logf("非法行权价 method=%v 判定依据: err=%v", m, err)
		if !errors.Is(err, pricing.ErrParameterOutOfRange) {
			t.Fatalf("method=%v 期望 ErrParameterOutOfRange, 实际 %v", m, err)
		}
	}
}

func TestMonteCarloReproducible(t *testing.T) {
	spec := pricing.Spec{Right: marketdata.Call, Style: pricing.European, Strike: 100, Expiry: 1.0}
	mc := pricing.MCParams{Paths: 50000, Seed: 20260926}
	p1, err1 := pricing.Price(spec, testEnv, pricing.MonteCarlo, mc)
	p2, err2 := pricing.Price(spec, testEnv, pricing.MonteCarlo, mc)
	t.Logf("相同种子两次定价: %v, %v", p1, p2)
	if err1 != nil || err2 != nil {
		t.Fatalf("定价失败: %v, %v", err1, err2)
	}
	if p1 != p2 {
		t.Fatalf("相同种子结果不一致: %v vs %v", p1, p2)
	}
	// 不同种子应给出（通常）不同但都接近解析解的结果。
	p3, _ := pricing.Price(spec, testEnv, pricing.MonteCarlo, pricing.MCParams{Paths: 50000, Seed: 1})
	an, _ := pricing.Price(spec, testEnv, pricing.Analytic, pricing.MCParams{})
	t.Logf("种子1: %v 种子20260926: %v 解析: %v", p3, p1, an)
	// 5 万条对偶路径标准误约 0.07，容差取约 2 倍标准误。
	if math.Abs(p1-an) > 0.15 || math.Abs(p3-an) > 0.15 {
		t.Fatalf("MC 结果偏离解析解过多")
	}
}
