/* Rumil observation deck: render one review session as a live console.
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

  var scroll = document.getElementById("console-scroll");
  var sessionId = timeline.dataset.sessionId;
  var live = timeline.dataset.live === "true";
  var startedAt = Date.parse(timeline.dataset.startedAt) || Date.now();

  var statusBadge = document.getElementById("session-status");
  var clock = document.getElementById("session-clock");
  var toolCount = document.getElementById("session-tool-calls");
  var activity = document.getElementById("activity");
  var activityLabel = document.getElementById("activity-label");
  var activityTime = document.getElementById("activity-time");

  var tools = new Map();
  var activeThinking = null;
  var activeOutput = null;
  var lastOutput = null;
  var lastSeq = 0;
  var toolCalls = 0;
  var ended = false;
  var following = true;
  var followQueued = false;
  var activityText = "";
  var activityStartedAt = 0;

  var clockTimer = window.setInterval(tick, 1000);
  tick();

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

  if (scroll) {
    scroll.addEventListener("scroll", function () {
      var distance = scroll.scrollHeight - scroll.scrollTop - scroll.clientHeight;
      following = distance < 80;
    }, { passive: true });
  }

  function render(event) {
    var payload = event.payload || {};
    removePlaceholder();

    switch (event.type) {
      case "turn_start":
        setActivity("Thinking…");
        break;

      case "step_start":
        setActivity("Thinking…");
        break;

      case "reasoning_start":
        beginThinking();
        setActivity("Thinking…");
        break;

      case "reasoning_delta":
        appendText(thinkingBlock().body, payload.delta);
        setActivity("Thinking…");
        break;

      case "reasoning_end":
        thinkingBlock().complete();
        setActivity("Thinking…");
        break;

      case "message_start":
        beginOutput();
        setActivity("Reviewing…");
        break;

      case "message_delta":
        appendText(outputBlock().body, payload.delta);
        setActivity("Reviewing…");
        break;

      case "message_end":
        setMessage(payload.content);
        setActivity("Reviewing…");
        break;

      case "tool_call_start":
        var started = toolCard(payload);
        setActivity(toolActivity(started));
        break;

      case "tool_args_delta":
        var writing = toolCard(payload);
        appendText(writing.args, payload.delta);
        setActivity(toolActivity(writing));
        break;

      case "tool_call_end":
        var parsedCall = toolCard(payload);
        if (parsedCall.details.dataset.status === "writing") {
          setToolState(parsedCall, "queued", "queued");
        }
        break;

      case "tool_call":
        var called = toolCard(payload);
        setToolArgs(called, payload.args);
        setToolState(called, "queued", "queued");
        setActivity(toolActivity(called));
        break;

      case "tool_start":
        var running = toolCard(payload);
        if (payload.args) {
          setToolArgs(running, payload.args);
        }
        setToolState(running, "running", "running");
        setActivity(toolActivity(running));
        break;

      case "tool_output":
        var streaming = toolCard(payload);
        setToolOutput(streaming, payload.chunk);
        setActivity(toolActivity(streaming));
        break;

      case "tool_result":
        var finished = toolCard(payload);
        setToolResult(finished, payload.result, payload.is_error);
        setActivity("Thinking…");
        break;

      case "tool_content":
        appendText(outputBlock().body, payload.content);
        setActivity("Reviewing…");
        break;

      case "step_usage":
        // The step_finish event carries the same usage with its finish reason.
        break;

      case "step_finish":
        usageLine(payload.usage);
        activeThinking = null;
        activeOutput = null;
        setActivity("Thinking…");
        break;

      case "turn_end":
        if (payload.error) {
          marker("error", "The turn ended with an error", payload.error);
        }
        break;

      case "session_metadata":
        marker("info", "Reviewing “" + (payload.title || "the pull request") + "”", metadataDetail(payload));
        break;

      case "guidelines_loaded":
        marker("info", "Repository guidelines loaded", guidelinesDetail(payload));
        break;

      case "guidelines_missing":
        marker("info", "No repository guidelines", "The review runs on the prompt alone.");
        break;

      case "repository_settings":
        marker("info", payload.found ? "Repository settings applied" : "No repository settings",
          repositorySettingsDetail(payload));
        break;

      case "provider_fallback": {
        var detail = (payload.from || "the primary provider") + " → " + (payload.to || "the fallback");
        if (payload.model) {
          detail += " (" + payload.model + ")";
        }
        if (payload.error) {
          detail += " · " + String(payload.error).slice(0, 300);
        }
        marker("warn", "Falling back to another provider", detail);
        break;
      }

      case "memory_retrieved":
        marker("info", "Recorded rules retrieved", count(payload.rules, "rule") + (payload.bytes ? " · " + bytes(payload.bytes) : ""));
        break;

      case "memory_failed":
        marker("warn", "Recorded rules could not be retrieved", payload.error);
        break;

      case "review_submitted":
        marker("review", "Review submitted", reviewDescription(payload));
        setActivity("Thinking…");
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

      case "snapshot_ready":
        marker("info", "Repository snapshot ready", count(payload.files, "file") + " at " + shortSHA(payload.ref) + " read locally.");
        break;

      case "snapshot_failed":
        marker("warn", "Repository snapshot unavailable", payload.error + " — the review falls back to the GitHub API tools.");
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
    window.clearInterval(clockTimer);
    clearActivity();

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
    if (payload.error && !document.querySelector(".notice-bad") && !timeline.querySelector(".event-error")) {
      marker("error", "The review failed", payload.error);
    }
    if (payload.status !== "FAILED") {
      marker("review", "Session complete", payload.status || "");
    }
    source.close();
    follow();
  }

  function beginThinking() {
    activeThinking = null;
    return thinkingBlock();
  }

  function thinkingBlock() {
    if (activeThinking) {
      return activeThinking;
    }

    var details = document.createElement("details");
    details.className = "thinking";
    details.open = true;
    details.dataset.state = "streaming";

    var summaryNode = document.createElement("summary");
    var glyph = span("glyph", "✦");
    var label = span("label", "Thinking…");
    var meta = span("meta", "");
    summaryNode.append(glyph, label, meta);

    var body = document.createElement("div");
    body.className = "thinking-text";
    body.setAttribute("dir", "auto");

    details.append(summaryNode, body);
    timeline.append(details);

    var block = {
      details: details,
      label: label,
      body: body,
      startedAt: Date.now(),
      completed: false,
      complete: function () {
        if (block.completed) {
          return;
        }
        block.completed = true;
        details.dataset.state = "complete";
        label.textContent = "Thought for " + duration(Date.now() - block.startedAt);
      }
    };

    activeThinking = block;
    return block;
  }

  function beginOutput() {
    activeOutput = null;
    return outputBlock();
  }

  function outputBlock() {
    if (activeOutput) {
      return activeOutput;
    }

    var box = document.createElement("div");
    box.className = "output";

    var glyph = span("glyph", "●");
    glyph.setAttribute("aria-hidden", "true");

    var body = document.createElement("div");
    body.className = "output-text";
    body.setAttribute("dir", "auto");

    box.append(glyph, body);
    timeline.append(box);

    activeOutput = { box: box, body: body };
    lastOutput = activeOutput;
    return activeOutput;
  }

  function setMessage(content) {
    // message_end can arrive after step_finish, which clears the streaming
    // cursor. Fall back to the last output block so the final text replaces
    // what was streamed instead of appearing twice.
    var block = activeOutput || lastOutput;
    if (!block) {
      block = outputBlock();
    }
    var streamed = block.body.textContent;
    block.body.replaceChildren();
    block.body.textContent = content || streamed;
  }

  function toolCard(payload) {
    var id = payload.id || payload.name || "tool";
    var card = tools.get(id);
    if (card) {
      if (payload.name && !card.name) {
        card.name = payload.name;
        card.details.dataset.toolName = payload.name;
        card.label.textContent = humanTool(card.name, card.parsedArgs);
      }
      return card;
    }

    var details = document.createElement("details");
    details.className = "tool";
    details.dataset.status = "pending";
    details.dataset.toolName = payload.name || "";

    var summaryNode = document.createElement("summary");
    var glyph = span("glyph", "●");
    var label = span("tool-label", humanTool(payload.name || "", null));
    var state = span("tool-state", "queued");
    var chev = span("chev", "›");
    chev.setAttribute("aria-hidden", "true");
    summaryNode.append(glyph, label, state, chev);

    var detail = document.createElement("div");
    detail.className = "tool-detail";

    var detailLabel = document.createElement("div");
    detailLabel.className = "tool-detail-label";
    detailLabel.textContent = "arguments";

    var args = document.createElement("pre");
    args.className = "tool-args";
    args.setAttribute("dir", "auto");

    detail.append(detailLabel, args);
    details.append(summaryNode, detail);

    var result = document.createElement("div");
    result.className = "tool-result";
    result.hidden = true;

    var resultGlyph = span("glyph", "⎿");
    var resultText = span("result-text", "");
    var raw = document.createElement("details");
    raw.className = "result-raw";
    raw.hidden = true;
    var rawSummary = document.createElement("summary");
    rawSummary.textContent = "raw";
    var rawBody = document.createElement("pre");
    rawBody.setAttribute("dir", "auto");
    raw.append(rawSummary, rawBody);
    result.append(resultGlyph, resultText, raw);

    timeline.append(details, result);

    card = {
      details: details,
      label: label,
      state: state,
      args: args,
      result: result,
      resultGlyph: resultGlyph,
      resultText: resultText,
      raw: raw,
      rawBody: rawBody,
      name: payload.name || "",
      parsedArgs: null,
      output: ""
    };
    tools.set(id, card);
    toolCalls += 1;
    updateToolCount();
    return card;
  }

  function setToolArgs(card, value) {
    card.parsedArgs = parsed(value);
    card.args.textContent = card.parsedArgs ? JSON.stringify(card.parsedArgs, null, 2) : text(value);
    card.label.textContent = humanTool(card.name, card.parsedArgs);
  }

  function setToolState(card, status, label) {
    card.details.dataset.status = status;
    card.state.textContent = label;
  }

  function setToolOutput(card, chunk) {
    card.output += chunk || "";
    card.result.hidden = false;
    card.resultGlyph.textContent = "⎿";
    card.resultText.textContent = "receiving output · " + bytes(card.output.length);
  }

  function setToolResult(card, value, isError) {
    card.result.hidden = false;
    card.result.dataset.error = isError ? "true" : "false";
    card.resultGlyph.textContent = isError ? "✖" : "⎿";
    card.resultText.textContent = describeResult(card.name, value, isError);

    var rawText = text(value);
    card.raw.hidden = rawText === "";
    card.rawBody.textContent = rawText;

    setToolState(card, isError ? "failed" : "done", isError ? "failed" : "");
  }

  function usageLine(usage) {
    if (!usage) {
      return;
    }
    var parts = usageParts(usage);
    if (!parts.length) {
      return;
    }
    var line = document.createElement("div");
    line.className = "usage-line";
    line.textContent = "· " + parts.join(" · ");
    timeline.append(line);
  }

  function usageParts(usage) {
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
    return parts;
  }

  function marker(kind, textValue, detail) {
    var item = document.createElement("div");
    item.className = "event event-" + kind;

    var glyph = span("event-glyph", glyphFor(kind));
    glyph.setAttribute("aria-hidden", "true");

    var body = document.createElement("div");
    body.className = "event-body";

    var line = span("event-text", textValue);
    body.append(line);

    if (detail) {
      var note = span("event-detail", detail);
      note.setAttribute("dir", "auto");
      body.append(note);
    }

    item.append(glyph, body);
    timeline.append(item);
  }

  function removePlaceholder() {
    var placeholder = document.getElementById("timeline-empty");
    if (placeholder) {
      placeholder.remove();
    }
  }

  function follow() {
    if (!following || !scroll || followQueued) {
      return;
    }
    followQueued = true;
    window.requestAnimationFrame(function () {
      followQueued = false;
      scroll.scrollTop = scroll.scrollHeight;
    });
  }

  function setActivity(label) {
    if (ended || !activity) {
      return;
    }
    activity.hidden = false;
    if (label !== activityText) {
      activityText = label;
      activityStartedAt = Date.now();
      if (activityLabel) {
        activityLabel.textContent = label;
      }
    }
    updateActivityTime();
  }

  function updateActivityTime() {
    if (!activityTime || !activityStartedAt || ended) {
      return;
    }
    var elapsed = Date.now() - activityStartedAt;
    activityTime.textContent = elapsed >= 1000 ? "· " + duration(elapsed) : "";
  }

  function clearActivity() {
    if (activity) {
      activity.hidden = true;
    }
    activityText = "";
    activityStartedAt = 0;
  }

  function tick() {
    if (!ended) {
      setClock(Date.now() - startedAt);
    }
    updateActivityTime();
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

  function updateToolCount() {
    if (toolCount) {
      toolCount.textContent = count(toolCalls, "tool call");
    }
  }

  function appendText(parent, value) {
    if (value === undefined || value === null || value === "") {
      return;
    }
    parent.append(document.createTextNode(String(value)));
  }

  function span(className, value) {
    var node = document.createElement("span");
    node.className = className;
    if (value !== undefined) {
      node.textContent = value;
    }
    return node;
  }

  function parsed(value) {
    if (value === null || value === undefined) {
      return null;
    }
    if (typeof value === "object") {
      return value;
    }
    if (typeof value !== "string") {
      return null;
    }
    try {
      return JSON.parse(value);
    } catch (error) {
      return null;
    }
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

  function humanTool(name, args) {
    var a = args || {};
    switch (name) {
      case "get_pull_request_diff":
        return "Reading the pull request diff";
      case "get_pull_request_metadata":
        return "Reading pull request metadata";
      case "read_file":
        return "Reading " + (a.path || a.file || "a file");
      case "search_memory":
        return "Searching recorded rules" + (a.query ? " for “" + shortText(a.query, 56) + "”" : "");
      case "submit_pr_review":
        return "Submitting the review" + (a.event ? " (" + a.event + ")" : "");
      case "resolve_review_thread":
        return "Resolving a review thread";
      case "list_review_threads":
        return "Reading review threads";
      case "list_pull_request_comments":
      case "get_pull_request_comments":
        return "Reading pull request comments";
      default:
        return name ? "Running " + String(name).replace(/_/g, " ") : "Running a tool";
    }
  }

  function toolActivity(card) {
    var args = card.parsedArgs || {};
    switch (card.name) {
      case "get_pull_request_diff":
      case "get_pull_request_metadata":
        return "Reviewing the pull request…";
      case "read_file":
        return "Reviewing " + (args.path || args.file || "a file") + "…";
      case "search_memory":
        return "Reviewing recorded rules…";
      case "submit_pr_review":
        return "Submitting the review…";
      case "resolve_review_thread":
        return "Resolving a review thread…";
      default:
        return "Reviewing…";
    }
  }

  function describeResult(name, value, isError) {
    var raw = text(value).trim();
    if (isError) {
      return firstLine(raw) || "failed";
    }

    var object = parsed(value);
    if (name === "submit_pr_review" && object && typeof object === "object" && !Array.isArray(object)) {
      var parts = [];
      if (object.event) {
        parts.push(object.event);
      }
      if (Array.isArray(object.comments) && object.comments.length) {
        parts.push(count(object.comments.length, "comment"));
      }
      if (parts.length) {
        return parts.join(" · ");
      }
    }

    if (name === "get_pull_request_diff" || name === "read_file" || name === "get_file_content") {
      return raw ? count(raw.split("\n").length, "line") : "no content";
    }
    if (Array.isArray(object)) {
      return count(object.length, "item");
    }
    if (object && typeof object === "object") {
      var keys = Object.keys(object);
      if (keys.length) {
        return count(keys.length, "field");
      }
    }
    if (!raw) {
      return "done";
    }
    var line = firstLine(raw);
    return line.length > 120 ? line.slice(0, 117) + "…" : line;
  }

  function firstLine(value) {
    return String(value || "").split("\n")[0].trim();
  }

  function shortText(value, length) {
    var textValue = String(value);
    return textValue.length > length ? textValue.slice(0, length - 1) + "…" : textValue;
  }

  function metadataDetail(payload) {
    var parts = [];
    if (payload.head_sha) {
      parts.push("head " + shortSHA(payload.head_sha));
    }
    if (payload.base_sha) {
      parts.push("base " + shortSHA(payload.base_sha));
    }
    if (payload.changed_files) {
      parts.push(count(payload.changed_files, "file"));
    }
    return parts.join(" · ");
  }

  function guidelinesDetail(payload) {
    var parts = [];
    if (payload.path) {
      parts.push(payload.path);
    }
    if (payload.ref) {
      parts.push("at " + shortSHA(payload.ref));
    }
    if (payload.bytes) {
      parts.push(bytes(payload.bytes));
    }
    return parts.join(" · ");
  }

  function repositorySettingsDetail(payload) {
    if (!payload.found) {
      return "The review runs on the operator's settings.";
    }
    var parts = [];
    if (payload.resolve_threads !== undefined) {
      parts.push("resolve threads " + (payload.resolve_threads ? "on" : "off"));
    }
    if (payload.request_changes !== undefined) {
      parts.push("request changes " + (payload.request_changes ? "allowed" : "off"));
    }
    if (payload.max_inline_comments) {
      parts.push("inline comments ≤ " + payload.max_inline_comments);
    }
    if (payload.ignore_paths && payload.ignore_paths.length) {
      parts.push("ignoring " + payload.ignore_paths.join(", "));
    }
    if (payload.default_focus) {
      parts.push("focus " + payload.default_focus);
    }
    if (payload.chat_provider) {
      parts.push("provider " + payload.chat_provider);
    }
    return parts.join(" · ");
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

  function glyphFor(kind) {
    switch (kind) {
      case "turn":
        return "▸";
      case "review":
        return "✔";
      case "warn":
        return "!";
      case "error":
        return "✖";
      default:
        return "●";
    }
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
