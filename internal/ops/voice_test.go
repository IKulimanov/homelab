package ops

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"homelab/internal/voice"
)

func withVoice(t *testing.T, f *fixture, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.svc.voice = voice.New(dir)
}

func TestAlertCategoriesFilter(t *testing.T) {
	f := newFixture(t)
	f.svc.cfg.Alerts = []string{"temp", "disk", "service", "llm"}
	f.metrics.snap.MemPct = 96
	f.metrics.snap.CPUTemp = 95
	f.svc.Tick(context.Background())
	s := strings.Join(f.takeSent(), "\n")
	if strings.Contains(s, "памяти") {
		t.Fatalf("алерт по выключенной памяти: %s", s)
	}
	if !strings.Contains(s, "🚨 Критично: температура CPU 95") {
		t.Fatalf("нет алерта по температуре: %s", s)
	}

	f.svc.HandleEvent(context.Background(), event("health_status: unhealthy", "budget-bot", "apps", nil))
	if s := f.takeSent(); len(s) != 0 {
		t.Fatalf("unhealthy при выключенной категории: %v", s)
	}
}

func TestAlertFactFirstThenPhraseAndGIF(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	withVoice(t, f, map[string]string{
		"died":      "{name} откинул копыта",
		"recovered": "{name} ожил",
		"repeat-1":  "всё ещё лежит",
	})
	if err := f.svc.store.AddGIF(ctx, "died", "gif-died"); err != nil {
		t.Fatal(err)
	}

	f.svc.HandleEvent(ctx, event("die", "budget-bot", "apps", map[string]string{"exitCode": "1"}))
	if len(f.gifs) != 1 {
		t.Fatalf("GIF не отправлен: %v, текст %v", f.gifs, f.sent)
	}
	id, caption, _ := strings.Cut(f.gifs[0], "|")
	lines := strings.Split(caption, "\n")
	if id != "gif-died" || lines[0] != "🚨 Критично: budget-bot упал (код выхода 1)" ||
		lines[len(lines)-1] != "🤖 budget-bot откинул копыта" {
		t.Fatalf("GIF %q, подпись %q", id, caption)
	}

	// GIF не ушёл — тот же текст обычным сообщением.
	f.gifs, f.gifErr = nil, errors.New("telegram недоступен")
	f.svc.HandleEvent(ctx, event("oom", "ops-bot", "platform", nil))
	if s := f.takeSent(); len(s) != 1 || !strings.HasPrefix(s[0], "🚨 Критично: ops-bot") {
		t.Fatalf("запасной текст: %v", s)
	}
}

func TestRepeatGetsAngrierPhraseWithoutGIF(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	withVoice(t, f, map[string]string{"temp": "горю", "repeat-1": "всё ещё горю", "repeat-3": "пишу завещание"})
	if err := f.svc.store.AddGIF(ctx, "temp", "gif-fire"); err != nil {
		t.Fatal(err)
	}
	f.metrics.snap.CPUTemp = 95
	f.svc.Tick(ctx)
	f.gifs = nil
	var got []string
	for range 4 {
		f.now = f.now.Add(time.Hour)
		f.svc.Tick(ctx)
		for _, s := range f.takeSent() {
			got = append(got, s[strings.LastIndex(s, "\n")+1:])
		}
	}
	want := []string{"🤖 всё ещё горю", "👉 " + hostTip("temp", "cpu-temp"), "🤖 пишу завещание", "🤖 пишу завещание"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("повторы: %q", got)
	}
	if len(f.gifs) != 0 {
		t.Fatalf("GIF на повторе: %v", f.gifs)
	}
}

func TestGIFCommand(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	say := func(text, gif string) string {
		return f.svc.HandleMessage(ctx, Incoming{ChatID: ownChat, Text: text, GIF: gif})[0].Text
	}
	if r := say("/gif temp", ""); !strings.Contains(r, "GIF") {
		t.Fatalf("без GIF: %q", r)
	}
	if r := say("/gif fire", "id1"); !strings.Contains(r, "temp") {
		t.Fatalf("неизвестное событие: %q", r)
	}
	say("/gif temp", "id1")
	say("/gif temp", "id2")
	if r := say("/gif", ""); !strings.Contains(r, "(temp): 2") {
		t.Fatalf("список: %q", r)
	}
	say("/gif clear temp", "")
	if ids, _ := f.svc.store.GIFs(ctx, "temp"); len(ids) != 0 {
		t.Fatalf("после очистки: %v", ids)
	}
	if r := f.svc.HandleMessage(ctx, Incoming{ChatID: 7, Text: "/gif temp", GIF: "id"}); r != nil {
		t.Fatal("чужой чат сохранил GIF")
	}
}

func TestGIFWithoutCommandAsksEventByButtons(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.svc.HandleMessage(ctx, Incoming{ChatID: ownChat, GIF: "id-disk"})
	var data []string
	for _, row := range r[0].Buttons {
		for _, b := range row {
			data = append(data, b.Data)
		}
	}
	if len(data) != len(gifEvents) || data[1] != "gif:disk" {
		t.Fatalf("кнопки событий: %v", data)
	}

	r = f.svc.Callback(ctx, ownChat, "gif:disk")
	if ids, _ := f.svc.store.GIFs(ctx, "disk"); len(ids) != 1 || ids[0] != "id-disk" {
		t.Fatalf("после кнопки: %v, ответ %q", ids, r[0].Text)
	}
}

func TestGIFCommandUsesLastGIFWithoutReply(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.svc.HandleMessage(ctx, Incoming{ChatID: ownChat, GIF: "id-last"})
	f.svc.HandleMessage(ctx, Incoming{ChatID: ownChat, Text: "/gif temp"})
	if ids, _ := f.svc.store.GIFs(ctx, "temp"); len(ids) != 1 || ids[0] != "id-last" {
		t.Fatalf("GIF без ответа: %v", ids)
	}
}

func TestGIFButtonWithoutGIFAfterRestart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.svc.Callback(ctx, ownChat, "gif:disk")
	if !strings.Contains(r[0].Text, "ещё раз") {
		t.Fatalf("ответ: %q", r[0].Text)
	}
	if ids, _ := f.svc.store.GIFs(ctx, "disk"); len(ids) != 0 {
		t.Fatalf("сохранено без GIF: %v", ids)
	}
}

type fakeCommenter struct {
	text string
	err  error
}

func (c *fakeCommenter) Comment(context.Context, string, string) (string, error) {
	return c.text, c.err
}

func TestWeeklyReportComment(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		c    *fakeCommenter
		want string
	}{
		{"Gemini ответил", &fakeCommenter{text: "Неделю отпахал, Фрай."}, "Неделю отпахал, Фрай."},
		{"Gemini упал", &fakeCommenter{err: errors.New("429")}, "запасная фраза"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			withVoice(t, f, map[string]string{"report-prompt": "Ты Бендер.", "report-fallback": "запасная фраза"})
			f.svc.commenter = tc.c
			f.svc.cfg.Report = Schedule{Day: time.Sunday, At: "20:00"}
			f.now = time.Date(2026, 10, 4, 20, 0, 0, 0, time.UTC)
			f.svc.Tick(ctx)
			var report string
			for _, s := range f.takeSent() {
				if strings.HasPrefix(s, "📊 Отчёт за неделю") {
					report = s
				}
			}
			if !strings.HasSuffix(report, "\n\n🤖 "+tc.want) {
				t.Fatalf("отчёт: %q", report)
			}
		})
	}
}

func TestGeminiCommenterRequest(t *testing.T) {
	var gotPath, gotKey string
	var body struct {
		SystemInstruction struct {
			Parts []struct{ Text string } `json:"parts"`
		} `json:"systemInstruction"`
		Contents []struct {
			Parts []struct{ Text string } `json:"parts"`
		} `json:"contents"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.Header.Get("x-goog-api-key")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		if gotKey != "ключ-ops" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"Неделю "},{"text":"отпахал."}]}}]}`)
	}))
	defer srv.Close()

	g := &GeminiCommenter{Base: srv.URL, Key: "ключ-ops", Model: "gemini-2.5-flash", HTTP: srv.Client()}
	got, err := g.Comment(context.Background(), "Ты Бендер.", "Отчёт за неделю")
	if err != nil || got != "Неделю отпахал." {
		t.Fatalf("ответ %q, %v", got, err)
	}
	if gotPath != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("путь %q", gotPath)
	}
	if body.SystemInstruction.Parts[0].Text != "Ты Бендер." || body.Contents[0].Parts[0].Text != "Отчёт за неделю" {
		t.Fatalf("тело запроса: %+v", body)
	}

	g.Key = "чужой"
	if _, err := g.Comment(context.Background(), "p", "r"); err == nil {
		t.Fatal("ошибка 401 не вернулась")
	}
}
