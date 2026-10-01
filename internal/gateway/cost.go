package gateway

import "math"

// Usage — usageMetadata из ответа generateContent.
type Usage struct {
	PromptTokens        int64 `json:"promptTokenCount"`
	CandidatesTokens    int64 `json:"candidatesTokenCount"`
	ThoughtsTokens      int64 `json:"thoughtsTokenCount"`
	CachedTokens        int64 `json:"cachedContentTokenCount"`
	ToolUsePromptTokens int64 `json:"toolUsePromptTokenCount"`
}

// InputTokens — входные токены без кэшированных: они оплачиваются по своей цене.
func (u Usage) InputTokens() int64 {
	return u.PromptTokens - u.cached() + u.ToolUsePromptTokens
}

// OutputTokens — ответ вместе с размышлениями: Google берёт за них как за выход.
func (u Usage) OutputTokens() int64 {
	return u.CandidatesTokens + u.ThoughtsTokens
}

// cached не больше промпта: promptTokenCount уже включает кэшированную часть.
func (u Usage) cached() int64 {
	return min(u.CachedTokens, u.PromptTokens)
}

// Cost — стоимость в микродолларах. Цена за миллион токенов в долларах равна цене за токен
// в микродолларах, поэтому множитель не нужен.
func (p Price) Cost(u Usage) int64 {
	c := float64(u.InputTokens())*p.Input + float64(u.cached())*p.CacheRead + float64(u.OutputTokens())*p.Output
	return int64(math.Round(c))
}

// USD переводит микродоллары в доллары для показа.
func USD(micros int64) float64 {
	return float64(micros) / 1e6
}

// Micros переводит доллары в микродоллары.
func Micros(usd float64) int64 {
	return int64(math.Round(usd * 1e6))
}
