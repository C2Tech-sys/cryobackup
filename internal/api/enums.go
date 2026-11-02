package api

import "github.com/c2tech-sys/cryobackup/internal/restic"

// This file holds the closed vocabularies of the protocol. Where a value is
// also stored in the database, the string is exactly the schema value
// (replica/schema.sql CHECK lists); enums_test.go compares the two. The
// vocabularies shared with internal/restic (Kind, Outcome, DeferredReason,
// ErrorClass) are used from there and not repeated.

// Arch is the CPU architecture of an endpoint (devices.arch).
type Arch string

// Architectures.
const (
	ArchAMD64 Arch = "amd64"
	ArchARM64 Arch = "arm64"
	ArchX86   Arch = "x86"
)

// DeviceClass says whether an endpoint is a laptop or a desktop, which selects
// the class-dependent policy defaults (devices.device_class). "unknown" is what
// an agent sends when it cannot tell.
type DeviceClass string

// Device classes.
const (
	DeviceClassLaptop  DeviceClass = "laptop"
	DeviceClassDesktop DeviceClass = "desktop"
	DeviceClassUnknown DeviceClass = "unknown"
)

// Custody says who holds the repository key (customers.key_custody,
// repo_key_escrow.custody, DH22).
type Custody string

// Custody values.
const (
	CustodyEscrowed     Custody = "escrowed"
	CustodyCustomerHeld Custody = "customer_held"
)

// ServiceState is what the agent service is doing (device_presence.service_state).
type ServiceState string

// Service states.
const (
	ServiceIdle     ServiceState = "idle"
	ServiceRunning  ServiceState = "running"
	ServicePaused   ServiceState = "paused"
	ServiceDeferred ServiceState = "deferred"
)

// Trigger is why a run started (runs.trigger_kind, architecture §6.2).
type Trigger string

// Triggers.
const (
	TriggerSchedule Trigger = "schedule"
	TriggerCatchup  Trigger = "catchup"
	TriggerWake     Trigger = "wake"
	TriggerBoot     Trigger = "boot"
	TriggerManual   Trigger = "manual"
	TriggerRetry    Trigger = "retry"
)

// CommandType is the type of a queued command (commands.type, architecture
// §6.1 "command executor", DH21).
type CommandType string

// Command types.
const (
	CommandBackupNow      CommandType = "backup_now"
	CommandCancel         CommandType = "cancel"
	CommandRefreshPolicy  CommandType = "refresh_policy"
	CommandRestartService CommandType = "restart_service"
	CommandUpdateAgent    CommandType = "update_agent"
	CommandPause          CommandType = "pause"
	CommandResume         CommandType = "resume"
	CommandHold           CommandType = "hold"
	CommandRestore        CommandType = "restore"
	CommandCheckIn        CommandType = "check_in"
	CommandUninstall      CommandType = "uninstall"
)

// CommandStatus is what an agent reports for a command: the subset of
// commands.status that an agent can say (queued, delivered, expired and
// cancelled are the console's).
type CommandStatus string

// Command result statuses.
const (
	CommandAcked     CommandStatus = "acked"
	CommandSucceeded CommandStatus = "succeeded"
	CommandFailed    CommandStatus = "failed"
)

// RestoreMode is where a restore lands (restore_requests.mode). The two
// download modes are admin-side only and never reach an agent.
type RestoreMode string

// Restore modes an agent sees.
const (
	RestoreDeviceOriginal RestoreMode = "device_original"
	RestoreDevicePath     RestoreMode = "device_path"
	RestoreDeviceRelay    RestoreMode = "device_relay"
)

// Overwrite is restic's --overwrite behaviour (restore_requests.overwrite).
type Overwrite string

// Overwrite values.
const (
	OverwriteAlways    Overwrite = "always"
	OverwriteIfChanged Overwrite = "if-changed"
	OverwriteIfNewer   Overwrite = "if-newer"
	OverwriteNever     Overwrite = "never"
)

// RunStatus is the console's verdict on one run record of a RunsRequest.
type RunStatus string

// Run result statuses: created (new run), updated (an open run was closed or
// refreshed), duplicate (already stored, nothing changed), rejected (see
// RunResult.Reason).
const (
	RunCreated   RunStatus = "created"
	RunUpdated   RunStatus = "updated"
	RunDuplicate RunStatus = "duplicate"
	RunRejected  RunStatus = "rejected"
)

// Reasons of a rejected RunResult. They are fixed codes, never text from the record.
const (
	ReasonInvalid  = "invalid"  // the record failed Validate
	ReasonConflict = "conflict" // the id exists with a different, already closed record
	ReasonInternal = "internal" // the console could not store it; retry later
)

// Channel is a release ring (agent_releases channel).
type Channel string

// Release channels.
const (
	ChannelStable Channel = "stable"
	ChannelPilot  Channel = "pilot"
)

// LobbyStatus is the state of a lobby enrollment request as the agent sees it
// (a subset of enrollment_requests.status: an expired request is a 404).
type LobbyStatus string

// Lobby statuses.
const (
	LobbyPending  LobbyStatus = "pending"
	LobbyRejected LobbyStatus = "rejected"
	LobbyApproved LobbyStatus = "approved"
)

// Enum names used as keys of Enums.
const (
	EnumArch                = "arch"
	EnumDeviceClass         = "device_class"
	EnumCustody             = "custody"
	EnumServiceState        = "service_state"
	EnumRunKind             = "run_kind"
	EnumRunTrigger          = "run_trigger"
	EnumRunOutcome          = "run_outcome"
	EnumDeferredReason      = "deferred_reason"
	EnumErrorClass          = "error_class"
	EnumCommandType         = "command_type"
	EnumCommandResultStatus = "command_result_status"
	EnumRestoreMode         = "restore_mode"
	EnumRestoreOverwrite    = "restore_overwrite"
	EnumRunResultStatus     = "run_result_status"
	EnumReleaseChannel      = "release_channel"
	EnumLobbyStatus         = "lobby_status"
	EnumErrorCode           = "error_code"
	EnumLeaseOutcome        = "lease_outcome"
)

var (
	archs        = []Arch{ArchAMD64, ArchARM64, ArchX86}
	deviceClass  = []DeviceClass{DeviceClassLaptop, DeviceClassDesktop, DeviceClassUnknown}
	custodies    = []Custody{CustodyEscrowed, CustodyCustomerHeld}
	states       = []ServiceState{ServiceIdle, ServiceRunning, ServicePaused, ServiceDeferred}
	triggers     = []Trigger{TriggerSchedule, TriggerCatchup, TriggerWake, TriggerBoot, TriggerManual, TriggerRetry}
	commandTypes = []CommandType{
		CommandBackupNow, CommandCancel, CommandRefreshPolicy, CommandRestartService, CommandUpdateAgent,
		CommandPause, CommandResume, CommandHold, CommandRestore, CommandCheckIn, CommandUninstall,
	}
	commandStatuses = []CommandStatus{CommandAcked, CommandSucceeded, CommandFailed}
	restoreModes    = []RestoreMode{RestoreDeviceOriginal, RestoreDevicePath, RestoreDeviceRelay}
	overwrites      = []Overwrite{OverwriteAlways, OverwriteIfChanged, OverwriteIfNewer, OverwriteNever}
	runStatuses     = []RunStatus{RunCreated, RunUpdated, RunDuplicate, RunRejected}
	channels        = []Channel{ChannelStable, ChannelPilot}
	lobbyStatuses   = []LobbyStatus{LobbyPending, LobbyRejected, LobbyApproved}
	recordedKinds   = []restic.Kind{
		restic.KindBackup, restic.KindRestore, restic.KindForget, restic.KindPrune,
		restic.KindCheck, restic.KindUnlock, restic.KindMaintenance,
	}
	outcomes = []restic.Outcome{
		restic.OutcomeSuccess, restic.OutcomePartial, restic.OutcomeFailed, restic.OutcomeCancelled,
		restic.OutcomeSkipped, restic.OutcomeDeferred, restic.OutcomeInterrupted, restic.OutcomeAbandoned,
	}
	deferredReasons = []restic.DeferredReason{
		restic.DeferredLock, restic.DeferredGateway, restic.DeferredBattery,
		restic.DeferredMetered, restic.DeferredWindow, restic.DeferredConditions,
		restic.DeferredFleetCap,
	}
	leaseOutcomes = []LeaseOutcome{LeaseGranted, LeaseBypassed, LeaseNotRequired}
)

// has reports whether v is in list.
func has[T comparable](list []T, v T) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// strs converts a typed enum list to strings.
func strs[T ~string](list []T) []string {
	out := make([]string, len(list))
	for i, v := range list {
		out[i] = string(v)
	}
	return out
}

// Enums returns every closed vocabulary of the protocol: enum name (the Enum*
// constants) to its allowed values. The slices are fresh copies. A test
// compares them with the CHECK constraints of replica/schema.sql, so the
// protocol and the database cannot drift apart: the enums that the protocol
// exposes in full must equal the schema list; those it exposes in part
// (command_result_status, restore_mode, lobby_status) must be a subset.
func Enums() map[string][]string {
	return map[string][]string{
		EnumArch:                strs(archs),
		EnumDeviceClass:         strs(deviceClass),
		EnumCustody:             strs(custodies),
		EnumServiceState:        strs(states),
		EnumRunKind:             strs(recordedKinds),
		EnumRunTrigger:          strs(triggers),
		EnumRunOutcome:          strs(outcomes),
		EnumDeferredReason:      strs(deferredReasons),
		EnumErrorClass:          strs(restic.AllClasses()),
		EnumCommandType:         strs(commandTypes),
		EnumCommandResultStatus: strs(commandStatuses),
		EnumRestoreMode:         strs(restoreModes),
		EnumRestoreOverwrite:    strs(overwrites),
		EnumRunResultStatus:     strs(runStatuses),
		EnumReleaseChannel:      strs(channels),
		EnumLobbyStatus:         strs(lobbyStatuses),
		EnumErrorCode:           ErrorCodes(),
		EnumLeaseOutcome:        strs(leaseOutcomes),
	}
}
