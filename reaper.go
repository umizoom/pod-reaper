package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
)

const (
	notifyTimeout   = 10 * time.Second
	historyWindow   = 24 * time.Hour
	timestampLayout = "2006-01-02 15:04:05 MST"
)

// finding describes why a pod qualifies for remediation.
type finding struct {
	Container      string
	Reason         string
	Restarts       int32
	LastExitCode   *int32 // from the container's last termination, if known
	LastTermReason string // e.g. Error, OOMKilled
	LastFinishedAt time.Time
}

func (f finding) attrs() []any {
	a := []any{"container", f.Container, "reason", f.Reason, "restarts", f.Restarts}
	if f.LastExitCode != nil {
		a = append(a, "last_exit_code", *f.LastExitCode, "last_termination_reason", f.LastTermReason)
		if !f.LastFinishedAt.IsZero() {
			a = append(a, "last_finished_at", f.LastFinishedAt)
		}
	}
	return a
}

func (f finding) summary(loc *time.Location) string {
	s := fmt.Sprintf("container=%s reason=%s restarts=%d", f.Container, f.Reason, f.Restarts)
	if f.LastExitCode != nil {
		s += fmt.Sprintf(" last_exit=%d (%s)", *f.LastExitCode, f.LastTermReason)
		if !f.LastFinishedAt.IsZero() {
			s += " at " + f.LastFinishedAt.In(loc).Format(timestampLayout)
		}
	}
	return s
}

// owner identifies who recreates a pod.
type owner struct {
	Ref      metav1.OwnerReference
	Key      string // namespace/Kind/name of the controlling owner; cooldown key
	Workload string // stable name across rollouts, e.g. Deployment/unpackerr
}

// ownerOf resolves the pod's controlling owner. Pods reach here only with owners.
func ownerOf(pod *v1.Pod) owner {
	ref := pod.OwnerReferences[0]
	if c := metav1.GetControllerOfNoCopy(pod); c != nil {
		ref = *c
	}
	workload := ref.Kind + "/" + ref.Name
	if ref.Kind == "ReplicaSet" {
		if h := pod.Labels["pod-template-hash"]; h != "" && strings.HasSuffix(ref.Name, "-"+h) {
			workload = "Deployment/" + strings.TrimSuffix(ref.Name, "-"+h)
		}
	}
	return owner{Ref: ref, Key: pod.Namespace + "/" + ref.Kind + "/" + ref.Name, Workload: workload}
}

// Reaper deletes pods stuck in a bad container state.
//
// Cooldown, rate-limit and history state is kept in memory and resets on restart.
type Reaper struct {
	client   kubernetes.Interface
	cfg      Config
	now      func() time.Time
	logger   *slog.Logger
	metrics  *Metrics
	recorder record.EventRecorder // optional
	notifier Notifier             // optional

	lastReap       map[string]time.Time   // owner key -> last reap time
	cooldownLogged map[string]bool        // owner key -> skip already logged this window
	reapTimes      []time.Time            // reaps within the last hour
	rateLogged     bool                   // rate-limit hit already logged
	history        map[string][]time.Time // namespace/workload -> reaps within historyWindow
}

func NewReaper(client kubernetes.Interface, cfg Config) *Reaper {
	return &Reaper{
		client:         client,
		cfg:            cfg,
		now:            time.Now,
		logger:         slog.Default(),
		metrics:        newMetrics(),
		lastReap:       map[string]time.Time{},
		cooldownLogged: map[string]bool{},
		history:        map[string][]time.Time{},
	}
}

func (r *Reaper) remediatePods(ctx context.Context) error {
	start := r.now()
	defer func() {
		end := r.now()
		r.metrics.scanDuration.Observe(end.Sub(start).Seconds())
		r.metrics.lastCompleted.Store(end.UnixNano())
	}()

	podList, err := r.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		r.metrics.scanErrors.Inc()
		return err
	}
	r.metrics.lastSuccess.Set(float64(r.now().Unix()))

	r.prune()

	var notes []string
	for i := range podList.Items {
		pod := &podList.Items[i]
		f, ok := r.shouldRemediate(pod)
		if !ok {
			continue
		}
		o := ownerOf(pod)
		log := r.logger.With("namespace", pod.Namespace, "pod", pod.Name, "owner", o.Key, "workload", o.Workload).With(f.attrs()...)

		if last, ok := r.lastReap[o.Key]; ok {
			if !r.cooldownLogged[o.Key] {
				log.Info("skipping pod: owner in cooldown",
					"reaped_ago", r.now().Sub(last).Round(time.Second).String(), "cooldown", r.cfg.Cooldown.String())
				r.cooldownLogged[o.Key] = true
				r.metrics.skipped.WithLabelValues("cooldown").Inc()
			}
			continue
		}

		if r.cfg.MaxReapsPerHour > 0 && len(r.reapTimes) >= r.cfg.MaxReapsPerHour {
			if !r.rateLogged {
				log.Warn("skipping pod: hourly reap limit reached; pausing reaps until it clears",
					"max_reaps_per_hour", r.cfg.MaxReapsPerHour)
				notes = append(notes, fmt.Sprintf("pod-reaper: hourly limit of %d reaps reached; reaping is paused until it clears (next skipped: %s/%s).",
					r.cfg.MaxReapsPerHour, pod.Namespace, pod.Name))
				r.rateLogged = true
				r.metrics.skipped.WithLabelValues("rate_limit").Inc()
			}
			continue
		}

		if r.cfg.DryRun {
			count := r.record(pod.Namespace, o)
			log.Info("dry-run: would reap pod", "reaps_last_24h", count)
			notes = append(notes, r.message(pod, o, f, true, count))
			continue
		}

		if err := r.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
			log.Error("failed to delete pod", "error", err)
			r.metrics.deleteErrors.Inc()
			continue
		}
		count := r.record(pod.Namespace, o)
		log.Info("reaped pod", "reaps_last_24h", count)
		r.metrics.reaped.WithLabelValues(pod.Namespace, o.Workload, f.Reason).Inc()
		r.emitEvents(pod, o, f)
		notes = append(notes, r.message(pod, o, f, false, count))
	}

	r.notify(ctx, notes)
	return nil
}

// record stores a (would-be) reap and returns the workload's reaps in the last 24h.
func (r *Reaper) record(namespace string, o owner) int {
	now := r.now()
	if r.cfg.Cooldown > 0 {
		r.lastReap[o.Key] = now
	}
	r.reapTimes = append(r.reapTimes, now)
	key := namespace + "/" + o.Workload
	r.history[key] = append(r.history[key], now)
	return len(r.history[key])
}

// prune drops expired cooldown entries, reaps older than an hour and stale history.
func (r *Reaper) prune() {
	now := r.now()
	for key, t := range r.lastReap {
		if now.Sub(t) >= r.cfg.Cooldown {
			delete(r.lastReap, key)
			delete(r.cooldownLogged, key)
		}
	}
	r.reapTimes = within(r.reapTimes, now, time.Hour)
	if r.cfg.MaxReapsPerHour == 0 || len(r.reapTimes) < r.cfg.MaxReapsPerHour {
		r.rateLogged = false
	}
	for key, times := range r.history {
		if kept := within(times, now, historyWindow); len(kept) > 0 {
			r.history[key] = kept
		} else {
			delete(r.history, key)
		}
	}
}

func within(times []time.Time, now time.Time, window time.Duration) []time.Time {
	kept := times[:0]
	for _, t := range times {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	return kept
}

func (r *Reaper) emitEvents(pod *v1.Pod, o owner, f finding) {
	if r.recorder == nil {
		return
	}
	summary := f.summary(r.cfg.Location)
	r.recorder.Eventf(pod, v1.EventTypeWarning, "Reaped", "Deleted by pod-reaper: %s", summary)
	ref := &v1.ObjectReference{
		Kind:       o.Ref.Kind,
		APIVersion: o.Ref.APIVersion,
		Name:       o.Ref.Name,
		Namespace:  pod.Namespace,
		UID:        o.Ref.UID,
	}
	r.recorder.Eventf(ref, v1.EventTypeWarning, "ReapedPod", "pod-reaper deleted pod %s: %s", pod.Name, summary)
}

func (r *Reaper) message(pod *v1.Pod, o owner, f finding, dryRun bool, count int) string {
	verb := "pod-reaper: reaped"
	if dryRun {
		verb = "pod-reaper [dry-run]: would reap"
	}
	s := fmt.Sprintf("%s %s/%s (%s): %s", verb, pod.Namespace, pod.Name, o.Workload, f.summary(r.cfg.Location))
	if count > 1 {
		s += fmt.Sprintf(" | %s reaped %d times in the last 24h", o.Workload, count)
	}
	return s
}

// notify sends this scan's messages as one webhook post.
func (r *Reaper) notify(ctx context.Context, notes []string) {
	if r.notifier == nil || len(notes) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	if err := r.notifier.Notify(ctx, strings.Join(notes, "\n")); err != nil {
		r.logger.Error("webhook notification failed", "error", err)
		r.metrics.notifyErrors.Inc()
	}
}

// shouldRemediate reports whether the pod should be deleted and why.
func (r *Reaper) shouldRemediate(pod *v1.Pod) (finding, bool) {
	if !r.inScope(pod) {
		return finding{}, false
	}
	statuses := append(append([]v1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for _, cs := range statuses {
		w := cs.State.Waiting
		if w == nil || !r.cfg.Reasons[w.Reason] {
			continue
		}
		if w.Reason == ReasonCrashLoopBackOff && cs.RestartCount < r.cfg.MinRestarts {
			continue
		}
		f := finding{Container: cs.Name, Reason: w.Reason, Restarts: cs.RestartCount}
		if t := cs.LastTerminationState.Terminated; t != nil {
			code := t.ExitCode
			f.LastExitCode = &code
			f.LastTermReason = t.Reason
			f.LastFinishedAt = t.FinishedAt.Time
		}
		return f, true
	}
	return finding{}, false
}

// inScope filters out pods that must never be deleted by the reaper.
func (r *Reaper) inScope(pod *v1.Pod) bool {
	switch {
	case pod.DeletionTimestamp != nil:
		return false // already terminating
	case pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed:
		return false // completed; nothing to restart
	case len(pod.OwnerReferences) == 0:
		return false // bare pod: deleting it loses it for good
	case pod.Annotations[v1.MirrorPodAnnotationKey] != "":
		return false // static pod mirror: delete is a no-op
	case pod.Annotations[SkipAnnotation] == "true":
		return false
	case r.cfg.ExcludeNamespaces[pod.Namespace]:
		return false
	case len(r.cfg.IncludeNamespaces) > 0 && !r.cfg.IncludeNamespaces[pod.Namespace]:
		return false
	}
	return true
}
