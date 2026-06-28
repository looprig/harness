// Self-contained, dependency-free behavior for the transcript export: bulk
// collapse/expand of every <details> (AI messages, thinking, tool cards, system
// prompt, nested subagents) and a jump-to-top control. Vanilla DOM only — no
// external scripts — so the page stays fully offline.
(function () {
  "use strict";

  function setAllDetails(open) {
    var all = document.querySelectorAll("details");
    for (var i = 0; i < all.length; i++) {
      all[i].open = open;
    }
  }

  function on(id, handler) {
    var el = document.getElementById(id);
    if (el) {
      el.addEventListener("click", handler);
    }
  }

  document.addEventListener("DOMContentLoaded", function () {
    on("collapse-all", function () {
      setAllDetails(false);
    });
    on("expand-all", function () {
      setAllDetails(true);
    });
    on("jump-to-top", function () {
      window.scrollTo({ top: 0, behavior: "smooth" });
    });
  });
})();
