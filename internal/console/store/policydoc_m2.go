package store

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// This file holds the pure pieces of the M2 policy slice (ticket M2-W2A): the
// document tree helpers (flatten, diff, sparse form, provenance marks), the
// composition of default ⊕ policy ⊕ override from section maps, and the impact
// token that binds a blast-radius preview to the change that is then applied.
// Nothing here touches the database.

// ---------------------------------------------------------------------------
// document trees
// ---------------------------------------------------------------------------

// decodeTree decodes one JSON value, keeping numbers as json.Number so a
// round trip never changes their text.
func decodeTree(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the JSON value")
	}
	return v, nil
}

func encodeTree(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// flattenInto records every leaf of a tree under its dotted path. Scalars,
// arrays and empty objects are leaves; a leaf is stored as compact JSON.
func flattenInto(out map[string]string, prefix string, v any) {
	if m, ok := v.(map[string]any); ok && len(m) > 0 {
		for k, c := range m {
			flattenInto(out, joinPath(prefix, k), c)
		}
		return
	}
	b, err := encodeTree(v)
	if err != nil {
		b = json.RawMessage("null")
	}
	out[prefix] = string(b)
}

// FlattenDocument returns the leaves of a policy document (the single-document
// form) as dotted path -> compact JSON.
func FlattenDocument(doc json.RawMessage) (map[string]string, error) {
	t, err := decodeTree(doc)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	flattenInto(out, "", t)
	delete(out, "") // a document that is not an object has no named leaves
	return out, nil
}

// sectionRank orders dotted paths by the document's section order, then by path.
func sectionRank(path string) int {
	sec, _, _ := strings.Cut(path, ".")
	for i, n := range policy.SectionNames {
		if n == sec {
			return i
		}
	}
	return len(policy.SectionNames)
}

func sortPaths(paths []string) {
	sort.SliceStable(paths, func(i, j int) bool {
		ri, rj := sectionRank(paths[i]), sectionRank(paths[j])
		if ri != rj {
			return ri < rj
		}
		return paths[i] < paths[j]
	})
}

// FieldChange is one leaf of a policy document that differs: Old and New are
// compact JSON, "" when the leaf does not exist on that side.
type FieldChange struct {
	Path string `json:"path"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

// DiffDocuments lists the leaves that differ between two documents, in section
// order. The result is empty when they are equal.
func DiffDocuments(oldDoc, newDoc json.RawMessage) ([]FieldChange, error) {
	a, err := FlattenDocument(oldDoc)
	if err != nil {
		return nil, err
	}
	b, err := FlattenDocument(newDoc)
	if err != nil {
		return nil, err
	}
	var paths []string
	seen := map[string]bool{}
	for p, av := range a {
		if bv, ok := b[p]; !ok || bv != av {
			paths = append(paths, p)
			seen[p] = true
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok && !seen[p] {
			paths = append(paths, p)
		}
	}
	sortPaths(paths)
	out := make([]FieldChange, 0, len(paths))
	for _, p := range paths {
		out = append(out, FieldChange{Path: p, Old: a[p], New: b[p]})
	}
	return out, nil
}

// diffTree returns the RFC 7386 merge patch that turns base into target, and
// false when they are equal. Objects diff recursively; a key missing from the
// target becomes null; arrays and scalars replace.
func diffTree(base, target any) (any, bool) {
	bm, bok := base.(map[string]any)
	tm, tok := target.(map[string]any)
	if !bok || !tok {
		if sameTree(base, target) {
			return nil, false
		}
		return target, true
	}
	patch := map[string]any{}
	for k, tv := range tm {
		if bv, ok := bm[k]; ok {
			if d, differs := diffTree(bv, tv); differs {
				patch[k] = d
			}
			continue
		}
		patch[k] = tv
	}
	for k := range bm {
		if _, ok := tm[k]; !ok {
			patch[k] = nil
		}
	}
	if len(patch) == 0 {
		return nil, false
	}
	return patch, true
}

func sameTree(a, b any) bool {
	x, err1 := encodeTree(a)
	y, err2 := encodeTree(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

func canonicalDoc(p policy.Policy) (json.RawMessage, error) {
	b, err := policy.Canonical(p)
	return json.RawMessage(b), err
}

// SparseSections is the stored form of a template or customer policy: for each
// of the nine sections, the merge patch that turns the default policy into
// target ("{}" when the section equals the default). Fields equal to the
// default follow the default; the editor says so. The default policy itself is
// stored in full (PolicySections).
func SparseSections(def, target policy.Policy) (map[string]json.RawMessage, error) {
	dd, err := canonicalDoc(def)
	if err != nil {
		return nil, err
	}
	td, err := canonicalDoc(target)
	if err != nil {
		return nil, err
	}
	dt, err := decodeTree(dd)
	if err != nil {
		return nil, err
	}
	tt, err := decodeTree(td)
	if err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(policy.SectionNames))
	dm, tm := dt.(map[string]any), tt.(map[string]any)
	for _, name := range policy.SectionNames {
		d, differs := diffTree(dm[name], tm[name])
		if !differs {
			out[name] = json.RawMessage("{}")
			continue
		}
		b, err := encodeTree(d)
		if err != nil {
			return nil, err
		}
		out[name] = b
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// composition
// ---------------------------------------------------------------------------

// sectionsPatch is the single-document merge patch a stored section map stands
// for: empty sections are skipped and the two unions are normalised, exactly as
// applySections does when it composes a policy.
func sectionsPatch(sections map[string]json.RawMessage) (json.RawMessage, error) {
	patch := make(map[string]json.RawMessage)
	for name, raw := range sections {
		if isEmptyObject(raw) {
			continue
		}
		patch[name] = normalizeSection(name, raw)
	}
	return json.Marshal(patch)
}

// defaultDoc is the canonical document of the default policy given its stored sections.
func defaultDoc(defSections map[string]json.RawMessage) (json.RawMessage, error) {
	p, err := policy.Decode(defSections)
	if err != nil {
		return nil, err
	}
	return canonicalDoc(p)
}

// composeLayers builds the canonical effective document layer by layer: the
// default policy, then sparse (a customer policy or template; nil for none),
// then an override patch ("" for none). It returns each intermediate document
// so a caller can attribute a field to a layer.
type composedDocs struct {
	Default json.RawMessage
	Policy  json.RawMessage // default ⊕ sparse
	Final   json.RawMessage // default ⊕ sparse ⊕ override, decoded and re-encoded canonically
	Parsed  policy.Policy
}

func composeLayers(defSections, sparse map[string]json.RawMessage, overridePatch string) (composedDocs, error) {
	var out composedDocs
	var err error
	if out.Default, err = defaultDoc(defSections); err != nil {
		return out, err
	}
	out.Policy = out.Default
	if sparse != nil {
		if out.Policy, err = applySections(out.Default, sparse); err != nil {
			return out, invalidf("policy: %v", err)
		}
	}
	doc := out.Policy
	if overridePatch != "" {
		if doc, err = applyOverride(doc, json.RawMessage(overridePatch)); err != nil {
			return out, invalidf("device override: %v", err)
		}
	}
	p, err := policy.DecodeDocument(doc, false)
	if err != nil {
		return out, invalidf("effective policy: %v", err)
	}
	if err := p.Validate(); err != nil {
		return out, &composeError{err}
	}
	out.Parsed = p
	if out.Final, err = canonicalDoc(p); err != nil {
		return out, err
	}
	return out, nil
}

// composeError is a composed policy that failed policy.Validate; it keeps the
// engine's own problem list for the field errors.
type composeError struct{ err error }

func (e *composeError) Error() string { return "effective policy: " + e.err.Error() }
func (e *composeError) Unwrap() error { return ErrInvalid }

// PolicyProblems turns an error from policy.Validate (or one wrapping it) into
// one FieldError per problem, keyed by the document path ("schedule.window").
// It returns nil for any other error.
func PolicyProblems(err error) []FieldError {
	var ce *composeError
	if errors.As(err, &ce) {
		err = ce.err
	}
	var ve *policy.ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	out := make([]FieldError, len(ve.Problems))
	for i, p := range ve.Problems {
		out[i] = FieldError{Field: p.Path, Message: p.Message}
	}
	return out
}

// ComposePolicy returns the policy a document stands for: the default policy's
// stored sections with sparse (nil for none) merged over them. It is the "full
// document" view the editor shows and Validate judges.
func ComposePolicy(defSections, sparse map[string]json.RawMessage) (policy.Policy, error) {
	c, err := composeLayers(defSections, sparse, "")
	return c.Parsed, err
}

// ---------------------------------------------------------------------------
// provenance marks
// ---------------------------------------------------------------------------

// marks says which leaves a patch layer sets. A leaf is set when the patch
// names it, or when the patch creates an object that the layer below did not
// have (a whole new retention.rules, say): everything under it came from the
// patch.
type marks struct {
	exact  map[string]bool
	prefix []string
}

func (m marks) has(path string) bool {
	if m.exact[path] {
		return true
	}
	for _, p := range m.prefix {
		if path == p || strings.HasPrefix(path, p+".") {
			return true
		}
	}
	return false
}

func patchMarks(patch, lower json.RawMessage) marks {
	m := marks{exact: map[string]bool{}}
	pt, err := decodeTree(patch)
	if err != nil {
		return m
	}
	lt, _ := decodeTree(lower)
	pm, ok := pt.(map[string]any)
	if !ok {
		return m
	}
	lm, _ := lt.(map[string]any)
	var walk func(p, l map[string]any, prefix string)
	walk = func(p, l map[string]any, prefix string) {
		for k, v := range p {
			path := joinPath(prefix, k)
			switch c := v.(type) {
			case nil:
				// A null removes the key (RFC 7386): the layer below shows through,
				// so the patch is not the source of anything at this path.
			case map[string]any:
				if lc, ok := l[k].(map[string]any); ok {
					walk(c, lc, path)
				} else {
					m.prefix = append(m.prefix, path)
				}
			default:
				m.exact[path] = true
			}
		}
	}
	walk(pm, lm, "")
	return m
}

// ---------------------------------------------------------------------------
// impact tokens
// ---------------------------------------------------------------------------

// Impact token errors. All are conflicts except a missing token, which is a
// field error on impact_token (the caller skipped the preview).
var (
	// ErrImpactStale: the thing previewed changed (or the token expired) since
	// the preview. The caller previews again.
	ErrImpactStale = fmt.Errorf("%w: the preview is out of date", ErrConflict)
	// ErrImpactMismatch: the token belongs to another change than the one sent.
	ErrImpactMismatch = fmt.Errorf("%w: the change differs from the one previewed", ErrConflict)
)

// impactTTL is how long a preview stays valid.
const impactTTL = 15 * time.Minute

var (
	impactKeyOnce sync.Once
	impactKey     []byte
	impactKeyErr  error
)

func impactSecret() ([]byte, error) {
	impactKeyOnce.Do(func() {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			impactKeyErr = err
			return
		}
		impactKey = k
	})
	return impactKey, impactKeyErr
}

// impactClaims is what a token binds: the kind of change, the object, the
// version it was previewed against, the default policy's version (everything
// composes over it), the population the change reaches (the customers and
// policy versions it was measured on), a digest of the exact proposal, the
// administrator who previewed it and a one-time id. Version, DefaultVersion and
// Population going out of date make the preview stale; a different proposal is a
// mismatch; a used id is refused (a token applies once).
type impactClaims struct {
	Kind           string `json:"k"`
	Object         string `json:"o"`
	Version        int    `json:"v"`
	DefaultVersion int    `json:"d"`
	Population     string `json:"p"`
	Digest         string `json:"h"`
	Admin          string `json:"a"`
	Expires        int64  `json:"e"`
	Nonce          string `json:"n"`
}

// usedImpact remembers the ids of applied tokens until they would have expired,
// so a token cannot be replayed even when the state it describes comes round
// again (assign A, then B, then A). In memory only: the key is per-process too.
var usedImpact = struct {
	sync.Mutex
	m map[string]int64
}{m: map[string]int64{}}

func impactUsed(nonce string) bool {
	usedImpact.Lock()
	defer usedImpact.Unlock()
	_, ok := usedImpact.m[nonce]
	return ok
}

// useImpact records an applied token and drops the expired ones.
func useImpact(c impactClaims, now time.Time) {
	usedImpact.Lock()
	defer usedImpact.Unlock()
	for k, exp := range usedImpact.m {
		if exp < now.UnixMilli() {
			delete(usedImpact.m, k)
		}
	}
	usedImpact.m[c.Nonce] = c.Expires
}

func digestOf(parts ...any) string {
	h := sha256.New()
	for _, p := range parts {
		switch v := p.(type) {
		case string:
			io.WriteString(h, v)
		case json.RawMessage:
			h.Write(v)
		default:
			fmt.Fprint(h, v)
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func mintImpact(c impactClaims, now time.Time) (string, time.Time, error) {
	key, err := impactSecret()
	if err != nil {
		return "", time.Time{}, err
	}
	exp := now.Add(impactTTL)
	c.Expires = exp.UnixMilli()
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", time.Time{}, err
	}
	c.Nonce = hex.EncodeToString(nonce)
	body, err := json.Marshal(c)
	if err != nil {
		return "", time.Time{}, err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(body) + "." + enc.EncodeToString(mac.Sum(nil)), exp, nil
}

// checkImpact verifies tok against the claims the caller computes now from the
// stored state and the proposal it received. A forged or garbled token, another
// object or administrator, or another proposal is ErrImpactMismatch; an expired
// or used token, or one taken against another version of the object, of the
// default policy or of the population, is ErrImpactStale. It does not consume
// the token: the caller calls useImpact once the change is applied.
func checkImpact(tok string, want impactClaims, now time.Time) (impactClaims, error) {
	if strings.TrimSpace(tok) == "" {
		return impactClaims{}, validation([]FieldError{fieldf("impact_token", "Preview the change first: its impact token is required.")})
	}
	key, err := impactSecret()
	if err != nil {
		return impactClaims{}, err
	}
	bodyPart, macPart, ok := strings.Cut(tok, ".")
	if !ok {
		return impactClaims{}, ErrImpactMismatch
	}
	enc := base64.RawURLEncoding
	body, err1 := enc.DecodeString(bodyPart)
	sig, err2 := enc.DecodeString(macPart)
	if err1 != nil || err2 != nil {
		return impactClaims{}, ErrImpactMismatch
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), sig) {
		return impactClaims{}, ErrImpactMismatch
	}
	var got impactClaims
	if json.Unmarshal(body, &got) != nil {
		return impactClaims{}, ErrImpactMismatch
	}
	if got.Kind != want.Kind || got.Object != want.Object || got.Admin != want.Admin {
		return impactClaims{}, ErrImpactMismatch
	}
	if impactUsed(got.Nonce) || now.UnixMilli() > got.Expires || got.Version != want.Version ||
		got.DefaultVersion != want.DefaultVersion || got.Population != want.Population {
		return impactClaims{}, ErrImpactStale
	}
	if got.Digest != want.Digest {
		// Same object, versions and population but another proposal: the sender
		// changed the form after the preview.
		return impactClaims{}, ErrImpactMismatch
	}
	return got, nil
}

// ---------------------------------------------------------------------------
// section hygiene
// ---------------------------------------------------------------------------

// stripNulls removes every null member of a JSON object, recursively. In a
// sparse section a null means "remove the pin" (RFC 7386): the field follows the
// layer below again, so nothing is stored for it. The two unions need no stored
// null either: the composer re-adds the dropped alternative itself.
func stripNulls(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	for k, c := range m {
		if c == nil {
			delete(m, k)
			continue
		}
		m[k] = stripNulls(c)
	}
	return m
}

// checkSectionShape reports every key of the proposed sections that the policy
// document does not have (the typo that would otherwise be stored and ignored).
// Field is the path of the unknown key.
func checkSectionShape(sections map[string]json.RawMessage) ([]FieldError, error) {
	schema, err := overrideSchema()
	if err != nil {
		return nil, err
	}
	var errs []FieldError
	for name, raw := range sections {
		if isEmptyObject(raw) {
			continue
		}
		t, err := decodeTree(raw)
		if err != nil {
			continue // sectionArgs reports malformed JSON
		}
		checkOverrideShape(map[string]any{name: t}, schema, "", "", &errs)
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Field < errs[j].Field })
	return errs, nil
}

// SparseWithPins is SparseSections that keeps the pins the policy already has:
// a stored field whose value the target still carries stays in the sparse form
// even when it equals the default's, so saving a form unchanged does not drop an
// explicit pin. prev is the policy's stored sections.
func SparseWithPins(def, target policy.Policy, prev map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	sp, err := SparseSections(def, target)
	if err != nil {
		return nil, err
	}
	td, err := canonicalDoc(target)
	if err != nil {
		return nil, err
	}
	tt, err := decodeTree(td)
	if err != nil {
		return nil, err
	}
	tm := tt.(map[string]any)
	for _, name := range policy.SectionNames {
		if isEmptyObject(prev[name]) {
			continue
		}
		pt, err := decodeTree(prev[name])
		if err != nil {
			continue
		}
		pm, ok := pt.(map[string]any)
		if !ok {
			continue
		}
		st, err := decodeTree(sp[name])
		if err != nil {
			return nil, err
		}
		sm, ok := st.(map[string]any)
		if !ok {
			sm = map[string]any{}
		}
		secTarget, _ := tm[name].(map[string]any)
		pins := map[string]string{}
		flattenInto(pins, "", stripNulls(pm))
		changed := false
		for path, val := range pins {
			if path == "" {
				continue
			}
			cur, ok := getTreePath(secTarget, path)
			if !ok {
				continue
			}
			if b, err := encodeTree(cur); err != nil || string(b) != val {
				continue
			}
			if _, have := getTreePath(sm, path); have {
				continue
			}
			setTreePath(sm, path, cur)
			changed = true
		}
		if changed {
			b, err := encodeTree(sm)
			if err != nil {
				return nil, err
			}
			sp[name] = b
		}
	}
	return sp, nil
}

func getTreePath(tree map[string]any, path string) (any, bool) {
	var cur any = tree
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func setTreePath(tree map[string]any, path string, v any) {
	segs := strings.Split(path, ".")
	cur := tree
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
	cur[segs[len(segs)-1]] = v
}
