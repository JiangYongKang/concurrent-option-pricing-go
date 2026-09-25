package market

import (
	"errors"
	"testing"
)

func loadNormal(t *testing.T) (*Snapshot, *DiscountCurve, *VolSurface) {
	t.Helper()
	snap, err := LoadSnapshot("../testdata/snapshot_normal.json")
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	curve, err := BuildDiscountCurve(snap.RateQuotes)
	if err != nil {
		t.Fatalf("build curve: %v", err)
	}
	surf, err := Calibrate(snap, curve)
	if err != nil {
		t.Fatalf("calibrate: %v", err)
	}
	return snap, curve, surf
}

// TestCalibrateRecoversKnownVols 快照中的报价由已知波动率
// (90->0.22, 100->0.20, 110->0.19, r=2%) 生成，校准应还原它们。
func TestCalibrateRecoversKnownVols(t *testing.T) {
	_, _, surf := loadNormal(t)
	want := map[float64]float64{90: 0.22, 100: 0.20, 110: 0.19}
	for _, expiry := range []float64{0.25, 0.5, 1.0} {
		for strike, w := range want {
			got := surf.Vol(expiry, strike)
			t.Logf("expiry=%v strike=%v wantVol=%.4f gotVol=%.6f", expiry, strike, w, got)
			if diff := got - w; diff < -1e-4 || diff > 1e-4 {
				t.Errorf("expiry=%v strike=%v: vol %.6f deviates from known %.4f", expiry, strike, got, w)
			}
		}
	}
}

// TestCalibrationDeterministic 相同输入重复校准必须得到完全相同的结果。
func TestCalibrationDeterministic(t *testing.T) {
	snap, curve, surf1 := loadNormal(t)
	surf2, err := Calibrate(snap, curve)
	if err != nil {
		t.Fatalf("second calibrate: %v", err)
	}
	for _, expiry := range surf1.Expiries() {
		for _, strike := range surf1.Strikes() {
			v1, v2 := surf1.Vol(expiry, strike), surf2.Vol(expiry, strike)
			if v1 != v2 {
				t.Fatalf("non-deterministic calibration at (%v,%v): %.12f vs %.12f", expiry, strike, v1, v2)
			}
		}
	}
	t.Logf("两次校准在 %d 个网格点上逐位一致", len(surf1.Expiries())*len(surf1.Strikes()))
}

// TestValidateFailures 各类非法输入必须被可区分地拒绝。
func TestValidateFailures(t *testing.T) {
	base := Snapshot{
		Name: "t", Spot: 100,
		RateQuotes: []RateQuote{{Tenor: 1, DiscountFactor: 0.98}},
		VolQuotes:  []VolQuote{{Expiry: 1, Strike: 100, Price: 8, IsCall: true}},
	}
	cases := []struct {
		name string
		mut  func(*Snapshot)
		want error
	}{
		{"spot 非正", func(s *Snapshot) { s.Spot = 0 }, ErrSnapshotInvalid},
		{"利率报价缺失", func(s *Snapshot) { s.RateQuotes = nil }, ErrQuoteMissing},
		{"期权报价缺失", func(s *Snapshot) { s.VolQuotes = nil }, ErrQuoteMissing},
		{"利率期限重复", func(s *Snapshot) {
			s.RateQuotes = append(s.RateQuotes, RateQuote{Tenor: 1, DiscountFactor: 0.97})
		}, ErrQuoteDuplicate},
		{"期权报价重复", func(s *Snapshot) {
			s.VolQuotes = append(s.VolQuotes, VolQuote{Expiry: 1, Strike: 100, Price: 8.1, IsCall: true})
		}, ErrQuoteDuplicate},
		{"贴现因子越界", func(s *Snapshot) {
			s.RateQuotes[0].DiscountFactor = 1.5
		}, ErrSnapshotInvalid},
		{"期权价为负", func(s *Snapshot) { s.VolQuotes[0].Price = -1 }, ErrSnapshotInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			// 深拷贝切片，避免用例间互相污染。
			s.RateQuotes = append([]RateQuote(nil), base.RateQuotes...)
			s.VolQuotes = append([]VolQuote(nil), base.VolQuotes...)
			tc.mut(&s)
			err := s.Validate()
			t.Logf("case=%q err=%v", tc.name, err)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

// TestTenorInversion 贴现因子随期限上升必须被拒绝为期限倒挂。
func TestTenorInversion(t *testing.T) {
	quotes := []RateQuote{
		{Tenor: 0.5, DiscountFactor: 0.99},
		{Tenor: 1.0, DiscountFactor: 0.995}, // 倒挂：更长期限 DF 更高
	}
	_, err := BuildDiscountCurve(quotes)
	t.Logf("inverted quotes err=%v", err)
	if !errors.Is(err, ErrTenorInversion) {
		t.Fatalf("want ErrTenorInversion, got %v", err)
	}
}

// TestUnsolvableVol 报价越出无套利边界时必须报 ErrVolUnsolvable，
// 不得静默兜底为某个默认波动率。
func TestUnsolvableVol(t *testing.T) {
	mk := func(price float64) *Snapshot {
		return &Snapshot{
			Name: "bad", Spot: 100,
			RateQuotes: []RateQuote{{Tenor: 1, DiscountFactor: 0.98}},
			VolQuotes: []VolQuote{
				{Expiry: 1, Strike: 100, Price: price, IsCall: true},
			},
		}
	}
	curve, err := BuildDiscountCurve(mk(0).RateQuotes)
	if err != nil {
		t.Fatal(err)
	}
	// 看涨价格高于标的价格：不可解。
	snap := mk(105)
	if _, err := Calibrate(snap, curve); !errors.Is(err, ErrVolUnsolvable) {
		t.Fatalf("price above spot: want ErrVolUnsolvable, got %v", err)
	} else {
		t.Logf("price=105 > spot=100 -> %v", err)
	}
	// 看涨价格低于内在价值 max(0, S-K*DF)=2：不可解。
	snap = mk(1.0)
	if _, err := Calibrate(snap, curve); !errors.Is(err, ErrVolUnsolvable) {
		t.Fatalf("price below intrinsic: want ErrVolUnsolvable, got %v", err)
	} else {
		t.Logf("price=1.0 < intrinsic=2.0 -> %v", err)
	}
}

// TestMissingGridCell 曲面网格缺格必须报 ErrQuoteMissing 而非猜测。
func TestMissingGridCell(t *testing.T) {
	snap, curve, _ := loadNormal(t)
	// 删掉一个 (expiry, strike) 格子，使网格不再完整。
	snap.VolQuotes = snap.VolQuotes[:len(snap.VolQuotes)-1]
	_, err := Calibrate(snap, curve)
	t.Logf("dropped last quote -> %v", err)
	if !errors.Is(err, ErrQuoteMissing) {
		t.Fatalf("want ErrQuoteMissing, got %v", err)
	}
}

// TestDiscountCurveInterp 验证贴现曲线的插值与零息利率。
func TestDiscountCurveInterp(t *testing.T) {
	_, curve, _ := loadNormal(t)
	// 快照利率为恒定 2%：DF(1) 应约等于 e^{-0.02}。
	got := curve.Discount(1.0)
	t.Logf("DF(1)=%.8f want≈%.8f", got, 0.980199)
	if d := got - 0.980199; d < -1e-6 || d > 1e-6 {
		t.Fatalf("DF(1)=%.8f off from quote", got)
	}
	r := curve.ZeroRate(0.75)
	t.Logf("zeroRate(0.75)=%.6f want≈0.02", r)
	if r < 0.019 || r > 0.021 {
		t.Fatalf("zeroRate(0.75)=%.6f, expected ≈0.02", r)
	}
}

// TestStressSnapshot 高波动/高利率市场状态下校准应还原
// 生成报价所用的已知波动率 (90->0.40, 100->0.35, 110->0.32, r=5%)。
func TestStressSnapshot(t *testing.T) {
	snap, err := LoadSnapshot("../testdata/snapshot_stress.json")
	if err != nil {
		t.Fatalf("load stress snapshot: %v", err)
	}
	curve, err := BuildDiscountCurve(snap.RateQuotes)
	if err != nil {
		t.Fatalf("build curve: %v", err)
	}
	surf, err := Calibrate(snap, curve)
	if err != nil {
		t.Fatalf("calibrate stress: %v", err)
	}
	want := map[float64]float64{90: 0.40, 100: 0.35, 110: 0.32}
	for _, expiry := range []float64{0.25, 0.5, 1.0} {
		for strike, w := range want {
			got := surf.Vol(expiry, strike)
			t.Logf("stress expiry=%v strike=%v wantVol=%.4f gotVol=%.6f", expiry, strike, w, got)
			if d := got - w; d < -1e-4 || d > 1e-4 {
				t.Errorf("expiry=%v strike=%v: vol %.6f deviates from known %.4f", expiry, strike, got, w)
			}
		}
	}
	// 压力快照利率 5%：零息利率应显著高于正常快照的 2%。
	r := curve.ZeroRate(1.0)
	t.Logf("stress zeroRate(1)=%.4f (normal snapshot ≈0.02)", r)
	if r < 0.04 || r > 0.06 {
		t.Fatalf("stress zeroRate(1)=%.4f, expected ≈0.05", r)
	}
}
