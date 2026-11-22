# 性能基准与压测工具

[English](benchmark.md) · [返回 README](../README.zh.md)

本文汇总 500 TPS 持续上报基准测试结果，并简要介绍仓库内置的压测与离线对账工具 `cmd/ranking-capacity`。

## 500 TPS 持续上报基准参考

**测试日期：2026-11-13。** 在 `2 vCPU / 1 GiB RAM（无 Swap）` 的 cgroup v2 限制下，服务连续 **1,200.727 秒**（约 20 分钟）承接了 **499.91 条/秒** 的新逻辑上报，同时消化了起始积压并完成 4 轮定时榜单缓存构建。

> **适用范围说明**：该基准验证了核心写入引擎（Go + SQLite WAL + `synchronous=FULL` + 单写串行队列 + 后台日结 + 原子缓存）的持续处理与积压恢复能力。测试基于引入 GitHub OAuth 注册与移除 `service_tier` 之前的构建（其中约 1% 请求走了旧注册接口，日快照含按 tier 拆分行），因此仅作为特定负载下的历史参考基准，不构成最大吞吐、零 503 或当前表结构的容量承诺。

### 测试环境与负载模型

| 项目 | 被测服务端主机 | 负载发生器主机 |
| --- | --- | --- |
| CPU / vCPU | Intel Xeon E5-2690 v3 @ 2.60GHz（3 vCPU KVM） | AMD Ryzen 9 9950X（2 vCPU KVM） |
| 可见内存 / 磁盘 | 约 2,978 MiB / 45 GiB VirtIO ext4 | 约 3,884 MiB / 50 GiB VirtIO ext4 |
| 操作系统 / 内核 | Debian 13 / 6.12.113 | Debian 13 / 6.12.113 |
| 服务 cgroup v2 限制 | `200%` CPU（`2 vCPU`）、`MemoryMax 1 GiB`、`MemoryHigh 768 MiB`、`MemorySwapMax 0` | — |
| 运行软件 | Go `1.26.10`（CGO）、Gin `1.12.0`、`go-sqlite3 1.14.53` / SQLite `3.54.1`（`WAL`、`FULL` 同步、`5m` 缓存刷新） | OpenSSH `10.0p2` 隧道访问回环 HTTP |

- **数据规模与到达模型**：初始预填 10,000 个身份与 90 天历史形状（确认段内增至 20,622 个身份），起始携带 **16,330 条待确认积压**并额外注入 **2,500 条标记积压**，叠加 500 TPS 开放式到达（今日约 90%、昨日约 10%；轻/中/重客户端 60%/30%/10%；10% 热点客户端贡献 50% 更新；请求体分桶上界 P50 ≤1,344 B、P95 ≤7,808 B、P99 ≤14,208 B）。
- **限流设置**：测试期间放宽了全局与单客户端限流（全局 `10 µs` / burst `100,000`，单客户端 `100 µs` / burst `1,000`），用于测量写入引擎能力而非生产默认限流值。

### 吞吐与积压恢复

| 指标 | 结果 |
| --- | --- |
| 普通新逻辑上报 | `600,254` 条（**499.91 条/秒**） |
| 正确 ACK（含积压消化） | `619,077` 条（**515.59 条/秒**） |
| HTTP 请求总数（上报 / 旧注册） | `708,155` 次（`702,108` / `6,047`，含 `84,445` 次重试） |
| 上报 `200`（版本匹配）/ `503` | `619,077` / `83,024`（占已收上报响应 `11.83%`） |
| 确认期缓存代际推进 | `4` 次 |
| 停压收尾后未确认 | `6` 条（其中 3 条已落库但 ACK 不确定，未计入成功） |

98.70% 的 `503` 发生在前 **185.727 秒**的消债阶段（峰值积压 `19,790` 条，`503` 占比 `42.36%`）。清空初始积压后的 **1,015.000 秒**稳定期维持约 `500.09 条/秒`，`503` 占比降至 **`0.212%`**（采样积压中位数 `5` 条，P99 `114` 条）。

### 端到端延迟（计划到达至正确 ACK）

| 样本范围 | 已确认样本数 | P50 | P95 | P99 | 最大值 |
| --- | --- | --- | --- | --- | --- |
| 整个确认段的新普通上报 | `600,247` | `14.24 ms` | `2.24 s` | `42.91 s` | `169.57 s` |
| 清空积压后的稳定阶段 | `507,584` | `11.44 ms` | `89.36 ms` | `205.34 ms` | `9.14 s` |

### 资源使用与账本一致性

- **服务主进程**：平均 CPU 约 `1.01` 核，RSS 峰值 `120.28 MiB`（不含 cgroup 文件缓存）；被测主机可用内存最低仍余 `1,544.69 MiB`。
- **离线对账**：基于种子库与压测账本重建后的结果与最终数据库完全一致——覆盖 `38,622` 条日修订记录、`246,565` 行日指标与 `920,000` 行周期指标的全部 12 项统计值，SQLite 完整性与外键检查全部通过，无错账或重复计数。

## 压测与对账工具（`cmd/ranking-capacity`）

`ranking-capacity` 是开发期可选的离线造数、有界发压与账本核对 CLI，**不打入生产 Docker 镜像**。仅可在配置好外部资源限额的隔离测试服务与合成数据库上运行。

> **安全提示**：生成的 fixture 清单包含派生合成 Bearer 凭据的种子；请将测试目录（`0700`）、清单（`0600`）与 JSONL 账本严格保密，切勿提交到仓库或与真实身份混用。

### 编译与离线造数

```sh
install -d -m 700 /path/to/private-test
go build -o /path/to/private-test/ranking-capacity ./cmd/ranking-capacity

# 标准 8 模型测试库（持续随机压测改用 `seed-sustained --model-seed 1`）
/path/to/private-test/ranking-capacity seed \
  --db /path/to/private-test/seed.sqlite \
  --fixture /path/to/private-test/fixture.json \
  --models config/models.json --clients 6000 --history-days 90 --backlog 1000
```

保留原始 `seed.sqlite` 作为对账基线，将隔离测试用的 `ranking` 服务指向其**副本**启动。

### 发送压测流量

工具提供三种针对预置席位的上报发压模式（均只调用 `PUT /v1/clients/report/{utc_day}`，不触发 GitHub OAuth）：

```sh
# 1. 定速开放式发压（不自动重试）
/path/to/private-test/ranking-capacity load \
  --fixture /path/to/private-test/fixture.json --url http://127.0.0.1:4210 \
  --rps 10 --duration 10m --revision 2 --changed 2 \
  --ledger /path/to/private-test/steady.jsonl

# 2. 同步齐发波峰 + 可重试周期背景流量
/path/to/private-test/ranking-capacity burst \
  --fixture /path/to/private-test/fixture.json --url http://127.0.0.1:4210 \
  --duration 2m --cycle 10m --changed 2 \
  --ledger /path/to/private-test/attempts.jsonl \
  --events /path/to/private-test/tasks.jsonl --samples /path/to/private-test/samples.jsonl

# 3. 交互式泊松持续发压（从标准输入读取 JSON 指令：stage / pause / resume / stop，支持 .jsonl 与 .jsonl.gz）
/path/to/private-test/ranking-capacity sustained \
  --fixture /path/to/private-test/fixture.json --models config/models.json \
  --model-seed 1 --arrival-seed 2 --url http://127.0.0.1:4210 \
  --journal /path/to/private-test/sustained.jsonl.gz
```

### 离线对账与读库探针

优雅停止测试服务确保 SQLite 快照一致后，执行对账与读库耗时检测：

```sh
# 查看数据库原始行数与总量
/path/to/private-test/ranking-capacity inspect --db /path/to/private-test/running.sqlite

# 核对 `load` / `burst` 账本（逗号分隔传入全部历史账本）
/path/to/private-test/ranking-capacity verify \
  --fixture /path/to/private-test/fixture.json \
  --db /path/to/private-test/running.sqlite \
  --ledgers /path/to/private-test/steady.jsonl

# 基于未修改种子库完整重建并核对 `sustained` 日志，同时生成阶段汇总
/path/to/private-test/ranking-capacity verify-sustained \
  --fixture /path/to/private-test/fixture.json --models config/models.json \
  --model-seed 1 --seed-db /path/to/private-test/seed.sqlite \
  --db /path/to/private-test/final.sqlite --journal /path/to/private-test/sustained.jsonl.gz \
  --expected-db /path/to/private-test/reconstructed.sqlite
/path/to/private-test/ranking-capacity summarize-sustained \
  --journal /path/to/private-test/sustained.jsonl.gz --models config/models.json

# 测量各周期只读统计查询耗时（不代表完整缓存构建耗时）
/path/to/private-test/ranking-capacity probe --db /path/to/private-test/running.sqlite --day "$(date -u +%F)"
```

完整参数及安全边界（`--max-in-flight`、`--max-pending`、`--max-journal-bytes`、`--active-budget` 及 `assess-sustained`）可通过 `ranking-capacity <子命令> -h` 查看。
