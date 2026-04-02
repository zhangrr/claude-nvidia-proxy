[English](./README.md) | 简体中文

# claude-nvidia-proxy (Go)

访问 https://build.nvidia.com/explore/discover，注册账号并生成 API 密钥。NVIDIA 提供多种模型，如 moonshotai/kimi-k2.5、z-ai/glm4.7、z-ai/glm5 和 minimaxai/minimax-m2.5

然后，配置 config.json 文件并运行程序，确保它监听 3001 端口。

暴露 `POST /v1/messages`（Anthropic/Claude 风格），转换为 OpenAI Chat Completions 并代理到 NVIDIA（通过 `config.json` 配置）。

## 配置

编辑 `config.json`：

- `nvidia_url` 默认 `https://integrate.api.nvidia.com/v1/chat/completions`
- `nvidia_key` 必填：用于上游认证，发送为 `Authorization: Bearer ...`

不要提交真实的 `nvidia_key`。

## 环境变量（可选覆盖）

- `CONFIG_PATH` 默认 `config.json`（相对于 `go/`）
- `PROVIDER_API_KEY` 可选：覆盖配置中的 `nvidia_key`
- `UPSTREAM_URL` 可选：覆盖配置中的 `nvidia_url`
- `SERVER_API_KEY` 可选：启用入站认证；接受 `Authorization: Bearer ...` 或 `x-api-key: ...`
- `ADDR` 默认 `:3001`
- `UPSTREAM_TIMEOUT_SECONDS` 默认 `300`
- `LOG_BODY_MAX_CHARS` 默认 `4096`（`0` 禁用请求体日志）
- `LOG_STREAM_TEXT_PREVIEW_CHARS` 默认 `256`（`0` 禁用流式预览日志）

## 运行

```bash
go run .
```

## CLAUDE CODE

使用 zai/glm5 模型：
```bash
export ANTHROPIC_BASE_URL=http://localhost:3001
export ANTHROPIC_AUTH_TOKEN=nvapi-api-key
export ANTHROPIC_DEFAULT_HAIKU_MODEL=z-ai/glm5
export ANTHROPIC_DEFAULT_SONNET_MODEL=z-ai/glm5
export ANTHROPIC_DEFAULT_OPUS_MODEL=z-ai/glm5

claude
```

使用 moonshotai/kimi-k2.5 模型：
```bash
export ANTHROPIC_BASE_URL=http://localhost:3001
export ANTHROPIC_AUTH_TOKEN=nvapi-api-key
export ANTHROPIC_DEFAULT_HAIKU_MODEL=moonshotai/kimi-k2.5
export ANTHROPIC_DEFAULT_SONNET_MODEL=moonshotai/kimi-k2.5
export ANTHROPIC_DEFAULT_OPUS_MODEL=moonshotai/kimi-k2.5
```

使用 zai/glm4.7 模型：
```bash
export ANTHROPIC_BASE_URL=http://localhost:3001
export ANTHROPIC_AUTH_TOKEN=nvapi-api-key
export ANTHROPIC_DEFAULT_HAIKU_MODEL=z-ai/glm4.7
export ANTHROPIC_DEFAULT_SONNET_MODEL=z-ai/glm4.7
export ANTHROPIC_DEFAULT_OPUS_MODEL=z-ai/glm4.7

claude
```

使用 minimaxai/minimax-m2.5 模型：
```bash
export ANTHROPIC_BASE_URL=http://localhost:3001
export ANTHROPIC_AUTH_TOKEN=nvapi-api-key
export ANTHROPIC_DEFAULT_HAIKU_MODEL=minimaxai/minimax-m2.5
export ANTHROPIC_DEFAULT_SONNET_MODEL=minimaxai/minimax-m2.5
export ANTHROPIC_DEFAULT_OPUS_MODEL=minimaxai/minimax-m2.5

claude
```

## API

### POST /v1/messages

- 入站认证：如果设置了 `SERVER_API_KEY`，必须发送 `Authorization: Bearer <SERVER_API_KEY>`（或 `x-api-key: <SERVER_API_KEY>`）。
- 上游认证：始终发送 `Authorization: Bearer <nvidia_key>` 到 NVIDIA。

非流式示例：

```bash
curl -sS http://127.0.0.1:3001/v1/messages \
 -H 'Content-Type: application/json' \
 -d '{"model":"z-ai/glm4.7","max_tokens":256,"messages":[{"role":"user","content":"hello"}]}'
```

流式示例：

```bash
curl -N http://127.0.0.1:3001/v1/messages \
 -H 'Content-Type: application/json' \
 -d '{"model":"z-ai/glm4.7","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"hello"}]}'
```

## 构建

本项目仅使用 Go 标准库（无外部依赖）。如果环境阻止了默认的 Go 构建缓存路径，请设置：

```bash
export GOCACHE=/tmp/go-build-cache
export GOMODCACHE=/tmp/gomodcache
```

Linux (amd64)：

```bash
mkdir -p dist
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/claude-nvidia-proxy_linux_amd64 .
```

Windows (amd64)：

```bash
mkdir -p dist
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/claude-nvidia-proxy_windows_amd64.exe .
```

## 注意事项 / 限制

- 流式转换支持 `delta.content` 文本块和 `delta.tool_calls` 工具调用块；其他 Anthropic 块尚未完全实现。
- 日志会显示转发的请求体；请将 `LOG_BODY_MAX_CHARS` 设置得小一些，并避免在提示词中包含敏感信息。