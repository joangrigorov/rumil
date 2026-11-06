/* Rumil color theme: the Stitch deck is dark by default, with an explicit
 * light choice or the operating system's preference.
 *
 * The script runs in the head so the resolved theme is on <html> before the
 * first paint. The stored value is the user's choice; data-theme is always the
 * resolved dark or light value, and data-theme-mode is the choice itself.
 *
 * There is one button. Clicking it moves to the next mode in the cycle, and the
 * icon it shows is the mode that is currently active.
 */
(function () {
  "use strict";

  var KEY = "rumil.theme";
  var MODES = ["dark", "light", "system"];
  var LABELS = { dark: "dark", light: "light", system: "auto" };
  var root = document.documentElement;
  var media = window.matchMedia("(prefers-color-scheme: dark)");
  var button = null;
  var current = stored();

  function stored() {
    try {
      var value = window.localStorage.getItem(KEY);
      return value === "dark" || value === "light" || value === "system" ? value : "dark";
    } catch (error) {
      return "dark";
    }
  }

  function resolved(mode) {
    if (mode === "dark" || mode === "light") {
      return mode;
    }
    return media.matches ? "dark" : "light";
  }

  function nextMode(mode) {
    var index = MODES.indexOf(mode);
    return MODES[(index + 1) % MODES.length];
  }

  function apply(mode) {
    current = mode;
    root.setAttribute("data-theme", resolved(mode));
    root.setAttribute("data-theme-mode", mode);

    if (button) {
      var following = nextMode(mode);
      button.setAttribute("aria-label", "Color theme: " + LABELS[mode] + ". Change to " + LABELS[following] + ".");
      button.setAttribute("title", "Theme: " + LABELS[mode] + " (click for " + LABELS[following] + ")");
    }
  }

  apply(current);

  function onSystemChange() {
    if (current === "system") {
      apply("system");
    }
  }

  if (media.addEventListener) {
    media.addEventListener("change", onSystemChange);
  } else if (media.addListener) {
    media.addListener(onSystemChange);
  }

  window.addEventListener("storage", function (event) {
    if (event.key === KEY) {
      apply(stored());
    }
  });

  document.addEventListener("DOMContentLoaded", function () {
    button = document.getElementById("theme-toggle");
    apply(stored());

    if (button) {
      button.addEventListener("click", function () {
        var following = nextMode(current);
        try {
          window.localStorage.setItem(KEY, following);
        } catch (error) {
          // A browser that blocks storage still gets the theme for this page.
        }
        apply(following);
      });
    }
  });
})();
