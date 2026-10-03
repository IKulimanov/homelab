package panel

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"homelab/internal/docker"
)

var tailChoices = []int{100, 500, 2000}

func tailOf(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	if !slices.Contains(tailChoices, n) {
		return tailChoices[0]
	}
	return n
}

type logsView struct {
	Names   []string
	Current string
	Tail    int
	Tails   []int
	Version string
	Uptime  string
	Dozzle  string
}

func (s *Server) logsPage(w http.ResponseWriter, r *http.Request) {
	list, err := s.managed(r.Context())
	if err != nil {
		s.render(w, r, "logs", http.StatusBadGateway, page{Title: "Бортовой журнал", Active: "logs", Toast: "Docker не ответил: " + err.Error()})
		return
	}
	v := logsView{Current: r.URL.Query().Get("svc"), Tail: tailOf(r), Tails: tailChoices, Dozzle: s.siteURL(r, 8889, "")}
	for _, c := range list {
		v.Names = append(v.Names, c.Name())
	}
	slices.Sort(v.Names)
	if !slices.Contains(v.Names, v.Current) && len(v.Names) > 0 {
		v.Current = v.Names[0]
	}
	for _, c := range list {
		if c.Name() == v.Current {
			v.Version, _ = s.version(r.Context(), c, map[string]docker.Image{})
			v.Uptime = uptimeOf(c.Status)
		}
	}
	s.render(w, r, "logs", http.StatusOK, page{Title: "Бортовой журнал", Active: "logs", Data: v})
}

// logsStream — поток строк лога в SSE. Обрывается, когда вкладку закрыли: ctx запроса отменяется,
// Docker закрывает поток, сканер выходит.
func (s *Server) logsStream(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("svc")
	c, err := s.find(r.Context(), name)
	if err != nil {
		http.Error(w, "нет такого сервиса", http.StatusNotFound)
		return
	}
	in, err := s.Docker.Inspect(r.Context(), c.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	body, err := s.Docker.LogStream(r.Context(), c.ID, tailOf(r), true, in.Config.Tty)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer body.Close()
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-r.Context().Done():
				return
			}
		}
	}()
	// Пинг раз в 20 секунд: без него прокси и браузер закрывают молчащее соединение.
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				fmt.Fprint(w, "event: end\ndata: поток закрыт\n\n")
				if flusher != nil {
					flusher.Flush()
				}
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(l, "\r", ""))
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		case <-r.Context().Done():
			return
		}
		if flusher != nil && len(lines) == 0 {
			flusher.Flush()
		}
	}
}

func (s *Server) logsDownload(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("svc")
	c, err := s.find(r.Context(), name)
	if err != nil {
		http.Error(w, "нет такого сервиса", http.StatusNotFound)
		return
	}
	in, err := s.Docker.Inspect(r.Context(), c.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	body, err := s.Docker.LogStream(r.Context(), c.ID, tailOf(r), false, in.Config.Tty)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.log"`, name, s.Now().Format("20060102-1504")))
	_, _ = io.Copy(w, io.LimitReader(body, 32<<20))
}

// siteURL — адрес соседнего сервиса на том же хосте, с которого открыта панель.
func (s *Server) siteURL(r *http.Request, port int, path string) string {
	host := r.Host
	if i := strings.LastIndexByte(host, ':'); i > 0 && !strings.HasSuffix(host, "]") {
		host = host[:i]
	}
	return fmt.Sprintf("http://%s:%d%s", host, port, path)
}
