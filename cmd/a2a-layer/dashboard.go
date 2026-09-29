package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/iluxav/a2a-layer/internal/dashboard"
	"github.com/iluxav/a2a-layer/internal/server"
)

// runDashboard serves the web UI that edits a config file and tries its agents:
//
//	a2a-layer dashboard -config agents.yaml -listen 127.0.0.1:7301
func runDashboard(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("dashboard", flag.ExitOnError)
	cfgPath := fs.String("config", "agents.yaml", "the YAML file to edit (created on the first change if missing)")
	listen := fs.String("listen", "127.0.0.1:7301", "where to serve the dashboard")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: a2a-layer dashboard [-config agents.yaml] [-listen 127.0.0.1:7301]\n\n"+
			"Serves a web UI to add, edit and remove the config's agents, change its settings, and a playground\n"+
			"that runs them. It has no login: keep it on a loopback address.\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	path, err := filepath.Abs(*cfgPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Printf("config: %s (new: the first change creates it)\n", path)
	} else {
		fmt.Printf("config: %s\n", path)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(*listen)
	loopback := host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
	if !loopback {
		log.Warn("the dashboard has no login and can change the config and run agents; it is reachable beyond this machine", "listen", *listen)
	}
	d, err := dashboard.New(dashboard.Options{
		Path:    path,
		BaseURL: server.LoopbackURL(ln.Addr().String()),
		AnyHost: !loopback,
		Log:     log,
	})
	if err != nil {
		ln.Close()
		return err
	}
	httpSrv := &http.Server{Handler: d.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	fmt.Printf("dashboard: http://%s\n", ln.Addr())
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	d.Shutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
