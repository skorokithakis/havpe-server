# Project overview

havpe-server is a standalone Go server that replaces Home Assistant as the voice pipeline backend for the Home Assistant Voice PE device. It connects to the device over the ESPHome native API (TCP port 6053, plaintext), subscribes to voice assistant events, and handles the full voice pipeline: when the device detects a wake word, the server captures the streamed PCM audio, runs local VAD (Silero, via ONNX) for endpoint detection, and streams audio in real time to ElevenLabs for transcription via their WebSocket-based realtime STT API.

Once the transcript is ready, it is checked against configurable regex-based shortcuts (which fire webhook POSTs directly) or forwarded to an external webhook (e.g. a chatbot). The webhook's response text is synthesized back to speech via the ElevenLabs TTS REST API and played on the device. The server also runs an HTTP server (port 8085) that serves audio files (tones, TTS responses) for the device to fetch, and exposes a Basic Auth-protected CRUD API for managing shortcuts. Configuration is done through environment variables (`ELEVENLABS_API_KEY`, `WEBHOOK_URL`, `WEBHOOK_PAYLOAD`, `DEVICE_HOST`, `API_PASSWORD`) and CLI flags (`-shortcuts`, `-record-dir`). The entire server is a single Go binary, with the bulk of the logic in `main.go`.

# Releasing

To release a new Docker image on GHCR, tag the commit with a semver tag and push:

```
git tag v0.X.Y && git push origin main v0.X.Y
```

The GitHub Actions workflow builds and pushes `ghcr.io/skorokithakis/havpe-server` with the version tag and `latest`.
