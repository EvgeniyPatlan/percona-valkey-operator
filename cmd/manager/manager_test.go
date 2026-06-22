package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	ginkgo "github.com/onsi/ginkgo/v2"
	gomega "github.com/onsi/gomega"
)

var _ = ginkgo.Describe("Manager boot", func() {
	ginkgo.It("starts, serves /readyz, and shuts down cleanly with no controllers", func() {
		opts := options{
			metricsAddr: "0", // ":0" => OS-assigned free port (avoid collisions)
			probeAddr:   "127.0.0.1:18181",
			// LeaderElection disabled so the test does not wait out a
			// Lease-acquisition cycle (GO-0.3).
			enableLeaderElection: false,
		}

		mgr, err := newManager(restCfg, opts)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
		gomega.Expect(mgr).NotTo(gomega.BeNil())

		ctx, cancel := context.WithCancel(context.Background())

		startErr := make(chan error, 1)
		go func() {
			defer ginkgo.GinkgoRecover()
			startErr <- mgr.Start(ctx)
		}()

		// /readyz should report 200 once the manager is up.
		gomega.Eventually(func() int {
			return probeStatus("http://" + opts.probeAddr + "/readyz")
		}, 30*time.Second, 250*time.Millisecond).Should(gomega.Equal(http.StatusOK))

		// healthz too.
		gomega.Eventually(func() int {
			return probeStatus("http://" + opts.probeAddr + "/healthz")
		}, 10*time.Second, 250*time.Millisecond).Should(gomega.Equal(http.StatusOK))

		// Start must return nil on context cancel (clean shutdown).
		cancel()
		gomega.Eventually(startErr, 30*time.Second).Should(gomega.Receive(gomega.BeNil()))
	})
})

// probeStatus issues a GET and returns the HTTP status code, or -1 on error.
func probeStatus(url string) int {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url) //nolint:noctx // short-lived test probe
	if err != nil {
		return -1
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return resp.StatusCode
}

// Compile-time assurance that watchNamespaces honours WATCH_NAMESPACE semantics.
var _ = ginkgo.Describe("watchNamespaces", func() {
	ginkgo.It("returns nil for empty/blank (cluster-wide)", func() {
		gomega.Expect(watchNamespaces("")).To(gomega.BeNil())
		gomega.Expect(watchNamespaces("   ")).To(gomega.BeNil())
		gomega.Expect(watchNamespaces(" , ,")).To(gomega.BeNil())
	})
	ginkgo.It("parses a comma-separated list", func() {
		m := watchNamespaces("ns1, ns2 ,ns3")
		gomega.Expect(m).To(gomega.HaveLen(3))
		for _, ns := range []string{"ns1", "ns2", "ns3"} {
			_, ok := m[ns]
			gomega.Expect(ok).To(gomega.BeTrue(), fmt.Sprintf("expected namespace %q in cache config", ns))
		}
	})
})
