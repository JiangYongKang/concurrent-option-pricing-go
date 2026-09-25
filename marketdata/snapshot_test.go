package marketdata_test

import (
	"errors"
	"testing"

	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
)

func loadNormal(t *testing.T) *marketdata.Snapshot {
	t.Helper()
	snap, err := marketdata.LoadSnapshot("../testdata/snapshots/normal.json")
	if err != nil {
		t.Fatalf("加载正常快照失败: %v", err)
	}
	return snap
}

func TestLoadNormalSnapshot(t *testing.T) {
	snap := loadNormal(t)
	t.Logf("快照 id=%s asOf=%s spot=%v 利率条数=%d 期权条数=%d",
		snap.ID, snap.AsOf, snap.Spot, len(snap.Rates), len(snap.Options))
	if snap.Spot != 100 {
		t.Fatalf("spot 应为 100, got %v", snap.Spot)
	}
}

func TestValidateFailureReasons(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		wantErr error
	}{
		{"缺失利率报价", "../testdata/snapshots/missing_rates.json", marketdata.ErrMissingQuote},
		{"重复期权报价", "../testdata/snapshots/duplicate_option.json", marketdata.ErrDuplicateQuote},
		{"利率期限倒挂", "../testdata/snapshots/inverted_tenors.json", marketdata.ErrInvertedMaturity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := marketdata.LoadSnapshot(tc.file)
			t.Logf("输入文件=%s 判定依据: err=%v", tc.file, err)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("期望错误 %v, 实际 %v", tc.wantErr, err)
			}
		})
	}
}

func TestValidateInvalidSpot(t *testing.T) {
	snap := loadNormal(t)
	snap.Spot = 0
	err := snap.Validate()
	t.Logf("输入 spot=%v 判定依据: err=%v", snap.Spot, err)
	if !errors.Is(err, marketdata.ErrInvalidValue) {
		t.Fatalf("期望 ErrInvalidValue, 实际 %v", err)
	}
}

func TestValidateDeterministic(t *testing.T) {
	snap := loadNormal(t)
	if err := snap.Validate(); err != nil {
		t.Fatalf("正常快照应通过校验: %v", err)
	}
	// 重复校验结果一致。
	for i := 0; i < 3; i++ {
		if err := snap.Validate(); err != nil {
			t.Fatalf("第 %d 次校验结果不一致: %v", i, err)
		}
	}
	t.Log("重复校验 3 次结果一致")
}
