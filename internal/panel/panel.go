// Package panel — веб-панель homelab «Планета Экспресс»: сервисы, логи, секреты, базы, расход LLM, доставки.
// Панель слушает только домашнюю сеть. Вход — одноразовой ссылкой из служебного бота, дальше cookie сессии.
package panel

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"homelab/internal/docker"
	"homelab/internal/gateway"
	"homelab/internal/voice"
)

//go:embed web
var webFS embed.FS

// Docker — то, что панели нужно от Docker Engine API.
type Docker interface {
	List(ctx context.Context) ([]docker.Container, error)
	Inspect(ctx context.Context, id string) (docker.Inspect, error)
	Image(ctx context.Context, id string) (docker.Image, error)
	Restart(ctx context.Context, id string) error
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Stats(ctx context.Context, id string) (docker.Stats, error)
	LogStream(ctx context.Context, id string, tail int, follow, tty bool) (io.ReadCloser, error)
}

// Gateway — админ-API шлюза LLM.
type Gateway interface {
	Status(ctx context.Context) (gateway.Status, error)
	Daily(ctx context.Context, since time.Time) ([]gateway.DailyRow, error)
	Ledger(ctx context.Context, kind string, usd float64) (gateway.Balance, error)
}

// Paths — где панель находит файлы хоста. В контейнере: /srv, /homelab (репозиторий, только чтение), /state.
type Paths struct {
	Srv     string // /srv: базы сервисов, секреты, триггеры
	Secrets string // /srv/secrets
	Homelab string // репозиторий homelab: образцы env, фразы, panel.yaml
	State   string // /var/lib/homelab хоста: history.jsonl, last-run.json, backups.json
	Trigger string // /srv/panel/trigger: файлы apply и backup для юнитов systemd
	DBCopy  string // /srv/panel/data/db-backups: копии баз перед записью из Лаборатории
	OpsDB   string // база замеров ops-bot
}

type Server struct {
	Docker   Docker
	Gateway  Gateway // nil — шлюз не настроен, Бухгалтерия пуста
	Metrics  Metrics // nil — нет базы замеров, Мостик без приборов
	Store    *Store
	Voice    *voice.Voice
	Notify   func(ctx context.Context, text string) error // сообщение в служебный бот; nil — не слать
	Paths    Paths
	Projects []string // compose-проекты, которыми можно управлять
	Self     string   // имя контейнера панели: себя не останавливает
	URL      string   // адрес панели для ссылок входа
	Token    string   // PANEL_TOKEN: им ops-bot просит ссылку входа
	Log      *slog.Logger
	Now      func() time.Time

	LoginTTL   time.Duration
	SessionTTL time.Duration

	once  sync.Once
	pages map[string]*template.Template
	lab   labState

	applyMu sync.Mutex
	applyAt time.Time // когда панель последний раз попросила update.sh
}

const cookieName = "pe_session"

func (s *Server) init() {
	s.once.Do(func() {
		if s.Now == nil {
			s.Now = time.Now
		}
		if s.LoginTTL == 0 {
			s.LoginTTL = 5 * time.Minute
		}
		if s.SessionTTL == 0 {
			s.SessionTTL = 30 * 24 * time.Hour
		}
		s.pages = mustPages()
		s.lab.modes = map[string]writeMode{}
	})
}

// Handler — страницы панели на публичном порту.
func (s *Server) Handler() http.Handler {
	s.init()
	static, _ := fs.Sub(webFS, "web/static")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)

	auth := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireSession(h)) }
	auth("POST /logout", s.logout)
	auth("GET /{$}", s.bridge)
	auth("GET /crew", s.crew)
	auth("POST /crew/{name}/{action}", s.crewAction)
	auth("POST /update", s.updateNow)
	auth("GET /logs", s.logsPage)
	auth("GET /logs/stream", s.logsStream)
	auth("GET /logs/download", s.logsDownload)
	auth("GET /vault", s.vault)
	auth("GET /vault/{file}", s.vault)
	auth("POST /vault/{file}", s.vaultSave)
	auth("POST /vault/{file}/reveal", s.vaultReveal)
	auth("GET /lab", s.labPage)
	auth("POST /lab/query", s.labQuery)
	auth("POST /lab/csv", s.labCSV)
	auth("POST /lab/write", s.labWrite)
	auth("GET /accounting", s.accounting)
	auth("POST /accounting/ledger", s.accountingLedger)
	auth("GET /deliveries", s.deliveries)
	auth("POST /deliveries/rollback", s.rollback)
	auth("POST /deliveries/unpin", s.unpin)
	auth("POST /deliveries/freeze", s.freeze)
	auth("GET /api/run", s.apiRun)
	auth("GET /api/poke", s.apiPoke)
	auth("GET /api/crew/stats", s.apiCrewStats)
	mux.HandleFunc("/", s.notFound)
	return secureHeaders(mux)
}

// InternalHandler — порт только для сети homelab: ссылка входа для ops-bot и healthcheck.
func (s *Server) InternalHandler() http.Handler {
	s.init()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /internal/login-link", s.internalLoginLink)
	return mux
}

// secureHeaders: same-origin — адрес страницы с пропуском не уходит наружу (шрифты Google, ссылки).
// no-referrer не подходит: с ним браузер шлёт в POST «Origin: null», и проверка Origin отклоняет свои же формы.
// CSP запрещает чужие скрипты; во фрейм панель не встраивается.
func secureHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; "+
			"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; "+
			"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; form-action 'self'")
		h.ServeHTTP(w, r)
	})
}

// sameOrigin — запрос пришёл со страницы самой панели. Сравнение с Host, а не с PANEL_URL:
// панель открывают и по IP, и по имени хоста.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return false
	}
	u, err := url.Parse(o)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

func (s *Server) requireSession(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && !sameOrigin(r) {
			s.Log.Warn("запрос с чужого адреса отклонён", "path", r.URL.Path, "origin", r.Header.Get("Origin"))
			http.Error(w, "запрос не со страницы панели", http.StatusForbidden)
			return
		}
		c, err := r.Cookie(cookieName)
		var exp time.Time
		ok := false
		if err == nil {
			exp, ok = s.Store.Session(r.Context(), c.Value, s.Now())
		}
		if !ok {
			if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasSuffix(r.URL.Path, "/stream") {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "нужен вход", http.StatusUnauthorized)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, sessionInfo{id: c.Value, expires: exp})))
	})
}

type sessionKey struct{}

type sessionInfo struct {
	id      string
	expires time.Time
}

func session(r *http.Request) sessionInfo {
	si, _ := r.Context().Value(sessionKey{}).(sessionInfo)
	return si
}

// act — действие из панели: запись в журнал и сообщение в служебный бот. text — без значений секретов.
func (s *Server) act(ctx context.Context, action, target, detail, text string) {
	if err := s.Store.Audit(ctx, AuditEntry{Time: s.Now(), Action: action, Target: target, Detail: detail}); err != nil {
		s.Log.Error("журнал действий", "err", err)
	}
	s.Log.Info("действие в панели", "action", action, "target", target, "detail", detail)
	if s.Notify == nil {
		return
	}
	// Telegram может ответить не сразу; ответ пользователю не ждёт его.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.Notify(ctx, actionIcon(action)+" Панель: "+text); err != nil {
			s.Log.Warn("сообщение в Telegram не отправлено", "err", err)
		}
	}()
}

// actionIcon — значок действия в сообщении Telegram: по нему видно, что сделали, не читая текст.
func actionIcon(action string) string {
	switch {
	case action == "login":
		return "🔑"
	case action == "secrets":
		return "🔐"
	case action == "rollback":
		return "⏪"
	case action == "unpin":
		return "⏩"
	case action == "backup":
		return "🧊"
	case action == "update":
		return "🔍"
	case strings.HasPrefix(action, "ledger"):
		return "💰"
	case strings.HasPrefix(action, "lab"):
		return "🧪"
	}
	return "🔧"
}

// page — данные для общего шаблона страницы.
type page struct {
	Title   string
	Active  string // пункт меню
	Toast   string
	Expires time.Time
	Data    any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, status int, p page) {
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "нет шаблона "+name, http.StatusInternalServerError)
		return
	}
	if p.Toast == "" {
		p.Toast = r.URL.Query().Get("toast")
	}
	p.Expires = session(r).expires
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := t.ExecuteTemplate(w, "layout", p); err != nil {
		s.Log.Error("шаблон", "page", name, "err", err)
	}
}

// back — вернуться на страницу с сообщением; POST → redirect → GET, чтобы обновление страницы не повторяло действие.
func back(w http.ResponseWriter, r *http.Request, path, toast string) {
	if toast != "" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		path += sep + "toast=" + url.QueryEscape(toast)
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login", http.StatusNotFound, page{Title: "Бендер украл эту страницу", Data: loginView{Variant: "notfound"}})
}

func (s *Server) apiPoke(w http.ResponseWriter, r *http.Request) {
	event := "panel-poke"
	if r.URL.Query().Get("bad") == "1" {
		event = "panel-poke-bad"
	}
	text := s.Voice.Line(event, "")
	if text == "" {
		text = "Поцелуй мой блестящий металлический зад!"
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": text})
}

func mustPages() map[string]*template.Template {
	funcs := template.FuncMap{
		"pct":   func(v float64) string { return fmt.Sprintf("%.0f", clamp(v, 0, 100)) },
		"usd":   func(v float64) string { return fmt.Sprintf("$%.2f", v) },
		"since": humanSince,
		"clock": func(t time.Time) string { return t.Local().Format("15:04") },
		"date":  dateOf,
		"add":   func(a, b int) int { return a + b },
		"size":  humanSize,
		"now":   time.Now,
	}
	layout := template.Must(template.New("layout").Funcs(funcs).ParseFS(webFS, "web/templates/layout.html", "web/templates/ship.html"))
	names, err := fs.Glob(webFS, "web/templates/page-*.html")
	if err != nil {
		panic(err)
	}
	out := map[string]*template.Template{}
	for _, n := range names {
		name := strings.TrimSuffix(strings.TrimPrefix(n, "web/templates/page-"), ".html")
		out[name] = template.Must(template.Must(layout.Clone()).ParseFS(webFS, n))
	}
	return out
}

// dateOf принимает и *time.Time: так в шаблоне не нужна проверка на nil.
func dateOf(v any) string {
	switch t := v.(type) {
	case time.Time:
		return t.Local().Format("02.01")
	case *time.Time:
		if t != nil {
			return t.Local().Format("02.01")
		}
	}
	return ""
}

func clamp(v, lo, hi float64) float64 {
	return max(lo, min(hi, v))
}

// humanSince — «35 мин», «2 ч 25 мин», «6 дн».
func humanSince(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "меньше минуты"
	case d < time.Hour:
		return fmt.Sprintf("%d мин", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		return fmt.Sprintf("%d ч %d мин", h, int(d.Minutes())-h*60)
	default:
		return fmt.Sprintf("%d дн", int(d.Hours()/24))
	}
}
