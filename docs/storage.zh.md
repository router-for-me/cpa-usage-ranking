# 存储、结算与缓存架构

[English](storage.md) · [返回 README](../README.zh.md)

服务端仅依赖单个本地 SQLite 数据库，启动时直接初始化当前表结构——无需外部数据库服务，也不创建示例参与者或迁移旧版表结构。

## 存储引擎、文件位置与连接池

- **内置 SQLite 引擎**：标准构建通过 `database/sql` 与 `github.com/mattn/go-sqlite3` `v1.14.53` 静态链接 SQLite `3.54.1`（已包含 3.51.3 引入的官方 [WAL-reset 修复](https://sqlite.org/wal.html#walreset)）。编译期需要 CGO 与 C 编译器，运行期不依赖宿主机 `sqlite3` 动态库。
- **数据文件路径（`RANKING_SQLITE_PATH`）**：源码默认 `data/ranking.db`，容器镜像默认 `/data/ranking.db`。严格按普通文件路径处理（会转义 URI 字符，不提供内存模式）。缺失的父目录按 `0700` 创建（受 umask 影响）；路径不可访问或文件损坏会在启动时直接失败，不会自动删除、改名或覆盖原库。
- **本地存储与 WAL 文件**：WAL 模式依赖本地文件系统协调，切勿将数据库放在 NFS/网络共享或供多副本共用。运行期间保留 `-wal` 与 `-shm` 伴随文件；离线拷贝前须先优雅停服（详见[部署、配置与运维指南](deployment.zh.md#数据权限停机与离线备份)）。

| 连接池配置 | 当前值 |
| --- | --- |
| 写连接池 | `1` 打开 / `1` 空闲（`BEGIN IMMEDIATE`） |
| 读连接池 | `8` 打开 / `4` 空闲（`mode=ro`） |
| 日志与同步 | `WAL`（打开时校验）+ `synchronous=FULL` |
| 外键约束 | 启用 |
| 锁等待超时 | `5s` |

读事务在独立的 WAL 快照上执行，不阻塞写入。HTTP 优雅停机完成后再依次关闭读写连接池。

## 核心表结构概览

启动时在单事务内幂等创建当前表结构与索引；结构不兼容变更需使用全新数据库。

| 表名 | 主键与职责 |
| --- | --- |
| `clients` | 自增正整数 `id`；唯一席位 `(github_id, server_id)`；`display_name`、`avatar_id`、`created_at_ms`；独立的 `disabled`、`ranking_hidden`、`withdrawn` 标志 |
| `client_writers` | `(id, client_id)` 与全局唯一 32 字节 `SHA-256` `token_hash`（不存凭据原文，不跨席位改绑） |
| `registration_requests` | `WITHOUT ROWID`（主键 `id`）；有界 OAuth 请求（最长 `10m` 有效、全站最多 `2,000` 笔未过期；容量满时 `denied`、`failed` 或最早的 `completed` 终态记录可能被提前回收，提前回收后 `complete` 返回 `410 registration_request_expired`），保存 `server_id`、`return_to`、`avatar_id`、摘要、state/PKCE 与完成结果 |
| `models` | 稳定正整数 ID 与唯一标准模型名映射（ID `0` 保留给内部总计；别名与价格不入库） |
| `daily_reports` | `WITHOUT ROWID`（主键 `(day, client_id)`）；席位单日父表（记录 `received_at_ms`），日结删除时级联清理版本与统计行 |
| `daily_writer_versions` | `WITHOUT ROWID`（主键 `(day, writer_id)`）；各上报端最新 `revision` 与 32 字节内容摘要 |
| `daily_stats` | `WITHOUT ROWID`（主键 `(day, client_id, model_id)`）；生效快照的 12 项原始汇总（`model_id = 0` 为全模型总计行） |
| `period_stats` | 主键 `(kind, start_day, model_id, client_id)`；已日结单日总计及周、月、累计（`0001-01-01`）汇总行 |
| `report_window` | 单行表（`id = 1`），保存单调递增的 `closed_before_day` UTC 封窗水位线 |
| `admin_accounts` | 可选的数据库普通管理员账号（仅存 bcrypt 密码哈希） |

**注册事务一致性**：浏览器回调校验并消费一次性 state、浏览器 Cookie 凭证与 S256 PKCE，向 GitHub 换码期间不持有 SQLite 写锁，也不持久化 GitHub Token。仅 `POST /v1/clients/register/complete` 在单次 `IMMEDIATE` 事务内完成席位创建与上报端绑定，异常则整体回滚。

## 串行写队列、UTC 封窗与后台日结

- **单写串行队列**：内部专用写协程串行执行上报写入与后台维护任务，默认 `128` 容量的上报待写队列与单槽维护队列（双方均有任务时交替调度），单事务默认超时 `5s`（`RANKING_REPORT_TRANSACTION_TIMEOUT`）。SQLite 锁等待响应超时、取消与优雅停机信号。
- **持久 UTC 水位线（`closed_before_day`）**：根据服务端观察到的 UTC 时间仅向前推进（至少推进至 UTC 昨日），在启动时、后台周期检查（最长 `1m`）及每笔上报事务前后推进校验。时钟回拨、重试或重启均无法重新打开已关闭日期。
- **后台日结（Settlement）**：
  - 按 `(day, client_id)` 顺序扫描早于 `closed_before_day` 的日快照，逐席逐日单事务结转：将 12 项原始汇总累加至 `period_stats` 的周/月/累计行，并写入仅含总计的已日结单日行（空快照不生成）。
  - 同事务内删除 `daily_reports` 源行（级联清理 `daily_stats` 与 `daily_writer_versions`）；算术溢出检查或数据库错误会回滚该条以便重试，绝不重复累加。
- **自然保留策略（`RANKING_SETTLED_DAY_RETENTION_DAYS`）**：仅清理已日结的 `period_stats`（绝不触碰未结 `daily_stats`），经同一写协程分批执行：
  - **单日总计行**：默认保留 UTC 今日及此前 `30` 天（共 `31` 天，可配 `1`–`36500`）。
  - **周汇总行**：保留当前与上一完整 UTC 自然周（周一起）。
  - **月与累计汇总行**：长期保留。

## 一致性读快照与原子缓存刷新

- **单事务只读合并**：候选读取在单个 `mode=ro` 事务中合并 `period_stats` 已结行与未结 `daily_stats`（年度合并已结月行与本年未结日行，累计含全部未结积压），并同快照读取席位资料与状态标志，在评分前过滤封禁、下榜与退出席位。
- **指标存储与派生**：Token 数、执行次数与耗时样本数按精确 `int64`（`INTEGER`）存储，`ttft_sum_ms` 与 `latency_sum_ms` 按 `float64`（`REAL`）存储。比例与均值直接派生而不改动底层行；分母为零返回 `null` 而非伪造零值。
- **原子 6 周期缓存**：单个串行 worker 在启动时及每 `RANKING_CACHE_REFRESH_INTERVAL`（`5m`）构建全部 6 周期 × 5 榜单，仅整轮成功后原子替换只读快照（失败保留旧快照）。普通上报与日结不触发立即重建，管理员治理、席位退出与席位恢复（`withdrawn` 从 `1` 变为 `0`）均在提交后触发异步合并刷新。
- **内存保护边界**：默认单视图最多 `100,000` 候选人、整轮编码上限 `16 MiB`、最多 `32` 个构建项，属于防 OOM 安全边界而非容量保证。
