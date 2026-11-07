package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestWebhookNotifier(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n, err := newWebhookNotifier(srv.URL + "/api/webhooks/1/token")
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Notify(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if got["content"] != "hello" || len(got) != 1 {
		t.Fatalf("payload = %v", got)
	}

	long := strings.Repeat("é", maxMessageLen+50)
	if err := n.Notify(context.Background(), long); err != nil {
		t.Fatal(err)
	}
	if c := got["content"]; utf8.RuneCountInString(c) != maxMessageLen || !strings.HasSuffix(c, "...") {
		t.Fatalf("message not truncated to %d runes: %d", maxMessageLen, utf8.RuneCountInString(c))
	}
}

func TestWebhookNotifierErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	n, _ := newWebhookNotifier(srv.URL)
	if err := n.Notify(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected 429 error, got %v", err)
	}

	// Transport errors must not leak the URL (it contains the webhook token).
	n, _ = newWebhookNotifier("http://127.0.0.1:1/api/webhooks/123/super-secret-token")
	err := n.Notify(context.Background(), "x")
	if err == nil || strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("expected redacted error, got %v", err)
	}
}

func TestWebhookFieldDetection(t *testing.T) {
	for url, field := range map[string]string{
		"https://discord.com/api/webhooks/1/abc": "content",
		"https://hooks.slack.com/services/T/B/x": "text",
		"https://example.com/hook":               "content",
	} {
		n, err := newWebhookNotifier(url)
		if err != nil || n.field != field {
			t.Errorf("%s: field = %v (err %v), want %s", url, n, err, field)
		}
	}
	for _, bad := range []string{"not a url", "ftp://x/y", "https://"} {
		if _, err := newWebhookNotifier(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}
