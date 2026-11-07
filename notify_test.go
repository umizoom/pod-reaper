package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var (
	alertTime  = time.Date(2026, 11, 7, 22, 16, 44, 0, time.UTC)
	crashTime  = time.Date(2026, 11, 7, 22, 15, 32, 0, time.UTC)
	exitCode3  = int32(3)
	est, _     = time.LoadLocation("America/New_York")
	sampleFind = finding{Container: "busybox", Reason: "CrashLoopBackOff", Restarts: 5, LastExitCode: &exitCode3, LastTermReason: "Error", LastFinishedAt: crashTime}
)

func reapedAlert(count int) Alert {
	return Alert{Kind: AlertReaped, Time: alertTime, Namespace: "reaper-test", Pod: "crasher-7bc4f787dd-hgghd",
		Workload: "Deployment/crasher", Finding: sampleFind, Count24h: count}
}

// capture records JSON bodies posted to a test server.
func capture(t *testing.T, status int) (*httptest.Server, *[]map[string]any) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func TestDiscordEmbed(t *testing.T) {
	e := reapedAlert(1).discordEmbed()
	if e.Title != "Reaped Deployment/crasher" || e.Color != colorReaped || e.Description != "`reaper-test/crasher-7bc4f787dd-hgghd`" {
		t.Fatalf("embed = %+v", e)
	}
	fields := map[string]string{}
	for _, f := range e.Fields {
		fields[f.Name] = f.Value
	}
	want := map[string]string{
		"Namespace":  "reaper-test",
		"Container":  "busybox",
		"Reason":     "CrashLoopBackOff",
		"Restarts":   "5",
		"Last exit":  "3 (Error)",
		"Last crash": "<t:1794089732:T> (<t:1794089732:R>)",
	}
	for k, v := range want {
		if fields[k] != v {
			t.Errorf("field %q = %q, want %q", k, fields[k], v)
		}
	}
	if _, ok := fields["Repeat offender"]; ok {
		t.Error("single reap should not be flagged as repeat offender")
	}
	if e.Timestamp != "2026-11-07T22:16:44Z" {
		t.Errorf("timestamp = %q", e.Timestamp)
	}

	e = reapedAlert(3).discordEmbed()
	if e.Color != colorRepeat || e.Fields[len(e.Fields)-1].Value != "Reaped **3×** in the last 24h" {
		t.Fatalf("repeat offender embed = %+v", e)
	}

	dry := reapedAlert(1)
	dry.Kind = AlertDryRun
	if e := dry.discordEmbed(); e.Color != colorDryRun || !strings.HasPrefix(e.Title, "[dry-run]") {
		t.Fatalf("dry-run embed = %+v", e)
	}

	lim := Alert{Kind: AlertRateLimited, Time: alertTime, Namespace: "media", Pod: "x", MaxPerHr: 20}
	if e := lim.discordEmbed(); e.Color != colorLimited || !strings.Contains(e.Description, "20 pods") || e.Fields[0].Value != "`media/x`" {
		t.Fatalf("rate-limit embed = %+v", e)
	}
}

func TestSlackText(t *testing.T) {
	got := reapedAlert(2).slackText(est)
	want := "*Reaped Deployment/crasher*\n`reaper-test/crasher-7bc4f787dd-hgghd`\n" +
		"container `busybox` · CrashLoopBackOff · restarts 5 · last exit 3 (Error) at 2026-11-07 17:15:32 EST\n" +
		"_Reaped 2× in the last 24h_"
	if got != want {
		t.Fatalf("slackText =\n%s\nwant\n%s", got, want)
	}
}

func TestWebhookDiscordPayload(t *testing.T) {
	srv, bodies := capture(t, http.StatusNoContent)
	n, err := newWebhookNotifier(srv.URL+"/api/webhooks/1/token", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	alerts := make([]Alert, 12)
	for i := range alerts {
		alerts[i] = reapedAlert(1)
	}
	if err := n.Notify(context.Background(), alerts); err != nil {
		t.Fatal(err)
	}
	if len(*bodies) != 2 {
		t.Fatalf("expected 12 embeds split into 2 posts, got %d", len(*bodies))
	}
	if got := len((*bodies)[0]["embeds"].([]any)); got != maxEmbedsPerPost {
		t.Fatalf("first post has %d embeds", got)
	}
	if got := len((*bodies)[1]["embeds"].([]any)); got != 2 {
		t.Fatalf("second post has %d embeds", got)
	}
	if _, ok := (*bodies)[0]["content"]; ok {
		t.Fatal("discord payload should use embeds only")
	}
}

func TestWebhookSlackPayload(t *testing.T) {
	srv, bodies := capture(t, http.StatusOK)
	n, _ := newWebhookNotifier(srv.URL, time.UTC)
	n.flavour = flavourSlack // test server isn't on slack.com
	if err := n.Notify(context.Background(), []Alert{reapedAlert(1), reapedAlert(1)}); err != nil {
		t.Fatal(err)
	}
	if len(*bodies) != 1 || strings.Count((*bodies)[0]["text"].(string), "*Reaped Deployment/crasher*") != 2 {
		t.Fatalf("slack payload = %v", *bodies)
	}
}

func TestWebhookNotifierErrors(t *testing.T) {
	srv, _ := capture(t, http.StatusTooManyRequests)
	n, _ := newWebhookNotifier(srv.URL, time.UTC)
	if err := n.Notify(context.Background(), []Alert{reapedAlert(1)}); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected 429 error, got %v", err)
	}

	// Transport errors must not leak the URL (it contains the webhook token).
	n, _ = newWebhookNotifier("http://127.0.0.1:1/api/webhooks/123/super-secret-token", time.UTC)
	err := n.Notify(context.Background(), []Alert{reapedAlert(1)})
	if err == nil || strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("expected redacted error, got %v", err)
	}
}

func TestWebhookFlavourDetection(t *testing.T) {
	for url, flavour := range map[string]string{
		"https://discord.com/api/webhooks/1/abc": flavourDiscord,
		"https://hooks.slack.com/services/T/B/x": flavourSlack,
		"https://example.com/hook":               flavourDiscord,
	} {
		n, err := newWebhookNotifier(url, nil)
		if err != nil || n.flavour != flavour {
			t.Errorf("%s: flavour = %v (err %v), want %s", url, n, err, flavour)
		}
	}
	for _, bad := range []string{"not a url", "ftp://x/y", "https://"} {
		if _, err := newWebhookNotifier(bad, nil); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate(strings.Repeat("é", 10), 5); got != "éé..." {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("short", 10); got != "short" {
		t.Fatalf("truncate = %q", got)
	}
}
