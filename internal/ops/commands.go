package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"homelab/internal/docker"
)

const (
	defaultLogLines = 50
	maxLogLines     = 2000
	// maxText — сколько символов лога слать текстом. Предел Telegram 4096, остальное — заголовок.
	maxText = 3500
)

const help = `Команды:
/status — сервисы, версии, активные алерты
/stats — температура, память, диски, батарея; мин/макс за день
/logs <сервис> [строк] — последние строки лога, по умолчанию 50
/restart <сервис> — перезапустить
/stop <сервис> — остановить, с подтверждением
/start <сервис> — запустить остановленный
/update — проверить обновления сейчас
/usage — расход LLM за месяц и за сегодня
/topup <сумма> — записать пополнение Gemini в долларах
/balance [сумма] — остаток; с суммой — сверка с AI Studio
/gif — GIF по событиям; добавить — просто пришли GIF боту
/panel — ссылка входа в панель «Планета Экспресс»`

// Incoming — сообщение из чата. GIF — file_id анимации из самого сообщения или из того, на которое ответили;
// GIF файлом или видео ops-bot заранее перезаливает анимацией.
type Incoming struct {
	ChatID int64
	Text   string
	GIF    string
}

func (s *Service) Handle(ctx context.Context, chatID int64, text string) []Reply {
	return s.HandleMessage(ctx, Incoming{ChatID: chatID, Text: text})
}

// HandleMessage выполняет команду из чата. Сообщения не из OPS_CHAT_ID молча игнорируются:
// бот управляет сервером, отвечать посторонним нельзя даже отказом.
func (s *Service) HandleMessage(ctx context.Context, in Incoming) []Reply {
	if in.ChatID != s.cfg.ChatID {
		s.log.Warn("сообщение из чужого чата проигнорировано", "chat_id", in.ChatID)
		return nil
	}
	cmd, args := parseCommand(in.Text)
	if in.GIF != "" {
		s.mu.Lock()
		s.lastGIF = in.GIF
		s.mu.Unlock()
	}
	var r Reply
	switch cmd {
	case "status":
		r = s.cmdStatus(ctx)
	case "stats":
		r = s.cmdStats(ctx)
	case "logs":
		r = s.cmdLogs(ctx, args)
	case "restart":
		r = s.withContainer(ctx, args, s.restart)
	case "start":
		if len(args) == 0 {
			r = Reply{Text: help}
		} else {
			r = s.withContainer(ctx, args, s.start)
		}
	case "stop":
		r = s.withContainer(ctx, args, s.askStop)
	case "update":
		r = s.cmdUpdate()
	case "usage":
		r = s.cmdUsage(ctx)
	case "topup":
		r = s.cmdLedger(ctx, "topup", args)
	case "balance":
		r = s.cmdLedger(ctx, "set", args)
	case "gif":
		r = s.cmdGIF(ctx, args, in.GIF)
	case "panel":
		r = s.cmdPanel(ctx)
	case "":
		if in.GIF != "" {
			r = Reply{Text: "🎬 GIF получил. Когда её показывать?", Buttons: gifButtons()}
			break
		}
		r = Reply{Text: s.say(help, "help", "")}
	default:
		r = Reply{Text: s.say(help, "help", "")}
	}
	return []Reply{r}
}

// Callback — нажатие кнопки: подтверждение остановки или выбор события для GIF.
func (s *Service) Callback(ctx context.Context, chatID int64, data string) []Reply {
	if chatID != s.cfg.ChatID {
		s.log.Warn("кнопка из чужого чата проигнорирована", "chat_id", chatID)
		return nil
	}
	if event, ok := strings.CutPrefix(data, "gif:"); ok {
		return []Reply{s.cmdGIF(ctx, []string{event}, "")}
	}
	name, ok := strings.CutPrefix(data, "stop:")
	if !ok {
		return []Reply{{Text: "Отменено."}}
	}
	return []Reply{s.withContainer(ctx, []string{name}, s.stop)}
}

// AboutGIF — сообщение с GIF относится к GIF: подписи нет, она не команда или это команда /gif.
// Ответ «/logs budget-bot» на сообщение с GIF к ней не относится.
func AboutGIF(text string) bool {
	cmd, _ := parseCommand(text)
	return cmd == "" || cmd == "gif"
}

// parseCommand: «/logs@ops_bot budget-bot 100» → «logs», [budget-bot 100].
func parseCommand(text string) (string, []string) {
	f := strings.Fields(text)
	if len(f) == 0 || !strings.HasPrefix(f[0], "/") {
		return "", nil
	}
	cmd, _, _ := strings.Cut(strings.TrimPrefix(f[0], "/"), "@")
	return strings.ToLower(cmd), f[1:]
}

var (
	errNotFound = errors.New("не найден")
	errForeign  = errors.New("не из проектов homelab, им бот не управляет")
)

// find ищет контейнер по имени или по имени сервиса compose, только среди своих проектов.
func (s *Service) find(ctx context.Context, name string) (docker.Container, error) {
	list, err := s.docker.List(ctx)
	if err != nil {
		return docker.Container{}, err
	}
	for _, c := range list {
		if c.Name() == name || c.Labels[docker.LabelService] == name {
			if !s.managed(c.Project()) {
				return docker.Container{}, fmt.Errorf("%s %w", name, errForeign)
			}
			return c, nil
		}
	}
	return docker.Container{}, fmt.Errorf("%s %w; список — /status", name, errNotFound)
}

func (s *Service) withContainer(ctx context.Context, args []string, fn func(context.Context, docker.Container) Reply) Reply {
	if len(args) == 0 {
		return Reply{Text: "Нужно имя сервиса, например: /restart budget-bot"}
	}
	c, err := s.find(ctx, args[0])
	if errors.Is(err, errForeign) {
		return Reply{Text: s.say(err.Error(), "cmd-denied", args[0])}
	}
	if err != nil {
		return Reply{Text: err.Error()}
	}
	return fn(ctx, c)
}

func (s *Service) markKill(name string) {
	s.mu.Lock()
	s.kills[name] = s.now()
	s.mu.Unlock()
}

func (s *Service) restart(ctx context.Context, c docker.Container) Reply {
	s.markKill(c.Name())
	if err := s.docker.Restart(ctx, c.ID); err != nil {
		return Reply{Text: fmt.Sprintf("%s не перезапущен: %v", c.Name(), err)}
	}
	return Reply{Text: s.say(c.Name()+" перезапущен. Лог: /logs "+c.Name(), "cmd-restart", c.Name())}
}

func (s *Service) start(ctx context.Context, c docker.Container) Reply {
	if err := s.docker.Start(ctx, c.ID); err != nil {
		return Reply{Text: fmt.Sprintf("%s не запущен: %v", c.Name(), err)}
	}
	return Reply{Text: s.say(c.Name()+" запущен.", "cmd-start", c.Name())}
}

func (s *Service) askStop(_ context.Context, c docker.Container) Reply {
	if slices.Contains(s.cfg.Protected, c.Name()) {
		return Reply{Text: c.Name() + " из чата не останавливается: после этого бот не сможет его запустить."}
	}
	return Reply{
		Text:    s.say("Остановить "+c.Name()+"? Пока он остановлен, обновления его не запускают.", "cmd-stop-ask", c.Name()),
		Buttons: [][]Button{{{Text: "Вырубай", Data: "stop:" + c.Name()}, {Text: "Отставить", Data: "cancel"}}},
	}
}

func (s *Service) stop(ctx context.Context, c docker.Container) Reply {
	if slices.Contains(s.cfg.Protected, c.Name()) {
		return Reply{Text: c.Name() + " из чата не останавливается."}
	}
	s.markKill(c.Name())
	if err := s.docker.Stop(ctx, c.ID); err != nil {
		return Reply{Text: fmt.Sprintf("%s не остановлен: %v", c.Name(), err)}
	}
	return Reply{Text: s.say(c.Name()+" остановлен. Запустить: /start "+c.Name(), "cmd-stopped", c.Name())}
}

func (s *Service) cmdLogs(ctx context.Context, args []string) Reply {
	if len(args) == 0 {
		return Reply{Text: "Нужно имя сервиса: /logs budget-bot [строк]"}
	}
	lines := defaultLogLines
	if len(args) > 1 {
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 {
			return Reply{Text: "Число строк — целое больше нуля."}
		}
		lines = min(n, maxLogLines)
	}
	c, err := s.find(ctx, args[0])
	if err != nil {
		return Reply{Text: err.Error()}
	}
	insp, err := s.docker.Inspect(ctx, c.ID)
	if err != nil {
		return Reply{Text: err.Error()}
	}
	data, err := s.docker.Logs(ctx, c.ID, lines, insp.Config.Tty)
	if err != nil {
		return Reply{Text: fmt.Sprintf("Лог %s не прочитан: %v", c.Name(), err)}
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return Reply{Text: "Лог " + c.Name() + " пуст."}
	}
	if utf8.RuneCountInString(text) > maxText {
		return Reply{
			Text: fmt.Sprintf("%s, последние %d строк — файлом.", c.Name(), lines),
			File: &File{Name: c.Name() + ".log", Data: data},
		}
	}
	return Reply{Text: c.Name() + ":\n" + text}
}

// cmdUpdate кладёт файл-триггер; homelab-update.path на хосте видит его и запускает update.sh all.
func (s *Service) cmdUpdate() Reply {
	if s.cfg.TriggerPath == "" {
		return Reply{Text: "Триггер обновления не настроен (OPS_TRIGGER)."}
	}
	if err := os.WriteFile(s.cfg.TriggerPath, []byte(s.now().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return Reply{Text: "Не удалось запустить обновление: " + err.Error()}
	}
	return Reply{Text: s.say("Проверка обновлений запущена. Если что-то обновится, итог придёт отдельным сообщением.", "cmd-update", "")}
}

func (s *Service) cmdPanel(ctx context.Context) Reply {
	if s.panel == nil {
		return Reply{Text: "Панель не настроена: нужен PANEL_TOKEN в /srv/secrets/homelab.env."}
	}
	link, err := s.panel(ctx)
	if err != nil {
		s.log.Warn("ссылка входа в панель", "err", err)
		return Reply{Text: "Панель не ответила: " + err.Error()}
	}
	return Reply{Text: "Вход в штаб, ссылка живёт 5 минут и работает один раз:\n" + link, NoPreview: true}
}

func (s *Service) cmdStatus(ctx context.Context) Reply {
	list, err := s.docker.List(ctx)
	if err != nil {
		return Reply{Text: "Docker не ответил: " + err.Error()}
	}
	var b strings.Builder
	active := s.alerts.Active()
	allRunning := true
	if len(active) > 0 {
		b.WriteString("Активные алерты:\n")
		for _, a := range active {
			fmt.Fprintf(&b, "- %s: %s\n", a.Level, a.Text)
		}
		b.WriteString("\n")
	}

	byProject := map[string][]docker.Container{}
	for _, c := range list {
		if s.managed(c.Project()) {
			byProject[c.Project()] = append(byProject[c.Project()], c)
		}
	}
	for _, p := range s.cfg.Projects {
		cs := byProject[p]
		if len(cs) == 0 {
			continue
		}
		sort.Slice(cs, func(i, j int) bool { return cs[i].Name() < cs[j].Name() })
		b.WriteString(p + "\n")
		for _, c := range cs {
			fmt.Fprintf(&b, "  %s — %s, %s\n", c.Name(), s.stateLine(ctx, c), s.version(ctx, c))
			allRunning = allRunning && c.State == "running"
		}
	}
	if b.Len() == 0 {
		return Reply{Text: "Контейнеров homelab нет."}
	}
	event := "status-bad"
	if allRunning && len(active) == 0 {
		event = "status-ok"
	}
	return Reply{Text: s.say(strings.TrimSpace(b.String()), event, "")}
}

func (s *Service) stateLine(ctx context.Context, c docker.Container) string {
	insp, err := s.docker.Inspect(ctx, c.ID)
	if err != nil {
		return c.State
	}
	switch {
	case insp.State.Restarting:
		return fmt.Sprintf("перезапускается (перезапусков %d)", insp.RestartCount)
	case insp.State.Running:
		line := "работает " + since(s.now().Sub(insp.State.StartedAt))
		if h := insp.Health(); h != "" {
			line += ", " + h
		}
		return line
	default:
		return fmt.Sprintf("остановлен %s назад, код %d", since(s.now().Sub(insp.State.FinishedAt)), insp.State.ExitCode)
	}
}

// cmdGIF: «/gif temp» — запомнить GIF из сообщения, из ответа или последнюю присланную; «/gif» — список;
// «/gif clear temp» — забыть все GIF события.
func (s *Service) cmdGIF(ctx context.Context, args []string, fileID string) Reply {
	if len(args) == 0 {
		counts, err := s.store.GIFCounts(ctx)
		if err != nil {
			return Reply{Text: "⚠️ Не прочитал список GIF: " + err.Error()}
		}
		var b strings.Builder
		b.WriteString("🎬 GIF по событиям\n\n")
		for _, e := range gifEvents {
			fmt.Fprintf(&b, "%s (%s): %d\n", gifLabels[e], e, counts[e])
		}
		b.WriteString("\n➕ Добавить: просто пришли GIF и выбери событие кнопкой.\n🗑 Убрать все GIF события: /gif clear <событие>")
		return Reply{Text: b.String()}
	}
	events := strings.Join(gifEvents, ", ")
	if args[0] == "clear" {
		if len(args) < 2 || !slices.Contains(gifEvents, args[1]) {
			return Reply{Text: "🤔 Какое событие очистить? Есть: " + events}
		}
		n, err := s.store.ClearGIFs(ctx, args[1])
		if err != nil {
			return Reply{Text: "⚠️ Не очистил: " + err.Error()}
		}
		return Reply{Text: fmt.Sprintf("🗑 %s: убрал GIF — %d.", gifLabels[args[1]], n)}
	}
	event := args[0]
	if !slices.Contains(gifEvents, event) {
		return Reply{Text: "🤔 Нет такого события. Есть: " + events}
	}
	if fileID == "" {
		s.mu.Lock()
		fileID = s.lastGIF
		s.mu.Unlock()
	}
	if fileID == "" {
		return Reply{Text: "🎬 Не вижу GIF. Пришли её ещё раз — и выбери событие кнопкой."}
	}
	if err := s.store.AddGIF(ctx, event, fileID); err != nil {
		return Reply{Text: "⚠️ GIF не сохранилась: " + err.Error()}
	}
	ids, _ := s.store.GIFs(ctx, event)
	return Reply{Text: fmt.Sprintf("✅ Запомнил для «%s». GIF у события: %d.", gifLabels[event], len(ids))}
}

// gifButtons — кнопки выбора события, по две в ряд.
func gifButtons() [][]Button {
	var rows [][]Button
	for i, e := range gifEvents {
		b := Button{Text: gifLabels[e], Data: "gif:" + e}
		if i%2 == 0 {
			rows = append(rows, []Button{b})
		} else {
			rows[len(rows)-1] = append(rows[len(rows)-1], b)
		}
	}
	return rows
}
