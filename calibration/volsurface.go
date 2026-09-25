package calibration

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
	"github.com/highcumontoa/concurrent-option-pricing-go/pricing"
)

// 波动率求解/曲面构建失败原因。
var (
	ErrVolNotSolvable    = errors.New("calibration: 隐含波动率不可解")
	ErrCalendarArbitrage = errors.New("calibration: 总方差随期限倒挂（日历套利）")
)

const (
	volLowerBound = 1e-4
	volUpperBound = 5.0
	volBisectIter = 200 // 固定迭代次数，保证结果可复现
)

// ImpliedVol 由期权价格反解隐含波动率（确定性二分法）。
//
// 价格越出无套利区间 [下界, 上界] 时返回 ErrVolNotSolvable，
// 不做任何截断或默认兜底。相同输入必得相同输出。
func ImpliedVol(price, spot, strike, expiry, rate float64, right marketdata.Right) (float64, error) {
	if price <= 0 || spot <= 0 || strike <= 0 || expiry <= 0 ||
		math.IsNaN(price) || math.IsNaN(spot) || math.IsNaN(strike) ||
		math.IsNaN(expiry) || math.IsNaN(rate) {
		return 0, fmt.Errorf("%w: 输入非法 (price=%v, S=%v, K=%v, T=%v, r=%v)",
			ErrVolNotSolvable, price, spot, strike, expiry, rate)
	}
	disc := math.Exp(-rate * expiry)
	var lo, hi float64 // 无套利价格区间
	if right == marketdata.Call {
		lo = math.Max(0, spot-strike*disc)
		hi = spot
	} else {
		lo = math.Max(0, strike*disc-spot)
		hi = strike * disc
	}
	const tol = 1e-12
	if price < lo-tol || price > hi+tol {
		return 0, fmt.Errorf("%w: 价格 %v 越出无套利区间 [%v, %v]",
			ErrVolNotSolvable, price, lo, hi)
	}
	if price <= lo+tol {
		return 0, fmt.Errorf("%w: 价格 %v 触及下界 %v，波动率无正解",
			ErrVolNotSolvable, price, lo)
	}
	env := pricing.MarketEnv{Spot: spot, Rate: rate}
	spec := pricing.Spec{Right: right, Style: pricing.European, Strike: strike, Expiry: expiry}
	at := func(sigma float64) float64 {
		env.Vol = sigma
		p, _ := pricing.BlackScholes(spec, env)
		return p
	}
	if at(volUpperBound) < price {
		return 0, fmt.Errorf("%w: 波动率上界 %v 处价格仍低于目标 %v",
			ErrVolNotSolvable, volUpperBound, price)
	}
	a, b := volLowerBound, volUpperBound
	for i := 0; i < volBisectIter; i++ {
		m := 0.5 * (a + b)
		if at(m) < price {
			a = m
		} else {
			b = m
		}
	}
	return 0.5 * (a + b), nil
}

// VolSurface 是按期限-行权价网格构建的隐含波动率曲面。
//
// 插值规则（确定性）：
//   - 期限方向：对总方差 w = sigma^2 * T 做线性插值；
//   - 行权价方向：对每个期限的微笑做线性插值，端点外平坦外推。
type VolSurface struct {
	expiries []float64   // 严格递增
	strikes  []float64   // 严格递增（公共行权价网格）
	vols     [][]float64 // [expiry][strike]
}

// BuildVolSurface 由快照中的期权报价反解隐含波动率并构建曲面。
// 任一报价不可解即整体失败；构建后校验日历无套利。
func BuildVolSurface(snap *marketdata.Snapshot, curve *DiscountCurve) (*VolSurface, error) {
	if snap == nil || curve == nil || len(snap.Options) == 0 {
		return nil, fmt.Errorf("%w: 缺少期权报价或贴现曲线", ErrVolNotSolvable)
	}
	// 收集并排序期限与行权价网格（排序保证与输入顺序无关）。
	expSet := map[float64]bool{}
	strSet := map[float64]bool{}
	for _, q := range snap.Options {
		expSet[q.Expiry] = true
		strSet[q.Strike] = true
	}
	expiries := make([]float64, 0, len(expSet))
	for e := range expSet {
		expiries = append(expiries, e)
	}
	sort.Float64s(expiries)
	strikes := make([]float64, 0, len(strSet))
	for k := range strSet {
		strikes = append(strikes, k)
	}
	sort.Float64s(strikes)
	strIdx := make(map[float64]int, len(strikes))
	for i, k := range strikes {
		strIdx[k] = i
	}

	// 逐报价反解，先放到 (expiry, strike) 网格上。
	grid := make([][]cell, len(expiries))
	for i := range grid {
		grid[i] = make([]cell, len(strikes))
	}
	expIdx := make(map[float64]int, len(expiries))
	for i, e := range expiries {
		expIdx[e] = i
	}
	for _, q := range snap.Options {
		r := curve.ZeroRate(q.Expiry)
		v, err := ImpliedVol(q.Price, snap.Spot, q.Strike, q.Expiry, r, q.Right)
		if err != nil {
			return nil, fmt.Errorf("期权 (expiry=%v, strike=%v, right=%v, price=%v): %w",
				q.Expiry, q.Strike, q.Right, q.Price, err)
		}
		grid[expIdx[q.Expiry]][strIdx[q.Strike]] = cell{vol: v, ok: true}
	}
	// 每个期限内：对缺失的行权价用该期限微笑的线性插值/平坦外推补齐。
	vols := make([][]float64, len(expiries))
	for i := range expiries {
		vols[i] = make([]float64, len(strikes))
		fillSmile(grid[i], vols[i])
	}
	vs := &VolSurface{expiries: expiries, strikes: strikes, vols: vols}
	// 日历无套利：公共网格上总方差须随期限非递减。
	for j, k := range strikes {
		prev := 0.0
		for i, e := range expiries {
			w := vols[i][j] * vols[i][j] * e
			if w < prev-1e-12 {
				return nil, fmt.Errorf("%w: 行权价 %v 处总方差由 %v 降至 %v (期限 %v -> %v)",
					ErrCalendarArbitrage, k, prev, w, expiries[i-1], e)
			}
			prev = w
		}
	}
	return vs, nil
}

// cell 是微笑网格上的一个节点。
type cell struct {
	vol float64
	ok  bool
}

// fillSmile 用已知点的线性插值与平坦外推补齐整行微笑。
func fillSmile(row []cell, out []float64) {
	n := len(row)
	known := []int{}
	for j := range row {
		if row[j].ok {
			known = append(known, j)
		}
	}
	if len(known) == 0 {
		return // 不会发生：每个期限至少一条报价
	}
	for j := 0; j < n; j++ {
		if row[j].ok {
			out[j] = row[j].vol
			continue
		}
		// 找 j 两侧最近的已知点。
		lo, hi := -1, -1
		for _, k := range known {
			if k < j {
				lo = k
			}
			if k > j && hi == -1 {
				hi = k
			}
		}
		switch {
		case lo == -1:
			out[j] = row[hi].vol
		case hi == -1:
			out[j] = row[lo].vol
		default:
			w := float64(j-lo) / float64(hi-lo)
			out[j] = row[lo].vol + w*(row[hi].vol-row[lo].vol)
		}
	}
}

// Vol 返回 (expiry, strike) 处的隐含波动率。
func (vs *VolSurface) Vol(expiry, strike float64) float64 {
	if expiry <= 0 {
		expiry = vs.expiries[0]
	}
	// 行权价方向：线性插值，平坦外推。
	volAt := func(i int) float64 {
		ss, vv := vs.strikes, vs.vols[i]
		n := len(ss)
		if strike <= ss[0] {
			return vv[0]
		}
		if strike >= ss[n-1] {
			return vv[n-1]
		}
		j := sort.SearchFloat64s(ss, strike)
		w := (strike - ss[j-1]) / (ss[j] - ss[j-1])
		return vv[j-1] + w*(vv[j]-vv[j-1])
	}
	// 期限方向：对总方差线性插值，平坦外推。
	es := vs.expiries
	n := len(es)
	if expiry <= es[0] {
		return volAt(0)
	}
	if expiry >= es[n-1] {
		return volAt(n - 1)
	}
	i := sort.SearchFloat64s(es, expiry)
	t0, t1 := es[i-1], es[i]
	w0 := volAt(i-1) * volAt(i-1) * t0
	w1 := volAt(i) * volAt(i) * t1
	w := w0 + (expiry-t0)/(t1-t0)*(w1-w0)
	return math.Sqrt(w / expiry)
}
