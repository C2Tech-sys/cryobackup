package state

import (
	"reflect"
	"testing"
	"time"
)

// setSuccess makes the device's last success d ago, consistently in both the
// LastSuccess fact and the newest backup run.
func setSuccess(f *DeviceFacts, d time.Duration) {
	f.LastSuccess = ago(d)
	f.LastBackup = successRun(d)
}

// partialRun builds a partial run finished 1 h ago.
func partialRun(failed, total int64, counts map[string]int) *RunFacts {
	return &RunFacts{
		Outcome:     "partial",
		StartedAt:   ago(70 * time.Minute),
		FinishedAt:  ago(1 * hour),
		FailedFiles: failed,
		TotalFiles:  total,
		CountsKnown: true,
		ErrorCounts: counts,
	}
}

func deferredRun(finishedAgo time.Duration, reason string) *RunFacts {
	return &RunFacts{
		Outcome:        "deferred",
		StartedAt:      ago(finishedAgo),
		FinishedAt:     ago(finishedAgo),
		DeferredReason: reason,
		CountsKnown:    true,
	}
}

// TestClassifyReference ports the reference classifier's cases A01-A22 (A23 is
// TestA23ExpectedVersion). Names carry the architecture 4.8 case id.
func TestClassifyReference(t *testing.T) {
	runClassifyCases(t, []classifyCase{
		{
			name: "A01_ok_fresh_success_recent_seen",
			want: expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none",
				check: func(t *testing.T, got DeviceState) {
					if len(got.Attrs) != 0 {
						t.Errorf("attrs = %v, want none", got.Attrs)
					}
					if want := t0.Add(-2 * hour); !got.Since.Equal(want) || !got.Detail.LastSuccess.Equal(want) {
						t.Errorf("since = %v, last success = %v, want %v", got.Since, got.Detail.LastSuccess, want)
					}
				}},
		},
		{
			name: "A02_blank_no_success_after_grace",
			mod:  graceExpired,
			want: expectation{primary: StateBlank, flags: []State{StateBlank}, alarm: "critical", reason: ReasonNoSuccessfulRun,
				check: func(t *testing.T, got DeviceState) {
					if want := t0.Add(-7 * day); !got.Since.Equal(want) {
						t.Errorf("since = %v, want first_backup_due_by %v", got.Since, want)
					}
				}},
		},
		{
			// last_success = 0 is NULL; with the grace gone the device is BLANK, and nothing panics.
			name: "A03_blank_zero_timestamp",
			mod:  func(f *DeviceFacts) { graceExpired(f); f.LastSuccess = 0 },
			want: expectation{primary: StateBlank, flags: []State{StateBlank}, alarm: "critical"},
		},
		{
			// A garbled success time is BLANK even inside the grace window: judged on the parsed value.
			name: "A04_blank_negative_timestamp",
			mod:  func(f *DeviceFacts) { newlyEnrolled(f, 1*hour); f.LastSuccess = -5 },
			want: expectation{primary: StateBlank, alarm: "critical", reason: ReasonNoSuccessfulRun,
				check: func(t *testing.T, got DeviceState) {
					if !reflect.DeepEqual(got.Detail.GarbledFields, []string{"last_success"}) {
						t.Errorf("garbled fields = %v", got.Detail.GarbledFields)
					}
				}},
		},
		{
			name: "A04_blank_far_future_garbage_timestamp",
			mod:  func(f *DeviceFacts) { newlyEnrolled(f, 1*hour); f.LastSuccess = 9_999_999_999_999_999 },
			want: expectation{primary: StateBlank, alarm: "critical"},
		},
		{
			name: "A04_blank_small_positive_garbage_timestamp",
			mod:  func(f *DeviceFacts) { f.LastSuccess = 1 },
			want: expectation{primary: StateBlank, alarm: "critical"},
		},
		{
			// 1.7e9 is a seconds-epoch value (2023), not milliseconds: out of range, so BLANK.
			name: "A05_blank_seconds_epoch_instead_of_ms",
			mod:  func(f *DeviceFacts) { f.LastSuccess = 1_700_000_000 },
			want: expectation{primary: StateBlank, alarm: "critical"},
		},
		{
			name: "A06_stuck_open_run_over_max_runtime",
			mod: func(f *DeviceFacts) {
				f.OpenRun = &RunFacts{StartedAt: ago(76 * hour)}
				f.ProgressAdvancedAt = ago(1 * time.Minute) // "100 % done" is not health
			},
			want: expectation{primary: StateStuck, flags: []State{StateStuck}, alarm: "critical",
				check: func(t *testing.T, got DeviceState) {
					if got.Detail.OpenRunAge != 76*hour || got.Detail.OpenRunLimit != 12*hour {
						t.Errorf("open run age/limit = %v/%v", got.Detail.OpenRunAge, got.Detail.OpenRunLimit)
					}
					if want := t0.Add(-76*hour + 12*hour); !got.Since.Equal(want) {
						t.Errorf("since = %v, want start + limit %v", got.Since, want)
					}
				}},
		},
		{
			name: "A07_open_run_under_threshold_not_stuck",
			mod:  func(f *DeviceFacts) { f.OpenRun = &RunFacts{StartedAt: ago(1 * hour)} },
			want: expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none"},
		},
		{
			name: "A08_failed_last_run_failed",
			mod: func(f *DeviceFacts) {
				f.LastSuccess = ago(5 * day)
				f.LastBackup = failedRun(1*hour, "network")
				f.ConsecutiveFailures = 1
				f.LastAttemptStart = ago(65 * time.Minute)
			},
			want: expectation{primary: StateFailed, flags: []State{StateFailed, StateStale}, alarm: "critical",
				check: func(t *testing.T, got DeviceState) {
					if got.Detail.ErrorClass != "network" || got.Detail.ConsecutiveFailures != 1 {
						t.Errorf("detail = %+v", got.Detail)
					}
					if want := t0.Add(-1 * hour); !got.Since.Equal(want) {
						t.Errorf("since = %v, want %v", got.Since, want)
					}
				}},
		},
		{
			name: "A09_blank_outranks_failed",
			mod: func(f *DeviceFacts) {
				graceExpired(f)
				f.LastBackup = failedRun(1*hour, "network")
				f.ConsecutiveFailures = 3
			},
			want: expectation{primary: StateBlank, flags: []State{StateBlank, StateFailed}, alarm: "critical", reason: ReasonOnlyFailures},
		},
		{
			name: "A10_unseen_over_threshold",
			mod:  func(f *DeviceFacts) { f.LastSeen = ago(200 * hour) },
			want: expectation{primary: StateUnseen, flags: []State{StateUnseen}, alarm: "critical", noAttrs: []Attr{AttrOffline},
				check: func(t *testing.T, got DeviceState) {
					if want := t0.Add(-200*hour + 168*hour); !got.Since.Equal(want) {
						t.Errorf("since = %v, want last_seen + limit %v", got.Since, want)
					}
				}},
		},
		{
			name: "A11_unseen_never_seen_uses_enrolled_at",
			mod: func(f *DeviceFacts) {
				f.LastSeen = 0
				f.EnrolledAt = ago(200 * hour)
				f.EnrollConfirmedAt = ago(199 * hour)
				f.FirstBackupDueBy = ago(128 * hour)
				f.Escrow = &EscrowFacts{Custody: "escrowed", CreatedAt: ago(200 * hour), VerifiedAt: ago(1 * day)}
				f.LastSuccess = ago(199 * hour)
				f.Thresholds.StaleAfter = 1000 * hour
			},
			want: expectation{primary: StateUnseen, flags: []State{StateUnseen}, alarm: "critical"},
		},
		{
			name: "A12_unseen_unparseable_last_seen",
			mod:  func(f *DeviceFacts) { f.LastSeen = -1 },
			want: expectation{primary: StateUnseen, flags: []State{StateUnseen}, alarm: "critical",
				check: func(t *testing.T, got DeviceState) {
					if !reflect.DeepEqual(got.Detail.GarbledFields, []string{"last_seen"}) {
						t.Errorf("garbled fields = %v", got.Detail.GarbledFields)
					}
				}},
		},
		{
			name: "A12_unseen_seconds_epoch_last_seen",
			mod:  func(f *DeviceFacts) { f.LastSeen = 1_700_000_000 },
			want: expectation{primary: StateUnseen, alarm: "critical"},
		},
		{
			name: "A13_unseen_boundary_is_strict",
			mod:  func(f *DeviceFacts) { f.LastSeen = ago(168 * hour) },
			want: expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none", attrs: []Attr{AttrOffline}},
		},
		{
			name: "A13_unseen_one_ms_past_boundary",
			mod:  func(f *DeviceFacts) { f.LastSeen = ms(t0.Add(-168*hour - time.Millisecond)) },
			want: expectation{primary: StateUnseen, flags: []State{StateUnseen}, alarm: "critical"},
		},
		{
			name: "A14_stale_over_threshold",
			mod:  func(f *DeviceFacts) { setSuccess(f, 80*hour) },
			want: expectation{primary: StateStale, flags: []State{StateStale}, alarm: "critical",
				check: func(t *testing.T, got DeviceState) {
					if want := t0.Add(-80*hour + 72*hour); !got.Since.Equal(want) {
						t.Errorf("since = %v, want last_success + limit %v", got.Since, want)
					}
				}},
		},
		{
			// no schedule (Interval 0) so MISSED cannot mask the boundary under test.
			name: "A15_stale_boundary_is_strict",
			mod:  func(f *DeviceFacts) { f.Thresholds.Interval = 0; setSuccess(f, 72*hour) },
			want: expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none", attrs: []Attr{AttrNoSchedule}},
		},
		{
			name: "A15_stale_one_ms_past_boundary",
			mod: func(f *DeviceFacts) {
				f.Thresholds.Interval = 0
				f.LastSuccess = ms(t0.Add(-72*hour - time.Millisecond))
				f.LastBackup = successRun(72 * hour)
			},
			want: expectation{primary: StateStale, flags: []State{StateStale}, alarm: "critical"},
		},
		{
			name: "A16_stale_never_applied_to_blank",
			mod:  graceExpired,
			want: expectation{primary: StateBlank, flags: []State{StateBlank}, alarm: "critical"},
		},
		{
			name: "A17_offline_but_recent_is_ok",
			mod:  func(f *DeviceFacts) { f.LastSeen = ago(10 * time.Minute) },
			want: expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none", exact: []Attr{AttrOffline}},
		},
		{
			name: "A18_outdated_compares_version_tuples",
			mod:  func(f *DeviceFacts) { f.AgentVersion, f.ExpectedVersion = "2.5.9", "2.5.10" },
			want: expectation{primary: StateOutdated, flags: []State{StateOutdated}, alarm: "none",
				check: func(t *testing.T, got DeviceState) {
					if got.Detail.AgentVersion != "2.5.9" || got.Detail.ExpectedVersion != "2.5.10" {
						t.Errorf("versions = %q/%q", got.Detail.AgentVersion, got.Detail.ExpectedVersion)
					}
				}},
		},
		{
			name: "A19_outdated_garbled_version_sorts_low",
			mod:  func(f *DeviceFacts) { f.AgentVersion, f.ExpectedVersion = "x.y", "2.5.10" },
			want: expectation{primary: StateOutdated, flags: []State{StateOutdated}, alarm: "none"},
		},
		{
			name: "A20_outdated_no_expected_version_never_fires",
			mod:  func(f *DeviceFacts) { f.AgentVersion, f.ExpectedVersion = "0.0.1", "" },
			want: expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none"},
		},
		{
			name: "A21_multiflag_primary_unseen",
			mod: func(f *DeviceFacts) {
				setSuccess(f, 100*hour)
				f.LastSeen = ago(200 * hour)
				f.AgentVersion = "1.4.0"
			},
			want: expectation{primary: StateUnseen, flags: []State{StateUnseen, StateStale, StateOutdated}, alarm: "critical"},
		},
		{
			name: "A22_multiflag_primary_stuck",
			mod: func(f *DeviceFacts) {
				f.OpenRun = &RunFacts{StartedAt: ago(76 * hour)}
				f.LastSuccess = ago(5 * day)
				f.LastBackup = failedRun(80*hour, "network")
				f.ConsecutiveFailures = 2
			},
			want: expectation{primary: StateStuck, flags: []State{StateStuck, StateFailed, StateStale}, alarm: "critical"},
		},
	})
}

func TestA23ExpectedVersion(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"A23_expected_version_derived_by_tuple_max", []string{"2.5.9", "2.5.10", "2.5.2"}, "2.5.10"},
		{"A23_expected_version_order_independent", []string{"2.5.2", "2.5.10", "2.5.9"}, "2.5.10"},
		{"A23_expected_version_empty_input", nil, ""},
		{"A23_expected_version_only_empty_and_garbled", []string{"", "x.y", "dev"}, ""},
		{"A23_expected_version_garbled_ignored", []string{"1.0.0", "garbage", "1.0.1"}, "1.0.1"},
		{"A23_expected_version_equal_tuples_deterministic", []string{"2.5.0", "2.5"}, "2.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExpectedVersion(c.in); got != c.want {
				t.Errorf("ExpectedVersion(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestClassifyMatrix covers the architecture 4.8 B cases (B16 is the schema
// test and does not live here).
func TestClassifyMatrix(t *testing.T) {
	notAlarming := func(t *testing.T, got DeviceState) {
		t.Helper()
		if got.Alarm != SeverityNone {
			t.Errorf("alarm = %s, want none", got.Alarm)
		}
	}
	runClassifyCases(t, []classifyCase{
		{
			name: "B01_pending_first_within_grace_no_alarm",
			mod:  func(f *DeviceFacts) { newlyEnrolled(f, 1*hour) },
			want: expectation{primary: StatePendingFirst, flags: []State{StatePendingFirst}, alarm: "none", reason: ReasonWithinGrace,
				check: func(t *testing.T, got DeviceState) {
					if want := t0.Add(-1 * hour); !got.Since.Equal(want) {
						t.Errorf("since = %v, want enrolled_at %v", got.Since, want)
					}
					if want := t0.Add(71 * hour); !got.Detail.GraceDeadline.Equal(want) {
						t.Errorf("grace deadline = %v, want %v", got.Detail.GraceDeadline, want)
					}
				}},
		},
		{
			name: "B02_pending_first_boundary_now_equals_deadline",
			mod:  func(f *DeviceFacts) { newlyEnrolled(f, 1*hour); f.FirstBackupDueBy = ms(t0) },
			want: expectation{primary: StatePendingFirst, alarm: "none"},
		},
		{
			name: "B02_pending_first_boundary_plus_1ms_is_blank",
			mod:  func(f *DeviceFacts) { newlyEnrolled(f, 1*hour); f.FirstBackupDueBy = ms(t0) - 1 },
			want: expectation{primary: StateBlank, alarm: "critical"},
		},
		{
			name: "B03_pending_first_expired_is_blank",
			mod:  func(f *DeviceFacts) { newlyEnrolled(f, 80*hour); f.FirstBackupDueBy = ago(1 * hour) },
			want: expectation{primary: StateBlank, flags: []State{StateBlank}, alarm: "critical", reason: ReasonNoSuccessfulRun},
		},
		{
			name: "B03_pending_first_deadline_derived_from_enrolled_at_plus_grace",
			mod:  func(f *DeviceFacts) { newlyEnrolled(f, 80*hour); f.FirstBackupDueBy = 0 },
			want: expectation{primary: StateBlank, alarm: "critical"},
		},
		{
			name: "B04_enrolled_never_reported_reason",
			mod:  func(f *DeviceFacts) { graceExpired(f); f.EnrollConfirmedAt = 0 },
			want: expectation{primary: StateBlank, alarm: "critical", reason: ReasonNeverReported},
		},
		{
			name: "B05_pending_first_hard_failure_breaks_out",
			mod: func(f *DeviceFacts) {
				newlyEnrolled(f, 2*hour)
				f.LastBackup = failedRun(30*time.Minute, "wrong_password")
				f.ConsecutiveFailures = 1
			},
			want: expectation{primary: StateBlank, flags: []State{StateBlank, StateFailed}, alarm: "critical", reason: ReasonOnlyFailures},
		},
		{
			name: "B06_pending_first_transient_failure_stays_pending",
			mod: func(f *DeviceFacts) {
				newlyEnrolled(f, 5*hour)
				f.LastBackup = failedRun(30*time.Minute, "network")
				f.ConsecutiveFailures = 2
			},
			want: expectation{primary: StatePendingFirst, flags: []State{StatePendingFirst}, alarm: "none",
				check: func(t *testing.T, got DeviceState) {
					if got.Detail.ConsecutiveFailures != 2 || got.Detail.ErrorClass != "network" {
						t.Errorf("attempts/class not shown: %+v", got.Detail)
					}
				}},
		},
		{
			name: "B07_pending_first_seed_progress_extends_grace",
			mod: func(f *DeviceFacts) {
				newlyEnrolled(f, 82*hour) // deadline passed 10 h ago
				f.Thresholds.SeedExtension = 24 * hour
				f.OpenRun = &RunFacts{Initial: true, StartedAt: ago(30 * hour)}
				f.ProgressAdvancedAt = ago(1 * hour)
			},
			want: expectation{primary: StatePendingFirst, flags: []State{StatePendingFirst}, alarm: "none", reason: ReasonSeedExtended},
		},
		{
			name: "B08_pending_first_seed_stalled_no_extension",
			mod: func(f *DeviceFacts) {
				newlyEnrolled(f, 82*hour)
				f.Thresholds.SeedExtension = 24 * hour
				f.OpenRun = &RunFacts{Initial: true, StartedAt: ago(8 * hour)}
				f.ProgressAdvancedAt = ago(6 * hour)
			},
			want: expectation{primary: StateBlank, flags: []State{StateBlank}, alarm: "critical"},
		},
		{
			name: "B09_adopted_device_gets_fresh_grace",
			mod: func(f *DeviceFacts) {
				newlyEnrolled(f, 1*hour)
				f.LastSuccess = ago(40 * day) // from the previous hardware
				f.LastBackup = successRun(40 * day)
			},
			want: expectation{primary: StatePendingFirst, flags: []State{StatePendingFirst}, alarm: "none", reason: ReasonWithinGrace,
				check: func(t *testing.T, got DeviceState) {
					if !got.Detail.LastSuccess.IsZero() {
						t.Errorf("a success from before re-enrollment counted: %v", got.Detail.LastSuccess)
					}
				}},
		},
		{
			name: "B10_paused_timed_suppresses_stale",
			mod: func(f *DeviceFacts) {
				f.PausedAt, f.PausedUntil, f.PauseReason = ago(1*hour), ahead(2*hour), "office closed"
				setSuccess(f, 10*day)
			},
			want: expectation{primary: StatePaused, flags: []State{StatePaused}, alarm: "none", noAttrs: []Attr{AttrPausedIndefinite},
				check: func(t *testing.T, got DeviceState) {
					if !got.Detail.PauseUntil.Equal(t0.Add(2*hour)) || got.Detail.PauseReason != "office closed" {
						t.Errorf("pause detail = %v %q", got.Detail.PauseUntil, got.Detail.PauseReason)
					}
					if !got.Since.Equal(t0.Add(-1 * hour)) {
						t.Errorf("since = %v, want paused_at", got.Since)
					}
				}},
		},
		{
			name: "B11_paused_expired_pause_ignored",
			mod: func(f *DeviceFacts) {
				f.PausedAt, f.PausedUntil = ago(3*day), ago(1*hour)
				setSuccess(f, 10*day)
			},
			want: expectation{primary: StateStale, flags: []State{StateStale}, alarm: "critical"},
		},
		{
			name: "B11_paused_expiry_unparseable_is_ignored",
			mod: func(f *DeviceFacts) {
				f.PausedAt, f.PausedUntil = ago(3*day), -7
				setSuccess(f, 10*day)
			},
			want: expectation{primary: StateStale, alarm: "critical"},
		},
		{
			name: "B12_paused_indefinite",
			mod: func(f *DeviceFacts) {
				f.PausedAt, f.PausedUntil = ago(3*day), 0
				setSuccess(f, 10*day)
			},
			want: expectation{primary: StatePaused, flags: []State{StatePaused}, alarm: "none", attrs: []Attr{AttrPausedIndefinite}},
		},
		{
			name: "B13_paused_does_not_suppress_escrow",
			mod: func(f *DeviceFacts) {
				f.PausedAt, f.PausedUntil = ago(1*hour), 0
				f.Escrow = &EscrowFacts{Custody: "escrowed", CreatedAt: ago(30 * hour)}
			},
			want: expectation{primary: StatePaused, flags: []State{StatePaused, StateEscrowUnverified}, alarm: "alarm"},
		},
		{
			name: "B13_paused_does_not_suppress_identity_or_maintenance",
			mod: func(f *DeviceFacts) {
				f.PausedAt, f.PausedUntil = ago(1*hour), 0
				f.IdentityConflictAt = ago(1 * hour)
				f.Maintenance.LastPrune = failedRun(1*hour, "storage")
			},
			want: expectation{primary: StatePaused, flags: []State{StatePaused, StateIdentityConflict, StateMaintFailed}, alarm: "critical"},
		},
		{
			name: "B14_retired_never_alarms",
			mod: func(f *DeviceFacts) {
				f.RetiredAt = ago(10 * day)
				setSuccess(f, 100*day)
				f.LastSeen = ago(100 * day)
			},
			want: expectation{primary: StateRetired, flags: []State{StateRetired}, alarm: "none"},
		},
		{
			name: "B14_retired_beats_incomplete_facts",
			mod:  func(f *DeviceFacts) { f.RetiredAt = ago(10 * day); f.Complete = false },
			want: expectation{primary: StateRetired, flags: []State{StateRetired}, alarm: "none"},
		},
		{
			name: "B17_partial_under_tolerance_is_success",
			mod: func(f *DeviceFacts) {
				f.LastSuccess = ago(30 * hour)
				f.LastBackup = partialRun(3, 90000, map[string]int{"permission": 3})
				f.LastAttemptStart = ago(65 * time.Minute)
			},
			want: expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none", attrs: []Attr{AttrPartialUnderTolerance},
				check: func(t *testing.T, got DeviceState) {
					if want := t0.Add(-1 * hour); !got.Detail.LastSuccess.Equal(want) {
						t.Errorf("effective last success = %v, want the partial's finish %v", got.Detail.LastSuccess, want)
					}
				}},
		},
		{
			name: "B17_partial_under_tolerance_clears_blank",
			mod: func(f *DeviceFacts) {
				graceExpired(f)
				f.LastBackup = partialRun(2, 5000, map[string]int{"locked": 2})
			},
			want: expectation{primary: StateOK, alarm: "none", attrs: []Attr{AttrPartialUnderTolerance}},
		},
		{
			name: "B18_partial_over_tolerance_pct",
			mod: func(f *DeviceFacts) {
				f.LastSuccess = ago(30 * hour)
				f.LastBackup = partialRun(1000, 99000, map[string]int{"permission": 1000}) // 1 % > 0.5 %
				f.LastAttemptStart = ago(65 * time.Minute)
			},
			want: expectation{primary: StatePartial, flags: []State{StatePartial}, alarm: "alarm", noAttrs: []Attr{AttrPartialUnderTolerance},
				check: func(t *testing.T, got DeviceState) {
					if got.Detail.FailedFiles != 1000 || got.Detail.TotalFiles != 99000 ||
						!reflect.DeepEqual(got.Detail.ErrorCounts, []ClassCount{{"permission", 1000}}) {
						t.Errorf("detail = %+v", got.Detail)
					}
				}},
		},
		{
			name: "B19_partial_over_tolerance_count",
			mod: func(f *DeviceFacts) {
				f.LastBackup = partialRun(25, 1000, map[string]int{"permission": 25}) // 25 > 20 and 2.4 % > 0.5 %
			},
			want: expectation{primary: StatePartial, flags: []State{StatePartial}, alarm: "alarm"},
		},
		{
			name: "B19_partial_count_rule_alone_passes",
			mod: func(f *DeviceFacts) {
				f.LastBackup = partialRun(15, 100, map[string]int{"permission": 15}) // 15 <= 20 though 13 % > 0.5 %
			},
			want: expectation{primary: StateOK, alarm: "none", attrs: []Attr{AttrPartialUnderTolerance}},
		},
		{
			name: "B19_partial_pct_rule_alone_passes",
			mod: func(f *DeviceFacts) {
				f.LastBackup = partialRun(100, 100000, map[string]int{"locked": 100}) // 100 > 20 though 0.1 % <= 0.5 %
			},
			want: expectation{primary: StateOK, alarm: "none", attrs: []Attr{AttrPartialUnderTolerance}},
		},
		{
			name: "B20_partial_missing_source_root_always_over",
			mod: func(f *DeviceFacts) {
				f.LastBackup = partialRun(1, 90000, map[string]int{"missing_source": 1})
			},
			want: expectation{primary: StatePartial, flags: []State{StatePartial}, alarm: "alarm"},
		},
		{
			name: "B20_partial_missing_source_in_error_class_field",
			mod: func(f *DeviceFacts) {
				f.LastBackup = partialRun(1, 90000, map[string]int{"permission": 1})
				f.LastBackup.ErrorClass = "missing_source"
			},
			want: expectation{primary: StatePartial, alarm: "alarm"},
		},
		{
			name: "B20_partial_unexpected_error_class_is_over",
			mod: func(f *DeviceFacts) {
				f.LastBackup = partialRun(1, 90000, map[string]int{"disk": 1})
			},
			want: expectation{primary: StatePartial, alarm: "alarm"},
		},
		{
			name: "B21_partial_without_error_counts_fails_loud",
			mod: func(f *DeviceFacts) {
				f.LastBackup = partialRun(0, 0, nil)
				f.LastBackup.CountsKnown = false
			},
			want: expectation{primary: StatePartial, flags: []State{StatePartial}, alarm: "alarm"},
		},
		{
			name: "B21_partial_failed_files_without_any_class_fails_loud",
			mod:  func(f *DeviceFacts) { f.LastBackup = partialRun(3, 90000, nil) },
			want: expectation{primary: StatePartial, alarm: "alarm"},
		},
		{
			name: "B22_partial_vss_fallback_flag_only",
			mod: func(f *DeviceFacts) {
				f.LastBackup = partialRun(3, 90000, map[string]int{"permission": 3})
				f.LastBackup.VSSFallback = []string{"D", "C"}
			},
			want: expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none",
				exact: []Attr{AttrPartialUnderTolerance, AttrVSSFallback},
				check: func(t *testing.T, got DeviceState) {
					if !reflect.DeepEqual(got.Detail.VSSVolumes, []string{"C", "D"}) {
						t.Errorf("vss volumes = %v, want sorted [C D]", got.Detail.VSSVolumes)
					}
				}},
		},
		{
			// 3.4: a VSS fallback on a volume that held a configured root is always over tolerance.
			name: "B22_partial_vss_fallback_on_configured_root_is_over",
			mod: func(f *DeviceFacts) {
				f.LastBackup = partialRun(3, 90000, map[string]int{"permission": 3})
				f.LastBackup.VSSFallback = []string{"C"}
				f.LastBackup.VSSFallbackOnRoot = true
			},
			want: expectation{primary: StatePartial, alarm: "alarm", attrs: []Attr{AttrVSSFallback}, noAttrs: []Attr{AttrPartialUnderTolerance}},
		},
		{
			name: "B23_deferred_gateway_within_sla_warning",
			mod: func(f *DeviceFacts) {
				f.LastSuccess = ago(20 * hour)
				f.LastBackup = deferredRun(3*hour, "gateway")
				f.ServiceState = "deferred"
				f.LastAttemptStart = ago(3 * hour)
			},
			want: expectation{primary: StateDeferred, flags: []State{StateDeferred}, alarm: "none", reason: ReasonDeferredGateway,
				check: func(t *testing.T, got DeviceState) {
					if !got.Since.Equal(t0.Add(-3 * hour)) {
						t.Errorf("since = %v, want the deferred run's start", got.Since)
					}
				}},
		},
		{
			name: "B23_deferred_unknown_reason_from_newer_agent",
			mod:  func(f *DeviceFacts) { f.LastBackup = deferredRun(1*hour, "solar_flare") },
			want: expectation{primary: StateDeferred, alarm: "none", reason: ReasonDeferredUnknown},
		},
		{
			// ADR 0005: a fleet-cap deferral is DEFERRED with its own reason, no alarm,
			// and does not touch the failure streak.
			name: "B23_deferred_fleet_cap_within_sla_no_alarm",
			mod: func(f *DeviceFacts) {
				f.LastSuccess = ago(20 * hour)
				f.LastBackup = deferredRun(10*time.Minute, "fleet_cap")
				f.ServiceState = "deferred"
				f.LastAttemptStart = ago(10 * time.Minute)
				f.ConsecutiveFailures = 0
			},
			want: expectation{primary: StateDeferred, flags: []State{StateDeferred}, alarm: "none", reason: ReasonDeferredFleetCap,
				check: func(t *testing.T, got DeviceState) {
					if hasState(got.Flags, StateFailed) || got.Detail.ConsecutiveFailures != 0 {
						t.Errorf("a fleet-cap deferral counted as a failure: %+v flags %v", got.Detail, got.Flags)
					}
				}},
		},
		{
			// A cap set too low: the deferrals go on past the SLA and the device turns STALE.
			name: "B24_deferred_fleet_cap_beyond_sla_becomes_stale",
			mod: func(f *DeviceFacts) {
				f.LastSuccess = ago(5 * day)
				f.LastBackup = deferredRun(5*time.Minute, "fleet_cap")
				f.ServiceState = "deferred"
			},
			want: expectation{primary: StateStale, flags: []State{StateStale}, alarm: "critical"},
		},
		{
			name: "B24_deferred_beyond_sla_becomes_stale",
			mod: func(f *DeviceFacts) {
				f.LastSuccess = ago(5 * day)
				f.LastBackup = deferredRun(5*day, "gateway")
				f.ServiceState = "deferred"
			},
			want: expectation{primary: StateStale, flags: []State{StateStale}, alarm: "critical"},
		},
		{
			name: "B25_deferred_lock_exit11_not_failure",
			mod: func(f *DeviceFacts) {
				f.LastBackup = deferredRun(30*time.Minute, "lock")
				f.ConsecutiveFailures = 1 // unchanged by the deferral
			},
			want: expectation{primary: StateDeferred, flags: []State{StateDeferred}, alarm: "none", reason: ReasonDeferredLock,
				check: func(t *testing.T, got DeviceState) {
					if got.Detail.ConsecutiveFailures != 1 || hasState(got.Flags, StateFailed) {
						t.Errorf("deferral counted as a failure: %+v flags %v", got.Detail, got.Flags)
					}
				}},
		},
		{
			name: "B27_abandoned_counts_as_failure",
			mod: func(f *DeviceFacts) {
				f.LastBackup = &RunFacts{Outcome: "abandoned", StartedAt: ago(2 * hour), FinishedAt: ago(1 * hour), CountsKnown: true}
				f.ConsecutiveFailures = 1
			},
			want: expectation{primary: StateFailed, flags: []State{StateFailed}, alarm: "critical"},
		},
		{
			name: "B28_consecutive_failure_threshold_one_failure_below",
			mod: func(f *DeviceFacts) {
				f.Thresholds.ConsecutiveFailuresToAlarm = 2
				f.LastBackup = failedRun(1*hour, "network")
				f.ConsecutiveFailures = 1
			},
			want: expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none"},
		},
		{
			name: "B28_consecutive_failure_threshold_two_failures_alarm",
			mod: func(f *DeviceFacts) {
				f.Thresholds.ConsecutiveFailuresToAlarm = 2
				f.LastBackup = failedRun(1*hour, "network")
				f.ConsecutiveFailures = 2
			},
			want: expectation{primary: StateFailed, flags: []State{StateFailed}, alarm: "critical"},
		},
		{
			name: "B29_failed_cleared_by_next_success",
			mod:  func(f *DeviceFacts) { setSuccess(f, 30*time.Minute); f.ConsecutiveFailures = 0 },
			want: expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none"},
		},
		{
			name: "B29_failed_run_older_than_last_success_does_not_alarm",
			mod: func(f *DeviceFacts) {
				f.LastBackup = failedRun(3*hour, "network") // the loader's "newest run" lags the success
				f.ConsecutiveFailures = 1
			},
			want: expectation{primary: StateOK, alarm: "none"},
		},
		{
			name: "B30_maintenance_failed_alarm",
			mod:  func(f *DeviceFacts) { f.Maintenance.LastPrune = failedRun(1*hour, "storage") },
			want: expectation{primary: StateMaintFailed, flags: []State{StateMaintFailed}, alarm: "alarm", reason: ReasonMaintFailed,
				check: func(t *testing.T, got DeviceState) {
					if got.Detail.MaintOp != "prune" || !got.Since.Equal(t0.Add(-1*hour)) {
						t.Errorf("maint op %q since %v", got.Detail.MaintOp, got.Since)
					}
				}},
		},
		{
			name: "B30_maintenance_guard_refused_is_failure",
			mod:  func(f *DeviceFacts) { f.Maintenance.LastForget = failedRun(1*hour, "guard_refused") },
			want: expectation{primary: StateMaintFailed, alarm: "alarm"},
		},
		{
			name: "B30_maintenance_check_with_errors_is_failure",
			mod: func(f *DeviceFacts) {
				f.Maintenance.LastCheck = successRun(1 * hour)
				f.Maintenance.LastCheck.ErrorCounts = map[string]int{"storage": 2}
			},
			want: expectation{primary: StateMaintFailed, alarm: "alarm", check: func(t *testing.T, got DeviceState) {
				if got.Detail.MaintOp != "check" {
					t.Errorf("maint op = %q", got.Detail.MaintOp)
				}
			}},
		},
		{
			name: "B31_maintenance_lock_deferral_not_failure",
			mod: func(f *DeviceFacts) {
				f.Maintenance.LastPrune = deferredRun(1*hour, "lock")
				f.Maintenance.DeferredCount = 3
			},
			want: expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none"},
		},
		{
			name: "B32_maintenance_starved_after_n_deferrals",
			mod: func(f *DeviceFacts) {
				f.Maintenance.LastPrune = deferredRun(1*hour, "lock")
				f.Maintenance.DeferredCount = 10
			},
			want: expectation{primary: StateMaintFailed, flags: []State{StateMaintFailed}, alarm: "alarm", reason: ReasonMaintStarved,
				check: func(t *testing.T, got DeviceState) {
					if got.Detail.MaintOp != "starved" {
						t.Errorf("maint op = %q", got.Detail.MaintOp)
					}
				}},
		},
		{
			name: "B32_maintenance_no_forget_within_two_cadences",
			mod: func(f *DeviceFacts) {
				f.Maintenance.LastForgetSuccessAt = ago(15 * day) // cadence 7 d: limit 14 d
			},
			want: expectation{primary: StateMaintFailed, alarm: "alarm", reason: ReasonMaintForgetOverdue},
		},
		{
			name: "B33_maintenance_cleared_by_success",
			mod: func(f *DeviceFacts) {
				// the newest prune is a success: the earlier failure no longer shows
				f.Maintenance.LastPrune = successRun(1 * hour)
				f.Maintenance.DeferredCount = 0
			},
			want: expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none"},
		},
		{
			name: "B34_escrow_unverified_after_grace",
			mod: func(f *DeviceFacts) {
				f.Escrow = &EscrowFacts{Custody: "escrowed", CreatedAt: ago(30 * hour)}
			},
			want: expectation{primary: StateEscrowUnverified, flags: []State{StateEscrowUnverified}, alarm: "alarm", reason: ReasonEscrowNeverVerified,
				check: func(t *testing.T, got DeviceState) {
					if want := t0.Add(-30*hour + 24*hour); !got.Since.Equal(want) {
						t.Errorf("since = %v, want created + grace %v", got.Since, want)
					}
				}},
		},
		{
			name: "B34_escrow_missing_row_is_unverified",
			mod:  func(f *DeviceFacts) { f.Escrow = nil },
			want: expectation{primary: StateEscrowUnverified, alarm: "alarm", reason: ReasonEscrowMissing},
		},
		{
			name: "B34_escrow_last_verify_failed",
			mod:  func(f *DeviceFacts) { f.Escrow.LastVerifyErrorClass = "storage" },
			want: expectation{primary: StateEscrowUnverified, alarm: "alarm", reason: ReasonEscrowVerifyFailed},
		},
		{
			name: "B35_escrow_within_grace_ok",
			mod: func(f *DeviceFacts) {
				f.Escrow = &EscrowFacts{Custody: "escrowed", CreatedAt: ago(2 * hour)}
			},
			want: expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none"},
		},
		{
			name: "B36_escrow_customer_held_no_alarm",
			mod:  func(f *DeviceFacts) { f.CustomerKeyCustody = "customer_held"; f.Escrow = nil },
			want: expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none", attrs: []Attr{AttrCustomerHeldKey}},
		},
		{
			name: "B36_escrow_customer_held_row",
			mod: func(f *DeviceFacts) {
				f.Escrow = &EscrowFacts{Custody: "customer_held", CreatedAt: ago(30 * day)}
			},
			want: expectation{primary: StateOK, alarm: "none", attrs: []Attr{AttrCustomerHeldKey}},
		},
		{
			name: "B37_escrow_recheck_overdue",
			mod: func(f *DeviceFacts) {
				f.Escrow = &EscrowFacts{Custody: "escrowed", CreatedAt: ago(90 * day), VerifiedAt: ago(40 * day)}
			},
			want: expectation{primary: StateEscrowUnverified, flags: []State{StateEscrowUnverified}, alarm: "alarm", reason: ReasonEscrowRecheckOverdue},
		},
		{
			name: "B38_identity_conflict_outranks_pending",
			mod:  func(f *DeviceFacts) { newlyEnrolled(f, 1*hour); f.IdentityConflictAt = ago(30 * time.Minute) },
			want: expectation{primary: StateIdentityConflict, flags: []State{StateIdentityConflict, StatePendingFirst}, alarm: "critical"},
		},
		{
			name: "B39_clock_skew_does_not_change_state",
			mod:  func(f *DeviceFacts) { f.ClockOffset = 3 * hour },
			want: expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none", exact: []Attr{AttrClockSkew}},
		},
		{
			name: "B39_clock_skew_negative_and_boundary",
			mod:  func(f *DeviceFacts) { f.ClockOffset = -2*time.Minute - time.Millisecond },
			want: expectation{primary: StateOK, attrs: []Attr{AttrClockSkew}},
		},
		{
			name: "B39_clock_skew_exactly_two_minutes_is_not_skew",
			mod:  func(f *DeviceFacts) { f.ClockOffset = 2 * time.Minute },
			want: expectation{primary: StateOK, noAttrs: []Attr{AttrClockSkew}},
		},
		{
			// B40 is the loader's clamp: finished_at beyond received_at is clamped. The engine computes
			// STALE from the value it is given (here the clamped one, 100 h ago).
			name: "B40_future_dated_success_clamped",
			mod:  func(f *DeviceFacts) { setSuccess(f, 100*hour) },
			want: expectation{primary: StateStale, flags: []State{StateStale}, alarm: "critical"},
		},
		{
			name: "B41_missed_run_warning",
			mod: func(f *DeviceFacts) {
				setSuccess(f, 71*hour)
				f.LastAttemptStart = ago(70 * hour)
			},
			want: expectation{primary: StateMissed, flags: []State{StateMissed}, alarm: "none",
				check: func(t *testing.T, got DeviceState) {
					if want := t0.Add(-70*hour + 48*hour); !got.Since.Equal(want) {
						t.Errorf("since = %v, want expected time %v", got.Since, want)
					}
				}},
		},
		{
			name: "B41_missed_run_window_slack_extends",
			mod: func(f *DeviceFacts) {
				setSuccess(f, 71*hour)
				f.LastAttemptStart = ago(70 * hour)
				f.Thresholds.WindowSlack = 30 * hour // 48 h + 30 h = 78 h > 70 h
			},
			want: expectation{primary: StateOK, alarm: "none"},
		},
		{
			name: "B41_missed_run_not_yet_over_factor",
			mod: func(f *DeviceFacts) {
				setSuccess(f, 71*hour)
				f.LastAttemptStart = ago(47 * hour)
			},
			want: expectation{primary: StateOK, alarm: "none"},
		},
		{
			name: "B41_missed_never_without_schedule",
			mod: func(f *DeviceFacts) {
				f.Thresholds.Interval = 0
				setSuccess(f, 71*hour)
				f.LastAttemptStart = ago(70 * hour)
			},
			want: expectation{primary: StateOK, alarm: "none", attrs: []Attr{AttrNoSchedule}},
		},
		{
			name: "B42_missed_not_when_running",
			mod: func(f *DeviceFacts) {
				setSuccess(f, 71*hour)
				f.LastAttemptStart = ago(70 * hour)
				f.OpenRun = &RunFacts{StartedAt: ago(1 * hour)}
			},
			want: expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none"},
		},
		{
			name: "B42_missed_not_when_deferred",
			mod: func(f *DeviceFacts) {
				setSuccess(f, 71*hour)
				f.LastAttemptStart = ago(70 * hour)
				f.ServiceState = "deferred"
			},
			want: expectation{primary: StateDeferred, flags: []State{StateDeferred}, alarm: "none"},
		},
		{
			name: "B42_missed_not_when_unseen",
			mod: func(f *DeviceFacts) {
				setSuccess(f, 71*hour)
				f.LastAttemptStart = ago(70 * hour)
				f.LastSeen = ago(200 * hour)
			},
			want: expectation{primary: StateUnseen, flags: []State{StateUnseen}, alarm: "critical"},
		},
		{
			name: "B43_ack_does_not_change_state",
			mod: func(f *DeviceFacts) {
				f.LastBackup = failedRun(30*time.Minute, "network")
				f.ConsecutiveFailures = 1
				f.Ack = &AckFacts{AckedAt: ago(1 * hour)}
			},
			want: expectation{primary: StateFailed, flags: []State{StateFailed}, alarm: "critical", attrs: []Attr{AttrAcknowledged},
				check: func(t *testing.T, got DeviceState) {
					if got.Ack == nil || !got.Ack.Muted || !got.Ack.AckedAt.Equal(t0.Add(-1*hour)) {
						t.Errorf("ack = %+v, want muted", got.Ack)
					}
				}},
		},
		{
			name: "B44_snooze_expiry_reraises",
			mod: func(f *DeviceFacts) {
				f.LastBackup = failedRun(30*time.Minute, "network")
				f.ConsecutiveFailures = 1
				f.Ack = &AckFacts{AckedAt: ago(3 * hour), SnoozeUntil: ago(1 * hour)}
			},
			want: expectation{primary: StateFailed, alarm: "critical", noAttrs: []Attr{AttrAcknowledged, AttrSnoozed},
				check: func(t *testing.T, got DeviceState) {
					if got.Ack == nil || got.Ack.Muted {
						t.Errorf("an expired snooze still mutes: %+v", got.Ack)
					}
				}},
		},
		{
			name: "B44_snooze_active_mutes",
			mod: func(f *DeviceFacts) {
				f.LastBackup = failedRun(30*time.Minute, "network")
				f.ConsecutiveFailures = 1
				f.Ack = &AckFacts{AckedAt: ago(3 * hour), SnoozeUntil: ahead(2 * hour)}
			},
			want: expectation{primary: StateFailed, alarm: "critical", attrs: []Attr{AttrSnoozed}, noAttrs: []Attr{AttrAcknowledged},
				check: func(t *testing.T, got DeviceState) {
					if got.Ack == nil || !got.Ack.Muted || !got.Ack.SnoozedUntil.Equal(t0.Add(2*hour)) {
						t.Errorf("ack = %+v", got.Ack)
					}
				}},
		},
		{
			name: "B45_snooze_until_next_success_holds_until_success",
			mod: func(f *DeviceFacts) {
				f.LastSuccess = ago(5 * hour)
				f.LastBackup = failedRun(1*hour, "network")
				f.ConsecutiveFailures = 1
				f.Ack = &AckFacts{AckedAt: ago(4 * hour), SnoozeUntilNextSuccess: true, AckedAtSuccess: ago(5 * hour)}
			},
			want: expectation{primary: StateFailed, alarm: "critical", attrs: []Attr{AttrSnoozed},
				check: func(t *testing.T, got DeviceState) {
					if got.Ack == nil || !got.Ack.Muted {
						t.Errorf("ack = %+v, want muted until a newer success", got.Ack)
					}
				}},
		},
		{
			name: "B45_snooze_until_next_success_clears",
			mod: func(f *DeviceFacts) {
				setSuccess(f, 2*hour) // newer than the success recorded when the ack was made
				f.Ack = &AckFacts{AckedAt: ago(4 * hour), SnoozeUntilNextSuccess: true, AckedAtSuccess: ago(5 * hour)}
			},
			want: expectation{primary: StateOK, alarm: "none", noAttrs: []Attr{AttrSnoozed, AttrAcknowledged},
				check: func(t *testing.T, got DeviceState) {
					if got.Ack == nil || got.Ack.Muted {
						t.Errorf("ack = %+v, want cleared", got.Ack)
					}
				}},
		},
		{
			name: "B47_unknown_device_class_ok",
			mod:  func(f *DeviceFacts) { f.DeviceClass = "tablet" },
			want: expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none"},
		},
		{
			name: "B26_neutral_outcomes_do_not_change_result_interrupted",
			mod:  neutralAfterSuccess("interrupted"),
			want: neutralWant(notAlarming),
		},
		{
			name: "B26_neutral_outcomes_do_not_change_result_cancelled",
			mod:  neutralAfterSuccess("cancelled"),
			want: neutralWant(notAlarming),
		},
		{
			name: "B26_neutral_outcomes_do_not_change_result_skipped",
			mod:  neutralAfterSuccess("skipped"),
			want: neutralWant(notAlarming),
		},
	})
}

func neutralAfterSuccess(outcome string) func(f *DeviceFacts) {
	return func(f *DeviceFacts) {
		f.LastBackup = &RunFacts{Outcome: outcome, StartedAt: ago(70 * time.Minute), FinishedAt: ago(1 * hour), CountsKnown: true}
		f.ConsecutiveFailures = 0
	}
}

func neutralWant(extra func(*testing.T, DeviceState)) expectation {
	return expectation{primary: StateOK, flags: []State{StateOK}, alarm: "none",
		check: func(t *testing.T, got DeviceState) {
			extra(t, got)
			if want := t0.Add(-2 * hour); !got.Detail.LastSuccess.Equal(want) {
				t.Errorf("last success moved to %v, want %v", got.Detail.LastSuccess, want)
			}
		}}
}

func TestB15RetiredExcludedFromRollup(t *testing.T) {
	t.Run("B15_retired_excluded_from_rollup", func(t *testing.T) {
		var ds []DeviceState
		for i := 1; i <= 3; i++ {
			f := baseFacts()
			f.ID, f.Hostname = devID(i), "ws-desk-0"+string(rune('0'+i))
			if i == 3 {
				f.RetiredAt = ago(10 * day)
			}
			ds = append(ds, Classify(f, t0))
		}
		cs := Rollup(CustomerFacts{ID: custID(1), Name: "Northwind Dental", State: "active"}, ds, t0)
		if cs.Active != 2 || cs.Retired != 1 || cs.Counts[StateOK] != 2 || cs.Counts[StateRetired] != 0 {
			t.Errorf("active %d retired %d counts %v, want 2/1/{OK:2}", cs.Active, cs.Retired, cs.Counts)
		}
		if !cs.HealthKnown || cs.HealthPct != 100 {
			t.Errorf("health = %v %v", cs.HealthPct, cs.HealthKnown)
		}
	})
}

func TestB46UnknownOutcomeValueIsFailure(t *testing.T) {
	t.Run("B46_unknown_outcome_value_is_failure", func(t *testing.T) {
		cases := []struct {
			outcome string
			partial bool
			want    ResultClass
		}{
			{"success", false, ClassSuccess},
			{"partial", true, ClassSuccess},
			{"partial", false, ClassPartial},
			{"failed", false, ClassFailure},
			{"abandoned", false, ClassFailure},
			{"cancelled", false, ClassNeutral},
			{"skipped", false, ClassNeutral},
			{"interrupted", false, ClassNeutral},
			{"deferred", false, ClassNeutral},
			{"exploded", false, ClassFailure},
			{"exploded", true, ClassFailure},
			{"", false, ClassFailure},
			{"SUCCESS", false, ClassFailure},
		}
		for _, c := range cases {
			if got := OutcomeClass(c.outcome, c.partial); got != c.want {
				t.Errorf("OutcomeClass(%q, %v) = %s, want %s", c.outcome, c.partial, got, c.want)
			}
		}
	})
	runClassifyCases(t, []classifyCase{{
		name: "B46_unknown_outcome_value_is_failure_state",
		mod: func(f *DeviceFacts) {
			f.LastBackup = &RunFacts{Outcome: "exploded", StartedAt: ago(70 * time.Minute), FinishedAt: ago(1 * hour)}
			f.ConsecutiveFailures = 0
		},
		want: expectation{primary: StateFailed, flags: []State{StateFailed}, alarm: "critical"},
	}})
}

// TestB48DeterministicAndClockFree asserts that Classify is a function of its
// arguments and that the package's non-test sources never read the clock.
func TestB48DeterministicAndClockFree(t *testing.T) {
	t.Run("B48_deterministic", func(t *testing.T) {
		f := baseFacts()
		f.LastBackup = partialRun(1000, 99000, map[string]int{"locked": 400, "permission": 600})
		f.LastBackup.VSSFallback = []string{"D", "C", "E"}
		f.Ack = &AckFacts{AckedAt: ago(1 * hour)}
		first := Classify(f, t0)
		for i := 0; i < 50; i++ {
			if got := Classify(f, t0); !reflect.DeepEqual(got, first) {
				t.Fatalf("run %d differs:\n%+v\n%+v", i, got, first)
			}
		}
	})
	t.Run("B48_now_is_the_only_clock", func(t *testing.T) {
		f := baseFacts()
		f.LastSeen = ago(30 * time.Second)
		later := t0.Add(200 * hour)
		if got := Classify(f, later); got.Primary != StateUnseen {
			t.Errorf("200 h later primary = %s, want UNSEEN: the injected now must drive the result", got.Primary)
		}
		if got := Classify(f, t0); got.Primary != StateOK {
			t.Errorf("at t0 primary = %s, want OK", got.Primary)
		}
		// the same instant expressed in another zone gives the same answer
		est := time.FixedZone("EST", -5*3600)
		if a, b := Classify(f, t0), Classify(f, t0.In(est)); !reflect.DeepEqual(a, b) {
			t.Errorf("zone of now changed the result:\n%+v\n%+v", a, b)
		}
	})
	t.Run("B48_inputs_not_mutated", func(t *testing.T) {
		f := baseFacts()
		f.LastBackup = partialRun(3, 90000, map[string]int{"permission": 3})
		f.LastBackup.VSSFallback = []string{"D", "C"}
		want := baseFacts()
		want.LastBackup = partialRun(3, 90000, map[string]int{"permission": 3})
		want.LastBackup.VSSFallback = []string{"D", "C"}
		Classify(f, t0)
		if !reflect.DeepEqual(f, want) {
			t.Errorf("Classify mutated its input")
		}
	})
}
