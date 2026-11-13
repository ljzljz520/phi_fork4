# Review：主会话模型共享额度（Shared Session Budget）

- **评审方式**：独立评审者（未参与实现）两轮只读评审 + 实现方修复 + 复核关闭
- **评审范围**：spec.md 14 条 AC（12 条 rule + 2 条 rubric）、tasks.md T1–T11 全部产物
- **最终结论**：**可交付**。12 条功能型 AC 全部 pass，AC-13/14 rubric 均 5/5，无阻断项

## AC 判定汇总

| AC | 判定 | 关键证据 |
|---|---|---|
| AC-1 未配置零变化 | pass | `budget.NewClient` nil 返回原客户端（client.go:17-19）；engine 不包装、无状态文件、无 budget 事件（controller/cmd 双侧测试）；budget 目录改为懒创建 |
| AC-2 原子预留/零网络拒绝 | pass | `Reserve` 锁内校验+登记，失败不改状态（budget.go:202-231）；80 并发限额 20 严格 20 成功（-race）；拒绝路径 inner 零调用 |
| AC-3 多退少补 | pass | `close` 按实际 CompletionTokens 结算、reserved 归零；remaining 断言测试 |
| AC-4 四类工作共享 | pass | 主代理/compact/子 job/重试均经同一包装；`TestEngineRunnerSharesParentBudget`、`TestRunCompactChargesSharedBudget`、`TestRetryHookReservesEachAttempt` |
| AC-5 取消/未知四档 | pass（二轮升级） | 有产出计估计、无产出释放输出、请求照计；预取消 ctx 经 `Release()` 全额释放不计请求（client.go:48-53, 133-136），专项测试覆盖 |
| AC-6 子 job 脱离上下文受限 | pass | `BudgetFn` Run 开头解析一次；第二 job failed 且错误含 budget exhausted，父子同一 ledger |
| AC-7 resume/切模型不重置 | pass | 状态文件 round-trip；resume 计数接续、new/fork 独立文件；SetModel 后 Budget 指针 Same |
| AC-8 耗尽停生成且对话可继续 | pass | 拒绝在 inner 调用前；assistant 条目不写入、stub 调用不增；session 历史完整 |
| AC-9 TUI 区分 | pass | 独立 OverlayBudgetAsk 面板（标题 Budget exhausted、两维度用量、仅 Copy/Dismiss、'r' no-op）；footer 90% 警示/耗尽 Destructive/nil 不渲染 |
| AC-10 headless 区分 | pass | ExitBudget=2；JSONL budget ready/exhausted 事件、error `kind:"budget_exhausted"`+快照、done.exitCode=2；stderr "budget exhausted:"；标志负数 ExitUsage |
| AC-11 压缩 usage 五路径 | pass | openai/responses/anthropic/gemini/extmodel 全部回填 CompactResult.Usage，各有解析测试；无 usage 走未知规则；PXB 无 wire 变更 |
| AC-12 原子持久化/损坏辨识 | pass（二轮消除并发保留） | CreateTemp 唯一名 + chmod 0644 + rename + defer 清理；32 goroutine 并发写无碰撞测试；损坏/版本不符/负计数明确报错不回零 |
| AC-13 架构内聚（rubric） | **5/5** | 单一 budget 包 + ChatClient 唯一包装收口；util 仅 context 钩子不反向依赖；nil 零包装零 goroutine；go.mod/go.sum 零 diff；deadcode 基线不变 |
| AC-14 可观测性（rubric） | **5/5** | TUI 面板/footer/JSONL/stderr 四通道数字同源 Snapshot（含 reserved 口径一致）；普通错误路径有对照测试；无密钥字段 |

## 首轮发现与修复闭环

| # | Severity | 问题 | 修复 | 复核 |
|---|---|---|---|---|
| 1 | P2 | SaveState 固定 `.tmp` 文件名，并发结算互相截断/ENOENT | `os.CreateTemp` 唯一名 + 同目录 rename，新增 32-writer 并发测试 | 关闭，无新缺陷 |
| 2 | P3 | FR-5(d) 的 `Release()` 在生产代码零接线，预取消 ctx 被误计 1 请求 | Stream/Compact 在 Reserve 后 inner 前检查 `ctx.Err()` 并 Release，inner 零调用；两个专项测试 | 关闭，幂等与无双重结算已验证 |
| 3 | P4 | `notify` 锁外读 onChange 字段，并发注册有 race 面 | 锁内取回调、锁外执行，与 onPersist 模式对齐 | 关闭，-race 干净 |
| 4 | P4 | 未配置 budget 也预建 `~/.phi/budget` 空目录 | ensureGlobalDirs 移除 BudgetBase，SaveState MkdirAll 懒建 | 关闭 |
| 5 | P4 | CHANGELOG 状态路径漏 cwd 编码分桶 | 改为 `~/.phi/budget/<encoded-cwd>/<session-id>.budget.json` | 关闭 |

## 验证记录

- `go test ./... -count=1`：79 包全部 ok，无失败行
- `go test -race`：budget / agent / tui/controller / cmd / project / util 全部通过，无 data race
- 新测试在 `-race -count=3` 下连跑三轮全 PASS
- `go vet ./...` 干净；`make fmt-check`、`make deadcode`（5 known baseline）、`make generate-check`（PXB 无漂移）通过
- `git diff --numstat go.mod go.sum` 空：无新增第三方依赖
- 残留观察（非缺陷）：activity spinner 激活时 footer 配额片段让位于活动标签，属既有槽位设计；真实 TUI 人工操作与 `phi run` 真机进程未执行，退出码由 runLoop 映射单测 + exit 常量覆盖

## 已知环境限制

- `make lint`（golangci-lint）在本机无法运行：golangci-lint 2.13.0 的 typechecker 不支持本机 Go 1.27.3 的导出数据格式（export data v5 > max v4），对标准库 import（os/strings）即报 typecheck 错误，与本次改动无关；`go clean -cache` 无效。CI 使用匹配工具链时应恢复通过。
