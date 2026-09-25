package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/highcumontoa/concurrent-option-pricing-go/market"
	"github.com/highcumontoa/concurrent-option-pricing-go/pricing"
)

func newTestEngine(t *testing.T, limits Limits) *Engine {
	t.Helper()
	snap, err := market.LoadSnapshot("../testdata/snapshot_normal.json")
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	curve, err := market.BuildDiscountCurve(snap.RateQuotes)
	if err != nil {
		t.Fatalf("build curve: %v", err)
	}
	surf, err := market.Calibrate(snap, curve)
	if err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	return New(snap, curve, surf, limits)
}

// sampleRequests 构造一批覆盖三种方法的请求。
func sampleRequests() []Request {
	var reqs []Request
	strikes := []float64{90, 100, 110}
	expiries := []float64{0.25, 0.5, 1.0}
	id := 0
	for _, e := range expiries {
		for _, k := range strikes {
			id++
			opt := pricing.Option{ID: fmt.Sprintf("C-%d", id), IsCall: true, Strike: k, Expiry: e}
			reqs = append(reqs, Request{Option: opt, Method: pricing.MethodAnalytic})
			reqs = append(reqs, Request{Option: opt, Method: pricing.MethodTree})
			reqs = append(reqs, Request{Option: opt, Method: pricing.MethodMonteCarlo,
				MC: pricing.MCConfig{Seed: uint64(1000 + id), MaxPaths: 20000, BatchSize: 2048}})
		}
	}
	return reqs
}

// TestRunReproducible 相同快照、相同种子、相同并发布局下，
// 多次运行必须得到逐位相同的结果。
func TestRunReproducible(t *testing.T) {
	reqs := sampleRequests()
	limits := Limits{MaxConcurrency: 4, TreeSteps: 200}
	var first []Outcome
	for run := 0; run < 3; run++ {
		eng := newTestEngine(t, limits) // 每次全新引擎，排除缓存影响
		out := eng.Run(context.Background(), reqs)
		if run == 0 {
			first = out
			continue
		}
		for i := range out {
			if out[i].Err != nil || first[i].Err != nil {
				t.Fatalf("run %d req %d unexpected err: %v / %v", run, i, out[i].Err, first[i].Err)
			}
			if out[i].Result != first[i].Result {
				t.Fatalf("run %d req %d differs: %+v vs %+v", run, i, out[i].Result, first[i].Result)
			}
		}
	}
	t.Logf("3 次运行 × %d 个请求结果逐位一致（含蒙特卡洛）", len(reqs))
}

// TestConcurrencyLayouts 不同并发上限下结果必须一致
// （每个请求的随机流只由自身种子决定）。
func TestConcurrencyLayouts(t *testing.T) {
	reqs := sampleRequests()
	base := newTestEngine(t, Limits{MaxConcurrency: 1, TreeSteps: 200}).Run(context.Background(), reqs)
	for _, conc := range []int{2, 8, 16} {
		out := newTestEngine(t, Limits{MaxConcurrency: conc, TreeSteps: 200}).Run(context.Background(), reqs)
		for i := range out {
			if out[i].Result != base[i].Result {
				t.Fatalf("concurrency=%d req %d differs: %+v vs %+v", conc, i, out[i].Result, base[i].Result)
			}
		}
		t.Logf("并发上限 %d 与串行结果逐位一致", conc)
	}
}

// TestBudgetExhausted 超出路径预算的请求必须被明确拒绝，
// 且拒绝集合与调度无关（按请求索引顺序预分配）。
func TestBudgetExhausted(t *testing.T) {
	reqs := sampleRequests()
	var mcIdx []int
	for i, r := range reqs {
		if r.Method == pricing.MethodMonteCarlo {
			mcIdx = append(mcIdx, i)
		}
	}
	// 预算只够前 3 个 MC 请求（每个 20000 路径）。
	limits := Limits{MaxConcurrency: 8, PathBudget: 3 * 20000, TreeSteps: 200}
	out := newTestEngine(t, limits).Run(context.Background(), reqs)
	okCount, rejCount := 0, 0
	for i, o := range out {
		if o.Request.Method != pricing.MethodMonteCarlo {
			if o.Err != nil {
				t.Fatalf("req %d (%v) unexpected err: %v", i, o.Request.Method, o.Err)
			}
			continue
		}
		if errors.Is(o.Err, ErrBudgetExhausted) {
			rejCount++
			t.Logf("req %d rejected: %v", i, o.Err)
		} else if o.Err != nil {
			t.Fatalf("req %d unexpected err: %v", i, o.Err)
		} else {
			okCount++
		}
	}
	t.Logf("budget=60000: MC 成功 %d 个，拒绝 %d 个", okCount, rejCount)
	if okCount != 3 || rejCount != len(mcIdx)-3 {
		t.Fatalf("want 3 ok / %d rejected, got %d / %d", len(mcIdx)-3, okCount, rejCount)
	}
}

// TestCancellation 取消的请求不得干扰其他请求、不得写入缓存。
func TestCancellation(t *testing.T) {
	eng := newTestEngine(t, Limits{MaxConcurrency: 2, TreeSteps: 200})
	reqs := sampleRequests()

	ctx, cancel := context.WithCancel(context.Background())
	// 先跑一个请求填充缓存基线，再取消。
	warm := eng.Run(context.Background(), reqs[:1])
	if warm[0].Err != nil {
		t.Fatalf("warmup: %v", warm[0].Err)
	}
	cancel() // 立即取消
	out := eng.Run(ctx, reqs)
	cancelled := 0
	for i, o := range out {
		if errors.Is(o.Err, context.Canceled) {
			cancelled++
			continue
		}
		if o.Err != nil {
			t.Fatalf("req %d unexpected err: %v", i, o.Err)
		}
		t.Logf("req %d completed despite cancelled ctx (cache hit)", i)
	}
	if cancelled == 0 {
		t.Fatalf("expected some requests to be cancelled")
	}
	t.Logf("取消后：%d 个请求标记为 context.Canceled", cancelled)

	// 取消不得污染缓存：用正常上下文重跑，全部成功且与无取消运行一致。
	clean := newTestEngine(t, Limits{MaxConcurrency: 2, TreeSteps: 200})
	want := clean.Run(context.Background(), reqs)
	got := eng.Run(context.Background(), reqs)
	for i := range got {
		if got[i].Err != nil {
			t.Fatalf("req %d after cancel: %v", i, got[i].Err)
		}
		if got[i].Result != want[i].Result {
			t.Fatalf("req %d result changed after cancellation: %+v vs %+v", i, got[i].Result, want[i].Result)
		}
	}
	t.Logf("取消后重跑 %d 个请求全部成功且结果与无污染基线一致", len(got))
}

// TestTimeout 超时的请求被标记为 DeadlineExceeded，其余语义与取消一致。
func TestTimeout(t *testing.T) {
	eng := newTestEngine(t, Limits{MaxConcurrency: 1, TreeSteps: 200})
	reqs := sampleRequests()
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond) // 确保超时已发生
	out := eng.Run(ctx, reqs)
	expired := 0
	for _, o := range out {
		if errors.Is(o.Err, context.DeadlineExceeded) {
			expired++
		}
	}
	t.Logf("超时上下文下 %d/%d 个请求标记为 DeadlineExceeded", expired, len(reqs))
	if expired == 0 {
		t.Fatalf("expected deadline-exceeded outcomes")
	}
}

// TestErrorKindsDistinct 方法不适用、参数越界、预算耗尽三类错误必须可区分。
func TestErrorKindsDistinct(t *testing.T) {
	eng := newTestEngine(t, Limits{MaxConcurrency: 2, TreeSteps: 200})
	american := pricing.Option{ID: "AMER", IsCall: false, IsAmerican: true, Strike: 100, Expiry: 0.5}
	bad := pricing.Option{ID: "BAD", IsCall: true, Strike: -5, Expiry: 0.5}
	reqs := []Request{
		{Option: american, Method: pricing.MethodAnalytic},
		{Option: bad, Method: pricing.MethodTree},
		{Option: pricing.Option{ID: "OK", IsCall: true, Strike: 100, Expiry: 0.5}, Method: pricing.MethodMonteCarlo,
			MC: pricing.MCConfig{Seed: 9, MaxPaths: 50000}},
	}
	// 预算只够 0 个 MC：第三个请求应被预算拒绝。
	eng2 := newTestEngine(t, Limits{MaxConcurrency: 2, PathBudget: 1, TreeSteps: 200})
	out := eng2.Run(context.Background(), reqs)
	if !errors.Is(out[0].Err, pricing.ErrMethodNotApplicable) {
		t.Fatalf("req0: want ErrMethodNotApplicable, got %v", out[0].Err)
	}
	if !errors.Is(out[1].Err, pricing.ErrParamOutOfRange) {
		t.Fatalf("req1: want ErrParamOutOfRange, got %v", out[1].Err)
	}
	if !errors.Is(out[2].Err, ErrBudgetExhausted) {
		t.Fatalf("req2: want ErrBudgetExhausted, got %v", out[2].Err)
	}
	for i, o := range out {
		t.Logf("req %d err=%v", i, o.Err)
	}
	_ = eng
}
