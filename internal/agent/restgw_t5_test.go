package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// restGateway is a minimal restic REST backend (API v2) kept in memory, with a
// switch: Stop closes the listener and every open connection (the gateway goes
// away mid-run), Start listens again on the same address with the same
// objects (the gateway comes back). It stands in for "rclone serve restic" in
// the live resume test, which has no rclone dependency this way.
type restGateway struct {
	t    *testing.T
	addr string

	mu   sync.Mutex
	objs map[string][]byte // "config", or "<type>/<name>"
	srv  *http.Server

	// DataSaves counts completed uploads of data packs.
	DataSaves atomic.Int64
	// StopAfterData, when above zero, stops the gateway as soon as that many
	// data packs are stored (once). The pack that makes the count is stored.
	StopAfterData atomic.Int64
	stopped       atomic.Bool
	// OnSave, when set before the first request, is called after each stored
	// object with its key ("data/<name>", "index/<name>", ...).
	OnSave func(key string)
}

// restTypes are the object types of the REST protocol.
var restTypes = map[string]bool{"data": true, "keys": true, "locks": true, "snapshots": true, "index": true}

const restV2 = "application/vnd.x.restic.rest.v2"

func newRestGateway(t *testing.T) *restGateway {
	t.Helper()
	g := &restGateway{t: t, objs: map[string][]byte{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g.addr = ln.Addr().String()
	g.serve(ln)
	t.Cleanup(g.Stop)
	return g
}

// Repository is the value of RESTIC_REPOSITORY for this gateway.
func (g *restGateway) Repository() string { return "rest:http://" + g.addr + "/" }

func (g *restGateway) serve(ln net.Listener) {
	srv := &http.Server{Handler: http.HandlerFunc(g.handle), ReadHeaderTimeout: 10 * time.Second}
	g.mu.Lock()
	g.srv = srv
	g.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
}

// Stop takes the gateway down: the listener and every connection close at once.
func (g *restGateway) Stop() {
	g.mu.Lock()
	srv := g.srv
	g.srv = nil
	g.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// Start brings the gateway back on the same address.
func (g *restGateway) Start() {
	g.t.Helper()
	g.mu.Lock()
	running := g.srv != nil
	g.mu.Unlock()
	if running {
		return
	}
	var ln net.Listener
	var err error
	for i := 0; i < 100; i++ {
		if ln, err = net.Listen("tcp", g.addr); err == nil {
			g.serve(ln)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	g.t.Fatalf("the gateway could not listen again: %v", err)
}

// Objects returns the number of stored objects of a type.
func (g *restGateway) Objects(typ string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for k := range g.objs {
		if strings.HasPrefix(k, typ+"/") {
			n++
		}
	}
	return n
}

func (g *restGateway) handle(w http.ResponseWriter, r *http.Request) {
	p := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(p, "/")
	switch {
	case p == "" && r.Method == http.MethodPost && r.URL.Query().Get("create") == "true":
		w.WriteHeader(http.StatusOK)
	case p == "config":
		g.object(w, r, "config")
	case len(parts) == 1 && restTypes[p] && r.Method == http.MethodGet:
		g.list(w, r, p)
	case len(parts) == 2 && restTypes[parts[0]]:
		g.object(w, r, p)
	default:
		http.NotFound(w, r)
	}
}

func (g *restGateway) object(w http.ResponseWriter, r *http.Request, key string) {
	switch r.Method {
	case http.MethodHead, http.MethodGet:
		g.mu.Lock()
		b, ok := g.objs[key]
		g.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
	case http.MethodPost:
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return // the upload was cut: nothing is stored
		}
		g.mu.Lock()
		g.objs[key] = b
		g.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		if g.OnSave != nil {
			g.OnSave(key)
		}
		if strings.HasPrefix(key, "data/") {
			n := g.DataSaves.Add(1)
			if lim := g.StopAfterData.Load(); lim > 0 && n >= lim && g.stopped.CompareAndSwap(false, true) {
				go g.Stop()
			}
		}
	case http.MethodDelete:
		g.mu.Lock()
		delete(g.objs, key)
		g.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (g *restGateway) list(w http.ResponseWriter, r *http.Request, typ string) {
	type entry struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	g.mu.Lock()
	var all []entry
	for k, v := range g.objs {
		if name, ok := strings.CutPrefix(k, typ+"/"); ok {
			all = append(all, entry{name, int64(len(v))})
		}
	}
	g.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	if strings.Contains(r.Header.Get("Accept"), restV2) {
		w.Header().Set("Content-Type", restV2)
		if all == nil {
			all = []entry{}
		}
		_ = json.NewEncoder(w).Encode(all)
		return
	}
	names := []string{}
	for _, e := range all {
		names = append(names, e.Name)
	}
	w.Header().Set("Content-Type", "application/vnd.x.restic.rest.v1")
	_ = json.NewEncoder(w).Encode(names)
}
