// Package ops — служебный бот homelab: алерты о сервере и контейнерах, команды управления, отчёт за день.
// Здесь вся логика без Telegram: команды получают текст и возвращают ответы, отправку делает cmd/ops-bot.
package ops

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"homelab/internal/alert"
	"homelab/internal/docker"
	"homelab/internal/gateway"
	"homelab/internal/host"
)

// Docker — то, что нужно от Docker Engine API.
type Docker interface {
	List(ctx context.Context) ([]docker.Container, error)
	Inspect(ctx context.Context, id string) (docker.Inspect, error)
	Image(ctx context.Context, id string) (docker.Image, error)
	Restart(ctx context.Context, id string) error
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Logs(ctx context.Context, id string, tail int, tty bool) ([]byte, error)
}

// LLM — админ-API шлюза.
type LLM interface {
	Status(ctx context.Context) (gateway.Status, error)
	Usage(ctx context.Context, since time.Time) ([]gateway.UsageRow, error)
	Ledger(ctx context.Context, kind string, usd float64) (gateway.Balance, error)
}

type Metrics interface {
	Collect() host.Snapshot
}

// Range — порог внимания и критичный порог.
type Range struct{ Warn, Crit float64 }

// Thresholds — пороги алертов. По умолчанию те же, что были в simply-monitoring.
type Thresholds struct {
	CPUTemp Range // °C
	SSDTemp Range // °C
	Mem     Range // %
	Swap    Range // %
	Disk    Range // %
	Load    Range // load average за 5 минут на одно ядро
	Battery Range // % заряда при работе от батареи, меньше — хуже
}

func DefaultThresholds() Thresholds {
	return Thresholds{
		CPUTemp: Range{75, 90},
		SSDTemp: Range{55, 70},
		Mem:     Range{85, 95},
		Swap:    Range{50, 80},
		Disk:    Range{80, 90},
		Load:    Range{2, 4},
		Battery: Range{20, 10},
	}
}

type Config struct {
	ChatID int64
	// Projects — compose-проекты, которыми бот управляет. Чужие контейнеры он не видит и не трогает.
	Projects []string
	// Protected — контейнеры, которые нельзя остановить из чата: без ops-bot не будет и команды /start.
	Protected   []string
	TriggerPath string
	Location    *time.Location
	ReportAt    string // «23:55»
	Thresholds  Thresholds
	// HeartbeatURL — адрес внешнего пульса (healthchecks.io). Пусто — пульса нет.
	HeartbeatURL string
}

// Reply — ответ на команду. Отправку делает адаптер Telegram.
type Reply struct {
	Text    string
	File    *File
	Buttons [][]Button
}

type File struct {
	Name string
	Data []byte
}

type Button struct{ Text, Data string }

type Service struct {
	cfg     Config
	docker  Docker
	llm     LLM // nil — шлюз не настроен
	metrics Metrics
	store   *Store
	alerts  *alert.Engine
	send    func(ctx context.Context, text string) error
	log     *slog.Logger
	now     func() time.Time

	mu     sync.Mutex
	kills  map[string]time.Time // когда контейнер останавливали штатно: его die — не авария
	images map[string]string    // id образа → версия; образы не меняются, кэш вечный
	last   host.Snapshot
	ticks  int
}

type Deps struct {
	Docker  Docker
	LLM     LLM
	Metrics Metrics
	Store   *Store
	Send    func(ctx context.Context, text string) error
	Log     *slog.Logger
	Now     func() time.Time
}

func New(cfg Config, d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	eng := alert.NewEngine(30 * time.Minute)
	eng.Now = d.Now
	return &Service{
		cfg: cfg, docker: d.Docker, llm: d.LLM, metrics: d.Metrics, store: d.Store, send: d.Send, log: d.Log, now: d.Now,
		alerts: eng,
		kills:  map[string]time.Time{},
		images: map[string]string{},
	}
}

func (s *Service) managed(project string) bool { return slices.Contains(s.cfg.Projects, project) }

// alert передаёт состояние движку и отправляет сообщение, если движок решил, что пора.
func (s *Service) alert(ctx context.Context, key string, level alert.Level, text string) {
	msg := s.alerts.Update(key, level, text)
	if msg == "" {
		return
	}
	s.log.Info("алерт", "key", key, "level", level.String())
	if err := s.send(ctx, msg); err != nil {
		s.log.Error("алерт не отправлен", "key", key, "err", err)
	}
}

// version — короткая версия образа: sha коммита из CI, иначе метка version, иначе тег.
func (s *Service) version(ctx context.Context, c docker.Container) string {
	s.mu.Lock()
	v, ok := s.images[c.ImageID]
	s.mu.Unlock()
	if ok {
		return v
	}
	v = imageTag(c.Image)
	if img, err := s.docker.Image(ctx, c.ImageID); err == nil {
		if rev := img.Config.Labels["org.opencontainers.image.revision"]; len(rev) >= 7 {
			v = "sha-" + rev[:7]
		} else if ver := img.Config.Labels["org.opencontainers.image.version"]; ver != "" {
			v = ver
		}
	}
	s.mu.Lock()
	s.images[c.ImageID] = v
	s.mu.Unlock()
	return v
}

func imageTag(image string) string {
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[i+1:]
	}
	return "latest"
}

// since — «3 д 4 ч», «2 ч 5 мин», «45 мин».
func since(d time.Duration) string {
	d = d.Round(time.Minute)
	days, hours, mins := int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60
	switch {
	case days > 0:
		return fmt.Sprintf("%d д %d ч", days, hours)
	case hours > 0:
		return fmt.Sprintf("%d ч %d мин", hours, mins)
	default:
		return fmt.Sprintf("%d мин", mins)
	}
}

func bytesHuman(b uint64) string {
	const unit = 1024
	units := []string{"Б", "КБ", "МБ", "ГБ", "ТБ"}
	v := float64(b)
	i := 0
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	if i >= 3 {
		return fmt.Sprintf("%.1f %s", v, units[i])
	}
	return fmt.Sprintf("%.0f %s", v, units[i])
}

func num(v float64, format string) string {
	if math.IsNaN(v) {
		return "нет данных"
	}
	return fmt.Sprintf(format, v)
}

func dayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// SplitText режет текст на части не длиннее limit символов, по возможности по границе строки.
func SplitText(text string, limit int) []string {
	var parts []string
	r := []rune(text)
	for len(r) > limit {
		cut := limit
		for i := limit; i > limit/2; i-- {
			if r[i-1] == '\n' {
				cut = i
				break
			}
		}
		parts = append(parts, string(r[:cut]))
		r = r[cut:]
	}
	return append(parts, string(r))
}
