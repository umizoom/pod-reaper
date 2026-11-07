package main

import (
	"fmt"
	"strings"
	"time"
)

// AlertKind distinguishes the notifications pod-reaper sends.
type AlertKind int

const (
	AlertReaped AlertKind = iota
	AlertDryRun
	AlertRateLimited
)

// repeatThreshold is the 24h reap count at which a workload is flagged red.
const repeatThreshold = 3

// Alert is one notification, rendered per webhook flavour.
type Alert struct {
	Kind      AlertKind
	Time      time.Time
	Namespace string
	Pod       string
	Workload  string
	Finding   finding
	Count24h  int // reaps of this workload in the last 24h, including this one
	MaxPerHr  int // for AlertRateLimited
}

func (a Alert) title() string {
	switch a.Kind {
	case AlertRateLimited:
		return "Reaping paused: hourly limit reached"
	case AlertDryRun:
		return "[dry-run] Would reap " + a.Workload
	default:
		return "Reaped " + a.Workload
	}
}

// Discord embed colours.
const (
	colorReaped  = 0xE67E22 // orange
	colorRepeat  = 0xE74C3C // red
	colorLimited = 0x992D22 // dark red
	colorDryRun  = 0x95A5A6 // grey
)

func (a Alert) color() int {
	switch {
	case a.Kind == AlertRateLimited:
		return colorLimited
	case a.Kind == AlertDryRun:
		return colorDryRun
	case a.Count24h >= repeatThreshold:
		return colorRepeat
	default:
		return colorReaped
	}
}

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type discordEmbed struct {
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color"`
	Fields      []discordField `json:"fields,omitempty"`
	Footer      *discordFooter `json:"footer,omitempty"`
	Timestamp   string         `json:"timestamp,omitempty"`
}

type discordFooter struct {
	Text string `json:"text"`
}

func (a Alert) discordEmbed() discordEmbed {
	e := discordEmbed{
		Title:     truncate(a.title(), 256),
		Color:     a.color(),
		Footer:    &discordFooter{Text: "pod-reaper"},
		Timestamp: a.Time.UTC().Format(time.RFC3339),
	}
	if a.Kind == AlertRateLimited {
		e.Description = fmt.Sprintf("%d pods were reaped in the last hour. Reaping resumes once the oldest falls out of the window.", a.MaxPerHr)
		e.Fields = []discordField{{Name: "Next skipped pod", Value: code(a.Namespace + "/" + a.Pod)}}
		return e
	}

	f := a.Finding
	e.Description = code(a.Namespace + "/" + a.Pod)
	e.Fields = []discordField{
		{Name: "Namespace", Value: a.Namespace, Inline: true},
		{Name: "Container", Value: f.Container, Inline: true},
		{Name: "Reason", Value: f.Reason, Inline: true},
		{Name: "Restarts", Value: fmt.Sprint(f.Restarts), Inline: true},
	}
	if f.LastExitCode != nil {
		e.Fields = append(e.Fields, discordField{Name: "Last exit", Value: exitText(f), Inline: true})
		if !f.LastFinishedAt.IsZero() {
			ts := f.LastFinishedAt.Unix()
			e.Fields = append(e.Fields, discordField{Name: "Last crash", Value: fmt.Sprintf("<t:%d:T> (<t:%d:R>)", ts, ts), Inline: true})
		}
	}
	if a.Count24h > 1 {
		e.Fields = append(e.Fields, discordField{
			Name:  "Repeat offender",
			Value: fmt.Sprintf("Reaped **%d×** in the last 24h", a.Count24h),
		})
	}
	return e
}

// slackText renders the alert as Slack mrkdwn.
func (a Alert) slackText(loc *time.Location) string {
	if a.Kind == AlertRateLimited {
		return fmt.Sprintf(":octagonal_sign: *%s*\n%d pods reaped in the last hour; next skipped: `%s/%s`",
			a.title(), a.MaxPerHr, a.Namespace, a.Pod)
	}
	f := a.Finding
	parts := []string{"container `" + f.Container + "`", f.Reason, fmt.Sprintf("restarts %d", f.Restarts)}
	if f.LastExitCode != nil {
		last := "last exit " + exitText(f)
		if !f.LastFinishedAt.IsZero() {
			last += " at " + f.LastFinishedAt.In(loc).Format(timestampLayout)
		}
		parts = append(parts, last)
	}
	s := fmt.Sprintf("*%s*\n`%s/%s`\n%s", a.title(), a.Namespace, a.Pod, strings.Join(parts, " · "))
	if a.Count24h > 1 {
		s += fmt.Sprintf("\n_Reaped %d× in the last 24h_", a.Count24h)
	}
	return s
}

func exitText(f finding) string {
	if f.LastExitCode == nil {
		return ""
	}
	if f.LastTermReason == "" {
		return fmt.Sprint(*f.LastExitCode)
	}
	return fmt.Sprintf("%d (%s)", *f.LastExitCode, f.LastTermReason)
}

func code(s string) string { return "`" + strings.ReplaceAll(s, "`", "'") + "`" }
