# hoarfrost browser extension

Minimal Chrome/Chromium extension (Manifest V3) that saves the current tab
with `chrome.pageCapture.saveAsMHTML()` and uploads it to `POST /api/upload`
of a hoarfrost instance. The page is stored as a WARC `resource` record in the
current week; optionally the URL is also queued for a regular HTTP capture.

Install: open `chrome://extensions`, enable developer mode, "Load unpacked"
and select this directory. In the extension settings enter the server URL and
an API token (hoarfrost → Account → API tokens, editor role or higher).

Alternatives without custom code:

- **SingleFile**: in its options enable *Destinations → upload to a REST form API*,
  URL `https://<server>/api/upload`, file field `file`, URL field `url`, and set
  the `Authorization: Bearer <token>` header.
- **ArchiveWeb.page**: export a WACZ and upload it with
  `curl -H "Authorization: Bearer <token>" -F file=@archive.wacz https://<server>/api/upload`.
