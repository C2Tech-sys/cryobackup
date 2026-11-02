package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

func loadedFixtures(t *testing.T) (*Store, *fakeClock) {
	t.Helper()
	s, clk := newStore(t)
	ok(t, LoadFixtures(context.Background(), s, clk.Now(), WithAdminPasswordHash("$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA")))
	return s, clk
}

// The fixtures, read back through the facts loader and state.Evaluate, give the
// primary state each device was built for, and the fleet is never all clear.
func TestFixturesEvaluateToTheIntendedStates(t *testing.T) {
	s, clk := loadedFixtures(t)
	ctx := context.Background()
	world := must[state.World](t)(s.LoadWorld(ctx, ScopeAll(), LoadOptions{}))
	if !world.LoadComplete || world.LoadErrors != 0 {
		t.Fatalf("fixtures did not load completely: complete=%v errors=%d", world.LoadComplete, world.LoadErrors)
	}
	if len(world.Customers) != 6 || len(world.Devices) != 26 || !world.FleetEverEnrolled {
		t.Fatalf("%d customers, %d devices, ever enrolled %v", len(world.Customers), len(world.Devices), world.FleetEverEnrolled)
	}

	check := func(label string, now time.Time) {
		view := state.Evaluate(world, now)
		got := map[string]state.State{}
		for _, d := range view.Devices {
			got[d.Hostname] = d.Primary
		}
		for _, want := range FixtureExpectations() {
			if got[want.Host] != want.Primary {
				t.Errorf("%s: %s (%s) is %s, want %s", label, want.Host, want.Customer, got[want.Host], want.Primary)
			}
		}
		if len(got) != len(FixtureExpectations()) {
			t.Errorf("%s: %d devices evaluated, %d expected", label, len(got), len(FixtureExpectations()))
		}
		for _, c := range view.Customers {
			if want, ok := FixtureCustomerExpectations()[c.Name]; !ok || c.Status != want {
				t.Errorf("%s: customer %s is %s, want %s", label, c.Name, c.Status, want)
			}
		}
		if view.AllClear() {
			t.Errorf("%s: a fleet with failures reported all clear", label)
		}
		if !view.Complete || len(view.Banners) != 0 {
			t.Errorf("%s: complete=%v banners=%+v; the fixtures load cleanly", label, view.Complete, view.Banners)
		}
	}
	check("at load time", clk.Now())
	// The board must still show the intended states when it is rendered a while after loading
	// (a long-running fixtures console): the margins are hours against day-scale thresholds.
	check("two hours later", clk.Now().Add(2*time.Hour))
}

func TestFixturesCoverTheStatesAndAttributesThatMatter(t *testing.T) {
	s, clk := loadedFixtures(t)
	world := must[state.World](t)(s.LoadWorld(context.Background(), ScopeAll(), LoadOptions{}))
	view := state.Evaluate(world, clk.Now())
	states := map[state.State]bool{}
	attrs := map[state.Attr]bool{}
	byHost := map[string]state.DeviceState{}
	for _, d := range view.Devices {
		states[d.Primary] = true
		byHost[d.Hostname] = d
		for _, a := range d.Attrs {
			attrs[a] = true
		}
	}
	// Every reachable state; UNKNOWN needs a failed load and is covered by the loader tests.
	for _, st := range state.Precedence() {
		if st == state.StateUnknown {
			continue
		}
		if !states[st] {
			t.Errorf("no fixture device is %s", st)
		}
	}
	for _, a := range []state.Attr{state.AttrSnoozed, state.AttrVSSFallback, state.AttrPartialUnderTolerance, state.AttrCustomerHeldKey, state.AttrPolicyDrift} {
		if !attrs[a] {
			t.Errorf("no fixture device carries the %s attribute", a)
		}
	}
	// Detail the design pages show: the failing device, the deferred reasons, the pending grace.
	if d := byHost["FRONTDESK-01"]; d.Detail.ConsecutiveFailures != 3 || d.Detail.ErrorClass != "vss" || len(d.Detail.VSSVolumes) != 1 {
		t.Errorf("FRONTDESK-01 detail = %+v", d.Detail)
	}
	if d := byHost["LT-BILLING-1"]; d.Reason != state.ReasonDeferredBattery {
		t.Errorf("LT-BILLING-1 reason = %s", d.Reason)
	}
	if d := byHost["WAITING-RM-PC"]; d.Reason != state.ReasonDeferredGateway {
		t.Errorf("WAITING-RM-PC reason = %s", d.Reason)
	}
	if d := byHost["NEW-EXAM-6"]; d.Detail.GraceDeadline.Sub(clk.Now()) != 41*time.Hour {
		t.Errorf("NEW-EXAM-6 grace left = %v, want 41h", d.Detail.GraceDeadline.Sub(clk.Now()))
	}
	if d := byHost["SCAN-STATION-1"]; d.Reason != state.ReasonNoSuccessfulRun {
		t.Errorf("SCAN-STATION-1 reason = %s", d.Reason)
	}
	if d := byHost["KIOSK-WAIT-01"]; d.Reason != state.ReasonNeverReported {
		t.Errorf("KIOSK-WAIT-01 reason = %s", d.Reason)
	}
	if d := byHost["XRAY-OLD-01"]; d.Detail.PauseReason != "awaiting replacement" || d.Detail.PauseUntil.IsZero() {
		t.Errorf("XRAY-OLD-01 pause detail = %+v", d.Detail)
	}
	if d := byHost["DR-OFFICE-PC"]; d.Detail.OpenRunAge != 25*time.Minute || d.Detail.OpenRunLimit != 24*time.Hour {
		t.Errorf("DR-OFFICE-PC open run %v / limit %v (the device override raises max_run_hours to 24)", d.Detail.OpenRunAge, d.Detail.OpenRunLimit)
	}
}

func TestFixturesRowsAreWhatTheyShouldBe(t *testing.T) {
	s, clk := loadedFixtures(t)
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		query string
		want  int
	}{
		{"storage targets", `SELECT count(*) FROM storage_targets`, 1},
		{"default policy", `SELECT count(*) FROM policies WHERE scope = 'default'`, 1},
		{"customers", `SELECT count(*) FROM customers`, 6},
		{"expected customers with no devices", `SELECT count(*) FROM customers c WHERE expected = 1 AND NOT EXISTS (SELECT 1 FROM devices d WHERE d.customer_id = c.id)`, 2},
		{"devices", `SELECT count(*) FROM devices`, 26},
		{"device tokens", `SELECT count(*) FROM device_tokens`, 26},
		{"gateway logins (one per device, retired one revoked)", `SELECT count(*) FROM gateway_logins`, 26},
		{"active gateway logins", `SELECT count(*) FROM gateway_logins WHERE revoked_at IS NULL`, 25},
		{"escrow rows", `SELECT count(*) FROM repo_key_escrow`, 26},
		{"customer-held escrow rows", `SELECT count(*) FROM repo_key_escrow WHERE custody = 'customer_held'`, 3},
		{"maintenance rows", `SELECT count(*) FROM maintenance_schedule`, 26},
		{"admins", `SELECT count(*) FROM admin_users`, 1},
		{"open backup runs", `SELECT count(*) FROM runs WHERE kind = 'backup' AND outcome IS NULL`, 2},
		{"deferred gateway runs", `SELECT count(*) FROM runs WHERE deferred_reason = 'gateway'`, 1},
		{"rows with a status column", `SELECT count(*) FROM pragma_table_info('devices') WHERE name LIKE '%status%' OR name LIKE '%state%'`, 0},
	} {
		var n int
		ok(t, s.Read(ctx, func(tx *Tx) error { return tx.QueryRow(c.query).Scan(&n) }))
		if n != c.want {
			t.Errorf("%s = %d, want %d", c.name, n, c.want)
		}
	}
	var presence, ring, snaps, nonMillis int
	ok(t, s.Read(ctx, func(tx *Tx) error {
		if err := tx.QueryRow(`SELECT count(*) FROM device_presence`).Scan(&presence); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT count(*) FROM heartbeats`).Scan(&ring); err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT count(*) FROM snapshots`).Scan(&snaps); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT count(*) FROM runs WHERE started_at < 1000000000000 OR started_at > ?`, clk.Now().UnixMilli()+1).Scan(&nonMillis)
	}))
	if presence != 24 || ring == 0 || snaps == 0 || nonMillis != 0 { // 26 devices minus the retired one and the never-confirmed kiosk
		t.Errorf("presence=%d ring=%d snapshots=%d runs-in-the-future-or-seconds=%d", presence, ring, snaps, nonMillis)
	}

	// The admin gets the hash the caller passed; the store never hashes anything.
	a := must[Admin](t)(s.GetAdminByUsername(ctx, FixtureAdminUsername))
	if a.PasswordHash != "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA" || a.Role != "owner" {
		t.Errorf("admin = %+v", a)
	}
	// htpasswd input excludes the retired device's login.
	if entries := must[[]HtpasswdEntry](t)(s.ListGatewayLoginsForHtpasswd(ctx)); len(entries) != 25 {
		t.Errorf("%d htpasswd entries, want 25", len(entries))
	}
	// The effective policy of a Summit device composes the customer policy and the override.
	devs := must[Listing[Device]](t)(s.ListDevices(ctx, ScopeAll(), ListDevicesOptions{Search: "DR-OFFICE-PC"}))
	if len(devs.Items) != 1 {
		t.Fatalf("DR-OFFICE-PC not found: %+v", devs.Items)
	}
	p, ver, _, err := s.EffectivePolicyFor(ctx, ScopeAll(), devs.Items[0].ID)
	ok(t, err)
	if p.Schedule.Window != "Mon-Fri/7-19;Sat/8-14" || p.SLA.MaxRunHours != 24 || ver != 1 {
		t.Errorf("effective policy of DR-OFFICE-PC: window %q, max_run_hours %d, version %d", p.Schedule.Window, p.SLA.MaxRunHours, ver)
	}
}

// Fixtures are fictional: the design page's names, nothing operator-specific, no secrets, no paths beyond snapshot roots.
func TestFixturesAreFictionalAndSecretFree(t *testing.T) {
	s, _ := loadedFixtures(t)
	ctx := context.Background()
	customers := must[[]Customer](t)(s.ListCustomers(ctx, ScopeAll()))
	var names []string
	for _, c := range customers {
		names = append(names, c.Name)
	}
	for _, want := range []string{"Summit Eye Care", "Harbor Family Practice", "Northwind Dental", "Pinecrest Legal", "Blue Ridge Accounting"} {
		if !strings.Contains(strings.Join(names, "|"), want) {
			t.Errorf("customer %q from the design page is missing: %v", want, names)
		}
	}
	for _, host := range []string{"LAB-RETINA-02", "FRONTDESK-01", "LT-OPTOM-04", "RECEPTION-2", "DR-OFFICE-PC", "NEW-EXAM-6", "LT-BILLING-1", "XRAY-OLD-01"} {
		l := must[Listing[Device]](t)(s.ListDevices(ctx, ScopeAll(), ListDevicesOptions{Search: host, IncludeRetired: true}))
		if len(l.Items) != 1 {
			t.Errorf("host %s from the design page: %d matches", host, len(l.Items))
		}
	}
	// Scan every text column of every table for operator-specific strings.
	forbidden := []string{"c2tech", "c2 tech", "cullman", "shirley", "drakkar", "gmail", "wasabisys.com", "amazonaws"}
	var tables []string
	ok(t, s.Read(ctx, func(tx *Tx) error {
		var err error
		tables, err = collect(tx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`, nil, func(sc scanner) (string, error) {
			var n string
			return n, sc.Scan(&n)
		})
		return err
	}))
	for _, tb := range tables {
		ok(t, s.Read(ctx, func(tx *Tx) error {
			cols, err := collect(tx, `SELECT name FROM pragma_table_info('`+tb+`')`, nil, func(sc scanner) (string, error) {
				var n string
				return n, sc.Scan(&n)
			})
			if err != nil {
				return err
			}
			for _, col := range cols {
				for _, f := range forbidden {
					var n int
					q := `SELECT count(*) FROM "` + tb + `" WHERE CAST("` + col + `" AS TEXT) LIKE ?`
					if err := tx.QueryRow(q, "%"+f+"%").Scan(&n); err != nil {
						return err
					}
					if n > 0 {
						t.Errorf("%s.%s contains %q", tb, col, f)
					}
				}
			}
			return nil
		}))
	}
	// Every credential-shaped column holds a hash, a wrapped blob or an unusable marker, never a usable secret.
	var tokenHashes, plainBcrypt int
	ok(t, s.Read(ctx, func(tx *Tx) error {
		if err := tx.QueryRow(`SELECT count(DISTINCT token_hash) FROM device_tokens`).Scan(&tokenHashes); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT count(*) FROM gateway_logins WHERE pending_secret_wrapped IS NOT NULL`).Scan(&plainBcrypt)
	}))
	if tokenHashes != 26 {
		t.Errorf("device token hashes are not all distinct random values: %d", tokenHashes)
	}
	if plainBcrypt != 0 {
		t.Errorf("%d fixture gateway logins carry a pending secret", plainBcrypt)
	}
	// Without a supplied hash nobody can log in.
	s2, _ := newStore(t)
	ok(t, LoadFixtures(ctx, s2, t0))
	if a := must[Admin](t)(s2.GetAdminByUsername(ctx, FixtureAdminUsername)); a.PasswordHash != "!" {
		t.Errorf("default admin hash = %q, want the unusable marker", a.PasswordHash)
	}
}

// The scope matrix holds on fixtures too: a scoped admin sees only its customer's devices and facts.
func TestFixturesRespectScope(t *testing.T) {
	s, clk := loadedFixtures(t)
	ctx := context.Background()
	var pinecrest Customer
	for _, c := range must[[]Customer](t)(s.ListCustomers(ctx, ScopeAll())) {
		if c.Name == "Pinecrest Legal" {
			pinecrest = c
		}
	}
	scope := ScopeOf(pinecrest.ID)
	world := must[state.World](t)(s.LoadWorld(ctx, scope, LoadOptions{}))
	if len(world.Customers) != 1 || len(world.Devices) != 3 {
		t.Fatalf("scoped world: %d customers, %d devices", len(world.Customers), len(world.Devices))
	}
	view := state.Evaluate(world, clk.Now())
	if len(view.Customers) != 1 || view.Customers[0].Status != state.CustomerOK || !view.AllClear() {
		// A healthy scoped slice of the fleet is legitimately all clear (it has devices, no banners, nothing alarming).
		t.Fatalf("scoped view: %+v allclear=%v", view.Customers, view.AllClear())
	}
	runs := must[Listing[Run]](t)(s.ListRuns(ctx, scope, "", Page{Limit: 1000}))
	if len(runs.Items) == 0 {
		t.Fatal("no runs visible to the scoped admin")
	}
	devs := must[Listing[Device]](t)(s.ListDevices(ctx, scope, ListDevicesOptions{IncludeRetired: true, Page: Page{Limit: 1000}}))
	if len(devs.Items) != 3 {
		t.Fatalf("scoped device list = %d", len(devs.Items))
	}
}

func TestFixturesCannotBeLoadedTwice(t *testing.T) {
	s, clk := loadedFixtures(t)
	if err := LoadFixtures(context.Background(), s, clk.Now()); err == nil {
		t.Fatal("loading the fixtures twice succeeded")
	}
	var n int
	ok(t, s.Read(context.Background(), func(tx *Tx) error { return tx.QueryRow(`SELECT count(*) FROM customers`).Scan(&n) }))
	if n != 6 {
		t.Fatalf("a failed second load changed the data: %d customers", n)
	}
}
