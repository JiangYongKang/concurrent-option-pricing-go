package market

import (
	"fmt"
	"math"
	"sort"
)

// 隐含波动率求解参数（确定性二分法）。
const (
	ivMinVol  = 1e-6
	ivMaxVol  = 5.0
	ivMaxIter = 200
	ivAbsTol  = 1e-8
	gridEps   = 1e-12
)

// VolSurface 由隐含波动率报价构建的波动率曲面。
// 报价必须构成 (到期日 × 行权价) 的完整矩形网格，且方向一致（全看涨或全看跌）；
// 网格内做双线性插值，网格外做最近边界的平坦外推（不静默发明数据）。
type VolSurface struct {
	expiries []float64
	strikes  []float64
	vols     [][]float64 // [expiryIdx][strikeIdx]
	isCall   bool
}

// normCDF 标准正态分布函数。
func normCDF(x float64) float64 { return 0.5 * math.Erfc(-x/math.Sqrt2) }

// bsPrice 带连续复利 r 的 Black-Scholes 价格（内部实现，避免包循环）。
func bsPrice(spot, strike, expiry, vol, rate float64, isCall bool) float64 {
	d1 := (math.Log(spot/strike) + (rate+0.5*vol*vol)*expiry) / (vol * math.Sqrt(expiry))
	d2 := d1 - vol*math.Sqrt(expiry)
	df := math.Exp(-rate * expiry)
	if isCall {
		return spot*normCDF(d1) - strike*df*normCDF(d2)
	}
	return strike*df*normCDF(-d2) - spot*normCDF(-d1)
}

// implyVol 以确定性二分法从期权市场价反解隐含波动率。
// 报价越过无套利边界或求解不收敛时返回 ErrVolUnsolvable。
func implyVol(spot, strike, expiry, rate, price float64, isCall bool) (float64, error) {
	df := math.Exp(-rate * expiry)
	// 无套利边界（容忍 gridEps 的报价舍入）。
	var lo, hi float64
	if isCall {
		lo = math.Max(0, spot-strike*df) - gridEps
		hi = spot + gridEps
	} else {
		lo = math.Max(0, strike*df-spot) - gridEps
		hi = strike*df + gridEps
	}
	if price < lo || price > hi {
		return 0, fmt.Errorf("%w: price %.10f outside no-arbitrage band [%.10f, %.10f] (expiry=%v strike=%v isCall=%v)",
			ErrVolUnsolvable, price, math.Max(0, lo), math.Min(hi, hi), expiry, strike, isCall)
	}
	loP := bsPrice(spot, strike, expiry, ivMinVol, rate, isCall)
	hiP := bsPrice(spot, strike, expiry, ivMaxVol, rate, isCall)
	if price < loP-gridEps || price > hiP+gridEps {
		return 0, fmt.Errorf("%w: price %.10f outside price range over vol [%v,%v]: [%.10f, %.10f] (expiry=%v strike=%v)",
			ErrVolUnsolvable, price, ivMinVol, ivMaxVol, loP, hiP, expiry, strike)
	}
	lo, hi = ivMinVol, ivMaxVol
	for i := 0; i < ivMaxIter; i++ {
		mid := (lo + hi) / 2
		p := bsPrice(spot, strike, expiry, mid, rate, isCall)
		if math.Abs(p-price) <= gridEps || hi-lo <= ivAbsTol {
			return mid, nil
		}
		// BS 价格关于波动率严格单调。
		if p < price {
			lo = mid
		} else {
			hi = mid
		}
	}
	return 0, fmt.Errorf("%w: bisection did not converge within %d iterations (expiry=%v strike=%v price=%v)",
		ErrVolUnsolvable, ivMaxIter, expiry, strike, price)
}

// Calibrate 对快照中的期权报价逐条反解隐含波动率并构建曲面。
// 纯函数：相同输入必定得到相同曲面（排序确定、求解确定性）。
func Calibrate(s *Snapshot, curve *DiscountCurve) (*VolSurface, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if curve == nil {
		return nil, fmt.Errorf("%w: nil discount curve", ErrSnapshotInvalid)
	}
	isCall := s.VolQuotes[0].IsCall
	expirySet := map[float64]struct{}{}
	strikeSet := map[float64]struct{}{}
	iv := map[[2]float64]float64{}
	for _, q := range s.VolQuotes {
		if q.IsCall != isCall {
			return nil, fmt.Errorf("%w: mixed call/put quotes in one surface (expiry=%v strike=%v)",
				ErrSnapshotInvalid, q.Expiry, q.Strike)
		}
		rate := curve.ZeroRate(q.Expiry)
		vol, err := implyVol(s.Spot, q.Strike, q.Expiry, rate, q.Price, q.IsCall)
		if err != nil {
			return nil, err
		}
		iv[[2]float64{q.Expiry, q.Strike}] = vol
		expirySet[q.Expiry] = struct{}{}
		strikeSet[q.Strike] = struct{}{}
	}
	expiries := make([]float64, 0, len(expirySet))
	for t := range expirySet {
		expiries = append(expiries, t)
	}
	sort.Float64s(expiries)
	strikes := make([]float64, 0, len(strikeSet))
	for k := range strikeSet {
		strikes = append(strikes, k)
	}
	sort.Float64s(strikes)

	// 要求完整矩形网格；缺失格子是明确的数据缺口，拒绝而非猜测。
	vols := make([][]float64, len(expiries))
	for i, t := range expiries {
		vols[i] = make([]float64, len(strikes))
		for j, k := range strikes {
			v, ok := iv[[2]float64{t, k}]
			if !ok {
				return nil, fmt.Errorf("%w: vol grid cell expiry=%v strike=%v", ErrQuoteMissing, t, k)
			}
			vols[i][j] = v
		}
	}
	return &VolSurface{expiries: expiries, strikes: strikes, vols: vols, isCall: isCall}, nil
}

// interp1 在排序网格 xs 上对 ys 做线性插值（平坦外推）。
func interp1(xs, ys []float64, x float64) float64 {
	if x <= xs[0] {
		return ys[0]
	}
	last := len(xs) - 1
	if x >= xs[last] {
		return ys[last]
	}
	i := sort.Search(last, func(i int) bool { return xs[i] >= x })
	x0, x1 := xs[i-1], xs[i]
	w := (x - x0) / (x1 - x0)
	return ys[i-1]*(1-w) + ys[i]*w
}

// Vol 返回 (expiry, strike) 处的隐含波动率：
// 先沿行权价在每个到期日上线性插值，再沿到期日线性插值；边界平坦外推。
func (v *VolSurface) Vol(expiry, strike float64) float64 {
	atExpiry := make([]float64, len(v.expiries))
	for i := range v.expiries {
		atExpiry[i] = interp1(v.strikes, v.vols[i], strike)
	}
	return interp1(v.expiries, atExpiry, expiry)
}

// Expiries 返回曲面到期日网格的副本（测试/诊断用）。
func (v *VolSurface) Expiries() []float64 {
	out := make([]float64, len(v.expiries))
	copy(out, v.expiries)
	return out
}

// Strikes 返回曲面行权价网格的副本（测试/诊断用）。
func (v *VolSurface) Strikes() []float64 {
	out := make([]float64, len(v.strikes))
	copy(out, v.strikes)
	return out
}
