package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	maxRequestBody  = 20 << 20
	maxResponseBody = 64 << 20
)

type Server struct {
	Config     *ConfigFile
	Keys       map[string]string // ключ клиента → имя
	APIKey     string            // настоящий ключ Gemini
	Upstream   string            // https://generativelanguage.googleapis.com
	AdminToken string
	Store      *Store
	HTTP       *http.Client
	Log        *slog.Logger
	Now        func() time.Time
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1beta/models/{call}", s.generate)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /admin/status", s.admin(s.status))
	mux.HandleFunc("GET /admin/usage", s.admin(s.usage))
	mux.HandleFunc("POST /admin/topup", s.admin(s.ledger("topup")))
	mux.HandleFunc("POST /admin/balance", s.admin(s.ledger("set")))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "шлюз пропускает только generateContent")
	})
	return mux
}

// MonthStart — начало текущего месяца в часовом поясе сервера: лимиты месячные.
func MonthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
}

func (s *Server) generate(w http.ResponseWriter, r *http.Request) {
	start := s.Now()
	model, action, _ := strings.Cut(r.PathValue("call"), ":")
	if action != "generateContent" || model == "" {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "шлюз пропускает только generateContent")
		return
	}

	client := s.client(r)
	if client == "" {
		s.Log.Warn("неизвестный ключ", "model", model, "remote", r.RemoteAddr)
		apiError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "ключ не принят шлюзом homelab")
		return
	}
	cfg := s.Config.Get()
	limits, ok := cfg.Clients[client]
	if !ok {
		s.Log.Warn("клиента нет в конфиге", "client", client)
		apiError(w, http.StatusForbidden, "PERMISSION_DENIED", "клиент "+client+" не описан в конфиге шлюза")
		return
	}

	call := Call{Time: start, Client: client, Model: model}
	if reason, err := s.overLimit(r.Context(), cfg, client, limits); err != nil {
		s.Log.Error("проверка лимита", "err", err)
		apiError(w, http.StatusInternalServerError, "INTERNAL", "шлюз не смог проверить лимит")
		return
	} else if reason != "" {
		call.Status, call.Blocked = http.StatusTooManyRequests, true
		s.record(call)
		apiError(w, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", reason)
		return
	}

	status, body, header, usage, err := s.forward(r)
	call.Duration = s.Now().Sub(start)
	if err != nil {
		s.Log.Error("запрос в Google", "client", client, "model", model, "err", err)
		call.Status = http.StatusBadGateway
		s.record(call)
		apiError(w, http.StatusBadGateway, "UNAVAILABLE", "шлюз не дозвался Google")
		return
	}
	call.Status, call.Usage = status, usage
	price, priced := cfg.PriceFor(model)
	call.Priced = priced
	if status == http.StatusOK {
		call.Cost = price.Cost(usage)
	}
	s.record(call)

	if ct := header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// client ищет клиента по ключу. Сравнение без раннего выхода: время ответа не подсказывает ключ.
func (s *Server) client(r *http.Request) string {
	key := r.Header.Get("x-goog-api-key")
	if key == "" {
		key = r.URL.Query().Get("key")
	}
	if key == "" {
		return ""
	}
	found := ""
	for k, name := range s.Keys {
		if subtle.ConstantTimeCompare([]byte(k), []byte(key)) == 1 {
			found = name
		}
	}
	return found
}

// overLimit возвращает причину отказа, если месячный лимит клиента или общий исчерпан.
func (s *Server) overLimit(ctx context.Context, cfg *Config, client string, c Client) (string, error) {
	by, total, err := s.Store.Spent(ctx, MonthStart(s.Now()))
	if err != nil {
		return "", err
	}
	if by[client] >= Micros(c.MonthlyLimitUSD) {
		return fmt.Sprintf("месячный лимит %s исчерпан: $%.2f", client, c.MonthlyLimitUSD), nil
	}
	if total >= Micros(cfg.TotalMonthlyLimitUSD) {
		return fmt.Sprintf("общий месячный лимит исчерпан: $%.2f", cfg.TotalMonthlyLimitUSD), nil
	}
	return "", nil
}

// forward отправляет запрос в Google с настоящим ключом. Ключ клиента не уходит ни заголовком, ни в URL.
func (s *Server) forward(r *http.Request) (int, []byte, http.Header, Usage, error) {
	var usage Usage
	q := r.URL.Query()
	q.Del("key")
	u := strings.TrimRight(s.Upstream, "/") + r.URL.EscapedPath()
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, u, http.MaxBytesReader(nil, r.Body, maxRequestBody))
	if err != nil {
		return 0, nil, nil, usage, err
	}
	req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	req.Header.Set("x-goog-api-key", s.APIKey)

	resp, err := s.HTTP.Do(req)
	if err != nil {
		return 0, nil, nil, usage, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return 0, nil, nil, usage, fmt.Errorf("прочитать ответ: %w", err)
	}
	var meta struct {
		UsageMetadata Usage `json:"usageMetadata"`
	}
	if json.Unmarshal(body, &meta) == nil {
		usage = meta.UsageMetadata
	}
	return resp.StatusCode, body, resp.Header, usage, nil
}

// record пишет учёт с отдельным таймаутом: клиент мог уже отключиться, а расход всё равно был.
func (s *Server) record(c Call) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Store.Record(ctx, c); err != nil {
		s.Log.Error("учёт", "err", err)
	}
	s.Log.Info("вызов", "client", c.Client, "model", c.Model, "status", c.Status, "blocked", c.Blocked,
		"in", c.Usage.InputTokens(), "cached", c.Usage.cached(), "out", c.Usage.OutputTokens(),
		"usd", USD(c.Cost), "ms", c.Duration.Milliseconds())
}

// apiError — ошибка в формате Gemini: клиенты разбирают error.status.
func apiError(w http.ResponseWriter, code int, status, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"code": code, "message": msg, "status": status}})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(buf.Bytes())
}
