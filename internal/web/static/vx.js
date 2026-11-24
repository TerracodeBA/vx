// vx web UI: live updates on the effort page and small layout helpers.
(function () {
  "use strict";

  // Bar widths come from data attributes (the page's CSP has no inline styles).
  document.querySelectorAll("[data-w]").forEach(function (el) {
    el.style.width = Math.max(2, Number(el.getAttribute("data-w")) || 0) + "%";
  });

  // Sidebar drawer on narrow screens.
  var menu = document.querySelector("[data-menu]");
  if (menu) {
    menu.addEventListener("click", function (e) {
      e.stopPropagation();
      document.body.classList.toggle("menu-open");
    });
    document.addEventListener("click", function (e) {
      if (document.body.classList.contains("menu-open") && !e.target.closest(".sidebar")) document.body.classList.remove("menu-open");
    });
  }

  function store(k, v) { try { if (v === undefined) return localStorage.getItem(k); localStorage.setItem(k, v); } catch (e) { return null; } }
  function unstore(k) { try { localStorage.removeItem(k); } catch (e) {} }

  // Unified or split diffs, remembered per viewer.
  function setMode(mode) {
    document.documentElement.setAttribute("data-diff", mode);
    document.querySelectorAll("[data-diff-mode]").forEach(function (b) {
      b.setAttribute("aria-pressed", String(b.getAttribute("data-diff-mode") === mode));
    });
  }
  setMode(store("vx:diff") === "split" ? "split" : "unified");
  document.querySelectorAll("[data-diff-mode]").forEach(function (b) {
    b.addEventListener("click", function () { var m = b.getAttribute("data-diff-mode"); store("vx:diff", m); setMode(m); });
  });

  // Diffs load when a file is first opened.
  function load(d) {
    if (!d.hasAttribute("data-src") || d.getAttribute("data-loaded")) return;
    d.setAttribute("data-loaded", "1");
    var body = d.querySelector(".fd-body");
    fetch(d.getAttribute("data-src"), { credentials: "same-origin" })
      .then(function (r) { if (!r.ok) throw new Error("HTTP " + r.status); return r.text(); })
      .then(function (html) { body.innerHTML = html; })
      .catch(function (e) { body.innerHTML = '<p class="muted small pad">Could not load this diff (' + e.message + ').</p>'; d.removeAttribute("data-loaded"); });
  }
  document.querySelectorAll("details.fd").forEach(function (d) {
    d.addEventListener("toggle", function () { if (d.open) load(d); });
    if (d.open) load(d);
  });

  // "Show code" opens every file of one change; "Expand all" every file.
  document.querySelectorAll("[data-expand]").forEach(function (b) {
    b.addEventListener("click", function () {
      var land = b.closest(".land");
      var files = land.querySelectorAll("details.fd");
      var open = b.getAttribute("data-open") !== "1";
      files.forEach(function (d) { d.open = open; });
      b.setAttribute("data-open", open ? "1" : "0");
      b.textContent = open ? "Hide code" : "Show code";
    });
  });
  document.querySelectorAll("[data-expand-all]").forEach(function (b) {
    b.addEventListener("click", function () {
      var open = b.getAttribute("data-open") !== "1";
      document.querySelectorAll(".all-files details.fd").forEach(function (d) {
        if (!open || !d.classList.contains("is-viewed")) d.open = open;
      });
      b.setAttribute("data-open", open ? "1" : "0");
      b.textContent = open ? "Collapse all" : "Expand all";
    });
  });

  // Jump links open the file they point to.
  document.querySelectorAll(".jump a").forEach(function (a) {
    a.addEventListener("click", function () {
      var d = document.getElementById(decodeURIComponent(a.hash.slice(1)));
      if (!d) return;
      d.open = true;
      requestAnimationFrame(function () { d.scrollIntoView({ block: "start" }); });
    });
  });

  // "Viewed" collapses a file and remembers it for this version of the file.
  document.querySelectorAll("details.filecard").forEach(function (d) {
    var box = d.querySelector("[data-viewed]");
    if (!box) return;
    var key = "vx:viewed:" + d.getAttribute("data-key");
    if (store(key) === "1") { box.checked = true; d.classList.add("is-viewed"); d.open = false; }
    box.closest("label").addEventListener("click", function (e) { e.stopPropagation(); });
    box.addEventListener("change", function () {
      d.classList.toggle("is-viewed", box.checked);
      if (box.checked) { store(key, "1"); d.open = false; } else { unstore(key); d.open = true; }
    });
  });

  // Highlight changes that landed since this viewer last looked.
  var live = document.querySelector("[data-poll]");
  if (!live) return;
  var key = "vx:seen:" + live.getAttribute("data-key");
  var top = live.getAttribute("data-top");
  var seen = null;
  try { seen = sessionStorage.getItem(key); } catch (e) {}
  if (seen && top && seen !== top) {
    var items = document.querySelectorAll(".timeline .land");
    for (var i = 0; i < items.length; i++) {
      if (items[i].getAttribute("data-hash") === seen) break;
      items[i].classList.add("fresh");
    }
  }
  try { if (top) sessionStorage.setItem(key, top); } catch (e) {}

  // Poll the effort head; reload when an agent lands or opens a workspace.
  var url = live.getAttribute("data-poll");
  var last = null;
  function tick() {
    if (document.hidden) return;
    fetch(url, { cache: "no-store", credentials: "same-origin" })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.text(); })
      .then(function (t) {
        live.classList.remove("offline");
        if (last === null) { last = t; return; }
        if (t === last) return;
        // Reading a diff? Offer the reload instead of losing the open files.
        if (document.querySelector("details.fd[open]")) {
          if (!document.querySelector(".fresh-bar")) {
            var bar = document.createElement("button");
            bar.type = "button";
            bar.className = "fresh-bar";
            bar.textContent = "New changes landed. Show them";
            bar.addEventListener("click", function () { location.reload(); });
            document.body.appendChild(bar);
          }
          return;
        }
        location.reload();
      })
      .catch(function () { live.classList.add("offline"); });
  }
  tick();
  setInterval(tick, 2500);
})();
