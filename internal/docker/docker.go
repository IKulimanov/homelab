// Package docker — небольшой клиент Docker Engine API через unix-сокет. Нужны только список,
// inspect, логи, start/stop/restart и поток событий; SDK Docker ради этого тянет сотни пакетов.
package docker

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Метки, которые ставит compose.
const (
	LabelProject = "com.docker.compose.project"
	LabelService = "com.docker.compose.service"
)

type Client struct {
	http *http.Client
	base string
}

// New — клиент к сокету. Таймаута на весь запрос нет: stop ждёт stop_grace_period, а events — бесконечный поток.
func New(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{http: &http.Client{Transport: tr}, base: "http://docker"}
}

// NewWithHTTP — клиент к произвольному адресу; для тестов через httptest.
func NewWithHTTP(c *http.Client, base string) *Client {
	return &Client{http: c, base: strings.TrimRight(base, "/")}
}

type Container struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	State   string            `json:"State"`  // running, exited, restarting, paused, created, dead
	Status  string            `json:"Status"` // «Up 3 hours (healthy)»
	Labels  map[string]string `json:"Labels"`
}

// Name — имя без ведущего «/».
func (c Container) Name() string {
	if len(c.Names) == 0 {
		return c.ID[:min(12, len(c.ID))]
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

func (c Container) Project() string { return c.Labels[LabelProject] }

type Inspect struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	Image string `json:"Image"`
	State struct {
		Status     string    `json:"Status"`
		Running    bool      `json:"Running"`
		Restarting bool      `json:"Restarting"`
		OOMKilled  bool      `json:"OOMKilled"`
		ExitCode   int       `json:"ExitCode"`
		StartedAt  time.Time `json:"StartedAt"`
		FinishedAt time.Time `json:"FinishedAt"`
		Health     *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	RestartCount int `json:"RestartCount"`
	Config       struct {
		Image  string            `json:"Image"`
		Tty    bool              `json:"Tty"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

// Health — статус healthcheck или "", если его нет.
func (i Inspect) Health() string {
	if i.State.Health == nil {
		return ""
	}
	return i.State.Health.Status
}

type Image struct {
	ID     string `json:"Id"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

// Event — событие контейнера из /events.
type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"` // die, oom, kill, start, health_status: unhealthy …
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	TimeNano int64 `json:"timeNano"`
}

func (e Event) Name() string    { return e.Actor.Attributes["name"] }
func (e Event) Project() string { return e.Actor.Attributes[LabelProject] }
func (e Event) Time() time.Time { return time.Unix(0, e.TimeNano) }

// ExitCode — код выхода из события die; -1, если его нет.
func (e Event) ExitCode() int {
	n, err := strconv.Atoi(e.Actor.Attributes["exitCode"])
	if err != nil {
		return -1
	}
	return n
}

// APIError — ответ Docker с кодом ошибки.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("docker %d: %s", e.Status, e.Message) }

// IsNotFound — контейнера или образа нет.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values) (*http.Response, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		defer resp.Body.Close()
		var m struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&m)
		return nil, &APIError{Status: resp.StatusCode, Message: m.Message}
	}
	return resp, nil
}

func (c *Client) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	resp, err := c.do(ctx, http.MethodGet, path, q)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("docker %s: %w", path, err)
	}
	return nil
}

// List — все контейнеры compose-проектов, в том числе остановленные.
func (c *Client) List(ctx context.Context) ([]Container, error) {
	var out []Container
	q := url.Values{"all": {"1"}, "filters": {`{"label":["` + LabelProject + `"]}`}}
	return out, c.getJSON(ctx, "/containers/json", q, &out)
}

func (c *Client) Inspect(ctx context.Context, id string) (Inspect, error) {
	var out Inspect
	return out, c.getJSON(ctx, "/containers/"+url.PathEscape(id)+"/json", nil, &out)
}

func (c *Client) Image(ctx context.Context, id string) (Image, error) {
	var out Image
	return out, c.getJSON(ctx, "/images/"+url.PathEscape(id)+"/json", nil, &out)
}

func (c *Client) post(ctx context.Context, id, action string) error {
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/"+action, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Restart, Start, Stop — без параметра t: Docker ждёт stop_grace_period из compose.
func (c *Client) Restart(ctx context.Context, id string) error { return c.post(ctx, id, "restart") }
func (c *Client) Start(ctx context.Context, id string) error   { return c.post(ctx, id, "start") }
func (c *Client) Stop(ctx context.Context, id string) error    { return c.post(ctx, id, "stop") }

// Logs — последние tail строк stdout и stderr вперемешку, как docker logs.
func (c *Client) Logs(ctx context.Context, id string, tail int, tty bool) ([]byte, error) {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {strconv.Itoa(tail)}}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs", q)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, 16<<20)
	if tty {
		return io.ReadAll(body)
	}
	return Demux(body)
}

// Demux склеивает мультиплексированный поток логов: у контейнера без TTY каждый кусок
// идёт с заголовком из 8 байт — номер потока, три нуля и длина.
func Demux(r io.Reader) ([]byte, error) {
	var out []byte
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, fmt.Errorf("логи: %w", err)
		}
		n := binary.BigEndian.Uint32(hdr[4:])
		chunk := make([]byte, n)
		if _, err := io.ReadFull(r, chunk); err != nil {
			return out, fmt.Errorf("логи: %w", err)
		}
		out = append(out, chunk...)
	}
}

// Events — поток событий контейнеров до отмены ctx или обрыва соединения. Канал закрывается в конце,
// причина — в errc.
func (c *Client) Events(ctx context.Context) (<-chan Event, <-chan error) {
	events := make(chan Event)
	errc := make(chan error, 1)
	go func() {
		defer close(events)
		q := url.Values{"filters": {`{"type":["container"]}`}}
		resp, err := c.do(ctx, http.MethodGet, "/events", q)
		if err != nil {
			errc <- err
			return
		}
		defer resp.Body.Close()
		dec := json.NewDecoder(bufio.NewReader(resp.Body))
		for {
			var ev Event
			if err := dec.Decode(&ev); err != nil {
				errc <- fmt.Errorf("поток событий: %w", err)
				return
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				errc <- ctx.Err()
				return
			}
		}
	}()
	return events, errc
}
