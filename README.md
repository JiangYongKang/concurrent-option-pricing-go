# concurrent-option-pricing-go

本机可复现的并行期权定价与市场数据校准引擎。全部能力仅依赖 Go 标准库，
不依赖外部服务、真实凭据或联网运行时。

## 构建与验证

```bash
go build ./...        # 编译
go test ./...         # 单元测试与集成测试
go test -race ./...   # 竞态检测（要求无数据竞争）
go run ./cmd/gensnap  # 重新生成 testdata/snapshots（输出逐字节可复现）
```

测试日志（`go test -v`）会打印关键输入与判定依据，例如失败报价的具体
期限/行权价/价格、无套利区间、蒙特卡洛标准误与容差等。

## 包结构

| 包 | 职责 |
|---|---|
| `marketdata` | 本地行情快照（JSON）的加载与校验 |
| `calibration` | 贴现曲线与隐含波动率曲面的构建 |
| `pricing` | 解析解 / 二叉树 / 蒙特卡洛三类估值路径 |
| `engine` | 带并发上限、路径预算与超时控制的批量估值 |
| `testdata/snapshots` | 本地行情快照样例（正常与各类异常） |

## 市场数据与校准规则

快照为本地 JSON 文件（见 `testdata/snapshots/normal.json`），包含标的现价、
连续复利零息利率报价与欧式期权报价。校验规则（`Snapshot.Validate`）：

- 报价缺失（利率或期权为空）→ `ErrMissingQuote`
- 报价重复（同期限利率、同 `(expiry, strike, right)` 期权）→ `ErrDuplicateQuote`
- 期限倒挂（利率期限未按升序给出）→ `ErrInvertedMaturity`
- 数值非法（非正、NaN、Inf）→ `ErrInvalidValue`

校验不做任何默认兜底；失败原因可用 `errors.Is` 区分。

校准（确定性算法，相同输入必得相同输出）：

- **贴现曲线**：零息利率在相邻期限节点线性插值，端点外平坦外推，
  贴现因子 `DF(t) = exp(-r(t)·t)`。
- **隐含波动率**：固定 200 次迭代的二分法。价格越出无套利区间
  （看涨 `[max(0, S-Ke^{-rT}), S]`，看跌 `[max(0, Ke^{-rT}-S), Ke^{-rT}]`）
  或触及下界时返回 `ErrVolNotSolvable`，不截断、不兜底。
- **波动率曲面**：期限方向对总方差 `w = σ²T` 线性插值，行权价方向线性插值、
  端点平坦外推；构建后校验日历无套利（总方差须随期限非递减），
  违反时报 `ErrCalendarArbitrage`。

## 估值方法适用范围

| 方法 | 适用范围 | 说明 |
|---|---|---|
| `Analytic` | 欧式 | Black-Scholes 闭式解 |
| `Binomial` | 欧式 / 美式 | CRR 树，默认 512 步 |
| `MonteCarlo` | 欧式 | GBM 终端抽样 + 对偶变量，种子确定性 |

估值失败原因互相区分：

`ErrMethodNotApplicable`（方法不适用，如对美式用
解析解/终端 MC）、`ErrNotConverged`（路径预算用尽后标准误仍超容差，默认
容差为价格的 1%）、`ErrParameterOutOfRange`（非法行权价/期限/波动率等）。
任何失败都不会返回零值冒充成功。

同一欧式工具在三种方法下的结果应在可解释容差内一致：树与解析解差异
量级为 `O(1/steps)`（512 步约 1e-2），MC 与解析解差异以标准误为尺度
（测试取约 2 倍标准误）。

## 并发与资源语义

`engine.Engine` 在固定市场环境下执行批量估值（`PriceBatch`）：

- **并发上限**：`Config.MaxWorkers` 固定大小的 worker 池。
- **路径预算**：`Config.PathBudget` 为单批次蒙特卡洛路径总数上限；
  按请求顺序确定性分配，超出的请求以 `ErrBudgetExceeded` 明确拒绝
  （不截断路径数、不降级精度），非 MC 请求不受预算约束。
- **超时**：`Config.Timeout` 作用于整个批次，超时请求报
  `context.DeadlineExceeded`。
- **取消**：`ctx` 取消后，未完成的请求报 `context.Canceled`；
  被取消/超时的请求**不写入结果缓存**、不影响其他请求，
  用新上下文重跑可正常完成。
- **可复现**：每个请求的随机性只来自其自身 `Seed`（PCG 随机源），
  与并发调度无关；相同环境、相同请求、相同 `Config` 在任意
  `MaxWorkers` 布局下结果逐位一致（见
  `TestBatchReproducibleAcrossLayouts`）。
- **缓存**：仅缓存成功结果，键为 `(Spec, Method, Seed, Paths)`。

## 本地验证场景

测试覆盖（均可通过 `go test ./...` 复现）：

- 不可解波动率：`unsolvable_vol.json` 中价格低于无套利下界 →
  `ErrVolNotSolvable`
- 重复校准：同一快照两次校准，曲线与曲面逐点一致
- 方法间一致性：解析/树/MC 在容差内一致；美式 put ≥ 欧式 put
- 并发取消：取消不污染缓存、不干扰其他请求；超时受控；
  预算超出按序拒绝；`-race` 下无数据竞争
