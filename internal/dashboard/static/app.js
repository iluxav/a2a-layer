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

  // Continue a run's conversation: pick its agent and context, then write the next message.
  const cont = e.target.closest(".continue[data-context]");
  if (cont) {
    const agent = document.getElementById("play-agent");
    if (agent && agent.value !== cont.dataset.agent) {
      agent.value = cont.dataset.agent;
      htmx.trigger(agent, "change");
    }
    document.getElementById("play-context").value = cont.dataset.context;
    const msg = document.getElementById("play-message");
    msg.value = "";
    msg.focus();
  }
});

// Ctrl+Enter (or Cmd+Enter) sends the playground message.
document.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && (e.ctrlKey || e.metaKey) && e.target.id === "play-message") {
    e.preventDefault();
    e.target.form.requestSubmit();
  }
});

// Keep a running task's log scrolled to its latest note.
document.addEventListener("htmx:afterSettle", () => {
  document.querySelectorAll(".run[hx-get] .log").forEach((log) => {
    log.scrollTop = log.scrollHeight;
  });
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
