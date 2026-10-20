package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/zzet/gortex/internal/daemon"
)

func init() {
	daemonCmd.AddCommand(newDaemonDashboardCommand(callDaemonDashboard))
}

func newDaemonDashboardCommand(call func(context.Context, daemon.DashboardParams) (daemon.DashboardStatus, error)) *cobra.Command {
	var address string
	var asJSON bool
	cmd := &cobra.Command{
		Use: "dashboard", Short: "Start, stop or inspect the running daemon's local dashboard",
		Long: "Control the dashboard and Prometheus listener without restarting the daemon. Metric collection and daily history continue when it is stopped. Commands change runtime state only; daemon.metrics_addr controls startup.",
	}
	cmd.PersistentFlags().BoolVar(&asJSON, "json", false, "print dashboard state as JSON")
	for _, action := range []string{"start", "stop", "status"} {
		sub := &cobra.Command{
			Use: action, Short: action + " the daemon dashboard", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				params := daemon.DashboardParams{Action: action}
				if action == "start" {
					params.Address = address
				}
				status, err := call(cmd.Context(), params)
				if err != nil {
					return err
				}
				if asJSON {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
				}
				if status.Running {
					fmt.Fprintln(cmd.OutOrStdout(), "Dashboard running:", status.URL)
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "Dashboard stopped; metric collection and history continue.")
				}
				return nil
			},
		}
		if action == "start" {
			sub.Flags().StringVar(&address, "address", "", "loopback listen address (startup address, or 127.0.0.1:9474)")
		}
		cmd.AddCommand(sub)
	}
	return cmd
}

func callDaemonDashboard(ctx context.Context, params daemon.DashboardParams) (daemon.DashboardStatus, error) {
	if err := ctx.Err(); err != nil {
		return daemon.DashboardStatus{}, err
	}
	client, err := daemon.DialContext(ctx, daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli-dashboard"})
	if err != nil {
		return daemon.DashboardStatus{}, fmt.Errorf("connect to running daemon (start it with gortex daemon start): %w", err)
	}
	defer client.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopClose()
	timeout := 5 * time.Second
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < timeout {
		timeout = time.Until(deadline)
		if timeout <= 0 {
			return daemon.DashboardStatus{}, context.DeadlineExceeded
		}
	}
	response, err := client.ControlWithTimeout(daemon.ControlDashboard, params, timeout)
	if ctx.Err() != nil {
		return daemon.DashboardStatus{}, ctx.Err()
	}
	if err != nil {
		return daemon.DashboardStatus{}, err
	}
	if !response.OK {
		return daemon.DashboardStatus{}, fmt.Errorf("daemon dashboard: %s", response.ErrorMsg)
	}
	var status daemon.DashboardStatus
	if err := json.Unmarshal(response.Result, &status); err != nil {
		return daemon.DashboardStatus{}, fmt.Errorf("decode dashboard state: %w", err)
	}
	return status, nil
}
