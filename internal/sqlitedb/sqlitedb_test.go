package sqlitedb

import (
	"path/filepath"
	"testing"
)

func TestOpenROReadsButRefusesWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	rw, err := Open(path, `CREATE TABLE t (v INTEGER); INSERT INTO t VALUES (1);`)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()

	ro, err := OpenRO(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var n int
	if err := ro.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("чтение: n=%d err=%v", n, err)
	}
	if _, err := ro.Exec(`INSERT INTO t VALUES (2)`); err == nil {
		t.Fatal("запись через OpenRO прошла")
	}
}

func TestOpenROMissingFile(t *testing.T) {
	if db, err := OpenRO(filepath.Join(t.TempDir(), "nope.db")); err == nil {
		db.Close()
		t.Fatal("открыл несуществующую базу: OpenRO не должен её создавать")
	}
}
