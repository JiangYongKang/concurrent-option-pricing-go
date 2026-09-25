package engine_test

import (
	"context"
	"testing"

	"github.com/highcumontoa/concurrent-option-pricing-go/calibration"
	"github.com/highcumontoa/concurrent-option-pricing-go/engine"
	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
	"github.com/highcumontoa/concurrent-option-pricing-go/pricing"
)

// TestEndToEnd 从本地快照出发：校验 -> 校准 -> 并发批量估值，
// 并验证解析/树/MC 三路径在曲面波动率下结果一致。
func TestEndToEnd(t *testing.T) {
	snap, err := marketdata.LoadSnapshot("../testdata/snapshots/normal.json")
	if err != nil {
		t.Fatalf("加载快照失败: %v", err)
	}
	curve, err := calibration.BuildDiscountCurve(snap)
	if err != nil {
		t.Fatalf("构建曲线失败: %v", err)
	}
	vs, err := calibration.BuildVolSurface(snap, curve)
	if err != nil {
		t.Fatalf("构建曲面失败: %v", err)
	}

	spec := pricing.Spec{Right: marketdata.Call, Style: pricing.European, Strike: 105, Expiry: 0.75}
	env := pricing.MarketEnv{
		Spot: snap.Spot,
		Rate: curve.ZeroRate(spec.Expiry),
		Vol:  vs.Vol(spec.Expiry, spec.Strike),
	}
	t.Logf("市场环境: spot=%v rate=%.4f vol=%.4f (T=%v, K=%v)",
		env.Spot, env.Rate, env.Vol, spec.Expiry, spec.Strike)

	eng := engine.New(env, engine.Config{MaxWorkers: 3, PathBudget: 1_000_000})
	reqs := []engine.Request{
		{ID: "analytic", Spec: spec, Method: pricing.Analytic},
		{ID: "tree", Spec: spec, Method: pricing.Binomial},
		{ID: "mc", Spec: spec, Method: pricing.MonteCarlo, Seed: 20260926, Paths: 200000},
	}
	res := eng.PriceBatch(context.Background(), reqs)
	prices := map[string]float64{}
	for _, r := range res {
		if r.Err != nil {
			t.Fatalf("请求 %s 失败: %v", r.ID, r.Err)
		}
		prices[r.ID] = r.Price
		t.Logf("%s = %.6f", r.ID, r.Price)
	}
	if d := prices["tree"] - prices["analytic"]; d < -0.02 || d > 0.02 {
		t.Fatalf("树与解析解差异过大: %v", d)
	}
	if d := prices["mc"] - prices["analytic"]; d < -0.05 || d > 0.05 {
		t.Fatalf("MC 与解析解差异过大: %v", d)
	}
	t.Log("端到端：快照校准后三种方法估值在容差内一致")
}
