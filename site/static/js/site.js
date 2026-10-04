// Bearing site: search dialog, "on this page" highlighting and the roadmap
// filter's deep links. Every page works without this file.
(() => {
  "use strict";

  const body = document.body;
  const isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
  document.querySelectorAll("[data-kbd]").forEach((k) => { k.textContent = isMac ? "⌘K" : "Ctrl K"; });

  // ---------- Search ----------
  const dialog = document.getElementById("search");
  const input = document.getElementById("search-input");
  const list = document.getElementById("search-results");
  const empty = dialog && dialog.querySelector(".cmdk__empty");
  let index = null;
  let loading = null;
  let active = 0;

  const load = () => {
    if (index) return Promise.resolve(index);
    if (!loading) {
      loading = fetch(body.dataset.search)
        .then((r) => (r.ok ? r.json() : []))
        .then((data) => {
          index = data.map((e) => ({ ...e, hay: [e.t, e.h, e.x, e.s].join(" ").toLowerCase() }));
          return index;
        })
        .catch(() => (index = []));
    }
    return loading;
  };

  const escape = (s) => s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
  // Splits the raw text on the terms and escapes each piece, so a term can
  // never match inside an entity or a <mark> added for another term.
  const highlight = (text, terms) => {
    const words = terms.filter((t) => t.length >= 2).map((t) => t.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));
    if (!words.length) return escape(text);
    return text
      .split(new RegExp("(" + words.join("|") + ")", "ig"))
      .map((part, i) => (i % 2 ? "<mark>" + escape(part) + "</mark>" : escape(part)))
      .join("");
  };

  const score = (e, terms, q) => {
    let s = 0;
    for (const t of terms) {
      if (!e.hay.includes(t)) return 0;
      const title = (e.h || e.t).toLowerCase();
      if (title.startsWith(t)) s += 6;
      else if (title.includes(t)) s += 4;
      else s += 1;
    }
    if ((e.h || e.t).toLowerCase() === q) s += 10;
    if (!e.h) s += 1; // whole pages rank just above their own sections
    return s;
  };

  const select = (i) => {
    const items = list.querySelectorAll("li");
    if (!items.length) return;
    active = (i + items.length) % items.length;
    items.forEach((li, n) => li.setAttribute("aria-selected", n === active ? "true" : "false"));
    items[active].scrollIntoView({ block: "nearest" });
    input.setAttribute("aria-activedescendant", items[active].id);
  };

  const render = () => {
    const q = input.value.trim().toLowerCase();
    const terms = q.split(/\s+/).filter(Boolean);
    let results;
    if (!terms.length) {
      results = index.filter((e) => !e.h && e.s !== "Roadmap").slice(0, 8);
    } else {
      results = index
        .map((e) => [score(e, terms, q), e])
        .filter(([s]) => s > 0)
        .sort((a, b) => b[0] - a[0])
        .slice(0, 12)
        .map(([, e]) => e);
    }
    list.innerHTML = results
      .map((e, n) => {
        const title = e.h ? e.h : e.t;
        const where = e.h ? e.s + " · " + e.t : e.s;
        return '<li role="option" id="sr-' + n + '"><a href="' + escape(e.u) + '">' +
          '<span class="cmdk__sec">' + escape(where) + "</span>" +
          '<span class="cmdk__t">' + highlight(title, terms) + "</span>" +
          (e.x ? '<span class="cmdk__x">' + escape(e.x) + "</span>" : "") + "</a></li>";
      })
      .join("");
    empty.hidden = results.length > 0;
    select(0);
  };

  const open = () => {
    if (!dialog || dialog.open) return;
    dialog.showModal();
    input.value = "";
    load().then(render);
  };

  if (dialog) {
    document.querySelectorAll("[data-search-open]").forEach((b) => b.addEventListener("click", open));
    document.addEventListener("keydown", (e) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        dialog.open ? dialog.close() : open();
      } else if (e.key === "/" && !dialog.open && !/input|textarea|select/i.test(e.target.tagName)) {
        e.preventDefault();
        open();
      }
    });
    input.addEventListener("input", () => index && render());
    input.addEventListener("keydown", (e) => {
      if (e.key === "ArrowDown") { e.preventDefault(); select(active + 1); }
      else if (e.key === "ArrowUp") { e.preventDefault(); select(active - 1); }
      else if (e.key === "Enter") {
        const a = list.querySelector('[aria-selected="true"] a');
        if (a) { e.preventDefault(); dialog.close(); window.location.href = a.href; }
      }
    });
    dialog.addEventListener("click", (e) => { if (e.target === dialog) dialog.close(); });
    list.addEventListener("click", (e) => { if (e.target.closest("a")) dialog.close(); });
  }

  // ---------- On this page ----------
  const toc = document.querySelector(".docs__toc");
  if (toc && "IntersectionObserver" in window) {
    const links = new Map();
    toc.querySelectorAll('a[href^="#"]').forEach((a) => links.set(decodeURIComponent(a.hash.slice(1)), a));
    const heads = [...links.keys()].map((id) => document.getElementById(id)).filter(Boolean);
    const visible = new Set();
    const mark = () => {
      let current = null;
      for (const h of heads) {
        if (visible.has(h)) { current = h; break; }
      }
      if (!current) {
        // Between headings: the last one above the viewport.
        for (const h of heads) if (h.getBoundingClientRect().top < 120) current = h;
      }
      links.forEach((a) => a.classList.remove("is-current"));
      if (current) {
        const a = links.get(current.id);
        a.classList.add("is-current");
        const box = toc.getBoundingClientRect();
        const r = a.getBoundingClientRect();
        if (r.top < box.top || r.bottom > box.bottom) a.scrollIntoView({ block: "nearest" });
      }
    };
    const io = new IntersectionObserver((entries) => {
      entries.forEach((en) => (en.isIntersecting ? visible.add(en.target) : visible.delete(en.target)));
      mark();
    }, { rootMargin: "-64px 0px -60% 0px" });
    heads.forEach((h) => io.observe(h));
  }

  // ---------- Roadmap filter deep links (#ready, #in-progress, #planned) ----------
  const applyHash = () => {
    const radio = document.querySelector('.filter input[id="' + CSS.escape(location.hash.slice(1)) + '"]');
    if (radio) {
      radio.checked = true;
      radio.closest(".features").scrollIntoView({ block: "start" });
    }
  };
  if (document.querySelector(".filter")) {
    window.addEventListener("hashchange", applyHash);
    if (location.hash) applyHash();
  }
})();
