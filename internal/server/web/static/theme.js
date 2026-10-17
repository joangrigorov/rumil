/* Rumil color theme: system by default, with an explicit dark or light choice.
 *
 * The script runs in the head so the resolved theme is on <html> before the
 * first paint. One button cycles Auto -> Dark -> Light; the stored value is the
 * user's choice and data-theme is always the resolved dark or light value.
 */
(function () {
  "use strict";

  var KEY = "rumil.theme";
  var MODES = ["system", "dark", "light"];
  var LABELS = { system: "Auto", dark: "Dark", light: "Light" };
  var root = document.documentElement;
  var media = window.matchMedia("(prefers-color-scheme: dark)");
  var toggle = null;

  function stored() {
    try {
      var value = window.localStorage.getItem(KEY);
      return value === "dark" || value === "light" ? value : "system";
    } catch (error) {
      return "system";
    }
  }

  function resolved(mode) {
    if (mode === "dark" || mode === "light") {
      return mode;
    }
    return media.matches ? "dark" : "light";
  }

  function next(mode) {
    return MODES[(MODES.indexOf(mode) + 1) % MODES.length];
  }

  function apply(mode) {
    root.setAttribute("data-theme", resolved(mode));
    root.setAttribute("data-theme-mode", mode);

    if (!toggle) {
      toggle = document.getElementById("theme-toggle");
    }
    if (!toggle) {
      return;
    }

    var nextMode = next(mode);
    toggle.setAttribute("aria-label", "Color theme: " + LABELS[mode] + ". Activate to switch to " + LABELS[nextMode] + ".");
    toggle.title = "Theme: " + LABELS[mode] + " (click for " + LABELS[nextMode] + ")";
  }

  apply(stored());

  function onSystemChange() {
    if (stored() === "system") {
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
    toggle = document.getElementById("theme-toggle");
    apply(stored());

    if (!toggle) {
      return;
    }
    toggle.addEventListener("click", function () {
      var mode = next(stored());
      try {
        window.localStorage.setItem(KEY, mode);
      } catch (error) {
        // A browser that blocks storage still gets the theme for this page.
      }
      apply(mode);
    });
  });
})();
