package config

import (
	"strings"
	"testing"
)

// P20-T1: smtp.allow_plaintext and the plain-security rules (ADR 0020 decision 1).
func TestSMTPPlaintextRules(t *testing.T) {
	base := func() Config {
		c, err := Load("config.example.json", noEnv)
		if err != nil {
			t.Fatal(err)
		}
		c.SMTP = SMTP{Host: "mail.example.com", Port: 25, Security: "plain", From: "a@example.com"}
		return c
	}
	cases := []struct {
		name string
		mod  func(*Config)
		want string // substring of the problem; empty means valid
	}{
		{"plain without the opt-in", func(c *Config) {}, "smtp.allow_plaintext"},
		{"plain with the opt-in", func(c *Config) { c.SMTP.AllowPlaintext = true }, ""},
		{"plain opted in with a username", func(c *Config) {
			c.SMTP.AllowPlaintext = true
			c.SMTP.Username = "u"
		}, "credentials are never sent in clear"},
		{"plain with a username and no opt-in names both", func(c *Config) { c.SMTP.Username = "u" }, "smtp.username"},
		{"starttls with a username", func(c *Config) {
			c.SMTP.Security = "starttls"
			c.SMTP.Username = "u"
		}, ""},
		{"tls with a username", func(c *Config) {
			c.SMTP.Security = "tls"
			c.SMTP.Username = "u"
		}, ""},
		{"opt-in on starttls is harmless", func(c *Config) {
			c.SMTP.Security = "starttls"
			c.SMTP.AllowPlaintext = true
		}, ""},
		{"no host: the block is not validated", func(c *Config) { c.SMTP = SMTP{Security: "plain", Username: "u"} }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mod(&c)
			err := c.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a problem containing %q, got %v", tc.want, err)
			}
		})
	}
	// The refusal says how to opt in, naming both the key and the variable.
	c := base()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "CRYO_SMTP_ALLOW_PLAINTEXT") {
		t.Fatalf("the opt-in message must name the environment variable: %v", err)
	}
	// Neither the host nor the user name is echoed.
	c.SMTP.Username = "operator-user"
	if err := c.Validate(); err == nil || strings.Contains(err.Error(), "operator-user") || strings.Contains(err.Error(), "mail.example.com") {
		t.Fatalf("a config error must not echo values: %v", err)
	}
}

func TestSMTPAllowPlaintextEnvAndFile(t *testing.T) {
	if got := EnvKeys()["CRYO_SMTP_ALLOW_PLAINTEXT"]; got != "smtp.allow_plaintext" {
		t.Fatalf("CRYO_SMTP_ALLOW_PLAINTEXT maps to %q", got)
	}
	d := Default()
	if d.SMTP.AllowPlaintext {
		t.Fatal("allow_plaintext must default to false")
	}
	env := map[string]string{
		"CRYO_TLS_CERT":             "agent.crt",
		"CRYO_TLS_KEY":              "agent.key",
		"CRYO_SMTP_HOST":            "mail.example.com",
		"CRYO_SMTP_PORT":            "25",
		"CRYO_SMTP_SECURITY":        "plain",
		"CRYO_SMTP_FROM":            "a@example.com",
		"CRYO_SMTP_ALLOW_PLAINTEXT": "true",
	}
	c, err := Read("", func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if !c.SMTP.AllowPlaintext {
		t.Fatal("CRYO_SMTP_ALLOW_PLAINTEXT=true did not set smtp.allow_plaintext")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("plain with the environment opt-in must validate: %v", err)
	}
	delete(env, "CRYO_SMTP_ALLOW_PLAINTEXT")
	c, err = Read("", func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err == nil {
		t.Fatal("plain without the opt-in must be a config error")
	}
	env["CRYO_SMTP_ALLOW_PLAINTEXT"] = "maybe"
	if _, err := Read("", func(k string) string { return env[k] }); err == nil {
		t.Fatal("a non-boolean CRYO_SMTP_ALLOW_PLAINTEXT must be an error")
	}
}

// The compose example carries an smtp block with an empty host (mail not
// configured), and both example files load strictly.
func TestComposeExampleHasAnEmptySMTPBlock(t *testing.T) {
	c, err := Read("../../../deploy/compose/config.example.json", func(string) string { return "" })
	if err != nil {
		t.Fatalf("the compose example must parse strictly: %v", err)
	}
	if c.SMTP.Host != "" || c.SMTP.Port != 587 || c.SMTP.Security != "starttls" || c.SMTP.AllowPlaintext {
		t.Fatalf("compose example smtp block = %+v", c.SMTP)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the compose example must validate: %v", err)
	}
}
