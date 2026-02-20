package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type fileConfig struct {
	NvidiaURL   string   `json:"nvidia_url"`
	NvidiaKey   string   `json:"nvidia_key"`   // deprecated: use nvidia_keys
	NvidiaKeys  []string `json:"nvidia_keys"`  // support multiple keys for rotation
	KeyRotation string   `json:"key_rotation"` // round_robin, random, least_used (default: round_robin)
}

type serverConfig struct {
	addr           string
	upstreamURL    string
	providerAPIKey string
	serverAPIKey   string
	timeout        time.Duration
	logBodyMax     int
	logStreamPreviewMax int
	keys           *keyManager
}

type keyManager struct {
	keys       []string
	rotation   string
	currentIdx atomic.Int64
	keyStats   []*keyStat // for least_used strategy
	statsMu    sync.RWMutex
}

type keyStat struct {
	inUse      atomic.Int64
	lastUsed   atomic.Int64 // timestamp
}

func newKeyManager(keys []string, rotation string) *keyManager {
	if len(keys) == 0 {
		return nil
	}
	km := &keyManager{
		keys:     keys,
		rotation: rotation,
		keyStats: make([]*keyStat, len(keys)),
	}
	for i := range km.keyStats {
		km.keyStats[i] = &keyStat{}
	}
	return km
}

func (km *keyManager) getNextKey() string {
	if km == nil || len(km.keys) == 0 {
		return ""
	}

	switch km.rotation {
	case "random":
		idx := rand.Intn(len(km.keys))
		return km.keys[idx]
	case "least_used":
		km.statsMu.Lock()
		defer km.statsMu.Unlock()
		var bestIdx int
		bestInUse := km.keyStats[0].inUse.Load()
		for i := 1; i < len(km.keyStats); i++ {
			inUse := km.keyStats[i].inUse.Load()
			if inUse < bestInUse {
				bestInUse = inUse
				bestIdx = i
			} else if inUse == bestInUse {
				// If tie, choose the one used least recently
				if km.keyStats[i].lastUsed.Load() < km.keyStats[bestIdx].lastUsed.Load() {
					bestIdx = i
				}
			}
		}
		km.keyStats[bestIdx].inUse.Add(1)
		km.keyStats[bestIdx].lastUsed.Store(time.Now().Unix())
		return km.keys[bestIdx]
	default: // round_robin
		idx := km.currentIdx.Add(1) % int64(len(km.keys))
		return km.keys[idx]
	}
}

func (km *keyManager) getKeyCount() int {
	if km == nil {
		return 0
	}
	return len(km.keys)
}

func (km *keyManager) releaseKey(idx int) {
	if km != nil && km.rotation == "least_used" && idx >= 0 && idx < len(km.keyStats) {
		km.keyStats[idx].inUse.Add(-1)
	}
}

func main() {
	checkGoVersion()

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		handleMessages(w, r, cfg)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": "claude-nvidia-proxy",
			"health":  "ok",
		})
	})

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      0, // allow streaming
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("listening on %s", cfg.addr)
	log.Printf("upstream: %s", cfg.upstreamURL)
	if cfg.keys != nil {
		log.Printf("using %d api key(s) with rotation: %s", cfg.keys.getKeyCount(), cfg.keys.rotation)
	}
	if cfg.serverAPIKey != "" {
		log.Printf("inbound auth: enabled")
	} else {
		log.Printf("inbound auth: disabled (SERVER_API_KEY not set)")
	}
	log.Fatal(srv.ListenAndServe())
}

func loadConfig() (*serverConfig, error) {
	fc, err := loadFileConfig(strings.TrimSpace(envOr("CONFIG_PATH", "config.json")))
	if err != nil {
		return nil, err
	}

	addr := strings.TrimSpace(envOr("ADDR", ":3001"))
	upstreamURL := strings.TrimSpace(envOr("UPSTREAM_URL", fc.NvidiaURL))
	serverAPIKey := strings.TrimSpace(envOr("SERVER_API_KEY", ""))

	// Handle multiple keys with rotation
	var keys []string
	keyRotation := strings.TrimSpace(envOr("KEY_ROTATION", fc.KeyRotation))
	if keyRotation == "" {
		keyRotation = "round_robin"
	}

	// Priority: PROVIDER_API_KEYS env > PROVIDER_API_KEY env > nvidia_keys config > nvidia_key config
	if envKeys := strings.TrimSpace(os.Getenv("PROVIDER_API_KEYS")); envKeys != "" {
		// Parse comma-separated keys
		splitKeys := strings.Split(envKeys, ",")
		for _, k := range splitKeys {
			if k = strings.TrimSpace(k); k != "" {
				keys = append(keys, k)
			}
		}
	} else if envKey := strings.TrimSpace(envOr("PROVIDER_API_KEY", "")); envKey != "" {
		keys = append(keys, envKey)
	} else if len(fc.NvidiaKeys) > 0 {
		keys = fc.NvidiaKeys
	} else if fc.NvidiaKey != "" {
		keys = append(keys, fc.NvidiaKey)
	}

	if len(keys) == 0 {
		return nil, errors.New("missing nvidia_key or nvidia_keys in config.json (or PROVIDER_API_KEY/PROVIDER_API_KEYS env)")
	}

	timeout := 5 * time.Minute
	if raw := strings.TrimSpace(envOr("UPSTREAM_TIMEOUT_SECONDS", "")); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds <= 0 {
			return nil, fmt.Errorf("invalid UPSTREAM_TIMEOUT_SECONDS: %q", raw)
		}
		timeout = time.Duration(seconds) * time.Second
	}

	logBodyMax := 4096
	if raw := strings.TrimSpace(envOr("LOG_BODY_MAX_CHARS", "")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LOG_BODY_MAX_CHARS: %q", raw)
		}
		logBodyMax = n
	}

	logStreamPreviewMax := 256
	if raw := strings.TrimSpace(envOr("LOG_STREAM_TEXT_PREVIEW_CHARS", "")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid LOG_STREAM_TEXT_PREVIEW_CHARS: %q", raw)
		}
		logStreamPreviewMax = n
	}

	if upstreamURL == "" {
		return nil, errors.New("missing nvidia_url in config.json (or UPSTREAM_URL)")
	}

	// Validate key rotation strategy
	switch keyRotation {
	case "round_robin", "random", "least_used":
		// valid
	default:
		return nil, fmt.Errorf("invalid key_rotation: %q (must be: round_robin, random, or least_used)", keyRotation)
	}

	return &serverConfig{
		addr:           addr,
		upstreamURL:    upstreamURL,
		providerAPIKey: keys[0], // fallback for compatibility
		serverAPIKey:   serverAPIKey,
		timeout:        timeout,
		logBodyMax:     logBodyMax,
		logStreamPreviewMax: logStreamPreviewMax,
		keys:           newKeyManager(keys, keyRotation),
	}, nil
}

func loadFileConfig(path string) (*fileConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var fc fileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &fc, nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func handleMessages(w http.ResponseWriter, r *http.Request, cfg *serverConfig) {
	reqID := fmt.Sprintf("req_%d", time.Now().UnixNano())
	if cfg.serverAPIKey != "" && !checkInboundAuth(r, cfg.serverAPIKey) {
		log.Printf("[%s] inbound unauthorized", reqID)
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var anthropicReq anthropicMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&anthropicReq); err != nil {
		log.Printf("[%s] invalid inbound json: %v", reqID, err)
		writeJSONError(w, http.StatusBadRequest, "invalid_json")
		return
	}
	if strings.TrimSpace(anthropicReq.Model) == "" {
		log.Printf("[%s] missing model", reqID)
		writeJSONError(w, http.StatusBadRequest, "missing_model")
		return
	}
	if anthropicReq.MaxTokens == 0 {
		// Anthropic requires max_tokens; NVIDIA/OpenAI also expects it. Default conservatively.
		anthropicReq.MaxTokens = 1024
	}

	openaiReq, err := convertAnthropicToOpenAI(&anthropicReq)
	if err != nil {
		log.Printf("[%s] request conversion failed: %v", reqID, err)
		writeJSONError(w, http.StatusBadRequest, "request_conversion_failed")
		return
	}

	logForwardedRequest(reqID, cfg, anthropicReq, openaiReq)

	if anthropicReq.Stream {
		if err := proxyStream(w, r, cfg, reqID, openaiReq); err != nil {
			log.Printf("[%s] stream proxy error: %v", reqID, err)
		}
		return
	}

	openaiRespBody, resp, err := doUpstreamJSON(r.Context(), cfg, openaiReq)
	if err != nil {
		log.Printf("[%s] upstream request failed: %v", reqID, err)
		writeJSONError(w, http.StatusBadGateway, "upstream_request_failed")
		return
	}
	defer resp.Body.Close()
	log.Printf("[%s] upstream status=%d", reqID, resp.StatusCode)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(openaiRespBody)
		logForwardedUpstreamBody(reqID, cfg, openaiRespBody)
		return
	}

	var openaiResp openaiChatCompletionResponse
	if err := json.Unmarshal(openaiRespBody, &openaiResp); err != nil {
		log.Printf("[%s] invalid upstream json: %v", reqID, err)
		logForwardedUpstreamBody(reqID, cfg, openaiRespBody)
		writeJSONError(w, http.StatusBadGateway, "invalid_upstream_json")
		return
	}
	anthropicResp := convertOpenAIToAnthropic(openaiResp)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(anthropicResp)
}

func checkInboundAuth(r *http.Request, expected string) bool {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		got := strings.TrimSpace(auth[len("bearer "):])
		return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
	}
	if got := strings.TrimSpace(r.Header.Get("x-api-key")); got != "" {
		return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
	}
	return false
}

func doUpstreamJSON(ctx context.Context, cfg *serverConfig, openaiReq openaiChatCompletionRequest) ([]byte, *http.Response, error) {
	var lastErr error

	for attempt := 0; attempt < 5; attempt++ {
		apiKey := cfg.providerAPIKey
		keyIdx := -1

		if cfg.keys != nil {
			apiKey = cfg.keys.getNextKey()
			// Find key index for least_used release
			for i, k := range cfg.keys.keys {
				if k == apiKey {
					keyIdx = i
					break
				}
			}
		}

		bodyBytes, err := json.Marshal(openaiReq)
		if err != nil {
			if keyIdx >= 0 {
				cfg.keys.releaseKey(keyIdx)
			}
			return nil, nil, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.upstreamURL, bytes.NewReader(bodyBytes))
		if err != nil {
			if keyIdx >= 0 {
				cfg.keys.releaseKey(keyIdx)
			}
			return nil, nil, err
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)

		client := &http.Client{Timeout: cfg.timeout}
		resp, err := client.Do(req)
		if err != nil {
			if keyIdx >= 0 {
				cfg.keys.releaseKey(keyIdx)
			}
			lastErr = err
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			resp.Body.Close()
			if keyIdx >= 0 {
				cfg.keys.releaseKey(keyIdx)
			}
			return nil, nil, err
		}
		_ = resp.Body.Close()
		// Re-wrap body so caller can optionally read again after status checks.
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		// Check for 429 (Rate Limit) error
		if resp.StatusCode == 429 {
			lastErr = fmt.Errorf("rate limited (429) on attempt %d, trying next key", attempt+1)
			if keyIdx >= 0 {
				cfg.keys.releaseKey(keyIdx)
			}
			// If we have more keys available, try next
			if cfg.keys != nil && len(cfg.keys.keys) > 1 {
				time.Sleep(time.Duration(100+attempt*100) * time.Millisecond) // exponential backoff
				continue
			}
			// If only one key, wait longer and retry
			time.Sleep(time.Duration(1+attempt) * time.Second)
			continue
		}

		// Success or other error, return as-is
		return respBody, resp, nil
	}

	// All attempts failed
	return nil, nil, fmt.Errorf("upstream request failed after 5 attempts: %w", lastErr)
}

func proxyStream(w http.ResponseWriter, r *http.Request, cfg *serverConfig, reqID string, openaiReq openaiChatCompletionRequest) error {
	openaiReq.Stream = true

	var lastErr error
	var upResp *http.Response
	var keyIdx int = -1

	for attempt := 0; attempt < 5; attempt++ {
		apiKey := cfg.providerAPIKey
		keyIdx = -1

		if cfg.keys != nil {
			apiKey = cfg.keys.getNextKey()
			for i, k := range cfg.keys.keys {
				if k == apiKey {
					keyIdx = i
					break
				}
			}
		}

		bodyBytes, err := json.Marshal(openaiReq)
		if err != nil {
			if keyIdx >= 0 {
				cfg.keys.releaseKey(keyIdx)
			}
			return err
		}

		upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, cfg.upstreamURL, bytes.NewReader(bodyBytes))
		if err != nil {
			if keyIdx >= 0 {
				cfg.keys.releaseKey(keyIdx)
			}
			return err
		}
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("Authorization", "Bearer "+apiKey)

		client := &http.Client{Timeout: 0} // streaming: no client timeout
		upRespTemp, err := client.Do(upReq)
		if err != nil {
			if keyIdx >= 0 {
				cfg.keys.releaseKey(keyIdx)
			}
			lastErr = err
			continue
		}

		log.Printf("[%s] upstream status=%d (stream, attempt %d)", reqID, upRespTemp.StatusCode, attempt+1)

		// Check for 429 error
		if upRespTemp.StatusCode == 429 {
			upRespTemp.Body.Close()
			lastErr = fmt.Errorf("rate limited (429) on attempt %d, trying next key", attempt+1)
			if keyIdx >= 0 {
				cfg.keys.releaseKey(keyIdx)
			}
			time.Sleep(time.Duration(100+attempt*100) * time.Millisecond)
			continue
		}

		// Error but not 429 - propagate as-is
		if upRespTemp.StatusCode < 200 || upRespTemp.StatusCode >= 300 {
			raw, _ := io.ReadAll(upRespTemp.Body)
			upRespTemp.Body.Close()
			if keyIdx >= 0 {
				cfg.keys.releaseKey(keyIdx)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(upRespTemp.StatusCode)
			_, _ = w.Write(raw)
			logForwardedUpstreamBody(reqID, cfg, raw)
			return fmt.Errorf("upstream status %d, body=%s", upRespTemp.StatusCode, string(raw))
		}

		// Success - set up for streaming
		upResp = upRespTemp
		break
	}

	if upResp == nil {
		return fmt.Errorf("upstream request failed after 5 attempts: %w", lastErr)
	}

	defer func() {
		upResp.Body.Close()
		if keyIdx >= 0 && cfg.keys.rotation == "least_used" {
			cfg.keys.releaseKey(keyIdx)
		}
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming_not_supported")
		return errors.New("http.Flusher not supported")
	}

	// Minimal OpenAI SSE -> Anthropic SSE conversion (text deltas).
	encoder := func(event string, payload any) error {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(b)); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	messageID := fmt.Sprintf("msg_%d", time.Now().UnixMilli())
	_ = encoder("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         openaiReq.Model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	})

	reader := bufio.NewReader(upResp.Body)
	chunkCount := 0
	textChars := 0
	toolDeltaChunks := 0
	toolArgsChars := 0
	var finishReason string
	var preview strings.Builder
	sawDone := false
	type toolState struct {
		contentBlockIndex int
		id                string
		name              string
	}
	toolStates := map[int]*toolState{}

	nextContentBlockIndex := 0
	currentContentBlockIndex := -1
	currentBlockType := "" // "text" | "tool_use"
	hasTextBlock := false

	assignContentBlockIndex := func() int {
		idx := nextContentBlockIndex
		nextContentBlockIndex++
		return idx
	}

	closeCurrentBlock := func() {
		if currentContentBlockIndex >= 0 {
			_ = encoder("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": currentContentBlockIndex,
			})
			currentContentBlockIndex = -1
			currentBlockType = ""
		}
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			sawDone = true
			break
		}

		var chunk openaiChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		chunkCount++
		delta := chunk.Choices[0].Delta

		// Tool calls: OpenAI streaming sends tool call deltas with partial arguments.
		if len(delta.ToolCalls) > 0 {
			for _, tc := range delta.ToolCalls {
				toolDeltaChunks++
				toolIndex := tc.Index
				if toolIndex < 0 {
					toolIndex = 0
				}
				state := toolStates[toolIndex]

				tcID := strings.TrimSpace(tc.ID)
				if tcID == "" {
					tcID = fmt.Sprintf("call_%d_%d", time.Now().UnixMilli(), toolIndex)
				}
				tcName := strings.TrimSpace(tc.Function.Name)
				if tcName == "" {
					tcName = fmt.Sprintf("tool_%d", toolIndex)
				}

				if state == nil {
					// Close any currently open block (text/tool) before starting a new tool block.
					closeCurrentBlock()
					idx := assignContentBlockIndex()
					state = &toolState{contentBlockIndex: idx, id: tcID, name: tcName}
					toolStates[toolIndex] = state

					_ = encoder("content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": idx,
						"content_block": map[string]any{
							"type":  "tool_use",
							"id":    state.id,
							"name":  state.name,
							"input": map[string]any{},
						},
					})
					currentContentBlockIndex = idx
					currentBlockType = "tool_use"
				} else {
					// Upgrade placeholder id/name if later deltas include them.
					if state.id == "" && tcID != "" {
						state.id = tcID
					}
					if state.name == "" && tcName != "" {
						state.name = tcName
					}
					// Switch current block if needed.
					currentContentBlockIndex = state.contentBlockIndex
					currentBlockType = "tool_use"
				}

				argsPart := tc.Function.Arguments
				if argsPart != "" {
					toolArgsChars += len([]rune(argsPart))
					_ = encoder("content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": state.contentBlockIndex,
						"delta": map[string]any{
							"type":         "input_json_delta",
							"partial_json": argsPart,
						},
					})
				}
			}
		}

		if delta.Content != nil && *delta.Content != "" {
			textChars += len([]rune(*delta.Content))
			if cfg.logStreamPreviewMax > 0 && preview.Len() < cfg.logStreamPreviewMax {
				preview.WriteString(takeFirstRunes(*delta.Content, cfg.logStreamPreviewMax-preview.Len()))
			}
			// If we were in a tool block, close it before starting/continuing text.
			if currentBlockType != "" && currentBlockType != "text" {
				closeCurrentBlock()
			}
			if !hasTextBlock {
				hasTextBlock = true
				idx := assignContentBlockIndex()
				_ = encoder("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]any{
						"type": "text",
						"text": "",
					},
				})
				currentContentBlockIndex = idx
				currentBlockType = "text"
			}
			_ = encoder("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": currentContentBlockIndex,
				"delta": map[string]any{
					"type": "text_delta",
					"text": *delta.Content,
				},
			})
		}

		if chunk.Choices[0].FinishReason != nil {
			finishReason = *chunk.Choices[0].FinishReason
			stopReason := mapFinishReason(*chunk.Choices[0].FinishReason)
			_ = encoder("message_delta", map[string]any{
				"type": "message_delta",
				"delta": map[string]any{
					"stop_reason":   stopReason,
					"stop_sequence": nil,
				},
				"usage": map[string]any{
					"input_tokens":            0,
					"output_tokens":           0,
					"cache_read_input_tokens": 0,
				},
			})
		}
	}

	// Close any open content block (text or tool_use).
	closeCurrentBlock()

	// Ensure message_delta is always emitted before message_stop.
	if finishReason == "" {
		_ = encoder("message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   "end_turn",
				"stop_sequence": nil,
			},
			"usage": map[string]any{
				"input_tokens":            0,
				"output_tokens":           0,
				"cache_read_input_tokens": 0,
			},
		})
	}

	_ = encoder("message_stop", map[string]any{
		"type": "message_stop",
	})
	if cfg.logStreamPreviewMax > 0 {
		log.Printf("[%s] stream summary chunks=%d text_chars=%d tool_delta_chunks=%d tool_args_chars=%d finish_reason=%q saw_done=%v preview=%q", reqID, chunkCount, textChars, toolDeltaChunks, toolArgsChars, finishReason, sawDone, preview.String())
	} else {
		log.Printf("[%s] stream summary chunks=%d text_chars=%d tool_delta_chunks=%d tool_args_chars=%d finish_reason=%q saw_done=%v", reqID, chunkCount, textChars, toolDeltaChunks, toolArgsChars, finishReason, sawDone)
	}
	return nil
}

func writeJSONError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"type":    "proxy_error",
			"code":    code,
			"message": code,
		},
	})
}

// ----------------------
// Anthropic request types
// ----------------------

type anthropicMessageRequest struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature *float64        `json:"temperature,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	System      json.RawMessage `json:"system,omitempty"`
	Messages    []anthropicMsg  `json:"messages"`
	Tools       []anthropicTool `json:"tools,omitempty"`
	ToolChoice  any             `json:"tool_choice,omitempty"`
	Thinking    any             `json:"thinking,omitempty"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type anthropicContentBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// image
	Source *anthropicImageSource `json:"source,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// ----------------------
// OpenAI request types
// ----------------------

type openaiChatCompletionRequest struct {
	Model       string `json:"model"`
	Messages    []any  `json:"messages"`
	MaxTokens   int    `json:"max_tokens,omitempty"`
	Temperature any    `json:"temperature,omitempty"`
	Stream      bool   `json:"stream,omitempty"`
	Tools       []any  `json:"tools,omitempty"`
	ToolChoice  any    `json:"tool_choice,omitempty"`
}

func convertAnthropicToOpenAI(req *anthropicMessageRequest) (openaiChatCompletionRequest, error) {
	var messages []any

	if sys := strings.TrimSpace(extractSystemText(req.System)); sys != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": sys,
		})
	}

	for _, m := range req.Messages {
		role := strings.TrimSpace(m.Role)
		if role == "" {
			continue
		}

		// content can be string or array of blocks
		var asString string
		if err := json.Unmarshal(m.Content, &asString); err == nil {
			messages = append(messages, map[string]any{
				"role":    role,
				"content": asString,
			})
			continue
		}

		var blocks []anthropicContentBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			return openaiChatCompletionRequest{}, fmt.Errorf("invalid message content for role %q", role)
		}

		switch role {
		case "user":
			userMsgs, err := convertAnthropicUserBlocksToOpenAIMessages(blocks)
			if err != nil {
				return openaiChatCompletionRequest{}, err
			}
			messages = append(messages, userMsgs...)
		case "assistant":
			assistantMsg, err := convertAnthropicAssistantBlocksToOpenAIMessage(blocks)
			if err != nil {
				return openaiChatCompletionRequest{}, err
			}
			messages = append(messages, assistantMsg)
		default:
			// pass through unknown roles as string if possible
			text := joinTextBlocks(blocks)
			messages = append(messages, map[string]any{
				"role":    role,
				"content": text,
			})
		}
	}

	out := openaiChatCompletionRequest{
		Model:       req.Model,
		Messages:    messages,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		Stream:      req.Stream,
	}

	if len(req.Tools) > 0 {
		out.Tools = make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			var params any
			if len(t.InputSchema) > 0 {
				_ = json.Unmarshal(t.InputSchema, &params)
			}
			out.Tools = append(out.Tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  params,
				},
			})
		}
	}

	if req.ToolChoice != nil {
		out.ToolChoice = convertToolChoice(req.ToolChoice)
	}

	return out, nil
}

func extractSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return joinTextBlocks(blocks)
	}
	return ""
}

func joinTextBlocks(blocks []anthropicContentBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" && blk.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

func convertAnthropicUserBlocksToOpenAIMessages(blocks []anthropicContentBlock) ([]any, error) {
	var out []any

	// tool_result blocks become separate OpenAI "tool" messages.
	for _, blk := range blocks {
		if blk.Type != "tool_result" || strings.TrimSpace(blk.ToolUseID) == "" {
			continue
		}
		contentStr := ""
		if len(blk.Content) > 0 {
			var s string
			if err := json.Unmarshal(blk.Content, &s); err == nil {
				contentStr = s
			} else {
				contentStr = string(blk.Content)
			}
		}
		out = append(out, map[string]any{
			"role":         "tool",
			"tool_call_id": blk.ToolUseID,
			"content":      contentStr,
		})
	}

	// remaining text/image blocks become a user message
	var parts []any
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			if blk.Text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": blk.Text})
			}
		case "image":
			if blk.Source == nil {
				continue
			}
			url := ""
			switch blk.Source.Type {
			case "base64":
				if blk.Source.MediaType == "" || blk.Source.Data == "" {
					continue
				}
				// Validate base64 to avoid obviously invalid payloads.
				if _, err := base64.StdEncoding.DecodeString(blk.Source.Data); err != nil {
					continue
				}
				url = "data:" + blk.Source.MediaType + ";base64," + blk.Source.Data
			case "url":
				url = blk.Source.URL
			default:
				continue
			}
			if url != "" {
				parts = append(parts, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": url,
					},
				})
			}
		}
	}

	if len(parts) == 0 {
		out = append(out, map[string]any{"role": "user", "content": ""})
		return out, nil
	}
	if len(parts) == 1 {
		if p, ok := parts[0].(map[string]any); ok && p["type"] == "text" {
			if t, ok := p["text"].(string); ok {
				out = append(out, map[string]any{"role": "user", "content": t})
				return out, nil
			}
		}
	}

	out = append(out, map[string]any{
		"role":    "user",
		"content": parts,
	})
	return out, nil
}

func convertAnthropicAssistantBlocksToOpenAIMessage(blocks []anthropicContentBlock) (any, error) {
	text := joinTextBlocks(blocks)

	var toolCalls []any
	for _, blk := range blocks {
		if blk.Type != "tool_use" || strings.TrimSpace(blk.ID) == "" || strings.TrimSpace(blk.Name) == "" {
			continue
		}
		args := "{}"
		if len(blk.Input) > 0 {
			args = string(blk.Input)
		}
		toolCalls = append(toolCalls, map[string]any{
			"id":   blk.ID,
			"type": "function",
			"function": map[string]any{
				"name":      blk.Name,
				"arguments": args,
			},
		})
	}

	msg := map[string]any{
		"role": "assistant",
	}
	if text != "" {
		msg["content"] = text
	} else {
		msg["content"] = nil
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return msg, nil
}

func convertToolChoice(v any) any {
	// Anthropic forms:
	// - {"type":"auto"}
	// - {"type":"tool","name":"my_tool"}
	// - string values (rare)
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	typ, _ := m["type"].(string)
	switch typ {
	case "auto", "none", "required":
		return typ
	case "tool":
		name, _ := m["name"].(string)
		if name == "" {
			return "auto"
		}
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": name,
			},
		}
	default:
		return v
	}
}

// ----------------------
// OpenAI response types
// ----------------------

type openaiChatCompletionResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role      string `json:"role"`
			Content   *string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments any    `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details,omitempty"`
	} `json:"usage,omitempty"`
}

type anthropicMessageResponse struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	Role         string `json:"role"`
	Model        string `json:"model"`
	Content      []any  `json:"content"`
	StopReason   string `json:"stop_reason"`
	StopSequence any    `json:"stop_sequence"`
	Usage        any    `json:"usage"`
}

func convertOpenAIToAnthropic(resp openaiChatCompletionResponse) anthropicMessageResponse {
	content := make([]any, 0, 4)

	var finishReason string
	if len(resp.Choices) > 0 {
		ch := resp.Choices[0]
		finishReason = ch.FinishReason
		if ch.Message.Content != nil && *ch.Message.Content != "" {
			content = append(content, map[string]any{
				"type": "text",
				"text": *ch.Message.Content,
			})
		}
		if len(ch.Message.ToolCalls) > 0 {
			for _, tc := range ch.Message.ToolCalls {
				input := map[string]any{}
				switch v := tc.Function.Arguments.(type) {
				case string:
					_ = json.Unmarshal([]byte(v), &input)
				case map[string]any:
					input = v
				default:
					input = map[string]any{"text": fmt.Sprintf("%v", v)}
				}
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": input,
				})
			}
		}
	}

	inputTokens := 0
	outputTokens := 0
	cacheRead := 0
	if resp.Usage != nil {
		cacheRead = 0
		if resp.Usage.PromptTokensDetails != nil {
			cacheRead = resp.Usage.PromptTokensDetails.CachedTokens
		}
		inputTokens = resp.Usage.PromptTokens - cacheRead
		outputTokens = resp.Usage.CompletionTokens
	}

	return anthropicMessageResponse{
		ID:    resp.ID,
		Type:  "message",
		Role:  "assistant",
		Model: resp.Model,
		Content: content,
		StopReason: mapFinishReason(finishReason),
		StopSequence: nil,
		Usage: map[string]any{
			"input_tokens":           inputTokens,
			"output_tokens":          outputTokens,
			"cache_read_input_tokens": cacheRead,
		},
	}
}

func mapFinishReason(finish string) string {
	switch finish {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "stop_sequence"
	default:
		if finish == "" {
			return "end_turn"
		}
		return "end_turn"
	}
}

// ----------------------
// Streaming chunk types
// ----------------------

type openaiChatCompletionChunk struct {
	Model  string `json:"model,omitempty"`
	Choices []struct {
		Delta struct {
			Content *string `json:"content,omitempty"`
			ToolCalls []struct {
				Index int `json:"index,omitempty"`
				ID    string `json:"id,omitempty"`
				Type  string `json:"type,omitempty"`
				Function struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				} `json:"function,omitempty"`
			} `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason,omitempty"`
	} `json:"choices"`
}

func logForwardedRequest(reqID string, cfg *serverConfig, anthropicReq anthropicMessageRequest, openaiReq openaiChatCompletionRequest) {
	inSummary := map[string]any{
		"model":      anthropicReq.Model,
		"max_tokens": anthropicReq.MaxTokens,
		"stream":     anthropicReq.Stream,
		"messages":   len(anthropicReq.Messages),
		"tools":      len(anthropicReq.Tools),
	}
	log.Printf("[%s] inbound summary=%s", reqID, mustJSONTrunc(inSummary, cfg.logBodyMax))

	out := sanitizeOpenAIRequest(openaiReq)
	log.Printf("[%s] forward url=%s", reqID, cfg.upstreamURL)
	log.Printf("[%s] forward headers=%s", reqID, mustJSONTrunc(map[string]any{
		"Content-Type":  "application/json",
		"Authorization": "Bearer <redacted>",
	}, cfg.logBodyMax))
	log.Printf("[%s] forward body=%s", reqID, mustJSONTrunc(out, cfg.logBodyMax))
}

func logForwardedUpstreamBody(reqID string, cfg *serverConfig, body []byte) {
	if cfg.logBodyMax == 0 {
		return
	}
	s := string(body)
	if len([]rune(s)) > cfg.logBodyMax {
		s = string([]rune(s)[:cfg.logBodyMax]) + "...(truncated)"
	}
	log.Printf("[%s] upstream body=%s", reqID, s)
}

func mustJSONTrunc(v any, maxChars int) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"_error":"json_marshal_failed","detail":%q}`, err.Error())
	}
	s := string(b)
	if maxChars == 0 {
		return "(disabled)"
	}
	if len([]rune(s)) > maxChars {
		return string([]rune(s)[:maxChars]) + "...(truncated)"
	}
	return s
}

func sanitizeOpenAIRequest(req openaiChatCompletionRequest) openaiChatCompletionRequest {
	out := req
	out.Messages = sanitizeOpenAIMessages(req.Messages)
	out.Tools = sanitizeAnySlice(req.Tools)
	return out
}

func sanitizeOpenAIMessages(msgs []any) []any {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		cp := map[string]any{}
		for k, v := range mm {
			cp[k] = v
		}
		if content, ok := cp["content"]; ok {
			cp["content"] = sanitizeMessageContent(content)
		}
		// tool_calls may carry huge arguments; keep but truncate strings.
		if tc, ok := cp["tool_calls"]; ok {
			cp["tool_calls"] = sanitizeAny(tc)
		}
		out = append(out, cp)
	}
	return out
}

func sanitizeMessageContent(content any) any {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := make([]any, 0, len(v))
		for _, p := range v {
			pm, ok := p.(map[string]any)
			if !ok {
				parts = append(parts, p)
				continue
			}
			cp := map[string]any{}
			for k, vv := range pm {
				cp[k] = vv
			}
			if cp["type"] == "image_url" {
				if iu, ok := cp["image_url"].(map[string]any); ok {
					if url, ok := iu["url"].(string); ok && strings.HasPrefix(url, "data:") {
						iu2 := map[string]any{}
						for k, vv := range iu {
							iu2[k] = vv
						}
						iu2["url"] = "data:<redacted>"
						cp["image_url"] = iu2
					}
				}
			}
			parts = append(parts, cp)
		}
		return parts
	default:
		return sanitizeAny(v)
	}
}

func sanitizeAnySlice(v []any) []any {
	if len(v) == 0 {
		return nil
	}
	out := make([]any, 0, len(v))
	for _, it := range v {
		out = append(out, sanitizeAny(it))
	}
	return out
}

func sanitizeAny(v any) any {
	switch t := v.(type) {
	case map[string]any:
		cp := map[string]any{}
		for k, vv := range t {
			cp[k] = sanitizeAny(vv)
		}
		return cp
	case []any:
		return sanitizeAnySlice(t)
	case string:
		// keep strings; truncation is handled at final JSON layer
		return t
	default:
		return v
	}
}

func takeFirstRunes(s string, max int) string {
	if max <= 0 || s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func checkGoVersion() {
	version := runtime.Version()
	// runtime.Version() returns strings like "go1.22.0", "go1.21.0", "go1.20"
	// We need to parse the major and minor version
	var major, minor int
	if _, err := fmt.Sscanf(version, "go%d.%d", &major, &minor); err != nil {
		log.Printf("warning: unable to parse Go version %q, proceeding anyway", version)
		return
	}

	// Require Go 1.22 or later
	if major < 1 || (major == 1 && minor < 22) {
		log.Fatalf("fatal: Go 1.22 or later is required (detected: %s). Please upgrade Go to run this proxy.", version)
	}
}
