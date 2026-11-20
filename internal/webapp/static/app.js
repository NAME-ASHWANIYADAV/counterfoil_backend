// Counterfoil: light live-refresh. Pages that carry <main data-live="1">
// re-fetch themselves as a partial every 2.5 s, skipping refreshes while
// the person is typing or has a dialog open. No framework, no surprises.
(function () {
  var main = document.getElementById("main");
  if (!main || main.dataset.live !== "1") return;
  var timer = setInterval(function () {
    if (document.activeElement &&
        /INPUT|TEXTAREA|SELECT|BUTTON/.test(document.activeElement.tagName)) return;
    var url = new URL(window.location.href);
    url.searchParams.set("partial", "1");
    fetch(url.toString(), { headers: { "X-Partial": "1" } })
      .then(function (r) { return r.ok ? r.text() : null; })
      .then(function (html) {
        if (html === null) return;
        var sc = { x: window.scrollX, y: window.scrollY };
        var tmp = document.createElement("div");
        tmp.innerHTML = html;
        var next = tmp.querySelector("main") || tmp;
        main.innerHTML = next.innerHTML;
        window.scrollTo(sc.x, sc.y);
      })
      .catch(function () {});
  }, 2500);
  window.addEventListener("beforeunload", function () { clearInterval(timer); });
})();

// Keep chat scrolled to the latest message.
(function () {
  var log = document.querySelector(".chat .log");
  if (log) log.scrollTop = log.scrollHeight;
})();
