---
id: hs-vdd9
status: closed
deps: []
links: []
created: 2026-04-03T23:16:29Z
type: task
priority: 2
assignee: Stavros Korokithakis
---
# Fall back to last partial transcript when committed transcript is empty

In readSTTMessages, track the last partial_transcript text. When a committed_transcript arrives empty, substitute the last partial. The partial_transcript messages have message_type 'partial_transcript' and a 'text' field. Thread this through the existing transcriptChannel — the consumer (waitForTranscript) should not need to change. Only main.go needs editing.

## Acceptance Criteria

Short voice commands that produce partials but empty committed transcripts use the partial text instead of sending empty strings downstream.

