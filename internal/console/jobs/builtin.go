package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
	"github.com/c2tech-sys/cryobackup/internal/console/restores"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// Settings keys the htpasswd-sync job keeps (global key/value rows).
const (
	SettingHtpasswdGeneration = "htpasswd_generation"
	SettingHtpasswdBodySHA    = "htpasswd_body_sha"
)

// Signal is a coalescing wake-up for a job: Notify never blocks and any number
// of notifications before the job runs make it run once.
type Signal struct {
	once sync.Once
	ch   chan struct{}
}

func (s *Signal) init() { s.once.Do(func() { s.ch = make(chan struct{}, 1) }) }

// Notify wakes the job.
func (s *Signal) Notify() {
	s.init()
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

// C is the channel to put in Job.Signal.
func (s *Signal) C() <-chan struct{} { s.init(); return s.ch }

// HtpasswdSync renders the gateway's htpasswd file from the active gateway
// logins plus the derived probe login and replaces it atomically (architecture
// §7.2, §5.7). It runs every five minutes and on Signal (an enrollment, a
// retirement).
//
// bcrypt is salted, so hashing the probe password afresh on every run would
// make the reconcile see drift forever. The job parses the deployed file and
// keeps its probe line for as long as gateway.CheckPassword(thatHash,
// gateway.ProbePassword(master)) holds, hashing a new one only when the line is
// absent or stale (first sync, a changed master key).
//
// The generation is kept in the settings table (htpasswd_generation). The job
// refuses to overwrite a deployed file whose generation is newer than its own
// view (two consoles on one gateway: an unsupported topology, detected here) and
// raises an audit event instead; an out-of-band edit is repaired and audited as
// gateway.htpasswd.drift.
//
// Runs never overlap in this process, whoever starts them (the signal, the timer, Run
// now): a run takes the slot before it reads the logins, so a later run can never be
// overwritten by an earlier one. Across consoles the runner's lease and the generation
// header are what stands guard (two consoles on one gateway is unsupported, ADR D3).
//
// Check reads the deployed file. It is the only thing that can say a login has left it.
type HtpasswdSync struct {
	Store  *store.Store
	Master *escrow.MasterKey
	Dir    string
	Sig    *Signal

	slotOnce sync.Once
	slot     chan struct{} // one holder: a run, or a check
	last     atomic.Pointer[LoginFileCheck]
}

// Classes of a LoginFileCheck that could not read the file.
const (
	LoginFileMissing    = "file_missing"
	LoginFileUnreadable = "file_unreadable"
	LoginFileGarbled    = "file_garbled"
	LoginFileNoStore    = "store_unreadable"
	// LoginFileBusy: a run or a check held the slot for the caller's whole wait. Nothing was read.
	LoginFileBusy = "sync_busy"
)

// LoginFileCheck is what one read of the deployed file found: never hashes, and never logged.
type LoginFileCheck struct {
	// At is when the file was read, or when reading it failed.
	At time.Time
	// InFile lists the usernames of retired devices in the file. A failed read carries the newest
	// clean read's list forward: only a clean read can clear a login.
	InFile []string
	// ReadClass is why the file could not be read ("" = a clean read).
	ReadClass string
}

// Has reports whether the device's login was in the file at the newest clean read.
func (c LoginFileCheck) Has(deviceID string) bool { return slices.Contains(c.InFile, deviceID) }

func (j *HtpasswdSync) take(ctx context.Context) error {
	j.slotOnce.Do(func() { j.slot = make(chan struct{}, 1) })
	select {
	case j.slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (j *HtpasswdSync) give() { <-j.slot }

// Check reads the deployed file and returns which retired devices' logins are in it. It
// waits for a run in flight, because that run may hold logins read before a retire
// committed: a device absent from this answer stays absent, since every later run reads
// the logins after this check, and so after any retire committed before the check began.
// It changes nothing but the newest measurement. A wait that ends first reads nothing
// (LoginFileBusy) and records nothing.
func (j *HtpasswdSync) Check(ctx context.Context) LoginFileCheck {
	if err := j.take(ctx); err != nil {
		c := LoginFileCheck{At: j.Store.Now(), ReadClass: LoginFileBusy}
		if prev := j.last.Load(); prev != nil {
			c.InFile = prev.InFile
		}
		return c
	}
	defer j.give()
	return j.measure(context.WithoutCancel(ctx)) // a caller that gave up must not record a store failure that never happened
}

// measure reads the file with the slot held and records the result.
func (j *HtpasswdSync) measure(ctx context.Context) LoginFileCheck {
	c := LoginFileCheck{At: j.Store.Now()}
	in, class := j.retiredInFile(ctx)
	if class == "" {
		c.InFile = in
	} else {
		c.ReadClass = class
		if prev := j.last.Load(); prev != nil {
			c.InFile = prev.InFile
		}
	}
	j.last.Store(&c)
	return c
}

func (j *HtpasswdSync) retiredInFile(ctx context.Context) ([]string, string) {
	raw, err := os.ReadFile(filepath.Join(j.Dir, gateway.FileName))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, LoginFileMissing
	case err != nil:
		return nil, LoginFileUnreadable
	}
	_, _, logins, err := gateway.Parse(raw)
	if err != nil {
		return nil, LoginFileGarbled
	}
	retired, err := j.Store.RetiredDeviceIDs(ctx)
	if err != nil {
		return nil, LoginFileNoStore
	}
	var in []string
	for _, l := range logins {
		if slices.Contains(retired, l.Username) {
			in = append(in, l.Username)
		}
	}
	return in, ""
}

// Facts is the newest measurement as the engine reads it (banner C17). The evaluator adds
// the retired devices; the prober adds its cadence.
func (j *HtpasswdSync) Facts() *state.LoginFileFacts {
	c := j.last.Load()
	if c == nil {
		return &state.LoginFileFacts{}
	}
	return &state.LoginFileFacts{MeasuredAt: state.Millis(c.At.UnixMilli()), ReadFailed: c.ReadClass, InFile: len(c.InFile)}
}

// Job returns the schedule for the runner.
func (j *HtpasswdSync) Job() Job {
	if j.Sig == nil {
		j.Sig = &Signal{}
	}
	return Job{Name: NameHtpasswdSync, Every: 5 * time.Minute, Run: j.Run, Signal: j.Sig.C()}
}

func (j *HtpasswdSync) settingInt(ctx context.Context, key string) int64 {
	s, err := j.Store.GetSetting(ctx, key)
	if err != nil {
		return 0
	}
	var n int64
	if json.Unmarshal(s.Value, &n) != nil {
		return 0
	}
	return n
}

func (j *HtpasswdSync) settingString(ctx context.Context, key string) string {
	s, err := j.Store.GetSetting(ctx, key)
	if err != nil {
		return ""
	}
	var v string
	_ = json.Unmarshal(s.Value, &v)
	return v
}

func (j *HtpasswdSync) audit(ctx context.Context, action, result, severity string, details string) {
	_, _ = j.Store.AppendAudit(ctx, store.AuditEvent{ActorType: "system", Method: "system", Action: action, ObjectType: "gateway",
		ObjectID: "htpasswd", Result: result, Severity: severity, Details: []byte(details)})
}

// Run is one sync pass, measured at its end whatever it did, so a banner follows a run at once.
func (j *HtpasswdSync) Run(ctx context.Context) error {
	if err := j.take(ctx); err != nil {
		return err
	}
	defer j.give()
	defer func() { j.measure(context.WithoutCancel(ctx)) }()
	return j.run(ctx)
}

func (j *HtpasswdSync) run(ctx context.Context) error {
	if !j.Master.Loaded() {
		return &ClassError{Class: ClassMasterKey}
	}
	probePlain, err := gateway.ProbePassword(j.Master.Bytes())
	if err != nil {
		return &ClassError{Class: ClassInternal, Err: err}
	}
	entries, err := j.Store.ListGatewayLoginsForHtpasswd(ctx)
	if err != nil {
		return err
	}
	logins := make([]gateway.Login, len(entries))
	names := make([]string, len(entries))
	for i, e := range entries {
		logins[i] = gateway.Login{Username: e.Username, Hash: e.PasswordHash}
		names[i] = e.Username
	}

	path := filepath.Join(j.Dir, gateway.FileName)
	deployed, rerr := os.ReadFile(path)
	exists := rerr == nil
	if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return &ClassError{Class: ClassIO, Err: rerr}
	}
	var deployedGen int64
	parsed := false
	deployedSHA := ""
	probeHash := ""
	if exists {
		if gen, sha, ls, perr := gateway.Parse(deployed); perr == nil {
			parsed, deployedGen, deployedSHA = true, gen, sha
			for _, l := range ls {
				if l.Username == gateway.ProbeUsername && gateway.CheckPassword(l.Hash, probePlain) {
					probeHash = l.Hash
				}
			}
		}
	}
	if probeHash == "" {
		if probeHash, err = gateway.HashPassword(probePlain); err != nil {
			return &ClassError{Class: ClassInternal, Err: err}
		}
	}

	// Compare bodies at the deployed generation: a generation change alone is not drift.
	compare, err := gateway.RenderE(logins, probeHash, deployedGen)
	if err != nil {
		return &ClassError{Class: ClassInternal, Err: err}
	}
	drifted, err := gateway.Drifted(j.Dir, compare)
	if err != nil {
		return &ClassError{Class: ClassIO, Err: err}
	}
	if !drifted {
		return nil
	}

	myGen := j.settingInt(ctx, SettingHtpasswdGeneration)
	newGen := myGen + 1
	content, err := gateway.RenderE(logins, probeHash, newGen)
	if err != nil {
		return &ClassError{Class: ClassInternal, Err: err}
	}
	if err := gateway.SyncIfOwner(j.Dir, content, newGen); err != nil {
		if errors.Is(err, gateway.ErrNewerGeneration) {
			j.audit(ctx, "gateway.htpasswd.newer_generation", "error", "critical", `{"reason":"another console wrote a newer generation"}`)
			return &ClassError{Class: ClassNewerGen, Err: err}
		}
		return &ClassError{Class: ClassIO, Err: err}
	}
	_, sha, cerr := gateway.Current(j.Dir)
	if cerr != nil {
		return &ClassError{Class: ClassIO, Err: cerr}
	}
	// Drift: the file existed but is not what this job last wrote.
	lastSHA := j.settingString(ctx, SettingHtpasswdBodySHA)
	if exists && (!parsed || (lastSHA != "" && deployedSHA != lastSHA)) {
		j.audit(ctx, "gateway.htpasswd.drift", "ok", "warning", `{"repaired":true}`)
	}
	if err := j.Store.PutSetting(ctx, SettingHtpasswdGeneration, json.RawMessage(strconv.FormatInt(newGen, 10)), ""); err != nil {
		return err
	}
	shaJSON, _ := json.Marshal(sha)
	if err := j.Store.PutSetting(ctx, SettingHtpasswdBodySHA, shaJSON, ""); err != nil {
		return err
	}
	if _, err := j.Store.MarkHtpasswdSynced(ctx, names); err != nil {
		return err
	}
	j.audit(ctx, "gateway.htpasswd.sync", "ok", "info", `{"logins":`+strconv.Itoa(len(names))+`}`)
	return nil
}

// HeartbeatRingTrim deletes heartbeat rows older than the ring (setting
// heartbeat_ring_hours, 24 by default), every ten minutes, and the expired
// fleet start leases (ADR 0005; a lease request also drops its target's
// expired rows, so this only keeps the table small).
type HeartbeatRingTrim struct {
	Store *store.Store
}

// Job returns the schedule for the runner.
func (j *HeartbeatRingTrim) Job() Job {
	return Job{Name: NameHeartbeatRingTrim, Every: 10 * time.Minute, Run: j.Run}
}

// Run deletes in batches of 5000.
func (j *HeartbeatRingTrim) Run(ctx context.Context) error {
	hours := int64(24)
	if s, err := j.Store.GetSetting(ctx, "heartbeat_ring_hours"); err == nil {
		var n int64
		if json.Unmarshal(s.Value, &n) == nil && n > 0 {
			hours = n
		}
	}
	if _, err := j.Store.TrimHeartbeats(ctx, j.Store.Now().Add(-time.Duration(hours)*time.Hour), 5000); err != nil {
		return err
	}
	_, err := j.Store.DeleteExpiredStartLeases(ctx, j.Store.Now())
	return err
}

// RestorePurge expires spent technician restores (clearing their encrypted
// selection) every five minutes and scrubs restore archives an earlier version
// staged on disk (restores.Service.Purge, ADR 0008).
type RestorePurge struct {
	Svc *restores.Service
}

// Job returns the schedule for the runner.
func (j *RestorePurge) Job() Job {
	return Job{Name: NameRestorePurge, Every: 5 * time.Minute, Run: func(ctx context.Context) error {
		_, err := j.Svc.Purge(ctx)
		return err
	}}
}

// SessionExpire deletes expired and revoked sessions every hour.
type SessionExpire struct {
	Store *store.Store
}

// Job returns the schedule for the runner.
func (j *SessionExpire) Job() Job {
	return Job{Name: NameSessionExpire, Every: time.Hour, Run: func(ctx context.Context) error {
		_, err := j.Store.DeleteExpiredSessions(ctx)
		return err
	}}
}

// CommandExpiry marks queued or delivered commands past their expiry as expired
// (store.ExpireCommands) every minute (architecture: 60 s), under the runner's lease like every
// job: an agent that never comes back must not leave commands waiting forever,
// and an expired command can no longer be acknowledged.
type CommandExpiry struct {
	Store *store.Store
}

// Job returns the schedule for the runner.
func (j *CommandExpiry) Job() Job {
	return Job{Name: NameCommandExpiry, Every: time.Minute, Run: j.Run}
}

// Run expires what is due.
func (j *CommandExpiry) Run(ctx context.Context) error {
	_, err := j.Store.ExpireCommands(ctx)
	return err
}
