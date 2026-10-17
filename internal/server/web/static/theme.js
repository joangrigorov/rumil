/* Rumil color theme: system by default, with an explicit dark or light choice.
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

  function apply(mode) {
    root.setAttribute("data-theme", resolved(mode));
    root.setAttribute("data-theme-mode", mode);

    var choices = document.querySelectorAll("[data-theme-choice]");
    for (var i = 0; i < choices.length; i++) {
      choices[i].setAttribute("aria-pressed", String(choices[i].getAttribute("data-theme-choice") === mode));
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
    apply(stored());

    var choices = document.querySelectorAll("[data-theme-choice]");
    for (var i = 0; i < choices.length; i++) {
      choices[i].addEventListener("click", function () {
        var mode = this.getAttribute("data-theme-choice");
        try {
          window.localStorage.setItem(KEY, mode);
        } catch (error) {
          // A browser that blocks storage still gets the theme for this page.
        }
        apply(mode);
      });
    }
  });
})();
