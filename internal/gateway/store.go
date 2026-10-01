package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"homelab/internal/sqlitedb"
)

const schema = `
CREATE TABLE IF NOT EXISTS calls (
	id              INTEGER PRIMARY KEY,
	ts              INTEGER NOT NULL,            -- unix, миллисекунды
	client          TEXT    NOT NULL,
	model           TEXT    NOT NULL,
	status          INTEGER NOT NULL,            -- HTTP-статус, который получил клиент
	blocked         INTEGER NOT NULL DEFAULT 0,  -- 1: отклонён шлюзом, в Google не ходил
	priced          INTEGER NOT NULL DEFAULT 1,  -- 0: модели нет в таблице цен
	input_tokens    INTEGER NOT NULL DEFAULT 0,
	cached_tokens   INTEGER NOT NULL DEFAULT 0,
	output_tokens   INTEGER NOT NULL DEFAULT 0,
	thoughts_tokens INTEGER NOT NULL DEFAULT 0,
	cost_micros     INTEGER NOT NULL DEFAULT 0,
	duration_ms     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS calls_ts ON calls(ts);

-- Пополнения (topup) и сверка с AI Studio (set). Баланс = последняя сверка + пополнения после неё − расход после неё.
CREATE TABLE IF NOT EXISTS ledger (
	id     INTEGER PRIMARY KEY,
	ts     INTEGER NOT NULL,
	kind   TEXT    NOT NULL CHECK (kind IN ('topup', 'set')),
	micros INTEGER NOT NULL,
	note   TEXT    NOT NULL DEFAULT ''
);
`

type Store struct {
	db *sql.DB
}

func OpenStore(path string) (*Store, error) {
	db, err := sqlitedb.Open(path, schema)
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Call — одна запись учёта.
type Call struct {
	Time     time.Time
	Client   string
	Model    string
	Status   int
	Blocked  bool
	Priced   bool
	Usage    Usage
	Cost     int64
	Duration time.Duration
}

func (s *Store) Record(ctx context.Context, c Call) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO calls (ts, client, model, status, blocked, priced,
			input_tokens, cached_tokens, output_tokens, thoughts_tokens, cost_micros, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Time.UnixMilli(), c.Client, c.Model, c.Status, c.Blocked, c.Priced,
		c.Usage.InputTokens(), c.Usage.cached(), c.Usage.CandidatesTokens, c.Usage.ThoughtsTokens,
		c.Cost, c.Duration.Milliseconds())
	if err != nil {
		return fmt.Errorf("записать вызов: %w", err)
	}
	return nil
}

// Spent — расход по клиентам и общий с момента since, в микродолларах.
func (s *Store) Spent(ctx context.Context, since time.Time) (map[string]int64, int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT client, SUM(cost_micros) FROM calls WHERE ts >= ? GROUP BY client`, since.UnixMilli())
	if err != nil {
		return nil, 0, fmt.Errorf("расход: %w", err)
	}
	defer rows.Close()
	by := map[string]int64{}
	var total int64
	for rows.Next() {
		var client string
		var micros int64
		if err := rows.Scan(&client, &micros); err != nil {
			return nil, 0, fmt.Errorf("расход: %w", err)
		}
		by[client] = micros
		total += micros
	}
	return by, total, rows.Err()
}

// UsageRow — сводка по клиенту и модели.
type UsageRow struct {
	Client         string  `json:"client"`
	Model          string  `json:"model"`
	Calls          int64   `json:"calls"`
	Errors         int64   `json:"errors"`
	Blocked        int64   `json:"blocked"`
	Unpriced       int64   `json:"unpriced"`
	InputTokens    int64   `json:"input_tokens"`
	CachedTokens   int64   `json:"cached_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	ThoughtsTokens int64   `json:"thoughts_tokens"`
	CostUSD        float64 `json:"cost_usd"`
}

func (s *Store) Usage(ctx context.Context, since time.Time) ([]UsageRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT client, model, COUNT(*),
			SUM(status >= 400 AND blocked = 0), SUM(blocked), SUM(status < 400 AND priced = 0),
			SUM(input_tokens), SUM(cached_tokens), SUM(output_tokens), SUM(thoughts_tokens), SUM(cost_micros)
		FROM calls WHERE ts >= ?
		GROUP BY client, model ORDER BY client, model`, since.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("сводка: %w", err)
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		var micros int64
		if err := rows.Scan(&r.Client, &r.Model, &r.Calls, &r.Errors, &r.Blocked, &r.Unpriced,
			&r.InputTokens, &r.CachedTokens, &r.OutputTokens, &r.ThoughtsTokens, &micros); err != nil {
			return nil, fmt.Errorf("сводка: %w", err)
		}
		r.CostUSD = USD(micros)
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddLedger пишет пополнение (topup) или сверку (set).
func (s *Store) AddLedger(ctx context.Context, at time.Time, kind string, micros int64, note string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO ledger (ts, kind, micros, note) VALUES (?, ?, ?, ?)`,
		at.UnixMilli(), kind, micros, note)
	if err != nil {
		return fmt.Errorf("записать %s: %w", kind, err)
	}
	return nil
}

// Balance — расчётный остаток предоплаты.
type Balance struct {
	Known  bool       `json:"known"` // false — ни пополнений, ни сверок ещё не было
	USD    float64    `json:"usd"`
	SetAt  *time.Time `json:"set_at,omitempty"`
	Topups float64    `json:"topups_usd"` // пополнения после сверки
	Spent  float64    `json:"spent_usd"`  // расход после сверки
}

func (s *Store) Balance(ctx context.Context) (Balance, error) {
	var b Balance
	var base, from int64
	var setID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, ts, micros FROM ledger WHERE kind = 'set' ORDER BY id DESC LIMIT 1`).Scan(&setID, &from, &base)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		setID, from, base = 0, 0, 0
	case err != nil:
		return b, fmt.Errorf("баланс: %w", err)
	default:
		t := time.UnixMilli(from)
		b.SetAt = &t
		b.Known = true
	}

	var topups, count int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(micros), 0), COUNT(*) FROM ledger WHERE kind = 'topup' AND id > ?`, setID,
	).Scan(&topups, &count); err != nil {
		return b, fmt.Errorf("баланс: %w", err)
	}
	if count > 0 {
		b.Known = true
	}
	var spent int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_micros), 0) FROM calls WHERE ts >= ?`, from,
	).Scan(&spent); err != nil {
		return b, fmt.Errorf("баланс: %w", err)
	}
	b.USD, b.Topups, b.Spent = USD(base+topups-spent), USD(topups), USD(spent)
	return b, nil
}
