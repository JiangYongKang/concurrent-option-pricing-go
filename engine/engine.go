// Package engine 提供带并发上限、路径预算与超时控制的批量估值。
//
// 语义约定：
//   - 可复现：每个请求的随机性只来自其自身 Seed，与并发调度无关；
//     相同环境、相同请求列表、相同 Config 必得相同结果。
//   - 预算：批次内蒙特卡洛路径总数超出 PathBudget 时，按请求顺序
//     分配预算，超出部分的请求以 ErrBudgetExceeded 明确拒绝（不降级、
//     不截断路径数，保证可复现）。
//   - 取消/超时：被取消的请求不写入缓存、不影响其他请求；
//     其错误为 context.Canceled 或 context.DeadlineExceeded。
//   - 缓存：仅缓存成功结果；键为 (Spec, Method, Seed, Paths)。
package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/highcumontoa/concurrent-option-pricing-go/pricing"
)

// 引擎级失败原因。
var (
	ErrBudgetExceeded = errors.New("engine: 超出路径预算")
)

// Request 是一次估值请求。
type Request struct {
	ID     string
	Spec   pricing.Spec
	Method pricing.Method
	Seed   uint64 // 蒙特卡洛种子；其他方法忽略
	Paths  int    // 蒙特卡洛路径数；<=0 时使用 Config.PerReqPaths
}

// Result 是一次估值结果。
type Result struct {
	ID    string
	Price float64
	Err   error
}

// Config 控制引擎并发与资源语义。
type Config struct {
	MaxWorkers  int           // 并发上限；<=0 时取 4
	PathBudget  int64         // 单批次蒙特卡洛路径总预算；<=0 表示不限
	PerReqPaths int           // 每个 MC 请求的默认路径数；<=0 时取 100000
	Timeout     time.Duration // 单批次超时；<=0 表示不限
}

func (c *Config) withDefaults() Config {
	out := *c
	if out.MaxWorkers <= 0 {
		out.MaxWorkers = 4
	}
	if out.PerReqPaths <= 0 {
		out.PerReqPaths = 100000
	}
	return out
}

// cacheKey 是结果缓存键。
type cacheKey struct {
	spec   pricing.Spec
	method pricing.Method
	seed   uint64
	paths  int
}

// Engine 在固定市场环境下执行批量估值。
type Engine struct {
	env pricing.MarketEnv
	cfg Config

	mu    sync.Mutex
	cache map[cacheKey]float64
}

// New 创建引擎。
func New(env pricing.MarketEnv, cfg Config) *Engine {
	return &Engine{env: env, cfg: cfg.withDefaults(), cache: map[cacheKey]float64{}}
}

// PriceBatch 批量估值；结果顺序与请求顺序一致。
//
// 取消/超时的请求不会写入缓存，也不会干扰其他请求。
func (e *Engine) PriceBatch(ctx context.Context, reqs []Request) []Result {
	if e.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.cfg.Timeout)
		defer cancel()
	}
	results := make([]Result, len(reqs))
	for i, r := range reqs {
		results[i].ID = r.ID
	}

	// 预算预分配：按请求顺序确定性地分配，保证与并发调度无关。
	unlimited := e.cfg.PathBudget <= 0
	budgetLeft := e.cfg.PathBudget
	runnable := make([]bool, len(reqs))
	for i, r := range reqs {
		if r.Method != pricing.MonteCarlo {
			runnable[i] = true
			continue
		}
		paths := int64(e.pathsFor(r))
		if !unlimited && paths > budgetLeft {
			results[i].Err = fmt.Errorf("%w: 请求 %s 需要 %d 条路径，剩余预算 %d",
				ErrBudgetExceeded, r.ID, paths, budgetLeft)
			continue
		}
		if !unlimited {
			budgetLeft -= paths
		}
		runnable[i] = true
	}

	type job struct{ idx int }
	jobs := make(chan job)
	var wg sync.WaitGroup
	workers := e.cfg.MaxWorkers
	if workers > len(reqs) {
		workers = len(reqs)
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				e.runOne(ctx, reqs[j.idx], &results[j.idx])
			}
		}()
	}
	for i := range reqs {
		if runnable[i] {
			jobs <- job{i}
		}
	}
	close(jobs)
	wg.Wait()
	return results
}

func (e *Engine) pathsFor(r Request) int {
	if r.Paths > 0 {
		return r.Paths
	}
	return e.cfg.PerReqPaths
}

// runOne 执行单个请求。取消时不写缓存。
func (e *Engine) runOne(ctx context.Context, r Request, out *Result) {
	if err := ctx.Err(); err != nil {
		out.Err = err
		return
	}
	key := cacheKey{spec: r.Spec, method: r.Method, seed: r.Seed, paths: e.pathsFor(r)}
	e.mu.Lock()
	cached, hit := e.cache[key]
	e.mu.Unlock()
	if hit {
		out.Price = cached
		return
	}
	price, err := pricing.Price(r.Spec, e.env, r.Method, pricing.MCParams{
		Paths: e.pathsFor(r),
		Seed:  r.Seed,
	})
	if err != nil {
		out.Err = err
		return
	}
	// 定价完成后再次检查取消：已取消的批次不回写缓存，
	// 避免把“无人认领”的部分状态留给后续批次。
	if err := ctx.Err(); err != nil {
		out.Err = err
		return
	}
	e.mu.Lock()
	e.cache[key] = price
	e.mu.Unlock()
	out.Price = price
}
