// Package marketdata 定义本地行情快照的接入、校验与加载。
//
// 校验原则：报价缺失、重复、期限倒挂或数值非法时必须以可区分的
// 错误明确拒绝，绝不用默认值静默兜底。
package marketdata

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
)

// 可区分的校验失败原因。
var (
	ErrMissingQuote     = errors.New("marketdata: 缺少必要报价")
	ErrDuplicateQuote   = errors.New("marketdata: 报价重复")
	ErrInvertedMaturity = errors.New("marketdata: 期限倒挂")
	ErrInvalidValue     = errors.New("marketdata: 报价数值非法")
)

// Right 表示期权方向。
type Right int

const (
	Call Right = iota
	Put
)

func (r Right) String() string {
	if r == Call {
		return "Call"
	}
	return "Put"
}

// MarshalJSON 输出 "Call"/"Put"。
func (r Right) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.String())
}

// UnmarshalJSON 支持 "Call"/"Put" 或 0/1 两种写法。
func (r *Right) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		switch s {
		case "Call", "call":
			*r = Call
		case "Put", "put":
			*r = Put
		default:
			return fmt.Errorf("%w: 未知期权方向 %q", ErrInvalidValue, s)
		}
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("%w: 期权方向格式非法", ErrInvalidValue)
	}
	if n != 0 && n != 1 {
		return fmt.Errorf("%w: 期权方向取值非法 %d", ErrInvalidValue, n)
	}
	*r = Right(n)
	return nil
}

// RateQuote 是一条零息利率报价（用于贴现曲线）。
type RateQuote struct {
	Tenor float64 `json:"tenor"` // 年
	Rate  float64 `json:"rate"`  // 连续复利零息利率
}

// OptionQuote 是一条欧式期权报价（用于隐含波动率曲面）。
type OptionQuote struct {
	Expiry float64 `json:"expiry"`
	Strike float64 `json:"strike"`
	Right  Right   `json:"right"`
	Price  float64 `json:"price"`
}

// Snapshot 是一份本地行情快照。
type Snapshot struct {
	ID      string        `json:"id"`
	AsOf    string        `json:"asOf"`
	Spot    float64       `json:"spot"`
	Rates   []RateQuote   `json:"rates"`
	Options []OptionQuote `json:"options"`
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Validate 校验快照，返回可区分的失败原因；不做任何默认兜底。
//
// 校验规则：
//   - 标的现价必须为正且有限；
//   - 利率报价不可缺失，期限必须为正、有限、严格递增（倒挂或重复即拒绝）；
//   - 期权报价不可缺失，期限/行权价/价格必须为正且有限；
//   - 同一 (expiry, strike, right) 的期权报价重复即拒绝。
func (s *Snapshot) Validate() error {
	if !finite(s.Spot) || s.Spot <= 0 {
		return fmt.Errorf("%w: 标的现价必须为正且有限, got %v", ErrInvalidValue, s.Spot)
	}
	if len(s.Rates) == 0 {
		return fmt.Errorf("%w: 利率报价为空", ErrMissingQuote)
	}
	tenors := make([]float64, len(s.Rates))
	for i, q := range s.Rates {
		if !finite(q.Tenor) || q.Tenor <= 0 {
			return fmt.Errorf("%w: 利率期限必须为正且有限, got %v", ErrInvalidValue, q.Tenor)
		}
		if !finite(q.Rate) {
			return fmt.Errorf("%w: 利率必须有限, got %v", ErrInvalidValue, q.Rate)
		}
		tenors[i] = q.Tenor
	}
	// 利率期限必须按升序给出：相等为重复，逆序为期限倒挂。
	for i := 1; i < len(tenors); i++ {
		if tenors[i] == tenors[i-1] {
			return fmt.Errorf("%w: 利率期限 %v 重复", ErrDuplicateQuote, tenors[i])
		}
		if tenors[i] < tenors[i-1] {
			return fmt.Errorf("%w: 利率期限 %v 出现在 %v 之后",
				ErrInvertedMaturity, tenors[i], tenors[i-1])
		}
	}
	if len(s.Options) == 0 {
		return fmt.Errorf("%w: 期权报价为空", ErrMissingQuote)
	}
	type optKey struct {
		expiry, strike float64
		right          Right
	}
	seen := make(map[optKey]struct{}, len(s.Options))
	for _, q := range s.Options {
		if !finite(q.Expiry) || q.Expiry <= 0 {
			return fmt.Errorf("%w: 期权期限必须为正且有限, got %v", ErrInvalidValue, q.Expiry)
		}
		if !finite(q.Strike) || q.Strike <= 0 {
			return fmt.Errorf("%w: 行权价必须为正且有限, got %v", ErrInvalidValue, q.Strike)
		}
		if !finite(q.Price) || q.Price <= 0 {
			return fmt.Errorf("%w: 期权价格必须为正且有限, got %v", ErrInvalidValue, q.Price)
		}
		k := optKey{q.Expiry, q.Strike, q.Right}
		if _, dup := seen[k]; dup {
			return fmt.Errorf("%w: 期权报价 (expiry=%v, strike=%v, right=%v)",
				ErrDuplicateQuote, q.Expiry, q.Strike, q.Right)
		}
		seen[k] = struct{}{}
	}
	return nil
}

// LoadSnapshot 从本地 JSON 文件加载并校验快照。
func LoadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("marketdata: 读取快照文件失败: %w", err)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("marketdata: 解析快照失败: %w", err)
	}
	if err := snap.Validate(); err != nil {
		return nil, err
	}
	return &snap, nil
}
