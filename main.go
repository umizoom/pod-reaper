package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
)

// apiTimeout bounds every API request so a hung call cannot stall the loop.
const apiTimeout = 30 * time.Second

func main() {
	kubeconfig := flag.String("kubeconfig", "", "Path to kubeconfig file (defaults to $KUBECONFIG or ~/.kube/config)")
	flag.Parse()

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	logger := newLogger(cfg, os.Stdout)
	slog.SetDefault(logger)

	if err := run(cfg, *kubeconfig, logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(cfg Config, kubeconfig string, logger *slog.Logger) error {
	restConfig, err := buildRestConfig(kubeconfig, logger)
	if err != nil {
		return err
	}
	restConfig.Timeout = apiTimeout

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	reaper := NewReaper(clientset, cfg)
	reaper.logger = logger

	if cfg.Events {
		broadcaster := record.NewBroadcaster(record.WithContext(ctx))
		broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: clientset.CoreV1().Events("")})
		defer broadcaster.Shutdown()
		reaper.recorder = broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "pod-reaper"})
	}

	if cfg.WebhookURL != "" {
		n, err := newWebhookNotifier(cfg.WebhookURL, cfg.Location)
		if err != nil {
			return err
		}
		reaper.notifier = n
	}

	var srv *http.Server
	if cfg.MetricsAddr != "" {
		srv = &http.Server{
			Addr:              cfg.MetricsAddr,
			Handler:           reaper.metrics.handler(cfg.Interval, reaper.now, reaper.now()),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("metrics server failed", "addr", cfg.MetricsAddr, "error", err)
				stop()
			}
		}()
	}

	logger.Info("starting pod-reaper",
		"dry_run", cfg.DryRun,
		"interval", cfg.Interval.String(),
		"include_namespaces", keys(cfg.IncludeNamespaces),
		"exclude_namespaces", keys(cfg.ExcludeNamespaces),
		"reasons", keys(cfg.Reasons),
		"min_restarts", cfg.MinRestarts,
		"cooldown", cfg.Cooldown.String(),
		"max_reaps_per_hour", cfg.MaxReapsPerHour,
		"events", cfg.Events,
		"metrics_addr", cfg.MetricsAddr,
		"webhook", cfg.WebhookURL != "",
	)

	wait.UntilWithContext(ctx, func(ctx context.Context) {
		if err := reaper.remediatePods(ctx); err != nil && ctx.Err() == nil {
			logger.Error("scan failed", "error", err)
		}
	}, cfg.Interval)

	logger.Info("shutting down")
	if srv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
	return nil
}

// buildRestConfig prefers in-cluster config and falls back to kubeconfig.
// The legacy inCluster=true env var forces in-cluster mode.
func buildRestConfig(kubeconfig string, logger *slog.Logger) (*rest.Config, error) {
	forceInCluster := os.Getenv("inCluster") == "true"
	if kubeconfig == "" {
		if c, err := rest.InClusterConfig(); err == nil {
			logger.Info("using in-cluster configuration")
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
	logger.Info("using kubeconfig configuration")
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
