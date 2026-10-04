package alert

import (
	"strings"
	"testing"
	"time"
)

func TestEngineCooldownEscalationAndRecovery(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	e := NewEngine(30 * time.Minute)
	e.Now = func() time.Time { return now }
	step := func(d time.Duration, level Level, wantPrefix string) {
		t.Helper()
		now = now.Add(d)
		got := e.Update("ram", level, "RAM 90 %").Text
		if wantPrefix == "" && got != "" || wantPrefix != "" && !strings.HasPrefix(got, wantPrefix) {
			t.Fatalf("через %v уровень %v: %q, ждали %q", d, level, got, wantPrefix)
		}
	}

	step(0, OK, "")                          // норма без беды — тишина
	step(0, Warn, "⚠️ Внимание:")            // первая беда — сразу
	step(time.Minute, Warn, "")              // повтор раньше cooldown — тишина
	step(10*time.Minute, Crit, "🚨 Критично") // ухудшение — сразу, несмотря на cooldown
	step(10*time.Minute, Warn, "")           // полегчало, но не норма — тишина
	step(10*time.Minute, Crit, "🚨 Критично") // снова хуже — сразу
	step(29*time.Minute, Crit, "")
	step(time.Minute, Crit, "🚨 Критично, всё ещё") // прошло 30 минут — напоминание
	step(time.Minute, OK, "✅ Восстановилось")
	step(time.Minute, OK, "") // «восстановилось» — один раз
}

func TestEngineWarnNotRepeated(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	e := NewEngine(time.Hour)
	e.Now = func() time.Time { return now }
	if m := e.Update("disk:/", Warn, "диск 85 %"); m.Kind != New {
		t.Fatalf("первое: %+v", m)
	}
	now = now.Add(5 * time.Hour)
	if m := e.Update("disk:/", Warn, "диск 86 %"); m.Text != "" {
		t.Fatalf("«Внимание» повторилось: %+v", m)
	}
}

func TestEngineRepeatCounter(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	e := NewEngine(time.Hour)
	e.Now = func() time.Time { return now }
	e.Update("cpu", Crit, "91")
	for _, want := range []int{1, 2, 3, 4} {
		now = now.Add(time.Hour)
		m := e.Update("cpu", Crit, "91")
		if m.Kind != Repeat || m.Repeat != want {
			t.Fatalf("повтор %d: %+v", want, m)
		}
	}
	if m := e.Update("cpu", OK, "70"); m.Kind != Recovered {
		t.Fatalf("восстановление: %+v", m)
	}
	e.Update("cpu", Crit, "92")
	now = now.Add(time.Hour)
	if m := e.Update("cpu", Crit, "92"); m.Repeat != 1 {
		t.Fatalf("счётчик не сброшен после восстановления: %+v", m)
	}
}

func TestEngineKeysIndependent(t *testing.T) {
	e := NewEngine(time.Hour)
	if e.Update("disk:/", Warn, "a").Text == "" || e.Update("disk:/data", Warn, "b").Text == "" {
		t.Fatal("разные проверки глушат друг друга")
	}
	if got := e.Active(); len(got) != 2 {
		t.Fatalf("активных %d", len(got))
	}
}

func TestThresholds(t *testing.T) {
	cases := []struct {
		got, want Level
	}{
		{High(74.9, 75, 90), OK},
		{High(75, 75, 90), Warn},
		{High(95, 75, 90), Crit},
		{Low(25, 20, 10), OK},
		{Low(20, 20, 10), Warn},
		{Low(5, 20, 10), Crit},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("случай %d: %v, ждали %v", i, c.got, c.want)
		}
	}
}
