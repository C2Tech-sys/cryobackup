package restic

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestEnvironStripsAndAppends(t *testing.T) {
	base := []string{
		"PATH=C:\\Windows",
		"RESTIC_PASSWORD=stale",
		"restic_repository=stale-lowercase",
		"RESTIC_PASSWORD_FILE=stale",
		"Temp=C:\\old",
		"TMP=C:\\old",
		"GOMAXPROCS=99",
		"=C:=C:\\keep",
		"USERNAME=svc",
		"TEMPORARY=keep-me",
	}
	e := Env{Repository: "rest:https://d:p@gw/d/", Password: "pw", CacheDir: "C:\\cache", TempDir: "C:\\tmp", GOMAXPROCS: 2}
	got, err := e.Environ(base)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PATH=C:\\Windows", "=C:=C:\\keep", "USERNAME=svc", "TEMPORARY=keep-me",
		"RESTIC_REPOSITORY=rest:https://d:p@gw/d/", "RESTIC_PASSWORD=pw", "RESTIC_CACHE_DIR=C:\\cache",
		"TEMP=C:\\tmp", "TMP=C:\\tmp", "GOMAXPROCS=2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Environ =\n%q\nwant\n%q", got, want)
	}
	if len(base) != 10 || base[1] != "RESTIC_PASSWORD=stale" {
		t.Fatal("Environ modified its input")
	}
}

func TestEnvironOptionalFields(t *testing.T) {
	e := Env{Repository: "r", Password: "p", CacheDir: "c"}
	got, err := e.Environ([]string{"TEMP=x", "GOMAXPROCS=3", "A=b"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A=b", "RESTIC_REPOSITORY=r", "RESTIC_PASSWORD=p", "RESTIC_CACHE_DIR=c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

// TestEnvironNoCache is the console's form (ADR 0008): no cache directory is
// required, none is exported, and a stale RESTIC_CACHE_DIR in the base is
// stripped, so restic cannot fall back to a cache on disk.
func TestEnvironNoCache(t *testing.T) {
	e := Env{Repository: "r", Password: "p", NoCache: true, TempDir: "/tmp/cryo-maintain/tmp"}
	got, err := e.Environ([]string{"RESTIC_CACHE_DIR=/var/stale", "A=b"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A=b", "RESTIC_REPOSITORY=r", "RESTIC_PASSWORD=p", "TEMP=/tmp/cryo-maintain/tmp", "TMP=/tmp/cryo-maintain/tmp"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	if NoCacheFlag != "--no-cache" || forbiddenArg(NoCacheFlag) {
		t.Fatalf("NoCacheFlag must be restic's --no-cache and pass Common.Extra: %q", NoCacheFlag)
	}
	if _, err := Snapshots("", Common{Extra: []string{NoCacheFlag}}); err != nil {
		t.Fatalf("a builder refuses NoCacheFlag in Extra: %v", err)
	}
}

func TestEnvironErrors(t *testing.T) {
	ok := Env{Repository: "r", Password: "p", CacheDir: "c"}
	tests := []struct {
		name string
		mut  func(*Env)
	}{
		{"no repository", func(e *Env) { e.Repository = "" }},
		{"no password", func(e *Env) { e.Password = "" }},
		{"no cache dir", func(e *Env) { e.CacheDir = "" }},
		{"no-cache with a cache dir", func(e *Env) { e.NoCache = true }},
		{"negative GOMAXPROCS", func(e *Env) { e.GOMAXPROCS = -1 }},
		{"NUL in password", func(e *Env) { e.Password = "a\x00b" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := ok
			tt.mut(&e)
			out, err := e.Environ(nil)
			if err == nil || out != nil {
				t.Fatalf("got %q, %v; want error", out, err)
			}
			if !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("error %v does not wrap ErrInvalidOption", err)
			}
			if strings.Contains(err.Error(), "a\x00b") {
				t.Fatal("error leaks a value")
			}
		})
	}
}

func TestGatewayRepository(t *testing.T) {
	tests := []struct {
		name     string
		base     string
		id       string
		pw       string
		insecure bool
		want     string
		wantErr  bool
	}{
		{name: "https", base: "https://gw.example.com", id: "dev-01", pw: "abc123", want: "rest:https://dev-01:abc123@gw.example.com/dev-01/"},
		{name: "port", base: "https://gw.example.com:8443", id: "dev-01", pw: "abc123", want: "rest:https://dev-01:abc123@gw.example.com:8443/dev-01/"},
		{name: "single slash tolerated", base: "https://gw.example.com/", id: "d.1_x", pw: "p", want: "rest:https://d.1_x:p@gw.example.com/d.1_x/"},
		{name: "uppercase scheme", base: "HTTPS://gw.example.com", id: "d", pw: "p", want: "rest:https://d:p@gw.example.com/d/"},
		{name: "ipv6 host", base: "https://[2001:db8::1]:8443", id: "d", pw: "p", want: "rest:https://d:p@[2001:db8::1]:8443/d/"},
		{name: "escaped password", base: "https://gw.example.com", id: "d", pw: "p@ss:w/rd#?%", want: "rest:https://d:p%40ss%3Aw%2Frd%23%3F%25@gw.example.com/d/"},
		{name: "http refused", base: "http://gw.example.com", id: "d", pw: "p", wantErr: true},
		{name: "http lab", base: "http://127.0.0.1:8080", id: "d", pw: "p", insecure: true, want: "rest:http://d:p@127.0.0.1:8080/d/"},
		{name: "https with insecure flag", base: "https://gw.example.com", id: "d", pw: "p", insecure: true, want: "rest:https://d:p@gw.example.com/d/"},
		{name: "ftp refused", base: "ftp://gw.example.com", id: "d", pw: "p", insecure: true, wantErr: true},
		{name: "no scheme", base: "gw.example.com", id: "d", pw: "p", wantErr: true},
		{name: "empty", base: "", id: "d", pw: "p", wantErr: true},
		{name: "path", base: "https://gw.example.com/restic", id: "d", pw: "p", wantErr: true},
		{name: "path with slash", base: "https://gw.example.com/restic/", id: "d", pw: "p", wantErr: true},
		{name: "userinfo", base: "https://u:p@gw.example.com", id: "d", pw: "p", wantErr: true},
		{name: "userinfo only name", base: "https://u@gw.example.com", id: "d", pw: "p", wantErr: true},
		{name: "query", base: "https://gw.example.com?x=1", id: "d", pw: "p", wantErr: true},
		{name: "empty query marker", base: "https://gw.example.com?", id: "d", pw: "p", wantErr: true},
		{name: "fragment", base: "https://gw.example.com#frag", id: "d", pw: "p", wantErr: true},
		{name: "no host", base: "https://", id: "d", pw: "p", wantErr: true},
		{name: "bad port", base: "https://gw.example.com:http", id: "d", pw: "p", wantErr: true},
		{name: "device id with slash", base: "https://gw.example.com", id: "a/b", pw: "p", wantErr: true},
		{name: "device id with space", base: "https://gw.example.com", id: "a b", pw: "p", wantErr: true},
		{name: "device id empty", base: "https://gw.example.com", id: "", pw: "p", wantErr: true},
		{name: "device id too long", base: "https://gw.example.com", id: strings.Repeat("a", 65), pw: "p", wantErr: true},
		{name: "device id 64 ok", base: "https://gw.example.com", id: strings.Repeat("a", 64), pw: "p", want: "rest:https://" + strings.Repeat("a", 64) + ":p@gw.example.com/" + strings.Repeat("a", 64) + "/"},
		{name: "device id dots only", base: "https://gw.example.com", id: "..", pw: "p", wantErr: true},
		{name: "device id colon", base: "https://gw.example.com", id: "a:b", pw: "p", wantErr: true},
		{name: "empty password", base: "https://gw.example.com", id: "d", pw: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GatewayRepository(tt.base, tt.id, tt.pw, tt.insecure)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("got %q, want error", got)
				}
				if !errors.Is(err, ErrInvalidOption) {
					t.Fatalf("error %v does not wrap ErrInvalidOption", err)
				}
				if tt.pw != "" && strings.Contains(err.Error(), tt.pw) && len(tt.pw) > 1 {
					t.Fatalf("error leaks the password: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
			if !strings.HasSuffix(got, "/") {
				t.Fatal("trailing slash is mandatory (engine §3.1)")
			}
		})
	}
}

func TestAdminRepository(t *testing.T) {
	tests := []struct {
		name                           string
		remote, bucket, prefix, device string
		want                           string
		wantErr                        bool
	}{
		{name: "with prefix", remote: "admin", bucket: "backups", prefix: "cryo", device: "dev-01", want: "rclone:admin:backups/cryo/dev-01"},
		{name: "without prefix", remote: "admin", bucket: "backups", prefix: "", device: "dev-01", want: "rclone:admin:backups/dev-01"},
		{name: "slashes trimmed", remote: "admin", bucket: "/backups/", prefix: "/a/b/", device: "d", want: "rclone:admin:backups/a/b/d"},
		{name: "prefix only slashes", remote: "admin", bucket: "b", prefix: "//", device: "d", want: "rclone:admin:b/d"},
		{name: "empty remote", remote: "", bucket: "b", device: "d", wantErr: true},
		{name: "colon in remote", remote: "a:b", bucket: "b", device: "d", wantErr: true},
		{name: "slash in remote", remote: "a/b", bucket: "b", device: "d", wantErr: true},
		{name: "empty bucket", remote: "r", bucket: "", device: "d", wantErr: true},
		{name: "bucket only slashes", remote: "r", bucket: "/", device: "d", wantErr: true},
		{name: "dotdot prefix", remote: "r", bucket: "b", prefix: "a/../b", device: "d", wantErr: true},
		{name: "dotdot bucket", remote: "r", bucket: "..", device: "d", wantErr: true},
		{name: "control char", remote: "r", bucket: "b\n", device: "d", wantErr: true},
		{name: "bad device", remote: "r", bucket: "b", device: "a/b", wantErr: true},
		{name: "empty device", remote: "r", bucket: "b", device: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AdminRepository(tt.remote, tt.bucket, tt.prefix, tt.device)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("got %q, want error", got)
				}
				if !errors.Is(err, ErrInvalidOption) {
					t.Fatalf("error %v does not wrap ErrInvalidOption", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestAdminOptions(t *testing.T) {
	tests := []struct {
		program string
		bypass  bool
		want    []string
	}{
		{"rclone", false, []string{"-o", "rclone.program=rclone", "-o", "rclone.args=serve restic --stdio"}},
		{"rclone", true, []string{"-o", "rclone.program=rclone", "-o", "rclone.args=serve restic --stdio --s3-bypass-governance-retention"}},
		{`C:\bin\rclone.exe`, false, []string{"-o", `rclone.program=C:\bin\rclone.exe`, "-o", "rclone.args=serve restic --stdio"}},
	}
	for _, tt := range tests {
		if got := AdminOptions(tt.program, tt.bypass); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("AdminOptions(%q, %v) = %q, want %q", tt.program, tt.bypass, got, tt.want)
		}
	}
}
