package main

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// httpDefaultAddr mirrors internal/httpapi.DefaultAddr — duplicated here
// instead of imported so the `hive` CLI binary doesn't pull in the
// embedded SPA HTML (which would balloon the binary by ~the size of
// internal/httpapi/ui/index.html). Keep these two values in sync.
const httpDefaultAddr = "127.0.0.1:8910"

// httpAddrForDisplay resolves the address hived will serve the UI on,
// then rewrites it for browser display (no `:8910` shorthand, no
// `0.0.0.0`). Returns "" when HTTP is explicitly disabled.
func httpAddrForDisplay() string {
	addr := os.Getenv("HIVE_HTTP_ADDR")
	if addr == "" {
		// Empty string + the _DISABLED override means "really off"; otherwise
		// fall through to the daemon's default.
		if os.Getenv("HIVE_HTTP_ADDR_DISABLED") != "" {
			return ""
		}
		addr = httpDefaultAddr
	}
	// Browser-facing rewrite: ":8910" or "0.0.0.0:8910" → 127.0.0.1:8910.
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	if strings.HasPrefix(addr, "0.0.0.0:") {
		addr = "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
	}
	return addr
}

// cmdUI prints the UI URL plus an SSH tunnel hint when the user is on
// a remote host. Runs offline; never dials the daemon.
func cmdUI(_ context.Context, args []string) {
	if maybeHandleHelpFlag("ui", args) {
		return
	}
	addr := httpAddrForDisplay()
	if addr == "" {
		fmt.Println(`hive: HTTP UI is disabled (HIVE_HTTP_ADDR="").`)
		fmt.Println(`      Unset HIVE_HTTP_ADDR (and HIVE_HTTP_ADDR_DISABLED) to enable.`)
		return
	}
	url := "http://" + addr
	fmt.Printf("hive: UI at %s\n", url)
	if os.Getenv("SSH_CONNECTION") == "" {
		return
	}
	target := sshTunnelTarget()
	port := portFromAddr(addr)
	fmt.Println("      You're on a remote host. From your local machine:")
	fmt.Printf("        ssh -L %s:localhost:%s %s\n", port, port, target)
	fmt.Printf("      Then open %s in your browser.\n", url)
}

// sshTunnelTarget returns "<user>@<host>" if both are resolvable, or
// "<host>" / "<this-host>" as fallbacks.
func sshTunnelTarget() string {
	user := os.Getenv("USER")
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "<this-host>"
	}
	if user == "" {
		return host
	}
	return user + "@" + host
}

// portFromAddr extracts the port from "host:port"; falls back to "8910"
// if the address doesn't have one (defensive — httpAddrForDisplay
// always returns host:port today).
func portFromAddr(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 && i < len(addr)-1 {
		return addr[i+1:]
	}
	return "8910"
}
