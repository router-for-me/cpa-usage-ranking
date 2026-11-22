<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="./assets/ranking-logo-dark.svg" />
    <source media="(prefers-color-scheme: light)" srcset="./assets/ranking-logo-light.svg" />
    <img src="./assets/ranking-logo-light.svg" alt="CPA Usage Ranking" width="520" />
  </picture>
</p>

<p align="center">
  <a href="./README.md">English</a> ｜ <a href="./README.zh.md"><strong>简体中文</strong></a>
</p>

<h1 align="center">CPA Usage Ranking</h1>

<p align="center">面向 CLIProxyAPI（CPA）生态的开源 AI 用量排行榜服务端与可视化面板。</p>

<p align="center">
  <strong><a href="#客户端接入">客户端接入</a>&emsp;|&emsp;<a href="#快速部署">快速部署</a>&emsp;|&emsp;<a href="#排行榜与评分">排行榜与评分</a>&emsp;|&emsp;<a href="#管理员控制台">管理员控制台</a>&emsp;|&emsp;<a href="#配置">配置参考</a></strong>
</p>

---

**CPA Usage Ranking** 是一个独立部署的开源排行榜服务端，专为 **[CLIProxyAPI（CPA）](https://github.com/router-for-me/CLIProxyAPI)** 下游生态设计——无论是 **cpa-usage-keeper（Keeper）**、**cpamp**、**oh-my-cpa**，还是其他 CPA 面板、统计采集器与自建工具，都可以通过一套简洁的公开 HTTP 接口完成身份注册、UTC 日快照上报与排行榜读取。

服务端与具体客户端实现完全解耦：不依赖特定 SDK 或上游数据库表结构，不收集用户的上游模型 API Key、提示词（Prompt）或响应正文，仅接收按模型聚合后的脱敏计数与耗时统计，并在单机 SQLite 上提供多维度评分、周期结算、原子缓存榜单与可选的管理后台。

## 核心特性

- **开放的 CPA 客户端接入协议**：客户端自行生成并保管 32 字节随机凭据（服务端仅存 `SHA-256` 摘要），不预发账号。经 GitHub 授权后，同一 GitHub 账号与 CPA UUID 的多个上报端共用排名席位。
- **隐私最小化与协议幂等**：仅上报模型级 Token、请求数、成功率与延迟统计；各端独立维护 `revision`；同席每天只保留一份完整快照，网络重发不会覆盖其他端的新快照。
- **五维榜单与完整自然周期**：内置**综合总榜、用量榜、缓存复用榜、成功率榜、首字响应榜**，支持**今日、昨日、周榜（上一完整 UTC 自然周）、月榜（上一完整 UTC 自然月）**切换，64 位大整数全程十进制字符串传输与无损格式化。
- **轻量单机高吞吐架构**：基于 Go + SQLite（`WAL`、`FULL` 同步、单写串行队列），后台自动完成封窗日结与历史裁剪，内存原子缓存秒级响应公开读请求；历史构建在 2 vCPU / 1 GiB 下实测 [500 TPS 日快照上报](docs/benchmark.md)，并非当前协议的容量声明。
- **内嵌响应式前端与可选管理台**：同域托管极简 Web 榜单（桌面、平板、手机自适应，内置 66 款 WebP 头像）；按需挂载配置文件即可开启管理员控制台，支持用户检索、封禁与独立下榜管理。

## 客户端接入

适用于 `keeper`、`cpamp`、`oh-my-cpa` 等任意需要上报用量或展示排行榜的下游项目。完整字段约束、重试策略与状态码请查阅 **[通用客户端接入指南](docs/integration.md)** 与 **[接入时序流程图](docs/integration-flow.md)**。

### 接入流程概览

| 步骤 | 接口 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| 1. 生成凭据 | 客户端本地生成 | — | 生成 32 字节密码学安全随机数，编码为 43 字符无填充 `base64url` 并本地持久化 |
| 2. 开始注册 | `POST /v1/clients/register/start` | `Bearer <secret>` | 提交持久 CPA UUID 与 `return_to`；在浏览器打开返回的授权链接 |
| 3. 完成注册 | `POST /v1/clients/register/complete` | 原 `Bearer` | 提交 `request_id`；`201` 新席位，`200` 复用或恢复已办结结果，`202` 待授权 |
| 4. 上报日快照 | `PUT /v1/clients/report/{utc_day}` | `Bearer <secret>` | UTC **今日/昨日**；各端独立维护修订号，最新提交的完整快照覆盖整席当日数据 |
| 5. 退出席位（可选） | `POST /v1/clients/withdraw` | `Bearer <secret>` | 无正文；`204` 表示整席退出已落库，所有端停止上报 |
| 读取排行榜 | `GET /v1/leaderboards/{board}/{period}` | 无需凭据 | 读取最新发布的公开缓存榜单（最多 100 名） |

> **上报聚合约定**：客户端在提交前须将同一 UTC 日、同一标准模型的统计合并为单条记录（不含 `service_tier` 字段）；`ttft_avg_ms` 与 `latency_avg_ms` 须按各自真实样本数加权计算均值，无样本时传 `null` 且样本数传 `"0"`。未知模型不会报错，但仅纳入目录的[标准模型及别名](docs/model-catalog.md)计入榜单统计。

### 最小接入示例（cURL）

使用已[配置 GitHub OAuth 的 HTTPS 服务](docs/docker.zh.md#github-oauth-app)。下方 UUID 仅为示例，正式接入读取 CPA 提供的持久 UUID；返回页按需替换。

```sh
# 1. 生成并妥善保存 43 字符无填充 base64url 凭据（32 字节随机数）
SECRET=$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n')

# 2. 领号，保存返回的 request_id，并在浏览器打开 authorize_url
BASE=https://ranking.example.com
curl -sS -X POST "$BASE/v1/clients/register/start" \
  -H "Authorization: Bearer $SECRET" -H 'Content-Type: application/json' \
  -d '{"cpa_uuid":"550e8400-e29b-41d4-a716-446655440000","return_to":"http://localhost:3001/settings?tab=ranking#status"}'

# 3. GitHub 授权后，用原编号和同一凭据办结；收到 200 或 201 后再上报
REQUEST_ID='替换为返回的 request_id'
curl -sS -X POST "$BASE/v1/clients/register/complete" \
  -H "Authorization: Bearer $SECRET" -H 'Content-Type: application/json' \
  -d "{\"request_id\":\"$REQUEST_ID\"}"

# 4. 上报当前 UTC 日的完整累计快照
TODAY=$(date -u +%F)
curl -sS -X PUT "$BASE/v1/clients/report/$TODAY" \
  -H "Authorization: Bearer $SECRET" \
  -H "Content-Type: application/json" \
  -d '{
    "schema_version": 1,
    "revision": "1",
    "items": [{
      "model": "claude-opus-4-5",
      "total_tokens": "150000",
      "input_tokens": "100000",
      "output_tokens": "50000",
      "cache_read_tokens": "60000",
      "cache_write_tokens": "0",
      "reasoning_tokens": "10000",
      "request_count": "42",
      "success_count": "41",
      "ttft_avg_ms": 320.5,
      "ttft_samples": "41",
      "latency_avg_ms": 1850.0,
      "latency_samples": "41"
    }]
  }'

# 读取公开综合总榜无需注册
curl -sS "$BASE/v1/leaderboards/overall/today"
```

## 排行榜与评分

公开接口 `GET /v1/leaderboards/{board}/{period}` 直接从内存原子缓存返回结构化榜单，计算公式与并列规则详见 **[榜单接口与评分说明](docs/leaderboards.md)**。

### 五大榜单（`board`）

| 标识 | 名称 | 排序指标与说明 |
| --- | --- | --- |
| `overall` | 综合总榜 | 满分 100 分：用量分位（40）+ Wilson 成功率下界（30）+ 缓存复用率（20）+ 首字响应表现（10），附带 6 项核心指标明细 |
| `usage` | 用量榜 | 周期内已匹配模型的总 Token 数（64 位精确整数排序） |
| `cache` | 缓存复用榜 | `cache_read_tokens / input_tokens` 百分比（需 `input_tokens > 0`） |
| `success` | 成功率榜 | 请求成功率的 95% Wilson 置信下界得分（兼顾高成功率与样本规模） |
| `ttft` | 首字响应榜 | 平均首字延迟 `ttft_mean_ms` 升序排列（越低越靠前，需有首字延迟样本） |

### 统计周期（`period`）

| 标识 | 页面名称 | UTC 时间范围（左闭右开 `[start, end)`） |
| --- | --- | --- |
| `today` | 今日 | 当前 UTC 自然日 `[今日 00:00, 明日 00:00)` |
| `yesterday` | 昨日 | 前一 UTC 自然日 `[昨日 00:00, 今日 00:00)` |
| `week` | 周榜 | **上一完整 UTC 自然周** `[上周一 00:00, 本周一 00:00)` |
| `month` | 月榜 | **上一完整 UTC 自然月** `[上月 1 日 00:00, 本月 1 日 00:00)` |

> 后端缓存同时构建并维护 `year`（当前 UTC 自然年）与 `all`（累计）周期，可供 API 按需读取，前端页面默认聚焦上述四个常用周期。

## 快速部署

推荐使用 Docker Compose 部署（支持 `linux/amd64` 与 `linux/arm64`），镜像已内置静态编译的 Go/SQLite 服务、Web 前端、头像资源与模型目录，无需在宿主机安装 Go 或 Node.js。完整说明见 **[Docker 部署指南](docs/docker.zh.md)**。

### 1. 本地构建并启动

```sh
docker compose -p cpa-ranking up -d --build
docker compose -p cpa-ranking ps
```

### 2. 使用已发布的 GHCR 镜像

镜像发布采用严格的 `vMAJOR.MINOR.PATCH` 与 `sha-<commit>` 不可变标签（不提供浮动 `latest` 标签）：

```sh
export RANKING_IMAGE=ghcr.io/<owner>/cpa-usage-ranking:v0.1.0
docker compose -p cpa-ranking pull ranking
docker compose -p cpa-ranking up -d --no-build
```

### 生产环境安全提示

- **开启 GitHub 注册**：创建 GitHub OAuth App，将回调设为公开 HTTPS 入口下的 `/auth/github/callback`，在运行时传入 OAuth 配置。未配置时仍可读榜、接收已有上报端的写入。部署步骤见 [GitHub OAuth App](docs/docker.zh.md#github-oauth-app)。

- **必须启用 HTTPS**：客户端注册与上报请求携带 Bearer 凭据。示例 `compose.yaml` 默认仅绑定宿主机回环 `127.0.0.1:8080:8080`，对公网提供服务时请前置 Nginx / Caddy / Traefik 等反向代理终止 TLS，切勿在公网明文暴露 HTTP，且不要记录凭据、Cookie 或 OAuth 完整查询串。
- **可信代理配置**：服务端默认不信任 `X-Forwarded-For` 等转发头；前置反向代理后，请将代理 IP/网段填入 `RANKING_TRUSTED_PROXIES`，以确保按真实客户端来源执行注册与登录限流（详见[注册来源说明](docs/registration-sources.md)）。
- **单实例与持久化卷**：容器以非特权 `10001:10001` 运行，SQLite 数据持久化于 `/data`（命名卷 `cpa-ranking_ranking-data`）。请保持单实例本地磁盘运行，勿在 NFS 或多副本间共享同一 SQLite 文件。

## 管理员控制台

管理后台默认关闭，公开排行榜与客户端上报无需开启后台即可独立运行。

如需管理参与者与管理员账号，在静态目录之外准备权限为 `0600` 或 `0640` 的主管理员配置文件（容器内由 `10001` 可读），并通过 `RANKING_ADMIN_CONFIG_FILE` 指定路径后重启服务：

```json
{
  "username": "admin",
  "password": "replace-with-strong-password-12-to-72-bytes"
}
```

启用后，页面右上角将出现**登录 / 控制台**入口，提供：
- **榜单管理**：预览四周期五维榜单、手动触发缓存刷新、对指定身份执行**下榜 / 恢复上榜**（下榜后仍正常接收该客户端上报，但从全部榜单与评分参考样本中排除）。
- **用户管理**：按昵称子串或精确 ID 搜索已注册客户端，执行**封禁 / 解封**（封禁后拒绝后续上报并退出全部榜单；封禁与下榜状态彼此独立）。
- **管理员管理**（仅主管理员可见）：新增或删除数据库普通管理员（删除后其活跃会话立即失效）。

详细部署与安全边界请参阅 **[管理员控制台文档](docs/admin.md)**。

## 配置

以下列出核心运行与管理配置（源码运行默认值与[容器镜像默认值](docs/docker.zh.md#配置和健康检查)已在表中标明；显式空值或非法格式将在启动时直接报错退出）：

| 环境变量 | 源码默认值（镜像默认值） | 说明 |
| --- | --- | --- |
| `RANKING_HTTP_ADDR` | `127.0.0.1:8080`（镜像 `0.0.0.0:8080`） | HTTP 监听地址 |
| `RANKING_SQLITE_PATH` | `data/ranking.db`（镜像 `/data/ranking.db`） | SQLite 数据库文件路径（启动自动建表，同结构重启保留数据） |
| `RANKING_MODELS_PATH` | `config/models.json`（镜像 `/app/config/models.json`） | 启动加载的模型目录与别名映射文件 |
| `RANKING_WEB_DIR` | 未设置（镜像 `/app/web`） | 前端构建产物目录；未设置时仅暴露后端 API |
| `RANKING_CACHE_REFRESH_INTERVAL` | `5m` | 排行榜统一缓存定时刷新周期 |
| `RANKING_SHUTDOWN_TIMEOUT` | `10s` | 优雅停机各阶段排空超时 |
| `RANKING_TRUSTED_PROXIES` | 未设置（不信任任何转发头） | 逗号分隔的可信反向代理 IP 或 CIDR |
| `RANKING_PUBLIC_ORIGIN` | 未设置 | 公开 HTTPS 入口；注册须与以下两个变量同时配置 |
| `RANKING_GITHUB_CLIENT_ID` | 未设置 | GitHub OAuth App Client ID |
| `RANKING_GITHUB_CLIENT_SECRET` | 未设置 | GitHub OAuth App Client Secret，仅运行时注入 |
| `RANKING_REGISTRATION_ENABLED` | `true` | 配置 OAuth 后允许领号及绑定新上报端；关闭不影响已有凭据上报 |
| `RANKING_REGISTRATION_IPV6_64` | `true` | 原生 IPv6 来源按 `/64` 前缀归并计算注册/登录限流 |
| `RANKING_SETTLED_DAY_RETENTION_DAYS` | `31` | 已日结的单日汇总保留天数（范围 `1`–`36500`） |
| `RANKING_ADMIN_CONFIG_FILE` | 未设置（关闭管理后台） | 主管理员 JSON 配置文件路径（须位于 `RANKING_WEB_DIR` 之外） |

领号请求窗口和容量与上报并发/速率桶等细粒度调优参数，详见 **[注册 API 配置](docs/registration-api.md#startup-configuration)**、**[注册准入限流](docs/registration-admission.md)** 与 **[日快照上报规范](docs/report-snapshots.md)**。

### 健康与就绪探针

- `GET /healthz`：返回 `200 {"status":"ok"}`，仅表示 HTTP 进程存活。
- `GET /readyz`：仅当 SQLite、模型与头像目录初始化完成，且**首轮真实榜单缓存构建成功**后返回 `200 {"status":"ready"}`（容器健康检查默认使用此端点）。

## 性能基准

历史基准中，在 2 vCPU / 1 GiB 内存（无 Swap）的 cgroup 环境下，原服务面对 20,000+ 旧版注册客户端与起始积压负载，完成了连续 20 分钟、约 **500 TPS** 的日快照持续写入与积压恢复验证。完整硬件环境、负载模型、延迟分布与适用边界请参阅 **[500 TPS 持续上报基准测试报告](docs/benchmark.md)** 及 **[容量测试工具说明](docs/capacity-tool.md)**。

## 文档导航

- **客户端接入**：[通用客户端接入指南](docs/integration.md) · [接入时序与流程图](docs/integration-flow.md) · [凭据格式与哈希](docs/client-credentials.md) · [注册 API](docs/registration-api.md) · [昵称规范](docs/display-names.md) · [日快照上报 API](docs/report-snapshots.md)
- **榜单与数据**：[榜单 API 与评分规则](docs/leaderboards.md) · [模型目录与别名](docs/model-catalog.md) · [内置头像目录](docs/avatar-catalog.md) · [SQLite 存储与结算](docs/storage.md) · [客户端存储表](docs/client-storage.md)
- **运维与安全**：[Docker 部署指南](docs/docker.zh.md) · [管理员控制台](docs/admin.md) · [注册准入限流](docs/registration-admission.md) · [可信代理与来源识别](docs/registration-sources.md) · [基准测试](docs/benchmark.md)

## 本地开发与验证

<details>
<summary>展开查看目录结构、本地源码运行与测试命令</summary>

### 目录结构

```text
cmd/ranking/           生产服务端入口、装配与优雅关停
cmd/ranking-capacity/  离线合成数据与有界容量压测工具（不打入生产镜像）
config/models.json     标准模型名称、别名与参考价格目录
internal/
  admin/               主管理员配置、数据库普通管理员、会话与封禁/下榜服务
  auth/                Bearer 凭据严格解析与 SHA-256 哈希鉴权
  avatars/             内置 66 张 WebP 头像目录与内存分发
  cache/               多周期原子快照缓存与合并刷新调度
  clients/             客户端身份存储与字面量分页检索
  httpserver/          Gin 路由、安全响应头、os.OpenRoot 静态托管与同源/CSRF 保护
  leaderboard/         五维榜单评分、Wilson 下界、中位数参考与 Top 100 截断
  models/              模型目录校验与标准名称/别名解析
  periods/             UTC 自然日/周/月/年周期边界计算
  registration/        GitHub OAuth 注册事务、来源及全局领号限流
  reports/             严格 JSON 解码、快照摘要、单写串行队列、后台日结与自然保留
  statistics/          只读事务合并已结周期表与未结日表候选快照
  storage/             SQLite 连接池与当前表结构初始化
web/                   React 19 + TypeScript + Tailwind CSS 前端与测试套件
deploy/                CI 门禁、GHCR 发布标签策略与 Docker 冒烟验证脚本
docs/                  对外协议、部署与运维文档
```

### 环境要求

- **Go 1.26.10+**（启用 `CGO_ENABLED=1`）及 C 编译器（如 GCC / Clang，用于编译 `go-sqlite3`）
- **Node.js 24.15+**（24.x）与 `npm`

### 本地前后端联调

在两个终端分别启动后端与 Vite 开发服务器（Vite 默认将 `/healthz`、`/readyz`、`/v1/leaderboards`、`/v1/admin` 与 `/avatars/*` 代理至 `http://127.0.0.1:8080`）：

```sh
# 终端 1：启动后端 API
go run ./cmd/ranking

# 终端 2：安装前端依赖并启动开发服务器
npm --prefix web ci
npm --prefix web run dev
```

单进程托管构建后的前端与 API：

```sh
npm --prefix web run build
go build -o bin/ranking ./cmd/ranking
RANKING_WEB_DIR=web/dist ./bin/ranking
```

### 质量检查与测试

```sh
make verify       # gofmt、go vet、Go 单元测试、前端 typecheck/lint/Vitest 及双端构建
make test-race    # Go 全包并发竞态检查（-race）
make test-browser # 启动独立回环实例运行 Playwright 端到端浏览器检查
```

复用本机已有 Chromium 可指定 `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/path/to/chromium make test-browser`。品牌图标说明见 [docs/brand-icons.md](docs/brand-icons.md)。

</details>

## License

本项目基于 [MIT License](./LICENSE) 开源。第三方源码声明与许可详见 [THIRD_PARTY_NOTICES.md](./THIRD_PARTY_NOTICES.md)。
