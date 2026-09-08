const POLL_INTERVAL_MS = 3000;

const $ = (id) => document.getElementById(id);

async function api(path, options) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...options,
  });
  const text = await res.text();
  const body = text ? JSON.parse(text) : null;
  if (!res.ok) throw new Error(body?.error || `${res.status} ${res.statusText}`);
  return body;
}

const STEP_LABELS = {
  QUEUED: "順番待ち",
  CREATE_PROJECT: "Project を作成中",
  LINK_BILLING: "請求先アカウントを紐付け中",
  GRANT_IAM: "権限を付与中",
  ENABLE_SERVICES: "API を有効化中",
  APPLY_QUOTAS: "Quota を設定中",
  DONE: "完了",
};

function renderAllocations(allocations) {
  const el = $("allocations");
  if (!allocations.length) {
    el.innerHTML = `<p class="empty">まだ払い出しはありません。</p>`;
    return;
  }
  el.innerHTML = allocations.map((a) => {
    const consoleURL = `https://console.cloud.google.com/home/dashboard?project=${encodeURIComponent(a.projectID)}`;
    const link = a.status === "READY"
      ? `<p class="meta"><a href="${consoleURL}" target="_blank" rel="noopener">Google Cloud Console を開く</a></p>`
      : "";
    const detail = a.status === "FAILED"
      ? `<p class="meta error">${escapeHTML(a.error || "払い出しに失敗しました")}</p>`
      : `<p class="meta">${escapeHTML(STEP_LABELS[a.step] || a.step)}</p>`;
    return `<article class="alloc">
      <div class="alloc-head">
        <code>${escapeHTML(a.projectID || "(Project ID を採番中)")}</code>
        <span class="badge ${a.status}">${a.status}</span>
        <span class="meta">${escapeHTML(a.eventCode)}</span>
      </div>
      ${detail}
      ${link}
    </article>`;
  }).join("");
}

function escapeHTML(v) {
  return String(v ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  })[c]);
}

let pollTimer = null;

async function refresh() {
  const allocations = await api("/api/allocations");
  renderAllocations(allocations);
  const running = allocations.some((a) => a.status === "PENDING" || a.status === "PROVISIONING");
  clearTimeout(pollTimer);
  if (running) pollTimer = setTimeout(() => refresh().catch(console.error), POLL_INTERVAL_MS);
}

$("allocate").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const button = ev.target.querySelector("button");
  const error = $("formError");
  error.hidden = true;
  button.disabled = true;
  try {
    await api("/api/allocations", {
      method: "POST",
      body: JSON.stringify({ eventCode: $("eventCode").value.trim() }),
    });
    $("eventCode").value = "";
    await refresh();
  } catch (e) {
    error.textContent = e.message;
    error.hidden = false;
  } finally {
    button.disabled = false;
  }
});

(async () => {
  try {
    const me = await api("/api/me");
    document.querySelector(".me").textContent = me.email;
  } catch (e) {
    document.querySelector(".me").textContent = e.message;
  }
  refresh().catch((e) => {
    $("allocations").innerHTML = `<p class="error">${escapeHTML(e.message)}</p>`;
  });
})();
