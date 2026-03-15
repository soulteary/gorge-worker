# gorge-worker 技术报告

## 1. 概述

gorge-worker 是 Gorge 平台中的异步任务消费微服务，为 Phorge（Phabricator 社区维护分支）提供后台任务处理能力。

该服务的核心目标是替代 Phorge 原有的 PHP `PhabricatorTaskmasterDaemon` 守护进程。`PhabricatorTaskmasterDaemon` 是 Phabricator/Phorge 内置的后台任务处理组件，由 `phd` 守护进程管理器启动，从数据库任务队列中拉取待执行的异步任务（搜索索引、邮件发送、Feed 推送、Herald Webhook 等），逐个或批量执行。gorge-worker 以 Go 重新实现了任务消费和调度层，对于复杂的业务逻辑通过 Conduit API 委托给 PHP 后端执行，保持与 Phorge 的完全兼容。

## 2. 设计动机

### 2.1 原有方案的问题

Phorge 的后台任务处理基于 PHP 守护进程 `PhabricatorTaskmasterDaemon`：

1. **进程管理复杂**：`phd`（PhabricatorDaemon 管理器）需要作为常驻进程运行，管理多个 Taskmaster 守护进程的启停、重启和日志。PHP 进程不适合作为长期运行的守护进程——存在内存泄漏累积、opcode cache 失效、信号处理不完善等问题，通常需要定期重启。
2. **并发模型受限**：每个 Taskmaster 进程是单线程的，一次只能处理一个任务。要提高吞吐量只能启动更多进程，但每个 PHP 进程的内存开销在 30-50MB 级别，20 个进程就需要 600MB-1GB 内存。
3. **轮询效率低**：Taskmaster 直接查询 MySQL `worker_activetask` 表获取待处理任务，在任务稀疏时产生大量无效查询。多个 Taskmaster 进程之间通过数据库行锁竞争任务，高并发时锁争用严重。
4. **部署耦合**：Taskmaster 与 Phorge PHP 应用绑定在同一部署环境，无法独立扩缩容。需要同时部署完整的 PHP 运行环境和所有 Phorge 代码，即使守护进程只使用其中的 Worker 类。
5. **监控困难**：Taskmaster 的运行状态主要通过 `phd status` 命令行查看，缺乏 HTTP 健康检查和指标暴露接口，不适合容器化编排环境的健康检测和自动恢复。

### 2.2 gorge-worker 的解决思路

以 Go 重写任务消费调度层，保留 PHP 业务逻辑：

- **轻量并发**：Go goroutine 的内存开销约 4KB（初始栈），一个进程可以轻松并发处理数十个任务，等效于数十个 PHP Taskmaster 进程，但内存开销减少两个数量级。
- **队列解耦**：不直接查询 MySQL 任务表，而是从 gorge-task-queue（独立的任务队列服务）通过 HTTP API 拉取任务。队列服务负责任务状态管理、租约和去重，消费者只需关注执行逻辑。
- **委托模式**：对于依赖大量 PHP 类和数据库交互的复杂任务（搜索索引、邮件发送、事务发布等），通过 Conduit API 的 `worker.execute` 方法委托给 PHP 后端执行。Go 负责调度和生命周期管理，PHP 负责业务逻辑，各取所长。
- **独立部署**：作为独立容器运行，可以根据任务负载独立扩缩容。不依赖 PHP 运行时和 Phorge 代码库。
- **可观测性**：提供 HTTP 健康检查和统计接口，适配容器编排（Kubernetes、Docker Compose）的健康检测和自动恢复。

## 3. 系统架构

### 3.1 在 Gorge 平台中的位置

```
┌──────────────────────────────────────────────────────────┐
│                       Gorge 平台                          │
│                                                           │
│  ┌──────────┐                                             │
│  │  Phorge  │── 写入任务 ──┐                               │
│  │  (PHP)   │◄── Conduit ──┼──────────────────┐           │
│  └──────────┘              │                  │           │
│                            ▼                  │           │
│               ┌────────────────────┐          │           │
│               │  gorge-task-queue  │          │           │
│               │      :8090        │          │           │
│               │  (任务队列服务)     │          │           │
│               └────────┬───────────┘          │           │
│                        │                      │           │
│               Lease/Complete/Fail/Yield        │           │
│                        │                      │           │
│                        ▼                      │           │
│               ┌────────────────────┐          │           │
│               │   gorge-worker    │          │           │
│               │      :8170        │          │           │
│               │                  │          │           │
│               │  Consumer        │          │           │
│               │    ↓ Registry    │          │           │
│               │      ↓ Handlers ─┘          │           │
│               │        ├─ Conduit 委托 ──────┘           │
│               │        ├─ Feed HTTP (本地)               │
│               │        └─ Noop (占位)                    │
│               └────────────────────┘                      │
│                                                           │
│  其他 Go 服务: gorge-conduit, gorge-mailer,               │
│               gorge-notification, gorge-diff ...          │
└──────────────────────────────────────────────────────────┘
```

### 3.2 核心流水线

gorge-worker 的工作流程是一个持续运行的消费循环：

```
┌─────────────────────────────────────────────────────────────────┐
│                     Consumer 消费循环                            │
│                                                                  │
│  Ticker ──► Lease(limit) ──► 过滤 ──► 并发执行 ──► 上报结果      │
│    │              │              │          │            │        │
│    │              ▼              │          ▼            ▼        │
│    │        任务队列拉取          │    sem 信号量     Complete     │
│    │                            │    goroutine      Fail        │
│    │                            ▼    并发控制       Yield        │
│    │                   shouldProcess                             │
│    │                   filter + registry                         │
│    │                                                             │
│    └──── 无任务? ──► 空闲计时 ──► 超时? ──► 休眠 3 分钟           │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

### 3.3 模块划分

项目采用 Go 标准布局，分为五个内部模块：

| 模块 | 路径 | 职责 |
|------|------|------|
| config | `internal/config/` | 环境变量配置加载 |
| taskqueue | `internal/taskqueue/` | 任务队列 HTTP 客户端，封装 Lease/Complete/Fail/Yield 操作 |
| worker | `internal/worker/` | 消费引擎（Consumer）和处理器注册表（Registry） |
| handlers | `internal/handlers/` | 具体的任务处理器实现 |
| httpapi | `internal/httpapi/` | HTTP API 路由、认证中间件和管理接口 |

入口程序 `cmd/server/main.go` 串联所有模块：加载配置 → 创建队列客户端 → 注册处理器 → 启动消费者 → 启动 HTTP 服务 → 信号处理与优雅关闭。

## 4. 核心实现分析

### 4.1 配置模块

配置模块位于 `internal/config/config.go`，通过环境变量加载所有配置项。

#### 4.1.1 数据结构

```go
type Config struct {
    ListenAddr   string
    ServiceToken string

    TaskQueueURL   string
    TaskQueueToken string

    LeaseLimit     int
    PollIntervalMs int
    MaxWorkers     int
    IdleTimeoutSec int

    ConduitURL   string
    ConduitToken string

    TaskClassFilter []string
}
```

配置分为四组：

- **服务参数**：`ListenAddr` 和 `ServiceToken` 控制 HTTP 服务的监听地址和 API 认证。
- **队列连接**：`TaskQueueURL` 和 `TaskQueueToken` 指定 gorge-task-queue 的地址和认证凭证。
- **消费行为**：`LeaseLimit`（单次拉取上限）、`PollIntervalMs`（轮询间隔）、`MaxWorkers`（并发数）、`IdleTimeoutSec`（空闲休眠阈值）控制消费引擎的工作节奏。
- **Conduit 委托**：`ConduitURL` 和 `ConduitToken` 指定 Phorge Conduit API 地址。`ConduitURL` 非空时启用 Conduit 委托处理器，将复杂任务转交给 PHP 执行。

#### 4.1.2 任务类型过滤

```go
TaskClassFilter: splitCSV(envStr("TASK_CLASS_FILTER", "")),
```

`TASK_CLASS_FILTER` 接受逗号分隔的任务类型列表。设置后，消费者仅处理列表中的任务类型，其余任务标记为临时失败（60 秒后重试）。这个机制支持两种部署模式：

- **全量消费**：不设置过滤，一个 gorge-worker 实例处理所有支持的任务类型。
- **专一消费**：设置过滤，不同的 gorge-worker 实例专注于不同的任务类型。例如一组实例专门处理 `PhabricatorSearchWorker`（搜索索引，CPU 密集），另一组处理 `PhabricatorMetaMTAWorker`（邮件发送，I/O 密集），实现负载隔离和独立扩缩容。

```go
func splitCSV(s string) []string {
    if s == "" {
        return nil
    }
    parts := strings.Split(s, ",")
    out := make([]string, 0, len(parts))
    for _, p := range parts {
        p = strings.TrimSpace(p)
        if p != "" {
            out = append(out, p)
        }
    }
    return out
}
```

`splitCSV` 在分割后对每个元素做 `TrimSpace`，容忍输入中的多余空格（如 `"TaskA, TaskB"` 与 `"TaskA,TaskB"` 等价），减少配置出错的可能。

### 4.2 任务队列客户端

任务队列客户端位于 `internal/taskqueue/client.go`，封装了与 gorge-task-queue 的所有 HTTP 交互。

#### 4.2.1 客户端初始化

```go
type Client struct {
    baseURL    string
    token      string
    leaseOwner string
    httpClient *http.Client
}

func NewClient(baseURL, token string) *Client {
    hostname, _ := os.Hostname()
    pid := os.Getpid()
    leaseOwner := fmt.Sprintf("%s:%d:%d:github.com/soulteary/gorge-worker",
        hostname, pid, time.Now().Unix())

    return &Client{
        baseURL:    baseURL,
        token:      token,
        leaseOwner: leaseOwner,
        httpClient: &http.Client{
            Timeout: 30 * time.Second,
        },
    }
}
```

`leaseOwner` 标识格式为 `hostname:pid:timestamp:module`，包含四个维度：

- **hostname**：区分不同的物理/容器主机。
- **pid**：区分同一主机上的多个 gorge-worker 进程。
- **timestamp**：区分同一进程 PID 被复用的情况（进程重启后可能获得相同 PID）。
- **module**：固定为 `github.com/soulteary/gorge-worker`，标识消费者的来源模块。

这个标识用于任务租约——队列服务通过 `X-Lease-Owner` 头识别任务的持有者，防止同一任务被多个消费者同时处理。30 秒的 HTTP 超时确保在队列服务响应缓慢时不会无限等待。

#### 4.2.2 任务数据结构

```go
type Task struct {
    ID            int64  `json:"id"`
    TaskClass     string `json:"taskClass"`
    LeaseOwner    string `json:"leaseOwner,omitempty"`
    LeaseExpires  *int64 `json:"leaseExpires,omitempty"`
    FailureCount  int    `json:"failureCount"`
    DataID        int64  `json:"dataID"`
    FailureTime   *int64 `json:"failureTime,omitempty"`
    Priority      int    `json:"priority"`
    ObjectPHID    string `json:"objectPHID,omitempty"`
    ContainerPHID string `json:"containerPHID,omitempty"`
    DateCreated   int64  `json:"dateCreated"`
    DateModified  int64  `json:"dateModified"`
    Data          string `json:"data,omitempty"`
}
```

`Task` 结构体映射了 Phorge `worker_activetask` 表的关键字段：

- **`TaskClass`**：任务类型的 PHP 类名（如 `PhabricatorSearchWorker`），用于在 Registry 中查找对应的处理器。
- **`Data`**：JSON 字符串形式的任务负载，由 `worker_taskdata` 表提供。不同任务类型的 Data 格式不同——搜索索引包含对象 PHID，邮件发送包含邮件 ID，Feed 推送包含 story ID 和目标 URI。
- **`FailureCount`**：累计失败次数，某些处理器可根据此值调整重试策略（如超过阈值后标记为永久失败）。
- **`Priority`**：任务优先级，队列服务在 Lease 时优先返回高优先级任务。
- **`LeaseOwner` / `LeaseExpires`**：租约信息，`omitempty` 表示在请求中可省略，由队列服务填充。

#### 4.2.3 四个队列操作

```go
func (c *Client) Lease(ctx context.Context, limit int) ([]*Task, error)
func (c *Client) Complete(ctx context.Context, taskID int64, durationUs int64) error
func (c *Client) Fail(ctx context.Context, taskID int64, permanent bool, retryWait *int) error
func (c *Client) Yield(ctx context.Context, taskID int64, durationSec int) error
```

四个操作覆盖了任务的完整生命周期：

**Lease**：向队列服务请求最多 `limit` 个待处理的任务。请求携带 `X-Lease-Owner` 头，队列服务据此设置任务的租约持有者和过期时间。如果消费者在租约期内未完成任务（崩溃、超时等），队列服务会在租约过期后自动释放任务供其他消费者处理。

**Complete**：标记任务成功完成，上报执行耗时（微秒精度）。队列服务会将任务从活跃队列移除或归档。

**Fail**：标记任务失败。`permanent=true` 表示永久失败（任务存在不可恢复的问题，如数据格式错误），不再重试；`permanent=false` 表示临时失败（如网络超时），可重试。`retryWait` 可选，指定重试前的等待时间（秒）。

**Yield**：主动让出任务，指定延迟秒数后重新变为可处理状态。与临时失败的区别：Yield 不增加失败计数，语义上表示"当前不适合处理，稍后再来"（如依赖的外部服务暂时不可用）。

#### 4.2.4 HTTP 通信层

```go
func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
    url := c.baseURL + path
    var bodyReader io.Reader
    if body != nil {
        bodyReader = bytes.NewReader(body)
    }
    req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
    if err != nil {
        return nil, err
    }
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Accept", "application/json")
    if c.token != "" {
        req.Header.Set("X-Service-Token", c.token)
    }
    return req, nil
}
```

所有请求统一设置 `Content-Type` 和 `Accept` 为 `application/json`，使用 `X-Service-Token` 头进行服务间认证。`http.NewRequestWithContext` 确保请求可以通过 context 取消——当消费者收到关闭信号时，正在进行的 HTTP 请求会被立即取消。

```go
func (c *Client) doJSON(req *http.Request, out any) error {
    resp, err := c.httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("http: %w", err)
    }
    defer func() { _ = resp.Body.Close() }()

    respBody, err := io.ReadAll(resp.Body)
    if err != nil {
        return fmt.Errorf("read body: %w", err)
    }

    var envelope apiResponse
    if err := json.Unmarshal(respBody, &envelope); err != nil {
        return fmt.Errorf("unmarshal (status %d): %w", resp.StatusCode, err)
    }

    if envelope.Error != nil {
        return fmt.Errorf("api error [%s]: %s", envelope.Error.Code, envelope.Error.Message)
    }

    if out != nil && envelope.Data != nil {
        if err := json.Unmarshal(envelope.Data, out); err != nil {
            return fmt.Errorf("unmarshal data: %w", err)
        }
    }
    return nil
}
```

`doJSON` 使用两层 JSON 反序列化——先解析外层信封（`apiResponse`），检查是否有 API 错误；无错误时再将 `Data` 字段反序列化为目标类型。这种信封模式与 gorge-task-queue 的 API 响应格式匹配，统一了成功和错误的处理路径。

`envelope.Data` 使用 `json.RawMessage` 延迟解析，避免在只关心错误（如 Complete/Fail/Yield 操作）时做不必要的反序列化。

### 4.3 处理器注册表（Registry）

注册表位于 `internal/worker/registry.go`，是连接消费引擎和具体处理器的核心组件。

#### 4.3.1 TaskHandler 类型

```go
type TaskHandler func(ctx context.Context, task *taskqueue.Task, data json.RawMessage) error
```

`TaskHandler` 是一个函数类型，而非接口。选择函数类型而非接口的原因：

- 大多数处理器是轻量的闭包（如 Conduit 委托处理器只需捕获一个 `ConduitClient`），不需要额外的结构体。
- 函数类型支持内联定义，减少模板代码。
- Go 的闭包天然支持依赖注入——通过构造函数参数注入依赖，返回闭包。

`data` 参数使用 `json.RawMessage`（即 `[]byte`），将 JSON 反序列化推迟到处理器内部。不同任务类型的 Data 格式不同，注册表无需了解具体格式。

#### 4.3.2 三级错误语义

```go
type PermanentError struct {
    Msg string
}

func (e *PermanentError) Error() string { return e.Msg }

type YieldError struct {
    Msg      string
    Duration int // seconds, minimum 5
}

func (e *YieldError) Error() string { return e.Msg }
```

处理器通过返回值的类型（而非错误码或枚举）表达执行结果：

| 返回值 | 语义 | 消费者行为 |
|--------|------|-----------|
| `nil` | 成功 | 调用 `Complete`，增加 `processed` 计数 |
| `*PermanentError` | 永久失败 | 调用 `Fail(permanent=true)`，增加 `failed` 计数 |
| `*YieldError` | 主动让出 | 调用 `Yield(duration)`，不增加失败计数 |
| 其他 `error` | 临时失败 | 调用 `Fail(permanent=false)`，增加 `failed` 计数 |

使用 Go 的 `errors.As` 进行类型匹配，支持 error wrapping——处理器可以返回被包装过的 `*PermanentError`（如 `fmt.Errorf("parse: %w", &PermanentError{...})`），消费者仍然能正确识别。

`YieldError.Duration` 有 5 秒的最小值约束，由消费者在调用 `Yield` 前强制执行，防止处理器设置过短的延迟导致任务"空转"。

#### 4.3.3 注册表实现

```go
type Registry struct {
    handlers map[string]TaskHandler
}

func NewRegistry() *Registry {
    return &Registry{
        handlers: make(map[string]TaskHandler),
    }
}

func (r *Registry) Register(taskClass string, handler TaskHandler) {
    r.handlers[taskClass] = handler
}

func (r *Registry) Get(taskClass string) (TaskHandler, bool) {
    h, ok := r.handlers[taskClass]
    return h, ok
}

func (r *Registry) Has(taskClass string) bool {
    _, ok := r.handlers[taskClass]
    return ok
}

func (r *Registry) SupportedClasses() []string {
    classes := make([]string, 0, len(r.handlers))
    for k := range r.handlers {
        classes = append(classes, k)
    }
    return classes
}
```

注册表使用简单的 `map[string]TaskHandler` 实现，key 是任务类型字符串（对应 PHP 的 Worker 类名），value 是处理器函数。

注册表没有使用锁保护——这是安全的，因为注册发生在程序启动阶段（`main` 函数中的 `handlers.RegisterAll` 调用），此时消费者尚未启动。消费者运行后只执行读操作（`Get`/`Has`），Go 的 map 支持并发读。

`SupportedClasses` 返回所有已注册的任务类型列表，用于日志输出和统计接口。

### 4.4 消费引擎（Consumer）

消费引擎位于 `internal/worker/consumer.go`，是整个服务的核心组件。

#### 4.4.1 数据结构

```go
type Consumer struct {
    client       *taskqueue.Client
    registry     *Registry
    leaseLimit   int
    pollInterval time.Duration
    maxWorkers   int
    idleTimeout  time.Duration
    filter       map[string]bool

    processed atomic.Int64
    failed    atomic.Int64
    active    atomic.Int32
}
```

Consumer 的字段分为两组：

**配置字段**（初始化后不变）：
- `client`：任务队列客户端。
- `registry`：处理器注册表。
- `leaseLimit`：单次 Lease 拉取的任务上限。
- `pollInterval`：轮询间隔。
- `maxWorkers`：最大并发 worker 数。
- `idleTimeout`：连续无任务多久后进入休眠。
- `filter`：任务类型白名单，`map[string]bool` 支持 O(1) 查找。

**运行时统计（原子变量）**：
- `processed`：累计成功处理的任务数。
- `failed`：累计失败的任务数（包括永久和临时失败）。
- `active`：当前正在执行的任务数。

使用 `atomic` 类型而非互斥锁保护统计变量：统计更新是热路径操作（每个任务执行前后各一次），原子操作的开销远小于锁获取/释放。`processed` 和 `failed` 使用 `Int64` 避免溢出（高吞吐场景下 `Int32` 约 22 亿次后溢出），`active` 使用 `Int32` 即可（并发数不会很大）。

#### 4.4.2 任务过滤

```go
func NewConsumer(..., taskClassFilter []string) *Consumer {
    filter := make(map[string]bool, len(taskClassFilter))
    for _, tc := range taskClassFilter {
        filter[tc] = true
    }
    // ...
}

func (c *Consumer) shouldProcess(task *taskqueue.Task) bool {
    if len(c.filter) > 0 && !c.filter[task.TaskClass] {
        return false
    }
    return c.registry.Has(task.TaskClass)
}
```

`shouldProcess` 执行两级检查：

1. **过滤器检查**：如果配置了 `TASK_CLASS_FILTER`（`filter` 非空），只允许白名单中的任务类型通过。
2. **注册表检查**：即使通过了过滤器，还需要确认注册表中有对应的处理器。这防止了配置了过滤器但忘记注册处理器的情况。

不满足条件的任务被标记为临时失败，60 秒后重试（`Fail(permanent=false, retryWait=60)`）——不是永久失败，因为可能有其他支持该类型的 gorge-worker 实例能处理它。

#### 4.4.3 消费循环

```go
func (c *Consumer) Run(ctx context.Context) {
    log.Printf("[worker] consumer started: lease_limit=%d poll=%s workers=%d supported=%v",
        c.leaseLimit, c.pollInterval, c.maxWorkers, c.registry.SupportedClasses())

    sem := make(chan struct{}, c.maxWorkers)
    ticker := time.NewTicker(c.pollInterval)
    defer ticker.Stop()

    var idleSince *time.Time

    for {
        select {
        case <-ctx.Done():
            log.Println("[worker] consumer shutting down")
            return
        case <-ticker.C:
            tasks, err := c.client.Lease(ctx, c.leaseLimit)
            if err != nil {
                log.Printf("[worker] lease error: %v", err)
                continue
            }

            if len(tasks) == 0 {
                if idleSince == nil {
                    now := time.Now()
                    idleSince = &now
                } else if c.idleTimeout > 0 && time.Since(*idleSince) > c.idleTimeout {
                    log.Printf("[worker] idle for %s, hibernating...", c.idleTimeout)
                    c.hibernate(ctx)
                    idleSince = nil
                }
                continue
            }

            idleSince = nil
            var wg sync.WaitGroup
            for _, task := range tasks {
                if !c.shouldProcess(task) {
                    log.Printf("[worker] skipping unsupported taskClass=%s id=%d",
                        task.TaskClass, task.ID)
                    retryWait := 60
                    _ = c.client.Fail(ctx, task.ID, false, &retryWait)
                    continue
                }

                sem <- struct{}{}
                wg.Add(1)
                go func(t *taskqueue.Task) {
                    defer func() {
                        <-sem
                        wg.Done()
                    }()
                    c.processTask(ctx, t)
                }(task)
            }
            wg.Wait()
        }
    }
}
```

消费循环的设计有几个关键决策：

**信号量并发控制**：`sem := make(chan struct{}, c.maxWorkers)` 创建一个容量为 `maxWorkers` 的 channel 作为信号量。每个任务在启动 goroutine 前向 channel 发送一个值（`sem <- struct{}{}`），goroutine 完成后取回值（`<-sem`）。当 channel 满时，发送操作阻塞，自动限制并发数。

相比使用 `sync.Pool` 或第三方 worker pool 库，channel 信号量是 Go 中最惯用、最轻量的并发控制方式，无需引入额外依赖。

**批次同步**：`wg.Wait()` 确保当前批次的所有任务完成后才拉取下一批。这是有意的设计选择——避免任务堆积。如果不等待，连续快速的 Lease 可能拉取远超 `maxWorkers` 数量的任务，堆积在信号量的等待队列中。批次同步确保系统中的活跃任务数始终不超过 `leaseLimit`。

**空闲检测**：使用 `idleSince` 指针追踪空闲起始时间。指针为 `nil` 表示非空闲状态，非 `nil` 表示从指向的时间点开始空闲。一旦拉取到任务，立即重置为 `nil`。这种设计避免了额外的布尔标志和时间变量。

**错误容忍**：Lease 失败时只记录日志并 `continue`，不中断消费循环。网络抖动等临时问题在下一次轮询时通常会自恢复。不支持任务的 `shouldProcess` 检查失败时也只标记为临时失败，不影响其他任务的处理。

#### 4.4.4 任务执行

```go
func (c *Consumer) processTask(ctx context.Context, task *taskqueue.Task) {
    c.active.Add(1)
    defer c.active.Add(-1)

    start := time.Now()

    handler, ok := c.registry.Get(task.TaskClass)
    if !ok {
        log.Printf("[worker] no handler for taskClass=%s id=%d", task.TaskClass, task.ID)
        _ = c.client.Fail(ctx, task.ID, true, nil)
        c.failed.Add(1)
        return
    }

    var data json.RawMessage
    if task.Data != "" {
        data = json.RawMessage(task.Data)
    }

    err := handler(ctx, task, data)
    durationUs := time.Since(start).Microseconds()

    if err == nil {
        log.Printf("[worker] completed: taskClass=%s id=%d duration=%s",
            task.TaskClass, task.ID, time.Since(start))
        _ = c.client.Complete(ctx, task.ID, durationUs)
        c.processed.Add(1)
        return
    }

    var permErr *PermanentError
    var yieldErr *YieldError

    if errors.As(err, &permErr) {
        log.Printf("[worker] permanent failure: taskClass=%s id=%d err=%s",
            task.TaskClass, task.ID, err)
        _ = c.client.Fail(ctx, task.ID, true, nil)
        c.failed.Add(1)
    } else if errors.As(err, &yieldErr) {
        dur := yieldErr.Duration
        if dur < 5 {
            dur = 5
        }
        log.Printf("[worker] yield: taskClass=%s id=%d duration=%ds reason=%s",
            task.TaskClass, task.ID, dur, err)
        _ = c.client.Yield(ctx, task.ID, dur)
    } else {
        log.Printf("[worker] temporary failure: taskClass=%s id=%d err=%s",
            task.TaskClass, task.ID, err)
        _ = c.client.Fail(ctx, task.ID, false, nil)
        c.failed.Add(1)
    }
}
```

`processTask` 的执行流程：

1. **活跃计数**：`active.Add(1)` / `defer active.Add(-1)` 追踪当前并发执行的任务数，通过 `Stats()` 接口暴露。
2. **处理器查找**：从注册表获取处理器。理论上 `shouldProcess` 已经确认了处理器存在，但 `Get` 仍然做了二次检查——防御性编程。找不到处理器时标记为永久失败，因为这表明系统配置错误。
3. **数据转换**：`task.Data`（字符串）转为 `json.RawMessage`（字节切片），空字符串转为 `nil`。
4. **执行与计时**：调用处理器，记录执行耗时。耗时以微秒精度上报给队列服务。
5. **结果分发**：根据返回错误的类型调用对应的队列操作。使用 `errors.As` 而非类型断言，支持 wrapped error。

注意所有队列操作（Complete/Fail/Yield）的错误都被忽略（`_ = c.client.XXX`）。这是有意的设计——队列操作失败通常是网络问题，任务的租约会在过期后自动释放。记录结果失败不应影响消费者继续处理其他任务。

#### 4.4.5 空闲休眠

```go
func (c *Consumer) hibernate(ctx context.Context) {
    timer := time.NewTimer(3 * time.Minute)
    defer timer.Stop()
    select {
    case <-ctx.Done():
    case <-timer.C:
    }
}
```

当消费者连续 `idleTimeout`（默认 180 秒）内未拉取到任何任务时，进入休眠状态，暂停轮询 3 分钟。休眠的目的是减少对队列服务的无效请求——在任务稀疏的时段，每秒一次的 Lease 请求几乎全部返回空结果，消耗了队列服务的带宽和处理能力。

休眠使用 `select` 同时监听 context 取消和定时器，确保在收到关闭信号时能立即唤醒。休眠结束后，`idleSince` 被重置为 `nil`，消费者恢复正常轮询节奏。

### 4.5 处理器实现

#### 4.5.1 处理器注册

```go
func RegisterAll(registry *worker.Registry, conduitURL, conduitToken string) {
    if conduitURL != "" {
        conduit := NewConduitClient(conduitURL, conduitToken)

        registry.Register("FeedPublisherHTTPWorker", NewConduitDelegateHandler(conduit))
        registry.Register("PhabricatorSearchWorker", NewConduitDelegateHandler(conduit))
        registry.Register("PhabricatorMetaMTAWorker", NewConduitDelegateHandler(conduit))
        registry.Register("PhabricatorApplicationTransactionPublishWorker", NewConduitDelegateHandler(conduit))
        registry.Register("HeraldWebhookWorker", NewConduitDelegateHandler(conduit))
    }
}
```

`RegisterAll` 是处理器注册的入口，由 `main` 函数调用。只有在配置了 `CONDUIT_URL` 时才注册 Conduit 委托处理器。这意味着如果没有配置 Conduit，gorge-worker 启动后不会注册任何处理器，拉取到的所有任务都会被标记为临时失败。

每种任务类型使用独立的 `NewConduitDelegateHandler(conduit)` 调用创建处理器——虽然它们共享同一个 `ConduitClient`，但每个处理器是独立的闭包实例。这种设计留下了扩展空间：未来可以为不同任务类型创建不同类型的处理器，而不需要修改注册逻辑。

五种 Conduit 委托的任务类型及其选择委托模式的原因：

| 任务类 | PHP 依赖 | 委托原因 |
|--------|----------|----------|
| `FeedPublisherHTTPWorker` | `FeedStory`、`PhabricatorFeedStoryPublisher` | 需要从数据库加载 FeedStory 并渲染文本内容 |
| `PhabricatorSearchWorker` | `PhabricatorIndexEngine`、多个搜索引擎扩展 | 依赖完整的搜索索引引擎和可扩展的索引器接口 |
| `PhabricatorMetaMTAWorker` | `PhabricatorMetaMTAMail`、邮件适配器 | 需要渲染邮件模板、处理多种邮件发送适配器 |
| `PhabricatorApplicationTransactionPublishWorker` | Transaction Editor、Herald、Feed | 涉及事务应用、通知生成、Herald 规则评估等复杂链路 |
| `HeraldWebhookWorker` | `HeraldWebhookRequest`、`HeraldWebhook` | 需要加载 webhook 配置、构造请求、处理签名验证 |

#### 4.5.2 Conduit 客户端

```go
type ConduitClient struct {
    baseURL    string
    token      string
    httpClient *http.Client
}

type ConduitResponse struct {
    Result    json.RawMessage `json:"result"`
    ErrorCode *string         `json:"error_code"`
    ErrorInfo *string         `json:"error_info"`
}

func (c *ConduitClient) Call(ctx context.Context, method string, params map[string]any) (*ConduitResponse, error) {
    if params == nil {
        params = make(map[string]any)
    }
    params["__conduit__"] = true

    body, err := json.Marshal(params)
    if err != nil {
        return nil, fmt.Errorf("marshal params: %w", err)
    }

    url := fmt.Sprintf("%s/api/%s", c.baseURL, method)
    req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
    if err != nil {
        return nil, fmt.Errorf("build request: %w", err)
    }
    req.Header.Set("Content-Type", "application/json")
    if c.token != "" {
        req.Header.Set("X-Service-Token", c.token)
    }

    // ... HTTP 执行与响应解析 ...
}
```

Conduit 客户端封装了与 Phorge Conduit API（通过 gorge-conduit 网关或直接连接 PHP）的通信。

**`__conduit__` 标记**：`params["__conduit__"] = true` 是 Phorge Conduit 协议的约定。这个标记告诉 Conduit 端点请求来自 API 调用（而非浏览器表单提交），触发 JSON 解析路径而非表单解析路径。

**响应格式**：Conduit API 的响应格式与 gorge-task-queue 不同——使用 `result` / `error_code` / `error_info` 三个顶层字段，而非 `data` / `error` 信封。`ConduitResponse` 结构体映射了这个格式。`ErrorCode` 和 `ErrorInfo` 使用指针类型（`*string`），在 JSON 中 `null` 时解析为 `nil`，方便判断是否有错误。

**认证方式**：使用 `X-Service-Token` 头，与 Gorge 平台其他服务的认证方式一致。这个 token 通常由 gorge-conduit 网关验证，授权内部服务调用 Conduit 方法。

#### 4.5.3 Conduit 委托处理器

```go
func NewConduitDelegateHandler(conduit *ConduitClient) worker.TaskHandler {
    return func(ctx context.Context, task *taskqueue.Task, data json.RawMessage) error {
        params := map[string]any{
            "taskID":    task.ID,
            "taskClass": task.TaskClass,
            "data":      string(data),
        }

        result, err := conduit.Call(ctx, "worker.execute", params)
        if err != nil {
            return fmt.Errorf("conduit worker.execute for %s (id=%d): %w",
                task.TaskClass, task.ID, err)
        }

        log.Printf("[delegate] taskClass=%s id=%d delegated via conduit, result=%s",
            task.TaskClass, task.ID, string(result.Result))
        return nil
    }
}
```

委托处理器是整个 gorge-worker 架构中最关键的设计模式——**Go 调度，PHP 执行**。

处理器调用 Conduit 的 `worker.execute` 方法，传入三个参数：

- `taskID`：任务 ID，PHP 端据此从数据库加载完整的任务记录。
- `taskClass`：任务类型，PHP 端据此实例化对应的 Worker 类。
- `data`：任务负载 JSON，PHP 端据此反序列化任务数据。

PHP 端的 `worker.execute` 方法执行实际的业务逻辑（如索引构建、邮件发送），然后返回结果。如果 PHP 执行成功，处理器返回 `nil`（成功）。如果 Conduit 调用失败（网络错误、PHP 抛出异常等），处理器返回普通 error（临时失败），消费者会将任务标记为可重试。

这种委托模式的优势：

1. **渐进迁移**：可以逐步将任务处理逻辑从 PHP 迁移到 Go。先全部委托，再逐个替换为 Go 本地实现。
2. **风险隔离**：PHP 业务逻辑的 bug 不会影响消费引擎的稳定性。即使 PHP 崩溃，消费者仍然正常运行，将任务标记为失败后继续处理其他任务。
3. **代码复用**：避免在 Go 中重写 Phorge 大量复杂的 PHP 业务逻辑（搜索引擎、邮件模板、Herald 规则评估等），这些逻辑经过多年的迭代已经相当复杂。

#### 4.5.4 Feed HTTP 处理器

```go
type FeedHTTPData struct {
    URI     string `json:"uri"`
    StoryID int64  `json:"storyID"`
}

func NewFeedHTTPHandler() worker.TaskHandler {
    client := &http.Client{
        Timeout: 15 * time.Second,
    }

    return func(ctx context.Context, task *taskqueue.Task, data json.RawMessage) error {
        var td FeedHTTPData
        if err := json.Unmarshal(data, &td); err != nil {
            return &worker.PermanentError{Msg: fmt.Sprintf("invalid task data: %v", err)}
        }

        if td.URI == "" {
            return &worker.PermanentError{Msg: "missing URI in task data"}
        }

        formData := url.Values{}
        formData.Set("storyID", fmt.Sprintf("%d", td.StoryID))

        req, err := http.NewRequestWithContext(ctx, http.MethodPost, td.URI,
            strings.NewReader(formData.Encode()))
        if err != nil {
            return fmt.Errorf("build request: %w", err)
        }
        req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

        resp, err := client.Do(req)
        if err != nil {
            return fmt.Errorf("http request to %s failed: %w", td.URI, err)
        }
        defer func() { _ = resp.Body.Close() }()

        if resp.StatusCode >= 200 && resp.StatusCode < 300 {
            return nil
        }

        return fmt.Errorf("feed HTTP hook %s returned status %d", td.URI, resp.StatusCode)
    }
}
```

Feed HTTP 处理器是一个**Go 本地实现**的任务处理器（相对于 Conduit 委托），展示了如何用 Go 直接处理简单任务。

工作原理：`FeedPublisherHTTPWorker` 任务的数据包含目标 URI 和 story ID，处理器将 story ID 以 `application/x-www-form-urlencoded` 格式 POST 到目标 URI。与 PHP 版本的 `PhabricatorFeedPublisherHTTPWorker::doWork()` 行为一致。

错误处理体现了三级语义的运用：

- **数据解析失败** → `PermanentError`：数据格式错误是不可恢复的问题，重试不会改变结果。
- **URI 为空** → `PermanentError`：同上，缺少必要数据。
- **HTTP 请求失败** → 普通 error（临时失败）：网络问题通常是暂时的，重试可能成功。
- **非 2xx 响应** → 普通 error（临时失败）：目标服务可能暂时不可用。

HTTP 客户端设置了 15 秒超时，比 Conduit 客户端的 30 秒更短——Feed HTTP 推送是简单的通知行为，目标服务应该快速响应。

当前 `setup.go` 中 `FeedPublisherHTTPWorker` 注册的是 Conduit 委托处理器而非此本地处理器——因为 PHP 版本在 POST 前还需要从数据库加载 `FeedStory` 并渲染文本内容。如果目标 URI 只需要 story ID（由接收方自行获取 story 详情），则可以切换到此本地处理器，跳过 PHP 调用。

#### 4.5.5 Noop 处理器

```go
func NewNoopHandler(taskClass string) worker.TaskHandler {
    return func(ctx context.Context, task *taskqueue.Task, data json.RawMessage) error {
        log.Printf("[noop] completed taskClass=%s id=%d", taskClass, task.ID)
        return nil
    }
}
```

Noop 处理器只记录日志并返回成功，用于两种场景：

- **占位**：某些任务类型不需要在 Go 端做任何处理（PHP 端已经在写入队列前完成了实际工作），只需要确认消费即可。
- **测试**：在开发和调试阶段，注册 Noop 处理器可以快速消费队列中的任务，验证消费引擎的工作流程。

当前 `setup.go` 中未使用 Noop 处理器，但作为工具保留，供部署时按需注册。

### 4.6 HTTP API

HTTP API 位于 `internal/httpapi/handlers.go`，基于 Echo 框架实现。

#### 4.6.1 路由注册

```go
func RegisterRoutes(e *echo.Echo, deps *Deps) {
    e.GET("/", healthPing())
    e.GET("/healthz", healthPing())

    g := e.Group("/api/worker")
    g.Use(tokenAuth(deps))

    g.GET("/stats", stats(deps))
}
```

路由设计简洁：

- **健康检查**（`/` 和 `/healthz`）：不需要认证，返回 `{"status":"ok"}`。两个路径提供相同的功能——`/` 用于简单探测，`/healthz` 符合 Kubernetes 健康检查约定。Docker `HEALTHCHECK` 使用 `/healthz`。
- **管理接口**（`/api/worker/*`）：通过 `tokenAuth` 中间件保护。当前只有一个端点 `/api/worker/stats`，返回消费者的运行统计。

#### 4.6.2 Token 认证中间件

```go
func tokenAuth(deps *Deps) echo.MiddlewareFunc {
    return func(next echo.HandlerFunc) echo.HandlerFunc {
        return func(c echo.Context) error {
            if deps.Token == "" {
                return next(c)
            }
            token := c.Request().Header.Get("X-Service-Token")
            if token == "" {
                token = c.QueryParam("token")
            }
            if token == "" || token != deps.Token {
                return c.JSON(http.StatusUnauthorized, &apiResponse{
                    Error: &apiError{Code: "ERR_UNAUTHORIZED", Message: "missing or invalid service token"},
                })
            }
            return next(c)
        }
    }
}
```

认证中间件的行为与 Gorge 平台其他服务一致：

- **未配置 Token 时跳过认证**：`deps.Token == ""` 时直接放行。这简化了开发环境的使用——不需要配置 Token 即可访问统计接口。
- **两种认证方式**：优先从 `X-Service-Token` 请求头读取，其次从 `?token=` 查询参数读取。请求头方式适合程序化调用，查询参数方式适合浏览器直接访问。
- **统一错误格式**：认证失败返回 JSON 格式的错误响应，与 API 的正常响应格式一致。

#### 4.6.3 统计接口

```go
type ConsumerStats struct {
    Processed int64    `json:"processed"`
    Failed    int64    `json:"failed"`
    Active    int32    `json:"active"`
    Supported []string `json:"supported"`
}

func stats(deps *Deps) echo.HandlerFunc {
    return func(c echo.Context) error {
        s := deps.Consumer.Stats()
        return c.JSON(http.StatusOK, &apiResponse{Data: s})
    }
}
```

统计接口返回四个指标：

| 字段 | 含义 |
|------|------|
| `processed` | 累计成功处理的任务数 |
| `failed` | 累计失败的任务数（永久 + 临时） |
| `active` | 当前正在执行的任务数 |
| `supported` | 已注册的任务类型列表 |

`supported` 字段对运维有用——可以快速确认 gorge-worker 实例支持哪些任务类型，用于排查"任务没有被消费"的问题。

### 4.7 入口程序

```go
func main() {
    cfg := config.LoadFromEnv()

    client := taskqueue.NewClient(cfg.TaskQueueURL, cfg.TaskQueueToken)

    registry := worker.NewRegistry()
    handlers.RegisterAll(registry, cfg.ConduitURL, cfg.ConduitToken)

    consumer := worker.NewConsumer(
        client, registry,
        cfg.LeaseLimit, cfg.PollIntervalMs,
        cfg.MaxWorkers, cfg.IdleTimeoutSec,
        cfg.TaskClassFilter,
    )

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    go consumer.Run(ctx)

    e := echo.New()
    e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
        LogStatus: true, LogURI: true, LogMethod: true,
        LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
            c.Logger().Infof("%s %s %d", v.Method, v.URI, v.Status)
            return nil
        },
    }))
    e.Use(middleware.Recover())

    httpapi.RegisterRoutes(e, &httpapi.Deps{
        Consumer: consumer,
        Token:    cfg.ServiceToken,
    })

    go func() {
        sigCh := make(chan os.Signal, 1)
        signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
        <-sigCh
        cancel()
        _ = e.Shutdown(context.Background())
    }()

    e.Logger.Fatal(e.Start(cfg.ListenAddr))
}
```

启动流程的设计要点：

**初始化顺序**：配置 → 队列客户端 → 注册表和处理器 → 消费者 → HTTP 服务。每一步的依赖由上一步提供，形成清晰的依赖链。

**消费者与 HTTP 并行**：`go consumer.Run(ctx)` 在后台 goroutine 中运行消费循环，主 goroutine 运行 Echo HTTP 服务。两者通过 `ctx` 共享生命周期——取消 context 会同时停止消费者和 HTTP 服务。

**优雅关闭**：信号处理 goroutine 捕获 `SIGINT`（Ctrl+C）和 `SIGTERM`（`docker stop`），执行两步关闭：
1. `cancel()` 取消 context，通知消费者停止拉取新任务。正在执行的任务通过 context 传播获得取消信号。
2. `e.Shutdown(context.Background())` 优雅关闭 HTTP 服务，等待正在处理的请求完成。

**请求日志**：Echo 的 `RequestLoggerWithConfig` 中间件记录每个 HTTP 请求的方法、URI 和响应状态码。`Recover` 中间件捕获 handler 中的 panic，返回 500 而非进程崩溃。

## 5. 委托模式的工作原理

### 5.1 完整的委托调用链

```
                      gorge-worker                gorge-conduit / Phorge
                      ─────────────               ────────────────────────

1. Consumer.Run()
   │
2. client.Lease()  ──────────────────► gorge-task-queue
   ◄─── tasks ──────────────────────  (返回待处理任务)
   │
3. processTask()
   │ registry.Get(taskClass)
   │
4. handler(ctx, task, data)
   │ (ConduitDelegateHandler)
   │
5. conduit.Call("worker.execute")  ──► POST /api/worker.execute
   │                                    │
   │                                    ▼
   │                              PHP: WorkerExecuteConduitAPIMethod
   │                                    │
   │                                    ▼
   │                              实例化 Worker 类
   │                              $worker = new $taskClass()
   │                              $worker->setData($data)
   │                              $worker->executeTask()
   │                                    │
   │                                    ▼
   │                              执行业务逻辑
   │                              (索引/邮件/Feed/...)
   │                                    │
   ◄─── result ────────────────────────┘
   │
6. client.Complete(taskID)  ────────► gorge-task-queue
                                      (标记任务完成)
```

### 5.2 与直接 PHP 消费的对比

| 维度 | PhabricatorTaskmasterDaemon (PHP) | gorge-worker + Conduit 委托 |
|------|-----------------------------------|---------------------------|
| 进程模型 | 多个 PHP 进程，每个单线程 | 单个 Go 进程，goroutine 并发 |
| 任务获取 | 直接查询 MySQL | 通过 gorge-task-queue HTTP API |
| 并发度 | 进程数 × 1 | maxWorkers（goroutine） |
| 内存开销 | 30-50MB/进程 | ~4KB/goroutine + 共享运行时 |
| 扩缩容 | 增减 PHP 进程 | 调整 maxWorkers 或增加实例 |
| 业务逻辑 | PHP 直接执行 | Go 调度 → Conduit → PHP 执行 |
| 额外延迟 | 无 | 一次 Conduit HTTP 往返（通常 <50ms） |
| 容错 | PHP 进程崩溃需 phd 重启 | Go 进程持续运行，任务级错误隔离 |

## 6. 并发模型

### 6.1 goroutine 分布

gorge-worker 运行时的 goroutine 分布：

```
main goroutine (Echo HTTP 服务)
  └── 处理 HTTP 请求 (/healthz, /api/worker/stats)

consumer goroutine (消费循环)
  ├── task worker goroutine 1  (受 sem 信号量限制)
  ├── task worker goroutine 2
  ├── ...
  └── task worker goroutine N  (N ≤ maxWorkers)

signal handler goroutine (信号处理)
  └── 等待 SIGINT/SIGTERM
```

正常运行时的 goroutine 数量：3（main + consumer + signal）+ 活跃任务数（0 ~ maxWorkers）。以默认 `maxWorkers=4` 计算，满载时为 7 个 goroutine。

### 6.2 并发安全分析

gorge-worker 的并发安全建立在以下保证之上：

**无共享可变状态的模块**：
- `Config`：初始化后只读，无并发修改。
- `Registry`：初始化后只读（`Register` 在 `main` 中调用，消费者启动前完成）。
- `taskqueue.Client`：所有字段在初始化后不变。`httpClient` 是并发安全的（Go 标准库保证）。

**原子操作保护的统计变量**：
- `Consumer.processed`、`Consumer.failed`：`atomic.Int64`，多个 worker goroutine 可以安全并发更新。
- `Consumer.active`：`atomic.Int32`，同上。

**channel 协调的并发控制**：
- `sem`：channel 信号量限制并发 goroutine 数量。
- `wg`：`sync.WaitGroup` 确保批次内所有任务完成后再拉取下一批。

**无锁设计**：整个 gorge-worker 没有使用 `sync.Mutex` 或 `sync.RWMutex`。这得益于：
- 配置和注册表的"初始化后只读"模式。
- 统计变量的原子操作。
- 任务间无共享状态——每个任务在独立的 goroutine 中处理自己的数据，通过值传递（`*taskqueue.Task` 指针但各任务指向不同的 Task 实例）隔离。

## 7. 与 PhabricatorTaskmasterDaemon 的对应关系

| Go 实现 | PHP 原始实现 | 差异说明 |
|---------|-------------|----------|
| `consumer.Run()` | `PhabricatorTaskmasterDaemon::run()` | 轮询消费循环 |
| `consumer.processTask()` | `PhabricatorWorker::executeTask()` | 执行单个任务 |
| `worker.Registry` | `PhabricatorWorker::getWorkerClass()` | 任务类型到处理器的映射 |
| `taskqueue.Client.Lease()` | `PhabricatorWorkerLeaseQuery` | 获取待处理任务 |
| `taskqueue.Client.Complete()` | `PhabricatorWorkerActiveTask::archiveTask()` | 标记任务完成 |
| `taskqueue.Client.Fail()` | `PhabricatorWorkerActiveTask::setFailureCount()` | 标记任务失败 |
| `taskqueue.Client.Yield()` | `PhabricatorWorkerYieldException` | 主动让出任务 |
| `worker.PermanentError` | `PhabricatorWorkerPermanentFailureException` | 永久失败 |
| `worker.YieldError` | `PhabricatorWorkerYieldException` | 让出重试 |
| `handlers.ConduitDelegateHandler` | (无对应) | Go 新增的委托模式 |
| `consumer.hibernate()` | (无对应) | Go 新增的空闲休眠机制 |
| `httpapi.stats()` | `phd status` | 运行状态查询 |

关键行为兼容点：

- **错误语义**：Go 的 `PermanentError` / `YieldError` / 普通 error 三级语义与 PHP 的 `PhabricatorWorkerPermanentFailureException` / `PhabricatorWorkerYieldException` / 普通 `Exception` 一一对应。
- **任务类名**：Go 使用与 PHP 相同的任务类名字符串（如 `PhabricatorSearchWorker`），确保队列中的任务可以被任一消费者（PHP 或 Go）处理。
- **Yield 最小时长**：Go 强制 Yield 最少 5 秒，与 PHP 的 `PhabricatorWorkerYieldException` 默认行为一致。

## 8. 部署方案

### 8.1 Docker 镜像

采用多阶段构建：

- **构建阶段**：基于 `golang:1.26-alpine3.22`，使用 `CGO_ENABLED=0` 静态编译，`-ldflags="-s -w"` 去除调试信息和符号表以缩小二进制体积。
- **运行阶段**：基于 `alpine:3.20`，仅包含编译后的二进制和 CA 证书（Conduit 和队列通信可能需要 HTTPS）。

暴露 8170 端口。

内置 Docker `HEALTHCHECK`，每 10 秒通过 `wget` 检查 `/healthz` 端点，启动等待 5 秒，超时 3 秒，最多重试 3 次。

### 8.2 部署模式

**单实例全量消费**：一个 gorge-worker 实例处理所有支持的任务类型。适合中小规模部署。

```bash
docker run -p 8170:8170 \
  -e TASK_QUEUE_URL=http://task-queue:8090 \
  -e CONDUIT_URL=http://conduit:8080 \
  -e MAX_WORKERS=8 \
  gorge-worker
```

**多实例专一消费**：多组 gorge-worker 实例，每组通过 `TASK_CLASS_FILTER` 只处理特定任务类型。适合大规模部署和负载隔离。

```bash
# 搜索索引专用实例（CPU 密集，多 worker）
docker run -e TASK_CLASS_FILTER=PhabricatorSearchWorker \
  -e MAX_WORKERS=8 gorge-worker

# 邮件发送专用实例（I/O 密集，少 worker 即可）
docker run -e TASK_CLASS_FILTER=PhabricatorMetaMTAWorker \
  -e MAX_WORKERS=2 gorge-worker
```

### 8.3 资源估算

每个 gorge-worker 实例的资源占用：

- **内存**：Go 运行时基础 ~10-20MB + 每个活跃任务 ~4KB goroutine 栈 + HTTP 客户端缓冲。默认 `maxWorkers=4` 时，总内存约 20-30MB。即使 `maxWorkers=64`，内存也不超过 50MB。
- **CPU**：消费引擎本身开销极低（轮询 + HTTP 调用）。CPU 占用主要取决于任务处理逻辑——Conduit 委托模式下几乎无 CPU 消耗（HTTP I/O），本地处理器取决于具体逻辑。
- **网络**：每个轮询周期一次 Lease HTTP 请求，每个任务一次 Complete/Fail/Yield 请求 + 一次 Conduit 委托请求。默认配置下每秒最多 1 次 Lease + 4 次 Complete + 4 次 Conduit = 9 次 HTTP 请求。

## 9. 依赖分析

| 依赖 | 版本 | 用途 |
|------|------|------|
| `labstack/echo/v4` | v4.15.1 | HTTP 框架，路由和中间件 |

直接依赖仅一个。HTTP 客户端使用标准库 `net/http`，JSON 编解码使用标准库 `encoding/json`，并发控制使用标准库 `sync` 和 `sync/atomic`，信号处理使用标准库 `os/signal`。

选择 Echo 而非标准库 `net/http` 的原因：gorge-worker 的 HTTP 端只有少量路由，标准库完全够用。但 Gorge 平台的其他 Go 服务（gorge-db-api、gorge-conduit 等）统一使用 Echo 框架，保持技术栈一致性有利于团队维护和代码复用（如 Token 认证中间件的模式）。

## 10. 总结

gorge-worker 是一个职责专注的异步任务消费微服务，核心价值在于：

1. **替代 PHP 守护进程**：以 Go 替代 `PhabricatorTaskmasterDaemon`，消除 PHP 长期运行进程的内存泄漏、进程管理和并发限制问题。单进程 goroutine 并发取代多进程单线程模型，内存效率提升两个数量级。
2. **委托模式保持兼容**：通过 Conduit API 将复杂业务逻辑委托给 PHP 执行，Go 只负责调度和生命周期管理。这种分工让迁移零风险——PHP 代码不需要修改，业务行为完全一致。
3. **三级错误语义**：`PermanentError` / `YieldError` / 普通 error 精确映射 Phorge 原有的异常体系，确保任务重试策略与 PHP 版本一致。
4. **弹性消费**：信号量并发控制、批次同步、空闲休眠、任务类型过滤四个机制协同工作，在高负载时充分利用并发能力，在低负载时自动节省资源，在专一部署时实现负载隔离。
5. **极简实现**：唯一的第三方依赖是 Echo HTTP 框架，核心代码约 500 行（不含测试），消费引擎、注册表、处理器和 HTTP API 各司其职，模块边界清晰，易于理解和扩展。
