package maintain

import (
	"time"

	"github.com/c2tech-sys/cryobackup/internal/logx"
)

// The attribute keys the worker logs (architecture §8.4 item 2). Every call site
// builds its attributes with the helpers below, which are thin wrappers over the
// typed constructors of internal/logx: a value that is not a short token is
// logged as "invalid", and logx's handler is the path-free safety net behind
// them. There is no worker-local handler or redaction heuristic any more.
const (
	keyDevice  = "device_id"
	keyRequest = "request_id"
	keyKind    = "kind"
	keyMethod  = "method"
	keyExit    = "exit_code"
	keyClass   = "class"
	keyOutcome = "outcome"
	keyCount   = "count"
	keyElapsed = "elapsed_ms"
	// keyStorageHost carries Config.StorageHost (logx.Host) on a run that
	// could not reach the storage.
	keyStorageHost = "storage_host"
)

func attrDevice(id string) logx.Attr  { return logx.ID(keyDevice, id) }
func attrRequest(id string) logx.Attr { return logx.ID(keyRequest, id) }
func attrKind(k string) logx.Attr     { return logx.Kind(k) }
func attrMethod(m string) logx.Attr   { return logx.Str(keyMethod, m) }
func attrExit(code int) logx.Attr     { return logx.Int(keyExit, code) }
func attrClass(c string) logx.Attr    { return logx.Class(c) }
func attrOutcome(o string) logx.Attr  { return logx.Str(keyOutcome, o) }
func attrCount(n int) logx.Attr       { return logx.Count(keyCount, n) }
func attrElapsed(d time.Duration) logx.Attr {
	return logx.Dur(keyElapsed, d)
}
