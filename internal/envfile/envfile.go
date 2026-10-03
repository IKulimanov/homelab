// Package envfile читает и правит файлы секретов /srv/secrets/*.env, не трогая комментарии и порядок строк.
// Разбор совпадает с env_get, env_set и required_keys из scripts/lib.sh.
package envfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// RequiredMark — слово в комментарии над переменной, которое делает её обязательной.
const RequiredMark = "Обязателен"

type File struct {
	lines []string
}

func Parse(data []byte) *File {
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return &File{}
	}
	return &File{lines: strings.Split(s, "\n")}
}

func Read(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data), nil
}

func (f *File) Bytes() []byte {
	if len(f.lines) == 0 {
		return nil
	}
	return []byte(strings.Join(f.lines, "\n") + "\n")
}

// keyOf — имя переменной в строке KEY=value или "".
func keyOf(line string) string {
	k, _, ok := strings.Cut(line, "=")
	if !ok || !keyRe.MatchString(k) {
		return ""
	}
	return k
}

// Get — значение без кавычек. Если ключ повторяется, берётся последнее, как у compose.
func (f *File) Get(key string) (string, bool) {
	val, found := "", false
	for _, l := range f.lines {
		if keyOf(l) == key {
			val, found = unquote(l[len(key)+1:]), true
		}
	}
	return val, found
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// Keys — ключи в порядке первого появления.
func (f *File) Keys() []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range f.lines {
		if k := keyOf(l); k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// Set заменяет первое вхождение ключа и убирает повторы; нового ключа нет — дописывает в конец.
func (f *File) Set(key, value string) error {
	if !keyRe.MatchString(key) {
		return fmt.Errorf("имя %q: латинские буквы, цифры и _", key)
	}
	enc, err := encode(value)
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	line := key + "=" + enc
	out := f.lines[:0:0]
	done := false
	for _, l := range f.lines {
		if keyOf(l) == key {
			if !done {
				out = append(out, line)
				done = true
			}
			continue
		}
		out = append(out, l)
	}
	if !done {
		out = append(out, line)
	}
	f.lines = out
	return nil
}

// encode — значение для env_file. compose подставляет $ и отрезает « #комментарий» в значениях без кавычек,
// а в одинарных кавычках берёт всё как есть. Экранирования в одинарных кавычках нет, поэтому ' там нельзя.
func encode(v string) (string, error) {
	if strings.ContainsAny(v, "\n\r") {
		return "", errors.New("перевод строки в значении")
	}
	needQuote := strings.Contains(v, "$") || strings.Contains(v, " #") || v != strings.TrimSpace(v) ||
		strings.HasPrefix(v, `"`) || strings.HasPrefix(v, "'")
	if !needQuote {
		return v, nil
	}
	if strings.Contains(v, "'") {
		return "", errors.New("одинарная кавычка вместе с $, # или пробелами по краям: такое значение не записать")
	}
	return "'" + v + "'", nil
}

// Required — обязательные ключи образца. Комментарий относится ко всем переменным сразу под ним,
// до пустой строки или следующего комментария после переменной.
func Required(example []byte) []string {
	var out []string
	req, prevComment := false, false
	for _, l := range strings.Split(string(example), "\n") {
		switch {
		case strings.TrimSpace(l) == "":
			req, prevComment = false, false
		case strings.HasPrefix(l, "#"):
			if !prevComment {
				req = false
			}
			if strings.Contains(l, RequiredMark) {
				req = true
			}
			prevComment = true
		default:
			if k := keyOf(l); k != "" {
				if req {
					out = append(out, k)
				}
				prevComment = false
			}
		}
	}
	return out
}

// Help — комментарий прямо над ключом в образце, без «#»; для подсказки в Сейфе.
func Help(example []byte, key string) string {
	var block []string
	for _, l := range strings.Split(string(example), "\n") {
		switch {
		case strings.HasPrefix(l, "#"):
			block = append(block, strings.TrimSpace(strings.TrimPrefix(l, "#")))
		case keyOf(l) == key:
			return strings.Join(block, " ")
		case keyOf(l) != "":
			// Переменные под одним комментарием делят его подсказку.
		default:
			block = nil
		}
	}
	return ""
}

// WriteFile заменяет файл целиком: запись во временный файл рядом с правами 0600, затем rename.
// Читатель видит либо старое, либо новое содержимое, но не половину.
func WriteFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Equal — одинаковы ли файлы по смыслу строк; для проверки «ничего не изменилось».
func Equal(a, b *File) bool { return bytes.Equal(a.Bytes(), b.Bytes()) }
