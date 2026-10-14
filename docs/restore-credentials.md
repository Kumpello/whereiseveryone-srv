# Android Restore Credentials

The Android app enrolls a system-managed restore key after sign-in or sign-up and
when an existing signed-in user opens the map. On a restored device, the launcher
uses that key to sign in before showing the authentication screens. Backup remains
disabled for application data; the restore-key provider handles key transfer.
Saved passwords, access tokens and local database contents are not backed up by
this feature.

Creation first requests encrypted cloud backup. If Google Play services reports
that encrypted backup is unavailable, the app creates a key for device-to-device
transfer instead. Devices without a supported provider continue through normal
password authentication.

## API contract

All four routes use `POST` and `application/json`, with the existing 16 KiB body
limit and database admission limits. Restore requests also share source/global
rate limits. Registration requires a currently valid Bearer access session and
the session's device identifier. Authentication is discoverable and needs no old
session or username.

| Route | Request | Response |
| --- | --- | --- |
| `/auth/restore/register/options` | `app_id`, `device_token` | `challenge_id`, `request_json` |
| `/auth/restore/register/finish` | Options-request fields, `challenge_id`, `credential_json` | `204` |
| `/auth/restore/login/options` | `app_id`, `device_token` | `challenge_id`, `request_json` |
| `/auth/restore/login/finish` | Options-request fields, `challenge_id`, `credential_json` | `id`, `username`, `token`, `refresh_token` |

The JSON strings are native WebAuthn creation/request options and responses.
Challenges are random, expire after five minutes, are bound to their ceremony,
app, device and enrollment account, and are consumed atomically in MongoDB.
Registration retries are idempotent only when an already accepted response is
identical and belongs to the same authenticated account, app and source device.

Signature, challenge, RP ID, native app origin, user handle and backup flags are
verified with `go-webauthn`. User presence and verification are deliberately not
required for these silent, system-managed keys. This policy must not be reused
for user-facing passkeys. Authentication issues fresh tokens for the destination
device; a different source device's existing session is revoked immediately.

## Configuration

`app.restoreRPID` defaults to `where-is-everyone.com`.
`app.restoreAndroidOrigins` is a JSON object encoded as a configuration string,
mapping each allowed Android package to its certificate origins. The defaults
permit the production and premium release packages with the signing certificate
verified from the current locally built production APK:

```json
{
  "com.kumpello.whereiseveryone": [
    "android:apk-key-hash:FZ8-huqbfDmzEVV-OxwPrelm4SCHu-7sw6XK_Xd-4qs"
  ],
  "com.kumpello.whereiseveryone.premium": [
    "android:apk-key-hash:FZ8-huqbfDmzEVV-OxwPrelm4SCHu-7sw6XK_Xd-4qs"
  ]
}
```

The public SHA-256 certificate fingerprint is
`159f3e86ea9b7c39b311557e3b1c0fade966e12087bbeeecc3a5cafd777ee2ab`.
If Google Play signs distributed APKs with a different certificate, configure its
app-signing fingerprint instead. Certificate rotation can list multiple trusted
origins. Never accept a client-supplied origin as the allowlist or use a wildcard.
Set `app.restoreAndroidOrigins` to the string `"{}"` to disable restoration.
Debug/development packages and their certificates must be explicitly configured
on a development server; production defaults do not trust them.

MongoDB uses the existing replica-set requirement. Startup creates indexes for
`restore_keys` and `restore_challenges`; `restore_accounts` serializes enrollment
transactions per account. No existing user or token schema migration is needed.

## Backend guidelines

- Keep restore keys in their own collection, distinct from passwords and
  user-managed passkeys. They use silent verification and do not appear in a
  passkey-management UI.
- Limit orphaned keys after reinstall/data clear: inactive keys expire after
  365 days, and registration retains at most five keys per source device/app and
  twenty per account. The most recent registration is retained even when
  timestamps tie. Android renews enrollment after 180 days.
- Keep transferred copies usable when a source device logs out. Logout clears
  the local platform key; it does not immediately revoke server records. Their
  inactivity expiry and bounded retention perform eventual cleanup.
- Store multiple keys per account, each associated with its application and a
  hashed source-device identifier. An assertion authenticates a destination
  device independently of that source identifier.
- Clear the local key after explicit logout or terminal server-session
  invalidation. Android persists a restoration guard if provider cleanup fails,
  protects session writes against logout races, and preserves keys during
  temporary network/server failures.

## Validation and phone testing

Automated tests cover real ES256 signatures without user presence/verification,
bad signatures/origins/RP IDs/user handles, expired and replayed challenges,
synced counters, atomic consumption across replicas, key retention limits,
authenticated enrollment and HTTP request limits. Android tests cover encrypted
pending enrollment, lost responses, restoration, unsupported providers,
cancellation, failed cleanup and concurrent logout/session changes.

After deploying the backend and installing the release on the source device,
open the map while signed in so its key is enrolled. Use a separate device during
normal device setup, or Android Studio's **Backup App Data → Device to Device**
and **Restore App Data** workflow for an explicitly allowed debug package.
Opening the restored app should go straight to the map. Logging out and opening
it again must remain signed out. A source-device logout after creating a backup
must not prevent the destination copy from restoring. Keep a test account and
perform uninstall/data-clear steps manually.

The existing `allowBackup=false` remains unchanged. Android Studio's cloud-backup
simulation does not support this manifest setting; use its device-to-device
simulation or an actual device setup to validate transfer.

References: [Android implementation](https://developer.android.com/identity/sign-in/restore-credentials-implementation),
[Android transfer testing](https://developer.android.com/identity/sign-in/test-restore-credentials),
[Go WebAuthn library](https://github.com/go-webauthn/webauthn).

## Deployment status

This change implements both repositories locally. Production deployment and
live Cloudflare/Hetzner protection verification have not been performed. Before
deployment, include the four routes above in the active API allowlist and follow
the existing backend `AGENTS.md` requirements for proxy, origin firewall, SSH,
certificate renewal, public API and direct-origin checks.
