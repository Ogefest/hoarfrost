const $ = (id) => document.getElementById(id);

document.addEventListener("DOMContentLoaded", async () => {
  const cfg = await chrome.storage.sync.get({ server: "", token: "", storage: "", queue: false });
  $("server").value = cfg.server;
  $("token").value = cfg.token;
  $("storage").value = cfg.storage;
  $("queue").checked = cfg.queue;
  $("save").addEventListener("click", async () => {
    let server;
    try {
      server = new URL($("server").value.trim());
    } catch {
      $("status").textContent = "Invalid server URL.";
      return;
    }
    const granted = await chrome.permissions.request({ origins: [server.origin + "/*"] });
    if (!granted) {
      $("status").textContent = "Permission to contact the server was denied.";
      return;
    }
    await chrome.storage.sync.set({ server: server.origin, token: $("token").value.trim(), storage: $("storage").value.trim(), queue: $("queue").checked });
    $("status").textContent = "Saved.";
  });
});
