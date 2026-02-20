# claude-nvidia-proxy (Go)

Go https://build.nvidia.com/explore/discover, register an account, and generate an API key.
NVIDIA provides several models, like moonshotai/kimi-k2.5、z-ai/glm4.7、z-ai/glm5 and minimaxai/minimax-m2.1
Then, configure the config.json file and run the program, ensuring it listens on port 3001.

Expose `POST /v1/messages` (Anthropic/Claude style), convert to OpenAI Chat Completions, and proxy to NVIDIA (configured via `config.json`).

## Requirements

- **Go 1.22 or later** - This proxy uses Go 1.22+ method-aware routing syntax. If you use Go 1.21 or earlier, the program will exit with an error message.

## Config

Edit `config.json`:

- `nvidia_url` default `https://integrate.api.nvidia.com/v1/chat/completions`
- `nvidia_key` (deprecated, use `nvidia_keys`): single API key for upstream auth
- `nvidia_keys` array of API keys for rotation and failover (recommended)
- `key_rotation` strategy: `round_robin` (default), `random`, or `least_used`

Example with multiple keys:
```json
{
  "nvidia_url": "https://integrate.api.nvidia.com/v1/chat/completions",
  "nvidia_keys": [
    "nvapi-key1",
    "nvapi-key2",
    "nvapi-key3"
  ],
  "key_rotation": "round_robin"
}
```

Do not commit your real keys.

## Env (optional overrides)

- `CONFIG_PATH` default `config.json` (relative to `go/`)
- `PROVIDER_API_KEY` optional: overrides `nvidia_key` from config (single key)
- `PROVIDER_API_KEYS` optional: comma-separated multiple keys for rotation
- `KEY_ROTATION` optional: key rotation strategy (`round_robin`, `random`, `least_used`)
- `UPSTREAM_URL` optional: overrides `nvidia_url` from config
- `SERVER_API_KEY` optional: enable inbound auth; accepts `Authorization: Bearer ...` or `x-api-key: ...`
- `ADDR` default `:3001`
- `UPSTREAM_TIMEOUT_SECONDS` default `300`
- `LOG_BODY_MAX_CHARS` default `4096` (`0` disables body logging)
- `LOG_STREAM_TEXT_PREVIEW_CHARS` default `256` (`0` disables stream preview logging)

## Run

```bash
go run .
```

## CLAUDE CODE

use zai/glm5 model
```bash
export ANTHROPIC_BASE_URL=http://localhost:3001
export ANTHROPIC_AUTH_TOKEN=nvapi-api-key
export ANTHROPIC_DEFAULT_HAIKU_MODEL=z-ai/glm5
export ANTHROPIC_DEFAULT_SONNET_MODEL=z-ai/glm5
export ANTHROPIC_DEFAULT_OPUS_MODEL=z-ai/glm5

claude
```

use moonshotai/kimi-k2.5 model
```
export ANTHROPIC_BASE_URL=http://localhost:3001
export ANTHROPIC_AUTH_TOKEN=nvapi-api-key
export ANTHROPIC_DEFAULT_HAIKU_MODEL=moonshotai/kimi-k2.5
export ANTHROPIC_DEFAULT_SONNET_MODEL=moonshotai/kimi-k2.5
export ANTHROPIC_DEFAULT_OPUS_MODEL=moonshotai/kimi-k2.5
```

use zai/glm4.7 model
```bash
export ANTHROPIC_BASE_URL=http://localhost:3001
export ANTHROPIC_AUTH_TOKEN=nvapi-api-key
export ANTHROPIC_DEFAULT_HAIKU_MODEL=z-ai/glm4.7
export ANTHROPIC_DEFAULT_SONNET_MODEL=z-ai/glm4.7
export ANTHROPIC_DEFAULT_OPUS_MODEL=z-ai/glm4.7

claude
```

use zai/glm4.7 model
```bash
export ANTHROPIC_BASE_URL=http://localhost:3001
export ANTHROPIC_AUTH_TOKEN=nvapi-api-key
export ANTHROPIC_DEFAULT_HAIKU_MODEL=minimaxai/minimax-m2.1
export ANTHROPIC_DEFAULT_SONNET_MODEL=minimaxai/minimax-m2.1
export ANTHROPIC_DEFAULT_OPUS_MODEL=minimaxai/minimax-m2.1

claude
```

## API

### POST /v1/messages

- Inbound auth:
  - If `SERVER_API_KEY` is set, you must send `Authorization: Bearer <SERVER_API_KEY>` (or `x-api-key: <SERVER_API_KEY>`).
- Upstream auth:
  - Always sends `Authorization: Bearer <nvidia_key>` to NVIDIA.

Example (non-stream):

```bash
curl -sS http://127.0.0.1:3001/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"z-ai/glm4.7",
    "max_tokens":256,
    "messages":[{"role":"user","content":"hello"}]
  }'
```

Example (stream):

```bash
curl -N http://127.0.0.1:3001/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"z-ai/glm4.7",
    "max_tokens":256,
    "stream":true,
    "messages":[{"role":"user","content":"hello"}]
  }'
```

## Build

This project uses only Go stdlib (no external deps). If your environment blocks the default Go build cache path, set:

```bash
export GOCACHE=/tmp/go-build-cache
export GOMODCACHE=/tmp/gomodcache
```

Linux (amd64):

```bash
mkdir -p dist
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/claude-nvidia-proxy_linux_amd64 .
```

Windows (amd64):

```bash
mkdir -p dist
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/claude-nvidia-proxy_windows_amd64.exe .
```

## API

### POST /v1/messages

- Inbound auth:
  - If `SERVER_API_KEY` is set, you must send `Authorization: Bearer <SERVER_API_KEY>` (or `x-api-key: <SERVER_API_KEY>`).
- Upstream auth:
  - Always sends `Authorization: Bearer <nvidia_key>` to NVIDIA.

Example (non-stream):

```bash
curl -sS http://127.0.0.1:3001/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"z-ai/glm4.7",
    "max_tokens":256,
    "messages":[{"role":"user","content":"hello"}]
  }'
```

Example (stream):

```bash
curl -N http://127.0.0.1:3001/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{
    "model":"z-ai/glm4.7",
    "max_tokens":256,
    "stream":true,
    "messages":[{"role":"user","content":"hello"}]
  }'
```

## Notes / Limitations

- Streaming conversion supports `delta.content` text and `delta.tool_calls` tool-use blocks; other Anthropic blocks are not fully implemented.
- Logs show forwarded request bodies; keep `LOG_BODY_MAX_CHARS` small and avoid secrets in prompts.

## Rate Limiting & Key Rotation

The proxy supports multiple API keys with automatic rotation and 429 (rate limit) retry:

- **Multiple Keys**: Configure `nvidia_keys` array or `PROVIDER_API_KEYS` env (comma-separated)
- **Rotation Strategies**:
  - `round_robin`: Keys are used in sequence (default)
  - `random`: Random key selection for each request
  - `least_used`: Select the key with the fewest active requests
- **429 Retry**: When a request returns HTTP 429, the proxy automatically retries with the next key (up to 5 attempts)

Example with environment variables:
```bash
export PROVIDER_API_KEYS="nvapi-key1,nvapi-key2,nvapi-key3"
export KEY_ROTATION="round_robin"
go run .
```
