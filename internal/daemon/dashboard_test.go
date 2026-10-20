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

type dashboardTestController struct {
	Controller
	call func(context.Context, DashboardParams) (DashboardStatus, error)
}

func (c dashboardTestController) Dashboard(ctx context.Context, p DashboardParams) (DashboardStatus, error) {
	return c.call(ctx, p)
}

func TestDashboardControlUnixFramingAndErrorRecovery(t *testing.T) {
	listener, path := contextTestSocket(t)
	s := New("", "test", zap.NewNop())
	var state DashboardStatus
	s.Controller = dashboardTestController{call: func(_ context.Context, p DashboardParams) (DashboardStatus, error) {
		switch p.Action {
		case "start":
			state = DashboardStatus{Running: true, Address: "127.0.0.1:9474", URL: "http://127.0.0.1:9474"}
		case "stop":
			state = DashboardStatus{}
		case "status":
		default:
			return state, errors.New("invalid action")
		}
		return state, nil
	}}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		s.handle(conn)
	}()
	client, err := DialToContext(t.Context(), path, Handshake{Mode: ModeControl})
	require.NoError(t, err)
	defer func() {
		_ = client.Close()
		select {
		case <-serverDone:
		case <-time.After(time.Second):
			t.Error("control session leaked")
		}
	}()
	for _, action := range []string{"start", "status", "invalid", "stop", "status", "start", "stop"} {
		response, err := client.ControlWithTimeout(ControlDashboard, DashboardParams{Action: action}, time.Second)
		require.NoError(t, err)
		if action == "invalid" {
			assert.False(t, response.OK)
			assert.Contains(t, response.ErrorMsg, "invalid action")
			continue
		}
		require.True(t, response.OK, "%s", response.ErrorMsg)
		var status DashboardStatus
		require.NoError(t, json.Unmarshal(response.Result, &status))
		if action == "start" {
			assert.True(t, status.Running)
		}
		if action == "stop" {
			assert.False(t, status.Running)
		}
	}
	response, err := client.ControlWithTimeout(ControlDashboard, map[string]any{"action": 23}, time.Second)
	require.NoError(t, err)
	assert.False(t, response.OK)
	response, err = client.ControlWithTimeout(ControlDashboard, DashboardParams{Action: "status"}, time.Second)
	require.NoError(t, err)
	assert.True(t, response.OK, "a malformed dashboard request must not poison control framing")
}

func TestDashboardControlDispatch(t *testing.T) {
	s := New("", "test", zap.NewNop())
	for _, action := range []string{"start", "stop", "status"} {
		params := DashboardParams{Action: action}
		if action == "start" {
			params.Address = "127.0.0.1:9474"
		}
		called := false
		s.Controller = dashboardTestController{call: func(ctx context.Context, actual DashboardParams) (DashboardStatus, error) {
			called = true
			assert.Equal(t, params, actual)
			return DashboardStatus{Running: action == "start"}, ctx.Err()
		}}
		payload, err := json.Marshal(params)
		require.NoError(t, err)
		response := s.handleControl(t.Context(), nil, ControlRequest{Kind: ControlDashboard, Params: payload})
		require.True(t, response.OK, "%s", response.ErrorMsg)
		assert.True(t, called)
		var status DashboardStatus
		require.NoError(t, json.Unmarshal(response.Result, &status))
		assert.Equal(t, action == "start", status.Running)
	}
	s.Controller = dashboardTestController{call: func(ctx context.Context, _ DashboardParams) (DashboardStatus, error) {
		return DashboardStatus{}, ctx.Err()
	}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	response := s.handleControl(ctx, nil, ControlRequest{Kind: ControlDashboard, Params: json.RawMessage(`{"action":"start"}`)})
	assert.False(t, response.OK)
	assert.Contains(t, response.ErrorMsg, "context canceled")
	response = s.handleControl(t.Context(), nil, ControlRequest{Kind: ControlDashboard, Params: json.RawMessage(`{"action":`)})
	assert.False(t, response.OK)
	// A legacy controller implements the original control interface but not
	// the optional dashboard extension. It must remain usable and reject
	// this command with an actionable compatibility message.
	s.Controller = struct{ Controller }{}
	response = s.handleControl(t.Context(), nil, ControlRequest{Kind: ControlDashboard, Params: json.RawMessage(`{"action":"status"}`)})
	assert.False(t, response.OK)
	assert.Contains(t, response.ErrorMsg, "update and restart")
}
