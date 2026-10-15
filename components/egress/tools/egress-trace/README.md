# egress-trace

A read-only viewer for how the OpenSandbox egress sidecar admits outbound traffic. Runs inside the egress container; ships with the image (`/usr/local/bin/egress-trace`). No sidecar configuration or code changes needed.

## What it shows

```text
12:00:25.100 POLICY mode=allowlist | default=deny rules=2
         allow *.github.com
         allow pypi.org
12:00:25.400 LEASE+ set=allow_v4      140.82.121.6                     dns-learned ttl=58s
12:00:25.700 FLOW   140.82.121.6:443 ESTABLISHED admitted via allow set (ttl 58s)
12:01:25.400 LEASE- set=allow_v4      140.82.121.6
```

Three local signals, polled and merged:

| Source | Meaning |
|---|---|
| `GET /policy` | Current policy, mode, enforcement mode; reprinted on change |
| `nft -j list ruleset` | `opensandbox` table: static sets + DNS-learned leases appearing/expiring live (with TTL) |
| `/proc/net/tcp{,6}` | Real outbound connections (non-loopback), annotated `static allow` / `dns-learned lease` / unmatched |

Not shown on purpose: decrypted L7 request contents. `mitmdump` runs with `flow_detail=0` and its output is filtered, so request URLs/statuses never reach any log.

## Usage

```bash
# live trace until Ctrl-C (auth token is read from the container env automatically)
docker exec -it <egress-container> egress-trace

# resolve one domain through the sidecar DNS proxy + report the policy verdict
docker exec -it <egress-container> egress-trace probe api.github.com

# faster polling
docker exec -it <egress-container> egress-trace -interval 250ms
```

Flags: `-addr` (policy API, default `http://127.0.0.1:18080`, env `OPENSANDBOX_EGRESS_HTTP_ADDR`), `-nft`, `-interval`, `-proc`.

## probe output

```text
policy verdict : ALLOW (matched: *.github.com)
dns resolution : 140.82.121.6, 140.82.121.7
outcome        : resolved; expect an nft lease for these IPs and an admitted dial
```

`NXDOMAIN` + verdict `DENY` means the DNS layer dropped it; a resolver error is an upstream failure, distinct from a policy denial.

## Build

Part of the egress Go module, stdlib only. Rebuild after egress image builds, or standalone:

```bash
cd components/egress
GOOS=linux GOARCH=amd64 go build -o egress-trace ./tools/egress-trace
docker cp egress-trace <egress-container>:/usr/local/bin/
```
