// Package engine 提供带并发上限、路径预算与超时控制的批量估值。
package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/highcumontoa/concurrent-option-pricing-go/market"
	"github.com/highcumontoa/concurrent-option-pricing-go/pricing"
)

// ErrBudgetExhausted 本批次蒙特卡洛路径预算耗尽，请求被明确拒绝。
var ErrBudgetExhausted = errors.New("engine: monte carlo path budget exhausted")

// Request 单个估值请求。
type Request struct {
	Option pricing.Option
	Method pricing.Method
	MC     pricing.MCConfig
}

// Limits 引擎资源约束。
type Limits struct {
	MaxConcurrency int   // 并发上限（<=0 表示 4）
	PathBudget     int64 // 本批次蒙特卡洛总路径预算（<=0 表示不限）
	TreeSteps      int   // 二叉树步数（<=0 用默认值）
}

// Outcome 单个请求的结果或失败原因。
type Outcome struct {
	Request Request
	Result  pricing.Result
	Err     error
}

// Engine 批量估值引擎。对同一快照可复现。
type Engine struct {
	snap   *market.Snapshot
	curve  *market.DiscountCurve
	surf   *market.VolSurface
	limits Limits

	mu    sync.Mutex
	cache map[cacheKey]pricing.Result // 仅缓存成功且未被取消的结果
}

// cacheKey 缓存键：完整刻画一次估值的全部输入。
type cacheKey struct {
	method    pricing.Method
	isCall    bool
	isAmer    bool
	spot      float64
	strike    float64
	expiry    float64
	vol       float64
	rate      float64
	seed      uint64
	maxPaths  int
	batchSize int
	tolerance float64
	steps     int
}

// New 基于已校准的快照构建引擎。
func New(snap *market.Snapshot, curve *market.DiscountCurve, surf *market.VolSurface, limits Limits) *Engine {
	if limits.MaxConcurrency <= 0 {
		limits.MaxConcurrency = 4
	}
	if limits.TreeSteps <= 0 {
		limits.TreeSteps = pricing.DefaultTreeSteps
	}
	return &Engine{
		snap:   snap,
		curve:  curve,
		surf:   surf,
		limits: limits,
		cache:  map[cacheKey]pricing.Result{},
	}
}

// keyOf 由请求与快照派生缓存键。波动率与利率取自曲面/曲线，
// 保证相同快照下相同请求得到相同键。
func (e *Engine) keyOf(r Request) cacheKey {
	o := r.Option
	return cacheKey{
		method:    r.Method,
		isCall:    o.IsCall,
		isAmer:    o.IsAmerican,
		spot:      e.snap.Spot,
		strike:    o.Strike,
		expiry:    o.Expiry,
		vol:       e.surf.Vol(o.Expiry, o.Strike),
		rate:      e.curve.ZeroRate(o.Expiry),
		seed:      r.MC.Seed,
		maxPaths:  r.MC.MaxPaths,
		batchSize: r.MC.BatchSize,
		tolerance: r.MC.Tolerance,
		steps:     e.limits.TreeSteps,
	}
}

// priceOne 执行单个请求的定价（不含并发与预算控制）。
func (e *Engine) priceOne(r Request) (pricing.Result, error) {
	o := r.Option
	o.Spot = e.snap.Spot
	o.Vol = e.surf.Vol(o.Expiry, o.Strike)
	o.Rate = e.curve.ZeroRate(o.Expiry)
	switch r.Method {
	case pricing.MethodAnalytic:
		return pricing.PriceAnalytic(&o)
	case pricing.MethodTree:
		return pricing.PriceTree(&o, e.limits.TreeSteps)
	case pricing.MethodMonteCarlo:
		return pricing.PriceMonteCarlo(&o, r.MC)
	default:
		return pricing.Result{}, fmt.Errorf("engine: unknown method %d", r.Method)
	}
}

// Run 批量执行估值请求，返回与请求一一对应的结果切片。
//
// 语义：
//   - 并发上限由 Limits.MaxConcurrency 控制；
//   - 蒙特卡洛路径预算按请求索引顺序预分配（最坏情况 = MC.MaxPaths），
//     超预算的请求在启动前即被 ErrBudgetExhausted 明确拒绝，
//     拒绝集合只由输入顺序决定，与 goroutine 调度无关；
//   - ctx 取消/超时时，未启动的请求标记为 ctx.Err()；已启动请求若完成时
//     上下文已取消，则结果丢弃，不写入缓存、不残留部分状态；
//   - 结果可复现：每个请求的随机流只由自身种子决定，与并发布局无关。
func (e *Engine) Run(ctx context.Context, reqs []Request) []Outcome {
	outcomes := make([]Outcome, len(reqs))
	for i := range reqs {
		outcomes[i].Request = reqs[i]
	}

	sem := make(chan struct{}, e.limits.MaxConcurrency)
	var wg sync.WaitGroup
	var reserved int64
	for i := range reqs {
		r := reqs[i]
		// 已取消：直接标记，不启动 goroutine。
		if err := ctx.Err(); err != nil {
			for j := i; j < len(reqs); j++ {
				outcomes[j].Err = err
			}
			break
		}
		// 预算按索引顺序预分配，保证拒绝集合确定。
		var need int64
		if r.Method == pricing.MethodMonteCarlo && r.MC.MaxPaths > 0 {
			need = int64(r.MC.MaxPaths)
		}
		if e.limits.PathBudget > 0 && need > 0 && reserved+need > e.limits.PathBudget {
			outcomes[i].Err = fmt.Errorf("%w: request %d needs %d paths, budget %d, already reserved %d",
				ErrBudgetExhausted, i, need, e.limits.PathBudget, reserved)
			continue
		}
		reserved += need

		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			r := reqs[i]
			key := e.keyOf(r)

			e.mu.Lock()
			cached, hit := e.cache[key]
			e.mu.Unlock()
			if hit {
				outcomes[i].Result = cached
				return
			}

			res, err := e.priceOne(r)
			if err != nil {
				outcomes[i].Err = err
				return
			}
			// 上下文已取消：丢弃结果，不写缓存、不残留部分状态。
			if cerr := ctx.Err(); cerr != nil {
				outcomes[i].Err = cerr
				return
			}
			e.mu.Lock()
			e.cache[key] = res
			e.mu.Unlock()
			outcomes[i].Result = res
		}(i)
	}
	wg.Wait()
	return outcomes
}
