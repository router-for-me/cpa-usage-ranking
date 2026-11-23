# 排行榜与评分规则

[English](leaderboards.md) · [返回 README](../README.zh.md)

`GET /v1/leaderboards/{board}/{period}` 仅从内存原子缓存读取最新一轮完整构建结果。支持的榜单为 `overall`、`usage`、`cache`、`success`、`ttft`，支持的周期为 `today`、`yesterday`、`week`、`month`、`year`、`all`。不接受任何查询参数、自定义日期、分页或模型筛选。

## 响应格式

```json
{
  "board": "usage",
  "period": "today",
  "start": "2026-12-31",
  "end": "2027-01-01",
  "generated_at": "2026-12-31T12:00:00Z",
  "entries": [
    {
      "rank": 1,
      "client_id": "42",
      "display_name": "Example",
      "avatar_id": "1",
      "metric": "9007199254740993"
    }
  ]
}
```

- `start` 包含、`end` 不包含，均为 UTC 日期（`all` 使用 `start: "0001-01-01"`、`end: null`）。
- `generated_at` 为本轮缓存构建的 UTC 开始时间，而非客户端最后上报时间。
- `client_id`、`avatar_id` 与 `metric` 均为十进制字符串以保留完整精度；除 `usage` 为整数外，其余指标保留两位小数。`rank` 为 JSON 整数。昵称按纯文本渲染，头像 ID 对应[头像目录](catalogs.zh.md#头像目录)。
- 空榜返回 `200` 与 `entries: []`；首轮缓存构建完成前返回 `503`（`leaderboard_unavailable`）；非法参数或附带查询串返回 `400`（`invalid_parameter`）。后续刷新失败时保留上一轮完整快照及其原始周期边界与 `generated_at`。
- 响应不暴露凭据、摘要、GitHub ID、CPA `server_id`、IP 地址或各分项得分拆解。

### 综合总榜明细（`details`）

仅 `overall` 条目额外包含 `details` 对象，与综合分源自同一周期和同一轮缓存：

```json
{
  "total_tokens": "100",
  "request_count": "4",
  "success_rate": "75.00",
  "cache_read_rate": "35.71",
  "ttft_mean_ms": "75.00",
  "latency_mean_ms": null
}
```

| 字段 | 含义 |
| --- | --- |
| `total_tokens` | 精确总 Token 数（int64 十进制字符串） |
| `request_count` | 精确已接纳执行次数（int64 十进制字符串） |
| `success_rate` | 实际 `success_count / request_count × 100`（**不是** Wilson 成功率得分） |
| `cache_read_rate` | `cache_read_tokens / input_tokens × 100`；无输入 Token 时为 `null` |
| `ttft_mean_ms` | `ttft_sum / ttft_samples`（毫秒）；无样本时为 `null` |
| `latency_mean_ms` | `latency_sum / latency_samples`（毫秒）；无样本时为 `null` |

比例（`"0.00"`–`"100.00"`，自动钳制在 `[0, 100%]` 内）与均值均为保留两位小数的字符串。跨日均值按耗时总和除以总样本数加权计算，而非对每日均值再平均。真实零保持 `"0.00"`，缺失值为 JSON `null`（页面显示 `—`）。其他分榜不包含 `details`。

## 统计周期与参榜资格

| 周期 | 页面名称 | UTC 范围（左闭右开 `[start, end)`） |
| --- | --- | --- |
| `today` | 今日 | 当前 UTC 自然日 `[今日, 明日)` |
| `yesterday` | 昨日 | 前一 UTC 自然日 `[昨日, 今日)` |
| `week` | 周榜 | 上一完整 UTC 自然周（周一起）`[上周一, 本周一)` |
| `month` | 月榜 | 上一完整 UTC 自然月 `[上月 1 日, 本月 1 日)` |
| `year` | 仅 API | 当前 UTC 自然年（合并已结月汇总与本年未结日快照） |
| `all` | 仅 API | 历史累计全量统计 |

- 由于上报窗口开放 UTC 昨日，周一的 `week` 与月初 1 日的 `month` 在昨日封窗前仍可能随修订更新。
- 参榜资格要求席位处于**未封禁、未下榜、未退出**状态，且在所选 UTC 周期内至少有 1 次已接纳执行（不设最低样本数或成功率门槛）。仅汇总全模型总计行。
- 资料、状态标志与统计数据在同一只读 SQLite 快照中读取；封禁、下榜与退出席位在候选上限和评分参考计算**之前**即被过滤。
  - 下榜（`ranking_hidden`）仍允许上报；封禁（`disabled`）与[主动退出](integration.zh.md#退出席位withdraw)会拒绝上报。详见[管理员控制台](admin.zh.md)与[存储与缓存架构](storage.zh.md)。

## 五维评分与排序规则

| 榜单 | 指标 | 资格与排序方向 |
| --- | --- | --- |
| `overall` | 0–100 综合分 | 所有有资格席位；按百分之一分整数降序 |
| `usage` | 已匹配模型的 `total_tokens` | 需 `total_tokens > 0`；按精确 int64 降序 |
| `cache` | `cache_read_tokens / input_tokens × 100` | 需 `input_tokens > 0`；降序 |
| `success` | 双侧 95% Wilson 置信下界（`0`–`100` 分） | 所有有资格席位；降序 |
| `ttft` | 实测平均首字延迟（`ms`） | 需 `ttft_samples > 0`；**升序**（越低越靠前） |

### 综合总榜公式（满分 100 分）

- **用量分（40 分）**：在全部正 Token 席位中的中位分位百分位（相同总量共享同一分位）；零 Token 得 `0` 分。
- **稳定性分（30 分）**：`success_count / request_count` 的双侧 95% Wilson 置信下界（兼顾高成功率与样本量，非原始成功率）。
- **缓存复用分（20 分）**：`cache_read_tokens / input_tokens`（全程精确整数运算并四舍五入，兼容 int64 边界）。无输入 Token 得 `0` 分；实测零缓存读取有效（得 `0` 分）。
- **首字响应分（10 分）**：`1 / (1 + mean / median_positive_mean)` 乘以样本覆盖率（`ttft_samples / request_count`），覆盖率使用精确有理数半入舍入。若所有实测均值均为 `0`，性能因子取 `0.5`；无样本得 `0` 分。

四个加权项各自按**四舍五入保留两位小数（`0.01` 分）**后再求和；缺失分项不向其他项重分配权重。

### 舍入、并列与截断

- `cache`、`success` 与 `ttft` 指标在排序与发布前先四舍五入至两位小数，发布值相等即判定并列。
- 并列采用标准竞争排名（`1, 1, 3`），同分项按稳定的 `client_id` 确定顺序且不改变名次。
- 每个榜单在完成全部候选与参考样本计算后再截断为 **前 100 名**，不因并列超额展开。
- 榜单数据来自客户端自主上报，不构成真实性认证或 SLA 性能承诺。

## Web 前端展示规则

- 前端页面提供全部五个榜单的 `today`、`yesterday`、`week`、`month` 视图（默认 `overall/today`），清晰区分加载、空榜与错误状态；切换或重试失败时，已保留的结果始终保持自身对应的榜单、周期与 `generated_at` 标签。
- 数值列使用等宽数字对齐。Token 与请求数采用紧凑 `K`（`1,000`）、`M`（`1,000,000`）、`B`（`1,000,000,000`）单位，通过精确 `BigInt` 整数缩放后保留两位小数，不经过 JavaScript `Number` 转换 int64 计数。
- 耗时在 `ms`、`s`、`m`、`h`、`d` 间自适应切换；正的亚毫秒值显示 `<1 ms`，实测零显示 `0 ms`，缺失值显示 `—`。数字与单位以空格分隔（如 `1.20 B`、`99.00 %`、`1.75 s`）。
- 紧凑格式仅用于页面展示，不影响 API 原始精度、评分或并列判定。
