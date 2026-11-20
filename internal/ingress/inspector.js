if (window.self !== window.top) return;

const host = document.createElement("div");
const root = host.attachShadow({mode: "closed"});
const style = document.createElement("style");
style.textContent = `
  button, dialog { font: 1rem system-ui, sans-serif; }
  button { cursor: pointer; }
  .open { position: fixed; right: 1rem; bottom: 1rem; z-index: 2147483647;
          width: 2.75rem; height: 2.75rem; border-radius: 50%;
          background: #202938; color: white; border: 1px solid #627187; }
  dialog { position: fixed; inset: auto 1rem 4.25rem auto; margin: 0;
           max-width: min(24rem, calc(100vw - 4rem)); overflow-wrap: anywhere;
           background: #fff; color: #202938; border: 1px solid #627187;
           border-radius: .5rem; padding: 1rem; }
  h2 { margin: 0 0 .75rem; font-size: 1rem; }
  a { display: block; margin-bottom: .65rem; color: #174b9c; }
`;

const open = document.createElement("button");
open.className = "open";
open.type = "button";
open.textContent = "π";
open.setAttribute("aria-label", "Preview deployment details");
open.setAttribute("aria-haspopup", "dialog");

const panel = document.createElement("dialog");
panel.setAttribute("aria-labelledby", "heading");
const heading = document.createElement("h2");
heading.id = "heading";
heading.textContent = "Preview deployment";
panel.append(heading);

for (const [label, href] of [
  [deployment.Issue, deployment.IssueURL],
  [deployment.Commit, deployment.CommitURL],
  ["Live logs", deployment.LogsURL],
]) {
  if (!href) continue;
  const link = document.createElement("a");
  link.href = href;
  link.textContent = label;
  link.target = "_blank";
  link.rel = "noopener noreferrer";
  panel.append(link);
}

const close = document.createElement("button");
close.type = "button";
close.textContent = "Close";
close.addEventListener("click", () => panel.close());
panel.append(close);
open.addEventListener("click", () => panel.showModal());
root.append(style, open, panel);
document.body.append(host);
