package jobs

import (
	"context"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/console/store"
	"github.com/c2tech-sys/cryobackup/internal/state"
)

// MailFactsTimeout bounds the store reads behind the mail facts of one evaluation.
const MailFactsTimeout = 5 * time.Second

// Not-read reasons of the alert e-mail check, worded for its "not checked" banner (C03b).
const (
	notReadNoStore    = "the delivery log is not wired"
	notReadRecipients = "the alert recipients could not be read"
	// notReadDigestRecipients is the digest's (C14b): who receives it could not be read.
	notReadDigestRecipients = "the digest recipients could not be read"
)

// mailFacts is the alert e-mail check (C03, C03b, C18) and the daily digest's facts (C14), read from
// the delivery log, the digest settings and the recipients (store.MailFacts; ADR 0020 decisions 9
// and 10), with the SMTP probe's result carried in. What cannot be read is the facts' NotRead
// ("not checked"), never healthy. NoAlertRecipient and the digest's NoRecipient (C20) are judged
// only while a relay is configured; without one the digest facts say MailNotSetUp, which is the
// "not set up" warning C14b rather than the red C14 about a send that could not happen (P20-W2B).
func (h *HealthProber) mailFacts(probe state.ProbeRun) (*state.SMTPFacts, *state.DigestFacts) {
	if h.Store == nil {
		return &state.SMTPFacts{Probe: probe, NotRead: notReadNoStore}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), MailFactsTimeout)
	defer cancel()
	smtp, digest, err := h.Store.MailFacts(ctx, store.ScopeAll(), probe)
	if err != nil { // only a scope error, which ScopeAll cannot be; never leave the check empty
		return &state.SMTPFacts{Probe: probe, NotRead: "the delivery log could not be read"}, nil
	}
	digest.MailNotSetUp = !h.SMTP.Configured()
	if h.SMTP.Configured() {
		recipients, err := h.Store.ListRecipients(ctx, store.ScopeAll())
		switch {
		case err != nil:
			if smtp.NotRead == "" {
				smtp.NotRead = notReadRecipients
			}
			if digest.NotRead == "" {
				digest.NotRead = notReadDigestRecipients
			}
		default:
			smtp.NoAlertRecipient, digest.NoRecipient = true, true
			for _, r := range recipients {
				smtp.NoAlertRecipient = smtp.NoAlertRecipient && !r.Alerts
				digest.NoRecipient = digest.NoRecipient && !r.Digest
			}
		}
	}
	return smtp, digest
}
