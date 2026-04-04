---
id: hs-vn2b
status: closed
deps: []
links: []
created: 2026-04-03T23:52:54Z
type: task
priority: 2
assignee: Stavros Korokithakis
---
# Make API_PASSWORD required unconditionally

Move the API_PASSWORD env var check out of the shortcuts-only block so it is required at startup regardless of flags. Update the requireAuth function's realm from 'shortcuts' to something more general (e.g. 'havpe'). Remove the conditional around apiPassword that gates it on shortcutsFilePath.

## Acceptance Criteria

Server refuses to start without API_PASSWORD set, even when -shortcuts is not used.

