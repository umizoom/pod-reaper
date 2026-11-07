package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Notifier delivers a human-readable message somewhere outside the cluster.
type Notifier interface {
	Notify(ctx context.Context, msg string) error
}

// maxMessageLen is Discord's content limit; Slack allows more.
const maxMessageLen = 2000

// webhookNotifier posts JSON to a Discord or Slack incoming webhook.
// The URL embeds a credential, so it is never included in errors or logs.
type webhookNotifier struct {
	url    string
	field  string // "content" (Discord) or "text" (Slack)
	client *http.Client
}

func newWebhookNotifier(rawURL string) (*webhookNotifier, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("invalid WEBHOOK_URL: must be an http(s) URL")
	}
	field := "content"
	if host := u.Hostname(); host == "hooks.slack.com" || strings.HasSuffix(host, ".slack.com") {
		field = "text"
	}
	return &webhookNotifier{url: rawURL, field: field, client: &http.Client{Timeout: 10 * time.Second}}, nil
}

func (w *webhookNotifier) Notify(ctx context.Context, msg string) error {
	body, err := json.Marshal(map[string]string{w.field: truncate(msg, maxMessageLen)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return errors.New("webhook request could not be built")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL, which contains the webhook token
		}
		return fmt.Errorf("webhook request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook returned %s", resp.Status)
	}
	return nil
}

// truncate shortens s to at most max runes, marking the cut with "...".
func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-3]) + "..."
}
