package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// GeminiCommenter — комментарий к отчёту через Gemini API (на сервере — через llm-gateway, со своим ключом).
type GeminiCommenter struct {
	Base  string // http://llm-gateway:8080 или https://generativelanguage.googleapis.com
	Key   string
	Model string
	HTTP  *http.Client
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

func (g *GeminiCommenter) Comment(ctx context.Context, prompt, report string) (string, error) {
	raw, err := json.Marshal(map[string]any{
		"systemInstruction": geminiContent{Parts: []geminiPart{{Text: prompt}}},
		"contents":          []geminiContent{{Role: "user", Parts: []geminiPart{{Text: report}}}},
	})
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(g.Base, "/") + "/v1beta/models/" + g.Model + ":generateContent"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", g.Key)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("gemini: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gemini ответил %d", resp.StatusCode)
	}
	var out struct {
		Candidates []struct {
			Content geminiContent `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("gemini: %w", err)
	}
	if len(out.Candidates) == 0 {
		return "", nil
	}
	var b strings.Builder
	for _, p := range out.Candidates[0].Content.Parts {
		b.WriteString(p.Text)
	}
	return b.String(), nil
}
