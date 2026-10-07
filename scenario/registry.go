package scenario

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/highcumontoa/concurrent-option-pricing-go/marketdata"
)

// ErrUnknownScenario 表示指定了不存在的市场状态。
// 拒绝是显式的：绝不静默回退到任何默认状态。
var ErrUnknownScenario = errors.New("scenario: 未知市场状态")

// Registry 是命名市场状态（本地快照）的注册表。
//
// 市场状态以 JSON 快照文件的形式存放在同一目录下，
// 状态名即文件名（去掉 .json 后缀）。注册表只负责“有哪些状态、
// 按名取哪份快照”，快照的校验仍由 marketdata.LoadSnapshot 完成：
// 数据缺失或非法的状态会出现在 List 里，但 Load 时以可区分的
// 校验错误明确拒绝，绝不用默认值补齐。
type Registry struct {
	dir   string
	names []string // 已排序，保证 List 输出确定
}

// LoadRegistry 扫描目录下的 *.json 快照，建立名称注册表。
// 目录不存在或不可读时返回错误；目录为空是合法的（List 为空）。
func LoadRegistry(dir string) (*Registry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("scenario: 读取市场状态目录失败: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	return &Registry{dir: dir, names: names}, nil
}

// List 返回全部可用市场状态名（按字典序，确定性）。
func (r *Registry) List() []string {
	return append([]string(nil), r.names...)
}

// Has 报告指定名字的市场状态是否存在。
func (r *Registry) Has(name string) bool {
	i := sort.SearchStrings(r.names, name)
	return i < len(r.names) && r.names[i] == name
}

// Load 按名字加载并校验快照。
//
// 名字不存在时返回 ErrUnknownScenario（显式拒绝，不回退默认状态）；
// 快照存在但数据缺失/非法时返回 marketdata 的可区分校验错误，
// 调用方应将其归类为“数据不可用”，绝不允许用补齐值蒙混。
func (r *Registry) Load(name string) (*marketdata.Snapshot, error) {
	if !r.Has(name) {
		return nil, fmt.Errorf("%w: %q（可用状态: %s）",
			ErrUnknownScenario, name, strings.Join(r.names, ", "))
	}
	return marketdata.LoadSnapshot(filepath.Join(r.dir, name+".json"))
}
