#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.12"
# dependencies = ["torch", "soundfile", "packaging"]
# ///

import argparse
import glob
import os

import soundfile as sf
import torch


def load_audio(input_path: str) -> tuple[torch.Tensor, int]:
    data, sample_rate = sf.read(input_path, dtype="float32")
    # soundfile returns (samples,) for mono or (samples, channels) for multi-channel.
    # Silero VAD expects a 1D tensor of mono float32 samples.
    waveform = torch.from_numpy(data)
    if waveform.ndim > 1:
        waveform = waveform.mean(dim=1)
    return waveform, sample_rate


def get_speech_segments(waveform: torch.Tensor, sample_rate: int) -> list[dict]:
    model, utils = torch.hub.load(
        repo_or_dir="snakers4/silero-vad",
        model="silero_vad",
        trust_repo=True,
    )
    get_speech_timestamps = utils[0]
    timestamps = get_speech_timestamps(waveform, model, sampling_rate=sample_rate)
    return timestamps


def save_segments(
    waveform: torch.Tensor,
    sample_rate: int,
    timestamps: list[dict],
    output_dir: str,
    padding_samples: int,
) -> None:
    total_samples = len(waveform)
    os.makedirs(output_dir, exist_ok=True)

    # Find the highest existing NNN.wav number so we continue the sequence.
    existing = glob.glob(os.path.join(output_dir, "[0-9][0-9][0-9].wav"))
    start_number = max((int(os.path.basename(f)[:3]) for f in existing), default=0)

    print(f"Found {len(timestamps)} segment(s).")

    for index, segment in enumerate(timestamps, start=start_number + 1):
        start = max(0, segment["start"] - padding_samples)
        end = min(total_samples, segment["end"] + padding_samples)

        start_seconds = start / sample_rate
        end_seconds = end / sample_rate

        output_filename = f"{index:03d}.wav"
        output_path = os.path.join(output_dir, output_filename)

        segment_data = waveform[start:end].numpy()
        sf.write(output_path, segment_data, sample_rate)

        print(f"  {output_filename}: {start_seconds:.3f}s – {end_seconds:.3f}s")


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Split a WAV file into speech segments using Silero VAD."
    )
    parser.add_argument("input_wav", help="Path to the input WAV file.")
    parser.add_argument("output_dir", help="Directory to write segmented WAV files.")
    parser.add_argument(
        "--padding-ms",
        type=int,
        default=200,
        help="Padding in milliseconds to add on each side of a segment (default: 200).",
    )
    args = parser.parse_args()

    waveform, sample_rate = load_audio(args.input_wav)
    padding_samples = int(args.padding_ms * sample_rate / 1000)
    timestamps = get_speech_segments(waveform, sample_rate)
    save_segments(waveform, sample_rate, timestamps, args.output_dir, padding_samples)


if __name__ == "__main__":
    main()
