package main

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// remediationReasons are container waiting reasons that trigger a pod delete.
var remediationReasons = map[string]bool{
	"CreateContainerError": true,
	"CrashLoopBackOff":     true,
}

// Reaper deletes pods stuck in a bad container state.
type Reaper struct {
	client kubernetes.Interface
	cfg    Config
}

func (r *Reaper) remediatePods(ctx context.Context) error {
	podList, err := r.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, pod := range podList.Items {
		reason, ok := r.shouldRemediate(&pod)
		if !ok {
			continue
		}

		if r.cfg.DryRun {
			r.log("[dry-run] Would remediate pod %s/%s (%s)", pod.Namespace, pod.Name, reason)
			continue
		}

		r.log("Remediating pod %s/%s (%s)...", pod.Namespace, pod.Name, reason)
		err := r.client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
		if err != nil {
			r.log("Failed to delete pod %s/%s: %v", pod.Namespace, pod.Name, err)
		} else {
			r.log("Pod %s/%s deleted successfully.", pod.Namespace, pod.Name)
		}
	}

	return nil
}

// shouldRemediate reports whether the pod should be deleted and why.
func (r *Reaper) shouldRemediate(pod *v1.Pod) (string, bool) {
	if !r.inScope(pod) {
		return "", false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && remediationReasons[w.Reason] {
			return w.Reason, true
		}
	}
	return "", false
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

func (r *Reaper) log(format string, args ...any) {
	logAt(r.cfg.Location, format, args...)
}

func logAt(loc *time.Location, format string, args ...any) {
	timestamp := time.Now().In(loc).Format("2006-01-02 15:04:05 MST")
	fmt.Printf("[%s] %s\n", timestamp, fmt.Sprintf(format, args...))
}
