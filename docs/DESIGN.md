# Why Sling works the way it does

The [README](../README.md) shows what Sling does and how the pieces connect. This
page is about the decisions behind it: what each one protects against, and what it
costs. Sling deploys one app to one server, and most choices follow from taking that
limit seriously instead of pretending to be a small Kubernetes.

## 1. Deploy an exact commit, never a branch

Sling takes a 40-character SHA and refuses anything else. It clones, checks out that
commit detached, and then runs `git rev-parse HEAD` to confirm it got what it asked
for. The image is tagged with the full SHA, and the app receives it as `APP_VERSION`.

**Why:** `main` is a moving name. If a deploy is "whatever `main` was when the clone
ran", two servers, or one server and a rollback, can disagree about what is running.
With a SHA, "what is live?" has one answer, the history means something, and a
rollback goes to a specific build, not "the previous state of a branch".

**Cost:** the caller has to resolve the SHA. The webhook gets it from the push, and
the CLI takes `$(git rev-parse HEAD)`.

## 2. Users never see a version that hasn't answered first

The new container starts beside the old one, on a random port bound to `127.0.0.1`,
and has to return `200` from `/health` **three times in a row** (every 500ms, within
30s) before the proxy hears about it. If the container exits during that wait, the
check fails at once instead of waiting out the deadline.

**Why three in a row:** one `200` proves the process can answer once. An app that
answers and then crashes as its background workers start would pass a single probe.
Three consecutive passes over a second and a half is cheap and filters out most
"up for a moment" failures.

**Why a random loopback port:** two versions must run at the same time, so neither can
own a fixed port. Loopback keeps the unproven version unreachable from outside.

## 3. Check again through the proxy, as users reach it

After Caddy is pointed at the new container, Sling probes the **public** address and
reads the `X-Sling-Upstream` header that Caddy stamps on every proxied response. It
needs three passes that came from the new container.

**Why:** a container that is healthy on its own port proves little about the path
users take. The proxy config may not have applied, the container network may be wrong,
or an old upstream may still be answering. Checking the header means Sling doesn't just
hope the switch happened; it sees the new version answer through the proxy. Caddy adds
the header, so the app doesn't need to cooperate.

## 4. Keep watching for 30 seconds before letting go

After the switch, Sling watches the new version for a 30-second grace window and rolls
back after **three failed probes in a row**. The old container keeps running the whole
time, so a rollback is one proxy update, not a rebuild. Only after the window passes is
the old version stopped, with `docker stop` (SIGTERM, up to 10s to drain), not
`docker rm -f`, so requests in flight finish. Its image is kept, so `sling rollback`
can bring it back later without building.

**Why:** some failures only show up under real traffic or after a delay: a pool that
runs dry, a cache that fills, a worker that crashes on its first job. The recording in
the README shows exactly this case: `fails-later` passes every check, goes live, starts
returning 500 eight seconds later, and is rolled back with no one touching anything.

**Why three in a row and not one:** a single slow response during a deploy is normal.
Rolling back on one blip would make deploys flaky, while three in a row at 500ms is
still a fast reaction to a real failure.

**Cost:** for 30 seconds both versions use memory, and each deploy takes at least that
long. On a small VPS that is the price of an instant rollback.

## 5. One proxy, driven through its API

Traffic switching goes through Caddy's admin API (`POST /load`, on `127.0.0.1:2019`).
Caddy applies a new config without dropping open connections, and it handles TLS: with
`SLING_DOMAIN` set it gets and renews Let's Encrypt certificates itself.

**Why not nginx plus a reload, or iptables:** both work, but each needs config files
on disk, a reload command and its error handling. Caddy's API takes one JSON document
and either applies it whole or rejects it. That is the cleanest switch to reason about,
and the proxy's config always matches what Sling last sent it.

## 6. One deploy at a time, even across processes

Only one deploy or rollback runs per host. That's enforced with an in-process mutex
**and** a kernel file lock (`flock`) on `$XDG_RUNTIME_DIR/sling.lock`, so the webhook
server and a CLI deploy you start by hand can't interleave their proxy switches.

**Why `flock` and not a PID file:** the kernel drops the lock when the holder exits,
even on a crash or `kill -9`. A PID file left behind by a dead process would need
stale-lock heuristics, and those are where lock bugs live.

The webhook queue holds at most one waiting deploy. If three pushes arrive during a
deploy, only the newest is built and the others are recorded as `superseded`. Building
commits nobody will keep is wasted time on a small server.

## 7. History survives a crash, and so does the truth about the crash

Every status change is written to SQLite (`history.db`, mode `0600`, because error
text can include container logs). If Sling is killed mid-deploy, the history would show
that deploy as still running forever. So the next deploy, which holds the lock and
therefore knows nothing else is running, marks any such row `interrupted` and records
the stage it reached.

**Why not a separate cleanup job:** the lock already answers "is anything running?".
Using it to fix the history needs no extra moving parts.

## 8. Run no shell, and trust no input

Every external command (`git`, `docker`) runs through `exec` with an argument list and
a timeout. Nothing goes through a shell, so a repo URL or SHA can't become a command.
SHAs must match 40 lowercase hex characters, repo URLs must be `https` on an allowed
host (local paths only from the CLI), and webhook deliveries need a valid HMAC-SHA256
signature, compared in constant time, and must name the configured repo and branch.

## What it deliberately doesn't do

- **More than one host.** Sling's guarantees come from owning the whole machine: one
  proxy, one lock, one history. Spreading that across hosts needs consensus, which is
  the complexity it exists to avoid.
- **More than one app per host, for now.** Several apps behind one Caddy is on the
  roadmap. The proxy config and lock would become per-app.
- **Database migrations.** For the grace window, two versions run at once against the
  same database. Migrations must be backward compatible (add columns before using them,
  drop them a release later). Sling can't enforce that, so it's the app's job.
- **Hide failures.** A failed deploy leaves the previous version serving and says why,
  in the terminal, in `sling history` and through `GET /deploys`. It never retries on
  its own.

## A bug the recording caught

Making the README recording turned up a real gap. `sling history` showed an empty
ERROR column for rolled-back deploys. The rollback set its status, which saves the
row, before the error that explains it existed, and nothing saved the row again. Now
the deploy runner saves once more after attaching the error, and
`TestRolledBackKeepsErrorInHistory` holds that in place.
