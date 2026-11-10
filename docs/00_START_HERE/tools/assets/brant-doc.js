(function () {
  "use strict";
  var $ = function (s, r) { return (r || document).querySelector(s); };
  var $$ = function (s, r) { return Array.prototype.slice.call((r || document).querySelectorAll(s)); };
  function esc(s) { return String(s).replace(/[&<>"]/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]; }); }

  var sidebar = $(".sidebar");
  var menu = $(".mobile-menu");
  if (menu && sidebar) {
    menu.addEventListener("click", function () {
      var open = sidebar.classList.toggle("open");
      menu.setAttribute("aria-expanded", open ? "true" : "false");
    });
  }
  $$(".nav-scroll a").forEach(function (a) {
    a.addEventListener("click", function () { if (sidebar) { sidebar.classList.remove("open"); } });
  });

  var printBtn = $("#print-doc");
  if (printBtn) {
    printBtn.addEventListener("click", function () {
      $$("details").forEach(function (d) { d.open = true; });
      window.print();
    });
  }
  var expandBtn = $("#expand-all");
  if (expandBtn) {
    expandBtn.addEventListener("click", function () {
      var ds = $$("details");
      var anyClosed = ds.some(function (d) { return !d.open; });
      ds.forEach(function (d) { d.open = anyClosed; });
    });
  }

  $$(".copy-code").forEach(function (b) {
    b.addEventListener("click", function () {
      var code = b.parentNode.querySelector("code");
      if (code && navigator.clipboard) { navigator.clipboard.writeText(code.textContent); b.textContent = "已复制"; }
    });
  });

  var progress = $(".progress");
  var sections = $$("section[id]");
  var links = {};
  $$(".nav-scroll a").forEach(function (a) { links[a.getAttribute("href").slice(1)] = a; });
  function onScroll() {
    var h = document.documentElement;
    var max = h.scrollHeight - h.clientHeight;
    if (progress) { progress.style.width = (max > 0 ? (h.scrollTop / max) * 100 : 0) + "%"; }
    var current = null;
    sections.forEach(function (s) { if (s.getBoundingClientRect().top < 130) { current = s.id; } });
    Object.keys(links).forEach(function (id) { links[id].classList.toggle("active", id === current); });
  }
  document.addEventListener("scroll", onScroll, { passive: true });
  onScroll();

  var input = $("#doc-search");
  var results = $(".search-results");
  if (input && results) {
    var index = sections.map(function (s) {
      var h = s.querySelector("h2");
      return { id: s.id, title: s.getAttribute("data-search-title") || (h ? h.textContent : s.id), text: s.textContent.replace(/\s+/g, " ") };
    });
    input.addEventListener("input", function () {
      var q = input.value.trim().toLowerCase();
      if (!q) { results.hidden = true; results.innerHTML = ""; return; }
      var hits = [];
      for (var i = 0; i < index.length && hits.length < 30; i++) {
        var pos = index[i].text.toLowerCase().indexOf(q);
        if (pos >= 0) { hits.push({ e: index[i], snip: index[i].text.slice(Math.max(0, pos - 24), pos + 52) }); }
      }
      results.innerHTML = hits.length ? hits.map(function (x) {
        return '<a href="#' + x.e.id + '"><b>' + esc(x.e.title) + "</b><br><span>…" + esc(x.snip) + "…</span></a>";
      }).join("") : "<p>没有匹配内容</p>";
      results.hidden = false;
    });
    document.addEventListener("click", function (ev) {
      if (!results.contains(ev.target) && ev.target !== input) { results.hidden = true; }
    });
  }

  $$("input[data-filter]").forEach(function (inp) {
    var table = document.getElementById(inp.getAttribute("data-filter"));
    if (!table) { return; }
    var counter = document.querySelector('[data-filter-count="' + inp.getAttribute("data-filter") + '"]');
    inp.addEventListener("input", function () {
      var q = inp.value.trim().toLowerCase();
      var n = 0;
      $$("tbody tr", table).forEach(function (tr) {
        var show = !q || tr.textContent.toLowerCase().indexOf(q) >= 0;
        tr.hidden = !show;
        if (show) { n++; }
      });
      if (counter) { counter.textContent = String(n); }
    });
  });
})();
