package market

import (
	"fmt"
	"math"
)

// RateQuote 一条贴现曲线报价：期限（年）与对应贴现因子。
type RateQuote struct {
	Tenor          float64 `json:"tenor"`
	DiscountFactor float64 `json:"discountFactor"`
}

// VolQuote 一条期权市场报价，用于反解隐含波动率。
type VolQuote struct {
	Expiry float64 `json:"expiry"`
	Strike float64 `json:"strike"`
	Price  float64 `json:"price"`
	IsCall bool    `json:"isCall"`
}

// Snapshot 一份本地行情快照。
type Snapshot struct {
	Name       string      `json:"name"`
	Spot       float64     `json:"spot"`
	RateQuotes []RateQuote `json:"rateQuotes"`
	VolQuotes  []VolQuote  `json:"volQuotes"`
}

// Validate 校验快照结构合法性，失败时返回可区分的错误：
//   - ErrSnapshotInvalid：字段缺失、非正数、非有限值；
//   - ErrQuoteMissing：利率或波动率报价为空；
//   - ErrQuoteDuplicate：相同期限的利率报价，或相同 (期限, 行权价, 方向) 的期权报价。
func (s *Snapshot) Validate() error {
	if s == nil {
		return fmt.Errorf("%w: nil snapshot", ErrSnapshotInvalid)
	}
	if !(s.Spot > 0) || math.IsNaN(s.Spot) || math.IsInf(s.Spot, 0) {
		return fmt.Errorf("%w: spot must be a positive finite number, got %v", ErrSnapshotInvalid, s.Spot)
	}
	if len(s.RateQuotes) == 0 {
		return fmt.Errorf("%w: no rate quotes", ErrQuoteMissing)
	}
	if len(s.VolQuotes) == 0 {
		return fmt.Errorf("%w: no vol quotes", ErrQuoteMissing)
	}
	seenTenor := map[float64]struct{}{}
	for i, q := range s.RateQuotes {
		if !(q.Tenor > 0) || math.IsInf(q.Tenor, 0) {
			return fmt.Errorf("%w: rate quote %d has non-positive/invalid tenor %v", ErrSnapshotInvalid, i, q.Tenor)
		}
		if !(q.DiscountFactor > 0) || q.DiscountFactor > 1 || math.IsNaN(q.DiscountFactor) {
			return fmt.Errorf("%w: rate quote %d has discount factor %v outside (0,1]", ErrSnapshotInvalid, i, q.DiscountFactor)
		}
		if _, dup := seenTenor[q.Tenor]; dup {
			return fmt.Errorf("%w: rate quote tenor %v", ErrQuoteDuplicate, q.Tenor)
		}
		seenTenor[q.Tenor] = struct{}{}
	}
	type volKey struct {
		expiry, strike float64
		isCall         bool
	}
	seenVol := map[volKey]struct{}{}
	for i, q := range s.VolQuotes {
		if !(q.Expiry > 0) || math.IsInf(q.Expiry, 0) {
			return fmt.Errorf("%w: vol quote %d has non-positive/invalid expiry %v", ErrSnapshotInvalid, i, q.Expiry)
		}
		if !(q.Strike > 0) || math.IsInf(q.Strike, 0) {
			return fmt.Errorf("%w: vol quote %d has non-positive/invalid strike %v", ErrSnapshotInvalid, i, q.Strike)
		}
		if !(q.Price >= 0) || math.IsNaN(q.Price) || math.IsInf(q.Price, 0) {
			return fmt.Errorf("%w: vol quote %d has negative/invalid price %v", ErrSnapshotInvalid, i, q.Price)
		}
		k := volKey{q.Expiry, q.Strike, q.IsCall}
		if _, dup := seenVol[k]; dup {
			return fmt.Errorf("%w: vol quote expiry=%v strike=%v isCall=%v", ErrQuoteDuplicate, q.Expiry, q.Strike, q.IsCall)
		}
		seenVol[k] = struct{}{}
	}
	return nil
}
