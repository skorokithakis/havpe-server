---
id: hs-r9g2
status: closed
deps: []
links: []
created: 2026-04-06T13:59:42Z
type: task
priority: 2
assignee: Stavros Korokithakis
---
# Initialize recordingFileCounter from existing files in record directory

On startup when recordDir is set, scan the directory for existing NNN.wav files and set recordingFileCounter to the highest number found. This prevents overwriting files from previous runs. The scan goes in main() right after the MkdirAll call (~line 826). Use filepath.Glob or os.ReadDir to find *.wav files, parse the numeric prefix, and take the max. Non-goal: do not change the file naming scheme or the recording logic itself.

## Acceptance Criteria

Server started with existing 001.wav and 002.wav in record-dir produces 003.wav as the next recording, not 001.wav.

