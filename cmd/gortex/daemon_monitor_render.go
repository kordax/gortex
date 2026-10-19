package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

func monitorRepoItemState(st daemon.StatusResponse, repo daemon.TrackedRepoStatus) string {
	if repo.Missing {
		return repoItemState(repo)
	}
	if st.CountsUnknown {
		return "counts unavailable"
	}
	return repoItemState(repo)
}

func (m statusTUI) renderMonitorProgress() string {
	var lines []string
	if m.err != nil {
		message := "status unavailable; reconnecting: " + m.err.Error()
		if m.hasStatus {
			message += "; displayed status is stale (last success " + m.lastUpdate.Format("15:04:05") + ")"
		}
		lines = append(lines, tuiErrInline.Render(message))
	}
	if m.startup != nil {
		s := m.startup
		text := fmt.Sprintf("startup: %s", s.StartupPhase)
		if s.MigrationName != "" {
			text += fmt.Sprintf("; migration %d: %s", s.MigrationVersion, s.MigrationName)
		}
		if s.StartupUpdatedAt > 0 {
			text += "; heartbeat " + humanizeDur(max(time.Since(time.UnixMilli(s.StartupUpdatedAt)), 0)) + " ago"
		}
		if s.StartupError != "" {
			text += "; " + s.StartupError
		}
		lines = append(lines, tuiVal.Render(text))
	}
	if !m.hasStatus {
		return strings.Join(lines, "\n")
	}
	lines = append(lines, tuiVal.Render(monitorProgressSummary(m.status)))
	lines = append(lines, m.renderMonitorResources()...)
	if m.status.CountsUnknown {
		lines = append(lines, tuiHint.Render("Repository counts unavailable; zeros are not measured counts."))
	} else if m.status.AggregateBusy {
		lines = append(lines, tuiHint.Render("Repository counts cached while aggregate is busy."))
	}
	return strings.Join(lines, "\n")
}

func (m statusTUI) renderMonitorResources() []string {
	st := m.status
	lines := []string{tuiVal.Render(fmt.Sprintf("Go heap %s · reserved %s · released %s · GC %d · %d goroutines", humanizeBytes(st.Runtime.Alloc), humanizeBytes(st.Runtime.Sys), humanizeBytes(st.Runtime.HeapReleased), st.Runtime.NumGC, st.Runtime.NumGoroutine))}
	if p := st.Process; p != nil && p.Available {
		text := "process RSS " + humanizeBytes(p.RSSBytes)
		if p.PSSAvailable {
			text += " · PSS " + humanizeBytes(p.PSSBytes)
		}
		if m.cpuPercent != nil {
			text += fmt.Sprintf(" · CPU %.1f%%", *m.cpuPercent)
		} else {
			text += " · CPU rate unavailable"
		}
		text += fmt.Sprintf(" · CPU total %.2fs", p.CPUSeconds)
		lines = append(lines, tuiVal.Render(text))
		text = fmt.Sprintf("children %d · RSS sum %s (shared pages counted per process)", p.Children, humanizeBytes(p.ChildrenRSSBytes))
		if !p.ChildrenComplete {
			text += " · incomplete"
		}
		lines = append(lines, tuiVal.Render(text))
		if p.IOAvailable {
			lines = append(lines, tuiVal.Render(fmt.Sprintf("process I/O read %s · write %s", humanizeBytes(p.ReadBytes), humanizeBytes(p.WriteBytes))))
		}
	} else {
		lines = append(lines, tuiHint.Render("Process resource sample unavailable."))
	}
	if c := st.TrigramCache; c != nil {
		lines = append(lines, tuiVal.Render(fmt.Sprintf("trigram cache %s / %s · %d/%d live repos · %d evictions", humanizeBytes(uint64(max(c.Bytes, 0))), humanizeBytes(uint64(max(c.MaxBytes, 0))), c.Live, c.MaxLive, c.Evictions)))
	}
	return lines
}

func (m statusTUI) renderSelectedRepo() string {
	selected, ok := m.repos.SelectedItem().(repoItem)
	if !ok {
		return tuiHint.Render("No repository selected; details unavailable.")
	}
	for _, repo := range m.status.TrackedRepos {
		if repo.Path != selected.path || firstNonEmpty(repo.Prefix, repo.Name) != selected.name {
			continue
		}
		lines := []string{tuiHeading.Render("selected repository: " + selected.name), tuiVal.Render("  " + repo.Path)}
		if m.status.CountsUnknown || repo.Unloaded {
			lines = append(lines, tuiHint.Render("  File and graph counts unavailable."))
		} else {
			lines = append(lines, tuiVal.Render(fmt.Sprintf("  %d indexed files; %d nodes; %d edges", repo.Files, repo.Nodes, repo.Edges)))
			lines = append(lines, tuiVal.Render(fmt.Sprintf("  %d failed files; %d unreadable", repo.FailedFiles, repo.UnreadableFiles)))
		}
		if repo.IndexHealthError != "" {
			lines = append(lines, tuiErrInline.Render("  Index health unavailable: "+repo.IndexHealthError))
		}
		lines = append(lines, tuiHint.Render("  Live file names, pending totals and per-file progress unavailable."))
		return strings.Join(lines, "\n")
	}
	return tuiHint.Render("Repository no longer present in latest status.")
}
