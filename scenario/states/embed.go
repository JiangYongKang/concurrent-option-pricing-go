// Package states 以内置资源形式提供本机可复现的市场状态快照。
//
// 这些文件由 cmd/gensnap 确定性生成（go run ./cmd/gensnap），
// 不依赖时钟、随机数或外部服务，重新生成逐字节一致。
package states

import _ "embed"

//go:embed normal.json
var Normal []byte

//go:embed liquidity_crunch.json
var LiquidityCrunch []byte

//go:embed price_gap.json
var PriceGap []byte

// Builtin 描述一个内置市场状态。
type Builtin struct {
	Name        string
	Description string
	Data        []byte
}

// All 按规范顺序返回全部内置市场状态。
func All() []Builtin {
	return []Builtin{
		{
			Name:        "normal",
			Description: "正常市场：3% 量级利率、22% 平值波动率、轻微微笑",
			Data:        Normal,
		},
		{
			Name:        "liquidity-crunch",
			Description: "流动性急剧收紧：短端利率跳升至 6.2%，波动率整体升至 55%+ 且强下行偏斜",
			Data:        LiquidityCrunch,
		},
		{
			Name:        "price-gap",
			Description: "价格大幅跳空：标的低开至 85、利率骤降至 0.8%，近月波动率 75% 后快速回落",
			Data:        PriceGap,
		},
	}
}

// Names 返回全部内置市场状态的名字。
func Names() []string {
	all := All()
	out := make([]string, len(all))
	for i, b := range all {
		out[i] = b.Name
	}
	return out
}
