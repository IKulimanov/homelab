// Package voice — фразы персонажа служебного бота. Каждое событие — файл <событие>.txt в каталоге,
// по фразе на строку; строки с # и пустые пропускаются. Файлы перечитываются после правки, перезапуск
// не нужен. Нет каталога или файла — пустая строка: сообщение уходит без фразы.
package voice

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type file struct {
	lines   []string
	text    string
	modTime time.Time
	size    int64
}

type Voice struct {
	dir string
	rnd func(n int) int

	mu    sync.Mutex
	files map[string]*file
	last  map[string]int // индекс прошлой фразы события: подряд одна и та же не повторяется
}

func New(dir string) *Voice {
	return &Voice{dir: dir, rnd: rand.IntN, files: map[string]*file{}, last: map[string]int{}}
}

// Line — случайная фраза события, {name} заменяется на name.
func (v *Voice) Line(event, name string) string {
	if v == nil || v.dir == "" {
		return ""
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	f := v.load(event)
	if f == nil || len(f.lines) == 0 {
		return ""
	}
	i := 0
	if n := len(f.lines); n > 1 {
		prev, seen := v.last[event]
		if !seen || prev >= n {
			i = v.rnd(n)
		} else {
			// Выбор из n-1 вариантов без прошлого: сдвиг на 1..n-1 от него.
			i = (prev + 1 + v.rnd(n-1)) % n
		}
	}
	v.last[event] = i
	return strings.ReplaceAll(f.lines[i], "{name}", name)
}

// Text — весь файл целиком, например инструкция для LLM.
func (v *Voice) Text(name string) string {
	if v == nil || v.dir == "" {
		return ""
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if f := v.load(name); f != nil {
		return f.text
	}
	return ""
}

// load читает файл, если он новый или изменился. Ошибка чтения — прежняя версия, если она была.
func (v *Voice) load(name string) *file {
	path := filepath.Join(v.dir, name+".txt")
	st, err := os.Stat(path)
	if err != nil {
		delete(v.files, name)
		return nil
	}
	if f, ok := v.files[name]; ok && f.modTime.Equal(st.ModTime()) && f.size == st.Size() {
		return f
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return v.files[name]
	}
	f := &file{text: strings.TrimSpace(string(data)), modTime: st.ModTime(), size: st.Size()}
	for _, l := range strings.Split(f.text, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			f.lines = append(f.lines, l)
		}
	}
	v.files[name] = f
	return f
}
