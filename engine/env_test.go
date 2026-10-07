package engine_test

import (
	"context"
	"testing"

	"github.com/highcumontoa/concurrent-option-pricing-go/engine"
	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
	"github.com/highcumontoa/concurrent-option-pricing-go/pricing"
)

// TestPerRequestEnv 同一引擎通过 Request.Env 承载不同曲面取值时，
// 结果必须随环境变化且缓存互不串扰；相同环境值（即使是不同指针）命中缓存。
func TestPerRequestEnv(t *testing.T) {
	eng := engine.New(pricing.MarketEnv{Spot: 100, Rate: 0.03, Vol: 0.2},
		engine.Config{MaxWorkers: 1})
	spec := pricing.Spec{Right: marketdata.Call, Style: pricing.European, Strike: 100, Expiry: 1}
	envLow := pricing.MarketEnv{Spot: 100, Rate: 0.03, Vol: 0.15}
	envHigh := pricing.MarketEnv{Spot: 100, Rate: 0.03, Vol: 0.60}

	low := eng.PriceOne(context.Background(), engine.Request{ID: "low", Spec: spec, Method: pricing.Analytic, Env: &envLow})
	high := eng.PriceOne(context.Background(), engine.Request{ID: "high", Spec: spec, Method: pricing.Analytic, Env: &envHigh})
	base := eng.PriceOne(context.Background(), engine.Request{ID: "base", Spec: spec, Method: pricing.Analytic})
	if low.Err != nil || high.Err != nil || base.Err != nil {
		t.Fatalf("估值失败: %v %v %v", low.Err, high.Err, base.Err)
	}
	t.Logf("低波动=%.4f 引擎环境=%.4f 高波动=%.4f", low.Price, base.Price, high.Price)
	if !(low.Price < base.Price && base.Price < high.Price) {
		t.Fatalf("价格应随波动率严格递增: low=%v base=%v high=%v", low.Price, base.Price, high.Price)
	}
	// 两个不同环境 + 引擎环境：缓存应有 3 个不同键，互不串扰。
	if n := eng.CacheLen(); n != 3 {
		t.Fatalf("缓存应隔离为 3 个键, got %d", n)
	}
	// 相同环境值、不同指针：必须命中既有缓存（缓存条数不增加）。
	envLowCopy := envLow
	again := eng.PriceOne(context.Background(), engine.Request{ID: "low2", Spec: spec, Method: pricing.Analytic, Env: &envLowCopy})
	if again.Price != low.Price {
		t.Fatalf("相同环境值应得到相同价格: %v vs %v", again.Price, low.Price)
	}
	if n := eng.CacheLen(); n != 3 {
		t.Fatalf("相同环境值应命中缓存，条数仍为 3, got %d", n)
	}
	t.Log("按环境值隔离/命中缓存验证通过，缓存条目数保持 3")
}
