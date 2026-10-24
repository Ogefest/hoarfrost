const $ = (id) => document.getElementById(id);

async function settings() {
  return chrome.storage.sync.get({ server: "", token: "", storage: "", queue: false });
}

async function activeTab() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  return tab;
}

async function archive() {
  const status = $("status");
  const cfg = await settings();
  if (!cfg.server || !cfg.token) {
    status.textContent = "Set the server URL and API token in Settings first.";
    return;
  }
  const tab = await activeTab();
  status.textContent = "Saving…";
  $("save").disabled = true;
  try {
    const mhtml = await chrome.pageCapture.saveAsMHTML({ tabId: tab.id });
    const form = new FormData();
    form.append("file", mhtml, "page.mhtml");
    form.append("url", tab.url);
    form.append("title", tab.title || "");
    form.append("date", new Date().toISOString());
    if ($("queue").checked) form.append("queue", "1");
    if (cfg.storage) form.append("storage", cfg.storage);
    const resp = await fetch(cfg.server.replace(/\/$/, "") + "/api/upload", {
      method: "POST",
      headers: { Authorization: "Bearer " + cfg.token },
      body: form,
    });
    const body = await resp.json().catch(() => ({}));
    status.textContent = resp.ok ? "Archived ✓" : "Error: " + (body.error || resp.status);
  } catch (e) {
    status.textContent = "Error: " + e.message;
  } finally {
    $("save").disabled = false;
  }
}

document.addEventListener("DOMContentLoaded", async () => {
  const tab = await activeTab();
  $("page").textContent = tab.title || tab.url;
  $("queue").checked = (await settings()).queue;
  $("save").addEventListener("click", archive);
});
