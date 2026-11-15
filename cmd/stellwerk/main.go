// stellwerk receives webhooks, routes them by rules and delivers them to
// targets such as end-to-end encrypted Matrix rooms.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/johnnycube/stellwerk/internal/adapters/in/source"
	"github.com/johnnycube/stellwerk/internal/adapters/out/target"
	"github.com/johnnycube/stellwerk/internal/bootstrap"
	"github.com/johnnycube/stellwerk/internal/config"

	// Target types register themselves, target/all lists them.
	_ "github.com/johnnycube/stellwerk/internal/adapters/engine/all"
	_ "github.com/johnnycube/stellwerk/internal/adapters/out/target/all"
)

var version = "dev"

type flags struct {
	file, dirs, crossSign           string
	check, printConfig, debug, test bool
	watch                           time.Duration
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "send":
			exit(runSend(os.Args[2:], os.Stdin, os.Stdout, os.Environ()))
			return
		case "init":
			exit(runInit(os.Args[2:], os.Stdin, os.Stdout))
			return
		}
	}
	var f flags
	flag.StringVar(&f.file, "config", "", "YAML config file (default $STELLWERK_CONFIG or stellwerk.yaml, optional)")
	flag.StringVar(&f.dirs, "config-dir", "", "comma-separated config directories, e.g. mounted Secrets (default $STELLWERK_CONFIG_DIR)")
	flag.BoolVar(&f.check, "check", false, "validate config, rules, templates and CEL, then exit")
	flag.BoolVar(&f.test, "test", false, "run the tests: section of the config against the rules, exit 1 if one fails")
	flag.BoolVar(&f.printConfig, "print-config", false, "print the merged config with secrets redacted, then exit")
	flag.StringVar(&f.crossSign, "bootstrap-cross-signing", "", "create a cross-signing identity for a Matrix account (target/account), print the recovery key and exit")
	flag.BoolVar(&f.debug, "debug", false, "debug logging, overrides log.level")
	flag.DurationVar(&f.watch, "watch", envDuration("STELLWERK_WATCH"), "check the config files every interval and reload on change, e.g. 30s (default $STELLWERK_WATCH, off)")
	showVersion := flag.Bool("version", false, "print version and exit")
	showSchema := flag.Bool("schema", false, "print the JSON Schema of the config file and exit")
	flag.Parse()

	if *showSchema {
		b, err := schema()
		if err != nil {
			exit(err)
		}
		fmt.Println(string(b))
		return
	}
	if *showVersion {
		fmt.Printf("stellwerk %s\nsource kinds: %v\ntarget types: %v\nha support: %v\n", version, source.Kinds(), target.Types(), bootstrap.HAAvailable())
		return
	}
	exit(run(f, os.Stdout))
}

func exit(err error) {
	if err == nil {
		return
	}
	if err != errTestsFailed {
		slog.New(slog.NewTextHandler(os.Stderr, nil)).Error("fatal", "err", err)
	}
	os.Exit(1)
}

var errTestsFailed = errors.New("tests failed")

func run(f flags, stdout io.Writer) error {
	var dirs []string
	for _, d := range strings.Split(f.dirs, ",") {
		if d = strings.TrimSpace(d); d != "" {
			dirs = append(dirs, d)
		}
	}
	opts := config.OptionsFromEnv(f.file, dirs, os.Environ())

	if f.printConfig {
		out, err := config.Redacted(opts)
		if err != nil {
			return err
		}
		fmt.Fprint(stdout, out)
		return nil
	}

	cfg, warnings, err := config.LoadFrom(opts)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Log, f.debug).With("node", cfg.NodeID)
	for _, w := range warnings {
		log.Warn("config: " + w)
	}

	a, err := bootstrap.Build(cfg, log)
	if err != nil {
		return err
	}
	a.Version = version
	if f.test {
		if a.RunTests(stdout) > 0 {
			return errTestsFailed
		}
		return nil
	}
	if f.check {
		accounts := 0
		for _, t := range cfg.Targets {
			accounts += len(t.AccountNames())
		}
		fmt.Fprintf(stdout, "config ok: %d sources, %d targets with %d accounts, %d destinations, %d routes, %d heartbeats, ha: %v\n",
			len(cfg.Sources), len(cfg.Targets), accounts, len(cfg.Destinations), len(cfg.Routes), len(cfg.Heartbeats), cfg.HA.Enabled)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := a.Connect(ctx); err != nil {
		return err
	}
	if f.crossSign != "" {
		key, err := a.BootstrapCrossSigning(ctx, f.crossSign)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Recovery key for %s (store it, set recovery_key so every node cross-signs itself):\n%s\n", f.crossSign, key)
		return nil
	}
	a.Load = func() (*config.Config, error) {
		cfg, warnings, err := config.LoadFrom(config.OptionsFromEnv(f.file, dirs, os.Environ()))
		for _, w := range warnings {
			log.Warn("config: " + w)
		}
		return cfg, err
	}
	go reloadOnSignal(ctx, a, log)
	if f.watch > 0 {
		go watchConfig(ctx, a, opts, f.watch, log)
	}
	log.Info("starting", "version", version)
	return a.Run(ctx)
}

// reloadOnSignal reloads the config on SIGHUP.
func reloadOnSignal(ctx context.Context, a *bootstrap.App, log *slog.Logger) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			if err := a.ReloadFromSource(); err != nil {
				log.Error("reload failed", "err", err)
			}
		}
	}
}

// watchConfig reloads when the config files change, for ConfigMaps and
// Secrets that Kubernetes updates in place.
func watchConfig(ctx context.Context, a *bootstrap.App, opts config.LoadOptions, every time.Duration, log *slog.Logger) {
	last := config.Fingerprint(opts)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if fp := config.Fingerprint(opts); fp != last {
				last = fp
				log.Info("config files changed, reloading")
				if err := a.ReloadFromSource(); err != nil {
					log.Error("reload failed", "err", err)
				}
			}
		}
	}
}

func envDuration(name string) time.Duration {
	d, _ := time.ParseDuration(os.Getenv(name))
	return d
}

func newLogger(c config.Log, debug bool) *slog.Logger {
	level := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}[c.Level]
	if debug {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func schema() ([]byte, error) { return config.Schema(source.Kinds(), target.Types()) }
