package main

import (
	"context"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type podOpt func(*v1.Pod)

func newPod(ns, name string, opts ...podOpt) *v1.Pod {
	p := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       ns,
			Name:            name,
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: name + "-rs"}},
		},
		Status: v1.PodStatus{Phase: v1.PodRunning},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func waiting(reason string) podOpt {
	return func(p *v1.Pod) {
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, v1.ContainerStatus{
			State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: reason}},
		})
	}
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
		Location:          time.UTC,
	}
}

func TestShouldRemediate(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name    string
		pod     *v1.Pod
		cfg     func(*Config)
		want    bool
		wantWhy string
	}{
		{name: "crashloop", pod: newPod("default", "a", waiting("CrashLoopBackOff")), want: true, wantWhy: "CrashLoopBackOff"},
		{name: "create error", pod: newPod("default", "a", waiting("CreateContainerError")), want: true, wantWhy: "CreateContainerError"},
		{name: "second container bad", pod: newPod("default", "a", waiting("ContainerCreating"), waiting("CrashLoopBackOff")), want: true, wantWhy: "CrashLoopBackOff"},
		{name: "healthy", pod: newPod("default", "a")},
		{name: "other waiting reason", pod: newPod("default", "a", waiting("ImagePullBackOff"))},
		{name: "bare pod", pod: newPod("default", "a", waiting("CrashLoopBackOff"), func(p *v1.Pod) { p.OwnerReferences = nil })},
		{name: "mirror pod", pod: newPod("default", "a", waiting("CrashLoopBackOff"), annotate(v1.MirrorPodAnnotationKey, "abc"))},
		{name: "skip annotation", pod: newPod("default", "a", waiting("CrashLoopBackOff"), annotate(SkipAnnotation, "true"))},
		{name: "skip annotation false", pod: newPod("default", "a", waiting("CrashLoopBackOff"), annotate(SkipAnnotation, "false")), want: true, wantWhy: "CrashLoopBackOff"},
		{name: "terminating", pod: newPod("default", "a", waiting("CrashLoopBackOff"), func(p *v1.Pod) { p.DeletionTimestamp = &now })},
		{name: "completed", pod: newPod("default", "a", waiting("CrashLoopBackOff"), func(p *v1.Pod) { p.Status.Phase = v1.PodSucceeded })},
		{name: "excluded namespace", pod: newPod("kube-system", "a", waiting("CrashLoopBackOff"))},
		{
			name: "not in include list",
			pod:  newPod("default", "a", waiting("CrashLoopBackOff")),
			cfg:  func(c *Config) { c.IncludeNamespaces = map[string]bool{"media": true} },
		},
		{
			name:    "in include list",
			pod:     newPod("media", "a", waiting("CrashLoopBackOff")),
			cfg:     func(c *Config) { c.IncludeNamespaces = map[string]bool{"media": true} },
			want:    true,
			wantWhy: "CrashLoopBackOff",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			if tt.cfg != nil {
				tt.cfg(&cfg)
			}
			r := &Reaper{cfg: cfg}
			why, got := r.shouldRemediate(tt.pod)
			if got != tt.want || why != tt.wantWhy {
				t.Fatalf("shouldRemediate() = (%q, %v), want (%q, %v)", why, got, tt.wantWhy, tt.want)
			}
		})
	}
}

func TestRemediatePods(t *testing.T) {
	pods := []*v1.Pod{
		newPod("default", "bad", waiting("CrashLoopBackOff")),
		newPod("default", "good"),
		newPod("default", "bare", waiting("CrashLoopBackOff"), func(p *v1.Pod) { p.OwnerReferences = nil }),
		newPod("kube-system", "system", waiting("CrashLoopBackOff")),
	}

	for _, dryRun := range []bool{false, true} {
		client := fake.NewSimpleClientset()
		for _, p := range pods {
			if _, err := client.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		cfg := testConfig()
		cfg.DryRun = dryRun
		r := &Reaper{client: client, cfg: cfg}
		if err := r.remediatePods(context.Background()); err != nil {
			t.Fatal(err)
		}

		remaining := map[string]bool{}
		list, _ := client.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
		for _, p := range list.Items {
			remaining[p.Namespace+"/"+p.Name] = true
		}
		wantBadGone := !dryRun
		if remaining["default/bad"] == wantBadGone {
			t.Errorf("dryRun=%v: default/bad present=%v", dryRun, remaining["default/bad"])
		}
		for _, k := range []string{"default/good", "default/bare", "kube-system/system"} {
			if !remaining[k] {
				t.Errorf("dryRun=%v: %s was deleted", dryRun, k)
			}
		}
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

	t.Setenv("EXCLUDE_NAMESPACES", "")
	if cfg, _ = loadConfig(); len(cfg.ExcludeNamespaces) != 0 {
		t.Fatalf("explicit empty EXCLUDE_NAMESPACES should clear default, got %v", cfg.ExcludeNamespaces)
	}

	t.Setenv("DRY_RUN", "nope")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected error for invalid DRY_RUN")
	}
}
