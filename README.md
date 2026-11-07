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
| `POLL_INTERVAL` | `10s` | How often to scan pods (Go duration) |
| `TIMEZONE` | `UTC` | Timezone for log timestamps |
| `inCluster` | _(auto)_ | Legacy; in-cluster config is now auto-detected, falling back to `--kubeconfig` / `$KUBECONFIG` / `~/.kube/config` |

Pods are never deleted when they:
- have no owner (bare pods would not be recreated)
- are static/mirror pods, already terminating, or completed
- carry the annotation `pod-reaper.io/skip: "true"`
