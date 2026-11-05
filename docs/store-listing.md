# Listing the browser extension in the stores

What to fill in for Zipline Download Catcher on the Chrome Web Store, Microsoft Edge Add-ons and addons.mozilla.org (AMO). The packages come from `packaging/package.sh`; how they're built, and the extension's ID, are in [releasing.md](releasing.md#the-browser-extension-in-the-stores).

| Store | Upload | Images |
|---|---|---|
| Chrome Web Store | `pkg/ext/zipline-chrome-store.zip` | `assets/store/`: the three `screenshot-*.png` (1280×800), `promo-small.png` (440×280), `promo-marquee.png` (1400×560) |
| Edge Add-ons | the same zip | the same screenshots and small tile; the marquee as the large promotional tile |
| AMO (listed) | `packaging/sign-firefox.sh --listed` | the three screenshots |

The store icon is `extension/icons/icon128.png`. `python3 tools/store_images.py` makes the images again after a visible change: it renders the popup from `extension/popup.html` and uses the window's pictures from `assets/screenshots/`.

## The listing

**Name:** Zipline Download Catcher

**Summary** (132 characters at most on the Chrome Web Store):

> Send links, videos and downloads to the Zipline download manager app for fast, resumable, multi-connection downloads.

**Category:** Chrome Web Store: Productivity › Tools. Edge: Productivity. AMO: Download Management.

**Language:** English.

**Description:**

> Zipline Download Catcher connects your browser to Zipline, a free, open-source download manager for Windows, macOS and Linux. The extension finds what you want to download and hands it to the app. The app downloads it on many connections at once, resumes after a dropped connection or a restart, and checks the file.
>
> What it does:
> • Right-click a link, picture or video and choose "Download with Zipline", or hold Alt and click a link.
> • Lists the videos a page plays (HLS and DASH streams, plain video files, and players that fetch the picture and the sound separately) in its popup and on a Download button over the player. Zipline saves each as one MP4, with the quality you choose.
> • "Download all links on this page" lets you pick files from a page; "Scan this site" opens Zipline's site grabber, signed in as your browser is.
> • Optionally takes over the browser's own downloads (off until you turn it on), with sizes, file types and sites to leave to the browser.
> • Sends the cookies and headers of the page with each download, so files you're signed in to see download too.
>
> It needs the Zipline app, from https://github.com/Oshan96/Zipline/releases. The app sets up the link to the browser by itself when it starts.
>
> Zipline doesn't download from YouTube or from DRM-protected streaming services, and the extension doesn't offer their videos.
>
> Nothing is collected or sent to anyone: the extension talks only to the Zipline app on your own computer. Privacy policy: https://github.com/Oshan96/Zipline/blob/main/extension/PRIVACY.md

**Homepage:** https://github.com/Oshan96/Zipline · **Support:** https://github.com/Oshan96/Zipline/issues

**Privacy policy:** https://github.com/Oshan96/Zipline/blob/main/extension/PRIVACY.md ([extension/PRIVACY.md](../extension/PRIVACY.md))

## Chrome Web Store: privacy practices

**Single purpose:**

> Hand the user's downloads, links and the videos playing on a page to the Zipline download manager app installed on the same computer.

**Why it needs each permission:**

| Permission | Justification |
|---|---|
| `nativeMessaging` | The extension talks to the Zipline app on the same computer through its native messaging host, which the app installs. Every download goes this way. |
| `downloads` | To take over a download the browser starts (only when the user turns that on), and to give it back to the browser if the user cancels it in Zipline. |
| `contextMenus` | The "Download with Zipline", "Download all links" and "Scan this site" menu items. |
| `cookies` | To send the cookies of the site a download comes from with that download, so files the user is signed in to see can be downloaded by the app. Only for that download's site, only to the local app. |
| `webRequest` | To see the requests a page's video player makes (stream playlists, video files) so the popup can list the videos, and the headers the player sent, which the download needs. Also to recognise a download the browser is starting. |
| `declarativeNetRequestWithHostAccess` | Reading a stream's playlist again (to list its qualities as one video) needs the player's Referer and Origin; a short-lived session rule gives that one request those headers. |
| `scripting` | To read the page's title and links when the user asks for them ("Download all links on this page"), and the length of a playing video. |
| `activeTab` | The popup and its buttons act on the tab the user is looking at. |
| `storage` | The extension's settings and, for the session, the videos found on each tab. |
| `alarms` | To check in with the app now and then (less and less often while it isn't running), and to stop waiting for a page opened to renew a download link. |
| Host permission `<all_urls>` | Videos and downloads can come from any site. The content scripts on every page draw the Download button over video players and catch Alt+click on links. |

**Remote code:** No. All of the extension's code is in the package.

**Data usage:** Nothing leaves the user's computer. The extension handles these, only to pass them to the Zipline app on the same computer when the user downloads something:

- **Authentication information:** the cookies of a download's site.
- **Web history:** the addresses of pages and of the files and videos on them.
- **Website content:** the links on a page, when the user asks for them.

Tick these as handled, since the extension reads them. If the form's wording makes clear that data passed only to a program on the same device isn't "collected", leave them unticked instead. Then tick all three statements: the data isn't sold to third parties, isn't used or transferred for purposes unrelated to the single purpose, and isn't used to determine creditworthiness or for lending.

**Test instructions** (for the reviewers):

> The extension needs the Zipline app. Download it from https://github.com/Oshan96/Zipline/releases/latest (Windows: Zipline-Windows-Setup.exe; macOS: Zipline-macOS.dmg; Linux: the .deb, .rpm or tar.gz) and start it once: it registers the extension's native messaging host. Then the popup says "Connected". To try it: right-click any link → "Download with Zipline" (the app's Add dialog opens); or open a page with a video, play it, and use the popup's Download button. No account is needed.

## Edge Add-ons

The same zip, description, privacy policy and images. Edge gives the extension its own ID: add it to `chromeExtIDs` in `cmd/zipline/nativehost.go` and `ZL_EXT_IDS` in `web/extlink.js` before releasing an app that should accept it ([releasing.md](releasing.md#the-browser-extension-in-the-stores)).

## addons.mozilla.org (listed)

`packaging/sign-firefox.sh --listed` submits the add-on for listing, with the listing's details from `packaging/stores/amo-metadata.json`. It needs Node.js and AMO API credentials (see the script). Mozilla reviews it before it shows on the site, and the signed `.xpi` comes back once the review passes.

- **Source code:** not asked for, since nothing in the add-on is minified or generated.
- **Data collection:** `manifest.firefox.json` declares `"data_collection_permissions": { "required": ["none"] }`, since the add-on sends nothing to its developer or to third parties, only to the Zipline app on the same computer. If AMO's review counts data passed to a native application as collected, declare `browsingActivity` and `authenticationInfo` there instead.
- **Notes for reviewers:** the test instructions above.
- **Version:** AMO takes each version once. `sign-firefox.sh` takes the version to submit (`packaging/sign-firefox.sh --listed 1.1.0`), as releases do; without one it uses the manifest's.
