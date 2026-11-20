const deletionStatus = document.getElementById("delete-status");
let deleting = false;

document.addEventListener("click", async (event) => {
  const button = event.target.closest("button[data-delete-url]");
  if (!button || deleting) return;

  const preview = button.dataset.preview;
  if (!window.confirm(`Delete ${preview}? This permanently removes the preview and its deployment history.`)) {
    return;
  }

  deleting = true;
  button.disabled = true;
  deletionStatus.textContent = `Deleting ${preview}…`;
  try {
    const response = await fetch(button.dataset.deleteUrl, {
      method: "POST",
      headers: {"X-Preview-Action": "delete"},
      mode: "same-origin",
      redirect: "error",
    });
    if (response.ok) {
      deletionStatus.textContent = `Deleted ${preview}.`;
    } else {
      deletionStatus.textContent = (await response.text()).trim();
    }
  } catch {
    deletionStatus.textContent = `Could not confirm deletion of ${preview}. Check the dashboard and retry if the preview is still listed.`;
  } finally {
    button.disabled = false;
    deleting = false;
  }
});
