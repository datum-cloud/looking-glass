// Package manager builds and runs the Looking Glass controller manager.
package manager

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	clusterv1alpha1 "github.com/karmada-io/api/cluster/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	lookingglassv1alpha "go.datum.net/looking-glass/api/v1alpha"
	"go.datum.net/looking-glass/internal/admission"
	"go.datum.net/looking-glass/internal/config"
	"go.datum.net/looking-glass/internal/placement"
	"go.datum.net/looking-glass/internal/session"
)

// readyzCacheSyncTimeout bounds how long a readiness probe waits for the
// informer caches before reporting not ready.
const readyzCacheSyncTimeout = time.Second

// NewScheme returns the scheme holding every type the manager reads or writes.
func NewScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering client-go types: %w", err)
	}
	if err := lookingglassv1alpha.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering Looking Glass types: %w", err)
	}
	return scheme, nil
}

// Options translates the configuration file into controller-runtime options.
func Options(cfg *config.Manager, scheme *runtime.Scheme) ctrl.Options {
	shutdown := cfg.GracefulShutdownTimeout.Duration
	opts := ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: cfg.Metrics.BindAddress,
		},
		HealthProbeBindAddress:        cfg.Health.BindAddress,
		LeaderElection:                cfg.LeaderElection.Enabled,
		LeaderElectionID:              cfg.LeaderElection.ID,
		LeaderElectionNamespace:       cfg.LeaderElection.Namespace,
		LeaderElectionReleaseOnCancel: true,
		GracefulShutdownTimeout:       &shutdown,
	}
	if cfg.Webhook.Enabled {
		opts.WebhookServer = webhook.NewServer(webhook.Options{
			Host:    cfg.Webhook.Host,
			Port:    cfg.Webhook.Port,
			CertDir: cfg.Webhook.CertDir,
		})
	}
	return opts
}

// NewSessionAdmission builds session admission from the configuration.
// Every project is served from the manager's own API server until the
// manager discovers project control planes (#46).
func NewSessionAdmission(restConfig *rest.Config, scheme *runtime.Scheme, cfg *config.Manager) (*admission.SessionAdmission, error) {
	deny, err := admission.ParsePrefixes(cfg.Admission.DestinationPolicy.Deny)
	if err != nil {
		return nil, fmt.Errorf("parsing destination deny list: %w", err)
	}
	allow, err := admission.ParsePrefixes(cfg.Admission.DestinationPolicy.Allow)
	if err != nil {
		return nil, fmt.Errorf("parsing destination allow list: %w", err)
	}
	// An uncached client: admission reads a location at most once per
	// request, and must see the capabilities the controller last wrote.
	cl, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("creating admission client: %w", err)
	}
	rl := cfg.Admission.CreateRateLimit
	return &admission.SessionAdmission{
		Projects:     admission.SingleClusterResolver{Client: cl},
		RateLimiter:  admission.NewProjectRateLimiter(rl.QPS, rl.Burst),
		Destinations: admission.DestinationPolicy{Deny: deny, Allow: allow},
	}, nil
}

// NewPlacer builds the configured placement source. For federation it also
// returns the federation hub cluster, which the caller adds to the manager so
// its cache starts and stops with it.
func NewPlacer(cfg config.PlacementConfig) (placement.Placer, cluster.Cluster, error) {
	if cfg.Federation == nil {
		return placement.StaticPlacer{Cells: cfg.Static}, nil, nil
	}
	restConfig, err := clientcmd.BuildConfigFromFlags("", cfg.Federation.Kubeconfig)
	if err != nil {
		return nil, nil, fmt.Errorf("loading federation kubeconfig: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := clusterv1alpha1.Install(scheme); err != nil {
		return nil, nil, fmt.Errorf("registering Karmada cluster types: %w", err)
	}
	hub, err := cluster.New(restConfig, func(o *cluster.Options) { o.Scheme = scheme })
	if err != nil {
		return nil, nil, fmt.Errorf("creating federation hub client: %w", err)
	}
	return placement.ClusterPlacer{
		Reader:        hub.GetClient(),
		LocationLabel: cfg.Federation.LocationLabel,
		OptInLabel:    cfg.Federation.OptInLabel,
	}, hub, nil
}

// addSessionController registers the session controller when placement is
// configured. Every project is served from the manager's own API server
// until the manager discovers project control planes (#46).
func addSessionController(mgr ctrl.Manager, cfg *config.Manager) error {
	if !cfg.Placement.Enabled() {
		mgr.GetLogger().Info("no placement source configured; session reconciliation is disabled")
		return nil
	}
	placer, hub, err := NewPlacer(cfg.Placement)
	if err != nil {
		return err
	}
	if hub != nil {
		if err := mgr.Add(hub); err != nil {
			return fmt.Errorf("adding federation hub: %w", err)
		}
	}
	r := &session.Reconciler{Client: mgr.GetClient(), Placer: placer}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("registering session controller: %w", err)
	}
	return nil
}

// New builds a manager with liveness and readiness checks, session admission
// and the session controller registered.
func New(restConfig *rest.Config, cfg *config.Manager) (ctrl.Manager, error) {
	scheme, err := NewScheme()
	if err != nil {
		return nil, err
	}
	mgr, err := ctrl.NewManager(restConfig, Options(cfg, scheme))
	if err != nil {
		return nil, fmt.Errorf("creating manager: %w", err)
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return nil, fmt.Errorf("adding liveness check: %w", err)
	}
	if cfg.Webhook.Enabled {
		adm, err := NewSessionAdmission(restConfig, scheme, cfg)
		if err != nil {
			return nil, err
		}
		if err := adm.SetupWithManager(mgr); err != nil {
			return nil, fmt.Errorf("registering session admission: %w", err)
		}
		if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
			return nil, fmt.Errorf("adding webhook readiness check: %w", err)
		}
	}
	if err := addSessionController(mgr, cfg); err != nil {
		return nil, err
	}
	// Readiness waits for the cache to start and its informers to sync. It
	// only reflects API server reachability once controllers register
	// informers.
	if err := mgr.AddReadyzCheck("informers", func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), readyzCacheSyncTimeout)
		defer cancel()
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			return errors.New("informer caches are not synced")
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("adding readiness check: %w", err)
	}
	return mgr, nil
}

// Run builds the manager and blocks until ctx is cancelled and every runnable
// has stopped or the graceful shutdown timeout has elapsed.
func Run(ctx context.Context, restConfig *rest.Config, cfg *config.Manager) error {
	mgr, err := New(restConfig, cfg)
	if err != nil {
		return err
	}
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("running manager: %w", err)
	}
	return nil
}
