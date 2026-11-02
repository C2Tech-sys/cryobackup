// Package commands executes the console's commands on the endpoint
// (architecture §6.1 "command executor", §5.1 GET /commands).
//
// Execution is idempotent on the command id: the journal's command log records
// every command before it runs, and a command id seen again returns its stored
// result without running again (the result is re-posted until the console
// acknowledges it, or refuses it with a final 4xx, which drops it).
//
// M0 implements:
//
//	backup_now      starts a backup that bypasses window and conditions; with a
//	                run active it is skipped (skip_if_running) and the result says so
//	cancel          asks the run named by params.run_id to stop (restic exit 130,
//	                outcome "cancelled"); another or finished run is left alone and
//	                the result says run_mismatch; without a run_id, any active run
//	refresh_policy  pulls the policy now
//	check_in        sends a heartbeat now
//
// pause, resume, hold, restart_service, update_agent, restore and uninstall are
// journaled as failed with error class "unknown" and the fixed reason
// "not_implemented_m0": never silently dropped, so the console shows that the
// device received the command and could not act. An unknown command type (from
// a newer console) fails with "unknown_command"; an invalid or expired command
// fails with "invalid" or "expired". Result bodies are small fixed objects and
// pass api.AssertPathFree in the uplink like every other body.
package commands
