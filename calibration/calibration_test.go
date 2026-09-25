package calibration_test

import (
	"errors"
	"testing"

	"github.com/highcumontoa/concurrent-option-pricing-go/calibration"
	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
)

func loadSnap(t *testing.T, name string) *marketdata.Snapshot {
	t.Helper()
	snap, err := marketdata.LoadSnapshot("../testdata/snapshots/" + name)
	if err != nil {
		t.Fatalf("加载快照 %s 失败: %v", name, err)
	}
	return snap
}

func buildAll(t *testing.T, snap *marketdata.Snapshot) (*calibration.DiscountCurve, *calibration.VolSurface) {
	t.Helper()
	curve, err := calibration.BuildDiscountCurve(snap)
	if err != nil {
		t.Fatalf("构建贴现曲线失败: %v", err)
	}
	vs, err := calibration.BuildVolSurface(snap, curve)
	if err != nil {
		t.Fatalf("构建波动率曲面失败: %v", err)
	}
	return curve, vs
}

func TestDiscountCurve(t *testing.T) {
	snap := loadSnap(t, "normal.json")
	curve, err := calibration.BuildDiscountCurve(snap)
	if err != nil {
		t.Fatalf("构建曲线失败: %v", err)
	}
	t.Logf("输入利率节点: %v", snap.Rates)
	// 节点处零息利率应与输入一致。
	for _, q := range snap.Rates {
		got := curve.ZeroRate(q.Tenor)
		t.Logf("tenor=%v 输入rate=%v 曲线rate=%v", q.Tenor, q.Rate, got)
		if got != q.Rate {
			t.Fatalf("tenor=%v 处利率不一致: 输入 %v, 曲线 %v", q.Tenor, q.Rate, got)
		}
	}
	// 贴现因子应随期限单调递减。
	prev := 1.0
	for _, tt := range []float64{0.1, 0.5, 1.0, 1.5, 2.0} {
		d := curve.Discount(tt)
		t.Logf("t=%v discount=%v", tt, d)
		if d <= 0 || d > prev {
			t.Fatalf("贴现因子非单调递减: t=%v d=%v prev=%v", tt, d, prev)
		}
		prev = d
	}
}

func TestVolSurfaceRecoversInputVols(t *testing.T) {
	snap := loadSnap(t, "normal.json")
	_, vs := buildAll(t, snap)
	// 曲面在报价节点处应还原生成报价时使用的隐含波动率。
	// 生成规则（见 testdata 生成器）：vol = base - 0.10*m + 0.40*m^2, m = K/S - 1。
	for _, q := range snap.Options {
		m := q.Strike/snap.Spot - 1
		base := 0.22 - 0.01*(q.Expiry-1)
		want := base - 0.10*m + 0.40*m*m
		got := vs.Vol(q.Expiry, q.Strike)
		if diff := got - want; diff < -1e-6 || diff > 1e-6 {
			t.Fatalf("节点 (T=%v, K=%v) 波动率还原失败: want %v, got %v",
				q.Expiry, q.Strike, want, got)
		}
	}
	t.Logf("全部 %d 个报价节点的隐含波动率均在 1e-6 内还原", len(snap.Options))
}

func TestImpliedVolUnsolvable(t *testing.T) {
	snap := loadSnap(t, "unsolvable_vol.json")
	curve, err := calibration.BuildDiscountCurve(snap)
	if err != nil {
		t.Fatalf("构建曲线失败: %v", err)
	}
	_, err = calibration.BuildVolSurface(snap, curve)
	t.Logf("输入: T=1.0 K=100 Call price=0.5 (无套利下界约 3.44) 判定依据: err=%v", err)
	if !errors.Is(err, calibration.ErrVolNotSolvable) {
		t.Fatalf("期望 ErrVolNotSolvable, 实际 %v", err)
	}
}

func TestCalibrationRepeatable(t *testing.T) {
	snap := loadSnap(t, "normal.json")
	c1, v1 := buildAll(t, snap)
	c2, v2 := buildAll(t, snap)
	// 相同输入重复校准，曲线与曲面必须逐点一致。
	for _, tt := range []float64{0.25, 0.7, 1.3, 2.0} {
		d1, d2 := c1.Discount(tt), c2.Discount(tt)
		if d1 != d2 {
			t.Fatalf("重复校准曲线不一致: t=%v %v vs %v", tt, d1, d2)
		}
	}
	for _, T := range []float64{0.25, 0.75, 1.5, 2.0} {
		for _, K := range []float64{85, 100, 115} {
			a, b := v1.Vol(T, K), v2.Vol(T, K)
			if a != b {
				t.Fatalf("重复校准曲面不一致: (T=%v,K=%v) %v vs %v", T, K, a, b)
			}
		}
	}
	t.Log("重复校准：曲线与曲面逐点一致")
}
