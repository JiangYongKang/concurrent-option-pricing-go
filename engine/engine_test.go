package engine_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/highcumontoa/concurrent-option-pricing-go/engine"
	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
	"github.com/highcumontoa/concurrent-option-pricing-go/pricing"
)

var testEnv = pricing.MarketEnv{Spot: 100, Rate: 0.035, Vol: 0.22}

func euroCall(strike float64) pricing.Spec {
	return pricing.Spec{Right: marketdata.Call, Style: pricing.European, Strike: strike, Expiry: 1.0}
}

func mcRequests(n int, seedBase uint64) []engine.Request {
	reqs := make([]engine.Request, n)
	for i := range reqs {
		reqs[i] = engine.Request{
			ID:     fmt.Sprintf("mc-%d", i),
			Spec:   euroCall(90 + float64(i%5)*5),
			Method: pricing.MonteCarlo,
			Seed:   seedBase + uint64(i),
			Paths:  50000,
		}
	}
	return reqs
}

func TestBatchReproducibleAcrossLayouts(t *testing.T) {
	reqs := mcRequests(12, 1000)
	var baseline []engine.Result
	// 相同请求在不同并发布局下结果必须一致。
	for _, workers := range []int{1, 2, 4, 8} {
		eng := engine.New(testEnv, engine.Config{MaxWorkers: workers})
		got := eng.PriceBatch(context.Background(), reqs)
		for i, r := range got {
			if r.Err != nil {
				t.Fatalf("workers=%d 请求 %s 失败: %v", workers, r.ID, r.Err)
			}
			if baseline == nil {
				continue
			}
			if baseline[i].Price != r.Price {
				t.Fatalf("workers=%d 时请求 %s 价格 %v 与基准 %v 不一致",
					workers, r.ID, r.Price, baseline[i].Price)
			}
		}
		if baseline == nil {
			baseline = got
			for _, r := range got {
				t.Logf("基准价格 %s = %.6f", r.ID, r.Price)
			}
		}
	}
	t.Log("1/2/4/8 并发布局下 12 个 MC 请求结果逐位一致")
}

func TestBatchRepeatSameLayout(t *testing.T) {
	reqs := mcRequests(8, 77)
	eng := engine.New(testEnv, engine.Config{MaxWorkers: 4})
	r1 := eng.PriceBatch(context.Background(), reqs)
	// 全新引擎（冷缓存）重跑，结果必须一致。
	eng2 := engine.New(testEnv, engine.Config{MaxWorkers: 4})
	r2 := eng2.PriceBatch(context.Background(), reqs)
	for i := range r1 {
		if r1[i].Price != r2[i].Price || (r1[i].Err == nil) != (r2[i].Err == nil) {
			t.Fatalf("重复运行结果不一致: %s %v vs %v", r1[i].ID, r1[i], r2[i])
		}
	}
	t.Log("相同快照+种子+并发布局下重复批量估值结果一致")
}

func TestBudgetExceeded(t *testing.T) {
	reqs := []engine.Request{
		{ID: "a", Spec: euroCall(100), Method: pricing.MonteCarlo, Seed: 1, Paths: 30000},
		{ID: "b", Spec: euroCall(100), Method: pricing.MonteCarlo, Seed: 2, Paths: 30000},
		{ID: "c", Spec: euroCall(100), Method: pricing.MonteCarlo, Seed: 3, Paths: 30000},
		{ID: "d", Spec: euroCall(100), Method: pricing.Analytic},
	}
	eng := engine.New(testEnv, engine.Config{MaxWorkers: 2, PathBudget: 50000})
	res := eng.PriceBatch(context.Background(), reqs)
	byID := map[string]engine.Result{}
	for _, r := range res {
		byID[r.ID] = r
		t.Logf("请求 %s: price=%v err=%v", r.ID, r.Price, r.Err)
	}
	// 预算 50000：a(30000) 通过，b(30000) 超出剩余 20000 被拒，c 亦被拒，d 非 MC 不受预算约束。
	if byID["a"].Err != nil {
		t.Fatalf("a 应成功: %v", byID["a"].Err)
	}
	if !errors.Is(byID["b"].Err, engine.ErrBudgetExceeded) {
		t.Fatalf("b 应报 ErrBudgetExceeded, 实际 %v", byID["b"].Err)
	}
	if !errors.Is(byID["c"].Err, engine.ErrBudgetExceeded) {
		t.Fatalf("c 应报 ErrBudgetExceeded, 实际 %v", byID["c"].Err)
	}
	if byID["d"].Err != nil {
		t.Fatalf("d 应成功: %v", byID["d"].Err)
	}
}

func TestCancellationIsolation(t *testing.T) {
	// 慢请求：大路径数 MC；快请求：解析解。
	slow := engine.Request{ID: "slow", Spec: euroCall(100), Method: pricing.MonteCarlo,
		Seed: 9, Paths: 2000000}
	fast := engine.Request{ID: "fast", Spec: euroCall(100), Method: pricing.Analytic}
	eng := engine.New(testEnv, engine.Config{MaxWorkers: 2})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan []engine.Result, 1)
	go func() { done <- eng.PriceBatch(ctx, []engine.Request{slow, fast}) }()
	cancel() // 立即取消
	res := <-done
	byID := map[string]engine.Result{}
	for _, r := range res {
		byID[r.ID] = r
		t.Logf("取消场景 请求 %s: price=%v err=%v", r.ID, r.Price, r.Err)
	}
	// 被取消的请求必须报 context.Canceled（fast 可能已在取消前完成，二者皆可接受，
	// 但 slow 绝不允许带着部分结果“成功”）。
	if byID["slow"].Err == nil {
		t.Fatalf("已取消的慢请求不应成功返回")
	}
	if !errors.Is(byID["slow"].Err, context.Canceled) {
		t.Fatalf("slow 应报 context.Canceled, 实际 %v", byID["slow"].Err)
	}

	// 取消不得污染缓存：用新上下文重跑，全部成功且价格正确。
	res2 := eng.PriceBatch(context.Background(), []engine.Request{slow, fast})
	for _, r := range res2 {
		t.Logf("重跑 请求 %s: price=%v err=%v", r.ID, r.Price, r.Err)
		if r.Err != nil {
			t.Fatalf("取消后重跑请求 %s 失败: %v", r.ID, r.Err)
		}
	}
	an, _ := pricing.BlackScholes(euroCall(100), testEnv)
	for _, r := range res2 {
		if r.ID == "fast" && r.Price != an {
			t.Fatalf("fast 价格 %v 与解析解 %v 不一致", r.Price, an)
		}
	}
}

func TestTimeout(t *testing.T) {
	slow := engine.Request{ID: "slow", Spec: euroCall(100), Method: pricing.MonteCarlo,
		Seed: 5, Paths: 5000000}
	eng := engine.New(testEnv, engine.Config{MaxWorkers: 1, Timeout: 50 * time.Millisecond})
	start := time.Now()
	res := eng.PriceBatch(context.Background(), []engine.Request{slow})
	elapsed := time.Since(start)
	t.Logf("超时场景: err=%v 耗时=%v", res[0].Err, elapsed)
	if !errors.Is(res[0].Err, context.DeadlineExceeded) {
		t.Fatalf("应报 context.DeadlineExceeded, 实际 %v", res[0].Err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("超时控制失效，耗时 %v", elapsed)
	}
}

func TestMixedMethodsBatch(t *testing.T) {
	reqs := []engine.Request{
		{ID: "an", Spec: euroCall(100), Method: pricing.Analytic},
		{ID: "tree", Spec: euroCall(100), Method: pricing.Binomial},
		{ID: "mc", Spec: euroCall(100), Method: pricing.MonteCarlo, Seed: 42, Paths: 100000},
	}
	eng := engine.New(testEnv, engine.Config{MaxWorkers: 3})
	res := eng.PriceBatch(context.Background(), reqs)
	prices := map[string]float64{}
	for _, r := range res {
		if r.Err != nil {
			t.Fatalf("请求 %s 失败: %v", r.ID, r.Err)
		}
		prices[r.ID] = r.Price
		t.Logf("方法 %s 价格 %.6f", r.ID, r.Price)
	}
	if d := prices["tree"] - prices["an"]; d < -0.02 || d > 0.02 {
		t.Fatalf("树与解析解不一致: diff=%v", d)
	}
	if d := prices["mc"] - prices["an"]; d < -0.05 || d > 0.05 {
		t.Fatalf("MC 与解析解不一致: diff=%v", d)
	}
}
