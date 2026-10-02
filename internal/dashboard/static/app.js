// a2a-layer dashboard: the few behaviours htmx does not cover.

document.addEventListener("click", (e) => {
  // Drop a repeated form row (an MCP server, a skill).
  const remove = e.target.closest(".remove-row");
  if (remove) {
    remove.closest(".row").remove();
    return;
  }

  // Pick one of the runner's models.
  const model = e.target.closest("[data-model]");
  if (model) {
    const input = model.closest("form").querySelector("[name=model]");
    input.value = model.dataset.model;
    markModel(input);
    return;
  }

  // Use a skill's example as the playground message.
  const example = e.target.closest("[data-example]");
  if (example) {
    const msg = document.getElementById("play-message");
    if (msg) {
      msg.value = example.dataset.example;
      msg.focus();
    }
    return;
  }

  // Copy an answer, as the agent wrote it.
  const copy = e.target.closest("[data-copy]");
  if (copy) {
    navigator.clipboard.writeText(copy.dataset.copy).then(() => {
      copy.textContent = "Copied";
      setTimeout(() => (copy.textContent = "Copy"), 1500);
    });
  }
});

// Ctrl+Enter (or Cmd+Enter) sends the playground message.
document.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && (e.ctrlKey || e.metaKey) && e.target.id === "play-message") {
    e.preventDefault();
    e.target.form.requestSubmit();
  }
});

// A message was sent: clear it, and follow the conversation to its end.
document.addEventListener("run-sent", () => {
  const msg = document.getElementById("play-message");
  if (msg) msg.value = "";
  document.querySelectorAll(".composer .note").forEach((n) => n.remove());
  followThread = true;
});

// The conversation shown scrolls with the page. Keep it at its latest message while the reader
// is there, as a running task's log grows or its answer arrives.
let followThread = true;
window.addEventListener("scroll", () => {
  followThread = window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 80;
});
function toEnd() {
  if (document.getElementById("thread") && followThread) {
    window.scrollTo(0, document.documentElement.scrollHeight);
  }
}

// Keep a running task's log scrolled to its latest note.
document.addEventListener("htmx:afterSettle", () => {
  document.querySelectorAll(".run[hx-get] .log").forEach((log) => {
    log.scrollTop = log.scrollHeight;
  });
  toEnd();
});
document.addEventListener("htmx:load", (e) => {
  // A page opened (or a conversation started): show its latest message.
  const elt = e.detail.elt;
  if (elt.id === "chat" || (elt.querySelector && elt.querySelector("#chat"))) {
    followThread = true;
    toEnd();
  }
});

// Highlight the model chip that matches the Model field.
function markModel(input) {
  const form = input.closest("form");
  form.querySelectorAll("[data-model]").forEach((chip) => {
    chip.classList.toggle("on", chip.dataset.model === input.value.trim());
  });
}
document.addEventListener("input", (e) => {
  if (e.target.name === "model") markModel(e.target);
});
document.addEventListener("htmx:load", () => {
  document.querySelectorAll("form [name=model]").forEach(markModel);
});
