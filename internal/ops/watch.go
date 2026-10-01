package ops

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"homelab/internal/alert"
	"homelab/internal/docker"
	"homelab/internal/host"
)

const (
	// killWindow — сколько после kill (docker stop, restart, пересоздание при обновлении) die считается штатным.
	killWindow = 2 * time.Minute
	// stableAfter — сколько контейнер должен проработать, чтобы падение считалось закрытым.
	// Без этого перезапуск по кругу давал бы пару «упал — восстановился» на каждом круге.
	stableAfter = 2 * time.Minute
	retention   = 30 * 24 * time.Hour
)

// Run — наблюдение до отмены ctx: поток событий Docker и проверка раз в минуту.
func (s *Service) Run(ctx context.Context, events func(context.Context) (<-chan docker.Event, <-chan error)) {
	var wg sync.WaitGroup
	wg.Go(func() { s.watchEvents(ctx, events) })

	t := time.NewTicker(time.Minute)
	defer t.Stop()
	s.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// watchEvents переподключается к потоку событий: он рвётся при перезапуске Docker.
func (s *Service) watchEvents(ctx context.Context, events func(context.Context) (<-chan docker.Event, <-chan error)) {
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		start := s.now()
		ch, errc := events(ctx)
		for ev := range ch {
			s.HandleEvent(ctx, ev)
		}
		err := <-errc
		if ctx.Err() != nil {
			return
		}
		if s.now().Sub(start) > time.Minute {
			backoff = 5 * time.Second
		}
		s.log.Warn("поток событий Docker прервался, переподключаюсь", "err", err, "через", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// HandleEvent превращает событие контейнера в алерт. Остановка по команде или при обновлении — не авария.
func (s *Service) HandleEvent(ctx context.Context, ev docker.Event) {
	if ev.Type != "container" || !s.managed(ev.Project()) {
		return
	}
	name := ev.Name()
	key := "container:" + name
	switch {
	case ev.Action == "kill":
		s.markKill(name)
	case ev.Action == "die":
		code := ev.ExitCode()
		if code == 0 || s.recentKill(name) {
			return
		}
		s.fall(ctx, name, fmt.Sprintf("код %d", code))
		s.alert(ctx, key, alert.Crit, fmt.Sprintf("%s упал, код выхода %d. Лог: /logs %s", name, code, name))
	case ev.Action == "oom":
		s.fall(ctx, name, "oom")
		s.alert(ctx, key, alert.Crit, fmt.Sprintf("%s: не хватило памяти, процесс убит (OOM). Лог: /logs %s", name, name))
	case strings.HasPrefix(ev.Action, "health_status"):
		switch strings.TrimSpace(strings.TrimPrefix(ev.Action, "health_status:")) {
		case "unhealthy":
			s.alert(ctx, "health:"+name, alert.Crit, name+": healthcheck не проходит. Лог: /logs "+name)
		case "healthy":
			s.alert(ctx, "health:"+name, alert.OK, name+": healthcheck снова проходит")
		}
	}
}

func (s *Service) recentKill(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.kills[name]
	return ok && s.now().Sub(at) < killWindow
}

func (s *Service) fall(ctx context.Context, name, reason string) {
	if err := s.store.AddFall(ctx, s.now(), name, reason); err != nil {
		s.log.Error("падение не записано", "err", err)
	}
}

// Tick — проверка раз в минуту.
func (s *Service) Tick(ctx context.Context) {
	snap := s.metrics.Collect()
	s.mu.Lock()
	s.last = snap
	s.ticks++
	ticks := s.ticks
	s.mu.Unlock()

	s.checkHost(ctx, snap)
	s.saveSamples(ctx, snap)
	dockerOK := s.checkContainers(ctx)
	if s.llm != nil {
		s.checkLLM(ctx)
	}
	s.maybeReport(ctx)
	// Пульс только когда Docker отвечает: внешний сервис напишет, если пропал сервер или Docker.
	if s.cfg.HeartbeatURL != "" && dockerOK && ticks%5 == 1 {
		s.heartbeat(ctx)
	}
	if ticks%60 == 1 {
		if err := s.store.Prune(ctx, s.now().Add(-retention)); err != nil {
			s.log.Error("чистка замеров", "err", err)
		}
	}
}

func (s *Service) checkHost(ctx context.Context, snap host.Snapshot) {
	th := s.cfg.Thresholds
	high := func(key, label string, v float64, r Range, unit string) {
		if !host.Known(v) {
			return
		}
		lvl := alert.High(v, r.Warn, r.Crit)
		s.alert(ctx, key, lvl, fmt.Sprintf("%s %.0f%s (пороги %.0f и %.0f)", label, v, unit, r.Warn, r.Crit))
	}
	high("cpu-temp", "температура CPU", snap.CPUTemp, th.CPUTemp, " °C")
	high("ssd-temp", "температура SSD", snap.SSDTemp, th.SSDTemp, " °C")
	high("mem", "занято памяти", snap.MemPct, th.Mem, " %")
	high("swap", "занято swap", snap.SwapPct, th.Swap, " %")
	for _, d := range snap.Disks {
		high("disk:"+d.Path, "диск "+d.Path+" занят на", d.UsedPct, th.Disk, " %")
	}
	if host.Known(snap.Load5) && snap.CPUs > 0 {
		cpus := float64(snap.CPUs)
		lvl := alert.High(snap.Load5/cpus, th.Load.Warn, th.Load.Crit)
		s.alert(ctx, "load", lvl, fmt.Sprintf("load average %.2f при %d ядрах (пороги %.0f и %.0f)",
			snap.Load5, snap.CPUs, th.Load.Warn*cpus, th.Load.Crit*cpus))
	}
	if b := snap.Battery; b != nil {
		if b.Discharging {
			// Работа от батареи — уже беда: отключилось питание.
			lvl := max(alert.Warn, alert.Low(b.Percent, th.Battery.Warn, th.Battery.Crit))
			s.alert(ctx, "power", lvl, fmt.Sprintf("сервер работает от батареи, заряд %.0f %%", b.Percent))
		} else {
			s.alert(ctx, "power", alert.OK, fmt.Sprintf("питание вернулось, заряд %.0f %%", b.Percent))
		}
	}
}

func (s *Service) saveSamples(ctx context.Context, snap host.Snapshot) {
	v := map[string]float64{}
	add := func(m string, x float64) {
		if host.Known(x) {
			v[m] = x
		}
	}
	add(mCPUTemp, snap.CPUTemp)
	add(mSSDTemp, snap.SSDTemp)
	add(mCPUBusy, snap.CPUBusyPct)
	add(mMem, snap.MemPct)
	add(mSwap, snap.SwapPct)
	add(mLoad, snap.Load5)
	for _, d := range snap.Disks {
		v[mDisk+d.Path] = d.UsedPct
	}
	if err := s.store.AddSamples(ctx, s.now(), v); err != nil {
		s.log.Error("замеры не записаны", "err", err)
	}
}

// checkContainers ловит то, что событие не покажет: падение, пока бот не работал, перезапуск по кругу,
// и закрывает падение, когда контейнер проработал stableAfter. Возвращает, ответил ли Docker.
func (s *Service) checkContainers(ctx context.Context) bool {
	list, err := s.docker.List(ctx)
	if err != nil {
		s.alert(ctx, "docker", alert.Crit, "Docker не отвечает: "+err.Error())
		return false
	}
	s.alert(ctx, "docker", alert.OK, "Docker снова отвечает")

	present := map[string]bool{}
	for _, c := range list {
		if !s.managed(c.Project()) {
			continue
		}
		name := c.Name()
		present[name] = true
		key := "container:" + name
		insp, err := s.docker.Inspect(ctx, c.ID)
		if err != nil {
			continue
		}
		switch {
		case insp.State.Restarting:
			s.alert(ctx, key, alert.Crit, fmt.Sprintf("%s перезапускается по кругу, перезапусков %d. Лог: /logs %s", name, insp.RestartCount, name))
		case insp.State.Running:
			if s.alerts.Is(key) && s.now().Sub(insp.State.StartedAt) >= stableAfter {
				s.alert(ctx, key, alert.OK, name+" снова работает")
			}
			if insp.Health() == "unhealthy" {
				s.alert(ctx, "health:"+name, alert.Crit, name+": healthcheck не проходит. Лог: /logs "+name)
			} else if insp.Health() == "healthy" {
				s.alert(ctx, "health:"+name, alert.OK, name+": healthcheck снова проходит")
			}
		default:
			// 137 и 143 — остановка сигналом (docker stop); такую остановку делают руками или обновление.
			code := insp.State.ExitCode
			if code != 0 && code != 137 && code != 143 {
				s.alert(ctx, key, alert.Crit, fmt.Sprintf("%s остановлен с кодом %d. Лог: /logs %s", name, code, name))
			} else if insp.State.OOMKilled {
				s.alert(ctx, key, alert.Crit, fmt.Sprintf("%s убит из-за нехватки памяти (OOM)", name))
			}
		}
	}
	// Контейнер удалили (например, при переезде) — его алерты закрываются молча.
	for _, a := range s.alerts.Active() {
		for _, prefix := range []string{"container:", "health:"} {
			if name, ok := strings.CutPrefix(a.Key, prefix); ok && !present[name] {
				s.alerts.Clear(a.Key)
			}
		}
	}
	return true
}

func (s *Service) checkLLM(ctx context.Context) {
	st, err := s.llm.Status(ctx)
	if err != nil {
		s.alert(ctx, "llm:gateway", alert.Warn, "шлюз LLM не отвечает: "+err.Error())
		return
	}
	s.alert(ctx, "llm:gateway", alert.OK, "шлюз LLM снова отвечает")

	for _, c := range st.Clients {
		p := percent(c.SpentUSD, c.LimitUSD)
		text := fmt.Sprintf("LLM %s: потрачено $%.2f из $%.2f за месяц (%.0f %%)", c.Name, c.SpentUSD, c.LimitUSD, p)
		if p >= 100 {
			text += ", запросы отклоняются до 1-го числа. Лимит — в stacks/platform/config/llm-gateway.yaml"
		}
		s.alert(ctx, "llm:"+c.Name, alert.High(p, 80, 100), text)
	}
	p := percent(st.TotalSpentUSD, st.TotalLimitUSD)
	s.alert(ctx, "llm:total", alert.High(p, 80, 100),
		fmt.Sprintf("LLM всего: $%.2f из $%.2f за месяц (%.0f %%)", st.TotalSpentUSD, st.TotalLimitUSD, p))

	if st.Balance.Known {
		lvl := alert.OK
		switch {
		case st.Balance.USD <= 0:
			lvl = alert.Crit
		case st.Balance.USD < st.BalanceAlertUSD:
			lvl = alert.Warn
		}
		s.alert(ctx, "llm:balance", lvl, fmt.Sprintf("баланс Gemini ~$%.2f (порог $%.2f). Пополнить в AI Studio, затем /topup <сумма>",
			st.Balance.USD, st.BalanceAlertUSD))
	}
	if len(st.UnpricedModels) > 0 {
		s.alert(ctx, "llm:prices", alert.Warn, "нет цены для моделей "+strings.Join(st.UnpricedModels, ", ")+": расход по ним не считается")
	} else {
		s.alert(ctx, "llm:prices", alert.OK, "цены есть для всех моделей")
	}
}

// maybeReport шлёт отчёт за день один раз после ReportAt; день отчёта хранится в базе,
// чтобы перезапуск в 23:57 не прислал отчёт второй раз.
func (s *Service) maybeReport(ctx context.Context) {
	if s.cfg.ReportAt == "" {
		return
	}
	now := s.now().In(s.cfg.Location)
	if now.Format("15:04") < s.cfg.ReportAt {
		return
	}
	today := now.Format(time.DateOnly)
	last, err := s.store.Meta(ctx, "report_day")
	if err != nil || last == today {
		return
	}
	if err := s.send(ctx, s.dailyReport(ctx, now)); err != nil {
		s.log.Error("отчёт не отправлен", "err", err)
		return
	}
	if err := s.store.SetMeta(ctx, "report_day", today); err != nil {
		s.log.Error("день отчёта не записан", "err", err)
	}
}

func (s *Service) heartbeat(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.HeartbeatURL, nil)
	if err != nil {
		s.log.Error("пульс", "err", err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.log.Warn("пульс не отправлен", "err", err)
		return
	}
	resp.Body.Close()
}
