package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// addEnrollM2Matrix adds the M2-W2B token-management methods to the §3.5 scope
// matrix (matrix() in scope_test.go calls it).
func addEnrollM2Matrix(m map[string]matrixCase) {
	actor := AuditActor{AdminID: "01J9Y4K3A0B1C2D3E4F5G6H7AA", Username: "matrix", Method: "api"}
	m["ListEnrollmentTokenRows"] = matrixCase{kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
		l, err := w.s.ListEnrollmentTokenRows(w.ctx, sc, EnrollTokenFilter{Size: 200})
		var out []string
		for _, r := range l.Rows {
			out = append(out, r.CustomerID)
		}
		return out, err
	}}
	m["GetEnrollmentTokenRow"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.GetEnrollmentTokenRow(w.ctx, sc, t.freshToken)
		return nil, err
	}, prep: func(w *world, t *target) {
		t.freshToken = must[EnrollmentToken](w.t)(w.s.CreateEnrollmentToken(w.ctx, ScopeAll(), EnrollmentToken{CustomerID: t.cust, TokenHash: sha256Hex(uniq("get")), ExpiresAt: w.clk.Now().Add(time.Hour)})).ID
	}}
	m["ListEnrollmentTokenDevices"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.ListEnrollmentTokenDevices(w.ctx, sc, t.freshToken)
		return nil, err
	}, prep: func(w *world, t *target) {
		t.freshToken = must[EnrollmentToken](w.t)(w.s.CreateEnrollmentToken(w.ctx, ScopeAll(), EnrollmentToken{CustomerID: t.cust, TokenHash: sha256Hex(uniq("devs")), ExpiresAt: w.clk.Now().Add(time.Hour)})).ID
	}}
	m["CreateEnrollmentTokenAudited"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.CreateEnrollmentTokenAudited(w.ctx, sc, EnrollTokenInput{CustomerID: t.cust, TokenHash: sha256Hex(uniq("aud")), ExpiresAt: w.clk.Now().Add(time.Hour)}, actor)
		return nil, err
	}}
	m["RevokeEnrollmentTokenAudited"] = matrixCase{kind: byID,
		prep: func(w *world, t *target) {
			t.freshToken = must[EnrollmentToken](w.t)(w.s.CreateEnrollmentToken(w.ctx, ScopeAll(), EnrollmentToken{CustomerID: t.cust, TokenHash: sha256Hex(uniq("raud")), ExpiresAt: w.clk.Now().Add(time.Hour)})).ID
		},
		call: func(w *world, sc Scope, t *target) ([]string, error) {
			_, err := w.s.RevokeEnrollmentTokenAudited(w.ctx, sc, t.freshToken, actor)
			return nil, err
		}}
}

func tokenInput(w *world, secret string) EnrollTokenInput {
	return EnrollTokenInput{CustomerID: w.custA.ID, Label: "Front office", TokenHash: sha256Hex(secret), ExpiresAt: w.clk.Now().Add(48 * time.Hour)}
}

func TestCreateEnrollmentTokenAuditedWritesOneRowAndNoSecret(t *testing.T) {
	w := seed(t)
	before := auditTotal(t, w.s)
	uses := 3
	in := tokenInput(w, "the-secret-value")
	in.MaxUses, in.DeviceClass = &uses, "laptop"
	ra := AuditActor{AdminID: w.admin.ID, Username: "tester", IP: "192.0.2.10", Method: "web"}
	tok, err := w.s.CreateEnrollmentTokenAudited(w.ctx, ScopeAll(), in, ra)
	if err != nil {
		t.Fatal(err)
	}
	if tok.TokenHash != sha256Hex("the-secret-value") || tok.CreatedBy != w.admin.ID || tok.DeviceClassDefault != "laptop" || tok.MaxUses == nil || *tok.MaxUses != 3 {
		t.Fatalf("token = %+v", tok)
	}
	if row := must[EnrollTokenRow](t)(w.s.GetEnrollmentTokenRow(w.ctx, ScopeAll(), tok.ID)); row.CreatedByName != "root" || row.CustomerName != "Alpha Dental" || row.LastUsedAt != nil {
		t.Fatalf("row names = %q / %q, last used %v", row.CreatedByName, row.CustomerName, row.LastUsedAt)
	}
	if got := auditTotal(t, w.s); got != before+1 {
		t.Fatalf("%d audit rows added, want exactly 1", got-before)
	}
	rows := auditRows(t, w.s, "enrollment_token.create", tok.ID)
	if len(rows) != 1 || rows[0].CustomerID != w.custA.ID || rows[0].ActorLabel != "tester" || rows[0].Result != "ok" {
		t.Fatalf("audit rows = %+v", rows)
	}
	all := string(rows[0].Details) + rows[0].ObjectID + rows[0].ActorLabel
	for _, bad := range []string{"the-secret-value", tok.TokenHash, "Front office"} {
		if strings.Contains(all, bad) {
			t.Errorf("the audit row carries %q: %s", bad, all)
		}
	}
}

func TestCreateEnrollmentTokenAuditedValidates(t *testing.T) {
	w := seed(t)
	other := must[PolicyRecord](t)(w.s.CreatePolicy(w.ctx, ScopeAll(), PolicyInput{Scope: "customer", CustomerID: w.custB.ID, Name: "B only"}))
	zero, many := 0, MaxTokenUses+1
	archived := must[Customer](t)(w.s.CreateCustomer(w.ctx, ScopeAll(), Customer{Name: "Old Customer"}))
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`UPDATE customers SET state = 'archived', archived_at = 1000000000000 WHERE id = ?`, archived.ID)
		return err
	}))
	for name, tc := range map[string]struct {
		mut   func(*EnrollTokenInput)
		field string
	}{
		"past expiry":   {func(in *EnrollTokenInput) { in.ExpiresAt = w.clk.Now().Add(-time.Minute) }, "expires"},
		"too long":      {func(in *EnrollTokenInput) { in.ExpiresAt = w.clk.Now().Add(MaxTokenLifetime + time.Hour) }, "expires"},
		"zero uses":     {func(in *EnrollTokenInput) { in.MaxUses = &zero }, "max_uses"},
		"too many uses": {func(in *EnrollTokenInput) { in.MaxUses = &many }, "max_uses"},
		"bad class":     {func(in *EnrollTokenInput) { in.DeviceClass = "server" }, "device_class"},
		"long label":    {func(in *EnrollTokenInput) { in.Label = strings.Repeat("x", MaxTokenLabel+1) }, "label"},
		"newline label": {func(in *EnrollTokenInput) { in.Label = "a\nb" }, "label"},
		"other policy":  {func(in *EnrollTokenInput) { in.PolicyID = other.ID }, "policy_id"},
		"archived":      {func(in *EnrollTokenInput) { in.CustomerID = archived.ID }, "customer_id"},
	} {
		in := tokenInput(w, "v-"+name)
		tc.mut(&in)
		_, err := w.s.CreateEnrollmentTokenAudited(w.ctx, ScopeAll(), in, testActor)
		if !errors.Is(err, ErrInvalid) || fieldsOf(err)[tc.field] == "" {
			t.Errorf("%s: err = %v, want a %s field error", name, err, tc.field)
		}
	}
	if got := len(auditRows(t, w.s, "enrollment_token.create", "")); got != 0 {
		t.Errorf("%d audit rows after rejected creates", got)
	}
}

func TestEnrollmentTokenMutationsAreRefusedWhenNotAudited(t *testing.T) {
	w := seed(t)
	live := must[EnrollmentToken](t)(w.s.CreateEnrollmentTokenAudited(w.ctx, ScopeAll(), tokenInput(w, "live"), testActor))
	list := func() int {
		l := must[EnrollTokenList](t)(w.s.ListEnrollmentTokenRows(w.ctx, ScopeAll(), EnrollTokenFilter{Size: 200}))
		return l.All
	}
	n := list()
	refuseAudits(t, w.s)
	if _, err := w.s.CreateEnrollmentTokenAudited(w.ctx, ScopeAll(), tokenInput(w, "refused"), testActor); !errors.Is(err, ErrAudit) {
		t.Fatalf("create without audit: %v, want ErrAudit", err)
	}
	if list() != n {
		t.Fatal("a token was created although its audit row could not be written")
	}
	if _, err := w.s.RevokeEnrollmentTokenAudited(w.ctx, ScopeAll(), live.ID, testActor); !errors.Is(err, ErrAudit) {
		t.Fatalf("revoke without audit: %v, want ErrAudit", err)
	}
	row := must[EnrollTokenRow](t)(w.s.GetEnrollmentTokenRow(w.ctx, ScopeAll(), live.ID))
	if row.Status(w.clk.Now()) != TokenActive {
		t.Fatalf("status %s: the token was revoked although the audit row failed", row.Status(w.clk.Now()))
	}
}

func TestRevokeEnrollmentTokenAuditedIsIdempotentAndStopsEnrollment(t *testing.T) {
	w := seed(t)
	tok := must[EnrollmentToken](t)(w.s.CreateEnrollmentTokenAudited(w.ctx, ScopeAll(), tokenInput(w, "rev-me"), testActor))
	// It enrolls a device while live.
	if _, err := w.s.AgentEnroll(w.ctx, enrollInput("rev-me", "BEFORE-1")); err != nil {
		t.Fatalf("enroll with a live token: %v", err)
	}
	changed, err := w.s.RevokeEnrollmentTokenAudited(w.ctx, ScopeAll(), tok.ID, testActor)
	if err != nil || !changed {
		t.Fatalf("revoke: changed=%v err=%v", changed, err)
	}
	if rows := auditRows(t, w.s, "enrollment_token.revoke", tok.ID); len(rows) != 1 || rows[0].CustomerID != w.custA.ID {
		t.Fatalf("revoke audit rows = %+v", rows)
	}
	changed, err = w.s.RevokeEnrollmentTokenAudited(w.ctx, ScopeAll(), tok.ID, testActor)
	if err != nil || changed {
		t.Fatalf("second revoke: changed=%v err=%v, want a quiet no-op", changed, err)
	}
	if rows := auditRows(t, w.s, "enrollment_token.revoke", tok.ID); len(rows) != 1 {
		t.Fatalf("a no-op revoke wrote an audit row (%d)", len(rows))
	}
	if _, err := w.s.AgentEnroll(w.ctx, enrollInput("rev-me", "AFTER-1")); err == nil {
		t.Fatal("a revoked token enrolled a device")
	}
	// Devices that already enrolled keep their row.
	devs := must[[]EnrolledDevice](t)(w.s.ListEnrollmentTokenDevices(w.ctx, ScopeAll(), tok.ID))
	if len(devs) != 1 || devs[0].Hostname != "BEFORE-1" {
		t.Fatalf("devices of the token = %+v", devs)
	}
}

func TestTokenUsesAndExpiryAreEnforcedAndShownInTheList(t *testing.T) {
	w := seed(t)
	two := 2
	in := tokenInput(w, "capped")
	in.MaxUses = &two
	tok := must[EnrollmentToken](t)(w.s.CreateEnrollmentTokenAudited(w.ctx, ScopeAll(), in, testActor))
	for i, host := range []string{"CAP-1", "CAP-2"} {
		if _, err := w.s.AgentEnroll(w.ctx, enrollInput("capped", host)); err != nil {
			t.Fatalf("enroll %d: %v", i+1, err)
		}
	}
	if _, err := w.s.AgentEnroll(w.ctx, enrollInput("capped", "CAP-3")); err == nil {
		t.Fatal("a used-up token enrolled a third device")
	}
	row := must[EnrollTokenRow](t)(w.s.GetEnrollmentTokenRow(w.ctx, ScopeAll(), tok.ID))
	if row.Status(w.clk.Now()) != TokenUsedUp || row.UsesLeft() != 0 || row.Devices != 2 || row.LastUsedAt == nil {
		t.Fatalf("row = status %s left %d devices %d last %v", row.Status(w.clk.Now()), row.UsesLeft(), row.Devices, row.LastUsedAt)
	}
	if row.CreatedByName != "" { // testActor is not a stored account
		t.Fatalf("created-by name %q for a missing account", row.CreatedByName)
	}
	// Expiry: the same token, past its time.
	exp := must[EnrollmentToken](t)(w.s.CreateEnrollmentTokenAudited(w.ctx, ScopeAll(), tokenInput(w, "short"), testActor))
	w.clk.Advance(49 * time.Hour)
	if _, err := w.s.AgentEnroll(w.ctx, enrollInput("short", "LATE-1")); err == nil {
		t.Fatal("an expired token enrolled a device")
	}
	if st := must[EnrollTokenRow](t)(w.s.GetEnrollmentTokenRow(w.ctx, ScopeAll(), exp.ID)).Status(w.clk.Now()); st != TokenExpired {
		t.Fatalf("status = %s, want expired", st)
	}
}

func TestListEnrollmentTokenRowsFiltersPagesAndScopes(t *testing.T) {
	w := seed(t) // seed leaves one active token for each of A and B
	revoked := must[EnrollmentToken](t)(w.s.CreateEnrollmentTokenAudited(w.ctx, ScopeAll(), tokenInput(w, "r1"), testActor))
	if _, err := w.s.RevokeEnrollmentTokenAudited(w.ctx, ScopeAll(), revoked.ID, testActor); err != nil {
		t.Fatal(err)
	}
	short := tokenInput(w, "e1")
	short.ExpiresAt = w.clk.Now().Add(time.Hour)
	must[EnrollmentToken](t)(w.s.CreateEnrollmentTokenAudited(w.ctx, ScopeAll(), short, testActor))
	w.clk.Advance(2 * time.Hour) // e1 and the seeded tokens' siblings expire; the seeded tokens live 24 h
	cases := []struct {
		status string
		want   int
	}{{"", 4}, {TokenActive, 2}, {TokenRevoked, 1}, {TokenExpired, 1}, {TokenUsedUp, 0}}
	for _, c := range cases {
		l := must[EnrollTokenList](t)(w.s.ListEnrollmentTokenRows(w.ctx, ScopeAll(), EnrollTokenFilter{Status: c.status}))
		if len(l.Rows) != c.want || l.Page.Total != c.want || l.All != 4 {
			t.Errorf("status %q: %d rows, total %d, all %d, want %d of 4", c.status, len(l.Rows), l.Page.Total, l.All, c.want)
		}
		for _, r := range l.Rows {
			if c.status != "" && r.Status(w.clk.Now()) != c.status {
				t.Errorf("status %q list holds a %s token", c.status, r.Status(w.clk.Now()))
			}
			if r.CustomerName == "" {
				t.Errorf("row without a customer name: %+v", r)
			}
		}
	}
	if _, err := w.s.ListEnrollmentTokenRows(w.ctx, ScopeAll(), EnrollTokenFilter{Status: "bogus"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown status: %v", err)
	}
	// Customer filter and scope.
	l := must[EnrollTokenList](t)(w.s.ListEnrollmentTokenRows(w.ctx, ScopeAll(), EnrollTokenFilter{CustomerID: w.custB.ID}))
	if len(l.Rows) != 1 || l.Rows[0].CustomerID != w.custB.ID {
		t.Fatalf("customer filter: %+v", l.Rows)
	}
	l = must[EnrollTokenList](t)(w.s.ListEnrollmentTokenRows(w.ctx, ScopeOf(w.custB.ID), EnrollTokenFilter{CustomerID: w.custA.ID}))
	if len(l.Rows) != 0 || l.All != 1 {
		t.Fatalf("a scoped caller filtering on another customer: %d rows, all %d", len(l.Rows), l.All)
	}
	// Paging: size 25 is the smallest page; 4 rows fit on one.
	if l := must[EnrollTokenList](t)(w.s.ListEnrollmentTokenRows(w.ctx, ScopeAll(), EnrollTokenFilter{Size: 25, Page: 9})); !l.Page.Clamped || l.Page.Page != 1 {
		t.Fatalf("page past the end: %+v", l.Page)
	}
}
