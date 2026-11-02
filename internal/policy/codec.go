package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// sectionPtrs returns a pointer to each section of p, by section name.
func sectionPtrs(p *Policy) map[string]any {
	return map[string]any{
		"schedule":               &p.Schedule,
		"conditions":             &p.Conditions,
		"selection":              &p.Selection,
		"retention":              &p.Retention,
		"throttle":               &p.Throttle,
		"sla":                    &p.SLA,
		"maintenance":            &p.Maintenance,
		"agent_permissions":      &p.AgentPermissions,
		"notification_overrides": &p.NotificationOverrides,
	}
}

func isNull(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

// unmarshalStrict decodes one JSON value into v, rejecting unknown fields and
// trailing data.
func unmarshalStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the JSON value")
	}
	return nil
}

// Decode builds a Policy from per-section JSON (the form stored in the
// policies table). It is tolerant, for forward compatibility (architecture
// §3.6): unknown sections and unknown fields are ignored, a missing or null
// section takes the ClassUnknown defaults, and a field missing from a present
// section keeps its default. Two unions are exceptions, because a default
// must not leak into an explicit choice: if a selection section names roots or
// presets, the default presets are dropped; if a retention section names a
// preset or rules, the default preset is dropped, and a preset without
// keep_within takes the preset's own keep-within. Decode does not validate.
func Decode(sections map[string]json.RawMessage) (Policy, error) {
	p := Default(ClassUnknown)
	ptrs := sectionPtrs(&p)
	for _, name := range SectionNames {
		raw, ok := sections[name]
		if !ok || isNull(raw) {
			continue
		}
		switch name {
		case "selection":
			if keys := topKeys(raw); keys["roots"] || keys["presets"] {
				p.Selection.Roots, p.Selection.Presets = nil, nil
			}
		case "retention":
			if keys := topKeys(raw); keys["preset"] || keys["rules"] {
				p.Retention = Retention{}
			}
		}
		if err := json.Unmarshal(raw, ptrs[name]); err != nil {
			return Policy{}, fmt.Errorf("policy section %q: %w", name, err)
		}
		if name == "retention" && p.Retention.KeepWithin == "" {
			if pr, ok := RetentionPresets[p.Retention.Preset]; ok {
				p.Retention.KeepWithin = pr.KeepWithin
			}
		}
	}
	return p.normalized(), nil
}

// topKeys returns the set of top-level keys of a JSON object (empty for
// anything else).
func topKeys(raw json.RawMessage) map[string]bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	keys := make(map[string]bool, len(m))
	for k := range m {
		keys[k] = true
	}
	return keys
}

// DecodeStrict is the write-side decoder. Every one of the nine sections must
// be present and non-null, unknown sections and unknown fields are errors, and
// nothing is defaulted: a field missing from a section is its zero value, so
// Validate (which Patch calls) reports whatever is required but absent.
func DecodeStrict(sections map[string]json.RawMessage) (Policy, error) {
	var p Policy
	ptrs := sectionPtrs(&p)
	var problems []string
	for name := range sections {
		if _, ok := ptrs[name]; !ok {
			problems = append(problems, fmt.Sprintf("unknown section %q", name))
		}
	}
	for _, name := range SectionNames {
		raw, ok := sections[name]
		if !ok || isNull(raw) {
			problems = append(problems, fmt.Sprintf("missing section %q", name))
			continue
		}
		if err := unmarshalStrict(raw, ptrs[name]); err != nil {
			problems = append(problems, fmt.Sprintf("section %q: %v", name, err))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return Policy{}, errors.New("policy: " + strings.Join(problems, "; "))
	}
	return p.normalized(), nil
}

// DecodeDocument decodes the single-document form {"schedule":{...},...}. With
// strict it behaves as DecodeStrict (and rejects trailing data), otherwise as
// Decode.
func DecodeDocument(b []byte, strict bool) (Policy, error) {
	var sections map[string]json.RawMessage
	if strict {
		if err := unmarshalStrict(b, &sections); err != nil {
			return Policy{}, fmt.Errorf("policy document: %w", err)
		}
		return DecodeStrict(sections)
	}
	if err := json.Unmarshal(b, &sections); err != nil {
		return Policy{}, fmt.Errorf("policy document: %w", err)
	}
	return Decode(sections)
}

// Encode returns the per-section JSON of p, one entry per section, for
// storage in the policies table. Slices and maps that are nil encode as []
// and {}.
func Encode(p Policy) (map[string]json.RawMessage, error) {
	p = p.normalized()
	out := make(map[string]json.RawMessage, len(SectionNames))
	for name, ptr := range sectionPtrs(&p) {
		b, err := marshal(ptr)
		if err != nil {
			return nil, fmt.Errorf("policy section %q: %w", name, err)
		}
		out[name] = b
	}
	return out, nil
}

// marshal is json.Marshal without HTML escaping and without the trailing
// newline an Encoder adds.
func marshal(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Canonical returns the deterministic encoding of p: the single-document form
// with object keys sorted at every level, no insignificant whitespace, numbers
// in Go's shortest round-trip form and no HTML escaping. Two policies with
// equal content have equal Canonical bytes regardless of how they were built
// or decoded.
func Canonical(p Policy) ([]byte, error) {
	b, err := marshal(p.normalized())
	if err != nil {
		return nil, err
	}
	return canonicalJSON(b)
}

// canonicalJSON re-encodes JSON with sorted keys and compact form, keeping
// every number's text as it is.
func canonicalJSON(b []byte) ([]byte, error) {
	v, err := decodeAny(b)
	if err != nil {
		return nil, err
	}
	return marshal(v)
}

func decodeAny(b []byte) (any, error) {
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

// ETag returns a strong HTTP entity tag for p: the SHA-256 of Canonical(p) in
// hex, in double quotes. It changes if and only if some field changes.
func ETag(p Policy) (string, error) {
	b, err := Canonical(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`, nil
}

// MergePatch applies an RFC 7396 JSON Merge Patch to doc and returns the
// result in canonical form (sorted keys, compact). Objects merge recursively,
// a null member deletes the key, and arrays and scalars replace. A patch that
// is not an object replaces the whole document.
func MergePatch(doc, patch json.RawMessage) (json.RawMessage, error) {
	target, err := decodeAny(doc)
	if err != nil {
		return nil, fmt.Errorf("merge patch: document: %w", err)
	}
	pv, err := decodeAny(patch)
	if err != nil {
		return nil, fmt.Errorf("merge patch: patch: %w", err)
	}
	return marshal(mergeValue(target, pv))
}

func mergeValue(target, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	tm, ok := target.(map[string]any)
	if !ok {
		tm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(tm, k)
			continue
		}
		tm[k] = mergeValue(tm[k], v)
	}
	return tm
}

// Patch applies an RFC 7396 merge patch, given in the single-document form
// (for example {"schedule":{"interval_minutes":720}}), to p and returns the
// validated result. It encodes p, merges, decodes strictly and validates, so
// a patch can neither introduce an unknown key nor leave the policy invalid.
// Because arrays and scalars replace and null deletes, switching retention
// from a preset to explicit rules needs {"retention":{"preset":null,"rules":{...}}}.
func Patch(p Policy, patch json.RawMessage) (Policy, error) {
	sections, err := Encode(p)
	if err != nil {
		return Policy{}, err
	}
	doc, err := marshal(sections)
	if err != nil {
		return Policy{}, err
	}
	merged, err := MergePatch(doc, patch)
	if err != nil {
		return Policy{}, err
	}
	out, err := DecodeDocument(merged, true)
	if err != nil {
		return Policy{}, err
	}
	if err := out.Validate(); err != nil {
		return Policy{}, err
	}
	return out, nil
}
