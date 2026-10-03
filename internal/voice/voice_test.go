package voice

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLineSubstitutesNameAndSkipsComments(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "died", "# комментарий\n\n{name} откинул копыта\n")
	v := New(dir)
	if got := v.Line("died", "budget-bot"); got != "budget-bot откинул копыта" {
		t.Fatalf("фраза: %q", got)
	}
}

func TestLineEmptyWithoutFiles(t *testing.T) {
	if got := New(t.TempDir()).Line("died", "x"); got != "" {
		t.Fatalf("без файла: %q", got)
	}
	if got := New("").Line("died", "x"); got != "" {
		t.Fatalf("без каталога: %q", got)
	}
	var v *Voice
	if got := v.Line("died", "x"); got != "" {
		t.Fatalf("nil: %q", got)
	}
}

func TestLineNeverRepeatsPrevious(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "temp", "раз\nдва\nтри\n")
	v := New(dir)
	// Генератор всегда выбирает первый вариант: без защиты фраза повторялась бы.
	v.rnd = func(int) int { return 0 }
	prev := v.Line("temp", "")
	for range 5 {
		got := v.Line("temp", "")
		if got == prev {
			t.Fatalf("фраза %q повторилась подряд", got)
		}
		prev = got
	}
}

func TestLinePicksUpChangedFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "disk", "старая\n")
	v := New(dir)
	if got := v.Line("disk", ""); got != "старая" {
		t.Fatalf("до правки: %q", got)
	}
	write(t, dir, "disk", "новая фраза\n")
	// Время изменения могло совпасть до секунды: сдвигаем явно.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(dir, "disk.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	if got := v.Line("disk", ""); got != "новая фраза" {
		t.Fatalf("после правки: %q", got)
	}
}

func TestText(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "report-prompt", "Ты Бендер.\nПиши коротко.\n")
	if got := New(dir).Text("report-prompt"); got != "Ты Бендер.\nПиши коротко." {
		t.Fatalf("текст: %q", got)
	}
}
