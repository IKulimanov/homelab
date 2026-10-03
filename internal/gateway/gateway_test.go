package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	realKey   = "real-google-key"
	budgetKey = "budget-key-0123456789"
	wellKey   = "wellbeing-key-0123456789"
	adminTok  = "admin-token"
)

const testConfig = `
prices:
  gemini-test:
    input: 1
    output: 4
    cache_read: 0.25
clients:
  budget-bot:
    monthly_limit_usd: 1
  wellbeing-bot:
    monthly_limit_usd: 1
total_monthly_limit_usd: 10
balance_alert_usd: 2
`

// okResponse — ответ Google: 1000 токенов промпта, из них 400 из кэша, 100 ответа и 50 размышлений.
const okResponse = `{"candidates":[{"content":{"parts":[{"text":"{}"}]}}],
"usageMetadata":{"promptTokenCount":1000,"cachedContentTokenCount":400,"candidatesTokenCount":100,"thoughtsTokenCount":50}}`

type upstream struct {
	srv    *httptest.Server
	calls  atomic.Int32
	status int
	body   string
	gotKey atomic.Value
	gotURL atomic.Value
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{status: http.StatusOK, body: okResponse}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.calls.Add(1)
		u.gotKey.Store(r.Header.Get("x-goog-api-key"))
		u.gotURL.Store(r.URL.String())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.status)
		_, _ = io.WriteString(w, u.body)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

type fixture struct {
	srv  *Server
	h    http.Handler
	up   *upstream
	now  time.Time
	path string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "llm-gateway.yaml")
	if err := os.WriteFile(cfgPath, []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg, err := LoadConfigFile(cfgPath, log)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "gw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := &fixture{up: newUpstream(t), now: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC), path: cfgPath}
	f.srv = &Server{
		Config:     cfg,
		Keys:       map[string]string{budgetKey: "budget-bot", wellKey: "wellbeing-bot"},
		APIKey:     realKey,
		Upstream:   f.up.srv.URL,
		AdminToken: adminTok,
		Store:      store,
		HTTP:       f.up.srv.Client(),
		Log:        log,
		Now:        func() time.Time { return f.now },
	}
	f.h = f.srv.Handler()
	return f
}

func (f *fixture) generate(key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-test:generateContent", strings.NewReader(`{"contents":[]}`))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("x-goog-api-key", key)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) adminDo(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+adminTok)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) status(t *testing.T) Status {
	t.Helper()
	rec := f.adminDo(http.MethodGet, "/admin/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d %s", rec.Code, rec.Body)
	}
	var st Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func errorStatus(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error struct {
			Status string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("ответ не в формате ошибки Gemini: %s", rec.Body)
	}
	return e.Error.Status
}

func TestCostCountsThoughtsAsOutputAndCacheSeparately(t *testing.T) {
	p := Price{Input: 1, Output: 4, CacheRead: 0.25}
	u := Usage{PromptTokens: 1000, CachedTokens: 400, CandidatesTokens: 100, ThoughtsTokens: 50}
	// 600 × 1 + 400 × 0.25 + 150 × 4 = 1300 микродолларов.
	if got := p.Cost(u); got != 1300 {
		t.Fatalf("Cost = %d, want 1300", got)
	}
}

func TestProxySendsRealKeyAndHidesClientKey(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-test:generateContent?key="+budgetKey+"&alt=json", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body)
	}
	if got := f.up.gotKey.Load(); got != realKey {
		t.Fatalf("в Google ушёл ключ %q", got)
	}
	if u := f.up.gotURL.Load().(string); strings.Contains(u, budgetKey) || !strings.Contains(u, "alt=json") {
		t.Fatalf("URL в Google: %s", u)
	}
	if rec.Body.String() != okResponse {
		t.Fatalf("тело ответа изменено: %s", rec.Body)
	}
	st := f.status(t)
	if st.Clients[0].Name != "budget-bot" || st.Clients[0].SpentUSD != 0.0013 {
		t.Fatalf("учёт: %+v", st.Clients)
	}
}

func TestUnknownKeyRejectedWithoutUpstream(t *testing.T) {
	f := newFixture(t)
	for _, key := range []string{"", "wrong-key-0123456789"} {
		rec := f.generate(key)
		if rec.Code != http.StatusUnauthorized || errorStatus(t, rec) != "UNAUTHENTICATED" {
			t.Fatalf("ключ %q: %d %s", key, rec.Code, rec.Body)
		}
	}
	if n := f.up.calls.Load(); n != 0 {
		t.Fatalf("в Google ушло %d запросов", n)
	}
}

func TestLimitBlocksOnlyThatClient(t *testing.T) {
	f := newFixture(t)
	// Лимит $1: дорогой ответ съедает его за один вызов.
	f.up.body = `{"usageMetadata":{"promptTokenCount":1000000}}`
	if rec := f.generate(budgetKey); rec.Code != http.StatusOK {
		t.Fatalf("первый вызов: %d", rec.Code)
	}
	f.up.body = okResponse

	rec := f.generate(budgetKey)
	if rec.Code != http.StatusTooManyRequests || errorStatus(t, rec) != "RESOURCE_EXHAUSTED" {
		t.Fatalf("сверх лимита: %d %s", rec.Code, rec.Body)
	}
	if n := f.up.calls.Load(); n != 1 {
		t.Fatalf("сверх лимита запрос ушёл в Google: вызовов %d", n)
	}
	if rec := f.generate(wellKey); rec.Code != http.StatusOK {
		t.Fatalf("другой клиент: %d", rec.Code)
	}

	// В новом месяце лимит снова свободен.
	f.now = time.Date(2026, 11, 1, 0, 0, 1, 0, time.UTC)
	if rec := f.generate(budgetKey); rec.Code != http.StatusOK {
		t.Fatalf("новый месяц: %d", rec.Code)
	}
}

func TestUpstreamErrorPassedThroughAndRecordedFree(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		f := newFixture(t)
		f.up.status = code
		f.up.body = `{"error":{"code":` + strconv.Itoa(code) + `,"status":"UNAVAILABLE"}}`
		rec := f.generate(budgetKey)
		if rec.Code != code || rec.Body.String() != f.up.body {
			t.Fatalf("код %d: получили %d %s", code, rec.Code, rec.Body)
		}
		rows, err := f.srv.Store.Usage(context.Background(), time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Errors != 1 || rows[0].CostUSD != 0 {
			t.Fatalf("код %d: учёт %+v", code, rows)
		}
	}
}

func TestOnlyGenerateContentProxied(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-test:streamGenerateContent", strings.NewReader(`{}`))
	req.Header.Set("x-goog-api-key", budgetKey)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound || f.up.calls.Load() != 0 {
		t.Fatalf("stream: %d, вызовов %d", rec.Code, f.up.calls.Load())
	}
}

func TestBalanceIsTopupsMinusSpendAndCorrection(t *testing.T) {
	f := newFixture(t)
	if rec := f.adminDo(http.MethodPost, "/admin/topup", `{"usd": 10}`); rec.Code != http.StatusOK {
		t.Fatalf("topup: %d %s", rec.Code, rec.Body)
	}
	f.up.body = `{"usageMetadata":{"promptTokenCount":500000}}` // $0.50
	f.generate(budgetKey)
	if b := f.status(t).Balance; !b.Known || b.USD != 9.5 {
		t.Fatalf("после расхода: %+v", b)
	}

	// Сверка с AI Studio заменяет расчёт; дальше расход вычитается уже из неё.
	f.now = f.now.Add(time.Minute)
	f.adminDo(http.MethodPost, "/admin/balance", `{"usd": 7}`)
	f.now = f.now.Add(time.Minute)
	f.generate(budgetKey)
	if b := f.status(t).Balance; b.USD != 6.5 {
		t.Fatalf("после сверки: %+v", b)
	}
}

func TestAdminRequiresToken(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/status", nil)
	req.Header.Set("Authorization", "Bearer "+budgetKey)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("код %d", rec.Code)
	}
}

func TestConfigReloadKeepsOldOnError(t *testing.T) {
	f := newFixture(t)
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(f.path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		// Время изменения в разных ФС грубое; размер файла тоже меняется, но сдвигаем и его.
		later := time.Now().Add(time.Duration(len(s)) * time.Second)
		_ = os.Chtimes(f.path, later, later)
	}

	write(strings.Replace(testConfig, "monthly_limit_usd: 1\n  wellbeing", "monthly_limit_usd: 5\n  wellbeing", 1))
	if got := f.srv.Config.Get().Clients["budget-bot"].MonthlyLimitUSD; got != 5 {
		t.Fatalf("лимит после правки: %v", got)
	}
	write("clients: [")
	if got := f.srv.Config.Get().Clients["budget-bot"].MonthlyLimitUSD; got != 5 {
		t.Fatalf("после ошибки в файле лимит %v, ждали прежний 5", got)
	}
}

func TestParseConfigRejectsZeroLimit(t *testing.T) {
	if _, err := ParseConfig([]byte("clients:\n  a:\n    monthly_limit_usd: 0\ntotal_monthly_limit_usd: 1\n")); err == nil {
		t.Fatal("нулевой лимит принят")
	}
	if _, err := ParseConfig([]byte("clients:\n  a:\n    monthly_limit: 3\ntotal_monthly_limit_usd: 1\n")); err == nil {
		t.Fatal("опечатка в ключе принята")
	}
}

func TestKeysFromEnv(t *testing.T) {
	keys, err := KeysFromEnv([]string{"LLM_KEY_BUDGET_BOT=" + budgetKey, "GEMINI_API_KEY=x", "PATH=/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[budgetKey] != "budget-bot" {
		t.Fatalf("keys = %v", keys)
	}
	if _, err := KeysFromEnv([]string{"LLM_KEY_A=short"}); err == nil {
		t.Fatal("короткий ключ принят")
	}
}

// Конфиг из репозитория доезжает до сервера через git pull; ошибка в нём не должна дожить до сервера.
func TestRepoConfigParses(t *testing.T) {
	data, err := os.ReadFile("../../stacks/platform/config/llm-gateway.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Prices[DefaultPrice]; !ok {
		t.Fatal("нет цены default: расход по новой модели не попадёт в лимиты")
	}
}

func TestDailySplitsByLocalDay(t *testing.T) {
	f := newFixture(t)
	// В UTC оба вызова попали бы в 14 октября; по местному времени (UTC+6) это разные сутки.
	loc := time.FixedZone("UTC+6", 6*3600)
	f.now = time.Date(2026, 10, 14, 23, 30, 0, 0, loc)
	f.generate(budgetKey)
	f.now = time.Date(2026, 10, 15, 0, 30, 0, 0, loc)
	f.generate(budgetKey)
	f.generate(wellKey)

	rec := f.adminDo(http.MethodGet, "/admin/daily?since="+url.QueryEscape("2026-10-01T00:00:00+06:00"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("daily: %d %s", rec.Code, rec.Body)
	}
	var rows []DailyRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		if r.CostUSD <= 0 {
			t.Errorf("нет стоимости: %+v", r)
		}
		got = append(got, r.Day+" "+r.Client+" "+strconv.FormatInt(r.Calls, 10))
	}
	want := []string{"2026-10-14 budget-bot 1", "2026-10-15 budget-bot 1", "2026-10-15 wellbeing-bot 1"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("дни: %v, ждали %v", got, want)
	}
}

func TestDailyRequiresToken(t *testing.T) {
	f := newFixture(t)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/daily?since=2026-10-01T00:00:00Z", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("код %d", rec.Code)
	}
}
