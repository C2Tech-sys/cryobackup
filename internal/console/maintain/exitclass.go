package maintain

import (
	"errors"
	"strings"
)

// Start-up failures that main.go reports on stderr. They are sentinels so the
// report is chosen by class (classText), never by printing err.Error(): an
// error that wrapped an OS error would carry a path.
var (
	errConfigJSON   = errors.New("the config file is not valid JSON")
	errConfigRead   = errors.New("the config file could not be read")
	errChecksumRead = errors.New("the restic checksum file could not be read")
	errChecksumNone = errors.New("the restic checksum file is empty")
)

// configSettings maps each setting a configuration error can name to the fixed
// hint printed for it. Only these words ever reach stderr, whatever the error
// says; the settings are names, not values.
var configSettings = []struct{ name, hint string }{
	{"BypassGovernance", "BypassGovernance is not supported in v0"},
	{"RcloneProgram", "CRYO_MAINTAIN_RCLONE must not contain whitespace, quotes or backslashes"},
	{"Remote", "CRYO_MAINTAIN_REMOTE must be a bare rclone remote name"},
	{"Bucket", "CRYO_MAINTAIN_BUCKET is required"},
	{"MaxStreams", "CRYO_MAINTAIN_MAX_STREAMS must be at least 1 and below the restic slot count"},
	{"ScratchDir", "CRYO_MAINTAIN_SCRATCH_DIR must be an absolute directory of its own (not a root or top-level directory, owned by the worker, no symlink in its path: use /run/cryo-maintain)"},
	{"ResticSHA256", "the restic SHA-256 must be 64 hex digits"},
	{"SocketPath", "the admin-path socket is required with --loop"},
	{"store", "the worker needs a store"},
	{"key unwrapper", "the worker needs a key unwrapper"},
}

// classText is the one line main prints for a start-up error: a fixed sentence
// chosen by the error's class, with no path, value or underlying OS text.
func classText(err error) string {
	switch {
	case errors.Is(err, errConfigJSON):
		return "maintain: the config file is not valid JSON"
	case errors.Is(err, errConfigRead):
		return "maintain: the config file could not be read"
	case errors.Is(err, errChecksumRead):
		return "maintain: the restic checksum file could not be read"
	case errors.Is(err, errChecksumNone):
		return "maintain: the restic checksum file is empty"
	case errors.Is(err, ErrResticBinary):
		return "maintain: the restic binary was not found or does not match the pinned checksum"
	case errors.Is(err, ErrResticVersion):
		return "maintain: restic is not the pinned version"
	case errors.Is(err, errConfig):
		msg := err.Error()
		for _, s := range configSettings {
			if strings.Contains(msg, s.name) {
				return "maintain: invalid configuration: " + s.hint
			}
		}
		return "maintain: invalid configuration"
	}
	return "maintain: start-up failed (details withheld: they may name a path)"
}
