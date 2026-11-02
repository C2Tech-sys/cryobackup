package maintain

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxRcloneConfBytes bounds how much of rclone.conf is scanned.
const maxRcloneConfBytes = 1 << 20

// resolveStorageHost is the host the storage-outage WARN names (Config.StorageHost,
// logs only). An explicit CRYO_MAINTAIN_STORAGE_HOST wins. Otherwise it is the
// endpoint of the rclone remote the worker actually runs against (cfg.Remote in
// the rclone.conf the children read), not the database's default storage target:
// the two can differ (a target row edited without the remote, a second remote),
// and a log line that names the wrong host sends the operator to the wrong DNS
// record. When the remote carries no endpoint the answer is "" and the log says
// "unconfigured", which is honest where a guess is not.
func resolveStorageHost(cfg Config, explicit string) string {
	if explicit != "" {
		return explicit
	}
	remote := cfg.Remote
	if remote == "" {
		remote = DefaultRemote
	}
	path := cfg.RcloneConfig
	if path == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return ""
		}
		path = filepath.Join(dir, "rclone", "rclone.conf")
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	return StorageHostOf(rcloneRemoteEndpoint(io.LimitReader(f, maxRcloneConfBytes), remote))
}

// rcloneRemoteEndpoint returns the `endpoint` value of the [remote] section of
// an rclone.conf. It retains nothing else: the file also holds the storage
// credential, which is skipped line by line and never kept.
func rcloneRemoteEndpoint(r io.Reader, remote string) string {
	var in bool
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || line[0] == '#' || line[0] == ';':
		case line[0] == '[':
			end := strings.IndexByte(line, ']')
			in = end > 0 && strings.TrimSpace(line[1:end]) == remote
		case in:
			k, v, ok := strings.Cut(line, "=")
			if ok && strings.TrimSpace(k) == "endpoint" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}
