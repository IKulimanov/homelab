// Package alert решает, когда слать алерт: при ухудшении сразу, при той же беде — не чаще cooldown,
// при возврате в норму — один раз «восстановилось».
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
	level Level
	sent  time.Time
	text  string
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

// Update сообщает новое состояние проверки key. Возвращает текст для отправки или "".
func (e *Engine) Update(key string, level Level, text string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.Now()
	st, active := e.state[key]

	if level == OK {
		if !active {
			return ""
		}
		delete(e.state, key)
		return "Восстановилось: " + text
	}
	if !active {
		e.state[key] = &state{level: level, sent: now, text: text}
		return level.String() + ": " + text
	}
	st.text = text
	switch {
	case level > st.level:
		st.level, st.sent = level, now
		return level.String() + ": " + text
	case level < st.level:
		// Стало легче, но не норма: молча понижаем, чтобы следующее ухудшение пришло сразу.
		st.level = level
		return ""
	case now.Sub(st.sent) >= e.Cooldown:
		st.sent = now
		return level.String() + ", всё ещё: " + text
	}
	return ""
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
