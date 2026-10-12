---
title: OSEP-0023 HTTP/2 integration
description: Approved experimental HTTP/2 admission and drain contract, implementation plan, and verification boundaries.
---

# OSEP-0023 HTTP/2 integration

Status: staged delivery of experimental H2 admission and bounded-drain wiring,
accepted on 2026-10-12. P1 direct/lazy wiring and independent review completed;
15 manual runtime cases passed on the first tested tree. Go held-old retry
acceptance failed; a separate early-completing-old case passed on the follow-up
tree. Full H2-A5, P2/P3 and Linux acceptance remain open. This page records the
experimental sidecar contract, not production readiness or deployment approval.
Public mode, Fast Sandbox, HTTP/3, CONNECT and cross-SNI coalescing remain out of
scope.

## Decisions and ownership

The 2026-10-12 design accepts delayed GOAWAY (D1), a first retry acceptance scope
of Go `net/http` synthetic GET only (D2), and a narrow exceptional
mitmdump process-exit fallback (D3). D2 initially selected Go 1.25.9; the user
subsequently approved upgrading the MBA's Go to the latest official stable
release instead. Each retry run must require and record the exact resulting
toolchain version, with no fallback to the old installation. This changes the
selected client version, not the GET-only scope. The verified selected MBA
toolchain is Go 1.27.2 on darwin/arm64; its held-old retry result is described
below and must not be generalized to other client versions or configurations.
The existing `LiveReceiver` →
`RevisionPublisher` → registry/receiver remains the only publication owner and
keeps Registry → Receiver lock order. The authenticated IPC protocol and exact
receiver type are unchanged. No dependency fork, global replacement, raw GOAWAY,
closed-state reset, or request replay is permitted.

The egress image explicitly installs `mitmproxy==11.0.2`, `h2==4.1.0` and
`hyperframe==6.0.1`. After copying the addon, its build runs the same exact-version
gate as the runtime adapter and fails on a missing or mismatched dependency.
Mitmproxy's transitive version ranges alone are not the image's compatibility
contract. Changing these pins requires a new adapter compatibility review;
the runtime gate must not be widened to accommodate an unverified image.

A request is admitted only when the system `requestheaders` hook obtains its
unique handle and immutable snapshot. Parsing HEADERS does not admit it.
The registry atomically records the actual inbound client stream ID and its
connection's monotonic high-water mark. Finished high IDs do not reduce that
mark. The first host removal freezes the upper bound, including zero when no
request has been admitted; repeated publication or remove/re-add cannot reopen
or raise it.

## Normal retirement and failure

1. Before mutation ACK, publication installs the snapshot, new-request fence,
   frozen client-stream upper bound and first retirement deadlines atomically.
2. Valid requests refused at requestheaders receive REFUSED_STREAM and never
   reach the origin. Previously admitted streams keep their own snapshot.
3. Once old streams and their local downstream/upstream output obligations are
   terminal, ordinary h2 APIs send one NO_ERROR GOAWAY with the frozen upper
   bound, followed by close. ACK does not wait for this drain.
4. Expiry force-closes the whole connection and can interrupt newer streams
   after a still-covered-host rotation. That is not graceful completion.

Delayed GOAWAY does not guarantee immediate library-driven migration while an
old response remains unfinished. In the selected Go 1.27.2 configuration,
REFUSED_STREAM retries can select the same cached connection until it is
retired. A new request can exhaust its deadline without reaching the origin.
The proxy does not replay it or change error codes to influence the pool.

The fixed adapter observes original pseudo-headers before transparent rewriting
and requires HTTPS, a unique ASCII authority with port 443, the actual target
port 443, optional consistent single Host, and agreement with connection/token
SNI. It cannot use rewritten scheme/host as original identity. A malformed or
missing association fails closed before credential injection.

Normal refusal consumes only a precisely marked kill/NO_RESPONSE path. A
pre-hook 400/413 branch, parser failure, or missing identity must not be relabeled
as retryable refusal. Pending identity storage is bounded by the request budget;
if paused hooks exhaust that association budget, the connection closes
abnormally rather than retaining an extra identity or falsely labeling an
unparsed request REFUSED. Ordinary registry-capacity denial still uses the
requestheaders refusal path.

Response/error hooks record application completion; they do not free an H2
handle while DATA, trailers, a queued upstream request or upload remains. The
adapter reads the dependency's actual upstream child and client/upstream ID
mapping; it does not rewrite those queues or force H1 upstreams. Reset-stranded
trailers wait only for an already registered first deadline, or close abnormally
immediately if no deadline exists. Early response plus unfinished upload plus
client reset can orphan upstream work; that closes the client and verified owned
upstreams immediately, even without buffered DATA or a revision change.

## Exceptional process exit (D3)

Only the following conjunction permits hard exit of the current mitmdump:

- Exact live runtime, tracked client/token, adapter, registered handler and
  actual HttpLayer-owned upstream/task identities still match on the owner loop.
- That exact task was captured before a normal close was requested, and the
  ordinary close command or same handler's synchronous close method was
  successfully consumed.
- The task has ended, but the CURRENT transport entry still identifies that
  task and an actual TCP StreamWriter that is not closing; the connection is
  CLOSED. An old pre-connect IO entry is not evidence about the current writer.
- The single runtime's once-only escalation flag has not been set.

Set the flag before `os._exit(86)`, with no logging, IPC, network operation or
cleanup hook before exit. Do not signal the Go owner. Normal RST, refusal,
pending task, canceled pre-connect task with no IO, closing writer, stale
identity or unknown ownership does not satisfy the fatal predicate. Nonfatal
also does not mean safe to release a handle.

Client disconnect immediately fences admission but retains exact cleanup
ownership until the related upstreams are verified terminal. Task callbacks
must survive canceled wakeups and run while actual handler registration remains
valid. Normal safe cleanup requires exact task/IO observations and writer-close
confirmation, not only a disconnected hook or CLOSED flag. The verified
ancestry is the shipped direct/lazy connection strategy. Configured chained
upstream proxies are rejected for this H2 path before adapter installation;
their tunnel/physical-transport ownership needs a separate proof. Existing H1
and legacy behavior is unchanged. Changed addon connection shapes must not
inherit this proof.

An actual unexpected owned-child exit is reaped by the existing Go generation
owner, which retains its session/readiness/recovery responsibilities. In the
experimental nft sidecar, recovery attempts namespace quarantine. This may stop
all sandbox networking, including loopback, DNS and control/health connections;
recovery requires destroying and recreating the sandbox. This availability cost
was accepted for this exception only. `QuarantineUnknown`, an exit code or a
503 is not confirmed packet isolation. No guarantee covers arbitrary addon
descendants/inherited FDs, origin business completion, replay safety, or a strict
total reap deadline. See [fault containment](/architecture/network/egress#experimental-runtime-fault-containment).

## Implementation and acceptance plan

The active write owners are registry/publication, fixed-version instance adapter,
LiveRuntime lifecycle, and system hook/identity integration. Tests accompany each
slice; there is no second revision owner or general transport framework.

| Stage | Required evidence | Current status |
| --- | --- | --- |
| P0 | Raw identity, exact hook admission seam, refusal/400/413/cleanup, upstream mapping/queues and termination interfaces | Old STOP retained; targeted D3 amendment independently passed on 2026-10-12 |
| P1 | Atomic registry races/rollback, actual runtime/hook wiring, snapshot identity, output-obligation lifecycle and bounded cleanup | Direct/lazy scope passed socket-free tests and independent review on 2026-10-12 |
| P2 | Actual production addon with authenticated Unix IPC, both cutover orders, rotation/removal, reset/disconnect and short/default deadlines | First tested tree: 15 loopback manual-peer cases passed. Follow-up tree: those 15 NOT_RUN; broader matrix remains incomplete |
| P3 | Fixed Go GET retry observations and approved Linux transparent 443 path, H1/policy/credential regressions, independent review | Go 1.27.2 held-old case failed on both tested trees; bounded-old follow-up passed only its narrower scenario; Linux path missing |

Stable coverage denominator:

- H2-A0: H1 regression, actual identity chain, invalid/missing adapter denies.
- H2-A1: normal post-fence REFUSED with zero origin handling; fragmented headers,
  same-batch DATA/RST, HPACK and flow-control credit.
- H2-A2: complete multi-stream output precedes the sole frozen-bound GOAWAY/EOF;
  unfinished upstream upload cannot be mistaken for completion.
- H2-A3: real IPC races, credential/path/policy rotation, retry commit and
  remove/re-add preserve snapshots and first deadlines.
- H2-A4: idempotent reset/error/disconnect, stranded trailers, orphan upload,
  actual default 30-second expiry and resource release; abnormal is not graceful.
- H2-A5: The explicitly selected Go version's configured `net/http` GET retry
  must independently pass its successful-migration acceptance without manual
  resends; logical requests reach origin at most once and old streams survive.
  This gate is not satisfied by recording observations alone. The held-old
  success case remains FAIL; the narrower bounded-old PASS does not complete
  H2-A5 or replace that failure.

Evidence records exact commit/tree, dependency and environment identities and
marks each required check fresh/reused/missing/blocked separately from pass/fail.
Pure layer/synthetic transport tests do not prove real socket/TLS/IPC behavior.
The P1 test denominator is 241 socket-free checks: 108 registry/publication,
32 adapter, 28 lifecycle, 15 system-hook and 58 existing H1-related checks.
They pass both in separate processes and together in CI-style alphabetical
order. Two additional module-isolation regressions check restoration of the
legacy fake test context without replacing real H2 dependency identities.
Independent registry review additionally exercised 38,400 deterministic model
transitions; lifecycle review independently checked failed-connect and
established-TCP/failed-TLS ordering. These numbers are not runtime-case counts.
The old W2 run remains single-scenario feasibility evidence only; it does not
establish new production acceptance. MBA loopback tests cannot replace Linux
transparent destination 443, restricted user, IPv4/IPv6 and quarantine readback.
The production port gate must not be relaxed to accommodate a test fixture.

The P2 loopback topology uses the actual system addon and a SOCKS5
connection whose real destination is `127.0.0.1:443`, followed by TLS with the
bound synthetic SNI. This exercises the proven direct/lazy child chain without
rewriting a high destination port. It is not a transparent-redirection test.
Execution requires an available, permitted loopback port 443, the pinned
mitmproxy environment, authenticated AF_UNIX IPC and permission for the isolated
fault-case mitmdump child to exit. The Go case additionally requires an explicit
expected toolchain version matching the actual runtime. Missing capabilities
are blocked or missing evidence, never successful skips. The test observer may
read bounded runtime metadata and hold
the specified asynchronous fault hook; it must not replace production owners,
mutate protocol queues or rewrite destinations.

### Running the bounded loopback suite

The explicit runner is
`components/egress/tests/test_credential_bound_h2_runtime.py`. Ordinary unittest
discovery does not start these network cases. `--list` reports the static
denominator without network operations; execution requires
`--run-real-runtime --artifacts NEW_DIRECTORY` with the pinned venv's Python.
Use a fresh artifact directory and the exact reviewed checkout. Occupied or
denied port 443 is blocked, with no alternative-port or destination rewrite.

The first batch contains 15 manual-peer cases: attachment/injection/redaction,
removal with two backpressured streams, rotation, remove/re-add, reset isolation
and disconnect, each with genuine H1 and H2 upstreams; short expiry, configured
default 30-second expiry and the isolated D3 child-exit case add three more.
The separate Go cases are selected by supplying both `--go-client BINARY` and
`--require-go-version go1.27.2`. Build only the standalone
`components/egress/tests/fixtures/h2_retry_client.go` with the independently
verified compiler, `GO111MODULE=off` and `GOTOOLCHAIN=local`; the runner never
installs a toolchain or infers the expected version from the candidate binary.

Reports retain PASS, FAIL, BLOCKED and NOT_RUN separately for all 15 manual
cases and each Go case, even when `--case` selects a smaller subset. A successful
selected batch is not full P2 acceptance. This batch checks post-ACK refusal,
not pre-ACK timing or a suspended-hook commit-before-admission race. Fragmented
HEADERS, upload/queue and stranded-trailer variants, path-only/policy-only
rotation and the Linux checks above remain explicit gaps. Process reap and
origin socket closure do not establish kernel quarantine or origin business
completion.

### Observed Go retry limit and staged delivery

The user-executed MBA run used commit
`38f7dca55139440cc861dcf5e1e25144e84adbaa`, tree
`39f946563ca06ecbcafcf6a919a04e5d88d4acb1`, with Go 1.27.2. All 15 manual
cases passed. The original `go_retry` case remains FAIL: the new logical GET
had one application `Do`, five connection-selection/write callbacks all on
connection 1, and `deadline_exceeded` at its 8-second budget. The origin
handled the old GET once and the new GET zero times; the old 27-byte response
then completed intact. Trace callbacks are not origin-processing counts.

The fixture held the old body until `new_result`, while D1 held normal GOAWAY
until the old response completed. The observed retries therefore exposed the
selected library/configuration's limitation while the old stream remained
occupied. This is not evidence that Go never retries or that a proxy sweep
failed. The original failure short-circuited its later natural runtime-cleanup
assertion; finally cleanup does not retroactively satisfy that assertion.
Go exit cleanup itself closes client connections, so origin closure alone does
not prove that a proxy GOAWAY initiated closure.

The follow-up MBA run used commit
`99c9d1c34f18a494cfa7b1d31771c7e133a706ae`, tree
`0b5e597748ab0815a20f9c696b7e127bc751de1d`, again with Go 1.27.2. Its separate
denominators were 15 manual cases NOT_RUN, held-old `go_retry` FAIL, and
`go_retry_bounded_old` PASS. Overall status remained FAIL, exit 1. These results
do not mean the manual cases were freshly run on the follow-up tree, even
though the production source was unchanged.

The bounded-old harness releases the old body after observing the new GET at
the original connection's post-ACK fence, without waiting for `new_result`.
It retains one `Do`, the 8-second new-request deadline, the same Transport and
production D1 behavior. In this run, release occurred 25.173 ms after the new
command. The old 27-byte HTTP/2 response completed intact; its origin connection
closed naturally, and its tracked runtime/registry/adapter/upstream-task state
was empty before the new request reached origin. Connection-selection IDs for
the new GET were `[1, 1, 2]`; the fresh connection returned a complete 26-byte
HTTP/1.1 200 response. Origin handled that logical GET once, with no deleted
credential. This proves only the observed cross-connection recovery after the
old response completed early, not H2-to-H2 migration, recovery for arbitrary
finite old-response durations, or migration while old remains unfinished.

The repeated held-old case still exhausted its 8-second deadline on connection
IDs `[1, 1, 1, 1, 1]`, with no new origin handling. Both follow-up cases completed
normal Go and mitmdump exits, natural origin closure without timeout, empty
tracked runtime state, and owned-child/socket cleanup. The runner checked
runtime and origin cleanup before its final retry-success assertion; cleanup
success does not make the unsuccessful new GET successful. Neither run captured
Go-peer END_STREAM, GOAWAY or RST frames. The follow-up did not sample
`goaway_sent=true`, and `proxy_first_goaway_proven` remains false: the timeline
does not prove GOAWAY initiated migration or establish its wire ordering.

Run artifacts were retained on the test machine; their private local paths are
not public evidence links. The runner records commit/tree, dependency versions,
logical outcomes and cleanup in `report.json` and per-case metadata. To reproduce
the two-case selection using the independently built fixture and pinned venv:

```sh
GODEBUG= "$PYTHON" components/egress/tests/test_credential_bound_h2_runtime.py \
  --run-real-runtime --artifacts "$OUT/go-retry-followup" \
  --go-client "$OUT/h2_retry_client" --require-go-version go1.27.2 \
  --case go_retry --case go_retry_bounded_old
```

The MBA runs required explicit user-performed administrator execution after an
unprivileged port-443 attempt was blocked. Reproduction requires the same
separately authorized permissions; the command does not bypass that requirement.
It does not cover the production restricted mitmproxy user or any Linux
network/quarantine behavior.

The accepted staged delivery is the experimental admission and bounded-drain
wiring with these limits documented. Complete client-migration acceptance and
the remaining P2/P3 and Linux matrix are follow-up work, not silently removed
requirements. No production change to error codes, connection pooling or
request replay is justified by the bounded-old result.

Rollback is the focused H2 integration commit set, restoring the existing H1
path while retaining historical evidence. Workflow edits, remote publication,
CI triggering, privileged networking and deployment require separate approval.
