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
)

// mailFacts is the alert e-mail check (C03, C03b, C18) and the daily digest's facts (C14), read from
// the delivery log, the digest settings and the recipients (store.MailFacts; ADR 0020 decisions 9
// and 10), with the SMTP probe's result carried in. What cannot be read is the facts' NotRead
// ("not checked"), never healthy. NoAlertRecipient is judged only while a relay is configured.
//
// The digest facts keep their NotInBuild marker while no daily-digest job exists (health.NotInBuild,
// CheckDigest): the digest ticket removes both together.
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
	digest.NotInBuild = true // M4: no daily-digest job yet (health.NotInBuild[CheckDigest])
	if h.SMTP.Configured() {
		recipients, err := h.Store.ListRecipients(ctx, store.ScopeAll())
		switch {
		case err != nil:
			if smtp.NotRead == "" {
				smtp.NotRead = notReadRecipients
			}
		default:
			smtp.NoAlertRecipient = true
			for _, r := range recipients {
				if r.Alerts {
					smtp.NoAlertRecipient = false
					break
				}
			}
		}
	}
	return smtp, digest
}
