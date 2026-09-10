# traeopenai

`traeopenai` 是一个使用 Go 标准库实现的轻量代理，将 Trae 企业版后端转换为 OpenAI 兼容 API，可供 ChatBox、NextChat 等客户端调用。

支持：

- `GET /v1/models`
- `POST /v1/chat/completions`
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

调用示例：

```bash
curl http://127.0.0.1:8686/v1/chat/completions \
  -H 'Authorization: Bearer sk-your-key' \
  -H 'Content-Type: application/json' \
  -d '{"model":"kimi-k2.6","messages":[{"role":"user","content":"你好"}]}'
```

可先通过 `/v1/models` 查询当前账号可用的模型。

## Docker

```bash
docker build -t traeopenai .
docker run --rm -p 8686:8686 \
  -e TRAE_PAT=trae-lt-... \
  -e TRAE_PROXY_API_KEY=sk-your-key \
  -v traeopenai-data:/data \
  traeopenai
```

## 配置

| 环境变量 | 说明 | 默认值 |
| --- | --- | --- |
| `TRAE_PAT` | Trae 个人访问令牌 | 无 |
| `TRAE_PROXY_API_KEY` | 客户端调用代理时使用的 API Key | 自动生成 |
| `TRAE_LISTEN` | 服务监听地址 | `127.0.0.1:8686` |
| `TRAE_HOST` | Trae 后端地址 | `https://api.enterprise.trae.cn` |
| `TRAE_STATE_FILE` | Token 状态文件路径 | `~/.trae-openai-state.json` |
| `TRAE_IDE_VERSION_CODE` | 提供给 Trae 后端的客户端版本号（后端会拒绝过旧版本） | 自动识别：优先读本机 traecli 二进制里的真实常量，读不到则用内置已验证值 `20260206` |
| `TRAE_CLI_BIN` | 指定用于识别版本号的 traecli 二进制路径 | 自动在 `PATH`、`~/.local/bin`、`~/.trae/bin` 等处查找 |
| `TRAE_ALLOW_VERSION_FALLBACK` | 设为 `1` 时允许版本被拒后用合成版本号重试一次（应急，见 DESIGN.md 坑 6） | 关闭 |
| `TRAE_DEBUG_SSE` | 设为 `1` 时打印上游 SSE 事件与外发请求，便于排障 | 关闭 |

请妥善保管 PAT、API Key 和状态文件，不要提交到 Git 仓库。使用本项目时请遵守 Trae 的服务条款。

## 排障

上游拒绝请求（客户端版本过旧、凭据失效、配额不足）时会以 HTTP 502 返回真实原因：

```json
{"error": {"message": "upstream error (code 1001): We're sorry, but we are not able to authenticate you.", "type": "upstream_error"}}
```

若已在流式输出中途失败，则保留已输出的部分并将 `finish_reason` 置为 `length`。需要更细的链路日志时用 `TRAE_DEBUG_SSE=1` 启动，会打印每条上游 SSE 事件和外发 payload。

## License

[MIT](LICENSE)
