// Command a2a-layer serves command-line agents (Claude Code, and other CLIs through the runner
// package) as A2A agents. One process, one port; each agent in the YAML config is its own
// endpoint at /<name>.
//
//	a2a-layer -config agents.yaml
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/iluxav/a2a-layer/internal/config"
	"github.com/iluxav/a2a-layer/internal/runner"
	"github.com/iluxav/a2a-layer/internal/server"
)

var version = "dev"

func main() {
	cfgPath := flag.String("config", "agents.yaml", "the YAML file defining the agents")
	check := flag.Bool("check", false, "load and validate the config, print the agents, and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(*cfgPath, *check, log); err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}

func run(cfgPath string, check bool, log *slog.Logger) error {
	if abs, err := filepath.Abs(cfgPath); err == nil {
		cfgPath = abs
	}
	fmt.Printf("config: %s\n", cfgPath)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	runners := map[string]runner.Runner{}
	for name, rc := range cfg.Runners {
		r, err := runner.New(rc.Type, runner.Options{Command: rc.Command, Args: rc.Args, Env: rc.Env})
		if err != nil {
			return fmt.Errorf("runner %s: %w", name, err)
		}
		runners[name] = r
		bin := rc.Command
		if bin == "" {
			bin = rc.Type
		}
		if _, err := exec.LookPath(bin); err != nil {
			log.Warn("runner command not found on PATH; tasks on it will fail", "runner", name, "command", bin)
		}
	}
	srv, err := server.New(cfg, runners, log)
	if err != nil {
		return err
	}
	for _, name := range cfg.AgentNames() {
		a := cfg.Agents[name]
		fmt.Printf("  %-12s %s/%s  (%s, %s)\n", name, cfg.PublicURL, name, a.Runner, orDefault(a.Model, "default model"))
	}
	if check {
		return nil
	}

	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	log.Info("serving", "listen", cfg.Listen, "agents", len(cfg.Agents))
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	srv.Shutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
