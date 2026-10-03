package envfile

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const example = `# /srv/secrets/habit-bot.env

# Токен из BotFather. Обязателен.
BOT_TOKEN=
BOT_ADMIN=
# Часовой пояс.
BOT_TIMEZONE=Asia/Bishkek

# Ключ шлюза. Обязателен.
# Пишет update.sh сам.
GEMINI_API_KEY=
GEMINI_BASE_URL=
# Необязательно.
EXTRA=
`

func TestRequiredFollowsCommentAboveUntilBlankLine(t *testing.T) {
	got := Required([]byte(example))
	// BOT_ADMIN под тем же комментарием, что BOT_TOKEN; BOT_TIMEZONE — под своим комментарием без пометки;
	// у GEMINI_* пометка в первой строке комментария из двух; EXTRA — новый комментарий после переменной.
	want := []string{"BOT_TOKEN", "BOT_ADMIN", "GEMINI_API_KEY", "GEMINI_BASE_URL"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSetKeepsCommentsOrderAndUnknownKeys(t *testing.T) {
	f := Parse([]byte("# шапка\nA=1\n# про B\nB=2\nOWN=x\nB=3\n"))
	if v, _ := f.Get("B"); v != "3" {
		t.Fatalf("Get берёт последнее значение, как compose: %q", v)
	}
	for k, v := range map[string]string{"B": "новое", "C": "c"} {
		if err := f.Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	want := "# шапка\nA=1\n# про B\nB=новое\nOWN=x\nC=c\n"
	if got := string(f.Bytes()); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if !slices.Equal(f.Keys(), []string{"A", "B", "OWN", "C"}) {
		t.Fatalf("ключи %v", f.Keys())
	}
}

func TestSetQuotesDollarAndRejectsNewline(t *testing.T) {
	f := Parse(nil)
	if err := f.Set("P", "a$b"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(f.Bytes()), "P='a$b'\n") {
		t.Fatalf("$ без кавычек: compose подставит переменную: %q", f.Bytes())
	}
	if v, _ := f.Get("P"); v != "a$b" {
		t.Fatalf("Get снимает кавычки: %q", v)
	}
	if err := f.Set("P", "a\nb"); err == nil {
		t.Fatal("перевод строки принят")
	}
	if err := f.Set("P", "it's $5"); err == nil {
		t.Fatal("кавычка вместе с $ принята: такое значение не записать в env_file")
	}
	if err := f.Set("bad key", "x"); err == nil {
		t.Fatal("неверное имя ключа принято")
	}
}

func TestWriteFileReplacesAtomicallyWith0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.env")
	if err := os.WriteFile(path, []byte("A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("A=2\n")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("права %v", st.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if string(data) != "A=2\n" {
		t.Fatalf("содержимое %q", data)
	}
	if left, _ := filepath.Glob(path + ".*"); len(left) != 0 {
		t.Fatalf("остались временные файлы: %v", left)
	}
}
