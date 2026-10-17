// Status page: polls /api/v1/status and renders one card per port.
// Must run on the kiosk's Chromium 62: ES2016 only, and no AbortController
// (Chrome 66), so polling uses XMLHttpRequest.
(function () {
  "use strict";

  var portsEl = document.getElementById("ports");
  var tmpl = document.getElementById("port-template");
  var banner = document.getElementById("banner");
  var logo = document.getElementById("logo");
  var cards = [];
  var logoVersion = null;
  var failures = 0;

  function fmt(v, digits) {
    return (v === null || v === undefined) ? "–" : Number(v).toFixed(digits);
  }

  function duration(sec) {
    sec = Math.max(0, Math.floor(sec));
    var h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
    var mm = (m < 10 ? "0" : "") + m, ss = (s < 10 ? "0" : "") + s;
    return h > 0 ? h + ":" + mm + ":" + ss : m + ":" + ss;
  }

  // ago formats an elapsed time in seconds, e.g. "12 min ago".
  function ago(sec) {
    var m = Math.floor(sec / 60), h = Math.floor(m / 60), days = Math.floor(h / 24);
    if (m < 1) return "just now";
    if (h < 1) return m + " min ago";
    if (h < 48) return h + " h " + (m % 60) + " min ago";
    return days + " days ago";
  }

  // Display state: label, CSS class and a sub-line.
  function describe(p) {
    if (!p.online) return { label: "Offline", cls: "offline", sub: "No communication with charger" };
    if (p.state === "fault") return { label: "Fault", cls: "fault", sub: "Fault code " + p.fault_code };
    if (!p.enabled) return { label: "Disabled", cls: "disabled", sub: "Charging disabled" };
    if (p.paused) return { label: "Waiting", cls: "paused", sub: "Waiting for available power" };
    switch (p.state) {
      case "idle": return { label: "Available", cls: "idle", sub: "Plug in to charge" };
      case "connected": return { label: "Connected", cls: "connected", sub: "Vehicle connected, not charging" };
      case "charging": return { label: "Charging", cls: "charging", sub: "" };
    }
    return { label: "State " + p.state_code, cls: "unknown", sub: "" };
  }

  function metric(dl, name, value) {
    var d = document.createElement("div");
    var dt = document.createElement("dt");
    var dd = document.createElement("dd");
    dt.textContent = name;
    dd.textContent = value;
    d.appendChild(dt);
    d.appendChild(dd);
    dl.appendChild(d);
  }

  function ensureCards(n) {
    if (cards.length === n) return;
    portsEl.innerHTML = "";
    cards = [];
    for (var i = 0; i < n; i++) {
      var node = tmpl.content.firstElementChild.cloneNode(true);
      portsEl.appendChild(node);
      cards.push(node);
    }
    portsEl.setAttribute("data-count", String(n));
  }

  function renderPort(card, p, showLast) {
    var d = describe(p);
    card.className = "port s-" + d.cls;
    card.querySelector(".port-name").textContent = p.name;
    card.querySelector(".state").textContent = d.label;

    var m = p.meter && p.meter.fresh ? p.meter : null;
    var sess = p.session;
    var big = card.querySelector(".big-value");
    var unit = card.querySelector(".big-unit");
    var label = card.querySelector(".big-label");
    var last = sess || !showLast ? null : p.last_session;
    var sub = d.sub;
    label.textContent = "";
    card.querySelector(".big").className = "big";

    if (last) {
      // No session in progress: show the previous one.
      label.textContent = "Last session \u00b7 ended " + ago(last.seconds_since_end);
      card.querySelector(".big").className = "big prev";
      if (last.energy_kwh !== null && last.energy_kwh !== undefined) {
        big.textContent = fmt(last.energy_kwh, 2);
        unit.textContent = "kWh";
      } else {
        big.textContent = duration(last.charging_seconds);
        unit.textContent = "charging";
      }
    } else if (p.state === "charging" && m) {
      big.textContent = fmt(m.power_w / 1000, 1);
      unit.textContent = "kW";
    } else if (sess && sess.energy_kwh !== null && sess.energy_kwh !== undefined) {
      big.textContent = fmt(sess.energy_kwh, 2);
      unit.textContent = "kWh";
    } else if (p.state === "charging") {
      big.textContent = fmt(p.offered_amps, 0);
      unit.textContent = "A max";
    } else {
      big.textContent = "";
      unit.textContent = "";
    }
    if (p.state === "charging" && sess && sess.energy_kwh !== null && sess.energy_kwh !== undefined && m) {
      sub = fmt(sess.energy_kwh, 2) + " kWh delivered";
    }
    card.querySelector(".sub").textContent = sub;

    var dl = card.querySelector(".metrics");
    dl.innerHTML = "";
    if (m) {
      metric(dl, "Current", fmt(m.current, 1) + " A");
      metric(dl, "Voltage", fmt(m.voltage, 0) + " V");
    }
    // Show the pilot limit only while a vehicle could use it; otherwise the
    // port's rating (an idle port may be held off for lack of capacity).
    var vehicle = p.state === "charging" || p.state === "connected";
    if (p.online && p.charging_active && vehicle) {
      metric(dl, "Limit", fmt(p.offered_amps, 1) + " A" + (p.throttled ? " ↓" : ""));
    } else {
      metric(dl, "Max", fmt(p.max_amps, 0) + " A");
    }
    if (sess) {
      metric(dl, "Time", duration(sess.duration_seconds));
    }
  }

  function render(st) {
    document.getElementById("station-name").textContent = st.station_name;
    document.title = st.station_name;
    ensureCards(st.ports.length);
    var disp = st.display || {};
    var showLast = disp.show_last_session !== false;
    for (var i = 0; i < st.ports.length; i++) renderPort(cards[i], st.ports[i], showLast);

    var ls = st.load_sharing;
    var sharing = "";
    if (ls.enabled) {
      sharing = "Load sharing" + (ls.dynamic ? " (dynamic)" : "") + ": " +
        fmt(ls.allocated_amps, 1) + " / " + fmt(ls.system_max_amps, 0) + " A offered";
      if (ls.measured_amps !== null && ls.measured_amps !== undefined) {
        sharing += " · drawing " + fmt(ls.measured_amps, 1) + " A";
      }
    }
    document.getElementById("sharing").textContent = sharing;

    var meters = st.meters || [];
    var down = meters.filter(function (m) { return !m.online; }).map(function (m) { return m.name; });
    document.getElementById("meters").textContent = down.length ? "Meter offline: " + down.join(", ") : "";
    document.documentElement.className = "theme-" + (disp.theme || "dark");
    document.getElementById("clock").hidden = disp.show_clock === false;

    document.getElementById("version").textContent = (st.simulated ? "SIMULATION · " : "") + "v" + st.version;

    if (logoVersion !== null && st.logo_version !== logoVersion) {
      logo.src = "/logo?v=" + st.logo_version;
    }
    logoVersion = st.logo_version;
  }

  function tick() {
    var xhr = new XMLHttpRequest();
    var done = false;
    function finish(st) {
      if (done) return;
      done = true;
      if (st) {
        failures = 0;
        banner.hidden = true;
        try {
          render(st);
        } catch (e) {
          if (window.console) console.error(e);
        }
      } else if (++failures >= 3) {
        banner.textContent = "Controller not responding";
        banner.hidden = false;
      }
      setTimeout(tick, 1000);
    }
    xhr.open("GET", "/api/v1/status?t=" + Date.now());
    xhr.timeout = 4000;
    xhr.onload = function () {
      var st = null;
      if (xhr.status === 200) {
        try { st = JSON.parse(xhr.responseText); } catch (e) { st = null; }
      }
      finish(st);
    };
    xhr.onerror = xhr.ontimeout = xhr.onabort = function () { finish(null); };
    xhr.send();
  }

  function clock() {
    var d = new Date();
    var h = d.getHours(), m = d.getMinutes();
    document.getElementById("clock").textContent = (h < 10 ? "0" : "") + h + ":" + (m < 10 ? "0" : "") + m;
  }

  logo.addEventListener("error", function () { logo.hidden = true; });
  logo.addEventListener("load", function () { logo.hidden = false; });
  clock();
  setInterval(clock, 5000);
  tick();
})();
