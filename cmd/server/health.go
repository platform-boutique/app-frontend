package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"

	"frontend/internal/obs"
)

// Probe endpoints (CODE-02, CODE-03, CODE-06). None of them needs a session or a signed-in user, see isStaticPath.
//
//	/healthz   liveness: 200 "ok" while the process serves HTTP; no dependency is looked at.
//	/_healthz  the same answer, kept for compose, the e2e tests and the faro runner.
//	/readyz    readiness: 200 only while the gRPC connections every page needs are READY, else 503.
//	/version   JSON with the version, commit and build time of this binary.

const (
	// readyTimeout bounds the whole /readyz check (all connections are checked in parallel).
	readyTimeout = 1500 * time.Millisecond
	// healthcheckTimeout bounds the `frontend healthcheck` probe (the Docker HEALTHCHECK timeout is longer).
	healthcheckTimeout = 3 * time.Second
)

// grpcConn is the part of *grpc.ClientConn that the readiness check uses.
type grpcConn interface {
	Connect()
	GetState() connectivity.State
	WaitForStateChange(ctx context.Context, sourceState connectivity.State) bool
}

type namedConn struct {
	name string
	conn grpcConn
}

// readinessConns are the connections without which no page can be rendered: the catalogue, the
// currency list and the cart are read by the home page and by every product, department and cart page.
// The others are left out on purpose: recommendation, ad, shipping and checkout failures degrade or
// fail one page, and the optional review/order/user/inventory/email services never fail a page that
// does not use them. Taking the pod out of the Service for those would turn a partial outage into a total one.
func (fe *frontendServer) readinessConns() []namedConn {
	var cs []namedConn
	for _, c := range []struct {
		name string
		conn *grpc.ClientConn
	}{{"product-catalog", fe.productCatalogSvcConn}, {"currency", fe.currencySvcConn}, {"cart", fe.cartSvcConn}} {
		if c.conn != nil { // a nil connection (tests that call no backend) is not a backend that is down
			cs = append(cs, namedConn{c.name, c.conn})
		}
	}
	return cs
}

// connReady starts a connection attempt if the channel is idle and waits until it is READY.
// A channel in TRANSIENT_FAILURE (the last attempt failed) or SHUTDOWN is not ready, without waiting.
func connReady(ctx context.Context, c grpcConn) bool {
	c.Connect()
	for {
		switch s := c.GetState(); s {
		case connectivity.Ready:
			return true
		case connectivity.TransientFailure, connectivity.Shutdown:
			return false
		default: // Idle, Connecting
			if !c.WaitForStateChange(ctx, s) {
				return false // deadline
			}
		}
	}
}

// notReady returns the names of the required connections that are not READY, within readyTimeout.
func notReady(ctx context.Context, cs []namedConn) []string {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		failed []string
	)
	for _, nc := range cs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !connReady(ctx, nc.conn) {
				mu.Lock()
				failed = append(failed, nc.name)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	sort.Strings(failed)
	return failed
}

func livenessHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprint(w, "ok")
}

func (fe *frontendServer) readinessHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if failed := notReady(r.Context(), fe.readinessConns()); len(failed) > 0 {
		// The names are our own backends, not user input; no error text from the connections is shown.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, "not ready: %s\n", strings.Join(failed, ", "))
		return
	}
	_, _ = fmt.Fprint(w, "ok")
}

// versionInfo is the body of /version.
type versionInfo struct {
	Service   string `json:"service"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
}

// buildTime is the linker-set obs.BuildTime, else the commit time Go embeds (vcs.time), else "unknown".
func buildTime(readBuildInfo func() (*debug.BuildInfo, bool)) string {
	if t := strings.TrimSpace(obs.BuildTime); t != "" {
		return t
	}
	if bi, ok := readBuildInfo(); ok {
		for _, kv := range bi.Settings {
			if kv.Key == "vcs.time" && kv.Value != "" {
				return kv.Value
			}
		}
	}
	return "unknown"
}

func currentVersionInfo() versionInfo {
	v, c := obs.ResolveBuild(os.Getenv)
	return versionInfo{Service: "frontend", Version: v, Commit: c, BuildTime: buildTime(debug.ReadBuildInfo)}
}

func versionHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(currentVersionInfo())
}

// healthcheck is the container health probe (`frontend healthcheck`): GET /healthz on the own port
// within healthcheckTimeout; exit code 0 for 200, 1 for anything else. The image needs no wget or curl.
func healthcheck(port string, out io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+baseUrl+"/healthz", http.NoBody)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	_, _ = fmt.Fprintln(out, resp.Status)
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// listenPort is the HTTP port: PORT, default 8080.
func listenPort() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return port
}
