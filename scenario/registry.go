// Package scenario 提供多市场状态（场景）的注册、选取与跨状态并发对比估值。
//
// 核心约定：
//   - 市场状态按名字显式选取；名字不存在时返回 ErrUnknownState 并附上
//     可用状态列表，绝不悄悄回退到某个默认状态。
//   - 快照数据缺失/非法时不做任何补齐或默认值兜底，而是以
//     ErrStateDataUnavailable 或 ErrStateNotCalibratable 暴露成可区分的失败。
//   - 每个市场状态持有独立的校准结果与引擎缓存，状态之间互不串扰。
package scenario

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/highcumontoa/concurrent-option-pricing-go/calibration"
	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
	"github.com/highcumontoa/concurrent-option-pricing-go/pricing"
	"github.com/highcumontoa/concurrent-option-pricing-go/scenario/states"
)

// 注册表相关的可区分失败原因。
var (
	// ErrUnknownState：按名字选取了未注册的市场状态。
	ErrUnknownState = errors.New("scenario: 未知市场状态")
	// ErrStateDataUnavailable：状态的快照数据缺失、无法解析或未通过校验，
	// 即“这个状态的数据本身就有问题”，绝不允许用默认值蒙混。
	ErrStateDataUnavailable = errors.New("scenario: 市场状态数据不可用")
	// ErrStateNotCalibratable：快照本身合法，但无法据此校准出可用的
	// 曲线/波动率曲面（如不可解波动率、日历套利）。
	ErrStateNotCalibratable = errors.New("scenario: 市场状态不可校准")
	// ErrStateExists：重复注册同名状态。
	ErrStateExists = errors.New("scenario: 市场状态重名")
)

// State 是一个具名的本地市场状态。
//
// 状态只持有原始快照字节（懒加载）：即使注册了大量状态，未被选中的
// 状态也不会占用校准结果与曲面内存，从而使资源占用只与实际运行的
// 状态数量相关。
type State struct {
	Name        string
	Description string
	raw         []byte
}

// loadedState 是解析、校验并校准完成的市场状态。每个状态各自持有一份，
// 互不共享底层数组以外的任何可变结构。
type loadedState struct {
	snap  *marketdata.Snapshot
	curve *calibration.DiscountCurve
	surf  *calibration.VolSurface
}

// Registry 是市场状态注册表。零值为空注册表；用 DefaultRegistry 获取
// 已注册全部内置状态的注册表。
type Registry struct {
	byName map[string]State
	order  []string
}

// DefaultRegistry 返回包含全部内置市场状态的注册表。
func DefaultRegistry() *Registry {
	r := &Registry{}
	for _, b := range states.All() {
		if err := r.Register(State{Name: b.Name, Description: b.Description, raw: b.Data}); err != nil {
			panic(err) // 内置状态不可能注册失败
		}
	}
	return r
}

// Register 注册一个市场状态；重名返回 ErrStateExists。
func (r *Registry) Register(s State) error {
	if s.Name == "" {
		return fmt.Errorf("%w: 状态名字不能为空", ErrStateExists)
	}
	if r.byName == nil {
		r.byName = map[string]State{}
	}
	if _, dup := r.byName[s.Name]; dup {
		return fmt.Errorf("%w: %s", ErrStateExists, s.Name)
	}
	r.byName[s.Name] = s
	r.order = append(r.order, s.Name)
	return nil
}

// RegisterJSON 以内联快照 JSON 注册一个状态（便于测试注入“坏状态”）。
func (r *Registry) RegisterJSON(name, description string, raw []byte) error {
	return r.Register(State{Name: name, Description: description, raw: raw})
}

// List 按注册顺序返回全部可用市场状态（内置状态按 normal /
// liquidity-crunch / price-gap 的规范顺序）。
func (r *Registry) List() []State {
	out := make([]State, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n])
	}
	return out
}

// Names 按 List 的顺序返回全部可用状态名。
func (r *Registry) Names() []string {
	return append([]string(nil), r.order...)
}

// Get 按名字返回市场状态。
//
// 名字不存在时返回 ErrUnknownState，错误信息中列出当前全部可用状态，
// 调用方必须显式改用存在的名字；本函数不提供任何“默认状态”回退。
func (r *Registry) Get(name string) (State, error) {
	if s, ok := r.byName[name]; ok {
		return s, nil
	}
	available := append([]string(nil), r.order...)
	sort.Strings(available)
	if len(available) == 0 {
		return State{}, fmt.Errorf("%w: %q（注册表为空）", ErrUnknownState, name)
	}
	return State{}, fmt.Errorf("%w: %q；可用市场状态: %s",
		ErrUnknownState, name, strings.Join(available, ", "))
}

// Load 解析、校验并校准该状态的快照。
//
//   - 数据缺失/JSON 非法/未通过 Snapshot.Validate → ErrStateDataUnavailable；
//   - 数据合法但无法构建曲线或波动率曲面 → ErrStateNotCalibratable。
//
// 两条路径都不做默认值补齐。
func (s State) Load() (*loadedState, error) {
	if len(s.raw) == 0 {
		return nil, fmt.Errorf("%w: 状态 %s 的快照数据为空", ErrStateDataUnavailable, s.Name)
	}
	var snap marketdata.Snapshot
	if err := json.Unmarshal(s.raw, &snap); err != nil {
		return nil, fmt.Errorf("%w: 状态 %s 快照无法解析: %v", ErrStateDataUnavailable, s.Name, err)
	}
	if err := snap.Validate(); err != nil {
		return nil, fmt.Errorf("%w: 状态 %s 快照校验失败: %v", ErrStateDataUnavailable, s.Name, err)
	}
	curve, err := calibration.BuildDiscountCurve(&snap)
	if err != nil {
		return nil, fmt.Errorf("%w: 状态 %s 贴现曲线不可构建: %v",
			ErrStateNotCalibratable, s.Name, err)
	}
	surf, err := calibration.BuildVolSurface(&snap, curve)
	if err != nil {
		return nil, fmt.Errorf("%w: 状态 %s 波动率曲面不可构建: %v",
			ErrStateNotCalibratable, s.Name, err)
	}
	return &loadedState{snap: &snap, curve: curve, surf: surf}, nil
}

// envFor 由校准结果为单个工具构建估值市场环境。
func (ls *loadedState) envFor(spec pricing.Spec) pricing.MarketEnv {
	return pricing.MarketEnv{
		Spot: ls.snap.Spot,
		Rate: ls.curve.ZeroRate(spec.Expiry),
		Vol:  ls.surf.Vol(spec.Expiry, spec.Strike),
	}
}
