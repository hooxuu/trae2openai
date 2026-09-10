// traeopenai: OpenAI-compatible proxy for Trae CLI backend.
//
// Auth flow (reproduced from traecli binary):
//  1. POST /cloudide/api/v3/trae/oauth/ExchangeToken  body {"RefreshToken":"<PAT>"}
//  2. POST /api/ide/v2/llm_raw_chat with Authorization: Cloud-IDE-JWT <token>
//
// Token is refreshed automatically before expiry (refresh reuses the same
// ExchangeToken endpoint, as the original binary does).
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type Config struct {
	Host           string // Trae backend, e.g. https://api.enterprise.trae.cn
	Listen         string // local listen address
	PAT            string // personal access token (= refresh token) bootstrap
	StateFile      string // token persistence path
	APIKey         string // client API key (sk-...) required by callers
	VersionCode    string // client version presented upstream (X-IDE-Version-Code)
	VersionFrom    string // where VersionCode came from (for logs)
	AllowVFallback bool   // TRAE_ALLOW_VERSION_FALLBACK=1: permit a synthetic code
}

func loadConfig() Config {
	versionCode, versionFrom := resolveVersionCode()
	cfg := Config{
		Host:           envOr("TRAE_HOST", "https://api.enterprise.trae.cn"),
		Listen:         envOr("TRAE_LISTEN", "127.0.0.1:8686"),
		PAT:            envOr("TRAE_PAT", os.Getenv("TRAECLI_PERSONAL_ACCESS_TOKEN")),
		VersionCode:    versionCode,
		VersionFrom:    versionFrom,
		AllowVFallback: os.Getenv("TRAE_ALLOW_VERSION_FALLBACK") == "1",
	}
	if f := os.Getenv("TRAE_STATE_FILE"); f != "" {
		cfg.StateFile = f
	} else {
		home, _ := os.UserHomeDir()
		cfg.StateFile = filepath.Join(home, ".trae-openai-state.json")
	}
	return cfg
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// resolveVersionCode decides which X-IDE-Version-Code to present upstream:
//
//  1. TRAE_IDE_VERSION_CODE - explicit operator override;
//  2. the constant embedded in the locally installed official CLI, so the proxy
//     tracks the real client instead of inventing a number;
//  3. defaultClientVersionCode - the last value verified against that client.
//
// resolveVersionCode decides which X-IDE-Version-Code to present upstream:
//
//  1. TRAE_IDE_VERSION_CODE - explicit operator override;
//  2. defaultClientVersionCode - the value the official client ships today.
//
// Deliberately no auto-detection. The adapter normally runs in a container
// that has no Trae CLI installed, so reading whatever binary happens to sit on
// the host would be both surprising and wrong. When Trae raises its version
// gate the call fails loudly (see consumeUpstream) instead of guessing, and
// defaultClientVersionCode gets bumped after checking the current CLI.
func resolveVersionCode() (code, source string) {
	if v := strings.TrimSpace(os.Getenv("TRAE_IDE_VERSION_CODE")); v != "" {
		return v, "TRAE_IDE_VERSION_CODE"
	}
	return defaultClientVersionCode, "built-in default"
}

// loadOrCreateAPIKey resolves the client API key (sk-...):
//  1. explicit TRAE_PROXY_API_KEY env var wins;
//  2. otherwise read from / generate a key file next to the state file.
func loadOrCreateAPIKey(cfg Config) string {
	if k := strings.TrimSpace(os.Getenv("TRAE_PROXY_API_KEY")); k != "" {
		return k
	}
	dir := filepath.Dir(cfg.StateFile)
	keyFile := filepath.Join(dir, ".trae-openai-apikey")
	if data, err := os.ReadFile(keyFile); err == nil {
		if k := strings.TrimSpace(string(data)); k != "" {
			return k
		}
	}
	key := generateAPIKey()
	if err := os.WriteFile(keyFile, []byte(key+"\n"), 0600); err != nil {
		log.Printf("[auth] WARN: persist api key failed (%v); key will not survive restarts", err)
	} else {
		log.Printf("[auth] generated new API key, saved to %s", keyFile)
	}
	return key
}

// generateAPIKey returns an OpenAI-style key: "sk-" + 48 random hex chars.
func generateAPIKey() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "sk-" + hex.EncodeToString(b)
}

const (
	appID         = "7b3f9dc2-8a4e-5c6d-2f1b-9e4a3c5b7df0"
	ideVersion    = "99.99.99"
	refreshMargin = 5 * time.Minute // refresh this long before expiry
	minTokenTTL   = 30 * time.Second

	// defaultClientVersionCode is the constant the official client ships today,
	// read straight out of the traecli string table ("X-App-Id" then
	// "20260206" then "99.99.99"). It is a protocol generation marker, not a
	// build date: traecli 0.120.52 (built 2026-08-12) still sends it, and the
	// backend refuses anything older. Never invent a value here - a version the
	// official client never released is exactly the kind of signal that makes
	// traffic look synthetic.
	defaultClientVersionCode = "20260206"

	// clientVersionFallback is a synthetic value, used only when the operator
	// opts in with TRAE_ALLOW_VERSION_FALLBACK=1 after the backend moved its
	// version gate past everything real we know about.
	clientVersionFallback = "20990101"

	// errCodeVersionRejected is the SSE error code Trae returns when it refuses
	// the client version. Its payload carries an empty message, so the code is
	// the only usable signal (verified 2026-09: code 4120 for stale versions).
	errCodeVersionRejected = 4120
)

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// Trae API types
// ---------------------------------------------------------------------------

type OauthToken struct {
	Token           string `json:"Token"`
	TokenExpireAt   int64  `json:"TokenExpireAt"`
	RefreshToken    string `json:"RefreshToken"`
	RefreshExpireAt int64  `json:"RefreshExpireAt"`
}

type apiEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"Data"`
}

// ---------------------------------------------------------------------------
// TokenManager: auth + periodic refresh + persistence
// ---------------------------------------------------------------------------

type TokenManager struct {
	mu   sync.RWMutex
	cfg  Config
	tok  *OauthToken
	stop chan struct{}
}

func NewTokenManager(cfg Config) *TokenManager {
	return &TokenManager{cfg: cfg, stop: make(chan struct{})}
}

func (tm *TokenManager) Bootstrap() error {
	if data, err := os.ReadFile(tm.cfg.StateFile); err == nil {
		var tok OauthToken
		if json.Unmarshal(data, &tok) == nil && tok.Token != "" && tok.RefreshToken != "" {
			tm.mu.Lock()
			tm.tok = &tok
			tm.mu.Unlock()
			log.Printf("[auth] loaded cached token from %s (expires %s)",
				tm.cfg.StateFile, time.UnixMilli(tok.TokenExpireAt).Format(time.RFC3339))
		}
	}
	if tm.current() == nil {
		if tm.cfg.PAT == "" {
			return fmt.Errorf("no cached token and no PAT: set TRAE_PAT / TRAECLI_PERSONAL_ACCESS_TOKEN or run traecli login first")
		}
		if err := tm.exchange(tm.cfg.PAT); err != nil {
			return fmt.Errorf("initial token exchange failed: %w", err)
		}
	} else if tm.nearlyExpired() {
		tm.RefreshNow()
	}
	return nil
}

func (tm *TokenManager) current() *OauthToken {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.tok
}

func (tm *TokenManager) nearlyExpired() bool {
	tok := tm.current()
	return tok == nil || time.Until(time.UnixMilli(tok.TokenExpireAt)) < refreshMargin
}

// AccessToken returns a valid access token, refreshing synchronously if needed.
func (tm *TokenManager) AccessToken(ctx context.Context) (string, error) {
	if !tm.nearlyExpired() {
		return tm.current().Token, nil
	}
	return tm.RefreshNow()
}

func (tm *TokenManager) refreshPAT() string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	if tm.tok != nil && tm.tok.RefreshToken != "" {
		return tm.tok.RefreshToken
	}
	return tm.cfg.PAT
}

// RefreshNow calls ExchangeToken with the latest refresh token.
func (tm *TokenManager) RefreshNow() (string, error) {
	pat := tm.refreshPAT()
	if pat == "" {
		return "", fmt.Errorf("no refresh token available")
	}
	if err := tm.exchange(pat); err != nil {
		return "", err
	}
	return tm.current().Token, nil
}

func (tm *TokenManager) exchange(refreshToken string) error {
	body, _ := json.Marshal(map[string]string{"RefreshToken": refreshToken})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST",
		tm.cfg.Host+"/cloudide/api/v3/trae/oauth/ExchangeToken", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("bad response: %w", err)
	}
	if env.Code != 0 {
		return fmt.Errorf("code=%d message=%s", env.Code, env.Message)
	}
	var tok OauthToken
	if err := json.Unmarshal(env.Data, &tok); err != nil || tok.Token == "" {
		return fmt.Errorf("no token in response")
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}

	tm.mu.Lock()
	tm.tok = &tok
	tm.mu.Unlock()

	data, _ := json.MarshalIndent(tok, "", "  ")
	if err := os.WriteFile(tm.cfg.StateFile, data, 0600); err != nil {
		log.Printf("[auth] WARN: persist token failed: %v", err)
	}
	log.Printf("[auth] token refreshed, expires %s",
		time.UnixMilli(tok.TokenExpireAt).Format(time.RFC3339))
	return nil
}

// StartRefreshLoop refreshes the token proactively before expiry.
func (tm *TokenManager) StartRefreshLoop() {
	go func() {
		for {
			tok := tm.current()
			var wait time.Duration
			if tok == nil {
				wait = time.Minute
			} else {
				wait = time.Until(time.UnixMilli(tok.TokenExpireAt)) - refreshMargin
				if wait < minTokenTTL {
					wait = minTokenTTL
				}
			}
			select {
			case <-tm.stop:
				return
			case <-time.After(wait):
				if _, err := tm.RefreshNow(); err != nil {
					log.Printf("[auth] refresh failed: %v (retry in 30s)", err)
					select {
					case <-tm.stop:
						return
					case <-time.After(30 * time.Second):
					}
				}
			}
		}
	}()
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// ---------------------------------------------------------------------------
// Model registry (from /api/ide/v1/cli/get_config_list)
// ---------------------------------------------------------------------------

type ModelInfo struct {
	ConfigName  string // exposed OpenAI model id, e.g. "qwen3.8-max"
	ModelName   string // wire model_name, e.g. "qwen3.8-max__dev"
	DisplayName string
	MaxTokens   int
	Multimodal  bool
}

type ModelRegistry struct {
	mu            sync.RWMutex
	host          string
	tm            *TokenManager
	versionCode   string
	allowFallback bool
	models        map[string]ModelInfo
	fetchErr      error
	lastAt        time.Time
}

func NewModelRegistry(host string, tm *TokenManager, versionCode string, allowFallback bool) *ModelRegistry {
	return &ModelRegistry{host: host, tm: tm, versionCode: versionCode, allowFallback: allowFallback,
		models: map[string]ModelInfo{}}
}

// Fetch loads the model catalog. The catalog is gated on X-IDE-Version-Code
// too, and a refused version answers HTTP 500 code 2001 with an empty list -
// which used to leave a stale catalog and nothing else. A synthetic retry only
// happens when the operator opted in with TRAE_ALLOW_VERSION_FALLBACK=1.
func (mr *ModelRegistry) Fetch(ctx context.Context) error {
	err := mr.fetchWith(ctx, mr.versionCode)
	if err != nil && mr.allowFallback && mr.versionCode != clientVersionFallback {
		log.Printf("[models] WARN: fetch with version_code=%s failed: %v; retrying with synthetic %s (TRAE_ALLOW_VERSION_FALLBACK=1)",
			mr.versionCode, err, clientVersionFallback)
		err = mr.fetchWith(ctx, clientVersionFallback)
	}

	mr.mu.Lock()
	mr.fetchErr = err
	mr.mu.Unlock()
	if err != nil {
		log.Printf("[models] WARN: catalog fetch failed: %v (will serve the previous list)", err)
	}
	return err
}

// LastError reports the most recent catalog fetch failure (nil after a
// success). Callers use it to explain an empty model list instead of answering
// a misleading "model not found".
func (mr *ModelRegistry) LastError() error {
	mr.mu.RLock()
	defer mr.mu.RUnlock()
	return mr.fetchErr
}

func (mr *ModelRegistry) fetchWith(ctx context.Context, versionCode string) error {
	token, err := mr.tm.AccessToken(ctx)
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, "POST",
		mr.host+"/api/ide/v1/cli/get_config_list",
		strings.NewReader(`{"function":"chat","version_code":`+versionCode+`}`))
	setIDEHeaders(req, token, versionCode)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var list struct {
		ConfigInfoList []struct {
			ConfigName    string `json:"config_name"`
			ConfigSwitch  bool   `json:"config_switch"`
			DisplayConfig struct {
				DisplayName string `json:"display_name"`
				Multimodal  bool   `json:"multimodal"`
			} `json:"display_config"`
			ModelDetailList []struct {
				ModelName string `json:"model_name"`
				MaxTokens int    `json:"max_tokens"`
			} `json:"model_detail_list"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("parse config list: %w (body: %s)", err, truncate(string(raw), 200))
	}

	models := map[string]ModelInfo{}
	for _, c := range list.ConfigInfoList {
		if !c.ConfigSwitch || c.ConfigName == "custom_model_placeholder" || c.ConfigName == "summary" {
			continue
		}
		info := ModelInfo{
			ConfigName:  c.ConfigName,
			ModelName:   c.ConfigName,
			DisplayName: c.DisplayConfig.DisplayName,
			Multimodal:  c.DisplayConfig.Multimodal,
		}
		if len(c.ModelDetailList) > 0 && c.ModelDetailList[0].ModelName != "" {
			info.ModelName = c.ModelDetailList[0].ModelName
			info.MaxTokens = c.ModelDetailList[0].MaxTokens
		}
		models[c.ConfigName] = info
	}
	if len(models) == 0 {
		return fmt.Errorf("config list empty (body: %s)", truncate(string(raw), 200))
	}

	mr.mu.Lock()
	mr.models = models
	mr.lastAt = time.Now()
	mr.mu.Unlock()
	log.Printf("[models] loaded %d models", len(models))
	return nil
}

func (mr *ModelRegistry) Lookup(name string) (ModelInfo, bool) {
	mr.mu.RLock()
	defer mr.mu.RUnlock()
	info, ok := mr.models[name]
	if !ok {
		// allow sending the wire name directly (e.g. "qwen3.8-max__dev")
		for _, m := range mr.models {
			if m.ModelName == name {
				return m, true
			}
		}
	}
	return info, ok
}

func (mr *ModelRegistry) List() []ModelInfo {
	mr.mu.RLock()
	defer mr.mu.RUnlock()
	out := make([]ModelInfo, 0, len(mr.models))
	for _, m := range mr.models {
		out = append(out, m)
	}
	return out
}

func (mr *ModelRegistry) StartRefetchLoop() {
	go func() {
		t := time.NewTicker(30 * time.Minute)
		defer t.Stop()
		for range t.C {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := mr.Fetch(ctx); err != nil {
				log.Printf("[models] refetch failed: %v", err)
			}
			cancel()
		}
	}()
}

// ---------------------------------------------------------------------------
// OpenAI-compatible HTTP server
// ---------------------------------------------------------------------------

type Server struct {
	cfg      Config
	tm       *TokenManager
	registry *ModelRegistry
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("POST /chat/completions", s.handleChat)
	// healthz stays public (no auth) for container/load-balancer probes.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	return s.authMiddleware(mux)
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if s.cfg.APIKey != "" {
			auth := r.Header.Get("Authorization")
			if auth != "Bearer "+s.cfg.APIKey {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid api key", "invalid_request_error")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func setIDEHeaders(req *http.Request, token, versionCode string) {
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-App-Id", appID)
	req.Header.Set("X-IDE-Function", "chat")
	req.Header.Set("X-IDE-Version", ideVersion)
	req.Header.Set("X-IDE-Version-Code", versionCode)
}

func writeOpenAIError(w http.ResponseWriter, status int, msg, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg, "type": typ, "code": status,
		},
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	list := s.registry.List()
	data := make([]map[string]any, 0, len(list))
	for _, m := range list {
		data = append(data, map[string]any{
			"id":       m.ConfigName,
			"object":   "model",
			"created":  0,
			"owned_by": "trae",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// --- OpenAI chat request/response types ---

type OAIMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type OAIRequest struct {
	Model       string          `json:"model"`
	Messages    []OAIMessage    `json:"messages"`
	Stream      bool            `json:"stream"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	User        string          `json:"user,omitempty"`
	StreamOpts  *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

type TraeMessage struct {
	Role       string            `json:"role"`
	Content    []json.RawMessage `json:"content"`
	ToolCalls  json.RawMessage   `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
}

type TraeChatRequest struct {
	ConfigName     string          `json:"config_name"`
	ConversationID string          `json:"conversation_id"`
	Messages       []TraeMessage   `json:"messages"`
	ModelName      string          `json:"model_name"`
	SessionID      string          `json:"session_id"`
	Tools          json.RawMessage `json:"tools,omitempty"`
	UserInput      string          `json:"user_input"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var oai OAIRequest
	if err := json.NewDecoder(r.Body).Decode(&oai); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid request body: "+err.Error(), "invalid_request_error")
		return
	}
	if oai.Model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}
	model, ok := s.registry.Lookup(oai.Model)
	if !ok {
		if err := s.registry.LastError(); err != nil {
			// The catalog is empty because the upstream call failed (version gate,
			// credentials, network). Saying "model not found" here hides the real
			// problem behind a misleading 404.
			writeOpenAIError(w, http.StatusBadGateway,
				"model catalog unavailable: "+err.Error(), "upstream_error")
			return
		}
		writeOpenAIError(w, http.StatusNotFound,
			fmt.Sprintf("model %q not found; see /v1/models", oai.Model), "model_not_found")
		return
	}

	ctx := r.Context()
	token, err := s.tm.AccessToken(ctx)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "token unavailable: "+err.Error(), "server_error")
		return
	}

	// Build Trae request
	sessionID := newUUID()
	convID := oai.User
	if convID == "" {
		convID = newUUID()
	}
	userInput := extractLastUserText(oai.Messages)

	traeMsgs, err := convertMessages(oai.Messages)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	tools, err := convertTools(oai.Tools)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	traeReq := TraeChatRequest{
		ConfigName:     model.ConfigName,
		ConversationID: convID,
		Messages:       traeMsgs,
		ModelName:      model.ModelName,
		SessionID:      sessionID,
		Tools:          tools,
		UserInput:      userInput,
	}
	payload, _ := json.Marshal(traeReq)

	if debugSSE {
		log.Printf("[req] payload=%s", truncate(string(payload), 2000))
	}

	extra := map[string]any{
		"agent_loop_id":         sessionID,
		"api_host":              s.cfg.Host,
		"api_key":               token,
		"base_url":              s.cfg.Host + "/trae-cli/api/v1/llm/proxy",
		"config_name":           model.ConfigName,
		"config_source":         1,
		"display_name":          model.DisplayName,
		"model_name":            model.ModelName,
		"real_api_key":          "",
		"real_base_url":         "",
		"session_id":            sessionID,
		"user_prompt_submit_id": sessionID,
	}
	extraJSON, _ := json.Marshal(extra)

	// open issues one upstream call with the given client version. It is a closure
	// so a version refusal can be retried without rebuilding the whole Trae
	// payload (TraeChatRequest has no version field - it lives in the
	// X-IDE-Version-Code header).
	open := func(versionCode string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "POST",
			s.cfg.Host+"/api/ide/v2/llm_raw_chat", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		setIDEHeaders(req, token, versionCode)
		req.Header.Set("extra", string(extraJSON))
		return http.DefaultClient.Do(req)
	}

	clientVersion := s.cfg.VersionCode
	resp, err := open(clientVersion)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream request failed: "+err.Error(), "server_error")
		return
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		msg := fmt.Sprintf("upstream http %d: %s", resp.StatusCode, truncate(string(raw), 400))
		writeOpenAIError(w, http.StatusBadGateway, msg, "server_error")
		return
	}

	chatID := "chatcmpl-" + randomHex(12)
	created := time.Now().Unix()

	if oai.Stream {
		s.streamSSE(w, resp, open, clientVersion, chatID, created, oai, model)
	} else {
		s.collectSSE(w, resp, open, clientVersion, chatID, created, oai, model)
	}
}

// convertMessages: OpenAI messages -> Trae messages (content as part array).
func convertMessages(msgs []OAIMessage) ([]TraeMessage, error) {
	out := make([]TraeMessage, 0, len(msgs))
	for _, m := range msgs {
		tm := TraeMessage{Role: m.Role, ToolCallID: m.ToolCallID, ToolCalls: denormalizeToolCalls(m.ToolCalls)}
		if len(m.Content) == 0 || string(m.Content) == "null" {
			tm.Content = []json.RawMessage{}
			out = append(out, tm)
			continue
		}
		// string content
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			part, _ := json.Marshal(map[string]string{"type": "text", "text": s})
			tm.Content = []json.RawMessage{part}
			out = append(out, tm)
			continue
		}
		// array content: keep text parts, pass others through as-is
		var parts []json.RawMessage
		if err := json.Unmarshal(m.Content, &parts); err != nil {
			return nil, fmt.Errorf("message content must be string or array")
		}
		if len(parts) == 0 {
			part, _ := json.Marshal(map[string]string{"type": "text", "text": ""})
			parts = []json.RawMessage{part}
		}
		tm.Content = parts
		out = append(out, tm)
	}
	return out, nil
}

// convertTools: Trae requires function.parameters as a JSON *string*, while
// OpenAI clients send it as an object. Stringify it for each tool.
func convertTools(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("tools must be an array: %w", err)
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		fn := map[string]any{"name": t.Function.Name}
		if t.Function.Description != "" {
			fn["description"] = t.Function.Description
		}
		if len(t.Function.Parameters) > 0 {
			if t.Function.Parameters[0] == '"' {
				fn["parameters"] = json.RawMessage(t.Function.Parameters) // already a string
			} else {
				fn["parameters"] = string(t.Function.Parameters) // stringify object
			}
		}
		typ := t.Type
		if typ == "" {
			typ = "function"
		}
		out = append(out, map[string]any{"type": typ, "function": fn})
	}
	return json.Marshal(out)
}

// denormalizeToolCalls is the request-direction inverse of normalizeToolCalls:
// OpenAI clients send assistant tool_calls as {id, type, function:{name,
// arguments}}, but Trae's eino backend emits/expects function_call (not
// function). Without this, the backend cannot register the tool_call_id,
// and the next turn's tool result is rejected with
// "tool_call_id is not found" (code 4027).
func denormalizeToolCalls(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return raw
	}
	var calls []map[string]any
	if json.Unmarshal(raw, &calls) != nil {
		return raw
	}
	for _, c := range calls {
		// already in Trae format (has function_call) — leave as-is
		if _, ok := c["function_call"]; ok {
			continue
		}
		if fn, ok := c["function"]; ok && fn != nil {
			c["function_call"] = fn
			delete(c, "function")
		}
	}
	out, err := json.Marshal(calls)
	if err != nil {
		return raw
	}
	return out
}

// normalizeToolCalls converts Trae's tool_call format
// ({id, type, function_call:{name, arguments}}) to OpenAI's
// ({id, type, function:{name, arguments}}).
func normalizeToolCalls(raw json.RawMessage) json.RawMessage {
	var calls []map[string]any
	if json.Unmarshal(raw, &calls) != nil {
		return raw
	}
	for i, c := range calls {
		if fc, ok := c["function_call"]; ok && fc != nil {
			c["function"] = fc
			delete(c, "function_call")
		}
		if fn, ok := c["function"].(map[string]any); ok {
			delete(fn, "partial_arguments")
			delete(fn, "namespace")
		}
		if _, ok := c["index"]; !ok {
			c["index"] = i
		}
	}
	out, err := json.Marshal(calls)
	if err != nil {
		return raw
	}
	return out
}

func extractLastUserText(msgs []OAIMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		var s string
		if json.Unmarshal(msgs[i].Content, &s) == nil {
			return s
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(msgs[i].Content, &parts) == nil {
			for _, p := range parts {
				if p.Type == "text" && p.Text != "" {
					return p.Text
				}
			}
		}
	}
	return ""
}

// --- SSE parsing ---

type sseEvent struct {
	name string
	data string
}

// parseSSE reads events until EOF or ctx done.
func parseSSE(ctx context.Context, body io.Reader, onEvent func(sseEvent)) error {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	var ev sseEvent
	var dataLines []string
	flush := func() {
		if len(dataLines) > 0 || ev.name != "" {
			ev.data = strings.Join(dataLines, "\n")
			onEvent(ev)
		}
		ev = sseEvent{}
		dataLines = nil
	}
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "event:"):
			ev.name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()
	return sc.Err()
}

type outputEvent struct {
	Response         string          `json:"response"`
	ReasoningContent *string         `json:"reasoning_content"`
	ToolCalls        json.RawMessage `json:"tool_calls"`
}

type usageEvent struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatResult struct {
	content   strings.Builder
	reasoning strings.Builder
	toolCalls json.RawMessage
	finish    string
	usage     *usageEvent
	sawDone   bool // set when the upstream "done" event is received

	// errCode/errMsg capture an upstream `event: error` payload. Without them the
	// reason for a contentless completion is thrown away and the caller only sees
	// a vague truncation - exactly how the 2026-09 version and auth rejections
	// stayed invisible for a week.
	errCode int
	errMsg  string
}

// empty reports whether the upstream produced no usable output at all.
func (r *chatResult) empty() bool {
	return r.content.Len() == 0 && r.reasoning.Len() == 0 && len(r.toolCalls) == 0
}

// upstreamErr renders the captured upstream failure, or "" when there was none.
func (r *chatResult) upstreamErr() string {
	if r.errCode == 0 && r.errMsg == "" {
		return ""
	}
	if r.errMsg != "" {
		return fmt.Sprintf("upstream error (code %d): %s", r.errCode, r.errMsg)
	}
	return fmt.Sprintf("upstream error (code %d)", r.errCode)
}

// setUpstreamError records an upstream error payload. Trae uses a flat shape
// ({"code":4120,"error":"","message":"..."}) but the OpenAI nesting
// ({"error":{"code":..,"message":..}}) is accepted too, so nothing is lost.
func (r *chatResult) setUpstreamError(data string) {
	var flat struct {
		Code    int    `json:"code"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(data), &flat) == nil {
		if flat.Code != 0 {
			r.errCode = flat.Code
		}
		if flat.Message != "" {
			r.errMsg = flat.Message
		} else if flat.Error != "" {
			r.errMsg = flat.Error
		}
	}
	if r.errMsg == "" {
		var nested struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &nested) == nil && nested.Error.Message != "" {
			if nested.Error.Code != 0 {
				r.errCode = nested.Error.Code
			}
			r.errMsg = nested.Error.Message
		}
	}
	if r.errMsg == "" {
		r.errMsg = truncate(data, 300) // keep the raw payload as a last resort
	}
	log.Printf("[sse] upstream error code=%d msg=%s", r.errCode, truncate(r.errMsg, 300))
}

// debugSSE logs every upstream SSE event and payload; enable with
// TRAE_DEBUG_SSE=1. It is what separates "the model returned an empty
// completion" from "the adapter dropped or misread the stream".
var debugSSE = os.Getenv("TRAE_DEBUG_SSE") == "1"

func handleEvents(ctx context.Context, body io.Reader, onOutput func(outputEvent), res *chatResult) error {
	return parseSSE(ctx, body, func(ev sseEvent) {
		if debugSSE {
			log.Printf("[sse] event=%q data=%s", ev.name, truncate(ev.data, 400))
		}
		switch ev.name {
		case "output":
			var o outputEvent
			if json.Unmarshal([]byte(ev.data), &o) != nil {
				return
			}
			res.content.WriteString(o.Response)
			if o.ReasoningContent != nil {
				res.reasoning.WriteString(*o.ReasoningContent)
			}
			if len(o.ToolCalls) > 0 && string(o.ToolCalls) != "null" {
				res.toolCalls = o.ToolCalls
			}
			if onOutput != nil {
				onOutput(o)
			}
		case "token_usage":
			var u usageEvent
			if json.Unmarshal([]byte(ev.data), &u) == nil {
				res.usage = &u
			}
		case "done":
			res.sawDone = true
			var d struct {
				FinishReason string `json:"finish_reason"`
			}
			if json.Unmarshal([]byte(ev.data), &d) == nil && d.FinishReason != "" {
				res.finish = d.FinishReason
			}
		case "error", "exception":
			// The upstream refused the request (stale client version, revoked
			// token, quota...). Keep the payload: it is the only description of
			// the failure, which the client would otherwise never see.
			res.setUpstreamError(ev.data)
		default:
			// Trae can push error side events under other names; never swallow
			// them silently, they are exactly the difference between "model
			// returned empty content" and "upstream failed".
			if strings.Contains(ev.name, "error") || strings.Contains(ev.name, "exception") ||
				strings.Contains(ev.data, `"error"`) {
				log.Printf("[sse] upstream error event %q: %s", ev.name, truncate(ev.data, 400))
				res.setUpstreamError(ev.data)
			} else if debugSSE {
				log.Printf("[sse] unhandled event %q", ev.name)
			}
		}
	})
}

func openAIUsage(u *usageEvent) map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
}

// consumeUpstream reads one upstream SSE stream into res. When the backend
// refuses our client version before producing any output, it retries once with
// clientVersionFallback. The retry is safe because neither serve path has
// written anything to the client at that point.
func (s *Server) consumeUpstream(ctx context.Context, resp *http.Response, open func(string) (*http.Response, error),
	versionCode string, onOutput func(outputEvent), res *chatResult) error {

	err := handleEvents(ctx, resp.Body, onOutput, res)
	resp.Body.Close()
	if err != nil {
		return err
	}
	if res.errCode != errCodeVersionRejected || !res.empty() || versionCode == clientVersionFallback || !s.cfg.AllowVFallback {
		return nil
	}

	log.Printf("[chat] WARN: backend rejected client version_code=%s; retrying once with synthetic %s (TRAE_ALLOW_VERSION_FALLBACK=1). Update traecli or set TRAE_IDE_VERSION_CODE if this repeats.",
		versionCode, clientVersionFallback)
	*res = chatResult{}
	retry, err := open(clientVersionFallback)
	if err != nil {
		return err
	}
	err = handleEvents(ctx, retry.Body, onOutput, res)
	retry.Body.Close()
	return err
}

// collectSSE: non-stream mode — aggregate all events, return one completion.
func (s *Server) collectSSE(w http.ResponseWriter, resp *http.Response, open func(string) (*http.Response, error),
	versionCode, id string, created int64, oai OAIRequest, model ModelInfo) {

	var res chatResult
	if err := s.consumeUpstream(context.Background(), resp, open, versionCode, nil, &res); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "stream read error: "+err.Error(), "server_error")
		return
	}
	if res.empty() && res.errMsg != "" {
		// Nothing usable came back, but the upstream said why: surface the real
		// reason instead of an empty completion the client renders as a
		// contentless truncation.
		log.Printf("[chat] ERROR: %s (model=%s)", res.upstreamErr(), model.ConfigName)
		writeOpenAIError(w, http.StatusBadGateway, res.upstreamErr(), "upstream_error")
		return
	}
	if !res.sawDone {
		log.Printf("[chat] WARN: upstream stream ended without done event (content=%d bytes, finish=%q)",
			res.content.Len(), res.finish)
	}
	if res.finish == "" {
		if res.sawDone {
			res.finish = "stop"
		} else {
			res.finish = "length" // stream terminated upstream without a stop
		}
	}
	if res.empty() {
		log.Printf("[chat] WARN: upstream returned empty completion (finish=%q, model=%s)",
			res.finish, model.ConfigName)
	}
	msg := map[string]any{"role": "assistant", "content": res.content.String()}
	if res.reasoning.Len() > 0 {
		msg["reasoning_content"] = res.reasoning.String()
	}
	if len(res.toolCalls) > 0 {
		msg["tool_calls"] = normalizeToolCalls(res.toolCalls)
		msg["content"] = nil
	}
	out := map[string]any{
		"id": id, "object": "chat.completion", "created": created,
		"model": model.ConfigName,
		"choices": []map[string]any{
			{"index": 0, "message": msg, "finish_reason": res.finish},
		},
	}
	if u := openAIUsage(res.usage); u != nil {
		out["usage"] = u
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// streamSSE: stream mode — emit chat.completion.chunk per output event.
func (s *Server) streamSSE(w http.ResponseWriter, resp *http.Response, open func(string) (*http.Response, error),
	versionCode, id string, created int64, oai OAIRequest, model ModelInfo) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming unsupported", "server_error")
		return
	}

	// Headers go out lazily, on the first real delta. As long as nothing has been
	// written an upstream refusal can still be reported as a proper HTTP error
	// instead of an empty 200 stream that clients render as a contentless
	// truncation.
	started := false
	send := func(chunk map[string]any) {
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)
			started = true
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	roleChunk := func() {
		send(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model.ConfigName,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
		})
	}

	var res chatResult
	ctx := context.Background()
	err := s.consumeUpstream(ctx, resp, open, versionCode, func(o outputEvent) {
		if o.Response == "" && o.ReasoningContent == nil && (len(o.ToolCalls) == 0 || string(o.ToolCalls) == "null") {
			return
		}
		if !started {
			roleChunk() // open on the first output, never on a doomed stream
		}
		delta := map[string]any{}
		if o.Response != "" {
			delta["content"] = o.Response
		}
		if o.ReasoningContent != nil {
			delta["reasoning_content"] = *o.ReasoningContent
		}
		if len(o.ToolCalls) > 0 && string(o.ToolCalls) != "null" {
			delta["tool_calls"] = normalizeToolCalls(o.ToolCalls)
		}
		send(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model.ConfigName,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": nil}},
		})
	}, &res)

	if !started {
		switch {
		case err != nil:
			writeOpenAIError(w, http.StatusBadGateway, "stream read error: "+err.Error(), "server_error")
			return
		case res.empty() && res.errMsg != "":
			log.Printf("[chat] ERROR: %s (model=%s)", res.upstreamErr(), model.ConfigName)
			writeOpenAIError(w, http.StatusBadGateway, res.upstreamErr(), "upstream_error")
			return
		default:
			roleChunk() // legitimate empty completion: upstream sent done, no output
		}
	}

	finish := res.finish
	if finish == "" {
		if err != nil {
			finish = "error"
		} else if res.sawDone {
			finish = "stop"
		} else {
			finish = "length" // stream ended upstream without a terminal done
		}
	}
	if !res.sawDone && err == nil {
		log.Printf("[chat] WARN: upstream stream ended without done event (content=%d bytes)",
			res.content.Len())
	}
	if res.errMsg != "" && !res.empty() {
		// A partial answer was already streamed: keep it and end honestly with
		// "length". (No error object is emitted mid-stream on purpose - clients
		// disagree on it and some abort the whole turn.)
		log.Printf("[chat] WARN: %s after partial output (finish=%q, model=%s)",
			res.upstreamErr(), finish, model.ConfigName)
	}
	if res.empty() {
		log.Printf("[chat] WARN: upstream returned empty completion (finish=%q, model=%s)",
			finish, model.ConfigName)
	}
	send(map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model.ConfigName,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
	})
	includeUsage := oai.StreamOpts != nil && oai.StreamOpts.IncludeUsage
	if includeUsage {
		send(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model.ConfigName,
			"choices": []map[string]any{},
			"usage":   openAIUsage(res.usage),
		})
	}
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	log.SetFlags(log.Ltime)
	cfg := loadConfig()
	cfg.APIKey = loadOrCreateAPIKey(cfg)
	log.Printf("[auth] client API key: %s", cfg.APIKey)
	if cfg.Host == "" || cfg.PAT == "" {
		log.Printf("[cfg] TRAE_HOST=%s state=%s", cfg.Host, cfg.StateFile)
	}
	log.Printf("[version] client version code %s (source: %s)", cfg.VersionCode, cfg.VersionFrom)
	if cfg.AllowVFallback {
		log.Printf("[version] WARN: synthetic fallback %s enabled (TRAE_ALLOW_VERSION_FALLBACK=1)", clientVersionFallback)
	}

	tm := NewTokenManager(cfg)
	if err := tm.Bootstrap(); err != nil {
		log.Fatalf("[auth] %v", err)
	}
	tm.StartRefreshLoop()

	registry := NewModelRegistry(cfg.Host, tm, cfg.VersionCode, cfg.AllowVFallback)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := registry.Fetch(ctx); err != nil {
		log.Printf("[models] initial fetch failed: %v (will serve empty list)", err)
	}
	cancel()
	registry.StartRefetchLoop()

	srv := &Server{cfg: cfg, tm: tm, registry: registry}
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv.routes()}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Printf("shutting down")
		shutCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		httpSrv.Shutdown(shutCtx)
	}()

	log.Printf("listening on http://%s (backend: %s)", cfg.Listen, cfg.Host)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}
