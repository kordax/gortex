package daemon

import "context"

// ControlProfiler changes the opt-in diagnostics listener on a running daemon.
const ControlProfiler = "profiler"

type ProfilerParams struct {
	Action  string `json:"action"`
	Address string `json:"address,omitempty"`
}

type ProfilerStatus struct {
	Running  bool   `json:"running"`
	Stopping bool   `json:"stopping,omitempty"`
	Address  string `json:"address,omitempty"`
}

// ProfilerController is optional for compatibility with control-only controllers.
type ProfilerController interface {
	Profiler(context.Context, ProfilerParams) (ProfilerStatus, error)
}
