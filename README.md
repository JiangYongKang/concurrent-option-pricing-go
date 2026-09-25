# concurrent-option-pricing-go

本机可复现的并行期权定价与市场数据校准引擎。全部能力仅依赖 Go 标准库，
不访问外部服务、不需要真实凭据、不需要联网获取运行时。

## 构建与验证

```bash
go build ./...        # 编译
go vet ./...          # 静态检查
go test ./...         # 单元测试
go test -race ./...   # 竞态检测（要求无数据竞争）
go test -v ./...      # 查看关键输入与判定依据日志
```

## 包结构

| 包 | 职责 |
|---|---|
| `market` | 本地行情快照接入与校验、贴现曲线构建、隐含波动率曲面校准 |
| `pricing` | 解析解（Black-Scholes）、CRR 二叉树、蒙特卡洛三类估值路径 |
| `engine` | 带并发上限、路径预算与超时控制的批量估值与结果缓存 |
| `testdata` | 本地行情快照：`snapshot_normal.json`（r=2%，vol≈0.2）、`snapshot_stress.json`（r=5%，vol≈0.35） |

## 市场数据与校准规则

快照为本地 JSON（`market.LoadSnapshot`），包含标的出现价、贴现因子报价
（期限 → DF）与期权市场报价（到期、行权价、价格、方向）。

- **贴现曲线**：对 `log(DF)` 做分段线性插值（相邻期限间远期利率恒定），
  `t=0` 处锚定 `DF=1`，端点外平坦外推。
- **隐含波动率**：对每条期权报价用确定性二分法（200 次迭代上限）反解
  Black-Scholes 波动率；曲面为 (到期 × 行权价) 完整矩形网格上的双线性插值，
  网格外平坦外推。
- **可复现性**：校准是纯函数——排序确定、求解确定，相同输入逐位相同
  （`TestCalibrationDeterministic`）。

### 可区分的失败原因（`errors.Is` 判定）

| 错误 | 触发条件 |
|---|---|
| `market.ErrSnapshotInvalid` | 字段缺失/非正/非有限、贴现因子超出 (0,1]、买卖方向混杂 |
| `market.ErrQuoteMissing` | 利率或期权报价为空、曲面网格缺格 |
| `market.ErrQuoteDuplicate` | 相同期限的利率报价、相同 (到期, 行权价, 方向) 的期权报价 |
| `market.ErrTenorInversion` | 贴现因子随期限上升（期限倒挂） |
| `market.ErrVolUnsolvable` | 报价越出无套利边界 `[内在价值, 上界]` 或二分不收敛 |

任何失败都**不会**用默认值静默兜底。

## 估值方法适用范围

| 方法 | 适用 | 不适用 |
|---|---|---|
| `pricing.PriceAnalytic` | 欧式看涨/看跌 | 美式、路径依赖 → `ErrMethodNotApplicable` |
| `pricing.PriceTree` | 欧式与美式 | 参数越界 → `ErrParamOutOfRange` |
| `pricing.PriceMonteCarlo` | 欧式（GBM 终点采样） | 美式 → `ErrMethodNotApplicable` |

可区分的估值错误：

- `pricing.ErrMethodNotApplicable`：方法不适用于该工具；
- `pricing.ErrNotConverged`：蒙特卡洛在路径预算内标准误未降到容差以下
  （不返回零值冒充成功）；
- `pricing.ErrParamOutOfRange`：现货/行权价/波动率/期限/利率越界。

**方法间一致性**：同一欧式期权在三种方法下的差异须落在可解释容差内——
树模型为 O(1/n) 离散误差（n=800 时 < 1e-2），蒙特卡洛为 3 倍标准误
（`TestMethodsConsistent`）。蒙特卡洛的随机流只由 `MCConfig.Seed` 决定，
相同种子逐位可复现（`TestMCDeterministic`）。

## 并发与资源语义（`engine`）

`Engine.Run(ctx, reqs)` 批量执行请求，返回与请求一一对应的 `Outcome`：

- **并发上限**：`Limits.MaxConcurrency`（信号量控制，默认 4）。
- **路径预算**：`Limits.PathBudget` 按请求索引顺序为每个蒙特卡洛请求
  预分配最坏路径数（`MC.MaxPaths`），超预算的请求在启动前即以
  `engine.ErrBudgetExhausted` 明确拒绝；拒绝集合只由输入顺序决定，
  与 goroutine 调度无关。
- **取消/超时**：`ctx` 取消后未启动的请求标记为 `ctx.Err()`；已完成计算
  但上下文已取消的结果被丢弃——不写入缓存、不残留部分状态、不影响其他请求。
- **缓存**：键完整刻画估值输入（方法、工具参数、曲面波动率、曲线利率、
  种子、路径配置、树步数）；仅缓存成功且未被取消的结果。
- **可复现性**：相同快照 + 相同种子 + 相同并发布局 ⇒ 逐位相同结果；
  且不同并发上限下结果也逐位相同（`TestConcurrencyLayouts`）。

## 测试覆盖

- 不可解波动率（越无套利边界）→ `TestUnsolvableVol`
- 重复校准逐位一致 → `TestCalibrationDeterministic`
- 期限倒挂 / 报价缺失 / 重复报价 / 网格缺格 → `market` 包测试
- 方法间一致性（解析/树/MC）→ `TestMethodsConsistent`
- 未收敛 vs 提前收敛 → `TestMCNotConverged` / `TestMCConverges`
- 并发取消不污染缓存、超时语义 → `TestCancellation` / `TestTimeout`
- 预算拒绝确定性 → `TestBudgetExhausted`
- 不同并发布局结果一致 → `TestConcurrencyLayouts`
- 压力市场状态校准 → `TestStressSnapshot`

单测日志（`go test -v`）打印关键输入与判定依据（报价、边界、波动率、
标准误、预算记账等）。`go test -race ./...` 下无数据竞争。
