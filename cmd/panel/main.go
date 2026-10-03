// panel — веб-панель homelab «Планета Экспресс». Публичный порт — страницы для домашней сети,
// внутренний — только для сети homelab: ссылка входа для ops-bot и healthcheck.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "time/tzdata"

	"homelab/internal/docker"
	"homelab/internal/ops"
	"homelab/internal/panel"
	"homelab/internal/voice"
)

// version подставляет сборка: -ldflags "-X main.version=sha-…".
var version = "dev"

func main() {
	healthcheck := flag.Bool("healthcheck", false, "проверить, что панель отвечает (для healthcheck в compose)")
	loginLink := flag.Bool("login-link", false, "напечатать одноразовую ссылку входа: запасной вход без Telegram")
	flag.Parse()
	internalAddr := envOr("PANEL_INTERNAL_ADDR", ":8081")
	if *healthcheck {
		os.Exit(probe(internalAddr))
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if *loginLink {
		if err := printLink(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := run(internalAddr, log); err != nil {
		log.Error("панель остановлена с ошибкой", "err", err)
		os.Exit(1)
	}
}

func server(log *slog.Logger) (*panel.Server, error) {
	store, err := panel.OpenStore(envOr("PANEL_DB", "/srv/panel/data/panel.db"))
	if err != nil {
		return nil, err
	}
	srv := envOr("PANEL_SRV", "/srv")
	homelab := envOr("PANEL_HOMELAB", "/homelab")
	s := &panel.Server{
		Docker: docker.New(envOr("DOCKER_SOCKET", "/var/run/docker.sock")),
		Store:  store,
		Voice:  voice.New(filepath.Join(homelab, "stacks/platform/config/voice")),
		Paths: panel.Paths{
			Srv:     srv,
			Secrets: filepath.Join(srv, "secrets"),
			Homelab: homelab,
			State:   envOr("PANEL_STATE", "/state"),
			Trigger: filepath.Join(srv, "panel/trigger"),
			DBCopy:  filepath.Join(srv, "panel/data/db-backups"),
			OpsDB:   filepath.Join(srv, "ops-bot/data/ops.db"),
		},
		Projects: list(envOr("PANEL_PROJECTS", "platform,apps,media-stack")),
		Self:     envOr("PANEL_SELF", "panel"),
		URL:      os.Getenv("PANEL_URL"),
		Token:    os.Getenv("PANEL_TOKEN"),
		Log:      log,
	}
	s.Metrics = &panel.LazyMetrics{Path: s.Paths.OpsDB}
	if t := os.Getenv("LLM_ADMIN_TOKEN"); t != "" {
		s.Gateway = &ops.GatewayClient{
			Base:  envOr("LLM_GATEWAY_URL", "http://llm-gateway:8080"),
			Token: t,
			HTTP:  &http.Client{Timeout: 15 * time.Second},
		}
	}
	token := os.Getenv("OPS_BOT_TOKEN")
	chatID, _ := strconv.ParseInt(os.Getenv("OPS_CHAT_ID"), 10, 64)
	if token != "" && chatID != 0 {
		s.Notify = panel.Telegram(&http.Client{Timeout: 15 * time.Second}, token, chatID)
	}
	return s, nil
}

func run(internalAddr string, log *slog.Logger) error {
	s, err := server(log)
	if err != nil {
		return err
	}
	defer s.Store.Close()
	if s.URL == "" {
		return errors.New("нужен PANEL_URL: адрес панели для ссылок входа, например http://192.168.1.50:8800")
	}
	if s.Token == "" {
		log.Warn("PANEL_TOKEN пуст: ops-bot не сможет выдать ссылку входа, остаётся только /app -login-link")
	}
	if s.Notify == nil {
		log.Warn("OPS_BOT_TOKEN или OPS_CHAT_ID пусты: действия в панели не уходят в Telegram")
	}

	// Без WriteTimeout: поток логов (SSE) живёт, пока открыта вкладка.
	public := &http.Server{Addr: envOr("PANEL_ADDR", ":8800"), Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	internal := &http.Server{Addr: internalAddr, Handler: s.InternalHandler(), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 2)
	for _, srv := range []*http.Server{public, internal} {
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	log.Info("панель запущена", "version", version, "addr", public.Addr, "internal", internal.Addr, "url", s.URL)
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	log.Info("панель останавливается")
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = internal.Shutdown(shutdown)
	return public.Shutdown(shutdown)
}

// printLink пишет пропуск прямо в базу панели: работает, даже когда ops-bot лежит.
func printLink() error {
	s, err := server(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		return err
	}
	defer s.Store.Close()
	if s.URL == "" {
		return errors.New("нужен PANEL_URL")
	}
	link, err := s.LoginLink(context.Background())
	if err != nil {
		return err
	}
	fmt.Println(link)
	return nil
}

// probe — healthcheck без curl: в distroless его нет.
func probe(addr string) int {
	c := &http.Client{Timeout: 3 * time.Second}
	port := addr
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		port = addr[i:]
	}
	resp, err := c.Get("http://127.0.0.1" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return 1
	}
	return 0
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
