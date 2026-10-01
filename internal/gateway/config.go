// Package gateway — шлюз к Gemini API: подменяет ключ сервиса на настоящий, считает токены и деньги,
// не пускает в Google сверх месячного лимита.
package gateway

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config — цены и лимиты. Файл лежит в git (stacks/platform/config/llm-gateway.yaml) и перечитывается
// при изменении: правка лимита доезжает через git pull без перезапуска.
type Config struct {
	Prices               map[string]Price  `yaml:"prices"`
	Clients              map[string]Client `yaml:"clients"`
	TotalMonthlyLimitUSD float64           `yaml:"total_monthly_limit_usd"`
	BalanceAlertUSD      float64           `yaml:"balance_alert_usd"`
}

// Price — доллары за миллион токенов, как в прайсе Google.
type Price struct {
	Input     float64 `yaml:"input"`
	Output    float64 `yaml:"output"`
	CacheRead float64 `yaml:"cache_read"`
}

type Client struct {
	MonthlyLimitUSD float64 `yaml:"monthly_limit_usd"`
}

// DefaultPrice — ключ таблицы цен для моделей, которых в ней нет.
const DefaultPrice = "default"

// PriceFor возвращает цену модели; false — цены нет, расход посчитается нулевым.
func (c *Config) PriceFor(model string) (Price, bool) {
	if p, ok := c.Prices[model]; ok {
		return p, true
	}
	p, ok := c.Prices[DefaultPrice]
	return p, ok
}

// ClientNames — имена клиентов по алфавиту.
func (c *Config) ClientNames() []string {
	names := make([]string, 0, len(c.Clients))
	for n := range c.Clients {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ParseConfig разбирает и проверяет конфиг. Нулевой лимит запрещён: опечатка не должна снимать ограничение.
func ParseConfig(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("разобрать конфиг: %w", err)
	}
	if len(c.Clients) == 0 {
		return nil, errors.New("в конфиге нет ни одного клиента")
	}
	for name, cl := range c.Clients {
		if cl.MonthlyLimitUSD <= 0 {
			return nil, fmt.Errorf("клиент %s: monthly_limit_usd должен быть больше нуля", name)
		}
	}
	if c.TotalMonthlyLimitUSD <= 0 {
		return nil, errors.New("total_monthly_limit_usd должен быть больше нуля")
	}
	for model, p := range c.Prices {
		if p.Input < 0 || p.Output < 0 || p.CacheRead < 0 {
			return nil, fmt.Errorf("цена %s: отрицательное значение", model)
		}
	}
	return &c, nil
}

// ConfigFile — конфиг из файла, который перечитывается при изменении времени или размера.
// Ошибка в новом файле не роняет шлюз: остаётся прежний конфиг, ошибка пишется в лог.
type ConfigFile struct {
	path string
	log  *slog.Logger

	mu      sync.Mutex
	modTime time.Time
	size    int64
	cfg     *Config
}

// LoadConfigFile читает конфиг при старте; здесь ошибка фатальна.
func LoadConfigFile(path string, log *slog.Logger) (*ConfigFile, error) {
	f := &ConfigFile{path: path, log: log}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("конфиг: %w", err)
	}
	cfg, err := readConfig(path)
	if err != nil {
		return nil, err
	}
	f.cfg, f.modTime, f.size = cfg, st.ModTime(), st.Size()
	return f, nil
}

func readConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("конфиг: %w", err)
	}
	return ParseConfig(data)
}

// Get возвращает актуальный конфиг. Запросов единицы в минуту, поэтому stat на каждый вызов дешевле,
// чем следить за файлом: git заменяет файл новым, и inotify на старый перестал бы срабатывать.
func (f *ConfigFile) Get() *Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, err := os.Stat(f.path)
	if err != nil || (st.ModTime().Equal(f.modTime) && st.Size() == f.size) {
		return f.cfg
	}
	f.modTime, f.size = st.ModTime(), st.Size()
	cfg, err := readConfig(f.path)
	if err != nil {
		f.log.Error("новый конфиг не принят, работаю со старым", "err", err)
		return f.cfg
	}
	f.cfg = cfg
	f.log.Info("конфиг перечитан", "clients", strings.Join(cfg.ClientNames(), ","))
	return f.cfg
}

// KeysFromEnv собирает ключи клиентов из переменных LLM_KEY_<ИМЯ>: LLM_KEY_BUDGET_BOT → budget-bot.
// Возвращает ключ → имя клиента.
func KeysFromEnv(environ []string) (map[string]string, error) {
	keys := map[string]string{}
	for _, kv := range environ {
		name, key, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "LLM_KEY_") {
			continue
		}
		client := strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(name, "LLM_KEY_")), "_", "-")
		if len(key) < 16 {
			return nil, fmt.Errorf("%s: ключ короче 16 символов", name)
		}
		if other, dup := keys[key]; dup {
			return nil, fmt.Errorf("один ключ у клиентов %s и %s", other, client)
		}
		keys[key] = client
	}
	return keys, nil
}
