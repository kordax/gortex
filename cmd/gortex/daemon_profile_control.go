package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/zzet/gortex/internal/daemon"
)

func addDaemonProfileControls(cmd *cobra.Command, call func(context.Context, daemon.ProfilerParams) (daemon.ProfilerStatus, error)) {
	for _, action := range []string{"start", "stop", "status"} {
		var address string
		var asJSON bool
		sub := &cobra.Command{
			Use: action, Short: action + " the running daemon's profiler listener", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				status, err := call(cmd.Context(), daemon.ProfilerParams{Action: action, Address: address})
				if err != nil {
					return err
				}
				if asJSON {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
				}
				if status.Stopping {
					fmt.Fprintln(cmd.OutOrStdout(), "Profiler is stopping; capture cleanup is finishing.")
				} else if status.Running {
					fmt.Fprintln(cmd.OutOrStdout(), "Profiler listener:", status.Address)
					fmt.Fprintln(cmd.OutOrStdout(), "Sampling is enabled only during a capture. Capture with: gortex daemon profile --address", status.Address, "--kind cpu --duration 10s")
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "Profiler stopped; no diagnostics port is open.")
				}
				return nil
			},
		}
		sub.Flags().BoolVar(&asJSON, "json", false, "print profiler state as JSON")
		if action == "start" {
			sub.Flags().StringVar(&address, "address", "", "loopback listen address (startup address, or 127.0.0.1:9475)")
		}
		cmd.AddCommand(sub)
	}
}

func callDaemonProfiler(ctx context.Context, params daemon.ProfilerParams) (daemon.ProfilerStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client, err := daemon.DialContext(ctx, daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli-profiler"})
	if err != nil {
		return daemon.ProfilerStatus{}, fmt.Errorf("connect to running daemon: %w", err)
	}
	defer client.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopClose()
	response, err := client.ControlWithTimeout(daemon.ControlProfiler, params, 5*time.Second)
	if ctx.Err() != nil {
		return daemon.ProfilerStatus{}, ctx.Err()
	}
	if err != nil {
		return daemon.ProfilerStatus{}, err
	}
	if !response.OK {
		return daemon.ProfilerStatus{}, fmt.Errorf("daemon profiler: %s", response.ErrorMsg)
	}
	var status daemon.ProfilerStatus
	if err := json.Unmarshal(response.Result, &status); err != nil {
		return daemon.ProfilerStatus{}, fmt.Errorf("decode profiler state: %w", err)
	}
	return status, nil
}
