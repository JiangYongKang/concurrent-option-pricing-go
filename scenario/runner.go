// Package scenario 提供多市场状态的并发场景评估：
// 命名市场状态注册、共享资源约束下的跨状态并发估值、
// 以及按标的的跨状态对比报告。
//
// 语义约定：
//   - 共享约束：一次 Run 内所有市场状态共用同一个 worker 池、
//     同一份路径预算与同一个超时；预算按（状态顺序 × 请求顺序）
//     确定性预分配，与并发调度无关。
//   - 隔离：每个市场状态有独立的校准结果与结果缓存，中间结果
//     绝不跨状态共享；单个状态的取消/超时只影响该状态本身，
//     其余状态照常完成，被取消的状态不留下任何缓存或半截结果。
//   - 可复现：每个请求的随机性只来自其自身 Seed；相同快照、
//     相同种子、相同并发安排下结果逐位一致。
//   - 资源受控：缓存按状态分隔且条目数有上限（MaxCacheEntries），
//     Run 结束后整体释放，不随状态数量无限增长。
//   - 数据完整性：状态数据缺失/不可校准时该状态整体记为
//     “数据不可用”失败，绝不用补齐值或默认值蒙混。
package scenario

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/highcumontoa/concurrent-option-pricing-go/calibration"
	"github.com/highcumontoa/concurrent-option-pricing-go/engine"
	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
	"github.com/highcumontoa/concurrent-option-pricing-go/pricing"
)

// Config 控制多状态评估的共享资源语义。
// 一次 Run 内所有市场状态共用同一份并发上限、路径预算与超时约束。
type Config struct {
	MaxWorkers        int           // 全部状态共用的 worker 池大小；<=0 时取 4
	PathBudget        int64         // 整批（跨状态）蒙特卡洛路径总预算；<=0 表示不限
	PerReqPaths       int           // 每个 MC 请求的默认路径数；<=0 时取 100000
	Timeout           time.Duration // 整批超时；<=0 表示不限
	MaxCacheEntries   int           // 每个状态的结果缓存条目上限；<=0 时取 1024
	DegradeToBinomial bool          // MC 未收敛/超预算时是否降级为二叉树（结果记为“已降级”）
}

func (c *Config) withDefaults() Config {
	out := *c
	if out.MaxWorkers <= 0 {
		out.MaxWorkers = 4
	}
	if out.PerReqPaths <= 0 {
		out.PerReqPaths = 100000
	}
	if out.MaxCacheEntries <= 0 {
		out.MaxCacheEntries = 1024
	}
	return out
}

// ScenarioSpec 指定一个待评估的市场状态。
type ScenarioSpec struct {
	Name string
	// Ctx 可选；非 nil 时该状态在共享约束之外再叠加一个独立上下文，
	// 用于表达“单个状态被取消/超时”：其取消只影响本状态。
	Ctx context.Context
}

// Runner 在多个市场状态下并发执行同一批估值请求。
type Runner struct {
	reg *Registry
	cfg Config
}

// NewRunner 创建多状态评估执行器。
func NewRunner(reg *Registry, cfg Config) *Runner {
	return &Runner{reg: reg, cfg: cfg.withDefaults()}
}

// cacheKey 是单个状态内部的结果缓存键。
type cacheKey struct {
	spec   pricing.Spec
	method pricing.Method
	seed   uint64
	paths  int
}

// scenarioState 是一个市场状态在一次 Run 内的独立运行现场：
// 校准结果与缓存均为该状态私有，Run 结束即整体释放。
type scenarioState struct {
	name  string
	spot  float64
	curve *calibration.DiscountCurve
	vs    *calibration.VolSurface

	sctx  context.Context    // 共享约束 + 可选的独立取消
	errFn func() error       // 该状态当前的取消/超时原因（未取消为 nil）

	mu    sync.Mutex
	cache map[cacheKey]float64
}

// env 为该状态下的单个请求构造市场环境。
func (s *scenarioState) env(spec pricing.Spec) pricing.MarketEnv {
	return pricing.MarketEnv{
		Spot: s.spot,
		Rate: s.curve.ZeroRate(spec.Expiry),
		Vol:  s.vs.Vol(spec.Expiry, spec.Strike),
	}
}

func methodName(m pricing.Method) string {
	switch m {
	case pricing.Analytic:
		return "Analytic"
	case pricing.Binomial:
		return "Binomial"
	case pricing.MonteCarlo:
		return "MonteCarlo"
	}
	return "Unknown"
}

// Run 把同一批请求放到多个市场状态下并发执行，返回对比报告。
//
// 结果中状态的顺序与 scenarios 一致，每个状态内标的的顺序与 reqs 一致。
// 未知状态与数据不可用状态都以状态级失败显式记录，不影响其余状态。
func (r *Runner) Run(ctx context.Context, scenarios []ScenarioSpec, reqs []engine.Request) *Report {
	if r.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.cfg.Timeout)
		defer cancel()
	}
	rep := &Report{Scenarios: make([]ScenarioResult, len(scenarios))}
	for _, q := range reqs {
		rep.RequestIDs = append(rep.RequestIDs, q.ID)
	}

	// 第一阶段：逐状态加载与校准。失败的状态整体记为状态级失败，
	// 绝不进入估值阶段，也不影响其他状态。
	states := make([]*scenarioState, len(scenarios))
	var cancels []context.CancelCauseFunc
	defer func() {
		for _, c := range cancels {
			c(nil)
		}
	}()
	for i, sc := range scenarios {
		rep.Scenarios[i].Name = sc.Name
		// 共享上下文（含整批超时）之上叠加可选的独立取消；
		// 用 CancelCause 保留取消原因（Canceled vs DeadlineExceeded）。
		sctx, cancel := context.WithCancelCause(ctx)
		cancels = append(cancels, cancel)
		if sc.Ctx != nil {
			context.AfterFunc(sc.Ctx, func() { cancel(sc.Ctx.Err()) })
		}
		snap, err := r.reg.Load(sc.Name)
		if err != nil {
			rep.Scenarios[i].Err = err
			continue
		}
		st, err := buildState(sc.Name, snap, sctx)
		if err != nil {
			rep.Scenarios[i].Err = err
			continue
		}
		states[i] = st
	}

	// 第二阶段：预算预分配。按（状态顺序 × 请求顺序）确定性地分配
	// 共享路径预算；超预算的 MC 请求按策略降级或明确拒绝。
	type job struct {
		scenario, req int
		degrade       error // 非空表示该请求已确定降级（如超预算）
	}
	var jobs []job
	items := make([][]ItemResult, len(scenarios))
	budgetLeft := r.cfg.PathBudget
	unlimited := r.cfg.PathBudget <= 0
	for si := range scenarios {
		if states[si] == nil {
			continue
		}
		items[si] = make([]ItemResult, len(reqs))
		for ri, q := range reqs {
			items[si][ri].ID = q.ID
			if q.Method != pricing.MonteCarlo {
				jobs = append(jobs, job{si, ri, nil})
				continue
			}
			paths := int64(r.pathsFor(q))
			if !unlimited && paths > budgetLeft {
				budgetErr := fmt.Errorf("%w: 状态 %s 请求 %s 需要 %d 条路径，剩余预算 %d",
					engine.ErrBudgetExceeded, scenarios[si].Name, q.ID, paths, budgetLeft)
				if r.cfg.DegradeToBinomial {
					jobs = append(jobs, job{si, ri, budgetErr})
				} else {
					items[si][ri].Outcome = OutcomeRejected
					items[si][ri].Reason = ReasonBudgetExceeded
					items[si][ri].Detail = budgetErr.Error()
				}
				continue
			}
			if !unlimited {
				budgetLeft -= paths
			}
			jobs = append(jobs, job{si, ri, nil})
		}
	}

	// 第三阶段：共享 worker 池并发执行全部（状态 × 请求）作业。
	if len(jobs) > 0 {
		ch := make(chan job)
		var wg sync.WaitGroup
		workers := r.cfg.MaxWorkers
		if workers > len(jobs) {
			workers = len(jobs)
		}
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range ch {
					r.runOne(states[j.scenario], reqs[j.req], j.degrade, &items[j.scenario][j.req])
				}
			}()
		}
		for _, j := range jobs {
			ch <- j
		}
		close(ch)
		wg.Wait()
	}

	// 第四阶段：汇总。每个状态内部结果顺序与请求顺序一致。
	for si := range scenarios {
		if states[si] != nil {
			rep.Scenarios[si].Items = items[si]
		}
	}
	return rep
}

// buildState 加载并校准一个市场状态，建立其独立运行现场。
// 快照数据缺失/非法或不可校准时返回错误（归类为数据不可用），
// 不做任何补齐或默认兜底。
func buildState(name string, snap *marketdata.Snapshot, sctx context.Context) (*scenarioState, error) {
	curve, err := calibration.BuildDiscountCurve(snap)
	if err != nil {
		return nil, fmt.Errorf("状态 %s 贴现曲线不可构建: %w", name, err)
	}
	vs, err := calibration.BuildVolSurface(snap, curve)
	if err != nil {
		return nil, fmt.Errorf("状态 %s 波动率曲面不可构建: %w", name, err)
	}
	return &scenarioState{
		name:  name,
		spot:  snap.Spot,
		curve: curve,
		vs:    vs,
		sctx:  sctx,
		errFn: func() error { return context.Cause(sctx) },
		cache: map[cacheKey]float64{},
	}, nil
}

func (r *Runner) pathsFor(q engine.Request) int {
	if q.Paths > 0 {
		return q.Paths
	}
	return r.cfg.PerReqPaths
}

// runOne 在指定状态下执行单个请求。
//
// 取消/超时的请求不写入缓存；MC 未收敛或已确定降级（超预算）时，
// 按策略回退二叉树并记为“已降级”，否则记为可区分原因的“已拒绝”。
func (r *Runner) runOne(st *scenarioState, q engine.Request, degrade error, out *ItemResult) {
	if err := st.errFn(); err != nil {
		out.Outcome = OutcomeRejected
		out.Reason = classify(err)
		out.Detail = err.Error()
		return
	}
	method, paths := q.Method, r.pathsFor(q)
	if degrade != nil { // 已确定降级（超预算）：直接用回退方法定价
		r.priceDegraded(st, q, pricing.Binomial, degrade, out)
		return
	}
	key := cacheKey{spec: q.Spec, method: method, seed: q.Seed, paths: paths}
	st.mu.Lock()
	cached, hit := st.cache[key]
	st.mu.Unlock()
	if hit {
		out.Outcome = OutcomePriced
		out.Price = cached
		out.Method = methodName(method)
		return
	}
	price, err := pricing.Price(q.Spec, st.env(q.Spec), method, pricing.MCParams{
		Paths: paths,
		Seed:  q.Seed,
	})
	if err != nil {
		// MC 未收敛按策略降级为二叉树；其余失败明确拒绝。
		if r.cfg.DegradeToBinomial && method == pricing.MonteCarlo &&
			errors.Is(err, pricing.ErrNotConverged) {
			r.priceDegraded(st, q, pricing.Binomial, err, out)
			return
		}
		out.Outcome = OutcomeRejected
		out.Reason = classify(err)
		out.Detail = err.Error()
		return
	}
	// 定价完成后再次检查取消：已取消的状态不回写缓存，
	// 避免留下“无人认领”的半截状态。
	if err := st.errFn(); err != nil {
		out.Outcome = OutcomeRejected
		out.Reason = classify(err)
		out.Detail = err.Error()
		return
	}
	st.mu.Lock()
	if len(st.cache) < r.cfg.MaxCacheEntries { // 缓存条目有界，满了即停止插入
		st.cache[key] = price
	}
	st.mu.Unlock()
	out.Outcome = OutcomePriced
	out.Price = price
	out.Method = methodName(method)
}

// priceDegraded 用回退方法定价并记为“已降级”，保留原始失败原因。
// 回退方法本身失败时记为“已拒绝”，原因取原始失败（更贴近根因）。
func (r *Runner) priceDegraded(st *scenarioState, q engine.Request, fallback pricing.Method, cause error, out *ItemResult) {
	price, err := pricing.Price(q.Spec, st.env(q.Spec), fallback, pricing.MCParams{})
	if err != nil {
		out.Outcome = OutcomeRejected
		out.Reason = classify(cause)
		out.Detail = fmt.Sprintf("降级为 %s 仍失败: %v（原始原因: %v）", methodName(fallback), err, cause)
		return
	}
	if err := st.errFn(); err != nil {
		out.Outcome = OutcomeRejected
		out.Reason = classify(err)
		out.Detail = err.Error()
		return
	}
	out.Outcome = OutcomeDegraded
	out.Price = price
	out.Method = methodName(fallback)
	out.Reason = classify(cause)
	out.Detail = cause.Error()
}
