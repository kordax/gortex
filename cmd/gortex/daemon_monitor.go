package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/observability"
	"github.com/zzet/gortex/internal/progress"
)

// monitorSnapshot deliberately keeps unavailable status separate from a real
// empty graph. Each JSON line is independently parseable after reconnects.
type monitorSnapshot struct {
	ObservedAt time.Time              `json:"observed_at"`
	Status     *daemon.StatusResponse `json:"status,omitempty"`
	Startup    *daemon.RuntimeState   `json:"startup,omitempty"`
	Error      string                 `json:"error,omitempty"`
}

func newDaemonMonitorCmd() *cobra.Command {
	var interval time.Duration
	var once, plain, jsonOutput, details bool
	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Monitor daemon progress and resources without starting or stopping it",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if interval < 500*time.Millisecond {
				return fmt.Errorf("monitor interval must be at least 500ms")
			}
			if jsonOutput && plain {
				return fmt.Errorf("choose either --json or --plain")
			}
			w := cmd.OutOrStdout()
			if once || jsonOutput || plain || noProgress || !progress.IsTTY(w) {
				return runMonitorStream(cmd.Context(), w, interval, once, jsonOutput, func() monitorSnapshot { return collectMonitorSnapshotContext(cmd.Context()) })
			}
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			model := newStatusTUI(interval)
			model.fetchCancel()
			model.fetchContext = ctx
			model.fetchCancel = cancel
			model.details = details
			_, err := tea.NewProgram(model, tea.WithContext(ctx), tea.WithOutput(w), tea.WithInput(cmd.InOrStdin()), tea.WithAltScreen()).Run()
			return err
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "poll interval (at least 500ms; requests never overlap)")
	cmd.Flags().BoolVar(&once, "once", false, "print one snapshot and exit")
	cmd.Flags().BoolVar(&plain, "plain", false, "print timestamped text snapshots instead of the interactive display")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print newline-delimited JSON snapshots")
	cmd.Flags().BoolVar(&details, "details", false, "expand selected repository details initially")
	return cmd
}

func init() { daemonCmd.AddCommand(newDaemonMonitorCmd()) }

func collectMonitorSnapshotContext(ctx context.Context) monitorSnapshot {
	snapshot := monitorSnapshot{ObservedAt: time.Now().UTC()}
	if startup, ok := daemon.ReadRuntimeState(); ok {
		snapshot.Startup = &startup
	}
	status, err := fetchMonitorStatus(ctx)
	if err != nil {
		snapshot.Error = err.Error()
	} else {
		snapshot.Status = &status
	}
	return snapshot
}

func fetchMonitorStatus(ctx context.Context) (daemon.StatusResponse, error) {
	var status daemon.StatusResponse
	if err := ctx.Err(); err != nil {
		return status, err
	}
	client, err := daemonControlClient()
	if err != nil {
		return status, err
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	response, err := client.ControlWithTimeout(daemon.ControlStatus, daemon.StatusParams{}, daemon.ControlTimeoutFor(daemon.ControlStatus))
	if err != nil {
		return status, err
	}
	if !response.OK {
		return status, fmt.Errorf("status rejected: %s %s", response.ErrorCode, response.ErrorMsg)
	}
	if err := json.Unmarshal(response.Result, &status); err != nil {
		return status, fmt.Errorf("parse status: %w", err)
	}
	return status, nil
}

func runMonitorStream(ctx context.Context, w io.Writer, interval time.Duration, once, jsonOutput bool, fetch func() monitorSnapshot) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		snapshot := fetch()
		if jsonOutput {
			if err := json.NewEncoder(w).Encode(snapshot); err != nil {
				return err
			}
		} else if _, err := io.WriteString(w, renderMonitorSnapshot(snapshot)); err != nil {
			return err
		}
		if once {
			if snapshot.Error != "" {
				return fmt.Errorf("daemon status unavailable: %s", snapshot.Error)
			}
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func renderMonitorSnapshot(snapshot monitorSnapshot) string {
	text := snapshot.ObservedAt.Format(time.RFC3339) + " gortex daemon\n"
	if startup := snapshot.Startup; startup != nil {
		text += fmt.Sprintf("  startup: %s", startup.StartupPhase)
		if startup.MigrationName != "" {
			text += fmt.Sprintf(" migration %d (%s)", startup.MigrationVersion, startup.MigrationName)
		}
		text += "\n"
	}
	if snapshot.Error != "" {
		return text + "  status unavailable; reconnecting: " + snapshot.Error + "\n"
	}
	if snapshot.Status == nil {
		return text + "  status unavailable\n"
	}
	st := snapshot.Status
	state := "warming up; references not ready"
	if st.Ready {
		state = "ready"
	}
	text += fmt.Sprintf("  %s %s pid %d uptime %s\n", versionString(st.Version), state, st.PID, humanizeDur(time.Duration(st.UptimeSeconds)*time.Second))
	text += "  " + monitorResourceSummary(*st) + "\n"
	text += "  " + monitorProgressSummary(*st) + "\n"
	if st.CountsUnknown {
		text += "  repository counts unavailable\n"
	} else if st.AggregateBusy {
		text += "  repository counts cached; aggregate busy\n"
	}
	for _, repo := range st.TrackedRepos {
		if st.CountsUnknown || repo.Unloaded {
			text += fmt.Sprintf("  repo %s: counts unavailable (%s)\n", firstNonEmpty(repo.Prefix, repo.Name, repo.Path), repoItemState(repo))
			continue
		}
		text += fmt.Sprintf("  repo %s: %d indexed files, %d failed, %d unreadable; %s\n", firstNonEmpty(repo.Prefix, repo.Name, repo.Path), repo.Files, repo.FailedFiles, repo.UnreadableFiles, firstNonEmpty(repoItemState(repo), "indexed"))
	}
	return text
}

func monitorProgressSummary(st daemon.StatusResponse) string {
	if st.Enrichment != nil {
		e := st.Enrichment
		text := fmt.Sprintf("enrichment: %d/%d repositories terminal", e.ReposDone, e.ReposTotal)
		if e.Current != nil {
			text += fmt.Sprintf("; %s / %s running %.0fs", e.Current.Repo, e.Current.Provider, e.Current.ElapsedSeconds)
		}
		return text
	}
	if st.EnrichmentComplete {
		return "enrichment complete; live per-file progress unavailable"
	}
	return "enrichment progress unavailable; live per-file progress unavailable"
}

func monitorResourceSummary(st daemon.StatusResponse) string {
	text := fmt.Sprintf("Go heap %s; reserved %s; released %s; GC %d; goroutines %d", humanizeBytes(st.Runtime.Alloc), humanizeBytes(st.Runtime.Sys), humanizeBytes(st.Runtime.HeapReleased), st.Runtime.NumGC, st.Runtime.NumGoroutine)
	if st.TrigramCache != nil {
		cache := st.TrigramCache
		text += fmt.Sprintf("; trigram cache %s / %s (%d/%d live, %d evictions)", humanizeBytes(uint64(max(cache.Bytes, 0))), humanizeBytes(uint64(max(cache.MaxBytes, 0))), cache.Live, cache.MaxLive, cache.Evictions)
	}
	if p := st.Process; p != nil && p.Available {
		text += fmt.Sprintf("; process RSS %s; CPU %.2fs", humanizeBytes(p.RSSBytes), p.CPUSeconds)
		if p.PSSAvailable {
			text += "; PSS " + humanizeBytes(p.PSSBytes)
		}
		if p.IOAvailable {
			text += fmt.Sprintf("; I/O read %s write %s", humanizeBytes(p.ReadBytes), humanizeBytes(p.WriteBytes))
		}
		text += fmt.Sprintf("; %d child processes RSS sum %s (shared pages counted per process)", p.Children, humanizeBytes(p.ChildrenRSSBytes))
		if !p.ChildrenComplete {
			text += "; child sample incomplete"
		}
	} else {
		text += "; process resource sample unavailable"
	}
	return text
}

// CPU percent is per logical CPU and may exceed 100 on a multi-core workload.
// Never subtract counters across PID reuse or report the first sample as zero.
func monitorCPUPercent(previous, current *observability.ProcessSnapshot) *float64 {
	if previous == nil || current == nil || !previous.Available || !current.Available || previous.PID != current.PID || previous.StartTicks != current.StartTicks || current.SampleUnix <= previous.SampleUnix || current.CPUSeconds < previous.CPUSeconds {
		return nil
	}
	percent := 100 * (current.CPUSeconds - previous.CPUSeconds) / float64(current.SampleUnix-previous.SampleUnix)
	return &percent
}
