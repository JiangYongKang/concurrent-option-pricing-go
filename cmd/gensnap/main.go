// Command gensnap 重新生成 testdata/snapshots 下的本地行情快照，
// 保证测试数据可本机复现：go run ./cmd/gensnap
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
	"github.com/highcumontoa/concurrent-option-pricing-go/pricing"
)

func main() {
	spot := 100.0
	rates := []marketdata.RateQuote{
		{Tenor: 0.25, Rate: 0.030},
		{Tenor: 0.5, Rate: 0.032},
		{Tenor: 1.0, Rate: 0.035},
		{Tenor: 2.0, Rate: 0.038},
	}
	// 微笑：vol = base + skew*(K/S-1) + smile*(K/S-1)^2，随期限略降。
	volOf := func(T, K float64) float64 {
		m := K/spot - 1
		base := 0.22 - 0.01*(T-1)
		return base - 0.10*m + 0.40*m*m
	}
	var opts []marketdata.OptionQuote
	for _, T := range []float64{0.25, 0.5, 1.0, 2.0} {
		r := 0.030 + (0.038-0.030)*(T-0.25)/(2.0-0.25) // 与曲线大致一致即可，校准用曲线自身利率
		_ = r
		for _, K := range []float64{80, 90, 100, 110, 120} {
			v := volOf(T, K)
			env := pricing.MarketEnv{Spot: spot, Rate: rateAt(T), Vol: v}
			spec := pricing.Spec{Right: marketdata.Call, Style: pricing.European, Strike: K, Expiry: T}
			p, err := pricing.BlackScholes(spec, env)
			if err != nil {
				panic(err)
			}
			opts = append(opts, marketdata.OptionQuote{Expiry: T, Strike: K, Right: marketdata.Call, Price: p})
		}
	}
	snap := marketdata.Snapshot{
		ID: "normal-2026-09-26", AsOf: "2026-09-26", Spot: spot,
		Rates: rates, Options: opts,
	}
	write("testdata/snapshots/normal.json", snap)

	// 缺失利率报价
	bad1 := snap
	bad1.ID = "missing-rates"
	bad1.Rates = nil
	write("testdata/snapshots/missing_rates.json", bad1)

	// 重复期权报价
	bad2 := snap
	bad2.ID = "duplicate-option"
	bad2.Options = append(append([]marketdata.OptionQuote{}, snap.Options...), snap.Options[0])
	write("testdata/snapshots/duplicate_option.json", bad2)

	// 期限倒挂（利率期限逆序）
	bad3 := snap
	bad3.ID = "inverted-tenors"
	bad3.Rates = []marketdata.RateQuote{{Tenor: 1.0, Rate: 0.035}, {Tenor: 0.5, Rate: 0.032}}
	write("testdata/snapshots/inverted_tenors.json", bad3)

	// 波动率不可解：价格低于无套利下界（深度虚值看涨价格取极小正数）
	bad4 := snap
	bad4.ID = "unsolvable-vol"
	bad4.Options = append([]marketdata.OptionQuote{}, snap.Options...)
	for i, q := range bad4.Options {
		if q.Expiry == 1.0 && q.Strike == 100 {
			bad4.Options[i].Price = 0.5 // 低于无套利下界 ~3.44
		}
	}
	write("testdata/snapshots/unsolvable_vol.json", bad4)
	fmt.Println("snapshots written")
}

func rateAt(t float64) float64 {
	// 与 normal.json 曲线节点线性插值一致
	type pt struct{ t, r float64 }
	pts := []pt{{0.25, 0.030}, {0.5, 0.032}, {1.0, 0.035}, {2.0, 0.038}}
	if t <= pts[0].t {
		return pts[0].r
	}
	for i := 1; i < len(pts); i++ {
		if t <= pts[i].t {
			w := (t - pts[i-1].t) / (pts[i].t - pts[i-1].t)
			return pts[i-1].r + w*(pts[i].r-pts[i-1].r)
		}
	}
	return pts[len(pts)-1].r
}

func write(path string, v any) {
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
}
