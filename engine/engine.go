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
	// Env 非 nil 时按请求覆盖引擎绑定的市场环境（用于同一引擎承载
	// 同一校准状态下不同标的各自的曲面取值）；nil 时使用引擎环境。
	Env *pricing.MarketEnv
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
	CacheLimit  int           // 成功结果缓存条目上限；<=0 表示不限
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

// cacheKey 是结果缓存键。请求覆盖环境时，按环境“值”参与键，
// 使同一工具在不同曲面取值下互不串扰，同时相同环境值即使来自
// 不同指针也能命中缓存。
type cacheKey struct {
	spec   pricing.Spec
	method pricing.Method
	seed   uint64
	paths  int
	env    pricing.MarketEnv
	hasEnv bool
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
	budgetLeft := e.cfg.PathBudget
	runnable := make([]bool, len(reqs))
	for i, r := range reqs {
		ok, err := e.PlanOne(r, &budgetLeft)
		if err != nil {
			results[i].Err = err
			continue
		}
		runnable[i] = ok
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

// PlanOne 按“引擎批次预算”的视角为一个请求预分配路径预算。
//
// 预算上限取自 e.cfg.PathBudget：<=0 表示该引擎批次不限预算。
// 需要在多个引擎之间共享一份外部预算的调用方（例如多状态运行器）
// 应使用纯函数式的 PlanRequest。
func (e *Engine) PlanOne(r Request, budgetLeft *int64) (bool, error) {
	return PlanRequest(r, e.cfg.PathBudget, e.cfg.PerReqPaths, budgetLeft)
}

// PlanRequest 是确定性的纯函数式路径预算规划：
//
//   - 非 MC 请求不占预算，恒返回 (true, nil)；
//   - budget <= 0 表示不限预算；
//   - 否则当请求所需路径数超过 *budgetLeft 时返回包装了
//     ErrBudgetExceeded 的错误且不扣减预算；否则扣减并返回 (true, nil)。
//
// 按请求顺序逐个调用即可得到与并发调度无关的确定性分配结果。
func PlanRequest(r Request, budget int64, perReqPaths int, budgetLeft *int64) (bool, error) {
	if r.Method != pricing.MonteCarlo {
		return true, nil
	}
	paths := r.Paths
	if paths <= 0 {
		paths = perReqPaths
	}
	if paths <= 0 {
		paths = 100000
	}
	if budget > 0 {
		if int64(paths) > *budgetLeft {
			return false, fmt.Errorf("%w: 请求 %s 需要 %d 条路径，剩余预算 %d",
				ErrBudgetExceeded, r.ID, paths, *budgetLeft)
		}
		*budgetLeft -= int64(paths)
	}
	return true, nil
}

// PriceOne 在本引擎的市场环境下执行单个估值请求（含缓存读写）。
//
// 与 PriceBatch 中的单请求路径语义完全一致：ctx 已取消/超时时返回
// 对应错误且不回写缓存，因此调用方可以用各自独立的上下文驱动
// 同一引擎而不互相污染缓存。
func (e *Engine) PriceOne(ctx context.Context, r Request) Result {
	var out Result
	out.ID = r.ID
	e.runOne(ctx, r, &out)
	return out
}

// CacheLen 返回当前成功结果缓存的条目数（主要用于测试与资源观测）。
func (e *Engine) CacheLen() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.cache)
}

// runOne 执行单个请求。取消时不写缓存。
func (e *Engine) runOne(ctx context.Context, r Request, out *Result) {
	if err := ctx.Err(); err != nil {
		out.Err = err
		return
	}
	env := e.env
	key := cacheKey{spec: r.Spec, method: r.Method, seed: r.Seed, paths: e.pathsFor(r)}
	if r.Env != nil {
		env = *r.Env
		key.env = env
		key.hasEnv = true
	}
	e.mu.Lock()
	cached, hit := e.cache[key]
	e.mu.Unlock()
	if hit {
		out.Price = cached
		return
	}
	price, err := pricing.Price(r.Spec, env, r.Method, pricing.MCParams{
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
	// 缓存上限：超出时淘汰一个既有条目。Go map 迭代顺序不确定，但淘汰
	// 只影响命中率、不影响任何价格结果，因此不损害可复现性。
	if e.cfg.CacheLimit > 0 && len(e.cache) >= e.cfg.CacheLimit {
		for k := range e.cache {
			delete(e.cache, k)
			break
		}
	}
	e.cache[key] = price
	e.mu.Unlock()
	out.Price = price
}
