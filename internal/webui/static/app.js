"use strict";

// ---------------------------------------------------------------------------
// Ad-Sighting Tracker — mobile-first frontend (MapLibre GL + OSM raster tiles).
// No build step: this file is plain ES2017 served statically by the Go backend.
// ---------------------------------------------------------------------------

// OpenFreeMap "positron": a muted, monochrome vector basemap — roads and
// political boundaries included, free and requiring no API key. Attribution
// (OpenFreeMap / OpenMapTiles / OSM) rides along in the style's sources.
const BASEMAP_STYLE = "https://tiles.openfreemap.org/styles/positron";

// Map/UI state lives in the query string so any view + time + toggles +
// selected point is bookmarkable. parseURLState reads it once at startup;
// syncURL (below) writes it back as the user pans, filters, and selects.
function parseURLState() {
  const q = new URLSearchParams(location.search);
  const num = (k) => (q.has(k) ? Number(q.get(k)) : undefined);
  return {
    lat: num("lat"), lon: num("lon"), zoom: num("z"), days: num("days"),
    gone: q.has("gone") ? q.get("gone") === "1" : undefined,
    flagged: q.has("flagged") ? q.get("flagged") === "1" : undefined,
    sel: q.get("sel") || null, // "l<id>" (location) or "p<id>" (proposed)
  };
}
const initialURL = parseURLState();
const hasURLView = Number.isFinite(initialURL.lat) &&
  Number.isFinite(initialURL.lon) && Number.isFinite(initialURL.zoom);

const map = new maplibregl.Map({
  container: "map",
  style: BASEMAP_STYLE,
  // Restore the bookmarked view; otherwise default (later overridden by geolocation).
  center: hasURLView ? [initialURL.lon, initialURL.lat] : [-122.4194, 37.7749],
  zoom: hasURLView ? initialURL.zoom : 13,
});
map.addControl(new maplibregl.NavigationControl({ showCompass: false }), "top-left");
map.addControl(
  new maplibregl.GeolocateControl({ positionOptions: { enableHighAccuracy: true }, trackUserLocation: true }),
  "top-left"
);

// Positron draws roads in near-white greys; nudge the road surfaces a little
// darker so roadways stand out against the muted basemap. Applied to whatever
// transportation *road* line layers the style defines (skipping rail/paths/
// piers), so it keeps working if OpenFreeMap tweaks the style upstream.
const ROAD_DARKEN = 12; // HSL lightness points to subtract

function rgbToHsl(r, g, b, a) {
  r /= 255; g /= 255; b /= 255;
  const max = Math.max(r, g, b), min = Math.min(r, g, b), d = max - min;
  let h = 0;
  if (d) {
    if (max === r) h = ((g - b) / d) % 6;
    else if (max === g) h = (b - r) / d + 2;
    else h = (r - g) / d + 4;
    h = (h * 60 + 360) % 360;
  }
  const l = (max + min) / 2;
  const s = d ? d / (1 - Math.abs(2 * l - 1)) : 0;
  return { h, s: s * 100, l: l * 100, a };
}

// toHsl parses the CSS colour forms Positron uses (#hex, rgb[a](), hsl[a]()).
function toHsl(c) {
  let m;
  if ((m = /^#([0-9a-f]{3,8})$/i.exec(c))) {
    let hex = m[1];
    if (hex.length < 6) hex = hex.split("").map((x) => x + x).join("");
    const r = parseInt(hex.slice(0, 2), 16), g = parseInt(hex.slice(2, 4), 16), b = parseInt(hex.slice(4, 6), 16);
    const a = hex.length >= 8 ? parseInt(hex.slice(6, 8), 16) / 255 : 1;
    return rgbToHsl(r, g, b, a);
  }
  if ((m = /^rgba?\(([^)]+)\)$/i.exec(c))) {
    const p = m[1].split(",").map((x) => parseFloat(x));
    return rgbToHsl(p[0], p[1], p[2], p[3] === undefined ? 1 : p[3]);
  }
  if ((m = /^hsla?\(([^)]+)\)$/i.exec(c))) {
    const p = m[1].split(",");
    return { h: parseFloat(p[0]), s: parseFloat(p[1]), l: parseFloat(p[2]), a: p[3] === undefined ? 1 : parseFloat(p[3]) };
  }
  return null;
}

// darken lowers a colour's lightness by dL points, preserving alpha. It walks
// data-driven expressions (arrays) so interpolated colours are darkened too.
function darken(color, dL) {
  if (Array.isArray(color)) return color.map((x) => darken(x, dL));
  if (typeof color !== "string") return color;
  const hsl = toHsl(color);
  if (!hsl) return color; // not a colour literal (e.g. "interpolate", "zoom")
  const l = Math.max(0, hsl.l - dL);
  return `hsla(${Math.round(hsl.h)}, ${Math.round(hsl.s)}%, ${Math.round(l)}%, ${hsl.a})`;
}

function emphasizeRoads() {
  const style = map.getStyle();
  if (!style || !style.layers) return;
  for (const layer of style.layers) {
    if (layer.type !== "line" || layer["source-layer"] !== "transportation") continue;
    if (/rail|pier|path|ferry/.test(layer.id)) {
      continue; // leave non-roads as-is
    }
    const c = map.getPaintProperty(layer.id, "line-color");
    if (c !== undefined) {
      map.setPaintProperty(layer.id, "line-color", darken(c, ROAD_DARKEN));
    }
  }
}
map.on("load", emphasizeRoads);

// --- state -----------------------------------------------------------------
let markers = [];
let candidateMarkers = [];         // transient highlights for nearby-report choices
let adjustFor = null;              // location id currently being relocated via next tap
const locationCoords = {};         // id -> [lat,lon], so existence reports reuse coords
let currentSelection = null;       // URL selection token for the open point: "l<id>" | "p<id>"
let lastLocation = {};             // id -> latest location marker object (for reselect-from-URL)
let lastProposed = {};             // id -> latest proposed sighting object (for reselect-from-URL)

// Radius for the "any previous reportings near here?" check when proposing a
// new report (metres). Kept small so the lookup bounding box stays tiny.
const NEARBY_RADIUS_M = 50;

// Below this zoom the viewport covers too much ground for reported-gone points to be
// useful, so the ⭕ toggle hides and gones are left out of the /api/map query. The
// checkbox's own state is preserved (and still round-trips through ?gone=) so the
// user's preference returns when they zoom back in.
const MISSING_MIN_ZOOM = 11;
const MIN_ADD_ZOOM = 18.0;

function canAddAtCurrentZoom() {
  if (map.getZoom() >= MIN_ADD_ZOOM) {
    return true;
  }
  toast("Please zoom in and set a location more precisely.");
  return false;
}

// --- time-back slider (logarithmic 7d .. 730d) -----------------------------
const MIN_DAYS = 7, MAX_DAYS = 730;
const slider = document.getElementById("timeslider");
const timeLabel = document.getElementById("timelabel");
const insincereBox = document.getElementById("insincere");
const showMissingBox = document.getElementById("showmissing");
const showMissingLabel = document.getElementById("showmissinglabel");
const reportCountLabel = document.getElementById("reportcount");

// The effective missing/gone state: the checkbox, gated by zoom. Read this rather
// than showMissingBox.checked anywhere the map query is concerned.
function wantMissing() { return showMissingBox.checked && map.getZoom() >= MISSING_MIN_ZOOM; }

function syncMissingToggle() {
  showMissingLabel.classList.toggle("hidden", map.getZoom() < MISSING_MIN_ZOOM);
}

function sliderToDays(t) {
  // t in [0,1000] -> exponential interpolation between MIN_DAYS and MAX_DAYS,
  // so both short (days) and long (~2yr) ranges get usable slider travel.
  return Math.round(MIN_DAYS * Math.pow(MAX_DAYS / MIN_DAYS, t / 1000));
}
function daysToSlider(days) {
  // Inverse of sliderToDays, for restoring the slider from a bookmarked ?days=.
  const t = 1000 * Math.log(days / MIN_DAYS) / Math.log(MAX_DAYS / MIN_DAYS);
  return Math.max(0, Math.min(1000, Math.round(t)));
}
function fmtDays(d) {
  if (d >= 365) {
    const y = (d / 365).toFixed(d % 365 ? 1 : 0);
    return `${y} yr`;
  }
  if (d >= 60) {
    return `${Math.round(d / 30)} mo`;
  }
  return `${d} days`;
}
function currentDays() { return sliderToDays(Number(slider.value)); }

slider.addEventListener("input", () => { timeLabel.textContent = fmtDays(currentDays()); });
slider.addEventListener("change", () => { refreshMap(); syncURL(); });
insincereBox.addEventListener("change", () => { refreshMap(); syncURL(); });
showMissingBox.addEventListener("change", () => { refreshMap(); syncURL(); });

// --- URL sync --------------------------------------------------------------
// Write the current view + time + toggles + selection to the query string.
// replaceState (not push) so a pan/zoom/filter gesture never spams the Back
// button, while the address bar stays bookmarkable at every moment.
function syncURL() {
  const lngLat = map.getCenter();
  const q = new URLSearchParams();
  q.set("z", map.getZoom().toFixed(2));
  
  if (lngLat.lat === undefined || lngLat.lng === undefined) {
    lngLat.lat = 34.202804;
    lngLat.lng = -84.003242;
  }

  q.set("lat", lngLat.lat.toFixed(6));
  q.set("lon", lngLat.lng.toFixed(6));
  q.set("days", String(currentDays()));
  q.set("gone", showMissingBox.checked ? "1" : "0");
  q.set("flagged", insincereBox.checked ? "1" : "0");
  if (currentSelection) q.set("sel", currentSelection);
  history.replaceState(null, "", location.pathname + "?" + q.toString());
}

// Re-open the point named in ?sel= after the first data load. The restored
// view + time + toggles should have brought it back into the fetched set.
function applyInitialSelection() {
  const sel = initialURL.sel;
  if (!sel) return;
  const id = Number(sel.slice(1));
  if (!Number.isFinite(id)) return;
  if (sel[0] === "l" && lastLocation[id]) locationSheet(lastLocation[id]);
  else if (sel[0] === "p" && lastProposed[id]) proposedSheet(lastProposed[id]);
}

// --- tiny helpers ----------------------------------------------------------
async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  let data = null;
  try {
    data = await res.json();
  } catch (_) { }
  if (!res.ok) {
    throw new Error((data && data.error) || `${res.status} ${res.statusText}`);
  }
  return data;
}

function toast(msg) {
  const t = document.getElementById("toast");
  t.textContent = msg;
  t.classList.remove("hidden");
  clearTimeout(toast._t);
  toast._t = setTimeout(() => t.classList.add("hidden"), 4000);
}

const sheet = document.getElementById("sheet");
const sheetBody = document.getElementById("submitting");
function openSheet(html) { sheetBody.innerHTML = html; sheet.classList.remove("hidden"); }
function closeSheet() { sheet.classList.add("hidden"); clearCandidateMarkers(); currentSelection = null; syncURL(); }
function sheetOpen() { return !sheet.classList.contains("hidden"); }
document.querySelector(".sheet-close").addEventListener("click", closeSheet);

// Escape dismisses the open sheet, matching the close (×) button.
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && sheetOpen()) {
    closeSheet();
  }
});

function el(html) { const d = document.createElement("div"); d.innerHTML = html.trim(); return d.firstChild; }
function esc(s) { return String(s || "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c])); }

// Great-circle (haversine) distance in metres — used to refine the small
// bounding-box lookup down to an exact "within N metres" test.
function distMeters(lat1, lon1, lat2, lon2) {
  const R = 6371000, rad = Math.PI / 180;
  const dLat = (lat2 - lat1) * rad, dLon = (lon2 - lon1) * rad;
  const a = Math.sin(dLat / 2) ** 2 +
    Math.cos(lat1 * rad) * Math.cos(lat2 * rad) * Math.sin(dLon / 2) ** 2;
  return 2 * R * Math.asin(Math.sqrt(a));
}

// streetViewLink returns a small walking-human link that opens Google Street
// View at (lat,lon) in a shared window named "gsv" (reused on repeat clicks).
const GSV_ICON = "@ 📸🚗"
function streetViewLink(lat, lon) {
  const url = `https://www.google.com/maps/@?api=1&map_action=pano&viewpoint=${lat},${lon}`;
  return `<a class="gsv" href="${url}" target="gsv" rel="noopener"
    title="Open Street View here" aria-label="Open Street View here">${GSV_ICON}</a>`;
}

// --- markers ---------------------------------------------------------------
function clearMarkers() { markers.forEach((m) => m.remove()); markers = []; }

function addMarker(lon, lat, cls, onClick) {
  const node = el(`<div class="marker ${cls}"></div>`);
  node.addEventListener("click", (e) => { e.stopPropagation(); onClick(); });
  markers.push(new maplibregl.Marker({ element: node }).setLngLat([lon, lat]).addTo(map));
}

// Candidate markers highlight the nearby existing reports offered as choices
// when proposing a new report. They live in their own layer so refreshMap's
// clearMarkers() leaves them alone; closeSheet() disposes of them.
function clearCandidateMarkers() {
  candidateMarkers.forEach((m) => m.remove());
  candidateMarkers = [];
}
function showCandidateMarkers(near) {
  clearCandidateMarkers();
  near.forEach((n) => {
    const node = el(`<div class="marker candidate"></div>`);
    candidateMarkers.push(new maplibregl.Marker({ element: node }).setLngLat([n.lon, n.lat]).addTo(map));
  });
}

// Bounding box sent to the server: the current viewport expanded to 4x its
// width and height (2x margin on every side), so modest pans/zoom-outs stay
// within already-loaded data.
function viewportBBox() {
  const b = map.getBounds();
  const west = b.getWest(), east = b.getEast(), south = b.getSouth(), north = b.getNorth();
  const w = east - west, h = north - south;
  const cx = (west + east) / 2, cy = (south + north) / 2;
  return { minLon: cx - 2 * w, maxLon: cx + 2 * w, minLat: cy - 2 * h, maxLat: cy + 2 * h };
}

async function refreshMap() {
  const params = new URLSearchParams({ days: String(currentDays()) });
  if (insincereBox.checked) params.set("include_insincere", "1");
  if (wantMissing()) params.set("include_gones", "1");
  const bb = viewportBBox();
  params.set("min_lon", String(bb.minLon));
  params.set("max_lon", String(bb.maxLon));
  params.set("min_lat", String(bb.minLat));
  params.set("max_lat", String(bb.maxLat));
  let data;
  try {
    data = await api("GET", `/api/map?${params}`);
  } catch (e) {
    console.error("Couldn't fetch pins.", e);
    toast("Couldn't fetch pins. " + e.message);
    return;
  }

  clearMarkers();
  lastLocation = {};
  lastProposed = {};
  (data.markers || []).forEach((m) => {
    lastLocation[m.location_id] = m;
    locationCoords[m.location_id] = [m.lat, m.lon];
    addMarker(m.lon, m.lat, m.exists ? "exists" : "gone", () => locationSheet(m));
  });
  (data.proposed || []).forEach((p) => {
    lastProposed[p.id] = p;
    addMarker(p.lon, p.lat, "proposed", () => proposedSheet(p));
  });

  // Total reports in the window: in-window reports linked to each
  // location, plus the unreconciled proposals (one report each).
  const total = (data.markers || []).reduce((sum, m) => sum + (m.sighting_count || 0), 0)
    + (data.proposed || []).length;
  reportCountLabel.textContent = `${total}`;
}

// --- interactions ----------------------------------------------------------

// Tap empty map -> report a new sign (or place an adjustment).
map.on("click", (e) => {
  if (!canAddAtCurrentZoom()) return;
  if (adjustFor !== null) {
    relocate(e.lngLat);
    return;
  }
  newSignSheet(e.lngLat);
});

// Re-query when the view changes so the bounding box tracks the viewport.
// Debounced so a single pan/zoom gesture makes at most one request.
// The ⭕ toggle follows the zoom continuously (not just at moveend) so it appears
// and disappears as the gesture crosses MISSING_MIN_ZOOM.
map.on("zoom", syncMissingToggle);

let moveTimer = null;
map.on("moveend", () => {
  syncURL(); // URL tracks the view immediately; the data refetch is debounced
  clearTimeout(moveTimer);
  moveTimer = setTimeout(refreshMap, 300);
});

// Proposing a report: first check for existing reports within NEARBY_RADIUS_M so
// the user can confirm one of those instead of creating a duplicate location.
async function newSignSheet(lngLat) {
  if (!canAddAtCurrentZoom()) return;
  let near = [];
  try {
    near = await nearbyReports(lngLat);
  } catch (e) {
    console.error("Couldn't fetch nearby pins.", e);
    toast("Couldn't fetch nearby pins. " + e.message);
  } // fall through to a new report
  if (near.length === 0) {
    newSignForm(lngLat);
    return;
  }
  nearbyChoiceSheet(lngLat, near);
}

// nearbyReports queries a small bounding box around the tap, across all history
// and including reported-gone locations, then refines to an exact 50m radius.
// Returns location markers and proposed points, nearest first.
async function nearbyReports(lngLat) {
  const dLat = NEARBY_RADIUS_M / 111320;
  const dLon = NEARBY_RADIUS_M / (111320 * Math.cos(lngLat.lat * Math.PI / 180));
  const params = new URLSearchParams({
    days: "3650",          // clamped to the server max — effectively "all history"
    include_gones: "1",  // gone locations are still worth reporting against
    min_lat: String(lngLat.lat - dLat),
    max_lat: String(lngLat.lat + dLat),
    min_lon: String(lngLat.lng - dLon),
    max_lon: String(lngLat.lng + dLon),
  });
  const data = await api("GET", `/api/map?${params}`);
  const near = [];
  (data.markers || []).forEach((m) => {
    const d = distMeters(lngLat.lat, lngLat.lng, m.lat, m.lon);
    if (d <= NEARBY_RADIUS_M) near.push({ kind: "location", item: m, lat: m.lat, lon: m.lon, d });
  });
  (data.proposed || []).forEach((p) => {
    const d = distMeters(lngLat.lat, lngLat.lng, p.lat, p.lon);
    if (d <= NEARBY_RADIUS_M) near.push({ kind: "proposed", item: p, lat: p.lat, lon: p.lon, d });
  });
  return near.sort((a, b) => a.d - b.d);
}

// nearbyChoiceSheet highlights the nearby points on the map and lets the user
// confirm one (one tap) or continue to report a brand-new location.
function nearbyChoiceSheet(lngLat, near) {
  showCandidateMarkers(near);
  const rows = near.map((n, i) => {
    const label = esc(n.item.title);
    const status = n.kind === "location" ? (n.item.exists ? "on map" : "gone") : "proposed";
    return `<button class="btn" data-i="${i}">${label} · ${Math.round(n.d)} m away from yours · currently ${status}</button>`;
  }).join("");
  openSheet(`
    <h2>Existing reports nearby</h2>
    <p>${near.length} report(s) within ${NEARBY_RADIUS_M} m of where you tapped.
      Confirm one still exists, or add a new location.</p>
    ${rows}
    <button class="btn primary" id="newhere">No, actually where I clicked!</button>
  `);
  sheetBody.querySelectorAll("button[data-i]").forEach((btn) => {
    btn.onclick = () => reportExisting(near[Number(btn.dataset.i)]);
  });
  document.getElementById("newhere").onclick = () => { clearCandidateMarkers(); newSignForm(lngLat); };
}

// locationRef spells the "which location is this about?" field for POST bodies.
// The Go side renamed its structs to Location/LocationID but kept the old JSON
// tag (`location_id`) on /api/sightings and /api/reconciliations, so
// the wire name lives in exactly one place here, ready to flip when it changes.
function locationRef(id) { return { location_id: id }; }

// reportExisting files a one-tap "still exists" against a chosen nearby point:
// continued_existence on a location (reusing its coords), or a fresh
// new_existence at a proposed point's coords (which has no location to attach to).
async function reportExisting(near) {
  if (!canAddAtCurrentZoom()) return;
  clearCandidateMarkers();
  const body = near.kind === "location"
    ? { lat: near.lat, lon: near.lon, to_exist: true, ...locationRef(near.item.location_id) }
    : { lat: near.lat, lon: near.lon, to_exist: true };
  try {
    await api("POST", "/api/sightings", body);
    closeSheet();
    toast("Received! A moderator may need to review it.");
    refreshMap();
  } catch (err) {
    console.error("Couldn't send sighting.", err);
    toast(err.message);
  }
}

// newSignForm is the original "report a brand-new location here" form.
function newSignForm(lngLat) {
  openSheet(`
    <h2>Report new</h2>
    <p>${lngLat.lat.toFixed(5)}, ${lngLat.lng.toFixed(5)} &mdash; ${streetViewLink(lngLat.lat, lngLat.lng)}</p> 
    </p>
    <label class="horizontal">Medium
      <select id="medium">
        <option value="unknown">unknown</option>
        <option value="placard">placard</option>
        <option value="ink">ink</option>
        <option value="sticker">sticker</option>
      </select>
    </label>
    <label class="horizontal">Message
      <select id="message">
        <option value="unknown">unknown</option>
        <option value="js">js</option>
        <option value="jicr">jicr</option>
        <option value="bij">bij</option>
        <option value="other">other</option>
      </select>
    </label>
    <label class="horizontal">Approx height
      <select id="height">
        <option value="unknown">unknown</option>
        <option value="reachable">reachable</option>
        <option value="7ft">7ft</option>
        <option value="10ft">10ft</option>
        <option value="15+ft">15+ft</option>
      </select>
    </label>
    <button type="button" class="btn seen" id="submit">Report sighting</button>
    <button type="button" class="btn gone" id="removed">Report sighted and removed</button>
  `);
  const buildBody = (toExist) => ({
    lat: lngLat.lat,
    lon: lngLat.lng,
    to_exist: toExist,
    medium: document.getElementById("medium").value,
    message: document.getElementById("message").value,
    height: document.getElementById("height").value,
  });
  document.getElementById("submit").onclick = async () => {
    if (!canAddAtCurrentZoom()) return;
    try {
      await api("POST", "/api/sightings", buildBody(true));
      closeSheet();
      toast("Reported. Thanks!");
      refreshMap();
    } catch (err) {
      console.error("Error reporting sighting.", err);
      toast(err.message);
    }
  };
  document.getElementById("removed").onclick = async () => {
    if (!canAddAtCurrentZoom()) return;
    try {
      await api("POST", "/api/sightings", buildBody(false));
      closeSheet();
      toast("Received! Thanks! A moderator may need to review it.");
      refreshMap();
    } catch (err) {
      console.error("Error reporting sighting.", err);
      toast(err.message);
    }
  };
}

function locationSheet(loc) {
  const status = loc.exists ? "on the map" : "reported gone";
  openSheet(`
    <h2>${esc(loc.title)}</h2>
    <p>Currently ${status} · ${loc.sighting_count} report(s) in time window</p>
    <p><tt>${Number(loc.lat).toFixed(5)}, ${Number(loc.lon).toFixed(5)}</tt>&emsp;${streetViewLink(loc.lat, loc.lon)}</p>
    <button class="btn seen"   id="still">Still exists</button>
    <button class="btn gone"   id="gone">Was no longer there</button>
    <button class="btn gone"   id="nixed">Found and removed</button>
    <!-- button disabled=disabled class="btn"        id="adjust">Propose movement</button-->
    <button class="btn"        id="hist">View history</button>
  `);
  document.getElementById("still").onclick = () => reportOnExistingLocation(loc.location_id, "seen");
  document.getElementById("gone").onclick = () => reportOnExistingLocation(loc.location_id, "missed");
  document.getElementById("nixed").onclick = () => reportOnExistingLocation(loc.location_id, "removed");
  //document.getElementById("adjust").onclick = () => {
  //  if (!canAddAtCurrentZoom()) return;
  //  adjustFor = loc.location_id; closeSheet();
  //  toast("Tap the map at the sign's correct location");
  //};
  document.getElementById("hist").onclick = () => showHistory(loc.location_id);
  currentSelection = "l" + loc.location_id;
  syncURL();
}

function proposedSheet(p) {
  openSheet(`
    <h2>Proposed sign ${streetViewLink(p.lat, p.lon)}</h2>
    <p>${esc(p.location_description) || "(no description)"}</p>
    <p>Reported ${esc(p.observed_at)}</p>
    <button class="btn primary" id="promote">Confirm &amp; add to map</button>
    <p style="margin-top:10px">Confirming creates a location and links this
      report to it (needs canonicalize + reconcile).</p>
  `);
  document.getElementById("promote").onclick = async () => {
    try {
      const loc = await api("POST", "/api/locations", { lat: p.lat, lon: p.lon, description: p.description });
      await api("POST", "/api/reconciliations", { sighting_id: p.id, ...locationRef(loc.location_id) });
      closeSheet();
      toast("Received! Thanks! A moderator may need to review it.");
      refreshMap();
    } catch (err) {
      console.error("Error sending reconsiliations.", err);
      toast(err.message);
    }
  };
  currentSelection = "p" + p.id;
  syncURL();
}

// continued_existence / non_existence report against a location,
// reusing the location's own coordinates.
async function reportOnExistingLocation(locationId, transpired) {
  const coords = locationCoords[locationId];
  if (coords === undefined) {
    console.error("Could not look up cached location of loc", locationId);
    return;
  }

  try {
    console.debug("Reporting that loc", locationId, "at", coords, "is", transpired);
    await api("POST", "/api/sightings", {
      lat: coords[0], lon: coords[1],
      transpired: transpired,
      ...locationRef(locationId),
    });
    closeSheet();
    toast("Report submitted");
    refreshMap();
  } catch (err) {
    toast(err.message);
  }
}

// "Propose movement": append a continued_existence at the tapped point.
async function relocate(lngLat) {
  if (!canAddAtCurrentZoom()) return;
  const id = adjustFor;
  adjustFor = null;
  try {
    await api("POST", "/api/sightings", {
      lat: lngLat.lat, lon: lngLat.lng,
      to_exist: true,
      ...locationRef(id),

    });
    toast("Location adjustment reported");
    refreshMap();
  } catch (err) {
    toast(err.message);
  }
}

async function showHistory(id) {
  const params = new URLSearchParams({ days: String(currentDays()) });
  if (insincereBox.checked) {
    params.set("include_insincere", "1");
  }

  try {
    const h = await api("GET", `/api/locations/${id}/history?${params}`);
    const rows = (h.reconciled || []).map((s) =>
      `<li>${s.to_exist ? "seen" : "gone"} — ${esc(s.observed_at)} <span style="color:#888">(${esc(s.author_opaque_id).slice(0, 8)})</span><br>${s.description}</li>`
    ).join("") || "<p>No reports in window.</p>";
    const near = (h.nearby_unrec || []).length;
    openSheet(`
      <h2>${esc(h.location.message) || "<i>unlabeled sign</i>"} history</h2>
      <ul>${rows}</ul>
      <p style="margin-top:8px;color:#888">${near} unreconciled report(s) within 100m</p>
    `);
  } catch (err) {
    toast(err.message);
  }
}

// --- boot ------------------------------------------------------------------
async function boot() {
  // Restore time window + toggles from the URL before the first fetch, so the
  // initial /api/map query already reflects the bookmarked state.
  if (Number.isFinite(initialURL.days)) slider.value = String(daysToSlider(initialURL.days));
  if (initialURL.gone !== undefined) showMissingBox.checked = initialURL.gone;
  if (initialURL.flagged !== undefined) insincereBox.checked = initialURL.flagged;
  timeLabel.textContent = fmtDays(currentDays());
  syncMissingToggle(); // a bookmarked low-zoom view starts with the ⭕ toggle hidden

  map.on("load", async () => {
    await refreshMap();
    applyInitialSelection(); // re-open ?sel= once markers exist
    syncURL();               // normalize the address bar (fills in any defaults)
  });

  // Auto-geolocate only when the URL didn't already pin a view.
  if (!hasURLView && navigator.geolocation) {
    navigator.geolocation.getCurrentPosition(
      (pos) => map.jumpTo({ center: [pos.coords.longitude, pos.coords.latitude], zoom: 15 }),
      () => {}, { enableHighAccuracy: true, timeout: 5000 }
    );
  }
}
boot();
