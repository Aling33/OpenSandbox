---
title: OSEP-0023 HTTP/2 integration
description: Approved experimental HTTP/2 admission and drain contract, implementation plan, and verification boundaries.
---

# OSEP-0023 HTTP/2 integration

Status: implementation in progress. This page records the approved experimental
sidecar contract, not production readiness or deployment approval. Public mode,
Fast Sandbox, HTTP/3, CONNECT and cross-SNI coalescing remain out of scope.

## Decisions and ownership

The 2026-10-12 design accepts delayed GOAWAY (D1), a first retry acceptance scope
of Go 1.25.9 `net/http` synthetic GET only (D2), and a narrow exceptional
mitmdump process-exit fallback (D3). The existing `LiveReceiver` →
`RevisionPublisher` → registry/receiver remains the only publication owner and
keeps Registry → Receiver lock order. The authenticated IPC protocol and exact
receiver type are unchanged. No dependency fork, global replacement, raw GOAWAY,
closed-state reset, or request replay is permitted.

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
| P1 | Atomic registry races/rollback, actual runtime/hook wiring, snapshot identity, output-obligation lifecycle and bounded cleanup | In progress |
| P2 | Actual production addon with authenticated Unix IPC, both cutover orders, rotation/removal, reset/disconnect and short/default deadlines | Missing; cloud AF_UNIX runtime blocked |
| P3 | Fixed Go GET retry observations and approved Linux transparent 443 path, H1/policy/credential regressions, independent review | Missing |

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
- H2-A5: Go 1.25.9 configured `net/http` GET retry is observed without manual
  resends; logical requests reach origin at most once and old streams survive.

Evidence records exact commit/tree, dependency and environment identities and
marks each required check fresh/reused/missing/blocked separately from pass/fail.
Pure layer/synthetic transport tests do not prove real socket/TLS/IPC behavior.
The old W2 run remains single-scenario feasibility evidence only; it does not
establish new production acceptance. MBA loopback tests cannot replace Linux
transparent destination 443, restricted user, IPv4/IPv6 and quarantine readback.
The production port gate must not be relaxed to accommodate a test fixture.

Rollback is the focused H2 integration commit set, restoring the existing H1
path while retaining historical evidence. Workflow edits, remote publication,
CI triggering, privileged networking and deployment require separate approval.
