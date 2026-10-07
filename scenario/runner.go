package scenario

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/highcumontoa/concurrent-option-pricing-go/engine"
	"github.com/highcumontoa/concurrent-option-pricing-go/pricing"
)

// RunConfig 是整批多状态估值共用的并发、路径预算与超时约束。
type RunConfig struct {
	// MaxWorkers 是跨全部市场状态共享的唯一 worker 池大小；<=0 时取 4。
	MaxWorkers int
	// PathBudget 是整批（全部状态合计）蒙特卡洛路径总预算，<=0 不限。
	// 按 (状态顺序, 标的顺序) 确定性分配，与并发调度无关。
	PathBudget int64
	// PerReqPaths 是未显式指定路径数的 MC 请求的默认路径数。
	PerReqPaths int
	// Timeout 是“每个市场状态各自”的超时：一个状态超时只取消该状态，
	// 不影响其他状态；<=0 表示不限。
	Timeout time.Duration
	// TimeoutFor 非 nil 时按状态名覆盖 Timeout（返回 <=0 表示该状态不限时），
	// 便于在同一批次中给个别状态单独的超时预算；各状态的上下文仍然彼此独立。
	TimeoutFor func(stateName string) time.Duration
	// Degrade 为 true 时，首选方法因方法不适用/未收敛失败，按确定性
	// 备选链改走非蒙特卡洛方法（见 fallbackFor）；数据/预算/取消类
	// 失败永不降级。建议通过 DefaultRunConfig 获取（默认 true）。
	Degrade bool
	// StateCacheLimit 是单个市场状态引擎的成功结果缓存条目上限，
	// <=0 时取默认值 4096，防止场景数量与请求数增长时缓存无限膨胀。
	StateCacheLimit int
}

// DefaultRunConfig 返回开启降级、带默认缓存上限的运行配置。
func DefaultRunConfig() RunConfig {
	return RunConfig{
		MaxWorkers:      4,
		PerReqPaths:     100000,
		Timeout:         0,
		Degrade:         true,
		StateCacheLimit: 4096,
	}
}

func (c RunConfig) withDefaults() RunConfig {
	out := c
	if out.MaxWorkers <= 0 {
		out.MaxWorkers = 4
	}
	if out.PerReqPaths <= 0 {
		out.PerReqPaths = 100000
	}
	if out.StateCacheLimit <= 0 {
		out.StateCacheLimit = 4096
	}
	return out
}

// Cell 是单个标的在单个市场状态下的对比单元格（三态互斥）。
type Cell struct {
	State   string
	Outcome Outcome
	Reason  Reason // Priced 时为 ReasonNone
	Detail  string // 人类可读的判定依据
	Price   float64
	Method  pricing.Method // 最终交付价格所用的方法
}

// Row 是一个标的跨全部市场状态的对比结果，顺序与 Run 的 states 入参一致。
type Row struct {
	Instrument string
	Cells      []Cell
}

// StateDiag 是单个市场状态的执行期资源/健康诊断。
type StateDiag struct {
	Name          string
	LoadOK        bool
	LoadReason    Reason // 加载失败时的分类原因
	LoadDetail    string
	CachedResults int // 运行结束后该状态引擎缓存的成功条目数
	Degraded      int // 该状态下按策略降级交付的标的数
	Rejected      int // 该状态下被明确拒绝的标的数
	Priced        int // 该状态下首选方法直接成功的标的数
}

// Report 是整批跨状态对比结果。
type Report struct {
	// States 是实际执行顺序的状态名（与入参一致）。
	States []string
	// Rows 按标的入参顺序给出跨状态对比行。
	Rows []Row
	// Diag 按状态顺序给出诊断信息。
	Diag []StateDiag
}

// Runner 在多个市场状态上并发执行同一批标的的估值对比。
//
// Runner 不跨次运行保留任何每状态引擎或缓存：每次 Run 都重新加载状态、
// 构建独立引擎，运行结束后这些引擎即不可达，因此一次运行中的取消、
// 超时或半截缓存绝不会遗留到下一次运行。
type Runner struct {
	reg *Registry
	cfg RunConfig
}

// NewRunner 创建多状态运行器。
func NewRunner(reg *Registry, cfg RunConfig) *Runner {
	return &Runner{reg: reg, cfg: cfg.withDefaults()}
}

type stateRuntime struct {
	name   string
	eng    *engine.Engine
	ctx    context.Context
	diag   StateDiag
	cancel context.CancelFunc
	loadOK bool
	loaded *loadedState
}

// job 标识跨状态作业矩阵中的一个单元格。
type job struct{ si, ii int }

// Run 并发执行多状态对比估值。
//
// 任一状态名未知时，在启动任何估值之前整体返回 ErrUnknownState，
// 不产生部分结果。
func (rn *Runner) Run(ctx context.Context, stateNames []string,
	reqs []engine.Request) (*Report, error) {
	nS, nI := len(stateNames), len(reqs)
	report := &Report{States: append([]string(nil), stateNames...)}

	// 1) 逐状态解析、校验、校准；失败的状态不构建引擎，但其名下每个标的
	//    仍得到一个数据类拒绝单元格，绝不静默丢弃或默认补齐。
	runtimes := make([]*stateRuntime, nS)
	for si, name := range stateNames {
		st, err := rn.reg.Get(name)
		if err != nil {
			return nil, err // ErrUnknownState：尚未启动任何估值
		}
		rt := &stateRuntime{name: name}
		rt.diag.Name = name
		ls, lerr := st.Load()
		if lerr != nil {
			rt.diag.LoadOK = false
			rt.diag.LoadReason = classifyError(lerr)
			rt.diag.LoadDetail = lerr.Error()
			rt.ctx, rt.cancel = context.WithCancel(ctx)
		} else {
			rt.loadOK = true
			rt.diag.LoadOK = true
			rt.loaded = ls
			sctx, cancel := context.WithCancel(ctx)
			d := rn.cfg.Timeout
			if rn.cfg.TimeoutFor != nil {
				d = rn.cfg.TimeoutFor(name)
			}
			if d > 0 {
				sctx, cancel = context.WithTimeout(ctx, d)
			}
			rt.ctx, rt.cancel = sctx, cancel
			// 每状态独立引擎 = 独立缓存；环境模板为零值，实际每标的
			// 环境通过 Request.Env 按该状态曲面绑定。
			rt.eng = engine.New(pricing.MarketEnv{}, engine.Config{
				MaxWorkers:  1,
				PerReqPaths: rn.cfg.PerReqPaths,
				CacheLimit:  rn.cfg.StateCacheLimit,
			})
		}
		runtimes[si] = rt
	}
	defer func() {
		for _, rt := range runtimes {
			rt.cancel()
		}
	}()

	// 2) 结果矩阵：每个单元格只被一个 worker 写一次，无需加锁。
	cells := make([][]Cell, nI)
	for i := range cells {
		cells[i] = make([]Cell, nS)
		for si := range stateNames {
			cells[i][si] = Cell{State: stateNames[si]}
		}
	}

	// 加载失败的状态：预填拒绝单元格。
	for si, rt := range runtimes {
		if rt.loadOK {
			continue
		}
		for i := range reqs {
			cells[i][si] = Cell{
				State:   rt.name,
				Outcome: OutcomeRejected,
				Reason:  rt.diag.LoadReason,
				Detail:  rt.diag.LoadDetail,
			}
			rt.diag.Rejected++
		}
	}

	// 3) 路径预算按 (状态, 标的) 顺序确定性预分配（跨全部状态共享一份）。
	//    使用纯函数式 PlanRequest：预算上限是本批次的 PathBudget，
	//    各状态引擎自身不持有预算，避免每个状态各自重置预算。
	var jobs []job
	var budgetLeft int64 = rn.cfg.PathBudget
	for si, rt := range runtimes {
		if !rt.loadOK {
			continue
		}
		for ii, rq := range reqs {
			ok, berr := engine.PlanRequest(rq, rn.cfg.PathBudget, rn.cfg.PerReqPaths, &budgetLeft)
			if berr != nil {
				cells[ii][si] = Cell{
					State:   rt.name,
					Outcome: OutcomeRejected,
					Reason:  ReasonBudgetExceeded,
					Detail:  berr.Error(),
				}
				rt.diag.Rejected++
				continue
			}
			if ok {
				jobs = append(jobs, job{si, ii})
			}
		}
	}

	// 4) 共享 worker 池：用容量 MaxWorkers 的信号量约束全局并发。
	//    每个状态由自己的 goroutine 串行领取槽位执行作业——这样一个状态
	//    的排队等待不消耗其超时预算（超时只覆盖真正执行的时间），
	//    从而保证“某状态超时不牵连其他状态”。结果与调度无关，因为每个
	//    单元格只被唯一的状态 goroutine 写入、随机性只来自请求种子。
	sem := make(chan struct{}, rn.cfg.MaxWorkers)
	var wg sync.WaitGroup
	for si := range runtimes {
		rt := runtimes[si]
		stateJobs := jobsForState(jobs, si)
		if len(stateJobs) == 0 {
			continue
		}
		wg.Add(1)
		go func(rt *stateRuntime, js []job) {
			defer wg.Done()
			for k, j := range js {
				select {
				case sem <- struct{}{}:
				case <-rt.ctx.Done():
					// 等待槽位期间本状态已超时/取消：剩余单元格直接按
					// ctx 错误落结果，绝不去占用其他状态仍需要的槽位。
					for _, jj := range js[k:] {
						c := rn.canceledCell(rt, reqs[jj.ii])
						cells[jj.ii][jj.si] = c
						rt.diag.Rejected++
					}
					return
				case <-ctx.Done():
					for _, jj := range js[k:] {
						c := rn.canceledCell(rt, reqs[jj.ii])
						cells[jj.ii][jj.si] = c
						rt.diag.Rejected++
					}
					return
				}
				cell := rn.priceOneCell(rt, reqs[j.ii])
				<-sem
				cells[j.ii][j.si] = cell
				switch cell.Outcome {
				case OutcomePriced:
					rt.diag.Priced++
				case OutcomeDegraded:
					rt.diag.Degraded++
				case OutcomeRejected:
					rt.diag.Rejected++
				}
			}
		}(rt, stateJobs)
	}
	wg.Wait()

	// 5) 汇总报告；每状态引擎在此刻后即不可达（连同其缓存一起被回收）。
	for i, rq := range reqs {
		report.Rows = append(report.Rows, Row{Instrument: rq.ID, Cells: cells[i]})
	}
	for _, rt := range runtimes {
		d := rt.diag
		if rt.eng != nil {
			d.CachedResults = rt.eng.CacheLen()
		}
		report.Diag = append(report.Diag, d)
	}
	return report, nil
}

// jobsForState 从确定性作业序列中筛出指定状态的作业，保持原顺序。
func jobsForState(all []job, si int) []job {
	var out []job
	for _, j := range all {
		if j.si == si {
			out = append(out, j)
		}
	}
	return out
}

// canceledCell 为等待槽位期间被取消/超时的单元格生成拒绝结果。
func (rn *Runner) canceledCell(rt *stateRuntime, rq engine.Request) Cell {
	reason := classifyError(rt.ctx.Err())
	return Cell{
		State:   rt.name,
		Outcome: OutcomeRejected,
		Reason:  reason,
		Detail:  fmt.Sprintf("标的 %s 在执行前 %s: %v", rq.ID, reason, rt.ctx.Err()),
	}
}

// priceOneCell 在单个状态内对单个标的执行“首选方法 + 确定性降级链”。
func (rn *Runner) priceOneCell(rt *stateRuntime, rq0 engine.Request) Cell {
	rq := bindEnv(rt.loaded, rq0)
	base := Cell{State: rt.name}
	res := rt.eng.PriceOne(rt.ctx, rq)
	if res.Err == nil {
		base.Outcome, base.Price, base.Method = OutcomePriced, res.Price, rq.Method
		base.Detail = fmt.Sprintf("首选方法 %s 成功", methodName(rq.Method))
		return base
	}
	reason := classifyError(res.Err)
	if !rn.cfg.Degrade || !reason.degradable() {
		base.Outcome, base.Reason = OutcomeRejected, reason
		base.Detail = fmt.Sprintf("首选方法 %s 被拒绝: %v", methodName(rq.Method), res.Err)
		return base
	}
	fb, ok := fallbackFor(rq)
	if !ok {
		base.Outcome, base.Reason = OutcomeRejected, reason
		base.Detail = fmt.Sprintf("首选方法 %s 失败(%s)且无可用降级路径: %v",
			methodName(rq.Method), reason, res.Err)
		return base
	}
	fres := rt.eng.PriceOne(rt.ctx, bindEnv(rt.loaded, fb))
	if fres.Err != nil {
		fr := classifyError(fres.Err)
		base.Outcome, base.Reason = OutcomeRejected, fr
		base.Detail = fmt.Sprintf("首选 %s 失败(%s: %v)；降级到 %s 仍失败: %v",
			methodName(rq.Method), reason, res.Err, methodName(fb.Method), fres.Err)
		return base
	}
	base.Outcome = OutcomeDegraded
	base.Reason = reason // 记录触发降级的原始原因
	base.Price, base.Method = fres.Price, fb.Method
	base.Detail = fmt.Sprintf("首选 %s %s，按策略降级到 %s 后交付",
		methodName(rq.Method), reason, methodName(fb.Method))
	return base
}

// fallbackFor 返回确定性备选请求（备选方法一律为非 MC，不额外消耗路径预算）：
//
//	解析解 → 二叉树（覆盖美式）；
//	二叉树 → 解析解（仅欧式有意义）；
//	蒙特卡洛 → 欧式走解析解，美式走二叉树。
func fallbackFor(rq engine.Request) (engine.Request, bool) {
	fb := engine.Request{ID: rq.ID, Spec: rq.Spec, Seed: rq.Seed, Paths: rq.Paths}
	switch rq.Method {
	case pricing.Analytic:
		fb.Method = pricing.Binomial
	case pricing.Binomial:
		if rq.Spec.Style != pricing.European {
			return engine.Request{}, false
		}
		fb.Method = pricing.Analytic
	case pricing.MonteCarlo:
		if rq.Spec.Style == pricing.European {
			fb.Method = pricing.Analytic
		} else {
			fb.Method = pricing.Binomial
		}
	default:
		return engine.Request{}, false
	}
	return fb, true
}

// bindEnv 把状态校准出的该标的专属市场环境（现价、期限利率、曲面波动率）
// 绑定到请求上；返回的请求与原请求不共享 Env 指针。
func bindEnv(ls *loadedState, rq engine.Request) engine.Request {
	env := ls.envFor(rq.Spec)
	rq.Env = &env
	return rq
}

func methodName(m pricing.Method) string {
	switch m {
	case pricing.Analytic:
		return "analytic"
	case pricing.Binomial:
		return "binomial"
	case pricing.MonteCarlo:
		return "monte-carlo"
	default:
		return "unknown"
	}
}
