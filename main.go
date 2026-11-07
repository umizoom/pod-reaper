package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "Path to kubeconfig file (defaults to $KUBECONFIG or ~/.kube/config)")
	flag.Parse()

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	restConfig, err := buildRestConfig(*kubeconfig, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	reaper := &Reaper{client: clientset, cfg: cfg}
	reaper.log("Starting Pod Reaper controller (dry-run=%t, interval=%s, include=%v, exclude=%v)...",
		cfg.DryRun, cfg.Interval, keys(cfg.IncludeNamespaces), keys(cfg.ExcludeNamespaces))

	wait.UntilWithContext(ctx, func(ctx context.Context) {
		if err := reaper.remediatePods(ctx); err != nil && ctx.Err() == nil {
			reaper.log("Error during remediation: %v", err)
		}
	}, cfg.Interval)

	reaper.log("Received shutdown signal, stopped.")
}

// buildRestConfig prefers in-cluster config and falls back to kubeconfig.
// The legacy inCluster=true env var forces in-cluster mode.
func buildRestConfig(kubeconfig string, cfg Config) (*rest.Config, error) {
	forceInCluster := os.Getenv("inCluster") == "true"
	if kubeconfig == "" {
		if c, err := rest.InClusterConfig(); err == nil {
			logAt(cfg.Location, "Using in-cluster configuration.")
			return c, nil
		} else if forceInCluster {
			return nil, fmt.Errorf("failed to load in-cluster config: %w", err)
		}
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	c, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}
	logAt(cfg.Location, "Using kubeconfig configuration.")
	return c, nil
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
