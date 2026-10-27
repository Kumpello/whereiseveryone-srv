# Points of interest

Each account can save up to five named map points. Points are visible to their
creator and accepted friends, independently of live location sharing. Pausing
sharing or wiping a live location leaves saved points visible. Pending and former
friends have no access through the friends API. Only the creator can edit or
delete a point.

## API

All routes use the existing `/me` authentication and database-admission middleware.

| Method | Route | Result |
| --- | --- | --- |
| GET | `/me/points` | Array of the authenticated user's points, possibly empty |
| POST | `/me/points` | Create a point; 201, or 409 when five slots are occupied |
| PUT | `/me/points/{id}` | Update an owned point; 200, or 404 for missing/other-owned IDs |
| DELETE | `/me/points/{id}` | Delete an owned point; 204, or 404 for missing/other-owned IDs |

Create and update accept `name` (required, at most 64 Unicode characters),
`description` (optional, at most 1000 Unicode characters), `latitude` (-90 to 90),
and `longitude` (-180 to 180). Coordinates must be present; zero is valid.
Names and descriptions are trimmed, and whitespace-only names are rejected.
Requests retain the existing 16 KiB body limit and JSON content-type requirement.

Responses contain `id`, `name`, `description`, `latitude`, `longitude`,
`creator_username`, and `created_at` (UTC Unix milliseconds). The server assigns
identity, creator, and creation time. Updates preserve identity and creation time.
Creator and timestamp fields supplied by clients cannot override server metadata.

Accepted entries in the existing paginated `GET /me/friends` response now include
`points_of_interest` when nonempty. Each page remains bounded to 50 friends, with
at most five points per friend. Points remain included when `location` is absent
or sharing with the viewer is paused. Pending entries omit points. The Android
map consumes these existing refreshes rather than adding another polling timer.

## Storage and validation

Points are embedded in the creator's user document. An atomic append conditioned
on the fifth array slot being absent enforces the cap under concurrent requests.
Updates and deletions match both the authenticated owner and point ID. No new
collection, index, database reset, or data migration is required.

Regression tests cover concurrent creates, immutable server metadata, ownership,
input validation, pending/former-friend privacy, and paused-location independence.
MongoDB tests use `MONGO_TEST_URI` and a replica set, as existing relationship tests do.

## Rollout and production protection

Deploy the backend before the Android client. Older clients ignore the additional
friend field. The new client needs the four routes above to load and manage its
points. Update any API route allowlist as needed for GET/POST `/me/points` and
PUT/DELETE `/me/points/{24-hex-character-id}`. The existing friends route's response
also grows to include the bounded point arrays.

Production protection verification is pending: this implementation task has not
accessed the production Cloudflare dashboard or Hetzner host. Before deploying,
recheck the active Cloudflare allowlist, authentication and body requirements,
proxying and Full (strict) HTTPS, Hetzner origin restrictions, Caddy trusted proxy
ranges, certificate renewal, and SSH access according to `AGENTS.md`. After
deployment, exercise point CRUD and friend visibility through the public hostname,
verify unknown routes are blocked, and verify direct-origin HTTP/HTTPS is blocked
while a fresh SSH connection still works. No production rules have been changed.
