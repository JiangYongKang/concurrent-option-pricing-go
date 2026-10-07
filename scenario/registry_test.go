package scenario_test

import (
	"errors"
	"testing"

	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
	"github.com/highcumontoa/concurrent-option-pricing-go/scenario"
)

const scenarioDir = "../testdata/scenarios"

// TestRegistryList 列出全部可用市场状态：应包含基准、两个极端状态
// 以及一个数据不完整的状态（不完整的状态可见但加载时被拒）。
func TestRegistryList(t *testing.T) {
	reg, err := scenario.LoadRegistry(scenarioDir)
	if err != nil {
		t.Fatalf("加载注册表失败: %v", err)
	}
	names := reg.List()
	t.Logf("可用市场状态: %v", names)
	want := map[string]bool{
		"normal": true, "gap_down": true,
		"liquidity_crunch": true, "incomplete": true,
	}
	if len(names) != len(want) {
		t.Fatalf("状态数量不符: got %v, want %v", names, want)
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("出现未预期状态 %q", n)
		}
	}
	// 列表必须有序（确定性输出）。
	for i := 1; i < len(names); i++ {
		if names[i] < names[i-1] {
			t.Fatalf("状态列表未按字典序: %v", names)
		}
	}
	t.Logf("结论: 列表完整且有序, 共 %d 个状态", len(names))
}

// TestRegistryLoadByName 按名字加载正常状态，应得到校验通过的快照。
func TestRegistryLoadByName(t *testing.T) {
	reg, err := scenario.LoadRegistry(scenarioDir)
	if err != nil {
		t.Fatalf("加载注册表失败: %v", err)
	}
	for _, n := range []string{"normal", "gap_down", "liquidity_crunch"} {
		snap, err := reg.Load(n)
		if err != nil {
			t.Fatalf("状态 %s 加载失败: %v", n, err)
		}
		t.Logf("状态 %s: id=%s asOf=%s spot=%v 利率报价=%d 期权报价=%d",
			n, snap.ID, snap.AsOf, snap.Spot, len(snap.Rates), len(snap.Options))
	}
	t.Log("结论: 三个可用状态均可按名加载并通过校验")
}

// TestRegistryUnknownRejected 指定不存在的状态必须被明确拒绝
// （ErrUnknownScenario），且错误信息中列出可用状态，绝不静默回退。
func TestRegistryUnknownRejected(t *testing.T) {
	reg, err := scenario.LoadRegistry(scenarioDir)
	if err != nil {
		t.Fatalf("加载注册表失败: %v", err)
	}
	_, err = reg.Load("nonexistent")
	if !errors.Is(err, scenario.ErrUnknownScenario) {
		t.Fatalf("未知状态应返回 ErrUnknownScenario, got %v", err)
	}
	t.Logf("未知状态拒绝信息: %v", err)
	t.Log("结论: 未知状态被显式拒绝, 未回退任何默认状态")
}

// TestRegistryIncompleteDataRejected 数据不完整的状态（利率报价缺失）
// 会出现在列表里，但加载时必须以可区分的数据校验错误被拒绝，
// 不允许用补齐值或默认值蒙混。
func TestRegistryIncompleteDataRejected(t *testing.T) {
	reg, err := scenario.LoadRegistry(scenarioDir)
	if err != nil {
		t.Fatalf("加载注册表失败: %v", err)
	}
	_, err = reg.Load("incomplete")
	if !errors.Is(err, marketdata.ErrMissingQuote) {
		t.Fatalf("数据缺失状态应返回 ErrMissingQuote, got %v", err)
	}
	t.Logf("数据缺失拒绝信息: %v", err)
	t.Log("结论: 数据不完整的状态被显式拒绝（数据不可用）, 未做任何补齐")
}
