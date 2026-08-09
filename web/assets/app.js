// Dashboard behaviour (UI-002). Plain ES modules-free browser JavaScript,
// served exactly as it sits in the tree (see ../embed.go): no framework, no
// bundler, and nothing fetched from a CDN at runtime.
//
// This file holds no logic of its own (CLAUDE.md "one core, thin skins").
// Every view is one call to internal/api on the same loopback origin the
// page was served from, rendering what core returned: status and
// replication lag from GET /status, backup history from GET /snapshots, and
// drill and promotion from their job verbs, whose progress arrives on the
// one CORE-002 event stream the CLI also renders. Nothing here decides
// anything the CLI would decide differently — where the browser cannot
// prompt, it sends the operator's answer as a request field and lets core
// refuse (tech-spec.md "API").
(function () {
  "use strict";

  // SETTINGS_KEY namespaces the connection form in localStorage. The form
  // holds paths and hostnames only; key material never reaches the browser
  // (KEY-003), and the API reads SSH keys from the operator's own machine.
  var SETTINGS_KEY = "farrier.connection";

  var $ = function (sel, root) {
    return (root || document).querySelector(sel);
  };
  var $$ = function (sel, root) {
    return Array.prototype.slice.call((root || document).querySelectorAll(sel));
  };

  var connectionForm = $("#connection-form");
  var drillForm = $('[data-form="drill"]');
  var promoteForm = $('[data-form="promote"]');

  // ---------------------------------------------------------------- forms

  function formValues(form) {
    var out = {};
    $$("input", form).forEach(function (input) {
      if (input.type === "checkbox") return;
      out[input.name] = input.value.trim();
    });
    return out;
  }

  function connection() {
    return formValues(connectionForm);
  }

  function loadSettings() {
    var saved;
    try {
      saved = JSON.parse(window.localStorage.getItem(SETTINGS_KEY) || "{}");
    } catch (err) {
      saved = {};
    }
    $$("input", connectionForm).forEach(function (input) {
      if (typeof saved[input.name] === "string") input.value = saved[input.name];
    });
  }

  function saveSettings() {
    try {
      window.localStorage.setItem(SETTINGS_KEY, JSON.stringify(connection()));
    } catch (err) {
      note("connection-note", "Could not save these in this browser: " + err.message);
    }
  }

  function require(values, names, what) {
    var missing = names.filter(function (name) {
      return !values[name];
    });
    if (missing.length === 0) return null;
    return what + " needs " + missing.join(", ") + " filled in above.";
  }

  // -------------------------------------------------------------- requests

  function query(params) {
    var q = new URLSearchParams();
    Object.keys(params).forEach(function (key) {
      if (params[key]) q.set(key, params[key]);
    });
    return q.toString();
  }

  // failure turns an API error response into an Error carrying the message
  // core wrote, so a view reports the actual defect — "confirm is required:
  // snapshot … is 3h old" — rather than a status code.
  function failure(response, body) {
    var detail = body && body.error ? body.error : "HTTP " + response.status;
    return new Error(detail);
  }

  function get(path, params) {
    var url = path + (params ? "?" + query(params) : "");
    return fetch(url, { headers: { Accept: "application/json" } }).then(function (response) {
      return response.json().catch(function () {
        return null;
      }).then(function (body) {
        if (!response.ok) throw failure(response, body);
        return body;
      });
    });
  }

  function post(path, body) {
    return fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json" },
      body: JSON.stringify(body),
    }).then(function (response) {
      return response.json().catch(function () {
        return null;
      }).then(function (parsed) {
        if (!response.ok) throw failure(response, parsed);
        return parsed;
      });
    });
  }

  // ------------------------------------------------------------ rendering

  function show(el, visible) {
    if (el) el.hidden = !visible;
  }

  function panel(name, visible) {
    show($('[data-panel="' + name + '"]'), visible);
  }

  function problem(name, message) {
    var el = $('[data-problem="' + name + '"]');
    if (!el) return;
    el.textContent = message || "";
    show(el, Boolean(message));
  }

  function note(id, message) {
    var el = document.getElementById(id);
    if (el) el.textContent = message;
  }

  function slot(name) {
    return $('[data-slot="' + name + '"]');
  }

  function clear(el) {
    while (el.firstChild) el.removeChild(el.firstChild);
    return el;
  }

  function cell(tag, text, className) {
    var el = document.createElement(tag);
    el.textContent = text;
    if (className) el.className = className;
    return el;
  }

  function definition(list, term, value, className) {
    list.appendChild(cell("dt", term));
    list.appendChild(cell("dd", value, className));
  }

  function busy(button, running) {
    if (!button) return;
    button.disabled = running;
    button.classList.toggle("busy", running);
  }

  // formatBytes renders a byte count for reading. Presentation only: the
  // API carries exact counts, exactly as the CLI's own formatter does.
  function formatBytes(n) {
    if (typeof n !== "number" || n < 1000) return (n || 0) + " B";
    var units = ["kB", "MB", "GB", "TB", "PB", "EB"];
    var value = n;
    var i = -1;
    while (value >= 1000 && i < units.length - 1) {
      value /= 1000;
      i++;
    }
    return value.toFixed(1) + " " + units[i];
  }

  function formatTime(iso) {
    if (!iso) return "unknown";
    var when = new Date(iso);
    return isNaN(when.getTime()) ? iso : when.toLocaleString();
  }

  // --------------------------------------------------------- event stream

  // streamJob renders one job's CORE-002 events into list, resolving when
  // the job reaches its terminal event. The stream is closed on that event
  // rather than left to end on its own: the server closes a finished job's
  // SSE response, and an EventSource that sees a closed response reconnects
  // and replays the whole job.
  function streamJob(jobId, list, outcome) {
    clear(list);
    outcome.textContent = "Running…";
    outcome.className = "outcome running";

    return new Promise(function (resolve) {
      var source = new EventSource("/jobs/" + encodeURIComponent(jobId) + "/events");

      var finish = function (state, detail) {
        source.close();
        outcome.textContent = detail;
        outcome.className = "outcome " + state;
        resolve(state);
      };

      // The server has no Last-Event-ID handling: every subscription
      // replays the job from its first event. So a reconnect delivers the
      // whole stream again, and the rendered list is rebuilt from scratch
      // rather than appended to twice.
      source.onopen = function () {
        clear(list);
      };

      source.onmessage = function (message) {
        var event;
        try {
          event = JSON.parse(message.data);
        } catch (err) {
          return;
        }
        var item = document.createElement("li");
        item.className = "event " + event.state;
        item.appendChild(cell("span", event.step || "job", "event-step"));
        item.appendChild(cell("span", event.detail || "", "event-detail"));
        list.appendChild(item);

        var terminal = event.state === "succeeded" || event.state === "failed";
        if (terminal && !event.step) finish(event.state, event.detail);
      };

      source.onerror = function () {
        if (source.readyState === EventSource.CLOSED) {
          finish("failed", "The progress stream closed before the job reported a result.");
        }
      };
    });
  }

  // ----------------------------------------------------------- status view

  function renderStatus(report) {
    var services = clear(slot("services"));
    report.services.forEach(function (service) {
      var row = document.createElement("tr");
      row.appendChild(cell("th", service.name));
      row.appendChild(cell("td", service.up ? "up" : "down", service.up ? "ok" : "bad"));
      row.appendChild(cell("td", service.detail || ""));
      services.appendChild(row);
    });

    var tls = clear(slot("tls"));
    var tlsState = !report.tls.valid
      ? "invalid or expired"
      : report.tls.expiringSoon
        ? "valid, expiring soon"
        : "valid";
    definition(tls, "Certificate", tlsState, report.tls.valid && !report.tls.expiringSoon ? "ok" : "bad");
    definition(tls, "Expires", formatTime(report.tls.notAfter));

    var disk = clear(slot("disk"));
    definition(disk, "Path", report.disk.path);
    definition(
      disk,
      "Available",
      formatBytes(report.disk.availableBytes) + " of " + formatBytes(report.disk.totalBytes)
    );

    panel("status", true);
  }

  function renderLag(lag) {
    var list = clear(slot("lag"));
    if (lag.state === "measured") {
      definition(list, "Last backup", lag.age + " ago", "ok");
      definition(list, "Captured", formatTime(lag.lastBackup));
      if (lag.skew && lag.skew !== "0s") {
        definition(list, "Clock skew", "the destination reads " + lag.skew + " ahead of the host");
      }
    } else if (lag.state === "no-backups") {
      definition(list, "Last backup", "no backups at this destination yet", "bad");
    } else {
      definition(
        list,
        "Last backup",
        "unmeasured — no golden-path destination configured, or an operator-assembled transport"
      );
    }
    show(slot("lag-hint"), false);
    panel("lag", true);
  }

  function refreshStatus(button) {
    var values = connection();
    var missing = require(values, ["bundleDir", "target"], "Status");
    if (missing) {
      problem("status", missing);
      return Promise.resolve();
    }

    problem("status", "");
    busy(button, true);
    return get("/status", {
      bundleDir: values.bundleDir,
      target: values.target,
      remoteDir: values.remoteDir,
      diskPath: values.diskPath,
      to: values.to,
      sshKeyFile: values.sshKeyFile,
      knownHostsFile: values.knownHostsFile,
      sshTimeout: values.sshTimeout,
    })
      .then(function (report) {
        renderStatus(report);
        renderLag(report.lag);
      })
      .catch(function (err) {
        problem("status", err.message);
        panel("status", false);
        panel("lag", false);
      })
      .then(function () {
        busy(button, false);
      });
  }

  // ---------------------------------------------------------- history view

  function renderHistory(snapshots) {
    var rows = clear(slot("history"));
    snapshots.forEach(function (snapshot) {
      var row = document.createElement("tr");
      row.appendChild(cell("th", snapshot.key));
      row.appendChild(cell("td", formatBytes(snapshot.sizeBytes)));
      row.appendChild(cell("td", formatTime(snapshot.modified)));
      row.appendChild(cell("td", snapshot.age + " ago"));
      rows.appendChild(row);
    });
    show(slot("history-empty"), snapshots.length === 0);
    panel("history", true);
  }

  function refreshHistory(button) {
    var values = connection();
    var missing = require(values, ["to"], "Backup history");
    if (missing) {
      problem("history", missing);
      return Promise.resolve();
    }

    problem("history", "");
    busy(button, true);
    return get("/snapshots", { to: values.to })
      .then(function (body) {
        renderHistory(body.snapshots || []);
      })
      .catch(function (err) {
        problem("history", err.message);
        panel("history", false);
      })
      .then(function () {
        busy(button, false);
      });
  }

  // ------------------------------------------------------------ drill view

  function runDrill(button) {
    var values = connection();
    var drill = formValues(drillForm);
    var missing =
      require(values, ["bundleDir", "to"], "A drill") ||
      (drill.target ? null : "A drill needs a scratch target.");
    if (missing) {
      problem("drill", missing);
      return Promise.resolve();
    }

    problem("drill", "");
    busy(button, true);
    panel("drill", true);
    return post("/drill", {
      bundleDir: values.bundleDir,
      target: drill.target,
      from: values.to,
      remoteDir: drill.remoteDir || undefined,
      sshKeyFile: values.sshKeyFile || undefined,
      knownHostsFile: values.knownHostsFile || undefined,
      sshTimeout: values.sshTimeout || undefined,
    })
      .then(function (accepted) {
        return streamJob(accepted.jobId, slot("drill-events"), slot("drill-outcome"));
      })
      .catch(function (err) {
        problem("drill", err.message);
        panel("drill", false);
      })
      .then(function () {
        busy(button, false);
      });
  }

  // -------------------------------------------------------- promotion view

  // reviewed is the snapshot the operator has confirmed, so the promote
  // request can only ever act on the one whose age was displayed (FAIL-002).
  // Editing any promotion or connection field clears it: a confirmation is
  // for one snapshot at one destination, not a standing permission.
  var reviewed = null;

  function clearReview() {
    reviewed = null;
    var confirm = $('[data-field="promote-confirm"]');
    if (confirm) confirm.checked = false;
    $('[data-action="run-promote"]').disabled = true;
    panel("promote-review", false);
  }

  function reviewPromote(button) {
    var values = connection();
    var promote = formValues(promoteForm);
    var missing =
      require(values, ["bundleDir", "to"], "A promotion") ||
      (promote.target ? null : "A promotion needs a standby target.");
    if (missing) {
      problem("promote", missing);
      return Promise.resolve();
    }

    problem("promote", "");
    clearReview();
    busy(button, true);
    return get("/snapshots", { to: values.to })
      .then(function (body) {
        var snapshots = body.snapshots || [];
        if (snapshots.length === 0) {
          throw new Error("There are no snapshots at " + values.to + " to promote.");
        }
        // Blank means "the most recent snapshot", which the API resolves
        // the same way for the real request; the list arrives newest-first
        // from core, so this shows that same snapshot rather than picking
        // one here.
        var chosen = snapshots[0];
        if (promote.snapshot) {
          chosen = snapshots.filter(function (s) {
            return s.key === promote.snapshot;
          })[0];
          if (!chosen) {
            throw new Error("No snapshot named " + promote.snapshot + " at " + values.to + ".");
          }
        }

        reviewed = chosen;
        slot("promote-review").textContent =
          "Promoting snapshot " +
          chosen.key +
          ", captured " +
          formatTime(chosen.modified) +
          " — " +
          chosen.age +
          " ago. Everything written to the forge since then is not in it.";
        slot("promote-confirm-label").textContent =
          "Yes — promote " + chosen.key + " to " + promote.target + " and flip DNS to it.";
        panel("promote-review", true);
      })
      .catch(function (err) {
        problem("promote", err.message);
        clearReview();
      })
      .then(function () {
        busy(button, false);
      });
  }

  function runPromote(button) {
    if (!reviewed) {
      problem("promote", "Review the snapshot and confirm it before promoting.");
      return Promise.resolve();
    }

    var values = connection();
    var promote = formValues(promoteForm);
    problem("promote", "");
    busy(button, true);
    panel("promote", true);
    return post("/promote", {
      bundleDir: values.bundleDir,
      target: promote.target,
      from: values.to,
      snapshot: reviewed.key,
      remoteDir: promote.remoteDir || undefined,
      dnsRecord: promote.dnsRecord || undefined,
      dnsValue: promote.dnsValue || undefined,
      sshKeyFile: values.sshKeyFile || undefined,
      knownHostsFile: values.knownHostsFile || undefined,
      sshTimeout: values.sshTimeout || undefined,
      confirm: true,
    })
      .then(function (accepted) {
        clearReview();
        return streamJob(accepted.jobId, slot("promote-events"), slot("promote-outcome"));
      })
      .catch(function (err) {
        problem("promote", err.message);
        panel("promote", false);
      })
      .then(function () {
        busy(button, false);
      });
  }

  // ------------------------------------------------------------ wiring up

  var actions = {
    "refresh-status": refreshStatus,
    "refresh-history": refreshHistory,
    "run-drill": runDrill,
    "review-promote": reviewPromote,
    "run-promote": runPromote,
  };

  document.addEventListener("click", function (event) {
    var button = event.target.closest ? event.target.closest("[data-action]") : null;
    if (!button) return;
    var action = actions[button.getAttribute("data-action")];
    if (action) action(button);
  });

  connectionForm.addEventListener("input", function () {
    saveSettings();
    clearReview();
  });
  promoteForm.addEventListener("input", clearReview);

  var confirmBox = $('[data-field="promote-confirm"]');
  confirmBox.addEventListener("change", function () {
    $('[data-action="run-promote"]').disabled = !(confirmBox.checked && reviewed);
  });

  $$("form").forEach(function (form) {
    form.addEventListener("submit", function (event) {
      event.preventDefault();
    });
  });

  loadSettings();
})();
