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
| `API_PASSWORD`       | Yes      | Password for the shortcuts and settings CRUD APIs (HTTP Basic Auth).                           |
| `STT_LANGUAGE`       | No       | ElevenLabs STT language code. Defaults to `en`.                                                |
| `TTS_SPEED`          | No       | ElevenLabs TTS playback speed. Defaults to `1.0`.                                              |

You can use a `.envrc` file with [direnv](https://direnv.net/) to set these
automatically.


## Shortcuts

Shortcuts are an ordered list of regex/URL pairs that are checked against each
transcript before it is sent to the webhook. The first matching regex wins: the
server POSTs an empty body to the associated URL, plays the confirmation tone on
a 200 response, or plays the error sound on any other outcome. If no shortcut
matches, the transcript falls through to the normal webhook flow.

The shortcuts file is a JSON array of `["regex", "url"]` pairs:

```json
[
  ["turn on the lights", "http://homeassistant.local:8123/api/webhook/lights-on"],
  ["^(good night|goodnight)$", "http://homeassistant.local:8123/api/webhook/goodnight"]
]
```

Pass the path to this file with the `-shortcuts` flag. If the file does not
exist it is created automatically as an empty list (`[]`).

### Shortcuts CRUD API

When `-shortcuts` is set, the server exposes a small REST API on the same port
(8085) for managing shortcuts at runtime. All endpoints require HTTP Basic Auth
with any username and `API_PASSWORD` as the password.

| Method   | Path                  | Description                                      |
|----------|-----------------------|--------------------------------------------------|
| `GET`    | `/shortcuts`          | Return the full list as a JSON array             |
| `POST`   | `/shortcuts`          | Append a new `["regex", "url"]` pair             |
| `PUT`    | `/shortcuts/{index}`  | Replace the shortcut at the given index          |
| `DELETE` | `/shortcuts/{index}`  | Remove the shortcut at the given index           |

Changes are persisted to the shortcuts file immediately after each write.


## Settings

Runtime settings (currently `stt_language` and `tts_speed`) can be adjusted
without restarting the server. They are stored in a JSON file specified by the
`-settings` flag (default: `settings.json`).

Precedence: settings file > environment variables > built-in defaults.

The server exposes a REST API on port 8085 for reading and updating settings at
runtime. All endpoints require HTTP Basic Auth with any username and
`API_PASSWORD` as the password.

| Method | Path        | Description                                      |
|--------|-------------|--------------------------------------------------|
| `GET`  | `/settings` | Return current settings as a JSON object         |
| `PUT`  | `/settings` | Update settings; accepts a partial JSON object   |

```bash
# Read current settings
curl -u :yourpassword http://localhost:8085/settings

# Update TTS speed
curl -u :yourpassword -X PUT -H 'Content-Type: application/json' \
  -d '{"tts_speed": 1.25}' http://localhost:8085/settings
```

Changes are persisted to the settings file immediately after each write.

For Docker, mount the settings file for persistence:

```bash
docker run --network host \
  -e ELEVENLABS_API_KEY \
  -e WEBHOOK_URL \
  -e WEBHOOK_PAYLOAD \
  -e API_PASSWORD \
  -v ./settings.json:/app/settings.json \
  ghcr.io/skorokithakis/havpe-server \
  -settings /app/settings.json
```


## Recording mode

Recording mode captures raw audio from the device and saves it as WAV files
for later use (e.g. as training data). In this mode the ElevenLabs and webhook
environment variables are not required.

```bash
docker run --network host \
  -v ./recordings:/app/recordings \
  ghcr.io/skorokithakis/havpe-server \
  -record-dir /app/recordings
```

Each wake-word trigger produces a single WAV file containing the full session
audio. The session ends automatically after 5 seconds of silence. Files are
numbered sequentially across sessions (`001.wav`, `002.wav`, ...).

**Note:** You should add `micro_wake_word.stop:` to `on_start` and
`micro_wake_word.start:` to `on_end` in your ESPHome YAML to prevent the
wake word model from falsely triggering during recording and killing the
session. Also ensure `silence_detection: false` is set on the
`voice_assistant.start` action.

### Segmenting recordings

The `segment.py` script splits a session WAV into individual utterances using
Silero VAD. It requires [uv](https://docs.astral.sh/uv/) (dependencies are
installed automatically on first run).

```bash
./segment.py recording.wav output_dir/
```

Files are numbered sequentially (`001.wav`, `002.wav`, ...). If the output
directory already contains numbered files, numbering continues from the
highest existing number. Use `--padding-ms` to control how much silence to
keep around each segment (default: 200ms).


## Running

### Docker

```bash
docker run --network host \
  -e ELEVENLABS_API_KEY \
  -e WEBHOOK_URL \
  -e WEBHOOK_PAYLOAD \
  -e API_PASSWORD \
  ghcr.io/skorokithakis/havpe-server
```

To use shortcuts, mount a file for persistence:

```bash
docker run --network host \
  -e ELEVENLABS_API_KEY \
  -e WEBHOOK_URL \
  -e WEBHOOK_PAYLOAD \
  -e API_PASSWORD \
  -v ./shortcuts.json:/app/shortcuts.json \
  ghcr.io/skorokithakis/havpe-server \
  -shortcuts /app/shortcuts.json
```

`--network host` is required so that mDNS discovery works and so the device
can reach the HTTP server. If you want to skip discovery, set `DEVICE_HOST` to
the hostname or IP of the device.

### From source

```bash
go build -o havpe-server . && ./havpe-server [-shortcuts shortcuts.json]
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
