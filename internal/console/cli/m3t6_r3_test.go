package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminapi"
	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M3-T6 round 3: the reviewer's two reproductions, in the real serve wiring. Both
// happen inside a few milliseconds, so they run many times. CRYO_RETIRE_TRIALS sets how
// many; the suite runs a small number to stay quick, and the evidence run used 400.
func retireTrials(t *testing.T) int {
	t.Helper()
	if v := os.Getenv("CRYO_RETIRE_TRIALS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("CRYO_RETIRE_TRIALS=%q", v)
		}
		return n
	}
	return 40
}

// r3World is a real App with its job runner running and n enrolled devices, every login in the file.
func r3World(t *testing.T, n int) (*retireWorld, []string) {
	t.Helper()
	app, _ := buildFromInit(t, "America/Chicago", "America/Chicago")
	ctx, cancel := context.WithCancel(context.Background())
	app.Runner.Start(ctx)
	t.Cleanup(func() { cancel(); app.Runner.Stop() })
	srv := httptest.NewServer(app.Server.AdminRouter)
	t.Cleanup(srv.Close)
	w := &retireWorld{t: t, app: app, c: r3SignIn(t, app, srv.URL), dir: app.Htpasswd.Dir, tok: "enroll-secret-r3"}
	cust, err := app.Store.CreateCustomer(ctx, store.ScopeAll(), store.Customer{Name: "Round Three Customer"})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(w.tok))
	if _, err := app.Store.CreateEnrollmentToken(ctx, store.ScopeAll(), store.EnrollmentToken{CustomerID: cust.ID, TokenHash: hex.EncodeToString(sum[:]), ExpiresAt: app.clock().Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, n)
	for i := range ids {
		ids[i] = w.enroll(fmt.Sprintf("R3-PC-%04d", i))
	}
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	w.waitFor("every login to reach the file", func() bool {
		app.Htpasswd.Sig.Notify()
		in, err := r3InFile(w, ids[n-1])
		return err == nil && in
	})
	return w, ids
}

// r3InFile reads the deployed file as the gateway would. A reader can meet the rename on
// Windows, so it tries again: a real answer is what counts.
func r3InFile(w *retireWorld, id string) (bool, error) {
	var err error
	for i := 0; i < 100; i++ {
		var raw []byte
		if raw, err = os.ReadFile(filepath.Join(w.dir, gateway.FileName)); err == nil {
			var logins []gateway.Login
			if _, _, logins, err = gateway.Parse(raw); err == nil {
				for _, l := range logins {
					if l.Username == id {
						return true, nil
					}
				}
				return false, nil
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false, err
}

// M1: another sync signal landing within a few milliseconds of the retire. On a3ae618 a run
// started in the retire's own millisecond counted as proof, and read the logins before the commit.
func TestRetireNeverSaysRemovedWithTheLoginInTheFileWhenASyncSignalLandsBesideIt(t *testing.T) {
	n := retireTrials(t)
	w, ids := r3World(t, n)
	removed, pending, falseRemoved := 0, 0, 0
	for _, id := range ids {
		type ans struct {
			code int
			out  adminapi.DeviceRetireResponse
			body []byte
			in   bool
			err  error
		}
		ch := make(chan ans, 1)
		go func() {
			code, b := w.api("POST", "/api/v1/devices/"+id+"/retire", adminapi.DeviceRetireRequest{Reason: "lab"})
			var out adminapi.DeviceRetireResponse
			_ = json.Unmarshal(b, &out)
			in, err := r3InFile(w, id)
			ch <- ans{code, out, b, in, err}
		}()
		end := time.Now().Add(time.Duration(rand.Int64N(int64(6 * time.Millisecond))))
		for time.Now().Before(end) {
		}
		w.app.Htpasswd.Sig.Notify()
		a := <-ch
		if a.code != 200 {
			t.Fatalf("retire: %d %s", a.code, a.body)
		}
		if a.err != nil {
			t.Fatalf("the file could not be read: %v", a.err)
		}
		if a.out.GatewayLogin == adminapi.GatewayLoginRemoved {
			removed++
			if a.in {
				falseRemoved++
				t.Errorf("%s answered removed with the login in the file", id)
			}
		} else {
			pending++
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("trials=%d removed=%d pending=%d falseRemoved=%d", n, removed, pending, falseRemoved)
	if removed == 0 {
		t.Error("no retire answered removed: the test exercised nothing")
	}
}

// M2: Run now overlapping the signalled run. On a3ae618 an older snapshot could be written last,
// so the login came back with the job reading ok and no banner.
func TestRetireHoldsWhenRunNowOverlapsTheSignalledRun(t *testing.T) {
	n := retireTrials(t)
	w, ids := r3World(t, n)
	ctx := context.Background()
	falseAtAnswer, cameBack, removed := 0, 0, 0
	for _, id := range ids {
		done := make(chan struct{})
		go func() { _, _ = w.app.Runner.RunOnce(ctx, "htpasswd-sync"); close(done) }()
		end := time.Now().Add(time.Duration(rand.Int64N(int64(8 * time.Millisecond))))
		for time.Now().Before(end) {
		}
		code, b := w.api("POST", "/api/v1/devices/"+id+"/retire", adminapi.DeviceRetireRequest{Reason: "lab"})
		if code != 200 {
			t.Fatalf("retire: %d %s", code, b)
		}
		var out adminapi.DeviceRetireResponse
		_ = json.Unmarshal(b, &out)
		atAnswer, err := r3InFile(w, id)
		if err != nil {
			t.Fatal(err)
		}
		<-done
		time.Sleep(30 * time.Millisecond)
		later, err := r3InFile(w, id)
		if err != nil {
			t.Fatal(err)
		}
		if out.GatewayLogin == adminapi.GatewayLoginRemoved {
			removed++
			if atAnswer {
				falseAtAnswer++
				t.Errorf("%s answered removed with the login in the file", id)
			}
			if later {
				cameBack++
				t.Errorf("%s answered removed and its login came back after the runs ended (C17=%q)", id, w.banner("C17"))
			}
		}
	}
	t.Logf("trials=%d removed=%d falseAtAnswer=%d cameBack=%d", n, removed, falseAtAnswer, cameBack)
	if removed == 0 {
		t.Error("no retire answered removed: the test exercised nothing")
	}
}
