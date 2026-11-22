# 接入流程与时序

参榜客户端提供完整日用量；只展示榜单的客户端直接匿名调用 `GET /v1/leaderboards/{board}/{period}`（无查询参数）。下列客户端本地状态由接入方负责，完整字段和错误码见[接入指南](integration.md)。

## 注册：客户端办结与浏览器回跳分开

```mermaid
sequenceDiagram
    participant C as 客户端
    participant B as 用户浏览器
    participant R as Ranking
    participant G as GitHub
    Note over C: 首次生成并持久化 32 字节 Bearer 凭据
    C->>R: POST /v1/clients/register/start：CPA UUID、return_to、Bearer
    R-->>C: 200：request_id、authorize_url（此时未参榜）
    C->>B: 打开完整 authorize_url
    B->>R: GET /auth/github/authorize
    R-->>B: 跳转到 GitHub，保存浏览器会话
    B->>G: 浏览器授权
    G-->>B: 回到 Ranking 固定回调
    B->>R: GET /auth/github/callback
    R->>G: 核验成功授权，换码和读取数字用户 ID
    R-->>B: 记录同意或拒绝后，跳回原 return_to，不附参数
    C->>R: POST /v1/clients/register/complete：原编号、原 Bearer
    R-->>C: 201 新席位 / 200 复用或重试 / 202 等待授权 / 业务错误
```

`202` 按 `Retry-After` 使用原编号重试。办结回复丢失时，未到期用原编号和原密钥重试；过期则保留原密钥重新领号，用相同 GitHub 帐号、相同 CPA UUID 授权再办结。已退出或被禁用的席位不会恢复。授权拒绝后不应自动再次发起授权。

## 上报：不同 writer、同一席位

```mermaid
sequenceDiagram
    participant A as 上报端 A
    participant R as Ranking（席位 42）
    participant B as 上报端 B
    Note over A,B: 每端用独立凭据；PUT /v1/clients/report/{utc_day}
    A->>R: 第 1 版，完整用量 100
    B->>R: 第 1 版，完整用量 120
    Note over R: 生效 120，不是 220
    A->>R: 原样重发第 1 版
    R-->>A: 200，revision 为 1；仍生效 120
    A->>R: 第 2 版，完整用量 110
    Note over R: 生效 110
    A->>R: POST /v1/clients/withdraw
    R-->>A: 204，整席退出已落库
    B->>R: 重发旧快照或新上报
    R-->>B: 403 client_withdrawn；A 也不能上报
```

上报端应先保存待发的日期、修订号与完整正文，再发送。收到 `200` 时核对回执中的 `revision` 是否是**本次请求**的值。响应超时或丢失不代表事务未提交；保留原任务有界退避并重试。`409` 返回本端当前版本，先核对状态，不盲目加号覆盖。`410 expired_day` 后不要改日期重报。`429` 或 `503 report_overloaded` 需遵守 `Retry-After`。上报窗口只接受 UTC 今天或昨天。

## 榜单缓存与成功判定

| 阶段 | 完成标志 | 不代表什么 |
|---|---|---|
| 领号 | 收到 `request_id` 和授权链接 | 没有上报权限 |
| 注册 | 收到 `201` 或 `200` | 未提交用量 |
| 日快照确认 | 收到 `200` 且回执修订号匹配本次请求 | 未保证入榜或已刷新 |
| 退出 | 收到 `204` | 旧榜单缓存不一定已替换 |
| 读榜 | `200` 返回 `entries`、实际边界、`generated_at` | 空榜是正常响应；可能保留上次成功轮次 |

后台基于已提交的资料和统计构建五个榜单、六个 UTC 周期；完整轮次成功后才原子发布。退出请求触发刷新，普通写入不触发；默认定时为 5 分钟，不承诺固定可见时间。`503 leaderboard_unavailable` 不是空榜。`week` / `month` 是上一完整 UTC 自然周 / 月；月首或周一时昨天仍可上报。`/healthz`、`/readyz` 供独立运维检查，不是接入步骤。
