(() => {
  const key = "preview-dashboard-theme";
  const modes = ["light", "dark", "system"];
  const icons = {light: "sun", dark: "moon", system: "display"};
  const system = window.matchMedia("(prefers-color-scheme: dark)");
  let mode = "system";
  try {
    const saved = localStorage.getItem(key);
    if (modes.includes(saved)) mode = saved;
  } catch {
    // Storage may be unavailable on this device.
  }

  function nextMode() {
    return modes[(modes.indexOf(mode) + 1) % modes.length];
  }

  function apply() {
    document.documentElement.dataset.theme = mode === "system"
      ? (system.matches ? "dark" : "light") : mode;
    const button = document.getElementById("theme-toggle");
    if (!button) return;
    button.title = `Color theme: ${mode}. Switch to ${nextMode()} mode.`;
    button.setAttribute("aria-label", button.title);
    button.querySelector("use").setAttribute("href", "/dashboard/assets/icons.svg#icon-" + icons[mode]);
    button.hidden = false;
  }

  apply();
  system.addEventListener("change", apply);
  document.addEventListener("DOMContentLoaded", () => {
    apply();
    document.getElementById("theme-toggle").addEventListener("click", () => {
      mode = nextMode();
      apply();
      try {
        localStorage.setItem(key, mode);
      } catch {
        // The current page still reflects the selected mode.
      }
    });
  });
})();
