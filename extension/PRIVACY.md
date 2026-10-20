# Zipline Download Catcher: privacy

The extension hands links, videos and downloads from your browser to the Zipline app **on the same
computer**. It talks only to that app, at `http://127.0.0.1:17890` (an address other computers can't
reach). It has no server of its own, sends nothing to its makers or anyone else, shows no ads, keeps
no statistics and loads no code from the internet. Nothing leaves your computer through the extension;
the Zipline app then downloads the files from the sites they're on, as your browser would.

## What it reads

- **Links, images and videos on a page**, only when you ask: right-click → Download with Zipline,
  Download all links, Scan this site, the video button, or the keyboard shortcut.
- **The video files and stream playlists a page plays**, to list them in the menu and on the video
  button, with the headers the player's request carried (Referer, Origin, User-Agent, whether it had
  cookies). They're kept for that tab only and forgotten when the tab closes or the browser does.
- **Downloads the browser starts**, only if you switch on "Take over browser downloads" (off by
  default): the download's address, name, size and type, the headers of the browser's request, and a
  form's fields when the download came from a form (so Zipline can make the same request once).
- **Cookies**, for the sites of the files you send to Zipline, from the window they came from (a
  private window's, or a Firefox container's), so downloads that need you signed in still work.
  "Download all links" sends no cookies with the list: only when you press Download in Zipline's
  picker does the extension send the cookies of the sites of the files you chose.
- **The page's title**, to name a video after it.

## What it sends, and where

Everything above goes only to the Zipline app on this computer, when you send something to it (or,
with takeover on, when the browser starts a download). Downloads from private windows are marked
private: Zipline doesn't keep them in its list once they're finished.

## What it keeps

- A copy of its settings (they belong to the Zipline app, Options → Browser), so it works while the
  app isn't running.
- For the browser session only (cleared when the browser closes): the videos found on each open tab,
  sites you sent to the browser on purpose with the hotkey, and work in progress (a page opened to get
  a fresh link for an expired download, a request being handed to Zipline).

## Permissions

| Permission | Why |
| --- | --- |
| `downloads` | take over downloads (if you switch it on), hand one back to the browser |
| `cookies` | send the right sign-in with a file you send to Zipline |
| `webRequest` (and `webRequestBlocking`, `webRequestFilterResponse` in Firefox) | see the video files and playlists a page plays, and the request a download came from |
| `declarativeNetRequestWithHostAccess` (Chrome) | read a stream playlist once more with the player's Referer and Origin |
| `scripting`, `activeTab` | collect a page's links when you ask, read the page's title |
| `contextMenus` | the right-click menu |
| `storage` | the settings copy, the per-tab video lists |
| `alarms` | check in with the app now and then (less often when it isn't running) |
| access to all sites | the above, on whatever site you use it |
