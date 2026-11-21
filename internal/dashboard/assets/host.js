(() => {
  const bar = document.getElementById("host-bar");
  if (!bar) return;
  const summary = bar.querySelector("summary");
  let sampledAt;
  function resetClock() {
    sampledAt = Date.now() - Number(summary.querySelector("[data-age]").dataset.age) * 1000;
    showAge();
  }
  function showAge() {
    const age = Math.max(0, Math.floor((Date.now() - sampledAt) / 1000));
    const label = summary.querySelector("[data-age]");
    label.classList.toggle("stale", age >= 30);
    label.textContent = age >= 30 ? `Stale · sampled ${age}s ago` : `Sampled ${age}s ago`;
  }
  async function refresh() {
    try {
      const response = await fetch("/dashboard/host", {signal: AbortSignal.timeout(10000)});
      if (!response.ok) return;
      const fragment = document.createElement("template");
      fragment.innerHTML = await response.text();
      const nextSummary = fragment.content.querySelector("summary");
      const readings = fragment.content.querySelector(".host-readings");
      if (!nextSummary?.querySelector("[data-age]") || !readings) return;
      summary.replaceChildren(...nextSummary.childNodes);
      bar.querySelector(".host-readings").replaceWith(readings);
      resetClock();
    } catch {
      // Keep the last sample visible and age it until polling recovers.
    }
  }
  resetClock();
  setInterval(showAge, 1000);
  setInterval(refresh, 15000);
})();
