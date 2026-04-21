# llm-proxy-debugger Go 工程重构方案

## 一、现状问题

| 问题 | 严重度 | 影响 |
|---|---|---|
| 全部代码扁平在 `package main` | 🔴 高 | 无法复用、无法独立测试、职责不清 |
| 全局变量 (`logger`, `hub`, `store`) | 🟡 中 | 隐式耦合，难以测试和管理生命周期 |
| 每次请求新建 `http.Transport` | 🟡 中 | 连接池浪费，高并发下性能劣化 |
| `go.mod` module path 为 `goproxy` | 🟢 低 | 不符合 Go 社区惯例 |
| config 使用 `flag` 全局指针 | 🟢 低 | 代码散落多处 `*flagVar` 解引用 |
| 仅 SSE 解析器有单测 | 🟡 中 | 核心代理/协议逻辑无测试保护 |

---

## 二、目标目录结构

```
llm-proxy-debugger/
├── cmd/
│   └── proxy/
│       └── main.go              # 程序入口：解析配置 → 组装依赖 → 启动服务
│
├── internal/
│   ├── config/
│   │   └── config.go            # Config struct + Load() 函数
│   │
│   ├── logger/
│   │   └── logger.go            # New(cfg) → *zap.Logger
│   │
│   ├── proxy/
│   │   ├── handler.go           # createReverseProxy, handleHTTP, handleWebSocket
│   │   ├── recorder.go          # responseRecorder
│   │   ├── transport.go         # 共享 Transport 工厂
│   │   └── helpers.go           # getClientIP, extractHeaders, truncateBody, singleJoiningSlash
│   │
│   ├── protocol/
│   │   ├── handler.go           # ProtocolHandler 接口 + ProtocolMetrics
│   │   ├── anthropic.go         # AnthropicHandler
│   │   ├── openai.go            # OpenAIHandler
│   │   ├── accumulator.go       # SSEEventAccumulator
│   │   └── handler_test.go      # 协议解析单元测试
│   │
│   ├── sse/
│   │   ├── parser.go            # SSEParser + SSEEvent
│   │   └── parser_test.go       # 现有测试迁移
│   │
│   ├── hub/
│   │   └── hub.go               # Hub struct + serveWS
│   │
│   └── store/
│       ├── store.go             # GlobalStore, Session, Rule, RequestLog 类型定义
│       └── api.go               # apiSessionsHandler, apiRulesHandler
│
├── web/                         # 前端（不动）
├── go.mod                       # module path 改为 github.com/<user>/llm-proxy-debugger
└── go.sum
```

### 设计原则

- **`cmd/`** 只做依赖组装和启动，不含业务逻辑
- **`internal/`** 防止外部导入，保持封装
- 每个子包**职责单一**，可独立编写测试
- 消除全局变量，通过构造函数注入依赖

---

## 三、各模块重构细节

### 3.1 `internal/config`

将 `flag` 全局指针收拢为一个结构体：

```go
package config

type Config struct {
    ListenAddr     string
    TargetAddr     string
    LogDir         string
    MaxBodyLogSize int
}

func Load() *Config {
    cfg := &Config{}
    flag.StringVar(&cfg.ListenAddr, "listen", "0.0.0.0:12337", "监听地址")
    flag.StringVar(&cfg.TargetAddr, "target", "http://127.0.0.1:28000", "转发目标地址")
    flag.StringVar(&cfg.LogDir, "logdir", "log", "日志目录")
    flag.IntVar(&cfg.MaxBodyLogSize, "maxbody", 10240, "最大记录 body (字节)")
    flag.Parse()
    return cfg
}
```

### 3.2 `internal/logger`

```go
package logger

func New(logDir string) *zap.Logger { ... }
```

移除全局 `var logger`，由 `main.go` 创建后注入其他模块。

### 3.3 `internal/proxy`

**核心改动**：将 `http.Transport` 提升为共享实例。

```go
package proxy

type Server struct {
    target    *url.URL
    transport *http.Transport  // 共享，不再每次请求新建
    hub       *hub.Hub
    store     *store.Store
    logger    *zap.Logger
    cfg       *config.Config
}

func NewServer(cfg *config.Config, ...) *Server { ... }
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { ... }
```

### 3.4 `internal/protocol`

接口与实现保持不变，仅迁移到独立包：

```go
package protocol

type Handler interface {
    Parse(data []byte) (*Metrics, error)
    Name() string
}
```

- `AnthropicHandler` → `anthropic.go`
- `OpenAIHandler` → `openai.go`
- `SSEEventAccumulator` → `accumulator.go`

### 3.5 `internal/sse`

纯粹迁移 `SSEParser` + `SSEEvent` 到独立包，**零业务依赖**，可独立复用。

### 3.6 `internal/hub`

```go
package hub

type Hub struct { ... }

func New() *Hub { ... }
func (h *Hub) Run()
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request)
func (h *Hub) Broadcast(msg interface{})
```

### 3.7 `internal/store`

```go
package store

type Store struct {
    sync.RWMutex
    Sessions map[string]*Session
    Rules    []Rule
}

func New() *Store { ... }
func (s *Store) SessionsHandler(w http.ResponseWriter, r *http.Request)
func (s *Store) RulesHandler(w http.ResponseWriter, r *http.Request)
```

### 3.8 `cmd/proxy/main.go`

```go
func main() {
    cfg := config.Load()
    log := logger.New(cfg.LogDir)
    defer log.Sync()

    h := hub.New()
    go h.Run()

    s := store.New()
    srv := proxy.NewServer(cfg, log, h, s)

    mux := http.NewServeMux()
    mux.Handle("/", srv)
    mux.HandleFunc("/api/ws", h.ServeWS)
    mux.HandleFunc("/api/sessions", s.SessionsHandler)
    mux.HandleFunc("/api/rules", s.RulesHandler)

    // ... http.Server 启动
}
```

---

## 四、重构执行步骤

建议**逐步迁移**，每步保证可编译+测试通过：

| 步骤 | 操作 | 验证方式 |
|---|---|---|
| 1 | 修改 `go.mod` module path | `go build ./...` |
| 2 | 抽取 `internal/config` | `go build ./...` |
| 3 | 抽取 `internal/logger` | `go build ./...` |
| 4 | 抽取 `internal/sse` | `go test ./internal/sse/...` |
| 5 | 抽取 `internal/protocol` | `go build ./...` |
| 6 | 抽取 `internal/hub` | `go build ./...` |
| 7 | 抽取 `internal/store` | `go build ./...` |
| 8 | 抽取 `internal/proxy` + 修复 Transport 复用 | `go build ./...` |
| 9 | 创建 `cmd/proxy/main.go`，删除根目录 `.go` 文件 | `go build ./cmd/proxy/` |
| 10 | 补充单元测试 | `go test ./...` |

---

## 五、风险与注意事项

> [!WARNING]
> `go.mod` 的 module path 修改后，所有 import 路径都会变化。建议在同一次 commit 中完成。

> [!IMPORTANT]
> 重构过程中前端 `web/` 目录不受影响，但如果前端有硬编码的 Go 编译产物路径（如 `goproxy.exe`），需要同步更新。

> [!TIP]
> 可以使用 `gofmt -s` 和 `go vet ./...` 在每步迁移后快速验证代码质量。
