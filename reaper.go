package main

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// finding describes why a pod qualifies for remediation.
type finding struct {
	Container string
	Reason    string
	Restarts  int32
}

func (f finding) String() string {
	return fmt.Sprintf("container=%s reason=%s restarts=%d", f.Container, f.Reason, f.Restarts)
}

// Reaper deletes pods stuck in a bad container state.
//
// Cooldown and rate-limit state is kept in memory and resets on restart.
type Reaper struct {
	client kubernetes.Interface
	cfg    Config
	now    func() time.Time

	lastReap       map[string]time.Time // owner key -> last reap time
	cooldownLogged map[string]bool      // owner key -> skip already logged this window
	reapTimes      []time.Time          // reaps within the last hour
	rateLogged     bool                 // rate-limit hit already logged
}

func NewReaper(client kubernetes.Interface, cfg Config) *Reaper {
	return &Reaper{
		client:         client,
		cfg:            cfg,
		now:            time.Now,
		lastReap:       map[string]time.Time{},
		cooldownLogged: map[string]bool{},
	}
}

func (r *Reaper) remediatePods(ctx context.Context) error {
	podList, err := r.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	r.prune()

	for i := range podList.Items {
		pod := &podList.Items[i]
		f, ok := r.shouldRemediate(pod)
		if !ok {
			continue
		}
		name := pod.Namespace + "/" + pod.Name
		owner := ownerKey(pod)

		if last, ok := r.lastReap[owner]; ok {
			if !r.cooldownLogged[owner] {
				r.log("Skipping pod %s (%s): owner %s reaped %s ago, cooldown %s",
					name, f, owner, r.now().Sub(last).Round(time.Second), r.cfg.Cooldown)
				r.cooldownLogged[owner] = true
			}
			continue
		}

		if r.cfg.MaxReapsPerHour > 0 && len(r.reapTimes) >= r.cfg.MaxReapsPerHour {
			if !r.rateLogged {
				r.log("Rate limit reached (%d reaps in the last hour); skipping pod %s (%s) and others until it clears",
					r.cfg.MaxReapsPerHour, name, f)
				r.rateLogged = true
			}
			continue
		}

		if r.cfg.DryRun {
			r.log("[dry-run] Would remediate pod %s (%s)", name, f)
			r.record(owner)
			continue
		}

		r.log("Remediating pod %s (%s)...", name, f)
		if err := r.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
			r.log("Failed to delete pod %s: %v", name, err)
			continue
		}
		r.log("Pod %s deleted successfully.", name)
		r.record(owner)
	}

	return nil
}

func (r *Reaper) record(owner string) {
	now := r.now()
	if r.cfg.Cooldown > 0 {
		r.lastReap[owner] = now
	}
	r.reapTimes = append(r.reapTimes, now)
}

// prune drops expired cooldown entries and reaps older than an hour.
func (r *Reaper) prune() {
	now := r.now()
	for owner, t := range r.lastReap {
		if now.Sub(t) >= r.cfg.Cooldown {
			delete(r.lastReap, owner)
			delete(r.cooldownLogged, owner)
		}
	}
	kept := r.reapTimes[:0]
	for _, t := range r.reapTimes {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	r.reapTimes = kept
	if r.cfg.MaxReapsPerHour == 0 || len(r.reapTimes) < r.cfg.MaxReapsPerHour {
		r.rateLogged = false
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
		return finding{Container: cs.Name, Reason: w.Reason, Restarts: cs.RestartCount}, true
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

// ownerKey identifies the controller that owns the pod, e.g.
// "media/ReplicaSet/unpackerr-9ccf76887". Pods reach here only with owners.
func ownerKey(pod *v1.Pod) string {
	ref := pod.OwnerReferences[0]
	if c := metav1.GetControllerOfNoCopy(pod); c != nil {
		ref = *c
	}
	return pod.Namespace + "/" + ref.Kind + "/" + ref.Name
}

func (r *Reaper) log(format string, args ...any) {
	logAt(r.cfg.Location, format, args...)
}

func logAt(loc *time.Location, format string, args ...any) {
	timestamp := time.Now().In(loc).Format("2006-01-02 15:04:05 MST")
	fmt.Printf("[%s] %s\n", timestamp, fmt.Sprintf(format, args...))
}
