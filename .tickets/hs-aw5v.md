---
id: hs-aw5v
status: closed
deps: []
links: []
created: 2026-04-03T22:02:48Z
type: feature
priority: 2
assignee: Stavros Korokithakis
---
# Stream audio to ElevenLabs realtime STT via WebSocket

Replace the batch ElevenLabs STT call with their WebSocket realtime API to reduce latency. Audio should be streamed to ElevenLabs concurrently while the user speaks, so transcription is nearly complete by the time VAD detects end-of-speech.

Key changes:
- Add a WebSocket library dependency (gorilla/websocket or nhooyr.io/websocket).
- Add a WebSocket connection field to pipelineState. Open it to wss://api.elevenlabs.io/v1/speech-to-text/realtime when the pipeline starts (in handleVoiceAssistantRequest when start=true). Query params: model_id=scribe_v2, language_code=en, audio_format=pcm_16000, commit_strategy=manual. Auth via xi-api-key header.
- In handleVoiceAssistantAudio: after appending to audioBuffer, also send each chunk to the WebSocket as an input_audio_chunk message (audio_base_64: base64-encoded PCM, commit: false, sample_rate: 16000). This happens alongside existing VAD processing, not instead of it.
- When VAD detects end-of-speech (or hard cap), send a final input_audio_chunk with commit: true.
- Start a goroutine that reads WebSocket messages and stores the latest committed_transcript text (protect with a mutex or use a channel).
- In runPipelineResponse: instead of calling transcribeAudio(audioBuffer), retrieve the committed transcript from the WebSocket session. Wait for the committed_transcript message with a reasonable timeout (e.g. 10s). Then close the WebSocket.
- The audioBuffer should still be accumulated (recording mode uses it, and it's harmless), but it is no longer sent to the batch API.
- Handle WebSocket errors (connection failure, error messages from ElevenLabs) by falling back to logging the error and sending VOICE_ASSISTANT_ERROR events, same as the current transcribeAudio error path.

Non-goals: no provider abstraction, no changes to TTS, no changes to VAD parameters, no changes to recording mode behavior.

## Design

The WebSocket connection lifecycle is tied to a single pipeline run. Open on pipeline start, close after getting the committed transcript (or on error/timeout). The connection should not be reused across runs.

For getting the transcript back to runPipelineResponse, a simple approach: store it in a field on pipelineState behind a channel (chan string with buffer 1). The reader goroutine sends the committed transcript text on the channel. runPipelineResponse reads from the channel with a timeout.

## Acceptance Criteria

Audio chunks are forwarded to ElevenLabs WebSocket in real-time during speech. Final transcript comes from committed_transcript WebSocket message. Existing pipeline behavior (events, shortcuts, webhook, TTS) unchanged. Recording mode unaffected. Server builds and existing tests pass.

