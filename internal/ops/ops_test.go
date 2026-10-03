package ops

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"homelab/internal/docker"
	"homelab/internal/gateway"
	"homelab/internal/host"
)

const ownChat = 42

type fakeDocker struct {
	containers []docker.Container
	inspect    map[string]docker.Inspect
	logs       string
	calls      []string
}

func (f *fakeDocker) List(context.Context) ([]docker.Container, error) {
	f.calls = append(f.calls, "list")
	return f.containers, nil
}
func (f *fakeDocker) Inspect(_ context.Context, id string) (docker.Inspect, error) {
	return f.inspect[id], nil
}
func (f *fakeDocker) Image(context.Context, string) (docker.Image, error) { return docker.Image{}, nil }
func (f *fakeDocker) Restart(_ context.Context, id string) error {
	f.calls = append(f.calls, "restart "+id)
	return nil
}
func (f *fakeDocker) Start(_ context.Context, id string) error {
	f.calls = append(f.calls, "start "+id)
	return nil
}
func (f *fakeDocker) Stop(_ context.Context, id string) error {
	f.calls = append(f.calls, "stop "+id)
	return nil
}
func (f *fakeDocker) Logs(context.Context, string, int, bool) ([]byte, error) {
	return []byte(f.logs), nil
}

type fakeLLM struct{ st gateway.Status }

func (f *fakeLLM) Status(context.Context) (gateway.Status, error) { return f.st, nil }
func (f *fakeLLM) Usage(context.Context, time.Time) ([]gateway.UsageRow, error) {
	return nil, nil
}
func (f *fakeLLM) Ledger(context.Context, string, float64) (gateway.Balance, error) {
	return gateway.Balance{}, nil
}

type fakeMetrics struct{ snap host.Snapshot }

func (f *fakeMetrics) Collect() host.Snapshot { return f.snap }

func container(id, name, project string) docker.Container {
	return docker.Container{ID: id, Names: []string{"/" + name}, State: "running",
		Labels: map[string]string{docker.LabelProject: project, docker.LabelService: name}}
}

type fixture struct {
	svc     *Service
	docker  *fakeDocker
	llm     *fakeLLM
	metrics *fakeMetrics
	sent    []string
	gifs    []string // «file_id|подпись»
	gifErr  error
	now     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "ops.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := &fixture{
		docker: &fakeDocker{
			containers: []docker.Container{
				container("c1", "budget-bot", "apps"),
				container("c2", "ops-bot", "platform"),
				container("c3", "postgres", "other"),
			},
			inspect: map[string]docker.Inspect{},
		},
		llm: &fakeLLM{},
		metrics: &fakeMetrics{snap: host.Snapshot{CPUTemp: host.Unknown, SSDTemp: host.Unknown, CPUBusyPct: host.Unknown,
			MemPct: 50, SwapPct: host.Unknown, Load1: host.Unknown, Load5: host.Unknown, Load15: host.Unknown}},
		now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	}
	for _, c := range f.docker.containers {
		var in docker.Inspect
		in.State.Running = true
		in.State.StartedAt = f.now.Add(-time.Hour)
		f.docker.inspect[c.ID] = in
	}
	f.svc = New(Config{
		ChatID:     ownChat,
		Projects:   []string{"platform", "apps", "media-stack"},
		Protected:  []string{"ops-bot"},
		Location:   time.UTC,
		Thresholds: DefaultThresholds(),
	}, Deps{
		Docker: f.docker, LLM: f.llm, Metrics: f.metrics, Store: store,
		Send: func(_ context.Context, text string) error { f.sent = append(f.sent, text); return nil },
		SendGIF: func(_ context.Context, id, caption string) error {
			if f.gifErr != nil {
				return f.gifErr
			}
			f.gifs = append(f.gifs, id+"|"+caption)
			return nil
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return f.now },
	})
	return f
}

func (f *fixture) takeSent() []string {
	s := f.sent
	f.sent = nil
	return s
}

func event(action, name, project string, attrs map[string]string) docker.Event {
	var ev docker.Event
	ev.Type, ev.Action = "container", action
	ev.Actor.Attributes = map[string]string{"name": name, docker.LabelProject: project}
	for k, v := range attrs {
		ev.Actor.Attributes[k] = v
	}
	return ev
}

func TestForeignChatIgnored(t *testing.T) {
	f := newFixture(t)
	if r := f.svc.Handle(context.Background(), 7, "/restart budget-bot"); r != nil {
		t.Fatalf("ответили чужому чату: %+v", r)
	}
	if r := f.svc.Callback(context.Background(), 7, "stop:budget-bot"); r != nil {
		t.Fatalf("кнопка из чужого чата сработала: %+v", r)
	}
	if len(f.docker.calls) != 0 {
		t.Fatalf("вызовы Docker: %v", f.docker.calls)
	}
}

func TestRestartOnlyManagedProjects(t *testing.T) {
	f := newFixture(t)
	r := f.svc.Handle(context.Background(), ownChat, "/restart postgres")
	if !strings.Contains(r[0].Text, "не из проектов homelab") {
		t.Fatalf("ответ: %q", r[0].Text)
	}
	f.svc.Handle(context.Background(), ownChat, "/restart@ops_bot budget-bot")
	want := []string{"list", "list", "restart c1"}
	if strings.Join(f.docker.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("вызовы: %v", f.docker.calls)
	}
}

func TestStopNeedsConfirmationAndProtectsOpsBot(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.svc.Handle(ctx, ownChat, "/stop budget-bot")
	if len(r[0].Buttons) == 0 || r[0].Buttons[0][0].Data != "stop:budget-bot" {
		t.Fatalf("нет кнопки подтверждения: %+v", r[0])
	}
	for _, c := range f.docker.calls {
		if strings.HasPrefix(c, "stop") {
			t.Fatal("остановили без подтверждения")
		}
	}
	f.svc.Callback(ctx, ownChat, "stop:budget-bot")
	if last := f.docker.calls[len(f.docker.calls)-1]; last != "stop c1" {
		t.Fatalf("после подтверждения: %v", f.docker.calls)
	}

	r = f.svc.Handle(ctx, ownChat, "/stop ops-bot")
	if len(r[0].Buttons) != 0 {
		t.Fatal("ops-bot можно остановить из чата")
	}
	f.svc.Callback(ctx, ownChat, "stop:ops-bot")
	if last := f.docker.calls[len(f.docker.calls)-1]; last == "stop c2" {
		t.Fatal("ops-bot остановлен подделанной кнопкой")
	}
}

func TestLongLogsSentAsFile(t *testing.T) {
	f := newFixture(t)
	f.docker.logs = strings.Repeat("строка лога номер\n", 300) // больше 4096 символов
	r := f.svc.Handle(context.Background(), ownChat, "/logs budget-bot 300")
	if r[0].File == nil || r[0].File.Name != "budget-bot.log" {
		t.Fatalf("длинный лог не файлом: %+v", r[0].Text)
	}
	f.docker.logs = "коротко\n"
	r = f.svc.Handle(context.Background(), ownChat, "/logs budget-bot")
	if r[0].File != nil || !strings.Contains(r[0].Text, "коротко") {
		t.Fatalf("короткий лог: %+v", r[0])
	}
}

func TestDockerEventsToAlerts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.svc.HandleEvent(ctx, event("die", "budget-bot", "apps", map[string]string{"exitCode": "2"}))
	if s := f.takeSent(); len(s) != 1 || !strings.Contains(s[0], "budget-bot упал, код выхода 2") {
		t.Fatalf("падение: %v", s)
	}

	// Штатная остановка: kill, затем die с кодом сигнала — без алерта.
	f.svc.HandleEvent(ctx, event("kill", "ops-bot", "platform", map[string]string{"signal": "15"}))
	f.svc.HandleEvent(ctx, event("die", "ops-bot", "platform", map[string]string{"exitCode": "143"}))
	if s := f.takeSent(); len(s) != 0 {
		t.Fatalf("штатная остановка дала алерт: %v", s)
	}

	f.svc.HandleEvent(ctx, event("oom", "ops-bot", "platform", nil))
	f.svc.HandleEvent(ctx, event("health_status: unhealthy", "llm-gateway", "platform", nil))
	if s := f.takeSent(); len(s) != 2 || !strings.Contains(s[0], "OOM") || !strings.Contains(s[1], "healthcheck") {
		t.Fatalf("oom и unhealthy: %v", s)
	}

	// Чужой проект не наблюдаем.
	f.svc.HandleEvent(ctx, event("die", "postgres", "other", map[string]string{"exitCode": "1"}))
	if s := f.takeSent(); len(s) != 0 {
		t.Fatalf("алерт по чужому контейнеру: %v", s)
	}
}

func TestFallClosedOnlyAfterStableRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.svc.HandleEvent(ctx, event("die", "budget-bot", "apps", map[string]string{"exitCode": "1"}))
	f.takeSent()

	in := f.docker.inspect["c1"]
	in.State.StartedAt = f.now.Add(-30 * time.Second)
	f.docker.inspect["c1"] = in
	f.svc.Tick(ctx)
	if s := f.takeSent(); len(s) != 0 {
		t.Fatalf("закрыли падение через 30 секунд работы: %v", s)
	}
	f.now = f.now.Add(2 * time.Minute)
	f.svc.Tick(ctx)
	if s := f.takeSent(); len(s) != 1 || !strings.Contains(s[0], "Восстановилось: budget-bot снова работает") {
		t.Fatalf("после стабильной работы: %v", s)
	}
}

func TestLLMLimitAlerts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.llm.st = gateway.Status{
		Clients:       []gateway.ClientStatus{{Name: "budget-bot", SpentUSD: 1.7, LimitUSD: 2}},
		TotalSpentUSD: 1.7, TotalLimitUSD: 15,
	}
	f.svc.Tick(ctx)
	if s := f.takeSent(); len(s) != 1 || !strings.HasPrefix(s[0], "Внимание: LLM budget-bot") {
		t.Fatalf("80 %%: %v", s)
	}
	f.llm.st.Clients[0].SpentUSD = 2
	f.svc.Tick(ctx)
	if s := f.takeSent(); len(s) != 1 || !strings.HasPrefix(s[0], "Критично") || !strings.Contains(s[0], "отклоняются") {
		t.Fatalf("100 %%: %v", s)
	}
}

func TestHostThresholdAndBattery(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.metrics.snap.MemPct = 96
	f.metrics.snap.Battery = &host.Battery{Percent: 80, Status: "Discharging", Discharging: true}
	f.svc.Tick(ctx)
	s := strings.Join(f.takeSent(), "\n")
	if !strings.Contains(s, "Критично: занято памяти 96 %") || !strings.Contains(s, "Внимание: сервер работает от батареи") {
		t.Fatalf("алерты: %s", s)
	}
}

func TestAggregatesAndWeeklyReportOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for i, v := range []float64{40, 60, 80} {
		f.now = time.Date(2026, 10, 1, 10, i, 0, 0, time.UTC)
		f.metrics.snap.CPUTemp = v
		f.svc.Tick(ctx)
	}
	// Вчерашний замер в сводку не попадает.
	if err := f.svc.store.AddSamples(ctx, time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC), map[string]float64{mCPUTemp: 99}); err != nil {
		t.Fatal(err)
	}
	aggs, err := f.svc.store.Aggregates(ctx, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if a := aggs[mCPUTemp]; a.Min != 40 || a.Avg != 60 || a.Max != 80 || a.N != 3 {
		t.Fatalf("CPU за день: %+v", a)
	}

	f.takeSent()
	f.metrics.snap.CPUTemp = host.Unknown // тики ниже не должны добавить замеров CPU
	f.svc.cfg.Report = Schedule{Day: time.Sunday, At: "20:00"}
	// 04.10.2026 — воскресенье. До 20:00 отчёта нет, в 20:00 — один, минутой позже — не второй.
	for _, at := range []time.Time{
		time.Date(2026, 10, 4, 19, 59, 0, 0, time.UTC),
		time.Date(2026, 10, 4, 20, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 4, 20, 1, 0, 0, time.UTC),
	} {
		f.now = at
		f.svc.Tick(ctx)
	}
	var reports int
	for _, s := range f.takeSent() {
		if strings.HasPrefix(s, "Отчёт за неделю 27.09–04.10") {
			reports++
			// Замер 30.09 в неделю входит, в отличие от сводки за день выше.
			if !strings.Contains(s, "CPU °C: 40 / 70 / 99") {
				t.Fatalf("отчёт: %s", s)
			}
		}
	}
	if reports != 1 {
		t.Fatalf("отчётов %d, ждали 1", reports)
	}
}

func TestWeeklyReportNotSentLate(t *testing.T) {
	f := newFixture(t)
	f.svc.cfg.Report = Schedule{Day: time.Sunday, At: "20:00"}
	// Бот впервые запущен в среду: отчёт за прошлое воскресенье слать поздно.
	f.now = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	f.svc.Tick(context.Background())
	for _, s := range f.takeSent() {
		if strings.HasPrefix(s, "Отчёт") {
			t.Fatalf("опоздавший отчёт: %s", s)
		}
	}
}

func TestParseSchedule(t *testing.T) {
	got, err := ParseSchedule("Sun 20:00")
	if err != nil || got.Day != time.Sunday || got.At != "20:00" {
		t.Fatalf("Sun 20:00: %+v, %v", got, err)
	}
	for _, bad := range []string{"20:00", "Вс 20:00", "Sun 25:00", "Sun"} {
		if _, err := ParseSchedule(bad); err == nil {
			t.Errorf("%q принято", bad)
		}
	}
}

func TestSplitTextByLines(t *testing.T) {
	text := strings.Repeat("абвгд\n", 10) // 60 символов
	parts := SplitText(text, 25)
	if strings.Join(parts, "") != text {
		t.Fatal("текст потерян при разбиении")
	}
	for _, p := range parts {
		if n := len([]rune(p)); n > 25 || !strings.HasSuffix(p, "\n") {
			t.Fatalf("часть %q: %d символов", p, n)
		}
	}
	if got := SplitText("коротко", 25); len(got) != 1 {
		t.Fatalf("короткий текст разбит: %v", got)
	}
}

func TestSeriesAveragesByStep(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "ops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	base := time.Unix(1_800_000_000, 0) // кратно 600
	for i, v := range []float64{10, 20, 30, 40} {
		at := base.Add(time.Duration(i) * 5 * time.Minute)
		if err := store.AddSamples(ctx, at, map[string]float64{"cpu_temp": v, "mem": 1}); err != nil {
			t.Fatal(err)
		}
	}
	pts, err := store.Series(ctx, "cpu_temp", base, base.Add(time.Hour), 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 || pts[0].V != 15 || pts[1].V != 35 || !pts[1].T.Equal(base.Add(10*time.Minute)) {
		t.Fatalf("точки: %+v", pts)
	}
}

func TestLatestTakesNewestFreshValue(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "ops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	base := time.Unix(1_800_000_000, 0)
	_ = store.AddSamples(ctx, base.Add(-time.Hour), map[string]float64{"swap": 5})
	_ = store.AddSamples(ctx, base.Add(-2*time.Minute), map[string]float64{"cpu_temp": 40, "mem": 50})
	_ = store.AddSamples(ctx, base.Add(-time.Minute), map[string]float64{"cpu_temp": 45})
	got, err := store.Latest(ctx, base.Add(-10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got["cpu_temp"] != 45 || got["mem"] != 50 {
		t.Fatalf("последние значения: %v", got)
	}
	if _, ok := got["swap"]; ok {
		t.Fatalf("старый замер не должен попасть: %v", got)
	}
}

func TestPanelCommandSendsLinkWithoutPreview(t *testing.T) {
	f := newFixture(t)
	if r := f.svc.Handle(context.Background(), ownChat, "/panel"); !strings.Contains(r[0].Text, "не настроена") {
		t.Fatalf("без панели: %q", r[0].Text)
	}
	f.svc.panel = func(context.Context) (string, error) { return "http://192.168.1.50:8800/login?t=abc", nil }
	r := f.svc.Handle(context.Background(), ownChat, "/panel")
	if !strings.Contains(r[0].Text, "http://192.168.1.50:8800/login?t=abc") || !r[0].NoPreview {
		t.Fatalf("ответ %+v", r[0])
	}
}
