package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func init() {
	daemonCmd.AddCommand(newDaemonProfileCommand())
}

func newDaemonProfileCommand() *cobra.Command {
	var address, kind, output string
	var duration time.Duration
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Save a bounded profile from the daemon's local diagnostics listener",
		Long:  "Save a private Go profile without stopping the daemon. Requires daemon start --diagnostics-addr or GORTEX_DIAGNOSTICS_ADDR. CPU captures are limited to 60s; trace, block and mutex captures to 30s. Block/mutex results are cumulative process profiles, with sampling enabled only during the capture.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			allowed := map[string]bool{"cpu": true, "heap": true, "allocs": true, "goroutine": true, "threadcreate": true, "trace": true, "block": true, "mutex": true}
			if !allowed[kind] {
				return fmt.Errorf("unsupported profile kind %q", kind)
			}
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("diagnostics address must be host:port")
			}
			if host == "localhost" {
				host = "127.0.0.1"
			}
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				return fmt.Errorf("diagnostics address must use a loopback IP or localhost")
			}
			seconds := int(duration / time.Second)
			if duration != time.Duration(seconds)*time.Second || seconds < 1 || seconds > 60 || (seconds > 30 && (kind == "trace" || kind == "block" || kind == "mutex")) {
				return fmt.Errorf("duration must be whole seconds in range 1s..60s (up to 30s for trace, block or mutex)")
			}
			url := "http://" + net.JoinHostPort(host, port) + "/debug/pprof/" + kind
			if kind == "cpu" || kind == "trace" || kind == "block" || kind == "mutex" {
				url += "?seconds=" + strconv.Itoa(seconds)
			}
			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext, ResponseHeaderTimeout: duration + 5*time.Second}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: duration + 10*time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			fmt.Fprintf(cmd.ErrOrStderr(), "Capturing %s from %s...\n", kind, address)
			response, err := client.Do(req)
			if err != nil {
				return fmt.Errorf("profile request failed; enable the daemon diagnostics listener: %w", err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusOK {
				return fmt.Errorf("profile endpoint returned HTTP %d", response.StatusCode)
			}
			if output == "" {
				extension := ".pprof"
				if kind == "trace" {
					extension = ".trace"
				}
				output = "gortex-" + kind + "-" + time.Now().UTC().Format("20060102T150405Z") + extension
			}
			file, err := os.OpenFile(filepath.Clean(output), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return fmt.Errorf("create profile (existing files are preserved): %w", err)
			}
			complete := false
			defer func() {
				_ = file.Close()
				if !complete {
					_ = os.Remove(output)
				}
			}()
			written, err := io.Copy(file, io.LimitReader(response.Body, (32<<20)+1))
			if err != nil {
				return fmt.Errorf("save profile: %w", err)
			}
			if written == 0 || written > 32<<20 {
				return fmt.Errorf("profile is empty or exceeds 32 MiB")
			}
			if err := file.Close(); err != nil {
				return err
			}
			complete = true
			fmt.Fprintf(cmd.OutOrStdout(), "Saved %s (%d bytes, mode 0600)\n", output, written)
			if kind == "trace" {
				fmt.Fprintf(cmd.OutOrStdout(), "Inspect: go tool trace %s\n", strconv.Quote(output))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Inspect: go tool pprof %s\n", strconv.Quote(output))
			}
			return nil
		},
	}
	addressDefault := strings.TrimSpace(os.Getenv("GORTEX_DIAGNOSTICS_ADDR"))
	if addressDefault == "" {
		addressDefault = "127.0.0.1:9475"
	}
	cmd.Flags().StringVar(&address, "address", addressDefault, "daemon loopback diagnostics address")
	cmd.Flags().StringVar(&kind, "kind", "cpu", "cpu|heap|allocs|goroutine|threadcreate|trace|block|mutex")
	cmd.Flags().DurationVar(&duration, "duration", 10*time.Second, "capture duration (whole seconds; not used for snapshots)")
	cmd.Flags().StringVar(&output, "out", "", "new output file, written with mode 0600")
	return cmd
}
