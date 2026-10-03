package panel

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"homelab/internal/envfile"
)

// panelConfig — stacks/platform/config/panel.yaml: подписи сервисов и ссылки «Развлечений».
type panelConfig struct {
	Roles map[string]string `yaml:"roles"`
	Fun   []funLink         `yaml:"fun"`
}

type funLink struct {
	Title     string `yaml:"title"`
	Hint      string `yaml:"hint"`
	Port      int    `yaml:"port"`
	Path      string `yaml:"path"`
	Container string `yaml:"container"`
}

// config читается на каждый запрос: файл маленький, а правка в git подхватывается без перезапуска.
func (s *Server) config() panelConfig {
	var c panelConfig
	data, err := os.ReadFile(filepath.Join(s.Paths.Homelab, "stacks/platform/config/panel.yaml"))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.Log.Warn("panel.yaml не прочитан", "err", err)
		}
		return c
	}
	if err := yaml.Unmarshal(data, &c); err != nil {
		s.Log.Warn("panel.yaml с ошибкой", "err", err)
	}
	return c
}

var secretsName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*\.env$`)

// secretsFiles — имена файлов в /srv/secrets: homelab.env первым, media.env последним, остальные по алфавиту.
func (s *Server) secretsFiles() []string {
	entries, err := os.ReadDir(s.Paths.Secrets)
	if err != nil {
		s.Log.Warn("каталог секретов не прочитан", "err", err)
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && secretsName.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	rank := func(n string) int {
		switch n {
		case "homelab.env":
			return 0
		case "media.env":
			return 2
		}
		return 1
	}
	sort.Slice(out, func(i, j int) bool {
		if rank(out[i]) != rank(out[j]) {
			return rank(out[i]) < rank(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// example — образец env/<name>.example из репозитория; нет образца — nil.
func (s *Server) example(name string) []byte {
	data, err := os.ReadFile(filepath.Join(s.Paths.Homelab, "env", name+".example"))
	if err != nil {
		return nil
	}
	return data
}

// missing — обязательные ключи образца, которые пусты в файле секретов.
func (s *Server) missing(name string) []string {
	req := envfile.Required(s.example(name))
	if len(req) == 0 {
		return nil
	}
	f, err := envfile.Read(filepath.Join(s.Paths.Secrets, name))
	if err != nil {
		return req
	}
	var out []string
	for _, k := range req {
		if v, _ := f.Get(k); v == "" {
			out = append(out, k)
		}
	}
	return out
}

// exampleServices — сервисы, у которых есть образец секретов: кандидаты в «ждёт секретов».
func (s *Server) exampleServices() []string {
	names, _ := filepath.Glob(filepath.Join(s.Paths.Homelab, "env", "*.env.example"))
	var out []string
	for _, n := range names {
		svc := strings.TrimSuffix(filepath.Base(n), ".env.example")
		if svc != "homelab" && svc != "media" {
			out = append(out, svc)
		}
	}
	slices.Sort(out)
	return out
}

// tagVar — переменная закреплённой версии сервиса в homelab.env: budget-bot → BUDGET_BOT_TAG.
func tagVar(svc string) string {
	return strings.ToUpper(strings.ReplaceAll(svc, "-", "_")) + "_TAG"
}

// delivery — строка history.jsonl, которую пишет update.sh.
type delivery struct {
	TS     int64  `json:"ts"`
	Stack  string `json:"stack"`
	Svc    string `json:"svc"`
	From   string `json:"from"`
	To     string `json:"to"`
	Result string `json:"result"` // ok, fail
	Reason string `json:"reason"` // image, config, image+config, install
	Text   string `json:"text"`
}

func (d delivery) Time() time.Time { return time.Unix(d.TS, 0) }

// history — доставки, новые сверху. Битые строки пропускаются: файл дописывается снаружи.
func (s *Server) history() []delivery {
	f, err := os.Open(filepath.Join(s.Paths.State, "history.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []delivery
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var d delivery
		if json.Unmarshal(sc.Bytes(), &d) == nil && d.Svc != "" {
			out = append(out, d)
		}
	}
	slices.Reverse(out)
	return out
}

// lastRun — last-run.json от update.sh: finished == nil — прогон идёт.
type lastRun struct {
	Started  *int64 `json:"started"`
	Finished *int64 `json:"finished"`
	Code     *int   `json:"code"`
	Policy   string `json:"policy"`
}

func (lr lastRun) Failed() bool { return lr.Code != nil && *lr.Code != 0 }

func (lr lastRun) ExitCode() int {
	if lr.Code == nil {
		return 0
	}
	return *lr.Code
}

func (s *Server) lastRun() lastRun {
	var lr lastRun
	data, err := os.ReadFile(filepath.Join(s.Paths.State, "last-run.json"))
	if err == nil {
		_ = json.Unmarshal(data, &lr)
	}
	return lr
}

// running — update.sh сейчас работает, или его попросили и он ещё не начал.
func (s *Server) running() bool {
	if _, err := os.Stat(filepath.Join(s.Paths.Trigger, "apply")); err == nil {
		return true
	}
	lr := s.lastRun()
	return lr.Started != nil && lr.Finished == nil
}

type backupFile struct {
	Svc  string `json:"svc"`
	File string `json:"file"`
	Size int64  `json:"size"`
	TS   int64  `json:"ts"`
}

func (b backupFile) Time() time.Time { return time.Unix(b.TS, 0) }

// backups — backups.json от backup.sh, новые сверху.
func (s *Server) backups() []backupFile {
	var out []backupFile
	data, err := os.ReadFile(filepath.Join(s.Paths.State, "backups.json"))
	if err == nil {
		_ = json.Unmarshal(data, &out)
	}
	return out
}

// trigger кладёт файл для юнита systemd: apply — update.sh auto, backup — backup.sh --all.
func (s *Server) trigger(name string) error {
	return os.WriteFile(filepath.Join(s.Paths.Trigger, name), []byte(s.Now().Format(time.RFC3339)+"\n"), 0o644)
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return trimFloat(float64(n)/(1<<30)) + " ГБ"
	case n >= 1<<20:
		return trimFloat(float64(n)/(1<<20)) + " МБ"
	case n >= 1<<10:
		return trimFloat(float64(n)/(1<<10)) + " КБ"
	}
	return fmt.Sprintf("%d Б", n)
}

// trimFloat — одна цифра после запятой, без лишнего нуля: 1,2 и 640.
func trimFloat(v float64) string {
	s := strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0")
	return strings.Replace(s, ".", ",", 1)
}
