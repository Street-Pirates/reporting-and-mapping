-- Ad-Sighting Tracker schema (SQLite, STRICT tables; requires SQLite >= 3.37).
--
-- ============================================================================
-- APPEND-ONLY DESIGN
-- ============================================================================
-- sightings, reconciliations, user_flags and reputation_notes are INSERT-only.
-- Nothing in them is ever UPDATEd or DELETEd. "Current state" (does a sign
-- still exist? is a user insincere?) is ALWAYS derived at query time from the
-- most recent relevant row(s). A vandalized/false sighting is corrected by
-- inserting a NEW corrective sighting, never by mutating history.
--
-- The `users` table is the deliberate exception: `last_seen_at` is an
-- operational heartbeat and `role` is mutable administrative state. Neither is
-- domain history, so both may be UPDATEd.
--
-- ============================================================================
-- PRIVACY BOUNDARY (opaque user IDs)
-- ============================================================================
-- The real oauth2_proxy identity (email / SSO sub) is NEVER stored, anywhere.
--
-- users.opaque_id -> the ONLY user identifier in this database, and the only
--                    one ever emitted by the API. It is derived from the
--                    request's identity at resolve time:
--
--                        base64url( HMAC-SHA256(key = seed, message = subject) )
--
--                    where seed is config.user_id_hash_seed (generated on first
--                    startup). Deterministic, so a returning user lands on the
--                    same row; non-reversible without the seed. See
--                    internal/identity.
--
-- The seed must NEVER be changed: rotating it re-derives every opaque_id and
-- orphans every existing user, their sightings and their flags.
-- ============================================================================

PRAGMA foreign_keys = ON;

-- ---------------------------------------------------------------------------
-- Configuration (key-values). Written at startup; operator-editable.
-- The only key today is 'user_id_hash_seed', the HMAC key behind
-- users.opaque_id -- see the PRIVACY BOUNDARY note above before touching it.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS config (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at TEXT NOT NULL                             -- UTC RFC-3339
) STRICT;

-- ---------------------------------------------------------------------------
-- Users & identity
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY,
  opaque_id     TEXT    NOT NULL UNIQUE,               -- derived, non-reversible
  role          TEXT    NOT NULL DEFAULT 'reporter',   -- reporter|editor|administrator|muted
  registered_at TEXT    NOT NULL,                      -- UTC RFC-3339
  last_seen_at  TEXT    NOT NULL,                      -- UTC RFC-3339 (heartbeat)
  called        TEXT    NOT NULL DEFAULT ''            -- user-defined note from POST /api/me?called=
) STRICT;

-- ---------------------------------------------------------------------------
-- Role -> permission mapping (data, not code, so it can change without deploy)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS role_permissions (
  role       TEXT NOT NULL,
  permission TEXT NOT NULL,
  PRIMARY KEY (role, permission)
) STRICT;

CREATE TABLE IF NOT EXISTS locations (
  id           INTEGER PRIMARY KEY,
  lat          REAL    NOT NULL,
  lon          REAL    NOT NULL,
  created_by   INTEGER NOT NULL REFERENCES users(id),
  created_at   TEXT    NOT NULL,
  validated_by INTEGER NULL REFERENCES users(id),
  CHEcK (lat BETWEEN -90 AND 90),
  CHECK (lon BETWEEN -180 AND 180)
) STRICT;

-- Bounding-box prefilter for spatial reconciliation (mirrors idx_sightings_bbox);
-- used when an import matches a record to a nearby existing location.
CREATE INDEX IF NOT EXISTS idx_location_bbox ON locations(lat, lon);

-- ---------------------------------------------------------------------------
-- Sightings (raw, append-only observations)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sightings (
  id           INTEGER PRIMARY KEY,
  reporter_id  INTEGER NOT NULL REFERENCES users(id),
  location_id  INTEGER NOT NULL REFERENCES locations(id),
  observed_at  TEXT    NOT NULL DEFAULT '2010-01-01T15:00:00.000Z',
  transpired   TEXT    NOT NULL,
  to_exist     INTEGER NOT NULL DEFAULT 1,
  image_url    TEXT    NULL,
  medium       TEXT    NOT NULL DEFAULT 'unknown',
  message      TEXT    NOT NULL DEFAULT 'unknown',
  height       TEXT    NOT NULL DEFAULT 'unknown',
  description  TEXT    NOT NULL DEFAULT '',
  -- Optional stable natural key for sightings that originate from an external
  -- import source (e.g. the pirate map), namespaced as "<source>:<hash>". It is
  -- NULL for ordinary user-submitted sightings. It lets a re-import reconcile to
  -- the records it created on a previous run instead of duplicating them. It is
  -- deliberately NOT unique: the import appends a new corrective sighting when a
  -- record's reported state changes, exactly like any other append-only edit.
  external_id  TEXT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_sightings_location  ON sightings(location_id);
CREATE INDEX IF NOT EXISTS idx_sightings_reporter  ON sightings(reporter_id);
CREATE INDEX IF NOT EXISTS idx_sightings_observed  ON sightings(observed_at);
CREATE INDEX IF NOT EXISTS idx_sightings_external  ON sightings(external_id);

CREATE TABLE IF NOT EXISTS suggested_location_movements (
  id                    INTEGER PRIMARY KEY,
  sighting_id           INTEGER NOT NULL REFERENCES sightings(id),
  suggested_by          INTEGER NOT NULL REFERENCES users(id),
  suggested_lat         REAL    NOT NULL,
  suggested_lon         REAL    NOT NULL,
  created_at            TEXT    NOT NULL
) STRICT;

-- ---------------------------------------------------------------------------
-- User flags (append-only; e.g. 'insincere'). Latest row per (target,type)
-- wins: value=1 sets the flag, value=0 clears it. History is preserved.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS user_flags (
  id             INTEGER PRIMARY KEY,
  target_user_id INTEGER NOT NULL REFERENCES users(id),
  flag_type      TEXT    NOT NULL DEFAULT 'insincere',
  value          INTEGER NOT NULL DEFAULT 1 CHECK (value IN (-1,1)),
  flagged_by     INTEGER NOT NULL REFERENCES users(id),
  created_at     TEXT    NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_flags_target ON user_flags(target_user_id, flag_type);

-- Derived "current" insincere set: newest flag row per user, still set to 1.
CREATE VIEW IF NOT EXISTS current_insincere AS
SELECT user_id FROM (
  SELECT target_user_id AS user_id, value,
         ROW_NUMBER() OVER (
           PARTITION BY target_user_id
           ORDER BY created_at DESC, id DESC
         ) AS rn
  FROM user_flags
  WHERE flag_type = 'insincere'
)
WHERE rn = 1 AND value = 1;

-- ---------------------------------------------------------------------------
-- Reputation notes ("gossip"; append-only). Read access is audit-scoped.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS reputation_notes (
  id             INTEGER PRIMARY KEY,
  target_user_id INTEGER NOT NULL REFERENCES users(id),
  author_user_id INTEGER NOT NULL REFERENCES users(id),
  note           TEXT    NOT NULL,
  created_at     TEXT    NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS idx_notes_target ON reputation_notes(target_user_id);

CREATE TABLE IF NOT EXISTS frequent_messages (
  abbr TEXT PRIMARY KEY
) STRICT;
INSERT INTO frequent_messages (abbr) values
  ('jicr'),
  ('js'),
  ('j+'),
  ('play'),
  ('verse') ON CONFLICT DO NOTHING;
