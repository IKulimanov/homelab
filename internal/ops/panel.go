package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// PanelClient просит у панели одноразовую ссылку входа: внутренний порт панели виден только в сети homelab.
type PanelClient struct {
	Base  string // http://panel:8081
	Token string
	HTTP  *http.Client
}

func (p *PanelClient) LoginLink(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.Base, "/")+"/internal/login-link", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("панель: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("панель ответила %d", resp.StatusCode)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || out.URL == "" {
		return "", fmt.Errorf("панель: непонятный ответ")
	}
	return out.URL, nil
}
