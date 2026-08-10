// Command agent-web-manager serves a local web UI for creating Docker
// Sandboxes (sbx) sandboxes and running coding-agent and shell terminal
// sessions inside them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Shonz1/agent-web-manager/internal/git"
	"github.com/Shonz1/agent-web-manager/internal/manager"
	"github.com/Shonz1/agent-web-manager/internal/notify"
	"github.com/Shonz1/agent-web-manager/internal/sbx"
	"github.com/Shonz1/agent-web-manager/internal/web"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Fatalf("agent-web-manager: %v", err)
	}
}

func run() error {
	var (
		addr     = flag.String("addr", "127.0.0.1:7788", "address to listen on")
		sbxBin   = flag.String("sbx", "sbx", "path to the sbx binary")
		gitBin   = flag.String("git", "git", "path to the git binary, used to read a workspace's changes")
		stateDir = flag.String("state-dir", defaultStateDir(), "directory for persisted sandbox state")
		kitsDir  = flag.String("kits-dir", sbx.DefaultKitsDir(), "directory of sbx kits a session can be started with")
		showVer  = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("agent-web-manager", version)
		return nil
	}

	client := sbx.New(*sbxBin)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err := client.Available(ctx)
	cancel()
	if err != nil {
		// Not fatal: the UI surfaces this so the user can fix their install
		// without the server refusing to start.
		log.Printf("warning: sbx is not usable yet: %v", err)
	}

	mgr, err := manager.New(client, *stateDir, manager.WithKitsDir(*kitsDir))
	if err != nil {
		return err
	}

	notifier, stopNotify, err := startNotify(mgr, *stateDir)
	if err != nil {
		return err
	}
	defer stopNotify()

	// Every project needs the base sandbox its sessions are cloned from. In
	// the background: there may be an image to pull for each, and the UI is
	// worth serving while that happens — a session started in the meantime
	// waits for the same create rather than starting one of its own.
	go ensureBaseSandboxes(mgr)

	webSrv := web.NewServer(mgr, client, notifier, git.New(*gitBin), web.StaticFS())
	srv := &http.Server{
		Addr:              *addr,
		Handler:           webSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: terminal WebSockets are long-lived.
	}
	// Shutdown waits for handlers to return without cancelling what they are
	// watching, and every open tab holds an event stream that would otherwise
	// wait for the browser to go first.
	srv.RegisterOnShutdown(webSrv.Shutdown)

	errCh := make(chan error, 1)
	go func() {
		log.Printf("agent-web-manager %s listening on http://%s", version, *addr)
		log.Printf("sandbox state: %s", *stateDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case <-stop:
		log.Println("shutting down; sandboxes are left running and can be resumed")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	mgr.Shutdown()
	return nil
}

// ensureBaseSandboxes makes the base sandbox of any project that has none —
// a project created while sbx was not working, or one from before base
// sandboxes existed. A project whose folder or agent image is no longer there
// is reported and skipped: the rest still get theirs, and the next session
// started in that one reports the failure to somebody who can act on it.
func ensureBaseSandboxes(mgr *manager.Manager) {
	ctx, cancel := context.WithTimeout(context.Background(), baseSweepTimeout)
	defer cancel()
	for id, err := range mgr.EnsureBaseSandboxes(ctx) {
		log.Printf("warning: project %s has no base sandbox: %v", id, err)
	}
}

// baseSweepTimeout bounds that whole sweep. Each sandbox in it may have an
// image to pull, so it is generous — but it does end, rather than leaving a
// goroutine pulling images into a process that is shutting down.
const baseSweepTimeout = 30 * time.Minute

// startNotify begins relaying the manager's events to Telegram. The relay runs
// whether or not a bot is configured yet, because the settings page can
// configure one while this is running. The returned function stops it.
//
// A bot that cannot be reached is a warning rather than a failure, the same as
// an sbx that is not usable yet: the manager's own job does not depend on it,
// and refusing to start would be a poor trade for a notifier.
func startNotify(mgr *manager.Manager, stateDir string) (*notify.Service, func(), error) {
	notifier, err := notify.NewService(stateDir)
	if err != nil {
		return nil, nil, err
	}

	if notifier.Settings().Enabled {
		checkCtx, cancelCheck := context.WithTimeout(context.Background(), 15*time.Second)
		who, err := notifier.Verify(checkCtx)
		cancelCheck()
		if err != nil {
			log.Printf("warning: telegram notifications are configured but not working: %v", err)
		} else {
			log.Printf("telegram notifications: sending as @%s", who)
		}
	}

	events, unsubscribe := mgr.Events()
	ctx, cancel := context.WithCancel(context.Background())
	go notifier.Relay(ctx, events)

	return notifier, func() {
		cancel()
		unsubscribe()
	}, nil
}

func defaultStateDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "agent-web-manager")
	}
	return ".agent-web-manager"
}
