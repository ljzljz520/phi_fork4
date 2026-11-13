# 主会话模型共享额度（Shared Session Budget）- 实施计划

依赖顺序：T1 → T2；T3、T4、T7 可并行；T5 依赖 T1+T3；T6 依赖 T5；T8 依赖 T2+T6+T7；T9 依赖 T8；T10 依赖 T6+T7；T11 收尾。

## Task 1: 新建 internal/budget 核心包（额度模型、预留与结算）
- **Status**: `done`
- **Completion Evidence**: internal/budget/budget.go 实现 Limits/Snapshot/Reservation/Budget，并发 -race 测试通过（budget_test.go）
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 新建 `internal/budget/budget.go`：定义 `Limits{MaxRequests, MaxOutputTokens int}`（0=不限）、`Snapshot`（限额、已结算 requests/output、在途预留、剩余、Exhausted）、请求类型常量（chat/compact）、哨兵 `ErrExhausted`（可 `errors.Is`，携带触顶维度与快照）。
  - `Budget` 用 mutex 保护，提供 `Reserve(kind, outputEstimate int) (Reservation, error)`：锁内原子校验请求数与输出令牌 headroom，不足返回 `ErrExhausted` 且不改变状态；通过则登记在途预留。
  - `Reservation` 提供 `Settle(usage llm.Usage, attempts int)`、`SettleUnknown(producedOutput bool, attempts int)`、`Release()`、`ReserveAttempt() error`（重试钩子用），保证一次性结算（重复调用无副作用）。
  - 结算规则：实际 usage 按 `CompletionTokens` 多退少补；未知且有产出按预留估计计、无产出计 0；请求数按 attempts（≥1）计；Release 不计请求、全额退还预留。
  - 预留估计口径：`ModelConfig.MaxTokens`，为 0 时用包内默认估计常量（注释说明）。
  - 可选变更回调 `OnChange(func(Snapshot))`，非阻塞、nil 安全，供后续 UI 接线。
- **Acceptance Criteria Addressed**: AC-2, AC-3, AC-5
- **Test Requirements**:
  - `rule` TR-1.1: 并发 Reserve 下成功数严格等于限额，失败者全部 `errors.Is ErrExhausted` 且状态未被修改；`go test -race` 通过（证据：budget 包测试输出）
  - `rule` TR-1.2: 预留 8000、实际结算 500 后 reserved 归零、settled=500、remaining 正确恢复（证据：结算单测断言）
  - `rule` TR-1.3: 未知用量四分支（有产出/无产出/未发出/重试）结算结果符合 FR-5（证据：表驱动单测）
  - `rule` TR-1.4: 零值 Limits（两个维度均 0）时任意 Reserve 成功，行为等同不限额（证据：单测）
- **Notes**: 不引第三方依赖；testing/testify 仅出现在 `*_test.go`。

## Task 2: Budget 本地持久化（状态文件）
- **Status**: `done`
- **Completion Evidence**: persist.go（SaveState temp+rename、LoadState、OpenOrCreate、BindPersistence 仅结算落盘）；project.go BudgetBase/BudgetDir/BudgetFile；round-trip/损坏文件/新限额测试通过
- **Priority**: high
- **Depends On**: T1
- **Description**:
  - 新建 `internal/budget/persist.go`：状态 JSON（version、sessionID、limits、settled requests/output、updatedAt），`Save` 走 temp+rename 原子写；`Load` 解析并重建 Budget（在途预留不持久化，加载后为零）。
  - Budget 在每次结算后触发一次落盘（预留变动不落盘）；落盘失败通过可注入的错误回调上抛，不吞错。
  - 在 `internal/project` 增加额度目录布局（`GlobalLayout.BudgetBase()` + `Project.BudgetDir()`/`BudgetFile(sessionID)`，与 session 目录的 cwd 编码分桶平行）。
  - 损坏/截断文件返回明确错误；限额配置与文件不一致时以新限额为准、累计计数保留。
- **Acceptance Criteria Addressed**: AC-7, AC-12
- **Test Requirements**:
  - `rule` TR-2.1: 结算→落盘→新 Budget Load，快照计数一致且 reserved 归零（证据：round-trip 单测）
  - `rule` TR-2.2: 写入路径无半截文件（temp+rename）；损坏文件 Load 返回错误而非零值 Budget（证据：持久化单测）
  - `rule` TR-2.3: 以不同 Limits 加载同一文件，新限额生效、旧累计保留（证据：单测断言）

## Task 3: util 重试钩子（按实际 HTTP 尝试预留）
- **Status**: `done`
- **Completion Evidence**: util/retry.go WithRetryHook/RetryHookFromContext，attempt>0 发请求前触发，util 不依赖 budget；retry 测试通过
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 在 `internal/util/retry.go` 增加 context 机制（如 `WithRetryHook(ctx, func(attempt int) error)` 与读取函数），`DoWithRetry` 在每次**重试尝试**发起前调用钩子；钩子返回错误时中止并返回该错误（包装保留可识别性）。
  - util 不依赖 budget 包；钩子类型定义在 util 内。
  - 首次尝试不触发钩子（其预留由上层 Reserve 完成），仅 attempt≥1 的重试触发。
- **Acceptance Criteria Addressed**: AC-4
- **Test Requirements**:
  - `rule` TR-3.1: 构造前两次 5xx、第三次成功的 httptest 服务，钩子被调用恰好 2 次且 attempt 序号正确（证据：retry 单测）
  - `rule` TR-3.2: 钩子返回错误时 DoWithRetry 立即中止并透传该错误，不再发起后续尝试（证据：单测）
  - `rule` TR-3.3: 无钩子 context 时行为与现状完全一致，现有 retry 测试不修改即通过（证据：测试输出）

## Task 4: CompactResult 携带 usage 并回填五条压缩路径
- **Status**: `done`
- **Completion Evidence**: llm/types.go CompactResult.Usage+UsageReported；openai/anthropic/gemini/responses/extmodel 五路径回填，各有用例，go test ./internal/llm/... 通过
- **Priority**: high
- **Depends On**: None
- **Description**:
  - `internal/llm/types.go` 的 `CompactResult` 增加 `Usage Usage` 字段（不改变 wire/session 格式）。
  - OpenAI chat completions、OpenAI Responses、Anthropic、Gemini 四个非流式 CompactRequest 解析响应体 usage 填入；extmodel `Compact` 从 done 帧 accumulator 取 usage 填入。
  - 响应缺 usage 时保持零值，由上层未知规则处理。
- **Acceptance Criteria Addressed**: AC-11
- **Test Requirements**:
  - `rule` TR-4.1: 五个实现各有 httptest/假帧测试断言 `CompactResult.Usage` 被正确解析（含缓存分桶字段时不断言错误口径）（证据：各 llm 子包测试）
  - `rule` TR-4.2: 响应不含 usage 时返回零值 Usage 且不报错（证据：单测）

## Task 5: budget ChatClient 包装（统一收口）
- **Status**: `done`
- **Completion Evidence**: internal/budget/client.go NewClient（nil 返回原客户端）、Stream/Compact 预留结算与 sync.Once、重试钩子接入；client_test.go 全分支覆盖
- **Priority**: high
- **Depends On**: T1, T3
- **Description**:
  - 新建 `internal/budget/client.go`：`NewClient(inner llm.ChatClient, b *Budget, estimate int) llm.ChatClient`；b 为 nil 时直接返回 inner（零开销禁用路径）。
  - `Stream`：先 Reserve（失败则返回仅产出 `ErrExhausted` 的 error seq，无任何 inner 调用）；将 `ReserveAttempt` 挂到 util 重试钩子 context；消费流事件时跟踪"是否收到任何产出"与最后 usage；done 按实际 usage 结算，错误/取消/consumer 停止按未知规则结算；`sync.Once` 保证恰好结算一次。
  - `Compact`：同构；成功且 `result.Usage.Reported()` 按实际结算，否则按"有文本=有产出"走未知规则；返回错误且无产出时输出计 0、请求照计。
  - 预留成功但 inner 调用前即失败（构造错误/ctx 已取消）走 Release。
- **Acceptance Criteria Addressed**: AC-2, AC-3, AC-4, AC-5, AC-11
- **Test Requirements**:
  - `rule` TR-5.1: fake ChatClient 测试：耗尽时 inner 零调用、错误为 `ErrExhausted`；正常/取消/流错误/压缩无 usage 各路径结算快照正确（证据：client 单测）
  - `rule` TR-5.2: 重试钩子在模拟 HTTP 重试时为每次尝试预留，尝试次数计入请求结算（证据：结合 T3 钩子的单测）
  - `rule` TR-5.3: nil Budget 时返回原客户端且类型/行为不变（证据：单测 + 现有客户端测试不回归）

## Task 6: agent 引擎接线与子代理共享传播
- **Status**: `done`
- **Completion Evidence**: WithBudget/engine.budget/newChatClient 包装、SetModel 保持指针、EngineRunner.BudgetFn、NewJobManager 5 参；engine_budget_test.go 与 engine_runner_test.go 覆盖耗尽/共享/压缩/nil
- **Priority**: high
- **Depends On**: T5
- **Description**:
  - `internal/agent`：新增 `WithBudget(*budget.Budget)` EngineOption 与 engine 字段；`newChatClient` 用 budget 包装产物（nil 不包装）；`SetModelWithHooks` 重建客户端时保持同一 Budget 指针。
  - `EngineRunner` 增加 Budget 取数函数（仿 `ExtensionsFn`，在 `Run` 开始时解析一次并传入子引擎 `WithBudget`），使进程级 job manager 在会话切换后仍能取到当前额度，且已启动 job 绑定启动时的实例。
  - `agent.NewJobManager` 扩展预算参数（同步修改 controller 与 runloop 两个调用点）。
  - `Loop`/`runCompact` 让 `ErrExhausted` 作为普通 loop error 上抛但不写入失败 assistant 条目（沿用现有错误路径，验证 session 不被污染）。
- **Acceptance Criteria Addressed**: AC-1, AC-4, AC-6, AC-7, AC-8
- **Test Requirements**:
  - `rule` TR-6.1: 引擎测试：小额 Budget 下 loop 在耗尽时终止、fake 客户端不再被调用、session 条目数不增加，且错误可 `errors.Is ErrExhausted`（证据：engine 测试）
  - `rule` TR-6.2: 子代理测试：父子引擎持同一 Budget 实例；模拟 wait 超时后子 job 继续运行直至耗尽，job 失败信息含耗尽错误（证据：engine_jobs/runner 测试）
  - `rule` TR-6.3: SetModel 前后 Budget 指针同一、计数不变；nil Budget 时现有全部 engine 测试不修改即通过（证据：测试输出）
  - `rule` TR-6.4: 压缩路径断言 Budget 计数随 Compact 调用增长（证据：压缩相关 engine 测试）

## Task 7: project 配置解析（config.yaml budget 块）
- **Status**: `done`
- **Completion Evidence**: ConfigDoc.Budget 指针字段 + Config.BudgetConfig + Limits(overrides)/Enabled，校验负数、round-trip 不凭空写出；budget_config_test.go 7 用例通过
- **Priority**: high
- **Depends On**: None
- **Description**:
  - `ConfigDoc` 增加 `Budget *BudgetDoc`（`max_requests`/`max_output_tokens` 用指针保持缺省语义），`Config` 增加 `BudgetConfig`；`parseConfigFileWithEnv` 解析并校验非负。
  - `ConfigDoc.Save` round-trip 保留该块；`phi config` 编辑路径不丢失字段。
  - 提供配置→`budget.Limits` 的转换辅助。
- **Acceptance Criteria Addressed**: FR-1（支撑 AC-1/AC-10 的配置前提）
- **Test Requirements**:
  - `rule` TR-7.1: 含 budget 块的 YAML 解析正确；缺省块得到零值（不限）；负数报错（证据：project 包单测）
  - `rule` TR-7.2: Read→Save→Read round-trip 后 budget 字段保持，缺省键不被凭空写出（证据：configdoc 测试）

## Task 8: TUI controller 额度生命周期与消息
- **Status**: `done`
- **Completion Evidence**: controller.go openBudget/attachBudget 接入 openEngine/newEngine/fork，OnChange 发 BudgetMsg（切走静默），finishLoop 标记 BudgetExhausted 且禁 Retry；msg.go 新消息体；editor 存快照；controller_budget_test.go + editor/budget_test.go 通过（-race）
- **Priority**: high
- **Depends On**: T2, T6, T7
- **Description**:
  - `EngineController` 持有当前 Budget：启动时按主会话 ID 创建/加载；Resume 加载对应状态文件；NewSession/Fork 创建新实例；SetModel 不更换。
  - 通过 `OnChange` 回调发布新 `BudgetMsg{Snapshot}`（controller/msg.go），Editor 存储最新快照。
  - `LoopFinishedMsg` 增加耗尽标记与快照（由 `errors.Is(loopErr, budget.ErrExhausted)` 识别），保持普通错误路径不变。
  - `newEngine`/job runner 取数函数接入 Budget；session 切换时旧 Budget 的落盘已在结算时完成。
- **Acceptance Criteria Addressed**: AC-7, AC-8, AC-9
- **Test Requirements**:
  - `rule` TR-8.1: controller 测试：resume 后 Budget 累计接续；new/fork 获得新实例；SetModel 不重置（证据：controller 测试，可用临时目录状态文件）
  - `rule` TR-8.2: 注入 `ErrExhausted` 时 LoopFinished 带耗尽标记，普通错误不带（证据：controller_error 测试扩展）
  - `rule` TR-8.3: Budget 结算后 Editor 收到 BudgetMsg 且快照与 Budget 一致（证据：bus/消息测试）

## Task 9: TUI 耗尽面板与 footer 额度标签
- **Status**: `done`
- **Completion Evidence**: overlays OverlayBudgetAsk/Dismiss + budget 模式 errorAskState（标题/用量行/仅 Copy+Dismiss，r 键 no-op，overlays/budget_test.go）；footer formatBudgetLabel/budgetPressureStyle（90% 警示/耗尽 Destructive/nil 不渲染，footer/budget_test.go）；editor 按 BudgetExhausted 路由（editor/budget_test.go）；全部 TUI 包测试通过
- **Priority**: high
- **Depends On**: T8
- **Description**:
  - overlays 新增额度耗尽面板：标题与文案明确（如 "Budget exhausted"），展示两维度已用/限额，动作仅 Dismiss/Copy（无 Retry）；与普通 Error 面板是不同 Overlay 分支。
  - Editor 按 LoopFinished 耗尽标记路由到新面板；dismiss 后对话保持可浏览，后续提示再次耗尽仍弹该面板。
  - footer 增加额度标签（如 `budget 12/500 req · ↓45.0k/200k`），接近上限警示色、耗尽 Destructive 色；未配置额度时不渲染。
- **Acceptance Criteria Addressed**: AC-9, AC-14
- **Test Requirements**:
  - `rule` TR-9.1: 耗尽面板与普通错误面板的动作集合不同（无 Retry）且文案含维度用量（证据：overlays 测试）
  - `rule` TR-9.2: footer 标签格式化/配色表驱动测试；零配置不显示（证据：footer tokens 测试扩展）
  - `rubric` TR-9.3: 文案清晰度与口径一致性；scale 1-5；anchors 1=通用错误文案，3=有区分但维度不全，5=维度齐全且数字与快照一致；threshold >= 4；证据：面板/标签快照测试

## Task 10: headless 接线（CLI 标志、退出码 2、JSONL 区分）
- **Status**: `done`
- **Completion Evidence**: exit.go ExitBudget=2；run.go --max-requests/--max-output-tokens（-1 未提供、负数 UsageError、0=不限）；runloop.go 按 sess.ID 经 OpenOrCreate+BindPersistence 建账、WithBudget 注入引擎、jobs 闭包共享；runLoop 区分 ErrExhausted（stderr "budget exhausted:"+退出码 2）；JSONL budget 事件（ready/exhausted）+ error kind=budget_exhausted + snapshot，done.exitCode=2；run_budget_test.go 覆盖（含耗尽不发请求、无预算旧契约）
- **Priority**: high
- **Depends On**: T6, T7
- **Description**:
  - `cmd/run.go` 增加 `--max-requests`、`--max-output-tokens`（非负整数，覆盖配置文件）；`runloop.go` 构建 Budget（按打开的会话 ID 归属状态文件）并注入引擎与 job manager。
  - `cmd/exit.go` 增加 `ExitBudget = 2`；`runLoop` 对 `errors.Is(err, budget.ErrExhausted)` 映射退出码 2 与明确 stderr 文案，普通错误仍为 1。
  - JSONL：error 事件增加可选 `kind`（`budget_exhausted`）与额度快照字段；新增 `budget` 事件（启动发限额、耗尽发快照）；`done` 事件退出码反映 2。更新 `phi run` 帮助中的退出码说明。
- **Acceptance Criteria Addressed**: AC-10, AC-14
- **Test Requirements**:
  - `rule` TR-10.1: 错误映射单测：耗尽→2、普通错误→1、用法错误→3（证据：cmd 包测试）
  - `rule` TR-10.2: JSONL schema 测试：耗尽 error 事件含 kind 与快照，普通 error 无 kind；budget 事件字段固定（证据：jsonl encoder 测试）
  - `rule` TR-10.3: 标志解析测试：负数/非法值报 usage 错误，标志覆盖配置值（证据：run flag 测试）

## Task 11: 文档、CHANGELOG 与全量验证
- **Status**: `done`
- **Completion Evidence**: CHANGELOG Unreleased/Added 条目；README budget 配置块+Session budget 小节+layout+headless 标志/退出码 2/JSONL；go.mod/go.sum 零变化；make fmt/fmt-check/deadcode/generate-check 通过；go vet 干净；go test ./... 79 包全绿；关键包 -race 通过（golangci-lint 因本机 Go1.27.3 导出数据 v5 不兼容无法 typecheck，环境问题，与改动无关）
- **Review**: review.md 两轮独立评审；首轮 5 项发现（P2 并发 temp 碰撞/P3 Release 未接线/P4 notify 锁/懒创建/文案）已全部修复并复核关闭；AC-13/14 rubric 终评 5/5，整体可交付
- **Priority**: medium
- **Depends On**: T9, T10
- **Description**:
  - `CHANGELOG.md` 的 `## [Unreleased] / ### Added` 记录共享额度特性（配置块、CLI 标志、退出码 2、TUI 面板）。
  - README 配置章节补充 `budget` 块示例与语义；如有其他配置参考一并同步。
  - 全量运行 `make fmt`、`make check`（fmt-check/lint/deadcode/generate-check）、`go test ./...` 与关键包 `go test -race`。
- **Acceptance Criteria Addressed**: AC-1, AC-13, AC-14
- **Test Requirements**:
  - `rule` TR-11.1: `make check` 与 `go test ./...` 全部通过，无遗留告警（证据：命令输出）
  - `rule` TR-11.2: CHANGELOG/README 含新配置块与退出码说明，措辞与实现一致（证据：文档 diff）
  - `rubric` TR-11.3: 架构内聚度；scale 1-5；anchors 见 spec AC-13；threshold >= 4；证据：包结构/依赖方向审查、go.mod 无新增依赖
