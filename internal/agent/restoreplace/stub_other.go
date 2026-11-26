//go:build !windows

package restoreplace

// Root is a held staging or side root (Windows only).
type Root struct{ path string }

// Path is the root's path.
func (r *Root) Path() string { return r.path }

// Close does nothing here.
func (r *Root) Close() {}

// Children refuses: device restore is Windows only.
func (r *Root) Children() ([]string, error) { return nil, ErrUnsupported }

// OpenRoot refuses: device restore is Windows only.
func OpenRoot(string, Options) (*Root, error) { return nil, ErrUnsupported }

// VerifyRoot refuses: device restore is Windows only.
func VerifyRoot(string, Options) (*Root, error) { return nil, ErrUnsupported }

// MakeChild refuses: device restore is Windows only.
func (r *Root) MakeChild(...string) (string, error) { return "", ErrUnsupported }

// RemoveChild refuses: device restore is Windows only.
func (r *Root) RemoveChild(string) (int, error) { return 0, ErrUnsupported }

// RemoveReparsePoints refuses: device restore is Windows only.
func RemoveReparsePoints(string) (int, error) { return 0, ErrUnsupported }

// Place refuses: device restore is Windows only.
func Place(string, string, []Rel, Mode) (Counts, error) { return Counts{}, ErrUnsupported }

// ResetInherit refuses: device restore is Windows only.
func ResetInherit(string, Options) (string, int, error) { return "", 0, ErrUnsupported }

// CountSide refuses: device restore is Windows only.
func CountSide(string, []Rel) (Counts, error) { return Counts{}, ErrUnsupported }

// FreeBytes refuses: device restore is Windows only.
func FreeBytes(string) (uint64, error) { return 0, ErrUnsupported }

// WithPrivileges runs f.
func WithPrivileges(f func() error) error { return f() }

// FixedDrives lists nothing here.
func FixedDrives() []string { return nil }
