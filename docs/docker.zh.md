# Docker 部署

[English](docker.md) · [返回 README](../README.zh.md)

镜像支持 Linux `amd64` 和 `arm64`，包含启用 CGO/SQLite 的 Go 服务、前端构建产物及第三方声明、内置头像和正式模型目录；运行时不需要 Node 或编译器。两个架构均已构建并做功能检查，其中 ARM64 使用显式 QEMU 用户态模拟，并非 ARM 原生实机验证。本项目不提供独立二进制发行包。

## 构建和启动

安装 Docker Engine、Buildx 和 Compose，在仓库根目录执行：

```sh
docker compose -p cpa-ranking up -d --build
docker compose -p cpa-ranking ps
docker compose -p cpa-ranking logs ranking
```

示例按构建机平台构建 `cpa-usage-ranking:local`，用 `cpa-ranking_ranking-data` 持久化 `/data`，只在宿主 `127.0.0.1:8080` 发布端口。也可单独构建指定架构而不运行：

```sh
docker buildx build --platform linux/amd64 --load -t cpa-usage-ranking:local .
# 将 linux/amd64 改为 linux/arm64 可构建 ARM64 镜像。
```

构建阶段运行在构建机架构上，必要时使用目标 C 交叉编译器；最终镜像阶段不运行目标架构命令。运行异构镜像仍需要合适的模拟器或原生主机。构建需要下载基础镜像和构建依赖；`--load` 需要支持将单平台镜像导入本机引擎的 Buildx 构建器。

## GHCR 镜像与发布

GHCR 实际发布镜像后，可将 `RANKING_IMAGE` 设置为完整镜像引用；以下账户、仓库和版本仅为示例，请替换为真实值，仓库路径使用小写。私有包需要相应 GHCR 访问权限；是否公开取决于包设置。

```sh
export RANKING_IMAGE=ghcr.io/owner/repository:v1.2.3
docker compose -p cpa-ranking pull ranking
docker compose -p cpa-ranking up -d --no-build
```

不设置 `RANKING_IMAGE` 时仍默认本地构建。使用仓库镜像时加 `--no-build`，避免 Compose 用本地构建替代。镜像标签可能被移动或重新发布；可复现部署应使用已验证的 manifest digest，例如 `ghcr.io/owner/repository@sha256:<digest>`，并保护 Git 发布标签。发布工作流不会创建 GitHub Release 或发行独立二进制。

配置的 GHCR 工作流采用以下策略：

- 推送的稳定 Git 标签须严格为 `vMAJOR.MINOR.PATCH`，数字除 `0` 外不能带前导零，且对应提交必须可从仓库默认分支到达。支持附注标签和轻量标签；拒绝预发布版本和未合并分支的标签。发布 `vMAJOR.MINOR.PATCH` 与 `sha-<完整提交SHA>`。
- 手动发布必须选择非默认分支，发布 `dev-sha-<完整提交SHA>`；若提供可选开发名称，还发布 `dev-<名称>`。名称须使用小写 Docker 标签字符、以字母或数字开头，最多 124 个字符；保留名称、版本式名称和 `sha-` 开头的名称会被拒绝。留空时仅发布 SHA 标签。
- 不发布 `latest`、数字版本、主版本或次版本别名，从而避免旧版本回退共享稳定通道；使用方选择完整版本或 digest。开发名称不能进入稳定命名空间；后续运行可以移动开发名称标签。
- 发布必须先通过策略校验和完整复用 CI；任何必要检查失败、取消或跳过都阻止发布。CI 执行 Go、前端、策略测试以及两种架构的镜像构建和运行检查。ARM64 在 GitHub 托管的 AMD64 runner 上通过 QEMU 模拟，不是原生硬件；该工作流不运行浏览器测试。最终多架构发布会重新构建，因此最终 digest 并非本地测试镜像的 digest。
- 只有发布任务获得 `packages: write`，使用 `GITHUB_TOKEN` 并从仓库上下文派生镜像名。PR 和普通 CI 不登录或推送 GHCR。Buildx 缓存保存在 GitHub Actions 缓存而非 GHCR。发布串行执行；GitHub 可能替换已有的等待中运行，须检查状态并按需重跑遗漏的发布。

工作流只是发布配置，不代表当前已有可用镜像或包。分支和标签保护、包可见性及仓库 Actions 权限仍须由管理员配置。

**不要将此 HTTP 服务直接暴露到公网。** 注册和上报携带 Bearer 凭据，应通过 HTTPS 反向代理终止 TLS，限制对容器和本机监听端口的直接访问，并确保代理日志不记录 Authorization 或请求体。镜像在容器内监听所有接口，即使宿主端口只绑定回环，可访问容器网络的其他容器仍可能直连。默认不信任转发来源 IP；确需代理时只信任真实代理的 IP/CIDR，见[注册来源说明](registration-sources.md)。

Compose 示例未设置 CPU 或内存上限。请按主机预算配置 `cpus`、`mem_limit` 和 `memswap_limit`；资源配额不是吞吐保证。冒烟检查使用 2 CPU 配额、1 GiB 内存上限且不额外使用交换空间，这不表示生产最低配置要求。

## 数据、权限和停止

服务以 UID/GID `10001:10001` 非 root 运行。新的 Docker 管理数据卷内 `/data` 初始所有者为 `10001:10001`、权限为 `0700`；数据库文件权限可能更宽，但受该目录保护。已有数据卷、绑定目录、rootless UID 映射及 SELinux 策略需确保该 UID 可写；镜像启动时不会递归修改所有权。在常规 rootful Linux 主机上，管理员例如可用 `install -d -m 0700 -o 10001 -g 10001 /srv/cpa-ranking` 准备一个空绑定目录；应防止其他用户访问。

只运行一个实例，使用本地持久文件系统，不使用 NFS/网络共享或多个副本共用数据库。SQLite 使用 WAL：运行中保留 `-wal` 和 `-shm` 文件，不要删除它们或只复制打开中的数据库文件。停止容器会向服务发送 SIGTERM。Compose 默认留 30 秒，`RANKING_SHUTDOWN_TIMEOUT` 默认对各适用排空阶段分别为 10 秒；增大后也应相应增加 Docker 停止宽限并留余量。强杀或超时不属于正常关停。`docker compose down -v` 会删除数据卷，除非确实要删除数据，否则不要使用。

## 配置和健康检查

服务配置通过 Compose 的 `environment` 或 `docker run -e` 传入；仅在宿主 shell 中 export 不会自动传入容器。`RANKING_IMAGE` 只用于 Compose 选择镜像，不是服务配置。

下列**镜像**默认值不同于 README 中源码运行的默认值：

| 环境变量 | 镜像默认值 | 用途 |
| --- | --- | --- |
| `RANKING_HTTP_ADDR` | `0.0.0.0:8080` | 容器内监听；Compose 宿主映射另行限制为回环 |
| `RANKING_SQLITE_PATH` | `/data/ranking.db` | 持久卷中的 SQLite 文件 |
| `RANKING_WEB_DIR` | `/app/web` | 前端构建目录 |
| `RANKING_MODELS_PATH` | `/app/config/models.json` | 必需的模型目录 |
| `RANKING_HEALTHCHECK_URL` | `http://127.0.0.1:8080/readyz` | 镜像健康检查目标，不是服务配置 |

修改监听地址或端口时，同时调整 Compose 端口映射和可从容器本机访问的 `RANKING_HEALTHCHECK_URL`。镜像以 `/readyz` 检查数据库、目录和首轮真实缓存是否就绪；`/healthz` 只表示存活。后续缓存刷新失败时保留上次成功结果，因此就绪不承诺数据新鲜度。关停、生产默认注册/上报限流等其余配置见 [README](../README.zh.md#配置)、[注册 API](registration-api.md)和[上报 API](report-snapshots.md)。显式空值或非法配置，以及缺失的必需文件，会导致启动失败。

## GitHub OAuth App

1. 创建 GitHub OAuth App。Authorization callback URL 使用 Ranking 的公开 HTTPS 入口加固定路径，例如 `https://ranking.example.com/auth/github/callback`，**不是**客户端传入的 `return_to`。不需要仓库或邮箱权限。
2. 启动时一起传入 `RANKING_PUBLIC_ORIGIN=https://ranking.example.com`、`RANKING_GITHUB_CLIENT_ID` 与 `RANKING_GITHUB_CLIENT_SECRET`。`compose.yaml` 从部署环境透传这三项及可选的 `RANKING_REGISTRATION_ENABLED`；`docker run` 可使用相应 `-e` 或受保护的运行时环境文件。不要把密钥提交到仓库、打入镜像或打印解析后的容器配置。
3. 将公网 HTTPS 下的 `/auth/github/*` 和业务 API 路由到同一 Ranking 实例。代理访问日志在授权和回调路由只记路径，不记录完整查询串；各层均不记录 Authorization、Cookie 或请求体。允许 Ranking 经 HTTPS 访问 `github.com` 和 `api.github.com`；服务不会主动请求客户端的 `return_to`。

未配置这三项或 `RANKING_REGISTRATION_ENABLED=false` 时，新注册关闭，已有上报端的上报、主动退出及公开读取仍可用。只配置部分 OAuth 变量会导致启动失败。从旧注册结构切换须使用**新数据卷**；没有迁移或补绑。按需另行保留原数据卷。

## 离线备份与恢复

升级前先正常停止服务并确认退出成功。以下在仓库根目录执行，服务停止期间归档默认卷的**全部**内容；将归档安全保存到独立的持久位置，另行保存部署设置和自定义模型目录。客户端也必须自行保存凭据：服务端仅保存凭据摘要。

```sh
docker compose -p cpa-ranking stop -t 30 ranking
# 确认输出为 0 后再继续备份。
docker inspect --format '{{.State.ExitCode}}' "$(docker compose -p cpa-ranking ps -aq ranking)"
docker volume inspect cpa-ranking_ranking-data >/dev/null
umask 077
BACKUP_DIR="$HOME/cpa-ranking-backups"
mkdir -p "$BACKUP_DIR" && chmod 700 "$BACKUP_DIR"
BACKUP_FILE="$BACKUP_DIR/ranking-data-$(date -u +%Y%m%dT%H%M%SZ).tar"
docker run --rm --network none \
  --mount type=volume,src=cpa-ranking_ranking-data,dst=/data,readonly \
  --entrypoint /bin/busybox "${RANKING_IMAGE:-cpa-usage-ranking:local}" \
  tar -C /data -cf - . > "$BACKUP_FILE"
# 适当时恢复原服务：
docker compose -p cpa-ranking start ranking
```

恢复时再次停止服务，将**可信**备份解压到新的空数据卷，不覆盖原卷或运行中的数据库。例如：

```sh
docker compose -p cpa-ranking stop -t 30 ranking
BACKUP_FILE=/absolute/path/to/verified-backup.tar
# 若卷已存在，请换一个名字。
docker volume inspect cpa-ranking-restored >/dev/null 2>&1 && { echo 'volume already exists'; exit 1; }
docker volume create cpa-ranking-restored
docker run --rm -i --network none --user 0:0 \
  --mount type=volume,src=cpa-ranking-restored,dst=/data \
  --entrypoint /bin/busybox "${RANKING_IMAGE:-cpa-usage-ranking:local}" \
  sh -c 'tar -C /data -xf - && chown -R 10001:10001 /data && chmod 700 /data' \
  < "$BACKUP_FILE"
```

通过额外的 `compose.restore.yaml` 将 Compose 指向新卷：

```yaml
volumes:
  ranking-data:
    external: true
    name: cpa-ranking-restored
```

```sh
docker compose -p cpa-ranking -f compose.yaml -f compose.restore.yaml up -d --no-build
```

检查就绪状态和预期数据。原卷和备份仍会保留；恢复会丢失备份之后的变更。启动支持新数据库或当前结构的数据库，不升级、降级或转换旧结构；不要假定不兼容的数据库可与其他镜像一起使用。

## 可重复的镜像检查

在安装 Python 3 的本地 Linux Docker 主机上执行。脚本创建带唯一标签的临时容器和数据卷、不发布端口，结束后删除测试资源：

```sh
mkdir -p /path/to/test-work
python3 deploy/test/docker_smoke.py --image cpa-usage-ranking:local \
  --arch amd64 --work-dir /path/to/test-work
```

ARM64 镜像使用 `--arch arm64`。AMD64 主机未注册 ARM binfmt 时，可传入 `--qemu /静态/qemu-aarch64-static的绝对路径`；脚本显式调用该模拟器并在模拟运行中禁用 Docker 容器内健康检查，同时通过 HTTP 检查 `/readyz`。脚本不会安装或注册模拟器。检查覆盖结构初始化、页面和头像、注册（离线批准，不访问 GitHub）、上报与退出、重建后的持久化、权限、SIGTERM 正常退出及启动失败；不属于负载或容量测试。
