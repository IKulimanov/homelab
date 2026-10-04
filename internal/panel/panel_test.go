package panel

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"homelab/internal/docker"
)

type fakeDocker struct {
	containers []docker.Container
	calls      []string
	stats      atomic.Int32 // замеры идут параллельно, поэтому счётчик, а не calls
}

func (f *fakeDocker) List(context.Context) ([]docker.Container, error) { return f.containers, nil }
func (f *fakeDocker) Inspect(context.Context, string) (docker.Inspect, error) {
	return docker.Inspect{}, nil
}
func (f *fakeDocker) Image(context.Context, string) (docker.Image, error) { return docker.Image{}, nil }
func (f *fakeDocker) Restart(_ context.Context, id string) error {
	f.calls = append(f.calls, "restart "+id)
	return nil
}
func (f *fakeDocker) Start(_ context.Context, id string) error {
	f.calls = append(f.calls, "start "+id)
	return nil
}
func (f *fakeDocker) Stop(_ context.Context, id string) error {
	f.calls = append(f.calls, "stop "+id)
	return nil
}
func (f *fakeDocker) Stats(context.Context, string) (docker.Stats, error) {
	f.stats.Add(1)
	return docker.Stats{CPUPercent: 1.5, MemBytes: 64 << 20}, nil
}
func (f *fakeDocker) LogStream(context.Context, string, int, bool, bool) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("строка 1\nстрока 2\n")), nil
}

func container(id, name, project, state, status string) docker.Container {
	return docker.Container{ID: id, Names: []string{"/" + name}, State: state, Status: status,
		Labels: map[string]string{docker.LabelProject: project}}
}

type fixture struct {
	s      *Server
	h      http.Handler
	docker *fakeDocker
	logs   *bytes.Buffer
	srv    string
	cookie *http.Cookie
}

const exampleEnv = `# /srv/secrets/budget-bot.env — секреты budget-bot. Права 0600.

# Токен бота от @BotFather. Обязателен.
BOT_TOKEN=

# Модель Gemini.
GEMINI_MODEL=gemini-2.5-flash
`

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	srv := filepath.Join(root, "srv")
	homelab := filepath.Join(root, "homelab")
	for _, d := range []string{"secrets", "panel/trigger", "panel/data", "budget-bot/data"} {
		if err := os.MkdirAll(filepath.Join(srv, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.MkdirAll(filepath.Join(homelab, "env"), 0o755)
	_ = os.MkdirAll(filepath.Join(root, "state"), 0o755)
	write(t, filepath.Join(homelab, "env", "budget-bot.env.example"), exampleEnv)
	write(t, filepath.Join(srv, "secrets", "budget-bot.env"), "# мой комментарий\nBOT_TOKEN=old-token\n# ещё один\nGEMINI_MODEL=gemini-2.5-flash\nEXTRA=1\n")
	write(t, filepath.Join(srv, "secrets", "media.env"), "PUID=1000\n")
	write(t, filepath.Join(srv, "secrets", "homelab.env"), "TZ=Asia/Bishkek\n")

	store, err := OpenStore(filepath.Join(srv, "panel/data/panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	fd := &fakeDocker{containers: []docker.Container{
		container("b1", "budget-bot", "apps", "running", "Up 3 hours"),
		container("p1", "panel", "platform", "running", "Up 1 hour (healthy)"),
		container("j1", "jellyfin", "media-stack", "exited", "Exited (1) 2 minutes ago"),
		container("x1", "stranger", "", "running", "Up 2 days"),
	}}
	logs := &bytes.Buffer{}
	s := &Server{
		Docker: fd, Store: store, Log: slog.New(slog.NewTextHandler(logs, nil)),
		Paths: Paths{Srv: srv, Secrets: filepath.Join(srv, "secrets"), Homelab: homelab, State: filepath.Join(root, "state"),
			Trigger: filepath.Join(srv, "panel/trigger"), DBCopy: filepath.Join(srv, "panel/data/db-backups")},
		Projects: []string{"platform", "apps", "media-stack"}, Self: "panel",
		URL: "http://panel.test:8800", Token: "secret-panel-token",
	}
	f := &fixture{s: s, h: s.Handler(), docker: fd, logs: logs, srv: srv}
	sid, err := store.NewSession(context.Background(), time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.cookie = &http.Cookie{Name: cookieName, Value: sid}
	return f
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// do — запрос к панели. form != nil — POST со своим Origin; origin "" — без заголовка Origin.
func (f *fixture) do(method, path string, form url.Values, origin string, auth bool) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, "http://panel.test:8800"+path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if auth {
		r.AddCookie(f.cookie)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func (f *fixture) post(path string, form url.Values) *httptest.ResponseRecorder {
	return f.do(http.MethodPost, path, form, "http://panel.test:8800", true)
}

func TestLoginTokenWorksOnce(t *testing.T) {
	f := newFixture(t)
	link, err := f.s.LoginLink(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(link)
	tok := u.Query().Get("t")

	// GET только показывает кнопку и пропуск не тратит.
	if w := f.do(http.MethodGet, "/login?t="+tok, nil, "", false); !strings.Contains(w.Body.String(), "Курьер прибыл") {
		t.Fatalf("страница входа: %d", w.Code)
	}
	w := f.do(http.MethodPost, "/login", url.Values{"t": {tok}}, "http://panel.test:8800", false)
	if w.Code != http.StatusSeeOther || len(w.Result().Cookies()) == 0 {
		t.Fatalf("первый вход: %d", w.Code)
	}
	c := w.Result().Cookies()[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie: %+v", c)
	}
	if w := f.do(http.MethodPost, "/login", url.Values{"t": {tok}}, "http://panel.test:8800", false); w.Code != http.StatusForbidden {
		t.Fatalf("второй вход по той же ссылке: %d", w.Code)
	}
}

func TestExpiredLoginTokenRejected(t *testing.T) {
	f := newFixture(t)
	tok, err := f.s.Store.NewLoginToken(context.Background(), time.Now().Add(-10*time.Minute), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if w := f.do(http.MethodPost, "/login", url.Values{"t": {tok}}, "http://panel.test:8800", false); w.Code != http.StatusForbidden {
		t.Fatalf("просроченный пропуск: %d", w.Code)
	}
}

func TestWithoutSessionRedirectsToLogin(t *testing.T) {
	f := newFixture(t)
	w := f.do(http.MethodGet, "/crew", nil, "", false)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("без cookie: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := f.do(http.MethodGet, "/api/run", nil, "", false); w.Code != http.StatusUnauthorized {
		t.Fatalf("api без cookie: %d", w.Code)
	}
}

func TestPostNeedsOwnOrigin(t *testing.T) {
	f := newFixture(t)
	form := url.Values{}
	if w := f.do(http.MethodGet, "/crew", nil, "", true); w.Header().Get("Referrer-Policy") != "same-origin" {
		t.Fatalf("Referrer-Policy %q: с no-referrer браузер шлёт Origin: null", w.Header().Get("Referrer-Policy"))
	}
	for name, origin := range map[string]string{"чужой": "http://evil.test", "без Origin": "", "null": "null"} {
		if w := f.do(http.MethodPost, "/crew/budget-bot/restart", form, origin, true); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if len(f.docker.calls) != 0 {
		t.Fatalf("Docker вызван: %v", f.docker.calls)
	}
}

func TestInternalLoginLinkNeedsToken(t *testing.T) {
	f := newFixture(t)
	h := f.s.InternalHandler()
	r := httptest.NewRequest(http.MethodPost, "/internal/login-link", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("без токена: %d", w.Code)
	}
	r = httptest.NewRequest(http.MethodPost, "/internal/login-link", nil)
	r.Header.Set("Authorization", "Bearer secret-panel-token")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var out map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != http.StatusOK || !strings.HasPrefix(out["url"], "http://panel.test:8800/login?t=") {
		t.Fatalf("с токеном: %d %v", w.Code, out)
	}
}

func TestCrewActionsOnlyOwnContainersAndNotSelf(t *testing.T) {
	f := newFixture(t)
	if w := f.post("/crew/stranger/stop", url.Values{}); w.Code != http.StatusForbidden {
		t.Fatalf("чужой контейнер: %d", w.Code)
	}
	if w := f.post("/crew/panel/stop", url.Values{}); w.Code != http.StatusForbidden {
		t.Fatalf("остановка себя: %d", w.Code)
	}
	if len(f.docker.calls) != 0 {
		t.Fatalf("Docker вызван: %v", f.docker.calls)
	}
	if w := f.post("/crew/budget-bot/restart", url.Values{}); w.Code != http.StatusSeeOther || len(f.docker.calls) != 1 || f.docker.calls[0] != "restart b1" {
		t.Fatalf("свой контейнер: %d %v", w.Code, f.docker.calls)
	}
}

func TestVaultSaveKeepsCommentsAndHidesValue(t *testing.T) {
	f := newFixture(t)
	w := f.post("/vault/budget-bot.env", url.Values{
		"v_BOT_TOKEN": {"new-$zq9 token"}, "t_BOT_TOKEN": {"1"},
		"v_GEMINI_MODEL": {""}, "t_GEMINI_MODEL": {""}, // пустое и нетронутое — не менять
		"v_EXTRA": {""}, "t_EXTRA": {"1"}, // пустое, но тронутое — очистить
	})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("сохранение: %d %s", w.Code, w.Body.String())
	}
	path := filepath.Join(f.srv, "secrets", "budget-bot.env")
	data, _ := os.ReadFile(path)
	want := "# мой комментарий\nBOT_TOKEN='new-$zq9 token'\n# ещё один\nGEMINI_MODEL=gemini-2.5-flash\nEXTRA=\n"
	if string(data) != want {
		t.Fatalf("файл:\n%s\nждали:\n%s", data, want)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("права: %v", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(f.srv, "panel/trigger/apply")); err != nil {
		t.Fatalf("триггер apply не записан: %v", err)
	}
	audit, _ := f.s.Store.RecentAudit(context.Background(), 10)
	for _, e := range audit {
		if strings.Contains(e.Detail, "zq9") || strings.Contains(e.Detail, "old-token") {
			t.Fatalf("значение в журнале: %+v", e)
		}
	}
	if len(audit) == 0 || !strings.Contains(audit[0].Detail, "BOT_TOKEN") {
		t.Fatalf("в журнале нет имени ключа: %+v", audit)
	}
	if strings.Contains(f.logs.String(), "zq9") {
		t.Fatalf("значение в логе: %s", f.logs.String())
	}
}

func TestVaultMediaIsReadOnly(t *testing.T) {
	f := newFixture(t)
	if w := f.post("/vault/media.env", url.Values{"v_PUID": {"0"}, "t_PUID": {"1"}}); w.Code != http.StatusForbidden {
		t.Fatalf("media.env: %d", w.Code)
	}
	if data, _ := os.ReadFile(filepath.Join(f.srv, "secrets", "media.env")); string(data) != "PUID=1000\n" {
		t.Fatalf("media.env изменён: %s", data)
	}
	if w := f.post("/vault/..%2Fpanel%2Fdata%2Fpanel.db", url.Values{}); w.Code != http.StatusNotFound {
		t.Fatalf("выход из каталога: %d", w.Code)
	}
}

func TestVaultRevealReturnsValue(t *testing.T) {
	f := newFixture(t)
	w := f.post("/vault/budget-bot.env/reveal", url.Values{"key": {"BOT_TOKEN"}})
	var out map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["value"] != "old-token" {
		t.Fatalf("reveal: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(f.logs.String(), "old-token") {
		t.Fatal("значение в логе")
	}
}

func newLabDB(t *testing.T, f *fixture) string {
	t.Helper()
	path := filepath.Join(f.srv, "budget-bot/data/app.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT);
		WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 1500) INSERT INTO items (id, name) SELECT x, 'n' || x FROM c;`); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f *fixture) query(t *testing.T, q string) labResult {
	t.Helper()
	w := f.post("/lab/query", url.Values{"db": {"budget-bot/app.db"}, "sql": {q}})
	var res labResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("ответ: %d %s", w.Code, w.Body.String())
	}
	return res
}

func TestLabReadModeRejectsWritesAndLimitsRows(t *testing.T) {
	f := newFixture(t)
	newLabDB(t, f)
	res := f.query(t, "SELECT * FROM items")
	if res.Error != "" || len(res.Rows) != labRows || !res.Truncated {
		t.Fatalf("лимит строк: %d %v %q", len(res.Rows), res.Truncated, res.Error)
	}
	if res := f.query(t, "UPDATE items SET name = 'x'"); !strings.Contains(res.Error, "Профессор запретил") {
		t.Fatalf("UPDATE в режиме чтения: %+v", res.Error)
	}
	if res := f.query(t, "SELECT 1; DELETE FROM items"); !strings.Contains(res.Error, "одному оператору") {
		t.Fatalf("два оператора: %q", res.Error)
	}
	if res := f.query(t, "ATTACH DATABASE '/tmp/x.db' AS x"); !strings.Contains(res.Error, "ATTACH") {
		t.Fatalf("ATTACH: %q", res.Error)
	}
	if res := f.query(t, "SELECT 'a;b' -- ; и комментарий\n;"); res.Error != "" || res.Rows[0][0] != "a;b" {
		t.Fatalf("; в строке и комментарии: %+v", res)
	}
}

func TestLabQueryTimeout(t *testing.T) {
	f := newFixture(t)
	newLabDB(t, f)
	f.s.lab.readTimeout = 300 * time.Millisecond
	start := time.Now()
	res := f.query(t, "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c) SELECT count(*) FROM c")
	if !strings.Contains(res.Error, "остановлен") || time.Since(start) > 5*time.Second {
		t.Fatalf("таймаут: %q за %s", res.Error, time.Since(start))
	}
}

func TestLabWriteModeCopiesBeforeWrite(t *testing.T) {
	f := newFixture(t)
	newLabDB(t, f)
	if w := f.post("/lab/write", url.Values{"db": {"budget-bot/app.db"}, "on": {"1"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("включение записи: %d", w.Code)
	}
	copies, _ := filepath.Glob(filepath.Join(f.srv, "panel/data/db-backups", "budget-bot-app-*.db"))
	if len(copies) != 1 {
		t.Fatalf("копия: %v", copies)
	}
	res := f.query(t, "DELETE FROM items WHERE id > 10")
	if res.Error != "" || !res.Write || res.Changed != 1490 {
		t.Fatalf("запись: %+v", res)
	}
	// В копии строки остались.
	db, _ := sql.Open("sqlite", "file:"+copies[0]+"?mode=ro")
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM items").Scan(&n); err != nil || n != 1500 {
		t.Fatalf("в копии %d строк: %v", n, err)
	}
	// Без копии запрос не идёт.
	_ = os.Remove(copies[0])
	if res := f.query(t, "DELETE FROM items"); !strings.Contains(res.Error, "копии базы нет") {
		t.Fatalf("без копии: %+v", res)
	}
}

// Шаблоны проверяются только при выполнении: каждая страница должна отрисоваться без ошибки.
func TestPagesRender(t *testing.T) {
	f := newFixture(t)
	newLabDB(t, f)
	write(t, filepath.Join(f.s.Paths.State, "history.jsonl"),
		`{"ts":1800000000,"stack":"apps","svc":"budget-bot","from":"sha-aaaaaaa","to":"sha-bbbbbbb","result":"ok","reason":"image","text":"обновлён"}`+"\n")
	write(t, filepath.Join(f.s.Paths.State, "last-run.json"), `{"started":1800000000,"finished":1800000060,"code":1,"policy":"auto"}`)
	write(t, filepath.Join(f.s.Paths.State, "backups.json"), `[{"svc":"budget-bot","file":"app-2027.db","size":2048,"ts":1800000000}]`)
	for _, p := range []string{"/", "/crew", "/crew?stack=apps", "/logs?svc=budget-bot", "/vault/budget-bot.env", "/vault/budget-bot.env?saved=1700000000&keys=BOT_TOKEN",
		"/vault/media.env", "/lab", "/accounting", "/deliveries", "/nope"} {
		w := f.do(http.MethodGet, p, nil, "", true)
		if w.Code != http.StatusOK && !(p == "/nope" && w.Code == http.StatusNotFound) {
			t.Errorf("%s: %d", p, w.Code)
		}
		if !strings.Contains(w.Body.String(), "</html>") {
			t.Errorf("%s: страница оборвана", p)
		}
	}
	for _, p := range []string{"/login", "/login?t=bad"} {
		if w := f.do(http.MethodGet, p, nil, "", false); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "</html>") {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if strings.Contains(f.logs.String(), "шаблон") {
		t.Fatalf("ошибки шаблонов:\n%s", f.logs.String())
	}
	if !strings.Contains(f.do(http.MethodGet, "/deliveries", nil, "", true).Body.String(), "Откатить на sha-aaaaaaa") {
		t.Fatal("нет кнопки отката")
	}
}

func TestRollbackPinsTag(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.s.Paths.State, "history.jsonl"),
		`{"ts":1800000000,"stack":"apps","svc":"budget-bot","from":"sha-aaaaaaa","to":"sha-bbbbbbb","result":"ok","reason":"image","text":"обновлён"}`+"\n")
	if w := f.post("/deliveries/rollback", url.Values{"svc": {"budget-bot"}, "to": {"sha-aaaaaaa; rm"}}); w.Code != http.StatusBadRequest {
		t.Fatalf("плохой тег: %d", w.Code)
	}
	if w := f.post("/deliveries/rollback", url.Values{"svc": {"jellyfin"}, "to": {"sha-aaaaaaa"}}); w.Code != http.StatusBadRequest {
		t.Fatalf("чужой стек: %d", w.Code)
	}
	if w := f.post("/deliveries/rollback", url.Values{"svc": {"budget-bot"}, "to": {"sha-aaaaaaa"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("откат: %d", w.Code)
	}
	data, _ := os.ReadFile(filepath.Join(f.srv, "secrets", "homelab.env"))
	if string(data) != "TZ=Asia/Bishkek\nBUDGET_BOT_TAG=sha-aaaaaaa\n" {
		t.Fatalf("homelab.env: %s", data)
	}
	if w := f.post("/deliveries/unpin", url.Values{"svc": {"budget-bot"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("unpin: %d", w.Code)
	}
	if data, _ := os.ReadFile(filepath.Join(f.srv, "secrets", "homelab.env")); !strings.Contains(string(data), "BUDGET_BOT_TAG=\n") {
		t.Fatalf("после unpin: %s", data)
	}
}

func TestTelegramErrorHidesToken(t *testing.T) {
	send := telegramAt(&http.Client{Timeout: time.Second}, "http://127.0.0.1:1", "123:SECRET", 42)
	err := send(context.Background(), "привет")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("ошибка: %v", err)
	}
}

// Юнит удаляет файл-триггер до старта update.sh: панель всё равно ждёт прогон, начатый после её запроса.
func TestRunningWaitsForRunAfterApply(t *testing.T) {
	f := newFixture(t)
	now := time.Unix(1_800_000_000, 0)
	f.s.Now = func() time.Time { return now }
	lastRun := filepath.Join(f.s.Paths.State, "last-run.json")
	write(t, lastRun, `{"started":1799999000,"finished":1799999100,"code":0}`)
	if f.s.running() {
		t.Fatal("до запроса прогона нет")
	}
	if err := f.s.trigger("apply"); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(f.s.Paths.Trigger, "apply"))
	if !f.s.running() {
		t.Fatal("файл уже удалён юнитом, но прогон ещё не начался")
	}
	write(t, lastRun, `{"started":1800000005,"finished":null}`)
	if !f.s.running() {
		t.Fatal("прогон идёт")
	}
	write(t, lastRun, `{"started":1800000005,"finished":1800000050,"code":0}`)
	if f.s.running() {
		t.Fatal("прогон закончился")
	}
	// update.sh так и не запустился: через 20 минут панель перестаёт ждать.
	if err := f.s.trigger("apply"); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(f.s.Paths.Trigger, "apply"))
	now = now.Add(21 * time.Minute)
	if f.s.running() {
		t.Fatal("ожидание без конца")
	}
}

func TestCrewPageDoesNotWaitForStats(t *testing.T) {
	f := newFixture(t)
	w := f.do(http.MethodGet, "/crew", nil, "", true)
	if w.Code != http.StatusOK || f.docker.stats.Load() != 0 {
		t.Fatalf("страница: %d, замеров Docker: %d", w.Code, f.docker.stats.Load())
	}
	if !strings.Contains(w.Body.String(), `data-res="budget-bot"`) {
		t.Fatal("нет места под CPU и память budget-bot")
	}

	w = f.do(http.MethodGet, "/api/crew/stats", nil, "", true)
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("ответ %d: %s", w.Code, w.Body.String())
	}
	// Остановленный jellyfin и чужой stranger не замеряются.
	want := map[string]string{"budget-bot": "1,5 % · 64 МБ", "panel": "1,5 % · 64 МБ"}
	if len(got) != len(want) || got["budget-bot"] != want["budget-bot"] || got["panel"] != want["panel"] {
		t.Fatalf("замеры: %v", got)
	}
	if w := f.do(http.MethodGet, "/api/crew/stats", nil, "", false); w.Code != http.StatusUnauthorized {
		t.Fatalf("без входа: %d", w.Code)
	}
}
