// Command linchange is the linchange mail and groupware server.
//
// It runs the IMAP, POP3 and SMTP listeners, the HTTP API and web interface,
// and the Microsoft compatibility endpoints from a single process, configured
// entirely by a YAML file given on the command line:
//
//	linchange --config /etc/linchange/linchange.yaml
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/app"
	"github.com/linchange/linchange/internal/logging"
)

const version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		// Startup failures are reported on stderr, not through the logger,
		// because the logger may be what failed to initialise.
		fmt.Fprintf(os.Stderr, "linchange: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("linchange", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `linchange %s - mail and groupware server

Usage:
  linchange --config <path.yaml>

Flags:
`, version)
		fs.PrintDefaults()
	}

	var (
		configPath  = fs.String("config", "", "path to the YAML configuration file (required)")
		showVersion = fs.Bool("version", false, "print the version and exit")
		checkOnly   = fs.Bool("check-config", false, "load and validate the configuration, then exit")
	)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if *showVersion {
		fmt.Printf("linchange %s\n", version)
		return nil
	}

	// The configuration file is mandatory. Running on built-in defaults would
	// mean accepting mail for a domain nobody configured.
	if *configPath == "" {
		fs.Usage()
		return config.ErrNoPath
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	log, err := logging.New(cfg.Log, "linchange")
	if err != nil {
		return err
	}

	if *checkOnly {
		fmt.Printf("%s is valid\n", *configPath)
		return nil
	}

	// SIGINT and SIGTERM both mean "shut down cleanly". A second signal is
	// left to the runtime so that an operator can force a kill.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Startup is bounded: a database that is unreachable must fail fast rather
	// than hang a container start forever.
	startupCtx, cancelStartup := context.WithTimeout(ctx, 60*time.Second)
	application, err := app.New(startupCtx, app.Options{Config: cfg, Logger: log})
	cancelStartup()
	if err != nil {
		return err
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		log.Info("shutdown signal received")
	}()

	// Listen until the context is cancelled. Later phases replace this with the
	// protocol listeners; for now it verifies that startup and shutdown are
	// correct before anything is layered on top.
	<-ctx.Done()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	application.Close(shutdownCtx)

	log.Info("stopped", "uptime", application.Uptime().Round(time.Second).String())
	<-shutdownDone
	return nil
}
