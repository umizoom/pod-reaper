package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// SkipAnnotation lets a pod opt out of remediation when set to "true".
const SkipAnnotation = "pod-reaper.io/skip"

// Config holds runtime settings, read from environment variables.
type Config struct {
	DryRun            bool
	IncludeNamespaces map[string]bool // empty means all namespaces
	ExcludeNamespaces map[string]bool
	Interval          time.Duration
	Location          *time.Location
}

func loadConfig() (Config, error) {
	cfg := Config{}

	var err error
	if cfg.DryRun, err = envBool("DRY_RUN", false); err != nil {
		return cfg, err
	}

	cfg.IncludeNamespaces = parseSet(os.Getenv("INCLUDE_NAMESPACES"))
	exclude, ok := os.LookupEnv("EXCLUDE_NAMESPACES")
	if !ok {
		exclude = "kube-system"
	}
	cfg.ExcludeNamespaces = parseSet(exclude)

	cfg.Interval = 10 * time.Second
	if v := os.Getenv("POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("invalid POLL_INTERVAL %q", v)
		}
		cfg.Interval = d
	}

	tz := os.Getenv("TIMEZONE")
	if tz == "" {
		tz = "UTC"
	}
	if cfg.Location, err = time.LoadLocation(tz); err != nil {
		return cfg, fmt.Errorf("invalid TIMEZONE %q: %w", tz, err)
	}

	return cfg, nil
}

func envBool(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Errorf("invalid %s %q: %w", key, v, err)
	}
	return b, nil
}

func parseSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			set[item] = true
		}
	}
	return set
}
