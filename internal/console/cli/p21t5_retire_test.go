package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// P21-T5 (operator ruling 2026-11-23), in the real serve wiring: a retire whose wait ends with a
// read of the file in flight answers with the newest read that finished during the wait, and the
// audit row names the same cause. The real App, its real file and its real check; the sync is not
// signalled, so the login stays in the file, and the test ends the wait (through the request's
// context, the parent of the wait's) while the second read is held, not on a timer.
func TestRetireAnswersTheNewestFinishedReadInRealServeWiring(t *testing.T) {
	app, _ := buildFromInit(t, "America/Chicago", "America/Chicago")
	ctx, end := context.WithCancel(context.Background())
	defer end()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var reads atomic.Int32
	// Set before anything is served: the handler reads these fields.
	app.Admin.GatewaySyncWait = time.Minute // only end() ends the wait
	app.Admin.SyncGateway = func() {}
	app.Admin.CheckGatewayLogins = func(rctx context.Context) jobs.LoginFileCheck {
		if reads.Add(1) == 1 {
			return app.Htpasswd.Check(rctx)
		}
		end() // the second read is in flight when the wait ends
		<-release
		return jobs.LoginFileCheck{}
	}
	srv := httptest.NewServer(app.Server.AdminRouter)
	t.Cleanup(srv.Close)
	w := &retireWorld{t: t, app: app, c: r3SignIn(t, app, srv.URL), dir: app.Htpasswd.Dir, tok: "enroll-secret-p21t5"}
	cust, err := app.Store.CreateCustomer(context.Background(), store.ScopeAll(), store.Customer{Name: "Newest Read Customer"})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(w.tok))
	if _, err := app.Store.CreateEnrollmentToken(context.Background(), store.ScopeAll(), store.EnrollmentToken{CustomerID: cust.ID, TokenHash: hex.EncodeToString(sum[:]), ExpiresAt: app.clock().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	w.cust = cust
	id := w.enroll("NEWEST-PC-01")
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := app.Htpasswd.Run(context.Background()); err != nil { // the runner is not started: nothing else touches the file
		t.Fatal(err)
	}
	if !w.inFile(w.dir, id) {
		t.Fatal("the login never reached the file")
	}

	req := httptest.NewRequest("POST", srv.URL+"/api/v1/devices/"+id+"/retire", strings.NewReader(`{"reason":"compromised endpoint"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL)
	req.Header.Set(auth.CSRFHeader, w.c.csrf)
	for _, ck := range w.c.cs {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	app.Server.AdminRouter.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("retire: %d %s", rec.Code, rec.Body.Bytes())
	}
	var got adminapi.DeviceRetireResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Retired || got.GatewayLogin != adminapi.GatewayLoginPending || got.GatewayLoginReason != "still_in_file" {
		t.Fatalf("answer: %s/%s, want pending/still_in_file (the first read's; not check_timed_out)", got.GatewayLogin, got.GatewayLoginReason)
	}
	if n := reads.Load(); n != 2 {
		t.Fatalf("%d reads began, want 2: the wait did not end at the held read", n)
	}
	if !w.inFile(w.dir, id) {
		t.Fatal("the test proves nothing: the login left the file")
	}
	if d := w.auditDetails(id); len(d) != 1 || !strings.Contains(d[0], `"gateway_login":"pending"`) || !strings.Contains(d[0], `"reason":"still_in_file"`) {
		t.Errorf("audit: %v", d)
	}
}
