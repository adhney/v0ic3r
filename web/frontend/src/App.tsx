import { useState, useEffect, useRef, useCallback } from "react";
import "./App.css";

interface Message {
  role: "user" | "agent";
  text: string;
}

function App() {
  const [isConnected, setIsConnected] = useState(false);
  const [isListening, setIsListening] = useState(false);
  const [messages, setMessages] = useState<Message[]>([]);
  const [status, setStatus] = useState<
    "idle" | "connecting" | "connected" | "error"
  >("idle");

  const socketRef = useRef<WebSocket | null>(null);
  const audioContextRef = useRef<AudioContext | null>(null);
  const processorRef = useRef<ScriptProcessorNode | null>(null);
  const mediaStreamRef = useRef<MediaStream | null>(null);
  const nextStartTimeRef = useRef(0);
  const transcriptRef = useRef<HTMLDivElement>(null);
  const lastAudioTimeRef = useRef(Date.now());
  const agentTextQueueRef = useRef("");

  const scrollToBottom = useCallback(() => {
    if (transcriptRef.current) {
      transcriptRef.current.scrollTop = transcriptRef.current.scrollHeight;
    }
  }, []);

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
    lastAudioTimeRef.current = Date.now();

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
        setIsListening(true);

        // Initialize Audio
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

        // Send ready signal
        ws.send(JSON.stringify({ type: "hello" }));
      };

      ws.onmessage = async (event) => {
        if (typeof event.data === "string") {
          try {
            const msg = JSON.parse(event.data);
            if (msg.role === "user") {
              setMessages((prev) => {
                const last = prev[prev.length - 1];
                if (last?.role === "user") {
                  return [
                    ...prev.slice(0, -1),
                    { ...last, text: last.text + msg.text },
                  ];
                }
                return [...prev, { role: "user", text: msg.text }];
              });
            } else if (msg.role === "agent") {
              agentTextQueueRef.current += msg.text;
            }
          } catch (e) {
            /* ignore */
          }
        } else if (event.data instanceof Blob) {
          const buffer = await event.data.arrayBuffer();
          playAudioChunk(buffer);
        }
      };

      ws.onclose = () => {
        setIsConnected(false);
        setIsListening(false);
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
    setIsListening(false);
    setStatus("idle");
  }, []);

  // Text sync render loop
  useEffect(() => {
    const interval = setInterval(() => {
      if (agentTextQueueRef.current.length > 0 && audioContextRef.current) {
        const timeSinceAudio = Date.now() - lastAudioTimeRef.current;
        const isAudioPlaying =
          nextStartTimeRef.current > audioContextRef.current.currentTime;
        const forceFlush = timeSinceAudio > 2000;

        if (isAudioPlaying || forceFlush) {
          const speed = forceFlush ? 5 : 1;
          const chunk = agentTextQueueRef.current.slice(0, speed);
          agentTextQueueRef.current = agentTextQueueRef.current.slice(speed);

          setMessages((prev) => {
            const last = prev[prev.length - 1];
            if (last?.role === "agent") {
              return [
                ...prev.slice(0, -1),
                { ...last, text: last.text + chunk },
              ];
            }
            return [...prev, { role: "agent", text: chunk }];
          });
        }
      }
    }, 16); // ~60fps

    return () => clearInterval(interval);
  }, []);

  // Auto-scroll
  useEffect(() => {
    scrollToBottom();
  }, [messages, scrollToBottom]);

  return (
    <div className="flex flex-col h-screen bg-gradient-to-b from-slate-950 to-slate-900">
      {/* Header */}
      <header className="flex items-center justify-between px-6 py-4 border-b border-slate-800/50 bg-slate-900/80 backdrop-blur-sm">
        <div className="flex items-center gap-3">
          <div className="w-10 h-10 rounded-xl bg-gradient-to-br from-blue-500 to-cyan-500 flex items-center justify-center">
            <svg
              className="w-6 h-6 text-white"
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
          <div>
            <h1 className="text-lg font-semibold text-white">
              Medicare Services
            </h1>
            <p className="text-xs text-slate-400">Voice Assistant</p>
          </div>
        </div>
        <div
          className={`px-3 py-1.5 rounded-full text-xs font-medium ${
            status === "connected"
              ? "bg-emerald-500/20 text-emerald-400 border border-emerald-500/30"
              : status === "connecting"
              ? "bg-yellow-500/20 text-yellow-400 border border-yellow-500/30"
              : status === "error"
              ? "bg-red-500/20 text-red-400 border border-red-500/30"
              : "bg-slate-700/50 text-slate-400 border border-slate-600/30"
          }`}
        >
          {status === "connected"
            ? "● Connected"
            : status === "connecting"
            ? "○ Connecting..."
            : status === "error"
            ? "● Error"
            : "○ Ready"}
        </div>
      </header>

      {/* Chat Area */}
      <div ref={transcriptRef} className="flex-1 overflow-y-auto p-6 space-y-4">
        {messages.length === 0 && !isConnected && (
          <div className="flex flex-col items-center justify-center h-full text-center space-y-4">
            <div className="w-20 h-20 rounded-full bg-gradient-to-br from-blue-500/20 to-cyan-500/20 flex items-center justify-center">
              <svg
                className="w-10 h-10 text-blue-400"
                fill="none"
                stroke="currentColor"
                viewBox="0 0 24 24"
              >
                <path
                  strokeLinecap="round"
                  strokeLinejoin="round"
                  strokeWidth={1.5}
                  d="M19 11a7 7 0 01-7 7m0 0a7 7 0 01-7-7m7 7v4m0 0H8m4 0h4m-4-8a3 3 0 01-3-3V5a3 3 0 116 0v6a3 3 0 01-3 3z"
                />
              </svg>
            </div>
            <p className="text-slate-500 max-w-md">
              Tap the button below to speak with your Medicare assistant. I can
              help with appointments, doctors, and prescriptions.
            </p>
          </div>
        )}

        {messages.map((msg, i) => (
          <div
            key={i}
            className={`flex ${
              msg.role === "user" ? "justify-end" : "justify-start"
            }`}
          >
            <div
              className={`max-w-[80%] px-4 py-3 rounded-2xl ${
                msg.role === "user"
                  ? "bg-blue-600 text-white rounded-tr-sm"
                  : "bg-slate-800 text-slate-100 rounded-tl-sm border border-slate-700/50"
              }`}
            >
              {msg.text}
            </div>
          </div>
        ))}
      </div>

      {/* Footer Controls */}
      <div className="p-6 pt-0">
        <div className="flex flex-col items-center gap-4">
          {/* Voice Orb */}
          <button
            onClick={isConnected ? endCall : startCall}
            className={`relative w-20 h-20 rounded-full transition-all duration-300 ${
              isListening
                ? "bg-gradient-to-br from-blue-500 to-cyan-500 shadow-lg shadow-blue-500/30 animate-pulse"
                : "bg-gradient-to-br from-slate-700 to-slate-800 hover:from-blue-600 hover:to-cyan-600"
            }`}
          >
            <svg
              className="w-8 h-8 text-white absolute top-1/2 left-1/2 -translate-x-1/2 -translate-y-1/2"
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

          <p className="text-sm text-slate-500">
            {isConnected ? "Tap to end call" : "Tap to start speaking"}
          </p>
        </div>
      </div>
    </div>
  );
}

export default App;
