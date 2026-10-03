// Package sqlitedb открывает SQLite одинаково для всех программ homelab.
package sqlitedb

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Open открывает базу в режиме WAL и применяет схему. Одно соединение: запись редкая,
// а так не бывает SQLITE_BUSY между своими же запросами.
func Open(path, schema string) (*sql.DB, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("открыть %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("схема %s: %w", path, err)
	}
	return db, nil
}

// OpenRO открывает существующую базу только на чтение: схема не применяется, файл не создаётся.
// query_only — вторая защита: даже если файл можно писать, запрос на запись получит ошибку.
func OpenRO(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?mode=ro" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("открыть %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("открыть %s: %w", path, err)
	}
	return db, nil
}
