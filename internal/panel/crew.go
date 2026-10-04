package panel

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"homelab/internal/docker"
)

// managed — контейнеры проектов, которыми панели можно управлять.
func (s *Server) managed(ctx context.Context) ([]docker.Container, error) {
	all, err := s.Docker.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []docker.Container
	for _, c := range all {
		if slices.Contains(s.Projects, c.Project()) {
			out = append(out, c)
		}
	}
	return out, nil
}

// find — контейнер по имени среди разрешённых. Чужой контейнер для панели не существует.
func (s *Server) find(ctx context.Context, name string) (docker.Container, error) {
	list, err := s.managed(ctx)
	if err != nil {
		return docker.Container{}, err
	}
	for _, c := range list {
		if c.Name() == name {
			return c, nil
		}
	}
	return docker.Container{}, errNotFound
}

var exitedRe = regexp.MustCompile(`^Exited \((\d+)\)`)

// condition — состояние контейнера словами. Остановленным вручную считается выход с кодом 0 или 143 (SIGTERM
// от docker stop); любой другой код — упал сам.
type condition struct {
	Text    string
	Level   string // ok, warn, bad, off
	Problem bool
	Running bool
}

func conditionOf(c docker.Container) condition {
	switch c.State {
	case "running":
		if strings.Contains(c.Status, "(unhealthy)") {
			return condition{Text: "болеет: healthcheck не проходит", Level: "bad", Problem: true, Running: true}
		}
		if strings.Contains(c.Status, "(health: starting)") {
			return condition{Text: "просыпается", Level: "warn", Running: true}
		}
		return condition{Text: "работает", Level: "ok", Running: true}
	case "restarting":
		return condition{Text: "падает и перезапускается", Level: "bad", Problem: true}
	case "exited":
		code := -1
		if m := exitedRe.FindStringSubmatch(c.Status); m != nil {
			code, _ = strconv.Atoi(m[1])
		}
		if code == 0 || code == 143 {
			return condition{Text: "остановлен вручную", Level: "off"}
		}
		return condition{Text: fmt.Sprintf("упал сам · код %d", code), Level: "bad", Problem: true}
	case "dead":
		return condition{Text: "мёртв: Docker не может его убрать", Level: "bad", Problem: true}
	}
	return condition{Text: c.State, Level: "off"}
}

// version — версия образа: sha коммита для своих сервисов, номер версии для чужих. link — коммит на GitHub.
func (s *Server) version(ctx context.Context, c docker.Container, cache map[string]docker.Image) (string, string) {
	img, ok := cache[c.ImageID]
	if !ok {
		var err error
		if img, err = s.Docker.Image(ctx, c.ImageID); err == nil {
			cache[c.ImageID] = img
		}
	}
	l := img.Config.Labels
	if rev := l["org.opencontainers.image.revision"]; len(rev) >= 7 {
		link := ""
		if src := l["org.opencontainers.image.source"]; strings.HasPrefix(src, "https://github.com/") {
			link = strings.TrimSuffix(src, ".git") + "/commit/" + rev
		}
		return "sha-" + rev[:7], link
	}
	if v := l["org.opencontainers.image.version"]; v != "" {
		return v, ""
	}
	if _, tag, ok := strings.Cut(c.Image, ":"); ok {
		return tag, ""
	}
	return "—", ""
}

type crewCard struct {
	Name      string
	Role      string
	Stack     string
	Cond      condition
	Version   string
	Link      string
	Uptime    string
	Res       string
	Missing   []string
	Installed bool
	Self      bool
}

func (c crewCard) CanRestart() bool { return c.Installed && !c.Self && c.Cond.Level != "off" }
func (c crewCard) CanStop() bool    { return c.Installed && !c.Self && c.Cond.Running }
func (c crewCard) CanStart() bool   { return c.Installed && !c.Cond.Running && c.Cond.Level != "warn" }

type crewView struct {
	Cards   []crewCard
	Filter  string
	Stacks  []string
	Summary string
}

func (s *Server) crewCards(ctx context.Context) ([]crewCard, error) {
	list, err := s.managed(ctx)
	if err != nil {
		return nil, err
	}
	cfg := s.config()
	images := map[string]docker.Image{}
	cards := make([]crewCard, 0, len(list))
	have := map[string]bool{}
	for _, c := range list {
		name := c.Name()
		have[name] = true
		v, link := s.version(ctx, c, images)
		res := "—"
		if c.State == "running" {
			res = "…" // замер подгрузит panel.js из /api/crew/stats
		}
		cards = append(cards, crewCard{
			Name: name, Role: cfg.Roles[name], Stack: c.Project(), Cond: conditionOf(c),
			Version: v, Link: link, Uptime: uptimeOf(c.Status), Res: res,
			Missing: s.missing(name + ".env"), Installed: true, Self: name == s.Self,
		})
	}
	// Сервис с образцом секретов, но без контейнера: приехал в compose и ждёт ключей или первой установки.
	for _, svc := range s.exampleServices() {
		if have[svc] {
			continue
		}
		card := crewCard{Name: svc, Role: cfg.Roles[svc], Stack: "apps", Version: "—", Uptime: "—", Res: "—", Missing: s.missing(svc + ".env")}
		if len(card.Missing) > 0 {
			card.Cond = condition{Text: "ждёт секретов", Level: "warn"}
		} else {
			card.Cond = condition{Text: "ждёт установки: update.sh поставит его в течение 5 минут", Level: "warn"}
		}
		cards = append(cards, card)
	}
	order := map[string]int{"platform": 0, "apps": 1, "media-stack": 2}
	sort.SliceStable(cards, func(i, j int) bool {
		oi, oj := order[cards[i].Stack], order[cards[j].Stack]
		if oi != oj {
			return oi < oj
		}
		return cards[i].Name < cards[j].Name
	})
	return cards, nil
}

// statsOf — CPU и память работающих контейнеров: имя → «1,5 % · 64 МБ». Каждый замер Docker делает около секунды,
// поэтому параллельно и с общим пределом времени; страница Экипажа их не ждёт и берёт через /api/crew/stats.
func (s *Server) statsOf(ctx context.Context, list []docker.Container) map[string]string {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	out := map[string]string{}
	for _, c := range list {
		if c.State != "running" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := s.Docker.Stats(ctx, c.ID)
			if err != nil {
				return
			}
			res := fmt.Sprintf("%s %% · %s", strings.Replace(fmt.Sprintf("%.1f", st.CPUPercent), ".", ",", 1),
				humanSize(int64(st.MemBytes)))
			mu.Lock()
			out[c.Name()] = res
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

func (s *Server) apiCrewStats(w http.ResponseWriter, r *http.Request) {
	list, err := s.managed(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{})
		return
	}
	writeJSON(w, http.StatusOK, s.statsOf(r.Context(), list))
}

// uptimeOf — «Up 3 hours (healthy)» → «3 hours»: Docker уже посчитал, переводить не стоит усилий.
func uptimeOf(status string) string {
	rest, ok := strings.CutPrefix(status, "Up ")
	if !ok {
		return "—"
	}
	if i := strings.Index(rest, " ("); i > 0 {
		rest = rest[:i]
	}
	return translateUptime(rest)
}

var uptimeWords = strings.NewReplacer(
	"About a minute", "около минуты", "About an hour", "около часа", "Less than a second", "только что",
	" seconds", " с", " second", " с", " minutes", " мин", " minute", " мин", " hours", " ч", " hour", " ч",
	" days", " дн", " day", " дн", " weeks", " нед", " week", " нед", " months", " мес", " month", " мес",
)

func translateUptime(s string) string { return uptimeWords.Replace(s) }

func (s *Server) crew(w http.ResponseWriter, r *http.Request) {
	cards, err := s.crewCards(r.Context())
	if err != nil {
		s.render(w, r, "crew", http.StatusBadGateway, page{Title: "Экипаж", Active: "crew", Toast: "Docker не ответил: " + err.Error()})
		return
	}
	v := crewView{Filter: r.URL.Query().Get("stack"), Stacks: []string{"platform", "apps", "media-stack"}}
	var running, bad, waiting int
	for _, c := range cards {
		switch {
		case !c.Installed:
			waiting++
		case c.Cond.Problem:
			bad++
		case c.Cond.Running:
			running++
		}
		if v.Filter == "" || c.Stack == v.Filter {
			v.Cards = append(v.Cards, c)
		}
	}
	v.Summary = fmt.Sprintf("%d работают", running)
	if bad > 0 {
		v.Summary += fmt.Sprintf(" · упали: %d", bad)
	}
	if waiting > 0 {
		v.Summary += fmt.Sprintf(" · ждут секретов или установки: %d", waiting)
	}
	s.render(w, r, "crew", http.StatusOK, page{Title: "Экипаж", Active: "crew", Data: v})
}

func (s *Server) crewAction(w http.ResponseWriter, r *http.Request) {
	name, action := r.PathValue("name"), r.PathValue("action")
	c, err := s.find(r.Context(), name)
	if err != nil {
		s.Log.Warn("действие с чужим контейнером отклонено", "name", name, "action", action)
		http.Error(w, s.Voice.Line("cmd-denied", name)+" Контейнера "+name+" нет среди сервисов homelab.", http.StatusForbidden)
		return
	}
	if name == s.Self {
		http.Error(w, "Себя не вырубаю: перезапусти панель с сервера, docker restart "+name, http.StatusForbidden)
		return
	}
	var do func(context.Context, string) error
	var done, event string
	switch action {
	case "restart":
		do, done, event = s.Docker.Restart, "перезапущен", "cmd-restart"
	case "stop":
		do, done, event = s.Docker.Stop, "остановлен", "cmd-stopped"
	case "start":
		do, done, event = s.Docker.Start, "запущен", "cmd-start"
	default:
		http.Error(w, "неизвестное действие", http.StatusBadRequest)
		return
	}
	if err := do(r.Context(), c.ID); err != nil {
		back(w, r, "/crew", "Не вышло: "+err.Error())
		return
	}
	s.act(r.Context(), action, name, "", name+" "+done)
	toast := "Хорошие новости, все! " + name + " " + done + "."
	if line := s.Voice.Line(event, name); line != "" {
		toast = line
	}
	back(w, r, "/crew?stack="+r.PostFormValue("stack"), toast)
}

func (s *Server) updateNow(w http.ResponseWriter, r *http.Request) {
	if err := s.trigger("apply"); err != nil {
		back(w, r, safeBack(r.PostFormValue("back")), "Триггер не записан: "+err.Error())
		return
	}
	s.act(r.Context(), "update", "all", "", "запрошено обновление сервисов")
	back(w, r, safeBack(r.PostFormValue("back")), "Корабль вылетел за обновлениями. Итог придёт в Telegram.")
}

// safeBack — путь возврата только внутри панели.
func safeBack(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return "/deliveries"
	}
	return p
}
