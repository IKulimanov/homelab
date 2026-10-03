package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"homelab/internal/gateway"
)

// GatewayClient — клиент админ-API llm-gateway.
type GatewayClient struct {
	Base  string // http://llm-gateway:8080
	Token string
	HTTP  *http.Client
}

func (g *GatewayClient) Status(ctx context.Context) (gateway.Status, error) {
	var st gateway.Status
	return st, g.call(ctx, http.MethodGet, "/admin/status", nil, &st)
}

func (g *GatewayClient) Usage(ctx context.Context, since time.Time) ([]gateway.UsageRow, error) {
	var rows []gateway.UsageRow
	return rows, g.call(ctx, http.MethodGet, "/admin/usage?since="+url.QueryEscape(since.Format(time.RFC3339)), nil, &rows)
}

// Daily — расход по суткам и клиентам, для графика в панели.
func (g *GatewayClient) Daily(ctx context.Context, since time.Time) ([]gateway.DailyRow, error) {
	var rows []gateway.DailyRow
	return rows, g.call(ctx, http.MethodGet, "/admin/daily?since="+url.QueryEscape(since.Format(time.RFC3339)), nil, &rows)
}

// Ledger — пополнение (kind "topup") или сверка баланса (kind "set").
func (g *GatewayClient) Ledger(ctx context.Context, kind string, usd float64) (gateway.Balance, error) {
	path := "/admin/topup"
	if kind == "set" {
		path = "/admin/balance"
	}
	var b gateway.Balance
	return b, g.call(ctx, http.MethodPost, path, gateway.LedgerRequest{USD: usd, Note: "ops-bot"}, &b)
}

func (g *GatewayClient) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(g.Base, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("шлюз: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("шлюз: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return fmt.Errorf("шлюз ответил %d: %s", resp.StatusCode, e.Error.Message)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("шлюз: %w", err)
	}
	return nil
}
