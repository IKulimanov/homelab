package ops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"homelab/internal/sqlitedb"
)

const schema = `
-- Замеры раз в минуту; из них считаются мин/сред/макс за день и неделю. Храним 30 дней.
CREATE TABLE IF NOT EXISTS samples (
	ts     INTEGER NOT NULL,  -- unix, секунды
	metric TEXT    NOT NULL,
	value  REAL    NOT NULL
);
CREATE INDEX IF NOT EXISTS samples_ts ON samples(ts);

-- Падения контейнеров для недельного отчёта.
CREATE TABLE IF NOT EXISTS falls (
	ts        INTEGER NOT NULL,
	container TEXT    NOT NULL,
	reason    TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS falls_ts ON falls(ts);

-- GIF для событий: file_id Telegram, сами файлы лежат у Telegram. Их же читает update.sh через sqlite3.
CREATE TABLE IF NOT EXISTS gifs (
	event   TEXT NOT NULL,
	file_id TEXT NOT NULL,
	PRIMARY KEY (event, file_id)
);

CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
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

func (s *Store) AddSamples(ctx context.Context, at time.Time, values map[string]float64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("замеры: %w", err)
	}
	defer tx.Rollback()
	for metric, v := range values {
		if _, err := tx.ExecContext(ctx, `INSERT INTO samples (ts, metric, value) VALUES (?, ?, ?)`, at.Unix(), metric, v); err != nil {
			return fmt.Errorf("замеры: %w", err)
		}
	}
	return tx.Commit()
}

type Agg struct {
	Min, Avg, Max float64
	N             int
}

// Aggregates — мин/сред/макс каждой метрики за [from, to).
func (s *Store) Aggregates(ctx context.Context, from, to time.Time) (map[string]Agg, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT metric, MIN(value), AVG(value), MAX(value), COUNT(*)
		FROM samples WHERE ts >= ? AND ts < ? GROUP BY metric`, from.Unix(), to.Unix())
	if err != nil {
		return nil, fmt.Errorf("сводка замеров: %w", err)
	}
	defer rows.Close()
	out := map[string]Agg{}
	for rows.Next() {
		var m string
		var a Agg
		if err := rows.Scan(&m, &a.Min, &a.Avg, &a.Max, &a.N); err != nil {
			return nil, fmt.Errorf("сводка замеров: %w", err)
		}
		out[m] = a
	}
	return out, rows.Err()
}

func (s *Store) AddFall(ctx context.Context, at time.Time, container, reason string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO falls (ts, container, reason) VALUES (?, ?, ?)`, at.Unix(), container, reason)
	if err != nil {
		return fmt.Errorf("падение: %w", err)
	}
	return nil
}

// Falls — число падений по контейнерам за [from, to).
func (s *Store) Falls(ctx context.Context, from, to time.Time) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT container, COUNT(*) FROM falls WHERE ts >= ? AND ts < ? GROUP BY container`, from.Unix(), to.Unix())
	if err != nil {
		return nil, fmt.Errorf("падения: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var c string
		var n int
		if err := rows.Scan(&c, &n); err != nil {
			return nil, fmt.Errorf("падения: %w", err)
		}
		out[c] = n
	}
	return out, rows.Err()
}

// Prune удаляет замеры и падения старше before.
func (s *Store) Prune(ctx context.Context, before time.Time) error {
	for _, q := range []string{`DELETE FROM samples WHERE ts < ?`, `DELETE FROM falls WHERE ts < ?`} {
		if _, err := s.db.ExecContext(ctx, q, before.Unix()); err != nil {
			return fmt.Errorf("чистка: %w", err)
		}
	}
	return nil
}

func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *Store) AddGIF(ctx context.Context, event, fileID string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO gifs (event, file_id) VALUES (?, ?)`, event, fileID)
	if err != nil {
		return fmt.Errorf("gif: %w", err)
	}
	return nil
}

func (s *Store) GIFs(ctx context.Context, event string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT file_id FROM gifs WHERE event = ?`, event)
	if err != nil {
		return nil, fmt.Errorf("gif: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("gif: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GIFCounts — сколько GIF у каждого события.
func (s *Store) GIFCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT event, COUNT(*) FROM gifs GROUP BY event`)
	if err != nil {
		return nil, fmt.Errorf("gif: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var e string
		var n int
		if err := rows.Scan(&e, &n); err != nil {
			return nil, fmt.Errorf("gif: %w", err)
		}
		out[e] = n
	}
	return out, rows.Err()
}

func (s *Store) ClearGIFs(ctx context.Context, event string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM gifs WHERE event = ?`, event)
	if err != nil {
		return 0, fmt.Errorf("gif: %w", err)
	}
	return res.RowsAffected()
}
