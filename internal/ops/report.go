package ops

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"homelab/internal/gateway"
	"homelab/internal/host"
)

// Метрики в базе замеров.
const (
	mCPUTemp = "cpu_temp"
	mSSDTemp = "ssd_temp"
	mCPUBusy = "cpu_busy"
	mMem     = "mem"
	mSwap    = "swap"
	mLoad    = "load5"
	mDisk    = "disk:"
)

func (s *Service) snapshot() host.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *Service) cmdStats(ctx context.Context) Reply {
	snap := s.snapshot()
	var b strings.Builder
	fmt.Fprintf(&b, "CPU: %s, загрузка %s\n", num(snap.CPUTemp, "%.0f °C"), num(snap.CPUBusyPct, "%.0f %%"))
	fmt.Fprintf(&b, "Load: %s / %s / %s, ядер %d\n", num(snap.Load1, "%.2f"), num(snap.Load5, "%.2f"), num(snap.Load15, "%.2f"), snap.CPUs)
	fmt.Fprintf(&b, "SSD: %s\n", num(snap.SSDTemp, "%.0f °C"))
	fmt.Fprintf(&b, "RAM: %s, swap %s\n", num(snap.MemPct, "%.0f %%"), num(snap.SwapPct, "%.0f %%"))
	for _, d := range snap.Disks {
		fmt.Fprintf(&b, "Диск %s: %.0f %%, свободно %s\n", d.Path, d.UsedPct, bytesHuman(d.Free))
	}
	if snap.Battery != nil {
		fmt.Fprintf(&b, "Батарея: %.0f %%, %s\n", snap.Battery.Percent, batteryStatus(snap.Battery.Status))
	}
	if snap.Uptime > 0 {
		fmt.Fprintf(&b, "Сервер работает %s\n", since(snap.Uptime))
	}

	now := s.now().In(s.cfg.Location)
	if aggs, err := s.store.Aggregates(ctx, dayStart(now), now.Add(time.Minute)); err == nil && len(aggs) > 0 {
		b.WriteString("\nЗа сегодня, мин / сред / макс:\n")
		b.WriteString(aggLines(aggs))
	}
	return Reply{Text: strings.TrimSpace(b.String())}
}

func batteryStatus(s string) string {
	switch s {
	case "Discharging":
		return "разряжается, питание отключено"
	case "Charging":
		return "заряжается"
	case "Full":
		return "заряжена"
	case "Not charging":
		return "не заряжается"
	}
	return s
}

func aggLines(aggs map[string]Agg) string {
	labels := []struct{ metric, label, format string }{
		{mCPUTemp, "CPU °C", "%.0f"},
		{mSSDTemp, "SSD °C", "%.0f"},
		{mCPUBusy, "Загрузка CPU %", "%.0f"},
		{mMem, "RAM %", "%.0f"},
		{mSwap, "Swap %", "%.0f"},
		{mLoad, "Load", "%.2f"},
	}
	var b strings.Builder
	for _, l := range labels {
		if a, ok := aggs[l.metric]; ok {
			f := l.format
			fmt.Fprintf(&b, "%s: "+f+" / "+f+" / "+f+"\n", l.label, a.Min, a.Avg, a.Max)
		}
	}
	var disks []string
	for m := range aggs {
		if strings.HasPrefix(m, mDisk) {
			disks = append(disks, m)
		}
	}
	sort.Strings(disks)
	for _, m := range disks {
		fmt.Fprintf(&b, "Диск %s %%: макс %.0f\n", strings.TrimPrefix(m, mDisk), aggs[m].Max)
	}
	return b.String()
}

func (s *Service) cmdUsage(ctx context.Context) Reply {
	if s.llm == nil {
		return Reply{Text: "Шлюз LLM не настроен: нет LLM_ADMIN_TOKEN."}
	}
	st, err := s.llm.Status(ctx)
	if err != nil {
		return Reply{Text: "Шлюз LLM не ответил: " + err.Error()}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "LLM за %s\n", st.Month)
	for _, c := range st.Clients {
		fmt.Fprintf(&b, "%s: $%.2f из $%.2f (%.0f %%)\n", c.Name, c.SpentUSD, c.LimitUSD, percent(c.SpentUSD, c.LimitUSD))
	}
	fmt.Fprintf(&b, "Всего: $%.2f из $%.2f (%.0f %%)\n", st.TotalSpentUSD, st.TotalLimitUSD, percent(st.TotalSpentUSD, st.TotalLimitUSD))
	b.WriteString(balanceLine(st.Balance) + "\n")

	now := s.now().In(s.cfg.Location)
	if rows, err := s.llm.Usage(ctx, dayStart(now)); err == nil {
		b.WriteString("\nСегодня:\n")
		if len(rows) == 0 {
			b.WriteString("вызовов не было\n")
		}
		for _, r := range rows {
			fmt.Fprintf(&b, "%s, %s: %d вызовов, вход %s, выход %s, $%.3f", r.Client, r.Model, r.Calls,
				tokens(r.InputTokens+r.CachedTokens), tokens(r.OutputTokens+r.ThoughtsTokens), r.CostUSD)
			if r.Errors > 0 || r.Blocked > 0 {
				fmt.Fprintf(&b, "; ошибок %d, отклонено по лимиту %d", r.Errors, r.Blocked)
			}
			b.WriteString("\n")
		}
	}
	if len(st.UnpricedModels) > 0 {
		fmt.Fprintf(&b, "\nНет цены для: %s — расход по ним не считается.\n", strings.Join(st.UnpricedModels, ", "))
	}
	return Reply{Text: strings.TrimSpace(b.String())}
}

func balanceLine(b gateway.Balance) string {
	if !b.Known {
		return "Баланс не задан: /balance <остаток из AI Studio>"
	}
	line := fmt.Sprintf("Баланс: ~$%.2f", b.USD)
	if b.SetAt != nil {
		line += fmt.Sprintf(" (сверка %s, после неё пополнено $%.2f, потрачено $%.2f)", b.SetAt.Format("02.01"), b.Topups, b.Spent)
	}
	return line
}

func (s *Service) cmdLedger(ctx context.Context, kind string, args []string) Reply {
	if s.llm == nil {
		return Reply{Text: "Шлюз LLM не настроен: нет LLM_ADMIN_TOKEN."}
	}
	if len(args) == 0 {
		if kind == "topup" {
			return Reply{Text: "Нужна сумма в долларах: /topup 10"}
		}
		st, err := s.llm.Status(ctx)
		if err != nil {
			return Reply{Text: "Шлюз LLM не ответил: " + err.Error()}
		}
		return Reply{Text: balanceLine(st.Balance)}
	}
	usd, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimPrefix(args[0], "$"), ",", "."), 64)
	if err != nil || usd < 0 || (kind == "topup" && usd == 0) {
		return Reply{Text: "Сумма — число в долларах, например 10 или 17.40"}
	}
	bal, err := s.llm.Ledger(ctx, kind, usd)
	if err != nil {
		return Reply{Text: "Не записано: " + err.Error()}
	}
	what := fmt.Sprintf("Пополнение $%.2f записано.", usd)
	if kind == "set" {
		what = fmt.Sprintf("Баланс сверен: $%.2f.", usd)
	}
	return Reply{Text: what + " " + balanceLine(bal)}
}

func percent(v, limit float64) float64 {
	if limit <= 0 {
		return 0
	}
	return 100 * v / limit
}

func tokens(n int64) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return strconv.FormatInt(n, 10)
}

// Schedule — время недельного отчёта: день недели и «20:00» в поясе бота.
type Schedule struct {
	Day time.Weekday
	At  string
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// ParseSchedule разбирает «Sun 20:00».
func ParseSchedule(v string) (Schedule, error) {
	day, at, ok := strings.Cut(strings.TrimSpace(v), " ")
	wd, known := weekdays[strings.ToLower(day)]
	if _, err := time.Parse("15:04", at); !ok || !known || err != nil {
		return Schedule{}, fmt.Errorf("время отчёта %q: нужно «день время», например Sun 20:00", v)
	}
	return Schedule{Day: wd, At: at}, nil
}

// last — последний момент отчёта, не позже now.
func (sc Schedule) last(now time.Time) (time.Time, bool) {
	at, err := time.Parse("15:04", sc.At)
	if err != nil {
		return time.Time{}, false
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), at.Hour(), at.Minute(), 0, 0, now.Location())
	for t.Weekday() != sc.Day || t.After(now) {
		t = t.AddDate(0, 0, -1)
	}
	return t, true
}

// weeklyReport — сводка за [from, to): метрики, падения контейнеров, LLM, активные алерты.
func (s *Service) weeklyReport(ctx context.Context, from, to time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Отчёт за неделю %s–%s\n\n", from.Format("02.01"), to.Format("02.01"))

	if aggs, err := s.store.Aggregates(ctx, from, to); err == nil && len(aggs) > 0 {
		b.WriteString("Мин / сред / макс:\n" + aggLines(aggs))
	} else {
		b.WriteString("Замеров за неделю нет.\n")
	}

	falls, err := s.store.Falls(ctx, from, to)
	switch {
	case err != nil:
		fmt.Fprintf(&b, "\nПадения: не прочитаны (%v)\n", err)
	case len(falls) == 0:
		b.WriteString("\nПадений контейнеров не было.\n")
	default:
		names := make([]string, 0, len(falls))
		for n := range falls {
			names = append(names, n)
		}
		sort.Strings(names)
		b.WriteString("\nПадения: ")
		for i, n := range names {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s %d", n, falls[n])
		}
		b.WriteString("\n")
	}

	if s.llm != nil {
		if st, err := s.llm.Status(ctx); err == nil {
			var week float64
			if rows, err := s.llm.Usage(ctx, from); err == nil {
				for _, r := range rows {
					week += r.CostUSD
				}
			}
			fmt.Fprintf(&b, "\nLLM: за неделю $%.2f, за месяц $%.2f из $%.2f. %s\n", week, st.TotalSpentUSD, st.TotalLimitUSD, balanceLine(st.Balance))
		}
	}

	if active := s.alerts.Active(); len(active) > 0 {
		b.WriteString("\nАктивные алерты:\n")
		for _, a := range active {
			fmt.Fprintf(&b, "- %s: %s\n", a.Level, a.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// maxComment — сколько символов комментария LLM оставить: отчёт не должен утонуть в болтовне.
const maxComment = 600

// reportComment — пара фраз персонажа к отчёту от LLM; без LLM или при ошибке — запасная фраза.
func (s *Service) reportComment(ctx context.Context, report string) string {
	prompt := s.voice.Text("report-prompt")
	if s.commenter != nil && prompt != "" {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		text, err := s.commenter.Comment(ctx, prompt, report)
		text = strings.TrimSpace(text)
		if err == nil && text != "" {
			if r := []rune(text); len(r) > maxComment {
				text = strings.TrimSpace(string(r[:maxComment])) + "…"
			}
			return text
		}
		s.log.Warn("комментарий к отчёту не получен", "err", err)
	}
	return s.voice.Line("report-fallback", "")
}
