package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Telegram — функция для Server.Notify: sendMessage токеном служебного бота. Панель не держит своего бота,
// пишет в тот же чат, что ops-bot.
func Telegram(client *http.Client, token string, chatID int64) func(ctx context.Context, text string) error {
	return telegramAt(client, "https://api.telegram.org", token, chatID)
}

func telegramAt(client *http.Client, base, token string, chatID int64) func(ctx context.Context, text string) error {
	return func(ctx context.Context, text string) error {
		body, _ := json.Marshal(map[string]any{
			"chat_id": chatID, "text": text,
			"link_preview_options": map[string]bool{"is_disabled": true},
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+token+"/sendMessage", bytes.NewReader(body))
		if err != nil {
			return errors.New("telegram: неверный запрос")
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			// В url.Error лежит адрес с токеном: в лог уходит только причина.
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return fmt.Errorf("telegram: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return fmt.Errorf("telegram %d: %s", resp.StatusCode, msg)
		}
		return nil
	}
}
