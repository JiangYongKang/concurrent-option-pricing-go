package scenario_test

import (
	"errors"
	"testing"

	"github.com/highcumontoa/concurrent-option-pricing-go/scenario"
)

func TestRegistryListAndGet(t *testing.T) {
	reg := scenario.DefaultRegistry()
	got := reg.Names()
	t.Logf("内置可用市场状态: %v", got)
	want := []string{"normal", "liquidity-crunch", "price-gap"}
	if len(got) != len(want) {
		t.Fatalf("内置状态数量不符: got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("内置状态/顺序不符: got=%v want=%v", got, want)
		}
	}
	for _, name := range want {
		s, err := reg.Get(name)
		if err != nil {
			t.Fatalf("获取状态 %s 失败: %v", name, err)
		}
		ls, err := s.Load()
		if err != nil {
			t.Fatalf("状态 %s 加载/校准失败: %v", name, err)
		}
		t.Logf("状态 %-18s 加载成功（描述: %s）", name, s.Description)
		_ = ls
	}
}

func TestUnknownStateRejected(t *testing.T) {
	reg := scenario.DefaultRegistry()
	if _, err := reg.Get("flash-crash-1987"); !errors.Is(err, scenario.ErrUnknownState) {
		t.Fatalf("未知状态应报 ErrUnknownState, 实际 %v", err)
	} else {
		t.Logf("未知状态被明确拒绝，判定依据: %v", err)
	}
}

func TestRegisterDuplicateRejected(t *testing.T) {
	reg := scenario.DefaultRegistry()
	err := reg.RegisterJSON("normal", "重复", []byte(`{}`))
	if !errors.Is(err, scenario.ErrStateExists) {
		t.Fatalf("重名注册应报 ErrStateExists, 实际 %v", err)
	}
	t.Logf("重名注册被拒: %v", err)
}

// TestBrokenStateExposed 验证状态数据本身有问题时绝不默认补齐，
// 而是暴露成可区分的数据失败（不同坏法对应不同哨兵错误）。
func TestBrokenStateExposed(t *testing.T) {
	reg := scenario.DefaultRegistry()
	// 1) 空数据。
	if err := reg.RegisterJSON("empty-data", "空", nil); err != nil {
		t.Fatal(err)
	}
	// 2) 缺失利率报价（沿用仓库中确定性的坏快照形态）。
	missingRates := []byte(`{
	  "id":"missing-rates","asOf":"2026-10-07","spot":100,
	  "rates":[],
	  "options":[{"expiry":1,"strike":100,"right":"Call","price":10}]
	}`)
	if err := reg.RegisterJSON("missing-rates", "缺利率", missingRates); err != nil {
		t.Fatal(err)
	}
	// 3) 不可解波动率：报价低于无套利下界。
	unsolvable := []byte(`{
	  "id":"bad-vol","asOf":"2026-10-07","spot":100,
	  "rates":[{"tenor":1,"rate":0.03}],
	  "options":[{"expiry":1,"strike":100,"right":"Call","price":0.5}]
	}`)
	if err := reg.RegisterJSON("unsolvable-vol", "坏波动率", unsolvable); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		wantData  bool
		wantCalib bool
	}{
		{"empty-data", true, false},
		{"missing-rates", true, false},
		{"unsolvable-vol", false, true},
	}
	for _, c := range cases {
		s, err := reg.Get(c.name)
		if err != nil {
			t.Fatalf("获取 %s 失败: %v", c.name, err)
		}
		_, lerr := s.Load()
		if lerr == nil {
			t.Fatalf("状态 %s 应当加载失败", c.name)
		}
		isData := errors.Is(lerr, scenario.ErrStateDataUnavailable)
		isCalib := errors.Is(lerr, scenario.ErrStateNotCalibratable)
		if isData != c.wantData || isCalib != c.wantCalib {
			t.Fatalf("状态 %s 失败分类不符: data=%v calib=%v (%v)",
				c.name, isData, isCalib, lerr)
		}
		t.Logf("坏状态 %-16s 结论: data-unavailable=%v not-calibratable=%v；依据: %v",
			c.name, isData, isCalib, lerr)
	}
}
