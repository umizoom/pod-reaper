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

// ReasonCrashLoopBackOff is the only reason MIN_RESTARTS applies to; other
// reasons (e.g. CreateContainerError) never increment the restart count.
const ReasonCrashLoopBackOff = "CrashLoopBackOff"

// Config holds runtime settings, read from environment variables.
type Config struct {
	DryRun            bool
	IncludeNamespaces map[string]bool // empty means all namespaces
	ExcludeNamespaces map[string]bool
	Reasons           map[string]bool
	MinRestarts       int32
	Cooldown          time.Duration
	MaxReapsPerHour   int // 0 means unlimited
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
	cfg.ExcludeNamespaces = parseSet(envDefault("EXCLUDE_NAMESPACES", "kube-system"))

	cfg.Reasons = parseSet(envDefault("REASONS", "CrashLoopBackOff,CreateContainerError"))
	if len(cfg.Reasons) == 0 {
		return cfg, fmt.Errorf("REASONS must not be empty")
	}

	minRestarts, err := envInt("MIN_RESTARTS", 3)
	if err != nil {
		return cfg, err
	}
	cfg.MinRestarts = int32(minRestarts)

	if cfg.MaxReapsPerHour, err = envInt("MAX_REAPS_PER_HOUR", 20); err != nil {
		return cfg, err
	}
	if cfg.Cooldown, err = envDuration("COOLDOWN", 10*time.Minute, true); err != nil {
		return cfg, err
	}
	if cfg.Interval, err = envDuration("POLL_INTERVAL", 10*time.Second, false); err != nil {
		return cfg, err
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

// envDefault returns def only when key is unset, so an explicit empty value
// can clear a default (e.g. EXCLUDE_NAMESPACES="").
func envDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
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

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 1<<30 {
		return def, fmt.Errorf("invalid %s %q: must be a non-negative integer", key, v)
	}
	return n, nil
}

func envDuration(key string, def time.Duration, allowZero bool) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 || (d == 0 && !allowZero) {
		return def, fmt.Errorf("invalid %s %q", key, v)
	}
	return d, nil
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
