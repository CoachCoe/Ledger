# M4 analyzer quality review

Measured on macOS arm64, Apple M1 Max, Go 1.27.3, on 2026-11-07. Each source contains distinct flat TypeScript functions. This is a single-iteration diagnostic comparison, not a release performance guarantee or end-to-end observer measurement.

```sh
go test ./internal/symbols -run '^$' -bench BenchmarkAnalyzeLargeFile -benchtime=1x
```

| Declarations | Before review | After review |
|---|---:|---:|
| 100 | 7.0 ms | 4.0 ms |
| 1,000 | 281.6 ms | 32.3 ms |
| 5,000 | 6,747.9 ms | 175.1 ms |

The earlier analyzer scanned every declaration when masking children and attributing each reference. Direct syntax-tree ownership and direct-child lists remove those repeated whole-file scans. The 5,000-declaration sample improved by about 38 times. Lexical binding analysis and all correctness regression tests remain enabled.
