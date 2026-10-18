# Public API v1

The stable client base is `/api/v1`, with snake_case JSON and seconds for public
playback/progress fields. The machine-readable contract is
[`contracts/openapi.json`](../contracts/openapi.json). Web and iOS must use this
same contract. No client code or deployment is included here.

All routes require an account session except setup/login and loopback health.
Authenticate with `Authorization: Bearer <token>` or the HttpOnly Strict SameSite
`fs_session` cookie. HTTPS origins issue Secure cookies. Browser mutation requests
must use the configured origin; cross-site Fetch Metadata is rejected. Native
clients can omit Origin. JSON mutation bodies require `application/json`, exact
field spelling, a single JSON value, and no duplicate/case-alias fields. Request
bodies are capped at 32 KiB, nesting at 32. Errors are `{"error":"stable_code"}`.
No private-service error body is forwarded.

## Accounts and profiles

| Method and route | Request / result |
|---|---|
| POST `/auth/setup` | First account only: `username`, `password`, `setup_token`; returns session token, expiry, account ID; administrator |
| POST `/auth/login` | `username`, `password`; returns session token, expiry, account ID |
| GET `/auth/me` | Account ID and admin flag |
| POST `/auth/logout` | Revokes current session; 204 |
| PUT `/auth/password` | `current_password`, `new_password`; changes password and invalidates all account sessions; 204 |
| POST `/accounts` | Admin only; `username`, `password`; creates a non-admin account |
| GET, POST `/profiles` | List owned profiles / create with `name`, `color` |
| GET, PUT, DELETE `/profiles/{profile}` | Read / replace name and color / delete owned profile |
| PUT `/profiles/{profile}/preferences` | `completed`, `format: both\|movie\|tv`, `genres` |
| POST `/profiles/{profile}/activity` | Record today's UTC activity; 204, no body required |
| GET `/status` | Redacted snapshot state and aggregate pending deletion/revocation counts |

Each account owns its profiles. An administrator can create household accounts
but cannot read another account's profiles. Cross-account IDs return the same 404
as absent IDs. Profile names are at most 80 bytes, colors 1–32 bytes, usernames
ASCII alphanumeric/underscore/hyphen (1–64, case-insensitive). Passwords are 12–256
bytes. There are at most 32 accounts, 32 concurrent retained login sessions per
account and 128 profiles total. Login admission is capped at 20 attempts/minute
per process, with two concurrent Argon2id operations; this intentionally favors a
small private household. Restart resets throttling, never authentication state.

Preferences accept action, comedy, drama, crime, scifi, animation, documentary,
horror and legacy family (no duplicates). Completion is persisted onboarding
state. The catalog's existing cached discovery/search supplies onboarding choices;
the reference's specific 48-choice/banner presentation is a future client concern.

## Catalog and browsing

| GET route | Result |
|---|---|
| `/browse` | Cached general shelf envelopes |
| `/search?q=...&offset=0` | Literal cached-title search; `scope: cached_titles`, 50/page |
| `/search-results?q=...` | Last catalog discovery result for the exact query |
| `/titles/{movie\|tv}/{id}` | Cached title envelope, including detail/summary distinction |
| `/tv/{id}/seasons/{season}` | Season envelope |
| `/tv/{id}/seasons/{season}/episodes/{episode}` | Exact episode with show ID, timestamps and state |
| `/artwork/{w300\|w500\|w1280}/{filename}` | 302 redirect to the public TMDB image CDN; no server image payload or cache |
| `/profiles/{profile}/home` | General shelves and profile-authorized recommendations, or null if missing |
| `/profiles/{profile}/recommendations?type=all\|movie\|tv` | Profile-authorized recommendation envelope |

Season zero is valid. Canonical identity is `(type,id)` with a positive TMDB ID;
IMDb and `mal_id` are optional. Runtime in catalog records remains **minutes**.
Missing records return 404, saved expired records remain 200 with `state: stale`.
Freshness is recomputed on reads. Catalog state is not source availability.
Authenticated title/season/search/browse reads enqueue bounded deduplicated refresh
intents locally; they do not wait for the catalog. One-minute background workers
submit those requests and replace the full retained snapshot after validation.
First-time data may take more than one sweep to arrive; this is not upstream-on-read
search or a freshness SLA. Offsets are 0–20000; queries 1–200 bytes.

The API never publishes the private snapshot or an arbitrary recommendation
handle. Public poster, backdrop and still references are rewritten to `https://image.tmdb.org/t/p/*`. Clients fetch and cache images directly; the API stores only metadata. Legacy `/api/v1/artwork/*` paths redirect to that fixed CDN. Personalized
views use the full ranked catalog pool with the catalog's format preference,
category filtering, 24 featured picks and franchise spacing; preference/input
fingerprint mismatch keeps old results usable but stale.

## Libraries and measured viewing

| Method and route under `/profiles/{profile}` | Behavior |
|---|---|
| GET `/library`, GET `/favorites` | Saved title summaries |
| PUT, DELETE `/library/{type}/{id}` | Add/remove watchlist entry; 204 |
| PUT, DELETE `/favorites/{type}/{id}` | Add/remove explicit favorite; 204 |
| GET `/history?offset=0` | Title/episode progress, 100/page, newest saved first |
| GET `/resume?type=tv&id=7&season=0&episode=1` | Targeted exact resume record; movie omits coordinates |
| PUT `/progress` | Cumulative measured viewing update for one playback ID |

Library and favorites are separate (1000 and 50 entries/profile). Additions can
use known IDs without catalog availability. Missing cached metadata yields an
identity-only saved title rather than inventing title details.

Progress body: `playback_id`, positive increasing `sequence`, `position_seconds`,
`duration_seconds`, cumulative `watched_seconds`. Obtain the playback ID through
setup; its stored title/episode is authoritative. Different clients create
separate playback IDs. Each counter measures active playback, excluding seeking,
pauses and buffering; it is client telemetry, not proof a person watched. The API
atomically adds only the new cumulative delta, ignores already-seen/older sequence
numbers, rejects higher-sequence counter regression, and retains sequence/counters
through restart. Equal cumulative time with a new sequence may update a seek.
Independent simultaneous playback IDs contribute their own measured time.

A position jump contributes **zero** measured time. Completion requires at least
90% position and 60% measured duration; completion is sticky. Position and duration
are at most 86400 seconds; cumulative time at most seven days and at most twice
elapsed session wall time plus 30 seconds tolerance. A final heartbeat is accepted
up to five minutes after lease expiry. Expired playback identities are retained
for seven days then cleaned by an owned worker; old identities cannot be reused
for progress. This is bounded telemetry retention, not indefinite event storage.
Progress only reads/writes its exact target and session, and never fetches the
whole library or runs cleanup. TV summaries count distinct exact episodes with
600 measured seconds; recommendation inputs aggregate totals by title, not episode
position. Up to 100 most recently active title summaries are synchronized, plus
preferences, 50 favorites and 30 current UTC activity dates.

## Playback

All routes below are under `/profiles/{profile}` and enforce ownership. Setup and
mutations require an ASCII `Idempotency-Key` (1–128 alphanumeric/underscore/hyphen).

| Method and route | Request / result |
|---|---|
| POST `/playback` | `type`, `id`, TV `season`/`episode`, optional `imdb_id`, `mal_id`, `device`, `preferences` |
| GET `/playback/{id}` | Reconcile relay state and issue an authorized URL |
| DELETE `/playback/{id}` | Persist revocation intent, revoke relay session; 204 after acknowledgment |
| POST `/playback/{id}/renew` | `expected_revision`; extend lease and issue replacement URL |
| POST `/playback/{id}/backup` | `expected_revision`; explicitly activate next stored backup |
| POST `/playback/{id}/refresh` | `expected_revision`; resolve uncached sources and replace selection |

Device fields: `formats` (HLS/MP4), optional `codecs`, `max_height`,
`allow_unknown_codecs`, `allow_unknown_resolution`, `supports_encrypted_hls`.
Defaults request HLS/MP4 with avc1/mp4a and encrypted-HLS support; unknown codecs
and resolution are not accepted by default. Playback preferences optionally carry
`audio_languages`, `subtitle_languages`, `require_audio`, `require_subtitles`,
`max_height`. Unknown language is never promoted to English.

A playback reply contains `id`, exact `title` identity/coordinates, relative
`url: /stream/{opaque}/entry?st=...`, `format`, `expires_at`, CAS `revision`,
`source_generation`, `reload_required`, and `resume_seconds`. Grants use returned
relay **auth_revision**, independently of CAS revision and generation. Grants are
Ed25519 JWTs, capped at six hours and the relay lease. Renew requires a later
whole-second expiry; an immediate renewal may return `renew_too_early`. Existing
VOD child grants do not renew themselves. Clients must replace the playback item
with the returned URL when instructed, then restore `resume_seconds`; source
switches do not promise timeline/language alignment or seamless playback.

Known-ID setup and refresh use stored request IDs plus optional durable local
catalog enrichment, with no synchronous catalog dependency. `id`/`mal_id` translate
explicitly to provider `tmdb_id`/`my_anime_list_id`. `context_key` is the trusted
random profile ID. The provider's complete Selection and backup order pass unchanged
to relay control. Raw selection durations remain integer Go nanoseconds; they are
never converted into public seconds or exposed to clients. No source URL, cookie,
provider metadata or credential is returned or logged. Media is served directly
by the relay, independently of API/provider/catalog availability.

One durable mutation at a time is admitted per playback. HTTP admission currently
serializes playback setup/control globally with immediate `playback_busy` rather
than an unbounded queue; browsing/progress remain independent. Stored intents and
prepared payloads recover lost replies; retries use the same key/revision. A lost
reply followed by GET and restart still cannot switch twice. A superseded old
mutation returns `operation_superseded`; a different key while unresolved returns
`operation_pending`. Backup exhaustion releases its intent so refresh can proceed.
The API must be the exclusive relay controller for sessions it owns.

Revocation intent blocks further API grants immediately. If the relay is down,
DELETE returns retryable failure and an independent worker retries the durable
intent. Previously issued capabilities remain usable at the relay until relay
revocation succeeds or their lease expires; immediate cross-service revocation
cannot be promised during a control outage. Profile deletion persists the same
revocation outbox and catalog-derived-data deletion, then removes personal rows.
Even an ambiguous create is reconciled using its original encrypted payload before
revocation. Successful deletion acknowledgments are persisted to avoid starvation.

Common errors: 400 invalid_request; 401 unauthorized; 403 forbidden/origin_rejected;
404 missing; 409 revision_conflict/idempotency_conflict/operation_pending;
410 revoked/session_expired; 422 no_source; 429 overloaded/playback_busy;
502 invalid private response; 503 private service/storage unavailable. Preserve
profile/session/key context when retrying; never display raw private bodies.
