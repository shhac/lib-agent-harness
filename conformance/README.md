# Conformance recordings

The scenarios here run the library's public API against what an engine's real
CLI said. `go test ./...` replays the recordings under `testdata/` with no
CLI installed, so CI needs nothing set up. A replay checks two things: the
library reads what the CLI really wrote, and the library still sends what the
CLI was sent when the recording was made. A launch that sends something
different, sends less, or is no longer made fails, naming the line.

A passing replay is not verification against the CLI. It only shows the
library still matches what was recorded, on the date and version the
recording's header names. Checking a change against the installed CLI means
recording again.

## Recording

Recording is a manual step that an operator runs, never something CI runs. It performs inference with the
operator's own login and spends what that login spends. fx and agy keep their
state in fixed homes (`~/.fx`, `~/.gemini`), so recording also adds the
scenarios' conversations there.

```sh
go test ./conformance -run 'TestFx' -record=fx
go test ./conformance -run 'TestAntigravity' -record=agy
```

`-record` takes engine names, comma separated. Each scenario runs once
against the installed CLI, asserts what its replay asserts, and replaces
`testdata/<cli>/<version>/<scenario>/`, where the version is the last word of
`<cli> --version`, then replays what it recorded. A new version gets a new directory beside the old one, and
both keep replaying until the old one is removed.

Before committing, read the diff. The recorder names per-run values: the
test's temporary directories, each launch's working directory and the user's
home become `@@NAME@@`. Each engine's `redactions` replace account fields
such as team, balance or e-mail with stand-ins of the same type. A recording
that still holds a credential-shaped token, an e-mail address that is not an
example one, or the user's home is removed and the scenario fails, naming the
line and the kind, never the text. That scan cannot recognise every private
value, such as a login URL or a project name, so the diff is the last check.

## Adding an engine or a scenario

A new engine, or a change to what an engine sends, comes with recordings made
against the CLI it claims to support. Hand-written fixtures in each package
still cover the shapes a real CLI will not produce on demand (malformed
frames, duplicated options, a stubborn agent). Recordings are the baseline
those fixtures are checked against.

A scenario asserts structure, never model wording: statuses, event kinds,
usage, refusal codes, account facts. Effects outside the wire, such as a file
a tool wrote, can only be checked while recording (`tape.Recording()`).
Replays play the CLI's answers back and never run a tool.

Each process launch is one `NNN.jsonl` file, in launch order. Its first line
is a header with the CLI, version, arguments, date and platform. Each line
after it is one of:
- `in`, a line the library sent;
- `out`, a line the CLI wrote;
- `err`, a line on standard error;
- `eof`, input closed;
- `exit`, with the exit code. No `exit` means the library killed the CLI.

Lines that parse as JSON are kept verbatim, and others as `in_text` and
`out_text`.
