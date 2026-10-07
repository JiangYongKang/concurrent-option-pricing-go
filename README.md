# concurrent-option-pricing-go

本机可复现的并行期权定价与市场数据校准引擎。全部能力仅依赖 Go 标准库，
不依赖外部服务、真实凭据或联网运行时。

## 构建与验证

```bash
go build ./...        # 编译
go test ./...         # 单元测试与集成测试
go test -race ./...   # 竞态检测（要求无数据竞争）
go run ./cmd/gensnap  # 重新生成 testdata/snapshots 与 scenario/states（输出逐字节可复现）
go run ./cmd/scenario list   # 列出全部可用市场状态
go run ./cmd/scenario run -states normal,liquidity-crunch,price-gap
```

测试日志（`go test -v`）会打印关键输入与判定依据，例如失败报价的具体
期限/行权价/价格、无套利区间、蒙特卡洛标准误与容差，以及多状态对比中
每个标的在每个状态下的结论（priced / degraded / rejected）与原因。

## 包结构

| 包 | 职责 |
|---|---|
| `marketdata` | 本地行情快照（JSON）的加载与校验 |
| `calibration` | 贴现曲线与隐含波动率曲面的构建 |
| `pricing` | 解析解 / 二叉树 / 蒙特卡洛三类估值路径 |
| `engine` | 带并发上限、路径预算与超时控制的批量估值 |
| `scenario` | 市场状态注册表与跨状态并发对比估值（场景评估） |
| `scenario/states` | 内嵌的内置市场状态快照（`go:embed`） |
| `testdata/snapshots` | 本地行情快照样例（正常与各类异常） |
| `cmd/gensnap` | 确定性生成全部快照 |
| `cmd/scenario` | 列出状态 / 按名指定状态跑多状态对比 |

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
- **缓存**：仅缓存成功结果，键为 `(Spec, Method, Seed, Paths, Env)`。

## 多市场状态场景评估（`scenario`）

在单一环境批量估值之上，`scenario` 包支持把**同一批标的**一次放到多个
具名市场状态下并发估值，并按标的给出跨状态对比结果，用于回测与压测时
直接比较组合在不同市场状态下的差别。

### 内置市场状态

| 名字 | 含义 | 关键特征 |
|---|---|---|
| `normal` | 正常市场 | spot=100、利率约 3%、平值波动率约 22%、轻微微笑 |
| `liquidity-crunch` | 流动性急剧收紧 | 短端利率跳升至 6.2%；波动率整体 55%+，强下行偏斜（skew=-0.30） |
| `price-gap` | 价格大幅跳空 | 标的低开至 85、利率骤降至 0.8%；近月波动率 75%，随后期限快速回落至 40% |

状态以 `go:embed` 内嵌在 `scenario/states`，由 `cmd/gensnap` 与其它快照
一起确定性生成（报价由 Black-Scholes 闭式解产生，落在无套利区间内，
且总方差随期限非递减、可通过日历无套利校验）。重新运行
`go run ./cmd/gensnap` 输出逐字节一致，无任何外部依赖。

自定义状态可用 `Registry.RegisterJSON(name, desc, rawJSON)` 注入；
状态采用**懒加载**，未被选中的状态只保留原始字节，不占用校准结果与曲面内存。

### 按名选取，未知即拒

- `Registry.List()` / `Names()` 列出当前全部可用状态；
- `Runner.Run` 必须显式给出状态名；名字不存在时在启动任何估值**之前**
  返回包装了 `scenario.ErrUnknownState` 的错误，信息中附上全部可用状态名，
  **绝不悄悄退回任何默认状态**，也不产生部分结果。

### 跨状态并发的资源与隔离语义

一次 `Run` 对全部状态共用**一份**约束（`scenario.RunConfig`）：

- **并发**：`MaxWorkers` 是跨全部状态共享的唯一并发槽位池（信号量）；
  每个状态由独立 goroutine 串行领取槽位，状态排队等待槽位的时间不计入
  该状态的超时预算。
- **路径预算**：`PathBudget` 是整批（所有状态合计）的蒙特卡洛路径总数上限，
  按 `(状态顺序, 标的顺序)` 确定性分配（`engine.PlanRequest`），超出的请求
  在执行前以 `budget-exceeded` 拒绝；非 MC 请求不占预算。
- **超时**：默认按**每个状态各自**的上下文独立计时（`Timeout`），可用
  `TimeoutFor` 给个别状态单独的预算。**一个状态超时只取消该状态**，
  其余状态的上下文、作业与结果不受影响；等待槽位期间超时的状态不会去
  抢占其他状态仍需要的槽位。父 `ctx` 取消则取消全部状态。
- **隔离**：每个状态持有独立加载的快照、校准结果与**独立引擎缓存**；
  一个状态的中间结果、缓存、取消/超时都不可能污染另一个状态。每次 `Run`
  都重新构建每状态引擎，运行结束即不可达（连同缓存被回收），因此不存在
  跨次运行的半截状态。
- **可复现**：结果矩阵的每个单元格只被唯一的状态 goroutine 写入；随机性
  只来自请求自身 `Seed`。相同快照、相同种子、相同预算在任意 `MaxWorkers`
  布局（1/2/4/8）与重复运行下，价格、判定与原因逐位一致
  （见 `TestRunReproducible`）。
- **内存可控**：单状态成功缓存条目数受 `StateCacheLimit` 约束（默认 4096），
  满了按策略淘汰；占用只与“实际运行的状态数 × 上限”相关，不随注册状态数
  或请求数无限增长。

### 对比结果的判定分类

每个 `(标的, 状态)` 单元格是三态互斥的结论：

| Outcome | 含义 |
|---|---|
| `priced` | 首选方法算出可交付结果 |
| `degraded` | 首选方法失败，按降级策略改用备选方法后交付（记录触发原因与备选方法） |
| `rejected` | 被明确拒绝，不返回任何价格（绝不用零值或默认值冒充成功） |

拒绝/降级原因（`Reason`）分门别类，可用枚举直接区分：

| Reason | 触发情形 |
|---|---|
| `data-unavailable` | **该市场状态的数据本身缺失/非法**（空数据、JSON 坏、缺报价、重复、倒挂、数值非法）——该状态名下每个标的整列拒绝，绝不补齐、不默认 |
| `not-calibratable` | 数据合法但曲线/曲面无法校准（不可解波动率、日历套利） |
| `method-not-applicable` | 方法不适用于该工具（如美式用解析解/终端 MC） |
| `not-converged` | 采样预算用尽后蒙特卡洛标准误仍超容差 |
| `parameter-out-of-range` | 行权价/期限/波动率等参数越界 |
| `budget-exceeded` | 整批共享路径预算用尽，按序拒绝 |
| `canceled` / `timeout` | 该状态所属上下文被取消 / 该状态自身超时 |

降级策略（`Degrade`，`DefaultRunConfig()` 默认开启，仅对
`method-not-applicable` 与 `not-converged` 生效；备选方法一律是非 MC，
不额外消耗路径预算）：

- 解析解 → 二叉树（覆盖美式）；
- 二叉树 → 解析解（仅欧式）；
- 蒙特卡洛 → 欧式走解析解，美式走二叉树。

数据问题、预算问题、取消/超时、参数越界**永不降级**。`Report.Diag`
按状态给出 `priced/degraded/rejected` 计数、缓存条目数与加载诊断。

### 本机复现

```bash
go run ./cmd/scenario list
go run ./cmd/scenario run -states normal,liquidity-crunch,price-gap -workers 6
go run ./cmd/scenario run -states normal,ghost        # 未知状态：清晰拒绝并列出可选项，退出码 1
go run ./cmd/scenario run -states normal,price-gap -budget 100000 -no-degrade
go test -run 'TestRegistry|TestRun|TestShared|TestUnknown|TestBroken|TestStateData|TestTimeout|TestCancellation|TestDegrade|TestCache' -v ./scenario/
```

## 本地验证场景

测试覆盖（均可通过 `go test ./...` 复现）：

- 不可解波动率：`unsolvable_vol.json` 中价格低于无套利下界 →
  `ErrVolNotSolvable`
- 重复校准：同一快照两次校准，曲线与曲面逐点一致
- 方法间一致性：解析/树/MC 在容差内一致；美式 put ≥ 欧式 put
- 并发取消：取消不污染缓存、不干扰其他请求；超时受控；
  预算超出按序拒绝；`-race` 下无数据竞争
- 多市场状态（`scenario` 包）：
  - 状态列出/按名选取、未知状态整体拒绝不回退（`TestRegistryListAndGet`、
    `TestUnknownStateRejected`、`TestUnknownStateAbortsRun`）；
  - 坏状态（空数据/缺利率/不可解波动率）暴露为可区分的数据失败，
    整列拒绝且不牵连健康状态（`TestBrokenStateExposed`、
    `TestStateDataMissingColumn`）；
  - 跨状态并发隔离与逐位可复现（`TestRunCrossStates`、`TestRunReproducible`）；
  - 共享路径预算按 `(状态,标的)` 序确定性拒绝（`TestSharedPathBudget`）；
  - 单状态超时/取消不影响其他状态、无半截状态遗留
    （`TestTimeoutIsolation`、`TestCancellationIsolation`）；
  - 降级开启/关闭下 `degraded` 与 `rejected` 分类
    （`TestDegradeClassification`）；缓存受 `StateCacheLimit` 约束
    （`TestCacheBounded`）。
