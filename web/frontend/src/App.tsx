import { useState, useCallback, useRef, useEffect } from "react";
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
  type:
    | "audio"
    | "text"
    | "audio_stop"
    | "audio_start"
    | "audio_end"
    | "agent_ready";
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
  const [isAgentReady, setIsAgentReady] = useState(false);

  // Buffer for accumulating audio chunks (as binary)
  const audioBufferRef = useRef<Uint8Array[]>([]);
  const playbackQueueRef = useRef<string[]>([]);
  const isPlayingRef = useRef(false);
  const playbackTimeoutRef = useRef<number | null>(null);
  const currentAudioRef = useRef<HTMLAudioElement | null>(null);
  const shouldIgnoreAudioRef = useRef(false); // Set after barge-in to ignore incoming chunks

  // Stop all audio playback (for barge-in)
  const stopAllAudio = useCallback(() => {
    console.log("[BARGE-IN] Stopping all audio");
    // Ignore any incoming audio until next audio_start
    shouldIgnoreAudioRef.current = true;
    // Clear buffer
    audioBufferRef.current = [];
    // Clear playback queue and revoke URLs
    playbackQueueRef.current.forEach((url) => URL.revokeObjectURL(url));
    playbackQueueRef.current = [];
    // Clear pending timeout
    if (playbackTimeoutRef.current) {
      clearTimeout(playbackTimeoutRef.current);
      playbackTimeoutRef.current = null;
    }
    // Stop current audio
    if (currentAudioRef.current) {
      currentAudioRef.current.pause();
      currentAudioRef.current = null;
    }
    isPlayingRef.current = false;
    setIsPlaying(false);
  }, []);

  // Handle data channel messages on 'audio' topic
  useDataChannel("audio", (msg) => {
    try {
      const data: AudioMessage = JSON.parse(
        new TextDecoder().decode(msg.payload)
      );

      if (data.type === "audio_stop") {
        stopAllAudio();
        return;
      }

      if (data.type === "audio_start") {
        // New audio stream starting, accept audio again
        shouldIgnoreAudioRef.current = false;
        return;
      }

      if (data.type === "agent_ready") {
        console.log("[AGENT] Agent is ready");
        setIsAgentReady(true);
        return;
      }

      if (data.type === "text" && data.text) {
        setAgentText(data.text);
        console.log("Agent text:", data.text);
      }

      if (data.type === "audio" && data.audio) {
        // Ignore audio if we're in barge-in stop mode
        if (shouldIgnoreAudioRef.current) {
          console.log("[BARGE-IN] Ignoring audio chunk (stopped)");
          return;
        }

        console.log("Received audio chunk:", data.audio.length, "chars");

        // Decode base64 to binary and buffer
        const binaryString = atob(data.audio);
        const bytes = new Uint8Array(binaryString.length);
        for (let i = 0; i < binaryString.length; i++) {
          bytes[i] = binaryString.charCodeAt(i);
        }
        audioBufferRef.current.push(bytes);

        // Reset the timeout - we'll play after 300ms of no new chunks
        if (playbackTimeoutRef.current) {
          clearTimeout(playbackTimeoutRef.current);
        }

        playbackTimeoutRef.current = window.setTimeout(() => {
          // Combine all buffered binary chunks
          if (audioBufferRef.current.length > 0) {
            const totalLength = audioBufferRef.current.reduce(
              (acc, arr) => acc + arr.length,
              0
            );
            const combined = new Uint8Array(totalLength);
            let offset = 0;
            for (const chunk of audioBufferRef.current) {
              combined.set(chunk, offset);
              offset += chunk.length;
            }

            console.log("Buffered complete audio:", combined.length, "bytes");

            // Create Blob and queue for playback
            const blob = new Blob([combined], { type: "audio/mpeg" });
            const url = URL.createObjectURL(blob);
            playbackQueueRef.current.push(url);
            audioBufferRef.current = [];

            if (!isPlayingRef.current) {
              playNextInQueue();
            }
          }
        }, 300);
      }
    } catch (e) {
      console.error("Failed to parse data channel message:", e);
    }
  });

  // Cleanup on unmount
  useEffect(() => {
    return () => {
      if (playbackTimeoutRef.current) {
        clearTimeout(playbackTimeoutRef.current);
      }
    };
  }, []);

  // Local VAD (Voice Activity Detection) for instant barge-in
  // Monitors microphone level and stops audio when user speaks - no STT delay
  useEffect(() => {
    if (!isConnected || localTracks.length === 0) return;

    const track = localTracks[0]?.publication?.track;
    if (!track || !track.mediaStreamTrack) return;

    const audioContext = new AudioContext();
    const mediaStream = new MediaStream([track.mediaStreamTrack]);
    const source = audioContext.createMediaStreamSource(mediaStream);
    const analyser = audioContext.createAnalyser();
    analyser.fftSize = 256;

    source.connect(analyser);

    const dataArray = new Uint8Array(analyser.frequencyBinCount);
    let animationId: number;

    const checkAudioLevel = () => {
      analyser.getByteFrequencyData(dataArray);
      const average = dataArray.reduce((a, b) => a + b, 0) / dataArray.length;

      // If user is speaking (level > 30) and we're playing audio, stop immediately
      // Threshold 30 filters out background noise, adjust if needed
      if (average > 50 && isPlayingRef.current) {
        console.log(
          "[LOCAL-VAD] User speaking detected, stopping audio instantly"
        );
        stopAllAudio();
      }

      animationId = requestAnimationFrame(checkAudioLevel);
    };

    checkAudioLevel();

    return () => {
      cancelAnimationFrame(animationId);
      audioContext.close();
    };
  }, [isConnected, localTracks, stopAllAudio]);

  // Play combined audio using Audio element
  const playNextInQueue = () => {
    if (playbackQueueRef.current.length === 0) {
      isPlayingRef.current = false;
      setIsPlaying(false);
      return;
    }

    isPlayingRef.current = true;
    setIsPlaying(true);

    const audioUrl = playbackQueueRef.current.shift()!;

    // Create audio element with blob URL
    const audio = new Audio(audioUrl);
    currentAudioRef.current = audio;

    audio.onended = () => {
      console.log("Audio playback finished");
      // Revoke the blob URL to free memory
      URL.revokeObjectURL(audioUrl);
      playNextInQueue();
    };

    audio.onerror = (e) => {
      console.error("Audio playback error:", e);
      URL.revokeObjectURL(audioUrl);
      playNextInQueue();
    };

    audio
      .play()
      .then(() => {
        console.log("Playing combined audio");
      })
      .catch((e) => {
        console.error("Failed to play audio:", e);
        URL.revokeObjectURL(audioUrl);
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
            : isAgentReady
            ? "Listening..."
            : isConnected && remoteParticipants.length > 0
            ? "Initializing..."
            : isConnected
            ? "Connecting to agent..."
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
          🔊 Audio: {isPlaying ? "Playing" : "Idle"} | Buffer:{" "}
          {audioBufferRef.current?.length || 0} | Tracks: {localTracks.length}
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
