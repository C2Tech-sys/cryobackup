# CryoBackup competitor feature sweep — Acronis, Datto, Axcient, Veeam, UrBackup

Clean-room sweep, public sources only (vendor help centers, public docs, pricing/partner PDFs, forum threads, and — for UrBackup, GPL — the manual plus source reads on GitHub described as behaviour, never copied). Date of sweep: 2026-10-20. Scope: **Windows workstation backup** only.

Conventions used in every FEATURE LIST:

- `area` = agent / console / storage / alerting / restore / security / deployment / reporting / multi-tenant / billing
- `priority` = must / should / could, judged for an **MSP workstation backup product** (laptops, ~200 endpoints, one operator)
- `[unverified]` = stated by a secondary source or by memory and NOT confirmed in a primary doc read during this sweep. Treat as a lead, not a fact.
- `[not documented]` = looked for it in the sources read; nothing found. Absence of evidence, not evidence of absence.

Fetch-tool caveat: several vendor pages were read through a summarising fetch tool. Quoted labels are as returned by that tool; where a label could not be confirmed verbatim it is flagged.

---

## 1. Acronis Cyber Protect Cloud (workstation backup features)

### 1.1 SOURCES

| URL | What it gave |
|---|---|
| https://www.acronis.com/en/support/documentation/CyberProtectionService/start-conditions.html | The 8 start conditions, which platforms each applies to, "all conditions must be met", fallback "run anyway after" period, manual backups ignore conditions |
| https://www.acronis.com/en/support/documentation/CyberProtectionService/save-battery-power.html | "Do not start when on battery" and "allow above charge level" options (via search snippet) |
| https://www.acronis.com/en/support/documentation/CyberProtectionService/user-is-idle.html | Idle = screensaver running or machine locked; example 9 PM job waits up to 2 h |
| https://www.acronis.com/en/support/documentation/CyberProtectionService/do-not-start-when-on-metered-connection.html | Metered condition also blocks local-disk destinations; auto-enables Wi-Fi-name condition ("android", "phone", "mobile", "modem") |
| https://www.acronis.com/en/support/documentation/CyberProtectionService/schedule-time.html | Monthly/Weekly/Daily/Hourly fields; Hourly interval 10–60 min suggested or custom; "Show more": missed-tasks-at-startup (default Disabled), prevent sleep (Enabled, Windows), wake-up (Disabled, needs wake timers, not WoL) |
| https://www.acronis.com/en/support/documentation/CyberProtectionService/schedule.html | Scheduling = scheme + trigger + options; schedules run in the agent's local time zone |
| https://www.acronis.com/en/support/documentation/CyberProtectionService/backup-options.html | Full option-availability matrix by backup source |
| https://www.acronis.com/en/support/documentation/CyberProtectionService/retention-rules.html | Number/Age/Size bases, "Keeping backups infinitely", cleanup before/after backup semantics, newest backup never deleted |
| https://www.acronis.com/en/support/documentation/CyberProtectionService/retention-rules-backup-scheme.html | Scheme names and which retention rules each supports |
| https://www.acronis.com/en/support/documentation/CyberProtectionService/encryption.html | AES-256-GCM, PBKDF2 1,048,576 iterations, password set in plan, not stored, not recoverable |
| https://www.acronis.com/en/support/documentation/CyberProtectionService/performance-and-backup-window.html | Weekly hour grid (green/blue/gray), CPU priority Low/Normal/High, output speed %/KB-s, disabled by default, running backup stopped on blocked hour + alert |
| https://www.acronis.com/en/support/documentation/CyberProtectionService/continuous-data-protection.html | CDP semantics, NTFS only, disabled by default, single CDP backup per set |
| https://www.acronis.com/en/support/documentation/CyberProtectionService/file-filters.html | Filters apply to entire-machine/disk/file backups; not on XFS/JFS/exFAT/ReFS/ReiserFS4 (intro only — syntax page not reached) |
| https://www.acronis.com/en/support/documentation/CyberProtectionService/task-failure-handling.html | Restart mid-backup continues automatically after reboot (default enabled) — via search snippet |
| https://www.acronis.com/en/support/documentation/CyberProtectionService/service-console.html | Console sections; offline = 5 consecutive missed 1-minute status checks while not transferring |
| https://www.acronis.com/en/support/documentation/CyberProtectionService/updating-agents.html | Settings > Agents default update method (manual/automatic), update channel, per-machine override; post-Nov-2024 plans block agent uninstall outside maintenance window |
| https://www.acronis.com/en/support/documentation/AcronisCyberCloud/audit-log.html | Audit log fields (Event, Severity, Date, Object, Tenant, Initiator, Method web/API, IP), 180-day retention, filters |
| https://www.acronis.com/en/support/documentation/AcronisCyberCloud/2fa.html | 2FA mandatory for partner tenants, optional for customer tenants, TOTP |
| https://www.acronis.com/en/support/documentation/AcronisCyberCloud/user-roles-read-only-admin.html and .../user-roles-available-for-each-service.html | Company admin, unit admin, read-only admin, per-service roles |
| https://developer.acronis.com/doc/vendor-side/rmm/monitoring.html | Per-workload API fields: protection status, plan name, last successful backup, next backup (`/resource_management/v4/resource_statuses`) |
| https://www.acronis.com/en/blog/posts/how-to-personalize-unattended-acronis-agent-installation-on-windows/ and developer.acronis.com equivalents | Silent install switches `--quiet`, `--reg-address`, `--reg-token`, `--registration=skip`; not `--quiet` with device-flow |
| https://www.acronis.com/en/support/documentation/AcronisCyberProtect_16/creating-mst-transform-extracting-installation-package.html | GPO deployment via .mst + .msi/.cab, token registration default (on-prem doc, same agent) |
| https://dl.acronis.com/u/rc/PAYG-Pricing-for-Acronis-service-provider-partners-EN-US.pdf and partner FAQ PDF | Per-workload vs per-GB models; one model per customer; Advanced Backup pack on both models (figures garbled in extraction) |
| https://www.acronis.com/legal/licensing.htm | M365 seat = user, shared mailbox, or public folder; Google Workspace seat analog |
| https://cobweb.com/acronis-per-workload-pricing/ | Reseller RRP example: Advanced Backup – Workstation £0.66/mo (secondary) |
| https://www.liquidweb.com/help-docs/acronis-notifications/ and Sophos integration docs | Alert names seen in the wild: "Backup did not start", "Storage quota exceeded", "Backup is corrupted", "Backup is waiting"; API types BackupFinishedWithWarnings, BackupNotResponding, ProtectionServiceNotWorking (secondary) |

### 1.2 FEATURE LIST

| feature | area | how Acronis does it | priority | notes |
|---|---|---|---|---|
| Scheduling model | agent | Protection plan > Backup module > Schedule: by time (Monthly/Weekly/Daily/Hourly) or by event; Hourly = pick weekdays + interval (10–60 min suggested, custom allowed) + time range | must | Interval-within-window is the laptop-friendly shape |
| Missed-schedule catch-up | agent | "If the machine is turned off, run missed tasks at the machine startup" — **default Disabled** | must | Off by default = laptops silently miss backups unless MSP flips it. CryoBackup should default ON |
| Sleep handling | agent | "Prevent the sleep or hibernate mode during backup" (default Enabled, Windows); "Wake up from the sleep or hibernate mode to start a scheduled backup" (default Disabled; needs "Allow wake timers"; no WoL) | should | |
| Start conditions | agent | 8 conditions, all must hold; fallback period "run anyway after N" [label unverified]; manual runs ignore: User is idle; backup location's host is available; Users logged off; Fits the time interval; Save battery power; Do not start when on metered connection; Do not start when connected to the following Wi-Fi networks; Check device IP address | must (battery, metered) / could (Wi-Fi, IP) | Best-in-class laptop gating. The "run anyway after" escape hatch is the key design: conditions delay, never cancel forever |
| Battery | agent | "Save battery power": do not start on battery, or allow when charge above X% | must | |
| Metered connection | agent | Condition blocks even local-disk targets when Windows flags metered; also enables hotspot-name block list | should | |
| Reboot mid-backup | agent | "Task failure handling": backup resumes minutes after an unexpected restart (default enabled) | should | |
| Backup scheme | storage | "Always incremental (single-file)", "Always full", "Weekly full, Daily incremental", "Monthly full, Weekly differential, Daily incremental (GFS)", "Custom" | must (one scheme) | Cloud storage is effectively single-file always-incremental [unverified which is default] |
| Retention | storage | Per scheme: "By number of backups", "By backup age" (separate monthly/weekly/daily/hourly), "By total size of backups" (not on single-file), "Keep backups indefinitely"; cleanup can run before or after backup; newest backup never deleted | must | "Before vs after" semantics documented with an example — copy that clarity |
| VSS / open files | agent | "Volume Shadow Copy Service" option (Disk and File on Windows), "File-level backup snapshot", "Multi-volume snapshot", "Pre/Post data capture commands" | must | |
| CBT | agent | "Changed block tracking (CBT)" option for Windows disk-level; "Fast incremental/differential backup" (by timestamp/size) | should | |
| Default exclusions | agent | "File filters" include/exclude masks; concrete default list not reached in this sweep | should | [not documented here] — fetch file-filters sub-pages before copying defaults |
| Encryption & key custody | security | Per-plan password; AES-256-GCM; random data key wrapped by PBKDF2(1,048,576 iters, SHA-2)-derived key; **password not stored anywhere; lost = unrecoverable** | must | Customer-held key, vendor cannot restore. Decide CryoBackup's stance explicitly |
| Compression | storage | "Compression level" option (all sources) | should | levels not enumerated in page read |
| Dedup | storage | [not documented] for cloud storage in pages read | could | |
| Destinations | storage | Cloud storage (Acronis-hosted), local folder, network folder, Acronis Cyber Infrastructure, script-defined; replication to second location (Advanced Backup) | must (cloud + local) | |
| Throttling / windows | agent | "Performance and backup window": CPU priority Low/Normal/High (Windows Below normal/Normal/High); output speed as % (Windows) or KB/s; weekly hour grid green=high / blue=low / gray=prohibited; disabled by default; **running backup is stopped and alert raised when hitting a prohibited hour** | should | The "kill on prohibited hour" is a design choice to avoid for laptops |
| Multi-tenant console | multi-tenant | Management Portal: partner > (folders/units) > customer tenants; per-tenant services and quotas; Cyber Protect console per tenant with hierarchy dropdown | must | |
| Per-device status | console | Devices view (simple/table); offline after 5 missed 1-min checks; API exposes protection status, plan, last successful backup, next backup | must | |
| Alert conditions | alerting | Alerts tab per tenant; names seen: "Backup did not start", "Backup failed", "Backup finished with warnings", "Backup not responding", "Storage quota exceeded", "Backup is corrupted", "Backup is waiting", "Protection service not working" (secondary sources) | must | [unverified] exact list; no "no backup for N days" alert confirmed |
| Reports | reporting | Reports section in Cyber Protect console; Management Portal usage + operations reports (per partner guide ToC) | should | |
| Restore: files | restore | File/folder recovery from any backup, download via web console or to original/alternate path | must | |
| Restore: image/BMR | restore | Entire-machine recovery via bootable media; "One-click recovery" option (Disk, Windows/Linux); recover to dissimilar hardware (Universal Restore) [unverified name] | should | |
| Restore: virtual disk / VM | restore | Run as VM / convert to VM [unverified in sweep] | could | |
| CDP | agent | "Items to protect continuously": tracked folders or files-by-app; single CDP backup per set, replaced after each scheduled run; NTFS only; disabled by default; Advanced Backup pack per datasheet | could | Nice marketing, operationally heavy |
| Deployment | deployment | Setup EXE with `--quiet --reg-address <url> --reg-token <token>`; `--registration=skip` for later; GPO via .mst+.msi; device-flow not with `--quiet`; tokens time-limited | must | |
| Self-update | deployment | Settings > Agents: default update method manual/automatic, update channel, maintenance window; per-machine override; auto-update requires agent ≥ 15.0.26986 | should | |
| Offline handling | agent | Condition "backup location's host is available" + "run anyway after"; task-failure handling | should | No local cache for cloud target found |
| Audit log | security | Management Portal audit log: Event, Severity, Date, Object name, Tenant, Initiator, Initiator's tenant, Method (web/API), IP; 180-day retention; filters by type/severity/date | should | |
| Roles / 2FA | security | Company administrator, Unit administrator, Read-only administrator, per-service roles (e.g. Cyber administrator); 2FA mandatory for partner tenants, optional for customers; TOTP; integration accounts must be service accounts | must | |
| API / webhooks | console | REST API (OAuth2 client credentials) incl. `resource_statuses`, alerts, agents update management; CyberApps alerts extension | should | |
| M365 / Google Workspace | billing | Separate "seat" SKU: 1 seat = M365 user / shared mailbox / public folder; Google Workspace seat analog; storage billed separately or hosted fair-use | could | Model: per-seat add-on, not per-device |
| Metering / billing | billing | Per-workload or per-GB (one model per customer, partner may mix across customers); Advanced packs (Backup, Security, Management, DR, Email Security, DLP) add per-workload; usage reports for billing | must | |
| Pricing gates | billing | Standard backup vs "Advanced Backup" pack (CDP, replication to 2nd location, app-aware extras, etc.); example reseller RRP £0.66/workstation/mo for Advanced Backup | n/a | |

### 1.3 SCREEN NOTES

1. **Management Portal > Clients** — tenant tree (partners/folders/customers), per-tenant services enabled, quotas, "Manage service" jump to the Cyber Protect console.
2. **Management Portal > Settings > Security** — "Two-factor authentication" toggle for the tenant (mandatory on partner tenants).
3. **Management Portal > Monitoring > Audit log** — table with Event / Severity / Date / Object name / Tenant / Initiator / Initiator's tenant / Method / IP; filter by type, severity, date.
4. **Cyber Protect console > Devices (table view)** — workload rows with protection status, plan, last/next backup; offline after 5 missed checks; actions Backup now / Recover / Protect / Details [action labels unverified].
5. **Protection plan > Backup module** — What to back up / Where to back up / Schedule / How long to keep / Encryption / Backup options. Schedule pane has "Show more" for missed-task, prevent-sleep, wake-up.
6. **Backup options > Performance and backup window** — 7×24 click-to-cycle grid (green/blue/gray) with per-colour CPU priority and output speed.
7. **Backup options > Task start conditions** — checklist of the 8 conditions plus "run anyway after".
8. **Settings > Agents** — default agent update settings (manual/automatic, channel, maintenance window).
9. **Monitoring > Alerts** — alert list per tenant (names per secondary sources).

### 1.4 DESIGN LESSONS

Adopt:
1. **Conditions delay, never cancel** — the "run the backup anyway after N" fallback on start conditions is the single best laptop idea in the sweep.
2. Battery + metered as first-class start conditions with a charge-level threshold.
3. Documented cleanup-before vs cleanup-after semantics with a worked example; "newest backup is never deleted" as a hard rule.
4. Audit log field set (initiator, tenant, method web/API, IP) with a fixed retention — small, boring, complete.
5. 2FA mandatory for the MSP tier, optional for customer-side logins.

Avoid:
1. "Run missed tasks at startup" defaulting to **off** — a backup product for laptops should default to catch-up.
2. Killing a running backup when the window closes (prohibited hour) — on laptops this means never finishing an initial full.
3. The sprawling option matrix (30+ backup options × source types). One operator cannot hold it in their head.

---

## 2. Datto Endpoint Backup (PC/endpoint product; EB for PCs legacy + Endpoint Backup v2 in UniView)

Two generations are documented side by side on continuity.datto.com. **DEBPC** = "Datto Endpoint Backup for PCs" (formerly Cloud Continuity for PCs; 2-hourly image backups, 1.5 TB cap). **EB2** = "Datto Endpoint Backup v2" managed in Kaseya **UniView** (policies, selective backup, 90d/1y/3y/7y retention). Rows below say which.

### 2.1 SOURCES

| URL | What it gave |
|---|---|
| https://continuity.datto.com/help/Content/kb/EB2/EB2-create-backup-policy.htm | EB2 policy: Business hours (every 2 h, 8 AM–6 PM local, default) / Continuous interval / Custom; limits on custom windows; retention choices; throttling; data selection; alerting thresholds; metered pause |
| https://continuity.datto.com/help/Content/kb/EB2/EB2-retention.htm | 90 Days / 1 Year / 3 Years / 7 Years GFS tiers; nightly run; runs even for paused agents; restore-safe deletion (v1.15) |
| https://continuity.datto.com/help/Content/kb/EB2/EB2-selective.htm | Inclusion/exclusion pattern syntax (`?`, `*`, `**`), drive-letter rules, exclusions beat inclusions, BMR warning |
| https://continuity.datto.com/help/Content/kb/EB2/EB2-deploying-backup-agents.htm | `msiexec /i ...DattoBackupAgentInstaller-x64.msi REG_TOKEN=... ENCRYPTION_KEY=... /qn`; Datto-managed vs customer key; region lock; shortcode pairing |
| https://continuity.datto.com/help/Content/kb/EB2/EB2-setup.htm | Organizations: sync from Autotask/BMS/ConnectWise/IT Glue, import, or manual; Groups |
| https://continuity.datto.com/help/Content/kb/EndpointBackup_v2/EndpointBackupv2.htm | EB2 overview: selective backup, differential restore, policy-driven, PSA/RMM/IT-doc integrations, Windows x64 now (mac/Linux "coming soon"), encryption in transit/at rest, customer-managed keys option; Assets section; restore types File Restore / Virtual Disk Export (coming soon) / BMR |
| https://continuity.datto.com/help/Content/kb/EB/EB-StatusPage.htm | EB (v1) Status page columns, "Last Ten Days" circle semantics, per-agent actions incl. Force Diffmerge |
| https://continuity.datto.com/help/Content/kb/DEBPC/360000997103.html | DEBPC Status page: org/agent rows, Last Backup, Last Screenshot, Last Ten Days colours, 1.5 TB red marker, pause durations, RMM template token |
| https://continuity.datto.com/help/Content/kb/DEBPC/360028796731.html | DEBPC Agent Settings: Volume Protection (default OS volume only, 1.5 TB cap), Pause while metered, Idle Limit (bps), In Use Limit (bps), Virtualization Storage Controller (VirtIO default) |
| https://continuity.datto.com/help/Content/kb/DEBPC/360001010326.html | DEBPC Agent Overview: tray icon, Agent ID, hidden "Check for update", auto-push updates, reboot only on driver update, processes/services |
| https://continuity.datto.com/help/Content/kb/EB/EB-BMRGettingStarted.htm and DEBPC/360029124392.html | BMR: emergency only, single partition, no software/FakeRAID, BitLocker restored unencrypted, port 3262 outbound, DNS names mothership.dtc.datto.com / agent-update.datto.com / dattolocal.net |
| https://helpdesk.kaseya.com/hc/en-gb/articles/43799368493201 | Kaseya-side retention definition (TBR: intra-daily 7 d, daily 1 wk, weekly 1 mo, rest 1 yr) |
| https://help.unitrends.kaseya.com/uniview-help/Content/MSP/groups-users.htm | UniView roles Superuser / Admin / Manage / Monitor; groups scope orgs; 2FA reset per user; KaseyaOne auto user creation |
| https://www.kaseya.com/es-la/platform/innovations/endpoint-backup-license-usage-visibility-uniview/ | Jan 2026: role-based visibility of EB license/usage data |
| https://datto.com/products/endpoint-backup/pc-backup/features | Auto-pause on connection loss and resume; screenshot verification; 1-year TBR marketing claim (search snippet) |
| https://community.immy.bot/t/datto-backup-agent-and-cloud-continuity-agent-install-update/2046 | RMM install/update packaging community notes (secondary) |
| https://invenioit.com/datto-backup/datto-end-point-backup-pcs-servers/ and G2 | Third-party pricing claims ($2.99/endpoint/mo, 10-endpoint minimum) — [unverified] |

### 2.2 FEATURE LIST

| feature | area | how Datto does it | priority | notes |
|---|---|---|---|---|
| Scheduling model (EB2) | agent | Policy > Backup Schedule: **Business hours** = every 2 h, 8 AM–6 PM asset-local (default); **Continuous interval** = every N; **Custom** = frequency within chosen days/time windows; cannot combine options or split a day into two windows | must | Default is a sane "work hours, every 2 h" — good default story |
| Scheduling model (DEBPC) | agent | Fixed: every 2 hours, no schedule editing | n/a | legacy |
| Missed-schedule / laptop | agent | Interval-based, so next eligible slot runs when the device is up; auto-pause when connection drops, auto-resume on reconnect (product page); "Last Ten Days" marks a missed day orange | must | No explicit "run at startup" setting needed because the schedule is interval-based |
| Metered / battery | agent | "Pause while metered" (DEBPC Agent Settings) / "pause automatically on metered connection" (EB2 policy); **battery: [not documented]** | should | |
| VSS / open files | agent | Image-based volume backup via VSS (implied; .OST excluded by VSS per selective doc); change-tracking driver requires one reboot after first install — without it max one backup/day | must | Reboot dependency is an onboarding trap |
| Default exclusions | agent | EB2 selective mode: none excluded unless rules set; VSS-excluded files (.OST) skipped; FAQ on temp/.ost | should | |
| Exclusion syntax (EB2) | agent | `?` one char, `*` zero+ chars (non-recursive), `**` recursive (cannot combine with other symbols), drive must be `C:\` not `C:`, `*\` = all volumes; exclusions beat inclusions; inclusions restrict to only those paths | should | Clean, small grammar — worth copying |
| Retention (EB2) | storage | Policy retention: 90 Days / 1 Year / 3 Years / 7 Years; all share Intradaily 7 d, Daily 14 d, Weekly 6 wk; Monthly 3/12/36/84 mo; nightly thinning; never breaks a chain; runs even for paused agents; won't delete a point an active restore is reading | must | Four fixed tiers; no custom numbers — simple for operator |
| Retention (DEBPC) | storage | 1-year time-based retention (TBR): intra-daily 7 d → daily 1 wk → weekly 1 mo → 1 yr | n/a | |
| Encryption & key custody | security | In transit and at rest always; EB2 deploy: **Datto-managed key (default, Datto can help recover)** or `ENCRYPTION_KEY=` customer key (lost key = data unrecoverable; must "decrypt the agent in UniView" to view restore points) | must | Two-tier custody choice at deploy time is a good pattern |
| Dedup / compression | storage | [not documented] beyond "differential restore" (changed blocks only) | could | |
| Destinations | storage | Datto Cloud only; storage region chosen per deployment token and **locked after first deploy** (AU East, CA East, DE, ZA, UK, US East, US West); no local/BYO target; (DEBPC) Local cache none | must (cloud) | Region lock per token — model this early |
| Throttling | agent | EB2: policy throttling off unless enabled, business-hours or custom; DEBPC: Idle Limit (bps) + In Use Limit (bps) where "in use" = mouse/keyboard activity | should | Idle vs in-use split is a nice laptop touch |
| Storage cap | storage | DEBPC: combined protected volumes ≤ 1.5 TB (red "!" on status page when exceeded, agent stops backing up all volumes) | n/a | A hard cap surfaced in the UI as a red marker |
| Multi-tenant console | multi-tenant | UniView: Organizations synced from PSA/IT Glue or manual; Groups scope what admins see; roles Superuser / Admin / Manage / Monitor independent of groups | must | |
| Per-device status (EB2) | console | Assets section: backup schedules, recent activity, health checks; detail page with Restore button | must | Exact status vocabulary [not documented] |
| Per-device status (DEBPC/EB v1) | console | Status page: Last Backup (elapsed), Last Screenshot (hover shows image), Last Ten Days circles: green = ≥1 success that day, half green/orange = mixed, orange = a missed backup, white = none; red "!" = 1.5 TB exceeded | must | The ten-day circle strip is the best compact history widget in the sweep |
| Alert conditions | alerting | EB2 policy "alerting thresholds" override defaults by backup status and time-since-last-successful-backup; DEBPC per-org "Email Alert Settings" | must | Threshold values [not documented] |
| Screenshot verification | alerting | Boots the latest image in the cloud and shows a screenshot on the Status page (DEBPC); EB2 "health checks" | should | |
| Reports | reporting | UniView reports (Monitor role is read-only incl. reports); EB license/usage visibility by role (2026) | should | |
| Restore: files | restore | File Restore from asset page (EB2) / org-level Restore launchpad "cloud file restore" (DEBPC) | must | |
| Restore: BMR | restore | Bare Metal Recovery via Datto Utilities ISO; emergency only; restores OS as a single partition; no software RAID/FakeRAID; BitLocker systems come back unencrypted; port 3262 outbound | should | Document BMR caveats like this |
| Restore: virtual disk / VM | restore | EB2 "Virtual Disk Export (coming soon)"; DEBPC cloud virtualization (Virtualization Storage Controller IDE/SATA/SCSI/VirtIO, default VirtIO) | could | |
| Deployment | deployment | `msiexec /i <url>.msi REG_TOKEN=<token> ENCRYPTION_KEY=<optional> /qn`; deployment tokens at login.backup.net; RMM/GPO/Intune; manual shortcode pairing in UniView; Datto RMM ComStore component; VSA 9/10 | must | |
| Self-update | deployment | Pushed automatically; hidden "Check for update" (Shift+right-click tray); reboot only if driver updated | should | |
| Offline handling | agent | Auto-pause/resume on connection loss; no local cache | should | |
| Pause | agent | Pause Backups 1/2/4/8/24 h or indefinitely (console-side); retention still runs on paused agents | should | Time-boxed pause from the console, not the user, is the right ownership |
| Audit log | security | [not documented] in pages read | should | |
| Roles / 2FA | security | UniView roles above; 2FA reset per user from Users view; KaseyaOne SSO; enforcement policy [not documented] | must | |
| API / webhooks | console | PSA/RMM/IT-doc integrations listed; public API [not documented] | could | |
| M365 / Google Workspace | billing | Separate product (Datto SaaS Protection), not part of EB | n/a | |
| Metering / billing | billing | Per-endpoint license with storage included (Licenses Purchased/Deployed/Available on Status page; G-Cloud listing "licence per device, secure cloud storage"); price points only from third parties ($2.99/endpoint/mo, 10 minimum) [unverified] | must | License counters on the status page are a good operator affordance |
| Pricing gates | billing | None documented beyond retention tier choice | n/a | |

### 2.3 SCREEN NOTES

1. **UniView > Endpoint Backup > Policy Management > + Policy** — Name, Backup Schedule (Business hours / Continuous interval / Custom), Retention (90 Days / 1 Year / 3 Years / 7 Years), Throttling, Data selection (selective inclusion/exclusion), Alerting thresholds, metered-connection pause.
2. **UniView > Settings > Organizations** — Sync (Autotask / BMS / ConnectWise / IT Glue), Import, New; Group assignment.
3. **UniView > Deployment Tokens** (login.backup.net) — token per storage region; region locked once used.
4. **UniView > Assets** — protected assets with schedule, recent activity, health checks; asset detail > **Restore** (File Restore / Virtual Disk Export / Bare Metal Recovery); decrypt agent for customer-key assets.
5. **UniView > Settings > Users** — roles Superuser / Admin / Manage / Monitor; Groups; "reset 2FA" in user edit menu.
6. **DEBPC/EB Status page** — Overview (Licenses Purchased / Deployed / Available), buttons Create RMM Template / Register Agent / Install Agent; org rows (Organization Name → Email Alert Settings, Restore, Agents, Protected Size, Unprotected Volumes, Last Backup, Last Screenshot, Last Ten Days); agent rows with ellipsis menu (Pause Backups, Manage Agent > Settings / Start a Backup / Force Diffmerge / Delete agent).
7. **DEBPC Agent Settings** — Volume Protection checkboxes (OS volume default; 1.5 TB cap), Bandwidth Throttling (Pause while metered, Idle Limit (bps), In Use Limit (bps)), Virtualization Storage Controller, "Apply Settings".
8. **Agent tray icon** — colour = status, hover = detail, right-click = version + Agent ID (click copies), Shift+right-click = UUID + Check for update.

### 2.4 DESIGN LESSONS

Adopt:
1. **"Last Ten Days" circle strip** per device/org with four states (success / mixed / missed / none). Cheapest honest history view found.
2. Fixed retention tiers (90 d / 1 y / 3 y / 7 y) with published thinning tables — removes a whole class of misconfiguration.
3. Deploy-time key custody choice: vendor-managed (recoverable) vs customer key (unrecoverable), with the consequence stated in the installer doc.
4. Console-owned, time-boxed pause (1/2/4/8/24 h/indefinite) and "retention still runs on paused agents" stated explicitly.
5. Idle-limit vs in-use-limit bandwidth split (in-use = keyboard/mouse activity).

Avoid:
1. Reboot-after-install dependency that silently degrades to one backup/day — if a driver needs a reboot, the console must show "pending reboot, incrementals disabled" as a warning state.
2. Storage-region lock per token with no migration path — fine for Datto, painful for a small MSP that picks wrong.
3. Two overlapping product generations with different status vocabularies on the same help site.

---

## 3. Axcient x360Recover Direct-to-Cloud (D2C) for endpoints

### 3.1 SOURCES

| URL | What it gave |
|---|---|
| https://help.axcient.com/002-install-direct-to-cloud/929226-x360recover-create-direct-to-cloud-schedules | Backup Policies > Schedules > Add Policy fields: Name, Enabled, Default, Schedule type (Classic / Policy-based), Initial Backup Start/End Time, Incremental type Interval/Manual with Start/End/Interval; 15-min RPO; chain-free; assign via Protected Systems > Schedule dropdown |
| https://axcient.helpjuice.com/2-configure-x360recover/how-to-use-retention-policy-manager-x360recover | Retention Policy Manager: Recommended / Space Saving (fixed), custom with workstation vs server values; default auto-assigned to new D2C systems; storage-license 3-yr/10-yr immutable; per-system legacy values |
| https://axcient.helpjuice.com/health-status-reports/1443722-health-status-reports-for-protected-systems | D2C statuses Parked / Normal / Warned / Troubled with rules (SLA threshold, local-cache "last OK older than SLA + 3 h"); job-level rules; appliance/vault rules |
| https://axcient.helpjuice.com/1-navigate-x360recover-manager-rmc/protected-system-details-page-x360recover | Details page panels: Recovery Wizard, Run AutoVerify, Geo+, Alert Settings (per-device override), Device Details, Protection Summary (SLA days), Cloud Storage Summary, Job History, 30-day health graph, AutoVerify screenshot, Local Cache Summary |
| https://axcient.helpjuice.com/3-manage-x360recover/2-health-dashboard-x360recover | Health Dashboard widgets, Quick Health Filters, Protected Systems columns, refresh cadence, 25 rows default |
| https://help.axcient.com/x30recover-faqs-appliance-errors-and-troubleshooting/115004693847-x360Recover-Alert-Definitions | Alert classes (Agent Status, Boot VM Check, Disk Usage 80/80/70 %, Failed Backup, Missed Backup, Pool Status, Protected System, Successful Backup, Replication/Vault ingestion delayed); email vs ConnectWise defaults |
| https://axcient.helpjuice.com/5-recover-with-x360recover/local-cache-for-d2c | Local Cache: encrypted/compressed/deduped block store; USB or UNC share; RAM 1 GB per 4 TB; 12 TB cap on 32-bit agent; verify every 24 h (`LOCAL_CACHE_VERIFY_FREQUENCY_HOURS` 1–720); >1 % missing blocks = backup Failed; cache unavailability = warning only |
| https://help.axcient.com/003-manage-d2c/x360recover-local-cache-verification-2 | Verification at lowest CPU/I-O priority; missing blocks re-sent next backup |
| https://help.axcient.com/install-an-agent/agent-install-windows-x360recover | Agent 2.x MSI (32-bit, Win7/2003 → Win11/2025) vs 3.x EXE (64-bit, Arm only so far); `msiexec /i AgentInstaller.msi /q TOKENID=<id>`; optional `BACKUP_VOLUMES`, `PASSWORD`, `ENABLE_SYSTRAY`, `LCPATH`, `LCUSER`, `LCPASSWD`; tokens now permanent; FAT/ReFS volumes excluded by default |
| https://help.axcient.com/003-manage-d2c/360050734614-x360Recover-Direct-to-Cloud-D2C-Billing | Usage-based; workstation vs server rate; Axcient-hosted vs self-hosted vault; billing stops only when endpoint deleted from vault; "cloud-only" orphan rate |
| https://axcient.helpjuice.com/billing-resources/360059272473-x360Recover-Billing-Overview | D2C not in License Portal (search snippet) |
| https://help.axcient.com/virtual-office-best-practices/virtual-office-billing-faqs-x360recover-vo | Virtual Office: 30 days/endpoint/calendar year incl. D2C; test and production count the same; quarter-day granularity; overage per day per endpoint; trial usage counts |
| https://axcient.helpjuice.com/175381-recover-with-replibit/x360recover-recovery-options | Recovery options and D2C applicability (BMR from local cache / vault / virtual disk; Recovery Center; Hyper-V export; Virtual Office) |
| https://help.axcient.com/5-recover-with-x360recover/x360recover-file-and-folder-recovery and .../recovery-center | Recovery Center = stand-alone app for D2C file/folder recovery; far faster with local cache |
| https://help.axcient.com/001-install-bdr-/1500000637281-x360Recover-How-to-set-agent-throttling-options | Agent 3.x: network limit (Mbps) in `config.json`, separate full-backup limit; agent 2.29+ business-hours bandwidth logic in backup policy |
| https://help.axcient.com/06-monitor/360023090853-x360Recover-Agent-Disk-I-O-Throttling | Disk I/O throttling (agent ≥ 2.23); CPU priority default "BelowNormal" |
| https://help.axcient.com/x360recover-faqs-passwords-and-networks/360042898374--x360Recover-Encryption-Overview-Agent-2-23-Appliance-OS-8-2-0- | TLS 1.2 in transit; agent has no at-rest encryption; appliance/vault pools AES-256 whole-disk with Axcient CKMS |
| https://axcient.helpjuice.com/x360Portal-User-Guide/877905-configuring-multifactor-authentication | MFA required by default on x360Portal (Dec 2025); app or SMS; 10 bad codes = 1-hour lock; admin-only reset |
| https://help.axcient.com/health-status-reports/configure-the-health-digest-for-an-organization-x360recover and .../x360recover-manager-configure-settings | Org Settings: SLA thresholds, PSA, reports; daily health digest emails; Org Admin / Org Manager / Org Read-only |
| https://help.axcient.com/3-manage-x360recover/x360recover-manager-integrate-psa and .../004-integrate-psa/connectwise-asio-integration | PSA: ConnectWise, Autotask (not at vault level); RMM: ConnectWise Asio ticket sync + agent deploy |
| https://axcient.com/blog/feature-focus-x360recover/ and https://help.axcient.com/en_US/about-x360recover | AirGap (deletion request separated from deletion mechanics; ZFS snapshot immutability), AutoVerify (nightly BootVM + deep volume tests on D2C) |
| https://help.axcient.com/en_US/d2c-[index] | Index redirected to SSO — not readable (noted) |
| rfp.wiki / SoftwareOne listings | 300 GB/desktop and 3 TB/server pooled fair-use, $0.05/GB overage, VO $10/VM/day overage — [unverified] |

### 3.2 FEATURE LIST

| feature | area | how Axcient does it | priority | notes |
|---|---|---|---|---|
| Scheduling model | agent | Backup policy: **Initial Backup** Start Time / End Time window; **Incremental Backup** type Interval or Manual, with Start Time / End Time / Interval (15-min RPO supported; hourly common); "Classic" vs "Policy-based" schedule type; policy can be assigned before agent install; incrementals wait for initial full | must | Interval-in-window again; explicit initial-full window is a good touch for laptops on hotel Wi-Fi |
| Missed-schedule / laptop | agent | Interval scheme means next slot runs when online; health model tolerates "new device whose first backup window hasn't opened" as Normal | must | No explicit catch-up setting documented |
| Battery / metered | agent | [not documented] | should | Gap in Axcient docs read |
| VSS / open files | agent | VSS snapshot per volume; nested volumes must be excluded (VSS can't snapshot mount point + physical disk image together); change-tracking via agent; "Partition layout change" or chkdsk failure forces next backup to full scan | must | |
| Default exclusions | agent | FAT and ReFS volumes excluded by default; `BACKUP_VOLUMES` install param; `aristos.cfg` for volumes | should | File-level excludes [not documented] — image-based product |
| Chain-free storage | storage | Each recovery point independent; no reseeding/consolidation | should | Marketing term, but the operational benefit (no chain breaks) is real |
| Retention | storage | Retention Policy Manager (≥ 12.5.0): **Recommended** and **Space Saving** fixed policies; custom policies with separate workstation (Windows desktop/macOS) and server values; default auto-assigned to new D2C systems; per-system legacy overrides; hosted vaults have max tier limits; storage-license systems fixed 3-yr or 10-yr immutable | must | Separate workstation vs server defaults is the right split |
| Encryption & key custody | security | TLS 1.2 agent→vault; vault pool AES-256 whole-disk, keys in Axcient CKMS (vendor-held); agent `PASSWORD` install param exists [role unverified]; local cache encrypted | must | Vendor-custody model, opposite of Acronis |
| Dedup / compression | storage | Local cache is compressed+deduped block store; vault dedup [not documented] | could | |
| Destinations | storage | Axcient-hosted vault or self-hosted (private) vault; optional **Local Cache** (USB or UNC share, block data only, not an independent backup) | must (cloud) / should (local cache) | "Local cache is not an independent local backup" — honest framing worth copying |
| Local cache semantics | storage | Path + optional domain creds; verify every 24 h after backup at lowest priority; missing blocks re-sent; >1 % missing → backup Failed; cache down → warnings only; recovery pulls missing blocks from cloud; ~1 GB RAM per 4 TB | should | |
| Throttling | agent | Business-hours bandwidth logic in policy (agent ≥ 2.29); agent 3.x `config.json` Mbps limits with separate full-backup limit; disk I/O throttle; CPU priority BelowNormal | should | |
| Multi-tenant console | multi-tenant | x360Recover Manager (RMC): Organization > Clients > Protected systems; vaults per partner; Org Admin / Org Manager / Org Read-only; client-level settings/digest | must | |
| Per-device status | console | Health status **Parked / Normal / Warned / Troubled**: Parked = replication job disabled; Normal = job normal, none yet, or first window not opened; Warned = job warned; Troubled = job troubled, or local cache enabled and last OK check older than SLA + 3 h. Job: Warned = no restore points but within threshold; Troubled = out of SLA threshold | must | Explicit "new device grace" + "no restore points yet" state = the blank-client problem solved in the state machine |
| SLA thresholds | alerting | Per-org SLA threshold in days for local/cloud jobs; per-device Alert Settings override; vault connectivity threshold in hours | must | Day-granularity SLA per org with device override |
| Alert conditions | alerting | Classes: Failed Backup, Missed Backup, Successful Backup (closes prior alerts), Agent Status, Boot VM Check (insufficient resources / screenshot count mismatch / check screenshots), Disk Usage (pool > 80 %, quota > 80 %, OS disk > 70 %), Pool Status, Protected System (chkdsk failed, configured volume missing, partition layout change), Replication (vault ingestion delayed by x days). Many are appliance-scoped; D2C relies on health/SLA | must | "Successful backup closes outstanding missed/failed alerts" is the correct auto-resolve rule |
| Reports | reporting | Daily health digest email per org/client; Reports in Manager; 7-day history chart; 30-day per-device graph | should | |
| Verification | alerting | AutoVerify: nightly BootVM of latest snapshot for every D2C system + deep volume tests (bootability, OS health, corruption, FS/app integrity); screenshot on device page; "Run AutoVerify" on demand | should | |
| Restore: files | restore | Recovery Center (stand-alone app) mounts vault snapshot, with or without local cache; web/vault file browse [D2C unverified] | must | |
| Restore: BMR | restore | Recovery Toolkit ISO; from local cache, from vault, or from exported virtual disk | should | |
| Restore: virtual disk / VM | restore | Virtual disk export (D2C), Hyper-V VM creation via Recovery Center, Azure direct restore; **Virtual Office** cloud failover (Test / Test & Dev / Production) | should (VO) | |
| AirGap | security | Deletion requests separated from deletion mechanics; ZFS snapshot immutability; recover deleted snapshots via support | should | |
| Geo+ | storage | Optional geo-redundant copy per device | could | |
| Deployment | deployment | `msiexec /i AgentInstaller.msi /q TOKENID=<id>` (2.x) / `xcloud-agent_arm64.exe /TOKENID=` (3.x); optional `BACKUP_VOLUMES PASSWORD ENABLE_SYSTRAY LCPATH LCUSER LCPASSWD`; client-specific installer carries token in filename (overrides CLI); token from Vault Clients key icon, now permanent, revocable via Replace; ConnectWise Asio packages; outbound TCP 80/443/9079/9082/9083/9090 | must | |
| Self-update | deployment | Agent updates via vault [mechanism not documented]; "software outdated" is a Warned condition | should | |
| Offline handling | agent | Local cache write errors ignored (warnings); recovery needs internet to vault + Manager | should | |
| Pause | agent | Disable replication job → "Parked" (health digest skips it) | should | Parked as a first-class state |
| Audit log | security | [not documented] | should | |
| Roles / 2FA | security | x360Portal MFA required by default (app or SMS), 10-strike 1-h lock, admin-only reset, recommend ≥ 2 admins; roles Org Admin / Org Manager / Org Read-only (RMC), Administrator / Accountant (portal, older) | must | |
| API / webhooks | console | PSA ConnectWise + Autotask (ticket sync both ways), ConnectWise Asio RMM; public REST API [not documented] | could | |
| M365 / Google Workspace | billing | Separate product (x360Cloud) | n/a | |
| Metering / billing | billing | Usage-based per D2C endpoint; workstation vs server rate; hosted vs self-hosted vault; storage bundled (fair-use 300 GB/desktop, 3 TB/server pooled [unverified]); billing stops only on deletion from vault; orphaned "cloud-only" rate; VO 30 days/endpoint/yr then per-day overage | must | "Deleted from vault" as the billing stop event is clean |
| Pricing gates | billing | Quote-driven via distributors; no public list | n/a | |

### 3.3 SCREEN NOTES

1. **x360Recover Manager > Health Dashboard** — Quick Actions (Add Client, Recovery Wizard, Org Settings), Current Client Health gauge (% SLA-compliant), 7-day Protected System Health History, Activities of Interest; Quick Health Filters (Troubled / Warned); refresh 5/15 min (default hourly); 25 rows default.
2. **Health Dashboard > Protected Systems table** — Protected System Name, Client, System Type (server/workstation), Health Status (+ explanation), Recommended Actions, AutoVerify (date + result), Last Successful Job, Backup Targets.
3. **Protected System Details** — Recovery Wizard, Run AutoVerify, Enable/Disable Geo+, Alert Settings; Device Details (added, OS, host/IP, package, agent version); Protection Summary (SLA threshold days); Cloud Storage Summary; Job History (status/type/completed); Device Summary Message banner with AutoVerify screenshot; Health Status History – Last 30 Days (red/yellow/green per day); Local Cache Summary (enabled, status, last check, path).
4. **Vault > Backup Policies > Schedules > Add Policy** — Name, Enabled, Default, Schedule type (Classic/Policy-based), Description, Initial Backup Start/End Time, Incremental Backup (Interval/Manual, Start/End, Interval).
5. **Policies > Retention** — Recommended / Space Saving / custom (workstation vs server tiers), Default flag; Protected Systems page retention dropdown.
6. **Vault > Clients (key icon)** — D2C token ID; Replace to revoke.
7. **Vault > device > Configure > Agent > Local Cache Settings** — toggle, Path (drive or UNC), Username/Password.
8. **Org Settings / Notification Configuration** — SLA thresholds, health digest recipient emails, PSA integration.
9. **Recovery Wizard** — Recover files/folders, run VM in Virtual Office (Make it a Test / Create Test & Dev Environment / Run in Production), export virtual disk, Azure direct restore.

### 3.4 DESIGN LESSONS

Adopt:
1. **Four-state device health with explicit grace rules** (Parked / Normal / Warned / Troubled; "first window not yet opened" = Normal; "no restore points but within threshold" = Warned). Maps directly onto CryoBackup's blank/stale/failed taxonomy.
2. **Per-org SLA threshold in days with per-device override** — one number drives both status colour and alerts.
3. "Successful backup closes outstanding missed/failed alerts" as the auto-resolve rule.
4. Separate workstation vs server retention defaults inside one policy object.
5. Honest local-cache framing: block cache that accelerates restore, verified daily, never presented as a second backup.

Avoid:
1. Alert catalogue that is mostly appliance-scoped, leaving D2C endpoints covered only by SLA health — be explicit which alerts apply to laptops.
2. Two agent lines (2.x MSI 32-bit, 3.x EXE Arm) with different CLI syntax (`TOKENID=` vs `/TOKENID=`).
3. Help-centre fragmentation (helpjuice vs help.axcient.com, SSO-gated index) — one canonical doc path.

---

## 4. Veeam Agent for Microsoft Windows (Free / Workstation; managed via VBR / Service Provider Console)

### 4.1 SOURCES

| URL | What it gave |
|---|---|
| https://helpcenter.veeam.com/docs/agentforwindows/userguide/license_editions.html | Free / Workstation / Server feature table (guest processing, parallel disk, advanced scheduling, object storage, Cloud Connect, synthetic full, CBT driver, backup cache, GFS, job counts) |
| https://helpcenter.veeam.com/docs/agentforwindows/userguide/backup_job_schedule_free_desktop.html | "Daily at" (Everyday / On week-days / On these days), "Backup once powered on" vs "Skip backup" (priority active full > synthetic > incremental), post-job Keep running / Sleep / Shutdown / Hibernate with countdown, events Lock / Log off / When backup target is connected / Eject removable storage, "Back up no more often than every N", wake-timer and Modern Standby notes |
| https://helpcenter.veeam.com/docs/agentforwindows/userguide/scheduled_backup_missed.html | 15-minute tolerance window; missed job starts right after power-on; Modern Standby delay |
| https://helpcenter.veeam.com/docs/agentforwindows/userguide/metered_connections.html | "Restrict metered connections usage" (Settings > Manage networks); default disabled-over-metered; scheduled job → cache if enabled else fails; manual → warning; CLI ignores |
| https://helpcenter.veeam.com/docs/agentforwindows/userguide/backup_cache.html | Cache = temporary local folder; targets: shared folder, object storage, VBR repo, Cloud Connect; syncs when target returns; laptop use case |
| https://helpcenter.veeam.com/docs/agentforwindows/userguide/settings_resources.html | "Throttle backup activity when system is busy" lowers process priority |
| https://helpcenter.veeam.com/docs/agentforwindows/userguide/backup_job_advanced_storage.html | Compression None / Dedupe-friendly / Optimal / High / Extreme; block size 4 MB / 1 MB / 512 KB / 256 KB; "Enable backup file encryption" + password + hint; enabling on existing job → active full, old chain stays unencrypted |
| https://helpcenter.veeam.com/docs/agentforwindows/userguide/backup_job_volumes.html | Automatic exclusions: temp folder, Recycle Bin, pagefile, hibernate file, VSS files in System Volume Information; "Back up all volumes except" by drive letter `C:\` |
| https://helpcenter.veeam.com/docs/vbr/userguide/appendix_d_windows_computer_backup_filters.html | Include/exclude masks; exclude wins; folder paths when whole volume, file masks when folder; `\%appdata%` system env vars only |
| https://helpcenter.veeam.com/archive/agentforwindows/20/userguide/retention_days.html | Workstation retention "keep restore points for N days" default 14, counted by days a backup ran, chain transform (archived 2.x doc) |
| https://helpcenter.veeam.com/docs/agentforwindows/userguide/performing_restore_tasks.html | Restore list: volumes, files/folders, Explorers, export disks VMDK/VHD/VHDX, publish disks, restore to vSphere/Hyper-V/AHV/Proxmox/HyperCore/oVirt/Azure/EC2/GCE, export restore points |
| https://helpcenter.veeam.com/docs/vac/provider_admin/appendix_alarms.html | VSPC predefined alarms for agents/companies (table in 4.2) |
| https://helpcenter.veeam.com/docs/vac/provider_admin/alarms.html | Severities Error/Warning/Resolved/Information; responses email + script; Active Alarms dashboard |
| https://helpcenter.veeam.com/docs/vac/provider_admin/configure_backup_schedule.html | VSPC Windows backup policy wizard (19 steps) and workstation vs server schedule options |
| https://helpcenter.veeam.com/docs/vac/provider_admin/configure_backup_agent_settings.html | Global agent settings incl. wake-up from sleep (needs wake timers) |
| https://helpcenter.veeam.com/docs/vac/provider_admin/manage_admin_portal_users.html | Roles Portal Administrator / Site Administrator / Portal Operator / Read-only User |
| https://helpcenter.veeam.com/docs/vac/provider_quick_start/specify_agent_rates.html and .../provider_admin/create_subscriptions.html | Subscription plan = price sheet; example $10/managed workstation, $12/server, $50 flat; Default plan has no rates |
| https://helpcenter.veeam.com/docs/vac/provider_user/mfa.html and https://helpcenter.veeam.com/docs/vac/rest/mfa.html | TOTP MFA per user; enabling breaks basic-auth REST for that user; API keys for integrations; 1-min clock tolerance |
| https://helpcenter.veeam.com/docs/vac/rest/reference_v3.html | REST API v3 (build 9.0): Backup Policies, Computer Discovery, Protected Workloads, Backup Agents, Companies |
| https://helpcenter.veeam.com/docs/backup/agents/protection_group_options.html | VBR protection group: "Auto-update backup agent", CBT driver install servers-only, automatic reboot option |
| https://helpcenter.veeam.com/docs/vbr/userguide/agent_policy_win_schedule_workstation.html | Modern Standby: missed backup may start several minutes after resume |

### 4.2 FEATURE LIST

| feature | area | how Veeam does it | priority | notes |
|---|---|---|---|---|
| Editions | billing | **Free**: 1 job (local/share/VBR repo), no cache, no Cloud Connect, no object storage, no CBT driver, no synthetic full, no GFS. **Workstation**: + object storage, Cloud Connect (unlimited CC jobs), synthetic full, CBT driver, backup cache, GFS; still 1 non-CC job. **Server**: unlimited jobs, guest processing, parallel disks, periodic/monthly schedules, retries, backup window. Expired licence → falls back to Free and jobs using paid options fail | n/a | Workstation edition is the comparison target |
| Scheduling model (Workstation) | agent | "Daily at" <time> with Everyday / On week-days / On these days; no periodic/hourly in Free/Workstation; plus event triggers | must | One-a-day is the key weakness vs interval models |
| Missed-schedule catch-up | agent | 15-minute tolerance window; then **"Backup once powered on"** (runs missed job on power-on; if several types missed runs highest priority active full > synthetic full > incremental) or **"Skip backup"**; Modern Standby adds minutes of delay | must | Best-documented catch-up semantics in the sweep |
| Event triggers | agent | "At the following events": Lock, Log off, When backup target is connected; "Eject removable storage once backup is completed"; "Back up no more often than every <N> <units>" (events only) | should | "When target connected" + rate limit is a great USB-drive story |
| Post-job power action | agent | Keep running / Sleep / Shutdown / Hibernate (scheduled runs only; countdown dialog) | could | |
| Wake from sleep | agent | VSPC global setting to wake managed computer at job time; requires "Allow wake timers"; not Modern Standby | should | |
| Metered connection | agent | "Restrict metered connections usage" (Settings > Manage networks); default: no backup over metered; scheduled run → saved to backup cache if enabled, else **job fails**; manual → warning prompt; CLI ignores | should | Failing the job (rather than deferring) is a design to avoid |
| VPN / Wi-Fi gating | agent | "Disabling Backup over VPN Connections", "Selecting Wireless Networks for Backup", "Limiting Bandwidth Consumption" (sibling pages) | could | |
| Battery | agent | [not documented] | should | Gap |
| Backup cache | agent | Temporary local folder+size set in job; used when target unreachable (or metered); uploads when target returns and becomes normal restore points; targets: share, object storage, VBR repo, Cloud Connect; Workstation+ only | should | The laptop feature Veeam gets right |
| VSS / open files | agent | VSS-based volume/file snapshot; Veeam CBT driver (Workstation+; VBR installs CBT driver on servers only) | must | |
| Default exclusions | agent | Auto-excluded for all users: temp files folder, Recycle Bin, pagefile, hibernate file, VSS snapshot files in System Volume Information; "Back up all volumes except" by `C:\` | must | Copy this exact list |
| Exclusion syntax | agent | Include/exclude masks (file-level jobs); exclude beats include; whole volume → folder paths; folder → file masks (`*.tmp`, `OldReports.rar`); `\%appdata%`-style system env vars; include masks not allowed when whole volume selected | should | |
| Retention | storage | Workstation: "keep restore points for N days" (default 14, counts days a backup actually ran); Server: N restore points (default 14); GFS long-term on Workstation+ to VBR/Cloud Connect; chain transformed, not blindly deleted | must | "Days counted only when a backup ran" is a laptop-correct subtlety |
| Encryption & key custody | security | "Enable backup file encryption" + password + hint; enabling later forces active full and leaves old chain unencrypted; Cloud Connect provider cannot read encrypted backups [unverified here] | must | |
| Compression / block size | storage | Compression None / Dedupe-friendly / Optimal / High / Extreme; storage optimization 4 MB / 1 MB / 512 KB / 256 KB (needs active full to apply) | should | |
| Dedup | storage | Inline per-job dedup within backup files [unverified in pages read] | could | |
| Destinations | storage | Local drive, network share, VBR repository, Veeam Cloud Connect repository, object storage (S3/Azure/GCS, Workstation+) | must | BYO everything; no vendor cloud |
| Throttling | agent | "Throttle backup activity when system is busy" (lowers process priority); bandwidth limits via "Limiting Bandwidth Consumption"; VBR network traffic rules | should | |
| Multi-tenant console | multi-tenant | VSPC: Resellers > Companies > Sites; Cloud Connect tenants/quotas; backup policies per company; roles Portal Administrator / Site Administrator / Portal Operator / Read-only User; company-side Company Owner/Admin [unverified] | must | |
| Per-device status | console | VSPC Managed Computers / Protected Workloads with last backup, RPO compliance; VBR protection groups + rescan | must | |
| Alert conditions | alerting | VSPC predefined alarms (enabled by default unless noted): Backup agent connection state; Backup agent job state (+ managed-by-backup-server variant); Backup agent version is out-of-date; **Computer without backup** (no backup within RPO interval; + managed-by-backup-server variant); Management agent connection state (off); Management agent version out-of-date (off); Management agent data collection delay; Max allowed backup agent job duration (off); Company server/workstation agents quota; Company cloud storage quota; Server/Workstation agent backups stored in cloud repository; Company lease expiration date; VSPC licenses exceeded; Discovery rule state; File-level recovery session state. Severities Error/Warning/Resolved/Information; responses email + script | must | "Computer without backup within RPO" + "max job duration" + "agent out of date" = the right trio |
| Reports | reporting | VSPC reports (Protected Computers, Protected Data, Usage) and alarms export [names unverified]; agent email notifications (SMTP, Gmail, M365) | should | |
| Restore: files | restore | File-level restore (mount restore point, copy to original/other location), also from object storage; Explorers for app items | must | |
| Restore: volumes / BMR | restore | Volume-level restore; bare-metal via Veeam Recovery Media (not on the restore-tasks index page but documented elsewhere) | should | |
| Restore: virtual disk / VM | restore | Export disks as VMDK/VHD/VHDX; publish disks; restore to vSphere/Hyper-V/AHV/Proxmox/HyperCore/oVirt/Azure/EC2/GCE; export restore points to standalone full | should | |
| Deployment | deployment | VSPC: discovery rules (AD/network/CSV) + management agent + master agent push; VBR protection groups with "Install backup agent automatically"; standalone silent EXE switches (`/silent /accepteula ...`) [unverified] | must | |
| Self-update | deployment | VBR protection group "Auto-update backup agent" (+ automatic reboot option); standalone "Checking for New Product Versions and Updates" | should | |
| Offline handling | agent | Backup cache; "When backup target is connected" trigger; target-unreachable job fails without cache | should | |
| Audit log | security | [not documented] in VSPC pages read | should | |
| Roles / 2FA | security | VSPC TOTP MFA per user (self or via REST PATCH); MFA breaks basic-auth REST → use API keys; clock skew ≤ 1 min | must | |
| API / webhooks | console | VSPC REST API v3 (companies, backup policies, discovery, protected workloads, backup agents, alarms, subscription plans); ConnectWise Manage/Automate plugins | should | |
| M365 / Google Workspace | billing | Separate product (Veeam Backup for Microsoft 365) integrated into VSPC billing | n/a | |
| Metering / billing | billing | VSPC subscription plan = price sheet per company: per managed workstation, per managed server, flat fee, storage, currency/tax/discount; invoices from usage; "managed workstation" = physical computer running agent in Workstation mode | must | |
| Pricing gates | billing | Free (no cloud, no cache) vs Workstation licence (VUL/rental) vs Server | n/a | |

### 4.3 SCREEN NOTES

1. **Veeam Agent Control Panel > Backup job wizard > Schedule (Workstation)** — "Daily at" + Everyday/On week-days/On these days; "If computer is powered off at this time": Backup once powered on / Skip backup; post-job Keep running/Sleep/Shutdown/Hibernate; "At the following events": Lock, Log off, When backup target is connected; "Eject removable storage once backup is completed"; "Back up no more often than every N".
2. **Control Panel > Settings > Manage networks** — "Restrict metered connections usage", VPN and wireless network selection, bandwidth limit.
3. **Control Panel > Settings** — "Throttle backup activity when system is busy"; email notifications; update check.
4. **Backup job > Advanced > Storage tab** — Compression level, Storage optimization block size, "Enable backup file encryption" + Password + Hint.
5. **Backup job > Local Backup Cache** (Workstation+) — folder + size.
6. **VSPC > Backup Policies > New Backup Policy (Windows)** — 19 steps: access type, name, operation mode (workstation/server), backup mode, volumes, folders, destination, local/share/VBR/cloud settings, cloud quota, advanced, backup cache, guest OS, schedule, summary.
7. **VSPC > Alarms** — predefined alarm list with Object type and Enabled flag; Active Alarms dashboard; per-alarm email/script actions.
8. **VSPC > Configuration > Portal Users** — roles Portal Administrator / Site Administrator / Portal Operator / Read-only User; MFA in user profile.
9. **VSPC > Configuration > Subscription Plans** — charge rate per managed workstation / server, flat fee, storage, currency, tax, discount.

### 4.4 DESIGN LESSONS

Adopt:
1. **Explicit missed-run policy per job**: "Backup once powered on" vs "Skip backup", with a short tolerance window (15 min) and documented type priority when several runs were missed.
2. **Backup cache** for targets that are unreachable or metered — queue locally, upload on return, promote to normal restore points.
3. Retention in "days on which a backup actually ran", not calendar days.
4. Event triggers "when backup target is connected" + "no more often than every N".
5. Alarm trio: *Computer without backup within RPO*, *max allowed job duration*, *agent version out-of-date*, each with on/off default and email/script response.

Avoid:
1. Once-a-day scheduling ceiling on the workstation tier — laptops need interval-in-window.
2. Failing the scheduled job outright when metered/no cache — defer, don't fail.
3. Edition/licence cliffs where an expired licence makes configured jobs fail.

---

## 5. UrBackup (open source, GPL) — Server 2.5.x, Windows client

### 5.1 SOURCES

| URL | What it gave |
|---|---|
| https://www.urbackup.org/administration_manual.html | Settings names/defaults (intervals, min/max counts, backup window syntax, advanced interval `;`/`@`, speed limits with `@window` and %, internet settings and defaults, data-plan limits, default dirs + `|name` + `/flags`, excluded files, virtual sub-clients, client permissions, mail settings, rights domains, internet client setup methods, UDP discovery 50 s, restore flows, archival, cleanup window, quotas, btrfs/ZFS) |
| https://www.urbackup.org/faq.html | Glob semantics (`*`, `:`, `?`, `[...]`), include patterns, case sensitivity, server trust-on-first-use (`server_idents.txt`), window semantics, archive icon, EFS/.ost/TrueCrypt/network-share limits, VHD mounting |
| https://www.urbackup.org/features.html | Feature claims: file-tree diff incrementals, sector-level image incrementals, VHD(X)/raw, file-level dedup, client overdue warning, email alerts when not backed up within configurable period, internet mode, restore USB |
| https://raw.githubusercontent.com/uroni/urbackup_backend/dev/urbackupserver/serverinterface/status.cpp | `status` JSON fields per client and how `file_ok`/`image_ok`/`*_disabled`/`rejected`/`no_backup_paths`/`status` codes are derived from the DB |
| https://raw.githubusercontent.com/uroni/urbackup_backend/dev/urbackupserver/ClientMain.cpp | Scheduling loop: ≤ 5-min pass, check order, due = no qualifying backup within interval, negative disables, window checks at queue and start, manual bypasses, backoff 20 min × 2^n, 5-min post-attempt pause, group lock, internet gating, data-plan check, `startup_backup_delay`, continuous-backup remnant |
| https://raw.githubusercontent.com/uroni/urbackup_backend/dev/urbackupserver/Alerts.cpp | Alert thread: 60-s pass, per-client `alerts_next_check`, inputs to Lua, params from `alert_script_params` + per-client `alert_params`, return bitmask → `clients.file_ok`/`image_ok` (1/0/−1), state persistence, Mailer bridge, Pulseway variant (script 100000) |
| https://raw.githubusercontent.com/uroni/urbackup_backend/dev/urbackupserver/alert.lua | Default alert logic: interval choice, `interval × alert_mult − passed_time < 0 ⇒ not ok`, mail on transition, `alert_nag_interval`, `alert_mail_ok`, next-check 30 min |
| https://raw.githubusercontent.com/uroni/urbackup_backend/dev/urbackupserver/server_cleanup.cpp | Nightly cleanup order, window gating (12 h min spacing), emergency cleanup, quota enforcement, DB backup |
| https://raw.githubusercontent.com/uroni/urbackup_backend/dev/urbackupclient/ClientServiceCMD.cpp | `PAUSE true/false` semantics, `STATUS DETAIL` JSON fields, `METERED`/`LOCKED` reporting (Windows), `2START BACKUP`/`3START FULL BACKUP`, `SAVE BACKUPDIRS`, `FILE RESTORE` confirm modes, `CLIENT_ACCESS_KEY` |
| https://raw.githubusercontent.com/uroni/urbackup_backend/dev/urbackupclient/ClientService.cpp | Settings merge rules (server-pushed vs kept local keys), `backup_alert_delay` → `NOA`/`NO_RECENT`, default-dirs save, `actionToStr` labels |
| https://github.com/uroni/urbackup_backend/tree/dev/urbackupserver | File inventory (Alerts.cpp, alert.lua, alert_pulseway.lua, report.lua, ContinuousBackup.cpp, DataplanDb.cpp, ThrottleUpdater.cpp, www/, www2/) |
| https://forums.urbackup.org/t/feature-request-do-not-backup-when-on-battery/2410 | No native battery option; `prefilebackup` script workaround |
| https://forums.urbackup.org/t/urbackup-speed-paused/7365 | Tray "Continue"; disabling "Allow client-side pausing of backups" |
| https://forums.urbackup.org/t/how-do-yall-backup-laptops-get-around-sleep-hibernate/12214 | Community laptop workarounds (keep awake on AC, scheduled task to stop service on battery) |
| https://forums.urbackup.org/t/no-recent-backup-not-shown-into-image-backup-status/2188 and related | Status cell strings: light-red "No recent backup", yellow "No paths to backup configured", image "No recent backup" bugs |
| https://forums.urbackup.org/t/status-is-no-recent-backup-should-be-disabled/4988 | Status computed by Lua alert script; disabled backups still shown as "no recent" (Lua missing on Arch package) |
| https://forums.urbackup.org/t/is-the-configuration-of-email-alerts-buggy/13458 | `alert_nag_interval` save bug fixed in 2.5.32 |
| (local) docs/02-urbackup-api-notes.md in this repo | Wrapper field list, `lastbackup == "-"` sentinel, rights model has no read-only modifier, urbstat taxonomy |

### 5.2 FEATURE LIST

| feature | area | how UrBackup does it | priority | notes |
|---|---|---|---|---|
| Scheduling model | agent/console | Server-driven, **interval-based**: "Interval for incremental file backups" 5 h, full file 30 d, incremental image 7 d, full image 30 d (settings `update_freq_incr`, `update_freq_full`, `update_freq_image_incr`, `update_freq_image_full`); negative = disabled; advanced form `1;4@1-7/20-6` varies interval by window | must | See 5.5 |
| Backup window | console | "Backup window" per backup type, default `1-7/0-24`; syntax `Mon-Fri/8:00-9:00, 19:30-20:30;Sat,Sun/0-24`; only gates starts; client-initiated ignore it | must | |
| Missed-schedule / laptop | agent | No "missed" concept: server loop (≤ 5 min) starts a due backup the moment the client is online and inside the window; "Delay after system start up" (`startup_backup_delay`, 0 min) | must | Inherent catch-up |
| Battery | agent | None natively; only a failing `prefilebackup` script can veto a start | must (gap) | |
| Metered | agent | Windows client reports `METERED` to server; server's data-plan estimate (metered Wi-Fi on Win10 ⇒ 1 GB/mo) gates **scheduled internet** backups via "Do not start file backups if current estimated data usage limit per month is smaller than" 5000 MB / image 20000 MB; hostname lookup in dataplan_db, unknown ⇒ 1 TB | should | Clever but opaque to the operator |
| Pause | agent | Tray Pause/Continue → `PAUSE true/false`; pauses idle-checker + file server in memory only (lost on restart); server ping sees `PAUSED`; permission "Allow client-side pausing of backups"; quitting the tray icon also pauses | should | Not surfaced as a status state in the server UI |
| VSS / open files | agent | VSS shadow copies on Windows; "Volumes to snapshot in groups" (image `image_snapshot_groups`, file `file_snapshot_groups`); per-dir flag `require_snapshot`; "Windows components backup" (`default=1` backs up Exchange/SQL/Hyper-V) | must | |
| Incremental detection | agent | File: fast file-tree diff (change journal on Windows per features page [mechanism not spelled out]); image: only used+changed sectors; incremental image style "based on last" or "based on last full" (differential); full image style full transfer or synthetic | must | |
| Default exclusions | agent | "Excluded files" default empty; no built-in Windows exclusion list documented (VSS itself drops .ost) | must (gap) | CryoBackup should ship Veeam's list by default |
| Exclusion syntax | agent | `;`-separated globs: `*` spans folders, `:` one folder level, `?` one char, `[...]`; case-insensitive on Windows; full paths or `C_\...` location names; `*/Temp/*`; include patterns (`include_files`) restrict to matches | should | |
| Default dirs / per-dir flags | agent | "Default directories to backup" `C:\Users|User files;D:\Data/optional,one_filesystem`; flags `optional, follow_symlinks, symlinks_optional, one_filesystem, require_snapshot, share_hashes, keep, required`; setting any flag replaces the defaults; "Directories to backup are optional by default" | should | |
| Volumes | agent | "Volumes to backup" default `C`; `ALL`, `ALL_NONUSB` | should | |
| Retention | storage | Count-based min/max per type: file incr max 100 / min 40, file full max 10 / min 2, image incr max 30 / min 4, image full max 5 / min 2; nightly cleanup deletes above max only; emergency cleanup may go down to min (favours images, oldest first); archival (`Archive every` + crontab-ish window) exempts backups; "Soft client quota"; "Global soft file system quota" (`90%`/`900G`) | must | |
| Encryption & key custody | security | Internet mode: "Encrypted transfer" (on) + "Compressed transfer" (on) with per-client auth key; **data at rest unencrypted on server** (FS-level only); access keys for restore | must (gap) | |
| Dedup / compression | storage | File-level dedup via hard links / btrfs cross-device reflinks / ZFS dedup; VHDZ compressed images; raw COW on btrfs/ZFS; hashed/block-diff transfer; "Calculate file-hashes on the client" lets clients skip files the server already has | should | |
| Destinations | storage | Self-hosted server only (local/LAN or internet mode); no vendor cloud; storage FS matters (btrfs/ZFS recommended) | must | |
| Throttling | agent/console | "Max backup speed for local network" / "for internet connection" per client, totals globally; `60;10@Mon-Fri/8-18` window syntax; percentages (`50%`) with periodic speed probing | should | Window-varying throttle syntax is good |
| Multi-tenant | multi-tenant | Client **groups** with group settings (`groupname` in status); rights domains scoped to client IDs or `all`; no tenant object, no per-customer admin UI | must (gap) | |
| Per-device status | console | `status` JSON: `online` (live connection), `lastseen` (max DB/live), `lastbackup`, `lastbackup_image` (0 or `"-"` when none), `file_ok`/`image_ok` (true only if DB flag 1 and a backup exists), `file_disabled`/`image_disabled` (−1), `no_backup_paths`, `last_filebackup_issues`, `status` (0 idle, process action, 11–17 error/startup states), `processes[{action,pcdone}]`, `delete_pending`, `rejected`, `groupname`, `client_version_string`, `os_version_string`, `ip`, `image_not_supported` | must | See 5.5 for the ok-computation |
| Status page strings | console | Columns Online / Last seen / Last file backup / Last image backup / Status [from UI, verbatim unverified]; cells green "ok", light-red "No recent backup", yellow "No paths to backup configured", "Disabled", image "Not supported" | must | |
| Alert conditions | alerting | Lua **alert script** per client (default id 1, Pulseway variant 100000): not-ok when `interval × alert_file_mult` (or `alert_image_mult`) elapsed since last backup (interval = incr, or full if shorter/incr disabled); mail on first failure, nag every `alert_nag_interval`, recovery mail if `alert_mail_ok`; `alert_emails`, `alert_important`; writes `file_ok`/`image_ok` | must | Defaults of the multipliers not read in this sweep [unverified] |
| Client overdue warning | agent | Tray shows "no recent backup" when `backup_alert_delay` since service start passed with no backup (`NO_RECENT`; `NOA` before delay) | should | |
| Reports | reporting | Logs page > Reports: recipients `;`-separated, "only failed / succeeded", min log level Info/Warning/Error/All; per user with `logs` right; `report.lua` | should | |
| Mail | alerting | "Mail server name / port / username / password", "Sender E-Mail Address", "Send mails only with SSL/TLS", "Check SSL/TLS certificate", "Server admin mail address" (fatal errors e.g. emergency cleanup failed), test mail | must | |
| Restore: files | restore | Web browse + download file or ZIP (≤ 4 GB); client-side restore button when online, confirmation `default` (client confirms) or `server-confirms` in `args.txt`; `CLIENT_ACCESS_KEY` tokens; share snapshots via SMB/FTP/WebDAV; images mountable in web UI (2.1+) | must | |
| Restore: image/BMR | restore | Debian-based restore CD/USB; target disk ≥ source; VHD files mountable natively (`uncompress_image.bat` for VHDZ) | should | |
| Restore to other client | restore | Virtual sub-clients publish allowed restore client names; `renameClient` carries settings | could | |
| Deployment | deployment | MSI/EXE installer; "Add new client" on Status page → auth key → preconfigured internet installer download (server, port, name, key); LAN auto-discovery UDP every 50 s; client trusts first server (`server_idents.txt`) | must | |
| Self-update | deployment | Server pushes client updates; "Perform auto-updates silently" permission (unchecked); update checks at server startup/cleanup | should | |
| Offline handling | agent | Internet clients resume via block-level transfer; "Connect to Internet backup server if connected to local backup server" (off) | should | |
| Audit log | security | None (auth log cleanup exists; no admin action audit) | should (gap) | |
| Roles / 2FA | security | Users with per-domain rights: `browse_backups, lastacts, progress, settings, client_settings, status, logs, manual_archive, stop_backup, mail_settings, download_image` (per-client or `all`) and all/none-only `piegraph, users, general_settings, usermod, remove_client, start_backup`; admin has no password by default; **no read-only modifier; no 2FA** | must (gap) | |
| API / webhooks | console | JSON web API at `/x` (status, progress, logs, backups, settings, users); alert script can call URLs (HTTP bridge) and run shell (2.3.2+) | should | |
| M365 / Google Workspace | billing | None | n/a | |
| Metering / billing | billing | None; usage stats per client (storage) | could | |
| Concurrency | console | "Max number of simultaneous backups" 10; "Maximum number of simultaneous jobs per client"; one file backup per group at a time unless manual; "Max number of recently active clients" 100 (seen in last 2 months) | should | |
| Continuous backup | agent | Vestigial (`ContinuousBackup.cpp`, `CONTINUOUS WATCH START`, queued once if client advertises) — not exposed in 2.x UI | n/a | Do not adopt |

### 5.3 SCREEN NOTES

1. **Status** — client table: Online, Last seen, Last file backup, Last image backup, Status/activity, IP, client version, OS, group; coloured ok / "No recent backup" / "No paths to backup configured" cells; buttons "Add new client", download client, start backup (incr/full file/image), remove client; "Discover new client via IP/hostname hint".
2. **Activities** — running backups/restores with action, % done, ETA; last activities list (`lastacts`).
3. **Backups** — per-client backup list with archive checkbox / stopwatch icon for timed archival; browse into file backups; download ZIP; restore button; image list with mount.
4. **Logs** — per-client logs filtered by level; bottom "Reports" section (recipients, failed/succeeded, min level).
5. **Statistics** — storage usage pie/graphs (needs `piegraph` + `users`).
6. **Settings > General** (tabs: Server, Internet, Mail, Clients/Permissions, Advanced, Archival, Alerts) — intervals, min/max counts, backup windows, speeds, cleanup window, quotas, internet server name/port, data-plan limits, mail server, client permission checkboxes, alert script + parameters.
7. **Settings > Clients / Groups** — per-client or per-group overrides of the same fields; "Default directories to backup", "Excluded files", "Volumes to backup", virtual sub-clients.
8. **Settings > Users** — create user, set rights per domain (client IDs or all).
9. **Client tray (Windows)** — Status (last backup, running job, %), Start backup (incr/full file/image), Settings (if allowed), Pause/Continue, Logs, Access/Restore; "no recent backup" balloon.

### 5.4 DESIGN LESSONS

Adopt:
1. **Interval-due + catch-up** scheduling (no calendar slots; a laptop that reappears is simply "due").
2. Backup-window and throttle **mini-language** (`Mon-Fri/8-18;Sat,Sun/0-24`, `60;10@Mon-Fri/8-18`, `50%`) — expressive, one line, one operator can read it.
3. Min/max count retention with emergency cleanup that never drops below min and favours images.
4. Per-directory flags (`optional`, `required`, `require_snapshot`, `one_filesystem`) instead of global switches.
5. Trust-on-first-use server identity + per-client internet auth key + preconfigured installer download.

Avoid:
1. **Status "ok" computed asynchronously by a script** (30–60 min lag, `-1` disabled bugs, Lua-missing packages show everything red) — compute the state synchronously from timestamps + intervals at render/poll time, and keep the alert engine separate.
2. No battery / metered / lock policy in the agent; pause state in memory only and invisible as a state on the server.
3. Rights model with no read-only modifier, no tenants, no 2FA, no audit log.

### 5.5 SCHEDULING SEMANTICS TO KEEP (exact behaviours and controlling settings)

**A. Due-ness is interval-based, evaluated continuously**
- Settings (per client/group, manual §8.3): "Interval for incremental file backups" `update_freq_incr` (5 h), "Interval for full file backups" `update_freq_full` (30 d), "Interval for incremental image backups" `update_freq_image_incr` (7 d), "Interval for full image backups" `update_freq_image_full` (30 d). **Negative value disables that type** (alert engine stores `-1`).
- Server loop per client runs at least every 5 minutes (or sooner on a pipe message). Each pass checks, in order: full file → incremental file → full image → incremental image → (vestigial) continuous; only one item is queued per pass.
- "Due" = the DB has **no qualifying backup newer than the interval**. There is no stored "next run" time; it is recomputed from the last-backup timestamps every pass. For a full, the incremental interval also participates in the same query.
- Advanced interval (§8.3.2): `1;4@1-7/20-6` = 1 h default, 4 h 20:00–06:00; `1;4@1-5/18-6;6@6,7/0-24` adds 6 h on weekends. Most-specific rule wins. Presence of `;` marks the interval "complex" for the alert script.

**B. Catch-up is inherent — there is no "missed backup"**
- Start conditions (all must hold): due; inside the backup window for that type; backoff elapsed; ≥ 5 min since last attempt of any type; data-plan check passes (internet clients); client online; no file backup already running in the same group (unless manual); global/per-client concurrency allows.
- Consequence: a laptop offline for a week starts its incremental within ≤ 5 min of connecting, provided the window is open. The product never has to decide "run once powered on vs skip".
- "Delay after system start up" `startup_backup_delay` (default 0 min): server waits that long after a client session begins before entering the loop; value is also sent to the client.
- Client-side overdue nag: `backup_alert_delay` measured from client service start; before it elapses the tray reports `NOA`, after it `NO_RECENT`; if no backup has ever run, `NO_RECENT` immediately; a forward clock jump > 1 min resets the timer.

**C. Backup windows gate starts, never stops**
- "Backup window" per type (`backup_window_incr_file`, `backup_window_full_file`, `backup_window_incr_image`, `backup_window_full_image`), default `1-7/0-24`.
- Syntax: days `1`–`7` (Mon=1) or `Mon…Sun`; hours `0-24` or `8:00-9:30`; `,` separates items, `-` inclusive ranges, `;` separates windows. Example `Mon-Fri/8:00-9:00, 19:30-20:30;Sat,Sun/0-24`.
- Checked when queued **and again just before start**. A backup that has started runs to completion past the window. Client-initiated (tray) backups bypass window, interval, backoff, pause and data-plan checks.
- Cleanup has its own window "Cleanup time window" `1-7/3-4` (nightly, min 12 h spacing); archival uses a crontab-like `hour;dom;month;dow` window.

**D. Failure handling**
- A failed backup that requests backoff increments a per-type failure counter; next attempt waits ~20 min × 2^failures (min 20 min). Success or a non-backoff failure resets it. Any attempt starts a 5-minute pause before other scheduled types may begin.
- "Max number of simultaneous backups" (10, global), "Maximum number of simultaneous jobs per client", file backups serialised per group.

**E. Internet (roaming) mode**
- Enable with "Internet server name/IP" + "Internet server port" (55415); client auth key per client; "Encrypted transfer" ☑, "Compressed transfer" ☑, "Calculate file-hashes on the client" ☐, "Connect to Internet backup server if connected to local backup server" ☐ (`internet_connect_always`).
- Gating: "Do image backups over internet" ☐ (`internet_image_backups`), "Do full file backups over internet" ☐ (`internet_full_file_backups`); incremental file backups always allowed; forced/manual backups bypass.
- Data-plan check (scheduled internet backups only): skip if estimated monthly allowance < "Do not start file backups if current estimated data usage limit per month is smaller than" 5000 MB / images 20000 MB. Estimate: Windows 10 metered Wi-Fi ⇒ 1 GB/mo; else hostname in dataplan_db (unlimited ⇒ always); unknown ⇒ 1 TB.
- Speed: "Max backup speed for internet connection" per client and "Total max backup speed for internet connection" global, window-varying `60;10@Mon-Fri/8-18`, percentages allowed. Internet transfer is block-diff and resumable.

**F. Pause, metered, lock, battery**
- `PAUSE true|false` from the tray pauses the client idle-checker and file server; in-memory only (lost on service restart); server sees `PAUSED` on ping; governed by "Allow client-side pausing of backups" ☑; "Allow clients to quit the tray icon" ☑ (quitting pauses).
- Windows client reports `METERED` and `LOCKED` to the server (channel + capability reply); only `METERED` has a documented effect (data-plan estimate).
- **No battery handling.** Only workaround: `prefilebackup` script fails ⇒ indexing fails ⇒ backup does not start (cannot stop a running one).

**G. Incremental / full / retention logic**
- Full-vs-incremental is purely interval-driven per type; incremental image style "based on last incremental" or "based on last full"; full image "full transfer" or "synthetic".
- Counts: "Maximal/Minimal number of incremental file backups" 100/40, full file 10/2, incremental image 30/4, full image 5/2. Nightly cleanup deletes only above max; emergency cleanup (out of space) deletes down to min, images first, oldest first; archived/locked/incomplete/parent-of-others are skipped; quotas (`global_soft_fs_quota`, `client_quota`) enforced after.
- "Directories to backup are optional by default" ☐; missing dirs otherwise fail the backup unless `/optional`.

**H. Exclusion / inclusion syntax**
- "Excluded files" `exclude_files`: `;`-separated globs, `*` crosses folder boundaries, `:` matches within one level, `?` one char, `[abc]` sets; `*/Temp/*`; full paths `C:\Users\x\AppData\Local\Temp\*` or location names `C_\Users\...`; case-insensitive on Windows; either slash.
- Include patterns `include_files`: if any set, only matching files are backed up.
- "Default directories to backup" `default_dirs`: `path|Name/flag1,flag2;path2`; flags `optional`, `follow_symlinks` (default), `symlinks_optional` (default), `one_filesystem`, `require_snapshot`, `share_hashes` (default), `keep`, `required`; specifying any flag replaces the default set; "Allow client-side changing of the directories to backup" ☑ lets the client override.
- "Volumes to backup" `image_letters` `C` | `ALL` | `ALL_NONUSB`; snapshot groups `all` / `C,D|E`.

**I. Continuous backup** — remnant only (`ContinuousBackup.cpp`, `CONTINUOUS WATCH START`, queued once if the client advertises it). Not configurable in 2.x UI. Leave it.

**J. Restore flows**
- Web: browse file backups, download single file or directory ZIP (4 GB cap); "Restore" button only when client online; client confirmation mode `default` (client prompt) vs `server-confirms` (`args.txt` on Windows, `/etc/default/urbackupclient` on Linux); restore tokens via `CLIENT_ACCESS_KEY`; images mountable in web UI (2.1+); snapshots exposable via SMB/FTP/WebDAV.
- Client: tray "Access/Restore backups" uses access key; `FILE RESTORE` pending until `RESTORE OK ok=true`.
- Image: restore CD/USB (Debian), target ≥ source; VHD/VHDZ files usable directly (`uncompress_image.bat`).

**K. Status-page semantics (what `file_ok` actually means)**
- `online` = live connection exists now. `lastseen` = later of DB value and live. `lastbackup`/`lastbackup_image` = last **successful** backup timestamp (0 / `"-"` when none).
- `file_ok` / `image_ok` are **not** computed at request time. The alert thread (60-s passes, per-client `alerts_next_check`, default next check 30 min, 1 h when both types failing) runs the Lua alert script with `incr_file_interval`, `full_file_interval`, `incr_image_interval`, `full_image_interval`, `passed_time_lastbackup_file/_image` (= min(now − last backup, now − client creation)), `passed_time_lastseen`, `image_support`, `no_images`, `no_file_backups`, `complex_interval`, previous `file_ok`/`image_ok`, and script params (`alert_file_mult`, `alert_image_mult`, `alert_emails`, `alert_mail_ok`, `alert_important`, `alert_nag_interval` — defaults seeded in `alert_script_params`, values not read in this sweep).
- Script rule: interval = incremental, or full if full > 0 and (full < incr or incr < 0); **not ok when `interval × mult − passed_time < 0`**; image also not ok if `image_support` false. Return bitmask 1 = file not ok, 2 = image not ok; DB stores `1` ok, `0` not ok, `-1` disabled (type disabled in settings, or non-complex interval with both incr and full negative). JSON then reports `file_ok = (flag == 1 && lastbackup != 0)`, `file_disabled = (flag == -1)`.
- `no_backup_paths` = `last_filebackup_issues` equals the "no backup paths" marker (count reported as 0) → UI yellow "No paths to backup configured". `status` codes 11–17 = startup/error states (e.g. rejected/identity problems); `rejected: true` rows are live-but-unknown clients shown only to full-rights users.
- Known failure modes to design against: lag between a backup finishing and `file_ok` flipping; `-1` not applied on some builds (forum); Lua absent ⇒ everything "No recent backup".

**L. Discovery, identity, settings precedence**
- LAN: UDP broadcast every 50 s; client answers with FQDN. Internet: "Add new client" → generated auth key → paste into client Internet settings, or download preconfigured installer; "Internet client authentication key" per client.
- Client trusts the **first** server (`server_idents.txt`); changing server identity ⇒ "Server rejected" until the new ident is added.
- Settings precedence: server pushes `settings.cfg`; client keeps local-only keys (internet mode/server/port/proxy/auth key/computername) if the server omits them; client-changed dirs/settings win when the corresponding "Allow client-side …" permission is on; `_def` fallbacks.

---

## 6. Cross-product convergence notes (for the matrix builder)

- **Laptop scheduling**: three models in the field — interval-in-window (Datto EB2, Axcient, UrBackup, Acronis Hourly), daily-with-catch-up (Veeam Workstation: "Backup once powered on"), and conditions-with-fallback (Acronis start conditions + "run anyway after"). The strongest combination is interval-in-window **plus** Acronis-style conditions that *delay* **plus** Veeam-style local cache. Nobody ships all three.
- **Battery**: only Acronis documents a battery condition with a charge threshold. Datto, Axcient, Veeam, UrBackup: not documented.
- **Metered**: Acronis (condition), Datto (pause while metered), Veeam (restrict; job fails without cache), UrBackup (data-plan estimate). Axcient: not documented.
- **Status vocabulary worth borrowing**: Axcient Parked/Normal/Warned/Troubled with "first window not opened" and "no restore points yet" grace; Datto "Last Ten Days" circles; Veeam "Computer without backup (RPO)" alarm; UrBackup's explicit `no_backup_paths` and `*_disabled` sub-states.
- **Key custody**: Acronis = customer password, unrecoverable; Datto = vendor-managed default with customer-key opt-in; Axcient = vendor-held (CKMS); Veeam = job password; UrBackup = transport-only. CryoBackup must pick and document one.
- **Retention shape**: fixed tiers (Datto), named policies with workstation/server split (Axcient), scheme-dependent rules (Acronis), days-with-backups (Veeam), min/max counts with emergency floor (UrBackup).
- **Default exclusions**: only Veeam publishes a concrete Windows list (temp, Recycle Bin, pagefile, hiberfil, VSS files in System Volume Information). Adopt it as the baseline.
- **Multi-tenant + billing**: Veeam VSPC (price-sheet subscription plans per company) and Acronis (per-workload/per-GB, one model per customer) are the reference models; Axcient bills on usage until the endpoint is deleted from the vault; Datto per-endpoint license counters on the status page.
