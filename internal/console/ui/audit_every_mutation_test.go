package ui

import (
	"context"
	"database/sql"
	"encoding/base32"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/auth"
	"github.com/c2tech-sys/cryobackup/internal/console/fixtures"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
)

// M2-W3D: every UI form post that mutates writes its audit row, and a change
// whose audit row cannot be written is refused (the UI half of
// internal/console/adminapi/audit_every_mutation_test.go).
//
// The set of routes is derived from Patterns(true): every pattern that is not a
// GET is a UI POST, and the table below must name exactly that set, in both
// directions. A route that writes nothing by design says why (zeroWhy); the
// preview routes, pure renders and the session-end routes are of that kind.

type uiMutation struct {
	// build prepares fixtures, leaves e.sess/e.csrf set to the acting session,
	// and returns the request. It is called once per attempt.
	build func(e *env) func() (*http.Response, string)
	// ok lists the statuses of a success (303 when nil).
	ok []int
	// audits is the number of audit rows a success writes (1 when zero and zeroWhy is empty).
	audits int
	// zeroWhy: the route writes no audit row, and why. Nothing to refuse either.
	zeroWhy string
	// bestEffort: the row is written but its failure does not refuse the request, and why.
	bestEffort string
	// noRerun: the best-effort rerun with the audit table down is skipped, and why
	// (the request spends something the rerun would need again).
	noRerun string
	// changesState: the route writes no audit row yet changes stored rows.
	changesState bool
	// knownGap: the route does NOT refuse an unaudited change today; why and what closes it.
	knownGap string
}

func (m uiMutation) okStatus(code int) bool {
	ok := m.ok
	if ok == nil {
		ok = []int{http.StatusSeeOther}
	}
	for _, s := range ok {
		if s == code {
			return true
		}
	}
	return false
}

func (e *env) mutDigest() string {
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

func (e *env) mutAuditCount() int {
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

func (e *env) mutAuditActionsSince(n int) []string {
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

func (e *env) mutRefuseAudits(on bool) {
	e.t.Helper()
	err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		if on {
			_, err := tx.Exec(`CREATE TRIGGER refuse_audits BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit refused by the test'); END`)
			return err
		}
		_, err := tx.Exec(`DROP TRIGGER IF EXISTS refuse_audits`)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// uiPostRoutes is the set the table must cover: every non-GET UI pattern.
func uiPostRoutes() map[string]bool {
	out := map[string]bool{}
	for _, p := range Patterns(true) {
		if !strings.HasPrefix(p, "GET ") {
			out[p] = true
		}
	}
	return out
}

func vals(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

var (
	resetLinkRE   = regexp.MustCompile(`(https?://[^<"\s]+/login/reset\?token=[A-Za-z0-9_-]+)`)
	sessionRevoke = regexp.MustCompile(`/ui/account/sessions/([A-Za-z0-9]+)/revoke`)
	tokenRevokeRE = regexp.MustCompile(`/ui/account/tokens/([A-Za-z0-9]+)/revoke`)
	loginNonceRE  = regexp.MustCompile(`name="login_nonce" value="([^"]+)"`)
)

func (e *env) adminID(user string) string {
	e.t.Helper()
	a, err := e.st.GetAdminByUsername(context.Background(), user)
	if err != nil {
		e.t.Fatal(err)
	}
	return a.ID
}

func (e *env) mustPost(path string, form url.Values, want int) string {
	e.t.Helper()
	res, body := e.post(path, form, false)
	if res.StatusCode != want {
		e.t.Fatalf("setup POST %s: %d (want %d)\n%.300s", path, res.StatusCode, want, body)
	}
	return body
}

// forcePending leaves e.sess a password-only session of user.
func (e *env) forcePending(user string) { e.loginAsPasswordOnly(user) }

func uiMutationCases() map[string]uiMutation {
	const (
		summitName = "Summit Eye Care"
	)
	return map[string]uiMutation{
		// ---- sign-in ----
		"POST /login": {
			bestEffort: "login is an authentication event: auth.Manager.audit records it and a failed audit write does not lock the operator out of the console",
			build: func(e *env) func() (*http.Response, string) {
				e.login() // enrols the owner so the password step needs the MFA step
				e.sess, e.csrf = nil, ""
				return func() (*http.Response, string) {
					return e.do("POST", "/login", vals("username", "admin", "password", testPassword))
				}
			},
		},
		"POST /logout": {
			bestEffort: "signing out leaves an auth.logout row like signing in leaves auth.login, but the row is best effort and never blocks the exit (auth.Manager.EndSession)",
			noRerun:    "a sign-out spends its session; the best-effort contract is pinned by auth.TestEndSessionWritesALogoutRowAndNeverBlocksTheExit",
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				return func() (*http.Response, string) { return e.post("/logout", vals(), false) }
			},
		},
		"POST /login/mfa": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				e.forcePending("admin")
				return func() (*http.Response, string) { return e.post("/login/mfa", vals("code", e.mfa.code("admin")), false) }
			},
		},
		"POST /login/mfa/recovery": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				e.forcePending("admin")
				code := e.mfa.codes["admin"][0]
				return func() (*http.Response, string) { return e.post("/login/mfa/recovery", vals("code", code), false) }
			},
		},
		"POST /login/mfa/enroll": {
			ok: []int{http.StatusOK},
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				e.addAccount("newbie", "operator")
				e.forcePending("newbie")
				return func() (*http.Response, string) {
					_, page := e.do("GET", "/login/mfa", nil)
					m := secretRE.FindStringSubmatch(page)
					if m == nil {
						e.t.Fatal("the enrollment page shows no key")
					}
					secret := decodeSecret(e.t, m[1])
					e.mfa.mu.Lock()
					e.mfa.secrets["newbie"] = secret
					e.mfa.mu.Unlock()
					return e.post("/login/mfa/enroll", vals("code", e.mfa.code("newbie")), false)
				}
			},
		},
		"POST /login/mfa/cancel": {
			bestEffort: "cancelling the sign-in ends the session like a sign-out does, with the same best-effort auth.logout row (auth.Manager.EndSession)",
			noRerun:    "the cancel spends its pending session; the best-effort contract is pinned by auth.TestEndSessionWritesALogoutRowAndNeverBlocksTheExit",
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				e.forcePending("admin")
				return func() (*http.Response, string) { return e.post("/login/mfa/cancel", vals(), false) }
			},
		},
		"POST /login/reset": {
			ok: []int{http.StatusOK},
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				e.addAccount("resetme", "operator")
				body := e.mustPost("/ui/accounts/"+e.adminID("resetme")+"/reset-password", vals(), http.StatusOK)
				m := resetLinkRE.FindStringSubmatch(body)
				if m == nil {
					e.t.Fatal("no reset link")
				}
				u, _ := url.Parse(html.UnescapeString(m[1]))
				e.sess, e.csrf = nil, ""
				res, page := e.do("GET", u.RequestURI(), nil)
				nonce := loginNonceRE.FindStringSubmatch(page)
				if res.StatusCode != 200 || nonce == nil {
					e.t.Fatalf("reset page: %d", res.StatusCode)
				}
				e.adopt(res)
				return func() (*http.Response, string) {
					return e.do("POST", "/login/reset", vals("token", u.Query().Get("token"), "password", "a new strong password", "confirm", "a new strong password",
						auth.LoginNonceField, nonce[1]), "Origin", e.srv.URL)
				}
			},
		},
		// ---- own account ----
		"POST /ui/account/email": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				return func() (*http.Response, string) {
					return e.post("/ui/account/email", vals("email", "owner-new@example.invalid"), false)
				}
			},
		},
		"POST /ui/account/password": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				return func() (*http.Response, string) {
					return e.post("/ui/account/password", vals("current_password", testPassword, "new_password", "a new strong password", "confirm_password", "a new strong password"), false)
				}
			},
		},
		"POST /ui/account/totp": {
			ok:      []int{http.StatusOK},
			zeroWhy: "starting an enrollment keeps the pending secret in memory only; nothing is stored until the confirm, which is audited",
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				return func() (*http.Response, string) { return e.post("/ui/account/totp", vals(), false) }
			},
		},
		"POST /ui/account/totp/confirm": {
			ok: []int{http.StatusOK},
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				return func() (*http.Response, string) {
					res, body := e.post("/ui/account/totp", vals(), false)
					m := secretRE.FindStringSubmatch(body)
					if res.StatusCode != 200 || m == nil {
						e.t.Fatalf("begin: %d", res.StatusCode)
					}
					secret := decodeSecret(e.t, m[1])
					e.mfa.mu.Lock()
					e.mfa.at = e.mfa.at.Add(auth.TOTPPeriod)
					at := e.mfa.at
					cur := auth.TOTPCode(e.mfa.secrets["admin"], at)
					fresh := auth.TOTPCode(secret, at)
					e.mfa.mu.Unlock()
					return e.post("/ui/account/totp/confirm", vals("code", fresh, "current_code", cur), false)
				}
			},
		},
		"POST /ui/account/recovery-codes": {
			ok: []int{http.StatusOK},
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				return func() (*http.Response, string) {
					return e.post("/ui/account/recovery-codes", vals("code", e.mfa.code("admin")), false)
				}
			},
		},
		"POST /ui/account/sessions/{id}/revoke": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				e.loginAs("admin", testPassword) // a second session; the acting one is the newer
				page := e.page("/account", 200)
				m := sessionRevoke.FindStringSubmatch(page)
				if m == nil {
					e.t.Fatal("no other session to revoke")
				}
				return func() (*http.Response, string) { return e.post("/ui/account/sessions/"+m[1]+"/revoke", vals(), false) }
			},
		},
		"POST /ui/account/tokens": {
			ok: []int{http.StatusOK},
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				return func() (*http.Response, string) {
					return e.post("/ui/account/tokens", vals("name", "rmm", "scope", "read", "days", "30"), false)
				}
			},
		},
		"POST /ui/account/tokens/{id}/revoke": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				e.mustPost("/ui/account/tokens", vals("name", "doomed", "scope", "read", "days", "30"), http.StatusOK)
				m := tokenRevokeRE.FindStringSubmatch(e.page("/account", 200))
				if m == nil {
					e.t.Fatal("no token to revoke")
				}
				return func() (*http.Response, string) { return e.post("/ui/account/tokens/"+m[1]+"/revoke", vals(), false) }
			},
		},
		// ---- accounts (owner) ----
		"POST /ui/accounts": {
			ok: []int{http.StatusOK},
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				return func() (*http.Response, string) {
					return e.post("/ui/accounts", vals("username", "newhire", "email", "newhire@example.invalid", "role", "operator"), false)
				}
			},
		},
		"POST /ui/accounts/{id}/role": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				e.addAccount("rolechange", "read_only")
				id := e.adminID("rolechange")
				return func() (*http.Response, string) {
					return e.post("/ui/accounts/"+id+"/role", vals("role", "operator"), false)
				}
			},
		},
		"POST /ui/accounts/{id}/disable": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				e.addAccount("disableme", "operator")
				id := e.adminID("disableme")
				return func() (*http.Response, string) {
					return e.post("/ui/accounts/"+id+"/disable", vals("reason", "left"), false)
				}
			},
		},
		"POST /ui/accounts/{id}/enable": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				e.addAccount("enableme", "operator")
				id := e.adminID("enableme")
				e.mustPost("/ui/accounts/"+id+"/disable", vals("reason", "setup"), http.StatusSeeOther)
				return func() (*http.Response, string) { return e.post("/ui/accounts/"+id+"/enable", vals(), false) }
			},
		},
		"POST /ui/accounts/{id}/delete": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				e.addAccount("deleteme", "operator")
				id := e.adminID("deleteme")
				return func() (*http.Response, string) {
					return e.post("/ui/accounts/"+id+"/delete", vals("confirm", "deleteme"), false)
				}
			},
		},
		"POST /ui/accounts/{id}/reset-mfa": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				e.addAccount("lostphone", "operator")
				e.loginAs("lostphone", testPassword) // enrols TOTP
				e.loginAs("admin", testPassword)
				id := e.adminID("lostphone")
				return func() (*http.Response, string) {
					return e.post("/ui/accounts/"+id+"/reset-mfa", vals("reason", "lost phone"), false)
				}
			},
		},
		"POST /ui/accounts/{id}/reset-password": {
			ok: []int{http.StatusOK},
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				e.addAccount("forgot", "operator")
				id := e.adminID("forgot")
				return func() (*http.Response, string) { return e.post("/ui/accounts/"+id+"/reset-password", vals(), false) }
			},
		},
		// ---- customers and devices ----
		"POST /ui/customers": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				return func() (*http.Response, string) {
					return e.post("/ui/customers", vals("name", "Mutation Test Co"), false)
				}
			},
		},
		"POST /ui/customers/{id}": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				id := e.customerID(summitName)
				return func() (*http.Response, string) {
					return e.post("/ui/customers/"+id, vals("name", summitName, "notes", "edited by the audit test"), false)
				}
			},
		},
		"POST /ui/devices/{id}/settings": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				id := e.dev["FRONTDESK-01"]
				return func() (*http.Response, string) {
					return e.post("/ui/devices/"+id+"/settings", vals("notes", "edited by the audit test", "device_class", "desktop", "update_ring", "stable"), false)
				}
			},
		},
		// ---- commands ----
		"POST /ui/devices/{id}/backup-now": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				id := e.dev["FRONTDESK-01"]
				return func() (*http.Response, string) {
					return e.post("/ui/devices/"+id+"/backup-now", vals("key", "audit-bn"), false)
				}
			},
		},
		"POST /ui/devices/{id}/cancel-run": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				id := e.dev["DR-OFFICE-PC"]
				return func() (*http.Response, string) {
					return e.post("/ui/devices/"+id+"/cancel-run", vals("key", "audit-cr"), false)
				}
			},
		},
		"POST /ui/devices/{id}/refresh-policy": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				id := e.dev["FRONTDESK-01"]
				return func() (*http.Response, string) {
					return e.post("/ui/devices/"+id+"/refresh-policy", vals("key", "audit-rp"), false)
				}
			},
		},
		"POST /ui/devices/{id}/check-in": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				id := e.dev["FRONTDESK-01"]
				return func() (*http.Response, string) {
					return e.post("/ui/devices/"+id+"/check-in", vals("key", "audit-ci"), false)
				}
			},
		},
		"POST /ui/devices/{id}/commands/{cid}/cancel": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				id := e.dev["FRONTDESK-01"]
				e.mustPost("/ui/devices/"+id+"/backup-now", vals("key", "audit-undo"), http.StatusSeeOther)
				cmds := e.commandsOfType(id, "backup_now")
				if len(cmds) == 0 {
					e.t.Fatal("no queued command")
				}
				return func() (*http.Response, string) {
					return e.post("/ui/devices/"+id+"/commands/"+cmds[0].ID+"/cancel", vals(), false)
				}
			},
		},
		"POST /ui/devices/{id}/identity/resolve": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				id := e.dev["FRONTDESK-02"]
				e.flagIdentityConflict(id)
				return func() (*http.Response, string) {
					return e.post("/ui/devices/"+id+"/identity/resolve", vals("action", "keep", "reason", "the same machine"), false)
				}
			},
		},
		"POST /ui/devices/{id}/identity/resolve#retire": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				id := e.dev["FRONTDESK-02"]
				e.flagIdentityConflict(id)
				return func() (*http.Response, string) {
					return e.post("/ui/devices/"+id+"/identity/resolve", vals("action", "retire", "confirm", "FRONTDESK-02", "reason", "clone of a restored image"), false)
				}
			},
		},
		// ---- policies ----
		"POST /ui/policies": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				form := e.editorForm(e.policyByName("Default").ID)
				form.Set("name", "Audit UI Template")
				form.Set("f.conditions.battery_floor_pct", "30")
				return func() (*http.Response, string) { return e.post("/ui/policies", form, false) }
			},
		},
		"POST /ui/policies/{id}/impact": {
			ok:      []int{http.StatusOK},
			zeroWhy: "a blast-radius preview changes nothing; the audited step is the save that carries its token",
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				tpl := e.newTemplate("Preview Me", map[string]string{"sla": `{"stale_after_hours":48}`})
				form := e.editorForm(tpl.ID)
				form.Set("f.sla.stale_after_hours", "36")
				return func() (*http.Response, string) { return e.post("/ui/policies/"+tpl.ID+"/impact", form, false) }
			},
		},
		"POST /ui/policies/{id}/edit": {
			ok:      []int{http.StatusOK},
			zeroWhy: "'Edit again' re-renders the editor with the values the admin had typed; it stores nothing",
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				tpl := e.newTemplate("Edit Again", map[string]string{"sla": `{"stale_after_hours":48}`})
				form := e.editorForm(tpl.ID)
				return func() (*http.Response, string) { return e.post("/ui/policies/"+tpl.ID+"/edit", form, false) }
			},
		},
		"POST /ui/policies/{id}": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				tpl := e.newTemplate("Save Me", map[string]string{"sla": `{"stale_after_hours":48}`})
				form := e.editorForm(tpl.ID)
				form.Set("f.sla.stale_after_hours", "36")
				_, review := e.post("/ui/policies/"+tpl.ID+"/impact", form, false)
				confirm := hiddenOfPage(review)
				if confirm.Get("impact_token") == "" {
					e.t.Fatalf("no impact token in the review: %.300s", review)
				}
				return func() (*http.Response, string) { return e.post("/ui/policies/"+tpl.ID, confirm, false) }
			},
		},
		"POST /ui/policies/{id}/delete": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				tpl := e.newTemplate("Delete Me", map[string]string{"sla": `{"stale_after_hours":48}`})
				confirm := hiddenOfPage(e.page("/policies/"+tpl.ID+"/delete", 200))
				if confirm.Get("impact_token") == "" {
					e.t.Fatal("no impact token on the delete page")
				}
				return func() (*http.Response, string) { return e.post("/ui/policies/"+tpl.ID+"/delete", confirm, false) }
			},
		},
		"POST /ui/policy/window/validate": {
			ok:      []int{http.StatusOK},
			zeroWhy: "validates a schedule window and returns the verdict as a fragment; it stores nothing",
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				return func() (*http.Response, string) {
					return e.post("/ui/policy/window/validate", vals("f.schedule.window", "*/0-24"), true)
				}
			},
		},
		"POST /ui/customers/{id}/policy/impact": {
			ok:      []int{http.StatusOK},
			zeroWhy: "a blast-radius preview changes nothing; the audited step is the assignment that carries its token",
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				id := e.customerID(summitName)
				tpl := e.newTemplate("Assign Preview", map[string]string{"sla": `{"stale_after_hours":48}`})
				return func() (*http.Response, string) {
					return e.post("/ui/customers/"+id+"/policy/impact", vals("policy_id", tpl.ID), false)
				}
			},
		},
		"POST /ui/customers/{id}/policy": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				id := e.customerID(summitName)
				tpl := e.newTemplate("Assign Me", map[string]string{"sla": `{"stale_after_hours":48}`})
				_, review := e.post("/ui/customers/"+id+"/policy/impact", vals("policy_id", tpl.ID), false)
				confirm := hiddenOfPage(review)
				if confirm.Get("impact_token") == "" {
					e.t.Fatalf("no impact token in the review: %.300s", review)
				}
				return func() (*http.Response, string) { return e.post("/ui/customers/"+id+"/policy", confirm, false) }
			},
		},
		"POST /ui/devices/{id}/override": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				id := e.dev["FRONTDESK-01"]
				return func() (*http.Response, string) {
					return e.post("/ui/devices/"+id+"/override", vals("o.sla.stale_after_hours", "1", "f.sla.stale_after_hours", "36", "reason", "audit test"), false)
				}
			},
		},
		"POST /ui/devices/{id}/override/clear": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				id := e.dev["FRONTDESK-01"]
				e.mustPost("/ui/devices/"+id+"/override", vals("o.sla.stale_after_hours", "1", "f.sla.stale_after_hours", "36", "reason", "setup"), http.StatusSeeOther)
				return func() (*http.Response, string) { return e.post("/ui/devices/"+id+"/override/clear", vals(), false) }
			},
		},
		// ---- enrollment ----
		"POST /ui/enroll/token": {
			ok: []int{http.StatusOK},
			build: func(e *env) func() (*http.Response, string) {
				e.svc.PublicURL = wizardURL
				e.svc.ArtifactsDir = e.t.TempDir()
				e.login()
				id := e.customerID(summitName)
				return func() (*http.Response, string) { return e.createToken(id, nil) }
			},
		},
		"POST /ui/enroll/tokens/{id}/revoke": {
			build: func(e *env) func() (*http.Response, string) {
				e.svc.PublicURL = wizardURL
				e.svc.ArtifactsDir = e.t.TempDir()
				e.login()
				cust := e.customerID(summitName)
				if res, body := e.createToken(cust, vals("label", "revoke me")); res.StatusCode != 200 {
					e.t.Fatalf("create token: %d %.300s", res.StatusCode, body)
				}
				toks, err := e.st.ListEnrollmentTokens(context.Background(), store.ScopeAll(), cust)
				if err != nil || len(toks) == 0 {
					e.t.Fatalf("tokens: %v", err)
				}
				var id string
				for _, t := range toks {
					if t.Label == "revoke me" {
						id = t.ID
					}
				}
				if id == "" {
					e.t.Fatal("the new token is not listed")
				}
				return func() (*http.Response, string) { return e.post("/ui/enroll/tokens/"+id+"/revoke", vals(), false) }
			},
		},
		// ---- restores ----
		"POST /ui/devices/{id}/restore": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				id := e.dev["DR-OFFICE-PC"]
				return func() (*http.Response, string) {
					return e.post("/ui/devices/"+id+"/restore", vals("snapshot", fixtures.FixtureSnapshotID, "action", "submit", "path", "/C/Users/Public/Desktop/Welcome.txt"), false)
				}
			},
		},
		"POST /ui/restores/{id}/download": {
			zeroWhy: "minting hands out a short-lived token and stores nothing durable; the audited event is the download that spends it",
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				id := e.dev["DR-OFFICE-PC"]
				res, body := e.post("/ui/devices/"+id+"/restore", vals("snapshot", fixtures.FixtureSnapshotID, "action", "submit", "path", "/C/Users/Public/Desktop/Welcome.txt"), false)
				loc := res.Header.Get("Location")
				if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/restores/") {
					e.t.Fatalf("restore: %d %s %.200s", res.StatusCode, loc, body)
				}
				rid := strings.TrimPrefix(strings.SplitN(loc, "?", 2)[0], "/restores/")
				return func() (*http.Response, string) { return e.post("/ui/restores/"+rid+"/download", vals(), false) }
			},
		},
		// ---- settings ----
		"POST /ui/settings": {
			// One audit row per changed key (ADR 0012). On a fresh database neither key has an
			// operator-stored value yet, so saving the zone and the refresh interval stores both.
			audits: 2,
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				return func() (*http.Response, string) {
					return e.post("/ui/settings", vals("display_timezone", "Asia/Tokyo", "refresh_interval_s", "30"), false)
				}
			},
		},
		// ---- M3-T4: the System page's "Run now" (written by the admin API's own handler, one row) ----
		"POST /ui/system/jobs/{name}/run": {
			build: func(e *env) func() (*http.Response, string) {
				e.login()
				e.svc.Jobs = newFakeJobs("alarm-evaluator") // before the first request: the page API reads it once
				return func() (*http.Response, string) {
					return e.post("/ui/system/jobs/alarm-evaluator/run", vals(), false)
				}
			},
		},
	}
}

func decodeSecret(t *testing.T, shown string) []byte {
	t.Helper()
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ReplaceAll(shown, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func (e *env) flagIdentityConflict(deviceID string) {
	e.t.Helper()
	if err := e.st.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE devices SET identity_conflict_at = ? WHERE id = ?`, e.now.UnixMilli(), deviceID)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

func baseRoute(key string) string { p, _, _ := strings.Cut(key, "#"); return p }

func TestEveryUIPostIsAudited(t *testing.T) {
	cases := uiMutationCases()

	want := uiPostRoutes()
	covered := map[string]bool{}
	for p := range cases {
		covered[baseRoute(p)] = true
		if !want[baseRoute(p)] {
			t.Errorf("uiMutationCases has a case for %q, which is not a UI POST route", p)
		}
	}
	for p := range want {
		if !covered[p] {
			t.Errorf("UI POST route %q has no case in uiMutationCases: add one (build valid fixtures, name the audit rows it writes)", p)
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
			mk := m.build(e)
			wantAudits := m.audits
			if wantAudits == 0 && m.zeroWhy == "" {
				wantAudits = 1
			}

			// 1. A change that cannot be audited is refused (a 4xx or 5xx page, never a success) and changes nothing.
			if m.zeroWhy == "" && m.bestEffort == "" {
				e.mutRefuseAudits(true)
				before, auditsBefore := e.mutDigest(), e.mutAuditCount()
				res, body := mk()
				var bad []string
				if res.StatusCode < 400 {
					bad = append(bad, fmt.Sprintf("answered %d although its audit row could not be written", res.StatusCode))
				}
				if strings.Contains(body, "audit refused") {
					bad = append(bad, "the page names the cause")
				}
				if after := e.mutDigest(); after != before {
					bad = append(bad, "changed stored rows although its audit row could not be written")
				}
				if got := e.mutAuditCount(); got != auditsBefore {
					bad = append(bad, fmt.Sprintf("audit rows moved from %d to %d with the audit table refusing inserts", auditsBefore, got))
				}
				e.mutRefuseAudits(false)
				t.Logf("refused with %d", res.StatusCode)
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

			// 2. The same request with a healthy audit table succeeds and writes the documented rows.
			before, digestBefore := e.mutAuditCount(), e.mutDigest()
			res, body := mk()
			if !m.okStatus(res.StatusCode) {
				t.Fatalf("%s: status %d (location %q), want one of %v\n%.400s", pattern, res.StatusCode, res.Header.Get("Location"), m.ok, body)
			}
			if got := e.mutAuditCount() - before; got != wantAudits {
				t.Errorf("%s wrote %d audit rows %v, want %d", pattern, got, e.mutAuditActionsSince(before), wantAudits)
			}
			// A route that writes no audit row has nothing to audit because it changes nothing.
			if m.zeroWhy != "" && !m.changesState && e.mutDigest() != digestBefore {
				t.Errorf("%s writes no audit row (%s) but changed stored rows", pattern, m.zeroWhy)
			}

			// 3. A best-effort route is documented to carry on when its audit table is down.
			if m.bestEffort != "" && m.noRerun == "" && wantAudits > 0 {
				e.mutRefuseAudits(true)
				res, body = mk()
				e.mutRefuseAudits(false)
				if !m.okStatus(res.StatusCode) {
					t.Errorf("best effort (%s): with the audit table down %s answered %d: %.200s", m.bestEffort, pattern, res.StatusCode, body)
				}
			}
		})
	}
}
