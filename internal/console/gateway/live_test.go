package gateway

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var servingRE = regexp.MustCompile(`Serving restic REST API on \[?(http://[^\]\s]+)`)

// rcloneServe starts `rclone serve restic` on an ephemeral loopback port with
// the given htpasswd file and returns its base URL (ending in "/"). The child
// is killed at cleanup. RCLONE_CONFIG points at an empty temp file for the
// child only.
func rcloneServe(t *testing.T, bin, htpasswd string) string {
	t.Helper()
	remote := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "rclone.conf")
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "serve", "restic",
		"--addr", "127.0.0.1:0", "--htpasswd", htpasswd, "--private-repos", remote)
	cmd.Env = append(os.Environ(), "RCLONE_CONFIG="+cfg)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var log bytes.Buffer
	urlCh := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			mu.Lock()
			log.WriteString(line + "\n")
			mu.Unlock()
			if m := servingRE.FindStringSubmatch(line); m != nil {
				select {
				case urlCh <- m[1]:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		<-done
		if t.Failed() {
			mu.Lock()
			t.Logf("rclone output:\n%s", log.String())
			mu.Unlock()
		}
	})

	select {
	case u := <-urlCh:
		if !strings.HasSuffix(u, "/") {
			u += "/"
		}
		return u
	case <-time.After(30 * time.Second):
		t.Fatal("rclone did not report a listen address within 30s")
		return ""
	}
}

// status requests <base><user>/config as user:password and returns the status.
func status(t *testing.T, base, user, password string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+user+"/config", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(user, password)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("request as %q: %v", user, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// authOK is the C01 rule: 200 or 404 (no repository yet) proves authentication.
func authOK(code int) bool { return code == http.StatusOK || code == http.StatusNotFound }

// TestLiveRclone proves a real rclone accepts the rendered file. It runs only
// when CRYO_RCLONE names an rclone binary.
func TestLiveRclone(t *testing.T) {
	bin := os.Getenv("CRYO_RCLONE")
	if bin == "" {
		t.Skip("CRYO_RCLONE not set")
	}
	dir := t.TempDir()

	devPW, _ := NewPassword(nil)
	devHash, err := HashPassword(devPW)
	if err != nil {
		t.Fatal(err)
	}
	// The raw "$2a$" spelling Go emits, to record whether rclone accepts it.
	rawPW, _ := NewPassword(nil)
	rawHash, err := bcrypt.GenerateFromPassword([]byte(rawPW), bcryptCost)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(rawHash), "$2a$") {
		t.Fatalf("expected a $2a$ hash from x/crypto, got %.4s", rawHash)
	}
	probePW, err := ProbePassword(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	probeHash, _ := HashPassword(probePW)

	const dev, raw, gone = "01TESTDEVICE0000000000000A", "01TESTDEVICE0000000000000B", "01TESTDEVICE0000000000000C"
	gonePW, _ := NewPassword(nil)
	goneHash, _ := HashPassword(gonePW)
	content, err := RenderE([]Login{{dev, devHash}, {raw, string(rawHash)}, {gone, goneHash}}, probeHash, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := SyncIfOwner(dir, content, 1); err != nil {
		t.Fatal(err)
	}
	base := rcloneServe(t, bin, filepath.Join(dir, FileName))

	if c := status(t, base, dev, devPW); !authOK(c) {
		t.Errorf("rendered $2y$ login rejected: %d", c)
	}
	if c := status(t, base, ProbeUsername, probePW); !authOK(c) {
		t.Errorf("probe login rejected: %d", c)
	}
	if c := status(t, base, dev, devPW+"x"); c != http.StatusUnauthorized {
		t.Errorf("wrong password: %d, want 401", c)
	}
	if c := status(t, base, ProbeUsername, devPW); c != http.StatusUnauthorized {
		t.Errorf("probe with a device password: %d, want 401", c)
	}
	if c := status(t, base, "nosuchuser", devPW); c != http.StatusUnauthorized {
		t.Errorf("unknown user: %d, want 401", c)
	}
	// Recorded finding, not a contract: does rclone accept Go's raw $2a$ form?
	rawCode := status(t, base, raw, rawPW)
	t.Logf("rclone with raw $2a$ hash: status %d (accepted=%v)", rawCode, authOK(rawCode))

	// Hot reload (engine sheet 3.1, architecture 7.2): revoke one login, add
	// another, Sync, and expect rclone to follow within 15 s.
	newPW, _ := NewPassword(nil)
	newHash, _ := HashPassword(newPW)
	const added = "01TESTDEVICE0000000000000D"
	next, err := RenderE([]Login{{dev, devHash}, {added, newHash}}, probeHash, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := SyncIfOwner(dir, next, 2); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
		// Finding: rclone on Windows keeps htpasswd open without
		// FILE_SHARE_DELETE, so rename-over is denied there. Production
		// gateways are Linux, where the rename above is the path under test.
		// Here, replace the content in place so the reload behaviour is still
		// exercised.
		t.Logf("windows: rename over rclone's open file denied (%v); writing in place", err)
		if err := os.WriteFile(filepath.Join(dir, FileName), next, fileMode); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		revoked := status(t, base, gone, gonePW) == http.StatusUnauthorized
		appeared := authOK(status(t, base, added, newPW))
		if revoked && appeared {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rclone did not reload within 15s (revoked=%v, added=%v)", revoked, appeared)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if c := status(t, base, dev, devPW); !authOK(c) {
		t.Errorf("surviving login rejected after reload: %d", c)
	}
	if c := status(t, base, ProbeUsername, probePW); !authOK(c) {
		t.Errorf("probe rejected after reload: %d", c)
	}
}
