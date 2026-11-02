package store

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestValidateSetting(t *testing.T) {
	cases := []struct {
		key, val string
		want     string // canonical value, or "" when it must be refused
	}{
		{SettingDisplayTimezone, `"America/Chicago"`, `"America/Chicago"`},
		{SettingDisplayTimezone, `"  Europe/Berlin "`, `"Europe/Berlin"`},
		{SettingDisplayTimezone, `"UTC"`, `"UTC"`},
		{SettingDisplayTimezone, `""`, ""},
		{SettingDisplayTimezone, `"Local"`, ""},
		{SettingDisplayTimezone, `"Mars/Olympus_Mons"`, ""},
		{SettingDisplayTimezone, `"../../etc/passwd"`, ""},
		{SettingDisplayTimezone, `5`, ""},
		{SettingDisplayTimezone, `{"a":1}`, ""},
		{SettingRefreshInterval, `30`, `30`},
		{SettingRefreshInterval, ` 5 `, `5`},
		{SettingRefreshInterval, `3600`, `3600`},
		{SettingRefreshInterval, `4`, ""},
		{SettingRefreshInterval, `3601`, ""},
		{SettingRefreshInterval, `12.5`, ""},
		{SettingRefreshInterval, `"30"`, ""},
		{SettingRefreshInterval, `null`, ""},
	}
	for _, c := range cases {
		got, err := ValidateSetting(c.key, json.RawMessage(c.val))
		if c.want == "" {
			fe := FieldErrors(err)
			if !errors.Is(err, ErrInvalid) || len(fe) != 1 || fe[0].Field != "value" {
				t.Errorf("%s=%s: err = %v, want one error on value", c.key, c.val, err)
			} else if strings.Contains(fe[0].Message, "Mars") || strings.Contains(fe[0].Message, "passwd") {
				t.Errorf("%s=%s: the message echoes the input: %q", c.key, c.val, fe[0].Message)
			}
			continue
		}
		if err != nil || string(got) != c.want {
			t.Errorf("%s=%s: got %s, %v; want %s", c.key, c.val, got, err, c.want)
		}
	}
	if _, err := ValidateSetting("no_such_key", json.RawMessage(`1`)); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown key: %v, want ErrNotFound", err)
	}
	// A setting nothing honours yet is shown but not editable.
	_, err := ValidateSetting("audit_retention_days", json.RawMessage(`120`))
	if fe := FieldErrors(err); len(fe) != 1 || fe[0].Field != "key" {
		t.Errorf("read-only setting: %v", err)
	}
}

func TestSetSettingsAuditedWritesOneRowPerChange(t *testing.T) {
	w := seed(t)
	actor := AuditActor{AdminID: w.admin.ID, Username: w.admin.Username, IP: "10.0.0.9", Method: "web"}
	before := auditCount(t, w, "settings.update")

	ch, err := w.s.SetSettingsAudited(w.ctx, ScopeAll(), []SettingInput{
		{SettingDisplayTimezone, json.RawMessage(`"America/New_York"`)},
		{SettingRefreshInterval, json.RawMessage(`45`)},
	}, actor)
	ok(t, err)
	if len(ch) != 2 || ch[0].Unchanged || ch[1].Unchanged {
		t.Fatalf("changes = %+v", ch)
	}
	if got := auditCount(t, w, "settings.update") - before; got != 2 {
		t.Fatalf("%d audit rows for two changed settings, want 2", got)
	}
	tb := must[AuditTable](t)(w.s.AuditTable(w.ctx, ScopeAll(), AuditQuery{Action: "settings.update"}))
	var seenZone bool
	for _, e := range tb.Rows {
		if e.ActorID != w.admin.ID || e.ActorLabel != w.admin.Username || e.Method != "web" || e.SourceIP != "10.0.0.9" || e.ObjectType != "setting" || e.Result != "ok" {
			t.Errorf("audit row = %+v", e)
		}
		var d map[string]json.RawMessage
		ok(t, json.Unmarshal(e.Details, &d))
		if e.ObjectID == SettingDisplayTimezone {
			seenZone = true
			if string(d["to"]) != `"America/New_York"` || d["from"] == nil {
				t.Errorf("zone details = %s", e.Details)
			}
		}
	}
	if !seenZone {
		t.Fatal("no audit row for the zone")
	}
	row := must[Setting](t)(w.s.GetSetting(w.ctx, SettingDisplayTimezone))
	if string(row.Value) != `"America/New_York"` || row.UpdatedBy != w.admin.ID {
		t.Fatalf("stored setting = %s by %q", row.Value, row.UpdatedBy)
	}

	// Saving what is already stored writes nothing and audits nothing.
	n := auditCount(t, w, "settings.update")
	ch, err = w.s.SetSettingsAudited(w.ctx, ScopeAll(), []SettingInput{{SettingDisplayTimezone, json.RawMessage(`"America/New_York"`)}}, actor)
	ok(t, err)
	if !ch[0].Unchanged || auditCount(t, w, "settings.update") != n {
		t.Fatalf("no-op save: %+v, audit rows %d -> %d", ch, n, auditCount(t, w, "settings.update"))
	}

	// One bad value refuses the whole call: the good one is not applied either.
	n = auditCount(t, w, "settings.update")
	_, err = w.s.SetSettingsAudited(w.ctx, ScopeAll(), []SettingInput{
		{SettingRefreshInterval, json.RawMessage(`60`)},
		{SettingDisplayTimezone, json.RawMessage(`"Nowhere/Land"`)},
	}, actor)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if v := must[Setting](t)(w.s.GetSetting(w.ctx, SettingRefreshInterval)); string(v.Value) != `45` || auditCount(t, w, "settings.update") != n {
		t.Fatalf("a refused call changed %s or wrote audit rows", v.Value)
	}

	// Duplicate keys, empty calls and a customer-scoped caller.
	if _, err = w.s.SetSettingsAudited(w.ctx, ScopeAll(), []SettingInput{{SettingRefreshInterval, json.RawMessage(`10`)}, {SettingRefreshInterval, json.RawMessage(`11`)}}, actor); !errors.Is(err, ErrInvalid) {
		t.Errorf("duplicate key: %v", err)
	}
	if _, err = w.s.SetSettingsAudited(w.ctx, ScopeAll(), nil, actor); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty call: %v", err)
	}
	if _, err = w.s.SetSettingsAudited(w.ctx, ScopeOf(w.custA.ID), []SettingInput{{SettingRefreshInterval, json.RawMessage(`10`)}}, actor); !errors.Is(err, ErrScope) {
		t.Errorf("scoped caller: %v, want ErrScope", err)
	}
}

// A change whose audit row cannot be written is refused and nothing changes
// ("NOT AUDITED"): the setting keeps its value.
func TestSetSettingsAuditedIsRefusedWhenItCannotBeAudited(t *testing.T) {
	w := seed(t)
	ok(t, w.s.Write(w.ctx, func(tx *Tx) error {
		_, err := tx.Exec(`CREATE TRIGGER t_no_audit BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT, 'audit down'); END`)
		return err
	}))
	_, err := w.s.SetSettingsAudited(w.ctx, ScopeAll(), []SettingInput{{SettingRefreshInterval, json.RawMessage(`77`)}}, AuditActor{AdminID: w.admin.ID, Username: "x", Method: "api"})
	if !errors.Is(err, ErrAudit) {
		t.Fatalf("err = %v, want ErrAudit", err)
	}
	if _, err := w.s.GetSetting(w.ctx, SettingRefreshInterval); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the setting was written without its audit row: %v", err)
	}
}

func auditCount(t *testing.T, w *world, action string) int {
	t.Helper()
	tb, err := w.s.AuditTable(w.ctx, ScopeAll(), AuditQuery{Action: action})
	ok(t, err)
	return tb.Page.Total
}

func TestListSettingValuesNamesWhoChangedIt(t *testing.T) {
	w := seed(t)
	vals := must[[]SettingValue](t)(w.s.ListSettingValues(w.ctx, ScopeAll()))
	byKey := map[string]SettingValue{}
	for _, v := range vals {
		byKey[v.Key] = v
	}
	if len(vals) != len(SettingDefs()) || string(byKey["audit_retention_days"].Stored) != "400" || byKey["audit_retention_days"].Editable {
		t.Fatalf("seeded values: %+v", byKey["audit_retention_days"])
	}
	if byKey[SettingRefreshInterval].Stored != nil {
		t.Fatalf("the refresh interval has no row until it is set: %s", byKey[SettingRefreshInterval].Stored)
	}
	_, err := w.s.SetSettingsAudited(w.ctx, ScopeAll(), []SettingInput{{SettingRefreshInterval, json.RawMessage(`20`)}}, AuditActor{AdminID: w.admin.ID, Username: w.admin.Username, Method: "web"})
	ok(t, err)
	got := must[[]SettingValue](t)(w.s.ListSettingValues(w.ctx, ScopeAll()))
	for _, v := range got {
		if v.Key == SettingRefreshInterval && (string(v.Stored) != "20" || v.UpdatedByName != w.admin.Username || v.UpdatedAt.IsZero()) {
			t.Fatalf("after the change: %+v", v)
		}
	}
	if _, err := w.s.ListSettingValues(w.ctx, ScopeOf(w.custA.ID)); !errors.Is(err, ErrScope) {
		t.Fatalf("scoped caller: %v", err)
	}
}

// ADR 0012: only a value the operator stored beats config.json; the migration's
// seed ("UTC") is nobody's choice.
func TestFreshDatabaseFollowsConfigNotTheMigrationSeed(t *testing.T) {
	w := seed(t)
	d := must[*DisplayPrefs](t)(w.s.LoadDisplayPrefs(w.ctx, "America/Chicago", 45))
	if d.Zone() != "America/Chicago" || d.RefreshS() != 45 || d.Note() != "" || len(d.ConfigNotes()) != 0 {
		t.Fatalf("fresh database: zone %q refresh %d note %q config notes %v", d.Zone(), d.RefreshS(), d.Note(), d.ConfigNotes())
	}
}

func TestOperatorStoredValuesWinAndSaysSoWhenConfigDiffers(t *testing.T) {
	w := seed(t)
	// init stores the zone as the operator's choice.
	ok(t, w.s.PutOperatorSetting(w.ctx, SettingDisplayTimezone, json.RawMessage(`"Asia/Tokyo"`), ""))
	d := must[*DisplayPrefs](t)(w.s.LoadDisplayPrefs(w.ctx, "America/Chicago", 30))
	if d.Zone() != "Asia/Tokyo" {
		t.Fatalf("zone %q: the operator's stored zone lost to config", d.Zone())
	}
	if n := d.ConfigNotes(); len(n) != 1 || !strings.Contains(n[0], "America/Chicago") || !strings.Contains(n[0], "Asia/Tokyo") || !strings.Contains(n[0], "wins") {
		t.Fatalf("notes: %v", n)
	}
	// The same value as config is not worth a note.
	d = must[*DisplayPrefs](t)(w.s.LoadDisplayPrefs(w.ctx, "Asia/Tokyo", 30))
	if len(d.ConfigNotes()) != 0 {
		t.Fatalf("notes for equal values: %v", d.ConfigNotes())
	}

	// A save through the console is the operator's choice, for both keys, even when it equals the seed.
	w2 := seed(t)
	actor := AuditActor{AdminID: w2.admin.ID, Username: w2.admin.Username, Method: "web"}
	ch := must[[]SettingChange](t)(w2.s.SetSettingsAudited(w2.ctx, ScopeAll(), []SettingInput{{SettingDisplayTimezone, json.RawMessage(`"UTC"`)}, {SettingRefreshInterval, json.RawMessage(`90`)}}, actor))
	if ch[0].Unchanged {
		t.Fatal("saving the seed value must still make it the operator's (a change, with its audit row)")
	}
	d = must[*DisplayPrefs](t)(w2.s.LoadDisplayPrefs(w2.ctx, "America/Chicago", 30))
	if d.Zone() != "UTC" || d.RefreshS() != 90 || len(d.ConfigNotes()) != 2 {
		t.Fatalf("after a save: zone %q refresh %d notes %v", d.Zone(), d.RefreshS(), d.ConfigNotes())
	}
	// Saving it again is a no-op.
	n := auditCount(t, w2, "settings.update")
	ch = must[[]SettingChange](t)(w2.s.SetSettingsAudited(w2.ctx, ScopeAll(), []SettingInput{{SettingDisplayTimezone, json.RawMessage(`"UTC"`)}}, actor))
	if !ch[0].Unchanged || auditCount(t, w2, "settings.update") != n {
		t.Fatalf("second save: %+v", ch)
	}
}

// A database initialised before the marker existed (PutSetting by init, no marker) keeps its zone.
func TestLegacyInitialisedDatabaseKeepsItsStoredZone(t *testing.T) {
	w := seed(t)
	ok(t, w.s.PutSetting(w.ctx, SettingDisplayTimezone, json.RawMessage(`"America/Denver"`), ""))
	d := must[*DisplayPrefs](t)(w.s.LoadDisplayPrefs(w.ctx, "America/Chicago", 30))
	if d.Zone() != "America/Denver" {
		t.Fatalf("legacy stored zone lost: %q", d.Zone())
	}
}

func TestDisplayPrefsFallBackAndApply(t *testing.T) {
	w := seed(t)
	// An operator-chosen value that does not resolve falls back and says so.
	ok(t, w.s.PutOperatorSetting(w.ctx, SettingDisplayTimezone, json.RawMessage(`"Not/AZone"`), ""))
	ok(t, w.s.PutOperatorSetting(w.ctx, SettingRefreshInterval, json.RawMessage(`2`), ""))
	d := must[*DisplayPrefs](t)(w.s.LoadDisplayPrefs(w.ctx, "America/Denver", 45))
	if d.Zone() != "America/Denver" || d.RefreshS() != 45 || !strings.Contains(d.Note(), "not a valid IANA zone") || !strings.Contains(d.Note(), "out of range") {
		t.Fatalf("invalid stored values: zone %q refresh %d note %q", d.Zone(), d.RefreshS(), d.Note())
	}
	// Apply replaces the zone for the next reader and clears the note.
	d.Apply([]SettingChange{{Key: SettingDisplayTimezone, New: json.RawMessage(`"Asia/Tokyo"`)}, {Key: SettingRefreshInterval, New: json.RawMessage(`90`)}})
	snap := d.Snapshot()
	if snap.Zone != "Asia/Tokyo" || snap.Loc.String() != "Asia/Tokyo" || snap.RefreshS != 90 || d.Note() != "" {
		t.Fatalf("after Apply: %+v note %q", snap, d.Note())
	}
	var nilPrefs *DisplayPrefs
	nilPrefs.Apply([]SettingChange{{Key: SettingRefreshInterval, New: json.RawMessage(`9`)}})
	bad := NewDisplayPrefs("Local", 1)
	if bad.Zone() != "UTC" || bad.RefreshS() != 30 {
		t.Fatalf("bad input: %q %d", bad.Zone(), bad.RefreshS())
	}
}

// Two owners saving different keys at once must both land: Apply computes from the
// current values under one lock instead of from a stale read.
func TestDisplayPrefsApplyDoesNotLoseAConcurrentUpdate(t *testing.T) {
	for round := 0; round < 300; round++ {
		d := NewDisplayPrefs("UTC", 30)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			d.Apply([]SettingChange{{Key: SettingDisplayTimezone, New: json.RawMessage(`"Asia/Tokyo"`)}})
		}()
		go func() {
			defer wg.Done()
			d.Apply([]SettingChange{{Key: SettingRefreshInterval, New: json.RawMessage(`90`)}})
		}()
		wg.Wait()
		if snap := d.Snapshot(); snap.Zone != "Asia/Tokyo" || snap.RefreshS != 90 {
			t.Fatalf("round %d: lost an update: %+v", round, snap)
		}
	}
}

func TestDisplayPrefsAreSafeForConcurrentUse(t *testing.T) {
	d := NewDisplayPrefs("UTC", 30)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				d.Apply([]SettingChange{{Key: SettingDisplayTimezone, New: json.RawMessage(`"America/Chicago"`)}})
				d.Apply([]SettingChange{{Key: SettingDisplayTimezone, New: json.RawMessage(`"Asia/Tokyo"`)}})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 400; j++ {
				if snap := d.Snapshot(); snap.Loc == nil || snap.Loc.String() != snap.Zone {
					t.Error("torn read: the zone name and the zone differ")
					return
				}
			}
		}()
	}
	wg.Wait()
}
