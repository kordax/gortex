package main

import "sync/atomic"

// pprofAddr is shared by the status response and the managed diagnostics
// listener. GORTEX_DAEMON_PPROF_ADDR remains a compatibility alias for that
// listener, with bounded profiles and no global DefaultServeMux registration.
var pprofAddr atomic.Value

func init() {
	pprofAddr.Store("")
}

func daemonPProfAddr() string {
	v, _ := pprofAddr.Load().(string)
	return v
}
