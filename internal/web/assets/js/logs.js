// Follows an app's log over server-sent events, appending lines as they arrive.
//
// The server renders the tail first, so this only ever adds to what is already
// on the page. If the stream cannot be established it hands the pane back to
// the htmx poll that rendered it, which is why that endpoint still exists: a
// network that eats SSE is otherwise a log pane that never fills and never says
// why.
(function () {
	"use strict";

	// Two failures before giving up. EventSource retries by itself and the
	// first failure is usually a redeploy or a laptop waking, so falling back
	// on one would trade a working stream for a poll at the first hiccup.
	var MAX_FAILURES = 2;

	function init(root) {
		var url = root.getAttribute("data-log-stream");
		if (!url || typeof EventSource === "undefined") return;

		// The server's offset from UTC, in seconds. Absent means no zone was
		// sent, and a line is then rendered without a time rather than with a
		// guessed one.
		var offset = parseInt(root.getAttribute("data-log-zone"), 10);

		var pane = root.querySelector(".logs");
		if (!pane) return; // nothing rendered yet, the poll will fill it

		// Polling and streaming at once would double every line.
		root.removeAttribute("hx-trigger");

		var failures = 0;
		var source = new EventSource(url);

		source.addEventListener("message", function (e) {
			// Read before appending: appending is what moves it.
			var pinned = atBottom(pane);
			pane.appendChild(lineNode(e.data, clock(e.lastEventId, offset)));
			if (pinned) pane.scrollTop = pane.scrollHeight;
		});

		source.addEventListener("open", function () {
			failures = 0;
		});

		source.addEventListener("error", function () {
			// readyState stays CONNECTING while EventSource retries on its own.
			// Only a closed stream is one it has given up on.
			if (source.readyState !== EventSource.CLOSED) return;
			if (++failures < MAX_FAILURES) return;
			fallBackToPolling(root);
		});

		window.addEventListener("pagehide", function () {
			source.close();
		});
	}

	// atBottom reports whether the reader is following the tail.
	//
	// Someone scrolled up is reading something, and yanking them back to the
	// bottom on every arriving line makes that impossible.
	function atBottom(pane) {
		return pane.scrollHeight - pane.scrollTop - pane.clientHeight < 24;
	}

	// clock is the time an event carries, on the clock the server renders in.
	//
	// The id field is the line's timestamp in nanoseconds since the epoch — the
	// same instant the tail above this pane was rendered from. Converting it
	// with the browser's own zone is the obvious version and the wrong one:
	// somebody reading this from another country would get the streamed half of
	// one pane on a different clock from the half above it. Hence an offset
	// from the server rather than Date's local getters.
	//
	// An empty id is a line whose timestamp could not be parsed. It gets no
	// time, which is what the server-rendered lines do with the same case.
	function clock(id, offsetSeconds) {
		if (!id || isNaN(offsetSeconds)) return "";
		var ns = Number(id);
		if (!isFinite(ns)) return "";

		var at = new Date(ns / 1e6 + offsetSeconds * 1000);
		if (isNaN(at.getTime())) return "";

		// UTC getters against an already-shifted instant, so the arithmetic is
		// the server's zone and never the browser's.
		return pad(at.getUTCHours()) + ":" +
			pad(at.getUTCMinutes()) + ":" +
			pad(at.getUTCSeconds());
	}

	function pad(n) {
		return n < 10 ? "0" + n : String(n);
	}

	function lineNode(text, at) {
		var line = document.createElement("div");
		line.className = "log-line";
		// Same element and class the server renders, so a streamed line and the
		// tail above it line up in one column instead of two.
		if (at) {
			var time = document.createElement("span");
			time.className = "log-time";
			time.textContent = at;
			line.appendChild(time);
		}
		var body = document.createElement("span");
		body.className = "log-text";
		// textContent, never innerHTML: this is untrusted output from somebody
		// else's container, and it lands on an authenticated page.
		body.textContent = text;
		line.appendChild(body);
		return line;
	}

	function fallBackToPolling(root) {
		root.setAttribute("hx-trigger", "load, every 5s");
		if (window.htmx) window.htmx.process(root);
	}

	function start() {
		document.querySelectorAll("[data-log-stream]").forEach(init);
	}

	if (document.readyState === "loading") {
		document.addEventListener("DOMContentLoaded", start);
	} else {
		start();
	}
})();
