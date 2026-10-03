package gateway

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Status — ответ /admin/status: всё, что нужно ops-bot для алертов и /usage.
type Status struct {
	Month           string         `json:"month"`
	Clients         []ClientStatus `json:"clients"`
	TotalSpentUSD   float64        `json:"total_spent_usd"`
	TotalLimitUSD   float64        `json:"total_limit_usd"`
	Balance         Balance        `json:"balance"`
	BalanceAlertUSD float64        `json:"balance_alert_usd"`
	UnpricedModels  []string       `json:"unpriced_models,omitempty"`
}

type ClientStatus struct {
	Name     string  `json:"name"`
	SpentUSD float64 `json:"spent_usd"`
	LimitUSD float64 `json:"limit_usd"`
}

// LedgerRequest — тело /admin/topup и /admin/balance.
type LedgerRequest struct {
	USD  float64 `json:"usd"`
	Note string  `json:"note"`
}

// admin пускает только с токеном. Без токена в окружении админ-API выключен.
func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.AdminToken == "" || !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.AdminToken)) != 1 {
			apiError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "нужен токен администратора")
			return
		}
		h(w, r)
	}
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config.Get()
	now := s.Now()
	since := MonthStart(now)
	by, total, err := s.Store.Spent(r.Context(), since)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	bal, err := s.Store.Balance(r.Context())
	if err != nil {
		apiError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	rows, err := s.Store.Usage(r.Context(), since)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}

	st := Status{
		Month:           since.Format("2006-01"),
		TotalSpentUSD:   USD(total),
		TotalLimitUSD:   cfg.TotalMonthlyLimitUSD,
		Balance:         bal,
		BalanceAlertUSD: cfg.BalanceAlertUSD,
	}
	for _, name := range cfg.ClientNames() {
		st.Clients = append(st.Clients, ClientStatus{Name: name, SpentUSD: USD(by[name]), LimitUSD: cfg.Clients[name].MonthlyLimitUSD})
	}
	unpriced := map[string]bool{}
	for _, row := range rows {
		if row.Unpriced > 0 {
			unpriced[row.Model] = true
		}
	}
	for m := range unpriced {
		st.UnpricedModels = append(st.UnpricedModels, m)
	}
	sort.Strings(st.UnpricedModels)
	writeJSON(w, http.StatusOK, st)
}

// usage — сводка с момента ?since=RFC3339.
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	since, err := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
	if err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "since: нужна дата в RFC3339")
		return
	}
	rows, err := s.Store.Usage(r.Context(), since)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	if rows == nil {
		rows = []UsageRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// daily — расход по суткам с момента ?since=RFC3339 для графика в панели. Сутки — в поясе сервера (TZ).
func (s *Server) daily(w http.ResponseWriter, r *http.Request) {
	since, err := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
	if err != nil {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "since: нужна дата в RFC3339")
		return
	}
	rows, err := s.Store.Daily(r.Context(), since, s.Now().Location())
	if err != nil {
		apiError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	if rows == nil {
		rows = []DailyRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// ledger — пополнение (topup, сумма больше нуля) или сверка с AI Studio (set, сумма не меньше нуля).
func (s *Server) ledger(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req LedgerRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "тело: {\"usd\": число}")
			return
		}
		if req.USD < 0 || (kind == "topup" && req.USD == 0) {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "сумма должна быть больше нуля")
			return
		}
		if err := s.Store.AddLedger(r.Context(), s.Now(), kind, Micros(req.USD), req.Note); err != nil {
			apiError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		s.Log.Info("баланс", "kind", kind, "usd", req.USD)
		bal, err := s.Store.Balance(r.Context())
		if err != nil {
			apiError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, bal)
	}
}
