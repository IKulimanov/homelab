// ops-bot — служебный Telegram-бот homelab: алерты, статистика, логи, перезапуск и остановка сервисов.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	// Отчёт и «сегодня» считаются в поясе TZ, а в distroless базы поясов нет.
	_ "time/tzdata"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"homelab/internal/docker"
	"homelab/internal/host"
	"homelab/internal/ops"
	"homelab/internal/voice"
)

// version подставляет сборка: -ldflags "-X main.version=sha-…".
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("ops-bot остановлен с ошибкой", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	token := os.Getenv("OPS_BOT_TOKEN")
	chatID, err := strconv.ParseInt(os.Getenv("OPS_CHAT_ID"), 10, 64)
	if token == "" || err != nil || chatID == 0 {
		return errors.New("нужны OPS_BOT_TOKEN и OPS_CHAT_ID")
	}
	th, err := thresholds()
	if err != nil {
		return err
	}
	report, err := ops.ParseSchedule(envOr("OPS_REPORT_AT", "Sun 20:00"))
	if err != nil {
		return err
	}
	cfg := ops.Config{
		ChatID:      chatID,
		Projects:    list(envOr("OPS_PROJECTS", "platform,apps,media-stack")),
		Protected:   []string{"ops-bot"},
		TriggerPath: envOr("OPS_TRIGGER", "/trigger/update"),
		Location:    time.Local,
		Report:      report,
		Thresholds:  th,
		// Остальные категории (mem, swap, load, battery, health, gateway) включаются этой же переменной.
		Alerts:       list(envOr("OPS_ALERTS", "temp,disk,service,llm")),
		HeartbeatURL: os.Getenv("HEALTHCHECK_URL"),
	}

	store, err := ops.OpenStore(envOr("OPS_DB", "/data/ops.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	dc := docker.New(envOr("DOCKER_SOCKET", "/var/run/docker.sock"))
	deps := ops.Deps{
		Docker: dc,
		Metrics: &host.Collector{
			Roots: host.Roots{Proc: "/proc", Sys: "/sys", FS: envOr("OPS_HOSTFS", "/hostfs")},
			Disks: list(envOr("OPS_DISKS", "/")),
		},
		Store: store,
		Voice: voice.New(os.Getenv("OPS_VOICE")),
		Log:   log,
	}
	// Комментарий к отчёту — через шлюз со своим ключом (install.sh llm-key ops-bot). Без ключа отчёт идёт
	// с запасной фразой.
	if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		deps.Commenter = &ops.GeminiCommenter{
			Base:  envOr("GEMINI_BASE_URL", "https://generativelanguage.googleapis.com"),
			Key:   key,
			Model: envOr("OPS_LLM_MODEL", "gemini-2.5-flash"),
			HTTP:  &http.Client{Timeout: time.Minute},
		}
	}
	if t := os.Getenv("LLM_ADMIN_TOKEN"); t != "" {
		deps.LLM = &ops.GatewayClient{
			Base:  envOr("LLM_GATEWAY_URL", "http://llm-gateway:8080"),
			Token: t,
			HTTP:  &http.Client{Timeout: 15 * time.Second},
		}
	}

	if t := os.Getenv("PANEL_TOKEN"); t != "" {
		pc := &ops.PanelClient{
			Base:  envOr("PANEL_INTERNAL_URL", "http://panel:8081"),
			Token: t,
			HTTP:  &http.Client{Timeout: 10 * time.Second},
		}
		deps.Panel = pc.LoginLink
	}

	var svc *ops.Service
	b, err := bot.New(token,
		bot.WithDefaultHandler(func(ctx context.Context, b *bot.Bot, u *models.Update) { handle(ctx, b, u, svc, chatID, log) }),
		bot.WithAllowedUpdates(bot.AllowedUpdates{"message", "callback_query"}),
	)
	if err != nil {
		return fmt.Errorf("создать бота: %w", err)
	}
	deps.Send = func(ctx context.Context, text string) error { return sendText(ctx, b, chatID, text, nil, false) }
	deps.SendGIF = func(ctx context.Context, fileID, caption string) error {
		_, err := b.SendAnimation(ctx, &bot.SendAnimationParams{
			ChatID: chatID, Animation: &models.InputFileString{Data: fileID}, Caption: caption,
		})
		return err
	}
	svc = ops.New(cfg, deps)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if _, err := b.SetMyCommands(ctx, &bot.SetMyCommandsParams{Commands: commands}); err != nil {
		log.Warn("меню команд не обновлено", "err", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Run(ctx, dc.Events)
	}()
	// Сообщения о запуске в чат нет: о новой версии и так пишет update.sh.
	log.Info("ops-bot запущен", "version", version, "projects", strings.Join(cfg.Projects, ","), "llm", deps.LLM != nil,
		"alerts", strings.Join(cfg.Alerts, ","), "comment", deps.Commenter != nil)
	b.Start(ctx)
	<-done
	return nil
}

var commands = []models.BotCommand{
	{Command: "status", Description: "сервисы, версии, алерты"},
	{Command: "stats", Description: "температура, память, диски"},
	{Command: "logs", Description: "лог сервиса: /logs <сервис> [строк]"},
	{Command: "restart", Description: "перезапустить сервис"},
	{Command: "stop", Description: "остановить сервис"},
	{Command: "start", Description: "запустить сервис"},
	{Command: "update", Description: "проверить обновления сейчас"},
	{Command: "usage", Description: "расход LLM"},
	{Command: "topup", Description: "записать пополнение Gemini"},
	{Command: "balance", Description: "баланс Gemini"},
	{Command: "gif", Description: "GIF для событий"},
	{Command: "panel", Description: "ссылка входа в панель"},
}

func handle(ctx context.Context, b *bot.Bot, u *models.Update, svc *ops.Service, ownChat int64, log *slog.Logger) {
	switch {
	case u.Message != nil:
		m := u.Message
		in := ops.Incoming{ChatID: m.Chat.ID, Text: m.Text}
		if in.Text == "" {
			in.Text = m.Caption
		}
		// Чужой чат ops.Service проигнорирует сам; файл из него не скачивается и не отправляется.
		if id, anim := gifOf(m); id != "" && m.Chat.ID == ownChat && ops.AboutGIF(in.Text) {
			if !anim {
				var err error
				if id, err = asAnimation(ctx, b, ownChat, id); err != nil {
					log.Warn("GIF не перезалита анимацией", "err", err)
					reply(ctx, b, ownChat, ops.Reply{Text: "⚠️ Не смог превратить файл в GIF: " + err.Error() +
						"\nПопробуй прислать GIF без галочки «Отправить как файл»."}, log)
					return
				}
			}
			in.GIF = id
		}
		for _, r := range svc.HandleMessage(ctx, in) {
			reply(ctx, b, m.Chat.ID, r, log)
		}
	case u.CallbackQuery != nil:
		q := u.CallbackQuery
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: q.ID})
		msg := q.Message.Message
		if msg == nil {
			return
		}
		// Кнопки убираются сразу: второе нажатие не должно остановить сервис ещё раз.
		_, _ = b.EditMessageReplyMarkup(ctx, &bot.EditMessageReplyMarkupParams{
			ChatID: msg.Chat.ID, MessageID: msg.ID,
			ReplyMarkup: &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}},
		})
		for _, r := range svc.Callback(ctx, msg.Chat.ID, q.Data) {
			reply(ctx, b, msg.Chat.ID, r, log)
		}
	}
}

// gifOf — GIF из сообщения или из того, на которое ответили. anim=false: GIF пришла файлом или видео.
// file_id такого файла sendAnimation не принимает, его нужно перезалить анимацией.
func gifOf(m *models.Message) (fileID string, anim bool) {
	for _, msg := range []*models.Message{m, m.ReplyToMessage} {
		switch {
		case msg == nil:
		case msg.Animation != nil:
			return msg.Animation.FileID, true
		case msg.Document != nil && (msg.Document.MimeType == "image/gif" || msg.Document.MimeType == "video/mp4"):
			return msg.Document.FileID, false
		case msg.Video != nil:
			return msg.Video.FileID, false
		}
	}
	return "", false
}

// maxGIFBytes — больше Bot API скачать не даёт.
const maxGIFBytes = 20 << 20

// asAnimation скачивает файл и отправляет его в чат анимацией, чтобы получить file_id анимации.
// Служебное сообщение с копией сразу удаляется: file_id остаётся рабочим.
func asAnimation(ctx context.Context, b *bot.Bot, chatID int64, fileID string) (string, error) {
	f, err := b.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return "", fmt.Errorf("Telegram не отдал файл: %w", err)
	}
	if f.FileSize > maxGIFBytes {
		return "", errors.New("файл больше 20 МБ")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.FileDownloadLink(f), nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		// В адресе скачивания токен бота: в текст ошибки и в лог он попасть не должен.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", fmt.Errorf("файл не скачался: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("файл не скачался: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxGIFBytes+1))
	if err != nil {
		return "", fmt.Errorf("файл не скачался: %w", err)
	}
	name := "bender.mp4"
	if strings.HasSuffix(strings.ToLower(f.FilePath), ".gif") {
		name = "bender.gif"
	}
	msg, err := b.SendAnimation(ctx, &bot.SendAnimationParams{
		ChatID: chatID, Animation: &models.InputFileUpload{Filename: name, Data: bytes.NewReader(data)},
		DisableNotification: true,
	})
	if err != nil {
		return "", fmt.Errorf("Telegram не принял анимацию: %w", err)
	}
	_, _ = b.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: chatID, MessageID: msg.ID})
	if msg.Animation == nil {
		return "", errors.New("Telegram сделал из файла видео, а не GIF: нужен ролик без звука")
	}
	return msg.Animation.FileID, nil
}

func reply(ctx context.Context, b *bot.Bot, chatID int64, r ops.Reply, log *slog.Logger) {
	var err error
	if r.File != nil {
		_, err = b.SendDocument(ctx, &bot.SendDocumentParams{
			ChatID:   chatID,
			Document: &models.InputFileUpload{Filename: r.File.Name, Data: bytes.NewReader(r.File.Data)},
			Caption:  r.Text,
		})
	} else {
		err = sendText(ctx, b, chatID, r.Text, r.Buttons, r.NoPreview)
	}
	if err != nil {
		log.Error("ответ не отправлен", "err", err)
	}
}

// sendText отправляет текст без разметки: в логах и именах встречаются символы, которые сломали бы HTML.
// Длинный текст режется по строкам на части в пределах лимита Telegram.
func sendText(ctx context.Context, b *bot.Bot, chatID int64, text string, buttons [][]ops.Button, noPreview bool) error {
	parts := ops.SplitText(text, 4000)
	for i, p := range parts {
		params := &bot.SendMessageParams{ChatID: chatID, Text: p}
		if noPreview {
			params.LinkPreviewOptions = &models.LinkPreviewOptions{IsDisabled: bot.True()}
		}
		if i == len(parts)-1 && len(buttons) > 0 {
			params.ReplyMarkup = keyboard(buttons)
		}
		if _, err := b.SendMessage(ctx, params); err != nil {
			return err
		}
	}
	return nil
}

func keyboard(buttons [][]ops.Button) *models.InlineKeyboardMarkup {
	rows := make([][]models.InlineKeyboardButton, 0, len(buttons))
	for _, row := range buttons {
		r := make([]models.InlineKeyboardButton, 0, len(row))
		for _, btn := range row {
			r = append(r, models.InlineKeyboardButton{Text: btn.Text, CallbackData: btn.Data})
		}
		rows = append(rows, r)
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// thresholds — пороги по умолчанию, поверх них переменные вида OPS_CPU_TEMP=75,90.
func thresholds() (ops.Thresholds, error) {
	th := ops.DefaultThresholds()
	for name, r := range map[string]*ops.Range{
		"OPS_CPU_TEMP": &th.CPUTemp, "OPS_SSD_TEMP": &th.SSDTemp, "OPS_MEM": &th.Mem, "OPS_SWAP": &th.Swap,
		"OPS_DISK": &th.Disk, "OPS_LOAD": &th.Load, "OPS_BATTERY": &th.Battery,
	} {
		v := os.Getenv(name)
		if v == "" {
			continue
		}
		w, c, ok := strings.Cut(v, ",")
		warn, err1 := strconv.ParseFloat(strings.TrimSpace(w), 64)
		crit, err2 := strconv.ParseFloat(strings.TrimSpace(c), 64)
		if !ok || err1 != nil || err2 != nil {
			return th, fmt.Errorf("%s=%q: нужно «внимание,критично», например 75,90", name, v)
		}
		*r = ops.Range{Warn: warn, Crit: crit}
	}
	return th, nil
}

func list(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
