package adminapi

import (
	"context"
	"errors"
	"testing"

	"github.com/c2tech-sys/cryobackup/internal/console/mail"
	"github.com/c2tech-sys/cryobackup/internal/console/notify"
)

// P20-W3B G2, H1: a send-now that met an error after some recipients were attempted answers 200 with
// the counts and incomplete true; only an error before anything was attempted is a 500.

type digestDouble struct {
	res notify.DigestResult
	err error
}

func (d digestDouble) PreviewDigest(context.Context) (notify.Content, error) {
	return notify.Content{}, errors.New("not used")
}

func (d digestDouble) SendDigestNow(context.Context, bool) (notify.DigestResult, error) {
	return d.res, d.err
}

func TestM4DigestSendStoppedHalfwayIsIncompleteNot500(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	m := e.withMail(mail.NewFakeSender())
	boom := errors.New("store: busy")
	for _, c := range []struct {
		name         string
		res          notify.DigestResult
		want         int
		sent, failed int
	}{
		{"one sent, then the error", notify.DigestResult{Sent: 1, Recipients: []notify.DigestRecipient{{RuleID: "r1", Outcome: notify.DigestSent}}}, 200, 1, 0},
		{"two failed, then the error", notify.DigestResult{Failed: 2, Recipients: []notify.DigestRecipient{{RuleID: "r1", Outcome: notify.DigestFailed}, {RuleID: "r2", Outcome: notify.DigestFailed}}}, 200, 0, 2},
		{"nothing attempted", notify.DigestResult{}, 500, 0, 0},
	} {
		m.Digest = digestDouble{res: c.res, err: boom}
		b := e.wantStatus("POST", "/api/v1/digest/send", owner, DigestSendRequest{Force: true}, c.want)
		if c.want != 200 {
			continue
		}
		d := decode[DigestSendResponse](t, b)
		if !d.Incomplete || d.Sent != c.sent || d.Failed != c.failed || len(d.Recipients) != len(c.res.Recipients) {
			t.Errorf("%s: %+v", c.name, d)
		}
	}
	// a send that ends without error is not incomplete, and says nothing about it
	m.Digest = digestDouble{res: notify.DigestResult{Sent: 1, Recipients: []notify.DigestRecipient{{RuleID: "r1", Outcome: notify.DigestSent}}}}
	if d := decode[DigestSendResponse](t, e.wantStatus("POST", "/api/v1/digest/send", owner, nil, 200)); d.Incomplete {
		t.Errorf("a complete send reads incomplete: %+v", d)
	}
}
