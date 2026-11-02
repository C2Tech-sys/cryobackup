# Comet Backup — clean-room recon map (Comet half for CryoBackup)

**Scope:** MSP core loop for Windows workstation file backup: enroll device → assign policy / Protected Items → scheduled runs → monitoring, alerts, reports → restore. Covers the Comet Server web console ("Management Console") and the Comet Backup desktop agent.

**Method:** Public sources only (docs.cometbackup.com, cometbackup.com marketing/pricing/videos page, release notes, public API reference). No binaries, bundles, logins, or scraping loops. Everything below is functionality and UX pattern, not copy or code. Where the docs did not say something, it is marked **[inferred]** or **[gap]**. Dates/versions are as of 2026-10-20 (latest release 26.8.9 "Voyager", Oct 15 2026).

**Vocabulary map (Comet → generic):** Management Console = admin web console. User / User profile = customer backup account (the security boundary; one per device or per customer, holds devices, vaults, items). Device = enrolled endpoint. Protected Item = backup set. Storage Vault = repository (encrypted, deduped). Storage Template = server-side recipe that auto-creates vaults. Backup Rule = schedule (Protected Item → Storage Vault + ScheduleConfig list + event triggers). User Policy = policy object assigned to users. Tenant (formerly Organization) = isolated reseller/customer zone with own admins. Live connection / Dispatcher = persistent agent↔server websocket used for remote actions. Lobby / Remote registration = unregistered agent awaiting assignment.

---

## 1. SOURCES

| # | Source | URL | What it gave |
|---|---|---|---|
| 1 | Docs index | https://docs.cometbackup.com/latest/ | Top-level section map |
| 2 | Start Here: overview | https://docs.cometbackup.com/latest/start-here/overview-setup | Console vs agent split; Self-Hosted vs Comet-Hosted; the 7-step quick path and its page URLs |
| 3 | Start Here steps 1–7 | https://docs.cometbackup.com/latest/start-here/add-user, /download-and-install, /log-in, /add-pi, /new-sv, /run-backup, /job-status | Each step's intent; users as security boundary; both-sides (agent or console) configuration; Job History red Cancel button; Recent Activity 10-day view. Mostly video-driven, few labels |
| 4 | Getting started (Self-Hosted) | https://docs.cometbackup.com/latest/guides/guide-getting-started | First-run wizard (SSL, admin account, Storage Gateway); Guides sidebar (24 pages) |
| 5 | Getting started with Comet Backup (agent) | https://docs.cometbackup.com/latest/guides/guide-comet-backup-getting-started | Add User quick action; agent Backup/Settings tabs; Protected Item wizard sections; command run order; retention override rule; Default Policy |
| 6 | Comet Backup installation | https://docs.cometbackup.com/latest/installation/comet-backup-installation | OS support; EXE/MSI/pkg/deb/rpm/run/spk; silent switches `/S /LOBBY`, `/CONFIGURE=user:pass`, `/TOKEN=`, `/SERVER=`, `/TRAYICON=disable`, `/SHORTCUT=disable`; `/dl/<platform>` download routes; lobby = "Unregistered" on Devices page; install tokens; uninstall; service names |
| 7 | Mass deployment guide | https://docs.cometbackup.com/latest/guides/mass-deployment-guide | Pre-deployment order (branding → codesigning → policies → storage templates); registration method choice; CSV user creation |
| 8 | User configuration | https://docs.cometbackup.com/latest/installation/user-configuration | Profile fields; irreversible "Allow administrator…" flags; quotas; device rules; all schedule frequencies and general options; retention pass cadence table (high/low power) |
| 9 | Management Console usage | https://docs.cometbackup.com/latest/installation/comet-server-usage | Nav menus; Users page buttons/export; Devices statuses and action list; Bulk Device Upgrade campaign rules; Recent Activity badges/timeline; Advanced options list; Replication modes; About this System |
| 10 | Self-Hosted console configuration | https://docs.cometbackup.com/latest/installation/comet-server-configuration | Settings sections; admin permissions; auth options (TOTP/WebAuthn/OIDC); Tenants mechanics; audit log options; email delivery modes; IP rate limit; session timeout; `GenerateMissedBackupEvents`; Storage Template; System Self-Backup |
| 11 | Comet-Hosted console configuration | https://docs.cometbackup.com/latest/installation/Comet-Hosted-Management-Console-Configuration | Hosted settings dialog (Custom Domain, Email via AWS SES + DMARC DNS, Admin users, Storage Template) |
| 12 | Files and Folders Protected Item | https://docs.cometbackup.com/latest/protected-items/files-and-folders | Item editor fields (snapshot/VSS, include/exclude glob+regex, rescan, extra permissions, EFS, network share login); system exclusions; rehydration note |
| 13 | Protected Item types | https://docs.cometbackup.com/latest/protected-items/overview | Engine list; items private per device; configurable from either side |
| 14 | How to set up retention | https://docs.cometbackup.com/latest/guides/how-to-set-up-retention | All 11 range labels; OR-combination; PI overrides vault; GFS recipe; keep-forever warnings |
| 15 | How to apply user policies | https://docs.cometbackup.com/latest/guides/how-to-apply-policies | Policy editor sections and labels; 5-state mode settings; Default Policy; assignment paths |
| 16 | How to configure email reporting | https://docs.cometbackup.com/latest/guides/configuring-email-reports | 4 report types; schedule; filter clauses; recipes |
| 17 | Comet job statuses | https://docs.cometbackup.com/latest/guides/guide-job-statuses | 12 status labels and meanings |
| 18 | Quotas | https://docs.cometbackup.com/latest/guides/configuring-quotas | 4 quota kinds and exceeded behaviour |
| 19 | MFA guide | https://docs.cometbackup.com/latest/guides/how-to-set-up-mfa | Admin TOTP/WebAuthn steps; end-user TOTP via Customer Web Portal; admin "Disable 2FA (TOTP)" |
| 20 | Rebrand console / rebrand client | https://docs.cometbackup.com/latest/guides/how-to-rebrand-comet-server, /client-rebranding-guide | System Branding fields; agent branding fields (Product name, Company name, Help URL, Default Server URL, Registration URL, icons, EULA, logo/tile) |
| 21 | Deep linking | https://docs.cometbackup.com/latest/guides/deep-linking-to-the-management-console | Console route structure (`#/users/<u>/devices/<id>/run-backup` etc.); dialog tab names |
| 22 | Disaster recovery | https://docs.cometbackup.com/latest/guides/guide-disaster-recovery | Replacement-device procedure; revoke old device; catch-up charge |
| 23 | Common problems | https://docs.cometbackup.com/latest/guides/guide-common-problems-and-solutions | Stuck job remediation; retry option (24.6.6); VSS diagnosis; vault lock |
| 24 | Troubleshooting | https://docs.cometbackup.com/latest/troubleshooting | Lock/reindex/abandoned procedures; auto-cleanup conditions; device ID change |
| 25 | Appendix | https://docs.cometbackup.com/latest/appendix | `backup-tool` CLI (login status states); registry keys; proxy env; Windows Event Log IDs |
| 26 | Client overview / chunking / encryption | https://docs.cometbackup.com/latest/Overview-and-concepts/comet-client-overview, /comet-chunking-overview, /Encryption-and-key-management | backup-tool + backup-interface split; CDC chunking; key hierarchy (L/R/A/E keys) |
| 27 | Storage methods / Object Lock | https://docs.cometbackup.com/latest/storage-configuration/Storage-methods, /s3-object-locking | Direct-to-cloud vs Gateway; Object Lock compliance-mode rules |
| 28 | API guide / Other APIs | https://docs.cometbackup.com/latest/api/api-guide, /other-apis | Form-POST JSON API; webhooks (+Slack format); WebSocket event stream; MCP server; install tokens; auth types |
| 29 | API methods (full, 470 kB) | https://docs.cometbackup.com/latest/api/api-methods | Full admin + end-user method surface (Dispatcher, Jobs, Policies, Organizations, Update campaigns, Squota, UserWeb*) |
| 30 | API data structures (full, 250 kB) | https://docs.cometbackup.com/latest/api/api-data-structures | UserProfileConfig, DeviceConfig, SourceConfig, BackupRuleConfig, ScheduleConfig, RetentionPolicy/Range, DestinationConfig, BackupJobDetail, UserPolicy, GroupPolicy, EmailReportConfig, SearchClause, Organization, WebhookOption, InstallToken, LiveUserConnection, UpdateCampaign*, RestoreJobAdvancedOptions |
| 31 | API constants | https://docs.cometbackup.com/latest/api/api-constants | JOB_STATUS, JOB_CLASSIFICATION, SCHEDULE_FREQUENCY, RETENTIONRANGE/MODE, SEVT events, DESTINATIONTYPE, ENGINE, EMAILREPORTTYPE, SEARCH operators, RESTORETYPE, UPDATESTATUS |
| 32 | Third-party addons | https://docs.cometbackup.com/latest/third-party-service-addons | Syncro (API token, alerts raise/clear), NinjaOne (OAuth, custom fields, tickets), Backup Radar, CheckCentral, PRTG, Prometheus, WHMCS, HostBill, MyClient, figaro |
| 33 | Security & compliance | https://docs.cometbackup.com/latest/security-compliance | Telemetry sent to licensing server; HIPAA/GDPR posture; crypto summary |
| 34 | Billing guide | https://docs.cometbackup.com/latest/guides/Managing_Your_Comet_Billing | Prepaid credit; device/booster fees start when a PI is added to a connected device; revoke stops charges; catch-up charges |
| 35 | Pricing page | https://cometbackup.com/pricing/ | Plans ($19 Starter self-hosted, $49 Growth/Professional, $99 Advanced, Enterprise); "from $2/endpoint/month"; add-on meters; Comet Storage $6.99/TB |
| 36 | Features page | https://cometbackup.com/features/ | Feature claims by plan (audit log, SSO, IP controls, MCP, air-gap licensing, weekly vs quarterly update tracks) |
| 37 | Integrations page | https://cometbackup.com/integrations | 37 listed partners (storage, billing, monitoring, RMM) |
| 38 | Videos page | https://cometbackup.com/videos | ~135 public YouTube walkthroughs incl. Steps 1–7, "Backup and Restore with the Comet Client Software", "Set up email reports", "Tenant Feature", "Re-branding", "Apply retention policies" (titles used as evidence that these UI areas exist; not transcribed) |
| 39 | Release notes index + 26.4.4 | https://docs.cometbackup.com/release-notes, /release-notes/26-4-4-phoebe-released | Recent features: scheduled throttling (26.8.5), Extra Fields (26.8.4), admin API tokens (26.8.3), custom JS/CSS branding (26.8.2), Recent Activity filters + running job count (26.4.4), "If last job was Missed" (21.12.3), "When PC starts" also on wake (23.6.0), MSI (25.6.0) |
| 40 | Support KB article (admin roles) | https://support.cometbackup.com/hc/en-us/articles/41909235394839 | No read-only admin role; tenants as the workaround |

Blocked: support.cometbackup.com section index and g2.com returned HTTP 403; not pursued further.

---

## 2. SCREEN INVENTORY

Status legend for "states seen": documented states only; **[gap]** where docs give none. Comet's console is a single-page app with hash routes (`#/users/<u>/...`, `#/dispatch/<id>`, `#/u/...` for end users), left menu groups: Management Console (Users, Devices, Backup Job Reporting: Job logs, Recent activity), Storage Gateway (Storage buckets), Cluster Manager, System (System Logs, Replication, Settings, About this System). Top-right user menu: My Account, Advanced options, Log out. A "Paste" button appears in the top bar after copying a Protected Item.

### Console (admin side)

| ID | Screen | How you get there | Purpose | Key components | States seen |
|---|---|---|---|---|---|
| S01 | Console login | Browser → console URL (tenant hostname shows tenant branding) | Admin auth | Username, Password; TOTP or FIDO2 WebAuthn step (HTTP 449 challenge pattern); OIDC button (Entra ID/Google/generic, "Skip Comet multi-factor authentication"); "Download backup agent" link for anyone if enabled; custom JS/CSS injection (26.8.2) | Filled; error on bad creds; MFA challenge; branded per tenant |
| S02 | Home / dashboard | Login → default route | Landing | News area (hideable via `HideNewsArea`), optional Comet blog link, running-job count (26.4.4), system history/info widgets gated by admin permissions "View system history on dashboard" / "View system info on dashboard" | **[gap]** widget layout not documented |
| S03 | Users list | Menu → Users | All customer accounts | Buttons: "Add user", "Add user group...", "Move users to group...", "Assign user to group", "Change user group", "Remove users from group"; "Filter..." live text filter; "Show ## entries" pagination; export to clipboard (TSV), CSV, Excel; column picker; grouped vs ungrouped views ("Ungrouped" default group); Advanced: "Bulk replace addresses" (vault URL failover) | Filled; grouped/ungrouped; **[gap]** empty/columns |
| S04 | Add User dialog | S03 → "Add user" / quick-actions menu "Add User" | Create customer account | Username, Password; "Allow administrator to reset password"; "Automatically create Storage Vaults for newly registered devices" = (none) / System Default / Storage Template; "Apply Policy" dropdown (Default Policy preselected); group; Advanced: "Create multiple accounts by CSV" | Single; bulk CSV |
| S05 | User profile — Profile tab | S03 → click user | Account settings | Username (immutable), password reset, "Allow administrator to reset password" (irreversible once off), "Allow administrator to view file names" (irreversible once off), 2FA state, auto-vault template, User group, Language, Timezone (IANA, default Etc/UTC), Quotas: "All Protected Items" bytes, device count limit, per-engine instance quotas; Extra Fields (26.8.4, `UserExtra`/`AdminExtra`); "New Tenant" transfer; "Actions" button: Suspend/Unsuspend, Reset Password, "Disabled 2FA (TOTP)" → Disable; Advanced: "Edit raw profile", "Login as user" | Active; Suspended (still billed); **[gap]** |
| S06 | User profile — Devices tab | `#/users/<u>/devices` | Enrolled endpoints for this account | Rows: friendly name (default hostname, renamable), Device ID, OS/platform, client version, status badge "Online" / "Online (2)" / "Online (outdated)" / "Offline"; "Terminate" on double connections; Revoke; click → device action dialog; "Register" for lobby devices appears on Devices page | Online, Online (n), Online (outdated), Offline, Unregistered |
| S07 | Device action dialog (live) | S06 → device, or `#/users/<u>/devices/<id>`, or `#/dispatch/<id>` | Remote control of a connected agent | Actions: "Immediately run backup", "Immediately run a scheduled backup", "Remotely restore", "Update software", "Uninstall software", "Update Login URL", "Refresh profile", "Import / scan for other installed products", "Apply retention rules now"; Advanced: "Drop connection", "Terminate process", "Unlock / clean up lock files", "Reindex", "Deep verify Vault contents", "Multiple actions" | Only for Online devices; disabled for Offline |
| S08 | Run Backup dialog (remote) | S07 → run backup, `#/.../run-backup` | Trigger job now | Pick Protected Item, pick Storage Vault (or pick an existing schedule); advanced BackupJobAdvancedOptions | **[gap]** |
| S09 | Remote Restore dialog | S07 → "Remotely restore", `#/.../restore` | Restore to the device from console | Same wizard as A10; Advanced: "Delete single snapshot" (19.12.2+), "View snapshot IDs" (21.12.8+) | Browse, loading snapshot list (requires live device) |
| S10 | User profile — Protected Items tab | `#/users/<u>/protected-items` | Backup sets per device | List grouped by device; per-item "Copy" link → "Paste" button in top bar (clone to another device/user); add/edit/delete (policy may lock) | Filled; locked-by-policy **[inferred]** |
| S11 | Protected Item editor dialog | S10 → item, or `#/users/<u>/protected-items/<guid>/<tab>` | Create/edit a backup set | Tabs: general (Name/Description, Type), auth (engine credentials e.g. "Log in to network share"), items (Choose files tree with live size estimate; Include filters; Exclude filters, glob or regex, unlimited; "Take filesystem snapshot" default on; "Rescan unchanged files"; "Back up extra system permissions and attributes" (24.6.2+); "I confirm EFS keys are exported"), commands (before/after/"thaw"), schedule (S12), retention (S13: "(default)" vs "(X rules)") | New; edit; **[gap]** validation |
| S12 | Schedule editor (Backup Rule) | S11 → schedule tab | When the item backs up, to which vault | Per rule: target Storage Vault (or policy selector: latest vault / all vaults / location type); frequencies: Hourly (minutes past), Daily (24h time), Weekly (days+time), Monthly, Yearly (months+time), Once only (date+time), Periodic (every N + Offset); restrict to time window ("RestrictRuntime" From/To) and days; event triggers: "When PC starts" (also on wake, 23.6.0; triggers on service start if uptime <10 min), "If the last job was Missed"; "Retry failed backups" count + minutes (24.6.6, Time tab); general: "Skip if already running", "Cancel if still running after", "Limit Storage Vault Speed" (+ "Limit speed at certain times" with days, 26.8.5), "Limit backup to use only 1 disk thread", "Prefer temporary files instead of RAM (slower)", "Allow backup of zero files to succeed"; "Random job delay" up to 300 min (policy-level too) | Multiple schedules per item; one per vault |
| S13 | Retention editor | S11 retention tab; S15 general tab; S25 retention | Which snapshots to keep | Mode: "Keep all backups forever" vs rule list (DELETE_EXCEPT); add range: "Last [...] backups", "Last [...] backups, at most one per day/week/month/year", "All backups in the last [...]" (days/weeks/months), "All backups newer than a specific date", "One backup each day/week/month/year, for the last [...]" with week-day / month-day / month offsets; "Apply retention policy after backup": Automatic / Run immediately / Run more often / Run less often | default / overridden |
| S14 | User profile — Storage Vaults tab | `#/users/<u>/storage-vaults` | Repositories for this account | List; "Stored" column clickable → Current vs Historic breakdown; "Retention Pass" to regenerate; add via template ("Request Storage Vault") or `/new-custom`; "Show other device vaults" checkbox | Filled; uninitialised (RepoInitTimestamp 0); suspended; RetentionError |
| S15 | Storage Vault editor dialog | S14 → vault, `#/.../storage-vaults/<id>/<tab>` | Vault settings | Tabs: general (name, default retention, quota "StorageLimitBytes"), storage (type + credentials; hidden if `RebrandStorage`), commands, encryption (informational, always on), associated-devices; "Object Lock days"; "allow removal of deleted files"; `PreventUserEdit` | Template-created (storage tab masked) vs custom |
| S16 | User profile — Job Logs tab | User → Job Logs | Per-account job history | Job rows → S22; export | **[gap]** columns |
| S17 | User profile — Policy tab | User → Policies | Assign policy | Dropdown of policies, "(Custom)" marker, "Save changes" | Default / named / Custom |
| S18 | User profile — Reporting (email reports) | User → Reporting | Per-account email reports | `Emails` list; "Email backup reports to this account" toggle; per-address report list (type, schedule, filter, Title 26.6.0); "Policy default" report row when enforced; preview (AdminPreviewUserEmailReport) | On/off; inherited vs custom |
| S19 | Devices page (all) | Menu → Devices | Fleet view | "Connected Devices" table (live connections: username, device, version, platform, IP, timezone, connected since), status badges, "Terminate"; "Unregistered" section for lobby devices with "Register" (assign to user) | Online / Online (outdated) / Offline / Unregistered |
| S20 | Bulk Device Upgrade (campaigns) | Accounts → Bulk device upgrade → "New Campaign" | Fleet agent upgrade | Targeting: "Upgrade older versions", "Reinstall current versions", "Downgrade newer versions", "Force upgrade running"; "Select devices..." rule builder (All of / At least one of / Not all of / None of; "Add rule", "Add group"); per-device status: Not seen, Ineligible, Pending, Request made, Update failed, Update confirmed, Incompatible OS; "Automatic Upgrade Campaign" toggle in Settings (runs on restart, ≥24h apart) | Running campaign; none; tenant admins cannot see this page |
| S21 | Job logs (Job History) page | Menu → Backup Job Reporting → Job logs | Search all jobs | Search (date range, user, custom SearchClause builder: field/operator/value with AND/OR groups), rows with status, classification, user, device, PI, vault, start/end, size; red "Cancel" on running; export report | Running, completed, filtered, empty **[inferred]** |
| S22 | Job detail / log viewer | S21 or S16 → job | Why a job failed | Job properties (status, times, totals, upload size, retry count, client version, snapshot ID); log entries filterable by severity/text; aggregated warnings/errors (26.4.4); report export for support; Cancel; Abandon (API) | Success, Warning, Error, Quota Exceeded, Missed, Cancelled, Skipped, Abandoned, Running, Running (revived), In Progress (retry), Not Yet Started |
| S23 | Recent activity page | Menu → Backup Job Reporting → Recent activity | 10-day at-a-glance grid | One row per user/device/Protected Item (sorted Username → Device → PI); badges: red "Offline" (device), yellow "Outdated" (device), red "No Schedule" (PI); Activity timeline oldest→newest, one block per day with job count, green = all OK, red = any failed, striped = mixed, blank = none yet today; click block → job search for that day; job filter; "show deleted Protected Items" | Mixed, all-green, blank-today |
| S24 | User Policies list | Menu → Policies (Settings area) | Policy objects | Rows with name, user count; triple-dot menu: "Make default", edit, delete; Advanced: "Edit raw policy" | Default marked |
| S25 | Policy editor | S24 → policy | Enforce defaults/restrictions | Sections: Summary; Storage Vaults ("Prevent Storage Vault actions", "Restrict Storage Vault types", "Automatically create Storage Vault for new devices" incl. "User controlled", "Storage Vault Selection Rules consider" Associated vaults / All vaults); Protected Items ("Prevent Protected Item actions", "Default Protected Item" + "Update existing devices", "Restrict Protected Item type"); Default settings (5-state: System default / Optional default on / Optional default off / Always ON / Always OFF for "Skip if already running", "Allow administrator to reset password", "Allow administrator to view file names remotely", "Require user to change password at next login"; retry count/time; "Random job delay"); Mandatory file and folder exclusions; Default email reports ("Policy default"); Default backup schedules (new PIs only; target vault or selector); Retention ("Keep all backups forever", "Enforce this retention policy for all Storage Vaults", "Prevent overriding retention policies on Protected Items", "Allow users to set the Object Lock duration"); App/account controls (`PreventOpenAppUI`, `RequirePasswordOpenAppUI`, `PreventOpenWebUI`, `PreventChangeAccountPassword`, `PreventChangeEmailSettings`, `PreventChangeAccountName`, `PreventDeleteSingleSnapshots`, `PreventViewDeviceNames`, `HideAppImport`, `HideAppVersion`); vault key rotation hours | New; editing default; **[gap]** validation |
| S26 | Settings page (index) | Menu → System → Settings | Server config | Sections: Serial number; Network (listen addresses, SSL/Let's Encrypt); Session options (timeout 30 min default, max 2880); Audit logging; IP rate limiting ("License and Access"); Admin accounts; System Self-Backup; Integrations; System branding; Management Console (login URL, "Keep job logs for: XX day(s)", Missed backups toggles, DB check level, replication); Storage Template; Email reporting; Backup Agent branding; Backup Agent downloads; Tenants; Storage Gateway; Cluster Manager; "Save changes" (some restart the service) | Saved; restart-pending |
| S27 | Settings — Email Reporting | S26 → Email reporting | Outbound mail | "Deliver via": None / MX Direct / SMTP server (Send as name, Send as email, server, port, username, password, Security SSL/TLS 465 / STARTTLS 587 / Plain 25, accept invalid certs) / Gmail account; custom HELO; Comet-Hosted: Default (AWS SES + "Validate DNS Records" for `cb-dmarc.<domain>`) or Custom SMTP ("Test SMTP Credentials"); global default report rules for all users | Configured / none |
| S28 | Settings — System Branding | S26 → System branding | Console white-label | "Page title text", header Image (jpg/gif/png/svg), Header color, Accent color, Tab icon (.ico), blog-link option, custom JavaScript/CSS (26.8.2) applied to login + all tenants | Default / branded; "Can edit branding" gated |
| S29 | Settings — Backup Agent branding + Codesigning | S26 → Backup Agent branding | Agent white-label and build | Product name (also install folder/service names), Company name, "Comet Storage name", Help URL (embedded frame or popup), Default Server URL, Registration URL, icons (.ico/.icns, 512² PNG auto-converted), EULA, Logo image 100×32, Tile image 150×150 + hex background, Background Logo hide, App icon 256²; codesigning: Windows Authenticode via AWS KMS / Google Cloud KMS / Azure Artifact Signing (26.8.9) or cert; macOS signing/notarization; "Software Build Role" MaxBuilders | Unsigned (MSI hidden) / signed |
| S30 | Download backup agent page | User menu / login page → "Download backup agent" | Get installers | Per-platform rows (Windows EXE x86/x64, Windows MSI x64 only when codesigned, macOS pkg arm64/x86_64, Linux .run/.deb/.rpm, Synology SPK DSM6/7); "Copy (as cURL)", "Copy (as wget)"; `/dl/<platform-id>` URLs; "Options" (enable public download) | Public / disabled; MSI hidden |
| S31 | Settings — Tenants | S26 → Tenants (Self-Hosted, 21.12.0+) | Isolated reseller zones | Tenant list; Hostname list (plain domain, must resolve + be in network settings); per-tenant: admin accounts, branding, email return address, policies, Storage Template, Cluster Manager, agent downloads; "Transfer user across tenants" ("New Tenant" on user profile) | Active / IsSuspended |
| S32 | Settings — Admin accounts | S26 → Admin accounts | Operators and permissions | Username, Password (hosted: ≥12 chars), "Allow login via" (password / +TOTP / +WebAuthn), IP allowlist (config-file only), Policies = permission flags (edit settings incl. branding/email/remote storage/webhooks/cluster/external auth/agent downloads; view dashboard history/info; shutdown/restart; change password; "Create and use personal API tokens"; vault create/delete/custom/show-cloud-branding; "Write access"; AllowedUserPolicies; restricted provider list); External authentication sources (LDAP, OIDC) | No read-only role exists (KB) |
| S33 | Storage Templates / Storage Gateway / Storage buckets | S26 → Storage Template; Menu → Storage Gateway → Storage buckets | Where data lands | Template: name (user-visible), Address Local/Remote (gateway creds), Type (cloud provider + master key), "Object Lock / Lock days", "prevent users from viewing the actual storage type", quota; Buckets page: list with size TiB/GiB, "Delete", "Add a new bucket" (random name/key shown once) | Template vs custom; Object Lock on/off |
| S34 | Settings — Integrations | S26 → Integrations tab | PSA/RMM hooks | "PSA & Alerting Services": Syncro (Syncro Domain, API token; needs Asset List/Search + RMM Alerts permissions), NinjaOne (Base URL, Client ID/Secret, "Authorize with NinjaOne", Tickets toggle, organization "Load"), ConnectWise (26.8.1); "Sync now" | Authorized / not |
| S35 | Settings — Webhooks / Audit logging / License & Access / Self-Backup | S26 subsections | Event export and ops | Webhooks: URL, CustomHeaders, Level, WhiteListedEventTypes, PayloadFormat raw/slack; Audit log: Key, File Name (year+dayofyear.log), Allow Event Types, Data Details full/none, Log Rotation days (Advanced plan); IP rate limit rules (MatchRegex, BytesPerSecond); System Self-Backup targets (Location, Schedule+tz, Retention, Encryption key, Compression Faster/Small, exclude job DB/logs), "Backup now" | Enabled / disabled |
| S36 | System Logs / About this System / Replication | Menu → System | Server health | System Logs (daily files, C:\ProgramData\Comet\logs or /var/log/cometd); About: version+codename, start time, "Serial number valid offline until", feature list green/red; Advanced: Shut down, Restart, "Edit raw settings"; Replication: status, deletion mode None / Partial / Complete | Healthy / licence-offline |
| S37 | Device News / Client bulletin | Menu → News; Reports → "Send a client bulletin email" | Push notices to agents / users | News entries shown in agent; bulletin subject+body plain text to all users; Advanced "Remove all" | Empty / entries |
| S38 | My Account (admin) | Top-right → My Account | Own credentials | "Two-factor authentication (TOTP)" toggle + QR; "Two-factor authentication (FIDO2 WebAuthn)" + "+" → "WebAuthn token saved"; personal API tokens (26.8.3) list/create/revoke | Enabled / disabled |
| S39 | Customer Web Portal (end-user web UI) | Console URL, log in with user creds (`#/u/...`) | Self-service without the desktop app | `#/u/protected-items`, `#/u/storage-vaults` (same dialogs as S11/S15), job history, run backup/restore on live devices, "My account" (password, TOTP), news | Policy may block (`PreventOpenWebUI`) |

### Desktop agent (Windows end-device side)

| ID | Screen | How you get there | Purpose | Key components | States seen |
|---|---|---|---|---|---|
| A01 | Installer | Run branded EXE/MSI | Install service + UI | Branded wizard: EULA (optional), install dir, shortcuts, tray icon; silent: `/S /LOBBY` or `/S /CONFIGURE=user:pass` or `/S /TOKEN=`; `/SERVER=`; GPO via MSI | Interactive / silent; SmartScreen if unsigned |
| A02 | Login / registration screen | First launch, or "Disconnect" | Bind device to a user profile | Username, Password; "Advanced Settings" reveals Server field (prefilled from branding Default Server URL); "Registration URL" link for self-signup if branded; lobby/unregistered waiting state; `backup-tool login status` states 0–4 ("Logged out" … "Unregistered device for the supplied credentials") | Logged out; waiting in lobby; device-limit error; suspended account |
| A03 | Main window — Backup tab | App open → "Backup" in left bar | List and run Protected Items | "Protected Items" header with hover "Add" (+); rows (name, type icon, last job status [inferred]); click row → "Backup Now" button; double-click/right-click → Edit, Delete; clicking a Protected Item reveals its Job logs; running job progress shown in main window (BytesDone/ItemsDone/ItemsTotal counters) | Empty (no items); filled; running; policy-locked (no Add/Edit/Delete) |
| A04 | Add / Edit Protected Item wizard | A03 → Add | Build a backup set | Pages: Name + Type (Files and Folders etc., restricted by policy); Items (Plus button → Choose files, drag-drop; Include/Exclude filters glob/regex); "Take filesystem snapshot"; Commands; Schedules; Retention; "Save" | New; edit; "Backup now?" after save [inferred] |
| A05 | Choose files dialog | A04 Items → + | Pick paths | Drive/folder tree with checkboxes, live estimated size (pre-dedup), drag-drop | Loading tree; large selection |
| A06 | Schedule editor | A04 → Schedules → Add | As S12 | Same fields as S12 (frequency, vault, event triggers, retry, limits, throttle schedule) | — |
| A07 | Retention editor | A04 → Retention; or vault | As S13 | "(default)" vs rules; same range types | — |
| A08 | Backup Now dialog | A03 → Backup Now | One-off run | Choose destination Storage Vault; advanced options (force rescan etc. [inferred from BackupJobAdvancedOptions]) | — |
| A09 | Job logs / progress view | A03 → click item; History | See what happened | Job list per item (status badge, start/end, size, uploaded); log entries with severity; running job: progress counters, Cancel | Running; retrying ("In Progress (retry)"); completed |
| A10 | Restore wizard | Left bar → "Restore" | Get data back | Step 1 Storage Vault → Step 2 Protected Item (expand to pick older snapshot; "Search" button; "Show items from other devices" + Refresh; "Show unknown items" for deleted PIs; right-click snapshot → "Delete this snapshot") → Step 3 files (all selected or "Choose files"; EFS files green) → Step 4 destination: custom path or "Restore to original paths" (Files and Folders only); Overwrite: "If the restored file is different" / "If the restored file is newer" / "Always" / "Never (skip existing files)"; Restore types: Files and Folders, Simulate restore only, Program Input, Compressed archive (zip/tar/tar.gz/tar.zst/sqfs), Granular, cloud/remote destination (26.8.3) | Loading snapshots; empty vault; locked vault |
| A11 | Account tab | Left bar → "Account" | Vaults, devices, identity | "Storage Vaults" list (+ Add; right-click Edit; Advanced submenu: "Apply retention rules now", "Rebuild indexes"); registered devices list; buttons "Account", "Import", version; Help tab if branded | — |
| A12 | Add Storage Vault dialog | A11 → + Add | New repo | Choose a Storage Template (request from server) or "Custom" (local path, SFTP, S3-compatible, SMB, FTP, WebDAV, Azure, B2, Storj…) | Template / custom; policy may forbid custom |
| A13 | Account dialog | A11 → "Account" | Identity settings | username, change password, privacy ("Allow administrator to reset password" / "view file names" visible to user), language, timezone, email report addresses; 2FA via web portal | Policy may lock password/email/name |
| A14 | Tray icon + exit menu | System tray | Background control | Open UI; exit options: Close (backups continue), "Lock" (log out of UI, backups continue), "Disconnect" (log out and stop schedules); tray can be disabled at install; news/bulletin surfacing [inferred] | Locked / disconnected |
| A15 | Throttle / runtime settings | Inside schedule (S12), not a Settings tab | Bandwidth + resource limits | "Limit Storage Vault Speed" with time-of-day/day-of-week table (26.8.5); 1 disk thread; temp-files-vs-RAM; prevent-sleep is automatic (18.3.9) | — |

Count: 39 console + 15 agent = 54 distinct screens (asked for 25–40; the extra rows are the Settings sub-pages, kept separate because each maps to a distinct feature area).

---

## 3. USER FLOWS

Click counts are happy-path estimates from documented steps; **[est]** where the docs don't give every click.

### F01 — Enroll a device via RMM silent install + lobby registration (Comet's recommended high-volume path)
Goal: agent on the PC, bound to the right customer account, with no credentials in the RMM script.
1. Pre-reqs once: S29 brand + codesign, S25 Default Policy (default Protected Item, default schedules, retention), S33 Storage Template, S04 users created (or created later).
2. RMM script (Syncro): download `https://<console>/dl/2` → run `install.exe /S /LOBBY /SERVER="https://<console>"` → delete installer.
3. Agent connects in lobby; console S19 shows it under "Unregistered" (event `SEVT_DEVICE_LOBBY_CONNECT`).
4. Admin: S19 → device → "Register" → pick user profile → confirm (`AdminInstallationDispatchRegisterDevice`).
5. Device becomes Online under that user (S06); policy auto-creates the Storage Vault (template) and the Default Protected Item + schedules; first scheduled run happens or admin fires S08.
Edge cases: unsigned installer fails silently under AV/SmartScreen; lobby membership is not persistent across reboot on macOS (Windows re-joins); device-count quota rejects registration; same hardware ID on cloned images → "multiple devices detected as one"; a device that reboots before registration must re-join lobby (`backup-tool login join-lobby`).
Clicks (admin side, after install): Devices → device → Register → choose user → confirm = **4–5**.

### F02 — Enroll a device with pre-seeded credentials or install token (no admin follow-up)
1. Create the user (S04) or `AdminAddUser`.
2. Either `install.exe /S /CONFIGURE=user:password` (credentials exposed in script) or `AdminCreateInstallToken` → `install.exe /S /TOKEN=<token>` (single-use, expiring).
3. Device registers itself on first service start; shows Online in S06; defaults from policy apply.
Edge cases: wrong credentials leave the device "Unregistered device for the supplied credentials" (`login status` = 4) → fix with `backup-tool login prompt`; expired/used token; re-running installer with a different brand name leaves an old folder.
Clicks: **0** in console after user creation (3–4 to create user).

### F03 — Enroll a device by hand from the console (one-off)
1. S03 → "Add user" → S04 (username, password, policy, vault template) → Save (3 clicks + typing).
2. User menu → "Download backup agent" (S30) → pick Windows → run A01 interactively (Next ×3–4).
3. A02 login (server prefilled if branded) → Sign in.
4. A03 → Add Protected Item (or wait for policy default) → run.
Edge cases: MSI download absent until codesigning configured; Let's Encrypt/SSL misconfig blocks agent connect; user's "Allow administrator to reset password" decision is permanent if turned off.
Clicks: **~12** end to end.

### F04 — Create a Protected Item with schedule and retention
Path A (agent): A03 → Add → A04 Name/Type "Files and Folders" → Items (+ → A05 pick folders; add Exclude filter) → leave "Take filesystem snapshot" on → Schedules → Add (A06: Daily 20:00, vault = the template vault; tick "If the last job was Missed"; "Skip if already running"; retry 1×/30 min) → Retention → add rules (A07: "Last 14 backups"; "Last 13 backups, at most one per week"; "Last 12 backups, at most one per month") → Save → Backup Now.
Path B (console, device must be Online): S10 → add → S11 same tabs → Save; or push via policy "Default Protected Item" + "Update existing devices".
Edge cases: schedule vs retention mismatch (keeping Sundays when backups run Mondays); network-share paths need UNC + service credentials and no VSS; policy mandatory exclusions merge in; "Prevent overriding retention policies on Protected Items" greys out the retention tab; "No Schedule" badge appears on S23 if no schedule saved; keep-forever grows RAM/index.
Clicks: **~18–22 [est]**.

### F05 — A scheduled backup run (and the missed/retry paths)
1. Service evaluates ScheduleConfig in the device's timezone (+ RandomDelaySecs); job created `JOB_STATUS_RUNNING_ACTIVE`, event `SEVT_JOB_NEW`; live progress streamed to console (Counter, BytesDone, ItemsDone/Total).
2. Order: Schedule Before → PI Before → Vault Before → VSS snapshot → chunk/dedup/encrypt/upload → Vault After → PI After → Schedule After → retention pass per "Apply retention policy after backup" cadence.
3. Completion → `SEVT_JOB_COMPLETED`; status Success / Warning / Error / Quota Exceeded; immediate email report if a rule matches; PSA alert raised or cleared.
Missed path: server generates `Missed` (7004) when a scheduled time passed without a job (`GenerateMissedBackupEvents`, `NoMissedBackupEventsBefore`); "If the last job was Missed" fires a catch-up when the device reconnects; "When PC starts" also fires on wake.
Failure path: with retry enabled the job sits "In Progress (retry)" then ends as Warning on success; "Cancel if still running after" → Cancelled; overlapping → Skipped; crash → Abandoned, later "Running (revived)" if it reports again.
Edge cases: vault locked by another device; quota hit mid-upload leaves chunks in vault; low-power device defers retention; Object Lock prevents deletion inside lock window.
Clicks: 0 (scheduled); manual = A03 item → "Backup Now" → vault → Start = **3**.

### F06 — Find out why a backup failed
1. Entry points: immediate failure email (full job log inline), Syncro RMM alert, S23 red day block, or S21 search Status ≠ Success.
2. S23 → click red block → S21 filtered to that day → row → S22.
3. S22: read status, aggregated warning/error groups, filter entries by severity, totals; for VSS errors note the 0x code and run `vssadmin list writers`; for "Cannot proceed - Locked by device" find the locking Device ID in S06; for "Reindex Needed!" run S07 Advanced → Reindex then "Apply retention rules now".
4. If stuck Running: Cancel; if Cancel ignored, S07 "Update software" to kill the process, then Reindex; or mark Abandoned via API.
Edge cases: device Offline → no live actions, only logs; job logs purge after "Keep job logs for" days (default 90); log cap removed 26.4.4; retention errors surface on the vault (`RetentionError`) not the job list.
Clicks: email link → job = **1–2**; console from scratch = **3–4**.

### F07 — Restore files to original or alternate location
1. Agent: left bar "Restore" → A10 Step 1 pick vault → Step 2 pick Protected Item, expand to pick snapshot date or "Search" filename → Step 3 "Choose files" tick items → Step 4 "Restore to original paths" or browse to custom path; overwrite rule → Restore.
2. Console (device Online): S07 "Remotely restore" (same steps; admin needs "Allow administrator to view file names" to browse).
3. Alternatives: "Compressed archive" (zip/tar/…) restore type; "Simulate restore only" for test; restore to cloud/remote location (26.8.3).
Edge cases: dedup rehydration needs free space; EFS files restored encrypted; permissions not remapped on mac/Linux; snapshot index download is slow for multi-TB vaults; `OverwriteForcePermissions`; vault locked → wait or unlock.
Clicks: **6–8**.

### F08 — Replace a dead PC (restore to a new device)
1. Install agent on the replacement, log in with the same user profile (lobby or credentials).
2. S06 confirm new device "Online".
3. On the new device A10 → tick "Show items from other devices" → Refresh → pick the old device's Protected Item → restore to original paths.
4. Recreate the backup set: S10 old device's item → "Copy" → new device → "Paste" (or policy default item appears automatically).
5. S06 old device → Revoke (stops schedules and billing).
Edge cases: user password unknown and admin reset disabled → cannot proceed without the password; old device reappearing later gets a catch-up charge; policy `PreventViewDeviceNames` hides other devices; vault "Show other device vaults" needed if old device used a device-specific vault.
Clicks: **~10 [est]** plus restore wizard.

### F09 — Decommission a device
1. Optional: final backup (S08).
2. S06 → device → Revoke (`AdminRevokeDevice`); its Protected Items are deleted and the Storage Vault's own retention takes over the orphaned snapshots (GFS recipe uses a 30-day vault default for this).
3. Optional: S07 "Uninstall software" while still Online (silent `Uninstall.exe /S /ISDELETECREDENTIAL=yes` otherwise).
4. If the whole customer leaves: Actions → Suspend (still billed) or Delete user (job history removed; `AdminDeleteUser`), then delete bucket (S33) after retention/Object Lock expire.
Edge cases: uninstalling without revoking keeps billing; deleting user then re-registering same Device ID triggers back-charges; Object Lock blocks bucket deletion until window ends.
Clicks: **3–5**.

### F10 — Bulk policy change across a tenant
1. Log in on the tenant hostname as tenant admin (or top-level).
2. S24 → policy → S25 edit (e.g. add mandatory exclusion, change default schedule, enforce vault retention) → Save; `SEVT_POLICY_UPDATED`.
3. Changes to settings apply to all users assigned that policy on their next profile refresh (S07 "Refresh profile" or auto). Default Protected Item changes reach existing devices only with "Update existing devices"; default schedules apply to new Protected Items only (not retroactive).
4. To move users between policies: S03 select → user → S17 dropdown → Save (per user; API `AdminSetUserProfile` for bulk; "Make default" affects new users only).
Edge cases: users marked "(Custom)" diverge from the policy; some restrictions must be re-enabled by the end user in the client; tenant-scoped policies reset on tenant transfer; no built-in multi-select "apply policy to N users" in UI (docs) → API.
Clicks: **4–6** for the policy edit; per-user reassignment 3 each.

### F11 — Set up alerting and reports for an MSP
1. S27 configure delivery (SMTP), send test.
2. S27 global rules: "Immediate job notification" with Status not equal Success → ops mailbox; "Summary table" Daily 07:00 with "Time since job start" ≤ 24 h; weekly "Grouped status report".
3. Per customer: S18 "Email backup reports to this account" + addresses; or S25 "Default email reports" so new users inherit.
4. S34 Syncro: domain + API token → alerts auto-open on failure and auto-clear on next success; S35 webhook (slack format) for a channel.
Edge cases: no first-class "device not seen for N days" alert — only the S23 "Offline" badge and Missed job events; CheckCentral/Backup Radar are the documented way to catch silence; MX Direct mail lands in spam; recipients capped per user list.
Clicks: **~10–15 [est]**.

### F12 — Fleet agent upgrade (bonus)
S20 → New Campaign → "Upgrade older versions" → optional device filter → Start; watch per-device status; or S35 "Automatic Upgrade Campaign". Edge: tenant admins cannot run campaigns; Synology DSM7 no remote upgrade; "Update software" also kills a stuck job. Clicks: **3–4**.

---

## 4. COMPONENTS

| Component | Variants / states | Used on |
|---|---|---|
| Data table with live filter, "Show ## entries" pager, column picker, export (clipboard TSV / CSV / XLSX) | grouped vs flat; selection for bulk actions | S03, S19, S21, S16, S33 |
| Status badge (device) | Online (green), Online (n) (double connection + Terminate), Online (outdated) (yellow), Offline (red), Unregistered (lobby) | S06, S19, S23 |
| Status badge (job) | Success, Warning, Error, Quota Exceeded, Missed, Cancelled, Skipped, Abandoned, Running, Running (revived), In Progress (retry), Not Yet Started; ranges 5xxx/6xxx/7xxx | S16, S21, S22, S23, A03, A09, email |
| Day-block activity timeline | green all-ok / red any-fail / striped mixed / blank none; count label; click → filtered job search | S23 |
| Attribute badges | "Offline", "Outdated" (device); "No Schedule" (Protected Item) | S23 |
| Live job progress | counter-based (BytesDone, ItemsDone/ItemsTotal), inactivity notice "This job has become inactive, or is experiencing communication issues"; red Cancel | S21, S22, A03, A09 |
| Log viewer | severity filter, text filter, aggregated repeated warnings/errors, export report, full log inline in immediate email | S22, A09, email |
| Search clause builder | field / operator (str_eq, int_lte, regex…) / value; AND/OR/NOT groups, nesting; reused as job search, email report filter, upgrade-campaign device filter | S21, S18, S27, S20 |
| Schedule editor | frequency tabs (Hourly…Periodic, Once only), day/time pickers, Offset/Random delay, runtime window, event-trigger checkboxes, retry, limits, throttle time-table | S12, A06, S25 (defaults), S35 self-backup, S27 report schedule |
| Retention rule list | mode toggle "Keep all backups forever"; add-range rows with type-specific inputs (count, days/weeks/months/years, weekday, month-day, date); "(default)"/"(X rules)" summary | S13, S15, S25, A07 |
| Tabbed modal dialog | PI: general/auth/items/commands/schedule/retention; Vault: general/storage/commands/encryption/associated-devices; deep-linkable `/<tab>` | S11, S15, S09, A04 |
| Wizard (stepper) | Restore 4 steps; Add Protected Item pages; installer | A10, S09, A04, A01 |
| File/folder tree picker | checkboxes, drag-drop, live size estimate, green EFS rows, "Show unknown items", snapshot expander | A05, A10 |
| Live device action list | enabled only when Online; Advanced section reveals destructive items; "Multiple actions" queue | S07 |
| 5-state policy mode select | System default / Optional default on / Optional default off / Always ON / Always OFF | S25 |
| "Advanced options" global toggle | reveals raw editors, CSV import, bulk replace, delete snapshot, shutdown/restart | user menu → S03, S05, S09, S24, S36, S37 |
| Actions dropdown (user) | Suspend/Unsuspend, Reset Password, Disable 2FA (TOTP), New Tenant | S05 |
| Triple-dot row menu | Make default, edit, delete, raw edit | S24 |
| Copy / Paste (top-bar sticky action) | Protected Item clipboard across devices/users | S10 |
| Toggle + QR enrolment | TOTP; WebAuthn "+" → "WebAuthn token saved" | S38, S39 |
| Branding preview surfaces | header title/image, colors, favicon, custom CSS/JS; per-tenant by hostname | S28, S01, S30, agent |
| Email templates | Immediate (full log), Summary table (one row per job: PI type, Device, Status…), Grouped status, Recent activity (10-day grid) | S18, S27 |
| Toasts / inline banners | "Save changes" confirmations; restart-required notice; "WebAuthn token saved"; **[gap]** exact toast behaviour not documented |
| Tray icon menu | Open, Close / Lock / Disconnect semantics | A14 |

---

## 5. INFERRED DATA MODEL

Confidence: **H** = field-level evidence in API data structures; **M** = documented behaviour, structure inferred; **L** = inference.

| Entity (Comet name) | Key fields | Relationships | Evidence | Conf |
|---|---|---|---|---|
| **Tenant** (`Organization`) | Name, Hosts[], IsSuspended, Branding, Email (return address), SoftwareBuildRole, ConstellationRole, RemoteStorage[] (templates), PSAConfigs[], WebhookOptions{}, AuditFileOptions{}, ExperimentalOptions | 1→N AdminAccount, UserProfile (`OrganizationID`), GroupPolicy (`OrganizationID`), UserGroup; shares system-wide Storage Gateway, SMTP deliverability, upgrade campaigns, replication | S31, API Organization, SEVT_TENANT_* | H |
| **AdminAccount** | Username, PasswordHash, AllowLoginVia (password / +TOTP / +WebAuthn), TOTP secret, WebAuthn credentials[], IPWhitelist regex, Permissions (`AdminUserPermissions` ~25 flags incl. PreventWriteActions, AllowedUserPolicies[], provider restrictions), personal API tokens[], OrganizationID | belongs to Tenant or top-level; External auth sources (LDAP/OIDC) | S32, S38, API | H |
| **UserProfile** (customer account; security boundary) | Username/AccountName, PasswordFormat/Hash, PasswordRecovery (empty = admin reset disabled), AllowPasswordLogin, AllowPasswordAndTOTPLogin, TOTPKey, RequirePasswordChange, LocalTimezone, LanguageCode, OrganizationID, GroupID, Emails[], SendEmailReports, OverrideEmailSettings{email→UserCustomEmailSettings}, Destinations{vaultId→DestinationConfig}, Sources{guid→SourceConfig}, BackupRules{guid→BackupRuleConfig}, Devices{deviceId→DeviceConfig}, IsSuspended, LastSuspended, AllProtectedItemsQuotaEnabled/Bytes, MaximumDevices, Quota* per engine, PolicyID + Policy (effective copy), ServerConfig, AutoStorageTemplateGUID, CreateTime, CreationGUID, UserExtra{}, AdminExtra{} | 1→N Device, ProtectedItem, Vault, BackupRule; N→1 Policy, UserGroup, Tenant; keys: L-key hash stored, R-key client-only, E-key stored encrypted by R | S05, API UserProfileConfig, encryption page | H |
| **Device** (`DeviceConfig`) | DeviceID (hardware-derived; `DeviceIdentificationID`), FriendlyName, RegistrationTime, PlatformVersion{os,arch,version,distribution,build}, ClientVersion, DeviceTimezone, Sources{guid→SourceBasicInfo} (private PIs), SyncroUUID, NinjaOneNodeId | N→1 UserProfile; owns ProtectedItems (`OwnerDevice`); shares all Vaults in the profile; `LiveUserConnection` (IPAddress, ConnectionTime, ReportedVersion, AllowsFilenames) when Online; lobby = `InstallationListActive` entry | S06, S19, API DeviceConfig | H |
| **ProtectedItem** (`SourceConfig`) | GUID, Engine (`engine1/file`…), Description, OwnerDevice, CreateTime/ModifyTime, EngineProps{} (paths, include/exclude filters, snapshot flag, rescan, extra perms…), PreExec/ThawExec/PostExec[], OverrideDestinationRetention{vaultId→RetentionPolicy}, PolicySourceID, ExistingUserUpdate, Statistics{LastStartTime, LastBackupJob, LastSuccessfulBackupJob}, Size | N→1 Device; 1→N BackupRule; retention override per Vault | S11, API SourceConfig | H |
| **BackupRule / Schedule** (`BackupRuleConfig` + `ScheduleConfig[]`) | Description, Source (PI guid), Destination (vault id or selector LATEST/ALL), Schedules[]{FrequencyType, SecondsPast, Offset, RandomDelaySecs, SelectedMonth/Day, RestrictRuntime From/To, RestrictDays}, EventTriggers{OnPCBoot, OnPCBootIfLastJobMissed, OnLastJobFailDoRetry, RetryCount, RetryTime}, BackupJobAdvancedOptions (skip-if-running, time limit, vault speed limit + schedule, 1 disk thread, temp files, allow zero files), PreExec/PostExec, PolicyBackupRuleID/Selector | N→1 ProtectedItem; N→1 Vault | S12, API BackupRuleConfig/ScheduleConfig | H |
| **RetentionPolicy** | Mode (KEEP_EVERYTHING / DELETE_EXCEPT), Ranges[]{Type (900–911), Jobs, Days, Weeks, Months, Years, Timestamp, WeekOffset, MonthOffset, YearOffset} | lives on Vault (DefaultRetention), on PI per-vault override, on Policy (DefaultStorageVaultRetention + enforce flags) | S13, API | H |
| **StorageVault** (`DestinationConfig`) | ID, Description, DestinationLocation{Type 1000–1012 + backend fields, Tag}, CreateTime/ModifyTime, SuspendedTime, RepoInitTimestamp, EncryptedEncryptionKey + method (A-key wrapped by E-key), StorageLimitEnabled/Bytes/ID, DefaultRetention, RebrandStorage, PreventUserEdit, RetentionError, AssociatedDevices[], Statistics{size, content, verification}, PreExec/ThawExec/PostExec, Object Lock days, "allow removal of deleted files" | N→1 UserProfile; shared by all its Devices (cross-device dedup); may be created from StorageTemplate; maps to a Bucket on Storage Gateway or cloud sub-prefix | S14/S15, API | H |
| **StorageTemplate** (`RemoteStorageOption`) | Name (user-visible), Address Local/Remote, Type/provider, master credentials, Object Lock lock days, hide-real-type flag, quota default | Tenant-scoped; UserProfile.AutoStorageTemplateGUID; policy `DefaultNewStorageVault` | S33 | M |
| **Bucket** (Storage Gateway) | name (random), access key (PSK hash), size, region | 1↔1 Vault of type COMET; pruned by Cluster Manager | S33, SEVT_BUCKET_* | M |
| **Job** (`BackupJobDetail`) | GUID, Username, DeviceID, SourceGUID, DestinationGUID, BackupRuleGUID, Classification (4001 backup … 4011 uninstall), Status (5000/6001–6004/7000–7007), StartTime, EndTime, RetryCount, SnapshotID, ClientVersion, Tags[], TotalDirectories/Files/Size/Chunks, UploadSize, DownloadSize, ConflictingJobID, CancellationID, Progress{Counter, SentTime, BytesDone, ItemsDone, ItemsTotal}, DestinationSizeStart/End; log entries (severity, text) stored for "Keep job logs for" days | N→1 Device, PI, Vault; produces 1 Snapshot on success | S21/S22, API | H |
| **Snapshot / restore point** (`VaultSnapshot`) | Snapshot id, EngineType, Source (PI guid), CreateTime, HasOriginalPathInfo, Tags[]; tree of `StoredObject`{name,type,size,mtime,...} | N→1 Vault; N→1 PI (incl. unknown/deleted PIs, other devices); deleted by retention pass or manual "Delete this snapshot" (classification DELETE_CUSTOM) | A10, API | H |
| **EmailReport** (`EmailReportConfig`) | ReportType (0 immediate, 1 summary, 2 grouped status, 3 recent activity), SummaryFrequency[] (ScheduleConfig), TimeSpan, Filter (SearchClause), Title | belongs to UserProfile per email address, to Policy (`DefaultEmailReports`), or server-wide; delivery via EmailOptions (Mode none/builtin/smtp/gmail) | S18, S27, API | H |
| **Alert / event** (`StreamableEvent`) | Type (SEVT 4000–4805), TypeString, Timestamp, Actor, OwnerOrganizationID, ResourceID, Data | fan-out: webhooks (WebhookOption{URL, CustomHeaders, Level, WhiteListedEventTypes, PayloadFormat}), WebSocket stream, audit log files (`AuditFileOptions`), PSA (Syncro alert create/clear; NinjaOne custom fields + tickets) | S35, S34, API | H |
| **Policy** (`GroupPolicy` → `UserPolicy`) | Description, OrganizationID, DefaultUserPolicy, CreatedDate/ModifiedDate; UserPolicy: ~45 fields (vault restrictions, StorageVaultProviders, DefaultNewStorageVault, DefaultStorageVaultRetention, EnforceStorageVaultRetention, PreventProtectedItemRetention, AllowEditObjectLockRetention, PI restrictions, ProtectedItemEngineTypes, FileAndFolderMandatoryExclusions[], DefaultSources{}, DefaultSourcesBackupRules{}, DefaultSourcesWithOSRestriction{}, DefaultBackupRules{}, DefaultEmailReports, Mode* 5-state ints, app/account Prevent*/Hide* flags, RandomDelaySecs, RotateStorageVaultKeysHours) | 1→N UserProfile (`PolicyID`); "(Custom)" = inline copy | S25, API | H |
| **UserGroup** | name, members[] | N→N UserProfile (one group per user), tenant-scoped | S03, API | H |
| **InstallToken** | Username, Server, Token, CreateTime, ExpireTime, Used | one-shot bind of Device→UserProfile | S06/F02, API | H |
| **UpdateCampaign** | Active, UpgradeOlder, ReinstallCurrentVer, DowngradeNewer, ForceUpgradeRunning, ApplyDeviceFilter, DeviceFilter (SearchClause), StartTime, TargetVersion; per-device UPDATESTATUS 0–6 | system-wide (not tenant) | S20, API | H |
| **Quota** (`Squota` shared quota + per-profile) | All-PI bytes, vault bytes, device count, engine instance counts; shared quota object attachable to many users | UserProfile, Vault | S05, S18, API AdminSquota* | M |

Relationship summary: Tenant ⊃ {Admins, Policies, UserGroups, UserProfiles}; UserProfile ⊃ {Devices, Vaults, BackupRules} and Device ⊃ ProtectedItems; BackupRule = (ProtectedItem × Vault × Schedule[]); Job → Snapshot in Vault; Events fan out to Email/Webhook/PSA/Audit. The **user profile, not the device, is the unit of isolation, billing identity and encryption key scope**; Comet's MSP guidance is one profile per device (or per small group sharing data for dedup).

---

## 6. FEATURE LIST (feed into features.csv)

Format: `feature | area | priority | how Comet does it | notes`

```
Silent install EXE | deployment | must | install.exe /S with /LOBBY, /CONFIGURE=user:pass, /TOKEN=, /SERVER=, /TRAYICON=disable, /SHORTCUT=disable | run from installer dir; quoting errors fail silently
MSI package for GPO/RMM | deployment | should | x64 MSI from 25.6.0; only offered when codesigning configured | /dl/22 route
Lobby / remote registration | deployment | must | agent connects unauthenticated, appears "Unregistered" on Devices page; admin clicks Register → user | lobby join not persistent on macOS; SEVT_DEVICE_LOBBY_*
Single-use install token | deployment | should | AdminCreateInstallToken → /TOKEN=; expires; Used flag | keeps creds out of RMM scripts
Pre-seeded credentials | deployment | must | /CONFIGURE=user:pass or env vars on Linux | documented as "may be exposed"
Default server URL baked into installer | deployment | must | branding "Default Server URL"; /SERVER= override | branded build per tenant hostname
Installer codesigning | deployment | should | Authenticode via cert or AWS KMS / GCP KMS / Azure Artifact Signing; macOS notarization | unsigned → SmartScreen, silent-install failures
Download agent page with per-platform builds | deployment | must | "Download backup agent", /dl/<id>, Copy as cURL/wget, public download toggle | branded per tenant
RMM deploy script samples | deployment | should | PowerShell/Bash examples: download /dl/2, run /S /LOBBY, delete | Syncro-compatible pattern
Remote agent upgrade (single) | deployment | must | device action "Update software" (also kills stuck jobs) | backup.elevator service
Bulk upgrade campaigns | deployment | should | New Campaign: upgrade older / reinstall / downgrade; device filter rules; per-device status; Automatic Upgrade Campaign | tenant admins excluded
Remote uninstall | deployment | should | device action "Uninstall software"; silent Uninstall.exe /S /ISDELETECREDENTIAL=yes | credentials persist unless flagged
Device revoke | deployment | must | AdminRevokeDevice; deletes its PIs; stops billing and schedules | vault retention then governs orphans
Device rename / friendly name | console | should | default hostname; admin editable | PreventViewDeviceNames policy
Device limit per account | console | should | MaximumDevices quota; login from extra device fails | 0 = unlimited
Device identity by hardware ID | deployment | must | DeviceIdentificationID; override/entropy registry keys for clones | hardware change = new device + catch-up billing
Multiple devices per account sharing vaults | storage | should | all devices in profile see all vaults; PIs private per device | cross-device dedup; restore from other device
Copy/paste Protected Item between devices/users | console | should | "Copy" link → top-bar "Paste" | used in dead-PC replacement
Files and Folders backup engine | agent | must | engine1/file; tree picker; drag-drop; live size estimate | only engine needed for workstation scope
Include filters (glob/regex) | agent | should | under a top directory; unlimited | 
Exclude filters (glob/regex) | agent | must | full-path match; unlimited; policy mandatory exclusions merge in | 
Built-in system exclusions | agent | must | $RECYCLE.BIN, System Volume Information, pagefile/hiberfil/swap, C:\Recovery, reparse points | 
VSS snapshot | agent | must | "Take filesystem snapshot" default on; off for network/FAT/exFAT | diagnose with vssadmin list writers
Rescan unchanged files | agent | could | force re-chunk ignoring size/mtime | 
Extra NTFS attributes/ACLs/ADS | agent | should | "Back up extra system permissions and attributes" (24.6.2) | 
EFS handling | agent | could | backed up encrypted; key export confirmation | 
Network share with credentials | agent | could | "Log in to network share"; UNC conversion; no VSS | 
Pre/post commands at schedule, item, vault levels | agent | could | ordered Before/After + thaw hooks; cmd.exe | 
Scheduled frequencies | agent | must | Hourly, Daily, Weekly, Monthly, Yearly, Once only, Periodic (+Offset) | device-local timezone
Multiple schedules per item, each to its own vault | agent | should | BackupRuleConfig per (PI, vault) | 
Random job delay | agent | should | up to 300 min; policy-level too | fleet load spreading
Run on PC start / wake | agent | must | "When PC starts" (service start if uptime <10 min; wake since 23.6.0) | 
Catch-up if last job Missed | agent | must | "If the last job was Missed" fires on reconnect | the "run when device comes online" pattern
Missed-job detection server-side | alerting | must | GenerateMissedBackupEvents, NoMissedBackupEventsBefore; status Missed (7004) | emitted by server, not agent
Retry failed backup | agent | should | count + minutes (24.6.6); "In Progress (retry)"; ends Warning | 
Skip if already running | agent | must | status Skipped (7006); 5-state policy default | 
Job time limit | agent | should | "Cancel if still running after" → Cancelled | 
Battery / metered-connection rules | agent | could | NOT FOUND in docs or release notes | gap to decide; Comet prevents sleep during jobs instead
Runtime window / allowed days | agent | should | RestrictRuntime From/To + RestrictDays | 
Bandwidth throttle | agent | must | "Limit Storage Vault Speed" per schedule; time-and-day table (26.8.5) | no global Settings-tab throttle documented
Disk/RAM limits | agent | could | 1 disk thread; temp files instead of RAM | 
Prevent sleep during job | agent | should | automatic on Windows (18.3.9) | 
Content-defined chunking + dedup | storage | must | variable-size CDC, compress, encrypt per chunk; dedup across devices sharing a vault | restore rehydrates
Compression level | storage | could | COMPRESS_LVL_4 default | 
Client-side encryption, zero-knowledge | security | must | AES-256-CTR + Poly1305; PBKDF2-SHA512 → L/R keys; vault A-key wrapped by E-key wrapped by R-key | admin reset possible only via PasswordRecovery root
Admin password reset (opt-in, irreversible opt-out) | security | must | "Allow administrator to reset password" | tell user it is permanent
Admin may view filenames (opt-in, irreversible opt-out) | security | should | "Allow administrator to view file names"; gates remote restore browse | 
Vault key rotation | security | could | automatic rotation hours per policy; 26.8.9 for B2/Gateway | 
Storage vault types | storage | must | S3-compatible, Comet Storage Gateway (local disk), SFTP, SMB, NFS, FTP, WebDAV, Azure Blob, B2, Storj, Swift, local path | for CryoBackup: local/S3 first
Storage Templates (auto-provision vault per user/device) | storage | must | server-side recipe; per-vault scoped access keys; "(none)/System Default/Template" per user | one template → thousands of vaults
Direct-to-cloud vs via Gateway | storage | should | agent uploads straight to S3 or through self-hosted gateway (hybrid, hides cloud creds) | 
Storage Gateway (self-hosted on-prem store) | storage | should | Storage Role; buckets with random name/PSK; NFSv4 (26.8.7) | 
S3 Object Lock (compliance) | security | should | template "Object Lock / Lock days"; delete markers; "allow removal of deleted files" | ransomware story
Vault quota | storage | should | StorageLimitBytes; job errors at limit; chunks stay | 
Account quota (all PIs) | storage | should | AllProtectedItemsQuotaBytes; job stops | 
Shared quotas across users | storage | could | AdminSquota* | 
Retention rules (11 range types, OR-combined) | storage | must | Last N; Last N one per day/week/month/year; all in last period; newer than date; one per day/week/month/year for last X | GFS via 3 rules
Retention precedence | storage | must | PI-per-vault override → vault default → policy enforce/prevent-override | 
Retention pass scheduling | storage | should | Automatic (high/low power tables) / immediately / more / less often; manual "Apply retention rules now" | 
Keep-forever default with warnings | storage | must | vault default "Keep all backups forever" | index/RAM growth
Vault maintenance: reindex, unlock, deep verify, remeasure | storage | should | device actions; job classifications 4004/4006/4009/4010 | needed for lock/abandon recovery
Delete single snapshot | storage | could | Restore wizard right-click (Advanced) | PreventDeleteSingleSnapshots policy
Job status model (12 states, 3 ranges) | reporting | must | 5000 success / 6001–6004 running / 7000–7007 failed | 
Job classification model | reporting | must | backup, restore, retention, unlock, delete, remeasure, update, import, reindex, deepverify, uninstall | 
Live progress to console | console | should | counter, bytes, items; inactivity notice | websocket dispatcher
Abandoned job detection/revival | reporting | must | Abandoned (7007) then "Running (revived)"; auto-cleanup on next exclusive vault op or update | 
Job log viewer with severity filter | reporting | must | AdminGetJobLogEntries; aggregated warnings (26.4.4) | 
Job history retention | reporting | should | "Keep job logs for: XX day(s)" default 90 | 
Job search with clause builder | reporting | should | SearchClause AND/OR/NOT, string/int/bool operators, regex | shared with reports and campaigns
Recent Activity 10-day grid | reporting | must | per user/device/PI row; coloured day blocks; Offline/Outdated/No Schedule badges | the "silence is an alarm" view
Running-jobs count | console | could | 26.4.4 | 
Email: immediate job notification | alerting | must | full log inline; filter e.g. Status ≠ Success | 
Email: scheduled summary table | alerting | must | one row per job; schedule via normal scheduler; time-span filter | 
Email: grouped status report | alerting | should | jobs grouped by status over span | 
Email: recent activity report | alerting | should | 10-day grid by email | 
Email delivery methods | alerting | must | None / MX Direct / SMTP (ports 465/587/25) / Gmail; hosted: SES + DMARC DNS; test buttons | 
Report scoping: server-wide, per-policy default, per-user per-address | alerting | must | three layers; "Email backup reports to this account" gate | 
Admin test email / test report | alerting | should | AdminMetaSendTestEmail/Report | 
Offline-device alert | alerting | must | NOT first-class; only "Offline" badge + Missed events; third-party (CheckCentral, Backup Radar) fill the gap | CryoBackup should do this natively
Syncro RMM alerts | alerting | should | Settings → Integrations: Syncro Domain + API token; alert on failure, auto-clear on success; matched by SyncroUUID | own implementation against Syncro API
NinjaOne custom fields + tickets | alerting | could | OAuth app; 5 custom fields; ticket open/update/resolve | 
ConnectWise PSA | alerting | could | 26.8.1 | 
Webhooks | alerting | should | POST JSON per SEVT event; custom headers; whitelist; Slack payload format; retries | 
WebSocket event stream | console | could | /api/v1/events/stream with allowList | 
Audit log | security | should | event-stream file sink; event-type filter; rotation days; Advanced plan | 
Client bulletin email / device news | console | could | plain-text email to all users; news shown in agent | 
Multi-tenant (Tenants) | multi-tenant | must | hostname-bound isolated zones; own admins/branding/policies/templates/email return address | shared SMTP, gateway, campaigns
Transfer user across tenants | multi-tenant | could | "New Tenant" on profile; online devices get new login URL | 
User groups | multi-tenant | should | group users (customer sites) within a tenant; bulk move | 
Console white-label | multi-tenant | should | title/image, colors, favicon, custom JS/CSS | per tenant hostname
Agent white-label | multi-tenant | should | product/company name, icons, EULA, help URL, logo/tile, hide storage brand | rename changes install folder
Admin accounts with granular permissions | security | must | ~25 flags; Write access; allowed policies; provider restrictions | no read-only role (KB)
Admin MFA: TOTP + FIDO2 WebAuthn | security | must | My Account toggles; HTTP 449 challenge | 
Admin SSO: OIDC (Entra/Google/generic) + LDAP | security | should | External authentication sources; skip-MFA option | 
End-user 2FA (TOTP) | security | should | set by user in web portal; admin can only disable | 
IP allowlist per admin | security | could | regex in config file only | 
IP rate limiting | security | could | regex + bytes/sec rules | 
Session timeout | security | should | 30 min default, max 48 h | 
Personal admin API tokens | security | should | 26.8.3; no password/2FA sharing | 
"Login as user" (impersonate) | console | should | Advanced; AdminAccountSessionStartAsUser | 
Policies: defaults + restrictions | console | must | see S25; Default Policy for new users; "(Custom)" per user | 
Policy: default Protected Item pushed to devices | console | must | DefaultSources (+OS restriction); "Update existing devices" | 
Policy: default schedules | console | must | apply to new PIs only; vault selector latest/all/type | 
Policy: mandatory exclusions | console | should | merged into every Files and Folders item | 
Policy: enforce vault retention / prevent PI override | console | must | ransomware-resilience framing | 
Policy: restrict engines / vault types / vault actions | console | should | | 
Policy: lock agent UI | console | should | PreventOpenAppUI, RequirePasswordOpenAppUI, hide version/import, prevent account edits | 
Policy: 5-state defaults | console | should | System default / Optional on / Optional off / Always ON / Always OFF | 
End-user web portal | console | could | run backup/restore, edit items, history, 2FA | PreventOpenWebUI
Deep-linkable routes | console | should | hash routes to user/device/dialog/tab | email → job link
Export tables (CSV/XLSX/clipboard) | reporting | should | Users and job reports | 
Bulk user create via CSV | console | should | Advanced in Add user | 
Bulk replace vault addresses | storage | could | replication failover | 
Suspend account | console | should | still billed; stops access | 
Require password change at next login | security | could | | 
Restore wizard (vault → item → snapshot → files → destination) | restore | must | 4 steps; search; show other devices; show unknown (deleted) items | 
Restore to original paths | restore | must | Files and Folders only | 
Restore to alternate path | restore | must | custom path | 
Overwrite rules | restore | must | different / newer / always / never | 
Restore as archive | restore | should | zip, tar, tar.gz, tar.zst, sqfs | 
Simulate restore | restore | could | RESTORETYPE_NULL test | 
Restore to cloud/remote location | restore | could | 26.8.3 | 
Remote restore from console | restore | should | device must be Online; admin needs view-filenames right | 
Restore from another device's backup | restore | must | "Show items from other devices" | dead-PC path
Point-in-time snapshot picker | restore | must | expand item to list snapshots by date | 
File search across snapshots | restore | should | AdminDispatcherSearchSnapshots | 
Self-update of server | console | could | quarterly vs weekly tracks; hosted managed | 
Server self-backup | console | should | daily zip; targets; retention; encryption | DR of the console
Replication of console/gateway | console | could | one-way; deletion modes | 
Clustered bucket cleanup (Constellation) | storage | could | prune unused buckets | 
Timezone per user/device, IANA | console | must | default Etc/UTC; device auto-sets | display local, store UTC
Localisation (13 languages) | agent | could | per-user LanguageCode | 
Usage metering per device/booster | billing | should | fees start when a PI is added to a connected device; revoke stops; catch-up on re-register | prepaid credit model; 30-day cycle
Per-user storage usage view | billing | should | vault "Stored" Current vs Historic | 
Billing exports | billing | could | Credit Usage History CSV/XLSX with Device ID + Tenant ID | Account Portal (hosted)
Plan-gated features | billing | could | audit log, SSO, IP controls, MCP by plan | 
Public API (form POST → JSON) | console | must | every UI action is an API call; SDKs PHP/Go/Ruby/JS | CryoBackup: same principle
MCP server for AI tooling | console | could | /mcp/ SSE, basic auth, read-only unless header | 
Prometheus / collectd exporters | reporting | could | official sidecars | 
Telemetry to vendor | security | n/a | mandatory usage/licensing phone-home | CryoBackup: none
HIPAA/GDPR posture doc | security | should | self-hosted needs no BAA with vendor; BAA with storage provider | 
Tray icon + Close/Lock/Disconnect | agent | should | Disconnect stops schedules | 
Agent CLI (backup-tool) | agent | should | login status/prompt/disconnect/join-lobby; info device-id | 
Windows service account | agent | must | NT SERVICE\backup.delegate virtual account; elevator for updates | 
Proxy support | agent | should | HTTP_PROXY/HTTPS_PROXY, socks5; must support websockets | 
Windows Event Log entries | agent | could | event IDs 50–55 | 
Import from other backup products | agent | could | "Import / scan for other installed products" | 
Disk image / bare-metal, Hyper-V, VMware, Proxmox, M365, SQL, Exchange, MongoDB, MySQL, System State, Synology | agent | n/a | separate engines, per-endpoint add-on fees | out of CryoBackup workstation scope
```

---

## 7. WHAT CANNOT BE CLONED (skip rows)

| Item | Reason |
|---|---|
| "Comet Storage powered by Wasabi" ($6.99/TB) and the Impossible Cloud / Storj / IDrive e2 partner templates | Commercial partner arrangements; CryoBackup offers generic S3/local only |
| Comet-Hosted Management Console (AWS-managed instance, Account Portal, Custom Domain, SES-backed email) | Vendor-hosted service; out of scope for a self-hosted MSP tool |
| Licensing server, serial numbers, "offline/air-gap licensing", mandatory telemetry, prepaid credit billing, catch-up charges | Comet's commercial model; CryoBackup has no phone-home (project rule) |
| Official Syncro / NinjaOne / ConnectWise integrations, WHMCS/HostBill modules, MyClient, figaro, Backup Radar, CheckCentral, Gradient | Partner-owned integrations and products; CryoBackup may write its own Syncro alert hook against Syncro's public API but must not reuse Comet's |
| Codesigning identity (Comet's certificates / KMS workflow) | Must be C2's own cert; only the *pattern* (sign before silent deploy) carries over |
| Branding assets, product/release names (Comet, Voyager, Janus, Phoebe…), icons, EULA text, docs copy, videos | Owned content; replica-brand creates fresh assets |
| Non-file engines: Disk Image/bare-metal + WinPE recovery media, Hyper-V, VMware, Proxmox, Microsoft 365, Exchange, SQL Server, MongoDB, MySQL, System State, Synology SPK | Out of workstation-file scope; some depend on Microsoft ADK / vendor SDK licences |
| Comet's chunking/crypto implementation details beyond the public description | Only the published design (CDC + AES-256-CTR/Poly1305 + key wrapping) is reused as a *pattern*; implement with standard libraries (e.g. restic/kopia-style or libsodium), never Comet code |
| Constellation / Cluster Manager, Replication, Storage Gateway product | Platform features beyond a status page + poller; defer |
| MCP server, Prometheus exporter, SDKs | Comet-owned code; concept only |
| G2/Capterra review content, support-KB text | Not fetched (403) and copyrighted regardless |

---

## 8. HARD PARTS TO REPLICATE CAREFULLY

1. **The silence state machine.** Comet surfaces four distinct "nothing happened" signals and they come from different places: `Missed` jobs are *generated by the server* from the schedule (`GenerateMissedBackupEvents`), `Abandoned`/`Running (revived)` come from heartbeat loss, "Offline"/"Outdated"/"No Schedule" badges are computed on Recent Activity, and there is still **no native "device not seen for N days" alert** (third parties fill it). CryoBackup must own all of these server-side, treat each as an alarm (CLAUDE.md rule), and make the 10-day grid the default view. Get the timezone right: schedules evaluate in device-local time, Missed is judged by the server.

2. **Retention semantics.** Rules are OR-combined "keep" sets (anything matching none is deleted), with precedence PI-per-vault override → vault default → policy enforce/prevent-override, GFS built from three rules, keep-forever as the default, retention passes throttled by device power class, and Object Lock turning deletes into delete-markers. Wrong = silent data loss or runaway storage. Unit-test this exhaustively (project rule says classification logic is the product; retention is the other half).

3. **Key hierarchy with opt-in admin recovery.** Zero-knowledge (password → L-key for login, R-key client-only; vault A-key wrapped by E-key wrapped by R-key) *plus* an admin password-reset path that is opt-in and irreversibly opt-out, and a separate irreversible "admin may view filenames" flag. Designing this so an MSP can rescue a forgotten password without the server holding plaintext keys is the hard part; copy the structure, not the code.

4. **Policy layering and propagation.** Defaults vs restrictions vs 5-state "mode" settings; Default Policy applies only to new users; default Protected Items only to new devices unless "Update existing devices"; default schedules only to new items; "(Custom)" users diverge; tenant transfer resets policy. Every rule needs an explicit "does this change touch existing objects?" answer or MSPs get surprises.

5. **Live-dispatch channel.** Every remote action (run backup, restore browse, retention, update, uninstall, lobby register, progress) rides a persistent agent→server websocket; offline devices simply cannot be acted on, double connections need "Terminate", and stuck jobs are cleared by side effects (update software, next exclusive vault op). Build the channel, the Online/Online(n)/Outdated/Offline status, and idempotent queued actions before the UI.

6. **Identity: user-profile as security boundary, device by hardware ID.** Multi-device per profile gives cross-device dedup and dead-PC restore but also means any device in the profile can read any other's data; device identity from hardware IDs breaks on cloned images and hardware swaps (and drives billing). Decide early: one profile per endpoint (Comet's MSP recommendation) and a stable, override-able device ID.

---

### Gaps (not found in public docs)
- Console home/dashboard widget layout; Users list columns; Job logs page column set.
- Desktop agent Settings tab (no documented global bandwidth/proxy/startup settings; throttle lives in schedules, proxy via env vars).
- Battery / metered-connection scheduling rules (absent from docs and release notes; likely not a feature).
- Native "device offline for N days" alert (absent; only badges + Missed).
- Exact Add User dialog field list (video-only step page); exact toast/validation behaviour.
- support.cometbackup.com section index and G2 screenshots (HTTP 403).
