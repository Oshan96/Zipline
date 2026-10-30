# Zipline user guide

Everything Zipline does, and how to use it. New here? Start with [Install](#install) and [Getting started](#getting-started).

- [Install](#install)
- [Getting started](#getting-started)
- [Why it's fast](#why-its-fast)
- [Downloads in detail](#downloads-in-detail)
- [Video and streams](#video-and-streams)
- [Torrents and magnet links](#torrents-and-magnet-links)
- [Cloud share links](#cloud-share-links)
- [Queues and scheduling](#queues-and-scheduling)
- [Speed limits and data plans](#speed-limits-and-data-plans)
- [Browser extension](#browser-extension)
- [Control from your phone](#control-from-your-phone)
- [Safety checks](#safety-checks)
- [The window](#the-window)
- [Command line](#command-line)
- [Where data is kept](#where-data-is-kept)
- [Security](#security)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## Install

Download Zipline from the [latest release](https://github.com/Oshan96/Zipline/releases/latest). Every download carries the browser extension and this guide.

### Windows 10 and 11

- **Installer (recommended):** run `Zipline-Windows-Setup.exe`. It puts Zipline in your user folder (no administrator rights needed), picks the x64 or Arm64 build for your PC, and adds a Start menu shortcut (a desktop one too, if you like) and an entry in **Settings → Apps** for uninstalling. Running a newer Setup updates Zipline in place.
- **Portable:** unzip `Zipline-Windows-x64.zip` (or `Zipline-Windows-arm64.zip`) anywhere and run `Zipline.exe` from there.

Zipline isn't code-signed yet, so the first time SmartScreen may show "Windows protected your PC". Click **More info → Run anyway**.

### macOS 11 or later

- **Disk image (recommended):** open `Zipline-macOS.dmg` and drag Zipline to Applications. It runs on Apple silicon and Intel Macs. The image also holds the browser extension: copy its **Browser extension** folder somewhere it can stay (your Documents folder, say) before you add it to your browser.
- **Zip:** `Zipline-macOS-apple-silicon.zip` or `Zipline-macOS-intel.zip` hold `Zipline.app` and the extension.

The app isn't signed with an Apple Developer ID or notarized yet, so the first time macOS says it can't check Zipline for malicious software. Open **System Settings → Privacy & Security** and click **Open Anyway** next to the message about Zipline (on macOS 14 and earlier, right-click the app → **Open** works too). Or run `xattr -dr com.apple.quarantine /Applications/Zipline.app` in Terminal.

### Linux (x64)

- **Debian, Ubuntu and their relatives:** `sudo apt install ./Zipline-Linux-x64.deb`. Zipline appears in your apps menu, and `zipline` starts it from a terminal. The browser extension is in `/usr/share/zipline/extension` (for Firefox: `/usr/share/zipline/extension-firefox` and `zipline-firefox.xpi`). `sudo apt remove zipline` uninstalls it.
- **Fedora, openSUSE and their relatives:** `sudo dnf install ./Zipline-Linux-x64.rpm` (on openSUSE, `sudo zypper install --allow-unsigned-rpm ./Zipline-Linux-x64.rpm`). It puts everything in the same places as the `.deb`, and `sudo dnf remove zipline` (or `sudo zypper remove zipline`) uninstalls it.
- **Any distribution:** unpack `Zipline-Linux-x64.tar.gz`, then `chmod +x zipline-linux-x64 && ./zipline-linux-x64`.

Install `secret-tool` (the `libsecret-tools` package on Debian and Ubuntu, `libsecret` on Fedora) so the key that protects saved passwords is kept in your desktop's password store (see [Security](#security)).

### Uninstall

- **Windows:** the installer's copy uninstalls from **Settings → Apps**. That removes the program, its shortcuts and its registrations for magnet links, `.torrent` files and the browser extension. It keeps the settings and download list unless you choose to remove them; files you downloaded are never touched. A portable copy is just its folder.
- **macOS:** move Zipline from Applications to the Bin. Settings stay in `~/Library/Application Support/Zipline`.
- **Linux:** `sudo apt remove zipline` or `sudo dnf remove zipline`, or delete the unpacked folder. Settings stay in `~/.config/Zipline`.

## Getting started

### The window

The window opens as an app window in Edge, Chrome or Brave. It uses its own private browser profile, so your normal browser's downloads, extensions and sign-in prompts never show up inside Zipline.

On Windows, Zipline also puts an icon in the notification area (system tray). Click it to open Zipline; right-click it to add a link, start all queues, pause all downloads, switch the speed limiter or the traffic profile, cancel a "when finished" countdown or exit. When the window is closed, the icon shows progress in its tooltip and pops up a note when a download finishes: one note for downloads that finish within a few seconds of each other, and for a queue (a batch, say) one when the whole queue has finished, not one per file. You can hide the icon in Options → General. Closing the window leaves Zipline running either way; run the program again to bring the window back. To quit, use **Tasks → Exit Zipline** or the tray menu.

Start it in the background with no window (for example from Startup apps): `Zipline.exe --nowindow`.

The window only opens from Zipline itself (its icon, its menu, or starting it again). If no window appears, `zipline --login-url` prints a one-time address to open in any browser; it works once, within a minute.

### Adding downloads

- Click **Add link**, paste a link (Ctrl+V) or drag and drop one onto the window.
- **Clipboard monitor:** copy a link to a file and Zipline offers to download it.
- **Batch download:** a list of links, or a numbered pattern such as `img*.jpg` from 1 to 50, into any queue.
- **From the browser:** the [browser extension](#browser-extension) adds **Download with Zipline** to links, images and videos, and can take over the browser's own downloads.
- **Downloading starts while you choose where to save.** As soon as the Add dialog has read the file's details, Zipline starts fetching it. Pick another folder or name and the part already downloaded moves with it. Download later keeps it, ready to resume, and Cancel deletes it. You can switch this off in Options → General.

Downloads pause and resume, including after closing the app or rebooting (when the server supports resuming). **Categories** (Compressed, Documents, Music, Programs, Video, Images) sort files into their own folders.

## Why it's fast

- **Up to 32 parallel connections per file.** The default is 16; change it in Options → Connection. A download starts with up to 8 and doubles them while that makes it faster, so a server that caps each connection's speed gets them all within a couple of seconds, and a fast one isn't flooded.
- **Dynamic re-splitting.** When a connection finishes, it takes half of the part with the longest time left, as long as that half would take more than about 2 seconds (as browsers do). A slow connection gets cut down to a small slice and a fresh connection takes the rest, so one bad connection can't hold up the end of a download.
- **Connections are reused.** Connections go back to a pool instead of being thrown away, and secure (TLS) sessions are resumed, so a new connection doesn't pay for a full handshake.
- **HTTP/1.1 on separate TCP connections.** HTTP/2 would put all the parts on one connection.
- **No extra connect step.** The first request also streams the first part of the file.
- **No merge step at the end.** Parts are written straight into one file. Its space is reserved up front on Linux; on Windows the file is sparse while it downloads, so NTFS doesn't stall filling it with zeros, and becomes a normal file when it's finished (so an ISO or VHD mounts).
- **Retries that survive an outage.** Each connection retries after 1, 2, 4 … up to 60 seconds, and a stalled one is reset after 30 s. One that drops after making progress reconnects straight away. While the computer is offline, waiting doesn't use up tries, and everything tries again at once when the network comes back or the computer wakes up. If every connection is lost, the download tries again by itself for about 12 minutes (the list shows "Retrying in …"). A server's "come back later" (Retry-After) is respected, up to an hour.
- **Several servers at once.** With mirrors (typed in, from a metalink, or listed by the server), each connection goes to whichever server is least busy for its speed. A server that serves a different file is dropped, and one that keeps failing is rested.
- **Learns the server's connection limit.** When a server turns away extra connections, Zipline settles on the number it accepts instead of retrying them, and tries one more now and then. Downloads from the same server share one set of connections fairly, and Options → Connection → Downloads at once caps how many downloads you start by hand run together (queues have their own number).

## Downloads in detail

### Links that expire carry on by themselves

Many sites give out download links that only work for a while. When one runs out part-way, Zipline gets a fresh link and carries on where it stopped:

- a link behind a redirect (GitHub releases, most CDNs) is renewed through the link it came from, with nothing for you to do;
- otherwise the browser extension opens the page the download came from in a background tab for a moment, finds the same file (same size and name), and closes the tab again (Options → Browser, on by default);
- and if that can't work, click the download link again on the page or paste it in the Add dialog: Zipline recognises the file and carries on the paused download instead of starting a new one. Right-click → Refresh download link… does this on purpose, and Properties takes a new link too.

### Checksums

Zipline checks a finished file against the checksum you paste in the Add dialog (MD5, SHA-1, SHA-256 or SHA-512, or a line from a checksum file), one the server sends, or one in a `SHA256SUMS`-style file next to the download (Options → File checks). Files that match show *Complete · verified*. One that doesn't is flagged *damaged* and Redownload fetches it again. For any finished file, Properties can calculate its SHA-256 or check it against one you paste.

### Mirrors and metalinks

Give a download other links for the same file (Add dialog → Checksum and mirrors, or Properties) and it downloads from all of them at once; the progress window shows what each server sent. Open a metalink (`.meta4` or `.metalink`) and Zipline takes its mirrors and checksums, and keeps the folders its file names have (`iso/x86/disk.iso` is saved in those folders under the one you pick; nothing can point outside it). Mirrors go by the metalink's priority, those in your country first (worked out from the computer's time zone or region).

With piece checksums, each piece is checked as soon as it's written, with or without a checksum for the whole file: a damaged piece is downloaded again from another server, a server that sends three damaged pieces is dropped (the progress window says so), and after a restart the pieces that were complete but not yet checked are checked first. A metalink that lists a torrent is downloaded with BitTorrent, its HTTP mirrors serving as web seeds. Servers that list mirrors in their replies (Metalink/HTTP, with their `pri` and `geo`) are used automatically.

### HTTP, HTTPS, FTP and FTPS

FTP downloads also use several connections and can resume. Zipline uses FTP over TLS when the server offers it (and `ftps://` links), finds files relative to the folder your account starts in, and uses fewer connections when the server says it has too many. Put the login in the link (`ftp://user:pass@host/file`) or in the Add dialog's login fields.

### Logins, cookies and site rules

- **Logins only where they're asked for.** A saved login (Options → Sites, the Add dialog or the link) is sent only when the server asks for one, with Digest when the server offers it. A plain-text (Basic) password goes only over HTTPS or to a computer on your own network. Cookies a site sets on the way to the file are kept for that download, like a browser does. Login, referer and cookies can be set for protected downloads.
- **Site rules** (Options → Sites). For each site you can limit its connections, for servers that block or slow down clients using many, and save a login that's sent automatically to that site (HTTP and FTP). A rule can cover a whole site (`example.com` includes its sub-sites) or just part of it (`example.com/members`), and the most specific rule wins. Saved passwords are kept encrypted (see [Security](#security)) and the window never gets them back: a saved one shows as "(saved)", typing replaces it and the × beside it forgets it.

### File names like a browser's

Names the server gives are read as browsers read them (including international names and badly formed headers), and names Windows can't use or that could run something (`CON`, `.lnk`, `.url`) are made safe.

### Move to folder

Right-click → Move to folder… (or Move… in Properties) moves a download's files to another folder, finished or not: one in progress pauses, moves and carries on, a torrent keeps sharing from its new place, a video keeps its subtitles and parts beside it. Moving to another drive copies first and removes the original only once the copy is complete, and a file already there is never overwritten.

### Proxies

Zipline follows your computer's proxy settings by default, including setup scripts (PAC files) and automatic detection, on Windows, macOS and Linux (GNOME or KDE settings, or the usual environment variables). When a proxy in the list doesn't answer, the next one is used. You can also set an HTTP or SOCKS5 proxy with a login and a "don't use for" list in Options → Proxy, with a Test button. FTP downloads go through a SOCKS5 proxy.

### Site grabber

Give it a start page (or an FTP folder), how many levels of links to follow, whether to stay on the site, domain or folder, and which file types you want. It respects robots.txt by default, shows what it finds as it scans, and lets you tick files before downloading them into any queue, optionally keeping the site's folder structure. Save it as a project to run again, or set it to re-check every few hours and queue only new files.

- It crawls politely (RFC 9309): at most 2 requests at a time to one server, spaced by the site's Crawl-delay or your **Wait between requests**, whichever is longer, with a little jitter; a server that says "too many requests" (429 or 503) is left alone for as long as it asks while other sites carry on. robots.txt rules are read as search engines do (`*` and `$`, the longest rule wins); when robots.txt itself can't be read because of a server error, the site isn't crawled until it can. Pages that fail after a few tries are listed with the reason ("3 pages couldn't be read").
- It finds more: links with no telling ending (`download.php?id=7`) are checked by their headers (a page is followed, a file is named from Content-Disposition), the site's sitemaps are read, and so are stylesheets (images, fonts), `og:video`/`og:image` tags and JSON-LD `contentUrl`s. Tracking and session parameters (`utm_*`, `fbclid`, `PHPSESSID`, `;jsessionid=`…) are ignored when telling links apart.
- A page that builds its links with JavaScript is pointed out, and **Collect with the browser** has the extension open it in a background tab and send back the links it shows (it starts at once through the extension's native messaging link; otherwise it can take up to 30 seconds, the time the extension takes to check in).
- Scans are saved: one that was cut off (Zipline closed or crashed) shows "Zipline was closed during this scan" with **Resume scan**; the last 20 are kept.
- **Scan this site** in the extension's menu or popup starts a scan signed in as you are in the browser: its cookies for that site go with the scan's requests to that site only, and to the files downloaded from it.

### Export and import

The Tasks menu exports the download list to a Zipline list (.json, with queues and progress) or plain links (.txt). Sign-in cookies are left out unless you tick the box. Import Zipline lists, text files of links, or .ef2 exports from other download managers.

## Video and streams

The browser extension puts a "Download video" button on video players and lists the videos it finds in its popup, one entry per video. Zipline downloads plain video files and both common streaming formats, HLS (`.m3u8`) and MPEG-DASH (`.mpd`). It lets you pick the quality, fetches the pieces on up to 12 connections, decrypts standard AES-128 HLS streams and resumes if interrupted. YouTube and DRM-protected services are excluded: Zipline turns their links down wherever they come from (the Add dialog, the extension, the command line, copied links, a site grab, the phone page) with "Zipline doesn't download from YouTube or DRM-protected streaming services".

### MP4 output, with nothing extra to install

When the download finishes, the pieces are repackaged into one regular `.mp4`. Nothing is re-encoded, so there's no quality loss, and a 1 GB video takes a few seconds.

- Streams that arrive as `.ts` pieces work with H.264 and HEVC video and AAC, MP3, AC-3 or E-AC-3 audio, including streams with ad breaks or quality changes part-way through.
- Streams that arrive as MP4 pieces become an ordinary MP4 that any player or editor opens.
- Streams that keep their audio in a separate track have it fetched too and merged in, in sync. This is how most DASH streams work.
- DASH streams in WebM (VP9 or AV1 video with Opus audio) become one `.webm` file with a seek index.
- Audio-only streams become `.m4a` (`.opus` for Opus, `.mka` for what MP4 can't hold). To keep the original `.ts` pieces instead, name the file `.ts` in the Add dialog.
- The rare stream that uses a format MP4 can't hold here (MPEG-2 video or DTS audio, for example) is saved as `.ts`, which plays in VLC. If a separate audio track can't be merged, it's saved next to the video. If [ffmpeg](https://ffmpeg.org) happens to be installed, Zipline uses it for those rare formats, but it's never required.

### Choosing a stream's tracks

Qualities are listed in the Add dialog in plain words ("1080p · 60 fps · H.264 · 6.2 Mbps", "4K · HDR · HEVC · 16 Mbps"), so H.264 and HEVC at the same bitrate can be told apart; the best non-HDR one is picked (HDR only when you choose it), or **Audio only** for the sound alone.

- Audio tracks are listed by language (in your language's words), name, channels ("5.1") and an "audio description" mark. Your own language is picked when the stream has it, then the stream's default. **Keep all languages** puts every audio track in the file, each labelled with its language and name, the chosen one first and playing by default.
- Subtitles (WebVTT, timed to the video through HLS's X-TIMESTAMP-MAP) are saved next to the video as `.srt` (`Movie.en.srt`, `Movie.en.forced.srt`, `Movie.en.sdh.srt`, the names Plex, Jellyfin, Kodi and VLC look for) and also put inside it: as text tracks (tx3g) in MP4, as WebVTT in Matroska and WebM. Your language is picked when there is one.
- The file type follows the tracks: MP4 when they all fit, WebM for VP8/VP9/AV1 with Opus or Vorbis, otherwise Matroska (`.mkv`), for example VP9 video with AAC sound.

### Ad breaks and changes part-way

A stream whose timestamps or format change part-way (`#EXT-X-DISCONTINUITY`, a new init section, several DASH periods) is joined into one file that plays straight through; a size change stays in the same track, and a part in another codec (an ad in H.264 inside an HEVC programme) is saved as a file of its own beside it. Breaks the stream marks as ads (SCTE-35 in DASH periods or HLS `#EXT-X-DATERANGE`, `#EXT-X-CUE-OUT`/`CUE-IN`) can be left out with **Leave out ad breaks**; it's off by default, since the marks aren't always right and a part left out can't be put back.

### Streams as robust as files

Every piece is checked before it's kept: an error page, a JSON message or anything that isn't video or sound (MPEG-TS, MP4/CMAF, WebM, AAC, MP3, AC-3) is fetched again, like a dropped connection, and a cut-off piece keeps only its whole frames. Pieces get the same retries as ordinary downloads (about 10 each, waiting longer each time, paused while you're offline), the server's Retry-After, its connection limit (fewer pieces at once when it turns extra ones away) and the whole-download retry. When the finished pieces hold noticeably less than the playlist promises (more than a second, or 1%), the download fails with "the video stopped early: got 41:10 of 48:32" instead of calling a short video complete.

- Signed links that run out part-way: the playlist is loaded again for fresh piece links; when the playlist's own link has run out, the browser extension finds a fresh one, or you click the video's link again, and the download carries on where it stopped.
- A download remembers the quality it started with (resolution, codecs, frame rate and a fingerprint of its first piece), so carrying on never mixes two qualities, even when the playlist is made again with other bitrates. When that quality is gone, it says so ("use Redownload").
- SAMPLE-AES streams with an ordinary key (H.264, AAC, AC-3 and E-AC-3 in MPEG-TS) are decrypted. FairPlay, Widevine and PlayReady are DRM and stay refused.

### Recording live streams

A live HLS stream is recorded as it plays, starting a little behind live (an event stream can also be recorded from its beginning), each piece written as it arrives so even a cut-off recording plays. It stops when the stream ends, at an optional **Stop after** time, when the stream stops updating, or when you press Pause, and then becomes one MP4. The list shows "Recording · 12:34".

## Torrents and magnet links

Paste a magnet link, drop a `.torrent` file on the window, or use Tasks → Open torrent file… (Ctrl+O). The Add dialog lists the files so you can tick just the ones you want; for a magnet link the list arrives from other peers while the dialog is open (you can start before it does and choose later in Properties). Zipline then:

- finds peers through the torrent's trackers (HTTP and UDP), the DHT (over IPv6 too; Zipline announces its torrents there and, once other nodes can reach it, answers for them as well), peers that pass on others they know (Zipline passes on its own too), and web servers listed in the torrent;
- encrypts its connections when the other peer can, so networks that slow BitTorrent down can't tell, and peers that insist on encryption let it in (Options → Torrents → Encryption can require it or turn it off);
- connects over uTP as well as TCP on the one listening port (uTP backs off when your line is busy, so sharing doesn't slow your browsing), and asks your router to open that port (UPnP, NAT-PMP or PCP) so peers on the internet can connect in. Options → Torrents says whether it worked, and warns when your provider shares one address among many customers, which no router can get round;
- asks trackers politely (BEP 12): the trackers of a tier in turn, the next tier only when they all fail, longer and longer waits for one that keeps failing, and their warnings shown in the progress window ("This tracker doesn't accept Zipline", say). Trackers can be added, removed and put in tiers in Properties;
- downloads the rarest pieces first and checks every piece as it arrives, in memory before it's written, so damaged data is fetched again from someone else and the peer that sent it is shut out. Each peer gets as many requests as it can serve in about three seconds, the last pieces are asked of several peers at once (the others are told to cancel), and at most 250 connections are open across all torrents;
- fetches the files you give a higher priority first (Skip, Low, Normal or High, in the Add dialog and Properties), and can download in order, each file's first and last pieces first, so you can watch or listen while it downloads (right-click or Properties);
- checks the files when it has to: a torrent added over files already in its folder keeps the pieces that are right, a start after a crash reads back only the files that changed, and right-click → Check files (or Redownload) reads everything back and fetches only what's damaged. Options → Torrents can have every torrent checked again when it finishes;
- shares what it has while downloading and, by default, keeps sharing a finished torrent until you've uploaded as much as you downloaded or for two hours of sharing (Options → Torrents), then stops by itself. Right-click → Stop sharing ends it sooner, and Share again (or Resume) starts a finished torrent sharing once more, for the same ratio or time counted from then. It looks at the files first, and says so instead of sharing when one is missing or has changed. At most five torrents share at once (you choose): beyond that, the ones that have shared longest stop first. Sharing pauses on a metered connection, while the traffic profile doesn't share, and outside the free hours for torrents from the Free data hours queue, and carries on afterwards;
- doesn't let a dead torrent hold up its queue: one downloading under 2 KB/s for a minute keeps trying in the background while the queue's next download starts, and a magnet link that hasn't got its file list from other peers after 15 minutes (you choose) stops with an error, so the queue can finish.

The progress window shows the peers (a lock for an encrypted connection, a uTP mark), trackers, files and a map of the pieces. Properties has the magnet link, lets you change which files to download and their priorities, and sets this torrent's own download and upload limits and when it stops sharing. Options → Torrents sets the upload limit, peers per torrent, the listening port, whether to use the DHT and protocol encryption. On Windows and Linux it can also make Zipline the app that opens magnet links and `.torrent` files. With the browser extension, clicking a magnet link or a `.torrent` file opens it in Zipline even while other downloads stay in the browser.

## Cloud share links

Paste a share link from Google Drive, Dropbox, OneDrive, MEGA, Yandex Disk, pCloud, MediaFire or WeTransfer (or copy it, if clipboard watching is on) and Zipline downloads the file itself, not the web page around it: the Add dialog says where it's from, with the file's real name and size. Big Google Drive files get past the "can't scan for viruses" page by themselves, and older Drive links that need their resource key keep working. Google Docs, Sheets and Slides come as Word, Excel and PowerPoint files. The download keeps the share link, so it can resume later even though the link the service hands out doesn't last. When a service refuses (a file over its download quota, removed, private or behind a password, a Dropbox account whose links were switched off for too much traffic), the download fails with the service's own words instead of saving its web page.

- **Shared folders** from Google Drive, OneDrive, MEGA, Yandex Disk and pCloud are listed in the Add dialog like a metalink's files: download all of them (into a folder named after the shared one, its folders kept) or pick one.
- **MEGA** files are decrypted as they arrive, on several connections at once, and checked against the MAC in their link when they finish; a file damaged on the way fails instead of being kept.
- **Checksums the services publish** are checked when the file finishes: OneDrive's QuickXorHash, the SHA-256 Yandex Disk and MediaFire give.

## Queues and scheduling

- **Multiple queues.** Each queue has its own "downloads at a time", schedule (start time, stop time, days, start again every N minutes or hours) and "when finished" action. The action (such as turning the computer off) runs only when everything in the queue downloaded; if some files failed, Zipline says so and leaves the computer on.
- **A scheduler that survives sleep.** Schedules go by real dates, not by watching the clock tick past a minute. A start missed while the computer slept or Zipline was closed runs as soon as it can, once, if it's still before the stop time (or within 12 hours without one). Each start runs exactly once, even across restarts and on the nights the clocks change, a window can run past midnight, and "start again every N minutes" stays inside the window. On Windows a queue can also wake the computer for its start (if Power Options allow wake timers).
- **Failed downloads try again.** Each queue tries a failed download again a few times (3 times, 5 minutes apart, by default), moving it to the end of the queue, and its list status says when. Errors that won't go away by themselves, such as a missing file, a refused password or a full disk, aren't retried. The queue isn't "finished" while a file waits for another try.
- **When finished.** A queue can exit Zipline, put the computer to sleep, hibernate it (where the computer supports it) or turn it off, and **Tasks → When all downloads finish** does the same once, for everything. Zipline first waits for other queues and downloads, then counts down for a minute so you can cancel: a dialog in the window, a notice on the phone, a balloon and a menu item from the tray icon. With nobody there, it goes ahead.
- **Synchronization queues.** Finished files stay in the queue. Each run re-checks them on the server (size, ETag or date) and downloads the ones that changed, replacing the old file.
- **Keeps the computer awake** while it downloads (Options → General), so a long download doesn't stop because the computer fell asleep. The screen can still turn off, and once nothing is downloading the computer sleeps as usual. Sharing a finished torrent doesn't count.
- **Picks up where you left off.** Downloads and queues that were running, and finished torrents that were sharing, carry on when Zipline starts again, after it was closed or crashed. It checks for a metered connection first and counts down a few seconds so you can cancel. Options → General → When Zipline starts can start with everything paused instead.

## Speed limits and data plans

- **Speed limits.** One overall limit, plus a separate limit per download from the progress window or Properties. Both apply immediately, even mid-download.
- **Traffic profiles** (Options → Connection). Named sets of limits: overall download and upload speed, downloads at once, connections per download, and whether finished torrents are shared. **Normal** is your usual settings, **Light** keeps the line free for others (512 KB/s down, 64 KB/s up, 2 at once, 4 connections, no sharing) and **Night** runs 8 at once; change them or add your own. Switch from the status bar, the tray menu or your phone, or let a schedule switch them (Light 09:00–18:00 on weekdays, say); outside the schedule's times the profile you chose applies, and switching by hand lasts until the schedule next changes. Changes apply at once, and the speed limiter stays an extra overall limit on top, so a daytime limit no longer slows the night's free-hours downloads.
- **Hourly data cap.** "No more than X MB every N hours" (Options → Connection). When it's reached, downloads (and torrent uploads, when they count) wait and carry on by themselves as the allowance frees up. The status bar shows how much is used.
- **Data quotas** (Options → Data plan) that match real plans: so much a day, or a month starting on your plan's reset day, counting downloads, uploads or both, for every network or just one (your home broadband, say, and a smaller one for a phone's hotspot). Zipline warns you at 80% (you choose), and at 100% downloads and uploads wait until the quota starts again, unless you carry on anyway for the rest of that day or month from the status bar. Data used during your free hours doesn't count. The status bar shows the quota ("Today 1.2 / 2 GB"), and daily totals per network are kept for 13 months.
- **Free data hours** (Options → Data plan). If your plan has free hours, such as free night-time data, tell Zipline when they are. Downloads in the **Free data hours** queue then run only in that time and stop a few minutes before it ends (you choose how many), so nothing spills into paid data. Anything unfinished carries on the next time. While the free hours haven't started, the Add dialog suggests that queue ("Download in Free Hours") and doesn't start downloading early. Zipline learns your free-hours speed and shows how many free periods the waiting downloads need and when they should finish. If you start one of them by hand outside the free hours, it asks first; the tray and your phone can't start them then. Uploads in the free hours are free too unless you say otherwise, and torrents from that queue share only in the free hours.
- **Pause on metered connections** (Options → Data plan). When the computer switches to a phone hotspot or another metered connection, downloads, torrent sharing, site grabs and sync checks pause, and they carry on once it's back on the normal connection. A download you start by hand keeps going. It uses Windows' metered setting and NetworkManager's on Linux. macOS doesn't tell apps which connections are metered, so it isn't available on a Mac.
- **One set of rules for all traffic.** Whatever starts a transfer (the window, the tray, your phone, the command line, a schedule, the site grabber, a sync queue), the same rules apply: the Free data hours queue runs only in its free hours unless you say yes in the window, queues wait out a metered connection and a used-up quota, and every byte counts towards the data cap and quotas, torrent uploads, video playlists, grabbed pages and sync checks included.
- **Polite mode** (Options → Connection). Zipline watches the delay on your line while it downloads or shares: the time to connect to the server it's downloading from, or to a few well-known internet addresses when only torrents run, against the lowest delay of the last 10 minutes. When something else needs the connection, such as a video call, a game or a TV streaming in another room, the delay grows; from 30 ms more Zipline eases off, and firmly from 60 ms (the target of LEDBAT++, which Windows Update uses), slowing torrent uploads too, then speeds back up once the line is free. With several internet connections each one is watched on its own. Unlike a fixed speed limit, nothing is lost while nobody else is online. The status bar shows when it's holding back.
- **Several internet connections at once** (Options → Connection). Wi-Fi plus a phone connected by USB, or two broadband lines: each download's connections are spread across them, so their speeds add up. Each connection shows its own speed and has a Test button. You can leave any of them out, and one that loses its internet is skipped for a minute and then tried again.

## Browser extension

The extension adds **Download with Zipline** to the right-click menu for links, images and video, passing along cookies and the referer so downloads that need a sign-in still work. Every Zipline download has it in the `extension` folder (Chrome and its relatives) and the `extension-firefox` folder and `zipline-firefox.xpi` (Firefox).

### Add it to your browser

**Chrome, Edge, Brave, Opera** (Chrome 116 or newer)

1. Open `chrome://extensions` (or `edge://extensions`).
2. Turn on **Developer mode**.
3. Click **Load unpacked** and choose the `extension` folder.

Keep the folder where it is afterwards: the browser loads the extension from it each time.

**Firefox** (version 115 or newer). Firefox only installs extensions permanently once Mozilla has signed them, so pick one of these:

- **Try it now:** open `about:debugging` → **This Firefox** → **Load Temporary Add-on…** and choose `manifest.json` in the `extension-firefox` folder. It stays until you restart Firefox.
- **Keep it installed, without signing:** in Firefox Developer Edition, Nightly or ESR, set `xpinstall.signatures.required` to `false` in `about:config`, then open `zipline-firefox.xpi`.
- **Keep it installed in regular Firefox:** have it signed for free as an unlisted add-on at addons.mozilla.org (needs a Mozilla account), then install the signed file.

The first time, click the Zipline button in the toolbar and press **Allow**: Firefox asks before an extension can read pages and send sign-in cookies.

If the extension was already installed, click reload on it in `chrome://extensions` after updating so new features switch on.

### What it does

- **Download all links on a page:** right-click the page → **Download all links with Zipline…**, or use the button in the extension's popup. Zipline opens a picker listing every link, image and video on the page, with the files ticked and the web pages not. If you've selected part of the page first, only links in the selection are collected. The list goes to Zipline without cookies; when you press **Download selected**, Zipline asks the extension for the cookies of the chosen files' sites only (the button says so while it waits, a few seconds at most). If the extension doesn't answer in time, the files are added without them, with a note.
- **Scan this site:** right-click a page → **Scan this site with Zipline…**, or use the button in the popup. The site grabber opens on that page, signed in as you are in the browser.
- **Download video:** a button shows when you point at a video player (videos inside other elements' shadow DOM too), and the popup lists the videos found on the current page. You can turn the button off in the popup or in Zipline's Options → Browser, or hide it on one site from the button's menu.
- **Magnet links and .torrent files** you click open in Zipline's Add dialog, even with takeover off (switch this off in Zipline's Options → Torrents). If Zipline isn't running, they open as they normally would.
- **Keyboard shortcuts:** **Alt+Shift+Z** sends the link under the pointer to Zipline, or, when the pointer isn't on a link, the page's video. **Alt+Shift+X** opens the extension's menu. Change them at `chrome://extensions/shortcuts` (Firefox: Add-ons → ⚙ → Manage Extension Shortcuts).
- **Hotkeys:** hold **Alt** (Option on a Mac) while clicking a link to send it straight to Zipline, even with takeover off. When takeover is on, hold **Ctrl+Shift** while clicking to keep that download in the browser. Both keys can be changed in Zipline's Options → Browser, and the extension picks up the change by itself.

### Taking over the browser's downloads

Taking over normal browser downloads is **off by default**. Switch it on in Zipline's Options → Browser or the extension's popup. Zipline then takes a download as the browser asked for it: the browser's own request (its exact headers, cookies included, and a form's fields for a download that came from a form) and what the server's reply said (the name, size and whether it can resume) go to Zipline's Add dialog, so the file is fetched once more, by Zipline, and not checked first. Links that work only once, or for a moment, carry on working. In Chrome, Edge, Brave and Opera this happens before the browser's own "Save as" dialog, so you see one dialog, not two. **Close Zipline's Add dialog** without adding the download and it goes back to the browser, which downloads it as usual.

A form's download is repeated as a POST with the same fields; a server that doesn't do byte ranges for a POST sends the whole file, which Zipline then downloads over one connection, starting again from the beginning if it's paused. File types to leave in the browser are judged by the file's own name and type, so links like `download.php?id=7` that serve a real file are taken over. If Zipline isn't running, downloads stay in the browser, untouched (the extension checks for the app less often then: after 30 seconds, then 2, 5 and every 10 minutes, and at once when you open its menu). Sites listed under Options → Browser → "Never take over downloads from" (your bank, a company intranet) always keep their downloads in the browser.

### Settings in one place

The extension's settings live in Zipline's Options → Browser: taking over downloads, the smallest file to take, file types to leave to the browser, the video button and the sites it's hidden on, along with the hotkeys and sites to leave alone. The extension gets them with each check-in and keeps a copy for when Zipline isn't running. Its popup has quick switches for taking over downloads and the video button (a change made while Zipline is closed reaches it when it starts), an **Open Zipline** button and **Browser options…**, and follows Zipline's light or dark theme.

The list of protected sites comes from the app with each check-in, so the extension and the app always agree; the extension keeps a copy for an older app or one that isn't running.

### Videos in the popup

**One entry per video.** A stream is listed once: its master playlist (or DASH manifest) together with the quality playlists, audio tracks and parts it lists, so you never get a video without sound by picking a single quality. The entry is named after the page (its `og:title`, `twitter:title` or title, without a trailing "| Site" or "- Site") with the length and the best quality when known. Zipline's Add dialog gets the master address and the page's title, so the file is named after the video and you pick the quality there. A playlist seen on its own, with no master, is still offered. The popup entry and the button on the video send exactly the same thing: the Referer, Origin, User-Agent and cookies of the player's own request (cookies only if that request had them).

**How streams are recognised.** By the reply's type (`application/vnd.apple.mpegurl`, `application/x-mpegURL`, `application/dash+xml`, `video/vnd.mpeg.dash.mpd`) or the address ending, and, for a vague type (`text/plain`, `application/octet-stream`) on an address with a word like `playlist`, `manifest`, `master`, `m3u8` or `mpd`, by looking at the first bytes (`#EXTM3U` or `<MPD`). Chrome's extensions can't read a reply's body, so the extension fetches the playlist once more (the first time it sees it on a page), with your cookies for that site and the player's Referer and Origin. Firefox reads the reply as it passes instead.

### Private windows and containers

Cookies come from the window a link or download came from: a private window's own, a Firefox container's, and those a site keeps for the page it was embedded in (partitioned cookies). A download from a private window is private in Zipline too: it's never written to Zipline's saved list, it shows a small "Private" mark while it runs, and it leaves the list once it's finished (the file stays). The extension only works in private windows if you allow it (Chrome: the extension's Details → Allow in Incognito).

### Privacy

The extension talks only to the Zipline app on your own computer (`127.0.0.1`); nothing leaves the computer through it. It reads links, videos and cookies only for what you send to Zipline (and, with takeover on, the downloads the browser starts), keeps no statistics and loads no outside code. [extension/PRIVACY.md](https://github.com/Oshan96/Zipline/blob/main/extension/PRIVACY.md) lists what it reads, sends and keeps, and why it needs each permission.

### How the extension reaches Zipline

Each time Zipline starts it registers itself, for your user only and without administrator rights, as the browsers' *native messaging host* `app.zipline.host` (as IDM and FDM do): registry keys under `HKEY_CURRENT_USER\Software\<browser>\NativeMessagingHosts` on Windows, small manifest files in each browser's `NativeMessagingHosts` folder under `~/Library/Application Support` on macOS and `~/.config` on Linux (`~/.mozilla/native-messaging-hosts` for Firefox). Only Zipline's own extension may use it: the ID `oogcbnoijbeekgjdkkjcbdbnhhjjbalf` in Chrome, Edge, Brave, Vivaldi and Opera (fixed by the `key` in `manifest.json`), and `zipline-catcher@zipline.app` in Firefox. The browser starts the Zipline program to carry the extension's messages, so both sides know who they're talking to; it starts Zipline in the background when you send it something while it isn't running, and lets Zipline call the extension at once when it needs a page opened (a fresh link, a site scan) instead of waiting for its next check-in. Installed a browser after Zipline last started? Start Zipline again so it registers there too.

Without that link (a browser Zipline doesn't know, or one run with its own profile folder), the extension uses `127.0.0.1:17890`. The first time, Zipline's window asks **A browser extension wants to connect to Zipline**: choose **Allow** only if you just installed or reset the extension. Its requests are then signed with the key Zipline gave it. Options → Browser shows how the extension is connected and its version, and lists the browsers allowed this way (**Forget** removes one; the popup's **Connect** asks again).

**Versions.** The extension and Zipline tell each other their versions and the oldest one they work with. If one side is too old, the toolbar button shows **!** and the popup says whether to update the extension or Zipline; Options → Browser says the same.

## Control from your phone

Open Options → Phone, or Tasks → Control from your phone…. Switch it on and press **Add a phone**, then scan the QR code with your phone's camera (or open the link shown and type the code). The window then asks "A phone wants to connect" with the phone's name and address, and the phone can use Zipline once you press **Allow**. The phone's browser then shows your downloads with their progress. From the phone you can:

- add a link the way the window's Add dialog does: the computer reads it first, so a `.torrent` or magnet link, a metalink, a video stream and a link that finishes a download whose link expired are all recognised. You see the name and size, choose the queue for **Download later** (the Free data hours queue is suggested outside the free hours), and start now or wait in the queue;
- choose which files of a torrent to download, and how soon each one comes (Skip, Low, Normal, High), when you add it and later from the download's menu, and stop sharing a torrent that is sharing;
- limit the speed of one download;
- pause, resume or remove a download;
- start or stop queues, switch the speed limit on or off, and switch the traffic profile.

The phone uses the same status words as the window's list (Complete, Paused, Queued, Error…) and the same Unfinished, Finished and Errors filters. It also shows the traffic profile in use and how much of your data quota is used. Starting a download or the queue of the Free data hours outside its free hours asks first, on the phone as in the window, because it would use your normal data.

It's a web page, so there's nothing to install, but the phone has to be on the same Wi-Fi as the computer, and Zipline only answers on your private networks (see [Security](#security)). A video stream's quality, audio and subtitles, a live stream and a folder to save to are chosen in the window, not from the phone. Each phone has its own key and can be removed at any time, from Options → Phone or from the phone itself (menu → Unpair this phone, which makes the computer forget its key too). Removing a download from the phone asks for a second tap.

## Safety checks

- **Virus scan when a download finishes (optional, off by default).** Turn it on in Options → Virus scan, or per download in Properties. It uses Microsoft Defender on Windows, ClamAV on Mac/Linux if installed, or any scanner you choose. You can also right-click a finished file → Scan for viruses now.
- **Checks before it starts.** Zipline checks there's room for the file, that the drive can hold it (FAT32 drives can't store files over 4 GB) and that the path isn't too long, and says so at once instead of failing half-way.
- **Marked as downloaded from the internet.** Finished files carry the same mark browsers add (Mark-of-the-Web on Windows, quarantine on macOS, the origin link on Linux), so SmartScreen, Office's Protected View and Gatekeeper check them as they would a browser download.
- **Honest about itself.** Zipline names itself to servers (a download handed over by the browser keeps the browser's name). If a server turns that away, it asks once more as a browser. When a site shows a "check you're a person" page instead of the file, the download says so: right-click → Open in browser, and the extension hands the file back.

## The window

- **Columns your way.** Right-click the column headers to show or hide columns (Added, Finished, Category, Queue, Link, Save to, Connections, Virus scan, Upload and more), drag headers to reorder them, or use View → Columns. Narrow windows drop the less important columns automatically.
- **Thousands of downloads, no slowdown.** The window draws only the rows on screen and hears only about what changed, a few times a second (less while it's minimized), so a list of 5,000 downloads stays as quick as one of 50.
- **Everything from the keyboard.** Alt or F10 opens the menus (arrow keys, Enter, Escape), Shift+F10 or the Menu key the context menu, and Tab moves between the menu bar, toolbar, sidebar, list and column headers. In the list, the arrow keys, Page Up/Down and Home/End move, Shift extends the selection, and Ctrl with the arrows and Space picks rows one by one; Up from the first row reaches the column headers, where Enter sorts. Screen readers hear a proper menu bar, list of views and grid.
- **Dialogs that behave.** Windows such as Options, Properties and Add are real dialogs: the keyboard stays inside them, Escape closes them and focus goes back where it was. Progress windows stay beside the list. Mistakes are shown next to the field, a button that's saving says so and can't be pressed twice, and a failed save says why. Options and Queues & scheduler both work with OK, Cancel and Apply.
- **"Download complete" without the pile-up.** A download you started yourself says it's finished in a small window with Open and Open folder (Options → General turns it off). Downloads that finish while it's open join it, and it lists them all with Show in list. A queue's downloads (a batch, the site grabber, a schedule) open no window: they're announced together in one quiet message, and the queue says when it's done.
- **Notifications you can follow.** Messages stack at the bottom (at most four), pause while you point at them and can be dismissed; the bell in the status bar keeps the last 50. Screen readers hear finished downloads and errors, a few at a time, and progress bars announce their percentage and time left.
- **Sounds** (Options → Sounds) when a download finishes or fails, or when a queue finishes. They play even when the window is closed, and a batch finishing together plays one sound.
- **Readable and calm.** Text and controls meet WCAG contrast in light and dark themes, every control shows a focus ring, and Zipline follows the system's reduced-motion, more-contrast and Windows high-contrast settings. The window and the phone page share one set of colours and words, and file names written right to left display correctly.
- **Fits any window.** Toolbar buttons that don't fit move into a ⋯ menu, the file name keeps its room while other columns give way, and menus always stay on screen, at any width and zoom.
- Light and dark themes, sortable and resizable columns, search, keyboard shortcuts, and a layout that adapts down to phone-sized windows.

## Command line

Add links, choose the folder, file name and queue, start queues, import, export or quit from scripts or shortcuts. If Zipline is already running, the request goes to it; if it isn't, Zipline starts in the background to carry it out (an export is written straight away without starting it, and `--quit` just says it isn't running). A password given with `--password` can be seen by other programs in the process list, so `--password-stdin` (with `--user`) reads it from standard input instead. See Help → Command-line options or run `Zipline --help`.

Other useful options: `--nowindow` starts Zipline in the background, `--login-url` prints a one-time address that opens the window in any browser, and `--version` prints the version.

## Where data is kept

- Settings and the download list are in `%AppData%\Zipline` on Windows, `~/Library/Application Support/Zipline` on macOS, and `~/.config/Zipline` on Linux.
- Files that aren't finished yet end in `.zdl` until they complete. Torrents write straight into their files; bytes of files you didn't tick that share a piece with ones you did are kept in a hidden `.parts` file next to them until the torrent is finished.
- Torrent files and the DHT's list of nodes are kept in the `torrents` folder and `dht.json` next to the settings.
- Data use is kept next to them too: `usage.json` (per minute, for the hourly cap), `usage-up.json` (uploads) and `usage-days.json` (totals per day and network for quotas, 13 months).
- Progress is saved every few seconds, after the downloaded data is safely on disk, so after a power cut a download carries on from where it really was. After an unclean shutdown Zipline fetches the last part of each unfinished piece again, in case the disk lost its final writes.
- `run.json` (only your user can read it) tells the command line and the browser's native messaging host where the running Zipline listens, with a secret made at each start; `zipline.lock` keeps a second Zipline from starting on the same folder; `extpair.json` holds the browsers allowed over HTTP (their keys stored like saved passwords). On Windows the native messaging manifests are in the `nmhost` folder.
- Each time the settings or the download list are saved, the previous copy is kept as a `.bak` file. If a file is damaged (for example after a power cut), Zipline loads the backup, keeps the damaged copy as `….damaged-<date>` and tells you once in the window.
- On macOS and Linux the data folder is readable only by you (`0700`) and so is every file Zipline writes in it (`0600`); files from older versions are fixed when Zipline starts.
- Zipline keeps a log (`zipline.log`) and, if it ever crashes, a report (`crash.log`) in the same folder.

## Security

Zipline's window and the browser extension talk to the app only on `127.0.0.1:17890`, which other computers can't reach, and nothing there answers without proof of who's asking:

- **The window** opens with a one-time code (good for a minute, once) that Zipline swaps for a session cookie scripts can't read and other sites can't send. Without it the page only says to open Zipline from its icon or menu, and requests from other sites (or other addresses that resolve to this computer) are turned away.
- **Programs on this computer** (a second launch, the command line, the native messaging host) prove themselves with the secret in `run.json`, which only your user can read, and Zipline proves itself back, so a program that took its port gets nothing.
- **The browser extension** comes through its native messaging host, or over HTTP once you've allowed it (signed requests; see [How the extension reaches Zipline](#how-the-extension-reaches-zipline)). Without either it only learns Zipline's version.
- **Running programs:** the virus-scan program (and its arguments), which runs after every download, can only be changed in the window, which shows the program and asks before saving.
- **One Zipline per user:** a lock file in your data folder, not the port. If another program has port 17890, Zipline takes a free one (the native messaging link still finds it; the window says so).

Two features listen on your network, each on its own port: torrent sharing, for other peers (Options → Torrents; TCP and UDP on one port, which Zipline asks your router to open unless you switch that off), and phone control, only while it's switched on (Options → Phone). Sign-in cookies and passwords only go to the site they came from, never to mirrors or other sites a download is spread across.

### Saved passwords and keys

Every secret Zipline stores is kept in one vault: the passwords for proxies and sites, passwords and cookies typed into the Add dialog or the site grabber (in downloads, links and projects), and phone keys. Each is saved as `enc:v1:` followed by the encrypted text, and is decrypted when Zipline starts. Settings from older versions are read as they were and saved encrypted the next time. The window is never sent a saved password, only that one is saved, and exports leave them out unless you ask. The key lives in:

- **Windows:** your Windows account (DPAPI), so copied files can't be read on another account or computer.
- **macOS:** a random key in your login Keychain.
- **Linux:** a random key in your desktop's password store (the Secret Service), when `secret-tool` (libsecret) is installed and a desktop session is running. Otherwise it is a file, `vault.key`, in the data folder, readable only by you. That keeps other users out and stops the data being read from a stray copy of `settings.json`, but anyone or anything that can read your files as you can read the key as well. Install `secret-tool` for better.
- If the key can't be fetched (a locked Keychain, say), Zipline leaves the protected values exactly as they are, never saves blanks over them, tells you in Options and uses them again once the key is back. A secret it couldn't protect when saving is kept as plain text, with a note saying so.

### Phone control

Phone control is off until you switch it on. It then listens on its own port (17891 by default) and serves only the phone page and a small set of actions; the main window's interface stays on `127.0.0.1`.

- **Private networks only.** Zipline listens only on the addresses of your private networks (home ranges such as `192.168.x.x`, `10.x.x.x` and `172.16.x.x`, and IPv6 local addresses), never on every address and never on a public one. A connection from outside the subnet it arrived on is dropped before anything is read. Zipline looks again when the network changes, so moving between Wi-Fi networks moves the page with you.
- **Public networks.** When the system calls the network **Public** (Windows' network profile, or the `public` zone on Linux), the phone page stays off there and Options → Phone says "This network is marked Public: the phone page is off here". **Allow on this network anyway** switches it on for that one network (a name and subnet) and remembers the choice. Windows is asked with `Get-NetConnectionProfile` (PowerShell, hidden, with a time limit); Linux is asked through NetworkManager's `nmcli` (the connection's firewall zone, or firewalld's default zone). macOS has no such setting, so every private network counts as trusted there. When the system can't say, the network counts as not Public.
- **Pairing.** The code the phone pairs with is its key. It's shown on screen and carried in the QR code's address after a `#`, which browsers never send, so it doesn't cross the network. Seeing the code is not enough: the first time a phone uses a new code it only asks to connect, and nothing works until you press **Allow** in Zipline's window ("A phone wants to connect: name, address"). **Deny** forgets the code.
- **Signed requests.** Every request from the phone is signed with that key (HMAC-SHA256 over the request and the time) and accepted only once, within 90 seconds.
- **Limits.** A pairing code that isn't used stops working after 15 minutes. An address that keeps sending wrong signatures is locked out for 10 minutes.
- **Storage.** Phone keys are stored like saved passwords (see above). Only browser extensions and programs on your own computer (the command line) can send links in from outside the window; websites can't. Links from the extension always ask you to confirm before they download.

Found a security problem? See the [security policy](https://github.com/Oshan96/Zipline/blob/main/SECURITY.md) for how to report it privately.

## Limitations

- FTP goes through a proxy only when it's SOCKS5; an HTTP proxy isn't used for FTP. The site grabber lists FTP folders from the server's root.
- A metalink's torrent is used for the files it holds, saved with the torrent's own names and folders; FTP mirrors can't serve as its web seeds, nor HTTP mirrors laid out differently from a torrent of several files. When the torrent can't be fetched or doesn't hold the file, the mirrors download it as usual.
- Proxy setup scripts (PAC) run in Zipline's own small JavaScript interpreter on macOS and Linux. It covers what PAC files use (and more), but not every modern JavaScript feature.
- Torrents use BitTorrent v1 (v2-only torrents are refused; hybrid ones work). With a proxy set in Options, peers and trackers go through it (SOCKS5, or an HTTP proxy's CONNECT for peers), and the DHT, uTP, incoming peers and port mapping are off meanwhile, as in qBittorrent; UDP trackers need a SOCKS5 proxy. A system HTTP proxy, usually there for web pages only, isn't used for torrents. Port mapping covers IPv4 only, and when the router gives a different outside port, trackers are still told the inside one. A finished torrent that stopped sharing (it reached its ratio or time, or you stopped it) shares again only when you ask, with Share again; one that was still sharing when Zipline closed carries on at the next start. On a Mac, choose which app opens magnet links and `.torrent` files in Finder (Get Info → Open with); dropping or pasting them into Zipline always works. Torrents were tested against libtorrent (the engine inside qBittorrent and Deluge) on one computer, not on the public swarms.
- Phone control uses plain HTTP on your home network: browsers offer HTTPS only for names and certificates a home computer doesn't have, so the page is not encrypted. Someone else on the same Wi-Fi who can watch the traffic could read the list of downloads, and someone able to tamper with it (a rogue access point, say) could hand the phone a changed page that takes its key. Signed requests mean watching alone doesn't let anyone control Zipline, and a new phone needs your Allow, but switch phone control off on networks you don't trust. The phone needs to be on the same network (it doesn't work over mobile data). If the computer's network address changes, pair the phone again from Options → Phone.
- Cloud services: every service was checked against simulated replies modelled on its documented or observed answers rather than against the live service. A file that isn't shared publicly (a Google Drive file not shared with "Anyone with the link", a password-protected Dropbox, MediaFire or pCloud link) has to be started from the browser, where the extension passes it on with your sign-in.
  - Best-effort, because they rest on interfaces the services don't publish and change from time to time: OneDrive personal links (OneDrive's sharing service with the anonymous token its own web viewer uses, kept for about a week; links on onedrive.cloud.microsoft go the same way), Google Drive folders (read from the folder's page as gdown does, without sizes), MediaFire (the download button on its page, including its newer scrambled form) and WeTransfer (the internal API its own page uses). If one of them stops working, click Download in the browser and the extension hands the download to Zipline. Work and school (SharePoint) links work directly; their folders can't be listed without a sign-in.
  - MEGA: file links (mega.nz/file/… and the older #!… links) and shared folders (mega.nz/folder/…, #F!…, and a file's link inside one). Files MEGA stores in several parts (CloudRAID) fail with a message saying so. When MEGA's free transfer quota for your connection is used up, the download stops and says when it frees up. MediaFire folder links aren't listed (copy a file's link); Dropbox folder links download as a zip (Dropbox's own way).
  - Yandex Disk and pCloud use their published public APIs (pCloud's US and EU regions are both asked). Folders list at most 2,000 files, eight folders deep.
- Taking a download over from the browser: the browser has already sent its request when it says a download is starting, so a link a server spends on the very first request (even one cancelled at once) can't be taken over by any extension; close Zipline's Add dialog and the browser gets it back. In Chrome, a download from a form (POST) in a private window stays in the browser: an extension can only start a private window's download by opening its address there. Downloads from private windows aren't saved in the list, but Zipline's log file still names them.
- Getting a fresh link from the page needs the browser extension and works when the page itself offers the file (a link or a download it starts). Pages behind a captcha, or ones where you have to click through several steps, need you to click the link yourself; Zipline still recognises it and carries on.
- Zipline only looks for published checksum files for downloads of 20 MB or more, and only next to the file (the same folder on the server). A wrong checksum is reported, but without a metalink's piece checksums the whole file has to be downloaded again.
- DRM-protected video (Netflix, Disney+, Prime Video and similar) and YouTube can't be downloaded. Zipline refuses those sites by address (the list is in the app: YouTube, Netflix, Disney+, Prime Video, Hulu, Max, Spotify, Apple TV+, Peacock, Paramount+, Crunchyroll and their video servers), and a stream from another server when it was played on one of those pages. A DRM service that streams from a server of its own, to a page that isn't on the list, isn't recognised by its address; Zipline then stops at the stream's own protection.
- Live DASH streams aren't recorded (live HLS streams are). SAMPLE-AES inside MP4 pieces (`cbcs`) is refused.
- A video whose pieces fall short by less than a second (or 1%) still counts as complete.
- Subtitles in TTML/IMSC (some DASH and HLS streams) and closed captions carried inside the video aren't downloaded; WebVTT subtitles are.
- Grouping a stream's playlists needs the extension to read them, and Chrome extensions can't read replies (Manifest V3), so the playlist is fetched a second time. A server that hands out a playlist only once, or turns the second request away (a one-time token), leaves the stream listed by its address without its qualities, and its separate playlists may then show as separate entries. A reply with a vague type is only looked at when its address says "playlist", "manifest", "master", "m3u8", "mpd" and the like; other addresses are never fetched a second time.
- Proxy logins use a plain username and password. Company proxies that only accept Windows sign-in (NTLM or Kerberos) aren't supported yet.
- Speed is still limited by your connection and by how many connections the server allows.
- The tray icon is Windows-only. On macOS and Linux, run Zipline again to bring its window back.
- The window's one-time code is on the browser's command line for a moment while the window opens; on a Mac or Linux computer shared with other people, another user could see it there and race the window to use it (it works once, within a minute). Pairing an extension over HTTP has a similar gap: a program that holds port 17890 at that moment could answer in Zipline's place. The native messaging link, set up automatically, has neither.
- Zipline's window runs in a browser profile of its own when Chrome, Edge, Brave or Chromium is installed. Without one it opens in your default browser, which then also sends the window's sign-in cookie to other web pages served from this computer (browsers keep cookies by address, not by port); close that tab when you're done.
- Waking the computer for a scheduled start is Windows-only: on macOS and Linux it needs administrator setup (`pmset`, `rtcwake`), so leave the computer awake for the start. Hibernate is offered only where the system allows it without a password (on Linux, when systemd says so); a Mac doesn't offer it.
- A download from a server without resume support can't wait out the data cap: if the server drops the idle connection, it starts again from the beginning.
- Several internet connections at once needs each connection to have its own address on this computer (Wi-Fi plus USB tethering or Ethernet, for example). A single download from a server that allows only one connection can't use more than one line. Routing was tested with several addresses on one machine, not yet on every combination of real adapters.
- Polite mode measures the delay to the server it's downloading from. On some connections, such as busy mobile networks, the delay changes on its own, so polite mode may ease off a little when it doesn't need to. With several internet connections at once it slows each one's own connections; torrent peers that connect to you aren't slowed then.
- Quotas count what Zipline itself moves (the bytes of files, pages and torrent traffic, not trackers or the DHT), not other apps, so set them a little under your plan's real allowance. A network is told apart by its router (the default gateway's address and hardware address), so a VPN, or a router that's replaced, counts as another network; with several internet connections at once, everything counts towards the network the computer's internet goes through.
- The Linux `.deb` and `.rpm` are for x64 computers; other distributions use the tar.gz.

## Troubleshooting

- **No window appears.** Zipline may already be running in the background: run it again to bring the window back, or run `zipline --login-url` and open the address it prints in any browser (it works once, within a minute).
- **"Windows protected your PC" or "Apple could not verify Zipline".** Zipline isn't code-signed yet; see [Install](#install) for how to let it run.
- **The extension shows "!"** One side is too old for the other: the popup says whether to update the extension or Zipline.
- **A download stopped with an error.** Its Description column says why, and so does its progress window (double-click it). For links that expired, see [Links that expire](#links-that-expire-carry-on-by-themselves); for a damaged file, right-click → Redownload.
- **Reporting a bug.** Help → Diagnostics shows your version, settings (without passwords) and the recent log, ready to copy into a [bug report](https://github.com/Oshan96/Zipline/issues/new/choose). Zipline keeps a log (`zipline.log`) and, if it ever crashes, a report (`crash.log`) in its [data folder](#where-data-is-kept). `Zipline --version` prints the version.
