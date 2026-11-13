# 主会话模型共享额度（Shared Session Budget）- 产品需求文档

## Overview

- **Summary**: 为一次主会话的全部模型工作增加可选的共享额度，维度为**请求数**与**输出令牌（completion tokens）**，统一约束主代理、并发子代理、压缩（compaction）请求与实际 HTTP 重试。额度在请求真正发出前原子预留、按最终用量结算；额度状态按主会话持久化，恢复会话与切换模型均不重置余额；耗尽时停止新生成但保留可继续的对话，并在 TUI 与 headless 两条链路上给出区别于普通错误的明确信号。
- **Purpose**: 现状只有 job 并发数（`MaxConcurrent=4`）、嵌套深度（`MaxDepth=1`）这类局部限制，工具回合上限 `max_rounds` 已在 0.29.8 移除；没有任何机制能够限制整项任务消耗的模型请求总数与输出令牌，长跑任务或并发子代理可能产生不可控费用。
- **Target Users**: 需要成本护栏的终端用户、CI/benchmark 使用者（headless）、需要在长跑会话中控制支出的 TUI 用户。

## Goals

- 一个主会话一个共享额度实例，主代理、子代理（含脱离单次工具上下文后仍在运行的 job）、压缩请求、HTTP 重试与引擎级重发全部计入同一余额。
- 请求发出前原子预留：请求数预留 1 次；输出令牌按该请求的输出上限（模型 `MaxTokens` 最坏值）预留，结算时按 provider 回报的实际 `CompletionTokens` 多退少补。
- 明确的取消 / 用量未知处理规则，且可测试。
- 额度状态按主会话 ID 持久化：恢复会话不重置；同会话内切换模型不重置。
- 额度耗尽时拒绝一切新的模型请求（不产生网络调用），已有对话与 session 文件保持完整、可继续。
- TUI 与 headless 都能把"额度耗尽"与普通运行错误区分开。
- 功能完全可选：不配置时行为、性能、产物与现状一致。

## Non-Goals

- 不做输入令牌 / 总令牌 / 费用金额维度的限额（仅请求数 + 输出令牌两个维度；输入与总量只做展示性记录，不参与拦截）。
- 不做跨会话的全局额度、按时间窗口刷新的额度、计费或价格表。
- 不做额度的远程/云端同步；状态文件仅本地持久化。
- 不恢复已移除的 `max_rounds` 工具回合预算，也不重新引入其配置项。
- 不限制工具执行本身（bash、读写等），只限制模型请求。
- `/fork` 产生的新会话不继承源会话余额（按新主会话处理，获得全新额度）。

## Background & Context

关键代码事实（均已核实）：

- 所有模型调用收敛于 `llm.ChatClient` 接口（[types.go](file:///Users/kkcarrot/swe-project/phi_fork4/internal/llm/types.go#L211-L214)）：`Stream` 承载主循环与子代理对话，`Compact` 承载压缩。内置实现 `internal/llm/client` 按 provider 分发；扩展模型经 `internal/llm/extmodel` 适配为同一接口。引擎在 [engine.go `newChatClient`](file:///Users/kkcarrot/swe-project/phi_fork4/internal/agent/engine.go#L78-L91) 构造客户端，并在 `rebindTools`/`SetModel` 时重建——这是包装客户端的唯一收口。
- 引擎主循环 [engine.go `Loop`](file:///Users/kkcarrot/swe-project/phi_fork4/internal/agent/engine.go#L250-L398)：每轮先 `runCompact` 再 `streamTurn`；溢出恢复会再次调用 `Stream`（引擎级重发自然产生新调用）。
- HTTP 重试在 [util/retry.go `DoWithRetry`](file:///Users/kkcarrot/spe-project/phi_fork4/internal/util/retry.go#L102-L153)（429/5xx、stale 连接，最多 3 次），位于所有内置 provider 的非流式 POST 与流式建连之下；扩展模型不走该路径。
- 子代理是**进程内** goroutine：[job/manager.go `Spawn`](file:///Users/kkcarrot/swe-project/phi_fork4/internal/job/manager.go#L146-L220) `go m.run(...)`，job 生命周期长于单次工具调用；[EngineRunner.Run](file:///Users/kkcarrot/swe-project/phi_fork4/internal/agent/engine_runner.go#L41-L111) 为每个 job 新建子 `Engine`，runner 由 `agent.NewJobManager` 统一构造，TUI 与 headless 各一个进程级 Manager。
- 会话持久化为 JSONL（[session/manager.go](file:///Users/kkcarrot/swe-project/phi_fork4/internal/session/manager.go)），恢复走 `OpenSession`；模型切换只追加 `ModelChangeEntry`，不改动历史。子代理 session 在 `~/.phi/jobs/<id>/session/`，与主会话是不同文件，因此额度状态不能只存 session JSONL。
- `llm.CompactResult` 当前只有 `Text/Truncated`，不带 usage；四个 provider 的非流式响应体本身都含 usage 字段，扩展模型 done 帧也带 `UsageJSON`（当前被丢弃）。
- headless：[cmd/runloop.go](file:///Users/kkcarrot/swe-project/phi_fork4/cmd/runloop.go) 消费同一个 `engine.Loop`，错误当前统一 `ExitError=1`；JSONL 事件 schema 在 `jsonlEncoder` 中显式定义。TUI 错误经 `LoopFinishedMsg.ErrorText` → `OverlayErrorAsk` 面板（Retry/Copy/Dismiss）。
- 配置体系：`config.yaml` → `project.ConfigDoc`（YAML，指针字段区分"缺省"与"零值"）→ `project.Config`；`phi run` 标志在 [cmd/run.go](file:///Users/kkcarrot/swe-project/phi_fork4/cmd/run.go) 定义。

## Functional Requirements

- **FR-1（配置）**: `config.yaml` 支持可选 `budget` 块：`max_requests` 与 `max_output_tokens` 两个非负整数，缺省或 0 表示该维度不限。`phi run` 支持 `--max-requests`、`--max-output-tokens` 标志，仅对本次运行生效并覆盖配置文件值。配置非法（负数）在加载/解析期报错。
- **FR-2（共享实例与传播）**: 每个主会话创建一个 `Budget` 实例并注入主引擎；进程级 job manager 的 runner 持有同一实例（或会话切换时的取数函数），子引擎创建时获得同一实例。子 job 在 `agent_wait` 超时、工具调用返回之后继续运行时，约束不解除。
- **FR-3（预留与结算）**: 每次 `Stream`/`Compact` 调用在任何网络 I/O 之前原子预留：请求数 +1、输出令牌按该请求输出上限估计（`ModelConfig.MaxTokens`；为 0 时用包内默认估计值）。拿到 provider usage 后按实际 `CompletionTokens` 结算输出、按实际 HTTP 尝试次数结算请求数，释放多占的预留。
- **FR-4（重试计入）**: `DoWithRetry` 的每一次实际重试尝试都必须先预留 1 个请求额度，预留失败则中止重试并返回额度耗尽错误；引擎级重发（溢出恢复等）作为新调用重新走完整预留。扩展模型无 HTTP 重试，按 1 次尝试结算。
- **FR-5（取消与未知用量）**:
  - provider 回报 usage：按实际值结算。
  - 已发出但无 usage 且**已收到部分输出**（用户取消、流中断、压缩成功但响应不含 usage）：输出按预留估计值计入（fail-closed，防止取消逃费），请求数按实际尝试次数计入。
  - 已发出但在任何产出之前失败（建连错误、4xx 等）：输出预留全额释放（计 0），请求数仍按尝试次数计入。
  - 预留成功但请求未能发出（构造失败、ctx 已取消等）：预留全额释放，不计请求数。
- **FR-6（压缩覆盖与 usage 补全）**: `llm.CompactResult` 增加 `Usage` 字段；OpenAI / OpenAI Responses / Anthropic / Gemini / extmodel 五条压缩路径尽可能填充真实 usage，填充不了时按 FR-5 未知规则处理。压缩的并行摘要（mid-turn cut 两个并发请求）各自独立预留，任一额度不足则该压缩请求失败并按耗尽错误上抛。
- **FR-7（持久化）**: 额度结算状态写入按主会话 ID 归属的本地状态文件（原子 temp+rename）。仅持久化已结算计数与限额，不持久化在途预留（进程退出后在途请求本就终止）。新进程恢复同一会话时加载该文件，余额接续；同进程切换模型不更换实例、不清零。配置限额在后续运行中被修改时，新限额生效、已结算累计保留。
- **FR-8（耗尽语义）**: 预留失败返回哨兵错误 `budget.ErrExhausted`（携带触顶维度、已用/限额信息），保证不发生任何网络调用。引擎让当前 loop 以该错误终止，失败的 assistant 消息不写入 session，已有历史完整保留；用户 dismiss 后可继续查看/复制对话，后续任何新提示在首次请求处再次快速失败。在途请求不强制中断（无法撤回已发送请求），其结算照常落盘。
- **FR-9（TUI 区分）**: 额度耗尽展示独立面板（标题与文案明确为额度耗尽，展示两维度已用/限额），不提供 Retry 动作（重试不可能成功），保留 Dismiss/Copy；普通错误面板保持现状。配置了额度时，footer 显示额度使用标签，接近/达到上限时用警示色。
- **FR-10（headless 区分）**: `phi run` 因额度耗尽终止时返回专用退出码 `2`（普通错误仍为 1，用法错误仍为 3）；`--jsonl` 的 error 事件带 `kind:"budget_exhausted"` 并附额度快照，stderr 打印明确的耗尽文案；非 jsonl 模式同样输出可区分的 stderr 文案。配置了额度时输出额度状态事件（启动时限额、耗尽时快照）。
- **FR-11（可观测性）**: 额度对象提供快照（限额、已结算、在途预留、剩余、是否耗尽），供 footer、TUI 面板、headless JSONL 复用；计数口径全局唯一。

## Non-Functional Requirements

- **NFR-1（零配置零成本）**: 未配置 budget 时不得新增每请求开销（无包装、无磁盘 I/O、无新 goroutine），现有测试与行为不变。
- **NFR-2（并发安全）**: 主代理与最多 4 个并发子 job、并行压缩摘要会并发访问同一 Budget；预留/结算必须在锁内原子完成，`go test -race` 下无数据竞争。
- **NFR-3（ lean ）**: 不新增第三方依赖；新包保持小而内聚，导出面最小；遵循 `make fmt`（120 列）与现有包命名风格。
- **NFR-4（持久化健壮性）**: 状态文件写入必须原子（temp+rename）；损坏/截断文件按明确错误上报，不得静默清零余额。
- **NFR-5（可测试性）**: 核心额度逻辑不依赖真实网络，使用 fake `ChatClient` 与可控重试即可完整验证；测试沿用 testify。
- **NFR-6（兼容性）**: session JSONL 格式不变；`CompactResult` 新增字段不破坏现有 provider 测试；扩展模型协议（PXB）无 wire 变更。

## Constraints

- **Technical**:
  - 工具循环约束：ExtensionPre → Gate/Ask → Run → ExtensionPost 不得绕过；额度不是权限 gate，位于模型客户端层。
  - `internal/util` 为底层包，不得反向依赖新预算包；重试钩子只能以 util 定义的 context 机制暴露。
  - PXB payload 由 schema 生成，不得手改 `msg_gen.go`；本特性不涉及 PXB 变更。
  - 配置保存走 `ConfigDoc` 指针字段模式，保证"缺省"与显式 0 可区分。
- **Business**: 用户可见行为变更必须更新 `CHANGELOG.md` 的 `## [Unreleased]`。
- **Dependencies**: 无新增依赖；复用 `session`/`project`/`job`/`llm` 现有结构。

## Assumptions

- 额度归属单位是**主会话**：TUI 中一次 `/new`、一次 resume、一次 fork 各自独立；headless 每次进程按其打开的会话 ID 归属。
- 输出令牌预留取 `ModelConfig.MaxTokens`；该值为 0（未知）时使用预算包内默认估计常量（实现时确定并在代码注释说明口径）。
- 请求数口径为"实际 HTTP 尝试次数"：逻辑调用 1 次 + 其触发的 HTTP 重试次数；SSE 建连成功后的流中断不触发 HTTP 重试，计 1 次。
- 状态文件目录复用 `~/.phi` 布局，按 cwd 编码分桶，与 session 目录布局平行；文件名含主会话 ID，session 列表只扫描 `*.jsonl`，不受影响。
- 切换模型后新模型的 `MaxTokens` 改变只影响后续请求的预留估计，已结算余额不变。
- 额度耗尽不属于"可重试错误"：TUI 面板不给 Retry；用户若想继续，需调整配置（提高限额）后重启/恢复会话。

## Acceptance Criteria

### AC-1: 未配置额度时行为零变化
- **Type**: `rule`
- **Given**: 配置文件与 CLI 均未提供任何 budget 限额
- **When**: 以 TUI 或 `phi run` 发起任意会话（含子代理、压缩、重试）
- **Then**: 引擎不构造额度包装客户端、不产生额度状态文件、不发布额度事件，全部现有测试不变更即通过
- **Pass Condition**: 代码中 budget 注入点对 nil Budget 直接返回原客户端；全量 `go test ./...` 通过；工作区无新增状态文件
- **Evidence**: 相关单测 + `go test ./...` 输出 + 文件系统检查

### AC-2: 请求发出前原子预留，超额拒绝且无网络调用
- **Type**: `rule`
- **Given**: 限额为请求数 2、输出令牌 10000 的 Budget，fake ChatClient 记录调用次数
- **When**: 并发发起超过限额的 Stream/Compact 调用
- **Then**: 恰好 2 个调用进入 fake 客户端，其余直接返回 `budget.ErrExhausted`，且被拒调用未触发任何 fake 调用
- **Pass Condition**: 并发测试（`-race`）中 fake 调用计数 ≤ 预留成功数；所有拒绝可被 `errors.Is(err, budget.ErrExhausted)` 识别
- **Evidence**: `internal/budget` 并发单测

### AC-3: 按最终用量结算，预留多退少补
- **Type**: `rule`
- **Given**: 一次请求按 MaxTokens=8000 预留，实际回报 `CompletionTokens=500`
- **When**: 流正常结束并结算
- **Then**: 已结算输出增加 500，在途预留清零，剩余额度 = 限额 - 500；后续请求可重新使用释放的 headroom
- **Pass Condition**: 单测断言结算前后快照的 reserved/settled/remaining 数值
- **Evidence**: budget 包结算单测

### AC-4: 四类模型工作共享同一额度
- **Type**: `rule`
- **Given**: 配置了限额的主会话，主代理产生对话、触发压缩、spawn 子代理并制造一次 HTTP 429 重试
- **When**: 上述工作全部完成
- **Then**: 同一 Budget 实例的请求计数 = 主代理调用数 + 压缩调用数 + 子代理调用数 + 实际重试尝试次数，输出计数覆盖全部调用的 completion tokens
- **Pass Condition**: 集成测试用 fake/httptest 服务驱动四条路径，断言单一 Budget 快照计数；子引擎与主引擎持有同一指针
- **Evidence**: agent 包集成测试 + budget client 包装测试

### AC-5: 取消与用量未知按规则处理
- **Type**: `rule`
- **Given**: 分别构造（a）收到部分 delta 后取消、（b）建连即失败无任何产出、（c）Compact 成功但无 usage 且有文本、（d）预留成功后请求未发出
- **When**: 各场景结束
- **Then**: (a)(c) 按预留估计计输出且计请求数；(b) 输出计 0 但计请求数；(d) 预留全额释放且不计请求数
- **Pass Condition**: 四个子场景各有单测断言结算快照与预留归零
- **Evidence**: budget 包未知用量规则单测

### AC-6: 子任务脱离单次工具上下文后仍受约束
- **Type**: `rule`
- **Given**: 主代理 spawn 一个长耗时子 job，`agent_wait` 超时返回后 job 仍在运行
- **When**: 子 job 继续发起模型请求直至额度耗尽
- **Then**: 子 job 的新请求被同一 Budget 拒绝并以 `ErrExhausted` 失败，job 落盘为 failed 且错误信息标明额度耗尽；主会话余额与子 job 所见余额一致
- **Pass Condition**: engine_jobs/runner 测试断言 job 失败原因 `errors.Is ErrExhausted`，且父子快照计数连续
- **Evidence**: agent 包子代理额度测试 + job meta 错误断言

### AC-7: 恢复会话与切换模型不重置余额
- **Type**: `rule`
- **Given**: 会话已消耗部分额度并完成状态文件落盘
- **When**: （a）新进程 resume 同一会话；（b）同会话内切换模型后再发请求
- **Then**: (a) 加载后的已结算计数与落盘一致，剩余额度接续；(b) 切换前后 Budget 为同一实例、计数不变，新请求按新模型 MaxTokens 预留
- **Pass Condition**: 持久化 round-trip 单测；切换模型测试断言指针同一性与计数不变
- **Evidence**: budget 持久化测试 + agent SetModel 测试

### AC-8: 耗尽后停止新生成且对话可继续
- **Type**: `rule`
- **Given**: 额度已耗尽的会话，session 中保留完整历史
- **When**: 用户再次提交提示
- **Then**: 首个模型请求在网络调用前被拒，loop 终止且不向 session 写入失败的 assistant 条目；重新打开/恢复该会话后历史消息完整可读
- **Pass Condition**: 引擎测试断言 session 条目数不增加、fake 客户端零调用；resume 后快照消息数与耗尽前一致
- **Evidence**: agent 耗尽测试 + session 条目计数

### AC-9: TUI 明确区分额度耗尽与普通错误
- **Type**: `rule`
- **Given**: 分别注入普通 loop 错误与 `ErrExhausted`
- **When**: LoopFinished 消息到达 Editor
- **Then**: 普通错误显示现有 Error 面板（含 Retry）；耗尽显示独立面板，标题/文案标明额度耗尽、展示两维度用量且无 Retry 动作；配置额度时 footer 出现额度标签并在耗尽时呈警示色
- **Pass Condition**: overlays/editor 单测断言两种面板类型与动作集合不同；footer 标签快照测试
- **Evidence**: TUI 相关 `*_test.go`

### AC-10: headless 以退出码与事件区分耗尽
- **Type**: `rule`
- **Given**: headless 运行中额度耗尽
- **When**: 进程退出
- **Then**: 退出码为 2；`--jsonl` 输出含 `kind:"budget_exhausted"` 的 error 事件与额度快照，stderr 文案含明确耗尽提示；普通错误退出码仍为 1 且无该 kind
- **Pass Condition**: runLoop 错误映射单测覆盖两种错误；JSONL schema 断言字段存在且固定
- **Evidence**: cmd 包测试

### AC-11: 压缩请求计入额度且 usage 尽量真实
- **Type**: `rule`
- **Given**: 触发一次自动压缩（含 mid-turn 双并发摘要场景）
- **When**: 压缩完成
- **Then**: 压缩调用经过预算包装并预留/结算；五个 Compact 实现（4 provider + extmodel）在响应含 usage 时回填 `CompactResult.Usage`，不含时走未知用量规则
- **Pass Condition**: 各 provider compact 测试断言 usage 解析；引擎压缩测试断言 Budget 计数增长
- **Evidence**: llm 各子包测试 + agent 压缩测试

### AC-12: 状态文件原子持久化且损坏可辨识
- **Type**: `rule`
- **Given**: 结算触发落盘；另构造一份截断/损坏的状态文件
- **When**: 正常落盘后加载；以及加载损坏文件
- **Then**: 正常路径文件为完整 JSON（temp+rename，无半截文件）；损坏文件返回明确错误而非零值 Budget
- **Pass Condition**: 持久化测试断言文件内容与原子写实现；损坏文件测试断言错误返回
- **Evidence**: budget 持久化单测

### AC-13: 设计内聚与 lean 程度
- **Type**: `rubric`
- **Dimension**: 架构内聚性（收口位置、包依赖方向、导出面、零新增依赖）
- **Scale**: 1-5
- **Anchors**: 1 = 额度逻辑散落多个包、util 反向依赖、重复计数口径；3 = 基本收口但存在两处以上计数/预留逻辑或多余配置面；5 = 单一 Budget 包 + 客户端包装收口，所有调用方只依赖同一接口，无新第三方依赖，nil 禁用路径零开销
- **Pass Threshold**: >= 4
- **Evidence**: 代码结构审查、`go.mod` 无变化、deadcode/lint 通过

### AC-14: 可观测性与文案质量
- **Type**: `rubric`
- **Dimension**: 耗尽信号在 TUI/footer/JSONL/stderr 四处的清晰度与口径一致性
- **Scale**: 1-5
- **Anchors**: 1 = 仅一个通用错误文案；3 = 有区分但维度信息不全或口径不一致；5 = 四处均明确标注耗尽维度、已用/限额，数字口径与 Budget 快照完全一致，普通错误路径文案不受影响
- **Pass Threshold**: >= 4
- **Evidence**: 面板/标签/JSONL 快照测试与人工文案核对

## Open Questions

无（四个关键决策已与用户确认：MaxTokens 最坏值预留、未知用量分档处理、config.yaml+CLI 双入口、headless 退出码 2 + JSONL kind）。
