import { useState, useCallback, useRef } from "react";
import {
  LiveKitRoom,
  RoomAudioRenderer,
  useConnectionState,
  useTracks,
  useRemoteParticipants,
  useDataChannel,
} from "@livekit/components-react";
import { ConnectionState, Track } from "livekit-client";
import "@livekit/components-styles";
import "./index.css";

interface TokenData {
  token: string;
  room: string;
  url: string;
}

interface AudioMessage {
  type: "audio" | "text";
  audio?: string; // base64
  sampleRate?: number;
  text?: string;
}

function VoiceUI() {
  const connectionState = useConnectionState();
  const localTracks = useTracks([Track.Source.Microphone]);
  const remoteParticipants = useRemoteParticipants();
  const isConnected = connectionState === ConnectionState.Connected;

  const [isPlaying, setIsPlaying] = useState(false);
  const [agentText, setAgentText] = useState<string>("");

  // Audio playback using Audio element (better for streaming MP3)
  const audioQueueRef = useRef<string[]>([]);
  const isPlayingRef = useRef(false);
  const currentAudioRef = useRef<HTMLAudioElement | null>(null);

  // Handle data channel messages on 'audio' topic
  useDataChannel("audio", (msg) => {
    try {
      const data: AudioMessage = JSON.parse(
        new TextDecoder().decode(msg.payload)
      );

      if (data.type === "text" && data.text) {
        setAgentText(data.text);
        console.log("Agent text:", data.text);
      }

      if (data.type === "audio" && data.audio) {
        console.log("Received audio data:", data.audio.length, "chars base64");
        // Add to queue and play
        audioQueueRef.current.push(data.audio);
        if (!isPlayingRef.current) {
          playNextInQueue();
        }
      }
    } catch (e) {
      console.error("Failed to parse data channel message:", e);
    }
  });

  // Play MP3 audio using Audio element
  const playNextInQueue = () => {
    if (audioQueueRef.current.length === 0) {
      isPlayingRef.current = false;
      setIsPlaying(false);
      return;
    }

    isPlayingRef.current = true;
    setIsPlaying(true);

    const base64Audio = audioQueueRef.current.shift()!;

    // Create audio element with data URL
    const audio = new Audio(`data:audio/mpeg;base64,${base64Audio}`);
    currentAudioRef.current = audio;

    audio.onended = () => {
      console.log("Audio chunk finished");
      playNextInQueue();
    };

    audio.onerror = (e) => {
      console.error("Audio playback error:", e);
      playNextInQueue();
    };

    audio
      .play()
      .then(() => {
        console.log("Playing audio chunk");
      })
      .catch((e) => {
        console.error("Failed to play audio:", e);
        playNextInQueue();
      });
  };

  return (
    <div className="flex flex-col items-center justify-center gap-6">
      {/* Connection Status */}
      <div className="text-center text-sm">
        <span className={isConnected ? "text-green-400" : "text-yellow-400"}>
          {isConnected ? "● Connected" : "○ " + connectionState}
        </span>
        {remoteParticipants.length > 0 && (
          <span className="ml-2 text-blue-400">
            • Agent: {remoteParticipants[0]?.identity}
          </span>
        )}
      </div>

      {/* Voice Visualizer Orb */}
      <div className="relative w-40 h-40 flex items-center justify-center">
        <div
          className={`absolute inset-0 rounded-full transition-all duration-500 ${
            isPlaying
              ? "bg-gradient-to-br from-green-500 to-emerald-500 shadow-2xl shadow-green-500/40 animate-pulse"
              : isConnected
              ? "bg-gradient-to-br from-blue-500 to-cyan-500 shadow-2xl shadow-blue-500/40"
              : "bg-gradient-to-br from-slate-600 to-slate-700"
          }`}
        />
        <div className="relative z-10">
          <div className="w-10 h-10 text-white/80">
            <svg viewBox="0 0 24 24" fill="currentColor">
              <path d="M12 14c1.66 0 3-1.34 3-3V5c0-1.66-1.34-3-3-3S9 3.34 9 5v6c0 1.66 1.34 3 3 3z" />
              <path d="M17 11c0 2.76-2.24 5-5 5s-5-2.24-5-5H5c0 3.53 2.61 6.43 6 6.92V21h2v-3.08c3.39-.49 6-3.39 6-6.92h-2z" />
            </svg>
          </div>
        </div>
      </div>

      {/* Status Text */}
      <div className="text-center">
        <p className="text-lg text-white/80">
          {isPlaying
            ? "Speaking..."
            : isConnected
            ? "Listening..."
            : "Connecting..."}
        </p>
      </div>

      {/* Agent Response */}
      {agentText && (
        <div className="max-w-md p-4 bg-slate-800/50 rounded-lg">
          <p className="text-sm text-white/70">{agentText}</p>
        </div>
      )}

      {/* Debug Info */}
      <div className="mt-4 p-3 bg-slate-800 rounded-lg max-w-md w-full">
        <p className="text-xs text-white/60 font-mono">
          🔊 Audio: {isPlaying ? "Playing" : "Idle"} | Queue:{" "}
          {audioQueueRef.current?.length || 0} | Tracks: {localTracks.length}
        </p>
      </div>
    </div>
  );
}

function App() {
  const [tokenData, setTokenData] = useState<TokenData | null>(null);
  const [isLoading, setIsLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const connect = useCallback(async () => {
    try {
      setIsLoading(true);
      setError(null);

      const response = await fetch("/api/livekit/token", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
      });

      if (!response.ok) {
        throw new Error("Failed to get connection token");
      }

      const data = await response.json();
      setTokenData(data);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Connection failed");
    } finally {
      setIsLoading(false);
    }
  }, []);

  const disconnect = useCallback(() => {
    setTokenData(null);
  }, []);

  return (
    <div className="min-h-screen bg-gradient-to-b from-slate-900 via-slate-800 to-slate-900 flex flex-col items-center justify-center p-8">
      {/* Header */}
      <div className="text-center mb-8">
        <h1 className="text-3xl font-bold text-white mb-2">
          Medicare Voice Assistant
        </h1>
        <p className="text-slate-400 text-sm">Powered by LiveKit</p>
      </div>

      {/* Main Content */}
      {!tokenData && !isLoading && (
        <div className="flex flex-col items-center gap-6">
          <button
            onClick={connect}
            className="px-8 py-4 bg-gradient-to-r from-blue-500 to-cyan-500 text-white font-semibold rounded-full shadow-lg shadow-blue-500/30 hover:shadow-blue-500/50 hover:scale-105 transition-all duration-300"
          >
            Start Conversation
          </button>
          {error && <p className="text-red-400 text-sm">{error}</p>}
        </div>
      )}

      {isLoading && (
        <div className="flex flex-col items-center gap-4">
          <div className="w-12 h-12 border-4 border-blue-500 border-t-transparent rounded-full animate-spin" />
          <p className="text-white/60">Getting room token...</p>
        </div>
      )}

      {tokenData && (
        <LiveKitRoom
          serverUrl={tokenData.url}
          token={tokenData.token}
          connect={true}
          audio={true}
          video={false}
          onDisconnected={disconnect}
          className="flex flex-col items-center gap-6"
        >
          <VoiceUI />
          <RoomAudioRenderer />

          <button
            onClick={disconnect}
            className="mt-4 px-6 py-2 border border-red-500/50 text-red-400 rounded-full hover:bg-red-500/10 transition-colors"
          >
            End Conversation
          </button>
        </LiveKitRoom>
      )}
    </div>
  );
}

export default App;
