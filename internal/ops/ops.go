// Package ops — служебный бот homelab: алерты о сервере и контейнерах, команды управления, отчёт за день.
// Здесь вся логика без Telegram: команды получают текст и возвращают ответы, отправку делает cmd/ops-bot.
package ops

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"homelab/internal/alert"
	"homelab/internal/docker"
	"homelab/internal/gateway"
	"homelab/internal/host"
	"homelab/internal/voice"
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

// Commenter пишет пару фраз персонажа к готовому отчёту. Цифры в отчёте считает код, не LLM.
type Commenter interface {
	Comment(ctx context.Context, prompt, report string) (string, error)
}

type Config struct {
	ChatID int64
	// Projects — compose-проекты, которыми бот управляет. Чужие контейнеры он не видит и не трогает.
	Projects []string
	// Protected — контейнеры, которые нельзя остановить из чата: без ops-bot не будет и команды /start.
	Protected   []string
	TriggerPath string
	Location    *time.Location
	Report      Schedule // пустой At — отчёта нет
	Thresholds  Thresholds
	// Alerts — включённые категории алертов (см. category). nil — все.
	Alerts []string
	// HeartbeatURL — адрес внешнего пульса (healthchecks.io). Пусто — пульса нет.
	HeartbeatURL string
}

// Reply — ответ на команду. Отправку делает адаптер Telegram.
type Reply struct {
	Text    string
	File    *File
	Buttons [][]Button
	// NoPreview — без превью ссылки: Telegram открыл бы одноразовую ссылку входа сам и потратил её.
	NoPreview bool
}

type File struct {
	Name string
	Data []byte
}

type Button struct{ Text, Data string }

type Service struct {
	cfg       Config
	docker    Docker
	llm       LLM // nil — шлюз не настроен
	metrics   Metrics
	store     *Store
	alerts    *alert.Engine
	voice     *voice.Voice // nil — без фраз
	commenter Commenter    // nil — отчёт без LLM
	send      func(ctx context.Context, text string) error
	sendGIF   func(ctx context.Context, fileID, caption string) error
	panel     func(ctx context.Context) (string, error) // nil — панель не настроена
	log       *slog.Logger
	now       func() time.Time

	mu     sync.Mutex
	kills  map[string]time.Time // когда контейнер останавливали штатно: его die — не авария
	images map[string]string    // id образа → версия; образы не меняются, кэш вечный
	last   host.Snapshot
	ticks  int
	// lastGIF — последняя присланная GIF: её сохраняет кнопка события или /gif без ответа на сообщение.
	// Только в памяти: после перезапуска бота GIF просто присылают ещё раз.
	lastGIF string
}

type Deps struct {
	Docker    Docker
	LLM       LLM
	Metrics   Metrics
	Store     *Store
	Voice     *voice.Voice
	Commenter Commenter
	Send      func(ctx context.Context, text string) error
	// SendGIF — GIF по file_id Telegram с подписью. nil — GIF не шлются.
	SendGIF func(ctx context.Context, fileID, caption string) error
	// Panel — одноразовая ссылка входа в панель. nil — команда /panel отвечает, что панели нет.
	Panel func(ctx context.Context) (string, error)
	Log   *slog.Logger
	Now   func() time.Time
}

func New(cfg Config, d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	eng := alert.NewEngine(time.Hour)
	eng.Now = d.Now
	return &Service{
		cfg: cfg, docker: d.Docker, llm: d.LLM, metrics: d.Metrics, store: d.Store, send: d.Send, log: d.Log, now: d.Now,
		voice: d.Voice, commenter: d.Commenter, sendGIF: d.SendGIF, panel: d.Panel,
		alerts: eng,
		kills:  map[string]time.Time{},
		images: map[string]string{},
	}
}

func (s *Service) managed(project string) bool { return slices.Contains(s.cfg.Projects, project) }

// alert передаёт состояние движку и отправляет сообщение, если движок решил, что пора.
// event — имя файла фраз (temp, died, ...). Первая строка сообщения — факт, фраза персонажа идёт после.
func (s *Service) alert(ctx context.Context, event, key string, level alert.Level, text string) {
	if !s.enabled(category(key)) {
		return
	}
	m := s.alerts.Update(key, level, text)
	if m.Text == "" {
		return
	}
	s.log.Info("алерт", "key", key, "level", level.String())
	name := key
	if _, after, ok := strings.Cut(key, ":"); ok {
		name = after
	}
	var line, gif string
	switch m.Kind {
	case alert.Repeat:
		// Повтор без GIF: персонаж злится сильнее с каждым разом.
		line = s.voice.Line(fmt.Sprintf("repeat-%d", min(m.Repeat, 3)), name)
	case alert.Recovered:
		line, gif = s.voice.Line("recovered", name), "recovered"
	default:
		line, gif = s.voice.Line(event, name), gifEvent(event)
	}
	if err := s.notify(ctx, withLine(m.Text, line), gif); err != nil {
		s.log.Error("алерт не отправлен", "key", key, "err", err)
	}
}

// category — группа алерта для фильтра OPS_ALERTS.
func category(key string) string {
	switch {
	case key == "cpu-temp" || key == "ssd-temp":
		return "temp"
	case strings.HasPrefix(key, "disk:"):
		return "disk"
	case strings.HasPrefix(key, "container:") || key == "docker":
		return "service"
	case strings.HasPrefix(key, "health:"):
		return "health"
	case key == "power":
		return "battery"
	case key == "llm:gateway" || key == "llm:prices":
		return "gateway"
	case strings.HasPrefix(key, "llm:"):
		return "llm"
	}
	return key // mem, swap, load
}

func (s *Service) enabled(cat string) bool {
	return s.cfg.Alerts == nil || slices.Contains(s.cfg.Alerts, cat)
}

// gifEvents — события, к которым можно привязать GIF командой /gif.
var gifEvents = []string{"temp", "disk", "died", "recovered", "update-ok", "update-fail", "llm-limit", "balance", "report", "backup-fail"}

// gifLabels — подписи кнопок выбора события для GIF.
var gifLabels = map[string]string{
	"temp": "🔥 Перегрев", "disk": "💾 Мало места", "died": "💀 Сервис упал", "recovered": "🎉 Починилось",
	"update-ok": "🚀 Обновление", "update-fail": "💥 Обновление сломалось", "llm-limit": "💸 Лимит LLM",
	"balance": "💰 Баланс Gemini", "report": "📊 Отчёт за неделю", "backup-fail": "🧊 Бэкап не сделан",
}

// gifEvent — общий GIF для близких событий: любая остановка сервиса — «died».
func gifEvent(event string) string {
	switch event {
	case "oom", "restart-loop", "docker":
		return "died"
	}
	return event
}

// activeLines — активные алерты списком: значок уровня и первая строка, без совета «что делать».
func activeLines(active []alert.Active) string {
	var b strings.Builder
	for _, a := range active {
		first, _, _ := strings.Cut(a.Text, "\n")
		fmt.Fprintf(&b, "%s %s\n", a.Level.Icon(), first)
	}
	return b.String()
}

func withLine(text, line string) string {
	if line == "" {
		return text
	}
	return text + "\n\n🤖 " + line
}

// say — ответ на команду с фразой персонажа.
func (s *Service) say(text, event, name string) string {
	return withLine(text, s.voice.Line(event, name))
}

// maxCaption — предел подписи к GIF в Telegram 1024 символа; длиннее — GIF без подписи и текст отдельно.
const maxCaption = 1000

// notify шлёт текст, с GIF события, если она есть. GIF не ушла — тот же текст обычным сообщением.
func (s *Service) notify(ctx context.Context, text, gif string) error {
	if id := s.pickGIF(ctx, gif); id != "" {
		caption, rest := text, ""
		if utf8.RuneCountInString(text) > maxCaption {
			caption, rest = "", text
		}
		err := s.sendGIF(ctx, id, caption)
		if err == nil && rest == "" {
			return nil
		}
		if err != nil {
			s.log.Warn("GIF не отправлен, шлю текст", "event", gif, "err", err)
		}
	}
	return s.send(ctx, text)
}

func (s *Service) pickGIF(ctx context.Context, event string) string {
	if event == "" || s.sendGIF == nil {
		return ""
	}
	ids, err := s.store.GIFs(ctx, event)
	if err != nil || len(ids) == 0 {
		return ""
	}
	return ids[rand.IntN(len(ids))]
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
