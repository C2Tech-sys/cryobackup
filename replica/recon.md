# Recon map: Comet Backup + workstation-backup field (web console + Windows agent)

Scope: the MSP core loop for Windows workstation file backup (enroll device, assign policy, scheduled runs with missed-run catch-up, monitoring and alerting, restore). Comet Backup (Comet Server web console + Windows desktop agent) is the primary reconned product; eight other products were swept for a feature union (N-able Cove, MSP360, IDrive, Acronis Cyber Protect, Datto Endpoint Backup, Axcient x360Recover Direct-to-Cloud, Veeam Agent for Windows, UrBackup); an engine capability sheet (restic, rclone serve restic, rest-server, S3 providers, Go Windows service) gates the feature matrix.
For: **CryoBackup**, the clone target name: an open-source (AGPL-3.0) workstation backup platform with three tiers (Agent: Go Windows service wrapping restic; Gateway: append-only rclone serve restic; Console: fleet control plane). Operated by one person across roughly 200 endpoints and many customer companies.
Date: 2026-10-20

**Method: clean-room recon from public documentation only.** Sources are vendor documentation, public API reference pages, pricing pages, release notes, vendor blogs, open-source manuals and, for the GPL product (UrBackup) and the engine stack, public source files read as described behaviour. No vendor binaries, bundles, private API endpoints, private accounts or scraping loops were used, and no vendor code or copy is reproduced. Everything below is functionality and UX pattern. Items tagged **[unverified]**, **[inferred]**, **[gap]** or **UNCERTAIN** carry the confidence label given in the research notes; the tag follows the item into the matrix notes.

Conventions: `engine §N` refers to the numbered sections of the engine capability sheet (section 7 summarises it). Vendor short names: Comet, Cove, MSP360, IDrive, Acronis, Datto, Axcient, Veeam, UrBackup.

## Sources

One row per source group; every row carries at least one URL. Secondary or competitor-authored sources are tagged. Dates are as of 2026-10-20 (Comet latest release 26.8.9, Oct 15 2026).

| # | product | source | URL | notes |
| --- | --- | --- | --- | --- |
| 1 | Comet | Docs index | https://docs.cometbackup.com/latest/ | top-level section map |
| 2 | Comet | Start Here overview | https://docs.cometbackup.com/latest/start-here/overview-setup | console vs agent split; 7-step quick path |
| 3 | Comet | Start Here steps 1-7 | https://docs.cometbackup.com/latest/start-here/add-user ; https://docs.cometbackup.com/latest/start-here/download-and-install ; https://docs.cometbackup.com/latest/start-here/log-in ; https://docs.cometbackup.com/latest/start-here/add-pi ; https://docs.cometbackup.com/latest/start-here/new-sv ; https://docs.cometbackup.com/latest/start-here/run-backup ; https://docs.cometbackup.com/latest/start-here/job-status | users as security boundary; Job History cancel; 10-day Recent Activity; mostly video-driven |
| 4 | Comet | Getting started (self-hosted) | https://docs.cometbackup.com/latest/guides/guide-getting-started | first-run wizard; guides sidebar |
| 5 | Comet | Getting started (agent) | https://docs.cometbackup.com/latest/guides/guide-comet-backup-getting-started | agent tabs; Protected Item wizard; command order; Default Policy |
| 6 | Comet | Agent installation | https://docs.cometbackup.com/latest/installation/comet-backup-installation | silent switches; /dl routes; lobby; tokens; uninstall; service names |
| 7 | Comet | Mass deployment guide | https://docs.cometbackup.com/latest/guides/mass-deployment-guide | pre-deployment order; registration choice; CSV user creation |
| 8 | Comet | User configuration | https://docs.cometbackup.com/latest/installation/user-configuration | profile fields; irreversible admin flags; quotas; schedule frequencies; retention pass cadence |
| 9 | Comet | Management Console usage | https://docs.cometbackup.com/latest/installation/comet-server-usage | menus; devices statuses and actions; bulk upgrade; Recent Activity; replication |
| 10 | Comet | Self-hosted console configuration | https://docs.cometbackup.com/latest/installation/comet-server-configuration | settings sections; admin permissions; auth; tenants; audit log; email; session timeout |
| 11 | Comet | Hosted console configuration | https://docs.cometbackup.com/latest/installation/Comet-Hosted-Management-Console-Configuration | hosted settings dialog (read for contrast; hosted model is out of scope) |
| 12 | Comet | Files and Folders Protected Item | https://docs.cometbackup.com/latest/protected-items/files-and-folders | item editor; snapshot/VSS; filters; extra permissions; EFS; system exclusions |
| 13 | Comet | Protected Item types | https://docs.cometbackup.com/latest/protected-items/overview | engine list; items private per device |
| 14 | Comet | How to set up retention | https://docs.cometbackup.com/latest/guides/how-to-set-up-retention | 11 range labels; OR-combination; precedence; GFS recipe |
| 15 | Comet | How to apply user policies | https://docs.cometbackup.com/latest/guides/how-to-apply-policies | policy editor sections; 5-state modes; assignment |
| 16 | Comet | Email reporting | https://docs.cometbackup.com/latest/guides/configuring-email-reports | 4 report types; schedule; filters |
| 17 | Comet | Job statuses | https://docs.cometbackup.com/latest/guides/guide-job-statuses | 12 status labels |
| 18 | Comet | Quotas | https://docs.cometbackup.com/latest/guides/configuring-quotas | 4 quota kinds and behaviour |
| 19 | Comet | MFA guide | https://docs.cometbackup.com/latest/guides/how-to-set-up-mfa | admin TOTP/WebAuthn; end-user TOTP |
| 20 | Comet | Rebrand console / client | https://docs.cometbackup.com/latest/guides/how-to-rebrand-comet-server ; https://docs.cometbackup.com/latest/guides/client-rebranding-guide | branding fields (pattern only) |
| 21 | Comet | Deep linking | https://docs.cometbackup.com/latest/guides/deep-linking-to-the-management-console | route structure; dialog tab names |
| 22 | Comet | Disaster recovery | https://docs.cometbackup.com/latest/guides/guide-disaster-recovery | replacement-device procedure; revoke |
| 23 | Comet | Common problems | https://docs.cometbackup.com/latest/guides/guide-common-problems-and-solutions | stuck jobs; retry; VSS diagnosis; vault lock |
| 24 | Comet | Troubleshooting | https://docs.cometbackup.com/latest/troubleshooting | lock / reindex / abandoned procedures |
| 25 | Comet | Appendix | https://docs.cometbackup.com/latest/appendix | agent CLI states; registry keys; proxy env; Event Log IDs |
| 26 | Comet | Client overview, chunking, encryption | https://docs.cometbackup.com/latest/Overview-and-concepts/comet-client-overview ; https://docs.cometbackup.com/latest/Overview-and-concepts/comet-chunking-overview ; https://docs.cometbackup.com/latest/Overview-and-concepts/Encryption-and-key-management | key hierarchy; CDC chunking (pattern only) |
| 27 | Comet | Storage methods / Object Lock | https://docs.cometbackup.com/latest/storage-configuration/Storage-methods ; https://docs.cometbackup.com/latest/storage-configuration/s3-object-locking | direct vs gateway; compliance-mode rules |
| 28 | Comet | API guide, other APIs | https://docs.cometbackup.com/latest/api/api-guide ; https://docs.cometbackup.com/latest/api/other-apis | public API; webhooks; event stream; MCP; install tokens |
| 29 | Comet | API methods (public reference) | https://docs.cometbackup.com/latest/api/api-methods | admin + end-user method surface |
| 30 | Comet | API data structures (public reference) | https://docs.cometbackup.com/latest/api/api-data-structures | profile, device, rule, schedule, retention, job, policy structures |
| 31 | Comet | API constants (public reference) | https://docs.cometbackup.com/latest/api/api-constants | job status, classification, frequency, search operators |
| 32 | Comet | Third-party add-ons | https://docs.cometbackup.com/latest/third-party-service-addons | Syncro, NinjaOne, Backup Radar, CheckCentral and others |
| 33 | Comet | Security and compliance | https://docs.cometbackup.com/latest/security-compliance | telemetry to vendor; HIPAA/GDPR posture |
| 34 | Comet | Billing guide | https://docs.cometbackup.com/latest/guides/Managing_Your_Comet_Billing | billing triggers (out of scope; contrast) |
| 35 | Comet | Pricing page | https://cometbackup.com/pricing/ | plans; feature gating |
| 36 | Comet | Features page | https://cometbackup.com/features/ | feature claims by plan |
| 37 | Comet | Integrations page | https://cometbackup.com/integrations | partner list |
| 38 | Comet | Videos page | https://cometbackup.com/videos | titles only, as evidence that UI areas exist; not transcribed |
| 39 | Comet | Release notes | https://docs.cometbackup.com/release-notes ; https://docs.cometbackup.com/release-notes/26-4-4-phoebe-released | scheduled throttling, Recent Activity filters, wake trigger |
| 40 | Comet | Support KB: admin roles | https://support.cometbackup.com/hc/en-us/articles/41909235394839 | no read-only admin role |
| 41 | Cove | Getting started | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/GettingStarted-Home.htm | setup order; cloud-first |
| 42 | Cove | Retention policies overview | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/service-management/console-new/retention-policies/retention-policies-overview.htm | policy model; default 30-day values |
| 43 | Cove | Backup FAQ | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/backup-manager/backup-manager-guide/faqs-backup.htm | defaults; refresh cadence; read-only mode |
| 44 | Cove | Backup steps (schedules) | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/backup-manager/backup-manager-guide/backup-steps.htm | schedule-based vs frequency-based; off/sleep behaviour |
| 45 | Cove | Dashboard filtering | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/service-management/console-new/filtering.htm | four doughnut widgets; filter panel |
| 46 | Cove | Manage-by-exception blog | https://www.n-able.com/blog/backup-monitoring-part-1-manage-by-exception-2025-update | predefined views; saved views; 48-hour workflow |
| 47 | Cove | Devices | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/service-management/console-new/devices.htm | device properties tabs; history columns; delete = 30-day purge |
| 48 | Cove | Notification service | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/service-management/console-new/notification-service/add-notification.htm | status alerts; critical configuration change |
| 49 | Cove | Scheduled report emails | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/service-management/console-new/emails.htm | column/row caps; external opt-in; subject variables |
| 50 | Cove | Profiles | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/service-management/console-new/profiles.htm | profile scope and hierarchy |
| 51 | Cove | Users and roles | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/service-management/console-new/users.htm | roles; Security Officer permission |
| 52 | Cove | Managed install | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/backup-manager/backup-manager-installation/managed-install.htm | Add Device wizard; multi-use installer; default profile |
| 53 | Cove | Features by OS | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/backup-manager/backup-manager-guide/features.htm | accelerator, LSV, archiving, open files, BMR |
| 54 | Cove | Architecture and security guides | https://documentation.n-able.com/covedataprotection/USERGUIDE/QSG/Content/Architecture-and-Security/Cove-Architecture-Guide.htm ; https://documentation.n-able.com/covedataprotection/USERGUIDE/QSG/Content/Architecture-and-Security/Cove-Security-Guide.htm | change detection; encryption modes; passphrase gate; control plane |
| 55 | Cove | Fortified Copies | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/backup-manager/backup-manager-guide/fortified-copies.htm ; https://www.n-able.com/press/press-releases/n-able-announces-new-fortified-copies-feature-in-cove-data-protection-driving-stronger-ransomware-protection | hourly immutable copies, 30 days |
| 56 | Cove | LocalSpeedVault monitoring | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/backup-manager/backup-manager-guide/monitor-lsv.htm | sync statuses; 14-day auto-disable |
| 57 | Cove | Recovery Testing | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/service-management/console-new/recovery-testing/recovery-testing-overview.htm | hosted boot-screenshot proof (image-based; skipped) |
| 58 | Cove | Standby Image | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/service-management/console-new/standby-image/self-hosted-sbi-overview.htm | incrementally updated VM (image-based; skipped) |
| 59 | Cove | Recovery steps | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/backup-manager/backup-manager-guide/recovery-steps.htm | restore tab; session markers; restore-only mode |
| 60 | Cove | Remote commands | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/service-management/console-new/remote-commands/primary-remote-commands.htm | backup now, cancel, restart, update |
| 61 | Cove | Backup filters and exclusions | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/service-management/console-new/backup-filters-exclusions.htm | default exclusions via FilesNotToBackup |
| 62 | Cove | Performance (bandwidth) | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/backup-manager/backup-manager-guide/performance.htm | scheduled bandwidth limit |
| 63 | Cove | Documents device type | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/backup-manager/backup-documents/home-new-dash.htm | auto extension-based selection |
| 64 | Cove | Microsoft 365 domain properties | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/service-management/console-new/Microsoft365/365-domain-properties-new-dash.htm | M365 modelled as a device type |
| 65 | Cove | RMM integrations | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/external-cove-integrations/Datto/datto.htm ; https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/external-cove-integrations/N-Sight/n-sight.htm | no Syncro integration documented |
| 66 | Cove | JSON API | https://documentation.n-able.com/covedataprotection/USERGUIDE/documentation/Content/service-management/json-api/home.htm | JSON-RPC; session token; installation-token endpoint |
| 67 | Cove | Auto-update | https://documentation.n-able.com/covedataprotection/USERGUIDE/QSG/Content/backup-manager/backup-manager-installation/update-backup-manager.htm | auto-update for 16.11+ |
| 68 | Cove | Pricing and fair-use policy | https://www.n-able.com/products/cove-data-protection/pricing ; https://www.n-able.com/legal/cove-data-protection-fair-use-policy | flat per device; pooled fair use (contrast only) |
| 69 | Cove | Reseller listing (secondary) | https://www.shi.com/product/50962776 | MSRP [unverified, reseller] |
| 70 | Cove | Deployment series blog (not read, 403) | https://www.n-able.com/blog/deployment-series-unified-installation-with-cove-data-protection | silent switch table not captured |
| 71 | MSP360 | About / table of contents | https://help.mspbackups.com/about | components; feature bullets |
| 72 | MSP360 | Management console | https://help.mspbackups.com/getting-started/management-console | menu structure; Home widgets; one session per provider |
| 73 | MSP360 | File backup (new format) wizard | https://help.mspbackups.com/backup/file/nbf | 10 wizard steps; retention default 3 months; filters; encryption |
| 74 | MSP360 | Forever Forward Incremental | https://help.mspbackups.com/backup/about/ffi | Windows + new format; early-deletion table |
| 75 | MSP360 | Retention policy, GFS | https://help.mspbackups.com/backup/about/retention-policy ; https://help.mspbackups.com/backup/about/gfs/gfs ; https://help.mspbackups.com/backup/about/gfs/gfs-limitations | generations; GFS limits |
| 76 | MSP360 | Overdue | https://help.mspbackups.com/reporting/monitoring-history/overdue | two overdue triggers |
| 77 | MSP360 | Backup history | https://help.mspbackups.com/reporting/monitoring-history/backup-history | calendar grid; statuses; Worst filter |
| 78 | MSP360 | Manage computers | https://help.mspbackups.com/remote-management/manage-computers | computer statuses; filters; group actions |
| 79 | MSP360 | Notification settings | https://help.mspbackups.com/organization/settings/notifications | consecutive failures; no-successful-backup threshold |
| 80 | MSP360 | Companies | https://help.mspbackups.com/organization/companies | company tabs; agent options; storage limit |
| 81 | MSP360 | Granular admin permissions | https://help.mspbackups.com/organization/administrators/granular-permissions | permission flags |
| 82 | MSP360 | 2FA and audit log | https://help.mspbackups.com/security/2fa/about ; https://help.mspbackups.com/organization/audit-log | forced 2FA; audit alerts |
| 83 | MSP360 | Master password | https://help.mspbackups.com/security/master-password | agent GUI/CLI protection |
| 84 | MSP360 | Silent installation | https://help.mspbackups.com/downloads/windows/silent-installation | installer switches; account add |
| 85 | MSP360 | Agent update | https://help.mspbackups.com/downloads/agent-update ; https://help.mspbackups.com/automation/agent-update | force update; public/sandbox builds |
| 86 | MSP360 | Client options | https://help.mspbackups.com/remote-management/client-options | remotely editable agent options |
| 87 | MSP360 | Bandwidth | https://help.mspbackups.com/backup/about/bandwidth | bandwidth schedule |
| 88 | MSP360 | File restore | https://help.mspbackups.com/restore/file/file-restore | restore plan options |
| 89 | MSP360 | Restore verification | https://help.mspbackups.com/backup/ibb/restore-verification | image boot check (skipped) |
| 90 | MSP360 | Reports | https://help.mspbackups.com/reporting/reports/storage-report ; https://help.mspbackups.com/reporting/reports/backuphistory-report ; https://help.mspbackups.com/reporting/reports/compliance-report ; https://help.mspbackups.com/reporting/reports/summary | scheduled report types |
| 91 | MSP360 | Rebranding | https://help.mspbackups.com/rebranding/rebrand-backup-agent/get-started ; https://help.mspbackups.com/rebranding/rebrand-backup-agent/edit-brand | paid-licence gating (avoid) |
| 92 | MSP360 | REST API | https://help.mspbackups.com/mbs-api-specification ; https://help.mspbackups.com/mbs-api-specification/methods/get-apimonitoring | monitoring endpoint; no webhooks |
| 93 | MSP360 | MSP360 Storage | https://help.mspbackups.com/billing-storage/storage-providers/msp360-storage | vendor storage pricing (contrast) |
| 94 | MSP360 | Immutability / Object Lock | https://help.mspbackups.com/backup/about/gfs/immutability ; https://help.mspbackups.com/backup/about/gfs/object-lock/default | two pages contradict each other |
| 95 | MSP360 | M365 / Google Workspace licensing | https://help.mspbackups.com/gapps-office365/licensing ; https://help.mspbackups.com/gapps-office365/f_a_q | per-user licence; 90-day retention after cancel |
| 96 | MSP360 | Pricing page | https://get.msp360.com/paid/sbackupgeneral | tiers; managed from $2.5 to $5 per device |
| 97 | MSP360 | Vendor blog vs Cove (competitor-authored) | https://www.msp360.com/resources/blog/msp360-vs-cove-backup-comparison-for-msps-and-it-professionals/ | used only for unpublished Cove overage claim |
| 98 | MSP360 | FFI launch context (secondary) | https://channelbuzz.ca/2022/11/msp360-looks-to-slash-msp-costs-by-adding-forever-forward-incremental-backup-and-intelligent-retention-to-their-backup-40017/ | Intelligent Retention background |
| 99 | MSP360 | Pricing page (not read, 403) | https://www.msp360.com/pricing/ | blocked |
| 100 | IDrive | 360 console FAQ (primary) | https://www.idrive.com/endpoint-backup/faq_backup | companies, plans, schedules, CDP, exclusions, snapshots, alerts, reports |
| 101 | IDrive | 360 marketing | https://www.idrive.com/endpoint/ ; https://www.idrive.com/endpoint-backup | deployment paths; 10 TB per device |
| 102 | IDrive | 360 pricing | https://www.idrive.com/endpoint/pricing | tier table (contrast) |
| 103 | IDrive | Entire Machine backup FAQ | https://www.idrive.com/endpoint-backup/machine-backup-faq | WinRE media; limits (skipped) |
| 104 | IDrive | Business dashboard FAQ | https://www.idrive.com/dashboard-faq | users/groups; computers; propagate; reports |
| 105 | IDrive | Windows app FAQ | https://www.idrive.com/online-backup-faq | missed-backup rule; versions; throttle; cleanup |
| 106 | IDrive | Scheduler help | https://www.idrive.com/help/Windows/schedule_backup.htm | scheduler fields |
| 107 | IDrive | CDP help | https://www.idrive.com/help/Windows/continuous_data_protection ; https://www.idrive.com/online-backup-cdp-faq | CDP semantics (skipped) |
| 108 | IDrive | Security FAQ | https://www.idrive.com/online-backup-security-faq | default vs private key; 2FA |
| 109 | IDrive | Pricing | https://www.idrive.com/pricing | tiers (contrast) |
| 110 | IDrive | e2 pricing and policies | https://www.idrive.com/e2/pricing ; https://e2help.idrive.com/hc/en-us/articles/10910766716573-IDrive-e2-pricing-policies | S3-compatible target |
| 111 | IDrive | Dashboard launch post (dated) | https://blog.idrive.com/2016/05/13/manage-all-your-devices-easily-with-our-brand-new-dashboard/ | propagate and group policies |
| 112 | IDrive | BMR appliance | https://www.idrive.com/bare-metal-recovery/ ; https://bmrhelp.idrive.com/hc/en-us/sections/9240536299037-Manage-devices | separate appliance product (skipped) |
| 113 | IDrive | Press: IDrive 360 (secondary) | https://www.helpnetsecurity.com/2021/09/15/idrive-360/ | image versions claim conflicts with docs |
| 114 | IDrive | Blocked pages (not read) | https://bmrhelp.idrive.com/hc/en-us/articles/9743345928093 | 403; blog post p=4884 returned 504 |
| 115 | Acronis | Start conditions | https://www.acronis.com/en/support/documentation/CyberProtectionService/start-conditions.html | 8 conditions; fallback period |
| 116 | Acronis | Battery, idle, metered | https://www.acronis.com/en/support/documentation/CyberProtectionService/save-battery-power.html ; https://www.acronis.com/en/support/documentation/CyberProtectionService/user-is-idle.html ; https://www.acronis.com/en/support/documentation/CyberProtectionService/do-not-start-when-on-metered-connection.html | condition semantics |
| 117 | Acronis | Schedule time and scheduling | https://www.acronis.com/en/support/documentation/CyberProtectionService/schedule-time.html ; https://www.acronis.com/en/support/documentation/CyberProtectionService/schedule.html | missed tasks default disabled; sleep options |
| 118 | Acronis | Backup options matrix | https://www.acronis.com/en/support/documentation/CyberProtectionService/backup-options.html | option availability |
| 119 | Acronis | Retention rules | https://www.acronis.com/en/support/documentation/CyberProtectionService/retention-rules.html ; https://www.acronis.com/en/support/documentation/CyberProtectionService/retention-rules-backup-scheme.html | cleanup before/after semantics |
| 120 | Acronis | Encryption | https://www.acronis.com/en/support/documentation/CyberProtectionService/encryption.html | customer-held password, unrecoverable |
| 121 | Acronis | Performance and backup window | https://www.acronis.com/en/support/documentation/CyberProtectionService/performance-and-backup-window.html | hour grid; kill on prohibited hour |
| 122 | Acronis | CDP, file filters, task failure handling | https://www.acronis.com/en/support/documentation/CyberProtectionService/continuous-data-protection.html ; https://www.acronis.com/en/support/documentation/CyberProtectionService/file-filters.html ; https://www.acronis.com/en/support/documentation/CyberProtectionService/task-failure-handling.html | CDP NTFS only; reboot resume (snippet) |
| 123 | Acronis | Service console, agent updates | https://www.acronis.com/en/support/documentation/CyberProtectionService/service-console.html ; https://www.acronis.com/en/support/documentation/CyberProtectionService/updating-agents.html | offline definition; update channel |
| 124 | Acronis | Audit log, 2FA, roles | https://www.acronis.com/en/support/documentation/AcronisCyberCloud/audit-log.html ; https://www.acronis.com/en/support/documentation/AcronisCyberCloud/2fa.html ; https://www.acronis.com/en/support/documentation/AcronisCyberCloud/user-roles-read-only-admin.html ; https://www.acronis.com/en/support/documentation/AcronisCyberCloud/user-roles-available-for-each-service.html | audit fields, 180 days; read-only admin |
| 125 | Acronis | RMM monitoring API | https://developer.acronis.com/doc/vendor-side/rmm/monitoring.html | per-workload status fields |
| 126 | Acronis | Unattended agent install | https://www.acronis.com/en/blog/posts/how-to-personalize-unattended-acronis-agent-installation-on-windows/ ; https://www.acronis.com/en/support/documentation/AcronisCyberProtect_16/creating-mst-transform-extracting-installation-package.html | registration token switches; GPO transform |
| 127 | Acronis | Partner pricing PDFs | https://dl.acronis.com/u/rc/PAYG-Pricing-for-Acronis-service-provider-partners-EN-US.pdf ; https://www.acronis.com/legal/licensing.htm | per-workload vs per-GB; M365 seat definition (contrast) |
| 128 | Acronis | Reseller pricing (secondary) | https://cobweb.com/acronis-per-workload-pricing/ | example RRP |
| 129 | Acronis | Alert names in the wild (secondary) | https://www.liquidweb.com/help-docs/acronis-notifications/ | alert catalogue [unverified] |
| 130 | Datto | EB2 backup policy | https://continuity.datto.com/help/Content/kb/EB2/EB2-create-backup-policy.htm | business-hours default; custom windows; throttling; alert thresholds |
| 131 | Datto | EB2 retention | https://continuity.datto.com/help/Content/kb/EB2/EB2-retention.htm | 90 d / 1 y / 3 y / 7 y tiers |
| 132 | Datto | EB2 selective backup | https://continuity.datto.com/help/Content/kb/EB2/EB2-selective.htm | pattern grammar; exclusions beat inclusions |
| 133 | Datto | EB2 deployment | https://continuity.datto.com/help/Content/kb/EB2/EB2-deploying-backup-agents.htm | msiexec token; managed vs customer key |
| 134 | Datto | EB2 setup | https://continuity.datto.com/help/Content/kb/EB2/EB2-setup.htm | organizations sync; groups |
| 135 | Datto | EB2 overview | https://continuity.datto.com/help/Content/kb/EndpointBackup_v2/EndpointBackupv2.htm | policy-driven; restore types |
| 136 | Datto | EB status page | https://continuity.datto.com/help/Content/kb/EB/EB-StatusPage.htm | last-ten-days semantics |
| 137 | Datto | DEBPC status, settings, agent | https://continuity.datto.com/help/Content/kb/DEBPC/360000997103.html ; https://continuity.datto.com/help/Content/kb/DEBPC/360028796731.html ; https://continuity.datto.com/help/Content/kb/DEBPC/360001010326.html | ten-day circles; throttling; tray icon |
| 138 | Datto | BMR | https://continuity.datto.com/help/Content/kb/EB/EB-BMRGettingStarted.htm ; https://continuity.datto.com/help/Content/kb/DEBPC/360029124392.html | emergency-only caveats (skipped) |
| 139 | Datto | Retention definition (Kaseya) | https://helpdesk.kaseya.com/hc/en-gb/articles/43799368493201 | TBR thinning |
| 140 | Datto | UniView roles | https://help.unitrends.kaseya.com/uniview-help/Content/MSP/groups-users.htm | Superuser/Admin/Manage/Monitor; groups |
| 141 | Datto | License usage visibility | https://www.kaseya.com/es-la/platform/innovations/endpoint-backup-license-usage-visibility-uniview/ | role-based license data |
| 142 | Datto | Product features | https://datto.com/products/endpoint-backup/pc-backup/features | auto-pause/resume; screenshot verification (snippet) |
| 143 | Datto | RMM packaging (secondary) | https://community.immy.bot/t/datto-backup-agent-and-cloud-continuity-agent-install-update/2046 | community install notes |
| 144 | Datto | Reseller pricing (secondary) | https://invenioit.com/datto-backup/datto-end-point-backup-pcs-servers/ | price claims [unverified] |
| 145 | Axcient | D2C schedules | https://help.axcient.com/002-install-direct-to-cloud/929226-x360recover-create-direct-to-cloud-schedules | initial window; interval; assignment |
| 146 | Axcient | Retention Policy Manager | https://axcient.helpjuice.com/2-configure-x360recover/how-to-use-retention-policy-manager-x360recover | Recommended / Space Saving; workstation vs server |
| 147 | Axcient | Health status reports | https://axcient.helpjuice.com/health-status-reports/1443722-health-status-reports-for-protected-systems | Parked / Normal / Warned / Troubled rules |
| 148 | Axcient | Protected system details | https://axcient.helpjuice.com/1-navigate-x360recover-manager-rmc/protected-system-details-page-x360recover | details panels; per-device alert override |
| 149 | Axcient | Health dashboard | https://axcient.helpjuice.com/3-manage-x360recover/2-health-dashboard-x360recover | widgets; quick filters; refresh cadence |
| 150 | Axcient | Alert definitions | https://help.axcient.com/x30recover-faqs-appliance-errors-and-troubleshooting/115004693847-x360Recover-Alert-Definitions | alert classes; auto-close on success |
| 151 | Axcient | Local cache for D2C | https://axcient.helpjuice.com/5-recover-with-x360recover/local-cache-for-d2c ; https://help.axcient.com/003-manage-d2c/x360recover-local-cache-verification-2 | cache semantics; verification |
| 152 | Axcient | Agent install (Windows) | https://help.axcient.com/install-an-agent/agent-install-windows-x360recover | 2.x MSI vs 3.x EXE; token parameters |
| 153 | Axcient | D2C billing | https://help.axcient.com/003-manage-d2c/360050734614-x360Recover-Direct-to-Cloud-D2C-Billing ; https://axcient.helpjuice.com/billing-resources/360059272473-x360Recover-Billing-Overview | usage-based (contrast) |
| 154 | Axcient | Virtual Office billing | https://help.axcient.com/virtual-office-best-practices/virtual-office-billing-faqs-x360recover-vo | image failover (skipped) |
| 155 | Axcient | Recovery options, file recovery | https://axcient.helpjuice.com/175381-recover-with-replibit/x360recover-recovery-options ; https://help.axcient.com/5-recover-with-x360recover/x360recover-file-and-folder-recovery ; https://help.axcient.com/5-recover-with-x360recover/recovery-center | Recovery Center |
| 156 | Axcient | Throttling | https://help.axcient.com/001-install-bdr-/1500000637281-x360Recover-How-to-set-agent-throttling-options ; https://help.axcient.com/06-monitor/360023090853-x360Recover-Agent-Disk-I-O-Throttling | bandwidth, disk I/O, CPU priority |
| 157 | Axcient | Encryption overview | https://help.axcient.com/x360recover-faqs-passwords-and-networks/360042898374--x360Recover-Encryption-Overview-Agent-2-23-Appliance-OS-8-2-0- | vendor-held keys |
| 158 | Axcient | MFA configuration | https://axcient.helpjuice.com/x360Portal-User-Guide/877905-configuring-multifactor-authentication | default MFA; lockout |
| 159 | Axcient | Health digest, org settings | https://help.axcient.com/health-status-reports/configure-the-health-digest-for-an-organization-x360recover ; https://help.axcient.com/health-status-reports/x360recover-manager-configure-settings | SLA thresholds; digest; roles |
| 160 | Axcient | PSA integration | https://help.axcient.com/3-manage-x360recover/x360recover-manager-integrate-psa ; https://help.axcient.com/004-integrate-psa/connectwise-asio-integration | ticket sync |
| 161 | Axcient | Feature focus, AirGap, AutoVerify | https://axcient.com/blog/feature-focus-x360recover/ ; https://help.axcient.com/en_US/about-x360recover | AirGap deletion separation; boot verification (skipped) |
| 162 | Axcient | D2C index (SSO-gated, not read) | https://help.axcient.com/en_US/d2c-[index] | redirected to SSO |
| 163 | Veeam | Editions | https://helpcenter.veeam.com/docs/agentforwindows/userguide/license_editions.html | Free / Workstation / Server feature table |
| 164 | Veeam | Job schedule (workstation) | https://helpcenter.veeam.com/docs/agentforwindows/userguide/backup_job_schedule_free_desktop.html | daily-at; powered-on catch-up; events; post-job actions |
| 165 | Veeam | Scheduled backup missed | https://helpcenter.veeam.com/docs/agentforwindows/userguide/scheduled_backup_missed.html | 15-minute tolerance; Modern Standby delay |
| 166 | Veeam | Metered connections | https://helpcenter.veeam.com/docs/agentforwindows/userguide/metered_connections.html | restrict; fails without cache |
| 167 | Veeam | Backup cache | https://helpcenter.veeam.com/docs/agentforwindows/userguide/backup_cache.html | local queue; sync on return |
| 168 | Veeam | Resources settings | https://helpcenter.veeam.com/docs/agentforwindows/userguide/settings_resources.html | throttle when busy |
| 169 | Veeam | Advanced storage | https://helpcenter.veeam.com/docs/agentforwindows/userguide/backup_job_advanced_storage.html | compression; block size; encryption |
| 170 | Veeam | Volumes, filters | https://helpcenter.veeam.com/docs/agentforwindows/userguide/backup_job_volumes.html ; https://helpcenter.veeam.com/docs/vbr/userguide/appendix_d_windows_computer_backup_filters.html | automatic exclusions; mask rules |
| 171 | Veeam | Retention in days (archived 2.x doc) | https://helpcenter.veeam.com/archive/agentforwindows/20/userguide/retention_days.html | days a backup ran |
| 172 | Veeam | Restore tasks | https://helpcenter.veeam.com/docs/agentforwindows/userguide/performing_restore_tasks.html | restore options (volume/VM paths skipped) |
| 173 | Veeam | Service Provider Console alarms | https://helpcenter.veeam.com/docs/vac/provider_admin/appendix_alarms.html ; https://helpcenter.veeam.com/docs/vac/provider_admin/alarms.html | predefined alarms; severities |
| 174 | Veeam | VSPC policies, agent settings | https://helpcenter.veeam.com/docs/vac/provider_admin/configure_backup_schedule.html ; https://helpcenter.veeam.com/docs/vac/provider_admin/configure_backup_agent_settings.html | policy wizard; wake-up setting |
| 175 | Veeam | VSPC users, subscriptions | https://helpcenter.veeam.com/docs/vac/provider_admin/manage_admin_portal_users.html ; https://helpcenter.veeam.com/docs/vac/provider_quick_start/specify_agent_rates.html ; https://helpcenter.veeam.com/docs/vac/provider_admin/create_subscriptions.html | roles; price sheets (contrast) |
| 176 | Veeam | VSPC MFA, REST | https://helpcenter.veeam.com/docs/vac/provider_user/mfa.html ; https://helpcenter.veeam.com/docs/vac/rest/mfa.html ; https://helpcenter.veeam.com/docs/vac/rest/reference_v3.html | TOTP; API keys; REST v3 |
| 177 | Veeam | Protection groups, Modern Standby | https://helpcenter.veeam.com/docs/backup/agents/protection_group_options.html ; https://helpcenter.veeam.com/docs/vbr/userguide/agent_policy_win_schedule_workstation.html | auto-update agent; resume delay |
| 178 | UrBackup | Administration manual (GPL project) | https://www.urbackup.org/administration_manual.html | settings, intervals, windows, speeds, dirs, permissions, restore |
| 179 | UrBackup | FAQ | https://www.urbackup.org/faq.html | glob semantics; trust-on-first-use; EFS/.ost limits |
| 180 | UrBackup | Features | https://www.urbackup.org/features.html | feature claims |
| 181 | UrBackup | Server status source | https://raw.githubusercontent.com/uroni/urbackup_backend/dev/urbackupserver/serverinterface/status.cpp | status JSON derivation (behaviour only) |
| 182 | UrBackup | Server scheduling source | https://raw.githubusercontent.com/uroni/urbackup_backend/dev/urbackupserver/ClientMain.cpp | due-ness loop; backoff (behaviour only) |
| 183 | UrBackup | Alert thread and script | https://raw.githubusercontent.com/uroni/urbackup_backend/dev/urbackupserver/Alerts.cpp ; https://raw.githubusercontent.com/uroni/urbackup_backend/dev/urbackupserver/alert.lua | async status computation (the pattern to avoid) |
| 184 | UrBackup | Cleanup source | https://raw.githubusercontent.com/uroni/urbackup_backend/dev/urbackupserver/server_cleanup.cpp | cleanup order; emergency cleanup |
| 185 | UrBackup | Client source | https://raw.githubusercontent.com/uroni/urbackup_backend/dev/urbackupclient/ClientServiceCMD.cpp ; https://raw.githubusercontent.com/uroni/urbackup_backend/dev/urbackupclient/ClientService.cpp | pause, metered, settings merge, overdue states |
| 186 | UrBackup | Server file inventory | https://github.com/uroni/urbackup_backend/tree/dev/urbackupserver | file list |
| 187 | UrBackup | Forum: battery, pause, laptops, status | https://forums.urbackup.org/t/feature-request-do-not-backup-when-on-battery/2410 ; https://forums.urbackup.org/t/urbackup-speed-paused/7365 ; https://forums.urbackup.org/t/how-do-yall-backup-laptops-get-around-sleep-hibernate/12214 ; https://forums.urbackup.org/t/no-recent-backup-not-shown-into-image-backup-status/2188 ; https://forums.urbackup.org/t/status-is-no-recent-backup-should-be-disabled/4988 ; https://forums.urbackup.org/t/is-the-configuration-of-email-alerts-buggy/13458 | community-sourced; failure modes |
| 188 | Engine | restic documentation | https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html ; https://restic.readthedocs.io/en/stable/040_backup.html ; https://restic.readthedocs.io/en/stable/045_working_with_repos.html ; https://restic.readthedocs.io/en/stable/047_tuning_parameters.html ; https://restic.readthedocs.io/en/stable/050_restore.html ; https://restic.readthedocs.io/en/stable/060_forget.html ; https://restic.readthedocs.io/en/stable/070_encryption.html ; https://restic.readthedocs.io/en/stable/075_scripting.html ; https://restic.readthedocs.io/en/stable/100_references.html ; https://restic.readthedocs.io/en/stable/manual_rest.html ; https://restic.readthedocs.io/en/stable/faq.html | VSS, exclusions, JSON, exit codes, restore, forget/prune, keys, repo format |
| 189 | Engine | restic releases and changelog | https://github.com/restic/restic/releases ; https://github.com/restic/restic/releases/tag/v0.17.0 ; https://github.com/restic/restic/releases/tag/v0.16.0 ; https://raw.githubusercontent.com/restic/restic/master/CHANGELOG.md ; https://raw.githubusercontent.com/restic/restic/master/doc/cache.rst | Windows fixes; 0.19.1 is latest (2026-07-05) |
| 190 | Engine | restic source files (read as behaviour) | https://raw.githubusercontent.com/restic/restic/master/cmd/restic/cmd_backup.go ; https://raw.githubusercontent.com/restic/restic/master/cmd/restic/cmd_restore.go ; https://raw.githubusercontent.com/restic/restic/master/cmd/restic/cmd_forget.go ; https://raw.githubusercontent.com/restic/restic/master/cmd/restic/cmd_check.go ; https://raw.githubusercontent.com/restic/restic/master/cmd/restic/cmd_cache.go ; https://raw.githubusercontent.com/restic/restic/master/cmd/restic/cmd_stats.go ; https://raw.githubusercontent.com/restic/restic/master/internal/fs/fs_local_vss.go ; https://raw.githubusercontent.com/restic/restic/master/internal/repository/lock.go | flags, VSS fallback, locks |
| 191 | Engine | restic PRs and issues | https://github.com/restic/restic/pull/3067 ; https://github.com/restic/restic/pull/4614 ; https://github.com/restic/restic/issues/2745 | VSS design; ADS PR closed unmerged; reserved names |
| 192 | Engine | restic forum threads (community, not authoritative) | https://forum.restic.net/t/windows-junction-backup-with-go1-23-or-later/8940 ; https://forum.restic.net/t/memory-usage-oom-etc-on-a-big-repository/8758 ; https://forum.restic.net/t/restic-prune-runtime-out-of-memory/6902 ; https://forum.restic.net/t/any-way-to-get-restic-to-work-with-s3-object-locking-or-a-read-only-key/6858 ; https://forum.restic.net/t/restic-malware-and-object-locks/7079 ; https://forum.restic.net/t/wasabi-compliance-bucket-and-lockfiles/2318 ; https://forum.restic.net/t/anyone-using-restic-with-b2-backblaze-and-object-locks/4277 ; https://forum.restic.net/t/restic-mount-windows/898 | junctions; memory; Object Lock; mount |
| 193 | Engine | restic rest-server | https://github.com/restic/rest-server ; https://raw.githubusercontent.com/restic/rest-server/master/README.md ; https://raw.githubusercontent.com/restic/rest-server/master/repo/repo.go | quota, metrics, append-only policy |
| 194 | Engine | rclone serve restic, S3, B2 | https://rclone.org/commands/rclone_serve_restic/ ; https://raw.githubusercontent.com/rclone/rclone/master/cmd/serve/restic/restic.go ; https://rclone.org/s3/ ; https://rclone.org/b2/ ; https://forum.rclone.org/t/serve-restic-append-only-environment-variable/17050 ; https://www.rsync.net/resources/notes/2025-q4-rsync.net_technotes.html | gateway flags; append-only semantics |
| 195 | Engine | Storage providers | https://wasabi.com/pricing ; https://docs.wasabi.com/docs/how-does-wasabis-minimum-storage-duration-policy-work ; https://docs.wasabi.com/docs/object-lock-with-the-wasabi-s3-api ; https://docs.wasabi.com/apidocs/object-lock-on-buckets-with-the-wasabi-s3-api ; https://www.backblaze.com/cloud-storage/pricing ; https://www.backblaze.com/cloud-storage/transaction-pricing ; https://www.backblaze.com/docs/cloud-storage-object-lock ; https://www.backblaze.com/docs/cloud-storage-s3-compatible-api ; https://www.backblaze.com/docs/cloud-storage-call-the-s3-compatible-api ; https://www.idrive.com/s3-storage-e2/pricing ; https://www.idrive.com/e2/object-lock ; https://idrive.com/e2/faq-dashboard | pricing, minimum duration, Object Lock (pages change; re-check before quoting) |
| 196 | Engine | Go Windows service libraries | https://pkg.go.dev/golang.org/x/sys/windows/svc ; https://raw.githubusercontent.com/golang/sys/master/windows/svc/service.go ; https://pkg.go.dev/golang.org/x/sys/windows/svc/eventlog ; https://pkg.go.dev/golang.org/x/sys/windows ; https://raw.githubusercontent.com/golang/sys/master/windows/types_windows.go ; https://pkg.go.dev/github.com/kardianos/service ; https://raw.githubusercontent.com/kardianos/service/master/service_windows.go ; https://raw.githubusercontent.com/minio/selfupdate/master/apply.go ; https://pkg.go.dev/embed | power/session events; job objects; self-update |
| 197 | Engine | Microsoft documentation | https://learn.microsoft.com/en-us/windows/win32/api/winsvc/nc-winsvc-lphandler_function_ex ; https://learn.microsoft.com/en-us/windows/win32/power/wm-powerbroadcast ; https://learn.microsoft.com/en-us/windows/win32/termserv/wm-wtssession-change ; https://learn.microsoft.com/en-us/windows/win32/api/winsvc/nf-winsvc-createservicew ; https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/code-signing-options ; https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/smartscreen-reputation | handler contract; power events; signing and SmartScreen |
| 198 | Engine | Packaging | https://docs.firegiant.com/wix/schema/wxs/serviceinstall/ ; https://github.com/mh-cbon/go-msi ; https://nsis.sourceforge.io/NSIS_Simple_Service_Plugin | WiX ServiceInstall; alternatives |

Blocked or not read (no usable URL): Comet support-centre section index and a review-site page returned HTTP 403; Veeam silent-install switches, Acronis file-filter syntax pages and Datto alert-threshold values were not reached. A product-datasheet PDF for IDrive 360 could not be rendered. These gaps are tagged where they matter.

## Core loop

The MSP loop is: **enroll a device** (installer carries a customer-scoped token, the device registers itself, and the console provisions its repository, gateway login and escrowed key) then **assign policy** (per customer, with per-device override) then **scheduled backup runs** (interval-due scheduling inside allowed windows, with missed-run catch-up on reconnect or boot and run-on-wake for laptops, delay-not-cancel start conditions, and defer-not-fail when the gateway or a lock is unavailable) then **monitor and alert** (device state computed at read time from timestamps and policy, so a device that stops reporting becomes an alarm, never a quiet green; exceptions first, grouped by customer, with a daily digest and acknowledge/snooze) then **restore** (point-in-time browse, restore on the device or a technician-side download that works when the device is dead).

Click counts reconned from Comet (happy path, admin side, after the installer has run), and the number to beat:

| Task | Comet today | Number to beat (CryoBackup) |
| --- | --- | --- |
| Enroll a new device and see it green | Lobby path F01: Devices, device, Register, choose user, confirm = 4-5 clicks; then either wait for the first scheduled run or force it from the device action dialog (about 4-5 more clicks [est]) and open Recent Activity (2 clicks) = about 10-12 clicks to force-green. Manual path F03 is about 12 clicks end to end. | 0 clicks per device: the installer carries a customer token, the device appears as "pending first backup (grace until T)" and turns green by itself. One-time cost of 3-4 clicks per customer to generate the token and script (B16). |
| Restore a file | F07: 6-8 clicks (Restore, vault, item and snapshot, Choose files, destination, overwrite rule, Restore). Remote restore from the console is the same wizard and needs the device online. | 5 clicks or fewer from the console, device online or not: customer, device, snapshot (latest preselected), search or tick the file, Download or Restore here (B17, F22). |

## Screens

Comet inventory carried over with stable IDs: 39 console screens (S01-S39) and 15 agent screens (A01-A15). Comet's console is a single-page app with a left menu (Management Console: Users, Devices, Backup Job Reporting with Job logs and Recent activity; Storage Gateway; Cluster Manager; System with Logs, Replication, Settings, About), a top-right user menu (My Account, Advanced options, Log out) and a sticky Paste action after copying a Protected Item. API method names and internal codes are omitted deliberately. "[gap]" marks states the public docs do not show.

### Console (admin side)

| ID | screen | route / how to reach | purpose | key components | states seen |
| --- | --- | --- | --- | --- | --- |
| S01 | Console login | browser to console URL (tenant hostname shows tenant branding) | admin auth | username + password; TOTP or WebAuthn challenge step; OIDC button (Entra ID / Google / generic); optional public "Download backup agent" link; custom CSS/JS injection (26.8.2) | filled; bad credentials; MFA challenge; branded per tenant |
| S02 | Home / dashboard | login, default route | landing | news area (hideable), optional blog link, running-job count (26.4.4), history/info widgets gated by admin permission | [gap] widget layout not documented |
| S03 | Users list | menu: Users | all customer accounts | Add user, Add user group, Move users to group, Assign/Change/Remove group; live Filter; Show N entries pager; export (clipboard TSV, CSV, Excel); column picker; grouped vs ungrouped (default group "Ungrouped"); Advanced: bulk replace vault addresses | filled; grouped / ungrouped; [gap] empty state and columns |
| S04 | Add User dialog | S03: Add user, or quick-actions menu | create customer account | username, password; allow-admin-password-reset; auto-create vaults for new devices (none / system default / storage template); policy dropdown (Default Policy preselected); group; Advanced: create many by CSV | single; bulk CSV |
| S05 | User profile: Profile tab | S03, click user | account settings | username (immutable); password reset; irreversible flags (admin may reset password, admin may view file names); 2FA state; auto-vault template; group; language; timezone (IANA, default UTC); quotas (all items, device count, per-engine); Extra Fields (26.8.4); New Tenant transfer; Actions: Suspend/Unsuspend, Reset Password, Disable 2FA; Advanced: edit raw profile, Login as user | active; suspended (still billed); [gap] |
| S06 | User profile: Devices tab | S05: Devices | enrolled endpoints for this account | rows: friendly name (default hostname, renamable), device ID, OS, client version, status badge Online / Online (2) / Online (outdated) / Offline; Terminate on double connection; Revoke; click for action dialog | Online; Online (n); Online (outdated); Offline; Unregistered |
| S07 | Device action dialog (live) | S06 device; also reachable by deep link | remote control of a connected agent | Immediately run backup; run a scheduled backup; Remotely restore; Update software; Uninstall software; Update login URL; Refresh profile; Import/scan other products; Apply retention rules now; Advanced: Drop connection, Terminate process, Unlock lock files, Reindex, Deep verify, Multiple actions | only for Online devices; disabled when Offline |
| S08 | Run Backup dialog (remote) | S07: run backup | trigger a job now | pick Protected Item and Storage Vault (or an existing schedule); advanced job options | [gap] |
| S09 | Remote Restore dialog | S07: Remotely restore | restore to the device from the console | same wizard as A10; Advanced: delete a single snapshot (19.12.2+), view snapshot IDs (21.12.8+) | browse; loading snapshot list (needs live device) |
| S10 | User profile: Protected Items tab | S05: Protected Items | backup sets per device | items grouped by device; Copy link then Paste in top bar (clone to another device/user); add/edit/delete (policy may lock) | filled; locked-by-policy [inferred] |
| S11 | Protected Item editor | S10 item (deep-linkable by tab) | create/edit a backup set | tabs: general, auth (network-share login), items (tree with live size estimate; include/exclude filters glob or regex; take filesystem snapshot default on; rescan unchanged; extra permissions; EFS confirm), commands (before / after / thaw), schedule (S12), retention (S13) | new; edit; [gap] validation |
| S12 | Schedule editor (Backup Rule) | S11: schedule tab | when an item backs up and where | target vault or selector (latest / all / type); frequencies: Hourly, Daily, Weekly, Monthly, Yearly, Once, Periodic (+Offset); runtime window and days; triggers "When PC starts" (also on wake, 23.6.0) and "If the last job was Missed"; retry count + minutes (24.6.6); skip if running; cancel if still running after; vault speed limit with time/day table (26.8.5); 1 disk thread; temp files vs RAM; allow zero-file success; random delay up to 300 min | multiple schedules per item; one per vault |
| S13 | Retention editor | S11 retention tab; S15; S25; A07 | which snapshots to keep | mode: keep everything vs rule list; range types (last N; last N one per day/week/month/year; all in last N days/weeks/months; newer than date; one per day/week/month/year for last N, with week-day/month-day offsets); apply-retention cadence (Automatic / immediately / more / less often) | default; overridden |
| S14 | User profile: Storage Vaults tab | S05: Storage Vaults | repositories for this account | list; Stored column opens Current vs Historic breakdown; Retention Pass; request via template or custom; show other device vaults | filled; uninitialised; suspended; RetentionError |
| S15 | Storage Vault editor | S14 vault (tabbed) | vault settings | general (name, default retention, storage limit); storage (type + credentials, hidden when rebranded); commands; encryption (informational); associated devices; Object Lock days; allow removal of deleted files; prevent user edit | template-created (storage tab masked); custom |
| S16 | User profile: Job Logs tab | S05: Job Logs | per-account job history | job rows open S22; export | [gap] columns |
| S17 | User profile: Policy tab | S05: Policies | assign a policy | dropdown of policies with a "(Custom)" marker; Save changes | default; named; custom |
| S18 | User profile: Reporting tab | S05: Reporting | per-account email reports | email list; "Email backup reports to this account" toggle; per-address report list (type, schedule, filter, title 26.6.0); "Policy default" row when enforced; preview | on/off; inherited vs custom |
| S19 | Devices page (all) | menu: Devices | fleet view | Connected Devices table (user, device, version, platform, IP, timezone, connected since); status badges; Terminate; Unregistered section with Register | Online; Online (outdated); Offline; Unregistered |
| S20 | Bulk Device Upgrade (campaigns) | Accounts: Bulk device upgrade, New Campaign | fleet agent upgrade | upgrade older / reinstall current / downgrade newer / force upgrade running; device rule builder (all-of / any-of / not-all / none, nested groups); per-device status Not seen, Ineligible, Pending, Request made, Update failed, Update confirmed, Incompatible OS; automatic campaign toggle | running campaign; none; tenant admins cannot see it |
| S21 | Job logs (Job History) | menu: Backup Job Reporting, Job logs | search all jobs | date range, user, clause builder (field / operator / value with AND/OR/NOT groups); rows with status, classification, user, device, item, vault, start/end, size; red Cancel on running; export | running; completed; filtered; empty [inferred] |
| S22 | Job detail / log viewer | S21 or S16 row | why a job failed | properties (status, times, totals, upload size, retry count, client version, snapshot ID); log entries filterable by severity/text; aggregated warnings and errors (26.4.4); report export; Cancel | Success, Warning, Error, Quota Exceeded, Missed, Cancelled, Skipped, Abandoned, Running, Running (revived), In Progress (retry), Not Yet Started |
| S23 | Recent activity | menu: Backup Job Reporting, Recent activity | 10-day at-a-glance grid | row per user/device/item; badges red Offline, yellow Outdated, red No Schedule; timeline blocks per day with job count (green all ok, red any failed, striped mixed, blank none yet); click block for that day's job search; job filter; show deleted items | mixed; all-green; blank-today |
| S24 | User Policies list | menu: Policies | policy objects | name, user count; row menu: Make default, edit, delete; Advanced: edit raw policy | default marked |
| S25 | Policy editor | S24 row | defaults and restrictions | sections: Summary; Storage Vaults (prevent actions, restrict types, auto-create vault, selection rules); Protected Items (prevent actions, default item + Update existing devices, restrict type); Default settings with 5-state mode (system default / optional on / optional off / always on / always off); mandatory exclusions; default email reports; default schedules (new items only); Retention (keep forever, enforce for all vaults, prevent override, allow Object Lock duration); app/account lock flags; vault key rotation hours | new; editing default; [gap] validation |
| S26 | Settings index | menu: System, Settings | server config | sections: serial number; network (listen, SSL / Let's Encrypt); session options (30 min default); audit logging; IP rate limiting; admin accounts; self-backup; integrations; system branding; console (login URL, keep job logs N days, missed-backup toggles, DB check, replication); storage template; email; agent branding and downloads; tenants; storage gateway; cluster manager; Save changes (some restart the service) | saved; restart-pending |
| S27 | Settings: Email Reporting | S26: Email reporting | outbound mail | deliver via None / MX direct / SMTP (name, address, server, port, user, password, SSL 465 / STARTTLS 587 / plain 25, accept invalid certs) / Gmail; custom HELO; hosted: SES + DNS validation or custom SMTP with test; global default report rules | configured; none |
| S28 | Settings: System Branding | S26: System branding | console white-label | page title, header image, header and accent colour, tab icon, blog-link option, custom JS/CSS | default; branded; gated by permission |
| S29 | Settings: Agent branding + codesigning | S26: Backup Agent branding | agent white-label and build | product / company name (also install folder and service names), help URL, default server URL, registration URL, icons, EULA, logo and tile images; codesigning via certificate or cloud KMS (Authenticode); build role limits | unsigned (MSI hidden); signed |
| S30 | Download backup agent | user menu or login page | get installers | per-platform rows (Windows EXE x86/x64, MSI x64 only when codesigned, macOS, Linux, NAS package); Copy as cURL / wget; download routes; Options: public download | public; disabled; MSI hidden |
| S31 | Settings: Tenants | S26: Tenants | isolated reseller zones | tenant list; hostname list; per-tenant admins, branding, email return address, policies, storage template, downloads; transfer user across tenants | active; suspended |
| S32 | Settings: Admin accounts | S26: Admin accounts | operators and permissions | username, password, allowed login (password / + TOTP / + WebAuthn), IP allowlist (config file only), permission flags (about 25), external auth sources (LDAP, OIDC) | no read-only role exists |
| S33 | Storage Templates / Gateway / Buckets | S26: Storage Template; menu: Storage Gateway, Storage buckets | where data lands | template (name, address local/remote, type + master key, Object Lock days, hide real type, quota); buckets page (size, Delete, Add bucket with random name/key shown once) | template vs custom; Object Lock on/off |
| S34 | Settings: Integrations | S26: Integrations | PSA / RMM hooks | Syncro (domain, API token), NinjaOne (OAuth, tickets toggle), ConnectWise (26.8.1); Sync now | authorized; not |
| S35 | Settings: Webhooks / Audit / Access / Self-backup | S26 subsections | event export and ops | webhooks (URL, headers, level, event whitelist, raw or chat payload); audit file sink (event filter, detail level, rotation days); IP rate-limit rules; console self-backup (location, schedule, retention, encryption key, compression, exclusions, Backup now) | enabled; disabled |
| S36 | System Logs / About / Replication | menu: System | server health | daily log files; version + codename, start time, licence validity, feature list; Advanced: shut down, restart, edit raw settings; replication status and deletion mode (None / Partial / Complete) | healthy; licence-offline |
| S37 | Device News / client bulletin | menu: News; Reports: client bulletin | push notices | news entries shown in agent; plain-text bulletin email to all users; Advanced remove all | empty; entries |
| S38 | My Account (admin) | top-right: My Account | own credentials | TOTP toggle + QR; WebAuthn toggle + "+" then "token saved"; personal API tokens (26.8.3) list/create/revoke | enabled; disabled |
| S39 | Customer Web Portal | console URL, log in as end user | self-service without the desktop app | protected items and vaults (same dialogs as S11/S15), job history, run backup/restore on live devices, my account (password, TOTP), news | policy may block |

### Desktop agent (Windows end-device side)

| ID | screen | route / how to reach | purpose | key components | states seen |
| --- | --- | --- | --- | --- | --- |
| A01 | Installer | run branded EXE or MSI | install service + UI | wizard: EULA (optional), install dir, shortcuts, tray icon; silent forms with lobby, pre-seeded credentials or install token; server URL switch; GPO via MSI | interactive; silent; SmartScreen warning if unsigned |
| A02 | Login / registration | first launch, or after Disconnect | bind device to a profile | username, password; Advanced Settings reveals Server field (prefilled from branding); registration-URL link; lobby waiting state; CLI login states 0-4 ("Unregistered device for the supplied credentials") | logged out; waiting in lobby; device-limit error; suspended |
| A03 | Main window: Backup tab | open app, Backup in left bar | list and run items | Protected Items header with hover Add (+); rows (name, type icon, last status [inferred]); Backup Now; Edit/Delete by context menu; click item for its job logs; running progress counters | empty; filled; running; policy-locked |
| A04 | Add / Edit Protected Item wizard | A03: Add | build a backup set | pages: name + type (restricted by policy); items (Choose files, drag-drop, include/exclude filters); take filesystem snapshot; commands; schedules; retention; Save | new; edit; "Backup now?" after save [inferred] |
| A05 | Choose files dialog | A04 Items: + | pick paths | drive/folder tree with checkboxes, live estimated size (pre-dedup), drag-drop | loading tree; large selection |
| A06 | Schedule editor | A04: Schedules, Add | as S12 | same fields as S12 | none beyond S12 |
| A07 | Retention editor | A04: Retention; or vault | as S13 | default vs rules | none beyond S13 |
| A08 | Backup Now dialog | A03: Backup Now | one-off run | choose destination vault; advanced options (force rescan [inferred]) | none |
| A09 | Job logs / progress view | A03 click item; History | see what happened | job list per item (status badge, start/end, size, uploaded); log entries with severity; running job progress and Cancel | running; retrying; completed |
| A10 | Restore wizard | left bar: Restore | get data back | step 1 vault; step 2 item (expand to older snapshot; Search; show items from other devices + Refresh; show unknown items for deleted items; right-click snapshot to delete); step 3 files (all or Choose files; EFS in green); step 4 destination (custom path or original paths) + overwrite rule (if different / if newer / always / never); restore types: files and folders, simulate only, program input, compressed archive (zip / tar / tar.gz / tar.zst / sqfs), granular, cloud or remote destination (26.8.3) | loading snapshots; empty vault; locked vault |
| A11 | Account tab | left bar: Account | vaults, devices, identity | Storage Vaults list (+ Add; Edit; Advanced: apply retention, rebuild indexes); registered devices list; Account, Import buttons; version; Help tab if branded | none |
| A12 | Add Storage Vault dialog | A11: + Add | new repository | choose a Storage Template (request from server) or Custom (local path, SFTP, S3-compatible, SMB, FTP, WebDAV, Azure, B2, Storj and others) | template; custom; policy may forbid custom |
| A13 | Account dialog | A11: Account | identity settings | username, change password, privacy flags visible to user, language, timezone, report email addresses; 2FA via web portal | policy may lock fields |
| A14 | Tray icon + exit menu | system tray | background control | open UI; Close (backups continue), Lock (log out of UI, backups continue), Disconnect (log out and stop schedules); tray can be disabled at install | locked; disconnected |
| A15 | Throttle / runtime settings | inside the schedule (S12), not a Settings tab | bandwidth and resource limits | speed limit with time-of-day / day-of-week table (26.8.5); 1 disk thread; temp files vs RAM; sleep prevention is automatic (18.3.9) | none |

Count: 39 console + 15 agent = 54 Comet screens.

### Borrowed screens (not in Comet, needed by the clone)

IDs are stable from here on. "From" names the product whose pattern the screen follows; the layout is original.

| ID | screen | from | route / how to reach | purpose | key components | states |
| --- | --- | --- | --- | --- | --- | --- |
| B01 | Fleet health board (per-customer) | Axcient | console home | exception-first overview; "who failed last night and whose machine is it" | customer rows with health gauge (percent within SLA), 7-day health history, quick filters (Troubled / Warned), expected-customer MISSING rows, freshness stamp, unreachable banner | all green; mixed; MISSING customer; console cannot reach gateway; partial data |
| B02 | Ten-day strip view | Datto | embedded in B01 rows and B07 | cheapest honest history | one circle per day: success, mixed, missed, none; hover for counts | new device (grey); paused |
| B03 | Backup-age ring dashboard | Cove | top of B01 | silence made visible | rings: state mix, last-success age (under 24 h / 24-48 h / over 48 h), unseen devices, customers; click a segment to filter the grid; saved views dropdown | empty fleet is an alarm, not an empty ring |
| B04 | Overdue list | MSP360 | B03 click or menu | everything that missed its SLA, with the reason | list with reason chips (missed run, no success in N days, interrupted, abandoned), age, customer, Acknowledge | none overdue; many overdue (grouped by customer) |
| B05 | Devices-not-seen alarm page | native (Comet and Cove lack it) | B03 unseen ring or menu | heartbeat silence per device and per customer | last seen, expected heartbeat, likely cause hint (cert / DNS / offline), snooze or acknowledge with note, retire action | none unseen; customer-wide silence collapsed to one row |
| B06 | Calendar-matrix history | MSP360 | menu: Reports | did it run every day this month | device by day matrix, Last / Worst toggle, Issue Only, run type, CSV export, click cell for run detail | month with gaps |
| B07 | Device detail panel | Cove (+ Axcient) | click any device row | everything about one device | tabs Summary, History, Errors, Settings (policy + override), Audit; 30-day health bar; ten-day strip; actions menu; key-escrow status; applied policy version | healthy; stale; unseen; paused; retired; first-backup pending |
| B08 | Customer page | native (Cove customers, UrBackup groups) | click a customer | devices, policy, storage usage, contacts, expected flag | device table, policy summary, usage meter, notification overrides, MISSING state, onboarding grace | zero devices (MISSING if expected) |
| B09 | Audit log viewer | Acronis | menu: Security | who did what | event, severity, date, object, customer, initiator, method (web/API), source IP; filters; no file paths | empty; filtered |
| B10 | Key custody and recovery | Cove + Datto | B07 key tab; Settings | escrow status, reveal with MFA, custody mode per customer | escrow health per device, reveal dialog (fresh MFA, reason, audit), custody mode warning text, master-key status | verified; missing escrow (alarm); customer-held |
| B11 | Storage usage and quota | MSP360 + Cove | menu: Storage | cost and growth visibility | per customer and device usage, growth rate, soft quota meter with 80/90/100 percent thresholds, next prune date | under quota; approaching; exceeded |
| B12 | Laptop start-conditions panel | Acronis + IDrive + Veeam | policy editor section | delay-not-cancel gating | battery floor, metered, Wi-Fi/VPN, idle, run-anyway-after-N, catch-up mode, startup delay | defaults for laptop vs desktop |
| B13 | Retention editor with calendar preview | Cove | policy editor section | see what survives | preset picker, rule editor, calendar greying dates that will be pruned, dry-run diff | preview loading; guard warning |
| B14 | Agent tray status | Datto + UrBackup | system tray | local reassurance | icon colour = state, hover detail, version and device ID copy, Check in now, Back up now | ok; deferred; unseen-by-console |
| B15 | Pause / maintenance dialog | Datto + Axcient | B07 actions | time-boxed pause | 1 / 2 / 4 / 8 / 24 h or until date, reason, shown as Parked state | active pause with expiry |
| B16 | Enrollment wizard and script generator | Cove + MSP360 | menu: Customers, Add devices | zero-click device enrollment | customer, policy template, custody mode, token expiry and use count, generated RMM script and MSI parameters, download links | token expired; token revoked |
| B17 | Technician restore | UrBackup + IDrive | B07: Restore | restore when the device is dead or offline | snapshot picker (latest preselected, by date), tree browse and filename search, per-file versions, Download zip / Restore on device / Restore to replacement, progress, audit note | no escrow (customer-held); large restore; expired link |

## Flows

F01-F12 are Comet's flows carried over (click counts are happy-path estimates from the documented steps; [est] where the docs do not give every click). F13-F18 are the flows the clone needs that Comet lacks, as requested. F19-F22 are four further flows the research showed are required by CryoBackup's architecture (server-side maintenance, expected-customer alarm, device replacement, technician restore); they are additions and can be struck by the reviewer.

```
F01 Enroll a device via RMM silent install + lobby registration (Comet's recommended high-volume path)
    one-time: S29 brand + codesign, S25 Default Policy, S33 storage template, S04 users
    A01 silent install with lobby switch -> S19 Devices (Unregistered) -> Register -> choose user -> confirm -> S06 (Online)
    policy auto-creates the vault and the default Protected Item + schedules; first scheduled run, or admin fires S07 -> S08
    happy path clicks: 4-5 admin clicks after install; RMM script carries no credentials
    edge: unsigned installer fails silently under AV/SmartScreen; lobby membership not persistent across reboot on some OSes; device-count quota rejects registration;
          cloned image gives "multiple devices detected as one"; a device that reboots before registration must re-join the lobby

F02 Enroll with pre-seeded credentials or an install token (no admin follow-up)
    S04 create user -> installer with credentials or single-use expiring token -> device registers itself -> S06 (Online), policy defaults apply
    happy path clicks: 0 in console after user creation (3-4 to create the user)
    edge: wrong credentials leave "Unregistered device for the supplied credentials"; expired or used token; credentials visible in the script when pre-seeded

F03 Enroll by hand from the console (one-off)
    S03 Add user -> S04 (username, password, policy, vault template) -> S30 download agent -> A01 interactive -> A02 sign in -> A03 add item or wait for policy default -> run
    happy path clicks: about 12
    edge: MSI absent until codesigning configured; SSL / Let's Encrypt misconfiguration blocks agent connect; the allow-admin-password-reset decision is permanent if turned off

F04 Create a Protected Item with schedule and retention
    path A (agent): A03 Add -> A04 name/type -> items (A05 pick folders, add exclude) -> snapshot on -> Schedules (A06: daily 20:00, vault, If-last-job-Missed, skip-if-running, retry 1x/30 min) -> Retention (A07: last 14; last 13 at most one per week; last 12 at most one per month) -> Save -> Backup Now
    path B (console, device Online): S10 -> S11 same tabs; or push via policy default item + "Update existing devices"
    happy path clicks: about 18-22 [est]
    edge: schedule vs retention mismatch (keeping Sundays when backups run Mondays); network shares need UNC + service credentials and no VSS; policy mandatory exclusions merge in;
          "prevent overriding retention" greys the tab; No Schedule badge on S23; keep-forever grows memory and index

F05 A scheduled backup run (and the missed / retry paths)
    service evaluates schedules in device time (+ random delay) -> job Running, progress streamed to console -> order: before-hooks, snapshot, chunk/dedup/encrypt/upload, after-hooks, retention per cadence -> Success / Warning / Error / Quota Exceeded -> email rule matches -> PSA alert raised or cleared
    missed: server generates Missed when a scheduled time passes without a job; "If the last job was Missed" fires a catch-up on reconnect; "When PC starts" also fires on wake
    failure: retry leaves "In Progress (retry)" then Warning on success; cancel-if-running-after gives Cancelled; overlap gives Skipped; crash gives Abandoned, later "Running (revived)"
    happy path clicks: 0 scheduled; manual Backup Now = 3 (A03 item, vault, Start)
    edge: vault locked by another device; quota hit mid-upload leaves chunks; low-power device defers retention; Object Lock blocks deletion inside the window

F06 Find out why a backup failed
    entry: immediate email (full log inline), PSA alert, S23 red block, or S21 Status != Success
    S23 click red block -> S21 filtered to the day -> row -> S22 (status, aggregated warnings, severity filter, totals; VSS 0x codes -> vssadmin list writers; "Locked by device" -> find the locking device in S06; "Reindex Needed" -> S07 Advanced Reindex then apply retention)
    stuck Running: Cancel; if ignored, S07 Update software to kill the process, then Reindex; or mark Abandoned via API
    happy path clicks: email link to job 1-2; from scratch 3-4
    edge: device Offline gives logs only; job logs purge after N days (default 90); retention errors surface on the vault, not the job list

F07 Restore files to original or alternate location
    A10: Restore -> vault -> item (expand to pick a snapshot date, or Search a filename) -> Choose files -> destination (original paths or custom) + overwrite rule -> Restore
    console: S07 Remotely restore (same steps; admin needs the view-file-names right; device Online)
    alternatives: compressed archive restore type; simulate only; restore to cloud/remote (26.8.3)
    happy path clicks: 6-8
    edge: dedup rehydration needs free space; EFS files restored encrypted; snapshot index download slow for multi-TB vaults; vault locked -> wait or unlock

F08 Replace a dead PC (restore to a new device)
    install agent on the replacement, log in with the same profile -> S06 confirm Online -> A10 "Show items from other devices" + Refresh -> pick the old device's item -> restore to original paths -> copy the item (S10 Copy / Paste) -> S06 old device Revoke
    happy path clicks: about 10 [est] plus the restore wizard
    edge: user password unknown and admin reset disabled blocks the path; old device reappearing triggers a catch-up charge; policy can hide other devices' names

F09 Decommission a device
    optional final backup (S08) -> S06 Revoke (items deleted, vault retention governs orphans) -> optional S07 Uninstall software (or silent uninstall switch) -> if the customer leaves: Suspend or Delete user, then delete bucket (S33) after retention / Object Lock expire
    happy path clicks: 3-5
    edge: uninstall without revoke keeps billing; delete then re-register triggers back-charges; Object Lock blocks bucket deletion until the window ends

F10 Bulk policy change across a tenant
    S24 policy -> S25 edit (e.g. add mandatory exclusion, change default schedule, enforce retention) -> Save -> users on the policy pick it up at next profile refresh (S07 Refresh profile or automatic)
    existing-object rules: default item reaches existing devices only with "Update existing devices"; default schedules apply to new items only; to move users between policies S03 -> user -> S17 -> Save (per user; API for bulk)
    happy path clicks: 4-6 for the edit; 3 per user reassignment
    edge: "(Custom)" users diverge from the policy; some restrictions must be re-enabled client-side; tenant-scoped policies reset on tenant transfer; no multi-select apply in the UI

F11 Set up alerting and reports for an MSP
    S27 delivery + test -> global rules (immediate on Status != Success; daily 07:00 summary; weekly grouped status) -> per customer S18, or S25 default reports -> S34 Syncro (domain + token; alerts raise and clear) -> S35 webhook
    happy path clicks: about 10-15 [est]
    edge: no first-class "device not seen for N days" alert (Offline badge and Missed events only; CheckCentral / Backup Radar fill the gap); MX-direct mail lands in spam

F12 Fleet agent upgrade
    S20 New Campaign -> upgrade older -> optional device filter -> Start -> watch per-device status; or the automatic campaign toggle in Settings
    happy path clicks: 3-4
    edge: tenant admins cannot run campaigns; NAS packages cannot upgrade remotely; Update software also kills a stuck job

F13 Laptop missed-run catch-up and run-on-wake (clone-only)
    A service: boot / resume (power event) / reconnect -> due check (no success within interval?) -> inside window? -> start conditions (battery, metered; run anyway after N) -> jitter -> one restic run covers every missed interval -> result journal -> heartbeat -> console state returns to ok (B07, B02)
    happy path clicks: 0
    edge: window closed at wake (wait for next opening; SLA clock keeps running); conditions delay but never cancel (run-anyway-after-N); gateway lock from server-side prune (exit 11 -> deferred, retry); Modern Standby delays the resume start by minutes;
          clock jump forward; device asleep for a week gives one catch-up run, not seven; fleet-wide wake at 08:00 spread by jitter; power event handler must return within 30 s (work on another goroutine)

F14 Device unseen for N days -> alarm -> acknowledge (clone-only)
    heartbeat silence crosses the SLA (computed at read time) -> B03 unseen ring / B05 / daily digest line -> B07 device -> Acknowledge with note and snooze (until date or next success) -> optional B15 pause or retire -> auto-resolves on next heartbeat + success
    happy path clicks: 2-3 from B05; 1-2 from a digest link
    edge: retired device must not alarm; customer-wide silence collapses to one customer alarm, not forty rows; laptop on holiday uses snooze until a date; snooze expiry re-raises; certificate or DNS failure looks identical to an offline device (this is the 18-day silent-outage class, so the alarm must not depend on the agent being able to reach the console)

F15 First-backup grace for a newly enrolled device (clone-only)
    B16 (token) -> A01 install -> device appears on B01 as "pending first backup (grace until T)" -> initial backup runs inside the initial window and is resumable -> turns green; grace deadline passes -> stale alarm (never "quietly pending forever")
    happy path clicks: 0 per device
    edge: installed but never contacts the console (alarm at grace expiry: "enrolled, never reported"); multi-day seed shows progress and counts as healthy while progress advances [policy decision]; re-enrolled or replaced device gets a fresh grace; cloned identity detected at enroll

F16 Key custody / escrow at deploy (clone-only)
    B16 choose custody per customer (escrow default / customer-held) -> enroll provisioning: unique repo password per device; one copy in the agent's admin-only file, one wrapped under the server master key on the console; escrow verified by test decrypt (B10 shows "verified") -> reveal later needs fresh MFA + reason + audit event
    happy path clicks: 0 per device (one choice per customer)
    edge: customer-held mode shows the consequence text (loss = data loss); console master-key backup and restore tested (console self-backup); reveal during an incident when MFA device is lost (break-glass procedure); repo key rotation needs the admin path and a new endpoint file; device enrolled but escrow failed -> alarm

F17 Storage quota approaching (clone-only)
    usage sampled per device (admin-path stats) -> B11 threshold 80 / 90 / 100 percent -> digest line + alert -> operator: raise quota, tighten retention (B13 preview), apply exclusion presets, run prune now, or contact the customer
    happy path clicks: 3-4 to tighten retention with preview
    edge: soft quota only (rclone gateway has no max-size); providers with minimum storage duration do not release space for 90 days after prune; a hostile endpoint can fill the bucket, so alarm on growth rate as well as level; per-device vs per-customer limits

F18 Gateway unreachable from the agent (clone-only)
    run starts -> connect / TLS / 401 failure classified (network vs cert vs auth vs lock) -> run state "deferred: waiting for gateway" (not failed) -> bounded exponential backoff -> results written to the local journal and delivered at least once when the console is reachable -> on return the run resumes via index checkpoints and completes
    local cache / queue (Veeam Backup Cache idea): v1 does not keep a second local copy; it keeps backing up from the last-known-good policy, journals results, and lets the console's heartbeat-loss logic raise the unseen / stale alarm. A local backup cache (local repo + restic copy) is a could.
    happy path clicks: 0
    edge: gateway certificate expiry fails the whole fleet at once (fleet-wide banner, not 200 rows); 401 after login rotation; captive portal / proxy; clock skew breaks TLS; outage longer than the SLA correctly goes stale; console unreachable as well as gateway (journal only)

F19 Server-side retention, prune and check cycle (clone-only)
    maintenance worker per device repo: pre-flight (no run in progress per heartbeat; queue defer command) -> forget with policy + keep-within (JSON; safety guard compares against dry-run) -> prune (exclusive lock; --max-unused; repack-cacheable-only by default; cadence per storage target) -> check (structure on schedule; read-data-subset occasionally) -> clear stale locks -> record maintenance run -> refresh usage sample
    happy path clicks: 0 (scheduled); manual "run maintenance now" 2
    edge: a backup arrives mid-prune (exit 11 -> deferred, not failed); a hostile endpoint can delete locks in its own repo including the admin's lock; Wasabi-style 90-day minimum makes frequent prune expensive; prune has no JSON so success is exit code + stderr; interrupted prune resumes; guard refuses mass removal (clock skew, snapshot flood); failure raises an alarm

F20 Expected customer missing (clone-only)
    customer flagged expected -> read-time rule: expected and zero active devices -> B01 shows a MISSING group, digest lists it, alert fires -> operator enrolls devices (B16), marks the customer not expected, or archives it
    happy path clicks: 2-3
    edge: onboarding grace per customer; all devices retired legitimately; customer renamed; devices moved to another customer

F21 Replace a device and adopt its repository (clone-only; CryoBackup version of F08)
    B07 Replace device -> new token bound to the old device identity -> install on new hardware -> console rotates the gateway password and delivers the key from escrow -> new agent adopts the repo under the same --host -> first run is incremental against history -> old record marked replaced
    happy path clicks: 3-4
    edge: backup paths changed (parent selection); old device reappears (identity conflict); customer-held key needs the key supplied; for data-only recovery without continuing history use F22

F22 Technician restore without the device (clone-only)
    B01 -> customer -> B07 device -> B17: snapshot (latest preselected) -> browse or search -> tick file(s) -> Download zip (admin host runs dump) or Restore on device / replacement
    happy path clicks: 5 or fewer
    edge: customer-held key without escrow blocks the server path; large restore needs temp space and purge-after; browse permission is a per-customer toggle and audited; device retired; link expiry; restore progress and cancel
```

## Components

Merged list (Comet's component inventory plus the components the borrowed screens and clone-only flows add). This feeds replica-design.

| component | variants | states | used on |
| --- | --- | --- | --- |
| Data table | live filter, pager, column picker, export (TSV / CSV / XLSX), grouped vs flat, row selection for bulk actions, saved views | loading, empty (never silent for "no data"), partial-data, error | S03, S19, S21, S16, S33, B01, B04, B05, B09 |
| Device state badge | ok, stale, failed, blank (pending first backup), offline, unseen, paused (Parked), retired | default, hover with reason, acknowledged, snoozed | S06, S19, S23, B01-B07 |
| Run outcome badge | success, partial, failed, cancelled, skipped, deferred, interrupted, abandoned | default, with retry count | S21, S22, A03, A09, B06, B07 |
| Attribute badges | Offline, Outdated agent, No Schedule, Escrow missing, Policy drift | default | S23, B01, B07 |
| Day-block activity timeline | green all-ok, red any-fail, striped mixed, blank none; count label; click filters | default, hover, selected | S23, B01 |
| Ten-day strip | success, mixed, missed, none circles | default, hover, paused | B02, B01, B07 |
| Age ring (doughnut) | state mix, last-success age, unseen, customers | default, segment hover, segment selected, empty-is-alarm | B03 |
| Calendar-matrix cell | success, warning, failed, missed, scheduled, running, deleted; Last / Worst | default, hover, selected | B06 |
| Health gauge | percent within SLA, 7-day history sparkline | default, degraded | B01 |
| Freshness stamp and unreachable banner | stamp in operator timezone; banner levels (console cannot reach gateway / storage / agents, partial data) | fresh, stale, unreachable | every page |
| MISSING customer group header | expected customer with zero devices | default, acknowledged | B01, B08 |
| Live run progress | percent, bytes, files, inactivity notice, Cancel | running, stalled, done | S21, S22, A03, A09, B07 |
| Log viewer | severity filter, text filter, aggregated repeated errors, export; path-free by default | loading, empty, long | S22, A09, B07 |
| Query / clause builder | field + operator + value with AND / OR / NOT groups; reused by search, reports, campaigns, saved views | valid, invalid, saved | S21, S18, S27, S20, B03 |
| Schedule editor | interval + window, fixed time, event triggers, retry, run-anyway-after-N, jitter | default, invalid, inherited vs overridden | S12, A06, B12 |
| Window / throttle mini-language field | text input with validation and visual 7x24 grid | valid, invalid | S12, A15, B12 |
| Retention rule list and presets | preset picker, rule rows, calendar preview, guard warning | default, overridden, preview loading | S13, S15, S25, A07, B13 |
| Selection editor | tree picker with live size estimate, presets, exclusion list with defaults diff, per-path flags | loading tree, large selection, policy-locked | S11, A04, A05 |
| Tabbed modal / side panel | device and customer panels, deep-linkable tab | open, loading, error | S11, S15, S09, A04, B07, B08 |
| Wizard (stepper) | restore, enrollment, installer | step states, back, error | A10, S09, A04, A01, B16, B17 |
| Device action list | enabled only for online devices; queued action for offline devices; Advanced section | enabled, disabled, queued | S07, B07 |
| Inherit / override control | inherited, overridden (sparse), locked by policy | each state with source label | S25, B07, B12 |
| Key reveal dialog | fresh MFA, reason field, audit notice, copy-once | locked, revealed, expired | B10 |
| Quota meter | level + growth rate, soft thresholds | under, approaching, exceeded | B11, B08 |
| Acknowledge / snooze control | note, snooze until date or next success | default, snoozed, expired | B04, B05, B07 |
| Actions dropdown and row menu | per-entity actions; Advanced reveal | closed, open | S05, S24, B07 |
| Copy / Paste sticky action | item clipboard across devices | idle, holding | S10 |
| Toggle + QR / token enrolment | TOTP, WebAuthn, API tokens | off, on, saved | S38, S39 |
| Branding preview surface | title, image, colours, favicon, sender; per-customer | default, branded | S28, S01, S30, reports |
| Email / report templates | immediate failure, daily digest (exceptions first), per-customer report (HTML + PDF), storage report | preview, sent, delivery failed | S18, S27, B09 |
| Toast and inline banner | save confirmation, restart-required, token saved, delivery failure | info, success, warning, error | all |
| Empty, loading and error states | per screen, designed | each | all |
| Tray icon menu and status icon | colour = state; Check in, Back up now, Lock, Disconnect | ok, deferred, unseen | A14, B14 |

## Inferred data model

Starting point: Comet's 19 entities (Tenant, AdminAccount, UserProfile, Device, ProtectedItem, BackupRule, RetentionPolicy, StorageVault, StorageTemplate, Bucket, Job, Snapshot, EmailReport, Alert/event, Policy, UserGroup, InstallToken, UpdateCampaign, Quota), reshaped for CryoBackup's decisions: **the device is the unit of isolation** (one restic repo per device under one gateway login, so Comet's user-profile-as-security-boundary moves down to Device); endpoints never hold storage credentials; prune runs server-side; **state is computed at read time** (no persisted "status" column and no async alert thread; only acknowledgements, notification history and facts are stored); customers group devices; policies are per customer with a sparse per-device override. Field lists are inferred shapes, not a schema; replica-architect turns them into SQL. "guess" means the shape is invented to serve a requirement, not observed.

```
Customer     id, name, slug, state (active | suspended | archived), expected (bool), onboarding_until,
             contacts[], policy_id, storage_target_id, display_tz (guess), device_cap (nullable), notes, created_at
             evidence: S03, S05, B08, F20; Cove customers, MSP360 companies, UrBackup groups (no tenant UI)
             confidence: high (existence), medium (fields)

Device       id (stable ID, also the restic --host), customer_id, source_kind (windows-endpoint now; other kinds later), hostname,
             friendly_name, os_version, arch, device_class (laptop | desktop; guess), agent_version, restic_version,
             update_ring, target_version, enrolled_at, enrolled_via (token | lobby), first_backup_due_by, last_seen_at,
             policy_version_applied, paused_until, pause_reason, retired_at, replaced_by_device_id, repo_path, hw_fingerprint (guess)
             No status column: state is computed (last_success_at, last_seen_at, SLA, grace, pause).
             evidence: S06, S19, B07, F01, F15, F21; Comet DeviceConfig; engine §1.7 (stable --host), §2.7
             confidence: high

EnrollmentToken  id, customer_id, token_hash, policy_id (optional), device_class_default, expires_at, max_uses (null = unlimited),
             used_count, server_fingerprint, created_by, revoked_at
             evidence: B16, F01, F02, F15; Comet install token, Cove multi-use installer, Datto REG_TOKEN, Axcient TOKENID
             confidence: high

Policy       id, scope (default | template | customer), customer_id, name, version, schedule {mode, interval, window, fixed_times, jitter,
             catchup, run_on_wake, startup_delay, retry, max_runtime}, conditions {battery_floor, metered, wifi_block, vpn, run_anyway_after},
             selection {roots, presets, excludes, includes, path_flags, exclude_cloud_files, vss}, retention {preset | keep rules, keep_within, keep_tag},
             throttle {limits, windows}, sla {stale_after, unseen_after, first_backup_grace, partial_tolerance}, maintenance {prune_cadence,
             max_unused, check_cadence, read_data_subset}, agent_permissions, notification_overrides
             evidence: S12, S13, S25, B12, B13; Comet UserPolicy (~45 fields), Axcient SLA, Datto policy, UrBackup settings
             confidence: high (existence), medium (shape)

DevicePolicyOverride  device_id, patch (sparse JSON over Policy fields), reason, set_by, set_at, expires_at (nullable)
             evidence: B07; MSP360 inherit chain, Axcient per-device alert override; avoids Comet "(Custom)" inline copies
             confidence: medium

Run          id (agent-generated, idempotent), device_id, kind (backup | restore | forget | prune | check | unlock | maintenance),
             trigger (schedule | catchup | wake | boot | manual | retry), started_at, finished_at,
             outcome (success | partial | failed | cancelled | skipped | deferred | interrupted | abandoned), restic_exit_code, snapshot_id,
             stats {files_new, files_changed, files_unmodified, data_added, bytes_processed, duration}, error_counts {class: n} (path-free),
             attempt, deferred_reason (lock | gateway | battery | metered | window | conditions), policy_version, agent_version
             evidence: S21, S22, A09, F05, F13, F18; Comet BackupJobDetail; restic summary JSON (engine §1.9)
             confidence: high

Snapshot     device_id, snapshot_id, taken_at, paths[], tags[], program_version, summary stats (cache of restic snapshots --json; repo is authoritative)
             evidence: A10, B17; restic snapshot JSON (engine §1.9)
             confidence: medium

Heartbeat    device_id, received_at, agent_version, uptime, power {on_ac, battery_pct}, network {metered, vpn} (guess), queue_depth,
             journal_depth, clock_offset, service_state (idle | running | paused | deferred), last_error_class
             latest row per device plus a short ring
             evidence: B05, B14, F13, F14; Comet live connection, Acronis 1-minute checks
             confidence: medium

GatewayLogin id, device_id, username (= device ID), password_hash (bcrypt), created_at, rotated_at, revoked_at, htpasswd_synced_at
             evidence: engine §3.1 (htpasswd, --private-repos); F21
             confidence: medium

RepoKeyEscrow  device_id, key_version, wrapped_key (under server master key), wrap_alg, custody (escrowed | customer_held), created_at, verified_at
             evidence: B10, F16; Cove Managed Encryption, Datto key choice; engine §2.5, §6
             confidence: medium

StorageTarget id, name, provider, endpoint, region, bucket, prefix_scheme, versioning (bool), object_lock_mode (none | governance, roadmap),
             min_storage_days, default_prune_cadence, credential_ref (secret store only), soft_quota_bytes
             evidence: B11, S33; Comet StorageTemplate / Bucket; engine §4
             confidence: medium

AlarmAck     id, subject (device | customer), alarm_type (stale | unseen | failed | first_backup_overdue | missing_customer | maintenance | escrow),
             acknowledged_by, acknowledged_at, note, snooze_until, snooze_until_next_success, cleared_at
             the only persisted alarm datum; alarm state itself is computed
             evidence: B04, B05, F14; Veeam active alarms, Axcient auto-resolve
             confidence: guess (shape), high (need)

NotificationRule  id, scope (global | customer | device), customer_id, channel (email | webhook | rmm), destination, triggers[], min_severity,
             digest_schedule, nag_interval, notify_recovery, enabled
             evidence: S18, S27, S34, S35; UrBackup nag, MSP360 overrides, Cove rules
             confidence: medium

NotificationLog  id, rule_id, dedupe_key, subject, sent_at, channel, result, error_class
             feeds nag, dedupe and the delivery-failure alarm
             evidence: MSP360 delivery-failure banner, UrBackup transition mails
             confidence: guess

Report       id, name, scope, saved_view_id, layout, format (html | pdf | csv), schedule, recipients[], active
             evidence: S18, S27; Cove scheduled reports, IDrive PDF reports, MSP360 reports
             confidence: medium

SavedView    id, owner, name, filter (clause tree), columns[], sort, shared
             evidence: B03; Cove saved views; Comet SearchClause
             confidence: medium

AdminUser    id, username, email, role (owner | admin | operator | read_only), customer_scope[] (null = all), totp_secret (encrypted),
             webauthn_credentials[], oidc_subject, recovery_codes_hash[], disabled, last_login_at
             evidence: S32, S38; Comet AdminAccount; Cove/Acronis/Veeam roles
             confidence: high

ApiToken     id, admin_id, name, scopes[], token_hash, created_at, last_used_at, expires_at, revoked_at
             evidence: S38; Comet personal tokens, Veeam API keys
             confidence: high

AuditEvent   id, at, actor, method (web | api | system), source_ip, action, object_type, object_id, customer_id, result, details (path-free)
             evidence: B09; Acronis audit fields; Cove per-device audit
             confidence: high

AgentRelease version, channel (pilot | stable), sha256, signature, restic_version, min_os, released_at
             evidence: S20; MSP360 sandbox/public builds, Acronis channel
             confidence: medium

UsageSample  device_id, sampled_at, repo_bytes, restore_size_bytes, snapshot_count, blob_count (sizes prune memory), source (restic stats | provider)
             evidence: B11, F17; Cove Selected vs Used storage; engine §1.9 stats, §2.3
             confidence: medium

Command      id, device_id, type (backup_now | cancel | refresh_policy | restart_service | update_agent | pause | restore | check_in),
             params, idempotency_key, status (queued | delivered | acked | succeeded | failed | expired), created_by, created_at, expires_at, result
             pull-based, so endpoints need no inbound port
             evidence: S07, B07; Comet Dispatcher (websocket live channel)
             confidence: medium

RestoreRequest  id, device_id, snapshot_id, mode (device_original | device_path | download_zip | download_tar), selection_ref (counts and hash;
             paths held only ephemerally), requested_by, status, created_at, expires_at, artifact_ref, bytes, purge_after
             evidence: B17, F22; UrBackup ZIP download, restic dump (engine §1.13)
             confidence: guess

Setting      key, value (scope global): SMTP, RMM adapter credentials (secret refs), display timezone, run-history retention (default 90 days),
             digest times, banner thresholds, master-key reference
             evidence: S26, S27, S34; Comet settings
             confidence: medium
```

24 entities. Relationships: Customer 1-n Device; Customer n-1 Policy (per customer, plus default and templates); Customer n-1 StorageTarget; Device 0-1 DevicePolicyOverride; Device 1-n GatewayLogin (one active; history on rotation); Device 1-n RepoKeyEscrow (by key_version); Device 1-n Run; Run 0-1 Snapshot; Device 1-n Snapshot (cache); Device 1-n Heartbeat (ring); Device 1-n Command; Device 1-n UsageSample; Device 1-n RestoreRequest; EnrollmentToken n-1 Customer, 0-1 Device (adoption); AlarmAck n-1 Device | Customer; NotificationRule 1-n NotificationLog; Report n-1 SavedView; AdminUser 1-n ApiToken; every mutating action 1-1 AuditEvent.

How Comet's 19 entities map:

| Comet entity | CryoBackup disposition |
| --- | --- |
| Tenant | not in v1 (single operator); hierarchy later (tenancy area); Setting + Customer cover it |
| AdminAccount | AdminUser (adds read-only role, customer scope) |
| UserProfile | split: Customer (grouping, policy, storage target) and Device (isolation, keys); no shared-vault profile |
| Device | Device (stable ID, repo path, no status column) |
| ProtectedItem | folded into Policy.selection; per-device deviation via DevicePolicyOverride |
| BackupRule / Schedule | folded into Policy.schedule and conditions |
| RetentionPolicy | Policy.retention, executed only by the server-side maintenance worker |
| StorageVault | per-device repo: Device.repo_path + GatewayLogin + RepoKeyEscrow |
| StorageTemplate | StorageTarget plus provisioning at enroll |
| Bucket | StorageTarget bucket / prefix |
| Job | Run |
| Snapshot | Snapshot (cache; repo authoritative) |
| EmailReport | NotificationRule + Report |
| Alert / event | computed state + AlarmAck + NotificationLog + AuditEvent |
| Policy | Policy (per customer, versioned) |
| UserGroup | Customer (flat); tags later |
| InstallToken | EnrollmentToken (customer-scoped, multi-use option) |
| UpdateCampaign | AgentRelease + Device.update_ring / target_version |
| Quota | StorageTarget.soft_quota + Policy + UsageSample |

M365 / Google Workspace later: `Device.source_kind` and a `source_id` on Run, Snapshot and Policy keep the model from assuming every protected thing is a Windows machine; the module itself is out of scope.

## Engine constraints (gate)

These limits bound the feature matrix. Sections cite the engine capability sheet (engine_capabilities.md; versions: restic 0.19.1, 2026-07-05). A matrix row may be `must` or `should` only if the engine can deliver it; otherwise it is `could` (workaround exists) or `skip`.

| constraint | limit | matrix consequence | engine § |
| --- | --- | --- | --- |
| Repo per device | A shared repo makes any prune/check/forget block every client (hours on multi-TB); any key holder decrypts everything; client RAM scales with blob count of the whole repo. Per-device repo bounds all of these. | One repo + one `--private-repos` gateway login per device. Price: no cross-device dedup (skip). Gateway user URL must equal the repo user, so a replacement device must adopt the identity (F21), not read a sibling repo. | 2.2, 2.3, 2.5, 2.7, 3.1 |
| Append-only semantics | `--append-only`: DELETE is 403 except under `locks/`; overwrite of an existing object is 403; client forget, prune, rewrite, key remove and key passwd fail. A hostile endpoint can still fill the bucket (no quota in rclone), create fake snapshots that push real ones out of `--keep-*` windows (so always add `--keep-within`), and delete any lock in its own repo including an admin's exclusive lock mid-prune. Env-var form of the flag has failed to enforce before: use the CLI flag and lab-test. | Server-side-only retention; prune safety guard; quota is soft; maintenance when device is quiesced. | 3.2 |
| Object Lock | Compliance mode is unsafe with restic (lock files and prune cannot delete, no bypass). Governance mode is plausible but untested. S3 default retention cannot be scoped to a prefix. | v1: append-only + provider versioning/lifecycle + `--keep-within` + gateway host hardening. Compliance = skip; governance = could (lab-test first). | 4.5 |
| Gateway features | rclone serve restic has no quota, no metrics, no upload verification; TLS floor defaults to 1.0. rest-server has all three but needs local disk. | Soft quotas computed by console; hard quota = could; set `--min-tls-version tls1.2`. | 3.1, 3.5 |
| VSS | Per-volume fallback, not abort: on snapshot failure restic reads the live filesystem; locked files then fail and the run exits 3. Mount points are all snapshotted unless excluded; `vss.timeout` default 120 s. | VSS default on; fallback reported as a warning naming the volume; options exposed. | 1.1 |
| Cloud files | `--exclude-cloud-files` (0.18) skips online-only placeholders; required for workstation profiles. | Default ON. | 1.1 |
| NTFS fidelity | ACLs, owner, attributes, creation time, EAs preserved since 0.17 (full SD needs privileges that LocalSystem holds). **Alternate Data Streams are not supported** (PR closed unmerged; silently omitted). **Junctions: target path only, never contents**; 0.19.1 junction reporting is UNCERTAIN. Invalid-character file names cannot be read. Minimum OS Windows 10 / Server 2016. `--one-file-system` fails on Windows. | ADS and junction contents = skip; documented limits. | 1.2, 1.3, 1.5 |
| No restic mount on Windows | `restic mount` is FUSE only. | Mount-as-drive = skip; browse via `ls`/`find`, restore via `restore` or `dump -a zip`. | 1.13 |
| Change detection | Windows compares path + size + mtime only (`--force` is the only effective flag); no USN journal or driver. | No CDP; no block-tracking driver; sub-hourly interval is a could. | 1.4, 6 |
| JSON and exit codes | JSON on stdout, errors and `exit_error` on stderr. Exit 0 ok, 3 partial (unreadable files, missing source), 10 repo missing, 11 lock conflict, 12 wrong password, 130 cancelled; any unknown code must be treated as failure. **prune has no JSON**, and `forget --prune` mixes JSON and text: run them separately. | Run outcome vocabulary and exit-code mapping; maintenance parses text for prune. | 1.9, 1.10 |
| Locks and resume | backup takes a non-exclusive lock; forget, prune, check take an exclusive lock; stale after 30 min; `--retry-lock` waits. No partial snapshot is ever written; uploaded packs are reused via periodic index files; prune resumes. | Lock contention is deferral; interrupted runs resume without special state. | 1.11, 2.1 |
| Throttle | `--limit-upload` / `--limit-download` are fixed for the run. | Scheduled throttle windows = agent restarts restic at boundaries (should); idle/in-use split = could. | 1.6, 6 |
| Retention | forget keep rules OR together; periods without a snapshot do not consume a slot; last snapshot of a group is never deleted without an explicit override; future-dated snapshots are ignored. Prune cost is repack bandwidth; Wasabi-style 90-day minimum bills early deletes. | Presets over free-form rules; prune cadence per provider; preview via `forget --dry-run --json`. | 1.14, 1.15, 4.2 |
| Cold tiers, copy | Storage-class flags apply to every object including locks and index; Glacier-class breaks restore/prune. `restic copy` needs matching chunker params. | Cold GFS archive = could via copy to a second repo; storage-class = skip. | 6 |
| Service and power events | `golang.org/x/sys/windows/svc` delivers suspend / resume / session-change events when `AcceptPowerEvent` / `AcceptSessionChange` are set; **kardianos/service drops them**. Handler must return in 30 s; shutdown leaves about 20 s (PreShutdown buys more). PBT/WTS constants may need local definition [UNCERTAIN]. | Run-on-wake is a must built on x/sys directly. | 5.1, 5.2 |
| Sleep | `SetThreadExecutionState` is not exported by x/sys [UNCERTAIN]; Modern Standby may ignore it from a service; lid-closed backup is not promised. | Prevent-sleep = should (best effort); wake timers = could. | 5.1 |
| Identity and paths | LocalSystem holds the needed privileges and can create VSS snapshots; its LOCALAPPDATA is the system profile. Child restic can be tied to the service with a job object (kill-on-close, memory cap). | LocalSystem service; explicit RESTIC_CACHE_DIR and password-file paths under ProgramData; job object. | 1.16, 5.3, 5.4 |
| Packaging and signing | WiX MSI for services; unsigned binaries are blocked by Smart App Control and EV no longer bypasses SmartScreen; restic.exe upstream is not Authenticode-signed [UNCERTAIN]. Bundle restic.exe as a second MSI component rather than embedding. Self-update by renaming the running exe works; v1 can rely on MSI upgrades via RMM. | Sign agent, installer and restic.exe; self-update = should. | 5.6-5.9 |
| Key loss is data loss | Lose the repo password, lose the repo; there is no recovery. | Key escrow before v1 ships (must); customer-held mode is an explicit opt-out with a warning. | 2.5, 6 |

## Design lessons

Drawn from the cross-vendor sections of the three sweep notes plus Comet's own recon. Each item reads "product: pattern - why".

### Adopt

- **Comet**: Recent Activity 10-day grid with Offline / Outdated / No Schedule badges as the default silence view - one screen shows days with no run, not only failed runs.
- **Comet**: lobby registration plus install-token enrollment, and policy defaults that give a new device a schedule on arrival - enrollment with no credentials in the RMM script.
- **Comet**: OR-combined keep rules with GFS built from three rules, and "enforce retention / prevent override" at policy level - maps directly onto restic forget and gives ransomware framing.
- **Comet**: Syncro-style alert raise on failure and auto-clear on next success; every UI action is an API call - clean integration and scripting from day one.
- **Cove**: exception-first dashboard with clickable summary rings (state, last-backup age), saved views and an "Unsuccessful backups" default - the age ring turns silence into a visible bucket for a sole operator.
- **Cove**: retention as a declarative policy object with a calendar preview and "last successful session always kept" - easy to reason about and to unit-test.
- **Cove**: Managed Encryption by default with a privileged reveal gate (Security Officer, 24 h one-time passphrase) - avoids the lost-key catastrophe while keeping an audit trail.
- **Cove**: per-device panel with Summary / History / Errors / Audit tabs, and a multi-use site-scoped silent installer bound to customer and a default 24-hour-RPO profile - every why-question is answerable without a shell, and a new device is compliant on day one.
- **MSP360**: Overdue as a first-class state with two independent triggers (did not run on schedule; no success in N hours/days) and the vocabulary Missed Run / Unexpectedly Closed / Interrupted - names the silent failure modes precisely.
- **MSP360**: calendar-matrix backup history (device by day, Last vs Worst, Issue Only) - fastest check that a laptop backed up every day this month.
- **MSP360**: inherit chain (global, company, computer) for notifications and agent toggles; sandbox vs public agent builds with Force Update to selected machines - tiny UI, large operational payoff, and a staged ring prevents fleet-wide regressions.
- **IDrive**: explicit laptop semantics (30-minute grace, run missed backup on power-on, battery floor, auto-pause throttle when the PC is in use, wake from sleep) - exactly the workstation edge cases the others leave undocumented.
- **IDrive**: published opinionated default exclusion list with a visible include switch, percentage failure thresholds (notify as failure above N percent failed files), and restore-as-of-date snapshots - fewer false alarms and an explainable restore model.
- **Acronis**: delaying start conditions (battery, metered, idle, Wi-Fi, target host) with a "run anyway after N" fallback - conditions delay, never cancel, so the SLA still gets met.
- **Acronis**: documented cleanup-before vs cleanup-after semantics with a worked example and "newest backup is never deleted"; audit log field set (event, severity, object, initiator, method, IP) with fixed retention - small, boring, complete.
- **Datto**: last-ten-days circle strip per device with four states - the cheapest honest history widget in the sweep.
- **Datto**: fixed retention tiers with published thinning, and a deploy-time key custody choice (vendor-managed recoverable vs customer key) with the consequence stated - removes a misconfiguration class and forces the key decision.
- **Datto**: console-owned time-boxed pause (1/2/4/8/24 h or indefinite) with "retention still runs on paused devices", and an idle vs in-use bandwidth split - the operator, not the user, owns pause.
- **Axcient**: four-state device health (Parked / Normal / Warned / Troubled) with per-customer SLA days, per-device override and explicit first-backup grace ("first window not yet open" is Normal) - the blank-client problem solved in the state machine.
- **Axcient**: a successful backup auto-closes outstanding missed / failed alerts; separate workstation vs server retention defaults; honest local-cache framing (accelerates restore, verified daily, never presented as a second backup) - correct resolve rule and honest promises.
- **Veeam**: explicit missed-run policy ("backup once powered on" vs "skip") with a short tolerance window and documented priority among missed run types - the best-specified catch-up semantics found.
- **Veeam**: Backup Cache for unreachable or metered targets (queue locally, upload on return), retention counted in days on which a backup actually ran, and a published default exclusion list (temp, Recycle Bin, pagefile, hiberfil, VSS files) - laptop-correct and copyable as the baseline.
- **Veeam**: alarm trio (computer without backup within RPO, max job duration, agent out of date) with email/script responses and event triggers such as "no more often than N" - the right small set of operational alarms.
- **UrBackup**: interval-due scheduling with inherent catch-up (a returning laptop is simply due; no stored next-run) - no "missed backup" decision needed, and recomputed every pass.
- **UrBackup**: one-line mini-languages for backup windows and throttles (`Mon-Fri/8-18;Sat,Sun/0-24`, `60;10@Mon-Fri/8-18`, `50%`), per-directory flags (optional, required, require snapshot), min/max retention with an emergency floor, and trust-on-first-use server identity - expressive and readable by one operator.

### Avoid

- **Comet**: no native "device not seen for N days" alert (Offline badge and Missed events only; third parties CheckCentral / Backup Radar fill the gap) - the canonical silent-outage hole; CryoBackup owns it.
- **Comet**: mandatory vendor telemetry and licensing phone-home - project rule: nothing phones home; also irreversible opt-out flags, defaults that apply to new objects only, "(Custom)" policy copies and no read-only admin role - each is a documented operator surprise.
- **Comet**: user profile as the shared-vault security boundary (any device in a profile reads any other's data) - CryoBackup isolates per device.
- **Cove**: Notification Service fires only on Failed / Completed with errors, with no native "no backup in N hours" alert - silence is covered only by looking at the age ring or a scheduled report.
- **Cove**: report caps (10 columns x 200 rows, external recipients needing opt-in within 48 hours), vendor-cloud-only storage and unpublished overage - hostile to a one-person shop and to bring-your-own storage.
- **MSP360**: two coexisting backup formats with different retention semantics and customer-held plan passwords with no escrow - half the docs are caveats; lost password means lost data.
- **MSP360**: Computer to User to Company indirection (every machine tied to a backup user account) and paid-licence gating of branding and reports - one device under one customer is enough, and the sender address should not be an upsell.
- **IDrive**: private-key reset deletes the account's data - the cliff between "we hold the key" and "you hold it and we wipe on reset" is too sharp.
- **IDrive**: alerts configured per computer and pushed rather than a central rule engine, plus three overlapping product lines with conflicting documented limits (30 vs 50 versions, varying quotas) - no console-wide silent-device rule; one console and one documented limit per feature.
- **Acronis**: "run missed tasks at startup" defaulting to OFF, killing a running backup at a prohibited hour, and a 30-option matrix - laptops silently miss backups, initial fulls never finish, and one operator cannot hold the matrix in their head.
- **Datto**: a reboot-after-install dependency that silently degrades to one backup per day, storage region locked per token with no migration, and two product generations with different status vocabularies - if anything needs a reboot, show "pending reboot, incrementals disabled".
- **Axcient**: an alert catalogue that is mostly appliance-scoped (D2C covered only by SLA health), two agent lines with different CLI syntax (`TOKENID=` vs `/TOKENID=`), and fragmented help centres - say which alerts apply to laptops and keep one canonical doc path.
- **Veeam**: failing a scheduled job outright when the connection is metered and no cache exists, a once-a-day scheduling ceiling on the workstation tier, and licence cliffs that make configured jobs fail - defer, do not fail; laptops need intervals.
- **UrBackup**: status "ok" computed asynchronously by an alert thread (30-60 minute lag, -1 bugs, everything red when the script is missing) - compute state synchronously from timestamps and intervals at read time; keep the alert engine separate.
- **UrBackup**: no battery or metered policy, an in-memory pause invisible to the server, and a rights model with no read-only modifier, no tenants, no 2FA and no audit log - the gaps CryoBackup exists to close.

## Feature matrix

See `features.csv` (header `feature,area,priority,original,clone,notes`; one canonical row per feature across the nine products; `clone` is `no` for every non-skip row and `skip` for skip rows; order is area then priority; `original` lists the vendors that have the feature, or `none (CryoBackup)` for clone-only differentiators). **Must: 121, should: 71, could: 45, skip: 21. Total 258 rows.**

Priority meaning: must = needed for v1 to replace the incumbent on a 200-workstation MSP fleet and to be credible against Comet; should = v1.x; could = later; skip = cannot (engine gap) or should not be cloned. Gate: no must/should row depends on something the engine cannot deliver (section 7); engine gaps appear as could (workaround exists) or skip.

Per-area counts:

| area | rows | must | should | could | skip |
| --- | --- | --- | --- | --- | --- |
| enrollment | 10 | 6 | 3 | 1 | 0 |
| policy | 10 | 5 | 5 | 0 | 0 |
| scheduling | 25 | 11 | 8 | 5 | 1 |
| selection | 14 | 6 | 4 | 2 | 2 |
| retention | 13 | 7 | 2 | 4 | 0 |
| storage | 20 | 8 | 5 | 3 | 4 |
| security | 22 | 11 | 5 | 5 | 1 |
| restore | 18 | 7 | 4 | 4 | 3 |
| monitoring | 23 | 18 | 5 | 0 | 0 |
| alerting | 17 | 9 | 6 | 1 | 1 |
| reporting | 8 | 0 | 5 | 3 | 0 |
| console | 23 | 11 | 5 | 4 | 3 |
| agent | 22 | 12 | 6 | 2 | 2 |
| deployment | 12 | 6 | 3 | 2 | 1 |
| tenancy | 9 | 1 | 4 | 3 | 1 |
| api | 6 | 2 | 1 | 2 | 1 |
| m365-gws | 6 | 1 | 0 | 4 | 1 |
| **total** | **258** | **121** | **71** | **45** | **21** |

## Out of scope (cannot or should not be cloned)

- **Image, volume and bare-metal backup and restore, boot media, restore to VM / virtual-disk export / cloud failover / standby images, boot-screenshot recovery proof** (all nine products have some form): restic is file-level only (engine §6). A separate engine decision if ever pursued; an existing image-capable tool stays in place for that need. Never bolted onto restic.
- **Continuous / real-time data protection (CDP)** (IDrive, Acronis, UrBackup vestigial): restic is snapshot-per-run with path + size + mtime change detection and no USN integration (engine §6). Sub-hourly intervals with `--skip-if-unchanged` are the honest substitute ("near-continuous", not CDP).
- **Application-aware SQL / Exchange and other non-file engines** (Hyper-V, VMware, Proxmox, MongoDB, MySQL, system state, NAS agents): no writer-specific coordination in restic (engine §6). Workstations are fine; servers dump first, then back up the dumps.
- **Cross-device deduplication** (Comet shared vaults, UrBackup, Cove): the consequence of one repo per device (engine §2.7). Accepted; mitigated with excludes and zstd.
- **Vendor-hosted storage pools and partner networks** (Comet Storage / Wasabi partner templates, MSP360 Storage, Cove cloud, Datto Cloud, Axcient vaults, IDrive cloud): commercial arrangements. CryoBackup is bring-your-own S3-compatible storage.
- **Licensing, billing, prepaid credit, subscription price sheets, reseller / distributor portals, fair-use pooling, hosted consoles, hardware appliances / BDR boxes, mandatory vendor telemetry** (Comet, Veeam, Acronis, MSP360, Cove, Datto, Axcient, IDrive): commercial models and hardware; project rule forbids phone-home.
- **Immutability via Object Lock**: compliance mode is unsafe with restic and governance mode is untested (engine §4.5). v1 immutability story is append-only gateway + provider versioning + `--keep-within`; governance mode is roadmap (could).
- **`restic mount` as a Windows drive** (UrBackup image mount, Axcient Recovery Center): FUSE only (engine §1.13). Use `ls` / `find` / `restore` / `dump`.
- **Alternate Data Streams and junction contents** (engine §1.3): documented limits, not workarounds. Symlinks and junction targets are recorded, never contents.
- **Cold-tier storage classes** (Acronis, MSP360 storage classes): storage-class flags hit every object including locks and Glacier-class breaks prune (engine §6). Cold archive later via `restic copy` (could).
- **Bundled endpoint security / RMM suites** (Acronis, MSP360): separate products.
- **Third-party partner integrations and owned assets** (Backup Radar, CheckCentral, WHMCS / HostBill modules, Comet branding, release names, icons, EULA, docs copy, SDKs, MCP server code): partner- or vendor-owned. CryoBackup writes its own alert adapters against public RMM APIs and fresh assets (replica-brand).
- **Microsoft 365 and Google Workspace backup** (Cove, MSP360, IDrive, Acronis, others): a later, separable module and a different engine; per-seat licensing is skipped. The data model must not preclude it (`Device.source_kind`, source references on Run / Snapshot / Policy; matrix row "Data model does not preclude non-device sources" is a must).
- **Linux and macOS agents, end-user self-service restore, white-label, localization, reseller hierarchy**: not skipped but deferred (could).

## Size

Screens: 71 (54 Comet screens: 39 console S01-S39 and 15 agent A01-A15, plus 17 borrowed B01-B17). Flows: 22 (F01-F12 from Comet, F13-F18 clone-only as requested, F19-F22 additions). Entities: 24 (Comet's 19 reshaped; section 6). Matrix: 258 rows (121 must).

**Hard parts**

1. **Windows service plus power events**: a Go service on `x/sys/windows/svc` (not kardianos) handling suspend / resume / session change, PreShutdown clean cancel of restic, a job object, LocalSystem identity, cache and secret paths, event log, crash recovery, MSI plus signing. Run-on-wake correctness on Modern Standby laptops cannot be fully guaranteed.
2. **VSS edge cases**: per-volume fallback to a live read that exits 3, mount-point stalls, OneDrive placeholders, locked Outlook files, reserved/invalid file names, junction warnings (UNCERTAIN on 0.19.1), minimum OS. Classifying partial success without crying wolf (failed-file tolerance) is product work, not engine work.
3. **Append-only plus server-side prune choreography**: forget (JSON) and prune (text) as separate invocations per device repo from an admin path that holds delete rights; exclusive-lock contention with backups (exit 11 as deferral); stale and hostile lock handling; provider minimum-duration economics; safety guard against snapshot floods and clock skew; resumable and observable.
4. **Key escrow**: unique password per device, wrapped copy on the console, verified by test decrypt, MFA-gated audited reveal, master-key custody and tested console restore, customer-held opt-out with an honest warning. Lose the console and the keys, lose every repo.
5. **Silence detection that never goes quietly green**: read-time state machine over heartbeat, last success, SLA, first-backup grace, pause, retirement and expected customers; unreachable gateway/console banners; partial-data handling; delivery failure as an alarm; digests that list silence. The classification logic is the product and needs exhaustive unit tests including the unreachable and partial-data paths.
6. **A polished console**: 71 screens' worth of states (empty, loading, error, partial) with first-class visual polish and no CDN assets, accessible and consistent. The recon found about 25 screens are needed to satisfy the must set.
7. Policy layering and propagation (live reference plus sparse override with an honest blast-radius preview) and retention semantics across presets and keep rules, with an exhaustive test matrix.
8. Fleet-scale unknowns: gateway throughput with 200 clients, prune memory at 1-10 TB, and cache growth are unmeasured (engine §2.3, §3.3).

**Size by tier** (template scale: S weekend, M few weeks, L a quarter, XL rescope)

| tier | size | why |
| --- | --- | --- |
| Agent | L | Small surface, high hazard: service, power events, scheduler with catch-up and conditions, VSS handling, restic supervision and JSON parsing, journal, MSI, signing, resumable behaviour. Hardening on real machines (laptops, Modern Standby) is most of the time. |
| Gateway (rclone serve restic, provisioning, maintenance worker, escrow service) | M | The gateway itself is configuration; the work is per-device provisioning, htpasswd hot reload, the forget / prune / check worker, usage sampling, key escrow, and lab-testing append-only. |
| Console | L | 24 entities, read-time state engine, alerting and digests, RBAC, audit, enrollment wizard, technician restore, polished UI. |
| **Overall** | **XL as the matrix is written; L only with the cut list below** | ADR 0001 and the project conventions set "feature-complete against the nine products" as the v1 bar; read literally (121 must plus 71 should) that is a rescope. |

**What v1 must drop to land at L**: build the 121 must rows only, shipping about 25 of the 71 screens (B01 board, B03 rings, B04/B05 combined overdue-and-unseen page, S23 ten-day grid, B07 device panel, B08 customer page, B09 audit, B10 key custody, B11 usage, B13 retention presets, B16 enrollment wizard, B17 technician restore, S21/S22 runs, S24/S25 simplified policy, S27 mail, S32/S38 accounts, S01 login, system health, A01 installer, A14/B14 tray). Defer: calendar matrix, saved views, PDF reports and report variants, SSO / WebAuthn / LDAP / granular permissions / customer-scoped roles, scheduled throttle windows, the start-condition framework beyond battery and metered, prevent-sleep and wake timers, policy templates / versioning / drift, release channels / self-update / campaigns (MSI upgrades via RMM instead), RMM adapters beyond webhook and email, soft-quota alarms (usage display only), automated restore tests, local backup cache, hard quotas, governance-mode lock, cold-tier copies, white-label, portals, and every could. The monitoring must rows (18) and the key-escrow rows are not droppable: they are the reason the product exists.

## Next step

`/replica-architect`: plan the stack, the SQL schema from the data model above, the API routes by flow, and the thin vertical slice (one device from enroll to green to restore) first.
