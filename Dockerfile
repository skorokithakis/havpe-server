FROM golang:1.24-bookworm AS build

RUN apt-get update && apt-get install -y --no-install-recommends wget unzip && rm -rf /var/lib/apt/lists/*

# ONNX Runtime is a C shared library that silero-vad-go links against via CGO.
# We install it to a fixed prefix so the CGO env vars below can point at it consistently.
RUN wget -q https://github.com/microsoft/onnxruntime/releases/download/v1.18.1/onnxruntime-linux-x64-1.18.1.tgz \
    && tar -xzf onnxruntime-linux-x64-1.18.1.tgz \
    && mkdir -p /usr/local/lib/onnxruntime \
    && cp -r onnxruntime-linux-x64-1.18.1/include /usr/local/lib/onnxruntime/ \
    && cp -r onnxruntime-linux-x64-1.18.1/lib /usr/local/lib/onnxruntime/ \
    && rm -rf onnxruntime-linux-x64-1.18.1 onnxruntime-linux-x64-1.18.1.tgz

# The Silero VAD model is not embedded in the Go binary; it is loaded at runtime from
# the working directory. We download it here so the runtime image can include it.
RUN wget -q https://github.com/snakers4/silero-vad/archive/refs/tags/v5.1.1.zip \
    && unzip -q v5.1.1.zip silero-vad-5.1.1/src/silero_vad/data/silero_vad.onnx \
    && mv silero-vad-5.1.1/src/silero_vad/data/silero_vad.onnx /silero_vad.onnx \
    && rm -rf silero-vad-5.1.1 v5.1.1.zip

# The official Go Docker image sets GOTOOLCHAIN=local, which prevents automatic
# toolchain downloads. We override it so Go 1.24 can fetch the 1.25 toolchain
# that go.mod requires.
ENV GOTOOLCHAIN=auto
ENV CGO_ENABLED=1
ENV LIBRARY_PATH=/usr/local/lib/onnxruntime/lib
ENV C_INCLUDE_PATH=/usr/local/lib/onnxruntime/include
# LD_RUN_PATH embeds the ONNX Runtime library search path into the binary's RPATH so
# the dynamic linker can find libonnxruntime.so without LD_LIBRARY_PATH at runtime.
ENV LD_RUN_PATH=/usr/local/lib/onnxruntime/lib

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o havpe-server .


FROM debian:bookworm-slim

# The server makes HTTPS calls to external APIs (ElevenLabs), so it needs
# trusted root certificates that bookworm-slim doesn't ship.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*

# Copy the ONNX Runtime shared libraries and register them with ldconfig so the
# dynamic linker finds them without requiring LD_LIBRARY_PATH in the container.
COPY --from=build /usr/local/lib/onnxruntime/lib/ /usr/local/lib/
RUN ldconfig

WORKDIR /app

COPY --from=build /src/havpe-server ./
COPY --from=build /silero_vad.onnx ./
COPY tone.wav error.wav ./

ENTRYPOINT ["./havpe-server"]
