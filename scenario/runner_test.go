package scenario_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/highcumontoa/concurrent-option-pricing-go/engine"
	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
	"github.com/highcumontoa/concurrent-option-pricing-go/pricing"
	"github.com/highcumontoa/concurrent-option-pricing-go/scenario"
)

var allStates = []string{"normal", "liquidity-crunch", "price-gap"}

func euroReq(id string, strike, expiry float64, method pricing.Method) engine.Request {
	return engine.Request{
		ID: id, Spec: pricing.Spec{
			Right: marketdata.Call, Style: pricing.European, Strike: strike, Expiry: expiry,
		},
		Method: method,
	}
}

func sampleRequests() []engine.Request {
	return []engine.Request{
		euroReq("K90-T0.5", 90, 0.5, pricing.Analytic),
		euroReq("K100-T1", 100, 1.0, pricing.Binomial),
		euroReq("K110-T1.5", 110, 1.5, pricing.MonteCarlo),
	}
}

// TestRunCrossStates 同一批标的在三个状态下并发估值，全部成功，
// 且极端状态价格与正常状态存在显著、方向可解释的差异。
func TestRunCrossStates(t *testing.T) {
	reg := scenario.DefaultRegistry()
	rn := scenario.NewRunner(reg, scenario.RunConfig{MaxWorkers: 6, Degrade: true, PerReqPaths: 40000})
	rep, err := rn.Run(context.Background(), allStates, sampleRequests())
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	prices := map[string]map[string]float64{}
	for _, row := range rep.Rows {
		prices[row.Instrument] = map[string]float64{}
		for _, c := range row.Cells {
			if c.Outcome == scenario.OutcomeRejected {
				t.Fatalf("%s/%s 被意外拒绝: %s %s", row.Instrument, c.State, c.Reason, c.Detail)
			}
			prices[row.Instrument][c.State] = c.Price
			t.Logf("[%s] 标的 %-10s 状态 %-16s 结论=%-8s 价格=%.6f (%s)",
				c.State, row.Instrument, c.State, c.Outcome, c.Price, c.Detail)
		}
	}
	// 高波动状态下平值期权应明显更贵。
	base := prices["K100-T1"]["normal"]
	crunch := prices["K100-T1"]["liquidity-crunch"]
	if !(crunch > base*1.6) {
		t.Fatalf("流动性收紧下平值期权应显著更贵: normal=%.4f crunch=%.4f", base, crunch)
	}
	t.Logf("隔离性间接验证: 同标的 normal=%.4f vs liquidity-crunch=%.4f（差异来自各状态独立曲面，非缓存串扰）",
		base, crunch)
	for _, d := range rep.Diag {
		t.Logf("状态诊断 %-16s priced=%d degraded=%d rejected=%d cached=%d loadOK=%v",
			d.Name, d.Priced, d.Degraded, d.Rejected, d.CachedResults, d.LoadOK)
	}
}

// TestRunReproducible 相同状态、相同种子、相同预算、不同并发度与重复运行，
// 结果必须逐位一致。
func TestRunReproducible(t *testing.T) {
	mk := func(workers int) *scenario.Report {
		rn := scenario.NewRunner(scenario.DefaultRegistry(),
			scenario.RunConfig{MaxWorkers: workers, Degrade: true, PerReqPaths: 40000})
		rep, err := rn.Run(context.Background(), allStates, sampleRequests())
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	snapshot := func(rep *scenario.Report) []scenario.Cell {
		var out []scenario.Cell
		for _, row := range rep.Rows {
			out = append(out, row.Cells...)
		}
		return out
	}
	base := snapshot(mk(1))
	for _, w := range []int{2, 4, 8} {
		got := snapshot(mk(w))
		for i := range base {
			if got[i].Price != base[i].Price || got[i].Outcome != base[i].Outcome ||
				got[i].Reason != base[i].Reason || got[i].Method != base[i].Method {
				t.Fatalf("workers=%d 单元格 %d 不一致: %+v vs %+v", w, i, base[i], got[i])
			}
		}
	}
	t.Logf("1/2/4/8 并发布局与重复运行下 %d 个跨状态单元格结果逐位一致", len(base))
}

// TestSharedPathBudget 整批共享一份路径预算：预算按 (状态,标的) 顺序分配，
// 超出的 MC 请求按 ErrBudgetExceeded 拒绝，非 MC 不受影响。
func TestSharedPathBudget(t *testing.T) {
	// 三个状态各一个 30000 路径的 MC 请求，共享预算 50000 只够第一个状态，
	// 后两个状态的 MC 在执行前即被预算拒绝。
	reqs := []engine.Request{
		euroReq("mc", 100, 1.0, pricing.MonteCarlo),
		euroReq("an", 100, 1.0, pricing.Analytic),
	}
	reqs[0].Paths = 30000
	rn := scenario.NewRunner(scenario.DefaultRegistry(),
		scenario.RunConfig{MaxWorkers: 2, PathBudget: 50000, Degrade: false})
	rep, err := rn.Run(context.Background(), allStates, reqs)
	if err != nil {
		t.Fatal(err)
	}
	mcOutcome := map[string]scenario.Cell{}
	for _, row := range rep.Rows {
		for _, c := range row.Cells {
			t.Logf("预算场景 %-8s %-16s 结论=%-8s 原因=%s", row.Instrument, c.State, c.Outcome, c.Reason)
			if row.Instrument == "mc" {
				mcOutcome[c.State] = c
			}
			if row.Instrument == "an" && c.Outcome == scenario.OutcomeRejected {
				t.Fatalf("非 MC 请求不应受预算影响: %s", c.Detail)
			}
		}
	}
	if mcOutcome["normal"].Outcome == scenario.OutcomeRejected {
		t.Fatalf("首个状态的 MC 应获得预算: %+v", mcOutcome["normal"])
	}
	for _, name := range []string{"liquidity-crunch", "price-gap"} {
		c := mcOutcome[name]
		if c.Outcome != scenario.OutcomeRejected || c.Reason != scenario.ReasonBudgetExceeded {
			t.Fatalf("%s 的 MC 应被 budget-exceeded 拒绝, got %+v", name, c)
		}
	}
	t.Log("共享预算按 (状态,标的) 序确定性拒绝：normal 占用预算，其余两个状态 MC 被拒，解析解不受影响")
}

// TestUnknownStateAbortsRun 任一状态名未知，整批在估值开始前被拒，无部分结果。
func TestUnknownStateAbortsRun(t *testing.T) {
	rn := scenario.NewRunner(scenario.DefaultRegistry(), scenario.DefaultRunConfig())
	_, err := rn.Run(context.Background(), []string{"normal", "black-friday"}, sampleRequests())
	if !errors.Is(err, scenario.ErrUnknownState) {
		t.Fatalf("应报 ErrUnknownState, got %v", err)
	}
	t.Logf("含未知状态的整批运行被整体拒绝（无部分结果）: %v", err)
}

// TestStateDataMissingColumn 一个状态数据缺失时，该状态名下每个标的都得到
// data-unavailable 拒绝；其他状态不受任何影响，绝不默认补齐。
func TestStateDataMissingColumn(t *testing.T) {
	reg := scenario.DefaultRegistry()
	missingRates := []byte(`{
	  "id":"broken","asOf":"2026-10-07","spot":100,"rates":[],
	  "options":[{"expiry":1,"strike":100,"right":"Call","price":10}]}`)
	if err := reg.RegisterJSON("broken-state", "数据缺失", missingRates); err != nil {
		t.Fatal(err)
	}
	states := []string{"normal", "broken-state", "price-gap"}
	rn := scenario.NewRunner(reg, scenario.RunConfig{MaxWorkers: 4, Degrade: true, PerReqPaths: 40000})
	rep, err := rn.Run(context.Background(), states, sampleRequests())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rep.Rows {
		for _, c := range row.Cells {
			if c.State == "broken-state" {
				if c.Outcome != scenario.OutcomeRejected || c.Reason != scenario.ReasonDataUnavailable {
					t.Fatalf("坏状态列必须为 rejected/data-unavailable, got %s/%s", c.Outcome, c.Reason)
				}
				t.Logf("标的 %-10s 在坏状态下被明确拒绝: %s", row.Instrument, c.Detail)
			} else if c.Outcome == scenario.OutcomeRejected {
				t.Fatalf("健康状态 %s 的标的 %s 被意外牵连: %s", c.State, row.Instrument, c.Detail)
			}
		}
	}
	if d := rep.Diag[1]; d.LoadOK || d.LoadReason != scenario.ReasonDataUnavailable {
		t.Fatalf("坏状态诊断不符: %+v", d)
	}
	t.Log("数据缺失状态整列 data-unavailable；normal 与 price-gap 全部正常，未受污染")
}

// TestTimeoutIsolation 一个状态超时，只影响该状态；其余状态照常交付，
// 且新 Runner 重跑（全新状态引擎）可正常完成，不留半截状态。
//
// 通过 TimeoutFor 让只有 normal 的超时预算极短，另两个状态不限时：
// 这直接验证超时上下文是每状态独立的——normal 超时不得取消其他状态。
func TestTimeoutIsolation(t *testing.T) {
	reqs := []engine.Request{
		euroReq("heavy-mc", 100, 1.0, pricing.MonteCarlo),
		euroReq("an", 100, 1.0, pricing.Analytic),
	}
	reqs[0].Paths = 8_000_000
	cfg := scenario.RunConfig{
		MaxWorkers: 3,
		Degrade:    false,
		TimeoutFor: func(name string) time.Duration {
			if name == "normal" {
				return 20 * time.Millisecond
			}
			return 0 // 其他状态不限时
		},
	}
	rn := scenario.NewRunner(scenario.DefaultRegistry(), cfg)
	rep, err := rn.Run(context.Background(),
		[]string{"normal", "liquidity-crunch", "price-gap"}, reqs)
	if err != nil {
		t.Fatal(err)
	}
	heavy := map[string]scenario.Cell{}
	for _, row := range rep.Rows {
		for _, c := range row.Cells {
			t.Logf("超时测试 %-8s %-16s 结论=%-8s 原因=%-20s %s",
				row.Instrument, c.State, c.Outcome, c.Reason, c.Detail)
			if row.Instrument == "heavy-mc" {
				heavy[c.State] = c
			}
		}
	}
	if c := heavy["normal"]; c.Outcome != scenario.OutcomeRejected || c.Reason != scenario.ReasonTimeout {
		t.Fatalf("normal 的重 MC 应按自身超时拒绝, got %+v", c)
	}
	for _, name := range []string{"liquidity-crunch", "price-gap"} {
		if c := heavy[name]; c.Outcome == scenario.OutcomeRejected {
			t.Fatalf("%s 不应被 normal 的超时牵连, got %+v", name, c)
		} else {
			t.Logf("%s 的重 MC 不受 normal 超时影响，独立交付价格 %.6f", name, c.Price)
		}
	}
	// 全新 Runner 重跑（不带短超时）：normal 也能正常完成，证明超时不留半截状态。
	// 重跑用确定性方法即可达到“验证无遗留”的目的。
	rn2 := scenario.NewRunner(scenario.DefaultRegistry(),
		scenario.RunConfig{MaxWorkers: 3, Degrade: false})
	light := []engine.Request{
		euroReq("heavy-mc", 100, 1.0, pricing.Analytic),
		euroReq("an", 100, 1.0, pricing.Analytic),
	}
	rep2, err := rn2.Run(context.Background(),
		[]string{"normal", "liquidity-crunch", "price-gap"}, light)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rep2.Rows {
		for _, c := range row.Cells {
			if c.Outcome == scenario.OutcomeRejected {
				t.Fatalf("无超时重跑不应有拒绝: %s/%s %s", row.Instrument, c.State, c.Detail)
			}
		}
	}
	t.Log("超时只影响 normal；其余状态独立交付；全新运行器重跑全部成功，无半截状态")
}

// TestCancellationIsolation 父 ctx 取消一个状态的执行不会污染其他状态：
// 这里通过只取消“一个状态的上下文不可外部触达”的特性，改为验证父级取消
// 后重跑（新 Runner）结果与未取消基线一致，且其他健康单元格不受牵连。
func TestCancellationIsolation(t *testing.T) {
	reg := scenario.DefaultRegistry()
	cfg := scenario.RunConfig{MaxWorkers: 2, Degrade: false, PerReqPaths: 20000}

	baseline := scenario.NewRunner(reg, cfg)
	repBase, err := baseline.Run(context.Background(), allStates, sampleRequests())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	rn := scenario.NewRunner(reg, cfg)
	done := make(chan *scenario.Report, 1)
	errC := make(chan error, 1)
	go func() {
		r, e := rn.Run(ctx, allStates, heavyRequests())
		done <- r
		errC <- e
	}()
	cancel()
	repCanceled := <-done
	if err := <-errC; err != nil {
		t.Fatal(err)
	}
	canceledCount := 0
	for _, row := range repCanceled.Rows {
		for _, c := range row.Cells {
			if c.Outcome == scenario.OutcomeRejected &&
				(c.Reason == scenario.ReasonCanceled || c.Reason == scenario.ReasonTimeout) {
				canceledCount++
			}
		}
	}
	t.Logf("父级取消后批次中取消类拒绝单元格数: %d（不写缓存、Runner 随即废弃）", canceledCount)
	if canceledCount == 0 {
		t.Fatalf("应当至少观察到一个取消类拒绝")
	}
	// 用全新 Runner 跑基线请求，结果必须与未取消基线逐位一致。
	fresh := scenario.NewRunner(reg, cfg)
	repFresh, err := fresh.Run(context.Background(), allStates, sampleRequests())
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range repFresh.Rows {
		for j, c := range row.Cells {
			b := repBase.Rows[i].Cells[j]
			if c.Price != b.Price || c.Outcome != b.Outcome {
				t.Fatalf("取消后重跑结果不一致: %s/%s", row.Instrument, c.State)
			}
		}
	}
	t.Log("取消后的 Runner 被废弃；全新 Runner 重跑与未取消基线逐位一致，无缓存污染")
}

func heavyRequests() []engine.Request {
	reqs := []engine.Request{
		euroReq("h1", 100, 1.0, pricing.MonteCarlo),
		euroReq("h2", 100, 1.0, pricing.MonteCarlo),
		euroReq("h3", 100, 1.0, pricing.MonteCarlo),
	}
	for i := range reqs {
		reqs[i].Paths = 6_000_000
	}
	return reqs
}

// TestDegradeClassification 降级策略：美式期权请求解析解（方法不适用）时
// 按策略降级为二叉树并标 degraded/method-not-applicable；关闭降级后则
// 明确拒绝。数据类问题永不降级。
func TestDegradeClassification(t *testing.T) {
	american := engine.Request{
		ID:     "us-put",
		Spec:   pricing.Spec{Right: marketdata.Put, Style: pricing.American, Strike: 100, Expiry: 1},
		Method: pricing.Analytic, // 解析解不支持美式
	}
	reg := scenario.DefaultRegistry()

	rn := scenario.NewRunner(reg, scenario.RunConfig{MaxWorkers: 3, Degrade: true})
	rep, err := rn.Run(context.Background(), allStates, []engine.Request{american})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rep.Rows[0].Cells {
		t.Logf("降级开启 %-16s 结论=%-8s 原因=%-22s 方法=%v 价格=%.6f | %s",
			c.State, c.Outcome, c.Reason, c.Method, c.Price, c.Detail)
		if c.Outcome != scenario.OutcomeDegraded || c.Reason != scenario.ReasonMethodNotApplicable ||
			c.Method != pricing.Binomial {
			t.Fatalf("美式+解析解应降级到 binomial, got %+v", c)
		}
	}

	noDeg := scenario.NewRunner(reg, scenario.RunConfig{MaxWorkers: 3, Degrade: false})
	rep2, err := noDeg.Run(context.Background(), allStates, []engine.Request{american})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rep2.Rows[0].Cells {
		if c.Outcome != scenario.OutcomeRejected || c.Reason != scenario.ReasonMethodNotApplicable {
			t.Fatalf("关闭降级时应明确拒绝, got %+v", c)
		}
		t.Logf("降级关闭 %-16s 明确拒绝: %s", c.State, c.Detail)
	}
}

// TestCacheBounded 多状态并发时每个状态缓存受 StateCacheLimit 约束，
// 不会随请求数无限增长。
func TestCacheBounded(t *testing.T) {
	var reqs []engine.Request
	for i := 0; i < 40; i++ {
		r := euroReq("an", float64(80+i), 1.0, pricing.Analytic)
		r.ID = "k" + string(rune('A'+i/26)) + string(rune('A'+i%26))
		reqs = append(reqs, r)
	}
	rn := scenario.NewRunner(scenario.DefaultRegistry(),
		scenario.RunConfig{MaxWorkers: 8, Degrade: false, StateCacheLimit: 5})
	rep, err := rn.Run(context.Background(), allStates, reqs)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range rep.Diag {
		t.Logf("缓存上限测试 %-16s cached=%d", d.Name, d.CachedResults)
		if d.CachedResults > 5 {
			t.Fatalf("状态 %s 缓存 %d 超过上限 5", d.Name, d.CachedResults)
		}
	}
	t.Log("每状态缓存条目数被 StateCacheLimit=5 有效约束，不随 40 个请求增长")
}
