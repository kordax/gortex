package daemon

import "context"

// ControlDashboard changes only the dashboard listener, not the daemon lifecycle.
const ControlDashboard = "dashboard"

type DashboardParams struct {
	Action  string `json:"action"`
	Address string `json:"address,omitempty"`
}

type DashboardStatus struct {
	Running bool   `json:"running"`
	Address string `json:"address,omitempty"`
	URL     string `json:"url,omitempty"`
}

// DashboardController is optional so older and control-only controllers remain compatible.
type DashboardController interface {
	Dashboard(context.Context, DashboardParams) (DashboardStatus, error)
}
