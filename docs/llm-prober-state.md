# LLM Prober Persistent State

The prober records the health state of each probe target on disk so it survives a
restart. Everything here is optional: with no state database the prober behaves
exactly as it did before persistence existed.

## Why

Each probe worker has two stages. The initial stage discovers a state it does not
yet know, and a first success there is silent by design — announcing "healthy" for
every endpoint on every startup would be noise. The normal-operation stage then
notifies on each transition.

Without persistence, every restart re-enters the initial stage. That loses the
recovery notification in exactly the case where it matters most: an endpoint fails,
the prober alerts, an operator rotates the offending API key and restarts the
workload, the next probe succeeds — and the success is swallowed as an initial
state. Nobody is told the outage ended.

With persistence, a target that has a stored state skips the initial stage and
resumes in the normal-operation stage, so that first success is a transition and
gets announced. The state it resumed *into* is not re-announced; the process that
recorded it already did that.

## What is stored

One record per probe target, updated in place. Probe attempts are not journalled.

| Field | Purpose |
|---|---|
| `provider`, `canonical_model`, `base_url`, `effective_model` | target identity (also the key) |
| `state` | `healthy` or `failing` |
| `state_changed_at` | when that state was entered |
| `last_probe_at` | when the target was last probed |

The store is [bbolt](https://github.com/etcd-io/bbolt): a single file, pure Go
(the prober builds `CGO_ENABLED=0`), transactional, and a negligible addition to
the module's dependencies.

### Identity

The key is the full endpoint tuple — provider name, canonical model name,
normalized base URL, and upstream model name. This mirrors the
`(base_url, effective_model)` pair that `NewProbeService` already deduplicates
workers on, plus the two names that label the target's metrics, so a record maps
to exactly one worker.

A config change that reroutes a model to a different provider or upstream name
therefore starts that target fresh rather than inheriting the state of the
endpoint it replaced. That is intended: it is a different endpoint. The old record
is simply never looked up again — there is no garbage collection.

### Write volume

State transitions are written through immediately, in their own transaction. They
are rare, and one lost to an ungraceful shutdown would resurrect the bug this
exists to fix.

Last-probe timestamps are held in memory and coalesced: one flusher writes every
pending timestamp in a single transaction per flush interval (default 5m), and on
graceful shutdown. Write volume is therefore flat — roughly 290 transactions a day
regardless of how many endpoints are configured or how fast they are being
retried. Writing each timestamp through instead would cost ~1,250 transactions a
day at steady state and ~18,700 during an outage that puts every target on its
retry interval.

An ungraceful kill can lose up to one flush interval of timestamps. The cost is at
most one early re-probe per target; notification correctness is unaffected, since
that rides on the state transitions, which are never buffered.

## Startup behaviour

On startup each worker with a valid record:

1. adopts the stored state and publishes its health gauge immediately, so
   `model_router_probe_healthy` has no gap while the first probe runs;
2. skips the initial stage;
3. schedules its first probe at `last_probe_at + interval` — the probe interval
   when healthy, the retry interval when failing — plus the usual jitter.

If that moment has already passed, the first probe runs immediately (still
jittered). Measuring from the last probe rather than the state change is what stops
a crash-looping pod from re-probing every endpoint on every restart;
`state_changed_at` is the fallback for a record written before any timestamp was
flushed.

## Failure handling

Nothing about the state is allowed to stop the prober from monitoring. A missing
file, a missing directory, a read-only volume, a lock held by an outgoing pod, or
an unreadable database each produce a **warning** and the prober runs without
persistence.

Individual records that cannot be trusted — undecodable, an unrecognized schema
version, an unknown state, an incomplete identity, a missing or future-dated
timestamp, or an identity that disagrees with the key it is stored under — are
logged and skipped. Those targets start as if never probed. Invalid records are
ignored, never deleted.

## Configuration

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-state-db` | `LLM_PROBER_STATE_DB` | `/var/lib/llm-prober/state.db` (set in the image) | Database path; empty disables persistence |
| `-state-flush-interval` | — | `5m` | How often pending timestamps are coalesced |
| `-dump-state` | — | — | Print the database as JSON and exit |

`-dump-state` opens the database read-only, so it is safe to run against a live
prober:

```bash
kubectl exec deploy/llm-prober -- llm-prober -dump-state | jq '.[] | select(.valid) | {key, state: .state.state}'
```

Records that would be skipped at load time are included in the dump, flagged with
`valid: false` and the reason, so a store the prober is silently ignoring can be
diagnosed.

## Deployment requirements

The manifests live in the `gitops-apps` repository. Three things matter:

- **Mount a PersistentVolume at `/var/lib/llm-prober`.** Without one the state
  lives in the container's writable layer: it survives an in-place container
  restart but not rescheduling.
- **Set `securityContext.fsGroup`.** The container runs as the non-root `prober`
  user, and a volume mounted `root:root` is not writable by it — the prober would
  warn and run stateless.
- **Use `strategy: Recreate`, not `RollingUpdate`.** bbolt takes an exclusive file
  lock. On a rolling update over a ReadWriteOnce volume the incoming pod finds the
  lock held by the outgoing one, waits 10s, then gives up and runs stateless for
  its entire lifetime.

The prober has a single replica by design; running two against one volume is not
supported (the second gets no persistence).
