package api

import (
	"fmt"
	"strconv"
	"strings"
)

// redactedText is what a Secret prints as.
const redactedText = "<redacted>"

// Secret is a string that is a credential: a device token, a gateway password
// or the repository password. It marshals to JSON as the plain string (the
// protocol delivers it once, architecture §5.1, DH13, DH17), but every way of
// printing it (String, %s, %v, %+v, %#v, %q, %x) gives "<redacted>", so a
// response struct that reaches a logger or an error message cannot leak it.
// Convert with string(s) where the value is needed, for example to build the
// restic environment.
//
// Responses that carry secrets also have a Redacted method that returns a copy
// with every Secret blanked.
type Secret string

// String returns "<redacted>", never the value.
func (s Secret) String() string { return redactedText }

// GoString is the %#v form: "<redacted>".
func (s Secret) GoString() string { return redactedText }

// Format implements fmt.Formatter so that no verb prints the value.
func (s Secret) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(redactedText))
}

// Gateway is the storage-gateway login delivered at enrollment (architecture
// §5.1, §7.2). The endpoint never holds a storage credential: this is the
// per-device login to the append-only gateway, not an S3 key.
type Gateway struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password Secret `json:"password"`
}

// redacted returns a copy of g with the password blanked.
func (g Gateway) redacted() Gateway {
	g.Password = ""
	return g
}

// formatSpec rebuilds the format directive that fmt is executing, so a
// Formatter can hand the same verb and flags to a redacted copy of itself.
func formatSpec(f fmt.State, verb rune) string {
	var b strings.Builder
	b.WriteByte('%')
	for _, flag := range "+-# 0" {
		if f.Flag(int(flag)) {
			b.WriteRune(flag)
		}
	}
	if w, ok := f.Width(); ok {
		b.WriteString(strconv.Itoa(w))
	}
	if p, ok := f.Precision(); ok {
		b.WriteByte('.')
		b.WriteString(strconv.Itoa(p))
	}
	b.WriteRune(verb)
	return b.String()
}

// The two response types that are held by pointer in other responses
// (PolicyResponse.PendingGatewaySecret, LobbyResponse.Enroll) implement
// fmt.Formatter and print their redacted copy. Without it, fmt prints a
// pointer field with the wrong verb (for example %s) by dumping the struct
// without consulting the field types, which would show a Secret. A test
// requires a Formatter on every pointee that can reach a Secret.
type (
	plainGatewaySecret  GatewaySecret
	plainEnrollResponse EnrollResponse
)

// Format prints the secret with its password redacted, for every verb.
func (g GatewaySecret) Format(f fmt.State, verb rune) {
	g.Password = ""
	_, _ = fmt.Fprintf(f, formatSpec(f, verb), plainGatewaySecret(g))
}

// Format prints the response with every secret redacted, for every verb.
func (r EnrollResponse) Format(f fmt.State, verb rune) {
	_, _ = fmt.Fprintf(f, formatSpec(f, verb), plainEnrollResponse(r.Redacted()))
}
