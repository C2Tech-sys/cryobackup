package adminapi

import (
	"context"
	"database/sql"
	"encoding/base32"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/jobs"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M2-W3D: every mutation of the admin API writes its audit row, and a change
// whose audit row cannot be written is refused (architecture section 10.2, M2
// acceptance "every mutation produces an audit row"; M2-COMMON console rule).
//
// The set of routes is derived from Patterns(): every pattern that is not a
// GET is a mutation, and so is the one GET that spends a token and streams a
// restore archive. The table below must name exactly that set, in both
// directions, so a new mutating route cannot be added without a case here.
// Each case builds its own fixtures, performs the mutation with valid input as
// an authorised user (the owner, or the account the route needs) and checks
//
//	1. a forced audit failure (a trigger that aborts every insert into
//	   audit_events) refuses the change: the response is not a success, the
//	   problem body does not name the cause, and a digest of every other table
//	   is unchanged;
//	2. the same call, repeated with the audit table healthy, succeeds and
//	   writes the documented number of audit rows (one unless the case says why
//	   not).

// call is one request of a case.
type call struct {
	method, path string
	body         any
	hdr          []string
	anon         bool     // no cookies (the password step)
	as           *session // overrides the owner session
}

type mutation struct {
	build  func(e *env, s *session) func() call
	status int // expected success status (200 when zero)
	audits int // expected new audit rows; 1 when zero and zeroWhy is empty
	// zeroWhy: the route legitimately writes no audit row, and why. It also
	// means there is nothing to refuse, so the refusal check is skipped.
	zeroWhy string
	// bestEffort: the audit row is written but its failure does not refuse the
	// request, and why. The refusal check is skipped; the row is still asserted.
	bestEffort string
	// changesState: the route writes no audit row yet changes stored rows; why
	// that is not an audited change belongs in zeroWhy.
	changesState bool
	// knownGap: the route does NOT refuse an unaudited change today; why, and
	// what closes it. The test expects the failure and fails once it is fixed,
	// so the gap cannot be forgotten and cannot silently stay open.
	knownGap string
}

const refuseAudits = `CREATE TRIGGER refuse_audits BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit refused by the test'); END`

// digest renders every table except the audit trail and tables the request
// path itself keeps current (job state, the heartbeat ring, presence, schema
// bookkeeping) in a stable order, so two digests are equal exactly when the
// request changed no stored row. Columns that move on every authenticated
// request (last_seen_at, last_used_at) are left out.
func (e *env) digest() string {
	e.t.Helper()
	skipTable := map[string]bool{"audit_events": true, "schema_migrations": true, "job_state": true, "heartbeats": true, "device_presence": true}
	skipCol := map[string]bool{"last_seen_at": true, "last_used_at": true}
	var b strings.Builder
	err := e.st.Read(context.Background(), func(tx *store.Tx) error {
		rows, err := tx.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
		if err != nil {
			return err
		}
		var tables []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			tables = append(tables, n)
		}
		rows.Close()
		for _, tbl := range tables {
			if skipTable[tbl] {
				continue
			}
			r, err := tx.Query(`SELECT * FROM "` + tbl + `" ORDER BY 1, 2`)
			if err != nil {
				return err
			}
			cols, _ := r.Columns()
			fmt.Fprintf(&b, "== %s\n", tbl)
			for r.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := r.Scan(ptrs...); err != nil {
					r.Close()
					return err
				}
				for i, c := range cols {
					if skipCol[c] {
						continue
					}
					if bs, ok := vals[i].([]byte); ok {
						vals[i] = string(bs)
					}
					fmt.Fprintf(&b, "%s=%v|", c, vals[i])
				}
				b.WriteString("\n")
			}
			r.Close()
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return b.String()
}

// auditRowCount counts every audit row.
func (e *env) auditRowCount() int {
	e.t.Helper()
	n := 0
	err := e.st.Read(context.Background(), func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&n)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// auditActionsSince lists the actions of the audit rows after the first n.
func (e *env) auditActionsSince(n int) []string {
	e.t.Helper()
	var out []string
	err := e.st.Read(context.Background(), func(tx *store.Tx) error {
		rows, err := tx.Query(`SELECT action FROM audit_events ORDER BY rowid LIMIT -1 OFFSET ?`, n)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a sql.NullString
			if err := rows.Scan(&a); err != nil {
				return err
			}
			out = append(out, a.String)
		}
		return rows.Err()
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) setAuditRefusal(on bool) {
	e.t.Helper()
	err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		if on {
			_, err := tx.Exec(refuseAudits)
			return err
		}
		_, err := tx.Exec(`DROP TRIGGER IF EXISTS refuse_audits`)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) do(c call, owner *session) (*http.Response, []byte) {
	e.t.Helper()
	s := owner
	if c.as != nil {
		s = c.as
	}
	if c.anon {
		s = nil
	}
	return e.req(c.method, c.path, s, c.body, c.hdr...)
}

// mutatingRoutes is the set the table must cover: every non-GET admin pattern
// and the GET that spends a download token.
func mutatingRoutes() map[string]bool {
	out := map[string]bool{PatternRestoreDownload: true}
	for _, p := range Patterns() {
		if !strings.HasPrefix(p, "GET ") {
			out[p] = true
		}
	}
	return out
}

// totpFor returns a current code for a user the env enrolled. Each call moves
// the test clock one step, so a code is never replayed.
func (e *env) totpFor(user string) string { return e.mfa.code(user) }

func (e *env) newVictim(name, role string) store.Admin {
	e.t.Helper()
	a, err := e.st.CreateAdmin(context.Background(), store.Admin{Username: name, Email: name + "@example.invalid", Role: role, PasswordHash: "!"})
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) mustReq(method, path string, s *session, body any, want int) []byte {
	e.t.Helper()
	res, b := e.req(method, path, s, body)
	if res.StatusCode != want {
		e.t.Fatalf("setup %s %s: %d %s (want %d)", method, path, res.StatusCode, b, want)
	}
	return b
}

func auditMutationCases() map[string]mutation {
	sla := func(h int) map[string]any { return map[string]any{"sla": map[string]any{"stale_after_hours": h}} }
	return map[string]mutation{
		// ---- authentication events ----
		PatternLogin: {
			bestEffort: "login is an authentication event: auth.Manager.audit records it and a failed audit write does not lock the operator out of the console",
			build: func(e *env, s *session) func() call {
				return func() call {
					return call{method: "POST", path: "/api/v1/auth/login", anon: true, body: LoginRequest{Username: "admin", Password: pw}}
				}
			},
		},
		PatternLogout: {
			status:     204,
			bestEffort: "signing out leaves an auth.logout row like signing in leaves auth.login, but the row is best effort and never blocks the exit (auth.Manager.EndSession)",
			build: func(e *env, s *session) func() call {
				// A fresh session per attempt: a sign-out spends its session.
				return func() call { return call{method: "POST", path: "/api/v1/auth/logout", as: e.login("admin", pw)} }
			},
		},
		PatternMFATOTP: {
			build: func(e *env, s *session) func() call {
				ps, _ := e.passwordOnly("admin", pw)
				return func() call {
					return call{method: "POST", path: "/api/v1/auth/mfa/totp", as: ps, body: CodeRequest{Code: e.totpFor("admin")}}
				}
			},
		},
		PatternMFARecovery: {
			build: func(e *env, s *session) func() call {
				ps, _ := e.passwordOnly("admin", pw)
				code := e.mfa.codes["admin"][0]
				return func() call {
					return call{method: "POST", path: "/api/v1/auth/mfa/recovery", as: ps, body: CodeRequest{Code: code}}
				}
			},
		},
		// ---- own account ----
		PatternMePatch: {
			build: func(e *env, s *session) func() call {
				return func() call {
					return call{method: "PATCH", path: "/api/v1/me", body: PatchMeRequest{Email: ptr("owner-new@example.invalid")}}
				}
			},
		},
		PatternTOTPBegin: {
			zeroWhy: "starting an enrollment keeps the pending secret in memory only; nothing is stored until the confirm, which is audited",
			build: func(e *env, s *session) func() call {
				return func() call { return call{method: "POST", path: "/api/v1/me/totp/begin"} }
			},
		},
		PatternTOTPConfirm: {
			build: func(e *env, s *session) func() call {
				return func() call {
					res, b := e.req("POST", "/api/v1/me/totp/begin", s, nil)
					if res.StatusCode != 200 {
						e.t.Fatalf("begin: %d %s", res.StatusCode, b)
					}
					tb := decode[TOTPBeginResponse](e.t, b)
					secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ReplaceAll(tb.Secret, " ", ""))
					if err != nil {
						e.t.Fatal(err)
					}
					e.mfa.mu.Lock()
					e.mfa.at = e.mfa.at.Add(auth.TOTPPeriod)
					at := e.mfa.at
					cur := auth.TOTPCode(e.mfa.secrets["admin"], at)
					fresh := auth.TOTPCode(secret, at)
					e.mfa.mu.Unlock()
					return call{method: "POST", path: "/api/v1/me/totp/confirm", body: TOTPConfirmRequest{Code: fresh, CurrentCode: cur}}
				}
			},
		},
		PatternRecoveryCodes: {
			build: func(e *env, s *session) func() call {
				return func() call {
					return call{method: "POST", path: "/api/v1/me/recovery-codes", body: CodeRequest{Code: e.totpFor("admin")}}
				}
			},
		},
		PatternSessionRevoke: {
			status: 204,
			build: func(e *env, s *session) func() call {
				other := e.login("admin", pw)
				_, b := e.req("GET", "/api/v1/me/sessions", other, nil)
				var victim string
				for _, ss := range decode[SessionsResponse](e.t, b).Sessions {
					if !ss.Current {
						victim = ss.ID
					}
				}
				if victim == "" {
					e.t.Fatal("no second session to revoke")
				}
				return func() call { return call{method: "DELETE", path: "/api/v1/me/sessions/" + victim} }
			},
		},
		PatternTokenCreate: {
			status: 201,
			build: func(e *env, s *session) func() call {
				return func() call {
					return call{method: "POST", path: "/api/v1/me/api-tokens", body: TokenCreateRequest{Name: "ci", Scope: "read", Days: 7}}
				}
			},
		},
		PatternTokenRevoke: {
			status: 204,
			build: func(e *env, s *session) func() call {
				tc := decode[TokenCreateResponse](e.t, e.mustReq("POST", "/api/v1/me/api-tokens", s, TokenCreateRequest{Name: "doomed", Scope: "read", Days: 7}, 201))
				return func() call { return call{method: "DELETE", path: "/api/v1/me/api-tokens/" + tc.APIToken.ID} }
			},
		},
		// ---- admin accounts ----
		PatternAdminCreate: {
			status: 201,
			build: func(e *env, s *session) func() call {
				return func() call {
					return call{method: "POST", path: "/api/v1/admins", body: AccountRequest{Username: "newadmin", Email: "newadmin@example.invalid", Role: "operator"}}
				}
			},
		},
		PatternAdminPatch: {
			build: func(e *env, s *session) func() call {
				v := e.newVictim("victim-patch", "read_only")
				return func() call {
					return call{method: "PATCH", path: "/api/v1/admins/" + v.ID, body: AccountRequest{Role: "admin"}}
				}
			},
		},
		PatternAdminDelete: {
			status: 204,
			build: func(e *env, s *session) func() call {
				v := e.newVictim("victim-delete", "read_only")
				return func() call { return call{method: "DELETE", path: "/api/v1/admins/" + v.ID} }
			},
		},
		PatternAdminDisable: {
			status: 204,
			build: func(e *env, s *session) func() call {
				v := e.newVictim("victim-disable", "operator")
				return func() call {
					return call{method: "POST", path: "/api/v1/admins/" + v.ID + "/disable", body: ReasonRequest{Reason: "left the company"}}
				}
			},
		},
		PatternAdminEnable: {
			status: 204,
			build: func(e *env, s *session) func() call {
				v := e.newVictim("victim-enable", "operator")
				e.mustReq("POST", "/api/v1/admins/"+v.ID+"/disable", s, ReasonRequest{Reason: "setup"}, 204)
				return func() call { return call{method: "POST", path: "/api/v1/admins/" + v.ID + "/enable"} }
			},
		},
		PatternAdminResetMFA: {
			status: 204,
			build: func(e *env, s *session) func() call {
				e.addAdmin("victim-mfa", "operator", nil)
				e.login("victim-mfa", pw) // enrolls TOTP
				var id string
				_ = e.st.Read(context.Background(), func(tx *store.Tx) error {
					return tx.QueryRow(`SELECT id FROM admin_users WHERE username = 'victim-mfa'`).Scan(&id)
				})
				return func() call {
					return call{method: "POST", path: "/api/v1/admins/" + id + "/reset-mfa", body: ReasonRequest{Reason: "lost phone"}}
				}
			},
		},
		PatternAdminResetPassw: {
			build: func(e *env, s *session) func() call {
				v := e.newVictim("victim-reset", "operator")
				return func() call { return call{method: "POST", path: "/api/v1/admins/" + v.ID + "/reset-password"} }
			},
		},
		// ---- customers and devices ----
		PatternCustomerCreate: {
			status: 201,
			build: func(e *env, s *session) func() call {
				return func() call {
					return call{method: "POST", path: "/api/v1/customers", body: map[string]any{"name": "Mutation Test Co"}}
				}
			},
		},
		PatternCustomerPatch: {
			build: func(e *env, s *session) func() call {
				c := e.customerNamed("Summit Eye Care")
				return func() call {
					return call{method: "PATCH", path: "/api/v1/customers/" + c.ID, body: map[string]any{"notes": "edited by the audit test"}}
				}
			},
		},
		PatternCustomerPatch + "#policy assignment": {
			build: func(e *env, s *session) func() call {
				c := e.customerNamed("Summit Eye Care")
				tpl := e.createTemplate(s, "Assign Me", sla(48))
				_, imp, raw := e.impact(s, tpl.ID, map[string]any{"assign_customer": c.ID})
				if imp.Token == "" {
					e.t.Fatalf("assignment preview: %s", raw)
				}
				return func() call {
					return call{method: "PATCH", path: "/api/v1/customers/" + c.ID, body: map[string]any{"policy_id": tpl.ID, "impact_token": imp.Token}}
				}
			},
		},
		PatternDevicePatch: {
			build: func(e *env, s *session) func() call {
				return func() call {
					return call{method: "PATCH", path: "/api/v1/devices/" + e.dev.ID, body: map[string]any{"notes": "edited by the audit test"}}
				}
			},
		},
		// ---- M3-T3: pause, resume, saved views, jobs, storage probe ----
		PatternDevicePause: {
			build: func(e *env, s *session) func() call {
				// EXAM-ROOM-3 has an open run: the pause also queues its cancel, and that is still one audit row.
				d := e.deviceNamed("EXAM-ROOM-3")
				return func() call {
					return call{method: "POST", path: "/api/v1/devices/" + d.ID + "/pause", body: DevicePauseRequest{Reason: "audit test", DurationHours: ptr(4)}}
				}
			},
		},
		PatternDeviceResume: {
			build: func(e *env, s *session) func() call {
				e.mustReq("POST", "/api/v1/devices/"+e.dev.ID+"/pause", s, DevicePauseRequest{Reason: "audit test", Indefinite: true}, 200)
				return func() call { return call{method: "POST", path: "/api/v1/devices/" + e.dev.ID + "/resume"} }
			},
		},
		PatternViewCreate: {
			status: 201,
			build: func(e *env, s *session) func() call {
				return func() call {
					return call{method: "POST", path: "/api/v1/views", body: CreateViewRequest{Name: "audit view", Entity: "devices", Shared: true}}
				}
			},
		},
		PatternViewPatch: {
			build: func(e *env, s *session) func() call {
				v := decode[ViewResponse](e.t, e.mustReq("POST", "/api/v1/views", s, CreateViewRequest{Name: "audit view", Entity: "runs"}, 201))
				return func() call {
					return call{method: "PATCH", path: "/api/v1/views/" + v.ID, body: PatchViewRequest{Name: ptr("audit view renamed")}}
				}
			},
		},
		PatternViewDelete: {
			status: 204,
			build: func(e *env, s *session) func() call {
				v := decode[ViewResponse](e.t, e.mustReq("POST", "/api/v1/views", s, CreateViewRequest{Name: "audit view", Entity: "customers"}, 201))
				return func() call { return call{method: "DELETE", path: "/api/v1/views/" + v.ID} }
			},
		},
		PatternSystemJobRun: {
			status: 202,
			build: func(e *env, s *session) func() call {
				// The audit row is written before the run is accepted, and a refused audit starts nothing.
				r := jobs.NewRunner(e.st, "audit-test", nil)
				r.Add(jobs.Job{Name: "audit-test-job", Every: time.Hour, Run: func(context.Context) error { return nil }})
				e.svc.Jobs = r
				e.t.Cleanup(jobBackground.Wait)
				return func() call { return call{method: "POST", path: "/api/v1/system/jobs/audit-test-job/run"} }
			},
		},
		PatternStorageTargetProbe: {
			build: func(e *env, s *session) func() call {
				return func() call {
					return call{method: "POST", path: "/api/v1/storage-targets/" + e.firstStorageTarget() + "/probe"}
				}
			},
		},
		// ---- commands ----
		PatternDeviceCommandCreate: {
			status: 201,
			build: func(e *env, s *session) func() call {
				return func() call {
					return call{method: "POST", path: "/api/v1/devices/" + e.dev.ID + "/commands", body: QueueCommandRequest{Type: "check_in", IdempotencyKey: "audit-test"}}
				}
			},
		},
		PatternDeviceCommandCancel: {
			status: 204,
			build: func(e *env, s *session) func() call {
				cr := decode[CommandResponse](e.t, e.mustReq("POST", "/api/v1/devices/"+e.dev.ID+"/commands", s, QueueCommandRequest{Type: "check_in", IdempotencyKey: "audit-cancel"}, 201))
				return func() call { return call{method: "DELETE", path: "/api/v1/devices/" + e.dev.ID + "/commands/" + cr.ID} }
			},
		},
		PatternRunCancel: {
			status: 201,
			build: func(e *env, s *session) func() call {
				run := e.openRun(e.dev.ID)
				return func() call { return call{method: "POST", path: "/api/v1/runs/" + run.ID + "/cancel"} }
			},
		},
		PatternDeviceIdentityResolv: {
			build: func(e *env, s *session) func() call {
				d := e.deviceNamed("FRONTDESK-02")
				if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
					_, err := tx.Exec(`UPDATE devices SET identity_conflict_at = ? WHERE id = ?`, e.clk.Now().UnixMilli(), d.ID)
					return err
				}); err != nil {
					e.t.Fatal(err)
				}
				return func() call {
					return call{method: "POST", path: "/api/v1/devices/" + d.ID + "/identity/resolve", body: IdentityResolveRequest{Action: "retire", Reason: "clone of a restored image"}}
				}
			},
		},
		PatternDeviceIdentityResolv + "#keep": {
			build: func(e *env, s *session) func() call {
				d := e.deviceNamed("FRONTDESK-02")
				if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
					_, err := tx.Exec(`UPDATE devices SET identity_conflict_at = ? WHERE id = ?`, e.clk.Now().UnixMilli(), d.ID)
					return err
				}); err != nil {
					e.t.Fatal(err)
				}
				return func() call {
					return call{method: "POST", path: "/api/v1/devices/" + d.ID + "/identity/resolve", body: IdentityResolveRequest{Action: "keep", Reason: "the same machine"}}
				}
			},
		},
		// ---- policies ----
		PatternPolicyCreate: {
			status: 201,
			build: func(e *env, s *session) func() call {
				return func() call {
					return call{method: "POST", path: "/api/v1/policies", body: map[string]any{"scope": "template", "name": "Audit Template", "sections": sla(48)}}
				}
			},
		},
		PatternPolicyPatch: {
			build: func(e *env, s *session) func() call {
				tpl := e.createTemplate(s, "Patch Me", sla(48))
				_, imp, raw := e.impact(s, tpl.ID, map[string]any{"sections": sla(36)})
				if imp.Token == "" {
					e.t.Fatalf("preview: %s", raw)
				}
				return func() call {
					return call{method: "PATCH", path: "/api/v1/policies/" + tpl.ID, body: map[string]any{"sections": sla(36), "impact_token": imp.Token}}
				}
			},
		},
		PatternPolicyDelete: {
			status: 204,
			build: func(e *env, s *session) func() call {
				tpl := e.createTemplate(s, "Delete Me", sla(48))
				_, imp, raw := e.impact(s, tpl.ID, map[string]any{"delete": true})
				if imp.Token == "" {
					e.t.Fatalf("preview: %s", raw)
				}
				return func() call {
					return call{method: "DELETE", path: "/api/v1/policies/" + tpl.ID + "?impact_token=" + url.QueryEscape(imp.Token)}
				}
			},
		},
		PatternPolicyImpact: {
			zeroWhy: "a blast-radius preview changes nothing; the audited step is the save that carries its token",
			build: func(e *env, s *session) func() call {
				tpl := e.createTemplate(s, "Preview Me", sla(48))
				return func() call {
					return call{method: "POST", path: "/api/v1/policies/" + tpl.ID + "/impact", body: map[string]any{"sections": sla(36)}}
				}
			},
		},
		PatternOverrideSet: {
			build: func(e *env, s *session) func() call {
				return func() call {
					return call{method: "PUT", path: "/api/v1/devices/" + e.dev.ID + "/override",
						body: map[string]any{"patch": map[string]any{"sla": map[string]any{"max_run_hours": 30}}, "reason": "audit test"}}
				}
			},
		},
		PatternOverrideClear: {
			status: 204,
			build: func(e *env, s *session) func() call {
				e.mustReq("PUT", "/api/v1/devices/"+e.dev.ID+"/override", s,
					map[string]any{"patch": map[string]any{"sla": map[string]any{"max_run_hours": 30}}, "reason": "setup"}, 200)
				return func() call { return call{method: "DELETE", path: "/api/v1/devices/" + e.dev.ID + "/override"} }
			},
		},
		// ---- enrollment tokens ----
		PatternEnrollTokenCreate: {
			status: 201,
			build: func(e *env, s *session) func() call {
				c := e.customerNamed("Summit Eye Care")
				return func() call {
					return call{method: "POST", path: "/api/v1/enrollment-tokens", body: map[string]any{"customer_id": c.ID, "label": "audit test"}}
				}
			},
		},
		PatternEnrollTokenRevoke: {
			status: 204,
			build: func(e *env, s *session) func() call {
				c := e.customerNamed("Summit Eye Care")
				tok, code, b := createToken(e.t, e, s, map[string]any{"customer_id": c.ID, "label": "revoke me"})
				if code != 201 {
					e.t.Fatalf("create token: %d %s", code, b)
				}
				return func() call { return call{method: "DELETE", path: "/api/v1/enrollment-tokens/" + tok.ID} }
			},
		},
		// ---- settings ----
		PatternSettingPut: {
			build: func(e *env, s *session) func() call {
				e.withDisplay("UTC")
				return func() call {
					return call{method: "PUT", path: "/api/v1/settings/refresh_interval_s", body: map[string]any{"value": 45}}
				}
			},
		},
		PatternSettingPut + "#display_timezone": {
			build: func(e *env, s *session) func() call {
				e.withDisplay("UTC")
				return func() call {
					return call{method: "PUT", path: "/api/v1/settings/display_timezone", body: map[string]any{"value": "America/Denver"}}
				}
			},
		},
		// ---- restores ----
		PatternRestoreCreate: {
			status: 202,
			build: func(e *env, s *session) func() call {
				return func() call {
					return call{method: "POST", path: "/api/v1/devices/" + e.dev.ID + "/restores",
						body: CreateRestoreRequest{SnapshotID: fixtures.FixtureSnapshotID, Mode: "download_zip", Path: "/C/Users/Public/Documents/Quarterly-Report.txt"}}
				}
			},
		},
		PatternRestoreMint: {
			zeroWhy: "minting hands out a short-lived token and stores nothing durable; the audited event is the download that spends it",
			build: func(e *env, s *session) func() call {
				rj := e.newRestore(s, "/C/Users/Public/Documents/Quarterly-Report.txt")
				return func() call { return call{method: "POST", path: "/api/v1/restores/" + rj.ID + "/download"} }
			},
		},
		PatternRestoreDownload: {
			bestEffort: "the archive is streamed before the row is written (ADR 0008: nothing is held at rest), so the download cannot be un-sent; a failed audit write is logged loudly and the stream is not cut",
			build: func(e *env, s *session) func() call {
				rj := e.newRestore(s, "/C/Users/Public/Documents/Quarterly-Report.txt")
				return func() call {
					u, _ := e.mint(s, rj.ID)
					return call{method: "GET", path: u}
				}
			},
		},
	}
}

// baseRoute strips a variant suffix ("pattern#variant"): a route with more
// than one code path has one case per path.
func baseRoute(key string) string { p, _, _ := strings.Cut(key, "#"); return p }

func ptr[T any](v T) *T { return &v }

func TestEveryAdminAPIMutationIsAudited(t *testing.T) {
	cases := auditMutationCases()

	// The table and the registered routes must agree, in both directions.
	want := mutatingRoutes()
	covered := map[string]bool{}
	for p := range cases {
		covered[baseRoute(p)] = true
	}
	for p := range want {
		if !covered[p] {
			t.Errorf("mutating route %q has no case in auditMutationCases: add one (build valid fixtures, name the audit rows it writes)", p)
		}
	}
	for p := range cases {
		if !want[baseRoute(p)] {
			t.Errorf("auditMutationCases has a case for %q, which is not a mutating admin route", p)
		}
	}
	if t.Failed() {
		return
	}

	names := make([]string, 0, len(cases))
	for p := range cases {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, pattern := range names {
		m := cases[pattern]
		t.Run(pattern, func(t *testing.T) {
			e := newEnv(t)
			owner := e.login("admin", pw)
			mk := m.build(e, owner)
			wantStatus := m.status
			if wantStatus == 0 {
				wantStatus = 200
			}
			wantAudits := m.audits
			if wantAudits == 0 && m.zeroWhy == "" {
				wantAudits = 1
			}

			// 1. A change that cannot be audited is refused and changes nothing.
			if m.zeroWhy == "" && m.bestEffort == "" {
				c := mk() // built first: a closure may sign in, and the MFA step is audited
				e.setAuditRefusal(true)
				before, auditsBefore := e.digest(), e.auditRowCount()
				res, body := e.do(c, owner)
				var bad []string
				if res.StatusCode >= 200 && res.StatusCode < 300 {
					bad = append(bad, fmt.Sprintf("succeeded (%d) although its audit row could not be written", res.StatusCode))
				}
				if strings.Contains(string(body), "audit refused") {
					bad = append(bad, "the problem body names the cause: "+string(body))
				}
				if after := e.digest(); after != before {
					bad = append(bad, "changed stored rows although its audit row could not be written")
				}
				if got := e.auditRowCount(); got != auditsBefore {
					bad = append(bad, fmt.Sprintf("audit rows moved from %d to %d with the audit table refusing inserts", auditsBefore, got))
				}
				e.setAuditRefusal(false)
				switch {
				case m.knownGap == "":
					for _, b := range bad {
						t.Errorf("%s: %s", pattern, b)
					}
				case len(bad) == 0:
					t.Errorf("%s: the known gap is closed (%s): remove knownGap from its case", pattern, m.knownGap)
				default:
					t.Logf("KNOWN GAP (%s): %s", m.knownGap, strings.Join(bad, "; "))
				}
			}

			// 2. The same call with a healthy audit table succeeds and writes the documented rows.
			c := mk()
			before, digestBefore := e.auditRowCount(), e.digest()
			res, body := e.do(c, owner)
			if res.StatusCode != wantStatus {
				t.Fatalf("%s: status %d, want %d: %s", pattern, res.StatusCode, wantStatus, body)
			}
			got := e.auditRowCount() - before
			if got != wantAudits {
				t.Errorf("%s wrote %d audit rows %v, want %d", pattern, got, e.auditActionsSince(before), wantAudits)
			}
			// A route that writes no audit row has nothing to audit because it changes nothing.
			if m.zeroWhy != "" && !m.changesState && e.digest() != digestBefore {
				t.Errorf("%s writes no audit row (%s) but changed stored rows", pattern, m.zeroWhy)
			}

			// 3. A best-effort route is documented to carry on when its audit table is down.
			//    Pin that contract, so a change to it is a decision and not an accident.
			if m.bestEffort != "" && wantAudits > 0 {
				c = mk()
				e.setAuditRefusal(true)
				res, body = e.do(c, owner)
				e.setAuditRefusal(false)
				if res.StatusCode != wantStatus {
					t.Errorf("best effort (%s): with the audit table down %s answered %d, want %d: %s", m.bestEffort, pattern, res.StatusCode, wantStatus, body)
				}
			}
		})
	}
}
