package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/policy"
)

// plantLeak is the path plantPaths puts into a value.
const plantLeak = `C:\Users\leak\secret.docx`

// plantResult is what plantPaths saw: every place it planted the leak (a
// JSON-like path: "a.b", "list[]" for any element, "m{}" for any map value,
// "m{key}" for a map key), the places where the guard did not catch it, and
// the exemptions it honoured.
type plantResult struct {
	visited map[string]bool
	missed  []string
	exempt  map[string]bool
}

// plantPaths puts plantLeak, one place at a time, into every string that root
// (a pointer) holds: exported struct fields by their JSON names, slice and
// array elements, map values whatever the key type, one added string map key
// per map, and the dynamic value behind an interface or a pointer. After each
// plant it calls guard and restores the value. It skips what AssertPathFree
// exempts (a pathBearing type, restic.Snapshot.Paths) and the values whose
// content other tests own (policy.Policy, time.Time, json.RawMessage).
//
// Map values and interface values are not addressable, so the walk works on a
// copy and writes it back with a commit chain: each level's commit stores its
// copy into its parent and calls the parent's commit.
func plantPaths(root reflect.Value, guard func() error) plantResult {
	p := &planter{guard: guard, res: plantResult{visited: map[string]bool{}, exempt: map[string]bool{}}}
	p.walk(root, "", func() {})
	sort.Strings(p.res.missed)
	return p.res
}

type planter struct {
	guard func() error
	res   plantResult
}

func (p *planter) plant(path string, set func(), unset func()) {
	set()
	caught := p.guard() != nil
	unset()
	p.res.visited[path] = true
	if !caught {
		p.res.missed = append(p.res.missed, path)
	}
}

func (p *planter) walk(rv reflect.Value, path string, commit func()) {
	t := rv.Type()
	switch t {
	case reflect.TypeOf(policy.Policy{}), reflect.TypeOf(time.Time{}), reflect.TypeOf(json.RawMessage(nil)):
		return
	}
	if k := rv.Kind(); k != reflect.Pointer && k != reflect.Interface &&
		(t.Implements(pathBearingType) || reflect.PointerTo(t).Implements(pathBearingType)) {
		p.res.exempt[t.Name()] = true
		return
	}
	switch rv.Kind() {
	case reflect.Pointer:
		if !rv.IsNil() {
			p.walk(rv.Elem(), path, commit)
		}
	case reflect.Interface:
		if rv.IsNil() {
			return
		}
		inner := reflect.New(rv.Elem().Type()).Elem()
		inner.Set(rv.Elem())
		p.walk(inner, path, func() { rv.Set(inner); commit() })
	case reflect.String:
		old := rv.String()
		p.plant(path,
			func() { rv.SetString(plantLeak); commit() },
			func() { rv.SetString(old); commit() })
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, skip := jsonName(f)
			if skip {
				continue
			}
			if t == snapshotType && f.Name == "Paths" {
				p.res.exempt["Snapshot.Paths"] = true
				continue
			}
			p.walk(rv.Field(i), joinPath(path, name), commit)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			p.walk(rv.Index(i), path+"[]", commit)
		}
	case reflect.Map:
		keys := rv.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i].Interface()) < fmt.Sprint(keys[j].Interface()) })
		for _, k := range keys {
			k := k
			val := reflect.New(t.Elem()).Elem()
			val.Set(rv.MapIndex(k))
			p.walk(val, path+"{}", func() { rv.SetMapIndex(k, val); commit() })
		}
		if t.Key().Kind() == reflect.String && len(keys) > 0 {
			lk := reflect.New(t.Key()).Elem()
			lk.SetString(plantLeak)
			first := rv.MapIndex(keys[0])
			p.plant(path+"{key}",
				func() { rv.SetMapIndex(lk, first); commit() },
				func() { rv.SetMapIndex(lk, reflect.Value{}); commit() })
		}
	}
}

// plantProbe holds a string in every shape plantPaths must reach and the guard
// must check. No wire type has a map or an interface field today; the probe
// proves both would be covered the day one is added.
type plantProbe struct {
	Map       map[string]string     `json:"map"`
	MapOfObj  map[string]plantInner `json:"map_of_obj"`
	MapIntKey map[int]string        `json:"map_int_key"`
	List      []string              `json:"list"`
	ListOfObj []plantInner          `json:"list_of_obj"`
	Array     [1]string             `json:"array"`
	Any       any                   `json:"any"`
	AnyObj    any                   `json:"any_obj"`
	AnyPtr    any                   `json:"any_ptr"`
}

type plantInner struct {
	S string `json:"s"`
}

func newPlantProbe() *plantProbe {
	return &plantProbe{
		Map: map[string]string{"k": "v"}, MapOfObj: map[string]plantInner{"k": {S: "v"}}, MapIntKey: map[int]string{7: "v"},
		List: []string{"v"}, ListOfObj: []plantInner{{S: "v"}}, Array: [1]string{"v"},
		Any: "v", AnyObj: plantInner{S: "v"}, AnyPtr: &plantInner{S: "v"},
	}
}

var (
	plantMapPlaces   = []string{"map_int_key{}", "map_of_obj{key}", "map_of_obj{}.s", "map{key}", "map{}"}
	plantSlicePlaces = []string{"list[]", "list_of_obj[].s"}
	plantIfacePlaces = []string{"any", "any_obj.s", "any_ptr.s"}
)

// The walk reaches every shape, and AssertPathFree catches a path in each:
// map values (string and int keys), map keys, slice and array elements, and
// the value behind an interface.
func TestPlantPathsReachesEveryShape(t *testing.T) {
	probe := newPlantProbe()
	res := plantPaths(reflect.ValueOf(probe), func() error { return AssertPathFree(probe) })
	want := append(append(append([]string{"array[]"}, plantMapPlaces...), plantSlicePlaces...), plantIfacePlaces...)
	sort.Strings(want)
	var got []string
	for p := range res.visited {
		got = append(got, p)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("planted at %v\nwant       %v", got, want)
	}
	if len(res.missed) != 0 {
		t.Errorf("AssertPathFree missed a path at %v", res.missed)
	}
	if !reflect.DeepEqual(probe, newPlantProbe()) {
		t.Errorf("plantPaths did not restore the value: %+v", probe)
	}
}

// Mutants of the guard: AssertPathFree run on a copy with every map, every
// slice or every interface emptied behaves like a guard that skips that shape.
// The walk must report exactly that shape's places as missed, so a guard that
// stops walking map values, slice elements or interface fields fails the wire
// test above.
func TestPlantPathsCatchesAGuardThatSkipsAShape(t *testing.T) {
	for _, m := range []struct {
		kind reflect.Kind
		want []string
	}{
		{reflect.Map, plantMapPlaces},
		{reflect.Slice, plantSlicePlaces},
		{reflect.Interface, plantIfacePlaces},
	} {
		probe := newPlantProbe()
		mutant := func() error { return AssertPathFree(without(reflect.ValueOf(probe), m.kind).Interface()) }
		res := plantPaths(reflect.ValueOf(probe), mutant)
		want := append([]string(nil), m.want...)
		sort.Strings(want)
		if !reflect.DeepEqual(res.missed, want) {
			t.Errorf("a guard that skips %s: missed %v, want %v", m.kind, res.missed, want)
		}
	}
}

// without returns a deep copy of v in which every value of the given kind is
// its zero value (a nil map, slice or interface). Struct fields must be
// exported (the probe's are).
func without(v reflect.Value, kind reflect.Kind) reflect.Value {
	out := reflect.New(v.Type()).Elem()
	if v.Kind() == kind {
		return out
	}
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			p := reflect.New(v.Type().Elem())
			p.Elem().Set(without(v.Elem(), kind))
			out.Set(p)
		}
	case reflect.Interface:
		if !v.IsNil() {
			out.Set(without(v.Elem(), kind))
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			out.Field(i).Set(without(v.Field(i), kind))
		}
	case reflect.Slice:
		if !v.IsNil() {
			s := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
			for i := 0; i < v.Len(); i++ {
				s.Index(i).Set(without(v.Index(i), kind))
			}
			out.Set(s)
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(without(v.Index(i), kind))
		}
	case reflect.Map:
		if !v.IsNil() {
			m := reflect.MakeMapWithSize(v.Type(), v.Len())
			iter := v.MapRange()
			for iter.Next() {
				m.SetMapIndex(iter.Key(), without(iter.Value(), kind))
			}
			out.Set(m)
		}
	default:
		out.Set(v)
	}
	return out
}
