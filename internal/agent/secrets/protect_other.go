//go:build !windows

package secrets

import (
	"os"

	"github.com/c2tech-sys/cryobackup/internal/agent/config"
)

func newProtector(insecure bool) (protector, error) {
	if !insecure {
		return nil, ErrUnsupported
	}
	return plainProtector{}, nil
}

// Elevated reports whether the process runs as root.
func Elevated() bool { return os.Geteuid() == 0 }

// RestrictToSystemAndAdmins has no meaning off Windows; it reports
// config.ErrACLNotApplied so the layout code treats it as "not applied".
func RestrictToSystemAndAdmins(path string) error { return config.RestrictDir(path) }
