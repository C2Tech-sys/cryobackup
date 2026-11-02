"""DDL test for schema.sql.

Run:  python -I replica/schema_test.py [path-to-schema.sql]   (ships next to schema.sql; 259 DDL assertions)

1. executescript() the whole file into :memory: (clean execution).
2. Insert one valid row into every table.
3. Constraint tests, two helpers:
     fire()   baseline row must insert, the same row with ONE violation must be rejected with the expected error class
     dup()    a row that collides with an existing row must be rejected; after a "release" statement (revoke / clear)
              the very same row must be accepted (positive control for partial unique indexes)
4. Structural checks: every FK column is the leading column of an index (or PK/UNIQUE), every table has created_at,
   every mutable table has updated_at, no table lacks a primary key.
Exit code 0 = all passed.
"""
import itertools
import sqlite3
import sys
from pathlib import Path

SCHEMA = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).resolve().parent / "schema.sql"
NOW = 1760000000000
counter = itertools.count(1)
BCRYPT = "$2y$10$" + "a" * 53
CHK, UNQ, FK = "CHECK constraint failed", "UNIQUE constraint failed", "FOREIGN KEY constraint failed"


def n():
    return next(counter)


def ulid():
    return "01TEST%020d" % n()


def hex64():
    return "%064x" % n()


conn = sqlite3.connect(":memory:", isolation_level=None)
conn.execute("PRAGMA foreign_keys = ON")
conn.executescript(SCHEMA.read_text(encoding="utf-8"))
conn.execute("PRAGMA foreign_keys = ON")
assert conn.execute("PRAGMA foreign_keys").fetchone()[0] == 1

results = []


def record(label, ok, detail=""):
    results.append((label, ok, detail))


def insert(table, row):
    cols = ", ".join(row.keys())
    ph = ", ".join("?" for _ in row)
    conn.execute(f"INSERT INTO {table} ({cols}) VALUES ({ph})", list(row.values()))


def _sp(fn):
    conn.execute("SAVEPOINT t")
    try:
        return fn()
    finally:
        conn.execute("ROLLBACK TO t")
        conn.execute("RELEASE t")


def fire(label, table, make, expect, **override):
    def base():
        try:
            insert(table, make())
            return None
        except sqlite3.Error as e:
            return str(e)
    err = _sp(base)
    if err:
        return record(label, False, f"BASELINE rejected: {err}")

    def bad():
        try:
            insert(table, make(**override))
            return None
        except sqlite3.Error as e:
            return str(e)
    err = _sp(bad)
    if err is None:
        return record(label, False, "violating row was ACCEPTED")
    record(label, expect.lower() in err.lower(), err[:110])


def dup(label, table, row, expect, release=None):
    def bad():
        try:
            insert(table, row)
            return None
        except sqlite3.Error as e:
            return str(e)
    err = _sp(bad)
    if err is None:
        return record(label, False, "colliding row was ACCEPTED")
    if expect.lower() not in err.lower():
        return record(label, False, err[:110])
    if release:
        def ctrl():
            try:
                for s, p in release:
                    conn.execute(s, p)
                insert(table, row)
                return None
            except sqlite3.Error as e:
                return str(e)
        err2 = _sp(ctrl)
        if err2:
            return record(label, False, f"control after release rejected: {err2}")
    record(label, True, err[:110] + (" | control ok" if release else ""))


def accept(label, table, row):
    def go():
        try:
            insert(table, row)
            return None
        except sqlite3.Error as e:
            return str(e)
    err = _sp(go)
    record(label, err is None, "accepted" if err is None else f"unexpectedly rejected: {err}")


def sql_fails(label, stmts, sql, params, expect):
    def go():
        try:
            for s, p in stmts:
                conn.execute(s, p)
        except sqlite3.Error as e:
            return "SETUP: " + str(e)
        try:
            conn.execute(sql, params)
            return None
        except sqlite3.Error as e:
            return str(e)
    err = _sp(go)
    if err is None:
        return record(label, False, "statement was ACCEPTED")
    record(label, expect.lower() in err.lower() and not err.startswith("SETUP"), err[:110])


def sql_ok(label, stmts, check_sql, check_params, expected):
    def go():
        try:
            for s, p in stmts:
                conn.execute(s, p)
            return conn.execute(check_sql, check_params).fetchone()[0]
        except sqlite3.Error as e:
            return "ERR " + str(e)
    got = _sp(go)
    record(label, got == expected, f"got {got!r} expected {expected!r}")


# ------------------------------------------------------------------ factories (valid + unique)
def f_admin(**o):
    k = n()
    r = dict(id=ulid(), username=f"user{k}", email=f"user{k}@example.test", role="admin",
             password_hash="$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA", created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_storage(**o):
    k = n()
    r = dict(id=ulid(), name=f"target{k}", provider="wasabi", endpoint="s3.example.test", bucket=f"bucket{k}",
             gateway_url="https://gw.example.test:9443", credential_ref="STORAGE_CRED_REF", created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_policy(**o):
    r = dict(id=ulid(), scope="template", name=f"policy{n()}", created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_customer(sid, **o):
    k = n()
    r = dict(id=ulid(), name=f"Customer {k}", slug=f"cust-{k}", storage_target_id=sid, created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_device(cid, **o):
    d = ulid()
    r = dict(id=d, customer_id=cid, hostname=f"host{n()}", enrolled_at=NOW, enrolled_via="token",
             repo_path=f"prefix/{d}/", created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_etok(cid, **o):
    r = dict(id=ulid(), customer_id=cid, token_hash=hex64(), expires_at=NOW + 86400000, created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_dtok(did, **o):
    r = dict(id=ulid(), device_id=did, token_hash=hex64(), created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_presence(did, **o):
    r = dict(device_id=did, received_at=NOW, service_state="idle", created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_hb(did, **o):
    r = dict(id=ulid(), device_id=did, received_at=NOW, service_state="idle", created_at=NOW)
    r.update(o); return r


def f_gw(did, **o):
    r = dict(id=ulid(), device_id=did, username=did, password_hash=BCRYPT, created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_escrow(did, **o):
    r = dict(id=ulid(), device_id=did, key_version=n() + 10, custody="escrowed", wrapped_key=b"\x01" * 48,
             wrap_alg="aes-256-gcm+hkdf-sha256/v1", master_key_id="mk1", created_at=NOW, updated_at=NOW, retired_at=NOW)
    r.update(o); return r


def f_ovr(did, **o):
    r = dict(id=ulid(), device_id=did, patch='{"schedule":{"interval":"4h"}}', reason="lab", created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_run(did, **o):
    r = dict(id=ulid(), device_id=did, kind="backup", trigger_kind="schedule", started_at=NOW, finished_at=NOW + 1000,
             received_at=NOW + 2000, outcome="success", created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_snap(did, **o):
    r = dict(device_id=did, snapshot_id=hex64(), taken_at=NOW, created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_usage(did, **o):
    r = dict(device_id=did, sampled_at=NOW + n(), repo_bytes=1024, source="restic_stats", created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_cmd(did, **o):
    r = dict(id=ulid(), device_id=did, type="backup_now", idempotency_key=f"k{n()}", expires_at=NOW + 60000,
             created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_rest(did, **o):
    r = dict(id=ulid(), device_id=did, snapshot_id=hex64(), mode="download_zip", expires_at=NOW + 3600000,
             created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_ack(did, **o):
    r = dict(id=ulid(), subject_type="device", device_id=did, alarm_type="stale", acknowledged_at=NOW,
             created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_rule(**o):
    r = dict(id=ulid(), scope="global", name=f"rule{n()}", channel="email", destination="ops@example.test",
             created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_nlog(**o):
    r = dict(id=ulid(), dedupe_key=f"dk{n()}", kind="alert", subject_type="device", subject_id="x", channel="email",
             result="sent", sent_at=NOW, created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_view(**o):
    r = dict(id=ulid(), name=f"view{n()}", entity="devices", created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_report(**o):
    r = dict(id=ulid(), name=f"report{n()}", scope="global", kind="customer_status", format="html", created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_audit(**o):
    r = dict(id=ulid(), occurred_at=NOW, actor_type="admin", actor_id="no-such-admin", method="web", action="test.action",
             result="ok", created_at=NOW)
    r.update(o); return r


def f_rel(**o):
    r = dict(id=ulid(), version=f"1.0.{n()}", channel="pilot", sha256=hex64(), signature="sig", restic_version="0.19.1",
             restic_sha256=hex64(), min_os="10.0.17763", manifest="{}", artifact_ref="agent.msi", released_at=NOW,
             created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_job(**o):
    r = dict(name=f"job{n()}", created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_sess(aid, **o):
    r = dict(id=ulid(), admin_id=aid, token_hash=hex64(), last_seen_at=NOW, expires_at=NOW + 1000,
             absolute_expires_at=NOW + 2000, created_at=NOW, updated_at=NOW)
    r.update(o); return r


def f_apitok(aid, **o):
    r = dict(id=ulid(), admin_id=aid, name=f"t{n()}", scopes="[]", token_prefix="cbk_aaaa", token_hash=hex64(),
             created_at=NOW, updated_at=NOW)
    r.update(o); return r


# ------------------------------------------------------------------ 1. one valid row in every table
insert("schema_migrations", dict(version=2, name="0002_test", checksum="x", created_at=NOW))
insert("settings", dict(key="test.key", value='{"a":1}', created_at=NOW, updated_at=NOW))
admin = f_admin(); insert("admin_users", admin); A = admin["id"]
insert("admin_recovery_codes", dict(id=ulid(), admin_id=A, code_hash="rc-hash-1", created_at=NOW, updated_at=NOW))
insert("admin_webauthn_credentials", dict(id=ulid(), admin_id=A, credential_id=b"cred1", public_key=b"pk", name="key", created_at=NOW, updated_at=NOW))
insert("sessions", f_sess(A))
insert("api_tokens", f_apitok(A, name="ci"))
storage = f_storage(); insert("storage_targets", storage); S = storage["id"]
dpol = f_policy(scope="default", name="Default"); insert("policies", dpol)
insert("policy_revisions", dict(id=ulid(), policy_id=dpol["id"], version=1, body="{}", created_at=NOW))
cust = f_customer(S, policy_id=dpol["id"]); insert("customers", cust); C = cust["id"]
cpol = f_policy(scope="customer", customer_id=C, name="Cust policy"); insert("policies", cpol)
etok = f_etok(C); insert("enrollment_tokens", etok)
dev = f_device(C, enrollment_token_id=etok["id"]); insert("devices", dev); D = dev["id"]
dtok = f_dtok(D); insert("device_tokens", dtok)
insert("device_presence", f_presence(D))
insert("heartbeats", f_hb(D))
insert("enrollment_requests", dict(id=ulid(), request_nonce="nonce1", hostname="h", expires_at=NOW + 1000, created_at=NOW, updated_at=NOW))
gw = f_gw(D); insert("gateway_logins", gw)
esc = f_escrow(D, key_version=1, retired_at=None); insert("repo_key_escrow", esc)
ovr = f_ovr(D); insert("device_policy_overrides", ovr)
insert("runs", f_run(D, snapshot_id=hex64()))
insert("snapshots", f_snap(D))
insert("usage_samples", f_usage(D))
insert("maintenance_schedule", dict(device_id=D, created_at=NOW, updated_at=NOW))
cmd = f_cmd(D, idempotency_key="idem-1"); insert("commands", cmd)
insert("restore_requests", f_rest(D))
ack = f_ack(D); insert("alarm_acks", ack)
rule = f_rule(); insert("notification_rules", rule)
insert("notification_log", f_nlog(rule_id=rule["id"]))
view = f_view(owner_id=A); insert("saved_views", view)
insert("reports", f_report(saved_view_id=view["id"]))
insert("audit_events", f_audit())
insert("agent_releases", f_rel())
insert("job_state", f_job())
insert("start_leases", dict(run_id=ulid(), device_id=D, storage_target_id=S, granted_at=NOW, lease_until=NOW + 900000, created_at=NOW, updated_at=NOW))

TABLES = [r[0] for r in conn.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")]
empty = [t for t in TABLES if conn.execute(f"SELECT count(*) FROM {t}").fetchone()[0] == 0]
record("valid row inserted into every table (%d tables)" % len(TABLES), not empty, f"empty: {empty}")

# ------------------------------------------------------------------ 2. constraint tests
# schema_migrations / settings
fire("schema_migrations: version > 0", "schema_migrations", lambda **k: dict(version=k.get("version", n() + 100), name="x", checksum="x", created_at=NOW), CHK, version=0)
dup("schema_migrations: unique version", "schema_migrations", dict(version=1, name="x", checksum="x", created_at=NOW), UNQ)
fire("settings: json_valid(value)", "settings", lambda **k: dict(key=f"k{n()}", value=k.get("value", "1"), created_at=NOW, updated_at=NOW), CHK, value="{not json")
fire("settings: non-empty key", "settings", lambda **k: dict(key=k.get("key", f"k{n()}"), value="1", created_at=NOW, updated_at=NOW), CHK, key="")
# storage_targets
fire("storage_targets: provider enum", "storage_targets", f_storage, CHK, provider="dropbox")
fire("storage_targets: object_lock_mode excludes compliance", "storage_targets", f_storage, CHK, object_lock_mode="compliance")
fire("storage_targets: governance requires lock days", "storage_targets", f_storage, CHK, object_lock_mode="governance")
fire("storage_targets: lock days require governance", "storage_targets", f_storage, CHK, object_lock_days=7)
accept("storage_targets: governance with days accepted", "storage_targets", f_storage(object_lock_mode="governance", object_lock_days=30))
fire("storage_targets: soft_quota_bytes > 0", "storage_targets", f_storage, CHK, soft_quota_bytes=0)
fire("storage_targets: prune cadence range", "storage_targets", f_storage, CHK, default_prune_cadence_days=0)
dup("storage_targets: unique name (nocase)", "storage_targets", f_storage(name=storage["name"].upper()), UNQ)
dup("storage_targets: unique (endpoint, bucket, prefix)", "storage_targets", f_storage(bucket=storage["bucket"]), UNQ)
conn.execute("UPDATE storage_targets SET is_default = 1 WHERE id = ?", (S,))
dup("storage_targets: only one default target", "storage_targets", f_storage(is_default=1), UNQ,
    release=[("UPDATE storage_targets SET is_default = 0 WHERE id = ?", (S,))])
# admin_users
fire("admin_users: role enum", "admin_users", f_admin, CHK, role="superuser")
fire("admin_users: owner cannot be customer-scoped", "admin_users", f_admin, CHK, role="owner", customer_scope='["x"]')
accept("admin_users: operator may be customer-scoped", "admin_users", f_admin(role="operator", customer_scope='["x"]'))
fire("admin_users: json_valid(customer_scope)", "admin_users", lambda **k: f_admin(role="operator", **k), CHK, customer_scope="[oops")
fire("admin_users: password or oidc subject required", "admin_users", f_admin, CHK, password_hash=None)
fire("admin_users: totp secret requires totp_enrolled_at", "admin_users", f_admin, CHK, totp_secret_wrapped=b"x")
fire("admin_users: failed_login_count >= 0", "admin_users", f_admin, CHK, failed_login_count=-1)
dup("admin_users: unique username (nocase)", "admin_users", f_admin(username=admin["username"].upper()), UNQ)
insert("admin_users", f_admin(oidc_subject="dup-sub"))
dup("admin_users: unique oidc_subject", "admin_users", f_admin(oidc_subject="dup-sub"), UNQ)
dup("admin_recovery_codes: unique code_hash", "admin_recovery_codes", dict(id=ulid(), admin_id=A, code_hash="rc-hash-1", created_at=NOW, updated_at=NOW), UNQ)
dup("admin_webauthn_credentials: unique credential_id", "admin_webauthn_credentials",
    dict(id=ulid(), admin_id=A, credential_id=b"cred1", public_key=b"pk", name="k", created_at=NOW, updated_at=NOW), UNQ)
fire("admin_webauthn_credentials: json_valid(transports)", "admin_webauthn_credentials",
     lambda **k: dict(id=ulid(), admin_id=A, credential_id=b"c%d" % n(), public_key=b"pk", name="k", created_at=NOW, updated_at=NOW, **k), CHK, transports="[")
# sessions
fire("sessions: token_hash is lowercase hex", "sessions", lambda **k: f_sess(A, **k), CHK, token_hash="A" * 64)
fire("sessions: token_hash is 64 chars", "sessions", lambda **k: f_sess(A, **k), CHK, token_hash="ab" * 10)
fire("sessions: absolute expiry >= idle expiry", "sessions", lambda **k: f_sess(A, **k), CHK, absolute_expires_at=NOW)
fire("sessions: FK admin", "sessions", lambda **k: f_sess(A, **k), FK, admin_id="01NOSUCHADMIN0000000000000")
dup("sessions: unique token_hash", "sessions", f_sess(A, token_hash=conn.execute("SELECT token_hash FROM sessions").fetchone()[0]), UNQ)
# api_tokens
fire("api_tokens: json_valid(scopes)", "api_tokens", lambda **k: f_apitok(A, **k), CHK, scopes="nope")
dup("api_tokens: one active token name per admin", "api_tokens", f_apitok(A, name="ci"), UNQ,
    release=[("UPDATE api_tokens SET revoked_at = 1 WHERE name = 'ci'", ())])
dup("api_tokens: unique token_hash", "api_tokens", f_apitok(A, token_hash=conn.execute("SELECT token_hash FROM api_tokens").fetchone()[0]), UNQ)
# policies
dup("policies: exactly one default policy", "policies", f_policy(scope="default", name="Another default"), UNQ)
fire("policies: customer scope requires customer_id", "policies", f_policy, CHK, scope="customer")
fire("policies: customer_id requires customer scope", "policies", f_policy, CHK, customer_id=C)
fire("policies: scope enum", "policies", f_policy, CHK, scope="global")
fire("policies: json_valid(schedule)", "policies", f_policy, CHK, schedule="{bad")
fire("policies: json_valid(retention)", "policies", f_policy, CHK, retention="")
fire("policies: template_device_class only on templates", "policies", lambda **k: f_policy(scope="customer", customer_id=C, **k), CHK, template_device_class="laptop")
fire("policies: template_device_class enum", "policies", f_policy, CHK, template_device_class="server")
fire("policies: version >= 1", "policies", f_policy, CHK, version=0)
insert("policies", f_policy(name="Laptop template", template_device_class="laptop"))
dup("policies: unique shared name (nocase)", "policies", f_policy(name="LAPTOP TEMPLATE"), UNQ)
dup("policies: unique name per customer", "policies", f_policy(scope="customer", customer_id=C, name="cust policy"), UNQ)
dup("policy_revisions: unique (policy, version)", "policy_revisions", dict(id=ulid(), policy_id=dpol["id"], version=1, body="{}", created_at=NOW), UNQ)
fire("policy_revisions: json_valid(body)", "policy_revisions", lambda **k: dict(id=ulid(), policy_id=dpol["id"], version=n() + 10, body=k.get("body", "{}"), created_at=NOW), CHK, body="x")
# customers
mk = lambda **k: f_customer(S, **k)
fire("customers: state enum", "customers", mk, CHK, state="deleted")
fire("customers: expected customer cannot be archived", "customers", mk, CHK, expected=1, state="archived", archived_at=NOW)
accept("customers: non-expected customer may be archived", "customers", mk(state="archived", archived_at=NOW))
fire("customers: archived state requires archived_at", "customers", mk, CHK, state="archived")
fire("customers: archived_at requires archived state", "customers", mk, CHK, archived_at=NOW)
fire("customers: slug charset", "customers", mk, CHK, slug="Bad Slug")
fire("customers: expected is boolean", "customers", mk, CHK, expected=2)
fire("customers: device_cap > 0", "customers", mk, CHK, device_cap=0)
fire("customers: key_custody enum", "customers", mk, CHK, key_custody="vendor")
fire("customers: json_valid(contacts)", "customers", mk, CHK, contacts="{")
dup("customers: unique slug", "customers", mk(slug=cust["slug"]), UNQ)
dup("customers: unique name (nocase)", "customers", mk(name=cust["name"].lower()), UNQ)
fire("customers: FK storage_target", "customers", mk, FK, storage_target_id="01NOSUCHTARGET00000000000")
fire("customers: FK policy", "customers", mk, FK, policy_id="01NOSUCHPOLICY0000000000")
sql_fails("customers: deleting an assigned storage target is RESTRICTed", [], "DELETE FROM storage_targets WHERE id = ?", (S,), FK)
sql_fails("customers: deleting an assigned policy is RESTRICTed", [], "DELETE FROM policies WHERE id = ?", (dpol["id"],), FK)
sql_fails("devices->customers: deleting a customer with devices is RESTRICTed", [], "DELETE FROM customers WHERE id = ?", (C,), FK)
# enrollment_tokens
dup("enrollment_tokens: unique token_hash", "enrollment_tokens", f_etok(C, token_hash=etok["token_hash"]), UNQ)
fire("enrollment_tokens: used_count <= max_uses", "enrollment_tokens", lambda **k: f_etok(C, **k), CHK, max_uses=1, used_count=2)
fire("enrollment_tokens: adoption token must be single-use", "enrollment_tokens", lambda **k: f_etok(C, **k), CHK, adopts_device_id=D)
accept("enrollment_tokens: adoption token with max_uses=1", "enrollment_tokens", f_etok(C, adopts_device_id=D, max_uses=1))
fire("enrollment_tokens: max_uses >= 1", "enrollment_tokens", lambda **k: f_etok(C, **k), CHK, max_uses=0)
fire("enrollment_tokens: token_hash is 64 hex chars", "enrollment_tokens", lambda **k: f_etok(C, **k), CHK, token_hash="not-a-hash")
fire("enrollment_tokens: FK customer", "enrollment_tokens", lambda **k: f_etok(C, **k), FK, customer_id="01NOSUCHCUSTOMER000000000")
# devices
mk = lambda **k: f_device(C, **k)
fire("devices: not both retired and paused", "devices", mk, CHK, retired_at=NOW, paused_at=NOW, pause_reason="x")
fire("devices: pause requires a reason", "devices", mk, CHK, paused_at=NOW)
fire("devices: empty pause reason rejected", "devices", mk, CHK, paused_at=NOW, pause_reason="")
fire("devices: paused_until requires paused_at", "devices", mk, CHK, paused_until=NOW + 1000)
fire("devices: paused_until after paused_at", "devices", mk, CHK, paused_at=NOW, pause_reason="x", paused_until=NOW)
accept("devices: indefinite pause (paused_until NULL) accepted", "devices", mk(paused_at=NOW, pause_reason="maintenance"))
accept("devices: timed pause accepted", "devices", mk(paused_at=NOW, pause_reason="maintenance", paused_until=NOW + 3600000))
fire("devices: replaced_by requires retired", "devices", mk, CHK, replaced_by_device_id=D)
accept("devices: retired + replaced_by accepted", "devices", mk(replaced_by_device_id=D, retired_at=NOW))
_selfid = ulid()
dup("devices: cannot be replaced by itself", "devices", f_device(C, id=_selfid, repo_path=f"p/{_selfid}/", replaced_by_device_id=_selfid, retired_at=NOW), "constraint failed")
dup("devices: unique repo_path", "devices", mk(repo_path=dev["repo_path"]), UNQ)
insert("devices", f_device(C, enroll_nonce="dupnonce"))
dup("devices: unique enroll_nonce", "devices", mk(enroll_nonce="dupnonce"), UNQ)
fire("devices: enrolled_at must be epoch milliseconds", "devices", mk, CHK, enrolled_at=1760000000)
fire("devices: enrolled_via enum", "devices", mk, CHK, enrolled_via="magic")
fire("devices: source_kind enum", "devices", mk, CHK, source_kind="nas")
fire("devices: device_class enum", "devices", mk, CHK, device_class="tablet")
fire("devices: update_ring enum", "devices", mk, CHK, update_ring="canary")
fire("devices: arch enum", "devices", mk, CHK, arch="mips")
fire("devices: FK customer", "devices", mk, FK, customer_id="01NOSUCHCUSTOMER000000000")
# device_tokens
dup("device_tokens: unique token_hash", "device_tokens", f_dtok(D, token_hash=dtok["token_hash"]), UNQ)
dup("device_tokens: one current token per device", "device_tokens", f_dtok(D), UNQ,
    release=[("UPDATE device_tokens SET superseded_at = 1, grace_until = 2 WHERE id = ?", (dtok["id"],))])
fire("device_tokens: superseded_at and grace_until come together", "device_tokens", lambda **k: f_dtok(D, revoked_at=NOW, **k), CHK, superseded_at=NOW)
fire("device_tokens: token_hash format", "device_tokens", lambda **k: f_dtok(D, revoked_at=NOW, **k), CHK, token_hash="xyz")
# presence / heartbeats




d2 = f_device(C); insert("devices", d2); D2 = d2["id"]
fire("device_presence: service_state enum", "device_presence", lambda **k: f_presence(D2, **k), CHK, service_state="sleeping")
fire("device_presence: battery_pct 0..100", "device_presence", lambda **k: f_presence(D2, **k), CHK, battery_pct=101)
fire("device_presence: received_at must be epoch milliseconds", "device_presence", lambda **k: f_presence(D2, **k), CHK, received_at=1760000000)
fire("device_presence: on_ac boolean", "device_presence", lambda **k: f_presence(D2, **k), CHK, on_ac=2)
fire("device_presence: json_valid(progress)", "device_presence", lambda **k: f_presence(D2, **k), CHK, progress="{")
dup("device_presence: one row per device", "device_presence", f_presence(D), "UNIQUE")
fire("heartbeats: service_state enum", "heartbeats", lambda **k: f_hb(D, **k), CHK, service_state="x")
fire("heartbeats: received_at must be epoch milliseconds", "heartbeats", lambda **k: f_hb(D, **k), CHK, received_at=5)
fire("heartbeats: FK device", "heartbeats", lambda **k: f_hb(D, **k), FK, device_id="01NOSUCHDEVICE00000000000")
# enrollment_requests
mk = lambda **k: dict(id=ulid(), request_nonce=k.pop("request_nonce", f"n{n()}"), hostname="h", expires_at=NOW + 1000, created_at=NOW, updated_at=NOW, **k)
dup("enrollment_requests: unique request_nonce", "enrollment_requests", mk(request_nonce="nonce1"), UNQ)
fire("enrollment_requests: status enum", "enrollment_requests", mk, CHK, status="maybe")
fire("enrollment_requests: approval requires customer and decision time", "enrollment_requests", mk, CHK, status="approved")
fire("enrollment_requests: rejection requires decision time", "enrollment_requests", mk, CHK, status="rejected")
accept("enrollment_requests: complete approval accepted", "enrollment_requests", mk(status="approved", customer_id=C, decided_at=NOW))
fire("enrollment_requests: json_valid(facts)", "enrollment_requests", mk, CHK, facts="{")
# gateway_logins
d3 = f_device(C); insert("devices", d3); D3 = d3["id"]
fire("gateway_logins: password_hash must be bcrypt", "gateway_logins", lambda **k: f_gw(D3, **k), CHK, password_hash="x" * 60)
fire("gateway_logins: password_hash must be 60 chars", "gateway_logins", lambda **k: f_gw(D3, **k), CHK, password_hash="$2y$10$short")
fire("gateway_logins: username is a 26-char device id", "gateway_logins", lambda **k: f_gw(D3, **k), CHK, username="short")
fire("gateway_logins: pending secret cleared once delivered", "gateway_logins", lambda **k: f_gw(D3, **k), CHK, pending_secret_wrapped=b"x", delivered_at=NOW)
dup("gateway_logins: one active login per device", "gateway_logins", f_gw(D, username=ulid()), UNQ,
    release=[("UPDATE gateway_logins SET revoked_at = 1 WHERE id = ?", (gw["id"],))])
dup("gateway_logins: one active username", "gateway_logins", f_gw(D3, username=gw["username"]), UNQ,
    release=[("UPDATE gateway_logins SET revoked_at = 1 WHERE id = ?", (gw["id"],))])
fire("gateway_logins: FK device", "gateway_logins", lambda **k: f_gw(D3, **k), FK, device_id="01NOSUCHDEVICE00000000000")
# repo_key_escrow
fire("repo_key_escrow: escrowed key needs wrapped_key", "repo_key_escrow", lambda **k: f_escrow(D3, **k), CHK, wrapped_key=None)
fire("repo_key_escrow: escrowed key needs master_key_id", "repo_key_escrow", lambda **k: f_escrow(D3, **k), CHK, master_key_id=None)
fire("repo_key_escrow: customer_held must not store a key", "repo_key_escrow", lambda **k: f_escrow(D3, **k), CHK, custody="customer_held")
accept("repo_key_escrow: customer_held without key accepted", "repo_key_escrow", f_escrow(D3, custody="customer_held", wrapped_key=None, wrap_alg=None, master_key_id=None))
fire("repo_key_escrow: custody enum", "repo_key_escrow", lambda **k: f_escrow(D3, **k), CHK, custody="vendor")
fire("repo_key_escrow: wrap_alg enum", "repo_key_escrow", lambda **k: f_escrow(D3, **k), CHK, wrap_alg="rot13")
fire("repo_key_escrow: key_version >= 1", "repo_key_escrow", lambda **k: f_escrow(D3, **k), CHK, key_version=0)
dup("repo_key_escrow: unique (device, key_version)", "repo_key_escrow", f_escrow(D, key_version=1, retired_at=NOW), UNQ)
dup("repo_key_escrow: one active key per device", "repo_key_escrow", f_escrow(D, key_version=2, retired_at=None), UNQ,
    release=[("UPDATE repo_key_escrow SET retired_at = 1 WHERE id = ?", (esc["id"],))])
# device_policy_overrides
dup("device_policy_overrides: one active override per device", "device_policy_overrides", f_ovr(D), UNQ,
    release=[("UPDATE device_policy_overrides SET cleared_at = 1 WHERE id = ?", (ovr["id"],))])
fire("device_policy_overrides: reason required", "device_policy_overrides", lambda **k: f_ovr(D3, **k), CHK, reason="")
fire("device_policy_overrides: json_valid(patch)", "device_policy_overrides", lambda **k: f_ovr(D3, **k), CHK, patch="{")
# runs
mk = lambda **k: f_run(D, **k)
first_run = conn.execute("SELECT id FROM runs").fetchone()[0]
dup("runs: unique run id (idempotency key)", "runs", mk(id=first_run), UNQ)
fire("runs: outcome enum", "runs", mk, CHK, outcome="great")
for oc in ("success", "partial", "failed", "cancelled", "skipped", "interrupted", "abandoned"):
    accept(f"runs: outcome '{oc}' accepted", "runs", mk(outcome=oc))
accept("runs: outcome 'deferred' with reason accepted", "runs", mk(outcome="deferred", deferred_reason="lock"))
fire("runs: kind enum", "runs", mk, CHK, kind="restore2")
for kd in ("backup", "restore", "forget", "prune", "check", "unlock", "maintenance"):
    accept(f"runs: kind '{kd}' accepted", "runs", mk(kind=kd, snapshot_id=None))
fire("runs: trigger enum", "runs", mk, CHK, trigger_kind="cron")
for tg in ("schedule", "catchup", "wake", "boot", "manual", "retry"):
    accept(f"runs: trigger '{tg}' accepted", "runs", mk(trigger_kind=tg))
fire("runs: origin enum", "runs", mk, CHK, origin="cloud")
fire("runs: finished_at >= started_at", "runs", mk, CHK, finished_at=NOW - 1)
fire("runs: outcome requires finished_at", "runs", mk, CHK, finished_at=None)
fire("runs: finished_at requires outcome", "runs", mk, CHK, outcome=None)
accept("runs: open run (no outcome, no finished_at) accepted", "runs", mk(outcome=None, finished_at=None))
fire("runs: deferred requires deferred_reason", "runs", mk, CHK, outcome="deferred")
fire("runs: deferred_reason only on deferred", "runs", mk, CHK, deferred_reason="lock")
fire("runs: deferred_reason enum", "runs", mk, CHK, outcome="deferred", deferred_reason="moon")
accept("runs: deferred_reason fleet_cap accepted (0003)", "runs", mk(outcome="deferred", deferred_reason="fleet_cap"))

# start_leases (0003) and the storage target cap
def f_lease(**o):
    r = dict(run_id=ulid(), device_id=D, storage_target_id=S, granted_at=NOW, lease_until=NOW + 900000, created_at=NOW, updated_at=NOW)
    r.update(o); return r
fire("start_leases: run_id is a ULID", "start_leases", f_lease, CHK, run_id="short")
fire("start_leases: lease_until > granted_at", "start_leases", f_lease, CHK, lease_until=NOW)
fire("start_leases: granted_at in ms", "start_leases", f_lease, CHK, granted_at=1760000000)
fire("start_leases: device FK", "start_leases", f_lease, FK, device_id="01NOSUCHDEVICE000000000000")
fire("start_leases: storage target FK", "start_leases", f_lease, FK, storage_target_id="01NOSUCHTARGET000000000000")
fire("storage_targets: max_concurrent_backups >= 0", "storage_targets", f_storage, CHK, max_concurrent_backups=-1)
fire("runs: snapshot_id only on backup runs", "runs", mk, CHK, kind="prune", snapshot_id=hex64())
fire("runs: snapshot_id is 64 hex", "runs", mk, CHK, snapshot_id="abc")
fire("runs: started_at must be epoch milliseconds", "runs", mk, CHK, started_at=1760000000, finished_at=None, outcome=None)
fire("runs: error_class enum", "runs", mk, CHK, error_class="mystery")
fire("runs: attempt >= 1", "runs", mk, CHK, attempt=0)
fire("runs: json_valid(error_counts)", "runs", mk, CHK, error_counts="x")
fire("runs: FK device", "runs", mk, FK, device_id="01NOSUCHDEVICE00000000000")
# snapshots
dup("snapshots: unique (device_id, snapshot_id)", "snapshots", f_snap(D, snapshot_id=conn.execute("SELECT snapshot_id FROM snapshots").fetchone()[0]), UNQ)
accept("snapshots: same snapshot_id under another device accepted", "snapshots", f_snap(D3, snapshot_id=conn.execute("SELECT snapshot_id FROM snapshots").fetchone()[0]))
fire("snapshots: snapshot_id is 64 hex", "snapshots", lambda **k: f_snap(D3, **k), CHK, snapshot_id="xyz")
fire("snapshots: json_valid(paths)", "snapshots", lambda **k: f_snap(D3, **k), CHK, paths="[")
fire("snapshots: FK device", "snapshots", lambda **k: f_snap(D3, **k), FK, device_id="01NOSUCHDEVICE00000000000")
# usage_samples
us = conn.execute("SELECT sampled_at FROM usage_samples").fetchone()[0]
dup("usage_samples: unique (device, sampled_at)", "usage_samples", f_usage(D, sampled_at=us), UNQ)
fire("usage_samples: repo_bytes >= 0", "usage_samples", lambda **k: f_usage(D3, **k), CHK, repo_bytes=-1)
fire("usage_samples: source enum", "usage_samples", lambda **k: f_usage(D3, **k), CHK, source="guess")
# maintenance_schedule
fire("maintenance_schedule: lease holder and lease_until together", "maintenance_schedule", lambda **k: dict(device_id=D3, created_at=NOW, updated_at=NOW, **k), CHK, lease_holder="w1")
dup("maintenance_schedule: one row per device", "maintenance_schedule", dict(device_id=D, created_at=NOW, updated_at=NOW), "UNIQUE")
# commands
dup("commands: unique idempotency key per device", "commands", f_cmd(D, idempotency_key="idem-1"), UNQ)
accept("commands: same idempotency key on another device accepted", "commands", f_cmd(D3, idempotency_key="idem-1"))
fire("commands: type enum", "commands", lambda **k: f_cmd(D3, **k), CHK, type="format_disk")
fire("commands: status enum", "commands", lambda **k: f_cmd(D3, **k), CHK, status="lost")
fire("commands: expires_at after created_at", "commands", lambda **k: f_cmd(D3, **k), CHK, expires_at=NOW)
fire("commands: json_valid(params)", "commands", lambda **k: f_cmd(D3, **k), CHK, params="{")
fire("commands: idempotency_key not empty", "commands", lambda **k: f_cmd(D3, **k), CHK, idempotency_key="")
# restore_requests
fire("restore_requests: mode enum", "restore_requests", lambda **k: f_rest(D3, **k), CHK, mode="email_me")
fire("restore_requests: status enum", "restore_requests", lambda **k: f_rest(D3, **k), CHK, status="eaten")
fire("restore_requests: overwrite enum", "restore_requests", lambda **k: f_rest(D3, **k), CHK, overwrite="sometimes")
fire("restore_requests: verify cannot combine with simulate", "restore_requests", lambda **k: f_rest(D3, **k), CHK, verify=1, simulate=1)
fire("restore_requests: artifact requires purge_after", "restore_requests", lambda **k: f_rest(D3, **k), CHK, artifact_ref="a.zip")
fire("restore_requests: artifact only for download modes", "restore_requests", lambda **k: f_rest(D3, **k), CHK, mode="device_path", artifact_ref="a.zip", purge_after=NOW)
accept("restore_requests: download with artifact + purge_after accepted", "restore_requests", f_rest(D3, artifact_ref="a.zip", purge_after=NOW + 1000))
fire("restore_requests: device_relay requires target_device_id", "restore_requests", lambda **k: f_rest(D3, **k), CHK, mode="device_relay")
fire("restore_requests: target_device_id requires device_relay mode", "restore_requests", lambda **k: f_rest(D3, **k), CHK, target_device_id=D)
fire("restore_requests: target device must differ from source", "restore_requests", lambda **k: f_rest(D3, **k), CHK, mode="device_relay", target_device_id=D3)
accept("restore_requests: relay to a replacement device accepted", "restore_requests", f_rest(D3, mode="device_relay", target_device_id=D2, artifact_ref="r.zip", purge_after=NOW + 1000))
fire("restore_requests: FK target device", "restore_requests", lambda **k: f_rest(D3, **k), FK, mode="device_relay", target_device_id="01NOSUCHDEVICE00000000000")
fire("restore_requests: snapshot_id is 64 hex", "restore_requests", lambda **k: f_rest(D3, **k), CHK, snapshot_id="abc")
# alarm_acks
dup("alarm_acks: one active ack per device and alarm type", "alarm_acks", f_ack(D), UNQ,
    release=[("UPDATE alarm_acks SET cleared_at = 1 WHERE id = ?", (ack["id"],))])
accept("alarm_acks: different alarm type on same device accepted", "alarm_acks", f_ack(D, alarm_type="unseen"))
fire("alarm_acks: alarm_type enum", "alarm_acks", lambda **k: f_ack(D3, **k), CHK, alarm_type="vibes")
fire("alarm_acks: device subject cannot carry a customer", "alarm_acks", lambda **k: f_ack(D3, **k), CHK, customer_id=C)
fire("alarm_acks: customer subject requires customer_id", "alarm_acks", lambda **k: f_ack(D3, **k), CHK, subject_type="customer")
accept("alarm_acks: customer ack accepted", "alarm_acks", f_ack(None, subject_type="customer", device_id=None, customer_id=C, alarm_type="missing_customer"))
insert("alarm_acks", f_ack(None, subject_type="customer", device_id=None, customer_id=C, alarm_type="missing_customer"))
dup("alarm_acks: one active ack per customer and alarm type", "alarm_acks",
    f_ack(None, subject_type="customer", device_id=None, customer_id=C, alarm_type="missing_customer"), UNQ)
# notification_rules / log
fire("notification_rules: global scope cannot carry a customer", "notification_rules", f_rule, CHK, customer_id=C)
fire("notification_rules: customer scope requires customer", "notification_rules", f_rule, CHK, scope="customer")
fire("notification_rules: device scope requires device", "notification_rules", f_rule, CHK, scope="device")
accept("notification_rules: device-scoped rule accepted", "notification_rules", f_rule(scope="device", device_id=D))
fire("notification_rules: channel enum", "notification_rules", f_rule, CHK, channel="carrier_pigeon")
fire("notification_rules: min_severity enum", "notification_rules", f_rule, CHK, min_severity="loud")
fire("notification_rules: failure_threshold >= 1", "notification_rules", f_rule, CHK, failure_threshold=0)
fire("notification_log: result enum", "notification_log", f_nlog, CHK, result="maybe")
fire("notification_log: kind enum", "notification_log", f_nlog, CHK, kind="gossip")
fire("notification_log: suppress_reason only when suppressed", "notification_log", f_nlog, CHK, suppress_reason="snoozed")
accept("notification_log: suppressed + reason accepted", "notification_log", f_nlog(result="suppressed", suppress_reason="snoozed"))
fire("notification_log: sent_at must be epoch milliseconds", "notification_log", f_nlog, CHK, sent_at=12)
sql_ok("notification_log: deleting a rule keeps its log rows (SET NULL)",
       [("DELETE FROM notification_rules WHERE id = ?", (rule["id"],))],
       "SELECT count(*) FROM notification_log WHERE rule_id IS NULL AND dedupe_key IS NOT NULL", (), 1)
# saved_views / reports
fire("saved_views: entity enum", "saved_views", f_view, CHK, entity="secrets")
fire("saved_views: json_valid(filter)", "saved_views", f_view, CHK, filter="(")
dup("saved_views: unique (owner, entity, name)", "saved_views", f_view(owner_id=A, name=view["name"]), UNQ)
fire("reports: customer scope requires customer", "reports", f_report, CHK, scope="customer")
fire("reports: global scope cannot carry customer", "reports", f_report, CHK, customer_id=C)
fire("reports: format enum", "reports", f_report, CHK, format="docx")
fire("reports: kind enum", "reports", f_report, CHK, kind="gossip")
sql_ok("reports: deleting a saved view nulls reports.saved_view_id",
       [("DELETE FROM saved_views WHERE id = ?", (view["id"],))],
       "SELECT count(*) FROM reports WHERE saved_view_id IS NULL", (), 1)
# audit_events
fire("audit_events: actor_type enum", "audit_events", f_audit, CHK, actor_type="ghost")
fire("audit_events: result enum", "audit_events", f_audit, CHK, result="shrug")
fire("audit_events: method enum", "audit_events", f_audit, CHK, method="smoke")
fire("audit_events: occurred_at must be epoch milliseconds", "audit_events", f_audit, CHK, occurred_at=99)
fire("audit_events: json_valid(details)", "audit_events", f_audit, CHK, details="{")
accept("audit_events: no FKs - ids of deleted/nonexistent objects accepted", "audit_events",
       f_audit(actor_id="01GONE00000000000000000000", object_id="01GONE00000000000000000001", customer_id="01GONE00000000000000000002"))
sql_fails("audit_events: UPDATE is blocked by trigger", [], "UPDATE audit_events SET action = 'tampered'", (), "immutable")
sql_ok("audit_events: DELETE (retention job) is allowed", [("DELETE FROM audit_events", ())], "SELECT count(*) FROM audit_events", (), 0)
# agent_releases / job_state
dup("agent_releases: unique (version, channel)", "agent_releases", f_rel(version=conn.execute("SELECT version FROM agent_releases").fetchone()[0]), UNQ)
accept("agent_releases: same version on the other channel accepted", "agent_releases", f_rel(version=conn.execute("SELECT version FROM agent_releases").fetchone()[0], channel="stable"))
fire("agent_releases: channel enum", "agent_releases", f_rel, CHK, channel="beta")
fire("agent_releases: sha256 format", "agent_releases", f_rel, CHK, sha256="zz")
fire("agent_releases: json_valid(manifest)", "agent_releases", f_rel, CHK, manifest="{")
fire("job_state: lease holder and lease_until together", "job_state", f_job, CHK, lease_holder="w1")
fire("job_state: last_status enum", "job_state", f_job, CHK, last_status="meh")
# ------------------------------------------------------------------ 3. cascade behaviour
sql_ok("cascade: deleting a device removes runs/snapshots/tokens/presence/heartbeats/commands/etc.",
       [("DELETE FROM devices WHERE id = ?", (D,))],
       "SELECT (SELECT count(*) FROM runs WHERE device_id = ?) + (SELECT count(*) FROM snapshots WHERE device_id = ?) + "
       "(SELECT count(*) FROM device_tokens WHERE device_id = ?) + (SELECT count(*) FROM gateway_logins WHERE device_id = ?) + "
       "(SELECT count(*) FROM repo_key_escrow WHERE device_id = ?) + (SELECT count(*) FROM commands WHERE device_id = ?) + "
       "(SELECT count(*) FROM heartbeats WHERE device_id = ?) + (SELECT count(*) FROM device_presence WHERE device_id = ?)",
       (D,) * 8, 0)
# customer purge path: devices removed first, then the customer; its customer-scoped policy cascades
cust2 = f_customer(S, policy_id=None); insert("customers", cust2)
cpol2 = f_policy(scope="customer", customer_id=cust2["id"], name="Cust2 policy"); insert("policies", cpol2)
conn.execute("UPDATE customers SET policy_id = ? WHERE id = ?", (cpol2["id"], cust2["id"]))
sql_ok("customer delete cascades its customer-scoped policy (circular FK does not deadlock)",
       [("DELETE FROM customers WHERE id = ?", (cust2["id"],))],
       "SELECT count(*) FROM policies WHERE id = ?", (cpol2["id"],), 0)

# ------------------------------------------------------------------ 4. structural checks
bad_fk = []
for t in TABLES:
    fks = conn.execute(f"PRAGMA foreign_key_list({t})").fetchall()
    indexes = []
    for idx in conn.execute(f"PRAGMA index_list({t})").fetchall():
        cols = [c[2] for c in conn.execute(f"PRAGMA index_info({idx[1]})").fetchall()]
        indexes.append(cols)
    pk = [c[1] for c in conn.execute(f"PRAGMA table_info({t})").fetchall() if c[5]]
    pk_sorted = sorted(conn.execute(f"PRAGMA table_info({t})").fetchall(), key=lambda c: c[5])
    pk_cols = [c[1] for c in pk_sorted if c[5]]
    for fk in fks:
        col = fk[3]
        if not any(cols and cols[0] == col for cols in indexes) and not (pk_cols and pk_cols[0] == col):
            bad_fk.append(f"{t}.{col}")
record("every foreign-key column is the leading column of an index/PK/UNIQUE", not bad_fk, f"unindexed: {bad_fk}")

no_created = [t for t in TABLES if "created_at" not in [c[1] for c in conn.execute(f"PRAGMA table_info({t})")]]
record("every table has created_at", not no_created, str(no_created))
IMMUTABLE = {"schema_migrations", "policy_revisions", "heartbeats", "audit_events"}
no_updated = [t for t in TABLES if t not in IMMUTABLE and "updated_at" not in [c[1] for c in conn.execute(f"PRAGMA table_info({t})")]]
record("every mutable table has updated_at (immutable: %s)" % ", ".join(sorted(IMMUTABLE)), not no_updated, str(no_updated))
no_pk = [t for t in TABLES if not any(c[5] for c in conn.execute(f"PRAGMA table_info({t})"))]
record("every table has a primary key", not no_pk, str(no_pk))
bad_ondelete = []
for t in TABLES:
    for fk in conn.execute(f"PRAGMA foreign_key_list({t})"):
        if fk[6] in ("NO ACTION", None, ""):
            bad_ondelete.append(f"{t}.{fk[3]}")
record("every FK has an explicit ON DELETE rule (not NO ACTION)", not bad_ondelete, str(bad_ondelete))
fkcheck = conn.execute("PRAGMA foreign_key_check").fetchall()
record("PRAGMA foreign_key_check is clean", not fkcheck, str(fkcheck))
integ = conn.execute("PRAGMA integrity_check").fetchone()[0]
record("PRAGMA integrity_check = ok", integ == "ok", integ)
ids_not_26 = [t for t in TABLES if any(c[1] == "id" and c[2].upper() == "TEXT" for c in conn.execute(f"PRAGMA table_info({t})"))]
record("tables with TEXT id PK (ULID): %d" % len(ids_not_26), True, "")

n_idx = conn.execute("SELECT count(*) FROM sqlite_master WHERE type='index' AND sql IS NOT NULL").fetchone()[0]
n_uniq = conn.execute("SELECT count(*) FROM sqlite_master WHERE type='index' AND sql LIKE 'CREATE UNIQUE%'").fetchone()[0]
n_part = conn.execute("SELECT count(*) FROM sqlite_master WHERE type='index' AND sql LIKE '% WHERE %'").fetchone()[0]

# ------------------------------------------------------------------ report
failed = [r for r in results if not r[1]]
for label, ok, detail in results:
    print(("PASS  " if ok else "FAIL  ") + label + ("" if ok else "   <-- " + detail))
print()
print(f"tables={len(TABLES)} explicit_indexes={n_idx} (unique={n_uniq}, partial={n_part})")
print(f"checks={len(results)} passed={len(results) - len(failed)} failed={len(failed)}")
sys.exit(1 if failed else 0)
