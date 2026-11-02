/* Rumil color theme: the Stitch deck is dark by default, with an explicit
 * light choice or the operating system's preference.
 *
 * The script runs in the head so the resolved theme is on <html> before the
 * first paint. The stored value is the user's choice; data-theme is always the
 * resolved dark or light value.
 */
(function () {
  "use strict";

  var KEY = "rumil.theme";
  var root = document.documentElement;
  var media = window.matchMedia("(prefers-color-scheme: dark)");
  var buttons = [];

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

  function apply(mode) {
    root.setAttribute("data-theme", resolved(mode));
    root.setAttribute("data-theme-mode", mode);

    for (var i = 0; i < buttons.length; i++) {
      var selected = buttons[i].getAttribute("data-theme-mode") === mode;
      buttons[i].setAttribute("aria-pressed", selected ? "true" : "false");
    }
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
    buttons = Array.prototype.slice.call(document.querySelectorAll("button[data-theme-mode]"));
    apply(stored());

    buttons.forEach(function (button) {
      button.addEventListener("click", function () {
        var mode = button.getAttribute("data-theme-mode");
        try {
          window.localStorage.setItem(KEY, mode);
        } catch (error) {
          // A browser that blocks storage still gets the theme for this page.
        }
        apply(mode);
      });
    });
  });
})();
