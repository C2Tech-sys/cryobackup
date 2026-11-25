//go:build ignore

// htpasswd.go prints one htpasswd line for the P22-S1 spike rig, hashed exactly
// as the console hashes gateway logins (internal/console/gateway.HashPassword:
// bcrypt cost 10, "$2y$" prefix). The login is the only argument; the password
// is read from standard input (first line) so it never appears in argv.
//
// Usage (from the repository root; the build tag keeps it out of ./...):
//
//	go run ./test/lab/p22/htpasswd.go <login> < password-file > htpasswd
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/c2tech-sys/cryobackup/internal/console/gateway"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] == "" {
		fmt.Fprintln(os.Stderr, "usage: htpasswd.go <login> < password-file")
		os.Exit(2)
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(os.Stderr, "no password on standard input")
		os.Exit(2)
	}
	pw := strings.TrimRight(line, "\r\n")
	h, err := gateway.HashPassword(pw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hash failed")
		os.Exit(1)
	}
	fmt.Printf("%s:%s\n", os.Args[1], h)
}
