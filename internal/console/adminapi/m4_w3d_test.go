package adminapi

import (
	"fmt"
	"sync"
	"testing"
)

// P20-W3D: follow-ups from the wave 3 blind checks.

// A recipient edit changes only what the request names, atomically: 30 pairs of a rename and
// switching alerts off, each pair sent at the same moment, all end with both changes.
func TestM4RecipientConcurrentPatchesBothStand(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	for i := 0; i < 30; i++ {
		r := decode[RecipientResponse](t, e.wantStatus("POST", "/api/v1/notification-rules", owner,
			map[string]any{"name": fmt.Sprintf("Old %d", i), "address": fmt.Sprintf("pair%d@example.com", i)}, 201)).Recipient
		name := fmt.Sprintf("New %d", i)
		var wg sync.WaitGroup
		start := make(chan struct{})
		codes := make([]int, 2)
		for j, body := range []map[string]any{{"name": name}, {"alerts": false}} {
			wg.Add(1)
			go func(j int, body map[string]any) {
				defer wg.Done()
				<-start
				res, _ := e.req("PATCH", "/api/v1/notification-rules/"+r.ID, owner, body)
				codes[j] = res.StatusCode
			}(j, body)
		}
		close(start)
		wg.Wait()
		if codes[0] != 200 || codes[1] != 200 {
			t.Fatalf("pair %d: statuses %v", i, codes)
		}
		list := decode[RecipientsResponse](t, e.wantStatus("GET", "/api/v1/notification-rules", owner, nil, 200))
		var got *RecipientJSON
		for k := range list.Recipients {
			if list.Recipients[k].ID == r.ID {
				got = &list.Recipients[k]
			}
		}
		if got == nil || got.Name != name || got.Alerts || !got.Recoveries || !got.Digest {
			t.Fatalf("pair %d lost an edit: %+v (want name %q, alerts off, the rest on)", i, got, name)
		}
	}
}

// An edit that changes nothing still answers the row and writes no audit row.
func TestM4RecipientNoOpPatchAnswersTheRowUnaudited(t *testing.T) {
	e := newEnv(t)
	owner := e.login("admin", pw)
	r := decode[RecipientResponse](t, e.wantStatus("POST", "/api/v1/notification-rules", owner,
		map[string]any{"name": "Ops", "address": "ops@example.com", "digest": false}, 201)).Recipient
	for _, body := range []map[string]any{{}, {"name": "Ops", "digest": false, "alerts": true}} {
		p := decode[RecipientResponse](t, e.wantStatus("PATCH", "/api/v1/notification-rules/"+r.ID, owner, body, 200))
		if p.Recipient.ID != r.ID || p.Recipient.Name != "Ops" || p.Recipient.Digest || !p.Recipient.Alerts || !p.Recipient.UpdatedAt.Equal(r.UpdatedAt) {
			t.Fatalf("no-op patch %v answered %+v", body, p.Recipient)
		}
	}
	if n := e.auditCount("recipient.update", r.ID); n != 0 {
		t.Fatalf("%d recipient.update audit rows for a no-op patch", n)
	}
}
