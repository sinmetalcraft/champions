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

function escapeHTML(v) {
  return String(v ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  })[c]);
}

function showError(el, message) {
  el.textContent = message;
  el.hidden = !message;
}

let events = [];
let current = null;

// ---- イベント一覧 ----

async function loadEvents(selectCode) {
  events = await api("/api/events");
  const el = $("eventList");
  if (!events.length) {
    el.innerHTML = `<p class="empty">イベントがまだありません。</p>`;
  } else {
    el.innerHTML = events.map((e) => `
      <button type="button" data-code="${escapeHTML(e.code)}" aria-current="${e.code === selectCode}">
        ${escapeHTML(e.code)}<span class="dot ${e.enabled ? "on" : ""}">●</span>
      </button>`).join("");
    el.querySelectorAll("button").forEach((b) => {
      b.addEventListener("click", () => selectEvent(b.dataset.code));
    });
  }
  if (selectCode) {
    const found = events.find((e) => e.code === selectCode);
    if (found) renderEditor(found);
  }
}

function selectEvent(code) {
  $("eventList").querySelectorAll("button").forEach((b) => {
    b.setAttribute("aria-current", String(b.dataset.code === code));
  });
  const e = events.find((x) => x.code === code);
  if (e) {
    renderEditor(e);
    loadAllocations().catch((err) => {
      $("allocations").innerHTML = `<p class="error">${escapeHTML(err.message)}</p>`;
    });
  }
}

// ---- エディタ ----

function renderEditor(e) {
  current = e;
  $("editor").hidden = false;
  $("editorTitle").textContent = e.code;
  $("folderName").textContent = e.folderName || "(未作成)";
  $("displayName").value = e.displayName || "";
  $("enabled").checked = !!e.enabled;
  $("maxAllocations").value = e.maxAllocations || 0;
  $("roles").value = (e.roles || []).join("\n");
  $("apis").value = (e.apis || []).join("\n");
  $("quotaRows").innerHTML = "";
  (e.quotas || []).forEach(addQuotaRow);
  showError($("formError"), "");
  $("saveState").textContent = "";
  $("allocations").innerHTML = `<p class="empty">-</p>`;
  $("shutdownResult").innerHTML = "";
  clearTimeout(shutdownTimer);
  $("shutdownEvent").disabled = false;
}

function addQuotaRow(q = {}) {
  const dimensions = Object.entries(q.dimensions || {})
    .map(([k, v]) => `${k}=${v}`)
    .join(", ");
  const tr = document.createElement("tr");
  tr.innerHTML = `
    <td><input class="q-service" value="${escapeHTML(q.service || "")}" placeholder="compute.googleapis.com"></td>
    <td><input class="q-id" value="${escapeHTML(q.quotaID || "")}" placeholder="CPUS-per-project-region"></td>
    <td><input class="q-dim" value="${escapeHTML(dimensions)}" placeholder="region=asia-northeast1"></td>
    <td><input class="q-value" type="number" step="1" value="${escapeHTML(q.preferredValue ?? 0)}"></td>
    <td><input class="q-email" value="${escapeHTML(q.contactEmail || "")}" placeholder="you@example.com"></td>
    <td><button type="button" class="ghost danger q-del">削除</button></td>`;
  tr.querySelector(".q-del").addEventListener("click", () => tr.remove());
  $("quotaRows").appendChild(tr);
}

function parseDimensions(value) {
  const dimensions = {};
  value.split(",").forEach((pair) => {
    const i = pair.indexOf("=");
    if (i < 0) return;
    const k = pair.slice(0, i).trim();
    const v = pair.slice(i + 1).trim();
    if (k) dimensions[k] = v;
  });
  return dimensions;
}

function collectQuotas() {
  return [...$("quotaRows").querySelectorAll("tr")].map((tr) => ({
    service: tr.querySelector(".q-service").value.trim(),
    quotaID: tr.querySelector(".q-id").value.trim(),
    dimensions: parseDimensions(tr.querySelector(".q-dim").value),
    preferredValue: Number(tr.querySelector(".q-value").value || 0),
    contactEmail: tr.querySelector(".q-email").value.trim(),
  })).filter((q) => q.service || q.quotaID);
}

function lines(value) {
  return value.split("\n").map((v) => v.trim()).filter(Boolean);
}

$("addQuota").addEventListener("click", () => addQuotaRow());

$("eventForm").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const button = ev.target.querySelector('button[type="submit"]');
  showError($("formError"), "");
  button.disabled = true;
  $("saveState").textContent = "保存中…";
  try {
    const updated = await api(`/api/events/${encodeURIComponent(current.code)}`, {
      method: "PUT",
      body: JSON.stringify({
        code: current.code,
        displayName: $("displayName").value.trim(),
        enabled: $("enabled").checked,
        maxAllocations: Number($("maxAllocations").value || 0),
        roles: lines($("roles").value),
        apis: lines($("apis").value),
        quotas: collectQuotas(),
      }),
    });
    current = updated;
    await loadEvents(updated.code);
    $("saveState").textContent = "保存しました";
  } catch (e) {
    $("saveState").textContent = "";
    showError($("formError"), e.message);
  } finally {
    button.disabled = false;
  }
});

$("ensureFolder").addEventListener("click", async () => {
  const button = $("ensureFolder");
  button.disabled = true;
  showError($("formError"), "");
  try {
    const updated = await api(`/api/events/${encodeURIComponent(current.code)}/folder`, { method: "POST" });
    current = updated;
    $("folderName").textContent = updated.folderName || "(未作成)";
    await loadEvents(updated.code);
  } catch (e) {
    showError($("formError"), e.message);
  } finally {
    button.disabled = false;
  }
});

$("deleteEvent").addEventListener("click", async () => {
  if (!confirm(`イベント ${current.code} の設定を削除します。払い出し済みの Project とフォルダは残ります。よろしいですか?`)) return;
  try {
    await api(`/api/events/${encodeURIComponent(current.code)}`, { method: "DELETE" });
    current = null;
    $("editor").hidden = true;
    await loadEvents();
  } catch (e) {
    showError($("formError"), e.message);
  }
});

// ---- 払い出し状況 ----

async function loadAllocations() {
  renderAllocations(await api(`/api/events/${encodeURIComponent(current.code)}/allocations`));
}

function renderAllocations(allocations) {
  const el = $("allocations");
  if (!allocations.length) {
    el.innerHTML = `<p class="empty">まだ払い出しはありません。</p>`;
    return;
  }
  el.innerHTML = `<table class="alloc-table"><tbody>${allocations.map((a) => `
    <tr>
      <td>${escapeHTML(a.userEmail)}</td>
      <td><code>${escapeHTML(a.projectID || "-")}</code></td>
      <td><span class="badge ${a.status}">${a.status}</span></td>
      <td class="meta">${escapeHTML(a.status === "READY" ? "" : a.error || a.step)}</td>
    </tr>`).join("")}</tbody></table>`;
}

const SHUTDOWN_POLL_INTERVAL_MS = 3000;
// Project の削除は 1 件ずつ順番に行うので、待ち時間の上限だけ決めておく。
const SHUTDOWN_POLL_TIMEOUT_MS = 15 * 60 * 1000;

let shutdownTimer = null;

// 削除がまだ終わっていない払い出しを数える。
function countShutdownTargets(allocations) {
  return allocations.filter((a) => a.projectID && a.status !== "SHUTDOWN").length;
}

async function pollShutdown(code, deadline) {
  const allocations = await api(`/api/events/${encodeURIComponent(code)}/allocations`);
  renderAllocations(allocations);

  const remaining = countShutdownTargets(allocations);
  const failed = allocations.filter((a) => a.status !== "SHUTDOWN" && a.error);
  const timedOut = Date.now() > deadline;
  const running = remaining > 0 && !timedOut;

  $("shutdownResult").innerHTML = `<div class="shutdown-summary">
    <h4>Shutdown ${running ? "実行中" : "完了"}</h4>
    <div class="meta">残り ${remaining} 件 / 削除依頼済み ${allocations.filter((a) => a.status === "SHUTDOWN").length} 件</div>
    ${failed.length ? `<ul>${failed.map((a) => `<li>${escapeHTML(a.projectID)}: ${escapeHTML(a.error)}</li>`).join("")}</ul>` : ""}
    ${timedOut && remaining > 0 ? `<div class="meta">時間がかかっています。Cloud Tasks のリトライを待つか、再読み込みで確認してください。</div>` : ""}
  </div>`;

  clearTimeout(shutdownTimer);
  if (running) {
    shutdownTimer = setTimeout(() => pollShutdown(code, deadline).catch(console.error), SHUTDOWN_POLL_INTERVAL_MS);
  } else {
    $("shutdownEvent").disabled = false;
  }
}

$("shutdownEvent").addEventListener("click", async () => {
  const code = current.code;
  const answer = prompt(
    `${code} で払い出した Project をすべて削除依頼状態にします。\n` +
    `イベントの払い出し受付も止まります。\n` +
    `実行するにはイベントコードを入力してください。`);
  if (answer !== code) return;

  $("shutdownEvent").disabled = true;
  $("shutdownResult").innerHTML = `<p class="meta">Shutdown を受け付けています…</p>`;
  try {
    const res = await api(`/api/events/${encodeURIComponent(code)}/shutdown`, { method: "POST" });
    $("shutdownResult").innerHTML = `<p class="meta">${res.targets} 件の Project を Shutdown します。</p>`;
    await loadEvents(code);
    // loadEvents はエディタを描き直してボタンを戻すので、ポーリング中は改めて止めておく。
    $("shutdownEvent").disabled = true;
    await pollShutdown(code, Date.now() + SHUTDOWN_POLL_TIMEOUT_MS);
  } catch (e) {
    $("shutdownResult").innerHTML = `<p class="error">${escapeHTML(e.message)}</p>`;
    $("shutdownEvent").disabled = false;
  }
});

$("reloadAllocations").addEventListener("click", () => {
  loadAllocations().catch((e) => {
    $("allocations").innerHTML = `<p class="error">${escapeHTML(e.message)}</p>`;
  });
});

// ---- イベント作成 ----

$("createEvent").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const button = ev.target.querySelector("button");
  showError($("createError"), "");
  button.disabled = true;
  try {
    const code = $("newCode").value.trim().toLowerCase();
    const created = await api("/api/events", {
      method: "POST",
      body: JSON.stringify({ code, displayName: code, enabled: false, maxAllocations: 0, roles: [], apis: [], quotas: [] }),
    });
    $("newCode").value = "";
    await loadEvents(created.code);
    selectEvent(created.code);
  } catch (e) {
    showError($("createError"), e.message);
    await loadEvents(current?.code);
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
  loadEvents().catch((e) => {
    $("eventList").innerHTML = `<p class="error">${escapeHTML(e.message)}</p>`;
  });
})();
