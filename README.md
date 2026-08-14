# Ad-Sighting Tracker

A mobile-first web app for reporting real-world physical advertising signs
("ads"), confirming/denying they still exist, and reconciling scattered
sightings into trusted **locations**. Go REST backend, SQLite
datastore, MapLibre GL + OpenStreetMap frontend.

It has some hints of ambitious behavior scattered throughout the code, like
vestigial organs, but the core is working.

## Principles in the design

### Don't block things that are easy to revert

Gather data, don't destroy data. Operations should be append-only, and the
most recent state is what is shown. There will be vandals, but it is easy
for an administrator to mark their changes as dismissed from consideration.
Creating a new email address is harder for a vandal than clicking "insincere"
in the UI is for an admin.

### Identity lives elsewhere

No visible email addresses and nothing is stored,
except a computed hash of the user's identity. Owning data is a liability,
from leaks, court orders, privacy laws, and more.

### Be open

Freely available data and code. Easy to replicate. Test at home. Contribute.


## Quick start

### Run the server

The server creates the SQLite database and all tables on startup (idempotent
`CREATE TABLE IF NOT EXISTS`), seeds the default role→permission mapping, and
serves the embedded frontend. The binary is self-contained (schema + web assets
are embedded).

Configuration (environment variables)

| Var           | Default                              | Meaning |
|---------------|--------------------------------------|---------|
| `ADDR`        | `:8080`                              | Listen address |
| `DB_PATH`     | `sightingmap.db`                     | SQLite file path (WAL mode) |
| `AUTH_HEADERS`| `X-Forwarded-Email,X-Forwarded-User` | Ordered list of identity headers to trust |
| `DEV_SUBJECT` | *(empty)*                            | Fallback identity when no header present. **Dev only — leave empty in prod.** |


#### Plain local development

```bash
env DB_PATH="`pwd`/sightingmap.db" ADDR=":8080" DEV_SUBJECT="jane@example.com" go run ./cmd/server 

# open http://localhost:8080
```

#### Demonstration

Set up an oauth client. Names do not matter here. You'll need the client id and client secret.

https://console.cloud.google.com/auth/clients

```bash
MAP_PORT=8080  # arbitrary
OA2P_PORT=18080  # arbitrary

# Run locally WITHOUT oauth2_proxy (dev only — auth is bypassed):
oauth2-proxy --banner - --cookie-secret=secret_for_demo~ --cookie-refresh=1h --show-debug-on-error --provider=google --client-id="${oauth_client_id?}" --client-secret="${oauth_client_secret?}" --email-domain=\* --upstream=http://localhost:${MAP_PORT?}/ --http-address=localhost:${OA2P_PORT?}

# or, better,
oauth2-proxy --banner '<a href="/public-help">Pirate Map</a>' --custom-sign-in-logo=https://upload.wikimedia.org/wikipedia/commons/thumb/f/ff/Flag_of_Edward_England.svg/960px-Flag_of_Edward_England.svg.png --footer - --cookie-secret=secret_for_demo~ --cookie-refresh=1h --show-debug-on-error --provider=google --client-id="${oauth_client_id?}" --client-secret="${oauth_client_secret?}" --email-domain=\* --skip-auth-route='/public-help' --upstream=http://localhost:${MAP_PORT?}/ --http-address=localhost:${OA2P_PORT?}

env DB_PATH="`pwd`/sightingmap.db" ADDR=":$MAP_PORT" go run ./cmd/server 

# Ask tailscale to create TLS cert and connect port to a name on the public web.
tailscale funnel --https=443 localhost:$OA2P_PORT
# open the URL emitted 
```

### Configure roles of users

After a user has interacted with the web UI, they will have an entry in the table of 
opaque ids. You can usually figure out which is the user you are interested in
promoting by the last-access and registration times.

```bash
env DB_PATH="`pwd`/sightingmap.db" go run ./cmd/user-role list

env DB_PATH="`pwd`/sightingmap.db" go run ./cmd/user-role set 1234 editor
```

## Architecture

```
cmd/server             main: config, wiring, graceful shutdown
cmd/import-pirate-map  one-shot importer of the "Pirate Map" Google My Maps KML
cmd/user-role          list and update user roles
internal/db            opens SQLite, applies embedded schema.sql, seeds permissions
internal/store         all datastore access (append-only writes + derived-state queries)
internal/auth          oauth2_proxy header -> internal user (middleware)
internal/api           REST handlers + per-endpoint permission gating
internal/geo           haversine + bounding box (the 100m reconciliation radius)
internal/webui         embedded mobile-first frontend (MapLibre GL + OSM)
```

## Importing the "Pirate Map"

`cmd/import-pirate-map` reconciles placemarks from the public
[Pirate Map](https://www.google.com/maps/d/u/0/viewer?mid=1RzIEJyke2uzahFOJh1vXA2Lh6mXl5jA)
Google My Maps into the datastore. It fetches the map's KML export (handling both
raw KML and KMZ), classifies each placemark by the folder ("group") it lives in,
and appends sightings under a dedicated importer identity.

```bash
go run ./cmd/import-pirate-map                 # fetch the live map and import into $DB_PATH
go run ./cmd/import-pirate-map -dry-run        # report what would change, write nothing
go run ./cmd/import-pirate-map -file map.kml   # import from a local KML/KMZ file
```

| Flag | Default | Meaning |
|------|---------|---------|
| `-db` | `$DB_PATH` or `sightingmap.db` | SQLite database path |
| `-mid` | the Pirate Map id | Google My Maps `mid=` to fetch |
| `-url` | *(derived from `-mid`)* | Override the full KML URL |
| `-file` | *(none)* | Read KML/KMZ locally instead of fetching |
| `-source` | `pirate-map` | Namespace prefix for the natural keys it writes |
| `-importer` | `import:pirate-map` | Importer identity (an oauth2_proxy subject) |
| `-dry-run` | `false` | Parse and report without writing sightings |

**Group rules**

- `Billboards` and `Sightings of Questionable Legality` are **discarded**.
- Groups whose name contains **"Reported"** are reported to **exist**.
- Groups whose name contains **"Plunder…"** or **"[Rr]emoved"** are reported to
  **no longer exist**.

**Reconciliation & idempotency.** This KML export carries no stable placemark
id, so each record's natural key is a hash of its name + coordinates, stored on
the sighting in the `sightings.external_id` column (namespaced `pirate-map:…`).
On every run the importer:

- matches each placemark to what it wrote before by `external_id`, so a record
  keeps the same canonical location across runs;
- appends a corrective sighting **only when the reported state changed**
  (existence ⇄ non-existence) — re-running an unchanged map writes nothing;
- for a genuinely new record, **adopts** a pre-existing location within
  the 100 m reconciliation radius (that no import record already owns) instead of
  creating a duplicate, otherwise creates a new one.

Because it is append-only, unchanged records keep the `observed_at` of the run
that last changed them; they are not "refreshed" on every import (so a long-idle
record eventually falls outside a short time-back window, as intended).

## Authentication — oauth2_proxy

The Go backend implements **no login flow**. It expects
[`oauth2_proxy`](https://oauth2-proxy.github.io/oauth2-proxy/) in front of it to
authenticate the request and inject the identity via a trusted HTTP header. The
backend trusts that header and auto-registers a new user (role `reporter`) on
first sight, updating `last_seen_at` on every request (the heartbeat).

- The identity header name is **configurable** via `AUTH_HEADERS` and defaults
  to `X-Forwarded-Email`, then `X-Forwarded-User`.
- **⚠ CONFIRM before deploying:** the exact header name(s) depend on your
  oauth2_proxy configuration. With oauth2_proxy you typically need
  `--set-xauthrequest` (and/or `--pass-user-headers`) plus, when running behind
  nginx `auth_request`, mapping `X-Auth-Request-Email` → the upstream header.
  Set `AUTH_HEADERS` to match your deployment rather than assuming the default.
- **Security requirement:** oauth2_proxy (or your ingress) must **strip these
  headers from inbound client requests** so a client cannot spoof identity.
  The backend trusts the header unconditionally by design.

## Privacy — opaque user IDs

Real identities (the oauth2_proxy subject, e.g. an email) are **never stored** —
not in any table, not in any column. The only user identifier that exists in the
database is `users.opaque_id`, derived from the subject at resolve time:

```
opaque_id = base64url( HMAC-SHA256(key = seed, message = subject) )
```

The seed is a random 256-bit key generated on first startup and kept in the
`config` table under `user_id_hash_seed`. The derivation is deterministic, so a
returning user resolves to their existing row, and non-reversible without the
seed. See `internal/identity`.

**Never change the seed.** Rotating it re-derives every `opaque_id` and orphans
every existing user along with their sightings, flags and notes.

The `audit` endpoints and gossip notes expose only opaque IDs.

## Append-only model

`sightings`, `reconciliations`, `user_flags`, and `reputation_notes` are
**INSERT-only** — never updated or deleted. "Current state" is always derived at
query time from the most recent relevant rows:

- A bogus **non-existence** report is not deleted; a later **continued-existence**
  report counteracts it, and the map flips back to "exists" because the latest
  sighting wins.
- A location's history is the union of all sightings linked to it
  (directly or via a reconciliation row).
- Insincere flags are append-only too (`value=1` sets, `value=0` clears); the
  full flag history is preserved and the newest row per user decides current
  status.

The only mutable columns are `users.last_seen_at` (heartbeat) and `users.role`
(administrative state) — neither is domain history.

## Map / tile provider — usage & cost

The frontend uses **MapLibre GL JS** (open source, no per-request billing) with
the **[OpenFreeMap](https://openfreemap.org) "positron" vector style** — a
muted, monochrome basemap (roads + political boundaries) served from
`https://tiles.openfreemap.org` with **no API key**. On load the frontend nudges
the road line-colors slightly darker (`emphasizeRoads` in
`internal/webui/static/app.js`) so roadways stand out against the light basemap.

- **Production note:** OpenFreeMap is free and unmetered (donation-funded) and
  can be **self-hosted** for full control — see their docs. To change basemaps,
  point `BASEMAP_STYLE` in `internal/webui/static/app.js` at another MapLibre
  style URL (e.g. a self-hosted OpenMapTiles/tileserver-gl, Protomaps, or a
  keyed provider like MapTiler/Stadia). MapLibre itself stays free either way.

## REST API & permissions

Permissions live in the `role_permissions` table (data, not code) so they can be
changed without a deploy. Default mapping:

| Role          | Permissions |
|---------------|-------------|
| `muted`       | `view` |
| `reporter`    | `sight`, `view` |
| `editor`      | `sight`, `view`, `reconcile`, `canonicalize`, `gossip` |
| `administrator` | all of the above + `audit`, `flag_insincere` |

| Method & path | Permission | Notes |
|---|---|---|
| `GET /api/me` | *(any authenticated)* | Whoami + heartbeat |
| `POST /api/sightings` | `sight` | Report types 1–3; non/continued-existence require `location_id` |
| `POST /api/locations` | `canonicalize` | Create a location |
| `GET /api/locations/{id}/history` | `view` | Reconciled ∪ unreconciled-within-100m; `?days=` / `?since=` / `?include_insincere=` |
| `POST /api/reconciliations` | `reconcile` | Link a sighting → canonical location |
| `GET /api/map` | `view` | Canonical markers (derived status) + unreconciled proposals; `?days=` / `?include_insincere=` |
| `GET /api/users/{opaque}/sightings` | `audit` | All sightings by an opaque user id |
| `POST /api/users/{opaque}/insincere` | `flag_insincere` | Body `{"insincere":bool}` (default true) |
| `POST /api/users/{opaque}/notes` | `gossip` | Add a reputation note |
| `GET /api/users/{opaque}/notes` | `audit` | Read reputation notes (audit-scoped) |

## Frontend UX

- Full-screen MapLibre map, mobile-first, discreet dismissable bottom sheets.
- **Time-back slider** (bottom bar): logarithmic scale from **7 days to 2
  years**, default **90 days**, so both short and long windows are usable.
- **Tap empty map** → report a new sign (new-existence).
- **Tap a canonical marker** → "Still exists" / "No longer exists" / "Adjust
  location" / "View history". Markers are green (exists) or red (reported gone).
- **Tap a proposed (yellow, dashed) marker** → confirm & add to map (creates a
  canonical location and reconciles the proposal).
- **"show flagged"** checkbox includes insincere-flagged contributions.
- **Bookmarkable state**: the map view (center+zoom), time window, both
  checkboxes, and the selected point are mirrored into the URL query string as
  you pan/filter/select (via `replaceState`, so panning doesn't spam Back).
  Opening a URL with those params restores the exact same view, filters, and
  open point — so any state can be bookmarked or shared.

