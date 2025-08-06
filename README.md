# Agora Go Publisher

A Go application for publishing audio and video streams to Agora using a parent-child process architecture.

## Prerequisites

Ubuntu Linux (20.04 or later) with the following installed:

```bash
# Install build essentials and Go
sudo apt-get update
sudo apt-get install -y build-essential git wget

# Install Go 1.21
wget https://go.dev/dl/go1.21.5.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.21.5.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin

# Install FlatBuffers compiler
sudo apt-get install -y flatbuffers-compiler
```

## Quick Setup

```bash
# 1. Clone the repository
git clone https://github.com/BenWeekes/agora-go-publish.git
cd agora-go-publish

# 2. Generate Go code from FlatBuffers
cd ipc
flatc --go ipc_defs.fbs
cd ..

# 3. Get Go dependencies
go mod tidy

# 4. Build the binaries
go build -o child child.go
go build -o parent parent.go
chmod +x child parent
```

## Run

Basic usage:

```bash
./parent \
  -appID "YOUR_AGORA_APP_ID" \
  -channelName "test-channel" \
  -userID "test-user-123"
```

Full options:

```bash
./parent \
  -appID "YOUR_AGORA_APP_ID" \
  -channelName "test-channel" \
  -userID "test-user-123" \
  -token "YOUR_TOKEN" \
  -audioFile "test_data/send_audio_16k_1ch.pcm" \
  -videoFile "test_data/send_video_cif.yuv" \
  -sampleRate 16000 \
  -audioChannels 1 \
  -width 352 \
  -height 288 \
  -frameRate 15 \
  -videoCodec "H264" \
  -bitrate 1000 \
  -minBitrate 100
```

Press `Ctrl+C` to stop streaming.

## Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| -appID | Agora App ID (required) | - |
| -channelName | Channel to join | test-channel |
| -userID | User identifier | test-user |
| -token | Authentication token | "" |
| -audioFile | PCM audio file | test_data/send_audio_16k_1ch.pcm |
| -videoFile | YUV420 video file | test_data/send_video_cif.yuv |
| -sampleRate | Audio sample rate (Hz) | 16000 |
| -audioChannels | Audio channels | 1 |
| -width | Video width | 352 |
| -height | Video height | 288 |
| -frameRate | Video FPS | 15 |
| -videoCodec | H264 or VP8 | H264 |
| -bitrate | Target bitrate (Kbps) | 1000 |
| -minBitrate | Min bitrate (Kbps) | 100 |
