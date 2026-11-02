; CryoBackup agent: Event Log message table (deploy/windows/README.md, "Event
; Log"). Every id has the agent's own line as its only insertion string and
; the text %1, so Event Viewer shows exactly what the agent wrote (%0 ends the
; message without a line break). The ids are the documented ones:
;   1000 agent started            1007 state directory quarantined
;   1001 restic verification      1008 retired (kept so old events render)
;   1002 policy rejected          1009 service configuration repaired
;   1003 agent stopped            1010 backup finished
;   1004 state directory refused  1011 backup failed
;   1005 lab flag in use          1012 backup deferred
;   1006 state directory tampered
;
; deploy/windows/eventlog/build-messages.ps1 compiles this file (MinGW windmc
; and windres) into cmd/cryo-agent/messages_windows_amd64.syso, which go build
; links into cryo-agent.exe; the Event Log source names cryo-agent.exe as its
; EventMessageFile. To add an id: add a block here and to
; internal/agent/service.EventIDs, run the script, commit all three.

MessageIdTypedef=DWORD
LanguageNames=(English=0x409:MSG00409)

MessageId=1000
SymbolicName=CRYO_EVENT_1000
Language=English
%1%0
.

MessageId=1001
SymbolicName=CRYO_EVENT_1001
Language=English
%1%0
.

MessageId=1002
SymbolicName=CRYO_EVENT_1002
Language=English
%1%0
.

MessageId=1003
SymbolicName=CRYO_EVENT_1003
Language=English
%1%0
.

MessageId=1004
SymbolicName=CRYO_EVENT_1004
Language=English
%1%0
.

MessageId=1005
SymbolicName=CRYO_EVENT_1005
Language=English
%1%0
.

MessageId=1006
SymbolicName=CRYO_EVENT_1006
Language=English
%1%0
.

MessageId=1007
SymbolicName=CRYO_EVENT_1007
Language=English
%1%0
.

MessageId=1008
SymbolicName=CRYO_EVENT_1008
Language=English
%1%0
.

MessageId=1009
SymbolicName=CRYO_EVENT_1009
Language=English
%1%0
.

MessageId=1010
SymbolicName=CRYO_EVENT_1010
Language=English
%1%0
.

MessageId=1011
SymbolicName=CRYO_EVENT_1011
Language=English
%1%0
.

MessageId=1012
SymbolicName=CRYO_EVENT_1012
Language=English
%1%0
.
