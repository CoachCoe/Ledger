---
inclusion: always
---

# Tool usage: avoid permission prompts

The user runs agents unattended. Any shell command that does not match the allow rules in `~/.kiro/settings/permissions.yaml` stops the run until the user approves it by hand. Follow these rules so work proceeds without prompts.

## Files

- Read files with the file-reading tool and search with the search tools. Do not use `cat`, `head`, `tail`, `grep`, `find` or `ls` in the shell for this.
- Edit files only with the edit and write tools (`str_replace`, `fs_write`, `fs_append`). Never edit through the shell: no `sed -i`, `awk`, `perl -i`, `tee`, heredocs or `>` / `>>` redirection.

## Shell

- Run one plain command per call, for example `go test -race ./...`, `golangci-lint run`, `make check` or `git commit -m "..."`.
- Set the directory with the tool's `cwd` parameter. Do not start commands with `cd`.
- Do not chain or wrap commands: no `&&`, `||`, `;`, pipes (`|`), `2>&1`, subshells `$(...)`, `{ ...; }` groups, or `for` / `if` / `while` loops.
- Do not prefix commands with environment variables (`FOO=bar go test`). If a test needs an environment variable, set it in the test code or a Makefile target.
- Call tools by their bare name (`go`, `git`, `make`, `golangci-lint`, `govulncheck`, `bash demos/m3.sh`), not by absolute path.
- If you need several commands, make several separate tool calls.
- If output is long, let the tool save it rather than piping it through `tail` or `head`.
