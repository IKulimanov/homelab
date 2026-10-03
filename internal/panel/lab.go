package panel

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"homelab/internal/sqlitedb"
)

const (
	labRows      = 1000
	labCSVRows   = 100_000
	writeWindow  = 15 * time.Minute
	copyKeepDays = 7
)

// labState — включённые режимы записи по базам. Живёт в памяти: перезапуск панели выключает запись.
type labState struct {
	mu           sync.Mutex
	modes        map[string]writeMode
	readTimeout  time.Duration
	writeTimeout time.Duration
}

type writeMode struct {
	until  time.Time
	backup string
}

func (l *labState) timeouts() (time.Duration, time.Duration) {
	rt, wt := l.readTimeout, l.writeTimeout
	if rt == 0 {
		rt = 10 * time.Second
	}
	// Меньше busy_timeout ботов (5 с): запрос из панели не держит базу дольше, чем бот готов ждать.
	if wt == 0 {
		wt = 2 * time.Second
	}
	return rt, wt
}

// mode — режим записи базы, если он включён и не истёк.
func (s *Server) mode(db string) (writeMode, bool) {
	s.lab.mu.Lock()
	defer s.lab.mu.Unlock()
	m, ok := s.lab.modes[db]
	if ok && !s.Now().Before(m.until) {
		delete(s.lab.modes, db)
		return writeMode{}, false
	}
	return m, ok
}

type labDB struct {
	ID   string // budget-bot/app.db
	Path string
	Size string
}

// databases — базы сервисов: /srv/<svc>/data/*.db. База самой панели не показывается: в ней сессии.
func (s *Server) databases() []labDB {
	names, _ := filepath.Glob(filepath.Join(s.Paths.Srv, "*", "data", "*.db"))
	var out []labDB
	for _, n := range names {
		svc := filepath.Base(filepath.Dir(filepath.Dir(n)))
		if svc == "panel" || svc == "secrets" {
			continue
		}
		st, err := os.Stat(n)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		out = append(out, labDB{ID: svc + "/" + filepath.Base(n), Path: n, Size: humanSize(st.Size())})
	}
	return out
}

func (s *Server) findDB(id string) (labDB, bool) {
	i := slices.IndexFunc(s.databases(), func(d labDB) bool { return d.ID == id })
	if i < 0 {
		return labDB{}, false
	}
	return s.databases()[i], true
}

type labTable struct {
	Name   string
	Rows   string
	Schema string
}

type labView struct {
	DBs       []labDB
	Current   string
	Tables    []labTable
	Write     bool
	Until     time.Time
	Backup    string
	Error     string
	SQL       string
	MaxRows   int
	WriteMins int
}

func (s *Server) labPage(w http.ResponseWriter, r *http.Request) {
	v := labView{DBs: s.databases(), Current: r.URL.Query().Get("db"), MaxRows: labRows, WriteMins: int(writeWindow.Minutes())}
	if v.Current == "" && len(v.DBs) > 0 {
		v.Current = v.DBs[0].ID
	}
	if d, ok := s.findDB(v.Current); ok {
		tables, err := s.tables(r.Context(), d.Path)
		if err != nil {
			v.Error = err.Error()
		}
		v.Tables = tables
		if m, ok := s.mode(d.ID); ok {
			v.Write, v.Until, v.Backup = true, m.until, filepath.Base(m.backup)
		}
		if len(tables) > 0 {
			v.SQL = fmt.Sprintf("SELECT * FROM %s LIMIT 100", quoteIdent(tables[0].Name))
		}
	} else {
		v.Current = ""
	}
	s.render(w, r, "lab", http.StatusOK, page{Title: "Лаборатория", Active: "lab", Data: v})
}

func (s *Server) tables(ctx context.Context, path string) ([]labTable, error) {
	db, err := sqlitedb.OpenRO(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT name, COALESCE(sql, '') FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var out []labTable
	for rows.Next() {
		var t labTable
		if err := rows.Scan(&t.Name, &t.Schema); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	rows.Close()
	for i := range out {
		var n int64
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+quoteIdent(out[i].Name)).Scan(&n); err == nil {
			out[i].Rows = fmt.Sprint(n)
		} else {
			out[i].Rows = "?"
		}
	}
	return out, rows.Err()
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

type labResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated"`
	Changed   int64    `json:"changed"`
	Write     bool     `json:"write"`
	Ms        int64    `json:"ms"`
	Error     string   `json:"error,omitempty"`
}

var errReadOnly = errors.New("Профессор запретил: база открыта только на чтение. Включи режим записи, если правда нужно.")

func (s *Server) labQuery(w http.ResponseWriter, r *http.Request) {
	d, ok := s.findDB(r.PostFormValue("db"))
	if !ok {
		writeJSON(w, http.StatusNotFound, labResult{Error: "нет такой базы"})
		return
	}
	start := s.Now()
	res, err := s.runSQL(r, d, r.PostFormValue("sql"))
	res.Ms = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = err.Error()
		writeJSON(w, http.StatusOK, res)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// runSQL сначала выполняет оператор на соединении только для чтения. Если SQLite отвечает, что это запись,
// и для базы включён режим записи — повторяет на обычном соединении с коротким таймаутом.
func (s *Server) runSQL(r *http.Request, d labDB, query string) (labResult, error) {
	stmt, err := checkSQL(query)
	if err != nil {
		return labResult{}, err
	}
	rt, wt := s.lab.timeouts()
	ro, err := sqlitedb.OpenRO(d.Path)
	if err != nil {
		return labResult{}, err
	}
	defer ro.Close()
	ctx, cancel := context.WithTimeout(r.Context(), rt)
	defer cancel()
	res, err := readRows(ctx, ro, stmt, labRows)
	if err == nil || !isReadOnlyErr(err) {
		return res, timeoutErr(ctx, err, rt)
	}
	m, ok := s.mode(d.ID)
	if !ok {
		return labResult{}, errReadOnly
	}
	if firstWord(stmt) == "PRAGMA" {
		return labResult{}, errors.New("PRAGMA с записью запрещён: он меняет настройки базы, а не данные")
	}
	if _, err := os.Stat(m.backup); err != nil {
		return labResult{}, errors.New("копии базы нет, запрос не выполнен: выключи и снова включи режим записи")
	}
	rw, err := openRW(d.Path)
	if err != nil {
		return labResult{}, err
	}
	defer rw.Close()
	wctx, wcancel := context.WithTimeout(r.Context(), wt)
	defer wcancel()
	out, err := rw.ExecContext(wctx, stmt)
	if err != nil {
		return labResult{}, timeoutErr(wctx, err, wt)
	}
	n, _ := out.RowsAffected()
	s.act(r.Context(), "lab-write", d.ID, fmt.Sprintf("изменено строк: %d", n),
		fmt.Sprintf("запрос на запись в базе %s, изменено строк: %d", d.ID, n))
	return labResult{Changed: n, Write: true}, nil
}

func readRows(ctx context.Context, db *sql.DB, stmt string, limit int) (labResult, error) {
	var res labResult
	rows, err := db.QueryContext(ctx, stmt)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	res.Columns, err = rows.Columns()
	if err != nil {
		return res, err
	}
	res.Rows = [][]any{}
	for rows.Next() {
		if len(res.Rows) == limit {
			res.Truncated = true
			break
		}
		vals := make([]any, len(res.Columns))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return res, err
		}
		for i, v := range vals {
			vals[i] = cell(v)
		}
		res.Rows = append(res.Rows, vals)
	}
	return res, rows.Err()
}

// cell — значение для JSON и CSV: текст как есть, двоичные данные — только размером.
func cell(v any) any {
	if b, ok := v.([]byte); ok {
		if utf8.Valid(b) {
			return string(b)
		}
		return fmt.Sprintf("<двоичные данные, %d байт>", len(b))
	}
	if t, ok := v.(time.Time); ok {
		return t.Format(time.RFC3339)
	}
	return v
}

func openRW(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func isReadOnlyErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "readonly")
}

func timeoutErr(ctx context.Context, err error, d time.Duration) error {
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("запрос остановлен: дольше %s", d)
	}
	return err
}

// checkSQL — ровно один оператор, без ATTACH, DETACH и VACUUM: они открывают или пишут другие файлы.
func checkSQL(q string) (string, error) {
	words, semis, rest := scanSQL(q)
	if len(words) == 0 {
		return "", errors.New("пустой запрос")
	}
	if semis > 0 && strings.TrimSpace(rest) != "" {
		return "", errors.New("по одному оператору за раз: убери второй после ;")
	}
	for _, w := range words {
		switch w {
		case "ATTACH", "DETACH", "VACUUM":
			return "", fmt.Errorf("%s запрещён в Лаборатории", w)
		}
	}
	return strings.TrimRight(strings.TrimSpace(q), "; \t\r\n"), nil
}

// scanSQL — слова вне строк и комментариев (в верхнем регистре), число ; и то, что после первой ;.
func scanSQL(q string) (words []string, semis int, rest string) {
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, strings.ToUpper(cur.String()))
			cur.Reset()
		}
	}
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '\'' || c == '"' || c == '`' || c == '[':
			flush()
			end := c
			if c == '[' {
				end = ']'
			}
			for i++; i < len(q) && q[i] != end; i++ {
			}
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			flush()
			for i < len(q) && q[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			flush()
			i += 2
			for i+1 < len(q) && !(q[i] == '*' && q[i+1] == '/') {
				i++
			}
			i++
		case c == ';':
			flush()
			if semis == 0 {
				rest = stripComments(q[i+1:])
			}
			semis++
		case c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			cur.WriteByte(c)
		default:
			flush()
		}
	}
	flush()
	return words, semis, rest
}

func stripComments(s string) string {
	words, _, _ := scanSQL(s)
	return strings.Join(words, " ")
}

func firstWord(q string) string {
	words, _, _ := scanSQL(q)
	if len(words) == 0 {
		return ""
	}
	return words[0]
}

func (s *Server) labCSV(w http.ResponseWriter, r *http.Request) {
	d, ok := s.findDB(r.PostFormValue("db"))
	if !ok {
		http.Error(w, "нет такой базы", http.StatusNotFound)
		return
	}
	stmt, err := checkSQL(r.PostFormValue("sql"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ro, err := sqlitedb.OpenRO(d.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer ro.Close()
	rt, _ := s.lab.timeouts()
	ctx, cancel := context.WithTimeout(r.Context(), rt)
	defer cancel()
	res, err := readRows(ctx, ro, stmt, labCSVRows)
	if err != nil {
		if isReadOnlyErr(err) {
			err = errors.New("в CSV выгружается только чтение")
		}
		http.Error(w, timeoutErr(ctx, err, rt).Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.csv"`,
		strings.ReplaceAll(d.ID, "/", "-"), s.Now().Format("20060102-1504")))
	cw := csv.NewWriter(w)
	_ = cw.Write(res.Columns)
	for _, row := range res.Rows {
		rec := make([]string, len(row))
		for i, v := range row {
			if v != nil {
				rec[i] = fmt.Sprint(v)
			}
		}
		_ = cw.Write(rec)
	}
	cw.Flush()
}

// labWrite включает или выключает режим записи. При включении сначала делается копия базы: нет копии — нет записи.
func (s *Server) labWrite(w http.ResponseWriter, r *http.Request) {
	d, ok := s.findDB(r.PostFormValue("db"))
	if !ok {
		http.Error(w, "нет такой базы", http.StatusNotFound)
		return
	}
	ret := "/lab?db=" + d.ID
	if r.PostFormValue("on") != "1" {
		s.lab.mu.Lock()
		delete(s.lab.modes, d.ID)
		s.lab.mu.Unlock()
		s.act(r.Context(), "lab-write-off", d.ID, "", "выключен режим записи в базе "+d.ID)
		back(w, r, ret, "Режим записи выключен. Профессор снова спокоен.")
		return
	}
	backup, err := s.copyDB(r.Context(), d)
	if err != nil {
		s.Log.Error("копия базы перед записью", "db", d.ID, "err", err)
		back(w, r, ret, "Копия базы не сделалась, запись не включена: "+err.Error())
		return
	}
	until := s.Now().Add(writeWindow)
	s.lab.mu.Lock()
	s.lab.modes[d.ID] = writeMode{until: until, backup: backup}
	s.lab.mu.Unlock()
	s.act(r.Context(), "lab-write-on", d.ID, filepath.Base(backup),
		fmt.Sprintf("включён режим записи в базе %s до %s, копия %s", d.ID, until.Local().Format("15:04"), filepath.Base(backup)))
	back(w, r, ret, "Хорошие новости, все! Теперь можно всё сломать. Копия: "+filepath.Base(backup))
}

// copyDB — VACUUM INTO в каталог копий; заодно удаляет копии старше недели.
func (s *Server) copyDB(ctx context.Context, d labDB) (string, error) {
	if err := os.MkdirAll(s.Paths.DBCopy, 0o700); err != nil {
		return "", err
	}
	s.pruneCopies()
	dst := filepath.Join(s.Paths.DBCopy, fmt.Sprintf("%s-%s.db",
		strings.TrimSuffix(strings.ReplaceAll(d.ID, "/", "-"), ".db"), s.Now().Format("20060102-150405")))
	// Без query_only: VACUUM INTO пишет только новый файл, а query_only его запрещает.
	db, err := sql.Open("sqlite", "file:"+d.Path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return "", err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		_ = os.Remove(dst)
		return "", err
	}
	return dst, nil
}

func (s *Server) pruneCopies() {
	entries, err := os.ReadDir(s.Paths.DBCopy)
	if err != nil {
		return
	}
	cut := s.Now().Add(-copyKeepDays * 24 * time.Hour)
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && info.Mode().IsRegular() && strings.HasSuffix(e.Name(), ".db") && info.ModTime().Before(cut) {
			_ = os.Remove(filepath.Join(s.Paths.DBCopy, e.Name()))
		}
	}
}
