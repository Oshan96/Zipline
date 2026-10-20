"use strict";
/* Zipline — words and numbers for the window and the phone page.

   The message catalogue (MESSAGES) holds every user-facing string of the shared vocabulary under a message ID,
   in English; a translation is another object under its language tag (MESSAGES.fr = {…}), and any ID it leaves
   out falls back to English. Messages are ICU-style, a small part of it:

     "Remove {name}?"                                   {name}: a value from the caller's vars
     "{n, plural, one {# file} other {# files}}"        plural: =0 =1 zero one two few many other, # is n
     "{kind, select, a {…} other {…}}"                  select: picks by the value's text, other is the fallback
     "<b>{n}</b> left", <i>, <code>, <br>               the only markup; th() and ht() keep it, t() drops it
     "Profile: <long>…</long>"                          <long>: words the narrow status bar leaves out
     "<ok>Yes</ok> <m>· a note</m>"                     <ok> <warn> <err> <m>: green, amber, red and muted text

   A number in vars is shown in the browser's locale. Write a literal brace or backslash as \{ \} \\ (an
   apostrophe is just an apostrophe).

     t(id, vars)    the message as plain text (for textContent, titles, aria labels, attributes)
     th(id, vars)   the message as a Msg { text, html }: text for the ears and logs, html for the screen. Every
                    value is HTML-escaped, and a name wrapped in nm() gets <bdi>, so a right-to-left file name
                    can't scramble the line around it. toast(), dialog() titles, confirmDialog() and the
                    notifications list take a Msg as well as a string
     ht(id, vars)   th(id, vars).html, for building markup;  te(id, vars)  t() escaped, for attributes and plain text in markup
     nm(text)       a name from a user or a server (file, queue, page, device): isolated in th() and ht()
     raw(html)      markup the caller made (escape what's in it with esc()), passed as a value
     fmtSecs(n)     "25 sec";  listOf(items)  "A, B and C" as a Msg (an item may be a nm() name)
     asHtml(v)      a string (escaped) or a Msg as markup;  put(node, v)  the same, into a node

   Sizes, speeds, durations and dates go through Intl in the browser's locale (LOCALE). Intl names kilobytes
   "kB" and bytes "byte" in English: the catalogue's "unit.*" entries replace the unit's own name where it has
   one ("KB", "B"), so the English text stays what it always was. */

const LOCALE = undefined; // (the browser's own)
const MESSAGES = {
  en: {
    // ---- units, times
    "size.unknown": "Unknown",
    "unit.byte": "B", "unit.kilobyte": "KB", "unit.megabyte": "MB", "unit.gigabyte": "GB", "unit.terabyte": "TB",
    "unit.megabit-per-second": "Mbps", "unit.byte-per-second": "B/s", "unit.kilobyte-per-second": "KB/s", "unit.megabyte-per-second": "MB/s", "unit.gigabyte-per-second": "GB/s", "unit.terabyte-per-second": "TB/s",
    "time.underMinute": "under a minute", "time.aMoment": "a moment",
    "when.today": "today {time}", "when.tomorrow": "tomorrow {time}", "when.day": "{day} {time}",
    "ago.never": "never", "ago.justNow": "just now", "ago.min": "{n} min ago", "ago.hr": "{n} h ago",
    // ---- actions: the one set of words, used in the toolbar, the menus, the context menu and the phone
    "a.open": "Open", "a.openFolder": "Open folder", "a.properties": "Properties…",
    "a.removeFromList": "Remove from list", "a.deleteFile": "Delete file",
    "a.resume": "Resume", "a.pause": "Pause", "a.pauseAll": "Pause all", "a.resumeAll": "Resume all unfinished", "a.redownload": "Redownload",
    "a.shareAgain": "Share again",
    "a.addLink": "Add link", "a.copyLink": "Copy link", "a.copyMagnet": "Copy magnet link",
    "a.remove": "Remove", "a.removeCompleted": "Remove completed",
    "a.options": "Options", "a.scheduler": "Scheduler", "a.startQueue": "Start queue", "a.stopQueue": "Stop queue",
    "a.speedLimiter": "Speed limiter", "a.batch": "Batch", "a.siteGrabber": "Site grabber",
    "a.startNamed": "Start {name}", "a.stopNamed": "Stop {name}", "a.addTo": "Add to {name}",
    // ---- buttons
    "btn.ok": "OK", "btn.cancel": "Cancel", "btn.close": "Close", "btn.save": "Save",
    "btn.apply": "Apply", "btn.reset": "Reset", "btn.delete": "Delete", "btn.remove": "Remove",
    // ---- the menu bar
    "menu.bar": "Main menu", "menu.file": "File", "menu.downloads": "Downloads", "menu.view": "View", "menu.tasks": "Tasks", "menu.help": "Help",
    "menu.limiterOn": "Turn speed limiter on", "menu.limiterOff": "Turn speed limiter off",
    "menu.toggleCategories": "Toggle categories panel", "menu.columns": "Columns…", "menu.notifications": "Notifications…",
    "menu.theme": "Theme: {name}", "theme.system": "system", "theme.light": "light", "theme.dark": "dark",
    "menu.addLink": "Add link…", "menu.siteGrabber": "Site grabber…", "menu.addBatch": "Add batch download…", "menu.pasteLink": "Paste link from clipboard", "menu.openTorrent": "Open torrent file…",
    "menu.phone": "Control from your phone…", "menu.import": "Import download list…", "menu.export": "Export download list…",
    "menu.whenDone": "When all downloads finish", "menu.exit": "Exit Zipline",
    "menu.integration": "Browser integration…", "menu.shortcuts": "Keyboard shortcuts", "menu.cli": "Command-line options",
    "menu.diagnostics": "Diagnostics…", "menu.about": "About Zipline",
    "menu.startQueue": "Start queue…", "menu.stopQueue": "Stop queue…", "menu.queues": "Queues & scheduler…", "menu.options": "Options…",
    // ---- the toolbar
    "tb.more": "More", "tb.moreCommands": "More commands", "tb.show": "Show", "tb.search": "Search downloads",
    "tb.startQueueAny": "Start a queue", "tb.stopQueueAny": "Stop a queue",
    "tb.limiterOn": "Speed limiter ON ({rate}) — click to turn off", "tb.limiterOff": "Speed limiter OFF — click to turn on",
    "tb.limiterNote": "An overall limit on top of the traffic profile", "tb.limiterState.on": "on", "tb.limiterState.off": "off",
    // ---- the sidebar and the view picker
    "sb.categories": "Categories", "sb.all": "All downloads", "sb.status": "Status", "sb.unfinished": "Unfinished", "sb.finished": "Finished", "sb.errors": "Errors",
    "sb.queues": "Queues", "sb.newQueue": "New queue…", "sb.show": "Show downloads", "sb.running": "Running",
    "cat.Compressed": "Compressed", "cat.Documents": "Documents", "cat.Music": "Music", "cat.Programs": "Programs", "cat.Video": "Video", "cat.Images": "Images", "cat.General": "General",
    // ---- the list: columns
    "col.name": "File name", "col.size": "Size", "col.status": "Status", "col.eta": "Time left", "col.speed": "Transfer rate", "col.up": "Upload", "col.lastTry": "Last try date",
    "col.desc": "Description", "col.added": "Added", "col.finished": "Finished", "col.category": "Category", "col.queue": "Queue", "col.url": "Link",
    "col.dir": "Save to", "col.conns": "Connections", "col.scan": "Virus scan",
    "col.sortTip": "Click to sort · drag to move · right-click for columns", "col.needsRoom": "needs a wider window",
    "col.arrange": "Arrange columns…", "col.reset": "Reset columns",
    "cols.title": "Columns", "cols.hint": "Tick the columns to show and set their order. You can also drag column headers, or right-click them.",
    "cols.up": "Move up", "cols.down": "Move down", "cols.always": "always", "cols.room": "when there's room",
    // ---- the list: rows, scan states, empty states
    "row.inQueue": "In queue: {name}", "row.uploadTip": "Uploading to other peers", "row.limitedTip": "Limited to {rate}",
    "row.private": "Private", "row.privateTip": "From a private window: it leaves this list once it's finished (the file stays)",
    "scan.waiting": "Waiting…", "scan.scanning": "Scanning…", "scan.clean": "No threats", "scan.threat": "Threat found", "scan.failed": "Not scanned",
    "empty.none": "<b>No downloads yet</b>Click {add}, paste a link (Ctrl+V), or drop one here.",
    "empty.view": "<b>Nothing here</b>No downloads match this view.",
    // ---- the row's context menu
    "ctx.showProgress": "Show progress…", "ctx.stopSharing": "Stop sharing", "ctx.chooseFiles": "Choose files…",
    "ctx.checkFiles": "Check files", "tp.checkTip": "Read the files back and download again any piece that doesn't match",
    "ctx.refreshLink": "Refresh download link…", "ctx.openInBrowser": "Open in browser", "ctx.checkFile": "Check the file…",
    "ctx.removeFromQueue": "Remove from queue", "ctx.moveUp": "Move up", "ctx.moveDown": "Move down",
    "ctx.scanNow": "Scan for viruses now", "ctx.exportSelected": "Export selected…",
    "ctx.linkCopied": "Link copied", "ctx.magnetCopied": "Magnet link copied",
    // ---- asking first
    "cf.freeHoursTitle": "Download outside your free hours?",
    "cf.freeHoursBody": "These downloads are waiting for your free data hours ({window}). Starting them now uses your normal data.",
    "cf.freeHoursNow": "Download now",
    "cf.queueFreeHoursTitle": "Start outside your free hours?",
    "cf.queueFreeHoursBody": "This queue normally runs in your free data hours ({window}). Starting it now uses your normal data.",
    "cf.startNow": "Start now",
    "cf.removeTitle": "Remove from list",
    "cf.removeBody": "{n, plural, one {Remove \"{name}\" from the list?} other {Remove # downloads from the list?}}{partial, select, none {\nFiles stay on disk.} single {\nWhat's downloaded so far is deleted.} other {\nWhat's downloaded so far of unfinished ones is deleted.}}",
    "cf.deleteTitle": "{n, plural, one {Delete file} other {Delete files}}",
    "cf.deleteBody": "{n, plural, one {Delete \"{name}\" and its file from disk?} other {Delete # downloads and their files from disk?}}",
    "cf.redownloadTitle": "Redownload",
    "cf.redownloadBody": "{n, plural, one {Start \"{name}\" again from the beginning?} other {Start # downloads again from the beginning?}}\nWhat's downloaded so far is discarded.{tor, plural, =0 {} one {\nThe torrent checks its files instead: the pieces already downloaded are kept.} other {\nThe torrents check their files instead: the pieces already downloaded are kept.}}",
    "cf.removeCompletedTitle": "Remove completed",
    "cf.removeCompletedBody": "{n, plural, one {Remove 1 completed download from the list?} other {Remove # completed downloads from the list?}}\nFiles stay on disk.",
    "n.limiterOn": "Speed limiter on — {rate} total, on top of the traffic profile", "n.limiterOff": "Speed limiter off",
    // ---- a download's status words (the list, the progress window, the tray)
    "st.complete": "Complete", "st.completeChecking": "Complete · checking {pct}", "st.completeDamaged": "Complete · damaged",
    "st.completeScanning": "Complete · scanning", "st.completeThreat": "Complete · threat found", "st.completeVerified": "Complete · verified",
    "st.completeSharing": "Complete · sharing", "st.completeSharingPaused": "Complete · sharing paused",
    "st.completeShareLater": "Complete · sharing later", "st.completeMissing": "Complete · files missing", "st.completeChanged": "Complete · files changed",
    "st.errorRetrying": "Error · retrying in {wait}", "st.error": "Error", "st.linkRefreshing": "Getting a fresh link…", "st.linkExpired": "Link expired",
    "st.connecting": "Connecting…", "st.retrying": "Retrying in {wait}", "st.reconnecting": "Reconnecting…",
    "st.recording": "Recording · {time}",
    "st.waitFreeHours": "Waiting for free hours", "st.waitSlot": "Waiting for a free slot", "st.waitMetered": "Waiting · metered connection",
    "st.waitQuota": "Waiting · data quota used up", "st.queued": "Queued",
    "st.paused": "Paused", "st.pausedMetered": "Paused · metered connection",
    "st.capReached": "Data cap reached", "st.quotaUsedUp": "Data quota used up",
    "st.withPct": "{pct} · {word}",
    "st.gettingList": "Getting the file list…", "st.gettingListPct": "Getting the file list… {pct}",
    "st.peers": "{pct} · {peers, plural, =0 {finding peers} one {# peer} other {# peers}}",
    "st.checkingFiles": "Checking files · {pct}",
    "size.approx": "~{size}",
    "queue.syncTag": "sync",
    // ---- the status bar
    "bar.downloading": "Downloading: <b>{n}</b>", "bar.connections": "Connections: <b>{n}</b>", "bar.unfinished": "Unfinished: <b>{n}</b>",
    "bar.queuesStopped": "Queues stopped", "bar.queueRunning": "{name} running", "bar.queueChecking": "{name} checking",
    "bar.queuesRunning": "{n, plural, one {# queue running} other {# queues running}}",
    "bar.limiterOn": "Limiter: <b>{rate}</b>", "bar.limiterOff": "Limiter: <b>off</b>", "bar.total": "Total: <b>{speed}</b>",
    "bar.upload": "Upload: <b>{speed}</b>", "bar.uploadPaused": "Upload: <b>paused</b>", "bar.uploadPausedTip": "Uploading is paused: a metered connection, a data limit, or free hours that haven't begun", "bar.sharingTip": "{n, plural, one {Sharing # finished torrent} other {Sharing # finished torrents}}",
    "bar.metered": "Metered<long> connection</long>", "bar.meteredPaused": "Metered<long> connection</long> · {n} paused",
    "bar.meteredTip": "Downloads carry on when you're back on your normal connection. One you start by hand keeps going.",
    "bar.freeUntil": "Free hours · until <b>{time}</b>", "bar.freeIn": "Free hours in <b>{wait}</b>",
    "bar.polite": "Polite: <b>{rate}</b>", "bar.politeUp": "↑ {rate}", "bar.fullSpeed": "full speed",
    "bar.politeLine": "{name}: {down} down, {up} up, +{ms} ms",
    "bar.politeTip": "Polite mode is easing off because the line is busy (extra delay {ms} ms)",
    "bar.politeTipUp": "Polite mode is easing off because the line is busy (extra delay {ms} ms) · uploads {up}",
    "bar.networks": "Networks: <b>{n}</b>",
    "bar.capHit": "Data cap reached · resumes in {wait}", "bar.capHitTip": "Downloads continue when part of the allowance frees up",
    "bar.cap": "Data cap: <b>{used}</b> of {limit}", "bar.capTip": "Download limit per {hours, plural, one {hour} other {# h}}",
    "bar.quotaUsedUp": "Quota used up", "bar.quotaClick": "{tip} — click to carry on anyway",
    "bar.quotaUsed": "<long>{period} </long><b>{used}</b> / {limit}",
    "bar.profile": "<long>Profile: </long><b>{name}</b>",
    "bar.then": "Then: {action}", "bar.thenTip": "{why}. Waiting for the other downloads to finish, then counting down for a minute.",
    "bar.whenDone": "When done: <b>{action}</b>", "bar.whenDoneTip": "When all downloads finish (Tasks menu)",
    "quota.today": "Today", "quota.month": "This month",
    "quota.name": "{period, select, month {This month's} other {Today's}} {limit} data quota",
    "quota.nameNet": "{period, select, month {This month's} other {Today's}} {limit} data quota on {net}",
    "quota.resetsMidnight": "at midnight", "quota.resetsNow": "now", "quota.resetsOn": "on {date}",
    "quota.tip": "{name} — starts again {resets}", "quota.tipFree": "{name} — starts again {resets}. Free data hours now: nothing counts",
    "quota.title": "Data quota", "quota.change": "Change quotas…", "quota.carryOn": "Carry on anyway", "quota.wait": "Wait",
    "quota.usedUp": "{name} is used up ({used}). {count, select, down {Downloads wait} up {Uploads wait} other {Downloads and uploads wait}} until it starts again {resets}.\nCarry on anyway, using data past the quota until then?",
    "quota.status": "{name}: {used} used{over, select, yes {, carrying on past it} other {}}. It starts again {resets}.",
    "profile.tip": "Traffic profile in use — click to switch", "profile.tipHand": "Traffic profile in use, chosen by hand until {time} — click to switch",
    "profile.tipSched": "Traffic profile in use, by the schedule — click to switch", "profile.edit": "Edit traffic profiles…",
    "profile.switched": "Traffic profile: {name}", "profile.switchedUntil": "Traffic profile: {name} until {time}",
    "n.capReached": "Data cap reached. Downloads carry on in {wait}",
    "app.title": "Zipline Download Manager", "app.titleSpeed": "{speed} · Zipline Download Manager",
    "bar.netLine": "{name}: {speed}",
    // ---- windows, toasts and what a screen reader hears
    "err.unreachable": "Couldn't reach Zipline. Is the app still running?",
    "btn.working": "Working…", "btn.show": "Show", "btn.closeTip": "Close",
    "toast.label": "Notification", "toast.dismiss": "Dismiss", "toast.dismissLong": "Dismiss notification",
    "live.more": "{first}. {n, plural, one {And # more message} other {And # more messages}}",
    "n.aDownload": "A download", "n.finished": "{n, plural, one {{name} finished} other {# downloads finished}}",
    "notes.title": "Notifications", "notes.clear": "Clear all",
    "notes.bellNew": "Notifications, {n} new", "notes.bellTip": "Notifications ({n} new)",
    "notes.empty": "Nothing yet. Finished downloads, problems and other messages from Zipline are listed here.",
    "pb.value": "{pct}", "pb.withLeft": "{pct} · {left} left",
    // ---- Add link
    "f.user": "User name", "f.password": "Password", "f.referer": "Referer", "f.cookie": "Cookie", "f.link": "Link",
    "dlg.advanced": "Advanced: login, referer, cookies",
    "dlg.addLink.title": "Add link", "dlg.addLink.placeholder": "https://example.com/file.zip  or  a magnet link",
    "dlg.addLink.bad": "Enter an http://, https://, ftp:// or ftps:// link, or a magnet link.",
    "n.torrentTooBig": "{name} is too big for a torrent file", "n.nameError": "{name}: {error}",
    // ---- the words in front of a value (file info, progress, properties)
    "f.category": "Category", "f.saveTo": "Save to folder", "f.fileName": "File name", "f.desc": "Description", "f.laterInto": "Later into",
    "f.quality": "Quality", "f.download": "Download", "f.checksum": "Checksum", "f.mirrors": "Mirrors",
    "f.record": "Record", "f.stopAfter": "Stop after", "fact.recording": "Recording",
    "fact.type": "Type", "fact.length": "Length", "fact.pieces": "Pieces", "fact.savedAs": "Saved as", "fact.size": "Size", "fact.resume": "Resume support",
    "fact.servers": "Servers", "fact.checksum": "Checksum", "fact.from": "From", "fact.mirrors": "Mirrors", "fact.downloaded": "Downloaded",
    "fact.freeHours": "Free data hours", "fact.findingPeers": "Finding peers", "fact.comment": "Comment",
    "fact.link": "Link", "fact.status": "Status", "fact.rate": "Transfer rate", "fact.timeLeft": "Time left", "fact.conns": "Connections", "fact.resumable": "Resumable",
    "yn.yes": "<ok>Yes</ok>", "yn.no": "<warn>No</warn>",
    "btn.browse": "Browse", "btn.chooseFolder": "Choose folder",
    // ---- file info
    "fi.title": "Download file info", "fi.requesting": "Requesting file information…",
    "fi.later": "Download later", "fi.laterFree": "Download in free hours", "fi.start": "Start download", "fi.adding": "Adding…", "fi.starting": "Starting…", "fi.switching": "Switching…",
    "fi.stream": "Stream",
    "fi.match": "This is the same file as <b>{name}</b>, which stopped at {pct}. Use this link to finish it instead of starting again?",
    "fi.renew": "Carry on with this link",
    "fi.private": "From a private window: Zipline doesn't keep it in its list once it's finished. The file stays where you save it.",
    "fi.challenge": "The site wants to check you're a person before it sends the file. Open the link in your browser; once the download starts there, the extension hands it to Zipline.",
    "fi.readFail": "Couldn't read file info: {error}<br>You can still add it — Zipline will retry when the download starts.",
    "fi.hlsType": "Video stream ({format})", "fi.hlsTypeMp4": "Video stream ({format}) · MP4 pieces",
    "fi.hlsPieces": "{n} · fetched on up to {conns, plural, one {# connection} other {# connections}}",
    "fi.savedMerged": "MP4 <m>· the separate audio track is merged in, no quality loss</m>", "fi.savedTs": "MPEG-TS (.ts) <m>· plays in VLC</m>",
    "fi.savedMp4": "MP4", "fi.savedRepack": "MP4 <m>· repackaged when the download finishes, no quality loss</m>",
    "fi.liveType": "Live stream ({format})", "fi.eventType": "Live event ({format}) <m>· everything since it began is still there</m>", "fi.liveLength": "Live",
    "fi.liveNow": "From now", "fi.liveStart": "From the start",
    "fi.liveSaved": "Starts a little behind live; each piece is saved as it arrives. The MP4 is made when the stream ends, at the time limit, or when you press Pause.",
    "fi.stopAfterPh": "h:mm", "fi.stopAfterAria": "Stop after, in hours:minutes",
    "fi.stopAfterHint": "Leave it empty to record until the stream ends.", "fi.stopAfterBad": "Write the length as hours:minutes, like 1:30.",
    "fi.metalink": "Metalink · {n, plural, one {# file} other {# files}}", "fi.metalinkSize": "Metalink · {n, plural, one {# file} other {# files}}, {size}",
    "fi.servers": "{n} · downloaded from several at once",
    "fi.sumPieces": "{algo} and one per piece: damaged pieces are fetched again", "fi.sumChecked": "{algo} · checked when it finishes",
    // (a metalink's piece checksums on their own, its torrent and its folders)
    "fi.sumPiecesOnly": "{algo} for each piece · checked as it arrives, damaged pieces are fetched again",
    "fact.torrent": "Torrent", "fi.mlTorrent": "Downloaded with BitTorrent",
    "fi.mlTorrentSeeds": "Downloaded with BitTorrent · {n, plural, one {the mirror helps} other {the # mirrors help}} as web seeds",
    "fact.folders": "Folders", "fi.mlFolders": "Kept, inside the folder you save to: {dir}{more, select, yes { and others} other {}}",
    "fi.sizeBytes": "{size} <m>({bytes, plural, one {# byte} other {# bytes}})</m>",
    "fi.resumeYes": "<ok>Yes</ok> · up to {n, plural, one {# parallel connection} other {# parallel connections}}", "fi.resumeNo": "<warn>No</warn> · single connection",
    "fi.fromShared": "{service} (shared link)",
    "fi.mirrorsMore": "{n, plural, one {# more server} other {# more servers}} listed by the site · used together",
    "fi.sumSent": "{algo} sent by the server · checked when it finishes",
    "fi.allFiles": "All {n} files", "fi.allFilesSize": "All {n} files ({size})",
    "fi.folder": "Shared folder · {n, plural, one {# file} other {# files}}", "fi.folderSize": "Shared folder · {n, plural, one {# file} other {# files}}, {size}",
    "fi.folderSaved": "In a folder named <b>{name}</b>, with its folders kept", "fi.folderOne": "Straight into the folder you choose",
    "fi.folderMore": "This folder is too big to list in full: the first {n} files are shown.",
    "fi.more": "Checksum and mirrors", "fi.sumPlaceholder": "Optional: MD5, SHA-1, SHA-256 or SHA-512",
    "fi.sumHint": "Paste it from the download page and Zipline checks the file when it finishes.",
    "fi.mirrorsHint": "Other links for the same file, one per line. Zipline downloads from all of them at once and drops any that serve a different file.",
    "fi.sumBad": "That doesn't look like an MD5, SHA-1, SHA-256 or SHA-512 checksum.", "fi.sumGuess": "{algo} · checked when the download finishes.",
    "fi.eachOwnName": "Each file keeps its own name", "fi.byType": "By file type",
    "fi.earlyDone": "<ok>Already complete</ok> <m>· it's saved where you choose</m>", "fi.cantStart": "Couldn't start",
    "fi.earlyError": "<err>{error}</err> <m>· Start tries again</m>",
    "fi.earlyProgress": "{done} ({pct})", "fi.earlyProgressSpeed": "{done} ({pct}) <m>· {speed}</m>",
    "fi.keepFail": "Couldn't keep the part already downloaded: {error}",
    // ---- a video stream's tracks in the Add dialog (quality, audio, subtitles, ad breaks)
    "f.audio": "Audio", "f.subs": "Subtitles", "f.ads": "Ad breaks",
    "fi.audioOnly": "Audio only",
    "q.res": "{p}p", "q.4k": "4K", "q.8k": "8K", "q.fps": "{n} fps", "q.hdr": "HDR",
    "codec.h264": "H.264", "codec.hevc": "HEVC", "codec.dv": "Dolby Vision", "codec.av1": "AV1", "codec.vp9": "VP9", "codec.vp8": "VP8",
    "codec.aac": "AAC", "codec.mp3": "MP3", "codec.ac3": "Dolby Digital", "codec.eac3": "Dolby Digital Plus", "codec.opus": "Opus", "codec.vorbis": "Vorbis",
    "codec.flac": "FLAC", "codec.alac": "ALAC", "codec.dts": "DTS", "codec.ac4": "AC-4", "codec.mpegh": "MPEG-H",
    "ch.mono": "mono", "ch.stereo": "stereo", "ch.atmos": "Atmos", "ch.n": "{n} channels",
    "lang.und": "Unknown language",
    "aud.desc": "audio description", "aud.all": "Keep all languages",
    "aud.allHint": "The one chosen above plays first; each track is labelled with its language.",
    "sub.none": "None", "sub.all": "All languages ({n})", "sub.forced": "forced", "sub.sdh": "SDH",
    "sub.hint": "Saved beside the video as .srt, and inside it.",
    "ads.drop": "Leave out ad breaks",
    "ads.hint": "{n, plural, one {The stream marks one break ({dur})} other {The stream marks # breaks ({dur} in all)}}. Off by default: marks aren't always right, and a part left out can't be put back without downloading again.",
    "fi.savedWebm": "WebM <m>· no quality loss</m>", "fi.savedMkv": "Matroska (.mkv) <m>· these tracks don't fit in MP4 or WebM; no quality loss</m>",
    "fi.savedM4a": "M4A <m>· the sound alone</m>", "fi.savedOpus": "Opus <m>· the sound alone</m>", "fi.savedMka": "Matroska audio (.mka) <m>· the sound alone</m>",
    "n.addedFree": "Added. It downloads in your free data hours ({window}).",
    "n.addedQueue": "Added to {queue}. Start the queue or set a schedule to download it.", "n.addedTo": "Added to {queue}.", "n.theQueue": "the queue",
    // ---- torrent in the Add dialog
    "tor.title": "Download torrent", "tor.exists": "This torrent is already in your list as <b>{name}</b>.", "tor.default": "Torrent",
    "tor.sub": "{size} · {n, plural, one {# file} other {# files}}", "tor.subPrivate": "{size} · {n, plural, one {# file} other {# files}} · private torrent", "tor.sizeUnknown": "Size not known yet",
    "tor.getting": "Getting the file list from peers…", "tor.looking": "Looking for peers that have this torrent.",
    "tor.files": "Files", "tor.firstN": "Showing the first {n} files.", "tor.chosen": "{checked} of {n} chosen · {size}",
    "tor.type": "Torrent{magnet, select, yes { (magnet link)} other {}}{priv, select, yes { · private: only its trackers find peers} other {}}",
    "tor.pieces": "{n} × {size} · each one checked as it arrives",
    "tor.trackers": "{n, plural, one {# tracker} other {# trackers}}", "tor.dht": "the DHT", "tor.webSeeds": "{n, plural, one {# web server} other {# web servers}}", "tor.inbound": "Peers that connect to Zipline",
    "tor.connected": "{n, plural, one {Connected to # peer} other {Connected to # peers}}… You can start now and choose files later in Properties.",
    "tor.connectedPct": "{n, plural, one {Connected to # peer} other {Connected to # peers}} · {pct}… You can start now and choose files later in Properties.",
    "tor.lookingStart": "Looking for peers that have this torrent. You can start now and choose files later in Properties.",
    "tor.seq": "Download in order", "tor.ends": "First and last pieces first", "tor.prioAria": "Priority of {name}",
    "tor.orderHint": "To watch or listen while it downloads: pieces arrive from the start onwards, and a player finds each file's beginning and end.",
    "prio.skip": "Skip", "prio.low": "Low", "prio.normal": "Normal", "prio.high": "High",
    "pick.wait": "Choose a folder in the window that just opened…", "pick.fail": "Couldn't open the folder picker.",
    // ---- progress windows
    "pf.progress": "Download progress", "pf.limit": "Limit this download to", "pf.limitAria": "Speed limit in KB/s",
    "pf.hide": "Hide details", "pf.show": "Show details",
    "pd.title": "{pct} {name}", "pd.titleName": "{name}",
    "pd.connN": "N.", "pd.connDone": "Downloaded", "pd.connInfo": "Info", "pd.servers": "Servers", "pd.server": "Server", "pd.connections": "Connections", "pd.received": "Received", "pd.main": "main",
    // (a metalink's piece checks, and the servers that sent damaged pieces)
    "pd.pieceCheck": "{ok} of {n} pieces checked", "pd.pieceCheckBad": "{ok} of {n} pieces checked · <warn>{bad, plural, one {# damaged, fetched again} other {# damaged, fetched again}}</warn>",
    "pd.srcBad": "<warn>{n, plural, one {sent # damaged piece} other {sent # damaged pieces}}</warn>", "pd.dropDamaged": "dropped: sent damaged data",
    "pd.st.downloading": "Receiving data…", "pd.st.connecting": "Connecting…", "pd.st.paused": "Paused", "pd.st.queued": "Waiting in queue", "pd.st.completed": "Complete",
    "pd.st.error": "Error: {error}", "pd.stage": "{stage}… {pct}",
    "pd.estimate": "~{size} (estimate)", "pd.workingOut": "Working it out…",
    "pd.doneHls": "{done} ({pct}) · piece {piece} of {total}", "pd.doneOf": "{done} ({pct})",
    "pd.fetching": "{n} fetching video pieces", "pd.oneOnly": "1 (the server doesn't allow more)", "pd.active": "{n} active", "pd.activeMax": "{n} active · up to {max}",
    "seg.receiving": "Receiving data…", "seg.connecting": "Connecting…", "seg.retry": "Retrying…", "seg.failed": "Failed", "seg.done": "Done", "seg.idle": "Waiting",
    "pd.hlsConns": "{n, plural, one {# connection} other {# connections}} fetching video pieces in parallel", "pd.noConns": "No active connections", "pd.noRanges": "1 connection (server does not support ranges)",
    // ---- torrent progress
    "fact.shared": "Shared", "a.stopSharing": "Stop sharing",
    "tp.mapTip": "Green: downloaded and checked · blue: partly here · striped: files you didn't choose", "tp.details": "Details",
    "tp.peers": "Peers", "tp.trackers": "Trackers", "tp.files": "Files", "tp.peersCount": "Peers ({n})",
    "tp.peer": "Peer", "tp.client": "Client", "tp.has": "Has", "tp.down": "Down", "tp.up": "Up", "tp.tracker": "Tracker", "tp.status": "Status", "tp.file": "File", "tp.size": "Size", "tp.done": "Done",
    "tp.sharing": "Complete · sharing with other peers", "tp.sharingLimit": "Complete · sharing with other peers · {limit}",
    "tp.stopsAt": "stops at ratio {ratio} or after {time}",
    "sh.toast": "Not sharing {name}: {why}", "sh.tip": "Not sharing: {why}",
    "sh.no.off": "its sharing ratio or time is set to 0 (change it in Options → Torrents, or in its Properties)",
    "sh.no.info": "Zipline no longer has the torrent's details — add the torrent again",
    "sh.no.missing": "\"{file}\" is missing from its folder — Check files will download it again",
    "sh.no.changed": "\"{file}\" has changed since it was downloaded — Check files will see if it is still good",
    "sh.no.parts": "the part kept for the files you didn't choose is gone — Check files will download it again",
    "tp.gettingPct": "Getting the file list from peers… {pct}", "tp.looking": "Looking for peers…", "tp.torrentFile": "Torrent file", "tp.notKnown": "Not known yet",
    "tp.sizeFiles": "{size} · {chosen} of {n, plural, one {# file} other {# files}}", "tp.rates": "↓ {down} · ↑ {up}", "tp.ratesHeld": "↓ {down} · ↑ paused",
    "tp.conn": "{n, plural, one {# peer} other {# peers}}{seeds, plural, =0 {} other { ({seeds} with the whole torrent)}}{known, plural, =0 {} other { · {known} more known}}",
    "tp.uploaded": "{size} uploaded · ratio {ratio}",
    "tp.noPeers": "No peers connected yet", "tp.loading": "Loading…", "tp.notRunning": "Not running",
    "tp.mEnc": "encrypted", "tp.mEncTip": "The connection is encrypted",
    "tp.mObf": "encrypted handshake", "tp.mObfTip": "The handshake is encrypted; the data that follows isn't",
    "tp.mUtp": "uTP", "tp.mUtpTip": "Over uTP, which makes way for your other traffic",
    "tp.mFast": "fast extension", "tp.mFastTip": "Speaks the fast extension: a pause in uploading doesn't lose what was asked for",
    "tp.dht": "DHT", "tp.dhtNote": "· peers find each other without a tracker", "tp.on": "On", "tp.noTrackers": "This torrent has no trackers",
    "tp.skipped": "Skipped", "tp.listArrives": "The file list arrives from peers",
    // ---- refreshing a link, checksums
    "n.freshLink": "Getting a fresh link from {host}…", "n.pageOpen": "The page is open in your browser. Click the download link there — Zipline picks it up and carries on.",
    "fi.sumList": "a checksum list", "fi.sumBase64": "a base64 checksum",
    "sum.you": "the one you gave", "sum.server": "sent by the server", "sum.metalink": "from the metalink", "sum.from": "from {name}",
    // ---- complete, properties
    "cd.title": "Download complete", "cd.noMore": "Don't show this again", "fact.file": "File", "fact.savedTo": "Saved to", "btn.opening": "Opening…",
    "btn.copy": "Copy", "btn.saving": "Saving…",
    "pr.title": "Properties — {name}", "pr.needLink": "Enter the link of the file.",
    "pr.linkHint": "Link expired? Paste a fresh link for the same file and resume — progress is kept.",
    "pr.mirrorsPh": "Other links for the same file, one per line",
    "f.speedLimit": "Speed limit", "pr.limitUnit": "KB/s · 0 = no limit", "f.virusScan": "Virus scan",
    "pr.scanDefault": "Use my setting ({state, select, on {scan} other {don't scan}})", "pr.scanOnFile": "Scan this file when it finishes", "pr.scanOffFile": "Don't scan this file",
    "pr.scanOnFiles": "Scan the files when they finish", "pr.scanOffFiles": "Don't scan these files",
    "pr.sumPh": "MD5, SHA-1, SHA-256 or SHA-512 to check against",
    "fact.category": "Category", "fact.added": "Added", "fact.finished": "Finished", "fact.queue": "Queue", "fact.fromPage": "From page", "fact.linkExpires": "Link expires",
    "fact.virusScan": "Virus scan", "fact.syncCheck": "Last sync check", "fact.infoHash": "Info hash",
    "pr.sizeBytes": "{size} ({bytes, plural, one {# byte} other {# bytes}})",
    "pr.linkRefreshing": "The link expired. Getting a fresh one from <b>{host}</b>…",
    "pr.linkWaiting": "Waiting for a fresh link: click the download link on <b>{host}</b>, or paste one below.",
    "pr.linkExpiredPage": "The link expired. Click the download link on <b>{host}</b> again, or paste a fresh link below — the part already downloaded is kept.",
    "pr.linkExpired": "The link expired. Paste a fresh link below — the part already downloaded is kept.",
    "pr.getFresh": "Get a fresh link", "pr.expired": "Expired {span} ago", "pr.expires": "{when} (in {span})",
    "pr.sumCopied": "Checksum copied.", "pr.magnetCopied": "Magnet link copied.",
    "f.magnet": "Magnet link", "tp.waitTitle": "The file list hasn't arrived yet", "tp.waitHint": "It comes from peers once the torrent is running. You can choose files here then.",
    "tp.filesToDownload": "Files to download", "tp.untick": "Unticking a file stops downloading it; what's already downloaded stays on disk.",
    "tp.sub": "{size}{n, plural, =0 {} other { · # files}}{priv, select, yes { · private torrent} other {}}",
    "tp.upState": "{size} uploaded · ratio {ratio}{sharing, select, yes { · sharing now} other {}}",
    // ---- the data plan (free hours)
    "plan.nowUntil": "now, until {time}", "plan.window": "{from} – {to}, in {wait}", "plan.none": "Nothing waiting in the Free data hours queue.",
    "plan.waiting": "{items, plural, one {# download waiting} other {# downloads waiting}}{left, select, none {} other {, {size} to go}}{est, select, none {} periods {{n, plural, one { · about # free period, finishing {when}} other { · about # free periods, finishing {when}}}} finish { · finishing {when}} other { · the finish time is estimated once Zipline has seen your free-hours speed}}.",
    "plan.usedSo": "Used so far: <b>{size}</b>.", "plan.usedLast": "Last free period: <b>{free}</b>. Since then, outside free hours: <b>{paid}</b>.",
    "plan.nowB": "<b>Free hours now</b>, until {time}", "plan.nextB": "<b>Next free hours:</b> {window}",
    // ---- the Options dialog's sections and the keep-in-browser keys
    "opt.general": "General", "opt.conn": "Connection", "opt.plan": "Data plan", "opt.sites": "Sites", "opt.proxy": "Proxy", "opt.save": "Save to", "opt.cats": "File types",
    "opt.scan": "File checks", "opt.sounds": "Sounds", "opt.integration": "Browser", "opt.torrents": "Torrents", "opt.phone": "Phone",
    "key.alt": "{alt} + click", "key.ctrlalt": "Ctrl + {alt} + click", "key.ctrlshift": "Ctrl + Shift + click", "key.shiftalt": "Shift + {alt} + click", "key.off": "Off",
    "n.selectOne": "Select one download in the list first", "pr.checking": "Checking the file… {pct}",
    // ---- Options
    "o.title": "Zipline options", "o.sections": "Section",
    "o.prog": "Show progress window when a download starts", "o.done": "Show \"Download complete\" window",
    "o.early": "Start downloading while the Add dialog is open",
    "o.earlyHint": "Saves the time you spend choosing a folder or name. If you pick another folder, the part already downloaded moves with it; Cancel deletes it.",
    "o.tray": "Show Zipline in the notification area (system tray)",
    "o.trayHint": "Click the icon to open Zipline; right-click it for quick actions. Closing the window keeps Zipline running there.",
    "f.theme": "Theme", "o.themeSystem": "System", "o.themeLight": "Light", "o.themeDark": "Dark",
    "o.power": "Power", "o.awake": "Keep the computer awake while downloading",
    "o.awakeHint": "The screen can still turn off, and once nothing is downloading the computer sleeps as usual. To sleep or turn off when downloads finish, see Tasks → When all downloads finish.",
    "o.startTitle": "When Zipline starts", "o.startResume": "Carry on with downloads that were running",
    "o.startResumeHint": "Queues that were running start again too. Zipline first checks whether the connection is metered, then waits a few seconds so you can cancel.",
    "o.startPaused": "Start with everything paused", "o.startPausedHint": "Nothing downloads until you resume it, or a queue's schedule starts it.",
    "o.conns": "Connections per download",
    "o.connsHint": "The most each download uses. Zipline starts with a few and adds more while that makes it faster; downloads from the same server share this many. Some servers allow fewer per client.",
    "o.maxact": "Downloads at once", "o.maxactHint": "For downloads you start yourself; more wait their turn. Queues have their own number in {link}.", "o.queuesLink": "Queues & scheduler",
    "o.limTotal": "Limit total to", "o.limAria": "Total speed limit in KB/s",
    "o.cap": "Data cap", "o.capNoMore": "No more than", "o.capAria": "Megabytes allowed", "o.capUnit": "MB every", "o.capHours": "Hours", "o.hours": "{n, plural, one {# hour} other {# hours}}",
    "o.capUse": "Used so far in this window: {used} of {limit}{blocked, select, yes { — downloads are waiting} other {}}.",
    "o.capIdle": "When the cap is reached, downloads wait and carry on automatically as the allowance frees up.",
    "o.limEach": "Each download can also have its own limit (progress window or Properties).",
    "o.tp": "Traffic profile", "o.tpAdd": "Add profile",
    "o.tpHint": "Sets of limits to switch between, such as Light while you work and Night for big downloads. They apply on top of the settings above, and the speed limiter stays an overall limit on top of the profile. \"Usual\" means the number above.",
    "o.tpUse": "Use", "o.tpOtherwise": "Otherwise use", "o.tpSched": "Switch profiles by the time of day",
    "o.tpSchedHint": "Outside these times the profile chosen above applies. Switching by hand (status bar, tray or phone) lasts until the next change.",
    "o.tpAddTime": "Add a time",
    "o.sharing": "Sharing the line", "o.polite": "Polite mode: ease off when other apps or devices need the internet",
    "o.politeHint": "Zipline watches the delay on your connection. When a video call, a game or a TV in another room needs it, Zipline slows its downloads and uploads, then speeds back up once the line is free. Unlike a speed limit, nothing is lost while nobody else is online. With several connections, each is watched on its own.",
    "o.multiTitle": "Several internet connections", "o.multi": "Use several internet connections at once",
    "o.multiHint": "For example Wi-Fi plus a phone connected by USB, or two broadband lines. Each download's connections are spread across them, so their speeds add up.",
    "o.netsLooking": "Looking for network adapters…", "o.netsNone": "No network adapters with an internet address were found.",
    "o.netsOne": "Only one connection found. Connect another one (Ethernet, a phone over USB or a second Wi-Fi adapter) and it appears here.",
    "o.netWifi": "Wi-Fi", "o.netEthernet": "Ethernet", "o.netMobile": "Mobile / USB", "o.netVpn": "VPN", "o.netOther": "Network",
    "o.netNoInternet": "no internet right now", "o.netIdle": "Idle", "o.netTest": "Test", "o.netOnline": "Online · {ms, plural, =0 {under 1 ms} other {# ms}}", "o.netNone": "No internet",
    "o.plan": "My internet plan has free data hours",
    "o.planHint": "Such as free night-time data. Downloads in the <b>Free data hours</b> queue run only then, and carry on the next time if they don't finish.",
    "o.planFrom": "Free from", "o.planFromAria": "Free from", "o.planUntilAria": "Free until", "o.planTo": "to", "o.planDays": "Starting on", "o.planMargin": "Stop early by",
    "o.minutes": "{n, plural, one {# minute} other {# minutes}}",
    "o.planMarginHint": "So the last bytes don't spill into paid data if your clock and the provider's differ a little.",
    "o.upFree": "Uploads are free too during free hours",
    "o.upFreeHint": "Torrents from the Free data hours queue then share only in the free hours. Untick it if your provider counts uploads at all hours.",
    "o.meterTitle": "Metered connections", "o.meter": "Pause downloads on a metered connection, such as a phone hotspot",
    "o.meterWin": "Windows treats phone hotspots as metered, and you can mark any Wi-Fi as metered in Windows Settings → Network. Downloads carry on when you're back on your normal connection; one you start by hand keeps going.",
    "o.meterLinux": "Uses NetworkManager's metered setting, which it guesses for phone hotspots. Downloads carry on when you're back on your normal connection.",
    "o.meterMac": "macOS doesn't tell apps which connections are metered, so this isn't available on a Mac.",
    "o.quotas": "Data quotas", "o.qAdd": "Add quota", "o.qNet": "This computer is on <b>{net}</b> now. A quota can be for one network or for all of them.",
    "o.qCount": "What counts", "o.qBoth": "Downloads and uploads", "o.qDown": "Downloads only", "o.qUp": "Uploads only",
    "o.qCountHint": "Towards the quotas and the data cap (Options → Connection). Data used during your free hours doesn't count towards quotas.",
    "o.qWarn": "Warn at", "o.qWarnOpt": "{pct} used",
    "o.qWarnHint": "When a quota is used up, the transfers it counts wait until it starts again. You can carry on anyway from the status bar.",
    "o.pmSystem": "Use system proxy settings",
    "o.pmSystemHint": "Follows your computer's proxy settings, including setup scripts (PAC) and automatic detection: Windows, macOS, and GNOME or KDE on Linux. If a proxy in the list doesn't answer, the next one is used.",
    "o.pmNone": "No proxy", "o.pmNoneHint": "Always connect directly.", "o.pmManual": "Manual proxy", "o.pmManualHint": "HTTP or SOCKS5 proxy for every download. FTP downloads use it when it's SOCKS5.",
    "o.pAddr": "Proxy address", "o.pBypass": "Don't use for", "o.pTest": "Test connection", "o.pFtp": "FTP downloads always connect directly.",
    "o.pTesting": "Testing…", "o.pOk": "Connected ({via})", "o.pFail": "Failed via {via}: {error}",
    "o.sumsTitle": "Checksums", "o.sums": "Look for checksums the site publishes, and check files when they finish",
    "o.sumsHint": "A file is checked against the checksum you paste in the Add dialog, one the server sends, or one in a <code>SHA256SUMS</code>-style file next to the download. A file that doesn't match is flagged. With a metalink, only the damaged pieces are downloaded again.",
    "o.scanTitle": "Virus scan", "o.scan": "Scan each file for viruses when it finishes downloading",
    "o.scanHint": "Off by default. You can also choose per download in Properties, or right-click a finished file → Scan for viruses now.",
    "o.scanner": "Scanner", "o.scanAuto": "Automatic", "o.scanCustom": "Another program…", "o.scanProgram": "Program", "o.scanArgs": "Arguments",
    "o.scanArgsHint": "%f is replaced with the downloaded file. Exit code 0 means clean; anything else is reported as a problem.",
    "o.scanWin": "Uses Microsoft Defender, which comes with Windows. If it finds a threat, Defender quarantines the file as usual.",
    "o.scanOther": "Uses ClamAV (clamscan) if it's installed. Otherwise choose another program.",
    "o.dir": "Download folder", "o.catf": "Save into a sub-folder per category (Video, Music, Documents…)",
    "o.catsHint": "File extensions that decide each category (space separated). Links to these types are also caught from the clipboard.",
    "o.clip": "Watch the clipboard for download links", "o.clipHint": "When you copy a link to a known file type, Zipline asks whether to download it.",
    "o.keysTitle": "Keys to hold while clicking a link", "o.kZip": "Send it to Zipline", "o.kBrowser": "Keep it in the browser",
    "o.kBrowserHint": "The second one matters only when the extension takes over browser downloads: hold it to keep a download in the browser.",
    "o.skipTitle": "Never take over downloads from", "o.skipAria": "Sites to leave to the browser",
    "o.skipHint": "One site per line. Downloads from these sites always stay in the browser (sub-sites are included). You can still send a link from them on purpose with the key above or the right-click menu.",
    "o.expiredTitle": "Expired links", "o.refresh": "When a download link expires, get a fresh one from its page",
    "o.refreshHint": "Many sites give out links that only work for a while. The extension opens the page in a background tab for a moment, finds the same file and Zipline carries on where it stopped. Clicking the link again yourself works too: Zipline recognises the file instead of starting a new download.",
    "o.extTitle": "Browser extension", "o.extBrowsers": "Chrome, Edge, Brave and Opera; Firefox is covered in the user guide.",
    "o.extStep1": "Open <code>chrome://extensions</code> (or <code>edge://extensions</code>).", "o.extStep2": "Turn on <b>Developer mode</b>.",
    "o.extStep3": "Click <b>Load unpacked</b> and choose the <code>extension</code> folder that came with Zipline.",
    "o.extHint": "The extension adds \"Download with Zipline\" to the right-click menu and passes cookies and referer so sign-in-protected files work. Taking over all browser downloads is off by default (switch it on above, or in the extension's menu). It reaches this app through the browser's native messaging link, which Zipline sets up each time it starts, or over <code>127.0.0.1:17890</code> once you allow it.",
    "o.tkTitle": "Browser downloads", "o.takeover": "Take over downloads the browser starts",
    "o.takeoverHint": "Off by default. Zipline takes a download as the browser asked for it (the same address, sign-in and form), before the browser's own Save dialog, without fetching it twice. Close Zipline's Add dialog and the browser downloads it after all. Downloads from private windows leave Zipline's list once they finish.",
    "o.minSize": "Only files of at least", "o.skipTypes": "Leave these types to the browser",
    "o.skipTypesHint": "File extensions, separated by spaces. Web pages always stay in the browser. Judged by the file's own name and type, so a link like <code>download.php?id=7</code> that serves a real file is taken over.",
    "o.vbTitle": "Video button", "o.vbtn": "Show a Download button on video players",
    "o.vbtnHint": "The button appears when you point at a video. The extension's menu lists the videos on a page too.",
    "o.noBtnAria": "Sites without the video button", "o.noBtnHint": "Sites where the button stays hidden, one per line. \"Don't show on this site\" in the button's menu adds one here.",
    "o.errMinSize": "Enter a size of 0 KB or more.",
    "o.afterTitle": "After a torrent finishes", "o.tsShare": "Keep sharing it for a while",
    "o.tsShareHint": "Torrents work because people share what they've downloaded. Zipline shares each finished torrent, then stops by itself.",
    "o.tsOff": "Stop as soon as it finishes", "o.tsOffHint": "Private trackers usually expect you to share, so check their rules.",
    "o.tStop": "Stop sharing at", "o.tRatio": "ratio", "o.tOrAfter": "or after", "o.tRatioAria": "Sharing ratio", "o.tTimeAria": "Sharing time",
    "o.tStopHint": "A ratio of 1 means you've uploaded as much as you downloaded. Sharing pauses on a metered connection and while the traffic profile doesn't share, and stops when you quit Zipline or with right-click → Stop sharing.",
    "o.tAtMost": "At most", "o.tAtMostAria": "Most torrents sharing at once", "o.tAtMostUnit": "sharing at once", "o.tAtMostHint": "Beyond that, the torrents that have shared longest stop first.",
    "o.tSpeedTitle": "Speed and connections", "o.tUp": "Upload limit", "o.tPeers": "Peers per torrent", "o.tMeta": "File list wait", "o.tNoLimit": "No limit",
    "o.tMetaHint": "A magnet link that hasn't got its file list from other peers by then stops with an error, so its queue can finish. A torrent that crawls under 2 KB/s for a minute keeps trying but lets the next download in its queue start.",
    "o.tPort": "Listening port", "o.tPortAuto": "Auto",
    "o.tPortHint": "Other peers connect to Zipline on this port. Forwarding it to this computer in your router finds more peers. Leave it empty to pick one automatically.",
    "o.tDht": "Find peers through the DHT as well as trackers", "o.tDhtHint": "Most magnet links need it. Private torrents never use it.",
    "o.tMap": "Open the port on my router automatically (UPnP, NAT-PMP, PCP)",
    "o.tMapHint": "Lets peers on the internet connect to Zipline, so it finds more of them. The port closes again when Zipline quits.",
    "o.tMapIdle": "Zipline opens the port when a torrent starts.", "o.tMapLooking": "Looking for your router…", "o.tMapProxy": "Off while a proxy is on",
    "o.tMapOpen": "Port {port} open via {via} on your router (external address {ip})",
    "o.tMapOpenAs": "Port {port} open via {via} on your router, as port {ext} outside (external address {ip})",
    "o.tMapOpenNoIP": "Port {port} open via {via} on your router",
    "o.tMapNone": "Your router doesn't allow port mapping: it didn't answer UPnP, NAT-PMP or PCP",
    "o.tMapRefused": "Your router doesn't allow port mapping ({error})",
    "o.tMapConflict": "Another device on your network already has port {port} on the router: choose another listening port",
    "o.tMapCGNAT": "Your provider shares one address among many customers (carrier-grade NAT): other peers can't connect in",
    "o.tMapPrivate": "Your router is behind another router (its address outside, {ip}, is a private one): other peers can't connect in unless that router forwards port {port} too",
    "o.tProxyNote": "While a proxy is on, torrents connect through it only: no DHT, no incoming peers.",
    "o.tProxyNoteHTTP": "While a proxy is on, torrents connect through it only: no DHT, no incoming peers. UDP trackers aren't used either: an HTTP proxy can't carry them.",
    "o.tEnc": "Encryption", "o.tEncPrefer": "Prefer encrypted", "o.tEncRequire": "Require encrypted", "o.tEncPlain": "Plain only",
    "o.tEncHint": "Encrypted connections hide BitTorrent traffic from networks that slow it down, and reach peers that accept nothing else. Requiring encryption leaves out peers that can't encrypt.",
    "o.tOpenTitle": "Opening torrents", "o.tBrowser": "Take magnet links and .torrent files from the browser",
    "o.tBrowserHint": "With the Zipline extension, a magnet link or .torrent file you click opens here, even while Zipline leaves other downloads to the browser. Hold the keep-in-browser keys (Options → Browser) to leave one to the browser.",
    "o.tAssoc": "Open magnet links and .torrent files with Zipline",
    "o.tCheckDone": "Check the files again when finished", "o.tCheckDoneHint": "Reads every piece back from the disk once a torrent is complete and downloads again any that doesn't match. Big torrents take a while.",
    "o.tAssocWin": "For every browser, and for .torrent files in File Explorer. If you chose another torrent app before, Windows may ask which one to use.",
    "o.tAssocOther": "For every browser, and for .torrent files in your file manager.",
    "o.tAssocMac": "On a Mac, magnet links and .torrent files open in the app chosen in Finder (Get Info → Open with). Dropping a .torrent file or pasting a magnet link into Zipline works too.",
    "o.tAssocNone": "Dropping a .torrent file or pasting a magnet link into Zipline works too.",
    "o.phone": "Let my phone control Zipline over this network",
    "o.phoneHint": "See your downloads, add links, pause and resume from your phone's web browser. Nothing to install: the phone just needs to be on the same Wi-Fi as this computer.",
    "o.phPort": "Port", "o.phFwWin": "The first time, Windows asks whether Zipline may be reached on your network: allow it for private networks.",
    "o.phFwMac": "If macOS asks whether Zipline may accept incoming connections, choose Allow.", "o.phFwOther": "If a firewall is on, allow this port for your home network.",
    "o.phones": "Phones", "o.phAdd": "Add a phone", "o.phScan": "Scan this with your phone's camera", "o.phOpen": "Or open {url} on the phone and type this code:",
    "o.phAddr": "Network address", "o.phWaiting": "Waiting for your phone…",
    "o.phReady": "Ready at {addr}:{port}", "o.phNoNet": "Ready, but this computer isn't on a home network (Wi-Fi or Ethernet)", "o.phStarting": "Starting…",
    "o.phPaired": "Paired {added} · last used {seen}", "o.phNone": "No phones yet. Press <b>Add a phone</b> to pair one.",
    "o.phRemoveTitle": "Remove this phone?", "o.phRemoveBody": "{name} won't be able to control Zipline any more.",
    "o.phDone": "✓ Paired with {name}", "o.phGone": "This code isn't waiting any more.",
    "o.phKeyHint": "Each phone gets its own key, which never crosses the network. Remove a phone and it can't connect any more.",
    "o.phHttp": "The phone page is plain HTTP, not encrypted. Someone who can watch or change traffic on this Wi-Fi could read your list of downloads, or hand the phone a changed page that takes its key. So Zipline listens only on your private network, lets in only devices on it, and asks you before a new phone connects. Use it on a Wi-Fi you trust.",
    "o.phNets": "Networks", "o.phNetsHint": "The phone page is only on your private networks, home Wi-Fi or Ethernet. Zipline looks again whenever the network changes.",
    "o.phNetPublic": "{n, plural, one {This network is marked Public: the phone page is off here.} other {These networks are marked Public: the phone page is off on them.}}",
    "o.phNetPublicHint": "Public is what your system calls a network you don't trust, like a café or a hotel. Zipline keeps the phone page off such a network unless you allow it for that network.",
    "o.phNetAllow": "Allow on this network anyway", "o.phNetStop": "Stop allowing",
    "o.phCat.public": "marked Public", "o.phCat.private": "marked Private", "o.phCat.domain": "domain network",
    "o.phSt.on": "phone page on", "o.phSt.off": "phone page off here", "o.phSt.allowed": "allowed here anyway", "o.phSt.away": "allowed anyway, not connected now",
    "o.phErr.public": "The phone page is off on this network because it's marked Public. Allow it for this network in the list above, then add the phone again.",
    "o.phErr.nonet": "This computer isn't on a home network (Wi-Fi or Ethernet), so a phone has nothing to connect to.",
    "o.phAsked": "A phone has opened the code and is waiting for you to allow it, below.",
    "phq.line": "A phone wants to connect: <b>{name}</b>, {addr}",
    "phq.title": "A phone wants to connect to Zipline",
    "phq.body": "It opened the link from the QR code you made in Options → Phone.",
    "phq.phoneL": "Phone", "phq.addrL": "Address",
    "phq.warn": "Allow it only if you are holding that phone. Once allowed, it can see all your downloads and add, pause and remove them, until you remove it in Options → Phone. If you didn't just scan a code, deny it.",
    "phq.allowed": "{name} is connected", "phq.gone": "That request has run out. The phone can ask again.",
    "o.sitesHint": "Settings for particular sites. The most specific rule wins, so <code>files.example.com</code> beats <code>example.com</code>, and <code>example.com/members</code> covers only that part of the site.",
    "o.siteAdd": "Add site", "o.sitePh": "example.com  or  example.com/members", "o.siteRemove": "Remove this site", "o.siteConns": "Connections",
    "o.siteEmpty": "No site rules yet. Add one to limit connections for a strict server, or to save a login so protected files download without asking.",
    // ---- saved passwords and cookies: how they're kept (the "vault" in the settings)
    "f.passSaved": "•••••••• (saved)", "f.passForget": "Forget the saved password",
    "o.vault.dpapi": "Saved passwords and cookies are encrypted with your Windows account, so they can't be read if Zipline's files are copied elsewhere. Passwords are sent only to the matching site.",
    "o.vault.keychain": "Saved passwords and cookies are encrypted with a key that stays in your Mac's login Keychain, so Zipline's files alone can't be read. Passwords are sent only to the matching site.",
    "o.vault.secret": "Saved passwords and cookies are encrypted with a key that stays in your desktop's password store (the Secret Service), so Zipline's files alone can't be read. Passwords are sent only to the matching site.",
    "o.vault.file": "Saved passwords and cookies are encrypted with a key kept in a file in Zipline's data folder. That keeps other users out, but anyone who can read your files as you can read the key too. Install <code>secret-tool</code> (libsecret) to keep it in your desktop's password store instead. Passwords are sent only to the matching site.",
    "o.vault.none": "Saved passwords are kept as plain text in Zipline's data folder, which only your user account can read.",
    "o.vault.locked": "<warn>Some saved passwords couldn't be unlocked because their key wasn't available. They're kept as they are and work again once it is: unlock the Keychain or password store, then quit and start Zipline again.</warn>",
    "o.vault.plain": "<warn>The key wasn't available when some new passwords were saved, so they were saved without protection.</warn>",
    "o.snd": "Play sounds", "o.sndDone": "When a download finishes", "o.sndFailed": "When a download fails", "o.sndQueue": "When a queue finishes",
    "o.sndPlay": "Play", "o.sndPlayTip": "Play this sound",
    "o.sndHint": "Sounds play even while the Zipline window is closed. When several downloads finish together you hear one sound, not a burst.",
    "o.saved": "Options saved", "o.savedNote": "Options saved.",
    "o.profDefault": "Profile", "o.profName": "Profile name", "o.profDelete": "Delete this profile", "o.profLast": "At least one profile stays", "o.profInUse": "In use now: switch to another one first",
    "o.profDown": "Download limit", "o.profUp": "Upload limit", "o.noLimit": "No limit", "o.profAtOnce": "Downloads at once", "o.profConns": "Connections each", "o.profSeed": "Share finished torrents",
    "o.usual": "Usual ({n})", "o.ruleProfile": "Profile", "o.ruleFrom": "From", "o.ruleTo": "To", "o.ruleRemove": "Remove this time",
    "o.ruleEmpty": "No times yet. Add one, such as Light from 09:00 to 18:00 on weekdays.",
    "o.qEvery": "Every network", "o.qThis": "This network — {name}", "o.qNetAria": "Network", "o.qRemove": "Remove this quota", "o.qAllowance": "Allowance", "o.qUnit": "Unit",
    "o.qGB": "GB", "o.qMB": "MB", "o.qPeriod": "Every", "o.qDay": "Day", "o.qMonth": "Month", "o.qReset": "Starts again on", "o.qDayN": "Day {n}",
    "o.qEmpty": "No quotas. Add one to match your plan, such as 100 GB a month at home or 2 GB a day on your phone's hotspot.",
    "o.qUsed": "{period}: <b>{used}</b> of {limit} used{state, select, carry { · carrying on past it} over { · <err>used up</err>} other {}} · starts again {resets}",
    "o.qNotHere": "Not this network now", "o.qCarry": "Carry on anyway",
    "o.errKeys": "Choose different keys for Zipline and for the browser.", "o.errSite": "Enter the site this rule is for.", "o.errDays": "Choose at least one day for the free hours.",
    "o.errAllDay": "Free all day, every day? Then no plan is needed: switch it off instead.", "o.errPort": "Choose a port between 1024 and 65535 (not 17890).",
    "o.errRatio": "Enter the ratio to share up to, such as 1.", "o.errTPort": "Choose a port between 1024 and 65535 that the phone page isn't using, or leave it empty.",
    "o.errNets": "Tick at least one network connection below.", "o.errProxy": "Enter the proxy address, or choose another option.",
    "o.errScanner": "Enter the scanner program, or choose Automatic.", "o.errDir": "Choose the folder downloads are saved to.", "o.errProfName": "Give the profile a name.",
    "o.errTimes": "Choose the times and at least one day.", "o.errQuota": "Enter the quota's allowance, such as 100 GB.",
    "o.errAssoc": "Couldn't change which app opens torrents: {error}",
    "list.comma": ", ", "list.and": "{rest} and {last}", "dl.count": "{n, plural, one {# download} other {# downloads}}",
    "dl.sharing": "{n, plural, one {sharing # torrent} other {sharing # torrents}}",
    // ---- when finished, the countdown, resuming at start-up
    "wd.none": "Do nothing", "wd.quit": "Exit Zipline", "wd.sleep": "Sleep", "wd.hibernate": "Hibernate", "wd.shutdown": "Turn off computer",
    "wd.toastNone": "When all downloads finish: do nothing",
    "wd.toast": "When all downloads finish: {action, select, quit {exit Zipline} sleep {sleep} hibernate {hibernate} shutdown {turn off computer} other {do nothing}} (once)",
    "wd.keepQuit": "Cancel to keep Zipline running.", "wd.keepSleep": "Cancel to keep the computer awake.", "wd.keepOn": "Cancel to keep the computer on.",
    "wd.now": "Do it now", "wd.cancelled": "Cancelled", "wd.countdown": "{text} in <b>{wait}</b>",
    "wd.resuming": "Resuming {list} in {wait}", "wd.staysPaused": "Everything stays paused", "wd.pressCancel": "{text}. Press Cancel in the notification to stop it.",
    // ---- Queues & scheduler
    "qm.title": "Queues & scheduler", "qm.queues": "Queues", "qm.new": "New queue", "f.name": "Name", "qm.typeDl": "Download", "qm.typeSync": "Synchronization",
    "qm.hintSync": "Finished files stay in this queue. Each run re-checks them on the server and downloads the ones that changed.", "qm.hintDl": "Files leave the queue once they finish downloading.",
    "qm.max": "Downloads at a time", "qm.fail": "If a download fails", "qm.tryAgain": "try again", "qm.triesAria": "Times to try again", "qm.times": "times,", "qm.every": "every",
    "qm.minsAria": "Minutes between tries", "qm.minutes": "minutes",
    "qm.failHint": "0 = never. A file that's tried again moves to the end of the queue. Errors that won't go away by themselves, such as a missing file or a refused password, aren't retried.",
    "qm.schedule": "Schedule", "qm.startAt": "Start at", "qm.startAria": "Start time", "qm.wake": "Wake the computer to start",
    "qm.wakeHint": "Windows must allow wake timers: Control Panel → Power Options → Change plan settings → Change advanced power settings → Sleep → Allow wake timers.",
    "qm.repeat": "Start again every", "qm.repeatAria": "Repeat every", "qm.unitAria": "Unit", "qm.unitMin": "minutes", "qm.unitHr": "hours", "qm.stopAt": "Stop at", "qm.stopAria": "Stop time",
    "qm.days": "On days", "qm.whenDone": "When finished",
    "qm.whenDoneHint": "Only when the schedule started the queue and every file downloaded. Zipline waits for other downloads, then counts down for a minute so you can cancel.",
    "qm.files": "Files in this queue", "qm.count": "{n, plural, one {# file} other {# files}}", "qm.run": "Start now", "qm.halt": "Stop", "qm.stopping": "Stopping…",
    "qm.saved": "Queues saved", "qm.savedNote": "Queues saved.", "qm.delTitle": "Delete queue",
    "qm.delBody": "Delete \"{name}\"? Its files stay in your download list. The queue goes when you press OK or Apply.", "qm.planTip": "Used by your free data hours (Options → Data plan)",
    "qm.timeHint": "Times use this computer's clock. A start missed while the computer slept or Zipline was closed runs as soon as it can, if still before the stop time (or within 12 hours).",
    "qm.timeHintOs": "Waking the computer for a start needs administrator setup on macOS and Linux, so leave it awake.",
    "qm.empty": "Empty. Use \"{later}\" in the file info window, or right-click downloads → {add}.",
    "qm.planNote": "Runs in your free data hours: <b>{window}</b>. Change them in {link}.", "qm.planLink": "Options → Data plan",
    "qm.whole": "Enter a whole number from {lo} to {hi}.", "qm.errName": "Give the queue a name.", "qm.errDays": "Choose at least one day, or untick Start at.", "qm.defaultName": "Queue {n}",
    // ---- Site grabber
    "g.titlePage": "Links on this page", "g.projects": "Saved projects", "g.run": "Run", "g.startLink": "Start link", "g.follow": "Follow links", "g.depth0": "This page only",
    "g.depthN": "{n, plural, one {# level deep} other {# levels deep}}", "g.stay": "Stay on", "g.scopeSite": "This site", "g.scopeDomain": "Whole domain", "g.scopeFolder": "This folder",
    "g.types": "File types", "g.extsPh": "Other extensions, e.g. csv iso torrent", "g.extsAria": "Other extensions", "g.options": "Options",
    "g.cross": "Also collect files hosted on other sites (CDNs)", "g.robots": "Respect the site's robots.txt", "g.stopAfter": "Stop after", "g.pages": "pages",
    "g.stop": "Stop", "g.filterPh": "Filter by name or link", "g.allTypes": "All types", "g.selectAll": "Select all", "g.none": "None", "g.saveTo": "Save to", "g.addTo": "Add to",
    "g.startNow": "Start it now", "g.keep": "Keep the site's folders", "g.projSave": "Save as a project", "g.projHint": "Run it again later, or on a timer that queues only new files.",
    "g.projName": "Project name", "g.projBtn": "Save project", "g.every": "Check again every", "g.everyUnit": "hours (0 = only when I run it)",
    "g.scanInstead": "Scan the site instead…", "g.back": "Back", "g.start": "Start", "g.dl": "Download selected", "g.dlN": "Download selected ({n})",
    "g.cookieWait": "Getting sign-in from the browser…",
    "g.st.running": "Scanning…", "g.st.done": "Finished", "g.st.stopped": "Stopped", "g.st.error": "Stopped with an error",
    "g.fromPage": "{n, plural, one {# link} other {# links}} on “{title}” · files are ticked, web pages aren't", "g.thisPage": "this page",
    "g.scanned": "{words} · {pages, plural, one {# page} other {# pages}} scanned · {files, plural, one {# file} other {# files}} found", "g.skipped": "{n} skipped (robots.txt or page limit)",
    "g.showingFirst": "Showing the first {shown} of {total}. Use the filter to narrow it down.", "g.looking": "Looking for files…",
    "g.noFiles": "No matching files. Try more file types, a deeper level, or allowing files from other sites.", "g.page": "page", "g.selected": "{n} selected",
    "n.addedFiles": "{n, plural, one {Added # file to {queue}} other {Added # files to {queue}}}",
    "n.addedNoSignin": "{n, plural, one {Added # file to {queue}} other {Added # files to {queue}}} without the browser's sign-in: the extension didn't answer. Files that need it may fail.",
    "g.projH": "{name} · every {n} h", "g.projMin": "{name} · every {n} min",
    "g.errUrl": "Enter the link to start from.", "g.errTypes": "Pick at least one file type, or type an extension.", "g.errName": "Give the project a name.",
    "g.savedEvery": "Saved. Zipline will check every {n} h and queue new files.", "g.saved": "Project saved.", "g.delTitle": "Delete project",
    "g.delBody": "Delete the project \"{name}\"? Files you already downloaded stay where they are.",
    // ---- site grabber: polite crawling, sitemaps, failed pages, the browser's help, saved scans
    "g.wait": "Wait between requests", "g.waitUnit": "seconds", "g.waitHint": "Zipline sends at most 2 requests at a time to one site, and waits longer if the site's robots.txt asks it to.",
    "g.sitemap": "Also read the site's sitemap",
    "g.slowWait": "Waiting {n, plural, one {# second} other {# seconds}}: {host} asked Zipline to slow down.",
    "g.crawlDelay": "{host} asks for a pause between requests, so Zipline waits {n, plural, one {# second} other {# seconds}} between them.",
    "g.failedN": "{n, plural, one {# page couldn't be read} other {# pages couldn't be read}}",
    "g.failStatus": "The server replied {status} {text}", "g.failNet": "Couldn't connect",
    "g.failRobots": "The site's robots.txt couldn't be read ({why}), so the page was left alone",
    "g.jsNote": "{n, plural, one {This page builds its links with JavaScript. Your browser can open it and collect them.} other {# pages build their links with JavaScript. Your browser can open them and collect the links.}}",
    "g.collect": "Collect with the browser", "g.collectAgain": "Try again", "g.asking": "Asking…",
    "g.collectWaiting": "Waiting for the browser to open the page and read its links…",
    "g.collectDone": "{n, plural, one {# link} other {# links}} collected by the browser.",
    "g.collectFail": "The browser couldn't do that: {why}.", "g.collectFail.timeout": "it didn't answer in time", "g.collectFail.empty": "it found no links on the page",
    "g.collectFail.blocked": "it isn't allowed to read that page", "g.collectFail.other": "something went wrong",
    "g.noExt": "The Zipline browser extension isn't connected. Open your browser with the extension installed, then try again.",
    "g.signedIn": "Signed in, as in your browser", "g.signedHint": "Signed in, as in your browser: Zipline uses the sign-in your browser has for this site, and sends it to this site only.",
    "g.scans": "Earlier scans", "g.scanOpen": "Open", "g.resume": "Resume scan", "g.scanOpt": "{host} · {when} · {files, plural, one {# file} other {# files}} · {state}",
    "g.st.interrupted": "Interrupted", "g.scanDelTitle": "Delete scan", "g.scanDelBody": "Delete the saved scan of \"{name}\"? Downloads you already added stay in the list.",
    "g.leftN": "Stopped with {n, plural, one {# page} other {# pages}} still to scan.",
    "g.interrupted": "Zipline was closed during this scan. {n, plural, one {# page is} other {# pages are}} still to scan.",
    // ---- export, import, copy, command line, diagnostics
    "ex.title": "Export download list", "ex.json": "Zipline list (.json)", "ex.jsonHint": "Everything: names, folders, queues and progress. Import it on another PC, or keep it as a backup.",
    "ex.txt": "Links only (.txt)", "ex.txtHint": "One link per line, for other apps or for sharing.",
    "ex.some": "{n, plural, one {Only the selected download} other {Only the # selected downloads}}", "ex.someNone": "Only the selected downloads",
    "ex.secrets": "Include sign-in cookies and passwords", "ex.secretsHint": "Leave this off if you'll share the file — anyone with it could use your sign-ins.",
    "ex.save": "Save…", "ex.descTxt": "Text file", "ex.descJson": "Zipline list", "n.savedName": "Saved {name}", "n.savedTo": "Saved to {path}",
    "im.title": "Import download list", "im.file": "File", "im.none": "No file chosen", "im.choose": "Choose…",
    "im.fileHint": "A Zipline list (.json), a text file of links, or an .ef2 export from another download manager.",
    "im.addTo": "Add links to", "im.listOnly": "Download list only", "im.addToHint": "Zipline lists keep their own queues; this is for plain links.",
    "im.skipDup": "Skip links that are already in the list", "im.startQueue": "Start the queue after importing", "im.go": "Import", "im.importing": "Importing…",
    "im.tooBig": "That file is too large to be a download list.",
    "im.done": "{n, plural, one {Imported # download} other {Imported # downloads}}{skipped, plural, =0 {} other { · skipped # already in the list}}",
    "n.copied": "Copied.", "n.copyFail": "Couldn't copy: the browser didn't allow it. Select the text and press Ctrl+C instead.",
    "cli.hint": "Use these from a command prompt, a script or a shortcut. If Zipline is already running, the request goes to it.",
    "diag.title": "Diagnostics", "diag.hint": "Details that help when something goes wrong: the version, your settings and the recent log. Passwords, keys, cookies and the secret parts of links are left out.",
    "diag.log": "Show log file", "diag.fail": "Couldn't read the diagnostics.", "diag.copied": "Diagnostics copied.",
    // ---- moving downloads, a torrent's own limits and trackers
    "st.moving": "Moving… {pct}", "ctx.moveTo": "Move to folder…",
    "mv.title": "Move — {name}", "mv.titleMany": "Move {n, plural, one {# download} other {# downloads}}", "f.moveTo": "Move to folder",
    "mv.hint": "{kind, select, torrent {The torrent's whole folder moves, and it carries on from there. A torrent keeps its own names, so it can't move to a folder that already has one with the same name.} active {A download that is still going pauses while its files move, then carries on from the new folder.} many {A file whose name is taken in the new folder gets a number added. Downloads that are still going pause while they move, then carry on.} other {If a file with the same name is already there, the moved one gets a number added.}}",
    "mv.go": "Move", "mv.moving": "Moving…", "mv.needDir": "Choose the folder to move to.", "mv.btn": "Move…", "mv.tip": "Move the files to another folder",
    "mv.some": "{n, plural, one {# download was moved} other {# downloads were moved}}, but not all.",
    "mv.done": "{n, plural, one {Moved # download to {dir}} other {Moved # downloads to {dir}}}",
    "tp.limitDown": "Download limit", "tp.limitUp": "Upload limit", "tp.moreTitle": "Sharing and trackers", "tp.shareLbl": "Sharing",
    "tp.shareDefault": "Use my setting ({what})", "tp.shareNone": "stops as soon as it finishes", "tp.shareCustom": "Stop sharing at…", "tp.shareOff": "Stop as soon as it finishes",
    "tp.shareHint": "For this torrent only; it takes the place of the setting in Options → Torrents.",
    "tp.trkPh": "udp://tracker.example.org:6969/announce",
    "tp.trkHint": "One address per line, and a blank line between tiers. The trackers of a tier are tried in order, and the next tier only when they all fail; the one that answers moves to the front.",
    "tp.ts.waiting": "Waiting", "tp.ts.asking": "Asking…", "tp.ts.working": "Working", "tp.ts.standby": "Standby",
    "tp.ts.nodns": "Couldn't find the tracker", "tp.ts.noanswer": "No answer", "tp.ts.noconn": "Couldn't connect",
    "tp.ts.refused": "This tracker doesn't accept Zipline (client not whitelisted)", "tp.ts.says": "The tracker says: {text}", "tp.ts.error": "Error: {text}",
    "tp.trkWarn": "Warning from the tracker: {text}", "tp.trkReply": "It replied: {text}", "tp.trkRetry": "Trying again in {span}", "tp.trkTier": "tier {n}",
    "tp.editTrackers": "Edit trackers…",
    // ---- Batch, About, shortcuts, exit
    "batch.title": "Batch download", "batch.tabsAria": "How to list the links", "batch.tabList": "List of links", "batch.tabPat": "Numbered pattern",
    "batch.listPh": "One link per line", "batch.listAria": "Links, one link per line", "batch.pat": "Link with *", "batch.range": "From / to", "batch.from": "From", "batch.to": "To",
    "batch.toWord": "to", "batch.digits": "digits", "batch.digitsAria": "Digits", "batch.add": "Add to queue", "batch.adding": "Adding {n} of {total}…",
    "batch.errList": "Enter at least one http, https or ftp link, one per line.", "batch.errPat": "Use * where the number goes, in an http, https or ftp link, and a range of 5000 or fewer.",
    "batch.fail": "Added {done} of {total, plural, one {# link} other {# links}}, then it stopped: {error} Press the button again to add the rest.",
    "batch.done": "{n, plural, one {Added # link to {queue}} other {Added # links to {queue}}}",
    "batch.preview": "{n, plural, one {# link} other {# links}}: {first} … {last}", "batch.star": "Use * where the number goes.",
    "about.version": "Version {version} · multi-connection downloader",
    "about.p1": "Splits each file into up to 32 parallel connections, re-splits slow parts on the fly, and resumes after errors, restarts or expired links.",
    "about.p2": "Settings and the download list are stored in your user config folder under \"Zipline\".",
    "sc.openTorrent": "Open torrent file", "sc.selectAll": "Select all", "sc.openShow": "Open file / show progress", "sc.pauseResume": "Pause / resume selected", "sc.properties": "Properties",
    "sc.search": "Search", "sc.altF10": "Alt or F10", "sc.menuBar": "Menu bar", "sc.contextMenu": "Context menu", "sc.move": "Move in the list", "sc.extend": "Extend the selection",
    "sc.moveNo": "Move without selecting", "sc.toggle": "Select or unselect a row",
    "quit.body": "Downloads are in progress. They will be paused and can be resumed next time.", "quit.exit": "Exit", "quit.bye": "Zipline has exited. You can close this window.",
    "btn.loading": "Loading…",
    "n.couldnt": "Couldn't download {name}: {error}", "n.couldntRetry": "Couldn't download {name} — trying again in {wait}: {error}",
    "n.virus": "Virus scan warning — {text}", "n.queueDone": "Queue finished", "n.quotaUsed": "Data quota used up. Downloads wait until it starts again",
    "n.lost": "Lost connection to Zipline. Is the app still running?",
    "sum.matches": "<ok>✓ The file matches its {algo} checksum</ok>{has, select, y { <m>({src})</m>} other {}}.",
    "sum.mismatch": "<err>✗ The file doesn't match its {algo} checksum</err> <m>({src})</m>. It's damaged or a different version — {redownload} fetches it again.",
    "sum.gotAlgo": "The file's {algo}: <code>{got}</code>",
    "sum.pending": "{algo} <m>({src})</m> · {done, select, y {press Save to check the file} other {checked when the download finishes}}.{pieces, plural, =0 {} one { Damaged pieces are found with the metalink's # piece checksum and downloaded again.} other { Damaged pieces are found with the metalink's # piece checksums and downloaded again.}}",
    "sum.sha256": "SHA-256 of the file: <code>{got}</code>", "sum.pasteOr": "Paste the checksum from the download page to check the file, or {calc}", "sum.calc": "Calculate SHA-256",
    "sum.pasteOnly": "Paste the checksum from the download page and the file is checked when it finishes.", "sum.ifSite": "If the site publishes one (or you paste it), the file is checked when it finishes.",
    "o.pAddrPh": "proxy.example.com:8080  or  socks5://host:1080", "g.urlPh": "https://example.com/gallery/  or  ftp://host/folder/",
    "tb.commands": "Commands",
    // ---- signing the window in (locked.html) and the browser extension's link (extlink.js)
    "lock.title": "Open Zipline from its icon or menu",
    "lock.body": "This page is Zipline's window, and for your safety it only opens from Zipline itself: click Zipline's icon in the notification area, choose it in your apps menu, or start Zipline again.",
    "lock.cli": "If no window opens, run <code>zipline --login-url</code> and open the address it prints. It works once, within a minute.",
    "pair.title": "A browser extension wants to connect to Zipline",
    "pair.body": "It asks to send links, videos and pages to Zipline, and to open the pages Zipline needs for fresh links and site scans.",
    "pair.browserL": "Browser", "pair.versionL": "Version", "pair.extL": "Extension", "pair.unknown": "not given",
    "pair.known": "This is the ID of Zipline's own extension.",
    "pair.firefox": "Firefox gives each installed extension an address of its own, so Zipline can't recognise it from the address.",
    "pair.unknownId": "This isn't the ID of Zipline's own extension. Deny it unless you built the extension yourself.",
    "pair.warn": "Allow it only if you just installed or reset Zipline's browser extension. Any program on this computer can ask; the extension's native link, which Zipline sets up for you, never needs to ask.",
    "pair.allow": "Allow", "pair.deny": "Deny", "pair.allowed": "{browser} is connected", "pair.aBrowser": "The browser",
    "lnk.native": "<ok>Connected</ok> through the browser's native messaging link",
    "lnk.http": "<ok>Connected</ok> over <code>127.0.0.1:{port}</code> (paired)",
    "lnk.none": "Not connected — the extension hasn't checked in",
    "lnk.unpaired": "<warn>Waiting</warn> — the extension asked to connect and hasn't been allowed. Press Connect in its popup to ask again.",
    "lnk.old": "<err>Needs updating</err> — an older Zipline extension tried to connect. Install version {min} or later.",
    "lnk.extOld": "<err>Needs updating</err> — the extension ({ver}) is older than this app works with ({min} or later).",
    "lnk.appOld": "<err>Needs updating</err> — the extension needs Zipline {min} or later; this is Zipline {ver}.",
    "lnk.meta": "Extension {ext} · Zipline {app}{seen, select, none {} other { · last check-in {seen}}}",
    "lnk.metaNoExt": "Zipline {app} · works with the extension {min} or later",
    "lnk.port": "Zipline is using port {port} because another program has {usual}. The extension still connects through its native link, but not over HTTP until {usual} is free and Zipline restarts.",
    "lnk.pairs": "Browsers allowed over HTTP",
    "lnk.pairMeta": "{origin} · allowed {added}{seen, select, none {} other { · last used {seen}}}",
    "lnk.forgetTitle": "Forget this browser?", "lnk.forgetBody": "{name} will have to ask again before it can connect over HTTP.", "lnk.forget": "Forget",
    "scan.confirmTitle": "Run this program after every download?",
    "scan.confirmBody": "Zipline will run<br><code>{cmd}</code><br>on each file when it finishes downloading. Only go on if you trust this program.",
    "scan.confirmOk": "Use this program",
    // ---- the phone page (m.html): it reuses the shared words above, and these are its own
    "a.start": "Start", "a.stop": "Stop", "a.tryAgain": "Try again",
    "ph.pairTitle": "Pair with Zipline", "ph.pairIntro": "Scan the QR code Zipline shows on your computer, or type its code here.",
    "ph.codeAria": "Pairing code", "ph.pairBtn": "Pair this phone", "ph.pairing": "Pairing…", "ph.codeLen": "The code has 16 letters and numbers.",
    "ph.path": "Zipline → {options} → {phone}",
    "ph.waitTitle": "Waiting for your computer", "ph.waitBody": "Zipline on your computer is asking whether to let <b>{name}</b> connect. Press <b>{allow}</b> there.",
    "ph.askLate": "Your computer hasn't answered. Is someone at it, with Zipline open?",
    "ph.denied": "Your computer didn't allow this phone. To try again, make a new code in {path}.",
    "ph.challenge": "The site wants to check you're a person before it sends the file. Open the link in a browser on your computer.",
    "ph.noLive": "This is a live stream. Add it from Zipline on your computer, where you can choose how long to record.",
    "ph.hlsAuto": "Zipline picks the best quality. To choose the quality, audio or subtitles, add it from Zipline on your computer.",
    "ph.torPeers": "{n, plural, one {Connected to # peer} other {Connected to # peers}}{pct, select, none {} other { · {pct}}}… You can start now and choose files later.",
    "ph.torLooking": "Looking for peers that have this torrent. You can start now and choose files later.",
    "ph.moreOpts": "More options", "ph.renewed": "{name} carries on with this link", "ph.noFiles": "The file list isn't known yet. Try again in a moment.", "ph.filesSaved": "Files updated",
    "ph.limitBad": "Type a whole number of KB/s, or 0 for no limit", "ph.limitOff": "No speed limit for this download",
    "ph.step1": "On your computer, open {path}.", "ph.step2": "Tap <b>{add}</b>.", "ph.step3": "Scan the QR code with this phone's camera, or type the code above.",
    "ph.more": "More", "ph.show": "Show", "ph.tabAll": "All",
    "ph.offline": "Can't reach Zipline. Is the computer on, with Zipline running, and on this Wi-Fi?",
    "ph.none.all": "No downloads yet. Tap + to add a link.", "ph.none.unfinished": "Nothing unfinished.", "ph.none.finished": "Nothing finished yet.", "ph.none.errors": "No errors.",
    "ph.powerWait": "{text} when the other downloads finish", "ph.powerIn": "{text} in {wait}",
    "ph.quotaOver": "{period, select, month {This month's} other {Today's}} data quota is used up, so transfers wait until it starts again. You can carry on anyway from Zipline on the computer.",
    "ph.quota": "{period}: <b>{used}</b> of {limit}{hasNet, select, y { on {net}} other {}}{over, select, yes { · used up} other {}}",
    "ph.profile": "{name} profile", "ph.limit": "Limit {speed}", "ph.left": "{time} left",
    "ph.of": "{done} of {total}", "ph.savedOnPc": "saved on your computer", "ph.gone": "That download isn't in the list any more",
    "ph.tapRemove": "Tap again to remove (the file stays)", "ph.tapDelete": "Tap again: the part downloaded is deleted",
    "ph.removedKeep": "Removed from the list. The file stays on your computer.", "ph.removed": "Removed from the list", "ph.download": "Download",
    "ph.addIntro": "Paste a link or a magnet link. It downloads on your computer.", "ph.addPh": "https://… or magnet:?…", "ph.addAria": "Link or magnet link",
    "ph.addBad": "Paste a web link (http:// or https://) or a magnet link", "ph.addedNow": "Downloading on your computer", "ph.addedQueue": "Added to the queue",
    "ph.connected": "Connected to your computer.", "ph.profLabel": "Traffic profile{hasUntil, select, y { until {time}} other {}}",
    "ph.limiterOn": "Speed limiter on", "ph.limiterOff": "Speed limiter off", "ph.qStopped": "Stopped", "ph.unpair": "Unpair this phone",
    "ph.unpaired": "This phone is no longer paired. To use it again, pair it from {path}.",
    "ph.notPaired": "This phone isn't paired any more. Add it again in {path}.",
    "ph.badCode": "That code didn't work. Make a new one in {path} (a code works for 15 minutes).",
    "ph.wrong": "Something went wrong",
    // @@NEXT@@
  },
};

// ---------------------------------------------------------------- messages
const esc = s => String(s ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
// Msg is a message in both forms; its string value is the text.
class Msg { constructor(text, html) { this.text = text; this.html = html; } toString() { return this.text; } }
class Name { constructor(s) { this.s = String(s ?? ""); } }
const nm = s => new Name(s);
const raw = (html, text) => new Msg(text ?? String(html).replace(/<[^>]*>/g, ""), html);

let LANG = "en";
try { const l = String(navigator.language || "").toLowerCase(); LANG = MESSAGES[l] ? l : MESSAGES[l.split("-")[0]] ? l.split("-")[0] : "en"; } catch {}
// the page's own language and reading direction follow the catalogue (a right-to-left language flips the layout)
try {
  const loc = new Intl.Locale(LANG), info = loc.getTextInfo ? loc.getTextInfo() : loc.textInfo;
  document.documentElement.lang = LANG; if (info && info.direction) document.documentElement.dir = info.direction;
} catch {}
const msgSource = id => (MESSAGES[LANG] && MESSAGES[LANG][id]) ?? MESSAGES.en[id];

// parseMsg reads a message into strings and { arg, type, cases } parts; cases hold parts of their own.
function parseMsg(s, i = 0, inner = false) {
  const parts = []; let text = "";
  const flush = () => { if (text) { parts.push(text); text = ""; } };
  while (i < s.length) {
    const c = s[i];
    if (c === "\\") { text += s[i + 1] ?? ""; i += 2; }
    else if (c === "}" && inner) break;
    else if (c === "{") {
      flush();
      let j = i + 1;
      const skip = () => { while (s[j] === " ") j++; };
      const word = () => { const k = j; while (j < s.length && !",{} ".includes(s[j])) j++; return s.slice(k, j); };
      skip(); const arg = word(); skip();
      if (s[j] === "}") { parts.push({ arg }); i = j + 1; continue; }
      j++; skip(); const type = word(); skip();
      if (s[j] === ",") j++;
      const cases = {};
      for (;;) {
        skip();
        if (s[j] === "}" || j >= s.length) break;
        const sel = word(); skip();
        if (!sel || s[j] !== "{") throw new Error("bad message: " + s);
        const body = parseMsg(s, j + 1, true); cases[sel] = body.parts; j = body.end + 1;
      }
      parts.push({ arg, type, cases }); i = j + 1;
    } else { text += c; i++; }
  }
  flush();
  return { parts, end: i };
}
const PARSED = new Map();
const parsedMsg = src => { let p = PARSED.get(src); if (!p) PARSED.set(src, p = parseMsg(src).parts); return p; };

let plainNumFmt = null, pluralRules = null;
const fmtNum = n => (plainNumFmt ||= new Intl.NumberFormat(LOCALE)).format(n);
const TAG = /&lt;(\/?(?:b|i|code)|br)&gt;/g, SPAN = /&lt;(\/?)(ok|warn|err|m|long)&gt;/g;
const SPAN_CLASS = { ok: "tn-ok", warn: "tn-warn", err: "tn-err", m: "tn-m", long: "sb-long" };
const literalText = (s, html) => html ? esc(s).replace(TAG, "<$1>").replace(SPAN, (_, c, k) => c ? "</span>" : `<span class="${SPAN_CLASS[k]}">`) : s.replace(/<br>/g, "\n").replace(/<\/?(?:b|i|code|ok|warn|err|m|long)>/g, "");
function valueText(v, html) {
  if (v instanceof Name) return html ? "<bdi>" + esc(v.s) + "</bdi>" : v.s;
  if (v instanceof Msg) return html ? v.html : v.text;
  if (typeof v === "number") return fmtNum(v);
  return v == null ? "" : html ? esc(v) : String(v);
}
function renderMsg(parts, vars, html, n) {
  let out = "";
  for (const p of parts) {
    if (typeof p === "string") out += literalText(n === undefined ? p : p.split("#").join(fmtNum(n)), html);
    else if (p.type === "plural") {
      const x = +vars[p.arg], key = "=" + x in p.cases ? "=" + x : (pluralRules ||= new Intl.PluralRules(LOCALE)).select(x);
      out += renderMsg(p.cases[key] || p.cases.other || [], vars, html, x);
    } else if (p.type === "select") out += renderMsg(p.cases[String(vars[p.arg])] || p.cases.other || [], vars, html, n);
    else out += valueText(vars[p.arg], html);
  }
  return out;
}
function t(id, vars = {}) { const s = msgSource(id); return s == null ? id : renderMsg(parsedMsg(s), vars, false); }
function th(id, vars = {}) { const s = msgSource(id); return s == null ? new Msg(id, esc(id)) : new Msg(renderMsg(parsedMsg(s), vars, false), renderMsg(parsedMsg(s), vars, true)); }
const ht = (id, vars) => th(id, vars).html;
const te = (id, vars) => esc(t(id, vars)); // t() escaped, for an attribute or text inside markup
// asHtml and put show a string (escaped) or a Msg (its markup) on the screen.
const asHtml = v => v instanceof Msg ? v.html : esc(v);
function put(node, v) { if (v instanceof Msg) node.innerHTML = v.html; else node.textContent = v; }

// ---------------------------------------------------------------- numbers, sizes, times, dates
const numberFormats = new Map();
// numberFormat makes (and keeps: they are slow to make) a formatter for a unit or percent with fixed digits.
function numberFormat(style, unit, digits) {
  const k = style + unit + digits;
  let f = numberFormats.get(k);
  if (!f) numberFormats.set(k, f = new Intl.NumberFormat(LOCALE, { style, unit: unit || undefined, unitDisplay: "short", minimumFractionDigits: digits, maximumFractionDigits: digits, useGrouping: false }));
  return f;
}
function unitText(n, unit, digits) {
  const own = msgSource("unit." + unit);
  return numberFormat("unit", unit, digits).formatToParts(n).map(p => p.type === "unit" && own != null ? own : p.value).join("");
}
const fmtPct = (p, digits) => numberFormat("percent", "", digits).format(p / 100); // (p is 0 to 100)
const SIZE_UNITS = ["byte", "kilobyte", "megabyte", "gigabyte", "terabyte"];
function fmtSize(b, perSecond) {
  if (b == null || b < 0) return t("size.unknown");
  let i = 0, v = b;
  while (v >= 1024 && i < SIZE_UNITS.length - 1) { v /= 1024; i++; }
  return unitText(v, SIZE_UNITS[i] + (perSecond ? "-per-second" : ""), i === 0 ? 0 : v < 10 ? 2 : v < 100 ? 1 : 0);
}
const fmtSpeed = (s, zero) => s >= 1 ? fmtSize(s, true) : zero ? fmtSize(0, true) : "";
const fmtKBps = kb => unitText(kb, "kilobyte-per-second", 0);
const fmtRatio = r => numberFormat("decimal", "", r >= 10 ? 0 : 2).format(r);
const fmtMbps = bps => unitText(bps / 1e6, "megabit-per-second", bps < 1e7 ? 1 : 0);
// unitsText writes a duration as its units in a row: "3 min 20 sec". A number stays with its unit
// (a non-breaking space), so a line breaks between the units, never inside "2 hr".
let unitList = null;
const unitsText = list => (unitList ||= new Intl.ListFormat(LOCALE, { style: "narrow", type: "unit" })).format(list.map(([u, n]) => unitText(n, u, 0).replace(/ /g, "\u00a0")));
function fmtTime(sec) {
  if (!isFinite(sec) || sec < 0 || sec > 99 * 86400) return ""; // (a stalled download's guess means nothing)
  sec = Math.round(sec);
  if (sec < 60) return unitsText([["second", sec]]);
  const m = Math.floor(sec / 60), s = sec % 60;
  if (m < 60) return unitsText([["minute", m], ["second", s]]);
  const hh = Math.floor(m / 60);
  if (hh < 48) return unitsText([["hour", hh], ["minute", m % 60]]);
  return unitsText([["day", Math.floor(hh / 24)]]);
}
function fmtIn(sec) {
  if (!isFinite(sec) || sec < 60) return t("time.underMinute");
  const m = Math.round(sec / 60);
  if (m < 60) return unitsText([["minute", m]]);
  const h = Math.floor(m / 60);
  return h < 48 ? unitsText(m % 60 ? [["hour", h], ["minute", m % 60]] : [["hour", h]]) : unitsText([["day", Math.round(h / 24)]]);
}
// fmtWait: "25 sec", "3 min" — how long until something happens.
const fmtWait = sec => sec < 60 ? unitsText([["second", Math.ceil(sec)]]) : unitsText([["minute", Math.ceil(sec / 60)]]);
// (date formatters are slow to make, so each is made once)
const DATE_FMT = new Intl.DateTimeFormat(LOCALE, { month: "short", day: "numeric", year: "numeric" });
const TIME_FMT = new Intl.DateTimeFormat(LOCALE, { hour: "2-digit", minute: "2-digit" });
function fmtDate(iso) {
  if (!iso || iso.startsWith("0001")) return "";
  const d = new Date(iso);
  return DATE_FMT.format(d) + " " + TIME_FMT.format(d);
}
// fmtAgo says how long ago a time was: "just now", "5 min ago", "3 h ago", then the date.
function fmtAgo(iso) {
  if (!iso || iso.startsWith("0001")) return t("ago.never");
  const s = (Date.now() - new Date(iso)) / 1000;
  return s < 90 ? t("ago.justNow") : s < 3600 ? t("ago.min", { n: Math.round(s / 60) }) : s < 86400 * 2 ? t("ago.hr", { n: Math.round(s / 3600) }) : fmtDate(iso);
}
const DAY_FMT = new Intl.DateTimeFormat(LOCALE, { month: "short", day: "numeric" });
const fmtDay = d => DAY_FMT.format(d); // ("Mar 5")
const fmtClock = d => TIME_FMT.format(d); // ("09:30")
// fmtSecs: "25 sec" (a countdown)
const fmtSecs = n => unitsText([["second", n]]);
// The free data hours (the Free data hours queue, plan.go), said the same in the window and on the phone.
const PLAN_Q = "freehours";
const hhmm = when => TIME_FMT.format(new Date(when));
function whenText(when) {
  const d = new Date(when), now = new Date();
  const days = Math.round((new Date(d.toDateString()) - new Date(now.toDateString())) / 864e5);
  return days === 0 ? t("when.today", { time: hhmm(d) }) : days === 1 ? t("when.tomorrow", { time: hhmm(d) }) : t("when.day", { day: weekdayName(d), time: hhmm(d) });
}
function planWindowText(p) {
  if (!p || !p.on) return "";
  return p.in ? t("plan.nowUntil", { time: hhmm(p.to) }) : t("plan.window", { from: whenText(p.from), to: hhmm(p.to), wait: fmtIn((new Date(p.from) - Date.now()) / 1000) });
}
// listOf joins items ("A, B and C") into a Msg; an item may be a string or an nm() name.
const itemMsg = x => x instanceof Name ? raw("<bdi>" + esc(x.s) + "</bdi>", x.s) : raw(esc(x), String(x));
function listOf(items) {
  const parts = items.map(itemMsg);
  if (parts.length < 2) return parts[0] || raw("", "");
  const comma = t("list.comma"), rest = parts.slice(0, -1).reduce((a, b) => raw(a.html + esc(comma) + b.html, a.text + comma + b.text));
  return th("list.and", { rest, last: parts[parts.length - 1] });
}
const WEEKDAY_FMT = new Intl.DateTimeFormat(LOCALE, { weekday: "short" });
const weekdayName = d => WEEKDAY_FMT.format(d); // ("Mon")
const WEEKDAYS = Array.from({ length: 7 }, (_, i) => weekdayName(new Date(2024, 0, 7 + i))); // Sunday first, like Date.getDay()
