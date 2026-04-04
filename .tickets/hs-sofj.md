---
id: hs-sofj
status: closed
deps: []
links: []
created: 2026-04-04T00:37:32Z
type: task
priority: 2
assignee: Stavros Korokithakis
---
# Skip webhook and play error sound on empty transcript

In runPipelineResponse, after waitForTranscript succeeds (nil error), check if the trimmed transcript is empty. If so, log a warning, send STT_END (with empty text), send ERROR event (code: stt-no-text-recognized), play error.wav via TTS_END, send RUN_END, and return early. This prevents empty transcripts from reaching shortcut matching or the webhook. Place this check immediately after the existing transcript-error block (around line 1796), following the same event pattern used there.

## Acceptance Criteria

1) An empty or whitespace-only transcript from STT causes error.wav to play on the device. 2) No webhook POST or shortcut match is attempted. 3) Proper pipeline events (STT_END, ERROR, TTS_END with error URL, RUN_END) are sent so the device UI resets.

