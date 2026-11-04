# SmartRoute: Agent Guidelines & Architecture Manual

## 1. Project Overview & Architecture

`SmartRoute` 是专为 `sing-box` 设计的高性能智能多维度选路出站扩展（Intelligent Multi-Attribute Routing Outbound Extension）。它结合网络质量（EWMA RTT、抖动 Jitter）、业务成本策略（Tier 等级、带宽上限、费用倍率）、主动/被动混合探测与熔断机制，实现毫秒级故障自动漂移、降级熔断与最佳链路优选。

### 核心代码结构

- **`smartroute.go`**:
  - `SmartRoute` 出站核心实现（实现 `adapter.Outbound`）。
  - 多属性路由决策算法（多维度成本打分模型、Tier 分层、Hold-Down 选路防抖锁定、快速故障转移）。
  - 支持配置热重载、运行时动态切换、SQLite 状态持久化机制。
- **`metrics.go`**:
  - `LinkMetrics` 节点指标评估引擎。
  - 基于 EWMA（指数加权移动平均）计算往返时延（RTT）及抖动（Jitter）。
  - 被动拨号探测与三态熔断器（Closed -> Open -> Half-Open）。
  - 瞬时吞吐量估算及拥塞感知。
- **`urltest.go`**:
  - 主动 1-RTT HTTP 快速探测引擎（默认目标：`http://1.1.1.1/cdn-cgi/trace`）。
  - 熔断前二次探测校验（避免因目标网站自身偶发故障导致节点误熔断）。
  - 针对嵌套 `select` / `urltest` 节点仅探测当前活跃选中的真实节点，避免级联风暴。
- **`options.go`**:
  - JSON 配置项定义与反序列化解析（`SmartRouteOptions`, `NodePolicy`, `ProbeConfig` 等）。
- **`context.go`**:
  - sing-box 上下文拦截器（`smartroute.Context(ctx)`），负责将 SmartRoute 出站注册进 sing-box 核心。
- **`clashapi.go`**:
  - 扩展 Clash API RESTful 路由：
    - `GET /smartroute`: 提供 Web 控制台 UI。
    - `GET /smartroute/status`: 导出节点状态与指标 JSON。
    - `POST /smartroute/config`: 运行时热更新路由策略。
    - `POST /smartroute/reset`: 重置选路状态与熔断器。
- **`smartroute_ui.html`**:
  - 内嵌纯静态无依赖单页 Web 控制台（原生 HTML/CSS/JS，完全脱机可用，无外部 CDN 依赖）。
- **`cmd/smartroute/`**:
  - 独立测试与开发调试用 CLI / 守护进程入口。
- **`cmd/sing-box/smartroute.go`**:
  - sing-box 主命令挂载 patch，通过 `cobra.PersistentPreRun` 注入上下文。

---

## 2. 权限与执行策略 (Permissions & Execution Policy)

- **工作区读写权限**:
  - 允许在本目录（`/home/chenmx/project/sing-box/smartroute`）及 sing-box 关联集成路径内执行完整的文件读取、创建、编辑与删除。
  - 创建或重构代码、更新测试用例和文档时遵循直接修改策略。
- **命令执行权限**:
  - 优先在标准沙箱环境下执行命令；若遇到 Git 索引锁或只读文件系统边界，根据需要启用 `BypassSandbox` 提权。
  - 执行非破坏性命令（如编译测试、静态检查、查看状态）无需向用户重复征询。

---

## 3. 开发规范与测试原则 (Development & Testing Guidelines)

### 3.1 单元测试第一 (Test-Driven Verification)
- 任何核心逻辑（路由算法、EWMA 计算、熔断状态机、选项解析、持久化）变更后，**必须运行单元测试验证**：
  ```bash
  go test -v .
  ```
- 新增功能特性时，必须在 `smartroute_test.go` 或 `metrics_test.go` 中补充覆盖测试用例。
- 涉及高并发与锁争用的逻辑，使用 `go test -race .` 检查是否存在潜在数据竞争。

### 3.2 兼容性与并发安全
- **sing-box 架构兼容**: `SmartRoute` 必须完全契约实现 `adapter.Outbound` 规范，不破坏上游接口契约。
- **并发与锁机制**: 指标统计与路由决策在各连接协程中高频调用，必须严格遵循 `sync.RWMutex` 锁分级原则，严禁长时间持有锁或在锁内执行阻塞 I/O 操作。
- **Web 控制台零外部依赖**: `smartroute_ui.html` 必须保持纯内嵌、零 CDN 依赖，保证在隔离局域网及无外网访问环境下依然能正常渲染与交互。

### 3.3 探测与熔断机制准则
- 主动探测必须控制频率，严格防范对下游代理或目标探测端造成流量雪崩。
- 被动拨号失败时，必须先结合主动探测进行二次确认（Active Probe Pre-verification），严防因目标服务异常误判为节点故障。

---

## 4. Git 与文档协同规范 (Git & Documentation Standards)

- **提交信息规范 (Conventional Commits)**:
  - `feat(smartroute): ...` - 新特性或功能增强
  - `fix(smartroute|urltest|metrics): ...` - Bug 修复
  - `docs(readme): ...` - 文档更新
  - `test(smartroute): ...` - 单元测试新增或调整
  - `refactor(smartroute): ...` - 代码重构
- **双语文档同步维护**:
  - 当 SmartRoute 配置选项（如 `tolerance`, `hold_down`, `cache_file`, `probe_interval` 等）或 API 接口变动时，必须同步更新：
    - `README.md` (中文文档)
    - `README.en.md` (英文文档)
- **提交前确认**:
  - 提交并推送前确保工作区干净，无未追踪的临时二进制或测试缓存文件。
