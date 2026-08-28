// Package main runs a metadata server using Hardcover as an upstream.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/blampe/rreading-glasses/cmd"
	"github.com/blampe/rreading-glasses/internal"
	"github.com/go-chi/chi/v5/middleware"
)

// cli contains our command-line flags.
type cli struct {
	Serve server `cmd:"" help:"Run an HTTP server."`

	Bust cmd.Bust `cmd:"" help:"Bust cache entries."`
}

type server struct {
	cmd.PGConfig
	cmd.LogConfig
	cmd.CloudflareConfig

	Port                     int           `default:"8788" env:"PORT" help:"Port to serve traffic on."`
	Proxy                    string        `default:"" env:"PROXY" help:"HTTP proxy URL to use for upstream requests."`
	Upstream                 string        `default:"api.hardcover.app" env:"UPSTREAM" help:"Upstream host (e.g. www.example.com)."`
	BatchInterval            time.Duration `default:"2s" env:"BATCH_INTERVAL" help:"Minimum interval between physical Hardcover GraphQL requests."`
	BatchSize                int           `default:"1" env:"BATCH_SIZE" help:"Maximum background GraphQL fields per request (Hardcover maximum: 5)."`
	SearchBatchSize          int           `default:"1" env:"SEARCH_BATCH_SIZE" help:"Maximum interactive GraphQL fields per request (Hardcover maximum: 5)."`
	MaxPendingQueries        int           `default:"100" env:"MAX_PENDING_QUERIES" help:"Maximum queued GraphQL operations before local admission control rejects requests."`
	RequestTimeout           time.Duration `default:"30s" env:"REQUEST_TIMEOUT" help:"Timeout for one physical Hardcover request."`
	RateLimitCooldown        time.Duration `default:"15m" env:"RATE_LIMIT_COOLDOWN" help:"Cooldown used when Hardcover returns 429 without a valid Retry-After header."`
	SearchResults            int           `default:"5" env:"SEARCH_RESULTS" help:"Maximum Hardcover search results to hydrate (1-15)."`
	AuthorRefreshConcurrency int           `default:"1" env:"AUTHOR_REFRESH_CONCURRENCY" help:"Maximum simultaneous full-author catalogue refreshes."`
	AuthorRefreshRetryBase   time.Duration `default:"1h" env:"AUTHOR_REFRESH_RETRY_BASE" help:"Initial delay before retrying an incomplete author refresh."`
	AuthorRefreshRetryMax    time.Duration `default:"24h" env:"AUTHOR_REFRESH_RETRY_MAX" help:"Maximum exponential delay for an incomplete author refresh."`
	AuthorRefreshMaxAttempts int           `default:"6" env:"AUTHOR_REFRESH_MAX_ATTEMPTS" help:"Attempts before an unhealthy author refresh pauses until explicitly requested or restarted."`
	AuthorRecoveryInterval   time.Duration `default:"5m" env:"AUTHOR_RECOVERY_INTERVAL" help:"Spacing between persisted author refreshes recovered at startup."`

	HardcoverAuth     string `required:"" env:"HARDCOVER_AUTH" xor:"hardcover-auth" help:"Hardcover Authorization header, e.g. 'Bearer ...'"`
	HardcoverAuthFile []byte `required:"" type:"filecontent" xor:"hardcover-auth" env:"HARDCOVER_AUTH_FILE" help:"File containing the Hardcover Authorization header, e.g. 'Bearer ...'"`
}

func (s *server) Run() error {
	_ = s.LogConfig.Run()
	reg := internal.NewMetrics()

	cf, err := s.Cache(reg)
	if err != nil {
		return fmt.Errorf("setting up cloudflare: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cache, err := internal.NewCache(ctx, s.DSN(), cf, reg)
	if err != nil {
		return fmt.Errorf("setting up cache: %w", err)
	}

	if len(s.HardcoverAuthFile) > 0 {
		s.HardcoverAuth = string(bytes.TrimSpace(s.HardcoverAuthFile))
	}

	gate := internal.NewPersistentRateLimitGate(ctx, s.RateLimitCooldown, cache)
	upstreamTransport := internal.ScopedTransport{
		Host: s.Upstream,
		RoundTripper: &internal.HeaderTransport{
			Key:          "Authorization",
			Value:        s.HardcoverAuth,
			RoundTripper: http.DefaultTransport,
		},
	}
	hcTransport := internal.RateLimitTransport{Gate: gate, RoundTripper: upstreamTransport}

	hcClient := &http.Client{Transport: hcTransport}

	batchConfig := internal.DefaultHardcoverBatcherConfig()
	batchConfig.BatchInterval = s.BatchInterval
	batchConfig.BatchSize = s.BatchSize
	batchConfig.SearchBatchSize = s.SearchBatchSize
	batchConfig.MaxPendingQueries = s.MaxPendingQueries
	batchConfig.RequestTimeout = s.RequestTimeout
	batchConfig.RateLimitFallback = s.RateLimitCooldown
	gql, err := internal.NewConfiguredBatchedGraphQLClient(ctx, "https://api.hardcover.app/v1/graphql", hcClient, batchConfig, gate, reg)
	if err != nil {
		return err
	}
	defer gql.Close()

	getter, err := internal.NewConfiguredHardcoverGetter(cache, gql, s.SearchResults)
	if err != nil {
		return err
	}

	persister, err := internal.NewPersister(ctx, cache, s.DSN())
	if err != nil {
		return err
	}

	controllerConfig := internal.DefaultControllerConfig()
	controllerConfig.AuthorRefreshConcurrency = s.AuthorRefreshConcurrency
	controllerConfig.AuthorRefreshRetryBase = s.AuthorRefreshRetryBase
	controllerConfig.AuthorRefreshRetryMax = s.AuthorRefreshRetryMax
	controllerConfig.AuthorRefreshMaxAttempts = s.AuthorRefreshMaxAttempts
	controllerConfig.AuthorRecoveryInterval = s.AuthorRecoveryInterval
	ctrl, err := internal.NewConfiguredController(cache, getter, persister, controllerConfig, reg)
	if err != nil {
		return err
	}
	h := internal.NewHandler(ctrl)
	mux := internal.NewMux(h, reg)

	mux = middleware.RequestSize(1024)(mux)  // Limit request bodies.
	mux = internal.Requestlogger{}.Wrap(mux) // Log requests.
	mux = middleware.RequestID(mux)          // Include a request ID header.
	mux = middleware.Recoverer(mux)          // Recover from panics.

	// TODO: The client doesn't send Accept-Encoding and doesn't handle
	// Content-Encoding responses. This would allow us to send compressed bytes
	// directly from the cache.

	addr := fmt.Sprintf(":%d", s.Port)
	server := &http.Server{
		Handler:  mux,
		Addr:     addr,
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("listening on " + addr)
		err := server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	controllerDone := make(chan struct{})
	go func() {
		defer close(controllerDone)
		ctrl.Run(ctx)
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutdown requested")
	case err := <-serverErr:
		stop()
		return fmt.Errorf("serving HTTP: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down HTTP server: %w", err)
	}
	ctrl.Shutdown(shutdownCtx)
	_ = gql.Close()

	select {
	case <-controllerDone:
	case <-shutdownCtx.Done():
		internal.Log(ctx).Warn("controller did not stop before shutdown deadline")
	}

	slog.Info("au revoir!")

	return nil
}

func main() {
	kctx := kong.Parse(&cli{})
	err := kctx.Run()
	if err != nil {
		internal.Log(context.Background()).Error("fatal", "err", err)
		os.Exit(1)
	}
}
