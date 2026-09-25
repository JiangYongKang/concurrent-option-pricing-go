package pricing

import (
	"errors"
	"testing"
)

func baseOption(id string, isCall bool) *Option {
	return &Option{
		ID: id, IsCall: isCall,
		Spot: 100, Strike: 100, Expiry: 1, Vol: 0.2, Rate: 0.02,
	}
}

// TestMethodsConsistent 同一欧式期权在三类方法下应在可解释容差内一致。
// 容差来源：树模型 O(1/n) 离散误差、蒙特卡洛标准误。
func TestMethodsConsistent(t *testing.T) {
	for _, isCall := range []bool{true, false} {
		o := baseOption("euro", isCall)
		an, err := PriceAnalytic(o)
		if err != nil {
			t.Fatalf("analytic: %v", err)
		}
		tr, err := PriceTree(o, 800)
		if err != nil {
			t.Fatalf("tree: %v", err)
		}
		mc, err := PriceMonteCarlo(o, MCConfig{Seed: 42, MaxPaths: 400000, BatchSize: 4000, Tolerance: 0})
		if err != nil {
			t.Fatalf("montecarlo: %v", err)
		}
		t.Logf("isCall=%v analytic=%.6f tree=%.6f mc=%.6f(mcStdErr=%.6f, paths=%d)",
			isCall, an.Price, tr.Price, mc.Price, mc.StdErr, mc.Paths)

		if d := tr.Price - an.Price; d > 0.01 || d < -0.01 {
			t.Errorf("tree-analytic gap %.6f exceeds 1e-2 (n=800 树离散容差)", d)
		}
		// MC 用 3 倍标准误作统计容差，再加 1e-9 防退化。
		tol := 3*mc.StdErr + 1e-9
		if d := mc.Price - an.Price; d > tol || d < -tol {
			t.Errorf("mc-analytic gap %.6f exceeds 3*stderr=%.6f", d, tol)
		}
	}
}

// TestMCDeterministic 相同种子必须得到完全相同的价格。
func TestMCDeterministic(t *testing.T) {
	o := baseOption("euro-call", true)
	cfg := MCConfig{Seed: 7, MaxPaths: 50000, BatchSize: 2048, Tolerance: 0}
	r1, err := PriceMonteCarlo(o, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := PriceMonteCarlo(o, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("seed=7 price1=%.10f price2=%.10f paths=%d", r1.Price, r2.Price, r1.Paths)
	if r1.Price != r2.Price || r1.Paths != r2.Paths {
		t.Fatalf("same seed gave different results: %.12f vs %.12f", r1.Price, r2.Price)
	}
	an, _ := PriceAnalytic(o)
	tol := 3*r1.StdErr + 1e-9
	t.Logf("analytic=%.6f, |mc-analytic|=%.6f, 3*stderr=%.6f", an.Price, r1.Price-an.Price, tol)
	if d := r1.Price - an.Price; d > tol || d < -tol {
		t.Fatalf("MC price %.6f too far from analytic %.6f (tol %.6f)", r1.Price, an.Price, tol)
	}
}

// TestMCNotConverged 预算不足时必须报 ErrNotConverged，而非返回零值。
func TestMCNotConverged(t *testing.T) {
	o := baseOption("euro-call", true)
	cfg := MCConfig{Seed: 1, MaxPaths: 64, BatchSize: 64, Tolerance: 1e-9}
	res, err := PriceMonteCarlo(o, cfg)
	t.Logf("budget=64 tolerance=1e-9 res=%+v err=%v", res, err)
	if !errors.Is(err, ErrNotConverged) {
		t.Fatalf("want ErrNotConverged, got %v", err)
	}
}

// TestMCConverges 预算充足时应提前收敛并报告标准误。
func TestMCConverges(t *testing.T) {
	o := baseOption("euro-call", true)
	cfg := MCConfig{Seed: 3, MaxPaths: 2000000, BatchSize: 4096, Tolerance: 0.02}
	res, err := PriceMonteCarlo(o, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("converged early: paths=%d stderr=%.6f price=%.6f", res.Paths, res.StdErr, res.Price)
	if res.Paths >= cfg.MaxPaths {
		t.Fatalf("expected early convergence, ran full budget %d", cfg.MaxPaths)
	}
	if res.StdErr > cfg.Tolerance {
		t.Fatalf("stderr %.6f > tolerance %.6f", res.StdErr, cfg.Tolerance)
	}
}

// TestMethodNotApplicable 美式期权对解析解与当前蒙特卡洛均不适用。
func TestMethodNotApplicable(t *testing.T) {
	o := baseOption("amer-put", false)
	o.IsAmerican = true
	if _, err := PriceAnalytic(o); !errors.Is(err, ErrMethodNotApplicable) {
		t.Fatalf("analytic american: want ErrMethodNotApplicable, got %v", err)
	}
	if _, err := PriceMonteCarlo(o, MCConfig{Seed: 1, MaxPaths: 100}); !errors.Is(err, ErrMethodNotApplicable) {
		t.Fatalf("mc american: want ErrMethodNotApplicable, got %v", err)
	}
	// 树方法支持美式，且美式看跌价格不应低于欧式。
	amer, err := PriceTree(o, 400)
	if err != nil {
		t.Fatalf("tree american: %v", err)
	}
	o.IsAmerican = false
	euro, err := PriceTree(o, 400)
	if err != nil {
		t.Fatalf("tree european: %v", err)
	}
	t.Logf("american put tree=%.6f, european put tree=%.6f", amer.Price, euro.Price)
	if amer.Price < euro.Price-1e-9 {
		t.Fatalf("american put %.6f below european %.6f", amer.Price, euro.Price)
	}
}

// TestParamOutOfRange 参数越界必须被明确拒绝。
func TestParamOutOfRange(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Option)
	}{
		{"spot<=0", func(o *Option) { o.Spot = 0 }},
		{"strike<=0", func(o *Option) { o.Strike = -1 }},
		{"expiry<=0", func(o *Option) { o.Expiry = 0 }},
		{"vol<=0", func(o *Option) { o.Vol = 0 }},
		{"vol 过大", func(o *Option) { o.Vol = 9 }},
		{"rate NaN", func(o *Option) { o.Rate = nan() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := baseOption("bad", true)
			tc.mut(o)
			if _, err := PriceAnalytic(o); !errors.Is(err, ErrParamOutOfRange) {
				t.Fatalf("want ErrParamOutOfRange, got %v", err)
			}
			if _, err := PriceTree(o, 100); !errors.Is(err, ErrParamOutOfRange) {
				t.Fatalf("tree: want ErrParamOutOfRange, got %v", err)
			}
			if _, err := PriceMonteCarlo(o, MCConfig{Seed: 1, MaxPaths: 10}); !errors.Is(err, ErrParamOutOfRange) {
				t.Fatalf("mc: want ErrParamOutOfRange, got %v", err)
			}
			t.Logf("case=%q 三类方法均拒绝: %v", tc.name, "ErrParamOutOfRange")
		})
	}
	// MC 特有：MaxPaths 非正。
	o := baseOption("bad-mc", true)
	if _, err := PriceMonteCarlo(o, MCConfig{Seed: 1, MaxPaths: 0}); !errors.Is(err, ErrParamOutOfRange) {
		t.Fatalf("MaxPaths=0: want ErrParamOutOfRange, got %v", err)
	}
}

func nan() float64 {
	// 避免在循环构造里引入 math 包名，集中在一处。
	var x float64
	x = 0
	return x / x
}
