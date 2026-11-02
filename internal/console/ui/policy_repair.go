package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// The repair editor (M2-W3A, item 5). A stored policy that does not compose over
// the default used to open the editor on the default policy alone, so a repair
// lost every other setting the policy carried. planRepair instead keeps every
// part of the stored policy that still composes, and names the parts that do
// not: the editor starts from what is kept, flags what was not usable, and the
// review lists exactly what a repair drops.
//
// "Part" is as fine as the document allows: a whole section is tried first, then
// each of its fields, then the fields of a field that is itself an object, so
// one bad retention rule does not cost the other retention rules.

// repairItem is one stored part that cannot be carried over.
type repairItem struct {
	// Path is the dotted document path, "retention.keep_within".
	Path   string
	Reason string
	// Stored is the stored JSON of that part.
	Stored json.RawMessage
}

// repairPlan is what survives of a stored policy and what does not.
type repairPlan struct {
	// Kept is the stored sections minus the unusable parts (sparse, like the stored form).
	Kept map[string]json.RawMessage
	// Base is the default policy with Kept on top: the editor's starting point.
	Base  policy.Policy
	Items []repairItem
}

// defaultSections reads the default policy's stored sections (the built-in
// default when the console has none yet).
func (s *Service) defaultSections(ctx context.Context) (map[string]json.RawMessage, error) {
	defSections, err := policy.Encode(policy.Default(policy.ClassUnknown))
	if err != nil {
		return nil, err
	}
	if d, derr := s.Store.DefaultPolicy(ctx); derr == nil {
		defSections = d.Sections
	} else if !errors.Is(derr, store.ErrNotFound) {
		return nil, derr
	}
	return defSections, nil
}

// defaultSectionsImpl is defaultSections behind a variable so a test can make it fail.
var defaultSectionsImpl = (*Service).defaultSections

// isBroken reports whether a stored (non-default) policy fails to compose over
// the default. It fails closed: when the default policy cannot be read the
// answer is unknown, and the caller must refuse the save (M2-W3E), because
// "not broken" would skip the drop check a repair save depends on.
func (s *Service) isBroken(ctx context.Context, rec store.PolicyRecord) (bool, error) {
	if rec.Scope == "default" || rec.ID == "" {
		return false, nil
	}
	defSections, err := defaultSectionsImpl(s, ctx)
	if err != nil {
		return false, err
	}
	_, err = store.ComposePolicy(defSections, rec.Sections)
	return err != nil, nil
}

// repairReviewImpl is repairReview behind a variable so a test can make it fail.
var repairReviewImpl = (*Service).repairReview

// repairPlanFor returns the repair plan of a stored policy, or nil when the
// policy composes (or is the default policy, which composes on its own).
func (s *Service) repairPlanFor(ctx context.Context, rec store.PolicyRecord) *repairPlan {
	if rec.Scope == "default" || rec.ID == "" {
		return nil
	}
	defSections, err := s.defaultSections(ctx)
	if err != nil {
		return nil
	}
	if _, err := store.ComposePolicy(defSections, rec.Sections); err == nil {
		return nil
	}
	plan, ok := planRepair(defSections, rec.Sections)
	if !ok {
		return nil
	}
	return plan
}

func decodeUseNumber(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func cloneTree(t map[string]any) map[string]any {
	b, err := json.Marshal(t)
	if err != nil {
		return map[string]any{}
	}
	v, err := decodeUseNumber(b)
	m, _ := v.(map[string]any)
	if err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

func setTreePath(t map[string]any, path []string, v any) {
	cur := t
	for _, k := range path[:len(path)-1] {
		next, ok := cur[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[k] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = v
}

func sectionsOfTree(t map[string]any) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(t))
	for k, v := range t {
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		out[k] = b
	}
	return out
}

func pathStrings(path []string) string { return strings.Join(path, ".") }

// planRepair splits stored (the sparse sections of one policy) into the parts
// that compose over defSections and the parts that do not. It reports false
// when the kept parts cannot be composed (which the construction rules out).
func planRepair(defSections, stored map[string]json.RawMessage) (*repairPlan, bool) {
	type leaf struct {
		path []string
		val  any
	}
	tree := map[string]any{}
	compose := func(t map[string]any) (policy.Policy, error) {
		return store.ComposePolicy(defSections, sectionsOfTree(t))
	}
	// try adds v at path to the kept tree when the result still composes.
	try := func(path []string, v any) bool {
		cand := cloneTree(tree)
		setTreePath(cand, path, v)
		if _, err := compose(cand); err != nil {
			return false
		}
		tree = cand
		return true
	}
	var pending []leaf
	var place func(path []string, v any)
	place = func(path []string, v any) {
		if try(path, v) {
			return
		}
		if m, ok := v.(map[string]any); ok && len(m) > 0 {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				place(append(append([]string(nil), path...), k), m[k])
			}
			return
		}
		pending = append(pending, leaf{append([]string(nil), path...), v})
	}

	names := make([]string, 0, len(stored))
	for n := range stored {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if ri, rj := sectionRank(names[i]), sectionRank(names[j]); ri != rj {
			return ri < rj
		}
		return names[i] < names[j]
	})
	var unreadable []repairItem
	for _, name := range names {
		v, err := decodeUseNumber(stored[name])
		if err != nil {
			unreadable = append(unreadable, repairItem{Path: name, Reason: "The stored value is not valid JSON.", Stored: stored[name]})
			continue
		}
		place([]string{name}, v)
	}
	// A part that failed may only have needed a part placed after it: retry until nothing moves.
	for progress := true; progress && len(pending) > 0; {
		progress = false
		var rest []leaf
		for _, l := range pending {
			if try(l.path, l.val) {
				progress = true
			} else {
				rest = append(rest, l)
			}
		}
		pending = rest
	}

	base, err := compose(tree)
	if err != nil {
		return nil, false
	}
	plan := &repairPlan{Kept: sectionsOfTree(tree), Base: base, Items: unreadable}
	for _, l := range pending {
		raw, _ := json.Marshal(l.val)
		plan.Items = append(plan.Items, repairItem{Path: pathStrings(l.path), Reason: reasonFor(compose, tree, l.path, l.val), Stored: raw})
	}
	sort.SliceStable(plan.Items, func(i, j int) bool {
		a, b := plan.Items[i].Path, plan.Items[j].Path
		sa, _, _ := strings.Cut(a, ".")
		sb, _, _ := strings.Cut(b, ".")
		if ra, rb := sectionRank(sa), sectionRank(sb); ra != rb {
			return ra < rb
		}
		return a < b
	})
	if len(plan.Items) == 0 {
		plan.Items = []repairItem{{Path: "", Reason: "These settings do not work together."}}
	}
	return plan, true
}

func sectionRank(name string) int {
	for i, s := range policy.SectionNames {
		if s == name {
			return i
		}
	}
	return len(policy.SectionNames)
}

// reasonFor says, in a fixed sentence, why path cannot be carried: the policy
// engine's own message for that field when it has one.
func reasonFor(compose func(map[string]any) (policy.Policy, error), tree map[string]any, path []string, v any) string {
	generic := "The stored value could not be read as this setting."
	cand := cloneTree(tree)
	setTreePath(cand, path, v)
	_, err := compose(cand)
	if err == nil {
		return generic
	}
	probs := store.PolicyProblems(err)
	p := pathStrings(path)
	for _, pr := range probs {
		if pr.Field == p || strings.HasPrefix(pr.Field, p+".") || strings.HasPrefix(pr.Field, p+"[") || strings.HasPrefix(p, pr.Field+".") {
			return oneLine(pr.Message)
		}
	}
	sec := path[0]
	for _, pr := range probs {
		if pr.Field == sec || strings.HasPrefix(pr.Field, sec+".") {
			return oneLine(pr.Message)
		}
	}
	if len(probs) > 0 {
		return oneLine(probs[0].Message)
	}
	return generic
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

// ---- views ----

// PolRepairItem is one unusable stored part, as the editor lists it.
type PolRepairItem struct {
	Section, Label, Reason, Stored, Anchor string
}

// PolRepair is the editor's account of a policy that did not compose.
type PolRepair struct {
	Items []PolRepairItem
	// Kept is the number of stored sections that were carried in whole.
	Kept int
}

// PolDropped is one stored part a repair does not carry, in the review.
type PolDropped struct {
	Section, Label, Stored, Reason, After string
}

func storedText(path string, raw json.RawMessage) string {
	txt := fmtValue(path, string(raw))
	if r := []rune(txt); len(r) > 240 {
		txt = string(r[:240]) + "…"
	}
	return txt
}

// repairView turns a plan into the editor's list. Each item links to the
// control that shows the default in its place, when there is one.
func repairView(plan *repairPlan) *PolRepair {
	v := &PolRepair{Kept: len(plan.Kept)}
	for _, it := range plan.Items {
		sec, _, _ := strings.Cut(it.Path, ".")
		item := PolRepairItem{Section: sectionTitle(sec), Label: labelOf(it.Path), Reason: it.Reason, Stored: storedText(it.Path, it.Stored), Anchor: "sec-" + sec}
		if k := fieldKeyFor(it.Path); k != "" {
			item.Label, item.Anchor = labelOf(k), fieldID(k)
		}
		if it.Path == "" {
			item.Section, item.Label, item.Anchor = "Policy", "Combination", "pol-identity"
		}
		v.Items = append(v.Items, item)
	}
	return v
}

// flagSections marks, on the drawn sections, every control that shows a default
// in place of an unusable stored value.
func flagSections(secs []PolSectionView, plan *repairPlan) {
	for _, it := range plan.Items {
		sec, _, _ := strings.Cut(it.Path, ".")
		key := fieldKeyFor(it.Path)
		for i := range secs {
			if secs[i].Key != sec {
				continue
			}
			secs[i].Open = true
			secs[i].Flags++
			flagged := false
			mark := func(fs []PolFieldView) {
				for j := range fs {
					if key != "" && fs[j].Path == key {
						fs[j].Flag, flagged = it.Reason, true
					}
				}
			}
			mark(secs[i].Fields)
			mark(secs[i].Adv)
			if !flagged {
				secs[i].FlagNotes = append(secs[i].FlagNotes, labelOf(it.Path)+": "+it.Reason)
			}
		}
	}
}

// droppedFor lists what saving newPol drops from the stored policy: every
// unusable part, with the value the repaired policy has there instead.
func droppedFor(plan *repairPlan, newPol policy.Policy) []PolDropped {
	tree, err := treeOf(newPol)
	if err != nil {
		tree = map[string]any{}
	}
	var out []PolDropped
	for _, it := range plan.Items {
		d := PolDropped{Section: sectionTitle(strings.SplitN(it.Path, ".", 2)[0]), Label: labelOf(it.Path), Stored: storedText(it.Path, it.Stored), Reason: it.Reason}
		if it.Path == "" {
			d.Section, d.Label = "Policy", "Combination"
		} else if cur, ok := getPath(tree, it.Path); ok {
			if b, err := json.Marshal(cur); err == nil {
				d.After = fmtValue(it.Path, string(b))
			}
		}
		if d.After == "" {
			d.After = "the default"
		}
		out = append(out, d)
	}
	return out
}

// repairReview is the review of a repair: what the saved policy drops, and its
// changes measured against what was kept of the stored policy (not against the
// default alone, which would list every kept setting as a change). ok is false
// when the plan or the proposal cannot be composed; the caller then keeps the
// store's own report.
func (s *Service) repairReview(ctx context.Context, rec store.PolicyRecord, prop store.ProposedPolicy) (dropped []PolDropped, changes []PolChangeRow, ok bool) {
	plan := s.repairPlanFor(ctx, rec)
	if plan == nil {
		return nil, nil, false
	}
	defSections, err := s.defaultSections(ctx)
	if err != nil {
		return nil, nil, false
	}
	newPol, err := store.ComposePolicy(defSections, prop.Sections)
	if err != nil {
		return nil, nil, false
	}
	oldDoc, err1 := policy.Canonical(plan.Base)
	newDoc, err2 := policy.Canonical(newPol)
	if err1 != nil || err2 != nil {
		return nil, nil, false
	}
	ch, err := store.DiffDocuments(oldDoc, newDoc)
	if err != nil {
		return nil, nil, false
	}
	return droppedFor(plan, newPol), changeRows(ch), true
}
