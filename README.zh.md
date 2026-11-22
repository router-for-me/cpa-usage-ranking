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

<p align="center">面向 CLIProxyAPI（CPA）生态的独立开源 AI 用量排行榜服务端与可视化面板。</p>

<p align="center">
  <strong><a href="#快速部署">快速部署</a>&emsp;|&emsp;<a href="#客户端接入">客户端接入</a>&emsp;|&emsp;<a href="#排行榜与管理后台">排行榜与管理后台</a>&emsp;|&emsp;<a href="#核心配置">核心配置</a>&emsp;|&emsp;<a href="#文档导航">文档导航</a></strong>
</p>

---

**CPA Usage Ranking** 是面向 **[CLIProxyAPI（CPA）](https://github.com/router-for-me/CLIProxyAPI)** 生态的自托管排行榜服务端。任意用量采集器、网关或面板均可通过简洁的 HTTP JSON 协议完成 GitHub OAuth 席位注册、UTC 日累计快照上报与公开榜单读取；服务端与具体客户端实现完全解耦，不收集上游 API Key、提示词或响应正文。

## 核心特性

- **开放的客户端接入协议**：客户端自行生成并保管 32 字节 `base64url` Bearer 凭据（服务端仅保存 `SHA-256` 摘要）；经 GitHub OAuth 将 `(GitHub 用户 ID, CPA UUID)` 绑定为同一席位，支持多个独立上报端。
- **隐私最小化与幂等上报**：仅接收 UTC 今日/昨日按模型聚合后的 Token、请求/成功次数与平均延迟；各端独立维护 `revision`，重试安全幂等。
- **五维榜单与完整 UTC 周期**：内置**综合总榜、用量榜、缓存复用榜、成功率榜（95% Wilson）、首字响应榜**，支持**今日、昨日、周榜（上一完整 UTC 周）、月榜（上一完整 UTC 月）**，全程保持 64 位整数精度。
- **单机 SQLite 引擎与内置前端**：基于 Go + SQLite（`WAL`、`FULL` 同步、单写队列、后台日结、原子缓存），同域托管极简响应式 Web 面板、66 款内置 WebP 头像与可选管理员控制台。

## 快速部署

推荐使用 Docker Compose（支持 `linux/amd64` 与 `linux/arm64`），镜像内置静态二进制、前端资源、头像与模型目录，无需宿主机安装 Go 或 Node.js。

```sh
# 本地构建并启动
docker compose -p cpa-ranking up -d --build
docker compose -p cpa-ranking ps
```

或拉取已发布的不可变 GHCR 镜像标签（仅发布 `vMAJOR.MINOR.PATCH` 与 `sha-<commit>`，无浮动 `latest` 标签）：

```sh
export RANKING_IMAGE=ghcr.io/<owner>/cpa-usage-ranking:v0.1.0
docker compose -p cpa-ranking pull ranking
docker compose -p cpa-ranking up -d --no-build
```

- **生产要点**：请前置 HTTPS 反向代理转发至 `127.0.0.1:8080`，配置 `RANKING_TRUSTED_PROXIES` 填入代理 IP/CIDR，配置 GitHub OAuth App（回调路径 `/auth/github/callback`）以开放新注册，并在本地磁盘单实例运行（勿使用 NFS）。详见[部署、配置与运维指南](docs/deployment.zh.md)。

## 客户端接入

接入方通过标准 HTTPS 与未压缩 UTF-8 JSON 通信。完整时序图、字段校验、错误码与 cURL 示例见**[客户端接入指南](docs/integration.zh.md)**。

| 步骤 | 接口 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| 开始注册 | `POST /v1/clients/register/start` | 本端 `Bearer <secret>` | 提交 CPA UUID 与 `return_to`，在浏览器打开返回的 `authorize_url` |
| 完成注册 | `POST /v1/clients/register/complete` | 同一 `Bearer <secret>` | 办结新席位（`201`）、复用/恢复（`200`）或轮询等待（`202`） |
| 上报日快照 | `PUT /v1/clients/report/{utc_day}` | 已注册 `Bearer` | 原子替换席位在 UTC 今日/昨日的完整累计快照 |
| 退出席位 | `POST /v1/clients/withdraw` | 已注册 `Bearer` | 整席永久退出（`204`，空请求体） |
| 读取榜单 | `GET /v1/leaderboards/{board}/{period}` | 无需 | 获取最新发布的 Top 100 缓存榜单 |

## 排行榜与管理后台

- **五大榜单**：`overall`（满分 100 综合分 + 6 项明细）、`usage`（精确 int64 总 Token）、`cache`（`cache_read / input`）、`success`（95% Wilson 置信下界）、`ttft`（平均首字延迟，越低越靠前）。
- **统计周期**：页面展示 `today`、`yesterday`、`week`（上一完整 UTC 自然周）与 `month`（上一完整 UTC 自然月）；API 额外支持 `year` 与 `all`。详见[榜单与评分规则](docs/leaderboards.zh.md)。
- **管理员控制台（可选）**：默认关闭。在 `RANKING_WEB_DIR` 外准备 `0600`/`0640` 权限的主管理员 JSON 并设置 `RANKING_ADMIN_CONFIG_FILE` 即可开启 `/admin/leaderboards`（下榜/恢复、手动刷新缓存）、`/admin/users`（搜索、封禁/解封）与 `/admin/accounts`（普通管理员管理）。详见[管理员控制台](docs/admin.zh.md)。

## 核心配置

下表列出常用环境变量（完整限流与写队列调优参数见[部署、配置与运维指南](docs/deployment.zh.md#环境变量参考)）：

| 环境变量 | 源码默认值（镜像默认值） | 说明 |
| --- | --- | --- |
| `RANKING_HTTP_ADDR` | `127.0.0.1:8080`（`0.0.0.0:8080`） | HTTP 监听地址 |
| `RANKING_SQLITE_PATH` | `data/ranking.db`（`/data/ranking.db`） | SQLite 数据库文件路径 |
| `RANKING_MODELS_PATH` | `config/models.json`（`/app/config/models.json`） | 标准模型与别名目录 |
| `RANKING_WEB_DIR` | 未设置（`/app/web`） | 前端构建目录（留空则纯 API 模式） |
| `RANKING_TRUSTED_PROXIES` | 未设置 | 可信反向代理 IP 或 CIDR |
| `RANKING_PUBLIC_ORIGIN` / `RANKING_GITHUB_CLIENT_ID` / `RANKING_GITHUB_CLIENT_SECRET` | 未设置 | GitHub OAuth 配置（三项同配开启注册） |
| `RANKING_ADMIN_CONFIG_FILE` | 未设置 | 主管理员私有 JSON 路径（启用后台） |

健康探针：`GET /healthz`（进程存活）与 `GET /readyz`（SQLite、目录及首轮榜单缓存就绪）。

## 文档导航

| 文档 | 内容范围 |
| --- | --- |
| [客户端接入指南](docs/integration.zh.md) | 接口总览、OAuth 与多端上报时序、日快照规范、cURL 示例、错误码与重试 |
| [榜单与评分规则](docs/leaderboards.zh.md) | 五大榜单、六个 UTC 周期、评分公式、并列规则与前端格式化 |
| [部署、配置与运维指南](docs/deployment.zh.md) | Docker Compose、GHCR 策略、GitHub OAuth、代理安全、全量环境变量与备份 |
| [管理员控制台](docs/admin.zh.md) | 主/普通管理员、用户检索、封禁与下榜状态矩阵、CSRF/HTTPS 安全 |
| [存储、结算与缓存架构](docs/storage.zh.md) | SQLite WAL 连接池、表结构、串行写队列、UTC 日结保留与原子缓存 |
| [模型、头像与品牌资源目录](docs/catalogs.zh.md) | `config/models.json` 解析、内置 WebP 头像目录与品牌图标生成 |
| [性能基准与压测工具](docs/benchmark.zh.md) | 500 TPS 持续上报基准参考与 `cmd/ranking-capacity` 工具速查 |

## 本地开发

<details>
<summary>环境要求、本地运行与质量检查命令</summary>

- **环境要求**：Go `1.26.10+`（`CGO_ENABLED=1` + GCC/Clang 编译 `go-sqlite3`）与 Node.js `24.15+`（`24.x`）。
- **前后端联调**（Vite 将 `/healthz`、`/readyz`、`/v1/leaderboards`、`/v1/admin`、`/avatars/*` 代理到 `http://127.0.0.1:8080`）：
  ```sh
  go run ./cmd/ranking
  npm --prefix web ci && npm --prefix web run dev
  ```
- **单二进制构建运行**：
  ```sh
  npm --prefix web run build
  go build -o bin/ranking ./cmd/ranking
  RANKING_WEB_DIR=web/dist ./bin/ranking
  ```
- **测试验证**：
  ```sh
  make verify       # gofmt、go vet、Go 单测、前端 typecheck/lint/Vitest 与双端构建
  make test-race    # Go 竞态检查（-race）
  make test-browser # 独立回环实例 + Playwright 浏览器检查
  ```

</details>

## License

本项目基于 [MIT License](./LICENSE) 开源。第三方声明与许可详见 [THIRD_PARTY_NOTICES.md](./THIRD_PARTY_NOTICES.md)。
