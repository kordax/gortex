package daemon

import "time"

// sessionStatusSnapshot describes registered sessions at one point in time.
// A transport-managed session can be active without a daemon-owned socket.
type sessionStatusSnapshot struct {
	total     int
	socket    int
	retained  int
	transport int
	rows      []MCPSessionStatus
}

func (r *SessionRegistry) statusSnapshot(includeRows bool) sessionStatusSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := sessionStatusSnapshot{total: len(r.sessions)}
	if includeRows {
		out.rows = make([]MCPSessionStatus, 0, out.total)
	}
	now := time.Now()
	for _, sess := range r.sessions {
		state := "transport"
		switch {
		case sess.Conn != nil:
			state = "socket"
			out.socket++
		case sess.LogicalSessionID != "":
			state = "retained"
			out.retained++
		default:
			out.transport++
		}
		if !includeRows {
			continue
		}
		name, version := sess.SnapshotClientInfo()
		row := MCPSessionStatus{
			ID: sess.ID, Cwd: sess.CWD, ClientName: name,
			ClientVersion: version, ConnectionState: state,
		}
		if !sess.StartedAt.IsZero() {
			row.ConnectedSecs = int64(now.Sub(sess.StartedAt).Seconds())
		}
		out.rows = append(out.rows, row)
	}
	return out
}
