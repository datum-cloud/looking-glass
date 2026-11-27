// Package cmd defines the looking-glass command line.
package cmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	"go.miloapis.com/instrumentation/logging"
	"go.miloapis.com/instrumentation/metrics"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"go.datum.net/looking-glass/internal/config"
	"go.datum.net/looking-glass/internal/fakebackend"
	"go.datum.net/looking-glass/internal/manager"
	"go.datum.net/looking-glass/internal/routerbackend"
)

// Execute runs the command line with args and returns the process exit code.
// A failure is reported once: through the configured logger when setup got
// that far, so it is a structured error event, otherwise (for example a bad
// flag) as a plain line on stderr.
func Execute(ctx context.Context, args []string, stderr io.Writer) int {
	var logger *logr.Logger
	root := newRoot(ctx, func(l logr.Logger) { logger = &l })
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		if logger != nil {
			logger.Error(err, "exiting")
		} else {
			_, _ = fmt.Fprintln(stderr, "error:", err)
		}
		return 1
	}
	return 0
}

// New returns the root command. Every subcommand stops gracefully when ctx is
// cancelled.
func New(ctx context.Context) *cobra.Command {
	return newRoot(ctx, nil)
}

// newRoot builds the command tree. onLogger, when set, receives the logger
// once it is configured.
func newRoot(ctx context.Context, onLogger func(logr.Logger)) *cobra.Command {
	zapOpts := zap.Options{}
	root := &cobra.Command{
		Use:           "looking-glass",
		Short:         "Datum Cloud Looking Glass services",
		SilenceUsage:  true,
		SilenceErrors: true,
		// A subcommand that defines its own PersistentPreRun replaces this
		// one, so it must set up the logger itself.
		PersistentPreRun: func(*cobra.Command, []string) {
			logger := logging.New(&zapOpts)
			ctrl.SetLogger(logger)
			if onLogger != nil {
				onLogger(logger)
			}
		},
	}
	root.SetContext(ctx)
	// A fresh flag set per command tree keeps New callable more than once.
	// controller-runtime registers --kubeconfig on flag.CommandLine, and
	// ctrl.GetConfig reads it from there, so share that flag's value.
	goFlags := flag.NewFlagSet("looking-glass", flag.ContinueOnError)
	zapOpts.BindFlags(goFlags)
	if kubeconfig := flag.CommandLine.Lookup("kubeconfig"); kubeconfig != nil {
		goFlags.Var(kubeconfig.Value, kubeconfig.Name, kubeconfig.Usage)
	}
	root.PersistentFlags().AddGoFlagSet(goFlags)

	root.AddCommand(newManagerCommand(), newBackendCommand(), newFakeBackendCommand())
	return root
}

func newManagerCommand() *cobra.Command {
	var configPath string
	c := &cobra.Command{
		Use:   "manager",
		Short: "Run the controller manager",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			restConfig, err := ctrl.GetConfig()
			if err != nil {
				return fmt.Errorf("loading kubeconfig: %w", err)
			}
			ctrl.Log.WithName("manager").Info("starting",
				"healthBindAddress", cfg.Health.BindAddress,
				"metricsBindAddress", cfg.Metrics.BindAddress,
				"leaderElection", cfg.LeaderElection.Enabled)
			return manager.Run(c.Context(), restConfig, cfg)
		},
	}
	c.Flags().StringVar(&configPath, "config", "", "path to the manager configuration file; defaults apply when unset")
	return c
}

func newBackendCommand() *cobra.Command {
	var configPath string
	c := &cobra.Command{
		Use:   "backend",
		Short: "Run the router backend",
		Long: "Serve lookingglass.backend.v1.DiagnosticService: run diagnostics on the routers in the\n" +
			"configuration file through their drivers. It serves gRPC on listen, with mutual TLS\n" +
			"unless listen is a loopback address, and Prometheus metrics on metrics.bindAddress.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := config.LoadBackend(configPath)
			if err != nil {
				return err
			}
			log := ctrl.Log.WithName("backend")
			backend, err := routerbackend.New(cfg, routerbackend.Options{Log: log})
			if err != nil {
				return err
			}
			defer backend.Close()
			var opts []grpc.ServerOption
			if cfg.TLS.CertFile != "" {
				creds, err := serverTLS(cfg.TLS)
				if err != nil {
					return err
				}
				opts = append(opts, grpc.Creds(creds))
			}
			srv := grpc.NewServer(opts...)
			backend.Register(srv)
			lis, err := (&net.ListenConfig{}).Listen(c.Context(), "tcp", cfg.Listen)
			if err != nil {
				return fmt.Errorf("listening: %w", err)
			}
			if cfg.Metrics.BindAddress != "0" {
				stop, err := serveMetrics(c.Context(), cfg.Metrics.BindAddress)
				if err != nil {
					_ = lis.Close()
					return err
				}
				defer stop()
			}
			log.Info("serving", "address", lis.Addr().String(), "cell", cfg.Cell,
				"locations", len(cfg.Locations), "tls", cfg.TLS.CertFile != "", "metricsBindAddress", cfg.Metrics.BindAddress)
			return serveUntilDone(c.Context(), srv, lis)
		},
	}
	c.Flags().StringVar(&configPath, "config", "", "path to the backend configuration file (required)")
	_ = c.MarkFlagRequired("config")
	return c
}

// serverTLS builds mutual TLS credentials: the server's certificate, and
// client certificates required and verified against the client CAs.
func serverTLS(cfg config.TLSConfig) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading tls.certFile and tls.keyFile: %w", err)
	}
	pem, err := os.ReadFile(cfg.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("reading tls.clientCAFile: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("tls.clientCAFile holds no PEM certificate")
	}
	return credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}), nil
}

// serveMetrics serves the instrumentation library's registry at /metrics
// until the returned function is called or ctx ends.
func serveMetrics(ctx context.Context, addr string) (stop func(), err error) {
	lis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listening for metrics: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(lis) }()
	return func() {
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}, nil
}

func newFakeBackendCommand() *cobra.Command {
	opts := fakebackend.DefaultOptions()
	listen := "127.0.0.1:9090"
	c := &cobra.Command{
		Use:   "fake-backend",
		Short: "Serve a fake backend for local development",
		Long: "Serve a fake lookingglass.backend.v1.DiagnosticService over plaintext gRPC.\n" +
			"It returns synthetic results and never probes a network. Do not expose it.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			fake, err := fakebackend.New(opts)
			if err != nil {
				return err
			}
			lis, err := (&net.ListenConfig{}).Listen(c.Context(), "tcp", listen)
			if err != nil {
				return fmt.Errorf("listening: %w", err)
			}
			srv := grpc.NewServer()
			fake.Register(srv)
			ctrl.Log.WithName("fake-backend").Info("serving", "address", lis.Addr().String(),
				"cell", opts.Cell, "location", opts.Location, "routers", opts.Routers)
			return serveUntilDone(c.Context(), srv, lis)
		},
	}
	f := c.Flags()
	f.StringVar(&listen, "listen", listen, "address to serve gRPC on")
	f.StringVar(&opts.Cell, "cell", opts.Cell, "cell the fake serves")
	f.StringVar(&opts.Location, "location", opts.Location, "location the fake serves")
	f.StringSliceVar(&opts.Routers, "routers", opts.Routers, "routers at the location")
	f.StringSliceVar(&opts.Unreachable, "unreachable", nil, "routers whose outcome is RouterUnreachable")
	f.StringSliceVar(&opts.Ineligible, "ineligible", nil, "routers the snapshot omits as Ineligible")
	f.DurationVar(&opts.OutcomeDelay, "outcome-delay", opts.OutcomeDelay, "pause before each router outcome; a long delay makes every router slow")
	return c
}

// serveUntilDone serves until ctx is cancelled, then drains in-flight streams
// for up to shutdownGrace before forcing them closed.
func serveUntilDone(ctx context.Context, srv *grpc.Server, lis net.Listener) error {
	const shutdownGrace = 10 * time.Second
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(lis) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	stopped := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(shutdownGrace):
		srv.Stop()
	}
	if err := <-errc; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}
