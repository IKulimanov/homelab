package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/iotest"
	"time"
)

func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestDemuxJoinsStdoutAndStderr(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(1, "строка 1\n"))
	in.Write(frame(2, "ошибка\n"))
	in.Write(frame(1, "строка 2\n"))
	got, err := Demux(&in)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "строка 1\nошибка\nстрока 2\n" {
		t.Fatalf("got %q", got)
	}
}

func TestEventsAndNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			io.WriteString(w, `{"Type":"container","Action":"die","Actor":{"ID":"abc","Attributes":{"name":"budget-bot","exitCode":"2","com.docker.compose.project":"apps"}},"timeNano":1}`+"\n")
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"No such container: x"}`)
		}
	}))
	defer srv.Close()
	c := NewWithHTTP(srv.Client(), srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, errc := c.Events(ctx)
	ev, ok := <-events
	if !ok || ev.Action != "die" || ev.Name() != "budget-bot" || ev.ExitCode() != 2 || ev.Project() != "apps" {
		t.Fatalf("событие %+v", ev)
	}
	if _, ok := <-events; ok {
		t.Fatal("поток не закрылся после конца ответа")
	}
	if err := <-errc; err == nil {
		t.Fatal("нет причины закрытия")
	}

	if _, err := c.Inspect(ctx, "x"); !IsNotFound(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestDemuxReaderHandlesFramesSplitAcrossReads(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(1, "первая\n"))
	in.Write(frame(2, "ошибка\n"))
	in.Write(frame(1, "вторая\n"))
	// По одному байту: заголовок и тело кадра приходят кусками, как из сети.
	got, err := io.ReadAll(NewDemuxReader(iotest.OneByteReader(&in)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "первая\nошибка\nвторая\n" {
		t.Fatalf("got %q", got)
	}
}

func TestDemuxReaderTruncatedFrame(t *testing.T) {
	data := frame(1, "длинная строка\n")
	_, err := io.ReadAll(NewDemuxReader(bytes.NewReader(data[:12])))
	if err == nil {
		t.Fatal("обрыв посреди кадра не замечен")
	}
}

func TestStatsCPUAndMemory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/abc/stats" || r.URL.Query().Get("stream") != "false" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.WriteString(w, `{
			"cpu_stats":{"cpu_usage":{"total_usage":3000},"system_cpu_usage":20000,"online_cpus":4},
			"precpu_stats":{"cpu_usage":{"total_usage":1000},"system_cpu_usage":10000},
			"memory_stats":{"usage":50000000,"stats":{"inactive_file":10000000}}}`)
	}))
	defer srv.Close()
	st, err := NewWithHTTP(srv.Client(), srv.URL).Stats(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	// (3000-1000)/(20000-10000) × 4 ядра × 100 = 80 %; память без файлового кэша.
	if st.CPUPercent != 80 || st.MemBytes != 40000000 {
		t.Fatalf("stats %+v", st)
	}
}
