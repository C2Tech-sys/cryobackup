// Command fakeconsole is the stand-in console of the MSI smoke test
// (deploy/windows/ci/smoke.ps1): it answers POST /enroll for one token with a
// complete, valid enrollment and every other agent route with 503, so an MSI
// installed with TOKEN and SERVER can enroll and its service can start, on a
// CI runner with no real console. It listens on loopback only, plain http (the
// MSI is installed with CRYO_INSECURE_LAB=1 against it). Never for real use.
//
//	fakeconsole -listen 127.0.0.1:18080
//
// The token is read from $CRYO_FAKE_TOKEN (not argv, which other processes can
// read).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/api"
	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// DeviceID is the device id every enrollment gets.
const DeviceID = "01JB0000000000000000000M51"

func main() {
	listen := flag.String("listen", "127.0.0.1:18080", "loopback address to listen on")
	flag.Parse()
	token := os.Getenv("CRYO_FAKE_TOKEN")
	if token == "" {
		log.Fatal("fakeconsole: CRYO_FAKE_TOKEN is not set")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		log.Fatal("fakeconsole: -listen must be a loopback address")
	}
	var enrolls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc(api.RouteEnroll, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderServerTime, api.FormatServerTime(time.Now()))
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "refused", http.StatusUnauthorized)
			return
		}
		enrolls.Add(1)
		pol := policy.Default(policy.ClassDesktop)
		pol.Selection.Roots, pol.Selection.Presets = []string{`C:\cryo-smoke-nothing-here`}, []string{}
		w.Header().Set("Content-Type", api.ContentTypeJSON)
		_ = json.NewEncoder(w).Encode(api.EnrollResponse{
			DeviceID: DeviceID, CustomerID: "01JB0000000000000000000C01", DeviceToken: "smoke-device-token", Custody: api.CustodyEscrowed,
			Gateway:      api.Gateway{URL: "http://127.0.0.1:1", Username: DeviceID, Password: "smoke-gateway-password"},
			RepoPassword: "smoke-repo-password",
			Policy:       api.PolicyResponse{Policy: pol, PolicyID: "01JB0000000000000000000P01", Version: 1, ETag: `"e1"`},
			ServerTime:   time.Now(),
		})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "ok %d\n", enrolls.Load())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(api.HeaderServerTime, api.FormatServerTime(time.Now()))
		http.Error(w, "fake console", http.StatusServiceUnavailable)
	})
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("fakeconsole: listening on %s", *listen)
	log.Fatal(srv.ListenAndServe())
}
