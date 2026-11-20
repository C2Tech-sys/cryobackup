package store

import (
	"errors"
	"time"

	"github.com/c2tech-sys/cryobackup/internal/state"
)

// addM4Matrix adds the ADR 0020 (M4) methods to the §3.5 scope matrix (matrix() in scope_test.go
// calls it). Episodes and acknowledgements belong to a device of a customer or to the customer
// itself: they run as byID or list cases. Recipients, the delivery log and the digest settings are
// console-wide and run as globalWrite cases (an all-customer caller succeeds, every other one gets
// ErrScope), the way the console-wide settings do.
func addM4Matrix(m map[string]matrixCase) {
	actorOf := func(w *world) AuditActor {
		return AuditActor{AdminID: w.admin.ID, Username: "matrix", Method: "api"}
	}
	// ensureEpisode opens an episode of the given alarm for the device and leaves every other open
	// episode as it was (a sync closes what the input omits).
	ensureEpisode := func(w *world, dev, alarm string) error {
		open, err := w.s.ListOpenEpisodes(w.ctx, ScopeAll())
		if err != nil {
			return err
		}
		keys := []EpisodeKey{{SubjectType: EpisodeDevice, SubjectID: dev, AlarmType: alarm}}
		for _, e := range open {
			keys = append(keys, EpisodeKey{SubjectType: e.SubjectType, SubjectID: e.SubjectID, AlarmType: e.AlarmType})
		}
		_, err = w.s.SyncEpisodes(w.ctx, ScopeAll(), keys, time.Time{})
		return err
	}
	openEpisodeID := func(w *world, dev, alarm string) (string, error) {
		if err := ensureEpisode(w, dev, alarm); err != nil {
			return "", err
		}
		open, err := w.s.ListOpenEpisodes(w.ctx, ScopeAll())
		for _, e := range open {
			if e.SubjectID == dev && e.AlarmType == alarm {
				return e.ID, err
			}
		}
		return "", errors.New("the seeded episode is missing")
	}
	// closedEpisode leaves one closed, alerted episode of the alarm for the device awaiting its recovery notice.
	closedEpisode := func(w *world, dev, alarm string) (string, error) {
		id, err := openEpisodeID(w, dev, alarm)
		if err != nil {
			return "", err
		}
		if _, err := w.s.MarkEpisodesNotified(w.ctx, ScopeAll(), []string{id}, time.Time{}); err != nil {
			return "", err
		}
		open, err := w.s.ListOpenEpisodes(w.ctx, ScopeAll())
		if err != nil {
			return "", err
		}
		var keys []EpisodeKey
		for _, e := range open {
			if e.ID != id {
				keys = append(keys, EpisodeKey{SubjectType: e.SubjectType, SubjectID: e.SubjectID, AlarmType: e.AlarmType})
			}
		}
		_, err = w.s.SyncEpisodes(w.ctx, ScopeAll(), keys, time.Time{})
		return id, err
	}
	customersOf := func(eps []Episode) []string {
		var out []string
		for _, e := range eps {
			out = append(out, e.CustomerID)
		}
		return out
	}

	m["SyncEpisodes"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		r, err := w.s.SyncEpisodes(w.ctx, sc, []EpisodeKey{{SubjectType: EpisodeDevice, SubjectID: t.dev, AlarmType: "FAILED"}}, time.Time{})
		if err == nil && len(r.Skipped) > 0 {
			err = ErrNotFound // P20-T2F item 6: a subject outside scope is skipped, which the matrix reads as "denied" (the fingerprint check proves nothing changed)
		}
		return nil, err
	}}
	m["ListOpenEpisodes"] = matrixCase{kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
		for _, d := range []string{w.devA.ID, w.devB.ID} {
			if err := ensureEpisode(w, d, "FAILED"); err != nil {
				return nil, err
			}
		}
		eps, err := w.s.ListOpenEpisodes(w.ctx, sc)
		return customersOf(eps), err
	}}
	m["ListUnnotifiedEpisodes"] = matrixCase{kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
		for _, d := range []string{w.devA.ID, w.devB.ID} {
			if err := ensureEpisode(w, d, "UNSEEN"); err != nil {
				return nil, err
			}
		}
		eps, err := w.s.ListUnnotifiedEpisodes(w.ctx, sc)
		return customersOf(eps), err
	}}
	m["ListRecoveryDueEpisodes"] = matrixCase{kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
		for _, d := range []string{w.devA.ID, w.devB.ID} {
			if _, err := closedEpisode(w, d, "BLANK"); err != nil {
				return nil, err
			}
		}
		eps, err := w.s.ListRecoveryDueEpisodes(w.ctx, sc)
		return customersOf(eps), err
	}}
	m["MarkEpisodesNotified"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		id, err := openEpisodeID(w, t.dev, "STALE")
		if err != nil {
			return nil, err
		}
		_, err = w.s.MarkEpisodesNotified(w.ctx, sc, []string{id}, time.Time{})
		return nil, err
	}}
	m["MarkRecoveryNotified"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		id, err := closedEpisode(w, t.dev, "STUCK")
		if err != nil {
			return nil, err
		}
		_, err = w.s.MarkRecoveryNotified(w.ctx, sc, []string{id}, time.Time{})
		return nil, err
	}}

	m["CreateAck"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		if err := ensureEpisode(w, t.dev, "FAILED"); err != nil {
			return nil, err
		}
		_, err := w.s.CreateAck(w.ctx, sc, AckInput{SubjectType: EpisodeDevice, SubjectID: t.dev, AlarmType: "failed", Note: "matrix"}, actorOf(w))
		return nil, err
	}}
	m["ClearAck"] = matrixCase{kind: byID, call: func(w *world, sc Scope, t *target) ([]string, error) {
		if err := ensureEpisode(w, t.dev, "STALE"); err != nil {
			return nil, err
		}
		a, err := w.s.CreateAck(w.ctx, ScopeAll(), AckInput{SubjectType: EpisodeDevice, SubjectID: t.dev, AlarmType: "stale"}, actorOf(w))
		if err != nil {
			return nil, err
		}
		_, err = w.s.ClearAck(w.ctx, sc, a.ID, actorOf(w))
		return nil, err
	}}
	m["ListActiveAcks"] = matrixCase{kind: list, call: func(w *world, sc Scope, t *target) ([]string, error) {
		for _, d := range []string{w.devA.ID, w.devB.ID} {
			if err := ensureEpisode(w, d, "UNSEEN"); err != nil {
				return nil, err
			}
			if _, err := w.s.CreateAck(w.ctx, ScopeAll(), AckInput{SubjectType: EpisodeDevice, SubjectID: d, AlarmType: "unseen"}, actorOf(w)); err != nil {
				return nil, err
			}
		}
		acks, err := w.s.ListActiveAcks(w.ctx, sc)
		var out []string
		for _, a := range acks {
			out = append(out, a.CustomerID)
		}
		return out, err
	}}

	// Console-wide: all-customer callers only.
	newRecipient := func(w *world) (Recipient, error) {
		return w.s.CreateRecipient(w.ctx, ScopeAll(), RecipientInput{Name: uniq("Ops"), Address: uniq("ops") + "@example.test", Alerts: true}, actorOf(w))
	}
	m["CreateRecipient"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.CreateRecipient(w.ctx, sc, RecipientInput{Name: uniq("Ops"), Address: uniq("ops") + "@example.test", Alerts: true}, actorOf(w))
		return nil, err
	}}
	m["GetRecipient"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		r, err := newRecipient(w)
		if err != nil {
			return nil, err
		}
		_, err = w.s.GetRecipient(w.ctx, sc, r.ID)
		return nil, err
	}}
	m["ListRecipients"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.ListRecipients(w.ctx, sc)
		return nil, err
	}}
	m["UpdateRecipient"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		r, err := newRecipient(w)
		if err != nil {
			return nil, err
		}
		_, err = w.s.UpdateRecipient(w.ctx, sc, r.ID, RecipientInput{Name: r.Name, Address: r.Address, Alerts: false, Digest: true}, actorOf(w))
		return nil, err
	}}
	m["PatchRecipient"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		r, err := newRecipient(w)
		if err != nil {
			return nil, err
		}
		_, err = w.s.PatchRecipient(w.ctx, sc, r.ID, RecipientPatch{Name: ptr(uniq("Ops"))}, actorOf(w))
		return nil, err
	}}
	m["DeleteRecipient"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		r, err := newRecipient(w)
		if err != nil {
			return nil, err
		}
		return nil, w.s.DeleteRecipient(w.ctx, sc, r.ID, actorOf(w))
	}}

	enqueue := func(w *world, sc Scope) (Delivery, error) {
		d, _, err := w.s.EnqueueDelivery(w.ctx, sc, DeliveryInput{Kind: DeliveryAlert, DedupeKey: uniq("alert:matrix"), SubjectType: "console"})
		return d, err
	}
	m["EnqueueDelivery"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, _, err := w.s.EnqueueDelivery(w.ctx, sc, DeliveryInput{Kind: DeliveryAlert, DedupeKey: uniq("alert:matrix"), SubjectType: "console"})
		return nil, err
	}}
	m["MarkDeliverySent"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		d, err := enqueue(w, ScopeAll())
		if err != nil {
			return nil, err
		}
		_, err = w.s.MarkDeliverySent(w.ctx, sc, d.ID)
		return nil, err
	}}
	m["MarkDeliveryFailed"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		d, err := enqueue(w, ScopeAll())
		if err != nil {
			return nil, err
		}
		_, err = w.s.MarkDeliveryFailed(w.ctx, sc, d.ID, "connect", nil)
		return nil, err
	}}
	m["ListDueRetries"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.ListDueRetries(w.ctx, sc, t0, 10)
		return nil, err
	}}
	m["ListRecentDeliveries"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.ListRecentDeliveries(w.ctx, sc, DeliveryFilter{}, Page{Limit: 10})
		return nil, err
	}}
	m["HasDelivered"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.HasDelivered(w.ctx, sc, "digest:2026-10-22:none")
		return nil, err
	}}
	m["LoadSendFacts"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.LoadSendFacts(w.ctx, sc)
		return nil, err
	}}
	m["MailFacts"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, _, err := w.s.MailFacts(w.ctx, sc, state.ProbeRun{})
		return nil, err
	}}
	m["ReadDigestSettings"] = matrixCase{kind: globalWrite, call: func(w *world, sc Scope, t *target) ([]string, error) {
		_, err := w.s.ReadDigestSettings(w.ctx, sc)
		return nil, err
	}}
}
