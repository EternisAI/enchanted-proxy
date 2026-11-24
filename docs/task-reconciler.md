# Task Reconciler

The task reconciler (`internal/task/reconciler.go`) keeps the `tasks` table in line
with the Temporal schedules that run the tasks, and with their owners' accounts and
plans. It runs inside the proxy, on every instance, once per interval.

## What a sweep does

A sweep lists the namespace's schedules of the `ScheduledTaskWorkflow` workflow, then
the `pending`, `active` and `paused` task rows, then looks the rows' owners up in
Firebase Auth. It then:

| Finding | Action |
|---|---|
| Schedule paused in Temporal, row `active` | Pause made again with the same note, then row set `paused`, reason from the first word of the note |
| Both paused, row's reason differs from the note's | Row takes the note's reason |
| Schedule running, row `paused` | Row set `active` |
| Row `pending` for over an hour with a schedule | Row set `active`, once the schedule is described again |
| Row `pending` for over an hour without a schedule | Row and its creation record deleted, so the attempt stops counting |
| One-time task whose run has passed | Row set `completed` |
| Any other row without a schedule | Row deleted |
| Schedule without a row | Schedule deleted |
| Owner deleted or disabled | Schedule and row paused, reason `account_missing` |
| Owner of a recurring task inactive for `TASK_INACTIVE_DAYS` | Schedule and row paused, reason `inactive` |
| Recurring task over its quota group's current plan | First seen time recorded; after `TASK_PLAN_ENDED_GRACE_HOURS`, paused with reason `plan_ended` (at once when the grace is 0) |
| `plan_ended` task that fits the plan again | Row set `active`, then the schedule resumed. The row is set as an owner's resume sets it: under the quota group's admission lock, if the plan still has room. This is the only pause the reconciler lifts. |

Inactive means no sign-in and no token refresh. The tasks of a quota group (the
accounts a purchase has been attached to; an account alone otherwise) are walked
oldest first against the best plan any member holds: a task is over when it is past
the plan's number of recurring tasks, runs more often than the plan's shortest
interval, or takes the group past the plan's runs per day. Lifting a hold uses the
same plan.

Pause notes start with the reason and a colon (`inactive: owner inactive for 90 days
(task reconciler)`). Pauses made by hand with the Temporal CLI should follow the same
form; a note with no known reason counts as `admin`, which owners cannot lift.

Every change is checked against Temporal or the database right before it is made: a
row is deleted only when Temporal answers that its schedule does not exist, a schedule
only when its row is still absent and the schedule itself still runs the task workflow,
and a schedule paused since the listing keeps its pause and note. Each check and change holds the task's lock, the same Postgres
advisory lock that creating, pausing, resuming and deleting a task hold, so they cannot
interleave with an owner's change to the same task.

A schedule can read paused while a resume that returned an error may still reach
Temporal, after an owner's resume or a lifted hold failed. In that case the row stays
`active`, so the task keeps counting. Before the reconciler sets such a row `paused`,
it makes the pause again with the same note. A resume is an update conditional on the
schedule's conflict token from the read it was decided on, and every pause changes that
token, so Temporal drops or refuses the earlier resume whenever it arrives.

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `TASK_RECONCILER_MODE` | `dry-run` | `off`, `dry-run` (report only) or `enforce`. An unknown value gives `dry-run`. |
| `TASK_RECONCILER_INTERVAL_MINUTES` | `60` | Time between sweeps, 1 to 10080 (a week). The first sweep is two minutes after start. |
| `TASK_INACTIVE_DAYS` | `90` | Days without sign-in or token refresh after which an owner's recurring tasks are paused, 1 to 3650. |
| `TASK_PLAN_ENDED_GRACE_HOURS` | `168` | How long a task may stay over its owner's plan before it is held, 0 to 8760 (a year). |
| `TASK_PAUSE_NOTIFICATIONS` | `false` | Push to the owner when tasks are paused as `inactive` or held as `plan_ended`, one push per owner and reason per sweep. |

All five are in the enclave's `env` allowlist. Unless the mode is `off`, the proxy
refuses to start with a database pool (`DB_MAX_OPEN_CONNS`) of fewer than three
connections. A sweep keeps one for its lock, a task's lock takes another, and the work
under that lock needs a third. The reconciler needs
`identitytoolkit.googleapis.com` (account lookups) and `oauth2.googleapis.com` (access
tokens) in the egress allowlist.

Leave notifications off for a sweep expected to pause many tasks at once, such as the
first enforced sweep on existing data, and turn them on afterwards. A push is sent only
when the reconciler itself pauses a task, so tasks paused earlier never trigger one.

## Reports

Each sweep writes one row to `task_reconciler_runs`, with the mode and a JSON report:
counts of rows, schedules and owners, and for each action (such as `pause:inactive`)
the task IDs it applied to, or in dry-run mode would apply to.

```sql
SELECT started_at, mode, report->'actions', report->'errors'
FROM task_reconciler_runs ORDER BY id DESC LIMIT 5;
```

The same summary is logged as `task reconciler sweep finished`, and each action as
`task reconciler: applied` or `task reconciler: would apply`.

## Coordination and safeguards

- **One sweep per interval.** A sweep holds a Postgres advisory lock in a transaction,
  and skips if another instance recorded a sweep within 90% of the interval.
- **Accounts unknown.** If the account lookup fails, or more than half of the owners
  (and at least five) have no account, which points at the wrong Firebase project, the
  account rules are skipped, `plan_ended` holds are kept until a sweep can read the
  accounts, and the report carries an error.
- **Shared Temporal namespace.** Every proxy deployment that uses the same Temporal
  namespace must use the same database: the reconciler deletes schedules that have no
  row in its own database. If more than half of the schedules (and at least ten) have
  no row, it deletes none and reports an error. Keep the reconciler in `dry-run` or
  `off` on any deployment that shares a namespace with another database.
