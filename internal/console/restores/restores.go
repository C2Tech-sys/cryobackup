// Package restores runs the technician restore lifecycle (architecture §5.2,
// §7.3, DH22, ADR 0008): one snapshot path - a file or a directory - streamed
// from the repository as a zip, through the maintenance worker and the console,
// to the technician's browser. Nothing restored is written to the console
// host's disk, not even briefly (ADR 0008 supersedes the staging part of DH27).
//
// The path travels only in the restore request (encrypted at rest under the
// master key, cleared when the restore is spent), over the adminpath socket
// and into the form that picks it; it is never logged, audited, cached or
// placed in a URL.
//
// Lifecycle: ready (Create validated the request; there is nothing to build)
// -> delivered (a download streamed to its last byte; size and SHA-256 are
// recorded then) -> expired (the restore-purge job cleared the selection at
// expires_at). A download starts with a single-use token bound to the minting
// session; a dropped download records its failure class on the row, leaves the
// status as it was, and starts again from the beginning with a new token (no
// resume, no Range). A delivered restore can be downloaded again until it
// expires: each download streams from the repository again. Customer-held
// custody is refused with ErrCustomerHeld (DH22).
package restores

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/adminpath"
	"github.com/c2tech-sys/cryobackup/internal/console/escrow"
	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// Errors returned by Create and the download helpers.
var (
	// ErrCustomerHeld: the device's key is held by the customer (HTTP 409 custody_customer_held).
	ErrCustomerHeld = errors.New("restores: customer-held key, no technician restore")
	// ErrMasterKey: the master key is not loaded (HTTP 503 master_key_unavailable).
	ErrMasterKey = errors.New("restores: master key unavailable")
	// ErrInvalid: a request field is not acceptable (HTTP 400).
	ErrInvalid = errors.New("restores: invalid request")
	// ErrNotReady: the restore cannot be downloaded (expired, failed, cancelled; HTTP 409).
	ErrNotReady = errors.New("restores: not ready")
	// ErrExpired: the restore is past expires_at (or already expired by the
	// purge job): no token is minted or accepted and no download starts (HTTP
	// 410). It wraps ErrNotReady.
	ErrExpired = fmt.Errorf("%w: the restore expired", ErrNotReady)
	// ErrBusy: a download of this restore is streaming right now (HTTP 409).
	ErrBusy = errors.New("restores: a download is already streaming")
	// ErrStreamsBusy: the maintenance worker's stream budget is used up by
	// other downloads (adminpath.CodeBusy). Nothing is recorded on the restore;
	// the technician tries again with a new token (HTTP 503 with Retry-After).
	ErrStreamsBusy = errors.New("restores: every restore stream is in use")
	// ErrToken: the download token is invalid, expired or already used (HTTP 403).
	ErrToken = errors.New("restores: invalid download token")
)

// DefaultTTL is how long a restore request can be downloaded.
const DefaultTTL = 24 * time.Hour

// TokenTTL is how long a minted download token works.
const TokenTTL = 2 * time.Minute

// Defaults of the minimum-rate guard of a download stream (Service.MinRateBytes
// and Service.MinRateWindow; console config restore_stream.min_bytes and
// restore_stream.window_s).
const (
	DefaultMinRateBytes  = 64 << 10
	DefaultMinRateWindow = 2 * time.Minute
	// DefaultFirstByteTimeout is Service.FirstByteTimeout's default (console
	// config restore_stream.first_byte_s).
	DefaultFirstByteTimeout = 5 * time.Minute
)

// Failure classes a download records besides the admin-path classes.
const (
	// ClassDisconnected: the technician's browser went away mid-stream.
	ClassDisconnected = "disconnected"
	// ClassUnavailable: the maintenance worker could not be reached.
	ClassUnavailable = "unavailable"
	// ClassTooSlow: fewer than MinRateBytes reached the browser in a
	// MinRateWindow, so the stream was aborted to free the worker's slot.
	ClassTooSlow = "too_slow"
	// ClassNoData: no byte arrived within FirstByteTimeout, so the stream was
	// cancelled before anything reached the browser.
	ClassNoData = "no_data"
)

// Actor is who asks for a restore.
type Actor struct {
	AdminID  string
	Username string
	IP       string
	// Method is how the request came in: "web" (the console pages) or "api"
	// (the admin JSON API). It is recorded in the audit row; empty means "web".
	Method string
}

// Service creates restore requests and streams their downloads.
type Service struct {
	Store   *store.Store
	Backend adminpath.Backend
	Master  *escrow.MasterKey
	// ArtifactsDir is where versions before ADR 0008 staged restore archives.
	// Nothing writes there any more; Purge deletes what an earlier version left.
	// Empty skips that scrub.
	ArtifactsDir string
	Clock        func() time.Time
	Log          *logx.Logger
	TTL          time.Duration
	// MinRateBytes and MinRateWindow are the minimum-rate guard of a download
	// (M1-HIPAA2): once the first byte went out, a stream that delivers fewer
	// than MinRateBytes to the browser in any MinRateWindow is aborted and
	// recorded as ClassTooSlow, so a slow reader cannot hold a restic slot of
	// the maintenance worker for hours. The window is measured on the monotonic
	// clock, not Clock. Zero takes DefaultMinRateBytes and DefaultMinRateWindow.
	MinRateBytes  int64
	MinRateWindow time.Duration
	// FirstByteTimeout bounds the time from the start of a download to its
	// first byte (restic that hangs before it produces anything would hold a
	// stream slot until the dump timeout): on expiry the stream is cancelled -
	// the worker's restic run with it - and recorded as ClassNoData. Zero takes
	// DefaultFirstByteTimeout.
	FirstByteTimeout time.Duration

	// guardClock drives the stream guard in tests; nil is wall time.
	guardClock *guardClock

	mu        sync.Mutex
	tokens    *tokenTable
	streaming map[string]bool
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return DefaultTTL
}

func (s *Service) minRateBytes() int64 {
	if s.MinRateBytes > 0 {
		return s.MinRateBytes
	}
	return DefaultMinRateBytes
}

func (s *Service) minRateWindow() time.Duration {
	if s.MinRateWindow > 0 {
		return s.MinRateWindow
	}
	return DefaultMinRateWindow
}

func (s *Service) firstByteTimeout() time.Duration {
	if s.FirstByteTimeout > 0 {
		return s.FirstByteTimeout
	}
	return DefaultFirstByteTimeout
}

func (s *Service) log() *logx.Logger {
	if s.Log != nil {
		return s.Log
	}
	return logx.Default()
}

var snapshotIDRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidPath accepts an absolute snapshot path: it starts with "/", has no
// control characters and is at most 4096 bytes. adminpath validates it again.
func ValidPath(p string) bool {
	if p == "" || p[0] != '/' || len(p) > 4096 {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f {
			return false
		}
	}
	return true
}

// Create validates and records a download_zip restore of one snapshot path (a
// file or a directory). There is nothing to build: the returned row is ready,
// and the archive is produced only while a download streams it.
func (s *Service) Create(ctx context.Context, who Actor, scope store.Scope, deviceID, snapshotID, filePath string) (store.RestoreRequest, error) {
	if !snapshotIDRE.MatchString(snapshotID) || !ValidPath(filePath) {
		return store.RestoreRequest{}, ErrInvalid
	}
	dev, err := s.Store.GetDevice(ctx, scope, deviceID)
	if err != nil {
		return store.RestoreRequest{}, err
	}
	if dev.RetiredAt != nil {
		return store.RestoreRequest{}, ErrInvalid
	}
	esc, err := s.Store.ActiveEscrow(ctx, scope, deviceID)
	if err != nil {
		return store.RestoreRequest{}, err
	}
	if esc.Custody != "escrowed" {
		return store.RestoreRequest{}, ErrCustomerHeld
	}
	if !s.Master.Loaded() {
		return store.RestoreRequest{}, ErrMasterKey
	}
	snaps, err := s.Store.ListSnapshots(ctx, scope, deviceID)
	if err != nil {
		return store.RestoreRequest{}, err
	}
	known := false
	for _, sn := range snaps {
		if sn.SnapshotID == snapshotID {
			known = true
			break
		}
	}
	if !known {
		return store.RestoreRequest{}, ErrInvalid
	}
	wrapped, err := s.Master.Wrap(escrow.ContextRestoreSelection, deviceID+"/"+snapshotID, 1, []byte(filePath))
	if err != nil {
		return store.RestoreRequest{}, err
	}
	sum := sha256.Sum256([]byte(filePath))
	now := s.now()
	row, err := s.Store.CreateRestoreRequestAudited(ctx, scope, store.RestoreRequest{
		DeviceID: deviceID, SnapshotID: snapshotID, Mode: "download_zip", Overwrite: "if-changed",
		SelectionCount: 1, SelectionHash: hex.EncodeToString(sum[:]), SelectionWrapped: wrapped,
		Status: "ready", RequestedBy: who.AdminID, ExpiresAt: now.Add(s.ttl()),
	}, store.AuditActor{AdminID: who.AdminID, Username: who.Username, IP: who.IP, Method: who.Method})
	if err != nil {
		return store.RestoreRequest{}, err
	}
	return row, nil
}

// Downloadable reports whether a download may start for r at now: a technician
// download that is ready or delivered, not past expires_at, whose encrypted
// selection is still there.
func (s *Service) Downloadable(r store.RestoreRequest) bool {
	return r.Mode == "download_zip" && (r.Status == "ready" || r.Status == "delivered") &&
		len(r.SelectionWrapped) > 0 && s.now().Before(r.ExpiresAt)
}

// notDownloadable is the error for a restore Downloadable refuses: ErrExpired
// once expires_at has passed or the purge job expired it, else ErrNotReady.
// Until expires_at a delivered restore stays downloadable (operator ruling
// 2026-10-29): every download needs a new single-use token and streams again.
func (s *Service) notDownloadable(r store.RestoreRequest) error {
	if r.Status == "expired" || r.Status == "purged" || !s.now().Before(r.ExpiresAt) {
		return ErrExpired
	}
	return ErrNotReady
}

// ---- download ----

type dlToken struct {
	hash      [32]byte
	sessionID string
	expires   time.Time
	seq       uint64 // mint sequence number (tokenTable.seq at mint)
}

// tokens holds the live download token of each restore, as a sha256 hash, in
// memory: minting again replaces (and so invalidates) the previous one, using
// it consumes it, a process restart invalidates all of them, and the plaintext
// is never stored.
type tokenTable struct {
	mu sync.Mutex
	m  map[string]dlToken
	// seq numbers every mint, process-wide and only upwards; lastMint is each
	// restore's latest mint, so a given-back token is restored only when
	// nothing was minted for its restore after it (giveBack). An entry is
	// dropped once its mint is older than TokenTTL: every token minted up to
	// then has expired, and giveBack refuses an expired token anyway.
	seq      uint64
	lastMint map[string]mintMark
}

type mintMark struct {
	seq uint64
	at  time.Time
}

func (s *Service) table() *tokenTable {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		s.tokens = &tokenTable{m: map[string]dlToken{}, lastMint: map[string]mintMark{}}
	}
	return s.tokens
}

// MintToken returns a download token for a downloadable restore, bound to the
// minting admin session and the restore id, valid for TokenTTL and stored only
// as its SHA-256. Minting again invalidates the previous token of that restore.
// The caller must keep the token out of every log and page: it is handed to
// the browser once, in a redirect.
func (s *Service) MintToken(r store.RestoreRequest, sessionID string) (string, time.Time, error) {
	if !s.Downloadable(r) {
		return "", time.Time{}, s.notDownloadable(r)
	}
	if sessionID == "" {
		return "", time.Time{}, ErrToken
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw[:])
	now := s.now()
	t := s.table()
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, d := range t.m {
		if now.After(d.expires) {
			delete(t.m, id)
		}
	}
	for id, mk := range t.lastMint {
		if now.After(mk.at.Add(TokenTTL)) {
			delete(t.lastMint, id)
		}
	}
	exp := now.Add(TokenTTL)
	t.seq++
	t.lastMint[r.ID] = mintMark{seq: t.seq, at: now}
	t.m[r.ID] = dlToken{hash: sha256.Sum256([]byte(tok)), sessionID: sessionID, expires: exp, seq: t.seq}
	return tok, exp, nil
}

// VerifyToken checks a presented token against the restore as it stands now
// and consumes it: it must be the latest minted for this restore, minted by
// this very session, not expired, and the restore must still be downloadable.
// A token works exactly once (there is no resume: a second download needs a new
// token and streams again from the beginning). Past expires_at every token of
// the restore, valid or not, is refused with ErrExpired and dropped.
func (s *Service) VerifyToken(token string, r store.RestoreRequest, sessionID string) error {
	_, err := s.takeToken(token, r, sessionID)
	return err
}

// takeToken is VerifyToken; on success it also returns the consumed entry, so
// a download that was refused before it began can give it back (giveBack).
func (s *Service) takeToken(token string, r store.RestoreRequest, sessionID string) (dlToken, error) {
	t := s.table()
	t.mu.Lock()
	if !s.Downloadable(r) && errors.Is(s.notDownloadable(r), ErrExpired) {
		delete(t.m, r.ID)
		t.mu.Unlock()
		return dlToken{}, ErrExpired
	}
	d, ok := t.m[r.ID]
	if !ok || token == "" || sessionID == "" {
		t.mu.Unlock()
		return dlToken{}, ErrToken
	}
	sum := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(sum[:], d.hash[:]) != 1 || d.sessionID != sessionID || s.now().After(d.expires) {
		t.mu.Unlock()
		return dlToken{}, ErrToken
	}
	delete(t.m, r.ID)
	t.mu.Unlock()
	if !s.Downloadable(r) {
		return dlToken{}, s.notDownloadable(r)
	}
	return d, nil
}

// giveBack restores a consumed token whose download never began (every
// stream was in use): the same URL works again until the token's own expiry.
// Any token minted for the restore after it wins - whether it is still in the
// table or was already taken by another download - and the given-back one then
// stays dead, as minting again always invalidates the previous token.
func (s *Service) giveBack(id string, d dlToken) {
	t := s.table()
	t.mu.Lock()
	defer t.mu.Unlock()
	if s.now().After(d.expires) {
		return
	}
	if last, ok := t.lastMint[id]; ok && last.seq != d.seq {
		return // minted again since
	}
	if _, live := t.m[id]; live {
		return
	}
	t.m[id] = d
}

// DownloadWithToken consumes the single-use token (VerifyToken) and streams the
// restore (Download). When the download is refused before it began because
// streams are busy - every stream slot of the maintenance worker in use
// (ErrStreamsBusy) or this restore already streaming (ErrBusy) - the token is
// given back, so a retry of the same URL after Retry-After can still stream,
// once. A download that began (completed or failed) never gives it back.
func (s *Service) DownloadWithToken(ctx context.Context, who Actor, r store.RestoreRequest, token, sessionID string, w io.Writer) (Result, error) {
	d, err := s.takeToken(token, r, sessionID)
	if err != nil {
		return Result{}, err
	}
	res, err := s.Download(ctx, who, r, w)
	if errors.Is(err, ErrStreamsBusy) || errors.Is(err, ErrBusy) {
		s.giveBack(r.ID, d)
	}
	return res, err
}

// FailedError is a download that did not complete. Class is path-free (an
// admin-path class or code, ClassDisconnected, ClassUnavailable, "timeout",
// "internal"); Code is the admin-path code when the worker answered one.
type FailedError struct {
	Class string
	Code  string
}

func (e *FailedError) Error() string { return "restores: download failed: " + e.Class }

// Result is a completed download.
type Result struct {
	Bytes  int64
	SHA256 string
}

// Download streams restore r into w: it unwraps the selection, asks the
// maintenance worker for the zip stream (adminpath Dump) and copies it to w as
// it arrives. Nothing touches the disk. On success the row becomes delivered
// with its size and SHA-256 (recorded only now) and download_count counts it;
// on failure the row keeps its status and records the failure class, the
// failure is logged (class only, path-free) and audited, and a *FailedError is
// returned. One download per restore streams at a time (ErrBusy); when the
// maintenance worker's stream budget is used up the download is refused with
// ErrStreamsBusy before any byte and nothing at all is recorded (not even
// started_at). A stream slower than the minimum rate (MinRateBytes per
// MinRateWindow) is aborted and recorded as ClassTooSlow; when w implements
// Abort() it is called so a blocked write ends at once. A stream with no byte
// within FirstByteTimeout is cancelled and recorded as ClassNoData (nothing
// was sent, so the caller can still answer with a problem). started_at is
// written together with the outcome. The caller must not present a failed
// stream as complete (the admin API aborts the HTTP response) and checks the
// token first (VerifyToken, or DownloadWithToken).
func (s *Service) Download(ctx context.Context, who Actor, r store.RestoreRequest, w io.Writer) (Result, error) {
	if !s.Downloadable(r) {
		return Result{}, s.notDownloadable(r)
	}
	if !s.Master.Loaded() {
		return Result{}, ErrMasterKey
	}
	if !s.begin(r.ID) {
		return Result{}, ErrBusy
	}
	defer s.end(r.ID)
	started := s.now()
	sel, err := s.Master.Unwrap(escrow.ContextRestoreSelection, r.DeviceID+"/"+r.SnapshotID, 1, r.SelectionWrapped)
	if err != nil {
		return Result{}, s.failed(ctx, who, r, started, "selection_unreadable", "")
	}
	all := store.ScopeAll()
	// The guard cancels the stream (the worker's restic run with it) when no
	// byte comes, and also cuts a blocked write when the browser takes too long.
	gctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	ab, _ := w.(aborter)
	guard := newRateGuard(w, s.minRateBytes(), s.minRateWindow(), s.firstByteTimeout(), s.guardClock, func(reason int32) {
		cancel(errStreamGuard)
		if reason == tripTooSlow && ab != nil {
			ab.Abort()
		}
	})
	defer guard.close()
	hw := &hashWriter{w: guard, h: sha256.New()}
	_, derr := s.Backend.Dump(gctx, adminpath.DumpRequest{DeviceRequest: adminpath.DeviceRequest{DeviceID: r.DeviceID, RequestID: r.ID},
		SnapshotID: r.SnapshotID, Path: string(sel)}, hw)
	guard.close()
	switch reason := guard.tripped(); {
	case reason == tripTooSlow:
		// Aborted, even if the last byte raced the guard: the connection was cut.
		return Result{}, s.failed(ctx, who, r, started, ClassTooSlow, "")
	case reason == tripNoData:
		return Result{}, s.failed(ctx, who, r, started, ClassNoData, adminpath.CodeTimeout)
	case derr != nil && hw.n == 0 && errors.Is(derr, &adminpath.Error{Code: adminpath.CodeBusy}):
		s.log().Warn("restore: download refused, every restore stream is in use", logx.ID("restore_id", r.ID), logx.Class("streams_busy"))
		return Result{}, ErrStreamsBusy
	case derr != nil:
		class, code := classify(ctx, derr, hw.failed)
		return Result{}, s.failed(ctx, who, r, started, class, code)
	}
	res := Result{Bytes: hw.n, SHA256: hex.EncodeToString(hw.h.Sum(nil))}
	finished := s.now()
	delivered, none := "delivered", ""
	progress, _ := json.Marshal(struct {
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
	}{res.Bytes, res.SHA256})
	bg := context.WithoutCancel(ctx)
	if _, err := s.Store.UpdateRestoreRequest(bg, all, r.ID, store.RestoreUpdate{Status: &delivered, Bytes: &res.Bytes, Progress: progress,
		ErrorClass: &none, StartedAt: &started, FinishedAt: &finished, CountDownload: true}); err != nil {
		s.log().Error("restore: cannot record the delivery", logx.ID("restore_id", r.ID), logx.Err(err))
	}
	s.log().Info("restore: delivered", logx.ID("restore_id", r.ID), logx.Int("bytes", int(res.Bytes)))
	return res, nil
}

// failed audits, records and logs a download that did not complete. The audit
// row is written first, so whoever sees the class on the restore also finds
// its restore.failed row; a failure to write either is logged as an error.
func (s *Service) failed(ctx context.Context, who Actor, r store.RestoreRequest, started time.Time, class, code string) error {
	bg := context.WithoutCancel(ctx)
	if _, err := s.Store.AppendAudit(bg, store.AuditEvent{ActorType: "system", Method: "system", Action: "restore.failed", ObjectType: "restore", ObjectID: r.ID,
		Result: "error", Severity: "warning", Details: []byte(`{"class":"` + class + `"}`)}); err != nil {
		s.log().Error("restore: cannot audit the failed download", logx.ID("restore_id", r.ID), logx.Class(class), logx.Err(err))
	}
	finished := s.now()
	_, uerr := s.Store.UpdateRestoreRequest(bg, store.ScopeAll(), r.ID, store.RestoreUpdate{ErrorClass: &class, StartedAt: &started, FinishedAt: &finished})
	s.log().Warn("restore: download failed", logx.ID("restore_id", r.ID), logx.Class(class), logx.Bool("recorded", uerr == nil))
	return &FailedError{Class: class, Code: code}
}

// classify turns a Dump failure into a path-free class.
func classify(ctx context.Context, err error, sinkFailed bool) (class, code string) {
	var ae *adminpath.Error
	switch {
	case sinkFailed, errors.Is(err, adminpath.ErrStreamWrite):
		return ClassDisconnected, ""
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled):
		return ClassDisconnected, ""
	case errors.As(err, &ae):
		if ae.Class != "" {
			return string(ae.Class), ae.Code
		}
		return ae.Code, ae.Code
	case errors.Is(err, adminpath.ErrUnavailable):
		return ClassUnavailable, adminpath.CodeUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", adminpath.CodeTimeout
	case errors.Is(err, context.Canceled):
		return ClassDisconnected, ""
	}
	return "internal", adminpath.CodeInternal
}

func (s *Service) begin(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streaming == nil {
		s.streaming = map[string]bool{}
	}
	if s.streaming[id] {
		return false
	}
	s.streaming[id] = true
	return true
}

func (s *Service) end(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.streaming, id)
}

// hashWriter counts and hashes what reached w and remembers a failed write.
type hashWriter struct {
	w      io.Writer
	h      hash.Hash
	n      int64
	failed bool
}

func (hw *hashWriter) Write(p []byte) (int, error) {
	n, err := hw.w.Write(p)
	hw.h.Write(p[:n])
	hw.n += int64(n)
	if err != nil {
		hw.failed = true
	}
	return n, err
}

// ---- purge ----

// legacyRE names the files versions before ADR 0008 staged in the artifacts
// directory: restore-<ULID>.zip and its .part and .raw.part temporaries.
var legacyRE = regexp.MustCompile(`^restore-[0-9A-Z]{26}\.zip(\.part|\.raw\.part)?$`)

// Purge is the restore-purge sweep (every five minutes). It expires spent
// restores - their encrypted selection is cleared, so the path is gone from
// the database - and scrubs what versions before ADR 0008 left on disk: a row
// that still names a staged archive has the file deleted and the reference
// cleared at once, and any restore-<ULID>.zip (or its .part) in the artifacts
// directory is deleted, since nothing writes one any more. A scrubbed file is
// logged as a warning with its count. It returns how many rows it expired plus
// how many files it removed. Safe to run twice and to interrupt.
func (s *Service) Purge(ctx context.Context) (int, error) {
	due, err := s.Store.ListExpiredRestores(ctx, s.now())
	if err != nil {
		return 0, errors.New("restores: cannot list the expired restores")
	}
	n, files := 0, 0
	for _, r := range due {
		u := store.RestoreUpdate{}
		if r.ArtifactRef != "" {
			if !s.removeLegacy(r.ArtifactRef) {
				continue // could not delete: leave the row, the next sweep retries
			}
			files++
			none := ""
			u.ArtifactRef = &none
		}
		spent := !s.now().Before(r.ExpiresAt) || (r.Status != "ready" && r.Status != "delivered")
		if spent {
			u.ClearSelection = true
			if r.Status == "queued" || r.Status == "running" || r.Status == "ready" || r.Status == "delivered" {
				expired := "expired"
				u.Status = &expired
			}
		}
		if _, err := s.Store.UpdateRestoreRequest(ctx, store.ScopeAll(), r.ID, u); err == nil && spent {
			n++
		}
	}
	orphans, err := s.sweepLegacy()
	files += orphans
	if files > 0 {
		s.log().Warn("restore: removed staged restore archives left by an earlier version, ADR 0008", logx.Int("files", files))
	}
	return n + files, err
}

// removeLegacy deletes one staged archive named by a row. A name that is not a
// bare file name inside the artifacts directory is never touched (and counts
// as handled: there is nothing of ours to delete).
func (s *Service) removeLegacy(ref string) bool {
	if s.ArtifactsDir == "" || ref != filepath.Base(ref) || strings.ContainsAny(ref, `/\`) || ref == "." || ref == ".." {
		return true
	}
	err := os.Remove(filepath.Join(s.ArtifactsDir, ref))
	return err == nil || errors.Is(err, os.ErrNotExist)
}

// sweepLegacy deletes every restore-<ULID>.zip (and .part) in the artifacts
// directory.
func (s *Service) sweepLegacy() (int, error) {
	if s.ArtifactsDir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(s.ArtifactsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, errors.New("restores: cannot read the artifacts directory")
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !legacyRE.MatchString(e.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(s.ArtifactsDir, e.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}
