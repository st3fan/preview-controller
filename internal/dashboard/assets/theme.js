(() => {
  const storageKey = "preview-dashboard-theme";
  const root = document.documentElement;

  try {
    if (localStorage.getItem(storageKey) === "dark") {
      root.dataset.theme = "dark";
    }
  } catch {
    // The toggle still works for this page when storage is unavailable.
  }

  document.addEventListener("DOMContentLoaded", () => {
    const toggle = document.getElementById("theme-toggle");
    toggle.setAttribute("aria-pressed", String(root.dataset.theme === "dark"));
    toggle.hidden = false;

    toggle.addEventListener("click", () => {
      const dark = root.dataset.theme !== "dark";
      const theme = dark ? "dark" : "light";
      root.dataset.theme = theme;
      toggle.setAttribute("aria-pressed", String(dark));

      try {
        localStorage.setItem(storageKey, theme);
      } catch {
        // The current page has already switched themes.
      }
    });
  });
})();
