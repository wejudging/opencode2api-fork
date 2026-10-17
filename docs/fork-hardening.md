# Fork reliability notes

This fork's hardening keeps inference restrictions separate from transport and
account health. Do not restore the old earliest-expiry anonymous fallback.

## Routing invariants

- `retry.max_attempts` remains the per-key-tier limit. `retry.max_total_attempts`
  defaults to **6** and caps all inference attempts across anonymous origins,
  proxies, authenticated tiers, and stale-reasoning repair for a normal request.
  Diagnostic single-key calls remain single-attempt. Rotation aliases have
  their own existing member-selection loop; they are not a normal route.
- A long 429 (`Retry-After >= 1h`) only exhausts the affected static exit/model.
  Other eligible exits are tried within the total budget. The second anonymous
  origin is skipped for that exhausted exit. An identical reset time is not
  evidence that all exits have exhausted their quotas.
- A final free-inference 403 sidelines that exit/model for 12h. Legacy/new
  anonymous origins are still allowed to fall back before a ban is recorded.
  Daily quota waits honor Retry-After (bounded to seven days), rather than the
  transient network backoff ceiling of 120s.
- Free restrictions are shared between anonymous and key-backed free requests.
  They do not cool the account used for paid models, and model discovery or a
  successful network check cannot clear them.
- Persistent state is `CONFIG_PATH.free-exits.json`, written atomically with
  owner-only permissions. IDs hash upstream configuration, proxy identity and
  model; credentials/addresses are not stored. Configuration reloads share the
  same store. Recent expired entries permit one concurrent half-open probe.
  An older in-flight success cannot erase a newer ban.
- This identifies a configured static exit, not its observed public NAT IP.
  Two proxy URLs sharing a public IP are not automatically deduplicated. A
  changed static proxy address/upstream configuration gets a new identity.
  Rotating `#pool` entries opt out because one URL does not identify one exit.
  Quota buckets are conservatively model-scoped until broader sharing is proven.
- If every considered free exit is cooling, return a local 429 with
  `code=free_exit_cooldown` and Retry-After, with zero upstream attempts.
- Corrupt/unreadable persisted restriction state fails startup explicitly;
  back it up before repair. Do not silently discard long bans on parse errors.

## Timeouts and monitoring

The existing attempt and first-event settings should be staged against actual
model latency. An example conservative starting point is 90s per attempt, 60s
for the first complete SSE event, and 300s total. First-event latency is not
first-text-token latency. Do not replay after downstream delivery begins.

`GET /healthz` remains a non-inference liveness/readiness check and explicitly
reports `inference: not_probed`; it cannot prove daily quota availability.
Authenticated `GET /api/monitor` includes `resources.free_exits`, outcome counts
by model/caller, client cancellations, and request first-event timing. Client
cancellations are separate from errors and excluded from success-rate
denominators. Failure HTTP statuses and stream errors are WARN, not INFO.
Use `status`, `outcome`, `attempts`, `legs`, and `first_event_ms`, not ERROR count
alone. Statistics are process-local; persisted logs are the long-term record.

## Publishing and safe access

`sync-and-build` resolves the **actual checked-out SHA after upstream sync**,
runs formatting/vet/race tests/build, and only publishes that exact source if
verification passes. Image version, revision and immutable SHA tag use the
same SHA. Deploy a validated digest and retain its predecessor for rollback.

The sample Compose binds the administration host port to `127.0.0.1` by
default. Keep the container's listener on `0.0.0.0`; use an SSH tunnel or local
HTTPS reverse proxy. Restrict API publication only after auditing its callers.
No production secrets or host-specific operational records belong in this
public repository; keep deployment evidence and runbooks in private storage.

## Regression checks

Run `go vet ./...` and `go test -race ./...`. Gateway tests cover hard cooldown
selection, independent exits, cross-key sharing, paid-route isolation,
persistence, single half-open admission, cancellation, stale successes, runtime
rebuilds, global budgets, first-event timing, established streams, and native
Google endpoints. All tests use local or in-memory upstreams, never paid APIs.
