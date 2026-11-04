# Sling

**Push a commit, and it goes live with no downtime or rolls itself back.**
Sling is a small self-hosted deploy orchestrator written in Go. You give it a git
repo and an exact commit. It builds a Docker image, starts the new version beside
the old one, proves it is healthy, switches a Caddy proxy over to it, and keeps
watching. If anything fails along the way, traffic goes back to the previous version.

```
go run . https://github.com/you/app <40-char-commit-sha>
```

---

## The problem it solves

Deploying to your own server usually looks like this: SSH in, `git pull`,
`docker compose up -d --build`, and hope. That approach has a few problems:

- **Downtime on every deploy.** The old container is stopped before the new one is ready.
- **Broken releases reach users.** Nothing checks that the new version works
  before it gets traffic. A crash on boot or a failing `/health` means an outage.
- **Manual, stressful rollbacks.** When a release is bad, you are back on SSH
  rebuilding the old version while users wait.
- **"What is actually running?"** Branch names move. Without pinning, the
  server can end up on a different commit than the one you meant to ship.

Hosted platforms (Heroku, Render, Railway) solve this, but they cost money and
run your app on their infrastructure. Sling brings the same safety to a single
VPS that you own.

## What Sling guarantees

| Guarantee | How |
|---|---|
| **Builds exactly the commit you asked for** | Only full 40-hex SHAs are accepted. After `git checkout --detach` it compares `git rev-parse HEAD` with the SHA you gave it. |
| **No downtime** | The new container runs beside the old one. The proxy switches only after the new one passes its health checks. |
| **A broken build never gets traffic** | It needs 3 healthy `/health` responses in a row from the container directly. If the container crashes, the check fails at once. |
| **It checks the new version is the one answering** | After the switch it probes through the proxy and requires the `X-Sling-Upstream` header to name the *new* container, not just "something answered 200". |
| **Automatic rollback** | If the post-switch check or the 30s grace window fails (3 probes in a row), the proxy is pointed back at the previous version. |
| **Old connections finish cleanly** | The previous version is stopped with `docker stop` (SIGTERM, 10s to finish), not killed, and is kept so a manual rollback is just start + switch. |
| **Resource limits** | Each app gets 256 MB of memory and 0.5 CPU, and its port is reachable only from the server itself (loopback). |
| **One deploy at a time** | A lock stops two deploys from fighting over the proxy. |
| **Live commit is not redeployed** | Redeploying the commit that is already live is skipped before any container is touched. |
| **Only signed pushes deploy** | Webhook deliveries must carry a valid HMAC-SHA256 signature, and come from the configured repo and branch. The clone URL comes from Sling's own config, never from the payload. |
| **Bursts of pushes don't pile up** | Deploys run one at a time with at most one waiting. A newer push replaces the waiting one. |

## Who it is for

- **Solo developers and small teams** with one VPS who want safe, zero-downtime
  deploys without the bulk of a full self-hosted PaaS.
- **Learners** who want to see how blue/green deploys, health gating and
  rollback actually work. The codebase is small and heavily commented.

### How it compares

| | Sling | Kamal | Coolify / Dokploy | Dokku |
|---|---|---|---|---|
| Footprint | one Go binary + Caddy | Ruby gem + kamal-proxy | dashboard + DB + workers | Bash + nginx plugins |
| Confirms the new version answers through the proxy | ✅ | partial | — | — |
| Automatic rollback during a grace window | ✅ | — | — | — |
| Web UI, TLS, multiple apps | roadmap | ✅ | ✅ | ✅ |

Sling's aim is to be **the smallest tool with the strongest rollback guarantees**,
not the one with the most features.

---

## High-level design (HLD)

```mermaid
flowchart LR
    dev([Developer]) -->|"sling {repo} {sha}"| sling
    gh([GitHub push webhook]) -->|"POST /webhook<br/>signed with HMAC"| sling

    subgraph host["Single host"]
        sling["Sling orchestrator<br/>(CLI or sling serve)"]

        subgraph docker["Docker"]
            subgraph net["sling-net (bridge network)"]
                caddy["sling-caddy<br/>Caddy reverse proxy"]
                blue["app-{old sha}<br/>(previous / blue)"]
                green["app-{new sha}<br/>(candidate / green)"]
            end
        end

        tmp[("$TMPDIR/paas-builds/{sha}<br/>build workspace")]
    end

    git[("Git host<br/>github.com")]
    users([Users])

    sling -->|git clone + checkout| git
    git --> tmp
    sling -->|docker build / run / stop| docker
    sling -->|"admin API :2019<br/>POST /load, GET /config"| caddy
    sling -.->|"direct health probes<br/>127.0.0.1:{random port}/health"| green
    sling -.->|"proxy probes<br/>127.0.0.1:8080/health"| caddy

    users -->|"127.0.0.1:8080"| caddy
    caddy -->|"before cutover"| blue
    caddy ==>|"after cutover"| green
```

**Components**

- **Orchestrator** (`main.go`): runs the deploy pipeline from start to finish and
  records each status change.
- **Webhook server** (`serve.go`, `webhook.go`): `sling serve` checks each GitHub
  push delivery and hands the commit to the deploy queue.
- **Deploy queue** (`queue.go`): one worker, one waiting slot. The latest push wins,
  and a deploy that is running always finishes, even during shutdown.
- **Proxy control** (`proxy.go`): makes sure `sling-net` and the `sling-caddy`
  container exist. It builds the Caddy JSON config and swaps the upstream
  in one step through the admin API.
- **Health gate** (`health.go`): `waitUntil` (N passes in a row before a
  deadline) and `watch` (fail after N failures in a row during a window).
- **Status tracking** (`status.go`): a status machine guarded by a mutex, with a
  timeline, ready for the planned dashboard API.

## Low-level design (LLD)

### Webhook flow

```mermaid
sequenceDiagram
    autonumber
    participant GH as GitHub
    participant H as webhookHandler
    participant Q as deployQueue
    participant W as worker
    participant P as runPipeline
    GH->>H: POST /webhook (X-Hub-Signature-256, X-GitHub-Event)
    H->>H: POST only, body ≤ 5 MB
    H->>H: HMAC-SHA256 over the raw body (constant-time compare)
    alt bad or missing signature
        H-->>GH: 401
    end
    H->>H: event: ping → 200 pong, other events → 200 ignored
    H->>H: repo == SLING_REPO (else 403), ref == refs/heads/SLING_BRANCH,<br/>not deleted, after is 40 hex
    H->>Q: submit(Deployment{cloneURL from config, sha})
    Note over Q: same sha already waiting → dropped<br/>another one waiting → marked superseded
    H-->>GH: 202 queued (GitHub gives up after 10 s)
    W->>Q: take()
    W->>P: run the deploy (no local repos allowed)
```

### Deploy sequence

```mermaid
sequenceDiagram
    autonumber
    participant CLI as main()
    participant P as pipeline()
    participant D as Docker
    participant C as Caddy admin API
    participant N as app-{new sha}
    participant O as app-{old sha}

    CLI->>P: runPipeline(d) [acquire deployMu]
    P->>P: validate SHA (40 hex) and repo URL (https, allowed host)
    P->>D: ensureProxy(): network inspect/create, caddy run/start
    P->>C: waitForAdmin()
    P->>C: currentUpstream() → prev
    alt prev == app-{sha}:3000
        P-->>CLI: error "commit already live" (nothing touched)
    end
    P->>P: git clone → checkout --detach SHA → rev-parse HEAD == SHA
    P->>D: docker build -t paas-app:{sha}
    P->>D: docker run -d app-{sha} (sling-net, 127.0.0.1::3000, 256m, 0.5 cpu)
    P->>D: docker port → host port
    loop waitHealthy: every 500ms, until 3× 200 in a row, ≤30s
        P->>D: containerRunning?
        P->>N: GET 127.0.0.1:{port}/health
    end
    P->>C: setUpstream(app-{sha}:3000)  [POST /load]
    loop waitUntil servedBy(new): 3 in a row, ≤10s
        P->>C: GET :8080/health → expect X-Sling-Upstream = new
    end
    loop watch: 30s grace window
        P->>C: GET :8080/health (fail on 3 in a row)
    end
    alt any post-cutover failure
        P->>C: rollback → setUpstream(prev)
        P->>D: docker rm -f app-{sha} (deferred cleanup)
        P-->>CLI: error, status rolled_back
    else healthy through the window
        P->>P: status = live
        P->>D: docker stop -t 10 app-{old sha}  (retirePrevious)
        P-->>CLI: ✅ Live + timeline
    end
```

### Deployment status machine

```mermaid
stateDiagram-v2
    [*] --> queued
    queued --> preparing: deployMu acquired
    queued --> superseded: newer push replaced it
    preparing --> skipped: commit already live
    preparing --> cloning
    cloning --> building
    building --> starting
    starting --> health_checking
    health_checking --> healthy: 3× 200 direct
    healthy --> switching: Caddy /load
    switching --> observing: new upstream confirmed via proxy
    observing --> live: grace window passed
    live --> [*]

    preparing --> failed
    cloning --> failed
    building --> failed
    starting --> failed
    health_checking --> failed: crash / timeout
    switching --> rolled_back: post-cutover check failed
    observing --> rolled_back: 3 probes failed in a row
    failed --> [*]
    rolled_back --> [*]
```

### Code map

```mermaid
classDiagram
    class Deployment {
        +RepoURL string
        +CommitSHA string
        +ImageTag string
        +ContainerID string
        +HostPort string
        +PreviousUpstream string
        +PreviousStopped bool
        -mu sync.Mutex
        -status Status
        -history []StatusChange
        +setStatus(Status)
        +Snapshot() (Status, []StatusChange)
        +printTimeline()
    }
    class main_go {
        runPipeline(d) error
        pipeline(d) error
        rollback(ctx, d, prev, cause) error
        retirePrevious(prev, cur) bool
        previousContainer(prev, cur) (string, bool)
        validateRepoURL(raw) error
        runStreamedCmd() / runCaptureCmd()
    }
    class proxy_go {
        ensureProxy(ctx) error
        ensureNetwork() error
        buildCaddyConfig(upstream) []byte
        setUpstream(ctx, upstream) error
        currentUpstream(ctx) (string, error)
        waitForAdmin(ctx, limit) error
    }
    class health_go {
        healthConfig
        check func(probeResult)(bool,string)
        is200 / servedBy(upstream)
        waitHealthy() / waitUntil()
        watch(ctx, cfg, url, pass, isRunning, dur, max)
        probe(ctx, client, url)
    }
    main_go --> Deployment : updates
    main_go --> proxy_go : cutover / rollback
    main_go --> health_go : gates every step
    health_go ..> proxy_go : upstreamHeader
```

### Key parameters

| Setting | Value | Where |
|---|---|---|
| Direct health check | every 500ms, 3 passes in a row, 30s deadline, 2s per request | `defaultHealthConfig` (health.go) |
| Post-cutover check | every 500ms, 3 passes in a row, 10s deadline | `postCutoverHealth` (proxy.go) |
| Grace window | 30s, roll back after 3 failures in a row | `graceWindow`, `graceMaxFailures` (main.go) |
| Timeouts | clone 1m, git 30s, build 10m, docker 30s | main.go |
| Public proxy | `127.0.0.1:8080` | proxy.go |
| Caddy admin | `127.0.0.1:2019` | proxy.go |
| App container port | `3000` (published on a random loopback port) | proxy.go |

---

## Usage

**Requirements:** Go 1.26+, Docker, and git. Sling pulls `caddy:2.11.7-alpine` on first run.

**What your app needs:**
- a `Dockerfile` at the repo root
- to listen on port `3000`
- `GET /health` returning `200` when ready
- optionally, reading `APP_VERSION`, which Sling sets to the commit SHA

```bash
go run . https://github.com/you/app 0123456789abcdef0123456789abcdef01234567
# or, during development, a local path:
go run . ../sling $(git -C ../sling rev-parse HEAD)
```

A successful run ends with the live container, URLs, the previous version's
state and a timeline of each stage. Deploying the commit that is already live
prints "Nothing to do" and exits 0.

### Deploy on `git push` (webhook server)

```bash
export SLING_WEBHOOK_SECRET=$(openssl rand -hex 32)   # required, at least 16 chars
export SLING_REPO=you/app                              # required, owner/name
export SLING_BRANCH=main                               # default: main
export SLING_LISTEN=127.0.0.1:9000                     # default: 127.0.0.1:9000
go run . serve
```

In the GitHub repo, go to **Settings → Webhooks → Add webhook** and set:
- **Payload URL:** `https://<your-host>/webhook`
- **Content type:** `application/json`
- **Secret:** the same `SLING_WEBHOOK_SECRET`
- **Events:** just the push event

The default listen address is loopback, so GitHub can't reach it directly. Put
a tunnel (cloudflared, ngrok) or a public HTTPS reverse proxy in front of it.

Webhook deploys always clone `https://github.com/$SLING_REPO.git`; local paths
are only accepted from the CLI. The first Ctrl-C (or SIGTERM) stops accepting
webhooks and lets the deploy in progress finish. A second Ctrl-C forces an exit.

To test without GitHub, send a signed push yourself:

```bash
SLING_WEBHOOK_SECRET=... ./scripts/send-webhook.sh you/app <40-char-sha> [branch] [url]
```

The sample app in [`../sling`](../sling) includes `make-cases.sh`, which creates
commits that fail on purpose (`crash`, `bad-health`, `slow-start`) plus two
good versions, to exercise every path above.

## Testing

```bash
go test -race ./...                                        # unit tests (webhook, queue, health, prune)
CADDY=/path/to/caddy go test -tags integration ./...       # against a real Caddy binary
./scripts/run-cases.sh ../sling                            # end to end: deploys every demo case with Docker
```

## Roadmap

- [x] Clone, build and run with exact commit pinning and input validation
- [x] Health gate before any traffic is routed
- [x] Caddy blue/green switch with a check that the new version answers
- [x] Grace window with automatic rollback, and stopping the old version cleanly
- [x] Webhook server (deploy on `git push`): signed deliveries, a queue where the latest push wins, clean shutdown
- [ ] Multiple apps with a lock per app and routing per domain
- [ ] Dashboard and status API (the `Snapshot()` API is already in place)
- [ ] TLS through Caddy's automatic HTTPS
- [x] Cleanup of old containers, images and build folders
- [ ] A `sling rollback` command
