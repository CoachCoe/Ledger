# Observer latency measurements

The committed JSON files record local runs of the production daemon, made before the harness was ported to Go with the same method. Both runs use ten detached workspaces of [NestJS](https://github.com/nestjs/nest) at `6e83cc83c99cbac0108384392902e7230ed94843` (2,743 tracked files), with its 1,621-package, approximately 540 MB ignored dependency tree installed in every workspace. These are filesystem observer measurements, not application performance benchmarks.

The harness makes plain file edits independently in each workspace every 1–3 seconds for 60 seconds. Separate clients read cached `workspace.status` over the public Unix API every 100 ms without session ids, so reads cannot trigger observation. It includes the final debounce window and fails on any missed edit, request error, or latency above two seconds. Latencies include observation, API scheduling and reader polling. The default 500 ms debounce and 10-second fallback poll remain enabled.

| Host | Samples | p50 | p95 | Maximum | Missed/errors |
|---|---:|---:|---:|---:|---:|
| macOS arm64, 10 CPUs | 299 | 749.58 ms | 1,249.15 ms | 1,680.72 ms | 0/0 |
| Linux arm64, Docker, 10 CPUs | 299 | 385.82 ms | 601.85 ms | 765.94 ms | 0/0 |

Both runs are on the same Apple Silicon host. macOS uses FSEvents and APFS dependency clones; Linux uses inotify inside a Docker filesystem. The Linux dependency tree was copied from the macOS installation solely to reproduce ignored filesystem load; no platform-specific Node packages were executed. This evidence establishes the M1 target on these environments, not a guarantee for every disk, repository or system load. Native race tests separately exercise poll-only observation and stopped-daemon edits.

## Reproduce

Build Ledger with Go 1.27.3 (cgo enabled on macOS). Use a fresh disposable repository: the harness creates root and session keys under a temporary configuration directory, initializes Ledger, creates signed intents and workspaces, copies dependencies, edits files, and leaves its records and results available for inspection. It stops its daemon on exit.

```sh
go build -o /tmp/ledger ./cmd/ledger
go build -o /tmp/observerbench ./tools/observerbench
git clone https://github.com/nestjs/nest.git /tmp/ledger-observer-nest
git -C /tmp/ledger-observer-nest checkout 6e83cc83c99cbac0108384392902e7230ed94843
cd /tmp/ledger-observer-nest
npm ci --legacy-peer-deps --ignore-scripts --no-audit --no-fund
/tmp/observerbench -seconds 60 /tmp/ledger /tmp/ledger-observer-nest
```

The harness uses only the public CLI and socket API and writes `.ledger/observer-benchmark.json` in that disposable repository. `--legacy-peer-deps` is needed by this pinned fixture's dependency graph. The application does not depend on Node or these packages.
