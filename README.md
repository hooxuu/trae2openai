# traeopenai

`traeopenai` 是一个使用 Go 标准库实现的轻量代理，将 Trae 企业版后端转换为 OpenAI 兼容 API，可供 ChatBox、NextChat 等客户端调用。

支持：

- `GET /v1/models`
- `POST /v1/chat/completions`
- `POST /v1/responses`（OpenAI 新版 Responses API，Codex CLI / openai SDK 等客户端使用）
- 流式响应、工具调用与 Token 自动续期
- API Key 鉴权

## 快速开始

需要 Go 1.22+ 和有效的 Trae 个人访问令牌（PAT）。

```bash
TRAE_PAT=trae-lt-... go run .
```

服务默认监听 `127.0.0.1:8686`。首次启动会在日志中输出自动生成的客户端 API Key，也可以自行指定：

```bash
TRAE_PAT=trae-lt-... \
TRAE_PROXY_API_KEY=sk-your-key \
TRAE_LISTEN=127.0.0.1:8686 \
go run .
```

调用示例（模型名用 `GET /v1/models` 查当前可用列表）：

```bash
curl http://127.0.0.1:8686/v1/chat/completions \
  -H 'Authorization: Bearer sk-your-key' \
  -H 'Content-Type: application/json' \
  -d '{"model":"kimi-k3","messages":[{"role":"user","content":"你好"}]}'
```

### Responses API（`/v1/responses`）

新版 OpenAI Responses 端点同样支持，非流式与流式皆可。适配层把 `instructions` 与
`input[]`（message / function_call / function_call_output）折叠成内部 chat 消息发给
Trae 后端，再把 chat 输出重新包装成 `response.*` 事件序列（流式）或单个 `response`
对象（非流式）。reasoning 内容映射为 message 项里的 `summary_text` 部分。

```bash
curl http://127.0.0.1:8686/v1/responses \
  -H 'Authorization: Bearer sk-your-key' \
  -H 'Content-Type: application/json' \
  -d '{"model":"kimi-k3","instructions":"You are terse.","input":"你好"}'
```

说明（有意为之的取舍）：

- **无状态**：`store`、`previous_response_id` 被接受但忽略——客户端需每次重发完整历史
  （Codex CLI / openai SDK 默认正是这么工作的）。
- **工具**：仅支持 `function` 类型；`web_search_preview` / `file_search` / `code_interpreter`
  等后端不支持的类型会被静默跳过，不会让请求报错。
- 不支持的字段（`background` / `include` / `metadata` / `truncation` 等）被解析后丢弃，
  不会到达上游。

## Docker 部署

镜像基于 `scratch`（无 shell、无包管理器，只有二进制和 CA 证书），默认监听 `0.0.0.0:8686`，状态文件固定在 `/data`。
因此：

- **必须挂载 `/data`**，否则重建容器就要重新登录（`state.json` 和 `.trae-openai-apikey` 都在里面）；
- **不能**用容器内命令做健康检查，要从外部探测 `/healthz`（k8s 用 `httpGet`，宿主机用 `curl`）。

### 构建

目标机是远端 linux/amd64 而本机是 Apple Silicon 时，用 buildx 交叉构建：

```bash
# 本机构建并加载（部署机与构建机同一台时）
docker buildx build --platform linux/amd64 -t traeopenai:latest --load .

# 构建并推送，供远端服务器拉取
docker buildx build --platform linux/amd64 -t <registry>/traeopenai:latest --push .
```

### 运行

```bash
docker run -d --name traeopenai --restart unless-stopped \
  -p 127.0.0.1:8686:8686 \
  -e TRAECLI_PERSONAL_ACCESS_TOKEN=trae-lt-... \
  -e TRAE_PROXY_API_KEY=sk-your-key \
  -v traeopenai-data:/data \
  traeopenai:latest
```

- `TRAECLI_PERSONAL_ACCESS_TOKEN`：官方 CLI 登录令牌（专为 Docker/CI 场景，见下文「凭据失效」）；等价别名是 `TRAE_PAT`。
- `TRAE_PROXY_API_KEY`：客户端调用本服务用的 `sk-` key；不填则自动生成并写入 `/data/.trae-openai-apikey`（启动日志会打印）。
- `-p 127.0.0.1:8686:8686` 表示只绑定回环、由反向代理对外暴露；需要局域网直连时改成 `-p 8686:8686`。

### docker compose

```yaml
services:
  traeopenai:
    image: traeopenai:latest
    container_name: traeopenai
    restart: unless-stopped
    ports:
      - "127.0.0.1:8686:8686"
    environment:
      TRAECLI_PERSONAL_ACCESS_TOKEN: "trae-lt-..."
      TRAE_PROXY_API_KEY: "sk-your-key"
      # 版本门槛变化时才需要显式指定（见「版本门槛」）
      # TRAE_IDE_VERSION_CODE: "20260206"
      # 排查上游问题时打开，会把每条上游 SSE 事件和外发 payload 写进日志
      # TRAE_DEBUG_SSE: "1"
    volumes:
      - traeopenai-data:/data

volumes:
  traeopenai-data:
```

### 升级

```bash
docker buildx build --platform linux/amd64 -t traeopenai:latest --load .
docker compose up -d --force-recreate   # 或 docker rm -f traeopenai && docker run ...
```

`/data` 里的登录态与客户端 key 会保留，升级不需要重新登录；除非令牌本身失效（见下文）。

### 对外暴露

本服务用 `Authorization: Bearer sk-...` 鉴权，但**明文 HTTP 不提供传输加密**。若像远端服务器那样直接暴露在公网（客户端 `baseUrl` 写 `http://<ip>:8686`），建议：

- 前面加一层 TLS 反向代理（nginx / Caddy），只对外开 80/443，8686 只绑回环；
- 或用安全组 / 防火墙把 8686 限制到可信 IP；
- 客户端 `baseUrl` 相应改成 `https://...`。

## 配置

| 环境变量 | 说明 | 默认值 |
| --- | --- | --- |
| `TRAE_PAT` | Trae 个人访问令牌（官方名 `TRAECLI_PERSONAL_ACCESS_TOKEN` 也接受，`TRAE_PAT` 优先） | 无 |
| `TRAE_PROXY_API_KEY` | 客户端调用代理时使用的 API Key | 自动生成 |
| `TRAE_LISTEN` | 服务监听地址 | `127.0.0.1:8686`（Docker 镜像内为 `0.0.0.0:8686`） |
| `TRAE_HOST` | Trae 后端地址 | `https://api.enterprise.trae.cn` |
| `TRAE_STATE_FILE` | Token 状态文件路径 | `~/.trae-openai-state.json`（Docker 镜像内为 `/data/state.json`） |
| `TRAE_IDE_VERSION_CODE` | 提供给 Trae 后端的客户端版本号（后端会拒绝过旧版本，见 DESIGN.md 坑 6） | 内置已验证值 `20260206`（官方客户端当前发送的值） |
| `TRAE_ALLOW_VERSION_FALLBACK` | 设为 `1` 时允许版本被拒后用合成版本号重试一次（应急，见 DESIGN.md 坑 6） | 关闭 |
| `TRAE_DEBUG_SSE` | 设为 `1` 时打印上游 SSE 事件与外发请求，便于排障 | 关闭 |

请妥善保管 PAT、API Key 和状态文件，不要提交到 Git 仓库。使用本项目时请遵守 Trae 的服务条款。

## 排障

上游拒绝请求（客户端版本过旧、凭据失效、配额不足）时会以 HTTP 502 返回真实原因：

```json
{"error": {"message": "upstream error (code 1001): We're sorry, but we are not able to authenticate you.", "type": "upstream_error"}}
```

若已在流式输出中途失败，则保留已输出的部分并将 `finish_reason` 置为 `length`。需要更细的链路日志时用 `TRAE_DEBUG_SSE=1` 启动，会打印每条上游 SSE 事件和外发 payload。

### 版本门槛（容器部署尤其注意）

Trae 会拒绝过旧的客户端版本：聊天返回 `event: error {"code":4120}`（adapter 转成 HTTP 502），模型目录返回 `code 2001`。这个版本号是**协议代号而不是构建日期**（官方 CLI 0.120.52 至今仍发送 `20260206`），所以容器里用 `-e TRAE_IDE_VERSION_CODE=...` 显式指定即可，不需要在容器里安装 CLI。真的被拒时 adapter 会明确报出上游错误，不会静默退化成空响应；应急可加 `-e TRAE_ALLOW_VERSION_FALLBACK=1` 让它在被拒后用合成值重试一次。

### 凭据失效（远程/容器部署最常见）

报 `upstream error (code 1001): ... not able to authenticate you.` 表示登录令牌被吊销或过期。按官方流程在 Trae 网页 **个人信息 > 访问令牌 > CLI 登录令牌** 重新生成（专为 Docker/CI 场景设计，无需浏览器登录），再把 `TRAECLI_PERSONAL_ACCESS_TOKEN` 传给容器。文档：https://docs.trae.cn/cli_login-token

### 令牌被拒：`code 30021` / `code 30022 "网络异常"`

**这不是网络问题**——Trae 对“刷新令牌无效 / 被吊销 / 租户或环境不匹配”统一返回这句误导性文案。adapter 会把它原样透出并附处理指引：

```
code=30022 message=网络异常 (not a network problem despite the wording: Trae rejects the
refresh token - revoked, expired or wrong tenant. Regenerate the CLI login token: ...)
```

处理：重新生成 CLI 登录令牌（见上），替换 `TRAECLI_PERSONAL_ACCESS_TOKEN`（或别名 `TRAE_PAT`）。另外确认令牌完整包含开头的 `trae-lt-`，且与 `TRAE_HOST` 属于同一环境/租户。

启动时拿不到令牌**不会直接退出**：进程继续提供服务，`/healthz` 正常，`/v1/models` 与对话接口返回 502 并说明原因，同时后台每 30 秒重试换取令牌。这样容器不会变成重启循环，错误也不会被重启日志冲掉。

## License

[MIT](LICENSE)
