# Architecture

## What the application does

`havpe-server` is a standalone Go server that replaces Home Assistant as the
voice pipeline backend for the Home Assistant Voice PE (ESPHome) device. It:

1. Connects to the device over the ESPHome native API (TCP port 6053, plaintext
   protobuf framing).
2. Subscribes to voice assistant events and receives raw PCM audio over the API
   connection.
3. Runs Silero VAD (ONNX model) locally to detect end-of-speech.
4. Transcribes the captured audio via the ElevenLabs STT API.
5. Checks the transcript against an ordered list of regex shortcuts; if one
   matches it POSTs to the shortcut's URL and plays a confirmation tone.
6. If no shortcut matches, POSTs the transcript to a configurable webhook and
   plays back the TTS response (synthesized via ElevenLabs TTS).
7. Serves audio files (tone.wav, error.wav, tts.mp3) over HTTP on port 8085 so
   the device can fetch them via its media player.
8. Exposes a small REST CRUD API on the same port for managing shortcuts at
   runtime (optional, enabled with the `-shortcuts` flag).

## Detected stack

- **Language**: Go 1.25 (`go.mod`)
- **Protobuf**: `google.golang.org/protobuf v1.36.11` — generated stubs in
  `api/` from `proto/api.proto` and `proto/api_options.proto`
- **mDNS discovery**: `github.com/hashicorp/mdns v1.0.6`
- **VAD**: `github.com/streamer45/silero-vad-go v0.2.2-*` (CGO, links against
  ONNX Runtime)
- **Build**: `go build` (no Makefile, no task runner)
- **Container**: multi-stage `Dockerfile` (build stage: `golang:1.24-bookworm`;
  runtime stage: `debian:bookworm-slim`)
- **CI/CD**: GitHub Actions (`.github/workflows/docker.yml`) — builds and
  pushes `ghcr.io/skorokithakis/havpe-server` on semver tags

## Conventions

### Formatting and linting
No linter config file is present (no `.golangci.yml`, no `pre-commit` config).
Standard `gofmt` formatting is implied by the Go toolchain.

### Type checking
Go's static type system; no additional type checker configured.

### Testing
- Framework: standard `testing` package, `go test ./...`
- Test files sit alongside the source files they test (`frame_test.go`,
  `pipeline_test.go`, `ping_test.go`), all in `package main`
- Tests use table-driven style and helper constructors (`buildFrame`,
  `marshalAudioChunk`, `makeZeroFrame`)
- Fake `net.Conn` implementations (`nullConn`, `writeCapture`) are defined
  inline in test files to avoid real network I/O

### Documentation
- `README.md` at the root covers prerequisites, configuration, shortcuts,
  running, and the webhook protocol
- All non-trivial functions and every package-level `var`/`const` block carry
  doc comments explaining the *why*, not just the what
- `AGENTS.md` documents the release process (tag + push triggers CI)

## Linting and testing commands

```bash
go test ./...          # from README.md "Running tests" section
```

No single "do everything" aggregator exists. There is no linter configured.

## Project structure hotspots

| Path | Role |
|---|---|
| `main.go` | Entire application: CLI flags, env-var validation, HTTP server, ESPHome handshake, voice pipeline, shortcuts CRUD, TTS/STT calls (~1425 lines) |
| `frame.go` | ESPHome plaintext frame codec (`ReadFrame` / `WriteFrame`) |
| `frame_test.go` | Unit tests for the frame codec |
| `pipeline_test.go` | Unit tests for `handleVoiceAssistantRequest` and `handleVoiceAssistantAudio` |
| `ping_test.go` | Concurrency test for `lockedWriter` + ping goroutine |
| `api/` | Auto-generated protobuf Go stubs (do not edit by hand) |
| `proto/` | Source `.proto` files for the ESPHome native API |
| `Dockerfile` | Multi-stage build; downloads ONNX Runtime and Silero VAD model |
| `.github/workflows/docker.yml` | CI: build + push to GHCR on semver tag |
| `silero_vad.onnx` | Silero VAD v5 model (loaded at runtime from working directory; not embedded) |
| `tone.wav` / `error.wav` | Audio files served to the device; gitignored locally, copied into the Docker image |

## Shortcuts: add / delete / update

Shortcuts are stored as a `[]shortcut` slice (package-level `var shortcuts`)
protected by `shortcutsMutex sync.RWMutex`. Each `shortcut` holds a compiled
`*regexp.Regexp`, a `rawPattern` string (the pattern without any `(?i)` prefix,
used when writing back to disk), and a target URL string.

**Persistence format** (`shortcutsFilePath`, set by `-shortcuts` flag):
```json
[["regex-pattern", "https://target-url"], ...]
```
Patterns are stored without the `(?i)` prefix. All patterns are compiled with
`(?i)` prepended at load time so all shortcuts are always case-insensitive.
`compileShortcutPattern` strips all leading `(?i)` prefixes before compiling,
so files that already contain accumulated prefixes are cleaned up transparently.

**CRUD flow** (all handlers in `main.go`):

| Operation | Handler | Mutex | Persistence |
|---|---|---|---|
| List | `handleGetShortcuts` (GET `/shortcuts`) | `Lock` → `loadShortcuts` | reads from disk on every request |
| Add | `handlePostShortcuts` (POST `/shortcuts`) | `Lock` → `loadShortcuts` → append → `saveShortcuts` | reads then writes disk |
| Replace | `handlePutShortcut` (PUT `/shortcuts/{index}`) | `Lock` → `loadShortcuts` → index replace → `saveShortcuts` | reads then writes disk |
| Remove | `handleDeleteShortcut` (DELETE `/shortcuts/{index}`) | `Lock` → `loadShortcuts` → slice splice → `saveShortcuts` | reads then writes disk |

Every API handler acquires a full `Lock()` (not `RLock`) and calls
`loadShortcuts` from disk before making any change. This means the on-disk file
is the source of truth: external edits to the file are picked up automatically
on the next request. `saveShortcuts` takes the updated slice as a parameter and
writes it back to disk immediately.

**Matching** (`tryShortcut` in `main.go`): called from `runPipelineResponse`
before the webhook. Iterates under `RLock` against the in-memory `shortcuts`
slice; first match wins; POSTs an empty body to the shortcut URL.

## Logging patterns

The codebase uses only the standard library `log` package (imported as `"log"`).
No structured logging, no log levels, no third-party logger.

Patterns in use:
- `log.Printf(...)` — the dominant form; used for all informational and
  diagnostic messages throughout `main.go`
- `log.Println(...)` — used once for the EOF case (`"connection closed by device"`)
- `log.Fatalf(...)` — used at startup for unrecoverable configuration errors
  (missing env vars, VAD init failure, shortcuts file creation failure)
- Every `sendEvent` call logs the event type via `log.Printf("sending event %s", eventType)`
- Errors that are handled locally (not propagated) are logged with
  `log.Printf("<context>: %v", err)` before continuing or returning

No `log.Panicf`, no `log.Print` (without f), no structured fields, no
request-scoped loggers.

## Do and don't patterns

### Do
- **Errors propagated via `fmt.Errorf("context: %w", err)`**: every error is
  wrapped with a short context string before being returned. (`main.go`,
  `frame.go`)
- **`log.Fatalf` only at startup**: unrecoverable configuration errors call
  `log.Fatalf`; all runtime errors are returned and logged by the caller.
  (`main.go` `init`, `main`)
- **Single-write frame assembly**: `WriteFrame` assembles the entire frame into
  one slice and calls `writer.Write` once to prevent interleaving under a mutex.
  (`frame.go`)
- **`sync.RWMutex` for shared mutable state**: `shortcuts` and `ttsAudioBuffer`
  are both protected by dedicated `RWMutex`/`Mutex` variables. (`main.go`)
- **Explicit `defer body.Close()` + drain before close**: HTTP response bodies
  are always drained with `io.Copy(io.Discard, ...)` before `Close()` to allow
  connection reuse. (`main.go` `tryShortcut`)
- **`pipelineState` reset by value assignment**: `*pipeline = pipelineState{}`
  is used to atomically zero all pipeline fields rather than resetting them one
  by one. (`main.go` `handleVoiceAssistantRequest`)

### Don't
- **No broad `recover()` / panic swallowing**: errors propagate naturally; there
  are no `recover` calls anywhere in the codebase.
- **No global HTTP mux with unauthenticated write endpoints**: shortcuts CRUD
  endpoints are only registered when `-shortcuts` is set, and every handler
  calls `requireAuth` before doing anything. (`main.go`)
- **No UDP audio streaming**: the server explicitly requests API audio mode
  (`Flags: 1`, `Port: 0`) rather than the UDP path. (`main.go`
  `handleVoiceAssistantRequest`)
- **No embedded ONNX model**: the model is loaded from the filesystem at
  startup, not embedded with `//go:embed`, so it can be updated without
  recompiling. (`main.go` `init`, `Dockerfile`)

## Open questions

None that materially affect implementation decisions. The codebase is
self-contained and well-commented.
