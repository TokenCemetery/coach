package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/TokenCemetery/coach/internal/media"
	"github.com/TokenCemetery/coach/internal/server"
	"github.com/TokenCemetery/coach/internal/state"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	data := flag.String("data", "./data", "private state directory")
	listen := flag.String("listen", "127.0.0.1:8097", "HTTP listen address")
	name := flag.String("name", "Coach", "server display name")
	upstream := flag.String("web-upstream", "", "optional Emby origin supplying only /web static assets")
	webDir := flag.String("web-dir", "", "optional local Emby Web directory containing index.html")
	mediaDir := flag.String("media-dir", "", "optional read-only movie directory; scanned at startup with ffprobe")
	init := flag.Bool("init", false, "initialize a local user; read password from stdin and exit")
	username := flag.String("username", "", "username for -init")
	flag.Parse()
	if *webDir != "" && *upstream != "" {
		return errors.New("choose either -web-dir or -web-upstream")
	}
	s, err := state.Open(*data)
	if err != nil {
		return fmt.Errorf("open state: %w", err)
	}
	defer s.Close()
	if *init {
		if s.Initialized() {
			return errors.New("already initialized; existing state was not modified")
		}
		if *username == "" {
			return errors.New("-init requires -username")
		}
		password, err := state.ReadPassword(os.Stdin)
		if err != nil {
			return err
		}
		if err := s.Initialize(*username, password); err != nil {
			return err
		}
		fmt.Println("Local user initialized.")
		return nil
	}
	if !s.Initialized() {
		return errors.New("initialize first with -init -username NAME and a password on stdin")
	}
	var web http.Handler
	if *webDir != "" {
		local, err := server.OpenWebDirectory(*webDir)
		if err != nil {
			return err
		}
		defer local.Close()
		web = local
	}
	if *upstream != "" {
		web, err = server.WebAssets(*upstream)
		if err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var catalog *media.Catalog
	if *mediaDir != "" {
		slog.Info("Scanning movie directory")
		scanCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		catalog, err = media.Scan(scanCtx, *mediaDir)
		cancel()
		if err != nil {
			return fmt.Errorf("scan movies: %w", err)
		}
		slog.Info("Movie scan complete", "movies", len(catalog.Items), "skipped", catalog.Skipped)
		defer catalog.Close()
	}
	api := server.New(s, *name, web, catalog)
	defer api.Close()
	httpServer := &http.Server{Addr: *listen, Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()
	slog.Info("Coach starting", "address", *listen, "version", server.Version)
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		api.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdown); err != nil {
			_ = httpServer.Close()
			return err
		}
		return nil
	}
}
