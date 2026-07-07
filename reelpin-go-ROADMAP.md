# ReelPin - Backend + DevOps Master Roadmap (hour-budgeted)

Companion to `reelpin-go-DESIGN.md` (architecture decisions). This is the plan you
hold yourself to: every phase has an hour budget, in the order you should do them.

## The contract

- You write 100% of the code. I explain, give skeletons with `// TODO` gaps, review.
- Pace: 10-15 focused hrs/week (plan assumes ~12.5 avg).
- This is a live production service, treated like an internship. Real users depend
  on the Python backend today; the Go one replaces it.
- Two goals: (1) ship the Go backend ASAP, (2) skill into DevOps and pass CKA.

## Total commitment (honest numbers)

| Track | Hours |
|-------|------:|
| A. Backend build (Go, to production-ready) | 207 |
| B. DevOps / CKA (Linux -> Docker -> K8s -> exam) | 135 |
| C. Integration (Elasticsearch, K8s deploy, CI/CD, prod monitoring) | 82 |
| **Total** | **424** |

At 12.5 hrs/week that's **~34 weeks (~8 months)**. Range: ~28 weeks at 15 hrs/wk,
~42 weeks at 10 hrs/wk.

Detailed estimation lands higher than my earlier rough "10 weeks" guess. That guess
was just the backend and didn't count you-write-everything overhead or the DevOps
track. This number is the real one. Don't be discouraged by 8 months, that's a
serious, resume-changing amount of skill acquired while shipping a real product.

**Key milestone: first live backend at ~140 hrs (~week 11, ~2.5 months).** That's
your "shipped ASAP" moment, after which everything else improves a system that's
already in production.

## The order (this is the part that matters)

```
1. Backend Phases 0-3   -> a backend that processes reels        (140h, ship it live)
2. Backend Phases 4-6   -> search, live progress, observability   (67h, production-ready)
3. DevOps Track B       -> Linux, Docker deep, K8s, CKA           (135h, get certified)
4. Integration Track C  -> ES, migrate onto K8s, CI/CD, monitoring (82h, full devops)
```

Ship first. Skill into DevOps second, using your shipped backend as the lab. Do NOT
front-load Linux/Docker/K8s, it delays shipping by months and you learn K8s better
with a real workload to run.

Exception: you deploy a trivial version early (end of Phase 1) just to build the
deploy habit. "Deploy on day one, even if it does nothing" is a real best practice.

---

## TRACK A - Backend build (207h)

### Phase 0 - Go fundamentals (30h)

| Task | Hours |
|------|------:|
| Tour of Go (syntax, types, structs, slices, maps) | 12 |
| Errors as values, error wrapping, interfaces | 6 |
| Goroutines + channels (intro only) | 4 |
| Project layout, modules, tooling (fmt/vet/test) | 3 |
| Throwaway in-memory CRUD to practice | 5 |

Done: you add an endpoint, encode/decode JSON, handle errors, explain `context`.

### Phase 1 - API + Postgres + job CRUD (22h)

| Task | Hours |
|------|------:|
| pgx (pool, queries) + goose migrations | 5 |
| Config from env, dependency injection, validation | 3 |
| Build job + reel endpoints, source-identity normalizer | 10 |
| Table-driven tests + docker-compose (Postgres) | 4 |

Done: create/read jobs and reels against real Postgres, tests green.
**+ deploy this trivial version to the droplet once, to build the pipeline.**

### Phase 2 - RabbitMQ + worker + concurrency (42h) - the core

| Task | Hours |
|------|------:|
| Queue concepts (ack/nack, prefetch, DLX, routing) | 6 |
| Go concurrency deep (goroutines, channels, select, context, WaitGroup) | 10 |
| RabbitMQ topology + publish/consume (amqp091-go) | 8 |
| Claim job in Postgres, ack/nack, retry, dead-letter | 8 |
| Graceful shutdown + stale-claim recovery + sweep | 5 |
| Race detector, debugging concurrency bugs | 5 |

Done: enqueue -> worker claims -> processes (stubbed) -> ack; force failure ->
retry -> dead-letter; kill worker mid-job -> recovers. Hardest phase.

### Phase 3 - Processing pipeline (34h)

| Task | Hours |
|------|------:|
| HTTP client, timeouts, retries, subprocess (ffmpeg) | 5 |
| Instagram downloader + cookie slots + Apify fallback (the hard part) | 12 |
| ffmpeg audio extract + Gemini transcription | 5 |
| Gemini extraction + categorization | 6 |
| Processing cache + failure classification + retry policy | 6 |

Done: a real Instagram URL goes queued -> processing -> completed with a saved reel.

**>>> SHIP HERE (~140h cumulative). Backend is live and useful. <<<**

### Phase 4 - Real search (33h) - the flagship

| Task | Hours |
|------|------:|
| Embeddings + vector similarity concepts | 4 |
| pgvector + Postgres FTS (tsvector) | 6 |
| Real Gemini embeddings + corpus backfill | 8 |
| Hybrid retrieval, RRF fusion, rerank, match_reasons | 12 |
| Evaluation harness (P@5, Recall@10, MRR, latency) | 8 |

Done: relevant search results, eval numbers beat old lexical search, before/after
shown. (Elasticsearch comes later in Track C; pgvector ships first.)

### Phase 5 - Redis + SSE progress streaming (15h)

| Task | Hours |
|------|------:|
| Redis types + go-redis | 4 |
| Redis streams + SSE endpoint | 6 |
| Rate limits, search cache, heartbeat, idempotency keys | 5 |

Done: live progress stream instead of polling.

### Phase 6 - Observability + reliability (19h)

| Task | Hours |
|------|------:|
| Prometheus metrics (client_golang) | 6 |
| Structured logging (slog) | 3 |
| Grafana dashboard | 4 |
| Reliability hardening (timeouts, cooldowns, shutdown, health) | 6 |

Done: dashboard showing throughput + search latency; every external call has a
timeout. **Backend is production-ready (~207h cumulative).**

---

## TRACK B - DevOps / CKA (135h)

Start this only after the backend is production-ready. CKA prereq order is correct:
Linux -> Docker -> Kubernetes -> exam. You now have a real app to containerize and
run, which makes every lesson concrete.

### B1 - Linux fundamentals (25h)

| Task | Hours |
|------|------:|
| Filesystem, permissions, users/groups, processes | 6 |
| Shell (bash), pipes, grep/sed/awk, scripting | 6 |
| systemd, journald, services (you already touch this on the droplet) | 4 |
| Networking (ports, iptables basics, DNS, ss/netstat) | 5 |
| Package mgmt, cron, logs, troubleshooting | 4 |

CKA is a hands-on Linux exam as much as a K8s one. This is not optional.

### B2 - Docker deep-dive (18h)

| Task | Hours |
|------|------:|
| Images, layers, Dockerfile, multi-stage builds | 5 |
| Volumes, networks, docker-compose (you've used it; go deeper) | 4 |
| Registries, tagging, image scanning, slimming images | 4 |
| Containerize the Go backend properly (multi-stage, distroless) | 5 |

### B3 - Kubernetes core / CKA syllabus (70h)

| Task | Hours |
|------|------:|
| Architecture (control plane, nodes, etcd, kubelet, API server) | 6 |
| Pods, ReplicaSets, Deployments, rollouts/rollbacks | 8 |
| Services, Ingress, networking, DNS | 8 |
| ConfigMaps, Secrets, env, volumes, PV/PVC, storage classes | 8 |
| Scheduling (taints, tolerations, affinity, resource limits) | 6 |
| RBAC, service accounts, security contexts | 6 |
| Cluster setup with kubeadm, upgrades, etcd backup/restore | 10 |
| Troubleshooting (nodes, pods, networking, logs) - heavily tested | 10 |
| kubectl fluency + speed (the exam is time-pressured) | 8 |

### B4 - CKA exam prep (22h)

| Task | Hours |
|------|------:|
| killer.sh simulator (comes with exam, 2 sessions) | 8 |
| Mock exams + timed practice | 10 |
| Weak-area review, book the exam, sit it | 4 |

Done: **CKA passed** (funded by your Lift scholarship).

---

## TRACK C - Integration: make it real DevOps (82h)

Now you take the live backend and run it the way a company would. This is where
Tracks A and B combine, and it's the strongest part of the resume.

### C1 - Elasticsearch (28h)

| Task | Hours |
|------|------:|
| ES concepts (inverted index, analyzers, mappings, relevance/BM25) | 6 |
| Run ES, index reels, query DSL | 6 |
| Integrate as a search backend, compare to pgvector+FTS on your eval set | 10 |
| Decide the final search architecture with numbers (ES vs pgvector vs both) | 6 |

Honest note: ES may or may not beat pgvector+FTS on your data. That's fine, the
point is you evaluated it with your eval harness and can defend the decision. That
is exactly what senior engineers do, and it's a great interview story either way.

### C2 - Kubernetes deployment of the backend (20h)

| Task | Hours |
|------|------:|
| Manifests: Deployments (API, worker), Services, Ingress | 6 |
| Secrets/ConfigMaps, resource requests/limits, HPA (autoscaling) | 6 |
| Run RabbitMQ, Redis, Postgres, ES (operators or managed) | 5 |
| Package with Helm | 3 |

### C3 - CI/CD (12h)

| Task | Hours |
|------|------:|
| GitHub Actions: build, test with -race, lint (golangci-lint), image scan | 6 |
| Build + push image, deploy to K8s on merge | 6 |

### C4 - Production monitoring + load (12h)

| Task | Hours |
|------|------:|
| Prometheus + Grafana in-cluster, alerts | 6 |
| Load test (k6), tune concurrency/limits, observe under load | 6 |

Done: the backend runs on Kubernetes you manage, deploys via CI/CD, is monitored,
and you've load-tested it. Full-loop DevOps on a real production service.

---

## Week-by-week schedule (the anti-deviation calendar)

At ~12.5 hrs/week. Adjust the calendar, not the hours.

| Weeks | Focus | Cumulative hrs |
|-------|-------|---------------:|
| 1-3   | Phase 0 Go fundamentals | 30 |
| 4-5   | Phase 1 API + Postgres + CRUD (+ trivial deploy) | 52 |
| 6-8   | Phase 2 RabbitMQ + worker + concurrency | 94 |
| 9-11  | Phase 3 pipeline -> **SHIP LIVE** | 128-140 |
| 12-14 | Phase 4 real search + eval | 173 |
| 15-16 | Phase 5 Redis + SSE | 188 |
| 17-18 | Phase 6 observability -> **production-ready** | 207 |
| 19-20 | B1 Linux | 232 |
| 21-22 | B2 Docker deep | 250 |
| 23-28 | B3 Kubernetes core | 320 |
| 29-30 | B4 CKA prep -> **CKA passed** | 342 |
| 31-32 | C1 Elasticsearch | 370 |
| 33-34 | C2 K8s deploy + C3 CI/CD + C4 monitoring | 424 |

**First live backend: ~week 11. Production-ready: ~week 18. CKA: ~week 30.
Full DevOps loop: ~week 34.**

---

## Rules to not deviate

1. One phase at a time, in order. Don't jump ahead to K8s because it's shiny.
2. Log hours per session in `LEARNING.md`. If you're consistently over budget on a
   phase, tell me, we adjust the plan, we don't silently fall behind.
3. Every phase ends runnable. No "I'll make it work later."
4. You write all the code. When it's hard, we slow down, we don't skip.
5. Deploy early, deploy often. The trivial deploy in week 5 is on purpose.
6. When the plan meets reality (the Instagram downloader will fight you, K8s
   networking will confuse you), the hours flex first, the order stays.
