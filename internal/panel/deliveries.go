package panel

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"homelab/internal/envfile"
)

var shaTag = regexp.MustCompile(`^sha-[0-9a-f]{7}$`)

// ownStacks — стеки, где версии собираем мы и откат через тег имеет смысл.
var ownStacks = []string{"platform", "apps"}

type pin struct {
	Svc string
	Tag string
}

type deliveryRow struct {
	delivery
	CanRollback bool
}

type deliveriesView struct {
	Running  bool
	LastRun  lastRun
	Pins     []pin
	Rows     []deliveryRow
	Backups  []backupFile
	Freezing bool
}

// Ship и Flame — режим корабля для шаблона ship.
func (v deliveriesView) Ship() string {
	if v.Running {
		return "flying"
	}
	return "idle"
}

func (v deliveriesView) Flame() string { return "mid" }

func (lr lastRun) StartedAt() time.Time {
	if lr.Started == nil {
		return time.Time{}
	}
	return time.Unix(*lr.Started, 0)
}

func (s *Server) pins() []pin {
	f, err := envfile.Read(filepath.Join(s.Paths.Secrets, "homelab.env"))
	if err != nil {
		return nil
	}
	var out []pin
	for _, svc := range s.ownServices() {
		if tag, _ := f.Get(tagVar(svc)); tag != "" && tag != "main" {
			out = append(out, pin{Svc: svc, Tag: tag})
		}
	}
	return out
}

// ownServices — сервисы своих стеков, которые встречались в истории доставок.
func (s *Server) ownServices() []string {
	var out []string
	for _, d := range s.history() {
		if slices.Contains(ownStacks, d.Stack) && !slices.Contains(out, d.Svc) {
			out = append(out, d.Svc)
		}
	}
	slices.Sort(out)
	return out
}

func (s *Server) deliveries(w http.ResponseWriter, r *http.Request) {
	v := deliveriesView{Running: s.running(), LastRun: s.lastRun(), Pins: s.pins(), Backups: s.backups()}
	_, v.Freezing = s.triggerPending("backup")
	h := s.history()
	for i, d := range h {
		if i == 100 {
			break
		}
		v.Rows = append(v.Rows, deliveryRow{delivery: d,
			CanRollback: d.Result == "ok" && slices.Contains(ownStacks, d.Stack) && shaTag.MatchString(d.From) && d.From != d.To})
	}
	s.render(w, r, "deliveries", http.StatusOK, page{Title: "Доставки", Active: "deliveries", Data: v})
}

func (s *Server) triggerPending(name string) (string, bool) {
	p := filepath.Join(s.Paths.Trigger, name)
	_, err := os.Stat(p)
	return p, err == nil
}

// rollback закрепляет версию: <SVC>_TAG=sha-… в homelab.env. update.sh скачает образ из ghcr и пересоздаст сервис.
func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	svc, to := r.PostFormValue("svc"), r.PostFormValue("to")
	if !slices.Contains(s.ownServices(), svc) || !shaTag.MatchString(to) {
		http.Error(w, "откатить можно только свой сервис на версию sha-xxxxxxx", http.StatusBadRequest)
		return
	}
	if err := s.setTag(svc, to); err != nil {
		back(w, r, "/deliveries", "Не записано: "+err.Error())
		return
	}
	s.act(r.Context(), "rollback", svc, to, svc+" закреплён на "+to)
	if err := s.trigger("apply"); err != nil {
		back(w, r, "/deliveries", "Закреплено, но update.sh не запущен: "+err.Error())
		return
	}
	back(w, r, "/deliveries", "Назад в прошлое! "+svc+" поедет на "+to+" в ближайшую минуту.")
}

func (s *Server) unpin(w http.ResponseWriter, r *http.Request) {
	svc := r.PostFormValue("svc")
	if !slices.Contains(s.ownServices(), svc) {
		http.Error(w, "нет такого сервиса", http.StatusBadRequest)
		return
	}
	if err := s.setTag(svc, ""); err != nil {
		back(w, r, "/deliveries", "Не записано: "+err.Error())
		return
	}
	s.act(r.Context(), "unpin", svc, "", svc+" снова на main")
	if err := s.trigger("apply"); err != nil {
		back(w, r, "/deliveries", "Записано, но update.sh не запущен: "+err.Error())
		return
	}
	back(w, r, "/deliveries", svc+" возвращается на main.")
}

func (s *Server) setTag(svc, tag string) error {
	path := filepath.Join(s.Paths.Secrets, "homelab.env")
	f, err := envfile.Read(path)
	if err != nil {
		return err
	}
	if err := f.Set(tagVar(svc), tag); err != nil {
		return err
	}
	return envfile.WriteFile(path, f.Bytes())
}

func (s *Server) freeze(w http.ResponseWriter, r *http.Request) {
	if err := s.trigger("backup"); err != nil {
		back(w, r, "/deliveries", "Триггер не записан: "+err.Error())
		return
	}
	s.act(r.Context(), "backup", "all", "", "запрошена копия всех баз")
	back(w, r, "/deliveries", "Кладём базы в морозильник. Фрай пролежал тысячу лет, им хватит пары минут.")
}

func (s *Server) apiRun(w http.ResponseWriter, r *http.Request) {
	lr := s.lastRun()
	_, freezing := s.triggerPending("backup")
	writeJSON(w, http.StatusOK, map[string]any{"running": s.running(), "finished": lr.Finished, "code": lr.Code, "freezing": freezing})
}
