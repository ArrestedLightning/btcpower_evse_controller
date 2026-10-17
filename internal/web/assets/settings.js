// Settings page. The browser supplies Basic auth credentials, which it
// already holds from loading /settings.
(function () {
  "use strict";

  var form = document.getElementById("form");
  var chargersEl = document.getElementById("chargers");
  var metersEl = document.getElementById("meters");
  var msgEl = document.getElementById("msg");
  var cfg = null;

  function show(text, ok) {
    msgEl.textContent = text;
    msgEl.className = "msg " + (ok ? "ok" : "err");
    msgEl.hidden = false;
    window.scrollTo(0, 0);
  }

  function api(method, path, body, raw) {
    var opts = { method: method, credentials: "same-origin", headers: {} };
    if (body !== undefined) {
      if (raw) {
        opts.body = body;
      } else {
        opts.body = JSON.stringify(body);
        opts.headers["Content-Type"] = "application/json";
      }
    }
    // Absolute URL without userinfo: fetch rejects relative URLs when the
    // page was opened as http://user:pass@host/settings.
    var url = location.protocol + "//" + location.host + path;
    return fetch(url, opts).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (data) {
        if (!r.ok) throw new Error(data.error || ("HTTP " + r.status));
        return data;
      });
    });
  }

  function el(tag, attrs, children) {
    var e = document.createElement(tag);
    Object.keys(attrs || {}).forEach(function (k) {
      if (k === "text") e.textContent = attrs[k];
      else e.setAttribute(k, attrs[k]);
    });
    (children || []).forEach(function (c) { e.appendChild(c); });
    return e;
  }

  function field(label, input) {
    return el("label", {}, [document.createTextNode(label), input]);
  }

  // ---- Chargers and meters ----

  function meterOptions(selected) {
    var sel = el("select", { "data-k": "meter" });
    sel.appendChild(el("option", { value: "", text: "None" }));
    var n = metersEl.children.length;
    for (var m = 1; m <= n; m++) {
      for (var c = 1; c <= 2; c++) {
        var v = m + ":" + c;
        var o = el("option", { value: v, text: "Meter " + m + " channel " + c });
        if (v === selected) o.selected = true;
        sel.appendChild(o);
      }
    }
    return sel;
  }

  function chargerRow(ch) {
    var meterSel = ch.meter ? ch.meter.meter + ":" + ch.meter.channel : "";
    var card = el("div", { "class": "sub-card charger" });
    var title = el("span", { text: "" });
    var remove = el("button", { type: "button", "class": "danger", text: "Remove" });
    remove.addEventListener("click", function () {
      if (chargersEl.children.length > 1) { card.remove(); renumber(); }
    });
    card.appendChild(el("div", { "class": "sub-card-head" }, [title, remove]));
    card.appendChild(el("div", { "class": "row" }, [
      field("Name", el("input", { type: "text", "data-k": "name", value: ch.name || "", maxlength: "40" })),
      field("Serial port", el("input", { type: "text", "data-k": "serial", value: ch.serial || "", placeholder: "/dev/ttyS1" })),
      field("Max current (A)", el("input", { type: "number", "data-k": "max_amps", value: ch.max_amps || 32, min: "12", max: "75", step: "1" })),
      field("Meter channel", meterOptions(meterSel))
    ]));
    return card;
  }

  function meterRow(m) {
    var card = el("div", { "class": "sub-card meter" });
    var title = el("span", { text: "" });
    var remove = el("button", { type: "button", "class": "danger", text: "Remove" });
    remove.addEventListener("click", function () { card.remove(); renumber(); });
    card.appendChild(el("div", { "class": "sub-card-head" }, [title, remove]));
    card.appendChild(el("div", { "class": "row" }, [
      field("Name", el("input", { type: "text", "data-k": "name", value: m.name || "", maxlength: "40" })),
      field("Serial port", el("input", { type: "text", "data-k": "serial", value: m.serial || "", placeholder: "/dev/ttyS2" }))
    ]));
    return card;
  }

  // Refresh titles, limits and meter channel choices after add/remove.
  function renumber() {
    Array.prototype.forEach.call(chargersEl.children, function (c, i) {
      c.querySelector(".sub-card-head span").textContent = "Charger " + (i + 1);
      var sel = c.querySelector('[data-k="meter"]');
      sel.parentNode.replaceChild(meterOptions(sel.value), sel);
    });
    Array.prototype.forEach.call(metersEl.children, function (c, i) {
      c.querySelector(".sub-card-head span").textContent = "Meter " + (i + 1);
    });
    document.getElementById("add-charger").disabled = chargersEl.children.length >= 4;
    document.getElementById("add-meter").disabled = metersEl.children.length >= 2;
  }

  function fill(c) {
    cfg = c;
    var f = form.elements;
    f.station_name.value = c.station_name;
    f.poll_interval_ms.value = c.poll_interval_ms;
    f.session_end_delay_sec.value = c.session_end_delay_sec;
    f.listen.value = c.listen;
    f.data_dir.value = c.data_dir;
    var ls = c.load_sharing;
    f.ls_enabled.checked = ls.enabled;
    f.ls_dynamic.checked = ls.dynamic;
    f.ls_system_max_amps.value = ls.system_max_amps;
    f.ls_min_amps.value = ls.min_amps;
    f.ls_throttle_margin_amps.value = ls.throttle_margin_amps;
    f.ls_throttle_headroom_amps.value = ls.throttle_headroom_amps;
    f.ls_throttle_delay_sec.value = ls.throttle_delay_sec;
    f.ls_probe_interval_sec.value = ls.probe_interval_sec;
    f.display_theme.value = c.display.theme;
    f.display_show_clock.checked = c.display.show_clock !== false;
    f.display_show_last_session.checked = c.display.show_last_session !== false;
    document.documentElement.className = "theme-" + c.display.theme;
    f.admin_password.value = "";
    metersEl.innerHTML = "";
    (c.meters || []).forEach(function (m) { metersEl.appendChild(meterRow(m)); });
    chargersEl.innerHTML = "";
    (c.chargers || []).forEach(function (ch) { chargersEl.appendChild(chargerRow(ch)); });
    renumber();
  }

  function collect() {
    var f = form.elements;
    var num = function (v) { return Number(v); };
    var out = {
      station_name: f.station_name.value.trim(),
      listen: f.listen.value.trim(),
      data_dir: f.data_dir.value.trim(),
      poll_interval_ms: num(f.poll_interval_ms.value),
      session_end_delay_sec: num(f.session_end_delay_sec.value),
      admin_password: f.admin_password.value,
      display: {
        theme: f.display_theme.value,
        show_clock: f.display_show_clock.checked,
        show_last_session: f.display_show_last_session.checked
      },
      chargers: [],
      meters: [],
      load_sharing: {
        enabled: f.ls_enabled.checked,
        dynamic: f.ls_dynamic.checked,
        system_max_amps: num(f.ls_system_max_amps.value),
        min_amps: num(f.ls_min_amps.value),
        throttle_margin_amps: num(f.ls_throttle_margin_amps.value),
        throttle_headroom_amps: num(f.ls_throttle_headroom_amps.value),
        throttle_delay_sec: num(f.ls_throttle_delay_sec.value),
        probe_interval_sec: num(f.ls_probe_interval_sec.value)
      }
    };
    Array.prototype.forEach.call(chargersEl.children, function (c) {
      var get = function (k) { return c.querySelector('[data-k="' + k + '"]').value; };
      var ch = { name: get("name").trim(), serial: get("serial").trim(), max_amps: num(get("max_amps")) };
      var m = get("meter");
      if (m) ch.meter = { meter: num(m.split(":")[0]), channel: num(m.split(":")[1]) };
      out.chargers.push(ch);
    });
    Array.prototype.forEach.call(metersEl.children, function (c) {
      var get = function (k) { return c.querySelector('[data-k="' + k + '"]').value; };
      out.meters.push({ name: get("name").trim(), serial: get("serial").trim() });
    });
    return out;
  }

  document.getElementById("add-charger").addEventListener("click", function () {
    chargersEl.appendChild(chargerRow({ max_amps: 32 }));
    renumber();
  });
  document.getElementById("add-meter").addEventListener("click", function () {
    metersEl.appendChild(meterRow({}));
    renumber();
  });

  form.addEventListener("submit", function (ev) {
    ev.preventDefault();
    api("PUT", "/api/v1/config", collect())
      .then(function (c) { fill(c); show("Settings saved.", true); loadPorts(); })
      .catch(function (e) { show("Not saved:\n" + e.message, false); });
  });

  // ---- Port enable/disable ----

  function loadPorts() {
    api("GET", "/api/v1/status").then(function (st) {
      var box = document.getElementById("port-control");
      box.innerHTML = "";
      st.ports.forEach(function (p) {
        var btn = el("button", { type: "button", text: p.enabled ? "Disable" : "Enable", "class": p.enabled ? "danger" : "primary" });
        btn.addEventListener("click", function () {
          api("POST", "/api/v1/ports/" + p.id + "/" + (p.enabled ? "disable" : "enable"))
            .then(loadPorts)
            .catch(function (e) { show(e.message, false); });
        });
        box.appendChild(el("div", { "class": "row" }, [
          el("span", { text: p.name + " — " + (p.enabled ? "enabled" : "disabled") + " (" + p.state + ")", style: "flex:1" }),
          btn
        ]));
      });
    });
  }

  // ---- Logo ----

  function refreshLogo() {
    document.getElementById("logo-preview").src = "/logo?t=" + Date.now();
  }
  document.getElementById("logo-file").addEventListener("change", function (ev) {
    var file = ev.target.files[0];
    if (!file) return;
    api("PUT", "/api/v1/logo", file, true)
      .then(function () { refreshLogo(); show("Logo updated.", true); })
      .catch(function (e) { show("Logo not saved: " + e.message, false); })
      .then(function () { ev.target.value = ""; });
  });
  document.getElementById("logo-reset").addEventListener("click", function () {
    api("DELETE", "/api/v1/logo")
      .then(function () { refreshLogo(); show("Default logo restored.", true); })
      .catch(function (e) { show(e.message, false); });
  });

  // ---- Sessions ----

  function loadSessions() {
    api("GET", "/api/v1/sessions?limit=25").then(function (list) {
      var tb = document.getElementById("sessions");
      tb.innerHTML = "";
      if (!list.length) {
        tb.appendChild(el("tr", {}, [el("td", { colspan: "6", "class": "muted", text: "No sessions yet" })]));
        return;
      }
      list.forEach(function (s) {
        var start = new Date(s.plugged_in);
        var end = s.plugged_out ? new Date(s.plugged_out) : null;
        var mins = function (sec) { return Math.round(sec / 60) + " min"; };
        tb.appendChild(el("tr", {}, [
          el("td", { text: s.port_name }),
          el("td", { text: start.toLocaleString() }),
          el("td", { text: end ? mins((end - start) / 1000) : "" }),
          el("td", { text: mins(s.charging_seconds) }),
          el("td", { text: s.energy_kwh === null ? "–" : s.energy_kwh.toFixed(2) + " kWh" }),
          el("td", { text: s.peak_amps === null ? "–" : s.peak_amps.toFixed(1) + " A" })
        ]));
      });
    });
  }

  api("GET", "/api/v1/config").then(fill).catch(function (e) { show(e.message, false); });
  loadPorts();
  loadSessions();
})();
