package main

import "strconv"

func buildLoaderJS(publicBase, queryKey string) string {
	return `(function () {
  const PATCH_BASE = ` + strconv.Quote(publicBase) + `;
  const PATCH_QUERY_KEY = ` + strconv.Quote(queryKey) + `;
  const AUTH_STORAGE_KEY = "cli-proxy-auth";
  const LANGUAGE_STORAGE_KEY = "cli-proxy-language";
  const THEME_STORAGE_KEY = "cli-proxy-theme";
  const STYLE_ID = "cpa-usage-patch-style";
  const HOST_CLASS = "cpa-usage-patch-host";
  const SECRET_SALT = "cli-proxy-api-webui::secure-storage";
  const USAGE_LABEL = "\u4f7f\u7528\u7edf\u8ba1";
  let lastSessionSignature = "";

  function normalizeApiBase(input) {
    let base = String(input || "").trim();
    if (!base) return "";
    base = base.replace(/\/?v0\/management\/?$/i, "");
    base = base.replace(/\/+$/g, "");
    if (!/^https?:\/\//i.test(base)) {
      base = "http://" + base;
    }
    return base;
  }

  function encodeText(text) {
    return new TextEncoder().encode(text);
  }

  function decodeText(bytes) {
    return new TextDecoder().decode(bytes);
  }

  function getKeyBytes() {
    try {
      return encodeText(SECRET_SALT + "|" + window.location.host + "|" + navigator.userAgent);
    } catch (_) {
      return encodeText(SECRET_SALT);
    }
  }

  function xorBytes(data, keyBytes) {
    const result = new Uint8Array(data.length);
    for (let i = 0; i < data.length; i += 1) {
      result[i] = data[i] ^ keyBytes[i % keyBytes.length];
    }
    return result;
  }

  function fromBase64(base64) {
    const binary = atob(base64);
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i += 1) {
      bytes[i] = binary.charCodeAt(i);
    }
    return bytes;
  }

  function deobfuscate(payload) {
    if (!payload || !payload.startsWith("enc::v1::")) {
      return payload;
    }
    try {
      const encrypted = fromBase64(payload.slice("enc::v1::".length));
      const decrypted = xorBytes(encrypted, getKeyBytes());
      return decodeText(decrypted);
    } catch (_) {
      return payload;
    }
  }

  function parseStoredJson(key) {
    try {
      const raw = localStorage.getItem(key);
      if (!raw) return null;
      return JSON.parse(raw);
    } catch (_) {
      return null;
    }
  }

  function readSessionFromStoredRaw(raw) {
    try {
      if (!raw) return null;
      const decoded = deobfuscate(raw);
      const parsed = JSON.parse(decoded);
      const state = parsed && typeof parsed === "object" && parsed.state ? parsed.state : parsed;
      const apiBase = normalizeApiBase(state && state.apiBase);
      const managementKey = typeof (state && state.managementKey) === "string"
        ? state.managementKey.trim()
        : "";
      if (!apiBase && !managementKey) return null;
      return { apiBase, managementKey };
    } catch (_) {
      return null;
    }
  }

  function readStoredSession() {
    try {
      return readSessionFromStoredRaw(localStorage.getItem(AUTH_STORAGE_KEY));
    } catch (_) {
      return null;
    }
  }

  function readStoredLanguage() {
    const value = parseStoredJson(LANGUAGE_STORAGE_KEY);
    return typeof value === "string" ? value : "";
  }

  function readStoredTheme() {
    const value = parseStoredJson(THEME_STORAGE_KEY);
    return typeof value === "string" ? value : "";
  }

  function extractBearer(value) {
    if (typeof value !== "string") return "";
    const match = value.match(/^Bearer\s+(.+)$/i);
    return match ? match[1].trim() : "";
  }

  function extractHeader(headers, headerName) {
    if (!headers) return "";
    const target = headerName.toLowerCase();
    if (typeof Headers !== "undefined" && headers instanceof Headers) {
      return headers.get(headerName) || "";
    }
    if (Array.isArray(headers)) {
      for (const entry of headers) {
        if (Array.isArray(entry) && String(entry[0] || "").toLowerCase() === target) {
          return String(entry[1] || "");
        }
      }
      return "";
    }
    if (typeof headers === "object") {
      for (const key of Object.keys(headers)) {
        if (key.toLowerCase() === target) {
          return String(headers[key] || "");
        }
      }
    }
    return "";
  }

  function publishSession(apiBase, managementKey) {
    const normalizedBase = normalizeApiBase(apiBase);
    const normalizedKey = String(managementKey || "").trim();
    if (!normalizedBase) return;
    const signature = normalizedBase + "::" + normalizedKey;
    if (signature === lastSessionSignature) return;
    lastSessionSignature = signature;

    fetch(PATCH_BASE + "/api/session", {
      method: "POST",
      mode: "cors",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        api_base: normalizedBase,
        management_key: normalizedKey
      })
    }).catch(function () {
      lastSessionSignature = "";
    });
  }

  function maybeCapture(urlLike, authHeader) {
    try {
      const resolved = new URL(urlLike, window.location.href);
      if (resolved.pathname.indexOf("/v0/management/") === -1) return;
      publishSession(resolved.origin, extractBearer(authHeader));
    } catch (_) {
      // Ignore malformed URLs.
    }
  }

  function patchFetch() {
    if (typeof window.fetch !== "function" || window.__cpaUsagePatchFetchWrapped) return;
    const originalFetch = window.fetch;
    window.fetch = function patchedFetch(input, init) {
      try {
        const inputUrl = typeof input === "string" ? input : (input && input.url) || "";
        const inputHeaders = input && input.headers ? input.headers : null;
        const authHeader =
          extractHeader(init && init.headers, "Authorization") ||
          extractHeader(inputHeaders, "Authorization");
        maybeCapture(inputUrl, authHeader);
      } catch (_) {
        // Ignore capture failures.
      }
      return originalFetch.apply(this, arguments);
    };
    window.__cpaUsagePatchFetchWrapped = true;
  }

  function patchXHR() {
    if (window.__cpaUsagePatchXHRWrapped) return;
    const originalOpen = XMLHttpRequest.prototype.open;
    const originalSetRequestHeader = XMLHttpRequest.prototype.setRequestHeader;
    const originalSend = XMLHttpRequest.prototype.send;

    XMLHttpRequest.prototype.open = function patchedOpen(method, url) {
      this.__cpaUsagePatchUrl = url;
      this.__cpaUsagePatchAuth = "";
      return originalOpen.apply(this, arguments);
    };

    XMLHttpRequest.prototype.setRequestHeader = function patchedSetRequestHeader(name, value) {
      if (String(name || "").toLowerCase() === "authorization") {
        this.__cpaUsagePatchAuth = value;
      }
      return originalSetRequestHeader.apply(this, arguments);
    };

    XMLHttpRequest.prototype.send = function patchedSend(body) {
      try {
        maybeCapture(this.__cpaUsagePatchUrl || "", this.__cpaUsagePatchAuth || "");
      } catch (_) {
        // Ignore capture failures.
      }
      return originalSend.call(this, body);
    };

    window.__cpaUsagePatchXHRWrapped = true;
  }

  function patchStorage() {
    if (window.__cpaUsagePatchStorageWrapped) return;
    if (typeof Storage === "undefined" || !Storage.prototype || typeof Storage.prototype.setItem !== "function") {
      return;
    }

    const originalSetItem = Storage.prototype.setItem;
    Storage.prototype.setItem = function patchedSetItem(key, value) {
      const result = originalSetItem.apply(this, arguments);
      try {
        if (this === window.localStorage && String(key) === AUTH_STORAGE_KEY) {
          const stored = readSessionFromStoredRaw(String(value || ""));
          if (stored) {
            publishSession(stored.apiBase, stored.managementKey);
          }
        }
      } catch (_) {
        // Ignore storage capture failures.
      }
      return result;
    };

    window.__cpaUsagePatchStorageWrapped = true;
  }

  function currentUrl() {
    return new URL(window.location.href);
  }

  function isPatchActive() {
    return currentUrl().searchParams.get(PATCH_QUERY_KEY) === "1";
  }

  function setPatchActive(active) {
    const url = currentUrl();
    if (active) {
      url.searchParams.set(PATCH_QUERY_KEY, "1");
    } else {
      url.searchParams.delete(PATCH_QUERY_KEY);
    }
    history.pushState(null, "", url.toString());
    render();
  }

  function clearPatchActive() {
    if (!isPatchActive()) return;
    const url = currentUrl();
    url.searchParams.delete(PATCH_QUERY_KEY);
    history.replaceState(null, "", url.toString());
  }

  function buildUsageEmbedSrc() {
    const url = new URL(PATCH_BASE + "/usage-embed");
    const language = readStoredLanguage();
    const theme = readStoredTheme();
    if (language) url.searchParams.set("lang", language);
    if (theme) url.searchParams.set("theme", theme);
    return url.toString();
  }

  function injectStyles() {
    if (document.getElementById(STYLE_ID)) return;
    const style = document.createElement("style");
    style.id = STYLE_ID;
    style.textContent = [
      ".main-content.cpa-usage-patch-active {",
      "  position: relative !important;",
      "}",
      "",
      ".nav-section.cpa-usage-patch-nav-active .nav-item.active:not([data-cpa-usage-patch='true']) {",
      "  background: transparent !important;",
      "  color: var(--text-primary) !important;",
      "  border-color: transparent !important;",
      "  box-shadow: none !important;",
      "  transform: none !important;",
      "}",
      "",
      ".nav-section.cpa-usage-patch-nav-active .nav-item.active:not([data-cpa-usage-patch='true']) .nav-icon {",
      "  color: currentColor !important;",
      "  background: linear-gradient(180deg, rgb(255 255 255 / 0.12), rgb(255 255 255 / 0)), color-mix(in srgb, var(--bg-secondary) 84%, transparent) !important;",
      "  box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--border-primary) 82%, transparent) !important;",
      "}",
      "",
      ".main-content.cpa-usage-patch-active > *:not(." + HOST_CLASS + ") {",
      "  display: none !important;",
      "}",
      "",
      "." + HOST_CLASS + " {",
      "  display: none;",
      "  min-height: calc(100vh - 160px);",
      "  width: 100%;",
      "}",
      "",
      "." + HOST_CLASS + " .cpa-usage-patch-frame {",
      "  width: 100%;",
      "  min-height: calc(100vh - 160px);",
      "  border: 0;",
      "  background: transparent;",
      "}",
      ""
    ].join("\n");
    document.head.appendChild(style);
  }

  function usageNavMarkup() {
    return "" +
      '<span class="nav-icon" aria-hidden="true">' +
      '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">' +
      '<path d="M4 19h16"/>' +
      '<path d="M7 16V9"/>' +
      '<path d="M12 16V5"/>' +
      '<path d="M17 16v-3"/>' +
      '<path d="M5 19V8a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2v11"/>' +
      '</svg>' +
      '</span>' +
      '<span class="nav-label">' + USAGE_LABEL + "</span>";
  }

  function ensureNavItem() {
    const navSection = document.querySelector(".nav-section");
    if (!navSection) return;

    let navItem = navSection.querySelector("[data-cpa-usage-patch='true']");
    if (!navItem) {
      navItem = document.createElement("a");
      navItem.setAttribute("href", "#");
      navItem.setAttribute("data-cpa-usage-patch", "true");
      navItem.className = "nav-item";
      navItem.setAttribute("title", USAGE_LABEL);
      navItem.innerHTML = usageNavMarkup();
      navItem.addEventListener("click", function (event) {
        event.preventDefault();
        const stored = readStoredSession();
        if (stored) {
          publishSession(stored.apiBase, stored.managementKey);
        }
        setPatchActive(true);
      });

      const navItems = Array.from(navSection.querySelectorAll(".nav-item"));
      const quotaItem = navItems.find(function (item) {
        const href = String(item.getAttribute("href") || "");
        return href.indexOf("/quota") !== -1;
      });
      if (quotaItem && quotaItem.nextSibling) {
        navSection.insertBefore(navItem, quotaItem.nextSibling);
      } else if (quotaItem) {
        navSection.appendChild(navItem);
      } else {
        navSection.appendChild(navItem);
      }
    }

    navItem.classList.toggle("active", isPatchActive());
    navSection.classList.toggle("cpa-usage-patch-nav-active", isPatchActive());
  }

  function ensureOtherNavHooks() {
    const items = document.querySelectorAll(".nav-section .nav-item[href]");
    items.forEach(function (item) {
      if (item.getAttribute("data-cpa-usage-patch") === "true") return;
      if (item.__cpaUsagePatchBound) return;
      item.addEventListener("click", function () {
        clearPatchActive();
      });
      item.__cpaUsagePatchBound = true;
    });
  }

  function ensureHost() {
    const mainContent = document.querySelector(".main-content");
    if (!mainContent) return;

    let host = null;
    for (const child of Array.from(mainContent.children)) {
      if (child.classList && child.classList.contains(HOST_CLASS)) {
        host = child;
        break;
      }
    }

    if (!host) {
      host = document.createElement("div");
      host.className = HOST_CLASS;

      const iframe = document.createElement("iframe");
      iframe.className = "cpa-usage-patch-frame";
      iframe.setAttribute("title", "CPA Usage Statistics");
      iframe.setAttribute("referrerpolicy", "no-referrer");
      iframe.setAttribute("loading", "eager");
      host.appendChild(iframe);
      mainContent.appendChild(host);
    }

    const iframe = host.querySelector("iframe");
    const active = isPatchActive();
    mainContent.classList.toggle("cpa-usage-patch-active", active);
    if (!iframe) return;

    if (active) {
      const nextSrc = buildUsageEmbedSrc();
      if (iframe.getAttribute("src") !== nextSrc) {
        iframe.setAttribute("src", nextSrc);
      }
      host.style.display = "block";
    } else {
      host.style.display = "none";
    }
  }

  function render() {
    injectStyles();
    const stored = readStoredSession();
    if (stored) {
      publishSession(stored.apiBase, stored.managementKey);
    }
    ensureNavItem();
    ensureOtherNavHooks();
    ensureHost();
  }

  patchFetch();
  patchXHR();
  patchStorage();
  render();
  setInterval(render, 1000);
  window.addEventListener("hashchange", render);
  window.addEventListener("popstate", render);
  document.addEventListener("visibilitychange", function () {
    if (!document.hidden) {
      render();
    }
  });
})();`
}
