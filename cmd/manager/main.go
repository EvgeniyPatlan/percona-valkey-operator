package main

import (
	"flag"
	"os"

	"go.uber.org/zap/zapcore"
	ctrl "sigs.k8s.io/controller-runtime"
	crzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func main() {
	opts := options{}
	flag.StringVar(&opts.metricsAddr, "metrics-bind-address", ":8080",
		"The address the metrics endpoint binds to.")
	flag.StringVar(&opts.probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	// Leader election defaults to TRUE: the operator's production posture is
	// replicas: 2+ with a single active reconciler (doc 04 §8). Pass
	// --leader-elect=false only for off-cluster local dev (`make run`).
	flag.BoolVar(&opts.enableLeaderElection, "leader-elect", true,
		"Enable leader election to ensure only one active controller manager.")
	flag.BoolVar(&opts.enableHTTP2, "enable-http2", false,
		"Enable HTTP/2 for the metrics and webhook servers (off mitigates Rapid Reset CVEs).")

	// WATCH_NAMESPACE (env, comma-separated) scopes the cache; empty =>
	// cluster-wide. A repeatable --watch-namespace flag mirrors the env for
	// off-cluster runs and overrides it when set.
	var watchNamespace multiFlag
	flag.Var(&watchNamespace, "watch-namespace",
		"Namespace to watch (repeatable). Empty => cluster-wide. Overrides WATCH_NAMESPACE when set.")

	zapOpts := crzap.Options{Development: true, StacktraceLevel: zapcore.FatalLevel}
	zapOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(crzap.New(crzap.UseFlagOptions(&zapOpts)))

	opts.watchNamespace = watchNamespace.String()
	if opts.watchNamespace == "" {
		opts.watchNamespace = watchNamespaceFromEnv()
	}

	mgr, err := newManager(ctrl.GetConfigOrDie(), opts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	setupLog.Info("starting manager (no controllers registered — M0 bootstrap)",
		"leaderElection", opts.enableLeaderElection,
		"watchNamespace", opts.watchNamespace)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited non-zero")
		os.Exit(1)
	}
}

// multiFlag is a repeatable string flag, accumulating each occurrence and
// joining them with commas for watchNamespaces() to split.
type multiFlag []string

func (m *multiFlag) String() string {
	return joinNonEmpty(*m)
}

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func joinNonEmpty(parts []string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += ","
		}
		out += p
	}
	return out
}
