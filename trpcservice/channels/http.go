package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func postJSON(ctx context.Context, client HTTPDoer, url string, headers map[string]string, payload any, result any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode delivery payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		// The Telegram API embeds the bot token in the URL. Never propagate a
		// URL-bearing parser error into logs, traces or audit records.
		return errors.New("create delivery request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("delivery network request failed")
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, 1<<20)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, limited)
		return fmt.Errorf("delivery returned HTTP %d", resp.StatusCode)
	}
	if result != nil {
		if err := json.NewDecoder(limited).Decode(result); err != nil {
			return fmt.Errorf("decode delivery response: %w", err)
		}
	}
	return nil
}

func chunks(text string, limit int) []string {
	if limit <= 0 {
		limit = 4000
	}
	runes := []rune(text)
	if len(runes) == 0 {
		return []string{""}
	}
	result := make([]string, 0, (len(runes)+limit-1)/limit)
	for len(runes) > 0 {
		n := limit
		if len(runes) < n {
			n = len(runes)
		}
		result = append(result, string(runes[:n]))
		runes = runes[n:]
	}
	return result
}
