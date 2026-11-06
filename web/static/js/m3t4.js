/* m3t4.js - first-party, no requests, nothing stored (M3-T4).
   The activity grid keeps today on its right edge. On a phone the grid is wider than the screen
   and scrolls inside its own region; this scrolls each such region to its right end, so the
   newest day is the first one in view, and again after a live refresh swaps the grid in.
   Without script the grid starts at its left edge and scrolls the same way. */
(function () {
  'use strict';
  function toEnd(root) {
    var list = (root || document).querySelectorAll('[data-scroll-end]');
    for (var i = 0; i < list.length; i++) list[i].scrollLeft = list[i].scrollWidth;
  }
  document.addEventListener('DOMContentLoaded', function () { toEnd(); });
  document.addEventListener('htmx:afterSwap', function (e) { toEnd(e.target); });
})();
