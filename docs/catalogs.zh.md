# 模型、头像与品牌资源目录

[English](catalogs.md) · [返回 README](../README.zh.md)

本文说明服务端启动加载的模型目录、内置静态头像目录以及项目品牌图标资源。

## 模型目录

服务端在打开 SQLite 或监听 HTTP 前，一次性加载 `config/models.json`（或 `RANKING_MODELS_PATH` 指定的文件）。缺失、格式非法或名称冲突都会阻止启动；修改后须重启生效（不支持远程拉取或热更新）。即使以纯 API 模式运行，也须随二进制一起部署 `models.json`。

### 解析与存储规则

- 启动时在单事务内将全部标准 `model_name` 注册到 SQLite `models` 字典表，分配稳定正整数 ID（ID `0` 保留给内部全模型总计），并保留标准名原始大小写用于落盘与展示。移除模型后历史字典行保留但不参与活跃匹配，重新加入时恢复原 ID。别名与价格不写入数据库。
- 目录查找索引统一按 **ASCII 小写**构建；加载时严格拒绝跨模型的大小写名称/别名冲突。
- 处理[日累计快照上报](integration.zh.md#utc-日累计快照上报)时，解析器先清理首尾空白并剥离最后一个 `/` 之前的路径前缀，再仅对最终模型段校验非空、合法 UTF-8、`≤128` 字节且无控制字符。匹配时先按不区分 ASCII 大小写查找标准名与别名；若未命中，则按受控白名单最多剥离两层末尾网关模式/渠道后缀后回退匹配：
  - 冒号渠道后缀：`:free`、`:thinking`、`:online`、`:nitro`、`:floor`、`:extended`、`:beta`、`:exacto`、`:batch`
  - 方括号上下文后缀：`[1m]`、`[2m]`
  - 圆括号思考预算后缀：`(minimal)`、`(low)`、`(medium)`、`(high)`、`(xhigh)`、`(max)`、`(none)`、`(off)`、`(auto)` 或 `(<1..8 位纯数字>)`
  - 连字符思考/分层后缀：`-minimal`、`-low`、`-medium`、`-high`、`-xhigh`、`-thinking`、`-nothinking`、`-non-thinking`、`-tiered`
  - **严禁自动剥离**日期后缀、`-max` 与 `-latest`；官方日期快照、跨渠道点号/连字符变体以及 Gemini 思考强度别名（如 `gemini-4-carbon-high`）均由 `models.json` 的 `alias` 清单显式维护。
- 命中的名称与别名归并到对应标准模型 ID；未命中或单条格式异常的模型名会在聚合阶段静默跳过，不会连坐拒收同一快照中的合法模型，也不会写入 SQLite。当前不提供模型管理 HTTP API。

### 文件格式与参考价格

- 顶层字段：可选参考元数据 `currency`（默认 `"USD"`）、`price_unit`（默认 `"per_million_tokens"`）以及必填 `models`（每项包含必填 `model_name` 与可选 `alias`、`prices`）。
- 模型名与别名全局无大小写冲突，别名不能占用其他模型的标准名或别名。
- `prices` 可选地将 `default`、`priority`、`flex` 映射为 `{input, output, cache_read, cache_write}` 非负十进制字符串（缺失或 `null` 表示未知，`"0"` 表示显式免费；缺失的档位不继承 `default`；未知档位或 `null` 档位会被忽略）。
- 价格档位仅作目录元数据参考，**不是**上报或统计维度（日快照中若携带 `service_tier` 会被静默忽略），服务端不执行计费或成本估算。
- `config/model-sources.json` 记录随库发布参考价格的来源链接、定价口径（凡厂商发布过官方标准美元单价——含官方已下架但历史公布过的官方标准价——一律采用厂商官方价而不使用第三方转售平台价；排除各厂商限时促销优惠价；分时长 `cache_write` 取短周期 TTL；仅在厂商从未公布美元按量标准价时参考 `models.dev` / `litellm`）与采集日期；运行时仅加载 `config/models.json`。

## 头像目录

服务端二进制内置 `internal/avatars/assets/catalog.json` 与 66 张 `128×128` WebP 静态头像（`images/001.webp`–`images/066.webp`，默认 ID 为 `1`，来源说明见 [PROVENANCE.md](../internal/avatars/assets/PROVENANCE.md)）。运行时无需外部目录；客户端可读取 `/avatars/catalog.json` 渲染注册头像选择器，并在 [`POST /v1/clients/register/start`](integration.zh.md#第一步开始注册start) 提交可选的十进制字符串 `avatar_id`（重新授权时若用户未主动修改头像请省略 `avatar_id`，以免覆盖老席位已有头像）；不支持自定义上传或独立修改头像接口。

### 公开 HTTP 接口

- `GET` / `HEAD` / `OPTIONS /avatars/catalog.json`：返回 `{"default_avatar_id": 1, "avatars": [{"avatar_id": 1, "path": "images/001.webp"}, ...]}`，带 `Cache-Control: no-cache`、内容 `ETag`、公开只读 CORS 响应头与 `Cross-Origin-Resource-Policy: cross-origin`。
- `GET` / `HEAD` / `OPTIONS /avatars/{path}`：返回 WebP 图片二进制，带 `Cache-Control: public, max-age=3600, must-revalidate`、`ETag`、条件请求 `304 Not Modified`、公开只读 CORS 响应头与 `Cross-Origin-Resource-Policy: cross-origin`。
- 仅暴露清单白名单内的文件；目录、来源文件、未知路径及路径穿越均返回 JSON `404`。`/assets/` 为前端独立静态命名空间。

### 清单格式与扩展约定

- `avatars` 不能为空；每个 `avatar_id` 须为唯一正 `int64`（为兼容前端须不超过 JS 安全整数 `9007199254740991`）；`default_avatar_id` 必须存在于列表中。
- `path` 须为匹配 `^images/[a-zA-Z0-9_./-]+$` 的相对普通文件路径，不含 `.` 或 `..`。
- **维护约定**：扩展头像时直接追加文件与条目并重新构建。切勿对已发布 ID 重新编号、将旧 ID 改指其他图片，或删除仍被数据库席位引用的文件。

## 品牌图标资源

品牌标志由暖色至紫色的圆角底块、三阶珊瑚色阶梯与角落紫点组成（纯视觉标志，非实时状态灯）。

- **README 横幅**：`assets/ranking-logo-light.svg`、`assets/ranking-logo-dark.svg`
- **Web 与图标源文件**：`assets/ranking-logo.svg`、`web/public/assets/brand/logo.svg`、`web/public/assets/brand/project-icon.svg`、`web/public/favicon.svg`
- **导出的位图资源**：`web/public/assets/brand/project-icon-{180,192,512}.png`、`web/public/assets/brand/favicon-{16,32,48}.png`、`web/public/assets/brand/favicon.ico`

位图产物已随 SVG 源文件一同提交，常规构建无需浏览器环境。修改 SVG 源文件后，在具备 Playwright Chromium 的环境中执行以下命令重生成：

```sh
npm --prefix web run icons:generate
```
