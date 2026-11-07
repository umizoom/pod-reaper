package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
)

type podOpt func(*v1.Pod)

func newPod(ns, name string, opts ...podOpt) *v1.Pod {
	isController := true
	p := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: name + "-rs", Controller: &isController},
			},
		},
		Status: v1.PodStatus{Phase: v1.PodRunning},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func waitingStatus(name, reason string, restarts int32) v1.ContainerStatus {
	return v1.ContainerStatus{
		Name:         name,
		RestartCount: restarts,
		State:        v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: reason}},
	}
}

// waiting adds an app container in the given waiting state.
func waiting(reason string, restarts int32) podOpt {
	return func(p *v1.Pod) {
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, waitingStatus("app", reason, restarts))
	}
}

func initWaiting(reason string, restarts int32) podOpt {
	return func(p *v1.Pod) {
		p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, waitingStatus("init", reason, restarts))
	}
}

func crashing(p *v1.Pod) { waiting(ReasonCrashLoopBackOff, 5)(p) }

func ownedBy(name string) podOpt {
	return func(p *v1.Pod) { p.OwnerReferences[0].Name = name }
}

func annotate(k, v string) podOpt {
	return func(p *v1.Pod) {
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[k] = v
	}
}

func testConfig() Config {
	return Config{
		IncludeNamespaces: map[string]bool{},
		ExcludeNamespaces: map[string]bool{"kube-system": true},
		Reasons:           map[string]bool{"CrashLoopBackOff": true, "CreateContainerError": true},
		MinRestarts:       3,
		Cooldown:          10 * time.Minute,
		MaxReapsPerHour:   20,
		Location:          time.UTC,
	}
}

func TestShouldRemediate(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name string
		pod  *v1.Pod
		cfg  func(*Config)
		want string // expected reason; "" means not remediated
	}{
		{name: "crashloop over threshold", pod: newPod("default", "a", crashing), want: "CrashLoopBackOff"},
		{name: "crashloop at threshold", pod: newPod("default", "a", waiting("CrashLoopBackOff", 3)), want: "CrashLoopBackOff"},
		{name: "crashloop below threshold", pod: newPod("default", "a", waiting("CrashLoopBackOff", 2))},
		{name: "threshold disabled", pod: newPod("default", "a", waiting("CrashLoopBackOff", 0)), cfg: func(c *Config) { c.MinRestarts = 0 }, want: "CrashLoopBackOff"},
		{name: "create error ignores threshold", pod: newPod("default", "a", waiting("CreateContainerError", 0)), want: "CreateContainerError"},
		{name: "init container crashloop", pod: newPod("default", "a", initWaiting("CrashLoopBackOff", 4)), want: "CrashLoopBackOff"},
		{name: "init container below threshold", pod: newPod("default", "a", initWaiting("CrashLoopBackOff", 1))},
		{name: "second container bad", pod: newPod("default", "a", waiting("ContainerCreating", 0), crashing), want: "CrashLoopBackOff"},
		{name: "healthy", pod: newPod("default", "a")},
		{name: "reason not configured", pod: newPod("default", "a", waiting("ImagePullBackOff", 0))},
		{name: "custom reason", pod: newPod("default", "a", waiting("RunContainerError", 0)), cfg: func(c *Config) { c.Reasons = map[string]bool{"RunContainerError": true} }, want: "RunContainerError"},
		{name: "default reason removed", pod: newPod("default", "a", crashing), cfg: func(c *Config) { c.Reasons = map[string]bool{"CreateContainerError": true} }},
		{name: "bare pod", pod: newPod("default", "a", crashing, func(p *v1.Pod) { p.OwnerReferences = nil })},
		{name: "mirror pod", pod: newPod("default", "a", crashing, annotate(v1.MirrorPodAnnotationKey, "abc"))},
		{name: "skip annotation", pod: newPod("default", "a", crashing, annotate(SkipAnnotation, "true"))},
		{name: "skip annotation false", pod: newPod("default", "a", crashing, annotate(SkipAnnotation, "false")), want: "CrashLoopBackOff"},
		{name: "terminating", pod: newPod("default", "a", crashing, func(p *v1.Pod) { p.DeletionTimestamp = &now })},
		{name: "completed", pod: newPod("default", "a", crashing, func(p *v1.Pod) { p.Status.Phase = v1.PodSucceeded })},
		{name: "excluded namespace", pod: newPod("kube-system", "a", crashing)},
		{name: "not in include list", pod: newPod("default", "a", crashing), cfg: func(c *Config) { c.IncludeNamespaces = map[string]bool{"media": true} }},
		{name: "in include list", pod: newPod("media", "a", crashing), cfg: func(c *Config) { c.IncludeNamespaces = map[string]bool{"media": true} }, want: "CrashLoopBackOff"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			if tt.cfg != nil {
				tt.cfg(&cfg)
			}
			f, ok := NewReaper(nil, cfg).shouldRemediate(tt.pod)
			if ok != (tt.want != "") || f.Reason != tt.want {
				t.Fatalf("shouldRemediate() = (%+v, %v), want reason %q", f, ok, tt.want)
			}
		})
	}
}

func TestOwnerOf(t *testing.T) {
	p := newPod("media", "a", ownedBy("app-123"))
	notController := false
	p.OwnerReferences = append([]metav1.OwnerReference{{Kind: "Other", Name: "x", Controller: &notController}}, p.OwnerReferences...)
	if o := ownerOf(p); o.Key != "media/ReplicaSet/app-123" || o.Workload != "ReplicaSet/app-123" {
		t.Fatalf("ownerOf() = %+v", o)
	}

	p = newPod("media", "unpackerr-9ccf76887-x", ownedBy("unpackerr-9ccf76887"))
	p.Labels = map[string]string{"pod-template-hash": "9ccf76887"}
	if o := ownerOf(p); o.Key != "media/ReplicaSet/unpackerr-9ccf76887" || o.Workload != "Deployment/unpackerr" {
		t.Fatalf("ownerOf() = %+v", o)
	}
}

func TestFindingTerminationInfo(t *testing.T) {
	finished := time.Date(2026, 10, 26, 9, 14, 39, 0, time.UTC)
	p := newPod("default", "a", crashing, func(p *v1.Pod) {
		p.Status.ContainerStatuses[0].LastTerminationState.Terminated = &v1.ContainerStateTerminated{
			ExitCode: 137, Reason: "OOMKilled", FinishedAt: metav1.NewTime(finished),
		}
	})
	f, ok := NewReaper(nil, testConfig()).shouldRemediate(p)
	if !ok || f.LastExitCode == nil || *f.LastExitCode != 137 || f.LastTermReason != "OOMKilled" || !f.LastFinishedAt.Equal(finished) {
		t.Fatalf("finding = %+v", f)
	}
	want := "container=app reason=CrashLoopBackOff restarts=5 last_exit=137 (OOMKilled) at 2026-10-26 09:14:39 UTC"
	if got := f.summary(time.UTC); got != want {
		t.Fatalf("summary() = %q, want %q", got, want)
	}
}

// harness runs remediation scans against a fake clientset with a fake clock.
type harness struct {
	t        *testing.T
	client   *fake.Clientset
	reaper   *Reaper
	clock    time.Time
	recorder *record.FakeRecorder
	notifier *fakeNotifier
}

type fakeNotifier struct {
	msgs []string
	err  error
}

func (n *fakeNotifier) Notify(_ context.Context, msg string) error {
	n.msgs = append(n.msgs, msg)
	return n.err
}

// events drains recorded events.
func (h *harness) events() []string {
	var out []string
	for {
		select {
		case e := <-h.recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func newHarness(t *testing.T, cfg Config, pods ...*v1.Pod) *harness {
	h := &harness{t: t, client: fake.NewClientset(), clock: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	for _, p := range pods {
		h.add(p)
	}
	h.recorder = record.NewFakeRecorder(100)
	h.notifier = &fakeNotifier{}
	h.reaper = NewReaper(h.client, cfg)
	h.reaper.now = func() time.Time { return h.clock }
	h.reaper.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	h.reaper.recorder = h.recorder
	h.reaper.notifier = h.notifier
	return h
}

func (h *harness) add(p *v1.Pod) {
	if _, err := h.client.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) scan(advance time.Duration) {
	h.clock = h.clock.Add(advance)
	if err := h.reaper.remediatePods(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) pods() []string {
	list, err := h.client.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, p := range list.Items {
		out = append(out, p.Namespace+"/"+p.Name)
	}
	sort.Strings(out)
	return out
}

func (h *harness) expectPods(want ...string) {
	h.t.Helper()
	got := h.pods()
	sort.Strings(want)
	if len(got) != len(want) {
		h.t.Fatalf("pods = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			h.t.Fatalf("pods = %v, want %v", got, want)
		}
	}
}

func TestRemediatePodsFilters(t *testing.T) {
	h := newHarness(t, testConfig(),
		newPod("default", "bad", crashing),
		newPod("default", "good"),
		newPod("default", "young", waiting("CrashLoopBackOff", 1)),
		newPod("default", "bare", crashing, func(p *v1.Pod) { p.OwnerReferences = nil }),
		newPod("kube-system", "system", crashing),
	)
	h.scan(0)
	h.expectPods("default/good", "default/young", "default/bare", "kube-system/system")

	events := h.events()
	if len(events) != 2 ||
		!strings.HasPrefix(events[0], "Warning Reaped Deleted by pod-reaper: container=app reason=CrashLoopBackOff restarts=5") ||
		!strings.HasPrefix(events[1], "Warning ReapedPod pod-reaper deleted pod bad:") {
		t.Fatalf("events = %q", events)
	}
	if got := testutil.ToFloat64(h.reaper.metrics.reaped.WithLabelValues("default", "ReplicaSet/bad-rs", "CrashLoopBackOff")); got != 1 {
		t.Fatalf("reaped_total = %v, want 1", got)
	}
	if len(h.notifier.msgs) != 1 || !strings.Contains(h.notifier.msgs[0], "pod-reaper: reaped default/bad (ReplicaSet/bad-rs): container=app") {
		t.Fatalf("notifications = %q", h.notifier.msgs)
	}

	// Nothing eligible: no further events or notifications.
	h.scan(time.Minute)
	if len(h.events()) != 0 || len(h.notifier.msgs) != 1 {
		t.Fatalf("unexpected follow-up events/notifications: %q", h.notifier.msgs)
	}
}

func TestRemediatePodsCooldown(t *testing.T) {
	h := newHarness(t, testConfig(),
		newPod("default", "a1", crashing, ownedBy("app-rs")),
		newPod("default", "a2", crashing, ownedBy("app-rs")),
		newPod("default", "other", crashing),
	)

	// One pod per owner per scan; the second app-rs pod waits for the cooldown.
	h.scan(0)
	if got := len(h.pods()); got != 1 {
		t.Fatalf("after first scan pods = %v, want 1 remaining", h.pods())
	}
	remaining := h.pods()[0]

	h.scan(9 * time.Minute)
	h.expectPods(remaining)

	h.scan(time.Minute) // cooldown elapsed
	h.expectPods()

	if got := testutil.ToFloat64(h.reaper.metrics.skipped.WithLabelValues("cooldown")); got != 1 {
		t.Fatalf("skipped{cooldown} = %v, want 1 (logged once per window)", got)
	}
	last := h.notifier.msgs[len(h.notifier.msgs)-1]
	if !strings.Contains(last, "ReplicaSet/app-rs reaped 2 times in the last 24h") {
		t.Fatalf("expected repeat-offender note, got %q", last)
	}
}

func TestHistoryWindow(t *testing.T) {
	h := newHarness(t, testConfig(), newPod("default", "a1", crashing, ownedBy("app-rs")))
	h.scan(0)
	h.add(newPod("default", "a2", crashing, ownedBy("app-rs")))
	h.scan(25 * time.Hour) // first reap has aged out of the 24h window
	if last := h.notifier.msgs[len(h.notifier.msgs)-1]; strings.Contains(last, "times in the last 24h") {
		t.Fatalf("stale history counted: %q", last)
	}
}

func TestNotifyFailureCounted(t *testing.T) {
	h := newHarness(t, testConfig(), newPod("default", "a", crashing))
	h.notifier.err = errors.New("boom")
	h.scan(0)
	h.expectPods()
	if got := testutil.ToFloat64(h.reaper.metrics.notifyErrors); got != 1 {
		t.Fatalf("notify_errors_total = %v, want 1", got)
	}
}

func TestScanErrorCounted(t *testing.T) {
	h := newHarness(t, testConfig())
	h.client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api down")
	})
	if err := h.reaper.remediatePods(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if got := testutil.ToFloat64(h.reaper.metrics.scanErrors); got != 1 {
		t.Fatalf("scan_errors_total = %v, want 1", got)
	}
	if h.reaper.metrics.lastCompleted.Load() == 0 {
		t.Fatal("failed scans should still count as completed for liveness")
	}
}

func TestRemediatePodsCooldownDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.Cooldown = 0
	h := newHarness(t, cfg,
		newPod("default", "a1", crashing, ownedBy("app-rs")),
		newPod("default", "a2", crashing, ownedBy("app-rs")),
	)
	h.scan(0)
	h.expectPods()
}

func TestRemediatePodsRateLimit(t *testing.T) {
	cfg := testConfig()
	cfg.MaxReapsPerHour = 2
	h := newHarness(t, cfg,
		newPod("default", "a", crashing),
		newPod("default", "b", crashing),
		newPod("default", "c", crashing),
	)
	h.scan(0)
	if got := len(h.pods()); got != 1 {
		t.Fatalf("pods = %v, want 1 remaining after hitting the cap", h.pods())
	}
	h.scan(59 * time.Minute)
	if got := len(h.pods()); got != 1 {
		t.Fatalf("pods = %v, cap should still apply", h.pods())
	}
	h.scan(time.Minute) // first reaps age out of the window
	h.expectPods()

	var limitNotes int
	for _, m := range h.notifier.msgs {
		if strings.Contains(m, "hourly limit of 2 reaps reached") {
			limitNotes++
		}
	}
	if limitNotes != 1 {
		t.Fatalf("expected one rate-limit notification, got %d in %q", limitNotes, h.notifier.msgs)
	}
}

func TestRemediatePodsDryRun(t *testing.T) {
	cfg := testConfig()
	cfg.DryRun = true
	cfg.MaxReapsPerHour = 1
	h := newHarness(t, cfg, newPod("default", "a", crashing), newPod("default", "b", crashing))
	h.scan(0)
	h.scan(time.Second)
	h.expectPods("default/a", "default/b")
	if len(h.events()) != 0 {
		t.Fatal("dry-run should not emit events")
	}
	if len(h.notifier.msgs) == 0 || !strings.HasPrefix(h.notifier.msgs[0], "pod-reaper [dry-run]: would reap") {
		t.Fatalf("notifications = %q", h.notifier.msgs)
	}
	if len(h.reaper.lastReap) != 1 || len(h.reaper.reapTimes) != 1 {
		t.Fatalf("dry-run should record would-be reaps: lastReap=%v reapTimes=%v", h.reaper.lastReap, h.reaper.reapTimes)
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("DRY_RUN", "true")
	t.Setenv("INCLUDE_NAMESPACES", "a, b,,")
	t.Setenv("POLL_INTERVAL", "30s")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DryRun || cfg.Interval != 30*time.Second || len(cfg.IncludeNamespaces) != 2 || !cfg.ExcludeNamespaces["kube-system"] {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.LogFormat != "text" || !cfg.Events || cfg.MetricsAddr != ":8080" || cfg.WebhookURL != "" {
		t.Fatalf("unexpected visibility defaults: %+v", cfg)
	}
	if cfg.MinRestarts != 3 || cfg.Cooldown != 10*time.Minute || cfg.MaxReapsPerHour != 20 ||
		len(cfg.Reasons) != 2 || !cfg.Reasons["CrashLoopBackOff"] || !cfg.Reasons["CreateContainerError"] {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}

	t.Setenv("MIN_RESTARTS", "0")
	t.Setenv("COOLDOWN", "0")
	t.Setenv("MAX_REAPS_PER_HOUR", "0")
	t.Setenv("REASONS", "RunContainerError")
	if cfg, err = loadConfig(); err != nil {
		t.Fatal(err)
	}
	if cfg.MinRestarts != 0 || cfg.Cooldown != 0 || cfg.MaxReapsPerHour != 0 || !cfg.Reasons["RunContainerError"] || len(cfg.Reasons) != 1 {
		t.Fatalf("overrides not applied: %+v", cfg)
	}

	t.Setenv("EXCLUDE_NAMESPACES", "")
	if cfg, _ = loadConfig(); len(cfg.ExcludeNamespaces) != 0 {
		t.Fatalf("explicit empty EXCLUDE_NAMESPACES should clear default, got %v", cfg.ExcludeNamespaces)
	}

	for key, bad := range map[string]string{
		"DRY_RUN":            "nope",
		"MIN_RESTARTS":       "-1",
		"MAX_REAPS_PER_HOUR": "lots",
		"COOLDOWN":           "-5m",
		"POLL_INTERVAL":      "0",
		"REASONS":            " , ",
		"LOG_FORMAT":         "xml",
		"EVENTS":             "maybe",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, bad)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("expected error for %s=%q", key, bad)
			}
		})
	}
}
