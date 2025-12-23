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

// iOS Check
const isIOS =
  typeof window !== "undefined" &&
  (/iPad|iPhone|iPod/.test(navigator.userAgent) ||
    (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1)) &&
  !(window as any).MSStream;

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

// Module-level deduplication for Strict Mode compatibility
let lastGlobalMessagePayload = "";

interface VoiceUIProps {
  audioContext: AudioContext | null;
}

function VoiceUI({ audioContext }: VoiceUIProps) {
  const connectionState = useConnectionState();
  const localTracks = useTracks([Track.Source.Microphone]);
  const { localParticipant } = useLocalParticipant();
  const isConnected = connectionState === ConnectionState.Connected;

  const [isPlaying, setIsPlaying] = useState(false);
  const [isAgentReady, setIsAgentReady] = useState(false);
  const [bargeInEnabled, setBargeInEnabled] = useState(true);
  const [browserSTTEnabled, setBrowserSTTEnabled] = useState(false);

  // Store format of current stream
  const currentFormatRef = useRef<string>("mp3");
  // Web Speech API Ref
  const recognitionRef = useRef<any>(null);
  const isRecognitionRunningRef = useRef(false);

  // Web Audio API Refs
  const audioContextRef = useRef<AudioContext | null>(null);
  const nextStartTimeRef = useRef<number>(0);
  const isPlayingRef = useRef(false);
  const shouldIgnoreAudioRef = useRef(false);

  // MSE Refs for MP3
  const mediaSourceRef = useRef<MediaSource | null>(null);
  const sourceBufferRef = useRef<SourceBuffer | null>(null);
  const mseQueueRef = useRef<Uint8Array[]>([]);
  const currentAudioRef = useRef<HTMLAudioElement | null>(null);
  const playbackTimeoutRef = useRef<number | null>(null);
  const bargeInResetTimerRef = useRef<number | null>(null); // Safety timer

  // Initialize AudioContext
  // Initialize AudioContext
  const initAudioContext = useCallback(() => {
    // If we were passed a pre-created context (from click handler), use it first
    if (!audioContextRef.current && audioContext) {
      audioContextRef.current = audioContext;
      // Initialize start time relative to this context
      nextStartTimeRef.current = audioContextRef.current.currentTime;
    }

    if (
      !audioContextRef.current ||
      audioContextRef.current.state === "closed"
    ) {
      audioContextRef.current = new (window.AudioContext ||
        (window as any).webkitAudioContext)();
      nextStartTimeRef.current = audioContextRef.current.currentTime;
    }

    // DO NOT reset nextStartTimeRef.current here on every call.
    // It should grow as chunks are scheduled.

    if (audioContextRef.current.state === "suspended") {
      audioContextRef.current.resume();
    }
    return audioContextRef.current;
  }, [audioContext]);

  // Sequential Processing Queue
  const isProcessingQueueRef = useRef(false);
  const audioQueueRef = useRef<Uint8Array[]>([]);

  const processAudioQueue = useCallback(async () => {
    if (isProcessingQueueRef.current || audioQueueRef.current.length === 0)
      return;

    isProcessingQueueRef.current = true;
    const ctx = initAudioContext();

    try {
      while (audioQueueRef.current.length > 0) {
        const bytes = audioQueueRef.current.shift()!;

        let finalBuffer: any = bytes;
        // Handle raw PCM wrapping if needed
        if (currentFormatRef.current === "pcm_s16le") {
          if (!(bytes.length >= 4 && bytes[0] === 0x52 && bytes[1] === 0x49)) {
            finalBuffer = addWavHeader(bytes as any, 44100);
          }
        }

        try {
          // Await decode to ensure serial scheduling
          const decodedBuffer = await ctx.decodeAudioData(
            finalBuffer.buffer as ArrayBuffer
          );

          const now = ctx.currentTime;
          // Schedule for next available slot, or now if we fell behind
          // Add a small buffer (0.05s) to prevent 'glitching' if we are too tight
          const startTime = Math.max(now, nextStartTimeRef.current);

          const source = ctx.createBufferSource();
          source.buffer = decodedBuffer;
          source.connect(ctx.destination);
          source.start(startTime);

          // Update next start time
          nextStartTimeRef.current = startTime + decodedBuffer.duration;

          // UI State
          isPlayingRef.current = true;
          setIsPlaying(true);

          source.onended = () => {
            if (ctx.currentTime >= nextStartTimeRef.current - 0.1) {
              isPlayingRef.current = false;
              setIsPlaying(false);
            }
          };
        } catch (e) {
          console.error("Audio decode error:", e);
        }
      }
    } finally {
      isProcessingQueueRef.current = false;
      // Check if more items arrived while we were processing
      if (audioQueueRef.current.length > 0) {
        processAudioQueue();
      }
    }
  }, [initAudioContext]);

  // Initialize MSE
  const initMSE = useCallback(() => {
    if (mediaSourceRef.current && mediaSourceRef.current.readyState === "open")
      return;

    console.log("[MSE] Initializing MediaSource for MP3");
    const ms = new MediaSource();
    mediaSourceRef.current = ms;

    // Create audio element for MSE
    if (!currentAudioRef.current) {
      const audio = new Audio();
      currentAudioRef.current = audio;

      // Track actual audio playback state via events
      audio.addEventListener("playing", () => {
        console.log("[AUDIO-ELEMENT] playing event");
        isPlayingRef.current = true;
        setIsPlaying(true);
        // Clear any pending timeout since we're actually playing
        if (playbackTimeoutRef.current)
          clearTimeout(playbackTimeoutRef.current);
      });
      audio.addEventListener("waiting", () => {
        console.log("[AUDIO-ELEMENT] waiting for data (buffering)");
        // Don't turn blue while buffering - keep green
      });
      audio.addEventListener("pause", () => {
        console.log("[AUDIO-ELEMENT] paused");
        // Only turn blue if actually paused (not just buffering)
        // Use a short delay to avoid flickering during natural pauses
        if (playbackTimeoutRef.current)
          clearTimeout(playbackTimeoutRef.current);
        playbackTimeoutRef.current = window.setTimeout(() => {
          if (currentAudioRef.current?.paused) {
            isPlayingRef.current = false;
            setIsPlaying(false);
          }
        }, 500);
      });
      audio.addEventListener("ended", () => {
        console.log("[AUDIO-ELEMENT] ended");
        isPlayingRef.current = false;
        setIsPlaying(false);
      });

      audio.src = URL.createObjectURL(ms);
      audio.play().catch((e) => console.error("MSE Play error:", e));
    } else {
      currentAudioRef.current.src = URL.createObjectURL(ms);
      currentAudioRef.current
        .play()
        .catch((e) => console.error("MSE Play error:", e));
    }

    const onSourceOpen = () => {
      console.log("[MSE] SourceOpen");
      if (currentAudioRef.current)
        URL.revokeObjectURL(currentAudioRef.current.src);
      try {
        if (MediaSource.isTypeSupported("audio/mpeg")) {
          if (sourceBufferRef.current) return;
          const sb = ms.addSourceBuffer("audio/mpeg");
          sourceBufferRef.current = sb;
          sb.addEventListener("updateend", () => processMSEQueue());
          setTimeout(processMSEQueue, 0);
        } else {
          console.error("[MSE] audio/mpeg not supported");
        }
      } catch (e) {
        console.error("[MSE] Failed to add SourceBuffer:", e);
      }
    };

    ms.addEventListener("sourceopen", onSourceOpen, { once: true });
  }, []);

  const processMSEQueue = useCallback(() => {
    if (!sourceBufferRef.current || sourceBufferRef.current.updating) return;

    if (mseQueueRef.current.length > 0) {
      const chunk = mseQueueRef.current.shift()!;
      try {
        sourceBufferRef.current.appendBuffer(chunk as any);
      } catch (e) {
        console.error("[MSE] Append error:", e);
      }
    }
  }, []);

  // Stop all audio playback (for barge-in)
  const stopAllAudio = useCallback(
    (fromBargeIn: boolean = false) => {
      // IDEMPOTENCY GUARD: If already ignoring audio, do nothing.
      // This prevents VAD spam from triggering multiple stops/interrupts.
      // Exception: If it's a remote stop command, we might want to ensure cleanup?
      // But for now, sticking to strict idempotency.
      if (shouldIgnoreAudioRef.current) {
        return;
      }

      console.log(
        `[BARGE-IN] Stopping all audio (source: ${
          fromBargeIn ? "local-vad" : "remote"
        })`
      );
      // Ignore any incoming audio until next audio_start
      shouldIgnoreAudioRef.current = true;

      // Stop AudioContext if active
      if (audioContextRef.current) {
        audioContextRef.current.close().catch(console.error);
        audioContextRef.current = null;
      }
      nextStartTimeRef.current = 0;

      // Stop MSE if active
      mseQueueRef.current = [];
      if (sourceBufferRef.current) {
        try {
          if (mediaSourceRef.current?.readyState === "open") {
            sourceBufferRef.current.abort();
          }
        } catch (e) {}
      }
      // Cleanup MSE refs entirely to force re-init
      sourceBufferRef.current = null;
      mediaSourceRef.current = null;

      // Re-create audio element to clear buffer
      if (currentAudioRef.current) {
        currentAudioRef.current.pause();
        currentAudioRef.current.removeAttribute("src");
        currentAudioRef.current.load();
      }

      // Safety: If no new audio starts within 10 seconds, assume false alarm or network issue and un-ignore
      if (bargeInResetTimerRef.current)
        clearTimeout(bargeInResetTimerRef.current);
      bargeInResetTimerRef.current = window.setTimeout(() => {
        console.log("[BARGE-IN] Safety reset: un-ignoring audio");
        shouldIgnoreAudioRef.current = false;
      }, 10000);

      // Send interrupt signal to backend ONLY if triggered locally
      if (fromBargeIn && localParticipant) {
        const msg = JSON.stringify({ type: "interrupt" });
        const encoder = new TextEncoder();
        localParticipant.publishData(encoder.encode(msg), { reliable: true });
      }

      isPlayingRef.current = false;
      setIsPlaying(false);
    },
    [processMSEQueue, localParticipant]
  );

  // Cleanup on unmount
  useEffect(() => {
    return () => {
      if (audioContextRef.current) {
        audioContextRef.current.close().catch(() => {});
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
    let speakingFrames = 0;

    const checkAudioLevel = () => {
      analyser.getByteFrequencyData(dataArray);
      const average = dataArray.reduce((a, b) => a + b, 0) / dataArray.length;

      // Threshold 45 is sensitive, but we require sustained speech (e.g. 5 frames ~ 80ms)
      const isActuallyPlaying =
        isPlayingRef.current ||
        (currentAudioRef.current && !currentAudioRef.current.paused);

      // IMPORTANT: Don't re-trigger if we are already ignoring audio (barge-in active)
      if (
        bargeInEnabled &&
        isActuallyPlaying &&
        !shouldIgnoreAudioRef.current
      ) {
        // Debug log every ~100 frames (approx 1.5s) to check mic levels
        if (speakingFrames === 0 && Math.random() < 0.01) {
          console.log(`[LOCAL-VAD] Current mic level: ${average.toFixed(2)}`);
        }

        if (average > 55) {
          speakingFrames++;
        } else {
          speakingFrames = 0;
        }

        if (speakingFrames > 10) {
          console.log(
            `[LOCAL-VAD] Sustained speech detected (level: ${average.toFixed(
              2
            )}), stopping audio`
          );
          stopAllAudio(true);
          speakingFrames = 0; // Reset
        }
      }

      animationId = requestAnimationFrame(checkAudioLevel);
    };

    checkAudioLevel();

    return () => {
      cancelAnimationFrame(animationId);
      source.disconnect();
      analyser.disconnect();
      audioContext.close().catch(() => {});
    };
  }, [isConnected, localTracks, stopAllAudio, bargeInEnabled]);

  // Handle incoming data channel messages on 'audio' topic
  useDataChannel("audio", (msg) => {
    try {
      const payloadString = new TextDecoder().decode(msg.payload);

      // DEDUPLICATION: Check if this is exactly the same message as last time
      // (Simple check to prevent double-firing in Strict Mode or rapid duplicate sends)
      if (lastGlobalMessagePayload === payloadString) {
        return;
      }
      lastGlobalMessagePayload = payloadString;

      const data: AudioMessage = JSON.parse(payloadString);

      // --- FILTERING LOGIC ---
      // If we interrupted recently, discard any "old" text or audio that might have been in flight.
      // We use a simple memory: if shouldIgnoreAudioRef is true, we ignore.
      // But we also need to ignore *stale* messages that arrive just after we un-ignore.
      // For now, strict "ignore if ignoring" + the backend cancellation should suffice.
      // The user issue "answering the old thing" implies backend sent it.
      // If backend sends it, it means backend didn't cancel fast enough or we have a race.

      if (data.type === "audio_stop") {
        stopAllAudio(false); // Remote stop
        return;
      }

      if (data.type === "audio_end") {
        // Note: Backend sends audio_end per sentence, not per full response.
        // So we don't reset isPlaying here - we use a debounce timeout instead.
        console.log(
          "[AUDIO] audio_end received (per-sentence marker, ignoring for UI)"
        );
        return;
      }

      if (data.type === "audio_start") {
        // New audio stream starting, accept audio again
        console.log("[AUDIO] audio_start received, starting playback");
        shouldIgnoreAudioRef.current = false;
        if (bargeInResetTimerRef.current)
          clearTimeout(bargeInResetTimerRef.current);

        // Clear any pending playback timeout from a previous sentence
        if (playbackTimeoutRef.current)
          clearTimeout(playbackTimeoutRef.current);

        const format = data.format || "mp3";
        currentFormatRef.current = format;

        // Set playing state immediately
        isPlayingRef.current = true;
        setIsPlaying(true);

        // Pick strategy
        // --- STRATEGY: MSE (MP3) ---
        // MSE is disabled on iOS because it lacks full MP3 MediaSource support
        if (currentFormatRef.current === "mp3" && !isIOS) {
          initMSE();
        } else {
          initAudioContext();
        }
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
        // Filter stale text?
        if (shouldIgnoreAudioRef.current) {
          console.log("[BARGE-IN] Ignoring stale text:", data.text);
          return;
        }
        console.log("Agent text:", data.text);
      }

      if (data.type === "audio" && data.audio) {
        // Ignore audio if we're in barge-in stop mode
        if (shouldIgnoreAudioRef.current) {
          return;
        }

        // Decode base64
        const binaryString = atob(data.audio);
        const bytes = new Uint8Array(binaryString.length);
        for (let i = 0; i < binaryString.length; i++) {
          bytes[i] = binaryString.charCodeAt(i);
        }

        // --- STRATEGY: MSE (MP3) ---
        // MSE is disabled on iOS because it lacks full MP3 MediaSource support
        if (currentFormatRef.current === "mp3" && !isIOS) {
          // Self-healing: If we missed audio_start, or just restarted, init MSE now
          if (!mediaSourceRef.current) {
            console.log("[MSE] Lazy init triggered by audio chunk");
            initMSE();
          }

          mseQueueRef.current.push(bytes);
          processMSEQueue();

          // Update UI state
          if (!isPlayingRef.current) {
            isPlayingRef.current = true;
            setIsPlaying(true);
          }
          // Reset UI state timer? MSE plays audio element.
          // We can listen to 'ended' on audio element if streaming stops.
          // But streaming implies continuous. For visualization, we just keep it on while receiving?

          // isPlaying state is now tracked via audio element events (playing, pause, ended)
          // No need for timeout-based detection here
        } else {
          // --- STRATEGY: AudioContext (PCM/WAV) ---
          // Init context if needed (safeguard)
          initAudioContext();

          // Push to queue and process
          audioQueueRef.current.push(bytes);
          processAudioQueue();
        }
      }
    } catch (e) {
      console.error("Failed to parse data channel message:", e);
    }
  });

  // Cleanup on unmount
  useEffect(() => {
    return () => {
      if (audioContextRef.current) {
        audioContextRef.current.close().catch(() => {});
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
    let speakingFrames = 0;

    const checkAudioLevel = () => {
      analyser.getByteFrequencyData(dataArray);
      const average = dataArray.reduce((a, b) => a + b, 0) / dataArray.length;

      // Threshold 45 is sensitive, but we require sustained speech (e.g. 5 frames ~ 80ms)
      const isActuallyPlaying =
        isPlayingRef.current ||
        (currentAudioRef.current && !currentAudioRef.current.paused);

      // IMPORTANT: Don't re-trigger if we are already ignoring audio (barge-in active)
      if (
        bargeInEnabled &&
        isActuallyPlaying &&
        !shouldIgnoreAudioRef.current
      ) {
        // Debug log every ~100 frames (approx 1.5s) to check mic levels
        if (speakingFrames === 0 && Math.random() < 0.01) {
          console.log(`[LOCAL-VAD] Current mic level: ${average.toFixed(2)}`);
        }

        if (average > 55) {
          speakingFrames++;
        } else {
          speakingFrames = 0;
        }

        if (speakingFrames > 10) {
          console.log(
            `[LOCAL-VAD] Sustained speech detected (level: ${average.toFixed(
              2
            )}), stopping audio`
          );
          stopAllAudio(true); // Pass true to send interrupt to backend
          speakingFrames = 0; // Reset
        }
      }

      animationId = requestAnimationFrame(checkAudioLevel);
    };

    checkAudioLevel();

    return () => {
      cancelAnimationFrame(animationId);
      audioContext.close();
    };
  }, [isConnected, localTracks, stopAllAudio, bargeInEnabled]);

  return (
    <div className="min-h-screen bg-zinc-900 flex flex-col items-center justify-center p-8 font-sans text-zinc-100">
      {/* Header */}
      <div className="text-center mb-8 space-y-2">
        <h1 className="text-5xl font-bold tracking-tight bg-gradient-to-r from-white via-blue-100 to-blue-200 bg-clip-text text-transparent">
          Voice Assistant
        </h1>
        <p className="text-zinc-400 text-sm font-medium">Powered by LiveKit</p>
      </div>

      {/* Connection status is now shown in the status text area below */}

      {/* Voice Visualizer Orb */}
      <div className="relative w-64 h-64 flex items-center justify-center -my-10">
        <AgentOrb isPlaying={isPlaying} isConnected={isConnected} />
      </div>

      {/* Status Text */}
      <div className="mt-12 text-center h-8">
        <div
          className={`transition-all duration-300 ${
            isPlaying ? "text-emerald-400" : "text-zinc-400"
          }`}
        >
          {isPlaying ? (
            <span className="flex items-center gap-2 justify-center">
              <span className="w-2 h-2 rounded-full bg-emerald-400 animate-pulse" />
              Speaking...
            </span>
          ) : isConnected && isAgentReady ? (
            <span className="text-sm text-blue-400 animate-pulse">
              Listening...
            </span>
          ) : (
            <span className="text-sm text-zinc-400 animate-pulse">
              Connecting...
            </span>
          )}
        </div>
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
  const [preloadedAudioContext, setPreloadedAudioContext] =
    useState<AudioContext | null>(null);

  const connect = useCallback(async () => {
    try {
      setIsLoading(true);
      setError(null);

      // iOS SAFARI FIX: Create and resume AudioContext immediately inside the click handler
      // This is crucial for audio playback permissions on iOS
      const AudioContextClass =
        window.AudioContext || (window as any).webkitAudioContext;
      const ctx = new AudioContextClass();
      if (ctx.state === "suspended") {
        await ctx.resume();
      }

      // iOS "Wake Up" Trick: Play a silent buffer immediately
      // This forces the audio unlock on some stubborn iOS versions
      try {
        const buffer = ctx.createBuffer(1, 1, 22050);
        const source = ctx.createBufferSource();
        source.buffer = buffer;
        source.connect(ctx.destination);
        source.start(0);
        console.log("[AudioContext] Silent buffer played to wake up engine");
      } catch (e) {
        console.error("[AudioContext] Failed to play silent buffer:", e);
      }

      setPreloadedAudioContext(ctx);

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
    <div className="min-h-screen bg-zinc-900 flex flex-col items-center justify-center p-8 font-sans text-zinc-100">
      {/* Show Connect Screen only if no token */}
      {!tokenData && (
        <>
          <div className="text-center mb-8 space-y-2">
            <h1 className="text-5xl font-bold tracking-tight bg-gradient-to-r from-white via-blue-100 to-blue-200 bg-clip-text text-transparent">
              Voice Assistant
            </h1>
            <p className="text-zinc-400 text-sm font-medium">
              Powered by LiveKit
            </p>
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
          <VoiceUI audioContext={preloadedAudioContext} />
          <RoomAudioRenderer />
        </LiveKitRoom>
      )}
    </div>
  );
}

export default App;
