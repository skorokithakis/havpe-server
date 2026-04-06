---
id: hs-rjx6
status: closed
deps: []
links: []
created: 2026-04-06T13:51:49Z
type: task
priority: 2
assignee: Stavros Korokithakis
---
# Add missing STT_VAD_END and STT_END events in recording mode

handleRecordingAudio (main.go:1612) sends RUN_END without first sending STT_VAD_END and STT_END. Every other pipeline teardown path sends these events. Add them before the RUN_END at line 1612, matching the pattern used in runPipelineResponse (line 1772+). Non-goal: do not change anything about the VAD detector lifecycle or the normal pipeline path.

## Acceptance Criteria

Recording mode session teardown sends STT_VAD_END and STT_END before RUN_END.

