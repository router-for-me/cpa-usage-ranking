# 客户端接入指南

[English](integration.md) · [返回 README](../README.zh.md)

本文定义面向任意用量采集器、网关或面板的公开 HTTP JSON 协议。接入方自行读取并归并本地用量数据，服务端不依赖特定客户端 SDK、数据库结构或上游事件表。

## 接口总览

所有路径均相对于部署者的 HTTPS 入口（例如 `https://ranking.example.com`）。

| 方法 | 路径 | 鉴权 | 用途 |
| --- | --- | --- | --- |
| `POST` | `/v1/clients/register/start` | 本端 `Bearer <secret>` | 提交 CPA `server_id`、`return_to` 与可选 `avatar_id`，获取 `request_id` 和 `authorize_url` |
| `POST` | `/v1/clients/register/complete` | 同一 `Bearer <secret>` | GitHub 授权后完成注册 |
| `POST` | `/v1/clients/withdraw` | 已注册的 `Bearer <secret>` | 退出整个席位 |
| `PUT` | `/v1/clients/report/{utc_day}` | 已注册的 `Bearer <secret>` | 原子替换该席位某个 UTC 日的完整累计快照 |
| `GET` | `/v1/leaderboards/{board}/{period}` | 无需 | 读取最新发布的公开缓存榜单（最多 100 名） |
| `GET` / `HEAD` | `/avatars/catalog.json` | 无需 | 获取头像清单、默认 ID 及相对图片路径 |
| `GET` / `HEAD` | `/avatars/{path}` | 无需 | 获取 WebP 头像二进制（`HEAD` 仅返回响应头） |
| `GET` | `/healthz` | 无需 | 存活探针（`200 {"status":"ok"}`） |
| `GET` | `/readyz` | 无需 | 就绪探针（数据库、目录及首轮缓存就绪返回 `200`，否则 `503`） |

浏览器在授权期间还会经由 `/auth/github/authorize` 与 `/auth/github/callback` 跳转，它们是浏览器重定向路由而非上报客户端 JSON API。未就绪或运行中 SQLite 不可用时，`/readyz` 返回 `503 {"status":"not_ready","reason":"initializing_or_unavailable"}`（非业务 `error.code` 信封）。

**传输与隐私要求：**
- 生产环境必须使用 **HTTPS**；回环 HTTP 仅用于本地调试或受信任的 TLS 反向代理之后。
- 请求正文使用 UTF-8 JSON，推荐设置 `Content-Type: application/json`（省略 `Content-Type` 或附带 `charset` 均可接受；注册与上报正文均支持 `identity` 与 `gzip` 压缩，且压缩流与解压后体积均受各自上限约束）。
- 切勿在 URL、请求体、日志、公开页面或代码仓库中暴露 Bearer 凭据，也不要上传上游模型 API Key、提示词或响应正文。
- 仅展示排行榜的应用无需注册或持有凭据。

## 凭据、注册与退出

### 客户端自持凭据

首次注册前，每个上报端独立生成 **32 字节密码学安全随机数**，推荐编码为 **43 字符无填充 `base64url`**（RFC 4648 URL 字符集 `A–Z a–z 0–9 - _`，不含 `=` 或空白），并先持久化到受保护的本地存储。

- **与 `server_id` 的区别**：Bearer 凭据是每个上报端用于接口**鉴权的密钥**（注册后日常上报与退出均靠它鉴权，不再请求 GitHub）；`server_id` 则是 CPA 实例的非机密标识，与 GitHub 用户 ID 组合用于确定排行榜席位。
- 仅发送单个 `Authorization: Bearer <secret>` 请求头（scheme 不区分大小写；对同一 32 字节密钥也兼容多余首尾空白、标准 `base64` 字符集及单个尾随 `=` 填充）。
- 服务端将凭据解码回 32 字节后计算 **`SHA-256` 摘要**存储，不保存原文。
- 同一席位的多个上报端各自持有独立凭据，不得共享同一密钥。
- 服务端不签发、不轮换、不提供找回凭据或会话登录接口；仅保存 `client_id` 无法完成上报或重试。

### GitHub OAuth 注册流程

```mermaid
sequenceDiagram
    participant C as 客户端
    participant B as 用户浏览器
    participant R as Ranking 服务端
    participant G as GitHub
    Note over C: 本地生成并持久化 32 字节 Bearer 凭据
    C->>R: POST /v1/clients/register/start（server_id、return_to、avatar_id?、Bearer）
    R-->>C: 200 {request_id, authorize_url}（此时未建立席位）
    C->>B: 打开完整 authorize_url
    B->>R: GET /auth/github/authorize
    R-->>B: 写入一次性 state、PKCE 与浏览器 Cookie 并跳转 GitHub
    B->>G: 用户同意或拒绝授权
    G-->>B: 回调至 /auth/github/callback
    B->>R: GET /auth/github/callback
    R->>G: 服务端换码并读取 GitHub 数字用户 ID
    R-->>B: 记录结果后原样跳回 return_to（不附加参数）
    C->>R: POST /v1/clients/register/complete（request_id、原 Bearer）
    R-->>C: 201 新席位 / 200 复用或恢复 / 202 待授权 / 业务错误
```

#### 第一步：开始注册（Start）

```http
POST /v1/clients/register/start
Authorization: Bearer <secret>
Content-Type: application/json

{
  "server_id": "550e8400-e29b-41d4-a716-446655440000",
  "return_to": "http://localhost:3001/settings?tab=ranking#status",
  "avatar_id": "12"
}
```

- `server_id`：CPA 实例的持久、非零 `8-4-4-4-12` 十六进制 UUID（不区分大小写，落盘转小写；对应 CPA 配置项 `server.server-id`，可通过 CPA 的 `GET /v8/management/server/server-id` 或任意已鉴权管理接口响应头 `X-CPA-SERVER-ID` 获取）。同一 CPA 实例下的多个采集端须提交相同 `server_id`，不要各自随机生成。
- `return_to`：不含用户信息的绝对 `http` 或 `https` URL（支持 `localhost`、局域网 IP、公网 IP 及域名，保留端口、路径、查询串与锚点）。切勿在其中携带密钥。
- `avatar_id`（可选）：无前导零的正 64 位整数十进制字符串（例如 `"12"`）或正 JSON 整数（例如 `12`），须存在于 `GET /avatars/catalog.json` 目录中。省略或传 `null` 时默认使用目录的 `default_avatar_id`（`"1"`）。格式错误（如 `""`、`"0"`、`0`、负数、前导零、非数字）返回 `400 invalid_request`；正整数合法但不在目录中返回 `422 invalid_avatar`。
- `200 OK` 返回 `{"request_id":"...","authorize_url":"https://ranking.example.com/auth/github/authorize?request_id=..."}`。保存 `request_id`（最长 10 分钟有效；当全站 2,000 笔容量满时，`denied`、`failed` 或最早的 `completed` 终态记录可能被提前回收，提前回收后 `complete` 返回 `410 registration_request_expired`，客户端可保留原 Bearer 凭据重新开始注册，无需等满 10 分钟），并在用户浏览器中原样打开 `authorize_url`。此步骤不创建席位也不授予写入权限。

#### 第二步：浏览器授权

- 服务端通过 `Secure; HttpOnly; SameSite=Lax` Cookie、一次性 state 与 `S256` PKCE 绑定浏览器会话。
- 服务端用回调授权码向 GitHub 换取访问令牌并读取数字用户 ID，不申请仓库或邮箱权限，也不持久化 GitHub Token。
- 记录同意或拒绝后，服务端带 `Referrer-Policy: no-referrer` 原样跳回 `return_to`（不附加任何参数），且绝不会从服务端主动请求 `return_to`。浏览器回跳**不是**注册成功的确认凭证。

#### 第三步：完成注册（Complete）

```http
POST /v1/clients/register/complete
Authorization: Bearer <开始注册时的同一 secret>
Content-Type: application/json

{"request_id": "<原 request_id>"}
```

- `202 Accepted` 伴随 `Retry-After`（秒，空响应体）：等待用户授权，按提示延迟后使用原 `request_id` 和原 Bearer 重试。
- `201 Created`：创建了新的参榜席位。
- `200 OK`：复用了已有席位，或取回了此前已完成的结果。

成功响应（`client_id` 与 `avatar_id` 均为正 64 位整数的十进制字符串）：

```json
{"client_id": "42", "display_name": "Alice", "avatar_id": "12"}
```

**席位归属、资料与恢复规则：**
- **席位主键**：由 **`(GitHub 数字用户 ID, server_id)`** 唯一确定。同一组合新增上报端时共用席位，不改动现有资料；同一凭据不可改绑到其他席位。
- **初始资料**：新建席位（`201 Created`）取 GitHub `name`（为空或不含可见字符时回退至 `login`），经 Unicode NFKC 规范化、清理控制/行段分隔与双向控制字符、去首尾空白并按完整码点边界安全截断至 `≤32` 码点且 `≤128` UTF-8 字节（须含可见内容），同时保存 `start` 传入的 `avatar_id`（未传则取目录默认项）。复用已有席位（`200 OK`）时保持原昵称与头像不变。允许重名，展示时按纯文本转义渲染；不同步 GitHub 头像，也无注册后资料修改接口。
- **丢包恢复**：`start` 响应丢失时用原凭据重新开始注册；`complete` 超时或丢包时，在记录尚未过期且未被容量淘汰前可用原 `request_id` 和原凭据重试；若已过期或在全站 2,000 笔容量满时被提前回收（返回 `410 registration_request_expired`），保留**原凭据**直接重新执行 `start` → 授权 → `complete`（须使用同一 GitHub 账号与 `server_id`，无需等满 10 分钟）。
- **GitHub 换码失败**：若服务端换取令牌或查询 GitHub 用户失败，该事务会被标记为 `failed`；该记录仍保留期间重试同一 `request_id` 返回 `500 internal_error`，到期或在容量满时被提前回收后返回 `410 registration_request_expired`。待上游恢复后，请使用**原 Bearer 凭据 + 新 `request_id`** 重新开始注册并授权，无需等满 10 分钟。
- **封禁、退出与撤销授权**：`disabled`（管理员封禁）无法由客户端通过重试或重新授权解除，但可由管理员解封；`withdrawn`（主动退出）为永久状态，客户端与管理员均不可恢复。在 GitHub 撤销 OAuth 授权**不会**撤销已绑定的上报端；停用整个席位请调用 `POST /v1/clients/withdraw`。

### 退出席位（Withdraw）

```http
POST /v1/clients/withdraw
Authorization: Bearer <已注册的 Bearer 凭据>
```

- 返回 `204 No Content` 表示整席退出已落库（同一席位任意上报端重复调用均幂等返回 `204`；可选查询参数或如 `{}` 等小体积请求体会被忽略，绝不覆盖 Bearer 鉴权身份）。
- 退出保留历史统计与管理员标记，但会立即阻止该席位**所有上报端**继续上报或重试（返回 `403 client_withdrawn`），并在下一次成功发布榜单缓存后从全部榜单与评分参考样本中移除。
- 同一 GitHub 账号下其他 `server_id` 的席位不受影响。退出后不提供公开重新加入接口。

## UTC 日累计快照上报

```mermaid
sequenceDiagram
    participant A as 上报端 A
    participant R as Ranking 服务端（席位 42）
    participant B as 上报端 B
    Note over A,B: 各端持独立凭据 — PUT /v1/clients/report/{utc_day}
    A->>R: revision "1"，完整用量 100
    B->>R: revision "1"，完整用量 120
    Note over R: 席位当日生效快照 = 120（最后提交覆盖，不累加）
    A->>R: 原样重发 revision "1"（用量 100）
    R-->>A: 200 {"revision":"1"}（幂等确认，不覆盖 120）
    A->>R: revision "2"，完整用量 110
    Note over R: 席位当日生效快照 = 110
    A->>R: POST /v1/clients/withdraw
    R-->>A: 204（整席退出）
    B->>R: 重发旧快照或新上报
    R-->>B: 403 client_withdrawn（所有端停止上报）
```

### 上报窗口与聚合约定

- `{utc_day}` 须为 **UTC 时区**的 `YYYY-MM-DD`（非客户端本地日期），且仅允许 **UTC 今日或昨日**。未来日期返回 `422 invalid_day`，已封窗的历史日期返回 `410 expired_day`。
- 每次 `PUT` 必须提交该席位、该 UTC 日的**完整累计统计**，而非单条事件或本次增量。客户端重启后须恢复当日完整汇总后再上报。
- 推荐先在本地按标准模型合并来源，每个标准模型汇总为一条记录（旧版 `service_tier` 字段会被静默忽略）。若同一份快照中出现多条指向同一标准模型的明细（含重复名称、前缀差异或别名），服务端会按加法自动合并。
- `ttft_avg_ms` 与 `latency_avg_ms` 须按耗时总和除以各自真实样本数计算（合并分组时按样本加权，切勿对各组均值再求平均）；无样本时均值传 `null`、样本数传 `"0"`，切勿补零。
- 模型名去除最后一个 `/` 之前的前缀后，按**精确、区分大小写**匹配[模型目录](catalogs.zh.md#模型目录)。合法但未匹配的模型会被静默跳过、不计入总量，服务端不返回被忽略列表；任一项格式非法（含未知模型）均会拒绝整份快照。

### 请求结构与校验规则

```http
PUT /v1/clients/report/{utc_day}
Authorization: Bearer <secret>
Content-Type: application/json
```

```json
{
  "schema_version": 1,
  "revision": "1",
  "items": [
    {
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
    }
  ]
}
```

下表给出标准的 13 字段明细口径（推荐用十进制字符串传递 64 位计数以避免前端精度丢失）。为兼容不同客户端实现，服务端同时接受 JSON 整数与十进制字符串形式的计数/修订号，省略的可选计数字段默认为 `0`、省略的均值字段默认为 `null`，并忽略未知扩展字段（含旧版 `service_tier`）；非法 UTF-8、类型错误、负数、整数溢出或尾随多余 JSON 仍会被拒绝。

| 字段 | 层级 | JSON 类型 | 约束说明 |
| --- | --- | --- | --- |
| `schema_version` | 顶层 | 整数 | 固定为 `1` |
| `revision` | 顶层 | 十进制字符串 | 本上报端当日单调递增的 int64（`0` 至 `9223372036854775807`） |
| `items` | 顶层 | 数组 | 完整日快照；空数组 `[]` 清空当日已匹配统计并推进修订号 |
| `model` | 明细项 | 字符串 | 非空合法 UTF-8，≤128 字节，无控制字符，去路径后非空 |
| `total_tokens` | 明细项 | 十进制字符串 | `>= input_tokens + output_tokens`（可包含未归类余量） |
| `input_tokens`<br>`output_tokens` | 明细项 | 十进制字符串 | 非负 int64 |
| `cache_read_tokens`<br>`cache_write_tokens` | 明细项 | 十进制字符串 | `cache_read_tokens + cache_write_tokens <= input_tokens` |
| `reasoning_tokens` | 明细项 | 十进制字符串 | `reasoning_tokens <= output_tokens` |
| `request_count`<br>`success_count` | 明细项 | 十进制字符串 | `success_count <= request_count`；非零用量必须有 `request_count > 0` |
| `ttft_avg_ms`<br>`latency_avg_ms` | 明细项 | 数字或 `null` | 有样本时有限且 `>= 0`；样本数为 `"0"` 时必须为 `null` |
| `ttft_samples`<br>`latency_samples` | 明细项 | 十进制字符串 | 各自 `<= request_count` |

十进制字符串不得带 `+` 号（建议不带前导零）。若因上游统计口径差异导致非负字段之间出现轻微不一致（如 `cache_read_tokens + cache_write_tokens > input_tokens`、`reasoning_tokens > output_tokens`、`input_tokens + output_tokens > total_tokens`、`success_count > request_count`、样本数与均值/请求数不匹配或有正用量但 `request_count` 为 `"0"`），服务端会自动执行下界补齐与安全钳制而不再整包拒收；负数、非有限浮点数以及整数或浮点聚合溢出仍会拒绝整份快照。

### 修订号（`revision`）、幂等与冲突处理

`revision` 是**按上报端 + UTC 日**独立维护的数据修订号（可从 `"0"` 或 `"1"` 起步，允许跳号，次日可重新起始），并非软件版本号。

| 提交情形 | 服务端行为 |
| --- | --- |
| 本端该日首次提交或更高 `revision` | 原子替换整席当日生效快照，返回 `200 {"revision":"<n>"}` |
| 同 `revision` 且与顺序无关的内容摘要一致 | 幂等返回 `200 {"revision":"<n>"}`，不覆盖现有数据，也不重新匹配模型目录 |
| 更低 `revision`，或同 `revision` 但内容不同 | 返回 `409 revision_conflict` 及本端当前十进制字符串 `revision` |

`409 revision_conflict` 会将本端当前 `revision` 放在顶层，与 `error` 并列返回：

```json
{
  "error": {
    "code": "revision_conflict",
    "message": "Report revision conflicts with the writer's current revision"
  },
  "revision": "8"
}
```

**客户端可靠性约定：**
- 发送前先持久化待发的 `{utc_day, revision, payload}`；仅当 HTTP 为 `200` **且**响应中的 `revision` 与本次请求发送的值**数值相等**时才标记成功（例如合法请求 `"001"` 会回执规范形式 `"1"`）。
- 超时、连接中断或响应无法解析不代表写入失败：保留原 `revision` 与完整正文，有界退避重试。
- 更高修订号可以下调统计或删除模型项；`items: []` 会清空当日已匹配统计。**切勿用空数组表示临时采集失败，也不要拆成多次增量 `PUT`。**
- 收到 `409 revision_conflict` 时，先核对返回的 `revision` 与本地状态、排查是否存在并发写入，切勿在无法重建当日完整权威快照时盲目加号覆盖。

## 最小端到端 cURL 示例

针对已[配置 GitHub OAuth 的 HTTPS 服务](deployment.zh.md#github-oauth-app)：

```sh
# 1. 首次生成并持久化 43 字符无填充 base64url 凭据（已存在则直接复用）
SECRET_FILE="$HOME/.config/cpa-ranking/writer.secret"
install -d -m 700 "$(dirname "$SECRET_FILE")"
if [ ! -f "$SECRET_FILE" ]; then
  umask 077 && openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n' > "$SECRET_FILE"
fi
SECRET=$(cat "$SECRET_FILE")
BASE=https://ranking.example.com
SERVER_ID='替换为当前 CPA 实例的 server-id'

# 2. 开始注册（avatar_id 可选，省略则使用目录默认头像）
#    保存返回的 request_id，并在浏览器打开 authorize_url
curl -sS -X POST "$BASE/v1/clients/register/start" \
  -H "Authorization: Bearer $SECRET" \
  -H 'Content-Type: application/json' \
  -d "{
    \"server_id\": \"$SERVER_ID\",
    \"return_to\": \"http://localhost:3001/settings?tab=ranking#status\",
    \"avatar_id\": \"12\"
  }"

# 3. 在 GitHub 同意授权后完成注册；加 -i 以便查看空体 202 + Retry-After 或 200/201
REQUEST_ID='替换为返回的 request_id'
curl -isS -X POST "$BASE/v1/clients/register/complete" \
  -H "Authorization: Bearer $SECRET" \
  -H 'Content-Type: application/json' \
  -d "{\"request_id\":\"$REQUEST_ID\"}"

# 4. 收到 200 或 201 后，上报当前 UTC 日完整累计快照（结构同上节请求示例）
TODAY=$(date -u +%F)
curl -sS -X PUT "$BASE/v1/clients/report/$TODAY" \
  -H "Authorization: Bearer $SECRET" \
  -H 'Content-Type: application/json' \
  -d '{
    "schema_version": 1,
    "revision": "1",
    "items": [{
      "model": "claude-opus-4-5",
      "total_tokens": "150000", "input_tokens": "100000", "output_tokens": "50000",
      "cache_read_tokens": "60000", "cache_write_tokens": "0", "reasoning_tokens": "10000",
      "request_count": "42", "success_count": "41",
      "ttft_avg_ms": 320.5, "ttft_samples": "41",
      "latency_avg_ms": 1850.0, "latency_samples": "41"
    }]
  }'

# 5. 读取公开综合总榜（无需凭据）
curl -sS "$BASE/v1/leaderboards/overall/today"
```

## 读取公开榜单与头像

### 公开榜单接口

```http
GET /v1/leaderboards/{board}/{period}
```

- `board`：`overall`、`usage`、`cache`、`success`、`ttft`
- `period`：`today`、`yesterday`、`week`（上一完整 UTC 自然周，周一起）、`month`（上一完整 UTC 自然月）、`year`（当前 UTC 自然年）、`all`（累计）
- 附加 URL 查询参数会被忽略（不提供分页或模型筛选）。
- `client_id`、`avatar_id`、`metric` 均为十进制字符串（用量为整数，其余保留两位小数）；`rank` 为 JSON 整数，采用竞争排名（`1, 1, 3`）。仅 `overall` 条目附带 `details` 对象（`total_tokens`、`request_count`、`success_rate`、`cache_read_rate`、`ttft_mean_ms`、`latency_mean_ms`）。
- 完整响应格式、五维评分公式与前端格式化规则见[排行榜与评分规则](leaderboards.zh.md)。

### 头像清单与图片

- `GET /avatars/catalog.json` 返回 `{"default_avatar_id": 1, "avatars": [{"avatar_id": 1, "path": "images/001.webp"}, ...]}`。客户端可据此渲染注册头像选择器并默认选中 `default_avatar_id`；清单中 `avatar_id` 为 JSON 整数，`POST /v1/clients/register/start` 同时接受十进制字符串（推荐，与注册、榜单响应一致）与正 JSON 整数。
- 将 `path` 相对 `/avatars/` 解析（例如 `/avatars/images/001.webp`），通过 `GET` 获取 `image/webp` 二进制图片或 `HEAD` 检查响应头，切勿按 ID 自行拼接路径。清单和图片均支持 `ETag` 条件请求（`304 Not Modified`）。详见[模型、头像与品牌资源目录](catalogs.zh.md#头像目录)。

### 完成标志与非保证边界

| 阶段 | 完成标志 | **不代表**什么 |
| --- | --- | --- |
| 开始注册 | 收到 `200`（含 `request_id` 与 `authorize_url`） | 已建立席位或具备上报权限 |
| 完成注册 | 收到 `201` 或 `200` | 已提交用量 |
| 日快照确认 | 收到 `200` 且回执 `revision` 与本次请求数值相等 | 已立即入榜或缓存已刷新 |
| 退出席位 | 收到 `204` | 旧榜单缓存已立即替换 |
| 读取榜单 | `200` 返回 `entries`、`start`/`end`、`generated_at` | 必然非空或缓存绝对最新 |

普通上报不触发立即重建缓存（默认每 `5m` 定时构建一轮；退出席位会触发异步刷新）。轮询时应展示 `generated_at`，不要用榜单接口代替写入确认。

## 错误码、重试策略与默认限流

业务接口错误统一返回如下 JSON 信封。请按 HTTP 状态码与 `error.code` 分支处理，勿匹配 `message` 文本；反向代理（以及 `/readyz`）可能返回其他格式，非 `200` 响应均不可视作成功确认。

```json
{"error": {"code": "report_rate_limited", "message": "Report admission limit reached"}}
```

| 状态码 | `error.code` | 客户端处理建议 |
| --- | --- | --- |
| `202` | —（含 `Retry-After` 头） | 等待浏览器授权；按间隔用原 `request_id` 与原 Bearer 重试 `complete` |
| `400` / `422` | `invalid_request`、`invalid_source`<br>`invalid_parameter`、`invalid_day`<br>`invalid_report`、`too_many_items` | 修正参数或正文，不要原样无限重试 |
| `422` | `invalid_avatar` | 所选 `avatar_id` 不在 `/avatars/catalog.json` 目录中，刷新目录后重新选择 |
| `401` | `invalid_credential`<br>（含 `WWW-Authenticate: Bearer`） | 检查入口、凭据与注册状态，不要自动重生成新凭据 |
| `403` | `github_authorization_denied` | 本次 GitHub 授权未获批准（如用户拒绝或 OAuth 应用受限），停止流程且不自动再次拉起授权 |
| `403` | `client_disabled` | 席位已被管理员封禁，停止上报并联系部署者 |
| `403` | `client_withdrawn` | 整席已退出，所有上报端停止注册重试与上报 |
| `404` / `405` | `not_found`、`method_not_allowed` | 检查路径与 HTTP 方法 |
| `409` | `revision_conflict`（附带 `revision`） | 核对本地持久化状态与并发写入后再修正 |
| `410` | `registration_request_expired` | 请求已过期或终态记录被提前回收；保留原凭据，如需继续参榜可直接重新开始注册并授权（无需等满 10 分钟） |
| `410` | `expired_day` | 停止补报该日期，不得把数据挪到其他日期 |
| `413` | `request_too_large` | 精简正文体积，不得拆成增量 `PUT` |
| `415` | `unsupported_media_type` | 改用 `application/json` 及 `identity` 或 `gzip` 编码 |
| `429` | `registration_rate_limited`<br>`registration_capacity`<br>`report_rate_limited`、`report_capacity` | 至少等待 `Retry-After` 秒并叠加随机抖动后重试 |
| `503` | `report_overloaded` | 保留原 `revision` 与正文，遵守 `Retry-After` 指数退避重试 |
| `503` | `registration_closed` | 暂停新注册；不影响已有端上报，也不影响未过期注册请求的完成恢复 |
| `503` | `leaderboard_unavailable` | 首轮缓存尚未就绪，延迟重试，不要伪装为空榜 |
| `500` / 网络 | `internal_error` | 有界退避加抖动（保留凭据 / `revision` 与正文）；若为 GitHub 换码失败，待恢复后用原 Bearer 重新生成 `request_id` |

### 默认限流与接口边界

可配置项可通过[环境变量](deployment.zh.md#环境变量参考)调整，注册事务上限与有效期为服务端固定常量；以下均不构成容量保证：

| 限制项 | 默认值 | 是否可配置 |
| --- | --- | --- |
| 注册请求体上限 | `4,096` 字节 | 可配置 |
| 开始注册请求额度 | 每来源 `20` 次、全局 `200` 次 / `1m` 窗口；最多 `10,000` 个活跃来源 | 可配置 |
| 注册事务状态 | 最长 `10m` 有效；全站最多保留 `2,000` 笔未过期事务（容量满时，`denied`、`failed` 或最早的 `completed` 终态记录可能被提前回收，之后 `complete` 返回 `410 registration_request_expired`） | 固定常量 |
| 上报正文与条目上限 | `131,072` 字节；`1,024` 条原始模型项（匹配目录前） | 可配置 |
| 单席位上报额度（同席各端共用） | 每 `10s` 补充 `1` 个令牌，突发 `10`（单席限流拒绝不消耗全局额度） | 可配置 |
| 全局上报额度 | 每 `5ms` 补充 `1` 个令牌，突发 `400` | 可配置 |

**协议边界：**
- 每个上报端应串行调度请求并持久化待确认任务，避免进程重启丢失凭据、修订号或当日汇总。
- 当前不提供凭据找回/轮换、资料修改、恢复退出席位、席位合并、单客户端统计查询、已提交快照回查、过期日期修复、逐模型或分页查询接口；服务端未内建 CORS，如需浏览器跨域调用由部署者在反向代理层配置，更推荐由服务端或本地受信任进程上报。
- `/healthz` 与 `/readyz` 供独立运维监控使用，不属于客户端接入流程。
