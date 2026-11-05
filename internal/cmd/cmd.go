// Package cmd defines the looking-glass command line.
package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"go.datum.net/looking-glass/internal/config"
	"go.datum.net/looking-glass/internal/fakebackend"
	"go.datum.net/looking-glass/internal/manager"
)

// New returns the root command. Every subcommand stops gracefully when ctx is
// cancelled.
func New(ctx context.Context) *cobra.Command {
	zapOpts := zap.Options{}
	root := &cobra.Command{
		Use:           "looking-glass",
		Short:         "Datum Cloud Looking Glass services",
		SilenceUsage:  true,
		SilenceErrors: true,
		// A subcommand that defines its own PersistentPreRun replaces this
		// one, so it must set up the logger itself.
		PersistentPreRun: func(*cobra.Command, []string) {
			ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
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

	root.AddCommand(newManagerCommand(), newFakeBackendCommand())
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

func newFakeBackendCommand() *cobra.Command {
	opts := fakebackend.DefaultOptions()
	listen := "127.0.0.1:9090"
	c := &cobra.Command{
		Use:   "fake-backend",
		Short: "Serve a fake Galactic LookingGlassService for local development",
		Long: "Serve a fake fabric.service.v1.LookingGlassService over plaintext gRPC.\n" +
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
			ctrl.Log.WithName("fake-backend").Info("serving", "address", lis.Addr().String(), "cell", opts.Cell, "routers", opts.Routers)
			return serveUntilDone(c.Context(), srv, lis)
		},
	}
	f := c.Flags()
	f.StringVar(&listen, "listen", listen, "address to serve gRPC on")
	f.StringVar(&opts.Cell, "cell", opts.Cell, "cell the fake serves")
	f.StringSliceVar(&opts.Routers, "routers", opts.Routers, "fabric routers in the fake cell")
	f.StringSliceVar(&opts.Unreachable, "unreachable", nil, "routers whose outcome is NodeUnreachable")
	f.DurationVar(&opts.OutcomeDelay, "outcome-delay", opts.OutcomeDelay, "pause before each router outcome")
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
