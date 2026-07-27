# 04A - Dedicated Cline Hub per Fresh Run：代码修改计划

**状态：** 实施中；使用公开 Cline Hub，不修改 Cline（2026-07-25）

**上位设计：** [`04-cline-dedicated-hub-per-run.md`](./04-cline-dedicated-hub-per-run.md)

**定位：** 本文只负责把方案 04 拆成可执行的代码改动，不引入新的会话发现方案。任何冲突都以方案 04 为准。

---

## 1. 实施目标

把当前 Cline 适配器从：

```text
临时 --data-dir
  -> 复制 provider settings
  -> 任务结束后扫描 session JSON
  -> 按分数选择 SessionID
```

fresh run 改成：

```text
每次 fresh Execute 创建专属 Hub
  -> 使用持久 Cline 状态和强制 Hub backend
  -> 启动前获取 history 快照
  -> 启动 NDJSON task 并冻结首条协议时间
  -> 用 history delta + Hub PID + root/cwd/timestamp 精确匹配
  -> 唯一候选才提前上报并持久化 SessionID
  -> task drain 后关闭专属 Hub
```

resume run 是更短的分支：

```text
Multica 已传入 ResumeSessionID
  -> 直接使用 Cline 长期驻留的共享 Hub
  -> 直接用 --id <ResumeSessionID> 启动 NDJSON task
  -> 实时上传 Cline 事件
  -> task drain 后结束 CLI，不关闭共享 Hub
```

resume 分支完全跳过 pre-history、SessionID timestamp window、candidate
matching、early lookup、final lookup，以及 private Hub 的目录、端口、启动、
readiness、stop 和 PID cleanup。已知 SessionID 加 exact
workdir 已经提供确定身份，不需要专属 Hub PID。

实现必须保持以下边界：

- 不使用 `--data-dir`，不复制用户设置。
- 不从 NDJSON 的 `taskId`、`agentId` 或合成 `sessionId` 猜测原生会话 ID。
- 不按最新时间、最接近时间或打分选择候选。
- 不允许 Hub 连接失败后回退到 local runtime。
- resume 只能复用长期驻留共享 Hub，不能启动、停止或 signal 该 Hub。
- 不把 provider 排队时间或 task 结束时间加入会话创建窗口。
- 不实现 Cline cron/schedule 兼容；发布前提是用户不使用该功能。
- Cline 的 text、thinking、tool use、tool result 和 error 必须在 task 运行中
  实时进入 Multica 执行日志，不能等 `run_result` 或进程退出后一次性补发。

---

## 2. 当前实现与目标的差距

### 2.1 `server/pkg/agent/cline.go`

当前代码存在以下需要替换的行为：

1. `prepareClinePaths` 为每次运行创建临时 data dir。
2. `seedClineSettingsIntoDataDir` 复制 `~/.cline-sr/data/settings`。
3. `buildClineArgs` 主动传入 `--data-dir`。
4. `discoverClineSessionID` 在 `cmd.Wait` 后扫描文件并按分数挑选候选。
5. `scoreClineSessionMatch` 使用 task 总时长和两分钟容差，provider 排队会扩大窗口。
6. `clineLine` 没有解析顶层 `ts`，`processEvents` 也没有暴露首条协议记录和首次 `iteration_start`。
7. task 使用 `exec.CommandContext` 的默认取消行为，无法保证先通过 Hub abort root session，再终止 CLI 进程树。
8. resume 结果没有实现“除明确拒绝外保留原 SessionID”的语义。

### 2.2 共享 daemon pin 路径不在本次修改范围

当前提前 pin 逻辑在 HTTP 请求发出前执行 `sessionPinned.Swap(true)`，第一次
请求失败后不会在同一次执行中重试。这是共享 daemon 基础设施的既有行为，
会影响所有 agent backend，不属于本次内部 Cline 工具适配的修改边界。

Cline 仍发送第一次发现的非空 SessionID；正常 terminal callback 会再次携带
`Result.SessionID` 并由现有 `CompleteTask` / `FailTask` 路径持久化。daemon 在
提前 pin 后崩溃且该次 HTTP 又失败的极窄窗口，作为已知限制保留，不为此修改
`server/internal/daemon/client.go` 或 `daemon.go`。

### 2.3 已有能力可以直接复用

- `agent.Config.Env` 已包含 daemon 创建的 task 私有 `TMPDIR`。
- `ExecOptions.ResumeSessionID` 和 daemon 的 exact-workdir resume gate 已存在。
- `MessageStatus.SessionID` 已是提前 pin 的通道。
- `Result.ResumeRejected` 和 fresh-session retry gate 已存在。
- `postJSONWithRetry` 已提供按瞬时/永久错误分类的有限重试。
- `configureProcessGroup`、`signalProcessGroup` 已提供 Unix 进程树控制基础。
- task 结束的 `CompleteTask` / `FailTask` 已会携带最终 `SessionID` 和 `WorkDir`。

因此不需要数据库迁移、新 API、前端改动或新的第三方依赖。

---

## 3. 阶段 0：冻结 Cline 控制接口并采集真实 NDJSON

Multica 代码不能根据未固定的命令输出猜 JSON。控制接口以公开 Cline 3.0.46 /
core 0.0.65 的真实 probe 固定；执行日志事件采集当前 Cline `--json` 原始输出，
以真实 NDJSON fixture 驱动 Multica adapter，不修改 Cline 事件协议。

### 3.0 当前公开版本实测基线

2026-07-25 对本机 Cline CLI 3.0.46 / core 0.0.65 做了不启动模型 session 的
回环 Hub 探针，结果如下：

- `CLINE_HUB_DISCOVERY_PATH` 能隔离 discovery 文件。
- `cline hub --host 127.0.0.1 --port 0 start` 能由操作系统分配端口，并输出实际
  `ws://127.0.0.1:<port>/hub` URL。
- discovery 文件中的 Hub ID、PID、URL、port、startedAt
  与 `cline hub status` 一致；`cline hub stop` 会停止 Hub 并删除 discovery。
- 公开 discovery/status 没有进程 `startToken`；Multica 从操作系统捕获 PID
  start token 和 executable identity。
- 公开 Hub 没有已验证的 owner lease、single-root、abort-owned-root、
  wait-drained 或 tool 子进程控制环境剥离契约。

因此 port `0` 路径已经确认，不应实现预分配端口。用户接受公开版缺少上述
高级控制能力的残余风险；Multica 使用私有 discovery、认证 status/shutdown、
OS 进程身份和有界强制清理实现最佳努力生命周期。cron/schedule 不支持。

### 3.1 命令与 JSON 契约

必须固定：

- Hub 启动命令、前台/后台行为和退出码。
- discovery 和认证 `/status` 的字段：Hub ID、PID、loopback URL、实际端口和启动时间。
- 认证 `/shutdown` 的行为和退出码。
- 私有 discovery path 和端口 `0` 的参数或环境变量语义。
- `history --json --limit N` 的顶层 JSON 结构及字段类型。
- NDJSON 顶层 `ts` 的类型、单位和精度。
- 原生 `sessionId` 格式及毫秒时间戳前缀规则。
- structured `session_not_found` 和 `session_replaced` 的事件/错误码。
- 实际 `agent_event` / `hook_event` / `run_result` 的所有事件类型、字段别名和
  嵌套结构。
- streaming content 是 delta 还是 snapshot、content block 标识，以及
  thinking/reasoning 的实际 `contentType`。
- 工具开始/结束事件中可用于提取 name、stable call ID、input、output/error
  的实际字段，以及并发工具调用时的配对方式。
- 实际 flush 时序：provider 请求或工具执行前已经产生的 NDJSON 是否能被
  reader 立即读到。
- `--id` 通过正常长期驻留共享 Hub 的连接行为，以及只终止该 session/turn
  的 abort-on-disconnect 契约。

建议将下列公开版输出作为 Multica 测试 fixture：

```text
hub-status-ready.json
hub-status-draining.json
hub-status-stopped.json
history-empty.json
history-one-root.json
history-root-with-subagent.json
resume-session-not-found.ndjson
resume-session-replaced.ndjson
stream-text-thinking.ndjson
stream-tools-serial.ndjson
stream-tools-concurrent.ndjson
stream-error-usage-result.ndjson
```

fixture 不能包含真实 token、用户路径或 provider 凭据。

### 3.2 公开版残余风险

- forced Hub backend 连接失败时直接失败，不进入 local runtime。
- SQLite 初始化失败时直接失败，不回退 file session index。
- 公开 Hub 不保证 single-root、owner lease、abort-owned-root、wait-drained 或
  tool 子进程环境剥离；这些不通过修改 Cline 解决。
- Multica 通过每次 fresh 唯一 discovery/token/port、只启动一个 task CLI、
  禁止 `--zen` 和结束时身份校验清理降低风险。
- Multica adapter 必须消费 Cline 已有 NDJSON，并把其中可观察的
  thinking/text、工具调用、结果和错误映射到统一 `agent.Message`。
- 只有真实 probe 证明某项必要语义在 NDJSON 中完全不存在或事件直到进程退出
  才 flush 时，将其记录为公开版协议限制；不能用修改 Cline 代替 adapter 对
  已有事件变体的兼容。
- resume CLI 断开时只能停止已知 session 的当前 turn，不能停止或退出共享
  Hub，也不能影响该 Hub 上的其他 session。

### 3.3 阶段验收

Multica 集成至少验证：

- 同一持久状态根下同时启动两个专属 Hub。
- 默认本地 Hub 与专属 Hub 同时运行。
- SQLite busy/初始化失败不会切换 backend。
- 一个真实工具调用在工具结束前即可从 stdout 读到带 name/input/call ID 的
  `tool_call`，结束后能读到同 call ID 的 `tool_result`。
- 一个长 provider/tool 操作之前已经生成的事件可立即被读取，不等 task
  结束才 flush。
- resume 与多个普通本地 session 共用长期驻留 Hub；取消 resume 只终止目标
  session，其他 session 和 Hub 继续运行。

---

## 4. 阶段 1：替换 Cline 启动参数和环境

### 4.1 修改 `server/pkg/agent/cline.go`

保留：

- `clineArgvPromptSentinel`。
- stdin 的 `SystemPrompt + "\n\n" + prompt` 传输。
- 现有 NDJSON text/tool/usage/result 映射。
- `-c`、`-m` 和 resume 时的 `--id`。

删除：

- `clinePaths`。
- `clineSettingsSourceDirOverride`。
- `defaultClineSettingsSourceDir`。
- `prepareClinePaths`。
- `seedClineSettingsIntoDataDir`。
- `copyClineSettingsDir` 和 `copyClineFile`。
- `clineSessionMatchHints`、`clineSessionFile`。
- `discoverClineSessionID` 及文件扫描/打分/prompt 匹配辅助函数。

调整 `buildClineArgs`：

- 去掉 `clinePaths` 参数。
- 永远不生成 `--data-dir`、`--config`、`-s` 或 `-t`。
- 继续把这些 flag 放在 `clineBlockedArgs`，防止 `CustomArgs` 重新注入。
- 把 `--zen` 加入 blocked args。
- positional argv 最后一项仍然只能是 `"\n"`。

### 4.2 增加按 fresh/resume 分支的 Cline 环境

fresh 分支对 `buildEnv(b.cfg.Env)` 的结果做精确 key 过滤，然后追加专属 Hub
的权威值：

```text
CLINE_HUB_HOST=127.0.0.1
CLINE_HUB_PORT=<actual-port>
CLINE_HUB_DISCOVERY_PATH=<private-path>
CLINE_SESSION_BACKEND_MODE=hub
```

必须从继承环境和 `Config.Env` 中移除：

- `CLINE_VCR`
- 旧的 `CLINE_HUB_HOST`
- 旧的 `CLINE_HUB_PORT`
- 旧的 `CLINE_HUB_DISCOVERY_PATH`
- 旧的 `CLINE_SESSION_BACKEND_MODE`
- 公开版中其他会强制 local/remote backend 的变量

不要把值设为空字符串；必须从最终 `[]string` 中删除 key，避免 CLI 把“存在但为空”解释为显式配置。

resume 分支不生成上述 private host/port/discovery 值，也不覆盖用户正常的
共享 Hub 连接配置。它只需要：

- 移除 `CLINE_VCR` 和明确强制 local runtime 的变量。
- 强制 `CLINE_SESSION_BACKEND_MODE=hub`。
- 保留 Cline 默认或用户配置的长期驻留 Hub discovery/host/port。
- 禁止调用 dedicated `hub start/status/stop` controller。

### 4.3 task 私有目录

不要调用无 base path 的 `os.MkdirTemp("", ...)`。那会读取 daemon 进程自身的环境，而不是 `Config.Env["TMPDIR"]`。

fresh 分支从 `b.cfg.Env["TMPDIR"]` 取 daemon 已创建的 task 私有目录，并创建：

```text
<TMPDIR>/cline-hub/<cryptographically-random-run-id>/production.json
```

要求：

- 拒绝 discovery path 是 symlink 或非普通文件。
- 不校验 POSIX permission bits；Windows 上的 Go file mode 不表达 ACL 权限。
- token 不进入 argv、普通日志、错误正文或 task 消息。
- 每次 `Execute` 生成新 run ID；同一个 backend 的 fresh retry 也不能复用目录。

resume 分支不创建 `cline-hub` runtime dir。

### 4.4 单元测试

替换 `cline_test.go` 中所有 data-dir/settings-seed 测试，新增：

- argv 不包含 `--data-dir`、`--config`、`-s`、`-t`、`--zen`。
- stdin/argv sentinel 契约不变。
- 用户 `CustomArgs` 不能重新加入被接管的 flag。
- inherited/config env 中的 Hub/VCR 值被删除，权威值只出现一次。
- fresh runtime dir 位于传入的 task `TMPDIR`，权限正确且不同 fresh Execute
  不复用。
- resume 不创建 runtime dir，不注入 private host/port/discovery，不调用 Hub
  controller，并保留共享 Hub 的普通连接配置。
- 默认测试始终使用 test-created fake executable，不查找用户安装的 `cline`。

---

## 5. 阶段 2：增加仅供 fresh 使用的专属 Hub controller

### 5.1 新文件建议

新增 `server/pkg/agent/cline_hub.go`，只承载 fresh Cline 的专属生命周期和
history 匹配。`cline.go` 保留 backend 分支、orchestration 和 NDJSON 映射。

这个拆分是必要的：当前 `cline.go` 已超过 1000 行，而 fresh Hub 启停、
认证、进程清理、history 和候选匹配是一个独立故障域。resume 不依赖该
controller。不要再抽象成跨 provider 的通用 process manager。

核心私有类型建议：

```go
type clineHub struct {
    runID        string
    runtimeDir   string
    discoveryPath string
    info         clineHubInfo
    process      clineProcessIdentity
}

type clineHubInfo struct {
    HubID       string
    PID         int
    Host        string
    Port        int
    StartedAt   time.Time
    StartToken  string // Multica 从 OS 捕获，不要求 discovery 提供
}
```

字段名以阶段 0 的公开版本 probe 为准。

### 5.2 启动与 readiness

`startClineHub` 按以下顺序执行：

1. 创建私有 runtime dir/discovery path。
2. 使用净化后的控制环境启动 Hub。
3. 以短 timeout 轮询 discovery file 和 authenticated status。
4. 校验 host 是 loopback、port 合法、PID 为正且存活。
5. 校验 discovery 与 status 的 Hub ID、PID、URL、port 一致。
6. 校验 Hub 启动时间不早于本次 launch attempt。
7. 从操作系统记录 PID start token 和 executable/command identity。

公开 3.0.46 已验证支持 port `0`，只走该路径，不实现预分配端口。

Hub readiness 失败时，`Execute` 直接返回错误，不能启动 task CLI。

### 5.3 认证控制操作

controller 仅允许访问 discovery/status 中校验过的 loopback URL，并提供：

- `status`
- `stop`

每个调用使用独立短 timeout 和 bounded response body。认证 token 只存在于内存和 discovery file 中，不能进入 argv、日志或 task message。

### 5.4 进程身份校验

增加平台文件，避免把 PID 复用后的其他进程杀掉：

```text
cline_process_linux.go
cline_process_darwin.go
cline_process_windows.go
```

实现只服务 Cline Hub：

- Linux：读取 `/proc/<pid>/stat` 的 start ticks 和 `/proc/<pid>/exe`/cmdline。
- macOS：使用系统进程查询接口获取启动标识和 executable identity。
- Windows：复用已有 `golang.org/x/sys/windows` 依赖读取 creation time 和 image path。

强制终止前必须再次匹配 PID、OS start token、Hub ID、port、discovery path 和
command identity。任何一项无法确认就不发送强信号。

### 5.5 生命周期测试

使用 fake Cline executable/fake loopback Hub 覆盖：

- readiness 成功和 timeout。
- discovery/status 不一致。
- 非 loopback host。
- discovery 权限或文件类型错误。
- port collision 后完整重试。
- token 不出现在日志和错误中。
- stale PID/start token 不会被 signal。

---

## 6. 阶段 3：实现 fresh history 快照和精确候选匹配

### 6.1 history runner

在 `cline_hub.go` 中加入 Cline 专用的 bounded command runner：

- 使用相同 configured executable。
- 使用相同持久状态根和专属 Hub 控制环境。
- 执行 `history --json --limit <fixed-limit>`。
- 使用独立短 timeout。
- stdout/stderr 都设置硬上限，超限视为 history failure。
- 不持续轮询；只执行 pre-run、有限 early retry 和必要的 final lookup。
- resume 永远不调用该 runner。

不要修改 `agent.Config` 或增加通用 command abstraction；这个 runner 只服务已固定的公开版 JSON 契约。

### 6.2 history 数据结构

用指针字段保留“缺失”和零值的区别，例如：

```go
type clineHistoryEntry struct {
    SessionID      string
    PID            int
    Source         *string
    Interactive    *bool
    IsSubagent     *bool
    ParentSessionID string
    Cwd            string
    WorkspaceRoot  string
    StartedAt      time.Time
}
```

解码规则以公开版 fixture 为准。额外字段可以忽略，但必需字段缺失、类型错误或时间不可解析时，该 entry 不能成为候选。

### 6.3 canonical workdir

task 启动前计算一次 canonical workdir：

1. `filepath.Abs`
2. `filepath.EvalSymlinks`
3. `filepath.Clean`
4. 可读取双方路径时优先用 `os.SameFile`
5. 平台需要时处理 volume/case 语义

删除当前只做 `filepath.Clean` 的 `samePathLoose`。canonicalization 失败时 fresh SessionID discovery fail closed，但 task 本身可以继续执行。

### 6.4 纯函数候选过滤器

实现一个无 I/O 的 `matchClineHistoryCandidate`，输入：

- pre-run root SessionID set
- 当前 history entries
- dedicated Hub PID
- canonical workdir
- `cmdStartWallMs`
- frozen `firstProtocolTimestampMs`
- 可选固定 `clockSkew`（默认必须为 0）

每个候选必须同时满足：

1. `sessionId` 非空且不在 pre-run set。
2. `pid == dedicatedHubPID`。
3. `source` 存在时等于公开版的 CLI enum。
4. `interactive` 存在时为 false。
5. `isSubagent` 明确为 false，`parentSessionId` 为空。
6. `cwd` 或 `workspaceRoot` 与 canonical workdir 相同。
7. SessionID 符合公开版格式，毫秒前缀可严格解析。
8. `cmdStartWallMs <= sessionIdTimestamp <= firstProtocolTimestampMs`。
9. `startedAt` 可解析，并位于同一个闭区间内。

返回值只允许：

```text
1 candidate  -> SessionID
0 candidates -> empty
N candidates -> empty
```

prompt 只能进入脱敏诊断，不能用于消歧。`clockSkew` 不是用户配置项，只有公开版压测证明需要时才改为一个固定小常量。

### 6.5 纯函数测试表

至少覆盖每个过滤条件单独失效：

- pre-history 已存在。
- PID 不同。
- source/interactive 不符。
- subagent 或有 parent。
- symlink/case/volume 后 cwd 不同。
- SessionID 格式错误、时间戳溢出或单位错误。
- ID timestamp 在 lower bound 前或 upper bound 后。
- `startedAt` 缺失、不一致或越界。
- 零候选、多候选。
- provider delay 很长但 upper bound 不变。

---

## 7. 阶段 4：fresh 冻结 NDJSON 时间并提前发现 SessionID

### 7.1 扩展协议解析

给 `clineLine` 增加公开版契约类型的顶层 `ts`。`processEvents` 继续负责现有消息映射，同时向 Execute orchestration 报告两种一次性 observation：

- 第一条合法 Cline protocol record 的 timestamp。
- 第一条 semantic `agent_event.iteration_start`。

合法 protocol record 必须满足：

- JSON 可解析。
- 顶层 type 是 `hook_event`、`agent_event` 或 `run_result`。
- 顶层 `ts` 存在并符合公开版契约。

第一条 timestamp 一旦记录就不可替换。父进程另记 monotonic receipt time，只用于日志和测试，不参与候选区间。

### 7.2 task 启动时序

fresh `Execute` 的顺序必须固定为：

1. 启动并验证 Hub。
2. 获取 pre-history。
3. 创建 stdout/stdin pipes。
4. 紧邻 `cmd.Start` 前调用一次 `time.Now()`，同时保留 wall/monotonic 分量。
5. `cmd.Start`。
6. 并发写 stdin 和 drain stdout。
7. parser 冻结 first protocol timestamp。
8. 第一次 `iteration_start` 触发有限 history retry。

不要在 stdout goroutine 启动后才记录 lower bound。

当 `ResumeSessionID` 非空时，不进入这套时序，直接走第 8 节的共享 Hub
resume 分支。stdout 仍然必须实时解析并上传执行事件。

### 7.3 early lookup coordinator

history lookup 不能阻塞 stdout scanner。使用容量固定的小 channel 或一次性 callback 通知独立 goroutine：

- pre-history 失败：不启动 early/final fresh discovery。
- 没有合法 first timestamp：不发现 fresh SessionID。
- 收到 iteration start：按短 schedule 重试 history。
- 找到唯一候选：只发送一次 `MessageStatus{Status: "running", SessionID: id}`。
- 第一次非空 ID 成为本次 fresh run 的 immutable ID。

`cmd.Wait` 后：

- early 未找到时，用相同 frozen interval 做一次 final lookup。
- early 已找到时可以确认，但不能替换。
- final 得到不同唯一 ID 时记录 invariant violation，结果仍使用 early ID。
- history failure 不改写 task 的 status/output/error。

### 7.4 无协议时间的行为

如果整个 stream 没有合法协议 `ts`：

- fresh `Result.SessionID` 为空。
- 不使用 `cmd.Wait`、provider 首 token 或 task end time 替代。
- task 的正常成功/失败结果照常返回。

### 7.5 实时事件进入 Multica 执行日志

不为专属 Hub 新增第二条 dashboard/menubar event bridge。沿用现有链路：

```text
Cline task stdout NDJSON
  -> clineBackend.processEvents
  -> agent.Message
  -> daemon.executeAndDrain
  -> ReportTaskMessages
  -> task_message + task:message WebSocket
  -> Multica 执行日志
```

服务端、WebSocket 和前端已经支持 `text`、`thinking`、`tool_use`、
`tool_result`、`error`；缺口位于 Cline NDJSON -> `agent.Message` adapter，
不需要新增数据库类型、前端页面或 Hub event bridge。fresh 与 resume 共用同一
套事件适配器。

先从实际 Cline 版本采集覆盖 text、thinking、串行/并行工具、失败和 usage 的
脱敏 NDJSON fixture，再修改 `processEvents`：

- 将 `agent_event` 的 text/reasoning content block 分别归一化为
  `MessageText` / `MessageThinking`，保留原始空白。
- 按实际 content block ID 和已确认的 delta/snapshot 语义维护状态；snapshot
  只发送相对上一版本的新后缀，不能重复追加累计文本。
- 将工具开始事件的字段别名归一化为
  `MessageToolUse{Tool, CallID, Input}`；input 同时兼容 object、JSON string
  和 Cline 已有的嵌套形式。
- 将工具结束事件归一化为
  `MessageToolResult{Tool, CallID, Output}`，保留 stdout/stderr/error 等实际
  结果。用显式 call ID 或 probe 确认的关联字段维护 in-flight map，禁止用
  单个 `lastToolCallID` 错配并行调用。
- 将可操作的错误事件映射为 `MessageError`；usage 和最后一个
  `run_result` 仍只更新 `Result`，不伪造成执行动作。
- `iteration_start` 等纯生命周期事件默认不制造用户可见文本；若真实 NDJSON
  只有这类事件能表达等待状态，则映射为低噪声 status/log，而不是工具调用。
- 未识别事件记录 type/字段名和 metric，但不记录敏感 payload，也不让单条
  未知事件中断任务。
- Cline 路径不得使用通道满即静默丢弃的 `trySend`。采用可取消的背压发送或
  等价有界队列，确保事件最终进入 daemon；取消时必须解除阻塞。

如果 probe 证明 NDJSON 根本没有工具名、关联 ID、thinking 区分或实时 flush，
该能力只能标记为上游协议缺口，再对内部 Cline 做最小补充。adapter 必须先
完整利用已经存在的 NDJSON，不能把事件映射本身推给 Cline。

测试必须证明事件在进程退出前到达 `Session.Messages`，并按以下顺序经 daemon
持久化：

```text
thinking/text -> tool_use -> tool_result -> final text
```

fake CLI 测试需要在 `tool_use` 与 `tool_result` 中间人为阻塞，断言 Multica
已经收到前者；另用超过 256 条的 burst 和交错并行 call ID 验证不丢事件、
不串配。只在进程结束后检查最终数组不足以证明实时性。

---

## 8. 阶段 5：长期驻留共享 Hub resume

resume 仍通过现有 daemon exact-workdir gate，但不创建或管理专属 Hub。

修改 `clineBackend.Execute`：

- 在任何 private runtime dir、Hub controller 或 history setup 之前按
  `ResumeSessionID != ""` 分支。
- 使用 Cline 默认或用户配置的长期驻留共享 Hub，只强制 Hub backend mode。
- `ResumeSessionID` 非空时传 `--id`。
- 初始 authoritative ID 设为请求的旧 ID。
- 不对旧 ID 应用 fresh timestamp window。
- 网络、provider 排队、quota、auth、5xx 或普通 task failure 均保留旧 ID。
- structured `session_not_found`：返回空 ID 和 `ResumeRejected=true`。
- structured `session_replaced`：原 resume 未成立，返回空 ID 和
  `ResumeRejected=true`，由 daemon 走一次 fresh retry；不在 resumed attempt
  中静默采用替代 ID。
- 不从错误文本模糊匹配 resume rejection。
- 不执行 history snapshot、timestamp capture/match、early lookup 或 final lookup。
- 无论 resume 成功或失败，仍实时解析并上传 NDJSON 执行事件。
- 正常结束后只等待 task CLI 和事件 drain，不执行 Hub drain/status/stop、PID
  验证或 private runtime cleanup。
- cancel/timeout 只终止 task CLI 进程树；attached `--id` client 断开必须由
  Cline 自身 abort 当前 turn。Multica 不调用共享 Hub control API，也绝不能
  stop、signal 或清理共享 Hub。

新增测试：

- resume 成功保留旧 ID。
- provider/network/auth failure 保留旧 ID。
- 明确 `session_not_found` 才触发 daemon fresh retry。
- 明确 replacement 与 `session_not_found` 一样触发 fresh retry，不能采用替代 ID。
- resume 不执行任何 fresh history/timestamp discovery 命令或状态初始化。
- resume 的 named tool/thinking/text 事件在 task 完成前进入 Multica 执行日志。
- resume 不调用 fake dedicated Hub controller；取消后 fake shared Hub 和其他
  fake session 仍然存活。

---

## 9. 阶段 6：沿用现有 daemon pin 接口

本次不修改共享 `server/internal/daemon/client.go`、`daemon.go` 或其测试。

- fresh Cline 发现唯一 ID 后仍只发送一次现有
  `MessageStatus{Status: "running", SessionID: id}`。
- backend 内的 authoritative ID 固定为第一次发现的非空 ID，final lookup 不得
  替换。
- terminal `Result.SessionID` 必须始终携带该 ID，使现有 terminal callback
  成为提前 pin 失败时的最终持久化路径。
- 提前 pin HTTP 瞬时失败与 daemon 同时崩溃的窗口是本次已知限制；若以后要
  修复，应作为独立的通用 daemon 变更评审，不能夹带在 Cline adapter 中。

---

## 10. 阶段 7：fresh 与 resume 分支化取消

### 10.1 取消顺序

fresh task 不要依赖 `exec.CommandContext` 默认直接 kill child。它需要接管取消顺序：

1. `runCtx.Done()`。
2. authenticated shutdown 专属 Hub。
3. 终止 task CLI 进程组。
4. 重新验证完整进程身份后，才允许 stronger Hub termination。

使用已有 `configureProcessGroup` / `signalProcessGroup`，不要另写一套通用进程树 helper。

resume task 使用另一条顺序：

1. `runCtx.Done()`。
2. 终止 Multica task CLI 进程树。
3. Cline attached client 断开并由共享 Hub abort 该 session 的当前 turn。
4. drain 已经收到的 NDJSON 事件。
5. 不调用 Hub controller，不执行 Hub stop、Hub PID signal 或 private path cleanup。

### 10.2 正常完成

fresh 正常路径固定为：

1. `cmd.Wait`。
2. 完成必要的 final SessionID lookup。
3. authenticated graceful stop。
4. 确认记录的进程身份已退出。
5. 删除本 run 的私有 runtime dir。

resume 正常路径只等待 task CLI、完成事件 drain 并返回保留的 SessionID；不
访问 Hub controller。

### 10.3 异常清理

- Hub 未确认退出前，不发送未校验 PID 的 kill。
- 只删除当前 run 创建的路径，不碰默认 Hub 或其他 Multica run。
- 公开 Hub 没有 owner lease；daemon 硬崩溃后的 orphan Hub 是已接受残余风险。
- 外层 task temp cleanup 会删除运行目录，因此 backend 正常返回前必须完成有界 shutdown。
- resume 没有 private Hub artifact；任何 cleanup 都不能以 shared Hub PID、
  discovery path 或 port 为目标。

### 10.4 测试

- context cancel 先到专属 Hub shutdown，再到 CLI process termination。
- idle watchdog 走同一个 shutdown/cleanup 路径。
- graceful stop timeout 后验证失败不会误杀其他 PID。
- cleanup 只删除当前 run 的目录。
- resume cancel 只终止 task CLI；Cline 的 disconnect 契约只终止目标
  session/turn，共享 Hub 和同 Hub 上其他 session 保持运行。
- resume 正常完成和失败都不调用任何 shared-Hub control API，也不 signal/kill Hub。

---

## 11. 文件级修改清单

| 文件 | 修改 |
| --- | --- |
| `server/pkg/agent/cline.go` | 删除 data-dir/settings/file-score discovery；在 setup 前分支 fresh 专属 Hub 与 shared-Hub resume；完整映射实时 thinking/tool/text/error 事件 |
| `server/pkg/agent/cline_hub.go` | 新增仅供 fresh 使用的专属 Hub、认证控制、history runner、纯候选匹配 |
| `server/pkg/agent/cline_process_linux.go` | Linux PID/start token/command identity 校验 |
| `server/pkg/agent/cline_process_darwin.go` | macOS 进程身份校验 |
| `server/pkg/agent/cline_process_windows.go` | Windows 进程身份校验 |
| `server/pkg/agent/cline_test.go` | 保留 NDJSON/argv/stdin 测试，删除旧 data-dir 测试，增加 fresh 专属 Hub、shared-Hub resume 和进程退出前事件可见性的 fake CLI 测试 |
| `server/pkg/agent/cline_hub_test.go` | Hub JSON、history、精确匹配、生命周期测试 |
| `server/pkg/agent/cline_process_*_test.go` | 当前进程/错误 PID/复用 token 的平台测试 |

预计不修改：

- `server/pkg/agent/agent.go`：现有 `Message`、`Result`、`ExecOptions`、`Config` 已足够。
- `server/internal/daemon/client.go`、`daemon.go` 及其测试：沿用现有 status pin
  和 terminal callback，不扩大共享代码修改面。
- handler、sqlc 和 migration：现有 session pin/terminal API 已足够。
- Web/desktop/mobile：没有 UI 或 API shape 变化。

如果公开 Cline 升级改变阶段 0 契约，再用最小 diff 更新本清单，不提前增加通用抽象。

---

## 12. 实施顺序和提交拆分

建议按以下顺序提交，保证每个提交都有独立可运行检查：

1. `test(cline): pin dedicated hub and history contracts`
   - 加入脱敏公开版 fixture 和失败的 parser/matcher tests。
2. `refactor(cline): remove temporary data dir discovery`
   - 删除 settings seed、`--data-dir` 和 score matcher，更新 argv/env tests。
3. `feat(cline): manage a dedicated hub for fresh execution`
   - 加入 fresh-only controller、readiness、认证 shutdown 和进程身份校验。
4. `feat(cline): discover native sessions with exact history matching`
   - pre-history、协议时间、early/final exact-one discovery。
5. `fix(cline): preserve resume ids unless positively rejected`
   - shared-Hub `--id` 分支、structured rejection/replacement semantics；resume
     完全绕过 private Hub 和 fresh discovery。
6. `feat(cline): stream structured execution events to Multica`
   - 公开版 event fields/flush contract、thinking/tool mapping 和 pre-exit delivery tests。
7. `test(cline): cover fresh hub and shared resume cancellation`
   - fresh cleanup、shared-Hub attached-client disconnect、并发和 real-agent
     opt-in smoke。

第 2 个提交不能单独进入发布分支；从移除旧 discovery 到 fresh 专属 Hub
集成完成之间，Cline fresh SessionID discovery 是不完整的。resume 独立使用
共享 Hub，但仍要等事件、取消和 rejection 测试完成后才能发布。实际合并可以
保留原子提交，但发布必须以整个序列通过为准。

---

## 13. 验证命令

开发时先跑最小集合：

```bash
cd server
go test ./pkg/agent -run 'TestCline' -count=1
```

平台实现至少做编译验证：

```bash
cd server
GOOS=linux go test ./pkg/agent -run 'TestCline' -count=1
GOOS=darwin go test -c ./pkg/agent -o /tmp/multica-agent-darwin.test
GOOS=windows go test -c ./pkg/agent -o /tmp/multica-agent-windows.test.exe
```

仓库级验证：

```bash
make test
make check
```

真实账号测试只能走已有 opt-in gate：

```bash
cd server
MULTICA_RUN_REAL_AGENT_SMOKE=1 go test -tags=agentintegration ./pkg/agent -run '<ClineSmokeTest>' -count=1 -v
```

并发/压力验收至少包括：

- 本地默认 Hub + 多个专属 Hub。
- 多个 fresh run，不同和相同 cwd。
- 长期驻留共享 Hub resume 旧 ID，同时运行多个普通 session 和 fresh 专属 Hub。
- 30 分钟模拟 provider queue。
- task CLI disconnect、PID reuse 和 port collision。
- SQLite migration/busy contention。
- tool 内嵌套启动 Cline 作为公开版残余风险观察项。

---

## 14. 完成标准

只有全部满足时才把方案 04 标记为 implemented：

- [ ] 公开 Cline Hub lifecycle probe 和 opt-in smoke 通过。
- [ ] task argv 不含 `--data-dir`，旧 settings seed/文件扫描/score matcher 已删除。
- [ ] 每次 fresh Execute 使用独立、认证、loopback 的 Hub。
- [ ] 每次 resume Execute 使用正常长期驻留共享 Hub，不创建或停止专属 Hub。
- [ ] Hub backend 和 SQLite backend 均 fail closed。
- [ ] lower bound 在 `cmd.Start` 前采集，upper bound 来自首条合法协议 `ts` 且不可变。
- [ ] fresh SessionID 只由 exact-one history candidate 产生。
- [ ] provider 排队和 task 总时长不扩大 timestamp window。
- [ ] early ID 不可被 final lookup 替换。
- [ ] 传入 `ResumeSessionID` 时完全不执行 private Hub lifecycle 或 fresh
  history/timestamp matching。
- [ ] resume 只在明确拒绝时清空旧 ID并设置 `ResumeRejected`。
- [ ] named tool/input/result、thinking、text 和 error 在 task 结束前实时进入
  Multica 执行日志，顺序与 Cline 实际动作一致。
- [ ] content delta/snapshot 不重复、不丢空白；并行工具按各自 call ID 正确配对。
- [ ] 超过 256 条的事件 burst 不因 Cline adapter 通道背压而静默丢失，取消
  可以解除发送阻塞。
- [ ] Cline NDJSON 每行及时 flush，长 provider/tool 操作不会阻塞前置事件可见性。
- [ ] early status 与 terminal result 携带同一个 immutable SessionID；共享
  daemon pin 的既有失败窗口不在本次 Cline adapter 修改范围。
- [ ] fresh cancel/timeout/idle watchdog 都先 shutdown 专属 Hub，再终止 task
  进程组。
- [ ] resume cancel/timeout/idle watchdog 只终止 task CLI；Cline disconnect
  只 abort 目标 turn，Multica 不调用、停止或 signal 共享 Hub。
- [ ] 默认测试不执行用户安装的 Cline。
- [ ] 单元、并发、平台编译和 opt-in real-agent smoke 全部通过。

完成前不能宣称执行日志事件 parity、fresh native SessionID discovery 或
shared-Hub resume 已完成。公开 Hub 没有 owner lease，因此 daemon 硬崩溃后的
crash-safe cleanup 不属于完成声明。
