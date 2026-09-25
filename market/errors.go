// Package market 负责本地行情快照的接入、校验，
// 以及贴现曲线与隐含波动率曲面的构建。
package market

import "errors"

// 可区分的行情/校准失败原因。调用方应使用 errors.Is 判定。
var (
	// ErrQuoteMissing 必要报价缺失（如某期限无任何报价）。
	ErrQuoteMissing = errors.New("market: quote missing")
	// ErrQuoteDuplicate 同一 (期限, 行权价) 出现重复报价。
	ErrQuoteDuplicate = errors.New("market: duplicate quote")
	// ErrTenorInversion 贴现因子随期限倒挂（非单调递减）。
	ErrTenorInversion = errors.New("market: tenor inversion in discount curve")
	// ErrVolUnsolvable 隐含波动率在给定报价下不可解
	// （报价越出无套利边界或数值求解不收敛）。
	ErrVolUnsolvable = errors.New("market: implied volatility unsolvable")
	// ErrSnapshotInvalid 快照结构非法（字段缺失、非正数等）。
	ErrSnapshotInvalid = errors.New("market: snapshot invalid")
)
