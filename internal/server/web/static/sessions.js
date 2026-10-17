/* Rumil observation deck: render one review session's SSE transcript.
 *
 * The server replays the stored events after the client's last seq and then
 * follows the live broker, so this file does the same work for a session that
 * finished last week and one that is running now.
 */
(function () {
  "use strict";

  var timeline = document.getElementById("timeline");
  if (!timeline) {
    return;
  }

  var sessionId = timeline.dataset.sessionId;
  var live = timeline.dataset.live === "true";
  var startedAt = Date.parse(timeline.dataset.startedAt) || Date.now();

  var statusBadge = document.getElementById("session-status");
  var clock = document.getElementById("session-clock");
  var toolCount = document.getElementById("session-tool-calls");

  var current = null;
  var stepNumber = 0;
  var lastSeq = 0;
  var toolCalls = 0;
  var ended = false;
  var following = live;
  var scrollQueued = false;

  var clockTimer = window.setInterval(tickClock, 1000);
  tickClock();

  var source = new EventSource("/api/sessions/" + encodeURIComponent(sessionId) + "/events");

  source.onopen = function () {
    if (!ended && live) {
      setBadge("live");
    }
  };

  source.onerror = function () {
    if (!ended && live) {
      setBadge("reconnecting");
    }
  };

  source.onmessage = function (message) {
    var event;
    try {
      event = JSON.parse(message.data);
    } catch (error) {
      return;
    }
    if (!event || !event.type) {
      return;
    }
    if (event.seq && event.seq <= lastSeq) {
      return;
    }
    if (event.seq) {
      lastSeq = event.seq;
    }
    render(event);
  };

  window.addEventListener("scroll", function () {
    var distance = document.documentElement.scrollHeight - window.scrollY - window.innerHeight;
    following = distance < 120;
  }, { passive: true });

  timeline.addEventListener("toggle", function (event) {
    if (!(event.target instanceof HTMLDetailsElement)) {
      return;
    }
    // A reader who opened an old block is not asking to be pulled away from it.
    if (!event.target.open) {
      following = false;
    }
  }, true);

  function render(event) {
    var payload = event.payload || {};
    removePlaceholder();

    switch (event.type) {
      case "turn_start":
        marker("turn", "Turn started");
        break;

      case "step_start":
        stepNumber = Number(payload.step) || stepNumber + 1;
        current = newStep(stepNumber);
        break;

      case "reasoning_start":
        reasoningBlock(step()).details.open = true;
        break;

      case "reasoning_delta":
        appendText(reasoningBlock(step()).body, payload.delta);
        break;

      case "reasoning_end":
        reasoningBlock(step()).details.classList.add("complete");
        break;

      case "message_start":
        messageBlock(step());
        break;

      case "message_delta":
        appendText(messageBlock(step()).body, payload.delta);
        break;

      case "message_end":
        setMessage(step(), payload.content);
        break;

      case "tool_call_start":
        setToolState(toolCard(step(), payload), "writing", "writing");
        break;

      case "tool_args_delta":
        var writing = toolCard(step(), payload);
        writing.parts.args.hidden = false;
        writing.parts.args.open = true;
        appendText(writing.parts.argsBody, payload.delta);
        setToolState(writing, "writing", "writing");
        break;

      case "tool_call_end":
        var parsed = toolCard(step(), payload);
        if (parsed.dataset.status === "writing") {
          setToolState(parsed, "pending", "queued");
        }
        break;

      case "tool_call":
        var called = toolCard(step(), payload);
        if (payload.args !== undefined && payload.args !== null) {
          setToolArgs(called, payload.args);
        }
        setToolState(called, "pending", "queued");
        break;

      case "tool_start":
        var running = toolCard(step(), payload);
        if (payload.args) {
          setToolArgs(running, payload.args);
        }
        setToolState(running, "running", "running");
        break;

      case "tool_content":
        appendText(messageBlock(step()).body, payload.content);
        break;

      case "tool_output":
        var output = toolCard(step(), payload);
        setToolOutput(output, payload.chunk, payload.is_stderr);
        break;

      case "tool_result":
        var result = toolCard(step(), payload);
        setToolResult(result, payload.result, payload.is_error);
        break;

      case "step_usage":
        setUsage(step(), payload);
        break;

      case "step_finish":
        finishStep(step(), payload);
        break;

      case "turn_end":
        if (payload.error) {
          marker("error", "The turn ended with an error", payload.error);
        }
        break;

      case "session_metadata":
        marker("info", "Reviewing " + quote(payload.title || "the pull request"));
        break;

      case "guidelines_loaded":
        marker("info", "Repository guidelines loaded", payload.path + " at " + shortSHA(payload.ref));
        break;

      case "guidelines_missing":
        marker("info", "No repository guidelines", "The review runs on the prompt alone.");
        break;

      case "memory_retrieved":
        marker("info", "Recorded rules retrieved", count(payload.rules, "rule") + ", " + bytes(payload.bytes));
        break;

      case "memory_failed":
        marker("warn", "Recorded rules could not be retrieved", payload.error);
        break;

      case "review_submitted":
        marker("review", "Review submitted", reviewDescription(payload));
        break;

      case "retry":
        marker("warn", "Retrying the model call", attemptDescription(payload));
        break;

      case "step_resume":
        marker("warn", "Resuming the step", attemptDescription(payload));
        break;

      case "warnings":
        marker("warn", "The session reported warnings", join(payload.warnings));
        break;

      case "compaction":
        marker("info", "Context compacted", compactionDescription(payload));
        break;

      case "events_dropped":
        marker("warn", "Some streaming fragments were dropped", count(payload.count, "fragment") + " under load; the stored message and tool results are intact.");
        break;

      case "error":
        marker("error", "The model call failed", payload.error);
        break;

      case "session_error":
        marker("error", "The review session failed", payload.error);
        break;

      case "session_end":
        finish(payload);
        return;
    }

    follow();
  }

  function finish(payload) {
    ended = true;
    following = false;
    window.clearInterval(clockTimer);

    if (payload.status) {
      if (statusBadge) {
        statusBadge.textContent = payload.status;
        statusBadge.className = "pill status-" + payload.status.toLowerCase();
      }
      if (live) {
        live = false;
        var badge = document.querySelector(".live-badge");
        if (badge) {
          badge.hidden = true;
        }
      }
    }

    var finishedAt = Date.parse(payload.finished_at);
    setClock((isNaN(finishedAt) ? Date.now() : finishedAt) - startedAt);

    if (payload.tool_calls !== undefined && payload.tool_calls !== null) {
      toolCalls = Number(payload.tool_calls) || toolCalls;
      updateToolCount();
    }
    // The engine records session_error before it returns, and the page shows a
    // notice for a stored failure. The terminal error is the fallback for a
    // run that stopped before either.
    if (payload.error && !document.querySelector(".notice-bad") && !timeline.querySelector(".event-error")) {
      marker("error", "The review failed", payload.error);
    }
    if (payload.summary) {
      summary(payload.summary);
    }
    source.close();
    follow();
  }

  function step() {
    if (!current || current.finished) {
      current = newStep(++stepNumber);
    }
    return current;
  }

  function newStep(number) {
    var item = document.createElement("li");
    item.className = "step";

    var card = document.createElement("article");
    card.className = "step-card";

    var head = document.createElement("header");
    head.className = "step-head";

    var index = document.createElement("span");
    index.className = "step-index";
    index.textContent = "Step " + number;
    head.append(index);

    card.append(head);
    item.append(card);
    timeline.append(item);

    return {
      item: item,
      card: card,
      head: head,
      reasoning: null,
      message: null,
      tools: new Map(),
      finished: false
    };
  }

  function reasoningBlock(target) {
    if (target.reasoning) {
      return target.reasoning;
    }
    var details = document.createElement("details");
    details.className = "block reasoning";
    details.open = true;

    var summaryNode = document.createElement("summary");
    summaryNode.textContent = "Reasoning";

    var body = document.createElement("div");
    body.className = "block-body";
    body.setAttribute("dir", "auto");

    details.append(summaryNode, body);
    target.card.append(details);

    target.reasoning = { details: details, body: body };
    return target.reasoning;
  }

  function messageBlock(target) {
    if (target.message) {
      return target.message;
    }
    var details = document.createElement("details");
    details.className = "block message";
    details.open = true;

    var summaryNode = document.createElement("summary");
    summaryNode.textContent = "Output";

    var body = document.createElement("div");
    body.className = "block-body";
    body.setAttribute("dir", "auto");

    details.append(summaryNode, body);
    target.card.append(details);

    target.message = { details: details, body: body };
    return target.message;
  }

  function setMessage(target, content) {
    var block = messageBlock(target);
    var streamed = block.body.textContent;
    block.body.replaceChildren();
    block.body.textContent = content || streamed;
    block.details.classList.add("complete");
  }

  function toolCard(target, payload) {
    var id = payload.id || payload.name || "tool";
    var card = target.tools.get(id);
    if (card) {
      if (payload.name && card.dataset.name === "") {
        card.dataset.name = payload.name;
        card.querySelector(".tool-name").textContent = payload.name;
      }
      return card;
    }

    card = document.createElement("div");
    card.className = "tool";
    card.dataset.status = "pending";
    card.dataset.name = payload.name || "";

    var head = document.createElement("div");
    head.className = "tool-head";

    var name = document.createElement("code");
    name.className = "tool-name";
    name.textContent = payload.name || "tool";

    var state = document.createElement("span");
    state.className = "tool-state";
    state.textContent = "queued";

    head.append(name, state);

    var args = document.createElement("details");
    args.className = "tool-args";
    var argsSummary = document.createElement("summary");
    argsSummary.textContent = "Arguments";
    var argsBody = document.createElement("pre");
    args.append(argsSummary, argsBody);

    var output = document.createElement("details");
    output.className = "tool-output";
    var outputSummary = document.createElement("summary");
    outputSummary.textContent = "Output";
    var outputBody = document.createElement("pre");
    outputBody.setAttribute("dir", "auto");
    output.append(outputSummary, outputBody);
    output.hidden = true;

    var result = document.createElement("div");
    result.className = "tool-result";
    result.hidden = true;
    var resultLabel = document.createElement("p");
    resultLabel.className = "tool-result-label";
    resultLabel.textContent = "Result";
    var resultBody = document.createElement("pre");
    resultBody.setAttribute("dir", "auto");
    result.append(resultLabel, resultBody);

    card.append(head, args, output, result);
    target.card.append(card);

    card.parts = { state: state, args: args, argsBody: argsBody, output: output, outputBody: outputBody, result: result, resultBody: resultBody };
    target.tools.set(id, card);
    toolCalls += 1;
    updateToolCount();
    return card;
  }

  function updateToolCount() {
    if (toolCount) {
      toolCount.textContent = count(toolCalls, "tool call");
    }
  }

  function setToolArgs(card, args) {
    card.parts.argsBody.textContent = text(args);
    card.parts.args.hidden = false;
  }

  function setToolState(card, status, label) {
    card.dataset.status = status;
    card.parts.state.textContent = label;
  }

  function setToolOutput(card, chunk, isStderr) {
    card.parts.output.hidden = false;
    card.parts.output.open = true;
    if (isStderr) {
      card.parts.output.classList.add("stderr");
    }
    appendText(card.parts.outputBody, chunk);
  }

  function setToolResult(card, value, isError) {
    card.parts.output.hidden = true;
    card.parts.result.hidden = false;
    card.parts.result.classList.toggle("is-error", Boolean(isError));
    card.parts.resultBody.textContent = text(value) || "(no result)";
    setToolState(card, isError ? "error" : "done", isError ? "failed" : "done");
  }

  function setUsage(target, usage) {
    if (!target.usage) {
      target.usage = document.createElement("p");
      target.usage.className = "step-usage";
      target.card.append(target.usage);
    }
    var parts = [];
    if (usage.input_tokens) {
      parts.push(formatNumber(usage.input_tokens) + " in");
    }
    if (usage.output_tokens) {
      parts.push(formatNumber(usage.output_tokens) + " out");
    }
    if (usage.reasoning_tokens) {
      parts.push(formatNumber(usage.reasoning_tokens) + " reasoning");
    }
    if (usage.cache_read) {
      parts.push(formatNumber(usage.cache_read) + " cached");
    }
    target.usage.textContent = parts.join(" · ");
  }

  function finishStep(target, payload) {
    target.finished = true;
    if (payload.usage) {
      setUsage(target, payload.usage);
    }
    if (payload.finish_reason) {
      var reason = document.createElement("span");
      reason.className = "step-reason";
      reason.textContent = payload.finish_reason;
      target.head.append(reason);
    }
    if (target.reasoning) {
      target.reasoning.details.classList.add("complete");
    }
    if (target.message) {
      target.message.details.classList.add("complete");
    }
  }

  function marker(kind, text, detail) {
    // An event that happens during a step belongs inside that step, so the
    // conversation keeps its order instead of collecting every marker after
    // the last card.
    var inStep = current && !current.finished;
    var item = document.createElement(inStep ? "div" : "li");
    item.className = "event event-" + kind;

    var dot = document.createElement("span");
    dot.className = "event-dot";
    dot.setAttribute("aria-hidden", "true");

    var body = document.createElement("div");
    body.className = "event-body";

    var line = document.createElement("p");
    line.className = "event-text";
    line.textContent = text;
    body.append(line);

    if (detail) {
      var note = document.createElement("p");
      note.className = "event-detail";
      note.textContent = detail;
      note.setAttribute("dir", "auto");
      body.append(note);
    }

    item.append(dot, body);
    if (inStep) {
      current.card.append(item);
    } else {
      timeline.append(item);
    }
  }

  function summary(text) {
    // A finished session already renders its summary on the server; a live
    // one learns it from the terminal event.
    if (document.querySelector(".summary")) {
      return;
    }
    var section = document.createElement("section");
    section.className = "summary";

    var heading = document.createElement("h2");
    heading.textContent = "Review summary";

    var paragraph = document.createElement("p");
    paragraph.textContent = text;
    paragraph.setAttribute("dir", "auto");

    section.append(heading, paragraph);
    timeline.after(section);
  }

  function removePlaceholder() {
    var placeholder = document.getElementById("timeline-empty");
    if (placeholder) {
      placeholder.remove();
    }
  }

  function follow() {
    if (!following || scrollQueued) {
      return;
    }
    scrollQueued = true;
    window.requestAnimationFrame(function () {
      scrollQueued = false;
      window.scrollTo({ top: document.documentElement.scrollHeight, behavior: "auto" });
    });
  }

  function tickClock() {
    if (ended) {
      return;
    }
    setClock(Date.now() - startedAt);
  }

  function setClock(elapsed) {
    if (clock) {
      clock.textContent = duration(elapsed);
    }
  }

  function setBadge(text) {
    var badge = document.querySelector(".live-badge");
    if (!badge) {
      return;
    }
    badge.dataset.state = text;
    var label = badge.querySelector(".live-label");
    if (label) {
      label.textContent = text;
    }
  }

  function appendText(parent, value) {
    if (value === undefined || value === null || value === "") {
      return;
    }
    parent.append(document.createTextNode(String(value)));
  }

  function text(value) {
    if (value === undefined || value === null) {
      return "";
    }
    if (typeof value === "string") {
      return value;
    }
    try {
      return JSON.stringify(value, null, 2);
    } catch (error) {
      return String(value);
    }
  }

  function reviewDescription(payload) {
    var parts = [];
    if (payload.event) {
      parts.push(payload.event);
    }
    if (payload.fallback) {
      parts.push("submitted as COMMENT");
    }
    if (payload.moved_to_summary) {
      parts.push(count(payload.moved_to_summary, "comment") + " moved to the summary");
    }
    if (payload.skipped_duplicates) {
      parts.push(count(payload.skipped_duplicates, "duplicate") + " skipped");
    }
    return parts.join(" · ");
  }

  function attemptDescription(payload) {
    var parts = [];
    if (payload.attempt) {
      parts.push("attempt " + payload.attempt);
    }
    if (payload.delay_ms) {
      parts.push("in " + duration(payload.delay_ms));
    }
    if (payload.error) {
      parts.push(payload.error);
    }
    return parts.join(" · ");
  }

  function compactionDescription(payload) {
    var parts = [];
    if (payload.original_tokens) {
      parts.push(formatNumber(payload.original_tokens) + " → " + formatNumber(payload.compacted_tokens || 0) + " tokens");
    }
    if (payload.messages_removed) {
      parts.push(count(payload.messages_removed, "message") + " removed");
    }
    if (payload.summary) {
      parts.push(payload.summary);
    }
    if (payload.error) {
      parts.push(payload.error);
    }
    return parts.join(" · ");
  }

  function quote(value) {
    return "“" + value + "”";
  }

  function shortSHA(value) {
    if (!value) {
      return "";
    }
    return String(value).slice(0, 7);
  }

  function count(value, noun) {
    var number = Number(value) || 0;
    return formatNumber(number) + " " + noun + (number === 1 ? "" : "s");
  }

  function bytes(value) {
    var number = Number(value) || 0;
    if (number < 1024) {
      return number + " B";
    }
    return (number / 1024).toFixed(1) + " KB";
  }

  function formatNumber(value) {
    return new Intl.NumberFormat().format(Number(value) || 0);
  }

  function join(values) {
    if (!Array.isArray(values)) {
      return text(values);
    }
    return values.join(" · ");
  }

  function duration(milliseconds) {
    var seconds = Math.max(0, Math.round((Number(milliseconds) || 0) / 1000));
    if (seconds < 1) {
      return "<1s";
    }
    if (seconds < 60) {
      return seconds + "s";
    }
    var minutes = Math.floor(seconds / 60);
    if (minutes < 60) {
      return minutes + "m " + String(seconds % 60).padStart(2, "0") + "s";
    }
    var hours = Math.floor(minutes / 60);
    return hours + "h " + String(minutes % 60).padStart(2, "0") + "m";
  }
})();
