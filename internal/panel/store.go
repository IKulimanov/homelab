package panel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"homelab/internal/sqlitedb"
)

// В базе лежат только хэши пропусков и сессий: копия panel.db не даёт войти в панель.
const schema = `
CREATE TABLE IF NOT EXISTS login_tokens (
	hash    TEXT PRIMARY KEY,
	expires INTEGER NOT NULL,  -- unix, секунды
	used    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS sessions (
	hash    TEXT PRIMARY KEY,
	created INTEGER NOT NULL,
	expires INTEGER NOT NULL
);

-- Журнал действий в панели. Значений секретов и текста запросов здесь нет, только что и над чем сделано.
CREATE TABLE IF NOT EXISTS audit (
	ts     INTEGER NOT NULL,
	action TEXT    NOT NULL,
	target TEXT    NOT NULL,
	detail TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS audit_ts ON audit(ts);
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

func newSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand не возвращает ошибку
	return hex.EncodeToString(b)
}

func hashOf(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// NewLoginToken — одноразовый пропуск. Сам пропуск возвращается один раз, в базе остаётся хэш.
func (s *Store) NewLoginToken(ctx context.Context, now time.Time, ttl time.Duration) (string, error) {
	tok := newSecret()
	_, err := s.db.ExecContext(ctx, `INSERT INTO login_tokens (hash, expires) VALUES (?, ?)`, hashOf(tok), now.Add(ttl).Unix())
	if err != nil {
		return "", fmt.Errorf("пропуск: %w", err)
	}
	// Старые пропуска не нужны даже для истории.
	_, _ = s.db.ExecContext(ctx, `DELETE FROM login_tokens WHERE expires < ?`, now.Add(-24*time.Hour).Unix())
	return tok, nil
}

// TokenValid — пропуск есть, не потрачен и не истёк. Только проверка, без траты: для страницы «Войти».
func (s *Store) TokenValid(ctx context.Context, tok string, now time.Time) (time.Time, bool) {
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT expires FROM login_tokens WHERE hash = ? AND used = 0 AND expires > ?`,
		hashOf(tok), now.Unix()).Scan(&exp)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(exp, 0), true
}

// UseToken тратит пропуск. Одним UPDATE: два одновременных входа по одной ссылке не пройдут оба.
func (s *Store) UseToken(ctx context.Context, tok string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE login_tokens SET used = 1 WHERE hash = ? AND used = 0 AND expires > ?`,
		hashOf(tok), now.Unix())
	if err != nil {
		return false, fmt.Errorf("пропуск: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) NewSession(ctx context.Context, now time.Time, ttl time.Duration) (string, error) {
	sid := newSecret()
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (hash, created, expires) VALUES (?, ?, ?)`,
		hashOf(sid), now.Unix(), now.Add(ttl).Unix())
	if err != nil {
		return "", fmt.Errorf("сессия: %w", err)
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires < ?`, now.Unix())
	return sid, nil
}

// Session — срок действия сессии; ok=false — сессии нет или она истекла.
func (s *Store) Session(ctx context.Context, sid string, now time.Time) (time.Time, bool) {
	if sid == "" {
		return time.Time{}, false
	}
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT expires FROM sessions WHERE hash = ? AND expires > ?`, hashOf(sid), now.Unix()).Scan(&exp)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(exp, 0), true
}

func (s *Store) DeleteSession(ctx context.Context, sid string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE hash = ?`, hashOf(sid))
	return err
}

type AuditEntry struct {
	Time   time.Time
	Action string
	Target string
	Detail string
}

func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit (ts, action, target, detail) VALUES (?, ?, ?, ?)`,
		e.Time.Unix(), e.Action, e.Target, e.Detail)
	if err != nil {
		return fmt.Errorf("журнал действий: %w", err)
	}
	return nil
}

// RecentAudit — последние записи журнала, новые сверху.
func (s *Store) RecentAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ts, action, target, detail FROM audit ORDER BY ts DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("журнал действий: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var ts int64
		if err := rows.Scan(&ts, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, fmt.Errorf("журнал действий: %w", err)
		}
		e.Time = time.Unix(ts, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

var errNotFound = errors.New("не найдено")
