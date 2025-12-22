import { useState, useCallback, useRef, useEffect } from "react";
import {
  LiveKitRoom,
  RoomAudioRenderer,
  useConnectionState,
  useTracks,
  useDataChannel,
  useLocalParticipant,
} from "@livekit/components-react";
import { ConnectionState, Track } from "livekit-client";
import "@livekit/components-styles";
import "./index.css";
import AgentOrb from "./components/AgentOrb";

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
    | "agent_ready"
    | "config";
  audio?: string; // base64
  sampleRate?: number;
  text?: string;
  config?: { barge_in_enabled: boolean; browser_stt_enabled?: boolean };
  format?: string;
}

// Helper to add WAV header
function addWavHeader(
  samples: Uint8Array,
  sampleRate: number = 44100
): Uint8Array {
  const buffer = new ArrayBuffer(44 + samples.length);
  const view = new DataView(buffer);

  // RIFF identifier
  writeString(view, 0, "RIFF");
  // file length
  view.setUint32(4, 36 + samples.length, true);
  // RIFF type
  writeString(view, 8, "WAVE");
  // format chunk identifier
  writeString(view, 12, "fmt ");
  // format chunk length
  view.setUint32(16, 16, true);
  // sample format (raw)
  view.setUint16(20, 1, true);
  // channel count
  view.setUint16(22, 1, true);
  // sample rate
  view.setUint32(24, sampleRate, true);
  // byte rate (sample rate * block align)
  view.setUint32(28, sampleRate * 2, true);
  // block align (channel count * bytes per sample)
  view.setUint16(32, 2, true);
  // bits per sample
  view.setUint16(34, 16, true);
  // data chunk identifier
  writeString(view, 36, "data");
  // data chunk length
  view.setUint32(40, samples.length, true);

  const bytes = new Uint8Array(buffer);
  bytes.set(samples, 44);
  return bytes;
}

function writeString(view: DataView, offset: number, string: string) {
  for (let i = 0; i < string.length; i++) {
    view.setUint8(offset + i, string.charCodeAt(i));
  }
}

function VoiceUI() {
  const connectionState = useConnectionState();
  const localTracks = useTracks([Track.Source.Microphone]);
  const { localParticipant } = useLocalParticipant();
  const isConnected = connectionState === ConnectionState.Connected;

  const [isPlaying, setIsPlaying] = useState(false);
  const [isAgentReady, setIsAgentReady] = useState(false);
  const [bargeInEnabled, setBargeInEnabled] = useState(true);
  const [browserSTTEnabled, setBrowserSTTEnabled] = useState(false);

  // Buffer for accumulating audio chunks (as binary)
  const audioBufferRef = useRef<Uint8Array[]>([]);
  // Store format of current stream
  const currentFormatRef = useRef<string>("mp3");
  // Web Speech API Ref
  const recognitionRef = useRef<any>(null);
  const isRecognitionRunningRef = useRef(false);

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
    currentFormatRef.current = "mp3"; // Reset default
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

  // Handle incoming data channel messages on 'audio' topic
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
        currentFormatRef.current = data.format || "mp3"; // Capture format if provided
        return;
      }

      if (data.type === "config" && data.config) {
        console.log("[CONFIG] Received:", data.config);
        if (data.config.barge_in_enabled !== undefined)
          setBargeInEnabled(data.config.barge_in_enabled);
        if (data.config.browser_stt_enabled !== undefined)
          setBrowserSTTEnabled(data.config.browser_stt_enabled);
        return;
      }

      if (data.type === "agent_ready") {
        console.log("[AGENT] Agent is ready");
        setIsAgentReady(true);
        return;
      }

      if (data.type === "text" && data.text) {
        console.log("Agent text:", data.text);
      }

      if (data.type === "audio" && data.audio) {
        // Ignore audio if we're in barge-in stop mode
        if (shouldIgnoreAudioRef.current) {
          console.log("[BARGE-IN] Ignoring audio chunk (stopped)");
          return;
        }

        console.log("Received audio chunk:", data.audio.length, "chars");

        // Update format if provided in chunk
        if (data.format) {
          currentFormatRef.current = data.format;
        }

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

            // Detect format
            let mimeType = "audio/mpeg";
            let finalBuffer: any = combined;

            // Check implicit format from stream or chunk
            const format = currentFormatRef.current;

            // Check for RIFF header (if provider sends WAV wrapped)
            const hasRiff =
              combined.length >= 4 &&
              combined[0] === 0x52 &&
              combined[1] === 0x49 &&
              combined[2] === 0x46 &&
              combined[3] === 0x46;

            if (format === "pcm_s16le" && !hasRiff) {
              // Wrap raw PCM in WAV header
              console.log("Wrapping Raw PCM in WAV header");
              finalBuffer = addWavHeader(combined as any, 44100);
              mimeType = "audio/wav";
            } else if (hasRiff) {
              console.log("Detected existing WAV header");
              mimeType = "audio/wav";
            }

            // Create Blob and queue for playback
            const blob = new Blob([finalBuffer], { type: mimeType });
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
      if (recognitionRef.current) {
        recognitionRef.current.stop();
      }
    };
  }, []);

  // Browser Native STT Logic
  useEffect(() => {
    if (
      browserSTTEnabled &&
      isConnected &&
      "webkitSpeechRecognition" in window
    ) {
      console.log("[BROWSER-STT] Initializing Speech Recognition");
      // @ts-ignore
      const recognition = new window.webkitSpeechRecognition();
      recognition.continuous = true;
      recognition.interimResults = false;
      recognition.lang = "en-US";

      recognition.onstart = () => {
        console.log("[BROWSER-STT] Started listening");
        isRecognitionRunningRef.current = true;
      };

      recognition.onerror = (event: any) => {
        console.error("[BROWSER-STT] Error:", event.error);
      };

      recognition.onend = () => {
        console.log("[BROWSER-STT] Ended");
        isRecognitionRunningRef.current = false;
        // Auto-restart if still enabled
        if (browserSTTEnabled && isConnected) {
          console.log("[BROWSER-STT] Restarting...");
          try {
            recognition.start();
          } catch (e) {}
        }
      };

      recognition.onresult = (event: any) => {
        const transcript =
          event.results[event.results.length - 1][0].transcript;
        console.log("[BROWSER-STT] Recognized:", transcript);

        if (transcript.trim().length > 0) {
          // Send to backend via Data Channel
          const msg = JSON.stringify({ type: "chat_input", text: transcript });
          const encoder = new TextEncoder();
          const payload = encoder.encode(msg);

          if (localParticipant) {
            localParticipant.publishData(payload, { reliable: true });
          }
        }
      };

      recognitionRef.current = recognition;
      try {
        recognition.start();
      } catch (e) {
        console.error(e);
      }

      return () => {
        if (recognitionRef.current) {
          recognitionRef.current.stop();
          recognitionRef.current = null;
        }
      };
    }
  }, [browserSTTEnabled, isConnected, localParticipant]);

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

      // If user is speaking (level > 20) and we're playing audio, stop immediately
      // Threshold 50 filters out quiet background noise but catches speech quickly
      if (bargeInEnabled && average > 50 && isPlayingRef.current) {
        console.log(
          `[LOCAL-VAD] User speaking detected (level: ${average.toFixed(
            2
          )}), stopping audio instantly`
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
  }, [isConnected, localTracks, stopAllAudio, bargeInEnabled]);

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
    <div className="min-h-screen bg-zinc-950 flex flex-col items-center justify-center p-8 font-sans text-zinc-100">
      {/* Header */}
      <div className="text-center mb-8 space-y-2">
        <h1 className="text-3xl font-semibold tracking-tight">
          Voice Assistant
        </h1>
        <p className="text-zinc-400 text-sm">Powered by LiveKit</p>
      </div>

      {/* Main Connection Check */}
      {!isConnected && (
        <div className="text-zinc-500 animate-pulse text-sm">
          Connecting to server...
        </div>
      )}

      {/* Voice Visualizer Orb */}
      <div className="relative w-64 h-64 flex items-center justify-center -my-10">
        <AgentOrb isPlaying={isPlaying} isConnected={isConnected} />
      </div>

      {/* Status Text */}
      <div className="mt-12 text-center h-8">
        {isConnected && (
          <div
            className={`transition-all duration-300 ${
              isPlaying ? "text-emerald-400" : "text-zinc-400"
            }`}
          >
            {isPlaying ? (
              <span className="flex items-center gap-2">
                <span className="w-2 h-2 rounded-full bg-emerald-400 animate-pulse" />
                Speaking...
              </span>
            ) : (
              <span
                className={`text-sm ${
                  isAgentReady ? "text-blue-400 animate-pulse" : "text-zinc-500"
                }`}
              >
                {isAgentReady ? "Listening..." : "Connecting..."}
              </span>
            )}
          </div>
        )}
      </div>

      {/* End Call Button */}
      <div className="mt-12">
        <button
          onClick={() => window.location.reload()} // Simple restart
          className="px-6 py-2 rounded-full border border-red-900/30 text-red-400 hover:bg-red-950/30 hover:border-red-800 transition-all text-sm"
        >
          End Conversation
        </button>
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
    <div className="min-h-screen bg-zinc-950 flex flex-col items-center justify-center p-8 font-sans text-zinc-100">
      {/* Show Connect Screen only if no token */}
      {!tokenData && (
        <>
          <div className="text-center mb-8 space-y-2">
            <h1 className="text-3xl font-semibold tracking-tight">
              Voice Assistant
            </h1>
            <p className="text-zinc-400 text-sm">Powered by LiveKit</p>
          </div>

          {!isLoading && (
            <div className="flex flex-col items-center gap-6">
              <button
                onClick={connect}
                className="px-8 py-3 bg-zinc-100 text-zinc-950 font-medium rounded-full hover:bg-zinc-200 transition-all duration-300"
              >
                Start Conversation
              </button>
              {error && <p className="text-red-400 text-sm">{error}</p>}
            </div>
          )}

          {isLoading && (
            <div className="flex flex-col items-center gap-4">
              <div className="w-8 h-8 border-2 border-zinc-500 border-t-zinc-200 rounded-full animate-spin" />
              <p className="text-zinc-500 text-sm">Connecting...</p>
            </div>
          )}
        </>
      )}

      {/* When Connected, LiveKitRoom takes over (VoiceUI handles the layout) */}
      {tokenData && (
        <LiveKitRoom
          serverUrl={tokenData.url}
          token={tokenData.token}
          connect={true}
          audio={true}
          video={false}
          onDisconnected={disconnect}
          className="w-full h-full"
        >
          <VoiceUI />
          <RoomAudioRenderer />
        </LiveKitRoom>
      )}
    </div>
  );
}

export default App;
