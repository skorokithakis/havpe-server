# havpe-server

A standalone Go server that replaces Home Assistant as the voice pipeline
backend for the [Home Assistant Voice PE](https://www.home-assistant.io/voice-pe/)
device. It connects directly to the device over the ESPHome native API,
captures microphone audio, transcribes it with ElevenLabs STT, posts the
transcript to a webhook (e.g. a chatbot), and plays back the TTS response.


## Prerequisites

- Docker
- An ElevenLabs API key (for STT and TTS)
- A webhook URL that accepts a JSON POST and returns a response
- The Voice PE device on the same network, reachable on port 6053


## Preparing the Voice PE firmware

The server uses the ESPHome native API in **plaintext mode** (no encryption,
no authentication). If the device's firmware has an `encryption:` block under
the `api:` section, you need to remove it. Open
`home-assistant-voice.yaml` (in the repository root) and make sure the `api:`
section looks like this:

```yaml
api:
  id: api_id
  on_client_connected:
    - script.execute: control_leds
  on_client_disconnected:
    - script.execute: control_leds
```

If there is an `encryption:` block with a `key:` underneath it, delete both
lines. Then re-flash the firmware to the device.


## Configuration

| Variable             | Required | Description                                                                                    |
|----------------------|----------|------------------------------------------------------------------------------------------------|
| `ELEVENLABS_API_KEY` | Yes      | Your ElevenLabs API key for STT and TTS                                                        |
| `WEBHOOK_URL`        | Yes      | URL to POST transcripts to (receives JSON, returns JSON)                                       |
| `WEBHOOK_PAYLOAD`    | Yes      | JSON template for the POST body; `$transcript` is replaced with the JSON-escaped transcript    |
| `DEVICE_HOST`        | No       | Hostname or IP of the Voice PE device. If not set, the server discovers it via mDNS by looking for ESPHome devices named `home-assistant-voice-*`. |

You can use a `.envrc` file with [direnv](https://direnv.net/) to set these
automatically.


## Running

### Docker

```bash
docker run --network host \
  -e ELEVENLABS_API_KEY \
  -e WEBHOOK_URL \
  -e WEBHOOK_PAYLOAD \
  ghcr.io/skorokithakis/havpe-server
```

`--network host` is required so that mDNS discovery works and so the device
can reach the HTTP server. If you want to skip discovery, set `DEVICE_HOST` to
the hostname or IP of the device.

### From source

```bash
go build -o havpe-server . && ./havpe-server
```

You need Go 1.25+ and must download `silero_vad.onnx` (the
[Silero VAD](https://github.com/snakers4/silero-vad) v5 ONNX model) and place
it in the working directory before running.

The server will:

1. Connect to the device on port 6053.
2. Start an HTTP server on port 8085 to serve audio files to the device.
3. Subscribe to voice assistant events and handle the full pipeline.

Say the wake word, and the server will capture your speech, transcribe it,
send it to the webhook, and play the response back through the device.


## Running tests

```bash
go test ./...
```


## Webhook protocol

The server POSTs the body defined by `WEBHOOK_PAYLOAD` to `WEBHOOK_URL`. Before
sending, the literal string `$transcript` in the template is replaced with the
actual transcript, JSON-escaped so that embedded quotes, backslashes, newlines,
and other control characters do not break the JSON structure.

For example, with the default template:

```
WEBHOOK_PAYLOAD='{"message": "$transcript", "source": "havpe", "sender": "stavros"}'
```

a transcript of `Hello, world!` produces:

```json
{"message": "Hello, world!", "source": "havpe", "sender": "stavros"}
```

The webhook should return JSON with a `response` field:

```json
{"response": "The text to speak back."}
```

If the response is empty or missing, the server plays the confirmation tone
instead of TTS audio.
