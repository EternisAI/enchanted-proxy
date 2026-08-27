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
| `-dump-state` | — | — | Print a **stopped** prober's database as JSON and exit |
| `-debug-listen` | — | `localhost:9091` | Loopback address serving `/debug/state`; empty disables it |

To read a **running** prober, use the `/debug/state` endpoint. bbolt holds an
exclusive lock on the file for as long as the prober has it open, and a
read-only open still needs a shared lock, so no second process can read the
database while the prober runs — the live view has to be served from inside the
process:

```bash
kubectl exec sts/llm-prober -- wget -qO- localhost:9091/debug/state \
  | jq '.[] | select(.valid) | {key, state: .state.state}'
```

It returns 503 with the reason when persistence is disabled or unavailable.

The endpoint is on its own listener bound to loopback, not on the metrics port.
The metrics port is bound on all interfaces so Prometheus can scrape it and
carries no authentication, so anything able to reach the pod can read what is
served there; the namespace has no NetworkPolicy or Istio AuthorizationPolicy to
narrow that. Since the only consumer of the state view is an operator already
inside the container, binding it to loopback means nothing outside the pod can
reach it at all.

`-debug-listen` only accepts a loopback host — a `127.0.0.0/8` or `::1` literal,
or `localhost`. Anything else, including `:9091` and `0.0.0.0:9091`, is refused
with a warning and leaves the endpoint switched off, so the isolation cannot be
widened by a flag change alone.

`localhost` binds **both** loopback families, because the runtime image maps the
name to `127.0.0.1` and `::1` alike and a client may pick either — the busybox
`wget` in the container picks `::1`. Binding one family only would answer
"connection refused" to a client that chose the other, which reads like the
endpoint is disabled rather than like an address mismatch. An explicit IP
literal is bound exactly as given.

`-dump-state` is for a prober that is stopped, or for a copy of its volume. Run
against a running prober it fails with a message pointing at the endpoint rather
than hanging on the lock.

Both views include records that would be skipped at load time, flagged with
`valid: false` and the reason, so a store the prober is silently ignoring can be
diagnosed.

## Deployment requirements

The manifests live in the `gitops-apps` repository, at
`apps/enchanted-proxy/workloads/llm-prober/`. The prober is a **StatefulSet**
rather than a Deployment, for three reasons that all trace back to the lock bbolt
holds on the database file:

- **A volume at `/var/lib/llm-prober`,** provisioned by a `volumeClaimTemplates`
  entry (1Gi — the smallest EBS volume available; the database is tens of KiB).
  The volume is not optional in Kubernetes: a restarted container is a *new*
  container built from the image, so anything written to the container's own
  filesystem is gone. Without a volume the state does not survive even a crash
  loop, and the prober simply starts fresh every time.
- **`securityContext.fsGroup: 101`,** matching the `prober` group in the image
  (the container runs as uid 100, gid 101). A freshly provisioned volume is
  mounted `root:root`, which that user cannot write — the prober would warn and
  run stateless.
- **An update strategy that never overlaps two pods.** A StatefulSet rolling
  update is ordered: the outgoing pod is fully terminated before the incoming one
  is created. The workload previously ran as a Deployment with
  `maxSurge: 33%, maxUnavailable: 0`, which creates the replacement first — over
  a ReadWriteOnce volume the incoming pod finds the lock held, waits 10s, gives
  up, and runs stateless for its entire lifetime. A Deployment with
  `strategy.type: Recreate` would also terminate before creating, so the ordering
  alone does not require a StatefulSet; the StatefulSet is what additionally
  gives a stable pod identity and a per-pod volume from `volumeClaimTemplates`,
  which is what makes "exactly one holder of this database" structural rather
  than a property of the rollout settings.

Two further couplings are easy to miss when editing those manifests:

- The `VaultStaticSecret` for `proxy-api` lists the prober in
  `rolloutRestartTargets`. That is what restarts it when a rotated API key lands
  in Vault — the exact scenario this state exists to serve — so the entry must
  name `kind: StatefulSet`.
- The per-cluster `PatchTransformer` that stamps version labels selects on
  `kind`, so it must target `StatefulSet` too, or the labels silently stop being
  applied.

The prober runs a single replica by design; running two against one volume is not
supported (the second gets no persistence).

A StatefulSet creates its volumes as part of creating a pod, so an environment
that has *never* run the prober above zero replicas has no PVC and no PV —
production is the only one that provisions storage. Scaling an existing prober
down to zero is a different case: its PVC is kept, because
`persistentVolumeClaimRetentionPolicy.whenScaled` is set to `Retain` (also the
default) precisely so the state is still there when it scales back up. Deleting
storage requires deleting the PVC.
