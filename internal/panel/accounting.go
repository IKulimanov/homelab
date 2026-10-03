package panel

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"homelab/internal/gateway"
)

type spendBar struct {
	Title    string
	SpentUSD float64
	LimitUSD float64
	Pct      float64
	Level    string
}

type dayBar struct {
	Day     string
	CostUSD float64
	Calls   int64
	Pct     float64
}

type accountingView struct {
	Status  *gateway.Status
	Total   spendBar
	Clients []spendBar
	Days    []dayBar
	Error   string
	LowBal  bool
}

func spend(title string, spent, limit float64) spendBar {
	b := spendBar{Title: title, SpentUSD: spent, LimitUSD: limit, Level: "ok"}
	if limit > 0 {
		b.Pct = clamp(spent/limit*100, 0, 100)
		switch {
		case spent >= limit:
			b.Level = "bad"
		case spent >= limit*0.8:
			b.Level = "warn"
		}
	}
	return b
}

func (s *Server) accounting(w http.ResponseWriter, r *http.Request) {
	var v accountingView
	if s.Gateway == nil {
		v.Error = "Шлюз LLM не настроен: нужен LLM_ADMIN_TOKEN в окружении панели."
		s.render(w, r, "accounting", http.StatusOK, page{Title: "Бухгалтерия", Active: "accounting", Data: v})
		return
	}
	st, err := s.Gateway.Status(r.Context())
	if err != nil {
		v.Error = "Шлюз не ответил: " + err.Error()
		s.render(w, r, "accounting", http.StatusOK, page{Title: "Бухгалтерия", Active: "accounting", Data: v})
		return
	}
	v.Status = &st
	v.Total = spend("Все сервисы", st.TotalSpentUSD, st.TotalLimitUSD)
	for _, c := range st.Clients {
		v.Clients = append(v.Clients, spend(c.Name, c.SpentUSD, c.LimitUSD))
	}
	sort.SliceStable(v.Clients, func(i, j int) bool { return v.Clients[i].SpentUSD > v.Clients[j].SpentUSD })
	v.LowBal = st.Balance.Known && st.Balance.USD < st.BalanceAlertUSD

	now := s.Now().Local()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	rows, err := s.Gateway.Daily(r.Context(), monthStart)
	if err != nil {
		v.Error = "Расход по дням не получен: " + err.Error()
	}
	v.Days = daysOf(rows, monthStart, now)
	s.render(w, r, "accounting", http.StatusOK, page{Title: "Бухгалтерия", Active: "accounting", Data: v})
}

// daysOf — столбики по дням месяца до сегодня, пустые дни тоже: так видно, когда расхода не было.
func daysOf(rows []gateway.DailyRow, from, to time.Time) []dayBar {
	sum := map[string]*dayBar{}
	for _, r := range rows {
		b := sum[r.Day]
		if b == nil {
			b = &dayBar{}
			sum[r.Day] = b
		}
		b.CostUSD += r.CostUSD
		b.Calls += r.Calls
	}
	var out []dayBar
	top := 0.0
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		b := dayBar{Day: d.Format("02")}
		if v := sum[key]; v != nil {
			b.CostUSD, b.Calls = v.CostUSD, v.Calls
		}
		top = max(top, b.CostUSD)
		out = append(out, b)
	}
	for i := range out {
		if top > 0 {
			out[i].Pct = out[i].CostUSD / top * 100
		}
	}
	return out
}

// accountingLedger — пополнение (topup) или сверка с AI Studio (set). Лимиты меняются только в git.
func (s *Server) accountingLedger(w http.ResponseWriter, r *http.Request) {
	if s.Gateway == nil {
		http.Error(w, "шлюз не настроен", http.StatusServiceUnavailable)
		return
	}
	kind := r.PostFormValue("kind")
	usd, err := strconv.ParseFloat(strings.Replace(strings.TrimSpace(r.PostFormValue("usd")), ",", ".", 1), 64)
	switch {
	case kind != "topup" && kind != "set":
		http.Error(w, "неизвестная операция", http.StatusBadRequest)
		return
	case err != nil || usd < 0 || usd > 10000 || (kind == "topup" && usd == 0):
		back(w, r, "/accounting", "Сумма должна быть числом в долларах, например 10 или 4,25.")
		return
	}
	bal, err := s.Gateway.Ledger(r.Context(), kind, usd)
	if err != nil {
		back(w, r, "/accounting", "Шлюз не принял: "+err.Error())
		return
	}
	text, toast := fmt.Sprintf("пополнение на $%.2f, баланс $%.2f", usd, bal.USD), "Заткнись и возьми мои деньги! Баланс: "+fmt.Sprintf("$%.2f", bal.USD)
	if kind == "set" {
		text, toast = fmt.Sprintf("баланс сверен с AI Studio: $%.2f", usd), "Гермес сверил книги. Баланс: "+fmt.Sprintf("$%.2f", bal.USD)
	}
	s.act(r.Context(), "ledger-"+kind, "llm-gateway", fmt.Sprintf("%.2f", usd), text)
	back(w, r, "/accounting", toast)
}
