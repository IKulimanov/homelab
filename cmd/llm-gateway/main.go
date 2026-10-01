// llm-gateway — шлюз учёта к Gemini API. Боты ходят сюда со своим ключом вместо Google.
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
	"syscall"
	"time"

	// Месяц для лимитов считается в поясе TZ, а в distroless базы поясов нет.
	_ "time/tzdata"

	"homelab/internal/gateway"
)

// version подставляет сборка: -ldflags "-X main.version=sha-…".
var version = "dev"

// shutdownWait — сколько ждать начатые запросы к модели при остановке; меньше stop_grace_period в compose.
const shutdownWait = 60 * time.Second

func main() {
	healthcheck := flag.Bool("healthcheck", false, "проверить, что шлюз отвечает (для healthcheck в compose)")
	flag.Parse()
	addr := envOr("LLM_ADDR", ":8080")
	if *healthcheck {
		os.Exit(probe(addr))
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(addr, log); err != nil {
		log.Error("шлюз остановлен с ошибкой", "err", err)
		os.Exit(1)
	}
}

func run(addr string, log *slog.Logger) error {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return errors.New("не задан GEMINI_API_KEY")
	}
	keys, err := gateway.KeysFromEnv(os.Environ())
	if err != nil {
		return err
	}
	cfg, err := gateway.LoadConfigFile(envOr("LLM_CONFIG", "/config/llm-gateway.yaml"), log)
	if err != nil {
		return err
	}
	for _, name := range cfg.Get().ClientNames() {
		if !hasClient(keys, name) {
			log.Warn("у клиента из конфига нет ключа LLM_KEY_*", "client", name)
		}
	}
	store, err := gateway.OpenStore(envOr("LLM_DB", "/data/llm-gateway.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	gw := &gateway.Server{
		Config:     cfg,
		Keys:       keys,
		APIKey:     apiKey,
		Upstream:   envOr("GEMINI_UPSTREAM", "https://generativelanguage.googleapis.com"),
		AdminToken: os.Getenv("LLM_ADMIN_TOKEN"),
		Store:      store,
		// Модели с размышлениями отвечают минутами; бот сам ограничивает своё ожидание.
		HTTP: &http.Client{Timeout: 5 * time.Minute},
		Log:  log,
		Now:  time.Now,
	}
	srv := &http.Server{Addr: addr, Handler: gw.Handler(), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("шлюз запущен", "version", version, "addr", addr, "clients", len(keys))

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownWait)
	defer cancel()
	return srv.Shutdown(sctx)
}

func hasClient(keys map[string]string, name string) bool {
	for _, n := range keys {
		if n == name {
			return true
		}
	}
	return false
}

// probe — healthcheck без curl: в distroless его нет.
func probe(addr string) int {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1" + portOf(addr) + "/healthz")
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

func portOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i:]
		}
	}
	return ":" + addr
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
