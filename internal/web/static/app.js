// Searchable choice inputs: <input data-combo list="id"> with a <datalist>.
// Without this script the browser's own datalist suggestions still work.
(function () {
  "use strict";

  function setup(input) {
    var list = document.getElementById(input.getAttribute("list"));
    if (!list) return;
    var options = Array.prototype.map.call(list.options, function (o) {
      return { value: o.value, count: o.dataset.count || "" };
    });
    input.removeAttribute("list");
    input.setAttribute("role", "combobox");
    input.setAttribute("aria-autocomplete", "list");
    input.setAttribute("aria-expanded", "false");

    var menu = document.createElement("ul");
    menu.className = "combo-menu";
    menu.id = list.id + "-menu";
    menu.setAttribute("role", "listbox");
    menu.hidden = true;
    input.setAttribute("aria-controls", menu.id);
    input.parentNode.appendChild(menu);

    var shown = [], active = -1, limit = 200;

    function render() {
      var q = input.value.trim().toLowerCase();
      shown = options.filter(function (o) { return !q || o.value.toLowerCase().indexOf(q) >= 0; });
      // Names starting with the text first.
      if (q) shown.sort(function (a, b) {
        return (a.value.toLowerCase().indexOf(q) !== 0) - (b.value.toLowerCase().indexOf(q) !== 0);
      });
      menu.textContent = "";
      var all = document.createElement("li");
      all.className = "combo-all";
      all.textContent = "All sources";
      all.dataset.value = "";
      menu.appendChild(all);
      shown.slice(0, limit).forEach(function (o) {
        var li = document.createElement("li");
        li.setAttribute("role", "option");
        li.dataset.value = o.value;
        var name = document.createElement("span");
        name.textContent = o.value;
        li.appendChild(name);
        if (o.count !== "" && o.count !== "0") {
          var c = document.createElement("span");
          c.className = "combo-count";
          c.textContent = Number(o.count).toLocaleString();
          li.appendChild(c);
        }
        menu.appendChild(li);
      });
      if (shown.length === 0) {
        var none = document.createElement("li");
        none.className = "combo-none";
        none.textContent = "No matching source";
        menu.appendChild(none);
      } else if (shown.length > limit) {
        var more = document.createElement("li");
        more.className = "combo-none";
        more.textContent = (shown.length - limit) + " more, type to narrow down";
        menu.appendChild(more);
      }
      active = -1;
    }

    function items() { return menu.querySelectorAll("li[data-value]"); }

    function highlight(i) {
      var els = items();
      if (!els.length) return;
      active = (i + els.length) % els.length;
      els.forEach(function (el, j) { el.classList.toggle("active", j === active); });
      els[active].scrollIntoView({ block: "nearest" });
    }

    function open() {
      render();
      menu.hidden = false;
      input.setAttribute("aria-expanded", "true");
    }

    function close() {
      menu.hidden = true;
      input.setAttribute("aria-expanded", "false");
    }

    function choose(value) {
      input.value = value;
      close();
    }

    input.addEventListener("focus", open);
    input.addEventListener("click", function () { if (menu.hidden) open(); });
    input.addEventListener("input", open);
    input.addEventListener("keydown", function (e) {
      if (e.key === "ArrowDown") { e.preventDefault(); if (menu.hidden) open(); highlight(active + 1); }
      else if (e.key === "ArrowUp") { e.preventDefault(); highlight(active - 1); }
      else if (e.key === "Escape") { close(); }
      else if (e.key === "Enter" && !menu.hidden && active >= 0) {
        e.preventDefault();
        choose(items()[active].dataset.value);
      }
    });
    // mousedown keeps the focus in the input until the choice is made.
    menu.addEventListener("mousedown", function (e) {
      var li = e.target.closest("li[data-value]");
      e.preventDefault();
      if (li) choose(li.dataset.value);
    });
    input.addEventListener("blur", function () {
      close();
      // Only exact names filter the archive: complete a unique match.
      var q = input.value.trim().toLowerCase();
      if (!q || options.some(function (o) { return o.value === input.value.trim(); })) return;
      var m = options.filter(function (o) { return o.value.toLowerCase().indexOf(q) >= 0; });
      if (m.length === 1) input.value = m[0].value;
    });
  }

  function init() {
    document.querySelectorAll("input[data-combo][list]").forEach(setup);
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init);
  else init();
})();
