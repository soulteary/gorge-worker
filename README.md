# gorge-worker

Gorge 平台中的异步任务消费微服务，以 Go 实现，替代 Phorge 原有的 PHP `PhabricatorTaskmasterDaemon` 守护进程。

从 gorge-task-queue 拉取任务，通过注册表分发到不同处理器执行。对复杂业务逻辑通过 Conduit API 委托给 PHP 后端处理，对简单任务（如 Feed HTTP 推送）可在 Go 内直接完成。支持并发控制、三级错误语义和空闲休眠。

## 架构

```
gorge-task-queue                         Phorge PHP
      │                                      ▲
      │ Lease/Complete/Fail/Yield             │ Conduit worker.execute
      ▼                                      │
┌─────────────────────────────────────────────┐
│              gorge-worker :8170             │
│                                             │
│  Consumer ── Registry ── Handlers           │
│  (轮询消费)    (任务分发)   (执行/委托)       │
└─────────────────────────────────────────────┘
```

## 配置

通过环境变量配置：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `LISTEN_ADDR` | `:8170` | HTTP 服务监听地址 |
| `SERVICE_TOKEN` | (空) | API 认证 Token，通过 `X-Service-Token` 传递 |
| `TASK_QUEUE_URL` | `http://task-queue:8090` | gorge-task-queue 服务地址 |
| `TASK_QUEUE_TOKEN` | (空) | 任务队列 Service Token |
| `LEASE_LIMIT` | `4` | 单次拉取任务数上限 |
| `POLL_INTERVAL_MS` | `1000` | 轮询间隔（毫秒） |
| `MAX_WORKERS` | `4` | 最大并发 worker 数 |
| `IDLE_TIMEOUT_SEC` | `180` | 空闲多久进入休眠（秒） |
| `CONDUIT_URL` | (空) | Phorge Conduit API 地址，设置后启用委托模式 |
| `CONDUIT_TOKEN` | (空) | Conduit Service Token |
| `TASK_CLASS_FILTER` | (空) | 仅处理指定任务类型（逗号分隔），空表示全部支持类型 |

## 支持的任务类型

当配置 `CONDUIT_URL` 时，注册以下 Conduit 委托处理器：

| 任务类 | 说明 |
|--------|------|
| `FeedPublisherHTTPWorker` | Feed 消息推送 |
| `PhabricatorSearchWorker` | 搜索索引构建 |
| `PhabricatorMetaMTAWorker` | 邮件发送 |
| `PhabricatorApplicationTransactionPublishWorker` | 事务发布与通知 |
| `HeraldWebhookWorker` | Herald 规则 Webhook |

## API

### GET /healthz

健康检查端点，不需要认证。

```json
{"status": "ok"}
```

### GET /api/worker/stats

返回消费者运行统计。配置 `SERVICE_TOKEN` 时需要认证。

认证方式：
- 请求头：`X-Service-Token: <token>`
- 查询参数：`?token=<token>`

**响应**：

```json
{
  "data": {
    "processed": 1024,
    "failed": 3,
    "active": 2,
    "supported": [
      "FeedPublisherHTTPWorker",
      "PhabricatorSearchWorker",
      "PhabricatorMetaMTAWorker",
      "PhabricatorApplicationTransactionPublishWorker",
      "HeraldWebhookWorker"
    ]
  }
}
```

## 构建 & 运行

```bash
go build -o gorge-worker ./cmd/server
./gorge-worker
```

## Docker

```bash
docker build -t gorge-worker .
docker run -p 8170:8170 \
  -e TASK_QUEUE_URL=http://task-queue:8090 \
  -e CONDUIT_URL=http://conduit:8080 \
  gorge-worker
```

## 项目结构

```
gorge-worker/
├── cmd/server/
│   └── main.go                  # 服务入口，启动消费者与 HTTP 服务
├── internal/
│   ├── config/
│   │   └── config.go            # 环境变量配置加载
│   ├── handlers/
│   │   ├── setup.go             # 处理器注册，按配置注册 Conduit 委托
│   │   ├── conduit.go           # Conduit API 客户端
│   │   ├── delegate.go          # Conduit 委托处理器，转发到 PHP 执行
│   │   ├── feed_http.go         # Feed HTTP 推送处理器（Go 本地实现）
│   │   └── noop.go              # 空操作处理器，日志确认后完成
│   ├── httpapi/
│   │   └── handlers.go          # HTTP 路由、Token 认证中间件、统计接口
│   ├── taskqueue/
│   │   └── client.go            # 任务队列 HTTP 客户端（Lease/Complete/Fail/Yield）
│   └── worker/
│       ├── consumer.go          # 任务消费引擎，轮询、并发控制、空闲休眠
│       └── registry.go          # 处理器注册表，任务类型到 Handler 的映射
├── Dockerfile                   # 多阶段 Docker 构建
├── go.mod
└── go.sum
```

## 技术栈

- **语言**：Go 1.26
- **HTTP 框架**：[Echo](https://echo.labstack.com/) v4.15.1
- **许可证**：Apache License 2.0
