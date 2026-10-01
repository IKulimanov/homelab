// ops-bot — служебный Telegram-бот homelab: алерты, статистика, логи, перезапуск и остановка сервисов.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	// Отчёт в 23:55 и «сегодня» считаются в поясе TZ, а в distroless базы поясов нет.
	_ "time/tzdata"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"homelab/internal/docker"
	"homelab/internal/host"
	"homelab/internal/ops"
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
	cfg := ops.Config{
		ChatID:       chatID,
		Projects:     list(envOr("OPS_PROJECTS", "platform,apps,media-stack")),
		Protected:    []string{"ops-bot"},
		TriggerPath:  envOr("OPS_TRIGGER", "/trigger/update"),
		Location:     time.Local,
		ReportAt:     envOr("OPS_REPORT_AT", "23:55"),
		Thresholds:   th,
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
		Log:   log,
	}
	if t := os.Getenv("LLM_ADMIN_TOKEN"); t != "" {
		deps.LLM = &ops.GatewayClient{
			Base:  envOr("LLM_GATEWAY_URL", "http://llm-gateway:8080"),
			Token: t,
			HTTP:  &http.Client{Timeout: 15 * time.Second},
		}
	}

	var svc *ops.Service
	b, err := bot.New(token,
		bot.WithDefaultHandler(func(ctx context.Context, b *bot.Bot, u *models.Update) { handle(ctx, b, u, svc, log) }),
		bot.WithAllowedUpdates(bot.AllowedUpdates{"message", "callback_query"}),
	)
	if err != nil {
		return fmt.Errorf("создать бота: %w", err)
	}
	deps.Send = func(ctx context.Context, text string) error { return sendText(ctx, b, chatID, text, nil) }
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
	log.Info("ops-bot запущен", "version", version, "projects", strings.Join(cfg.Projects, ","), "llm", deps.LLM != nil)
	_ = deps.Send(ctx, "ops-bot запущен, версия "+version)
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
}

func handle(ctx context.Context, b *bot.Bot, u *models.Update, svc *ops.Service, log *slog.Logger) {
	switch {
	case u.Message != nil:
		for _, r := range svc.Handle(ctx, u.Message.Chat.ID, u.Message.Text) {
			reply(ctx, b, u.Message.Chat.ID, r, log)
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

func reply(ctx context.Context, b *bot.Bot, chatID int64, r ops.Reply, log *slog.Logger) {
	var err error
	if r.File != nil {
		_, err = b.SendDocument(ctx, &bot.SendDocumentParams{
			ChatID:   chatID,
			Document: &models.InputFileUpload{Filename: r.File.Name, Data: bytes.NewReader(r.File.Data)},
			Caption:  r.Text,
		})
	} else {
		err = sendText(ctx, b, chatID, r.Text, r.Buttons)
	}
	if err != nil {
		log.Error("ответ не отправлен", "err", err)
	}
}

// sendText отправляет текст без разметки: в логах и именах встречаются символы, которые сломали бы HTML.
// Длинный текст режется по строкам на части в пределах лимита Telegram.
func sendText(ctx context.Context, b *bot.Bot, chatID int64, text string, buttons [][]ops.Button) error {
	parts := ops.SplitText(text, 4000)
	for i, p := range parts {
		params := &bot.SendMessageParams{ChatID: chatID, Text: p}
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
