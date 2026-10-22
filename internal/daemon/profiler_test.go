package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type profilerTestController struct {
	Controller
	call func(context.Context, ProfilerParams) (ProfilerStatus, error)
}

func (c profilerTestController) Profiler(ctx context.Context, p ProfilerParams) (ProfilerStatus, error) {
	return c.call(ctx, p)
}

func TestProfilerControlFramingAndRecovery(t *testing.T) {
	listener, path := contextTestSocket(t)
	s := New("", "test", zap.NewNop())
	var state ProfilerStatus
	s.Controller = profilerTestController{call: func(ctx context.Context, p ProfilerParams) (ProfilerStatus, error) {
		if err := ctx.Err(); err != nil {
			return state, err
		}
		switch p.Action {
		case "start":
			state = ProfilerStatus{Running: true, Address: p.Address}
		case "stop":
			state = ProfilerStatus{}
		case "status":
		default:
			return state, errors.New("invalid profiler action")
		}
		return state, nil
	}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err == nil {
			s.handle(conn)
		}
	}()
	client, err := DialToContext(t.Context(), path, Handshake{Mode: ModeControl})
	require.NoError(t, err)
	defer func() {
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("profiler control session leaked")
		}
	}()
	for _, action := range []string{"status", "start", "invalid", "stop", "status", "start", "stop"} {
		response, err := client.ControlWithTimeout(ControlProfiler, ProfilerParams{Action: action, Address: "127.0.0.1:0"}, time.Second)
		require.NoError(t, err)
		if action == "invalid" {
			assert.False(t, response.OK)
			continue
		}
		require.True(t, response.OK, "%s", response.ErrorMsg)
		var status ProfilerStatus
		require.NoError(t, json.Unmarshal(response.Result, &status))
		assert.Equal(t, action == "start", status.Running)
	}
	response, err := client.ControlWithTimeout(ControlProfiler, map[string]any{"action": 42}, time.Second)
	require.NoError(t, err)
	assert.False(t, response.OK)
	response, err = client.ControlWithTimeout(ControlProfiler, ProfilerParams{Action: "status"}, time.Second)
	require.NoError(t, err)
	assert.True(t, response.OK)
}

func TestProfilerControlLegacyControllerAndCancellation(t *testing.T) {
	s := New("", "test", zap.NewNop())
	s.Controller = struct{ Controller }{}
	request := ControlRequest{Kind: ControlProfiler, Params: json.RawMessage(`{"action":"status"}`)}
	response := s.handleControl(t.Context(), nil, request)
	assert.False(t, response.OK)
	assert.Contains(t, response.ErrorMsg, "update and restart")
	s.Controller = profilerTestController{call: func(ctx context.Context, _ ProfilerParams) (ProfilerStatus, error) {
		return ProfilerStatus{}, ctx.Err()
	}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	response = s.handleControl(ctx, nil, request)
	assert.False(t, response.OK)
	assert.Contains(t, response.ErrorMsg, "context canceled")
}
