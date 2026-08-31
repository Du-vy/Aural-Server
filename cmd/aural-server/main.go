// Command aural-server runs a self-hosted Aural voice and chat server.
//
// A first run needs no arguments: the configuration file and the database are
// created next to the binary, and a one-time owner token is printed for the
// first administrator to redeem.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aural-chat/aural-server/internal/buildinfo"
	"github.com/aural-chat/aural-server/internal/config"
	"github.com/aural-chat/aural-server/internal/gateway"
	"github.com/aural-chat/aural-server/internal/logging"
	"github.com/aural-chat/aural-server/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "aural-server: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath     = flag.String("config", "config.json", "path to the JSON configuration file")
		showVersion    = flag.Bool("version", false, "print the version and exit")
		newOwnerToken  = flag.Bool("new-owner-token", false, "issue a fresh owner token and exit")
		printConfigDoc = flag.Bool("print-config", false, "print the default configuration and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("%s %s\n", buildinfo.Name, buildinfo.Version)
		return nil
	}
	if *printConfigDoc {
		raw, err := json.MarshalIndent(config.Default(), "", "  ")
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", raw)
		return nil
	}

	cfg, created, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	log := logging.New(cfg.Log.Level, cfg.Log.Format)
	if created {
		log.Info("wrote a default configuration file", slog.String("path", *configPath))
	}

	// Signals cancel the root context, which unwinds the listener, the sessions
	// and the database in that order.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.Database.Path)
	if err != nil {
		return err
	}
	defer db.Close()

	server, err := gateway.New(ctx, &cfg, *configPath, db, log)
	if err != nil {
		return err
	}

	if *newOwnerToken {
		token, err := gateway.RotateOwnerToken(ctx, db)
		if err != nil {
			return err
		}
		printOwnerToken(token)
		return nil
	}

	token, err := gateway.EnsureOwnerToken(ctx, db, server.Hub())
	if err != nil {
		return err
	}
	if token != "" {
		printOwnerToken(token)
	}

	log.Info("starting",
		slog.String("version", buildinfo.Version),
		slog.String("name", cfg.Server.Name),
		slog.String("voice_mode", cfg.Voice.Mode),
		slog.String("database", cfg.Database.Path))

	return server.Run(ctx)
}

// printOwnerToken puts the token on stdout, where an operator copying it out of
// a terminal or a log file will actually see it.
func printOwnerToken(token string) {
	fmt.Printf(`
  ---------------------------------------------------------------
   Owner token: %s

   Redeem it once from a connected client to become this server
   administrator. It is stored hashed and cannot be shown again;
   run with -new-owner-token to issue a replacement.
  ---------------------------------------------------------------

`, token)
}
