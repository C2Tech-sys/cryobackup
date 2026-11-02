package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

var secretValues = []string{secretToken, secretGwPass, secretRepo, secretPend, secretRotate}

func TestSecretNeverPrints(t *testing.T) {
	s := Secret(secretToken)
	for _, got := range []string{
		s.String(), s.GoString(), fmt.Sprint(s), fmt.Sprintf("%v", s), fmt.Sprintf("%+v", s), fmt.Sprintf("%#v", s),
		fmt.Sprintf("%s", s), fmt.Sprintf("%q", s), fmt.Sprintf("%x", s), fmt.Sprintf("%d", s), fmt.Sprintf("%10.3s|", s),
	} {
		if strings.Contains(got, "SECRET") {
			t.Errorf("a formatting verb printed the secret: %q", got)
		}
	}
	if s.String() != "<redacted>" {
		t.Errorf("String() = %q", s.String())
	}
	// JSON is the protocol: the value must be there.
	b, err := json.Marshal(s)
	if err != nil || string(b) != `"`+secretToken+`"` {
		t.Errorf("JSON of a Secret = %s, %v", b, err)
	}
	var back Secret
	if err := json.Unmarshal(b, &back); err != nil || string(back) != secretToken {
		t.Errorf("JSON decode of a Secret = %q, %v", string(back), err)
	}
}

func TestResponsesNeverPrintSecrets(t *testing.T) {
	enroll := okEnrollResponse()
	lobby := okLobbyResponse()
	for name, v := range map[string]any{
		"EnrollResponse":      enroll,
		"*EnrollResponse":     &enroll,
		"PolicyResponse":      okPolicyResponse(),
		"TokenRotateResponse": okTokenRotate(),
		"LobbyResponse":       lobby,
		"*LobbyResponse":      &lobby,
		"Gateway":             enroll.Gateway,
		"GatewaySecret":       *okPolicyResponse().PendingGatewaySecret,
		"slice":               []EnrollResponse{enroll},
		"map":                 map[string]EnrollResponse{"x": enroll},
	} {
		for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
			out := fmt.Sprintf(format, v)
			for _, sec := range secretValues {
				if strings.Contains(out, sec) {
					t.Errorf("%s with %s printed a secret", name, format)
				}
			}
		}
	}
}

// countSecrets counts non-empty Secret values inside v, following pointers,
// structs, slices and maps.
func countSecrets(v reflect.Value) int {
	secretType := reflect.TypeOf(Secret(""))
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return 0
		}
		return countSecrets(v.Elem())
	case reflect.String:
		if v.Type() == secretType && v.Len() > 0 {
			return 1
		}
	case reflect.Struct:
		n := 0
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				n += countSecrets(v.Field(i))
			}
		}
		return n
	case reflect.Slice, reflect.Array:
		n := 0
		for i := 0; i < v.Len(); i++ {
			n += countSecrets(v.Index(i))
		}
		return n
	}
	return 0
}

// typeHoldsSecret reports whether a Secret field is reachable from t.
func typeHoldsSecret(t reflect.Type, seen map[reflect.Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return typeHoldsSecret(t.Elem(), seen)
	case reflect.String:
		return t == reflect.TypeOf(Secret(""))
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).IsExported() && typeHoldsSecret(t.Field(i).Type, seen) {
				return true
			}
		}
	}
	return false
}

func TestRedactedBlanksEverySecret(t *testing.T) {
	enroll := okEnrollResponse()
	policy := okPolicyResponse()
	rotate := okTokenRotate()
	lobby := okLobbyResponse()
	cases := []struct {
		name     string
		in       any
		redacted any
		secrets  int
	}{
		// token, gateway password, repo password, nested pending secret
		{"EnrollResponse", enroll, enroll.Redacted(), 4},
		{"PolicyResponse", policy, policy.Redacted(), 1},
		{"TokenRotateResponse", rotate, rotate.Redacted(), 1},
		{"LobbyResponse", lobby, lobby.Redacted(), 4},
	}
	for _, c := range cases {
		if n := countSecrets(reflect.ValueOf(c.in)); n != c.secrets {
			t.Errorf("%s: fixture holds %d secrets, want %d", c.name, n, c.secrets)
		}
		if n := countSecrets(reflect.ValueOf(c.redacted)); n != 0 {
			t.Errorf("%s: Redacted() still holds %d secrets", c.name, n)
		}
	}
	// Redacted is a copy: it must not blank the original, through a pointer either.
	if enroll.DeviceToken == "" || enroll.Policy.PendingGatewaySecret.Password == "" {
		t.Error("Redacted() modified the original EnrollResponse")
	}
	if policy.PendingGatewaySecret.Password != secretPend {
		t.Error("Redacted() modified the PendingGatewaySecret the original points to")
	}
	if lobby.Enroll.DeviceToken != secretToken {
		t.Error("Redacted() modified the EnrollResponse a LobbyResponse points to")
	}
	// The redacted form is still useful: identifiers survive.
	r := enroll.Redacted()
	if r.DeviceID != enroll.DeviceID || r.Gateway.URL != enroll.Gateway.URL || r.Custody != enroll.Custody {
		t.Error("Redacted() dropped non-secret fields")
	}
	b, _ := json.Marshal(r)
	for _, sec := range secretValues {
		if strings.Contains(string(b), sec) {
			t.Errorf("JSON of a redacted response contains a secret: %s", b)
		}
	}
	// No pending secret and no enroll payload are fine.
	if (PolicyResponse{}).Redacted().PendingGatewaySecret != nil || (LobbyResponse{}).Redacted().Enroll != nil {
		t.Error("Redacted() invented a value")
	}
}

// Every wire type that can hold a Secret must have a Redacted method, so adding
// a Secret field to a new response cannot go unnoticed.
func TestEverySecretBearingTypeHasRedacted(t *testing.T) {
	nested := map[string]bool{"Gateway": true, "GatewaySecret": true} // blanked by their parents
	found := 0
	for _, v := range wireValues() {
		typ := reflect.TypeOf(v)
		if !typeHoldsSecret(typ, map[reflect.Type]bool{}) || nested[typ.Name()] {
			continue
		}
		found++
		m, ok := typ.MethodByName("Redacted")
		if !ok {
			t.Errorf("%s holds a Secret but has no Redacted method", typ.Name())
			continue
		}
		if m.Type.NumIn() != 1 || m.Type.NumOut() != 1 || m.Type.Out(0) != typ {
			t.Errorf("%s.Redacted has the wrong signature: %v", typ.Name(), m.Type)
		}
	}
	if found != 4 {
		t.Errorf("found %d secret-bearing wire types, want 4 (EnrollResponse, PolicyResponse, TokenRotateResponse, LobbyResponse)", found)
	}
}

// A struct that is reached through a pointer field is printed by fmt without
// consulting its field types when the verb does not fit (for example %s of a
// pointer). So every pointee that can reach a Secret needs its own Formatter.
func TestPointeesThatReachSecretsAreFormatters(t *testing.T) {
	formatter := reflect.TypeOf((*fmt.Formatter)(nil)).Elem()
	checked := 0
	var walk func(typ reflect.Type, seen map[reflect.Type]bool)
	walk = func(typ reflect.Type, seen map[reflect.Type]bool) {
		if seen[typ] {
			return
		}
		seen[typ] = true
		switch typ.Kind() {
		case reflect.Pointer:
			if typeHoldsSecret(typ.Elem(), map[reflect.Type]bool{}) {
				checked++
				if !typ.Implements(formatter) {
					t.Errorf("%v reaches a Secret and is held by pointer, but is not a fmt.Formatter", typ)
				}
			}
			walk(typ.Elem(), seen)
		case reflect.Slice, reflect.Array:
			walk(typ.Elem(), seen)
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				if typ.Field(i).IsExported() {
					walk(typ.Field(i).Type, seen)
				}
			}
		}
	}
	for _, v := range wireValues() {
		walk(reflect.TypeOf(v), map[reflect.Type]bool{})
	}
	if checked < 2 {
		t.Errorf("found %d pointer fields that reach a Secret, want at least 2", checked)
	}
}
