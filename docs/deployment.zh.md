# 部署、配置与运维指南

[English](deployment.md) · [返回 README](../README.zh.md)

容器镜像支持 Linux `amd64` 与 `arm64`，内置静态编译的 Go/SQLite 服务、前端构建产物（含第三方声明）、嵌入式头像与模型目录，运行时无需在宿主机安装 Go、Node.js 或 C 编译器。本项目不分发独立二进制压缩包。

> CI 会对双架构执行构建与冒烟测试；其中 ARM64 冒烟检查在 AMD64 runner 上经由 QEMU 用户态模拟完成，并非 ARM 原生硬件。

## Docker Compose 快速部署

### 本地构建并启动

在仓库根目录执行（需已安装 Docker Engine、Buildx 与 Compose）：

```sh
docker compose -p cpa-ranking up -d --build
docker compose -p cpa-ranking ps
docker compose -p cpa-ranking logs ranking
```

默认 `compose.yaml` 构建 `cpa-usage-ranking:local`，将数据持久化在命名卷 `cpa-ranking_ranking-data`（挂载到 `/data`），并仅在宿主机 `127.0.0.1:8080` 监听。仅构建指定平台镜像：

```sh
docker buildx build --platform linux/amd64 --load -t cpa-usage-ranking:local .
# 构建 ARM64 时替换为 linux/arm64。
```

### 使用已发布的 GHCR 镜像

通过 `RANKING_IMAGE` 指定精确发布标签或不可变 manifest digest（替换为真实账户、小写仓库名与版本号）：

```sh
export RANKING_IMAGE=ghcr.io/<owner>/<repo>:v1.0.0
docker compose -p cpa-ranking pull ranking
docker compose -p cpa-ranking up -d --no-build
```

请保留 `--no-build`，避免 Compose 回退为本地构建。仓库标签本身仍可能被重新推送；追求严格不可变与可复现部署时请锁定 digest（`ghcr.io/<owner>/<repo>@sha256:<digest>`）。

### GHCR 镜像发布策略

- **稳定版标签**：推送严格匹配 `vMAJOR.MINOR.PATCH`（除 `0` 外无前导零、无预发布后缀）且提交属于默认分支祖先的 Git 标签，发布 `:vMAJOR.MINOR.PATCH` 与 `:sha-<40位提交SHA>`。
- **开发版标签**：在非默认分支手动触发 `workflow_dispatch`，发布 `:dev-sha-<40位提交SHA>`；若提供合法小写 `dev_tag`，额外发布 `:dev-<name>`。
- **不发布浮动别名**：不提供 `:latest`、`:v1`、`:v1.x`，确保使用方显式选择精确版本或 digest。
- **CI 门禁**：每次发布均须先通过策略校验与完整复用 CI（Go、前端、策略测试及双架构镜像冒烟检查），再由发布任务使用 `GITHUB_TOKEN` 推送至 GHCR。

## GitHub OAuth App

1. 创建 **GitHub OAuth App**，将 Authorization callback URL 设为公开 HTTPS 入口加 `/auth/github/callback`（例如 `https://ranking.example.com/auth/github/callback`）；注意这**不是**客户端的 `return_to`。无需勾选仓库或邮箱权限。
2. 在运行时同时传入 `RANKING_PUBLIC_ORIGIN`、`RANKING_GITHUB_CLIENT_ID` 与 `RANKING_GITHUB_CLIENT_SECRET`（切勿写入仓库或镜像）。`compose.yaml` 会自动透传这三个变量以及可选的 `RANKING_REGISTRATION_ENABLED` 与 `RANKING_TRUSTED_PROXIES`。
3. 将公网 HTTPS 下的 `/auth/github/*` 与 `/v1/*` 路由到同一 Ranking 实例，并确保容器可经 HTTPS 访问 `github.com` 与 `api.github.com`（服务端不会主动请求客户端 `return_to`）。仅用于本地联调时，`RANKING_PUBLIC_ORIGIN` 也允许回环 HTTP 入口（`http://localhost[:port]`、`http://127.0.0.1[:port]`、`http://[::1][:port]`）。

三个 OAuth 变量均未设置或为空白时，注册接口返回 `503 registration_closed`；若已配置 OAuth 且 `RANKING_REGISTRATION_ENABLED=false`，仅拒绝陌生未绑定凭据的新注册，已绑定老凭据仍可完成恢复或退榜重回，公开读榜、已有上报端写入与主动退出均正常工作；仅配置部分 OAuth 变量会在启动时报错退出。

## 反向代理、安全与来源识别

- **必须前置 HTTPS**：注册、上报和管理登录均携带敏感凭据，切勿将 HTTP 监听直接暴露公网。容器内监听 `0.0.0.0:8080`，即使宿主机仅映射 `127.0.0.1:8080`，也应隔离同一 Docker 网络中的非受信任容器。
- **脱敏代理日志**：禁止记录 `Authorization` 请求头、Cookie、请求体以及 `/auth/github/*` 的完整查询串（OAuth 路由仅记录路径）。
- **可信代理（`RANKING_TRUSTED_PROXIES`）**：默认未设置，服务端忽略一切转发头并仅使用连接 `RemoteAddr`。部署在反代之后时，仅填入实际代理 IP 或 CIDR（如 `127.0.0.1,10.2.0.0/24`，容忍空白分段与末尾逗号）；只有来自可信代理的连接才会解析 `X-Forwarded-For`（从右向左取首个非受信任跳）、`X-Real-IP`、`X-Forwarded-Proto`（取逗号首段且不区分大小写）以及合法单值 `X-Forwarded-Host`，格式异常时安全回退。若启用了管理员后台（且未开本地 HTTP 开关）或启用了 HTTPS GitHub OAuth 但 `RANKING_TRUSTED_PROXIES` 为空，启动会打印明确告警日志。切勿信任 `0.0.0.0/0` 或客户端可达网段。
- **IPv6 来源归并（`RANKING_REGISTRATION_IPV6_64`）**：默认开启（`true`），注册与管理员登录限流将原生 IPv6 地址按 `/64` 前缀归并（IPv4 映射的 IPv6 还原为 IPv4）。来源键仅用于防滥用限流，不代表身份或席位归属。
- **两层注册准入限流**：`POST /v1/clients/register/start` 在校验 Bearer 与正文**之前**先经过进程内每来源/全局固定窗口限流（重启清零）；被单来源限额或内存来源表满拒绝（`429 registration_capacity`）时不会消耗全局额度。通过初筛的合法请求再写入固定 `10m` 有效、最多 `2,000` 笔未过期记录的 SQLite 注册事务表（固定常量，不可通过环境变量修改）；表满时会优先清理已终结的 `denied`/`failed` 记录，再按需淘汰最早到期的 `completed` 记录，仅当活跃进行中的注册请求达到上限时返回 `429 registration_capacity`（此时已消耗来源与全局额度）。

## 环境变量参考

容器部署须通过 Compose `environment` 或 `docker run -e` 传入配置（仅在宿主机 shell `export` 不会自动进入容器，除非 `compose.yaml` 显式透传）。未设置或空白值会自动回退为默认值；非法格式会在启动时直接报错退出。

### 核心运行与健康检查

| 环境变量 | 源码 / 镜像默认值 | 说明 |
| --- | --- | --- |
| `RANKING_HTTP_ADDR` | 源码：`127.0.0.1:8080`<br>镜像：`0.0.0.0:8080` | HTTP 监听地址（`host:port`） |
| `RANKING_SQLITE_PATH` | 源码：`data/ranking.db`<br>镜像：`/data/ranking.db` | 本地持久存储上的 SQLite 文件路径 |
| `RANKING_MODELS_PATH` | 源码：`config/models.json`<br>镜像：`/app/config/models.json` | 启动加载的标准模型与别名目录 |
| `RANKING_WEB_DIR` | 源码：未设置<br>镜像：`/app/web` | 前端构建产物目录；源码运行不设置时为纯 API 模式 |
| `RANKING_CACHE_REFRESH_INTERVAL` | `5m` | 排行榜缓存定时刷新周期 |
| `RANKING_SHUTDOWN_TIMEOUT` | `10s` | 优雅停机各阶段排空超时 |
| `RANKING_SETTLED_DAY_RETENTION_DAYS` | `31` | 已日结单日总计保留天数（范围 `1`–`36500`） |
| `RANKING_HEALTHCHECK_URL` | 镜像：`http://127.0.0.1:8080/healthz` | 仅用于容器 `HEALTHCHECK`（Go 进程不读取） |

- **探针约定**：`GET` / `HEAD /healthz` 存活时返回 `200 {"status":"ok"}`（镜像默认 `HEALTHCHECK` 探测此接口，避免高写入负载下触发容器误重启；改端口时须同步更新 `RANKING_HEALTHCHECK_URL`）。`GET` / `HEAD /readyz` 在目录、首轮榜单缓存及带 `1s` 短时复用/并发合并的只读 SQLite 探针就绪后返回 `200 {"status":"ready"}`（不再争抢唯一写连接），否则返回 `503 {"status":"not_ready","reason":"initializing_or_unavailable"}`。

### 反向代理与 GitHub OAuth 注册

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RANKING_TRUSTED_PROXIES` | 未设置（不信任任何代理） | 逗号分隔的可信反向代理 IP 或 CIDR（容忍空白分段与末尾逗号） |
| `RANKING_REGISTRATION_IPV6_64` | `true` | 原生 IPv6 限流键按 `/64` 前缀归并 |
| `RANKING_PUBLIC_ORIGIN` | 未设置 | 公开 HTTPS 入口（`https://host[:port]`，无路径/查询/锚点；本地联调也兼容回环 `http://localhost`、`127.0.0.1`、`[::1]`） |
| `RANKING_GITHUB_CLIENT_ID` | 未设置 | GitHub OAuth App Client ID |
| `RANKING_GITHUB_CLIENT_SECRET` | 未设置 | GitHub OAuth App Client Secret（仅运行时注入） |
| `RANKING_REGISTRATION_ENABLED` | `true` | 配置 OAuth 后是否允许新/未绑定凭据注册（设为 `false` 时仍放行已绑定老凭据恢复或退榜重回） |
| `RANKING_REGISTRATION_MAX_BODY_BYTES` | `4096` | 注册 `start` / `complete` 请求体最大字节数 |
| `RANKING_REGISTRATION_SOURCE_REQUESTS` | `20` | 每来源每窗口 `/register/start` 次数上限 |
| `RANKING_REGISTRATION_GLOBAL_REQUESTS` | `200` | 全局每窗口 `/register/start` 次数上限 |
| `RANKING_REGISTRATION_REQUEST_WINDOW` | `1m` | 开始注册固定限流窗口时长 |
| `RANKING_REGISTRATION_REQUEST_MAX_SOURCES` | `10000` | 内存中最多跟踪的注册来源数 |

### 日快照上报限流与写队列

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RANKING_REPORT_MAX_BODY_BYTES` | `524288` | 单次上报 JSON 正文最大字节数（`512 KiB`；gzip 压缩流与解压后体积均受此限制） |
| `RANKING_REPORT_MAX_ITEMS` | `1024` | 单次上报原始模型项上限（匹配目录前） |
| `RANKING_REPORT_DECODE_CONCURRENCY` | `16` | 上报正文并发读取与解码上限 |
| `RANKING_REPORT_QUEUE_CAPACITY` | `128` | 串行上报写队列待处理容量 |
| `RANKING_REPORT_TRANSACTION_TIMEOUT` | `5s` | 单笔上报或后台维护 SQLite 事务超时 |
| `RANKING_REPORT_CLIENT_INTERVAL` | `10s` | 单席位令牌补充周期（同席所有上报端共用；单席限流或容量拒绝不消耗全局令牌） |
| `RANKING_REPORT_CLIENT_BURST` | `10` | 单席位令牌桶突发容量 |
| `RANKING_REPORT_GLOBAL_INTERVAL` | `5ms` | 全局上报令牌补充周期 |
| `RANKING_REPORT_GLOBAL_BURST` | `400` | 全局上报令牌桶突发容量 |
| `RANKING_REPORT_MAX_CLIENTS` | `100000` | 内存中最多跟踪的席位限流状态数 |
| `RANKING_REPORT_STATE_TTL` | `10m` | 已补满席位限流状态的空闲过期时间 |

### 管理员控制台

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RANKING_ADMIN_CONFIG_FILE` | 未设置（关闭后台） | 位于 `RANKING_WEB_DIR` 之外的主管理员私有 JSON（建议权限 `0600`/`0640`，权限过宽时仅在启动日志告警），详见[管理员控制台](admin.zh.md) |
| `RANKING_ADMIN_ALLOW_INSECURE_LOCAL_HTTP` | `false` | 本地或容器 HTTP 测试开关，允许非 HTTPS 管理员 Cookie 与会话（切勿对公网开启） |

## 数据权限、停机与离线备份

- **非 root 运行（`10001:10001`）**：新 Docker 命名卷会自动初始化 `/data` 为 `10001:10001`、`0700`。使用宿主机绑定目录时须提前设置权限（如 `install -d -m 0700 -o 10001 -g 10001 /srv/cpa-ranking`），容器启动不会递归修改所有权。
- **单实例本地磁盘**：切勿在多副本或 NFS/网络共享文件系统上共用 SQLite 数据库；运行中不要单独删除或复制 `-wal` / `-shm` 伴随文件。
- **优雅停机**：Compose 默认 `stop_grace_period: 30s`，服务端 `RANKING_SHUTDOWN_TIMEOUT` 每阶段默认 `10s`；调大停机超时请同步留足 Docker 停止宽限。除非确认销毁数据，否则避免执行 `docker compose down -v`。
- **资源配额**：建议按宿主机资源在 Compose 中显式配置 `cpus`、`mem_limit` 与 `memswap_limit`。

### 离线备份

先正常停止容器并确认其进入 `exited` 状态且退出码为 `0`，动态解析并校验实际挂载到 `/data` 的命名卷（避免恢复切换卷后误备旧卷），先写入临时文件成功后再原子改名：

```sh
docker compose -p cpa-ranking stop -t 30 ranking || exit 1
CID=$(docker compose -p cpa-ranking ps -aq ranking)
STATE=$(docker inspect --format '{{.State.Status}}:{{.State.ExitCode}}' "$CID")
[ "$STATE" = "exited:0" ] || { echo "unexpected container state: $STATE"; exit 1; }
DATA_VOLUME=$(docker inspect \
  --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' \
  "$CID")
[ -n "$DATA_VOLUME" ] && docker volume inspect "$DATA_VOLUME" >/dev/null \
  || { echo 'missing /data volume'; exit 1; }

umask 077
BACKUP_DIR="$HOME/cpa-ranking-backups"
mkdir -p "$BACKUP_DIR" && chmod 700 "$BACKUP_DIR"
BACKUP_FILE="$BACKUP_DIR/ranking-data-$(date -u +%Y%m%dT%H%M%SZ).tar"
TMP_FILE="$BACKUP_FILE.tmp"
status=0
if docker run --rm --network none \
  --mount "type=volume,src=$DATA_VOLUME,dst=/data,readonly" \
  --entrypoint /bin/busybox "${RANKING_IMAGE:-cpa-usage-ranking:local}" \
  tar -C /data -cf - . > "$TMP_FILE"; then
  mv "$TMP_FILE" "$BACKUP_FILE" || status=$?
else
  status=$?
  rm -f "$TMP_FILE"
fi
docker compose -p cpa-ranking start ranking || status=$?
exit "$status"
```

### 离线恢复

停服后将已校验归档解压至**新建的空数据卷**，不要直接覆盖原卷：

```sh
docker compose -p cpa-ranking stop -t 30 ranking
BACKUP_FILE=/absolute/path/to/verified-backup.tar
docker volume inspect cpa-ranking-restored >/dev/null 2>&1 \
  && { echo 'volume already exists'; exit 1; }
docker volume create cpa-ranking-restored
docker run --rm -i --network none --user 0:0 \
  --mount type=volume,src=cpa-ranking-restored,dst=/data \
  --entrypoint /bin/busybox "${RANKING_IMAGE:-cpa-usage-ranking:local}" \
  sh -c 'tar -C /data -xf - && chown -R 10001:10001 /data && chmod 700 /data' \
  < "$BACKUP_FILE"
```

通过附加 `compose.restore.yaml` 指向新卷启动：

```yaml
volumes:
  ranking-data:
    external: true
    name: cpa-ranking-restored
```

```sh
docker compose -p cpa-ranking -f compose.yaml -f compose.restore.yaml up -d --no-build
```

## 镜像冒烟验证

在安装 Python 3 的 Linux Docker 宿主机上，可使用一次性隔离容器与数据卷完成端到端镜像检查（覆盖建表、前端与头像、离线授权注册、上报与退出、重建持久化、权限、`SIGTERM` 与非法配置启动失败）：

```sh
mkdir -p /path/to/test-work
python3 deploy/test/docker_smoke.py --image cpa-usage-ranking:local \
  --arch amd64 --work-dir /path/to/test-work
```

验证 ARM64 镜像时传入 `--arch arm64`（若 AMD64 宿主机未注册 ARM binfmt，额外传入 `--qemu /path/to/qemu-aarch64-static`）。
