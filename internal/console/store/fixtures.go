package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// FixtureOption configures LoadFixtures.
type FixtureOption func(*fixtureConfig)

type fixtureConfig struct{ adminHash string }

// WithAdminPasswordHash sets the argon2id PHC hash of the fixture admin's
// password (hashing is the console's job; the store only keeps what it is
// given). Without it the admin gets the unusable hash "!", so nobody can log in.
func WithAdminPasswordHash(hash string) FixtureOption {
	return func(c *fixtureConfig) { c.adminHash = hash }
}

// FixtureAdminUsername is the account LoadFixtures creates.
const FixtureAdminUsername = "admin"

// FixtureExpectation is the intended primary state of one fixture device.
type FixtureExpectation struct {
	Customer string
	Host     string
	Primary  state.State
}

// FixtureExpectations lists every fixture device with the primary state the
// precedence table must give it. The fixtures test asserts each row; the
// console's --fixtures screens and tests can rely on the same table.
func FixtureExpectations() []FixtureExpectation {
	return append([]FixtureExpectation(nil), fixtureExpected...)
}

// FixtureCustomerExpectations maps each fixture customer to its rollup status.
func FixtureCustomerExpectations() map[string]state.CustomerStatus {
	return map[string]state.CustomerStatus{
		"Summit Eye Care":        state.CustomerAlarm,
		"Harbor Family Practice": state.CustomerWarn,
		"Northwind Dental":       state.CustomerOK,
		"Pinecrest Legal":        state.CustomerOK,
		"Blue Ridge Accounting":  state.CustomerMissing,
		"Lakeshore Pediatrics":   state.CustomerOnboarding,
	}
}

// fxRun scripts one run. For a finished run, ago is the finish time before now
// and dur its length; for an open run (outcome "") ago is the start time.
type fxRun struct {
	ago      time.Duration
	dur      time.Duration
	outcome  string
	kind     string // default backup
	origin   string // default agent
	errClass string
	deferred string
	failed   int64
	total    int64
	counted  bool // stats carry file counts
	vss      []string
	counts   map[string]int
}

type fxDevice struct {
	host, customer string
	class, os      string
	agent, target  string
	enrolledAgo    time.Duration
	unconfirmed    bool           // no heartbeat ever confirmed the enrollment
	seenAgo        *time.Duration // nil: no presence row
	serviceState   string
	progress       string        // JSON for device_presence.progress
	lastSuccessAgo time.Duration // 0: no success; a daily success history follows it
	runs           []fxRun
	firstDueIn     *time.Duration // override of first_backup_due_by (relative to now)
	pausedAgo      time.Duration
	pausedUntilIn  time.Duration
	pauseReason    string
	retiredAgo     time.Duration
	replacedBy     string
	identityAgo    time.Duration
	escrowVerified *time.Duration // ago; nil: never verified
	skipForget     bool           // newly enrolled: no maintenance history yet
	appliedPolicy  bool           // policy_version_applied = effective version
	override       string         // device policy override patch
	ackSnooze      time.Duration  // snooze for this long from now (adds an alarm_ack)
}

func dur(d time.Duration) *time.Duration { return &d }

const (
	hr  = time.Hour
	day = 24 * time.Hour
)

var fixtureExpected = []FixtureExpectation{
	{"Summit Eye Care", "LAB-RETINA-02", state.StateUnseen},
	{"Summit Eye Care", "FRONTDESK-01", state.StateFailed},
	{"Summit Eye Care", "LT-OPTOM-04", state.StateStale},
	{"Summit Eye Care", "RECEPTION-2", state.StatePartial},
	{"Summit Eye Care", "DR-OFFICE-PC", state.StateOK},
	{"Summit Eye Care", "EXAM-ROOM-3", state.StateStuck},
	{"Summit Eye Care", "SCAN-STATION-1", state.StateBlank},
	{"Summit Eye Care", "KIOSK-WAIT-01", state.StateBlank},
	{"Summit Eye Care", "TECH-BENCH-07", state.StateEscrowUnverified},
	{"Summit Eye Care", "PRINT-SRV-01", state.StateMaintFailed},
	{"Summit Eye Care", "LT-SURGERY-02", state.StateMissed},
	{"Summit Eye Care", "FRONTDESK-02", state.StateIdentityConflict},
	{"Summit Eye Care", "OLD-RECEPTION-PC", state.StateRetired},
	{"Summit Eye Care", "DR-LAPTOP-03", state.StateOutdated},
	{"Harbor Family Practice", "NEW-EXAM-6", state.StatePendingFirst},
	{"Harbor Family Practice", "LT-BILLING-1", state.StateDeferred},
	{"Harbor Family Practice", "WAITING-RM-PC", state.StateDeferred},
	{"Harbor Family Practice", "FRONTDESK-H1", state.StateOK},
	{"Harbor Family Practice", "EXAM-H2", state.StateOK},
	{"Harbor Family Practice", "BILLING-H3", state.StateOK},
	{"Northwind Dental", "XRAY-OLD-01", state.StatePaused},
	{"Northwind Dental", "OPS-ND-01", state.StateOK},
	{"Northwind Dental", "HYGIENE-PC-2", state.StateOK},
	{"Pinecrest Legal", "LEGAL-PC-01", state.StateOK},
	{"Pinecrest Legal", "LEGAL-PC-02", state.StateOK},
	{"Pinecrest Legal", "PARTNER-LT-02", state.StateOK},
}

// fixtureDevices scripts the devices. Every offset is relative to now, with
// margins of hours against the day-scale thresholds so the intended state still
// holds when the fixtures console has been running for a while.
func fixtureDevices() []fxDevice {
	w10, w11 := "Windows 10 Pro", "Windows 11 Pro"
	verified := dur(5 * day)
	var ok = func(host, customer, class, osv string, ago time.Duration) fxDevice {
		return fxDevice{host: host, customer: customer, class: class, os: osv, agent: "1.4.2", enrolledAgo: 90 * day,
			seenAgo: dur(40 * time.Second), serviceState: "idle", lastSuccessAgo: ago, escrowVerified: verified, appliedPolicy: true}
	}
	d := []fxDevice{}
	add := func(x fxDevice) { d = append(d, x) }

	// ---- Summit Eye Care: every state across its devices -------------------------------
	x := ok("LAB-RETINA-02", "Summit Eye Care", "desktop", w10, 11*day)
	x.seenAgo = dur(9 * day)
	add(x)

	x = ok("FRONTDESK-01", "Summit Eye Care", "desktop", w11, 70*hr)
	x.runs = []fxRun{
		{ago: 50 * hr, dur: 2 * time.Minute, outcome: "failed", errClass: "vss", vss: []string{"C"}},
		{ago: 26 * hr, dur: 2 * time.Minute, outcome: "failed", errClass: "vss", vss: []string{"C"}},
		{ago: 2 * hr, dur: 2 * time.Minute, outcome: "failed", errClass: "vss", vss: []string{"C"}},
	}
	add(x)

	x = ok("LT-OPTOM-04", "Summit Eye Care", "laptop", w11, 96*hr)
	x.ackSnooze = 24 * hr
	add(x)

	x = ok("RECEPTION-2", "Summit Eye Care", "desktop", w11, 30*hr)
	x.runs = []fxRun{{ago: 6 * hr, dur: 15 * time.Minute, outcome: "partial", errClass: "permission",
		failed: 23, total: 2000, counted: true, counts: map[string]int{"permission": 23}}}
	add(x)

	x = ok("DR-OFFICE-PC", "Summit Eye Care", "desktop", w11, 22*hr)
	x.serviceState = "running"
	x.progress = `{"percent_done":0.62,"files_done":23418,"total_files":37902,"bytes_done":4400000000}`
	x.runs = []fxRun{{ago: 25 * time.Minute}}
	x.override = `{"sla":{"max_run_hours":24}}`
	add(x)

	x = ok("EXAM-ROOM-3", "Summit Eye Care", "desktop", w10, 48*hr)
	x.serviceState = "running"
	x.runs = []fxRun{{ago: 40 * hr}}
	add(x)

	x = ok("SCAN-STATION-1", "Summit Eye Care", "desktop", w10, 0)
	x.enrolledAgo = 5 * day
	x.skipForget = true
	add(x)

	x = ok("KIOSK-WAIT-01", "Summit Eye Care", "desktop", w11, 0)
	x.enrolledAgo = 4 * day
	x.unconfirmed = true
	x.seenAgo = nil
	x.skipForget = true
	add(x)

	x = ok("TECH-BENCH-07", "Summit Eye Care", "desktop", w11, 5*hr)
	x.enrolledAgo = 40 * day
	x.escrowVerified = nil
	add(x)

	x = ok("PRINT-SRV-01", "Summit Eye Care", "desktop", "Windows Server 2022", 4*hr)
	x.runs = []fxRun{{ago: 2 * day, dur: 20 * time.Minute, outcome: "failed", kind: "prune", origin: "console", errClass: "storage"}}
	add(x)

	x = ok("LT-SURGERY-02", "Summit Eye Care", "laptop", w11, 60*hr)
	add(x)

	x = ok("FRONTDESK-02", "Summit Eye Care", "desktop", w11, 6*hr)
	x.identityAgo = 2 * hr
	add(x)

	x = ok("OLD-RECEPTION-PC", "Summit Eye Care", "desktop", w10, 21*day)
	x.seenAgo = nil
	x.retiredAgo = 20 * day
	x.replacedBy = "RECEPTION-2"
	x.enrolledAgo = 400 * day
	add(x)

	x = ok("DR-LAPTOP-03", "Summit Eye Care", "laptop", w11, 3*hr)
	x.target = "1.5.0"
	x.appliedPolicy = false
	add(x)

	// ---- Harbor Family Practice: pending first, deferred, tolerated partial ------------
	x = ok("NEW-EXAM-6", "Harbor Family Practice", "desktop", w11, 0)
	x.enrolledAgo = 31 * hr
	x.firstDueIn = dur(41 * hr)
	x.escrowVerified = dur(30 * hr)
	x.skipForget = true
	add(x)

	x = ok("LT-BILLING-1", "Harbor Family Practice", "laptop", w11, 19*hr)
	x.serviceState = "deferred"
	x.runs = []fxRun{{ago: 1 * hr, dur: 0, outcome: "deferred", deferred: "battery"}}
	add(x)

	x = ok("WAITING-RM-PC", "Harbor Family Practice", "desktop", w11, 27*hr)
	x.serviceState = "deferred"
	x.runs = []fxRun{{ago: 3 * hr, dur: 0, outcome: "deferred", deferred: "gateway", errClass: "network"}}
	add(x)

	add(ok("FRONTDESK-H1", "Harbor Family Practice", "desktop", w11, 5*hr))

	x = ok("EXAM-H2", "Harbor Family Practice", "desktop", w11, 32*hr)
	x.runs = []fxRun{{ago: 8 * hr, dur: 14 * time.Minute, outcome: "partial", errClass: "permission",
		failed: 4, total: 31000, counted: true, counts: map[string]int{"permission": 4}}}
	add(x)

	x = ok("BILLING-H3", "Harbor Family Practice", "desktop", w11, 7*hr)
	x.runs = []fxRun{{ago: 7 * hr, dur: 12 * time.Minute, outcome: "success", failed: 0, total: 18000, counted: true, vss: []string{"D"}}}
	x.lastSuccessAgo = 31 * hr
	add(x)

	// ---- Northwind Dental: one parked machine, rest healthy ----------------------------
	x = ok("XRAY-OLD-01", "Northwind Dental", "desktop", w10, 48*hr)
	x.pausedAgo, x.pausedUntilIn, x.pauseReason = 2*day, 2*day, "awaiting replacement"
	add(x)
	add(ok("OPS-ND-01", "Northwind Dental", "desktop", w11, 6*hr))
	add(ok("HYGIENE-PC-2", "Northwind Dental", "desktop", w11, 9*hr))

	// ---- Pinecrest Legal: customer-held keys, everything healthy -----------------------
	add(ok("LEGAL-PC-01", "Pinecrest Legal", "desktop", w11, 4*hr))
	add(ok("LEGAL-PC-02", "Pinecrest Legal", "desktop", w11, 5*hr))
	add(ok("PARTNER-LT-02", "Pinecrest Legal", "laptop", w11, 8*hr))
	return d
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand cannot fail on a supported platform
	}
	return hex.EncodeToString(b)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// fixtureBcrypt is a well-formed, unusable bcrypt string (60 characters): it
// satisfies the schema's format check and matches no password.
var fixtureBcrypt = "$2a$10$" + strings.Repeat("f", 53)

// LoadFixtures seeds a database with the fictional fleet of the design page
// (replica/design/concepts-v0.3-orbitron.html: Summit Eye Care, Harbor Family
// Practice, Northwind Dental, Pinecrest Legal, Blue Ridge Accounting) plus one
// onboarding customer, so fixtures mode renders the approved screens: one
// storage target, the default policy, six customers, 26 devices covering every
// reachable state of the precedence table, presence and heartbeat rows, run
// histories, snapshots, escrow, maintenance rows, one acknowledgement and one
// admin. Every timestamp is relative to now. It writes through the same schema
// and constraints as production data; there is no second code path. The loaded
// facts, evaluated by state.Evaluate, give FixtureExpectations (devices) and
// FixtureCustomerExpectations (rollups), and AllClear is false (it is a fleet
// with problems).
//
// Nothing here is a real name, host, secret or credential: tokens and
// fingerprints are random, hashes are unusable, endpoints are reserved
// example domains. Call it on an empty database (OpenMemory); a second call
// fails on the unique customer names.
func LoadFixtures(ctx context.Context, s *Store, now time.Time, opts ...FixtureOption) error {
	cfg := fixtureConfig{adminHash: "!"}
	for _, o := range opts {
		o(&cfg)
	}
	now = now.UTC()
	return s.Write(ctx, func(tx *Tx) error { return loadFixtures(tx, now, cfg) })
}

func loadFixtures(tx *Tx, now time.Time, cfg fixtureConfig) error {
	ago := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	nowMS := now.UnixMilli()

	// exec records the first failure and turns every later call into a no-op, so
	// the long seeding sequence below reads straight through and the error is
	// checked once at the end (Write then rolls everything back).
	var ferr error
	exec := func(q string, args ...any) {
		if ferr != nil {
			return
		}
		if _, err := tx.Exec(q, args...); err != nil {
			ferr = fmt.Errorf("fixtures: %w", err)
		}
	}

	// admin
	adminID := tx.id()
	exec(`INSERT INTO admin_users (id, username, email, display_name, role, password_hash, must_change_password, created_at, updated_at)
		VALUES (?,?,?,?,?,?,0,?,?)`, adminID, FixtureAdminUsername, "admin@example-msp.internal", "Fixtures Admin", "owner", cfg.adminHash, nowMS, nowMS)

	// storage target
	targetID := tx.id()
	exec(`INSERT INTO storage_targets (id, name, provider, endpoint, region, bucket, prefix, gateway_url, credential_ref, is_default,
			created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,1,?,?)`,
		targetID, "Example Storage", "wasabi", "s3.example-storage.invalid", "us-east-1", "cryo-fixtures", "fleet",
		"https://gateway.example-msp.internal:9443", "CRYO_FIXTURE_S3_CREDENTIALS", nowMS, nowMS)

	// default policy (complete document) and a sparse customer policy for Summit
	defSections, err := policy.Encode(policy.Default(policy.ClassUnknown))
	if err != nil {
		return err
	}
	insertPolicy := func(scope, customerID, name string, sections map[string]json.RawMessage) string {
		args, err := sectionArgs(sections)
		if err != nil && ferr == nil {
			ferr = err
		}
		id := tx.id()
		exec(`INSERT INTO policies (`+policyCols+`) VALUES (?,?,?,?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, scope, nullStr(customerID), name, nil, nil, args[0], args[1], args[2], args[3], args[4], args[5], args[6], args[7], args[8],
			nil, adminID, nowMS, nowMS)
		body, err := revisionBody(sections)
		if err != nil && ferr == nil {
			ferr = err
		}
		exec(`INSERT INTO policy_revisions (id, policy_id, version, body, changed_by, change_note, created_at) VALUES (?,?,1,?,?,?,?)`,
			tx.id(), id, body, adminID, "fixtures", nowMS)
		return id
	}
	insertPolicy("default", "", "Default", defSections)

	// customers
	type fxCustomer struct {
		name, custody, contact string
		expected               bool
		onboardingIn           time.Duration
	}
	customers := []fxCustomer{
		{"Summit Eye Care", "escrowed", `[{"name":"Dana Whitfield","email":"dana@summit-eye.example","role":"office manager"}]`, false, 0},
		{"Harbor Family Practice", "escrowed", `[{"name":"Marcus Bell","email":"marcus@harbor-family.example","role":"practice administrator"}]`, false, 0},
		{"Northwind Dental", "escrowed", `[]`, false, 0},
		{"Pinecrest Legal", "customer_held", `[{"name":"Priya Raman","email":"priya@pinecrest-legal.example","role":"managing partner"}]`, false, 0},
		{"Blue Ridge Accounting", "escrowed", `[]`, true, 0},
		{"Lakeshore Pediatrics", "escrowed", `[]`, true, 7 * day},
	}
	custID := map[string]string{}
	custCustody := map[string]string{}
	for _, c := range customers {
		id := tx.id()
		custID[c.name], custCustody[c.name] = id, c.custody
		var onboarding any
		if c.onboardingIn > 0 {
			onboarding = now.Add(c.onboardingIn).UnixMilli()
		}
		exec(`INSERT INTO customers (id, name, slug, state, expected, onboarding_until, contacts, storage_target_id, key_custody, created_at, updated_at)
			VALUES (?,?,?,'active',?,?,?,?,?,?,?)`, id, c.name, Slugify(c.name), boolInt(c.expected), onboarding, c.contact, targetID,
			c.custody, ago(120*day), ago(120*day))
	}
	summitPolicy := insertPolicy("customer", custID["Summit Eye Care"], "Summit Workstation", map[string]json.RawMessage{
		"schedule": json.RawMessage(`{"window":"Mon-Fri/7-19;Sat/8-14"}`),
	})
	exec(`UPDATE customers SET policy_id = ? WHERE id = ?`, summitPolicy, custID["Summit Eye Care"])

	// enrollment tokens: one per customer, summit's labelled as in the design
	for _, c := range customers {
		label := Slugify(c.name) + "-2026q4"
		exp := now.Add(30 * day).UnixMilli()
		exec(`INSERT INTO enrollment_tokens (id, customer_id, label, token_hash, device_class_default, expires_at, used_count, created_by, created_at, updated_at)
			VALUES (?,?,?,?, 'unknown', ?, 0, ?, ?, ?)`, tx.id(), custID[c.name], label, randHex(32), exp, adminID, nowMS, nowMS)
	}

	devs := fixtureDevices()
	devID := map[string]string{}
	for _, x := range devs {
		id := tx.id()
		devID[x.host] = id
		cust := custID[x.customer]
		enrolled := ago(x.enrolledAgo)
		due := enrolled + 72*hr.Milliseconds()
		if x.firstDueIn != nil {
			due = now.Add(*x.firstDueIn).UnixMilli()
		}
		var confirmed any = enrolled + int64(time.Minute/time.Millisecond)
		if x.unconfirmed {
			confirmed = nil
		}
		var applied any
		if x.appliedPolicy {
			applied = 1 // every policy in the fixtures is at version 1
		}
		var paused, until, pauseReason, pausedBy, retired, retReason, replaced, ident any
		if x.pausedAgo > 0 {
			paused, pauseReason, pausedBy = ago(x.pausedAgo), x.pauseReason, adminID
			if x.pausedUntilIn > 0 {
				until = now.Add(x.pausedUntilIn).UnixMilli()
			}
		}
		if x.retiredAgo > 0 {
			retired, retReason = ago(x.retiredAgo), "replaced by new hardware"
			if x.replacedBy != "" {
				replaced = devID[x.replacedBy]
			}
		}
		if x.identityAgo > 0 {
			ident = ago(x.identityAgo)
		}
		exec(`INSERT INTO devices (id, customer_id, hostname, os_version, arch, device_class, agent_version, restic_version, update_ring,
				target_version, enrolled_at, enrolled_via, enroll_nonce, enroll_confirmed_at, first_backup_due_by, policy_version_applied,
				paused_at, paused_until, pause_reason, paused_by, retired_at, retired_reason, replaced_by_device_id, repo_path,
				hw_fingerprint, identity_conflict_at, created_at, updated_at)
			VALUES (?,?,?,?, 'amd64', ?,?, '0.19.1', 'stable', ?, ?, 'token', ?,?,?,?, ?,?,?,?, ?,?,?, ?, ?, ?, ?, ?)`,
			id, cust, x.host, x.os, x.class, x.agent, nullStr(x.target), enrolled, randHex(16), confirmed, due, applied,
			paused, until, pauseReason, pausedBy, retired, retReason, replaced, "fleet/"+id+"/", randHex(32), ident, enrolled, nowMS)

		// token, gateway login, escrow, maintenance row
		var tokRevoked, gwRevoked, tokReason any
		if x.retiredAgo > 0 {
			tokRevoked, gwRevoked, tokReason = ago(x.retiredAgo), ago(x.retiredAgo), "device retired"
		}
		exec(`INSERT INTO device_tokens (id, device_id, token_hash, revoked_at, revoked_reason, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
			tx.id(), id, randHex(32), tokRevoked, tokReason, enrolled, enrolled)
		exec(`INSERT INTO gateway_logins (id, device_id, username, password_hash, delivered_at, revoked_at, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`,
			tx.id(), id, id, fixtureBcrypt, enrolled, gwRevoked, enrolled, enrolled)
		if custCustody[x.customer] == "escrowed" {
			var verifiedAt any
			if x.escrowVerified != nil {
				verifiedAt = ago(*x.escrowVerified)
			}
			exec(`INSERT INTO repo_key_escrow (id, device_id, key_version, custody, wrapped_key, wrap_alg, master_key_id, verified_at, created_at, updated_at)
				VALUES (?,?,1,'escrowed',?, 'aes-256-gcm+hkdf-sha256/v1', 'fixtures', ?, ?, ?)`,
				tx.id(), id, []byte("fixture-not-a-real-wrapped-key-"+x.host), verifiedAt, enrolled, enrolled)
		} else {
			exec(`INSERT INTO repo_key_escrow (id, device_id, key_version, custody, created_at, updated_at) VALUES (?,?,1,'customer_held',?,?)`,
				tx.id(), id, enrolled, enrolled)
		}
		exec(`INSERT INTO maintenance_schedule (device_id, enabled, created_at, updated_at) VALUES (?,?,?,?)`,
			id, boolInt(custCustody[x.customer] == "escrowed"), enrolled, enrolled)

		// presence and heartbeat ring
		if x.seenAgo != nil {
			seen := ago(*x.seenAgo)
			var progress any
			if x.progress != "" {
				progress = x.progress
			}
			var battery any
			if x.class == "laptop" {
				battery = 88
			}
			exec(`INSERT INTO device_presence (device_id, received_at, agent_version, uptime_s, on_ac, battery_pct, metered, vpn, queue_depth,
					journal_depth, clock_offset_ms, service_state, progress, created_at, updated_at)
				VALUES (?,?,?,?,?,?,0,0,0,0,?,?,?,?,?)`, id, seen, x.agent, 86400, boolInt(x.class != "laptop"),
				battery, 120, x.serviceState, progress, seen, seen)
			if x.progress != "" {
				// The engine reads progress freshness from the stored advanced_at, set as the heartbeat path does.
				exec(`UPDATE device_presence SET progress = json_set(progress, '$.advanced_at', ?) WHERE device_id = ?`, seen, id)
			}
			if *x.seenAgo < 6*hr {
				for k := 0; k < 4; k++ {
					at := seen - int64(k)*int64(15*time.Minute/time.Millisecond)
					exec(`INSERT INTO heartbeats (id, device_id, received_at, agent_version, uptime_s, on_ac, service_state, created_at)
						VALUES (?,?,?,?,?,?,?,?)`, tx.id(), id, at, x.agent, 86400, 1, x.serviceState, at)
				}
			}
		}

		// run history and snapshots
		type fxSnap struct {
			id string
			at int64
		}
		var snaps []fxSnap
		insertRun := func(r fxRun) {
			kind, origin := r.kind, r.origin
			if kind == "" {
				kind = "backup"
			}
			if origin == "" {
				origin = "agent"
			}
			var started, finished, outcome, exit, snapID, added any
			stats := "{}"
			if r.outcome == "" {
				started = ago(r.ago)
			} else {
				fin := ago(r.ago)
				started, finished, outcome = fin-r.dur.Milliseconds(), fin, r.outcome
				switch r.outcome {
				case "success":
					exit = 0
				case "partial":
					exit = 3
				case "failed":
					exit = 1
				}
				if kind == "backup" && (r.outcome == "success" || r.outcome == "partial") {
					sid := sha256Hex(id + "/" + fmt.Sprint(fin))
					snapID = sid
					snaps = append(snaps, fxSnap{sid, fin})
					added = int64(1_100_000_000)
				}
				m := map[string]any{}
				if r.counted {
					m["total_files_processed"], m["failed_files"] = r.total, r.failed
				}
				if len(r.vss) > 0 {
					m["vss_fallback_volumes"] = r.vss
				}
				if len(m) > 0 {
					b, _ := json.Marshal(m)
					stats = string(b)
				}
			}
			counts := "{}"
			if len(r.counts) > 0 {
				b, _ := json.Marshal(r.counts)
				counts = string(b)
			}
			startedMS := started.(int64)
			recv := startedMS
			if finished != nil {
				recv = finished.(int64)
			}
			var deferred any = nullStr(r.deferred)
			exec(`INSERT INTO runs (id, device_id, origin, kind, trigger_kind, started_at, finished_at, received_at, outcome, restic_exit_code,
					snapshot_id, stats, data_added_bytes, error_counts, error_class, deferred_reason, agent_version, created_at, updated_at)
				VALUES (?,?,?,?,'schedule',?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, tx.id(), id, origin, kind, started, finished, recv, outcome, exit,
				snapID, stats, added, counts, nullStr(r.errClass), deferred, x.agent, recv, recv)
		}
		if x.lastSuccessAgo > 0 {
			for k := 9; k >= 0; k-- { // oldest first: ten daily successes ending at lastSuccessAgo
				insertRun(fxRun{ago: x.lastSuccessAgo + time.Duration(k)*day, dur: 14 * time.Minute, outcome: "success", failed: 0, total: 40000 + int64(k), counted: true})
			}
		}
		for _, r := range x.runs {
			insertRun(r)
		}
		if x.retiredAgo == 0 && !x.skipForget && custCustody[x.customer] == "escrowed" {
			// A recent successful forget keeps the "no forget within 2x cadence" rule quiet.
			insertRun(fxRun{ago: 3 * day, dur: 40 * time.Second, outcome: "success", kind: "forget", origin: "console"})
		}
		for i, sn := range snaps {
			if i < len(snaps)-6 {
				continue // keep the newest six
			}
			exec(`INSERT INTO snapshots (device_id, snapshot_id, taken_at, hostname, paths, tags, program_version, summary, created_at, updated_at)
				VALUES (?,?,?,?,?,'[]', '0.19.1', '{}', ?, ?)`, id, sn.id, sn.at, id, `["C:\\Users"]`, sn.at, sn.at)
		}

		// device override and acknowledgement
		if x.override != "" {
			exec(`INSERT INTO device_policy_overrides (id, device_id, patch, reason, set_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
				tx.id(), id, x.override, "large initial dataset", adminID, nowMS, nowMS)
		}
		if x.ackSnooze > 0 {
			exec(`INSERT INTO alarm_acks (id, subject_type, device_id, alarm_type, acknowledged_by, acknowledged_at, note, snooze_until, created_at, updated_at)
				VALUES (?,'device',?,'stale',?,?,?,?,?,?)`, tx.id(), id, adminID, nowMS, "laptop is at the customer's other site",
				now.Add(x.ackSnooze).UnixMilli(), nowMS, nowMS)
		}
	}

	seedFixtureCommands(exec, now, adminID, devID) // M2-W2C (fixtures_commands_m2.go)

	// a short audit trail
	for _, ev := range []AuditEvent{
		{ActorType: "system", Method: "system", Action: "fixtures.load", ObjectType: "system"},
		{ActorType: "admin", ActorID: adminID, ActorLabel: FixtureAdminUsername, Method: "web", Action: "device.pause",
			ObjectType: "device", ObjectID: devID["XRAY-OLD-01"], CustomerID: custID["Northwind Dental"],
			Details: json.RawMessage(`{"reason":"awaiting replacement"}`)},
	} {
		if ferr != nil {
			break
		}
		if _, err := tx.AppendAudit(ev); err != nil {
			ferr = err
		}
	}
	exec(`UPDATE settings SET value = 'true', updated_at = ? WHERE key = 'fleet_ever_enrolled'`, nowMS)
	return ferr
}
