# pod-reaper

Watch for pods with CreateContainerError or CrashLoopBackOff and delete the pods
    
Ability to run local using kubectl or on the cluster its self by using a ENV inCluster

Docker file to create container image

## Configuration

| Env var | Default | Description |
|---|---|---|
| `DRY_RUN` | `false` | Log pods that would be deleted without deleting them |
| `INCLUDE_NAMESPACES` | _(all)_ | Comma-separated namespaces to act on |
| `EXCLUDE_NAMESPACES` | `kube-system` | Comma-separated namespaces to ignore (set to empty to ignore none) |
| `REASONS` | `CrashLoopBackOff,CreateContainerError` | Container waiting reasons (app or init containers) that trigger a delete |
| `MIN_RESTARTS` | `3` | Minimum restart count before a `CrashLoopBackOff` pod is deleted (0 disables; other reasons ignore it) |
| `COOLDOWN` | `10m` | After deleting a pod, wait this long before deleting another pod of the same owner (0 disables) |
| `MAX_REAPS_PER_HOUR` | `20` | Global cap on deletes in any rolling hour (0 = unlimited) |
| `POLL_INTERVAL` | `10s` | How often to scan pods (Go duration) |
| `TIMEZONE` | `UTC` | Timezone for log timestamps |
| `inCluster` | _(auto)_ | Legacy; in-cluster config is now auto-detected, falling back to `--kubeconfig` / `$KUBECONFIG` / `~/.kube/config` |

Pods are never deleted when they:
- have no owner (bare pods would not be recreated)
- are static/mirror pods, already terminating, or completed
- carry the annotation `pod-reaper.io/skip: "true"`

Cooldown and rate-limit state is held in memory and resets when pod-reaper restarts.
