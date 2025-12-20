import { useState, useRef, useCallback } from "react";
import "./App.css";

function App() {
  const [isConnected, setIsConnected] = useState(false);
  const [isSpeaking, setIsSpeaking] = useState(false); // When AGENT is speaking
  const [status, setStatus] = useState<
    "idle" | "connecting" | "connected" | "error"
  >("idle");

  const socketRef = useRef<WebSocket | null>(null);
  const audioContextRef = useRef<AudioContext | null>(null);
  const processorRef = useRef<ScriptProcessorNode | null>(null);
  const mediaStreamRef = useRef<MediaStream | null>(null);
  const nextStartTimeRef = useRef(0);

  const floatTo16BitPCM = (input: Float32Array): ArrayBuffer => {
    const output = new Int16Array(input.length);
    for (let i = 0; i < input.length; i++) {
      const s = Math.max(-1, Math.min(1, input[i]));
      output[i] = s < 0 ? s * 0x8000 : s * 0x7fff;
    }
    return output.buffer;
  };

  const playAudioChunk = useCallback(async (arrayBuffer: ArrayBuffer) => {
    if (!audioContextRef.current) return;

    // Agent is speaking!
    setIsSpeaking(true);

    if (arrayBuffer.byteLength % 2 !== 0) {
      arrayBuffer = arrayBuffer.slice(0, arrayBuffer.byteLength - 1);
    }

    const data = new Int16Array(arrayBuffer);
    const float32 = new Float32Array(data.length);
    for (let i = 0; i < data.length; i++) float32[i] = data[i] / 32768;

    const buffer = audioContextRef.current.createBuffer(
      1,
      float32.length,
      16000
    );
    buffer.getChannelData(0).set(float32);

    const source = audioContextRef.current.createBufferSource();
    source.buffer = buffer;
    source.connect(audioContextRef.current.destination);

    const currentTime = audioContextRef.current.currentTime;
    if (nextStartTimeRef.current < currentTime) {
      nextStartTimeRef.current = currentTime;
    }
    source.start(nextStartTimeRef.current);

    // Set speaking to false after this chunk finishes
    const duration = buffer.duration * 1000;
    setTimeout(() => {
      if (
        audioContextRef.current &&
        nextStartTimeRef.current <= audioContextRef.current.currentTime + 0.1
      ) {
        setIsSpeaking(false);
      }
    }, duration + 100);

    nextStartTimeRef.current += buffer.duration;
  }, []);

  const startCall = useCallback(async () => {
    try {
      setStatus("connecting");

      const ws = new WebSocket(`ws://${window.location.host}/ws`);
      socketRef.current = ws;

      ws.onopen = async () => {
        setStatus("connected");
        setIsConnected(true);

        audioContextRef.current = new AudioContext({ sampleRate: 16000 });
        nextStartTimeRef.current = audioContextRef.current.currentTime;

        const stream = await navigator.mediaDevices.getUserMedia({
          audio: {
            echoCancellation: true,
            noiseSuppression: true,
            autoGainControl: true,
          },
        });
        mediaStreamRef.current = stream;

        const source = audioContextRef.current.createMediaStreamSource(stream);
        processorRef.current = audioContextRef.current.createScriptProcessor(
          4096,
          1,
          1
        );
        source.connect(processorRef.current);
        processorRef.current.connect(audioContextRef.current.destination);

        processorRef.current.onaudioprocess = (e) => {
          if (ws.readyState === WebSocket.OPEN) {
            const pcmData = floatTo16BitPCM(e.inputBuffer.getChannelData(0));
            ws.send(pcmData);
          }
        };

        ws.send(JSON.stringify({ type: "hello" }));
      };

      ws.onmessage = async (event) => {
        if (event.data instanceof Blob) {
          const buffer = await event.data.arrayBuffer();
          playAudioChunk(buffer);
        }
      };

      ws.onclose = () => {
        setIsConnected(false);
        setIsSpeaking(false);
        setStatus("idle");
      };

      ws.onerror = () => setStatus("error");
    } catch (err) {
      console.error(err);
      setStatus("error");
    }
  }, [playAudioChunk]);

  const endCall = useCallback(() => {
    socketRef.current?.close();
    mediaStreamRef.current?.getTracks().forEach((t) => t.stop());
    audioContextRef.current?.close();
    setIsConnected(false);
    setIsSpeaking(false);
    setStatus("idle");
  }, []);

  return (
    <div className="flex flex-col items-center justify-center h-screen bg-gradient-to-b from-slate-950 to-slate-900">
      {/* Logo */}
      <div className="mb-8 text-center">
        <div className="w-16 h-16 mx-auto mb-4 rounded-2xl bg-gradient-to-br from-blue-500 to-cyan-500 flex items-center justify-center">
          <svg
            className="w-8 h-8 text-white"
            fill="none"
            stroke="currentColor"
            viewBox="0 0 24 24"
          >
            <path
              strokeLinecap="round"
              strokeLinejoin="round"
              strokeWidth={2}
              d="M4.318 6.318a4.5 4.5 0 000 6.364L12 20.364l7.682-7.682a4.5 4.5 0 00-6.364-6.364L12 7.636l-1.318-1.318a4.5 4.5 0 00-6.364 0z"
            />
          </svg>
        </div>
        <h1 className="text-2xl font-bold text-white">Medicare Services</h1>
        <p className="text-slate-400 text-sm mt-1">Voice Assistant</p>
      </div>

      {/* Voice Orb - Glows only when AGENT is speaking */}
      <button
        onClick={isConnected ? endCall : startCall}
        className={`relative w-32 h-32 rounded-full transition-all duration-500 ${
          isSpeaking
            ? "bg-gradient-to-br from-blue-500 to-cyan-500 shadow-2xl shadow-blue-500/40 scale-110"
            : isConnected
            ? "bg-gradient-to-br from-slate-600 to-slate-700"
            : "bg-gradient-to-br from-slate-700 to-slate-800 hover:from-blue-600 hover:to-cyan-600 hover:scale-105"
        }`}
      >
        {/* Pulse rings only when agent is speaking */}
        {isSpeaking && (
          <>
            <span className="absolute inset-0 rounded-full bg-blue-500/30 animate-ping" />
            <span className="absolute inset-[-8px] rounded-full border-2 border-blue-400/30 animate-pulse" />
            <span
              className="absolute inset-[-16px] rounded-full border border-blue-400/20 animate-pulse"
              style={{ animationDelay: "0.5s" }}
            />
          </>
        )}

        <svg
          className="w-12 h-12 text-white absolute top-1/2 left-1/2 -translate-x-1/2 -translate-y-1/2"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          {isConnected ? (
            <path
              strokeLinecap="round"
              strokeLinejoin="round"
              strokeWidth={2}
              d="M21 12a9 9 0 11-18 0 9 9 0 0118 0z M9 10a1 1 0 011-1h4a1 1 0 011 1v4a1 1 0 01-1 1h-4a1 1 0 01-1-1v-4z"
            />
          ) : (
            <path
              strokeLinecap="round"
              strokeLinejoin="round"
              strokeWidth={2}
              d="M19 11a7 7 0 01-7 7m0 0a7 7 0 01-7-7m7 7v4m0 0H8m4 0h4m-4-8a3 3 0 01-3-3V5a3 3 0 116 0v6a3 3 0 01-3 3z"
            />
          )}
        </svg>
      </button>

      {/* Status */}
      <div className="mt-8 text-center">
        <div
          className={`inline-flex items-center gap-2 px-4 py-2 rounded-full text-sm font-medium ${
            isSpeaking
              ? "bg-blue-500/20 text-blue-400"
              : status === "connected"
              ? "bg-emerald-500/20 text-emerald-400"
              : status === "connecting"
              ? "bg-yellow-500/20 text-yellow-400"
              : status === "error"
              ? "bg-red-500/20 text-red-400"
              : "bg-slate-800 text-slate-400"
          }`}
        >
          <span
            className={`w-2 h-2 rounded-full ${
              isSpeaking
                ? "bg-blue-400 animate-pulse"
                : status === "connected"
                ? "bg-emerald-400"
                : status === "connecting"
                ? "bg-yellow-400 animate-pulse"
                : status === "error"
                ? "bg-red-400"
                : "bg-slate-500"
            }`}
          />
          {isSpeaking
            ? "Speaking..."
            : status === "connected"
            ? "Listening..."
            : status === "connecting"
            ? "Connecting..."
            : status === "error"
            ? "Error"
            : "Tap to speak"}
        </div>
      </div>

      {/* Hint */}
      <p className="mt-12 text-slate-600 text-sm max-w-xs text-center">
        I can help you book appointments, answer questions about doctors, or
        assist with prescriptions.
      </p>
    </div>
  );
}

export default App;
