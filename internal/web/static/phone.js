// vx web UI on phones: the service worker (offline page, notifications)
// and the per-browser notification switch in Settings.
(function () {
  "use strict";
  if (!("serviceWorker" in navigator)) { pushPanel(null); return; }
  var ready = navigator.serviceWorker.register("/ui/sw.js", { scope: "/ui/" })
    .then(function () { return navigator.serviceWorker.ready; })
    .catch(function () { return null; });
  pushPanel(ready);

  function pushPanel(ready) {
    var panel = document.querySelector("[data-push]");
    if (!panel) return;
    var state = panel.querySelector("[data-push-state]");
    var on = panel.querySelector("[data-push-on]");
    var off = panel.querySelector("[data-push-off]");
    var test = panel.querySelector("[data-push-test]");
    function say(text) { state.textContent = text; }
    function show(subscribed) {
      on.hidden = subscribed;
      off.hidden = !subscribed;
      test.hidden = !subscribed;
    }
    if (!ready || !("PushManager" in window) || !("Notification" in window)) {
      say("This browser cannot show notifications from vx. On iPhone and iPad, add vx to the Home Screen first (Share, then Add to Home Screen) and open it from there.");
      return;
    }
    function post(path, body) {
      return fetch(path, {
        method: "POST", credentials: "same-origin",
        headers: { "Accept": "application/json" },
        body: new URLSearchParams(body)
      }).then(function (r) {
        return r.json().catch(function () { return {}; }).then(function (j) {
          if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
          return j;
        });
      });
    }
    function b64(buf) {
      var s = String.fromCharCode.apply(null, new Uint8Array(buf));
      return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
    }
    function bytes(b64url) {
      var s = atob(b64url.replace(/-/g, "+").replace(/_/g, "/"));
      var out = new Uint8Array(s.length);
      for (var i = 0; i < s.length; i++) out[i] = s.charCodeAt(i);
      return out;
    }
    function device() {
      var ua = navigator.userAgent;
      var b = /Edg\//.test(ua) ? "Edge" : /Firefox\//.test(ua) ? "Firefox" : /Chrome\//.test(ua) ? "Chrome" : /Safari\//.test(ua) ? "Safari" : "Browser";
      var os = /Android/.test(ua) ? "Android" : /iPhone|iPad/.test(ua) ? "iOS" : /Mac OS X/.test(ua) ? "macOS" : /Windows/.test(ua) ? "Windows" : /Linux/.test(ua) ? "Linux" : "";
      return os ? b + " on " + os : b;
    }
    function save(sub) {
      return post("/ui/push/subscribe", { endpoint: sub.endpoint, p256dh: b64(sub.getKey("p256dh")), auth: b64(sub.getKey("auth")), device: device() });
    }
    var reg = null;
    ready.then(function (r) {
      reg = r;
      if (!reg) { say("Notifications need the vx service worker, which did not start in this browser."); return null; }
      return reg.pushManager.getSubscription();
    }).then(function (sub) {
      if (!reg) return;
      if (sub) {
        // Keep the server's copy current: browsers renew subscriptions.
        save(sub).catch(function () {});
        say("Notifications are on in this browser.");
        show(true);
      } else if (Notification.permission === "denied") {
        say("This browser blocks notifications from vx. Allow them in the browser's site settings, then come back.");
        show(false);
        on.hidden = true;
      } else {
        say("Notifications are off in this browser.");
        show(false);
      }
    });
    on.addEventListener("click", function () {
      on.disabled = true;
      say("Asking the browser...");
      Notification.requestPermission().then(function (p) {
        if (p !== "granted") throw new Error("the browser did not allow notifications");
        return fetch("/ui/push/key", { credentials: "same-origin", headers: { "Accept": "application/json" } });
      }).then(function (r) {
        return r.json().then(function (j) { if (!r.ok) throw new Error(j.error || ("HTTP " + r.status)); return j.key; });
      }).then(function (key) {
        return reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: bytes(key) });
      }).then(save).then(function () {
        location.reload();
      }).catch(function (e) {
        say("Could not turn notifications on: " + e.message + ".");
        on.disabled = false;
      });
    });
    off.addEventListener("click", function () {
      off.disabled = true;
      reg.pushManager.getSubscription().then(function (sub) {
        if (!sub) return null;
        var endpoint = sub.endpoint;
        return sub.unsubscribe().then(function () { return post("/ui/push/unsubscribe", { endpoint: endpoint }); });
      }).then(function () {
        location.reload();
      }).catch(function (e) {
        say("Could not turn notifications off: " + e.message + ".");
        off.disabled = false;
      });
    });
    test.addEventListener("click", function () {
      test.disabled = true;
      reg.pushManager.getSubscription().then(function (sub) {
        if (!sub) throw new Error("notifications are off in this browser");
        return post("/ui/push/test", { endpoint: sub.endpoint });
      }).then(function () {
        say("Test sent. It should arrive in a few seconds.");
      }).catch(function (e) {
        say("The test did not go out: " + e.message + ".");
      }).then(function () { test.disabled = false; });
    });
  }
})();
