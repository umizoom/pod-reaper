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
| `TIMEZONE` | `UTC` | Timezone for log and notification timestamps |
| `LOG_FORMAT` | `text` | `text` or `json` (structured `log/slog` output) |
| `EVENTS` | `true` | Emit Kubernetes Events (`Reaped` on the pod, `ReapedPod` on its owner); needs `create`/`patch` on `events` |
| `METRICS_ADDR` | `:8080` | Address for `/metrics` (Prometheus) and `/healthz` (liveness); empty disables |
| `WEBHOOK_URL` | _(unset)_ | Optional Discord or Slack incoming webhook (auto-detected by host); load it from a Secret, it is never logged |
| `inCluster` | _(auto)_ | Legacy; in-cluster config is now auto-detected, falling back to `--kubeconfig` / `$KUBECONFIG` / `~/.kube/config` |

Pods are never deleted when they:
- have no owner (bare pods would not be recreated)
- are static/mirror pods, already terminating, or completed
- carry the annotation `pod-reaper.io/skip: "true"`

Cooldown and rate-limit state is held in memory and resets when pod-reaper restarts.

## Observability

Each reap logs, and optionally notifies, the container, reason, restart count, and the container's **last exit code and termination reason** (e.g. `last_exit=137 (OOMKilled)`). Repeat offenders are flagged with how many times the same workload was reaped in the last 24h.

Metrics:

| Metric | Labels |
|---|---|
| `pod_reaper_reaped_total` | `namespace`, `workload`, `reason` |
| `pod_reaper_skipped_total` | `reason` (`cooldown`, `rate_limit`) |
| `pod_reaper_delete_errors_total`, `pod_reaper_scan_errors_total`, `pod_reaper_notify_errors_total` | |
| `pod_reaper_last_successful_scan_timestamp_seconds`, `pod_reaper_scan_duration_seconds` | |

`/healthz` returns 503 if no scan has finished in `max(3 × POLL_INTERVAL, 2m)`.

RBAC for Events:

```yaml
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
```
