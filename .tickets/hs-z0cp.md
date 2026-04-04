---
id: hs-z0cp
status: closed
deps: [hs-z954]
links: []
created: 2026-04-03T23:53:13Z
type: task
priority: 2
assignee: Stavros Korokithakis
---
# Update README for new settings

Update the README to document: (1) API_PASSWORD is now always required, (2) new env vars STT_LANGUAGE and TTS_SPEED with defaults, (3) -settings CLI flag with default, (4) GET/PUT /settings API endpoints, (5) precedence order (settings file > env vars > defaults). Update the configuration table, add a Settings section, update the Docker run examples to include API_PASSWORD unconditionally.

