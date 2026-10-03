// Package alert решает, когда слать алерт: при ухудшении сразу, критичную беду — повторно не чаще cooldown,
// при возврате в норму — один раз «восстановилось». «Внимание» не повторяется: это шум, а не авария.
package alert

import (
	"sort"
	"sync"
	"time"
)

type Level int

const (
	OK Level = iota
	Warn
	Crit
)

func (l Level) String() string {
	switch l {
	case Warn:
		return "Внимание"
	case Crit:
		return "Критично"
	default:
		return "Норма"
	}
}

// High — уровень для метрики, где больше значит хуже (температура, память).
func High(v, warn, crit float64) Level {
	switch {
	case v >= crit:
		return Crit
	case v >= warn:
		return Warn
	}
	return OK
}

// Low — уровень для метрики, где меньше значит хуже (заряд батареи, баланс).
func Low(v, warn, crit float64) Level {
	switch {
	case v <= crit:
		return Crit
	case v <= warn:
		return Warn
	}
	return OK
}

type state struct {
	level   Level
	sent    time.Time
	text    string
	repeats int
}

// Kind — почему сообщение ушло.
type Kind int

const (
	None      Kind = iota
	New            // беда появилась
	Worse          // стало хуже
	Repeat         // беда всё ещё есть, прошёл cooldown
	Recovered      // вернулось в норму
)

// Msg — решение движка. Пустой Text — слать нечего.
type Msg struct {
	Text  string
	Kind  Kind
	Level Level
	// Repeat — номер повтора, начиная с 1; для остальных видов 0.
	Repeat int
}

type Engine struct {
	Cooldown time.Duration
	Now      func() time.Time

	mu    sync.Mutex
	state map[string]*state
}

func NewEngine(cooldown time.Duration) *Engine {
	return &Engine{Cooldown: cooldown, Now: time.Now, state: map[string]*state{}}
}

// Update сообщает новое состояние проверки key и возвращает, что отправить.
func (e *Engine) Update(key string, level Level, text string) Msg {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.Now()
	st, active := e.state[key]

	if level == OK {
		if !active {
			return Msg{}
		}
		delete(e.state, key)
		return Msg{Text: "Восстановилось: " + text, Kind: Recovered, Level: OK}
	}
	if !active {
		e.state[key] = &state{level: level, sent: now, text: text}
		return Msg{Text: level.String() + ": " + text, Kind: New, Level: level}
	}
	st.text = text
	switch {
	case level > st.level:
		st.level, st.sent = level, now
		return Msg{Text: level.String() + ": " + text, Kind: Worse, Level: level}
	case level < st.level:
		// Стало легче, но не норма: молча понижаем, чтобы следующее ухудшение пришло сразу.
		st.level = level
		return Msg{}
	case level == Crit && now.Sub(st.sent) >= e.Cooldown:
		st.sent = now
		st.repeats++
		return Msg{Text: level.String() + ", всё ещё: " + text, Kind: Repeat, Level: level, Repeat: st.repeats}
	}
	return Msg{}
}

// Is — активна ли проверка key: например, падение контейнера ещё не закрыто.
func (e *Engine) Is(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.state[key]
	return ok
}

// Clear закрывает проверку без сообщения: например, контейнер удалили.
func (e *Engine) Clear(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.state, key)
}

// Active — текущая беда для /status.
type Active struct {
	Key   string
	Level Level
	Text  string
}

// Active — все текущие беды, сначала критичные.
func (e *Engine) Active() []Active {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Active, 0, len(e.state))
	for k, s := range e.state {
		out = append(out, Active{Key: k, Level: s.level, Text: s.text})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Level != out[j].Level {
			return out[i].Level > out[j].Level
		}
		return out[i].Key < out[j].Key
	})
	return out
}
