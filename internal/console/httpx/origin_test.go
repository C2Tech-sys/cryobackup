package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInProcessOriginIsOffUnlessSet(t *testing.T) {
	if InProcessOrigin(context.Background()) {
		t.Fatal("a bare context reads as in-process")
	}
	ctx := WithInProcessOrigin(context.Background())
	if !InProcessOrigin(ctx) {
		t.Fatal("the marker did not stick")
	}
	// A child context keeps it; a sibling made from the parent does not have it.
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	if !InProcessOrigin(child) {
		t.Error("a derived context lost the marker")
	}
}

// No header, whatever its name or value, can stand in for the marker: a request
// that arrives over the network has a fresh context.
func TestInProcessOriginCannotBeSetByAHeader(t *testing.T) {
	for _, h := range []string{"X-In-Process-Origin", "X-Origin", "X-Cryo-Web", "In-Process-Origin"} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/x", nil)
		r.Header.Set(h, "true")
		if InProcessOrigin(r.Context()) {
			t.Errorf("header %s set the marker", h)
		}
	}
}
