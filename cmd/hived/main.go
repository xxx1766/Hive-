package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/anne-x/hive/internal/daemon"
	"github.com/anne-x/hive/internal/ipc"
	"github.com/anne-x/hive/internal/ns"
	"github.com/anne-x/hive/internal/version"
)

func main() {
	// If we were re-execed as the namespace init helper, do that job and
	// never return (see internal/ns).
	if ns.IsInitMode() {
		ns.RunInit()
		return
	}

	var (
		showVersion = flag.Bool("version", false, "print version and exit")
		socket      = flag.String("socket", "", "override socket path (default: $HIVE_SOCKET or XDG/home fallback)")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("hived %s\n", version.Version)
		return
	}

	sockPath := ipc.SocketPath()
	if *socket != "" {
		sockPath = *socket
	}

	if err := os.MkdirAll(ipc.StateRoot(), 0o750); err != nil {
		log.Fatalf("mkdir state root: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("hived: received signal %s, shutting down", sig)
		cancel()
	}()

	srv := ipc.NewServer(sockPath)
	registerMetaHandlers(srv)

	d, err := daemon.New()
	if err != nil {
		log.Fatalf("daemon init: %v", err)
	}
	d.Register(srv)
	defer d.Shutdown()

	if err := srv.Listen(); err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer srv.Close()

	log.Printf("hived %s listening on %s", version.Version, sockPath)

	if err := srv.Serve(ctx); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func registerMetaHandlers(srv *ipc.Server) {
	srv.Handle(ipc.MethodDaemonPing, func(ctx context.Context, params json.RawMessage, notify ipc.NotifyFunc) (any, error) {
		return ipc.PingResult{OK: true}, nil
	})
	srv.Handle(ipc.MethodDaemonVersion, func(ctx context.Context, params json.RawMessage, notify ipc.NotifyFunc) (any, error) {
		return ipc.VersionResult{Version: version.Version}, nil
	})
}
