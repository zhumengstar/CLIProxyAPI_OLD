(() => {
  const widgetId = "cpa-batch-reset-quota-widget";
  const securePrefix = "enc::v1::";
  const secureKeyName = "cli-proxy-api-webui::secure-storage";

  function bytes(text) {
    return new TextEncoder().encode(text);
  }

  function text(data) {
    return new TextDecoder().decode(data);
  }

  function secureKey() {
    try {
      return bytes(`${secureKeyName}|${window.location.host}|${navigator.userAgent}`);
    } catch {
      return bytes(secureKeyName);
    }
  }

  function xor(data, key) {
    const out = new Uint8Array(data.length);
    for (let i = 0; i < data.length; i += 1) {
      out[i] = data[i] ^ key[i % key.length];
    }
    return out;
  }

  function decodeStored(value) {
    if (!value) {
      return "";
    }
    let raw = value;
    try {
      if (raw.startsWith(securePrefix)) {
        const bin = atob(raw.slice(securePrefix.length));
        const data = new Uint8Array(bin.length);
        for (let i = 0; i < bin.length; i += 1) {
          data[i] = bin.charCodeAt(i);
        }
        raw = text(xor(data, secureKey()));
      }
      const parsed = JSON.parse(raw);
      return typeof parsed === "string" ? parsed : "";
    } catch {
      return raw.startsWith(securePrefix) ? "" : raw;
    }
  }

  function stored(name) {
    try {
      return decodeStored(localStorage.getItem(name));
    } catch {
      return "";
    }
  }

  function apiBase() {
    const saved = stored("apiBase") || stored("apiUrl");
    if (saved) {
      const base = saved.replace(/\/+$/, "");
      return /\/v0\/management$/.test(base) ? base : `${base}/v0/management`;
    }
    return `${window.location.origin}/v0/management`;
  }

  async function resetQuota({ provider, page, pageSize, key }) {
    const res = await fetch(`${apiBase()}/auth-files/reset-quota`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${key}`,
      },
      body: JSON.stringify({ provider, page, page_size: pageSize }),
    });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) {
      throw new Error(body.error || `HTTP ${res.status}`);
    }
    return body;
  }

  function show() {
    if (document.getElementById(widgetId)) {
      return;
    }
    const box = document.createElement("section");
    box.id = widgetId;
    box.style.cssText = "position:fixed;right:18px;bottom:18px;z-index:2147483647;width:320px;max-width:calc(100vw - 36px);padding:12px;border:1px solid #8b868066;border-radius:12px;background:var(--bg-primary,#fff);color:var(--text-primary,#111);box-shadow:0 12px 32px #0003;font:13px system-ui,-apple-system,BlinkMacSystemFont,Segoe UI,sans-serif";
    box.innerHTML = `
      <div style="display:flex;align-items:center;justify-content:space-between;gap:8px;margin-bottom:8px">
        <strong>批量刷新额度</strong>
        <button type="button" data-close style="border:0;background:transparent;color:inherit;cursor:pointer;font-size:18px;line-height:1">×</button>
      </div>
      <label style="display:block;margin:6px 0">Provider
        <input data-provider value="antigravity" style="box-sizing:border-box;width:100%;margin-top:4px;padding:6px 8px;border:1px solid #8b868066;border-radius:8px;background:transparent;color:inherit">
      </label>
      <div style="display:grid;grid-template-columns:1fr 1fr;gap:8px">
        <label>Page
          <input data-page type="number" min="1" value="1" style="box-sizing:border-box;width:100%;margin-top:4px;padding:6px 8px;border:1px solid #8b868066;border-radius:8px;background:transparent;color:inherit">
        </label>
        <label>Page size
          <input data-page-size type="number" min="1" max="100" value="100" style="box-sizing:border-box;width:100%;margin-top:4px;padding:6px 8px;border:1px solid #8b868066;border-radius:8px;background:transparent;color:inherit">
        </label>
      </div>
      <label style="display:block;margin:6px 0">Management key（自动读取失败时填写）
        <input data-key type="password" placeholder="默认读取已登录 key" style="box-sizing:border-box;width:100%;margin-top:4px;padding:6px 8px;border:1px solid #8b868066;border-radius:8px;background:transparent;color:inherit">
      </label>
      <div style="display:flex;gap:8px;margin-top:10px">
        <button type="button" data-run-page style="flex:1;padding:7px 10px;border:1px solid #8b868066;border-radius:8px;background:#8b86801f;color:inherit;cursor:pointer">刷新当前批次</button>
        <button type="button" data-run-all style="flex:1;padding:7px 10px;border:1px solid #8b868066;border-radius:8px;background:#8b86801f;color:inherit;cursor:pointer">一键刷新全部</button>
      </div>
      <pre data-output style="white-space:pre-wrap;max-height:180px;overflow:auto;margin:10px 0 0;padding:8px;border-radius:8px;background:#8b868012;color:inherit"></pre>
    `;
    document.body.appendChild(box);

    const $ = (selector) => box.querySelector(selector);
    const output = $("[data-output]");
    const keyInput = $("[data-key]");
    const getKey = () => keyInput.value.trim() || stored("managementKey");
    const getProvider = () => $("[data-provider]").value.trim() || "antigravity";
    const getPage = () => Math.max(1, Number($("[data-page]").value) || 1);
    const getPageSize = () => Math.max(1, Math.min(100, Number($("[data-page-size]").value) || 100));
    const log = (message) => {
      output.textContent = message;
    };

    $("[data-close]").onclick = () => box.remove();
    $("[data-run-page]").onclick = async () => {
      const key = getKey();
      if (!key) {
        log("缺少 management key，请手动填写。");
        return;
      }
      try {
        const result = await resetQuota({ provider: getProvider(), page: getPage(), pageSize: getPageSize(), key });
        log(`完成：成功 ${result.succeeded || 0}，失败 ${result.failed || 0}，total ${result.total || 0}，has_more=${!!result.has_more}`);
      } catch (err) {
        log(`失败：${err instanceof Error ? err.message : String(err)}`);
      }
    };
    $("[data-run-all]").onclick = async () => {
      const key = getKey();
      if (!key) {
        log("缺少 management key，请手动填写。");
        return;
      }
      const provider = getProvider();
      const pageSize = getPageSize();
      let page = getPage();
      let succeeded = 0;
      let failed = 0;
      try {
        for (;;) {
          log(`正在刷新 ${provider} 第 ${page} 页，每页 ${pageSize}...`);
          const result = await resetQuota({ provider, page, pageSize, key });
          succeeded += result.succeeded || 0;
          failed += result.failed || 0;
          if (!result.has_more) {
            break;
          }
          page += 1;
        }
        log(`全部完成：成功 ${succeeded}，失败 ${failed}。`);
      } catch (err) {
        log(`第 ${page} 页失败：${err instanceof Error ? err.message : String(err)}；已成功 ${succeeded}，失败 ${failed}`);
      }
    };
  }

  function maybeShow() {
    if (localStorage.getItem("isLoggedIn") === "true") {
      show();
    }
  }

  window.addEventListener("load", maybeShow);
  window.addEventListener("popstate", maybeShow);
  setTimeout(maybeShow, 1200);
})();
