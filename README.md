# Bugzilla Links for Mattermost

Personal Bugzilla link labels and hover cards for Mattermost **11.10+** web and desktop. No changes are made to stored messages or shared post metadata. Public bugs work without connecting a Bugzilla account; a connected user is queried immediately with their own key, with no anonymous fallback.

Supported links: `/58948`, `/show_bug.cgi?id=54753`, and `/show_bug.cgi?id=54753#c15`. Comment fragments identify the comment number within the bug, not its global database ID. Read-only: no changes to bugs, comments, or permissions.

## Install and configure

1. Build using `make dist`, or download a release bundle.
2. Upload `dist/space.altlinux.bugzilla-0.1.0.tar.gz` in **System Console → Plugins → Plugin Management**, then enable **Bugzilla Links**.
3. Open **System Console → Plugins → Bugzilla Links** and enter the **Bugzilla server URL**. The default is `https://bugzilla.altlinux.org`. HTTPS and a reachable `/rest` API are required.
4. For standard Bugzilla 5.2, leave **API key transport** set to **Query parameter**. Use the header option only when supported by your Bugzilla. Do not enable upstream query-string logging: standard Bugzilla transmits keys using `api_key`.
5. Configure an encryption secret before users connect personal accounts. Public previews require no encryption secret.

Generate a base64-encoded 32-byte secret with `openssl rand -base64 32`. Store it outside the Mattermost database, preferably as a mounted secret file, and configure either:

- `MM_BUGZILLA_ENCRYPTION_KEY_FILE=/run/secrets/mm_bugzilla_encryption_key`, or the **Encryption secret file** plugin setting with the absolute file path; or
- `MM_BUGZILLA_ENCRYPTION_KEY=<base64 value>` in the Mattermost server environment.

Only the path may be entered in plugin settings, never the secret itself. Keep the secret consistent across HA nodes and retain it in protected backups. Replacing the encryption secret requires users to reconnect their keys; existing ciphertext is not silently migrated.

Users open **Main menu → Bugzilla: личный API-ключ**, enter only their personal API key, and select **Подключить**. Password and login/email are not requested. Validation uses authenticated-only Bugzilla `User.get(match=...)` with a random match, `limit=1`, and `include_fields=id`. The validated key's owner determines all upstream access; the UI does not claim to know that owner's name or ID. Invalid keys and malformed/non-JSON responses are rejected.

The compact label starts as `🐞 BZ #123`. After a card has been loaded, the title/status can be resolved locally when the message formatter renders. Custom Markdown link labels and code blocks are preserved. Hover/focus opens a selectable card; clicking the link opens Bugzilla. The connection form is a private modal.

## Caching and access

- Server: bounded per-user/per-instance/per-connection-revision cache, **90 seconds**, with identical in-flight requests coalesced. At most 64 unique fetches may be in flight, with at most eight simultaneous upstream HTTP calls.
- Client: bounded **IndexedDB**, plus memory, **five minutes from the original Bugzilla response**. A server cache hit never restarts the five-minute clock. After expiration, old details are hidden until refreshed.
- Logout clears the current user's client data and invalidates late responses; the server cache expires naturally. Closing/reopening the client while signed in preserves IndexedDB.
- Disconnect/key replacement changes the authoritative revision stored in Mattermost KV. Each server request verifies the MM session and reads the current revision before cache access, and rechecks revision before returning a result. Requests from different users never share task responses.
- Public and personal responses have separate client cache namespaces. A private comment accidentally returned to an anonymous request is discarded. All text is rendered as text; no remote images, HTML, or attachments execute.
- API responses use `Cache-Control: no-store`; browsers/proxies must not create another cache outside the plugin's explicit IndexedDB policy.

Caching means access can remain visible for the remainder of the five-minute client lifetime after revocation in Bugzilla. No immediate cross-system revocation guarantee is claimed. Already displayed information cannot be recalled.

HA uses shared MM KV for connection records/revisions and a local cache on each node. A disconnect on one node prevents old personal cache entries being served on another even if a WebSocket event is lost. In-flight coalescing is node-local; duplicate upstream requests across nodes are possible. No Redis is required for the first implementation.

Native Android/iOS clients do not execute this webapp plugin. Their implementation can use the same authenticated server API and freshness policy; native UI integration is a separate stage.

## API

All routes require a verified Mattermost session. Mutation requests require `X-Requested-With: XMLHttpRequest`, and connection creation requires JSON.

- `GET /plugins/space.altlinux.bugzilla/api/v1/config`: configured URL, instance identity, connection revision, connected/encryption-ready flags. Never returns a key.
- `POST .../connection` with `{"api_key":"..."}`: validate and encrypt a personal key with AES-256-GCM. The user/instance identity is authenticated additional data.
- `DELETE .../connection`: replace the connection with a revisioned public-mode tombstone.
- `GET .../card?id=54753&comment=15`: normalized card; `comment` is optional. `refresh=true` bypasses successful-response caches, retaining concurrency limits.

Cards contain `source_fetched_at` and `max_age_ms`. Clients must not restart freshness when reopening cached cards. Errors expose stable codes and safe messages, not upstream URLs, credentials, or private response bodies.

## Development

Requires Go 1.26.4, Node 24+, npm, Python 3 for deterministic packaging.

```sh
make test
make dist
```

The bundle includes Linux amd64 and arm64 server executables. Tests cover encryption/AAD, cache isolation/TTL/singleflight, authenticated-only key validation, private comment filtering, local comment numbers, redirect handling, IndexedDB persistence/isolation, and safe message formatting. No credentials or private Bugzilla responses are in the repository.

The ALT public REST API was checked during development: `/rest/version` reports Bugzilla 5.2, `/rest/bug/58948` and `/rest/bug/54753/comment` return JSON. Production personal-key/private-bug tests require test accounts and cannot be inferred from public-only tests.
