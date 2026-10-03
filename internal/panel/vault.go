package panel

import (
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"homelab/internal/envfile"
)

// readOnlySecrets — файлы, которые панель только показывает: правка media.env пересоздала бы медиастек.
var readOnlySecrets = []string{"media.env"}

type vaultFile struct {
	Name     string
	Note     string
	Missing  int
	ReadOnly bool
}

type vaultKey struct {
	Key      string
	Help     string
	Required bool
	Set      bool
}

type vaultView struct {
	Files    []vaultFile
	Current  string
	Hint     string
	ReadOnly bool
	Keys     []vaultKey
	Svc      string
	Saved    *savedStatus
}

type savedStatus struct {
	Keys    string
	Waiting bool
	Results []delivery
}

func (s *Server) vault(w http.ResponseWriter, r *http.Request) {
	files := s.secretsFiles()
	cur := r.PathValue("file")
	if cur == "" {
		if len(files) == 0 {
			s.render(w, r, "vault", http.StatusOK, page{Title: "Сейф Гермеса", Active: "vault", Data: vaultView{}})
			return
		}
		http.Redirect(w, r, "/vault/"+files[0], http.StatusSeeOther)
		return
	}
	// Ссылка из сообщения «жду ключи» ведёт на /vault/<svc>, без .env.
	if !strings.HasSuffix(cur, ".env") {
		http.Redirect(w, r, "/vault/"+url.PathEscape(cur+".env"), http.StatusSeeOther)
		return
	}
	if !slices.Contains(files, cur) {
		s.notFound(w, r)
		return
	}
	v := vaultView{Current: cur, ReadOnly: slices.Contains(readOnlySecrets, cur), Svc: strings.TrimSuffix(cur, ".env")}
	for _, name := range files {
		vf := vaultFile{Name: name, ReadOnly: slices.Contains(readOnlySecrets, name), Missing: len(s.missing(name))}
		switch {
		case vf.ReadOnly:
			vf.Note = "только просмотр"
		case vf.Missing > 0:
			vf.Note = "не заполнено обязательных: " + strconv.Itoa(vf.Missing)
		default:
			vf.Note = "всё на месте"
		}
		v.Files = append(v.Files, vf)
	}
	ex := s.example(cur)
	v.Hint = exampleHint(ex)
	f, err := envfile.Read(filepath.Join(s.Paths.Secrets, cur))
	if err != nil {
		s.render(w, r, "vault", http.StatusInternalServerError, page{Title: "Сейф Гермеса", Active: "vault", Toast: "Файл не прочитан: " + err.Error(), Data: v})
		return
	}
	req := envfile.Required(ex)
	for _, k := range keysOf(ex, f) {
		val, _ := f.Get(k)
		v.Keys = append(v.Keys, vaultKey{Key: k, Help: envfile.Help(ex, k), Required: slices.Contains(req, k), Set: val != ""})
	}
	if ts, err := strconv.ParseInt(r.URL.Query().Get("saved"), 10, 64); err == nil {
		v.Saved = s.savedStatus(v.Svc, ts, r.URL.Query().Get("keys"))
	}
	s.render(w, r, "vault", http.StatusOK, page{Title: "Сейф Гермеса", Active: "vault", Data: v})
}

// keysOf — ключи образца в его порядке, затем ключи, которые есть только в файле.
func keysOf(example []byte, f *envfile.File) []string {
	keys := envfile.Parse(example).Keys()
	for _, k := range f.Keys() {
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// exampleHint — пояснение из первой строки образца: «# /srv/secrets/x.env — секреты x. …» → «секреты x».
func exampleHint(example []byte) string {
	first, _, _ := strings.Cut(string(example), "\n")
	_, hint, ok := strings.Cut(first, " — ")
	if !ok {
		return ""
	}
	hint, _, _ = strings.Cut(hint, ".")
	return hint
}

// savedStatus — что стало после «Сохранить и применить»: ждём update.sh или его итог по сервису.
func (s *Server) savedStatus(svc string, ts int64, keys string) *savedStatus {
	st := &savedStatus{Keys: keys, Waiting: s.running()}
	if st.Waiting {
		return st
	}
	for _, d := range s.history() {
		if d.TS < ts {
			break
		}
		if svc == "homelab" || d.Svc == svc || (svc == "llm-gateway" && d.Svc == "llm-gateway") {
			st.Results = append(st.Results, d)
		}
	}
	return st
}

func (s *Server) vaultSave(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	path, err := s.secretsPath(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if slices.Contains(readOnlySecrets, name) {
		http.Error(w, "Этот файл только для просмотра: правка пересоздала бы медиастек.", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "форма не прочитана", http.StatusBadRequest)
		return
	}
	f, err := envfile.Read(path)
	if err != nil {
		http.Error(w, "файл не прочитан: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var changed []string
	for _, k := range keysOf(s.example(name), f) {
		val, present := r.PostForm["v_"+k]
		if !present {
			continue
		}
		// Пустое поле без отметки «тронуто» — «не менять»: значения в форму не попадают, поле всегда пустое.
		if val[0] == "" && r.PostFormValue("t_"+k) != "1" {
			continue
		}
		if old, _ := f.Get(k); old == val[0] {
			continue
		}
		if err := f.Set(k, val[0]); err != nil {
			back(w, r, "/vault/"+name, "Не сохранено: "+err.Error())
			return
		}
		changed = append(changed, k)
	}
	if len(changed) == 0 {
		back(w, r, "/vault/"+name, "Ничего не изменилось. Гермес разочарован, но печать не тратит.")
		return
	}
	if err := envfile.WriteFile(path, f.Bytes()); err != nil {
		s.Log.Error("секреты не записаны", "file", name, "err", err)
		back(w, r, "/vault/"+name, "Не сохранено: "+err.Error())
		return
	}
	list := strings.Join(changed, ", ")
	s.act(r.Context(), "secrets", name, "изменены ключи: "+list, "изменены секреты "+name+": "+list)
	if err := s.trigger("apply"); err != nil {
		back(w, r, "/vault/"+name, "Сохранено, но update.sh не запущен: "+err.Error())
		return
	}
	http.Redirect(w, r, "/vault/"+name+"?saved="+strconv.FormatInt(s.Now().Add(-time.Second).Unix(), 10)+
		"&keys="+url.QueryEscape(list), http.StatusSeeOther)
}

func (s *Server) vaultReveal(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	path, err := s.secretsPath(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	f, err := envfile.Read(path)
	if err != nil {
		http.Error(w, "файл не прочитан", http.StatusInternalServerError)
		return
	}
	key := r.PostFormValue("key")
	val, ok := f.Get(key)
	if !ok {
		http.Error(w, "нет такого ключа", http.StatusNotFound)
		return
	}
	s.record(r, "reveal", name, key)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"value": val})
}

// secretsPath — путь к существующему файлу секретов. Имя проверяется: из каталога не выйти.
func (s *Server) secretsPath(name string) (string, error) {
	if !secretsName.MatchString(name) {
		return "", errNotFound
	}
	path := filepath.Join(s.Paths.Secrets, name)
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !st.Mode().IsRegular()) {
		return "", errNotFound
	}
	return path, err
}

// record — запись в журнал без сообщения в Telegram: для частых безвредных действий.
func (s *Server) record(r *http.Request, action, target, detail string) {
	if err := s.Store.Audit(r.Context(), AuditEntry{Time: s.Now(), Action: action, Target: target, Detail: detail}); err != nil {
		s.Log.Error("журнал действий", "err", err)
	}
}
