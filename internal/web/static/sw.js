// vx service worker: a "you are offline" page when a page cannot load, and
// Web Push notifications. It caches nothing else: vx pages always come
// from the server. Served at /ui/sw.js so its scope is /ui/.
"use strict";

var CACHE = "vx-offline-v1";
var OFFLINE = "/ui/offline";
var SHELL = [OFFLINE, "/ui/static/vx.css", "/ui/static/phone.css", "/ui/static/icon-192.png"];

self.addEventListener("install", function (e) {
  e.waitUntil(caches.open(CACHE).then(function (c) { return c.addAll(SHELL); }).then(function () { return self.skipWaiting(); }));
});

self.addEventListener("activate", function (e) {
  e.waitUntil(caches.keys().then(function (keys) {
    return Promise.all(keys.filter(function (k) { return k !== CACHE; }).map(function (k) { return caches.delete(k); }));
  }).then(function () { return self.clients.claim(); }));
});

self.addEventListener("fetch", function (e) {
  var r = e.request;
  if (r.method !== "GET") return;
  if (r.mode === "navigate") {
    e.respondWith(fetch(r).catch(function () { return caches.match(OFFLINE); }));
    return;
  }
  // The offline page's own styles and icon, when the network is gone.
  var u = new URL(r.url);
  if (u.origin === self.location.origin && SHELL.indexOf(u.pathname) >= 0) {
    e.respondWith(fetch(r).catch(function () { return caches.match(u.pathname); }));
  }
});

self.addEventListener("push", function (e) {
  var m = {};
  try { m = e.data ? e.data.json() : {}; } catch (err) { m = { body: e.data ? e.data.text() : "" }; }
  e.waitUntil(self.registration.showNotification(m.title || "vx", {
    body: m.body || "Something waits for you.",
    tag: m.tag || undefined,
    icon: "/ui/static/icon-192.png",
    badge: "/ui/static/icon-192.png",
    data: { url: m.url || "/ui/for-you" }
  }));
});

self.addEventListener("notificationclick", function (e) {
  e.notification.close();
  var target = new URL((e.notification.data && e.notification.data.url) || "/ui/for-you", self.location.origin);
  if (target.origin !== self.location.origin) target = new URL("/ui/for-you", self.location.origin);
  e.waitUntil(self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(function (list) {
    for (var i = 0; i < list.length; i++) {
      var c = list[i];
      if (new URL(c.url).origin === self.location.origin && "focus" in c) {
        return c.focus().then(function (w) { return w && "navigate" in w ? w.navigate(target.href) : w; });
      }
    }
    return self.clients.openWindow(target.href);
  }));
});
