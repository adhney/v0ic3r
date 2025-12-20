# V0ic3r 🎙️

A low-latency voice agent demo built with Go, featuring real-time speech-to-text, LLM processing, and text-to-speech.

## Features

- 🎤 **Real-time Speech Recognition** - Powered by Deepgram Nova 3
- 🧠 **AI Conversations** - Using Google Gemini 2.0 Flash
- 🔊 **Natural Voice Output** - ElevenLabs Flash TTS
- ⚡ **Low Latency** - Sub-second response times
- 🏥 **Medicare Demo** - Pre-configured as a healthcare assistant

## Architecture

```
Browser (React) <── WebSocket ──> Go Server
                                    │
         ┌──────────┬───────────────┼───────────────┐
         ▼          ▼               ▼               ▼
    Deepgram    Gemini LLM    ElevenLabs TTS    Browser
      (STT)                                    (Audio Out)
```

## Quick Start

### Prerequisites

- Go 1.21+
- Node.js 20+
- API Keys for: Deepgram, Google Gemini, ElevenLabs

### Setup

1. Clone and install:

```bash
git clone https://github.com/adhney/v0ic3r.git
cd v0ic3r
```

2. Create `.env` file:

```env
DEEPGRAM_API_KEY=your_key
GEMINI_API_KEY=your_key
ELEVENLABS_API_KEY=your_key
PORT=8080
```

3. Build frontend:

```bash
cd web/frontend
npm install
npm run build
cd ../..
```

4. Run:

```bash
go run cmd/server/main.go
```

5. Open http://localhost:8080

## Usage

1. Click the microphone orb to start
2. Speak naturally
3. Listen to the AI response
4. Click again to end

## Tech Stack

- **Backend**: Go, Gorilla WebSocket
- **Frontend**: React, TypeScript, Tailwind CSS
- **APIs**: Deepgram, Google Gemini, ElevenLabs

## License

MIT
