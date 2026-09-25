package market

import (
	"encoding/json"
	"fmt"
	"os"
)

// LoadSnapshot 从本地 JSON 文件加载行情快照并完成结构校验。
// 不访问任何外部服务；校验失败时返回可区分的错误。
func LoadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("market: read snapshot %q: %w", path, err)
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%w: snapshot %q is not valid JSON: %v", ErrSnapshotInvalid, path, err)
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("snapshot %q: %w", path, err)
	}
	return &s, nil
}
