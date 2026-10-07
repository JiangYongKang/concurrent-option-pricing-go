// Command gensnap 重新生成 testdata/snapshots 下的本地行情快照，
// 保证测试数据可本机复现：go run ./cmd/gensnap
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

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

	// ---- 多市场状态场景（testdata/scenarios） ----
	// 状态名即文件名；每个场景都是完整、可独立校准的本地快照。

	// 基准状态：与 normal.json 相同的正常市场。
	write("testdata/scenarios/normal.json", snap)

	// 极端状态一：价格大幅跳空（隔夜 -22%），波动率与利率同步抬升，
	// 行权价网格围绕跳空后的现价布置。
	gapRates := []marketdata.RateQuote{
		{Tenor: 0.25, Rate: 0.036},
		{Tenor: 0.5, Rate: 0.038},
		{Tenor: 1.0, Rate: 0.041},
		{Tenor: 2.0, Rate: 0.044},
	}
	gapDown := buildSnap("gap-down-2026-09-28", "2026-09-28", 78.0, gapRates,
		[]float64{60, 70, 78, 86, 94},
		func(T, K float64) float64 {
			m := K/78.0 - 1
			base := 0.50 - 0.01*(T-1)
			return base - 0.18*m + 0.60*m*m
		})
	write("testdata/scenarios/gap_down.json", gapDown)

	// 极端状态二：流动性急剧收紧，利率大幅上行、波动率曲面整体
	// 抬升且微笑显著变陡。
	crunchRates := []marketdata.RateQuote{
		{Tenor: 0.25, Rate: 0.055},
		{Tenor: 0.5, Rate: 0.058},
		{Tenor: 1.0, Rate: 0.062},
		{Tenor: 2.0, Rate: 0.065},
	}
	crunch := buildSnap("liquidity-crunch-2026-09-29", "2026-09-29", 100.0, crunchRates,
		[]float64{80, 90, 100, 110, 120},
		func(T, K float64) float64 {
			m := K/100.0 - 1
			base := 0.38 - 0.005*(T-1)
			return base - 0.25*m + 0.90*m*m
		})
	write("testdata/scenarios/liquidity_crunch.json", crunch)

	// 数据不完整的状态：利率报价缺失。该状态会出现在列表里，
	// 但加载/校准必须被明确拒绝（数据不可用），不允许补齐蒙混。
	incomplete := snap
	incomplete.ID = "incomplete-missing-rates"
	incomplete.Rates = nil
	write("testdata/scenarios/incomplete.json", incomplete)

	fmt.Println("snapshots written")
}

// buildSnap 用给定的利率曲线节点与波动率微笑函数生成一份完整快照：
// 期权价格由对应波动率下的 Black-Scholes 闭式解正算得到，
// 保证隐含波动率可解、日历无套利，且逐字节可复现。
func buildSnap(id, asOf string, spot float64, rates []marketdata.RateQuote,
	strikes []float64, volOf func(T, K float64) float64) marketdata.Snapshot {
	var opts []marketdata.OptionQuote
	for _, T := range []float64{0.25, 0.5, 1.0, 2.0} {
		for _, K := range strikes {
			env := pricing.MarketEnv{Spot: spot, Rate: interpRate(rates, T), Vol: volOf(T, K)}
			spec := pricing.Spec{Right: marketdata.Call, Style: pricing.European, Strike: K, Expiry: T}
			p, err := pricing.BlackScholes(spec, env)
			if err != nil {
				panic(err)
			}
			opts = append(opts, marketdata.OptionQuote{Expiry: T, Strike: K, Right: marketdata.Call, Price: p})
		}
	}
	return marketdata.Snapshot{ID: id, AsOf: asOf, Spot: spot, Rates: rates, Options: opts}
}

// interpRate 按曲线规则对零息利率做节点间线性插值、端点平坦外推。
func interpRate(rates []marketdata.RateQuote, t float64) float64 {
	if t <= rates[0].Tenor {
		return rates[0].Rate
	}
	for i := 1; i < len(rates); i++ {
		if t <= rates[i].Tenor {
			w := (t - rates[i-1].Tenor) / (rates[i].Tenor - rates[i-1].Tenor)
			return rates[i-1].Rate + w*(rates[i].Rate-rates[i-1].Rate)
		}
	}
	return rates[len(rates)-1].Rate
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
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
