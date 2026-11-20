const actionStatus = document.getElementById("delete-status");
let pending = false;
const messages = {
  delete: ["Deleting", "Deleted"],
  archive: ["Archiving", "Archived"],
  restore: ["Restoring", "Restored"],
};

document.addEventListener("click", async (event) => {
  const button = event.target.closest("button[data-delete-url], button[data-action-url]");
  if (!button || pending) return;

  const preview = button.dataset.preview;
  const action = button.dataset.action ?? "delete";
  if (action === "delete" && !window.confirm(`Delete ${preview}? This permanently removes the preview and its deployment history.`)) {
    return;
  }

  pending = true;
  button.disabled = true;
  actionStatus.textContent = `${messages[action][0]} ${preview}…`;
  try {
    const response = await fetch(button.dataset.actionUrl ?? button.dataset.deleteUrl, {
      method: "POST",
      headers: {"X-Preview-Action": action},
      mode: "same-origin",
      redirect: "error",
    });
    actionStatus.textContent = response.ok
      ? `${messages[action][1]} ${preview}.`
      : (await response.text()).trim();
  } catch {
    actionStatus.textContent = `Could not confirm the change to ${preview}. Check the dashboard and try again if needed.`;
  } finally {
    button.disabled = false;
    pending = false;
  }
});
