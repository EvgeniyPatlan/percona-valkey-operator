// Package main is the percona/valkey-operator manager entrypoint.
//
// In M0 (bootstrap) it registers NO controllers; it only proves the manager
// boots, serves /healthz and /readyz, exposes the controller-runtime metrics
// endpoint, and can hold a leader-election Lease. Controllers are appended to
// the pkg/controller registry from M2+ and wired here via controller.AddToManager.
//
// This file holds the testable newManager builder and scheme registration so
// the envtest manager-boot smoke (GO-0.3) can construct the manager from an
// envtest rest.Config without flag parsing or os.Exit. See
// docs/implementation/01-phase0-bootstrap.md (GO-0.2) and
// docs/architecture/04-control-plane.md §8 (leader election).
package main

import (
	"context"
	"crypto/tls"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	// Load all auth plugins so off-cluster `make run` can authenticate against
	// any provider (GKE/EKS/AKS/OIDC).
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	valkeyv1alpha1 "valkey.percona.com/percona-valkey-operator/pkg/apis/valkey/v1alpha1"
	"valkey.percona.com/percona-valkey-operator/pkg/controller"
	pkgtls "valkey.percona.com/percona-valkey-operator/pkg/tls"
	"valkey.percona.com/percona-valkey-operator/pkg/version"
)

// operatorVersion returns the operator semver compiled in via pkg/version, for the
// manager startup log (09 §1 — the full semver is logged at startup and aligns
// with the Deployment image tag).
func operatorVersion() string { return version.Version() }

// Webhook serving-cert startup gate (M5 GO-5.14 scaffold, consumed in M6).
//
// DEFERRED v1-CONVERSION PLAN (09 §6, impl 07 §10 / GO-6.13–6.18): the full
// v1alpha1 -> v1 graduation is intentionally NOT in this pass. v1alpha1 is still
// evolving, so introducing an api/v1 package + conversion webhook handler now would
// cause churn. When the API is field-stable the conversion lands as: (1) an
// api/v1 hub package with Hub() markers on all four kinds; (2) v1alpha1 spoke
// ConvertTo/ConvertFrom with lossless round-trip via the
// valkey.percona.com/conversion-data annotation (crVersion carried verbatim);
// (3) controllers re-pointed to the v1 hub; (4) the conversion webhook registered
// on the manager's webhook server, advertising ONLY after WaitForWebhookCert
// below passes; (5) the CRD conversion.strategy: Webhook + caBundle wiring in
// deploy/. Until then this gate is a guarded no-op: it activates only when the
// webhook-cert Secret is explicitly named (WEBHOOK_CERT_SECRET[/_NAMESPACE]), so a
// deployment with no webhook configured never blocks on a cert that will never
// arrive. No api/v1 package ships in this pass.
const (
	// envWebhookCertSecret names the webhook serving-cert Secret to gate on. Unset
	// => no webhook configured => the startup gate is skipped entirely.
	envWebhookCertSecret = "WEBHOOK_CERT_SECRET" //#nosec G101 -- env-var name, not a hardcoded credential
	// envWebhookCertNamespace overrides the namespace of the webhook-cert Secret.
	// Defaults to POD_NAMESPACE when unset.
	envWebhookCertNamespace = "WEBHOOK_CERT_SECRET_NAMESPACE"
	// webhookCertWaitTimeout bounds the startup gate so a misconfigured cert ref
	// surfaces as a bounded failure rather than an indefinite hang.
	webhookCertWaitTimeout = 2 * time.Minute
)

// leaderElectionID is the name of the coordination.k8s.io Lease used for
// leader election. It is a DNS-1123 label (no dots). See doc 04 §8.
const leaderElectionID = "valkey-operator-lock"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(valkeyv1alpha1.AddToScheme(scheme)) // empty in M0
}

// options carries the manager-construction knobs parsed from flags in main().
// Keeping them in a struct lets the GO-0.3 envtest smoke build a manager with a
// bespoke configuration (e.g. LeaderElection disabled) without touching flags.
type options struct {
	metricsAddr          string
	probeAddr            string
	enableLeaderElection bool
	enableHTTP2          bool
	watchNamespace       string
}

// newManager builds a controller-runtime manager from a rest.Config and the
// given options, registers the controller fan-out (empty in M0), and wires the
// healthz/readyz checks. It performs NO flag parsing and never calls os.Exit,
// so it is safe to call from tests.
func newManager(cfg *rest.Config, opts options) (manager.Manager, error) {
	// HTTP/2 is disabled by default to mitigate the Rapid Reset CVEs
	// (CVE-2023-44487 / CVE-2023-39325). Re-enable only behind --enable-http2.
	tlsOpts := []func(*tls.Config){}
	if !opts.enableHTTP2 {
		tlsOpts = append(tlsOpts, func(c *tls.Config) {
			c.NextProtos = []string{"http/1.1"}
		})
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: opts.metricsAddr,
			TLSOpts:     tlsOpts,
		},
		HealthProbeBindAddress: opts.probeAddr,
		LeaderElection:         opts.enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
		// LeaderElectionNamespace is intentionally left unset: controller-runtime
		// resolves the in-cluster namespace (POD_NAMESPACE / serviceaccount file).
		// Off-cluster `make run` disables leader election, so it is unset there too.
		Cache: cache.Options{
			// Empty map => cluster-wide watch. WATCH_NAMESPACE scopes the cache.
			DefaultNamespaces: watchNamespaces(opts.watchNamespace),
		},
	})
	if err != nil {
		return nil, err
	}

	if err := controller.AddToManager(mgr); err != nil { // no-op in M0
		return nil, err
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, err
	}

	return mgr, nil
}

// waitForWebhookCertGate blocks (bounded) until the configured webhook serving-cert
// Secret is populated, using the reusable M5 pkg/tls startup gate. It is a guarded
// no-op when WEBHOOK_CERT_SECRET is unset (no webhook configured) so a webhook-less
// deployment never blocks. When the conversion webhook lands (deferred v1 plan,
// see the const block above) this gate ensures the API server cannot route a
// conversion call at a webhook whose TLS material is not yet written (07 §3.5).
//
// It takes a client.Client (the manager's API client) so it polls the live Secret;
// callers invoke it after newManager and before mgr.Start.
func waitForWebhookCertGate(ctx context.Context, c client.Client) error {
	name := strings.TrimSpace(os.Getenv(envWebhookCertSecret))
	if name == "" {
		// No webhook configured: nothing to gate on.
		return nil
	}
	ns := strings.TrimSpace(os.Getenv(envWebhookCertNamespace))
	if ns == "" {
		ns = strings.TrimSpace(os.Getenv("POD_NAMESPACE"))
	}
	secret := types.NamespacedName{Namespace: ns, Name: name}
	return pkgtls.WaitForWebhookCert(ctx, c, secret, webhookCertWaitTimeout)
}

// watchNamespaces parses a comma-separated namespace list (from WATCH_NAMESPACE)
// into the controller-runtime cache configuration. An empty/blank value returns
// nil, which means cluster-wide watch. See doc 02 §7.
func watchNamespaces(raw string) map[string]cache.Config {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	namespaces := map[string]cache.Config{}
	for _, ns := range strings.Split(raw, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			namespaces[ns] = cache.Config{}
		}
	}
	if len(namespaces) == 0 {
		return nil
	}
	return namespaces
}

// watchNamespaceFromEnv reads the WATCH_NAMESPACE environment variable. Kept as
// a tiny helper so main() and tests share one source of truth.
func watchNamespaceFromEnv() string {
	return os.Getenv("WATCH_NAMESPACE")
}
