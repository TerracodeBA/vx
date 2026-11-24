// vx web UI: live updates on the effort page and small layout helpers.
(function () {
  "use strict";

  // Bar widths come from data attributes (the page's CSP has no inline styles).
  document.querySelectorAll("[data-w]").forEach(function (el) {
    el.style.width = Math.max(2, Number(el.getAttribute("data-w")) || 0) + "%";
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
        if (t !== last) location.reload();
      })
      .catch(function () { live.classList.add("offline"); });
  }
  tick();
  setInterval(tick, 2500);
})();
