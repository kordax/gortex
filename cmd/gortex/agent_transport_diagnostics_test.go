package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const agentMetricLimit = 1 << 20

func (r *agentMixedRecorder) watchSlow(ctx context.Context, operation string) func() {
	finished, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(r.slow)
		defer timer.Stop()
		select {
		case <-finished:
		case <-ctx.Done():
		case <-timer.C:
			r.capture(operation)
		}
	}()
	return func() {
		close(finished)
		<-done
	}
}

func agentDiagnosticKind(value string) (string, error) {
	if value == "" {
		return "cpu", nil
	}
	switch value {
	case "cpu", "mutex", "block", "trace":
		return value, nil
	default:
		return "", fmt.Errorf("GORTEX_AGENT_PROFILE_KIND must be cpu, mutex, block or trace")
	}
}

func (r *agentMixedRecorder) diagnosticKind() string {
	if r.profileKind == "" {
		return "cpu"
	}
	return r.profileKind
}

func (r *agentMixedRecorder) saveDiagnostic(filename string) error {
	kind := r.diagnosticKind()
	if kind == "cpu" {
		return r.saveCPU(filename)
	}
	// All expensive profile kinds share the private daemon's capture gate.
	r.cpuMu.Lock()
	defer r.cpuMu.Unlock()
	return agentSaveProfile(r.parent, r.addr, kind+"?seconds=2", filepath.Join(r.root, filename))
}

func agentSaveMetrics(parent context.Context, address, path string) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/metrics", nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("metrics returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, agentMetricLimit+1))
	if err != nil {
		return err
	}
	if len(body) > agentMetricLimit {
		return fmt.Errorf("metrics response exceeds %d bytes", agentMetricLimit)
	}
	if !strings.Contains(string(body), "gortex_") {
		return fmt.Errorf("metrics response has no Gortex samples")
	}
	return os.WriteFile(path, body, 0600)
}

func agentEnrichmentComplete(ctx context.Context, address, path string) bool {
	if err := agentSaveMetrics(ctx, address, path); err != nil {
		return false
	}
	body, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(body), `gortex_daemon_phase{phase="enrichment_complete"} 1`)
}

func (r *agentMixedRecorder) captureMetrics(filename string) {
	if r.metricsAddr == "" {
		return
	}
	if err := agentSaveMetrics(r.parent, r.metricsAddr, filepath.Join(r.root, filename)); err != nil {
		r.mu.Lock()
		r.profileErrs = append(r.profileErrs, fmt.Sprintf("%s: %v", filename, err))
		r.mu.Unlock()
	}
}

// Preserve the first complete response per client/operation. Timings alone
// cannot distinguish real impact analysis from a pending/empty graph reply.
func (r *agentMixedRecorder) recordResponse(client int, operation string, raw json.RawMessage) error {
	key := fmt.Sprintf("response-%s-%d.json", operation, client)
	if _, loaded := r.responses.LoadOrStore(key, true); loaded {
		return nil
	}
	if !json.Valid(raw) || len(raw) > agentMetricLimit {
		return fmt.Errorf("invalid or oversized diagnostic response")
	}
	return os.WriteFile(filepath.Join(r.root, key), raw, 0600)
}

func agentValidateChangeEvidence(raw json.RawMessage, target string) error {
	if err := agentMixedValidate(raw, target); err != nil {
		return err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if !agentJSONField(value, "id", target) {
		return fmt.Errorf("change analysis did not return the known target symbol %q: %s", target, raw)
	}
	payload, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("change analysis has no object payload")
	}
	if structured, ok := payload["structuredContent"].(map[string]any); ok {
		payload = structured
	}
	if blast, ok := payload["blast"].(map[string]any); ok {
		payload = blast
	}
	if affected, ok := payload["total_affected"].(float64); !ok || affected <= 0 {
		return fmt.Errorf("change analysis did not traverse the fixture's known callers: %s", raw)
	}
	return nil
}

func TestAgentChangeEvidenceRejectsPendingAndEchoOnly(t *testing.T) {
	const target = "repo/f.go::Target"
	require.NoError(t, agentValidateChangeEvidence(json.RawMessage(`{"changed_symbols":[{"id":"repo/f.go::Target"}],"total_affected":1}`), target))
	for _, raw := range []string{
		`{"graph_status":"pending","changed_symbols":[],"summary":"repo/f.go::Target"}`,
		`{"changed_files":["repo/f.go::Target"],"changed_symbols":[]}`,
		`{"changed_symbols":[{"id":"other/f.go::Other"}]}`,
		`{"changed_symbols":[{"id":"repo/f.go::Target"}],"total_affected":0}`,
	} {
		require.Error(t, agentValidateChangeEvidence(json.RawMessage(raw), target))
	}
}

func TestAgentEnrichmentReadinessRequiresPublishedPhase(t *testing.T) {
	for _, phase := range []string{"loading", "resolving", "enrichment_complete"} {
		t.Run(phase, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, "gortex_daemon_ready 1\ngortex_daemon_phase{phase=%q} 1\n", phase)
			}))
			defer server.Close()
			ready := agentEnrichmentComplete(t.Context(), strings.TrimPrefix(server.URL, "http://"), filepath.Join(t.TempDir(), "startup.prom"))
			assert.Equal(t, phase == "enrichment_complete", ready)
		})
	}
}

func TestAgentDiagnosticKind(t *testing.T) {
	for _, value := range []string{"", "cpu", "mutex", "block", "trace"} {
		kind, err := agentDiagnosticKind(value)
		require.NoError(t, err)
		assert.NotEmpty(t, kind)
	}
	_, err := agentDiagnosticKind("arbitrary-route")
	require.Error(t, err)
}

func TestAgentMetricsCaptureBoundsAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		valid  bool
	}{
		{"samples", http.StatusOK, "gortex_sqlite_pool_wait_total{pool=\"reader\"} 3\n", true},
		{"http_error", http.StatusServiceUnavailable, "unavailable", false},
		{"not_metrics", http.StatusOK, "not a metrics endpoint", false},
		{"oversized", http.StatusOK, "gortex_" + strings.Repeat("x", agentMetricLimit), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/metrics", r.URL.Path)
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "metrics.prom")
			err := agentSaveMetrics(t.Context(), strings.TrimPrefix(server.URL, "http://"), path)
			if test.valid {
				require.NoError(t, err)
				body, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, test.body, string(body))
			} else {
				require.Error(t, err)
				_, err := os.Stat(path)
				assert.True(t, os.IsNotExist(err))
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, agentSaveMetrics(ctx, "127.0.0.1:1", filepath.Join(t.TempDir(), "cancelled.prom")), context.Canceled)
}
