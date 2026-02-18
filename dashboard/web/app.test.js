// JavaScript tests for the dashboard UI logic.
// Runs with Node.js built-in test runner: node --test web/app.test.js
// No external dependencies required.

const { describe, it, beforeEach } = require("node:test");
const assert = require("node:assert/strict");

// ---------------------------------------------------------------------------
// Minimal DOM mock — just enough to run app.js event handlers.
// ---------------------------------------------------------------------------

// Global registry for getElementById lookups (includes dynamically created elements).
let elementRegistry;

class MockElement {
    constructor(tag) {
        this.tagName = tag || "DIV";
        this._className = "";
        this.textContent = "";
        this.innerHTML = "";
        this.children = [];
        this.style = {};
        this._id = "";
        this._listeners = {};
        this.parentNode = null;
        this.firstChild = null;
    }

    get id() { return this._id; }
    set id(val) {
        // Remove old id from registry.
        if (this._id && elementRegistry) {
            delete elementRegistry[this._id];
        }
        this._id = val;
        // Register new id.
        if (val && elementRegistry) {
            elementRegistry[val] = this;
        }
    }

    get className() { return this._className; }
    set className(val) { this._className = val; }

    get classList() {
        const self = this;
        return {
            add(c) {
                const classes = new Set(self._className.split(/\s+/).filter(Boolean));
                classes.add(c);
                self._className = [...classes].join(" ");
            },
            remove(c) {
                const classes = new Set(self._className.split(/\s+/).filter(Boolean));
                classes.delete(c);
                self._className = [...classes].join(" ");
            },
            toggle(c) {
                const classes = new Set(self._className.split(/\s+/).filter(Boolean));
                if (classes.has(c)) classes.delete(c);
                else classes.add(c);
                self._className = [...classes].join(" ");
            },
            contains(c) {
                return self._className.split(/\s+/).includes(c);
            },
            has(c) {
                return self._className.split(/\s+/).includes(c);
            },
        };
    }

    appendChild(child) {
        child.parentNode = this;
        this.children.push(child);
        this.firstChild = this.children[0];
        return child;
    }
    insertBefore(child, ref) {
        child.parentNode = this;
        const idx = this.children.indexOf(ref);
        if (idx >= 0) {
            this.children.splice(idx, 0, child);
        } else {
            this.children.push(child);
        }
        this.firstChild = this.children[0];
        return child;
    }
    remove() {
        if (this.parentNode) {
            const idx = this.parentNode.children.indexOf(this);
            if (idx >= 0) this.parentNode.children.splice(idx, 1);
            this.parentNode.firstChild = this.parentNode.children[0] || null;
        }
        // Remove from registry.
        if (this._id && elementRegistry && elementRegistry[this._id] === this) {
            delete elementRegistry[this._id];
        }
    }
    addEventListener(evt, fn) {
        if (!this._listeners[evt]) this._listeners[evt] = [];
        this._listeners[evt].push(fn);
    }
}

// Build a fake DOM with all the element IDs that app.js expects.
function buildFakeDOM() {
    elementRegistry = {};
    const ids = [
        "connection-status", "task-info", "task-description", "task-model",
        "task-max-iter", "summary", "total-iterations", "total-tokens",
        "total-duration", "total-errors", "progress-bar-container",
        "progress-bar", "progress-text", "iterations", "spinner",
        "completion-banner", "completion-title", "completion-message",
    ];
    for (const id of ids) {
        const el = new MockElement("DIV");
        el.id = id;
    }
    return elementRegistry;
}

// ---------------------------------------------------------------------------
// Extract the event handler from app.js by simulating its IIFE environment.
// ---------------------------------------------------------------------------

function loadApp() {
    const elements = buildFakeDOM();

    const mockDocument = {
        getElementById: (id) => elementRegistry[id] || new MockElement("DIV"),
        createElement: (tag) => new MockElement(tag.toUpperCase()),
    };

    let capturedOnMessage = null;
    class MockEventSource {
        constructor(url) {
            this.url = url;
            this.readyState = 1;
        }
        set onmessage(fn) { capturedOnMessage = fn; }
        get onmessage() { return capturedOnMessage; }
        set onerror(fn) { this._onerror = fn; }
        close() { this.readyState = 2; }
    }

    const fs = require("node:fs");
    const path = require("node:path");
    const code = fs.readFileSync(path.join(__dirname, "app.js"), "utf-8");

    const fn = new Function(
        "document", "EventSource", "setTimeout", "console",
        code
    );
    fn(mockDocument, MockEventSource, () => {}, console);

    return { elements, handleEvent: capturedOnMessage };
}

// Helper to send an SSE-like event to the handler.
function sendEvent(handler, data) {
    handler({ data: JSON.stringify(data) });
}

// Helper: convert textContent to string for comparison (app.js assigns numbers).
function text(el) {
    return String(el.textContent);
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe("Dashboard app.js", () => {
    let elements, handleEvent;

    beforeEach(() => {
        const app = loadApp();
        elements = app.elements;
        handleEvent = app.handleEvent;
    });

    describe("connected event", () => {
        it("sets connection status to Connected", () => {
            sendEvent(handleEvent, { type: "connected" });
            assert.equal(text(elements["connection-status"]), "Connected");
            assert.equal(elements["connection-status"].className, "status-badge connected");
        });
    });

    describe("task_info event", () => {
        it("shows task info section with task details", () => {
            sendEvent(handleEvent, {
                type: "task_info",
                task: "Build a REST API",
                model: "anthropic/claude-opus-4.6",
                max_iter: 10,
            });

            assert.equal(text(elements["task-description"]), "Build a REST API");
            assert.equal(text(elements["task-model"]), "anthropic/claude-opus-4.6");
            assert.equal(text(elements["task-max-iter"]), "10");
            assert.ok(!elements["task-info"].classList.contains("hidden"));
            assert.ok(!elements["summary"].classList.contains("hidden"));
        });

        it("shows Unlimited when max_iter is 0", () => {
            sendEvent(handleEvent, {
                type: "task_info",
                task: "test",
                model: "test",
                max_iter: 0,
            });
            assert.equal(text(elements["task-max-iter"]), "Unlimited");
        });
    });

    describe("iteration_start event", () => {
        it("creates a placeholder card with in-progress class", () => {
            sendEvent(handleEvent, { type: "task_info", task: "t", model: "m", max_iter: 0 });
            sendEvent(handleEvent, { type: "iteration_start", iteration: 1 });

            const container = elements["iterations"];
            assert.equal(container.children.length, 1);
            assert.ok(container.children[0].classList.contains("in-progress"));
            assert.equal(container.children[0].id, "iter-1");
        });

        it("shows spinner while iteration is in progress", () => {
            sendEvent(handleEvent, { type: "task_info", task: "t", model: "m", max_iter: 0 });
            sendEvent(handleEvent, { type: "iteration_start", iteration: 1 });

            assert.ok(!elements["spinner"].classList.contains("hidden"));
        });
    });

    describe("iteration_end event", () => {
        it("replaces placeholder with full iteration card", () => {
            sendEvent(handleEvent, { type: "task_info", task: "t", model: "m", max_iter: 0 });
            sendEvent(handleEvent, { type: "iteration_start", iteration: 1 });
            sendEvent(handleEvent, {
                type: "iteration_end",
                iteration: 1,
                max_iter: 0,
                duration_ms: 5000,
                tokens: { prompt: 100, completion: 50, total: 150 },
                orchestrator: "echo hello",
                claude_output: "hello\n",
            });

            const container = elements["iterations"];
            assert.equal(container.children.length, 1);
            assert.ok(!container.children[0].classList.contains("in-progress"));
            assert.equal(container.children[0].id, "iter-1");
        });

        it("hides spinner after iteration completes", () => {
            sendEvent(handleEvent, { type: "task_info", task: "t", model: "m", max_iter: 0 });
            sendEvent(handleEvent, { type: "iteration_start", iteration: 1 });
            sendEvent(handleEvent, {
                type: "iteration_end",
                iteration: 1,
                tokens: { prompt: 10, completion: 5, total: 15 },
            });

            assert.ok(elements["spinner"].classList.contains("hidden"));
        });

        it("updates summary counters", () => {
            sendEvent(handleEvent, { type: "task_info", task: "t", model: "m", max_iter: 0 });
            sendEvent(handleEvent, {
                type: "iteration_end",
                iteration: 1,
                duration_ms: 3000,
                tokens: { prompt: 100, completion: 50, total: 150 },
                orchestrator: "cmd1",
            });
            sendEvent(handleEvent, {
                type: "iteration_end",
                iteration: 2,
                duration_ms: 2000,
                tokens: { prompt: 200, completion: 100, total: 300 },
                orchestrator: "cmd2",
            });

            assert.equal(text(elements["total-iterations"]), "2");
            // Total tokens: 150 + 300 = 450
            assert.ok(text(elements["total-tokens"]).includes("450"));
            assert.equal(text(elements["total-errors"]), "0");
        });

        it("counts errors in iteration_end events", () => {
            sendEvent(handleEvent, { type: "task_info", task: "t", model: "m", max_iter: 0 });
            sendEvent(handleEvent, {
                type: "iteration_end",
                iteration: 1,
                tokens: { prompt: 10, completion: 5, total: 15 },
                error: "tmux error: session lost",
            });

            assert.equal(text(elements["total-errors"]), "1");
        });

        it("marks error cards with has-error class", () => {
            sendEvent(handleEvent, { type: "task_info", task: "t", model: "m", max_iter: 0 });
            sendEvent(handleEvent, {
                type: "iteration_end",
                iteration: 1,
                tokens: { prompt: 10, completion: 5, total: 15 },
                error: "something broke",
            });

            const card = elements["iterations"].children[0];
            assert.ok(card.classList.contains("has-error"));
        });
    });

    describe("error event", () => {
        it("increments error counter", () => {
            sendEvent(handleEvent, { type: "task_info", task: "t", model: "m", max_iter: 0 });
            sendEvent(handleEvent, {
                type: "error",
                iteration: 1,
                error: "API error (1/3): rate limited",
            });

            assert.equal(text(elements["total-errors"]), "1");
        });

        it("creates an error card in the iterations container", () => {
            sendEvent(handleEvent, { type: "task_info", task: "t", model: "m", max_iter: 0 });
            sendEvent(handleEvent, {
                type: "error",
                iteration: 1,
                error: "API error (1/3): rate limited",
            });

            const container = elements["iterations"];
            assert.equal(container.children.length, 1);
            assert.ok(container.children[0].classList.contains("has-error"));
        });
    });

    describe("complete event", () => {
        it("shows success banner on successful completion", () => {
            sendEvent(handleEvent, { type: "task_info", task: "t", model: "m", max_iter: 0 });
            sendEvent(handleEvent, { type: "complete", iteration: 3 });

            assert.ok(!elements["completion-banner"].classList.contains("hidden"));
            assert.ok(elements["completion-banner"].classList.contains("success"));
            assert.equal(text(elements["completion-title"]), "Task Complete");
            assert.ok(text(elements["completion-message"]).includes("3"));
        });

        it("shows failure banner when error is present", () => {
            sendEvent(handleEvent, { type: "task_info", task: "t", model: "m", max_iter: 0 });
            sendEvent(handleEvent, {
                type: "complete",
                iteration: 10,
                error: "reached maximum iterations (10)",
            });

            assert.ok(elements["completion-banner"].classList.contains("failure"));
            assert.equal(text(elements["completion-title"]), "Task Failed");
            assert.ok(text(elements["completion-message"]).includes("maximum iterations"));
        });

        it("hides spinner on completion", () => {
            sendEvent(handleEvent, { type: "task_info", task: "t", model: "m", max_iter: 0 });
            sendEvent(handleEvent, { type: "complete", iteration: 1 });

            assert.ok(elements["spinner"].classList.contains("hidden"));
        });
    });

    describe("progress bar", () => {
        it("shows progress bar when max_iter > 0", () => {
            sendEvent(handleEvent, { type: "task_info", task: "t", model: "m", max_iter: 5 });
            sendEvent(handleEvent, {
                type: "iteration_end",
                iteration: 2,
                max_iter: 5,
                tokens: { prompt: 10, completion: 5, total: 15 },
            });

            assert.ok(!elements["progress-bar-container"].classList.contains("hidden"));
            assert.equal(elements["progress-bar"].style.width, "40%");
            assert.equal(text(elements["progress-text"]), "2 / 5");
        });

        it("does not show progress bar when max_iter is 0 (unlimited)", () => {
            sendEvent(handleEvent, { type: "task_info", task: "t", model: "m", max_iter: 0 });
            sendEvent(handleEvent, {
                type: "iteration_end",
                iteration: 1,
                max_iter: 0,
                tokens: { prompt: 10, completion: 5, total: 15 },
            });

            // Width should not have been set.
            assert.equal(elements["progress-bar"].style.width, undefined);
        });
    });

    describe("iteration ordering", () => {
        it("inserts newest iterations at the top", () => {
            sendEvent(handleEvent, { type: "task_info", task: "t", model: "m", max_iter: 0 });
            sendEvent(handleEvent, {
                type: "iteration_end",
                iteration: 1,
                tokens: { prompt: 10, completion: 5, total: 15 },
                orchestrator: "first",
            });
            sendEvent(handleEvent, {
                type: "iteration_end",
                iteration: 2,
                tokens: { prompt: 10, completion: 5, total: 15 },
                orchestrator: "second",
            });

            const container = elements["iterations"];
            assert.equal(container.children.length, 2);
            // Newest (iteration 2) should be first child.
            assert.equal(container.children[0].id, "iter-2");
            assert.equal(container.children[1].id, "iter-1");
        });
    });

    describe("full iteration lifecycle", () => {
        it("handles task_info -> start -> end -> complete sequence", () => {
            sendEvent(handleEvent, {
                type: "task_info",
                task: "Full lifecycle test",
                model: "test-model",
                max_iter: 3,
            });

            // Iteration 1
            sendEvent(handleEvent, { type: "iteration_start", iteration: 1 });
            assert.ok(!elements["spinner"].classList.contains("hidden"));

            sendEvent(handleEvent, {
                type: "iteration_end",
                iteration: 1,
                max_iter: 3,
                duration_ms: 2000,
                tokens: { prompt: 50, completion: 25, total: 75 },
                orchestrator: "echo hello",
                claude_output: "hello",
            });
            assert.equal(text(elements["total-iterations"]), "1");
            assert.equal(elements["progress-bar"].style.width, "33%");

            // Iteration 2
            sendEvent(handleEvent, { type: "iteration_start", iteration: 2 });
            sendEvent(handleEvent, {
                type: "iteration_end",
                iteration: 2,
                max_iter: 3,
                duration_ms: 3000,
                tokens: { prompt: 100, completion: 50, total: 150 },
                orchestrator: "TASK_COMPLETE",
            });
            assert.equal(text(elements["total-iterations"]), "2");
            assert.equal(elements["progress-bar"].style.width, "67%");

            // Complete
            sendEvent(handleEvent, { type: "complete", iteration: 2 });
            assert.ok(elements["completion-banner"].classList.contains("success"));
            assert.ok(elements["spinner"].classList.contains("hidden"));

            // Verify final state
            const container = elements["iterations"];
            assert.equal(container.children.length, 2);
            assert.ok(text(elements["total-tokens"]).includes("225"));
        });
    });

    describe("malformed events", () => {
        it("ignores invalid JSON without throwing", () => {
            handleEvent({ data: "not valid json{{{" });
            // Should not have changed any state.
            assert.equal(text(elements["total-iterations"]), "");
        });

        it("ignores unknown event types", () => {
            sendEvent(handleEvent, { type: "unknown_type", foo: "bar" });
            assert.equal(text(elements["total-iterations"]), "");
        });
    });
});
