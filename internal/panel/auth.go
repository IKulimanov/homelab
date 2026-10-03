package panel

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type loginView struct {
	Variant string // login, confirm, expired, notfound
	Token   string
	Expires time.Time
}

func (v loginView) Ship() string {
	if v.Variant == "notfound" {
		return "smoke"
	}
	return "idle"
}

func (v loginView) Flame() string { return "mid" }

// LoginLink — новая одноразовая ссылка входа. Её же печатает запасной вход `/app -login-link`.
func (s *Server) LoginLink(ctx context.Context) (string, error) {
	s.init()
	tok, err := s.Store.NewLoginToken(ctx, s.Now(), s.LoginTTL)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(s.URL, "/") + "/login?t=" + url.QueryEscape(tok), nil
}

func (s *Server) internalLoginLink(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if s.Token == "" || !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(s.Token)) != 1 {
		http.Error(w, "нужен PANEL_TOKEN", http.StatusUnauthorized)
		return
	}
	link, err := s.LoginLink(r.Context())
	if err != nil {
		s.Log.Error("ссылка входа", "err", err)
		http.Error(w, "не удалось выдать пропуск", http.StatusInternalServerError)
		return
	}
	s.Log.Info("выдана ссылка входа")
	writeJSON(w, http.StatusOK, map[string]string{"url": link})
}

// loginPage только показывает кнопку «Войти»: GET не тратит пропуск, иначе его съело бы превью ссылки.
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("t")
	if tok == "" {
		s.render(w, r, "login", http.StatusOK, page{Title: "Ждём курьера", Data: loginView{Variant: "login"}})
		return
	}
	exp, ok := s.Store.TokenValid(r.Context(), tok, s.Now())
	if !ok {
		s.render(w, r, "login", http.StatusOK, page{Title: "Пропуск протух", Data: loginView{Variant: "expired"}})
		return
	}
	s.render(w, r, "login", http.StatusOK, page{Title: "Курьер прибыл", Data: loginView{Variant: "confirm", Token: tok, Expires: exp}})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		s.Log.Warn("вход с чужого адреса отклонён", "origin", r.Header.Get("Origin"), "host", r.Host)
		http.Error(w, "запрос не со страницы панели", http.StatusForbidden)
		return
	}
	ok, err := s.Store.UseToken(r.Context(), r.PostFormValue("t"), s.Now())
	if err != nil {
		s.Log.Error("вход", "err", err)
		http.Error(w, "ошибка базы панели", http.StatusInternalServerError)
		return
	}
	if !ok {
		s.Log.Warn("вход по негодному пропуску", "remote", r.RemoteAddr)
		s.render(w, r, "login", http.StatusForbidden, page{Title: "Пропуск протух", Data: loginView{Variant: "expired"}})
		return
	}
	sid, err := s.Store.NewSession(r.Context(), s.Now(), s.SessionTTL)
	if err != nil {
		s.Log.Error("вход", "err", err)
		http.Error(w, "ошибка базы панели", http.StatusInternalServerError)
		return
	}
	// Lax, а не Strict: переход по ссылке из Telegram — переход с чужого сайта, и со Strict cookie
	// не ушла бы до первого клика внутри панели. От подделки запросов защищает проверка Origin.
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: sid, Path: "/", MaxAge: int(s.SessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	s.act(r.Context(), "login", "panel", r.RemoteAddr, "вход в панель с "+hostOnly(r.RemoteAddr))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	_ = s.Store.DeleteSession(r.Context(), session(r).id)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func hostOnly(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i > 0 {
		return strings.Trim(addr[:i], "[]")
	}
	return addr
}
