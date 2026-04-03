---
id: hs-z3f1
status: closed
deps: [hs-aw5v]
links: []
created: 2026-04-03T22:02:53Z
type: chore
priority: 2
assignee: Stavros Korokithakis
---
# Remove dead batch STT code

Clean up code that is no longer used after switching to streaming STT:
- Remove the transcribeAudio() function entirely.
- Remove unused imports that were only needed by transcribeAudio (mime/multipart, net/textproto — verify they are actually unused before removing).
- Keep buildWAV() — it is still used by recording mode.
- Run pre-commit hooks and tests to verify nothing is broken.

