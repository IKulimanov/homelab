package panel

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"homelab/internal/ops"
)

// Metrics — замеры ops-bot для Мостика.
type Metrics interface {
	Latest(ctx context.Context, since time.Time) (map[string]float64, error)
	Series(ctx context.Context, metric string, from, to time.Time, step time.Duration) ([]ops.Point, error)
}

// LazyMetrics открывает базу замеров при первом обращении: ops-bot может создать её позже панели.
type LazyMetrics struct {
	Path string

	mu sync.Mutex
	st *ops.Store
}

func (l *LazyMetrics) store() (*ops.Store, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.st != nil {
		return l.st, nil
	}
	st, err := ops.OpenStoreRO(l.Path)
	if err != nil {
		return nil, err
	}
	l.st = st
	return st, nil
}

func (l *LazyMetrics) Latest(ctx context.Context, since time.Time) (map[string]float64, error) {
	st, err := l.store()
	if err != nil {
		return nil, err
	}
	return st.Latest(ctx, since)
}

func (l *LazyMetrics) Series(ctx context.Context, metric string, from, to time.Time, step time.Duration) ([]ops.Point, error) {
	st, err := l.store()
	if err != nil {
		return nil, err
	}
	return st.Series(ctx, metric, from, to, step)
}

type gauge struct {
	Title string
	Value string
	Note  string
	Pct   float64
	Level string // ok, warn, bad
}

type problem struct {
	Level string
	Title string
	Text  string
	Link  string
}

type funView struct {
	funLink
	URL   string
	State string // ok, off, warn
}

type chart struct {
	Title  string
	Unit   string
	Points string // для SVG polyline, поле 300×80
	Last   string
	Min    string
	Max    string
	Empty  bool
}

type bridgeView struct {
	Ship       string // idle, flying, smoke
	Flame      string // low, mid, high
	Phrase     string
	Gauges     []gauge
	Tanks      []gauge
	Problems   []problem
	Deliveries []delivery
	Fun        []funView
	Range      int
	Ranges     []int
	Charts     []chart
	NoMetrics  bool
}

func (s *Server) bridge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v := bridgeView{Ship: "idle", Flame: "low", Ranges: []int{1, 7, 30}, Range: 1}
	if r.URL.Query().Get("range") == "7" {
		v.Range = 7
	} else if r.URL.Query().Get("range") == "30" {
		v.Range = 30
	}

	cards, err := s.crewCards(ctx)
	if err != nil {
		v.Problems = append(v.Problems, problem{Level: "bad", Title: "Docker", Text: "не отвечает: " + err.Error()})
	}
	for _, c := range cards {
		switch {
		case c.Cond.Problem:
			v.Problems = append(v.Problems, problem{Level: c.Cond.Level, Title: c.Name, Text: c.Cond.Text, Link: "/logs?svc=" + c.Name})
		case !c.Installed && len(c.Missing) > 0:
			v.Problems = append(v.Problems, problem{Level: "warn", Title: c.Name, Text: "ждёт секретов: " + strings.Join(c.Missing, ", "), Link: "/vault/" + c.Name + ".env"})
		case c.Installed && len(c.Missing) > 0:
			v.Problems = append(v.Problems, problem{Level: "warn", Title: c.Name, Text: "пустые обязательные ключи: " + strings.Join(c.Missing, ", "), Link: "/vault/" + c.Name + ".env"})
		}
	}

	if s.Metrics != nil {
		latest, err := s.Metrics.Latest(ctx, s.Now().Add(-10*time.Minute))
		if err != nil || len(latest) == 0 {
			v.NoMetrics = true
		} else {
			v.Gauges, v.Tanks = gaugesOf(latest)
			for _, g := range append(slices.Clone(v.Gauges), v.Tanks...) {
				if g.Level != "ok" {
					v.Problems = append(v.Problems, problem{Level: g.Level, Title: g.Title, Text: g.Value + " " + g.Note})
				}
			}
			if load, ok := latest["load5"]; ok {
				perCore := load / float64(runtime.NumCPU())
				switch {
				case perCore >= 2:
					v.Flame = "high"
				case perCore >= 0.7:
					v.Flame = "mid"
				}
			}
		}
		v.Charts = s.charts(ctx, v.Range)
	} else {
		v.NoMetrics = true
	}

	switch {
	case s.running():
		v.Ship, v.Phrase = "flying", "Доставка в пути: update.sh работает."
	case hasBad(v.Problems):
		v.Ship, v.Phrase = "smoke", "Кажется, что-то горит. Не я, честно."
	case len(v.Problems) > 0:
		v.Phrase = "Летим, но кое-что требует внимания."
	default:
		v.Phrase = "Все на борту, ничего не горит. Скукота."
	}
	sort.SliceStable(v.Problems, func(i, j int) bool { return v.Problems[i].Level == "bad" && v.Problems[j].Level != "bad" })

	h := s.history()
	v.Deliveries = h[:min(3, len(h))]

	running := map[string]string{}
	for _, c := range cards {
		if c.Installed {
			running[c.Name] = c.Cond.Level
		}
	}
	for _, f := range s.config().Fun {
		fv := funView{funLink: f, URL: s.siteURL(r, f.Port, f.Path), State: "off"}
		if lvl, ok := running[f.Container]; ok {
			fv.State = lvl
		}
		v.Fun = append(v.Fun, fv)
	}
	s.render(w, r, "bridge", http.StatusOK, page{Title: "Мостик", Active: "bridge", Data: v})
}

func hasBad(ps []problem) bool {
	return slices.ContainsFunc(ps, func(p problem) bool { return p.Level == "bad" })
}

// gaugesOf — приборы и баки (диски) из последних замеров. Пороги те же, что у алертов ops-bot.
func gaugesOf(m map[string]float64) (gauges, tanks []gauge) {
	th := ops.DefaultThresholds()
	level := func(v float64, r ops.Range) string {
		switch {
		case v >= r.Crit:
			return "bad"
		case v >= r.Warn:
			return "warn"
		}
		return "ok"
	}
	if v, ok := m["cpu_temp"]; ok {
		gauges = append(gauges, gauge{Title: "Процессор", Value: fmt.Sprintf("%.0f °C", v), Note: "температура", Pct: v, Level: level(v, th.CPUTemp)})
	}
	if v, ok := m["ssd_temp"]; ok {
		gauges = append(gauges, gauge{Title: "SSD", Value: fmt.Sprintf("%.0f °C", v), Note: "температура", Pct: v / 80 * 100, Level: level(v, th.SSDTemp)})
	}
	if v, ok := m["mem"]; ok {
		gauges = append(gauges, gauge{Title: "Память", Value: fmt.Sprintf("%.0f %%", v), Note: "занято", Pct: v, Level: level(v, th.Mem)})
	}
	if v, ok := m["load5"]; ok {
		cpus := float64(runtime.NumCPU())
		gauges = append(gauges, gauge{Title: "Нагрузка", Value: trimFloat(v), Note: fmt.Sprintf("за 5 минут, ядер %.0f", cpus),
			Pct: v / cpus / th.Load.Crit * 100, Level: level(v/cpus, th.Load)})
	}
	var disks []string
	for k := range m {
		if strings.HasPrefix(k, "disk:") {
			disks = append(disks, k)
		}
	}
	slices.Sort(disks)
	for _, k := range disks {
		v := m[k]
		tanks = append(tanks, gauge{Title: strings.TrimPrefix(k, "disk:"), Value: fmt.Sprintf("%.0f %%", v), Note: "занято", Pct: v, Level: level(v, th.Disk)})
	}
	return gauges, tanks
}

func (s *Server) charts(ctx context.Context, days int) []chart {
	to := s.Now()
	from := to.Add(-time.Duration(days) * 24 * time.Hour)
	step := time.Duration(days) * 24 * time.Hour / 120
	defs := []struct{ metric, title, unit string }{
		{"cpu_temp", "Температура процессора", "°C"},
		{"mem", "Память", "%"},
		{"load5", "Нагрузка", ""},
	}
	var out []chart
	for _, d := range defs {
		pts, err := s.Metrics.Series(ctx, d.metric, from, to, step)
		c := chart{Title: d.title, Unit: d.unit}
		if err != nil || len(pts) == 0 {
			c.Empty = true
			out = append(out, c)
			continue
		}
		lo, hi := pts[0].V, pts[0].V
		for _, p := range pts {
			lo, hi = min(lo, p.V), max(hi, p.V)
		}
		span := hi - lo
		if span == 0 {
			span = 1
		}
		var b strings.Builder
		total := to.Sub(from).Seconds()
		for i, p := range pts {
			x := p.T.Sub(from).Seconds() / total * 300
			y := 76 - (p.V-lo)/span*72
			if i > 0 {
				b.WriteByte(' ')
			}
			fmt.Fprintf(&b, "%.1f,%.1f", x, y)
		}
		c.Points = b.String()
		c.Last, c.Min, c.Max = trimFloat(pts[len(pts)-1].V), trimFloat(lo), trimFloat(hi)
		out = append(out, c)
	}
	return out
}
