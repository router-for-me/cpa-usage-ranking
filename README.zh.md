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

<p align="center">
  面向 <a href="https://github.com/router-for-me/CLIProxyAPI">CLIProxyAPI（CPA）</a> 生态的开源 AI 用量排行榜服务端。<br />
  专为第三方采集器、网关与面板设计，通过简洁的 HTTP JSON 协议上报脱敏日用量并读取公开榜单。
</p>

<p align="center">
  <strong><a href="#客户端接入">客户端接入</a> · <a href="#排行榜">排行榜</a> · <a href="#文档导航">文档导航</a></strong>
</p>

## 客户端接入

接入方通过标准 **HTTPS + 未压缩 UTF-8 JSON** 与服务端通信，无需专用 SDK，也不依赖特定客户端实现：

- **只读展示端**：直接匿名调用 `GET /v1/leaderboards/{board}/{period}` 与 `/avatars/*`，无需注册或凭据。
- **用量上报端**：每个安装实例本地持久化一份 32 字节 `base64url` Bearer 凭据，经 GitHub OAuth 绑定 `(GitHub 用户 ID, CPA server_id)` 席位后，按 UTC 自然日上报完整累计快照。

| 步骤 | 接口 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| 开始注册 | `POST /v1/clients/register/start` | 本端 `Bearer` | 提交 `server_id`、`return_to` 与可选 `avatar_id`，获取 `authorize_url` |
| 完成注册 | `POST /v1/clients/register/complete` | 同一 `Bearer` | 绑定席位（`201` 新席位、`200` 复用/恢复、`202` 待授权） |
| 上报日快照 | `PUT /v1/clients/report/{utc_day}` | 已注册 `Bearer` | 原子替换席位在 UTC 今日/昨日的完整累计快照 |
| 退出席位 | `POST /v1/clients/withdraw` | 已注册 `Bearer` | 整席永久退出（`204`，空请求体） |
| 读取榜单 | `GET /v1/leaderboards/{board}/{period}` | 无需 | 获取最新发布的 Top 100 缓存榜单 |
| 获取头像 | `GET /avatars/catalog.json`、`/avatars/{path}` | 无需 | 读取头像清单与内置 WebP 图片（支持 `ETag`） |

**核心协议特性：**
- **凭据由客户端自持**：服务端仅存 32 字节随机密钥的 `SHA-256` 摘要；同一 `(GitHub ID, server_id)` 席位下的多个上报端各自持有独立凭据。
- **全量日快照幂等**：各上报端按日独立维护 `revision`；相同内容的重试会安全确认，不重复累加，也不会覆盖其他端的新快照。
- **隐私最小化**：仅接收按模型聚合的 Token 数、请求/成功次数与平均延迟，绝不收集上游 API Key、提示词或响应正文。

完整时序图、字段校验、可复制 cURL 流程、错误码与重试策略见**[客户端接入指南](docs/integration.zh.md)**；模型匹配与头像资源规范见**[模型、头像与品牌资源目录](docs/catalogs.zh.md)**。

## 排行榜

- **五大榜单**：`overall`（满分 100 综合分 + 6 项明细）、`usage`（精确 int64 总 Token）、`cache`（`cache_read / input`）、`success`（95% Wilson 置信下界）、`ttft`（平均首字延迟，越低越靠前）。
- **六个 UTC 周期**：Web 页面展示 `today`、`yesterday`、`week`（上一完整 UTC 自然周，周一起）与 `month`（上一完整 UTC 自然月）；API 额外提供 `year` 与 `all`。
- **精度与缓存**：所有 ID 与指标均采用精确十进制字符串传输以保留 64 位整数精度；公开 Top 100 快照由后台定时原子构建（默认 `5m`）。

完整评分公式、并列规则与前端格式化详见**[排行榜与评分规则](docs/leaderboards.zh.md)**。

## 文档导航

| 文档 | 内容范围 |
| --- | --- |
| [客户端接入指南](docs/integration.zh.md) | 接口总览、OAuth 与多端上报时序、日快照规范、cURL 示例、错误码与重试 |
| [排行榜与评分规则](docs/leaderboards.zh.md) | 五大榜单、六个 UTC 周期、评分公式、并列规则与前端格式化 |
| [模型、头像与品牌资源目录](docs/catalogs.zh.md) | 标准模型与别名匹配、内置 WebP 头像目录与品牌资源 |
| [部署、配置与运维指南](docs/deployment.zh.md) | Docker Compose、GHCR 策略、GitHub OAuth、反向代理、全量环境变量与备份 |
| [管理员控制台](docs/admin.zh.md) | 可选治理后台：主/普通管理员、用户检索、封禁与下榜规则 |
| [存储、结算与缓存架构](docs/storage.zh.md) | SQLite WAL 连接池、表结构、串行写队列、UTC 日结保留与原子缓存 |
| [性能基准与压测工具](docs/benchmark.zh.md) | 500 TPS 持续上报基准参考与 `cmd/ranking-capacity` 工具速查 |

> **需要自建排行榜服务端实例？** 多架构 Docker Compose（`linux/amd64`、`linux/arm64`）、GHCR 镜像标签、GitHub OAuth 配置与全量环境变量详见[部署、配置与运维指南](docs/deployment.zh.md)。

## 开源许可

本项目基于 [MIT License](./LICENSE) 开源。第三方声明与许可详见 [THIRD_PARTY_NOTICES.md](./THIRD_PARTY_NOTICES.md)。
