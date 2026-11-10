package httpx

import "context"

// originKey is its own key type so no other package's context value can collide
// with the in-process marker below.
type originKey struct{}

// WithInProcessOrigin marks ctx as belonging to a request the console's own web
// pages build and hand to the admin API handler in the same process (the page
// forms that act through the API). Only the page layer should call this.
//
// It is a context value, not a header, on purpose: a header is something a
// network client could send, and a client must not be able to make its audit
// rows read "web". Nothing that arrives over the wire can set a context value.
func WithInProcessOrigin(ctx context.Context) context.Context {
	return context.WithValue(ctx, originKey{}, true)
}

// InProcessOrigin reports whether ctx carries the marker set by
// WithInProcessOrigin, so the admin API can audit the action as "web" instead of
// "api".
func InProcessOrigin(ctx context.Context) bool {
	v, _ := ctx.Value(originKey{}).(bool)
	return v
}
