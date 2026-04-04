---
id: hs-0stn
status: closed
deps: [hs-vn2b]
links: []
created: 2026-04-03T23:53:03Z
type: task
priority: 2
assignee: Stavros Korokithakis
---
# Add settings file and STT_LANGUAGE / TTS_SPEED config

Add a -settings CLI flag (default 'settings.json'). Define a settings struct with SttLanguage (string, default 'en') and TtsSpeed (float64, default 1.0). On startup: apply hardcoded defaults, override with env vars STT_LANGUAGE and TTS_SPEED if set, then override with settings file if it exists. If the file doesn't exist, create it with current values. Protect the settings with a mutex for concurrent access.

Change the STT WebSocket URL from a const to a function that builds the URL using the current SttLanguage setting. Change synthesizeSpeech to include the current TtsSpeed in the TTS API payload (the ElevenLabs TTS API accepts a top-level 'speed' float field alongside 'text' and 'model_id' — note the payload map type needs to change from map[string]string to map[string]interface{} or use a struct).

Non-goals: no validation of language codes or speed ranges.

