(function() {
    "use strict";

    // State
    var totalIterations = 0;
    var totalTokens = 0;
    var totalDurationMs = 0;
    var totalErrors = 0;
    var maxIter = 0;

    // DOM references
    var els = {
        connectionStatus: document.getElementById("connection-status"),
        taskInfo: document.getElementById("task-info"),
        taskDescription: document.getElementById("task-description"),
        taskModel: document.getElementById("task-model"),
        taskMaxIter: document.getElementById("task-max-iter"),
        summary: document.getElementById("summary"),
        totalIterations: document.getElementById("total-iterations"),
        totalTokens: document.getElementById("total-tokens"),
        totalDuration: document.getElementById("total-duration"),
        totalErrors: document.getElementById("total-errors"),
        progressBarContainer: document.getElementById("progress-bar-container"),
        progressBar: document.getElementById("progress-bar"),
        progressText: document.getElementById("progress-text"),
        iterations: document.getElementById("iterations"),
        spinner: document.getElementById("spinner"),
        completionBanner: document.getElementById("completion-banner"),
        completionTitle: document.getElementById("completion-title"),
        completionMessage: document.getElementById("completion-message"),
        taskMode: document.getElementById("task-mode"),
        taskAgent: document.getElementById("task-agent"),
        taskAgentModel: document.getElementById("task-agent-model"),
        agentHealth: document.getElementById("agent-health"),
        healthIndicators: document.getElementById("health-indicators")
    };

    function formatDuration(ms) {
        if (ms < 1000) return ms + "ms";
        if (ms < 60000) return (ms / 1000).toFixed(1) + "s";
        var m = Math.floor(ms / 60000);
        var s = Math.round((ms % 60000) / 1000);
        return m + "m " + s + "s";
    }

    function formatNumber(n) {
        return n.toLocaleString();
    }

    function updateSummary() {
        els.totalIterations.textContent = totalIterations;
        els.totalTokens.textContent = formatNumber(totalTokens);
        els.totalDuration.textContent = formatDuration(totalDurationMs);
        els.totalErrors.textContent = totalErrors;

        if (maxIter > 0) {
            els.progressBarContainer.classList.remove("hidden");
            var pct = Math.min(100, Math.round((totalIterations / maxIter) * 100));
            els.progressBar.style.width = pct + "%";
            els.progressText.textContent = totalIterations + " / " + maxIter;
        }
    }

    function createIterationCard(data) {
        var card = document.createElement("div");
        card.className = "iteration-card";
        card.id = "iter-" + data.iteration;

        if (data.error) card.classList.add("has-error");

        // Header
        var header = document.createElement("div");
        header.className = "iteration-header expanded";

        var numSpan = document.createElement("span");
        numSpan.className = "iteration-number";
        numSpan.textContent = "Iteration " + data.iteration +
            (maxIter > 0 ? " / " + maxIter : "");

        var metaDiv = document.createElement("div");
        metaDiv.className = "iteration-meta";

        if (data.tokens) {
            var tokSpan = document.createElement("span");
            tokSpan.textContent = formatNumber(data.tokens.total) + " tokens";
            metaDiv.appendChild(tokSpan);
        }

        if (data.duration_ms) {
            var durSpan = document.createElement("span");
            durSpan.textContent = formatDuration(data.duration_ms);
            metaDiv.appendChild(durSpan);
        }

        if (data.error) {
            var errSpan = document.createElement("span");
            errSpan.className = "error";
            errSpan.textContent = "Error";
            metaDiv.appendChild(errSpan);
        }

        if (data.agent) {
            var badge = document.createElement("span");
            badge.className = "agent-badge " + data.agent;
            badge.textContent = data.agent;
            numSpan.appendChild(badge);
        }

        header.appendChild(numSpan);
        header.appendChild(metaDiv);

        // Body
        var body = document.createElement("div");
        body.className = "iteration-body";

        if (data.orchestrator) {
            var orchSection = document.createElement("div");
            orchSection.className = "iteration-section";
            var orchH3 = document.createElement("h3");
            var orchLabel = "Orchestrator \u2192 Agent";
            if (data.agent) {
                orchLabel = "Orchestrator \u2192 " + data.agent.charAt(0).toUpperCase() + data.agent.slice(1);
            }
            orchH3.textContent = orchLabel;
            var orchCode = document.createElement("pre");
            orchCode.className = "code-block";
            orchCode.textContent = data.orchestrator;
            orchSection.appendChild(orchH3);
            orchSection.appendChild(orchCode);
            body.appendChild(orchSection);
        }

        var agentOutputText = data.agent_output || data.claude_output;
        if (agentOutputText) {
            var ccSection = document.createElement("div");
            ccSection.className = "iteration-section";
            var ccH3 = document.createElement("h3");
            ccH3.textContent = "Agent Output";
            var ccCode = document.createElement("pre");
            ccCode.className = "code-block";
            ccCode.textContent = agentOutputText;
            ccSection.appendChild(ccH3);
            ccSection.appendChild(ccCode);
            body.appendChild(ccSection);
        }

        if (data.memory_facts && data.memory_facts.length > 0) {
            var memSection = document.createElement("div");
            memSection.className = "iteration-section";
            var memH3 = document.createElement("h3");
            memH3.textContent = "Memory Saved";
            var memList = document.createElement("ul");
            memList.className = "memory-facts";
            for (var mi = 0; mi < data.memory_facts.length; mi++) {
                var li = document.createElement("li");
                li.textContent = data.memory_facts[mi];
                memList.appendChild(li);
            }
            memSection.appendChild(memH3);
            memSection.appendChild(memList);
            body.appendChild(memSection);
        }

        if (data.error) {
            var errSection = document.createElement("div");
            errSection.className = "iteration-section";
            var errH3 = document.createElement("h3");
            errH3.textContent = "Error";
            var errPre = document.createElement("pre");
            errPre.className = "code-block";
            errPre.style.color = "var(--error)";
            errPre.textContent = data.error;
            errSection.appendChild(errH3);
            errSection.appendChild(errPre);
            body.appendChild(errSection);
        }

        // Toggle collapse on header click
        header.addEventListener("click", function() {
            body.classList.toggle("collapsed");
            header.classList.toggle("expanded");
        });

        card.appendChild(header);
        card.appendChild(body);
        return card;
    }

    function addIterationStartPlaceholder(iteration) {
        var existing = document.getElementById("iter-" + iteration);
        if (existing) existing.remove();

        var card = document.createElement("div");
        card.className = "iteration-card in-progress";
        card.id = "iter-" + iteration;

        var header = document.createElement("div");
        header.className = "iteration-header";
        var numSpan = document.createElement("span");
        numSpan.className = "iteration-number";
        numSpan.textContent = "Iteration " + iteration +
            (maxIter > 0 ? " / " + maxIter : "") + " \u2014 In Progress...";
        header.appendChild(numSpan);
        card.appendChild(header);

        // Insert at top (newest first)
        if (els.iterations.firstChild) {
            els.iterations.insertBefore(card, els.iterations.firstChild);
        } else {
            els.iterations.appendChild(card);
        }
        els.spinner.classList.remove("hidden");
    }

    function handleEvent(event) {
        var data;
        try {
            data = JSON.parse(event.data);
        } catch (e) {
            return;
        }

        switch (data.type) {
            case "connected":
                els.connectionStatus.textContent = "Connected";
                els.connectionStatus.className = "status-badge connected";
                break;

            case "task_info":
                els.taskInfo.classList.remove("hidden");
                els.taskDescription.textContent = data.task || "";
                els.taskAgent.textContent = data.agent || "Claude Code";
                els.taskAgentModel.textContent = data.agent_model || data.model || "";
                els.taskModel.textContent = data.model || "";
                maxIter = data.max_iter || 0;
                els.taskMaxIter.textContent = maxIter > 0 ? maxIter : "Unlimited";
                if (data.mode === "multi-agent" && els.taskMode) {
                    els.taskMode.innerHTML = "";
                    var modeBadge = document.createElement("span");
                    modeBadge.className = "mode-badge";
                    modeBadge.textContent = "Multi-Agent";
                    els.taskMode.appendChild(modeBadge);
                }
                els.summary.classList.remove("hidden");
                els.spinner.classList.remove("hidden");
                updateSummary();
                break;

            case "iteration_start":
                addIterationStartPlaceholder(data.iteration);
                break;

            case "iteration_end":
                els.spinner.classList.add("hidden");
                totalIterations = data.iteration;
                if (data.tokens) {
                    totalTokens += data.tokens.total;
                }
                if (data.duration_ms) {
                    totalDurationMs += data.duration_ms;
                }
                if (data.error) {
                    totalErrors++;
                }
                updateSummary();

                // Replace placeholder with full card
                var existing = document.getElementById("iter-" + data.iteration);
                if (existing) existing.remove();
                var card = createIterationCard(data);
                // Insert at top (newest first)
                if (els.iterations.firstChild) {
                    els.iterations.insertBefore(card, els.iterations.firstChild);
                } else {
                    els.iterations.appendChild(card);
                }
                break;

            case "error":
                totalErrors++;
                updateSummary();
                if (data.iteration > 0) {
                    var errExisting = document.getElementById("iter-" + data.iteration);
                    if (errExisting) errExisting.remove();
                    var errCard = createIterationCard(data);
                    if (els.iterations.firstChild) {
                        els.iterations.insertBefore(errCard, els.iterations.firstChild);
                    } else {
                        els.iterations.appendChild(errCard);
                    }
                }
                break;

            case "health_status":
                if (data.agents && data.agents.length > 0) {
                    els.agentHealth.classList.remove("hidden");
                    els.healthIndicators.innerHTML = "";
                    for (var hi = 0; hi < data.agents.length; hi++) {
                        var agent = data.agents[hi];
                        var card = document.createElement("div");
                        card.className = "health-card";

                        var dot = document.createElement("span");
                        dot.className = "health-dot " + (agent.alive ? "alive" : "dead");
                        card.appendChild(dot);

                        var label = document.createElement("span");
                        label.className = "health-label";
                        label.textContent = agent.role || agent.session;
                        card.appendChild(label);

                        var status = document.createElement("span");
                        status.className = "health-status-text";
                        status.textContent = agent.alive ? "Running" : "Dead (exit " + agent.exit_status + ")";
                        card.appendChild(status);

                        if (!agent.alive) {
                            var btn = document.createElement("button");
                            btn.className = "health-restart-btn";
                            btn.textContent = "Restart";
                            btn.setAttribute("data-session", agent.session);
                            btn.addEventListener("click", (function(sess) {
                                return function() {
                                    fetch("/api/restart?session=" + encodeURIComponent(sess), { method: "POST" })
                                        .then(function(r) {
                                            if (!r.ok) r.text().then(function(t) { alert("Restart failed: " + t); });
                                        })
                                        .catch(function(e) { alert("Restart error: " + e); });
                                };
                            })(agent.session));
                            card.appendChild(btn);
                        }

                        els.healthIndicators.appendChild(card);
                    }
                }
                break;

            case "complete":
                els.spinner.classList.add("hidden");
                els.completionBanner.classList.remove("hidden");
                if (data.error) {
                    els.completionBanner.classList.add("failure");
                    els.completionTitle.textContent = "Task Failed";
                    els.completionMessage.textContent = data.error;
                } else {
                    els.completionBanner.classList.add("success");
                    els.completionTitle.textContent = "Task Complete";
                    els.completionMessage.textContent =
                        "Completed successfully after " + data.iteration + " iteration(s).";
                }
                break;
        }
    }

    // SSE connection with auto-reconnect
    function connect() {
        var source = new EventSource("/events");

        source.onmessage = handleEvent;

        source.onerror = function() {
            els.connectionStatus.textContent = "Disconnected";
            els.connectionStatus.className = "status-badge disconnected";
            source.close();
            setTimeout(connect, 3000);
        };
    }

    connect();
})();
