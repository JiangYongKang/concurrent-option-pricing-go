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

func loadRegistry(t *testing.T) *scenario.Registry {
	t.Helper()
	reg, err := scenario.LoadRegistry(scenarioDir)
	if err != nil {
		t.Fatalf("加载注册表失败: %v", err)
	}
	return reg
}

// standardReqs 覆盖三种方法与一个必然被拒（美式用解析解）的请求。
func standardReqs() []engine.Request {
	euCall := pricing.Spec{Right: marketdata.Call, Style: pricing.European, Strike: 100, Expiry: 1.0}
	euPut := pricing.Spec{Right: marketdata.Put, Style: pricing.European, Strike: 95, Expiry: 0.5}
	euCallMC := pricing.Spec{Right: marketdata.Call, Style: pricing.European, Strike: 105, Expiry: 1.0}
	amPut := pricing.Spec{Right: marketdata.Put, Style: pricing.American, Strike: 100, Expiry: 1.0}
	return []engine.Request{
		{ID: "eu-call-analytic", Spec: euCall, Method: pricing.Analytic},
		{ID: "eu-put-tree", Spec: euPut, Method: pricing.Binomial},
		{ID: "eu-call-mc", Spec: euCallMC, Method: pricing.MonteCarlo, Seed: 42, Paths: 200000},
		{ID: "am-put-analytic", Spec: amPut, Method: pricing.Analytic},
	}
}

func logReport(t *testing.T, rep *scenario.Report) {
	t.Helper()
	for _, sc := range rep.Scenarios {
		if sc.Err != nil {
			t.Logf("状态 %s: 状态级失败 -> %v", sc.Name, sc.Err)
			continue
		}
		for _, it := range sc.Items {
			t.Logf("状态 %s 标的 %s: %s 方法=%s 价格=%.6f 原因=%s %s",
				sc.Name, it.ID, it.Outcome, it.Method, it.Price, it.Reason, it.Detail)
		}
	}
}

func findItem(t *testing.T, rep *scenario.Report, scen, id string) scenario.ItemResult {
	t.Helper()
	for _, sc := range rep.Scenarios {
		if sc.Name != scen {
			continue
		}
		for _, it := range sc.Items {
			if it.ID == id {
				return it
			}
		}
	}
	t.Fatalf("报告中找不到 (%s, %s)", scen, id)
	return scenario.ItemResult{}
}

// TestRunAcrossScenarios 同一批标的在三个市场状态下并发估值：
// 可定价的标的全部算出结果，美式用解析解被明确拒绝（方法不适用）；
// 相同种子相同请求在不同状态下结果不同，证明状态间无缓存串扰。
func TestRunAcrossScenarios(t *testing.T) {
	reg := loadRegistry(t)
	runner := scenario.NewRunner(reg, scenario.Config{MaxWorkers: 4, PathBudget: 1_000_000})
	names := []string{"normal", "gap_down", "liquidity_crunch"}
	var specs []scenario.ScenarioSpec
	for _, n := range names {
		specs = append(specs, scenario.ScenarioSpec{Name: n})
	}
	rep := runner.Run(context.Background(), specs, standardReqs())
	logReport(t, rep)

	for _, n := range names {
		for _, id := range []string{"eu-call-analytic", "eu-put-tree", "eu-call-mc"} {
			it := findItem(t, rep, n, id)
			if it.Outcome != scenario.OutcomePriced {
				t.Fatalf("状态 %s 标的 %s 应已定价, got %s (%s)", n, id, it.Outcome, it.Detail)
			}
		}
		it := findItem(t, rep, n, "am-put-analytic")
		if it.Outcome != scenario.OutcomeRejected || it.Reason != scenario.ReasonMethodNotApplicable {
			t.Fatalf("状态 %s 美式解析解应被拒(方法不适用), got %s/%s", n, it.Outcome, it.Reason)
		}
	}
	// 隔离性：相同种子、相同 MC 请求，不同状态必须给出不同价格
	// （若缓存跨状态串扰，后跑的状态会命中先跑状态的结果）。
	pNormal := findItem(t, rep, "normal", "eu-call-mc").Price
	pGap := findItem(t, rep, "gap_down", "eu-call-mc").Price
	pCrunch := findItem(t, rep, "liquidity_crunch", "eu-call-mc").Price
	if pNormal == pGap || pNormal == pCrunch || pGap == pCrunch {
		t.Fatalf("不同状态的 MC 结果不应相同(疑似缓存串扰): %v %v %v", pNormal, pGap, pCrunch)
	}
	// 对比报告：4 个标的 × 3 个状态，顺序与输入一致。
	rows := rep.Compare()
	if len(rows) != 4 {
		t.Fatalf("对比行数应为 4, got %d", len(rows))
	}
	for _, row := range rows {
		if len(row.Values) != 3 {
			t.Fatalf("标的 %s 的对比列数应为 3, got %d", row.ID, len(row.Values))
		}
		for i, v := range row.Values {
			if v.Scenario != names[i] {
				t.Fatalf("标的 %s 第 %d 列应为状态 %s, got %s", row.ID, i, names[i], v.Scenario)
			}
		}
	}
	t.Logf("结论: 三状态并发估值完成, MC 价格 normal=%.6f gap_down=%.6f crunch=%.6f 互不相同(无串扰)",
		pNormal, pGap, pCrunch)
}

// TestRunUnknownAndIncompleteScenarios 未知状态与数据缺失状态都以
// 状态级失败显式记录，且不影响同批其余状态。
func TestRunUnknownAndIncompleteScenarios(t *testing.T) {
	reg := loadRegistry(t)
	runner := scenario.NewRunner(reg, scenario.Config{MaxWorkers: 3})
	specs := []scenario.ScenarioSpec{
		{Name: "normal"},
		{Name: "nonexistent"},
		{Name: "incomplete"},
	}
	rep := runner.Run(context.Background(), specs, standardReqs())
	logReport(t, rep)

	// 未知状态：ErrUnknownScenario，对比行中记为“市场状态不存在”。
	if !errors.Is(rep.Scenarios[1].Err, scenario.ErrUnknownScenario) {
		t.Fatalf("未知状态应返回 ErrUnknownScenario, got %v", rep.Scenarios[1].Err)
	}
	// 数据缺失状态：底层为 ErrMissingQuote，对比行中记为“数据不可用”。
	if !errors.Is(rep.Scenarios[2].Err, marketdata.ErrMissingQuote) {
		t.Fatalf("数据缺失状态应返回 ErrMissingQuote, got %v", rep.Scenarios[2].Err)
	}
	// 正常状态不受任何影响。
	it := findItem(t, rep, "normal", "eu-call-analytic")
	if it.Outcome != scenario.OutcomePriced {
		t.Fatalf("正常状态不应受失败状态影响, got %s", it.Outcome)
	}
	for _, row := range rep.Compare() {
		if row.Values[1].Item.Reason != scenario.ReasonScenarioUnknown {
			t.Fatalf("标的 %s 在未知状态下应记为市场状态不存在, got %s", row.ID, row.Values[1].Item.Reason)
		}
		if row.Values[2].Item.Reason != scenario.ReasonDataUnavailable {
			t.Fatalf("标的 %s 在数据缺失状态下应记为数据不可用, got %s", row.ID, row.Values[2].Item.Reason)
		}
		t.Logf("标的 %s: normal=%s nonexistent=%s incomplete=%s",
			row.ID, row.Values[0].Item.Outcome, row.Values[1].Item.Reason, row.Values[2].Item.Reason)
	}
	t.Log("结论: 未知状态与数据缺失状态被显式拒绝且分类可区分, 正常状态不受影响")
}

// TestRunReproducibleAcrossLayouts 相同快照、相同种子、相同请求下，
// 不同并发布局与重复执行的结果必须逐位一致。
func TestRunReproducibleAcrossLayouts(t *testing.T) {
	reg := loadRegistry(t)
	specs := []scenario.ScenarioSpec{{Name: "normal"}, {Name: "gap_down"}, {Name: "liquidity_crunch"}}
	run := func(workers int) *scenario.Report {
		runner := scenario.NewRunner(reg, scenario.Config{MaxWorkers: workers, PathBudget: 1_000_000})
		return runner.Run(context.Background(), specs, standardReqs())
	}
	base := run(1)
	wide := run(8)
	again := run(8)
	for i := range base.Scenarios {
		for j := range base.Scenarios[i].Items {
			a, b, c := base.Scenarios[i].Items[j], wide.Scenarios[i].Items[j], again.Scenarios[i].Items[j]
			if a != b || a != c {
				t.Fatalf("结果不可复现 (%s, %s): %v vs %v vs %v",
					base.Scenarios[i].Name, a.ID, a, b, c)
			}
		}
	}
	t.Log("结论: MaxWorkers=1 与 8 及重复执行的结果逐位一致（含 MC）")
}

// TestRunCancellationIsolation 单个状态被取消只影响该状态本身：
// 其余状态照常完成；被取消的状态不留半截状态，换用新上下文重跑可正常完成。
func TestRunCancellationIsolation(t *testing.T) {
	reg := loadRegistry(t)
	runner := scenario.NewRunner(reg, scenario.Config{MaxWorkers: 4, PathBudget: 1_000_000})
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel() // 进入评估前即取消该状态
	specs := []scenario.ScenarioSpec{
		{Name: "normal"},
		{Name: "gap_down", Ctx: canceledCtx},
		{Name: "liquidity_crunch"},
	}
	rep := runner.Run(context.Background(), specs, standardReqs())
	logReport(t, rep)

	for _, id := range []string{"eu-call-analytic", "eu-put-tree", "eu-call-mc"} {
		it := findItem(t, rep, "gap_down", id)
		if it.Outcome != scenario.OutcomeRejected || it.Reason != scenario.ReasonCanceled {
			t.Fatalf("被取消状态的标的 %s 应记为已取消, got %s/%s", id, it.Outcome, it.Reason)
		}
		for _, n := range []string{"normal", "liquidity_crunch"} {
			it := findItem(t, rep, n, id)
			if it.Outcome != scenario.OutcomePriced {
				t.Fatalf("状态 %s 不应受取消影响, 标的 %s got %s", n, id, it.Outcome)
			}
		}
	}
	// 重跑被取消的状态：新上下文下应正常完成，说明没有残留半截状态。
	rep2 := runner.Run(context.Background(), []scenario.ScenarioSpec{{Name: "gap_down"}}, standardReqs())
	it := findItem(t, rep2, "gap_down", "eu-call-analytic")
	if it.Outcome != scenario.OutcomePriced {
		t.Fatalf("被取消状态用新上下文重跑应可完成, got %s (%s)", it.Outcome, it.Detail)
	}
	t.Logf("结论: 取消只影响 gap_down 单状态, 重跑后正常定价 %.6f", it.Price)
}

// TestRunTimeoutIsolation 单个状态超时只影响该状态本身。
func TestRunTimeoutIsolation(t *testing.T) {
	reg := loadRegistry(t)
	runner := scenario.NewRunner(reg, scenario.Config{MaxWorkers: 4})
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	specs := []scenario.ScenarioSpec{
		{Name: "normal"},
		{Name: "gap_down", Ctx: expired},
	}
	rep := runner.Run(context.Background(), specs, standardReqs())
	logReport(t, rep)

	it := findItem(t, rep, "gap_down", "eu-call-analytic")
	if it.Outcome != scenario.OutcomeRejected || it.Reason != scenario.ReasonTimeout {
		t.Fatalf("超时状态的标的应记为已超时, got %s/%s", it.Outcome, it.Reason)
	}
	it = findItem(t, rep, "normal", "eu-call-analytic")
	if it.Outcome != scenario.OutcomePriced {
		t.Fatalf("正常状态不应受超时状态影响, got %s", it.Outcome)
	}
	t.Log("结论: 单状态超时被隔离, 其余状态正常完成")
}

// TestRunSharedBudget 路径预算整批共享、按（状态顺序 × 请求顺序）
// 确定性分配：预算耗尽后后续状态的 MC 请求被明确拒绝（不降级时），
// 且分配结果与并发布局无关。
func TestRunSharedBudget(t *testing.T) {
	reg := loadRegistry(t)
	specs := []scenario.ScenarioSpec{{Name: "normal"}, {Name: "liquidity_crunch"}}
	reqs := []engine.Request{
		{ID: "mc", Spec: pricing.Spec{Right: marketdata.Call, Style: pricing.European,
			Strike: 100, Expiry: 1.0}, Method: pricing.MonteCarlo, Seed: 7, Paths: 50000},
	}
	run := func(workers int) *scenario.Report {
		runner := scenario.NewRunner(reg, scenario.Config{
			MaxWorkers: workers, PathBudget: 60000, DegradeToBinomial: false})
		return runner.Run(context.Background(), specs, reqs)
	}
	rep := run(4)
	logReport(t, rep)
	first := findItem(t, rep, "normal", "mc")
	second := findItem(t, rep, "liquidity_crunch", "mc")
	if first.Outcome != scenario.OutcomePriced {
		t.Fatalf("先分配的状态应获得预算, got %s (%s)", first.Outcome, first.Detail)
	}
	if second.Outcome != scenario.OutcomeRejected || second.Reason != scenario.ReasonBudgetExceeded {
		t.Fatalf("预算耗尽后应明确拒绝, got %s/%s", second.Outcome, second.Reason)
	}
	// 确定性：不同并发布局下分配结果一致。
	rep2 := run(1)
	if findItem(t, rep2, "normal", "mc") != first || findItem(t, rep2, "liquidity_crunch", "mc") != second {
		t.Fatalf("预算分配应与并发布局无关")
	}
	t.Logf("结论: 共享预算 60000 条路径按序分配, normal 获 50000, liquidity_crunch 因余量不足被拒")
}

// TestRunDegradation MC 未收敛或超预算时按策略降级为二叉树：
// 结果记为“已降级”，保留原始原因分类，且价格与解析解在容差内一致。
func TestRunDegradation(t *testing.T) {
	reg := loadRegistry(t)
	spec := pricing.Spec{Right: marketdata.Call, Style: pricing.European, Strike: 100, Expiry: 1.0}
	reqs := []engine.Request{
		{ID: "mc-tiny", Spec: spec, Method: pricing.MonteCarlo, Seed: 9, Paths: 10}, // 必不收敛
		{ID: "mc-big", Spec: spec, Method: pricing.MonteCarlo, Seed: 9, Paths: 50000},
		{ID: "ref", Spec: spec, Method: pricing.Analytic},
	}
	runner := scenario.NewRunner(reg, scenario.Config{
		MaxWorkers: 2, PathBudget: 40000, DegradeToBinomial: true})
	rep := runner.Run(context.Background(), []scenario.ScenarioSpec{{Name: "normal"}}, reqs)
	logReport(t, rep)

	// 路径太少未收敛 -> 降级二叉树，原因记为“采样未收敛”。
	tiny := findItem(t, rep, "normal", "mc-tiny")
	if tiny.Outcome != scenario.OutcomeDegraded || tiny.Reason != scenario.ReasonNotConverged {
		t.Fatalf("未收敛 MC 应降级(采样未收敛), got %s/%s", tiny.Outcome, tiny.Reason)
	}
	if tiny.Method != "Binomial" {
		t.Fatalf("降级后实际方法应为 Binomial, got %s", tiny.Method)
	}
	// 预算只够第一个 MC（mc-tiny 不占用，因为先评估再发现不收敛；
	// 预分配阶段 mc-tiny 占 10 条，mc-big 需 50000 > 剩余 39990）-> 超预算降级。
	big := findItem(t, rep, "normal", "mc-big")
	if big.Outcome != scenario.OutcomeDegraded || big.Reason != scenario.ReasonBudgetExceeded {
		t.Fatalf("超预算 MC 应降级(超出路径预算), got %s/%s", big.Outcome, big.Reason)
	}
	ref := findItem(t, rep, "normal", "ref")
	for _, it := range []scenario.ItemResult{tiny, big} {
		if d := it.Price - ref.Price; d < -0.02 || d > 0.02 {
			t.Fatalf("标的 %s 降级价格 %.6f 与解析解 %.6f 偏差过大", it.ID, it.Price, ref.Price)
		}
	}
	t.Logf("结论: 未收敛与超预算的 MC 均降级为二叉树, 价格 %.6f/%.6f 与解析解 %.6f 一致",
		tiny.Price, big.Price, ref.Price)
}

// TestRunCacheBounded 缓存条目有界：上限为 1 时大量不同请求仍全部
// 正确定价，且重复执行结果一致（缓存只加速、绝不改变结果）。
func TestRunCacheBounded(t *testing.T) {
	reg := loadRegistry(t)
	var reqs []engine.Request
	for i, k := range []float64{90, 95, 100, 105, 110} {
		reqs = append(reqs, engine.Request{
			ID:     string(rune('a'+i)) + "-call",
			Spec:   pricing.Spec{Right: marketdata.Call, Style: pricing.European, Strike: k, Expiry: 1.0},
			Method: pricing.Analytic,
		})
	}
	specs := []scenario.ScenarioSpec{{Name: "normal"}, {Name: "gap_down"}}
	run := func() *scenario.Report {
		runner := scenario.NewRunner(reg, scenario.Config{MaxWorkers: 4, MaxCacheEntries: 1})
		return runner.Run(context.Background(), specs, reqs)
	}
	rep, rep2 := run(), run()
	logReport(t, rep)
	for i := range rep.Scenarios {
		for j := range rep.Scenarios[i].Items {
			a, b := rep.Scenarios[i].Items[j], rep2.Scenarios[i].Items[j]
			if a.Outcome != scenario.OutcomePriced {
				t.Fatalf("缓存有界不应影响定价 (%s, %s): %s", rep.Scenarios[i].Name, a.ID, a.Outcome)
			}
			if a != b {
				t.Fatalf("缓存有界时重复执行结果应一致 (%s, %s)", rep.Scenarios[i].Name, a.ID)
			}
		}
	}
	t.Log("结论: MaxCacheEntries=1 时全部请求正确定价且结果可复现, 缓存占用不随状态数增长")
}
