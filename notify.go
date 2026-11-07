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

// Notifier delivers alerts somewhere outside the cluster.
type Notifier interface {
	Notify(ctx context.Context, alerts []Alert) error
}

const (
	maxSlackText       = 3000 // Slack section text limit
	maxEmbedsPerPost   = 10   // Discord limit per message
	flavourDiscord     = "discord"
	flavourSlack       = "slack"
	webhookHTTPTimeout = 10 * time.Second
)

// webhookNotifier posts to a Discord (embeds) or Slack (mrkdwn text) webhook.
// The URL embeds a credential, so it is never included in errors or logs.
type webhookNotifier struct {
	url     string
	flavour string
	loc     *time.Location
	client  *http.Client
}

func newWebhookNotifier(rawURL string, loc *time.Location) (*webhookNotifier, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("invalid WEBHOOK_URL: must be an http(s) URL")
	}
	flavour := flavourDiscord
	if host := u.Hostname(); host == "hooks.slack.com" || strings.HasSuffix(host, ".slack.com") {
		flavour = flavourSlack
	}
	if loc == nil {
		loc = time.UTC
	}
	return &webhookNotifier{url: rawURL, flavour: flavour, loc: loc, client: &http.Client{Timeout: webhookHTTPTimeout}}, nil
}

func (w *webhookNotifier) Notify(ctx context.Context, alerts []Alert) error {
	for _, payload := range w.payloads(alerts) {
		if err := w.post(ctx, payload); err != nil {
			return err
		}
	}
	return nil
}

func (w *webhookNotifier) payloads(alerts []Alert) []any {
	var out []any
	if w.flavour == flavourSlack {
		texts := make([]string, len(alerts))
		for i, a := range alerts {
			texts[i] = a.slackText(w.loc)
		}
		return append(out, map[string]string{"text": truncate(strings.Join(texts, "\n\n"), maxSlackText)})
	}
	for start := 0; start < len(alerts); start += maxEmbedsPerPost {
		end := min(start+maxEmbedsPerPost, len(alerts))
		embeds := make([]discordEmbed, 0, end-start)
		for _, a := range alerts[start:end] {
			embeds = append(embeds, a.discordEmbed())
		}
		out = append(out, map[string]any{"embeds": embeds})
	}
	return out
}

func (w *webhookNotifier) post(ctx context.Context, payload any) error {
	body, err := json.Marshal(payload)
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
