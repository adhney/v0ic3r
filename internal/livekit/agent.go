package livekit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/adhney/voice-agent/internal/llm"
	"github.com/adhney/voice-agent/internal/stt"
	"github.com/adhney/voice-agent/internal/tts"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
)

type ttsJob struct {
	text         string
	ctx          context.Context
	generationID uint64 // Unique ID for each response generation
}

// VoiceAgent handles the voice interaction pipeline within a LiveKit room
type VoiceAgent struct {
	client   *Client
	room     *lksdk.Room
	roomName string

	// Service clients
	sttClient *stt.DeepgramClient
	llmClient *llm.GeminiClient
	ttsClient tts.TTSClient

	// Audio channels
	ttsAudioChan chan []byte
	ttsQueue     chan ttsJob

	// Audio processing
	audioBuffer   chan []byte
	transcriptBuf strings.Builder
	debounceTimer *time.Timer

	// Interrupt handling
	interruptPlayback bool      // Set when user speaks during TTS
	isPlaying         bool      // True when TTS audio is being sent
	lastAudioSentTime time.Time // Track when audio was last sent for smart barge-in
	isProcessing      bool      // Prevent concurrent LLM/TTS requests
	cancelRequest     func()    // Cancel current LLM/TTS request on barge-in
	lastInterruptTime time.Time // Track when barge-in occurred for cooldown
	enableBargeIn     bool      // Feature flag for interruptions
	enableBrowserSTT  bool      // Feature flag for browser-native STT
	ttsProvider       string    // "elevenlabs" or "cartesia"
	currentGenID      uint64    // Current generation ID - incremented on each interrupt
	lastProcessTime   time.Time // Track when we last started processing to de-duplicate

	ctx        context.Context
	cancelFunc context.CancelFunc

	mu     sync.Mutex
	closed bool
}

// AgentConfig contains configuration for the voice agent
type AgentConfig struct {
	DeepgramAPIKey   string
	GeminiAPIKey     string
	ElevenLabsAPIKey string
	CartesiaAPIKey   string
	TTSProvider      string // "elevenlabs" or "cartesia"
	EnableBargeIn    bool
	EnableBrowserSTT bool
}

// AudioMessage sent via data channel
type AudioMessage struct {
	Type       string                 `json:"type"`
	Audio      string                 `json:"audio"` // base64 encoded
	SampleRate int                    `json:"sampleRate"`
	Text       string                 `json:"text,omitempty"`   // for transcript display
	Config     map[string]interface{} `json:"config,omitempty"` // for feature flags
	Format     string                 `json:"format,omitempty"` // "mp3" or "pcm_s16le"
}

// NewVoiceAgentWithConfig creates a new voice agent with full pipeline
func NewVoiceAgentWithConfig(client *Client, roomName string, cfg AgentConfig) (*VoiceAgent, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// Initialize STT client
	sttClient := stt.NewDeepgramClient(cfg.DeepgramAPIKey)

	// Initialize LLM client
	llmClient, err := llm.NewGeminiClient(ctx, cfg.GeminiAPIKey)
	if err != nil {
		cancel()
		return nil, err
	}

	// Initialize TTS client
	var ttsClient tts.TTSClient
	if cfg.TTSProvider == "cartesia" {
		ttsClient = tts.NewCartesiaClient(cfg.CartesiaAPIKey)
		log.Printf("[INIT] Using Cartesia TTS")
	} else {
		ttsClient = tts.NewElevenLabsClient(cfg.ElevenLabsAPIKey)
		log.Printf("[INIT] Using ElevenLabs TTS")
	}

	return &VoiceAgent{
		client:           client,
		roomName:         roomName,
		sttClient:        sttClient,
		llmClient:        llmClient,
		ttsClient:        ttsClient,
		audioBuffer:      make(chan []byte, 100),
		ttsAudioChan:     make(chan []byte, 50),
		ttsQueue:         make(chan ttsJob, 50),
		ctx:              ctx,
		cancelFunc:       cancel,
		enableBargeIn:    cfg.EnableBargeIn,
		enableBrowserSTT: cfg.EnableBrowserSTT,
		ttsProvider:      cfg.TTSProvider,
	}, nil
}

// NewVoiceAgent creates a basic voice agent (for backward compatibility)
func NewVoiceAgent(client *Client, roomName string) *VoiceAgent {
	return &VoiceAgent{
		client:       client,
		roomName:     roomName,
		audioBuffer:  make(chan []byte, 100),
		ttsAudioChan: make(chan []byte, 50),
	}
}

// Start connects to the LiveKit room and begins processing
func (a *VoiceAgent) Start(ctx context.Context) error {
	if ctx != nil {
		a.ctx = ctx
	}
	if a.ctx == nil {
		a.ctx = context.Background()
	}

	callback := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: a.onTrackSubscribed,
			OnTrackUnsubscribed: func(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				log.Printf("Track unsubscribed from %s", rp.Identity())
			},
			OnDataReceived: a.onDataPacket,
		},
		OnParticipantConnected: func(rp *lksdk.RemoteParticipant) {
			log.Printf("Participant connected: %s", rp.Identity())
		},
		OnParticipantDisconnected: func(rp *lksdk.RemoteParticipant) {
			log.Printf("Participant disconnected: %s", rp.Identity())
		},
	}

	room, err := a.client.JoinRoomAsAgent(a.ctx, a.roomName, callback)
	if err != nil {
		return err
	}

	a.room = room
	log.Printf("Voice agent started in room: %s", a.roomName)

	// Start TTS audio sender via data channel
	go a.runAudioSender()

	// Start TTS worker
	go a.runTTSWorker()

	// Bridge TTS client audio to agent's audio sender
	if a.ttsClient != nil {
		go func() {
			for chunk := range a.ttsClient.AudioChannel() {
				a.ttsAudioChan <- chunk
			}
		}()
	}

	// Start STT and TTS connections if initialized
	// If Browser STT is enabled, we don't connect backend STT
	if a.sttClient != nil && !a.enableBrowserSTT {
		if err := a.sttClient.Connect(a.ctx); err != nil {
			log.Printf("Failed to connect STT: %v", err)
		}

		// Start transcript processing
		go a.processTranscripts()

		// Start SpeechStarted listener for barge-in (Vocalis-style)
		go a.listenForSpeechStart()
	}

	if a.ttsClient != nil {
		// TTS client connection/warmup handled internally or lazy-loaded
	}

	// Send configuration to frontend
	// Send configuration to frontend
	a.sendAudioControl("config", map[string]interface{}{
		"barge_in_enabled":    a.enableBargeIn,
		"browser_stt_enabled": a.enableBrowserSTT,
	})
	// Signal to frontend that agent is fully ready
	a.sendAudioControl("agent_ready", nil)
	log.Printf("[AGENT] Sent agent_ready signal to frontend (BargeIn: %v)", a.enableBargeIn)

	return nil
}

// sendTTSAudioViaDataChannel sends TTS audio to browser via data channel
func (a *VoiceAgent) sendTTSAudioViaDataChannel() {
	for {
		select {
		case <-a.ctx.Done():
			return
		case pcmData := <-a.ttsClient.AudioChannel():
			if len(pcmData) == 0 {
				continue
			}

			// Check for end-of-audio marker (0xFF single byte)
			if len(pcmData) == 1 && pcmData[0] == 0xFF {
				a.mu.Lock()
				a.isPlaying = false
				a.mu.Unlock()
				a.sendAudioControl("audio_end")
				log.Printf("[AUDIO-CONTROL] Audio playback ended")
				continue
			}

			// Check for interrupt before sending
			a.mu.Lock()
			if a.interruptPlayback {
				a.mu.Unlock()
				log.Printf("[BARGE-IN] Audio interrupted, skipping chunk")
				continue
			}

			// Mark as playing and send audio_start if first chunk
			if !a.isPlaying {
				a.isPlaying = true
				a.mu.Unlock()
				a.sendAudioControl("audio_start")
				a.mu.Lock()
			}
			a.mu.Unlock()

			log.Printf("Sending TTS audio via data channel: %d bytes", len(pcmData))

			// Determine format based on provider
			format := "mp3"
			if a.ttsProvider == "cartesia" {
				format = "pcm_s16le"
			}

			// Encode audio as base64 and send via data channel
			msg := AudioMessage{
				Type:       "audio",
				Audio:      base64.StdEncoding.EncodeToString(pcmData),
				SampleRate: 44100, // Match ElevenLabs output
				Format:     format,
			}

			jsonData, err := json.Marshal(msg)
			if err != nil {
				log.Printf("Failed to marshal audio message: %v", err)
				continue
			}

			// Publish data using UserData with topic
			if a.room != nil {
				userData := lksdk.UserData(jsonData)
				err = a.room.LocalParticipant.PublishDataPacket(
					userData,
					lksdk.WithDataPublishReliable(true),
					lksdk.WithDataPublishTopic("audio"),
				)
				if err != nil {
					log.Printf("Failed to send data packet: %v", err)
				} else {
					log.Printf("Sent audio data packet: %d bytes", len(jsonData))
					// Track when we last sent audio for smart barge-in
					a.mu.Lock()
					a.lastAudioSentTime = time.Now()
					a.mu.Unlock()
				}
			}
		}
	}
}

// sendAudioControl sends audio control messages (audio_start, audio_end, audio_stop)
func (a *VoiceAgent) sendAudioControl(controlType string, config ...map[string]interface{}) {
	msg := AudioMessage{
		Type: controlType,
	}
	if len(config) > 0 && config[0] != nil {
		msg.Config = config[0]
	}

	jsonData, err := json.Marshal(msg)
	if err != nil {
		return
	}

	if a.room != nil {
		userData := lksdk.UserData(jsonData)
		a.room.LocalParticipant.PublishDataPacket(
			userData,
			lksdk.WithDataPublishReliable(true),
			lksdk.WithDataPublishTopic("audio"),
		)
		log.Printf("[AUDIO-CONTROL] Sent %s", controlType)
	}
}

// triggerInterrupt sets the interrupt flag and clears pending audio (barge-in)
func (a *VoiceAgent) triggerInterrupt() {
	if !a.enableBargeIn {
		// Log sparingly or once
		return
	}
	a.mu.Lock()
	// Check if audio was recently sent (within 10s) - this is more reliable than isPlaying
	// because backend finishes sending in ~1s but frontend plays for ~5-10s
	sinceLastAudio := time.Since(a.lastAudioSentTime)
	shouldInterrupt := a.isPlaying || sinceLastAudio < 10*time.Second

	if shouldInterrupt {
		a.interruptPlayback = true
		a.isPlaying = false

		// Clear transcript buffer so old text doesn't mix with new speech
		a.transcriptBuf.Reset()

		// Stop debounce timer to prevent processing stale transcript
		if a.debounceTimer != nil {
			a.debounceTimer.Stop()
			a.debounceTimer = nil
		}

		a.mu.Unlock()

		// Cancel any ongoing LLM request
		a.mu.Lock()
		if a.cancelRequest != nil {
			a.cancelRequest()
			log.Printf("[BARGE-IN] Cancelled LLM request")
		}
		a.mu.Unlock()

		// Cancel the TTS generation
		if a.ttsClient != nil {
			a.ttsClient.Cancel()
		}

		// Drain the TTS audio channel
		for {
			select {
			case <-a.ttsAudioChan:
				// Discard pending audio
			default:
				goto drainQueue
			}
		}
	drainQueue:
		// Drain the TTS sentence queue so pending sentences aren't spoken
		for {
			select {
			case job := <-a.ttsQueue:
				log.Printf("[BARGE-IN] Discarded queued sentence: %q", job.text)
			default:
				goto done
			}
		}
	done:
		// Notify frontend to stop audio
		a.sendAudioControl("audio_stop")
		log.Printf("[BARGE-IN] Interrupt triggered, TTS cancelled, transcript cleared")

		a.mu.Lock()
		a.interruptPlayback = false
		// Increment generation ID to invalidate any remaining TTS jobs from old generation
		a.currentGenID++
		log.Printf("[BARGE-IN] Incremented generation ID to %d", a.currentGenID)
		// Don't manually reset isProcessing - let the cancelled request's defer handle it
		// to prevent race conditions where new request starts before old one finishes cleanup
		a.lastInterruptTime = time.Now() // Track when barge-in occurred
		a.mu.Unlock()
	} else {
		a.mu.Unlock()
	}
}

// listenForSpeechStart listens for Deepgram's SpeechStarted events
// and triggers barge-in interrupt when user speaks during TTS playback
func (a *VoiceAgent) listenForSpeechStart() {
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-a.sttClient.SpeechStarted:
			a.mu.Lock()
			isPlaying := a.isPlaying
			a.mu.Unlock()

			log.Printf("[BARGE-IN-DEBUG] SpeechStarted event received. isPlaying=%v", isPlaying)

			if isPlaying {
				// log.Printf("[BARGE-IN] User started speaking while TTS playing, triggering interrupt")
				// a.triggerInterrupt()
				log.Printf("[BARGE-IN-DEBUG] SpeechStarted detected but backend interrupt DISABLED (relying on frontend VAD)")
			} else {
				log.Printf("[BARGE-IN-DEBUG] Ignored SpeechStarted because isPlaying=false")
			}
		}
	}
}

// processTranscripts handles incoming transcripts from Deepgram
func (a *VoiceAgent) processTranscripts() {

	processFn := func() {
		processStart := time.Now()

		a.mu.Lock()
		// De-duplication: Skip if we processed very recently (within 500ms)
		// This handles duplicate UtteranceEnd events from Deepgram
		if time.Since(a.lastProcessTime) < 500*time.Millisecond {
			a.mu.Unlock()
			log.Printf("[DEDUP] Skipping duplicate processing request (last: %vms ago)",
				time.Since(a.lastProcessTime).Milliseconds())
			return
		}

		// If already processing, skip (don't interrupt - let it complete)
		if a.isProcessing {
			a.mu.Unlock()
			log.Printf("[DEDUP] Skipping processing, already in progress")
			return
		}
		a.isProcessing = true
		a.lastProcessTime = processStart

		// Check if we recently did a barge-in - wait for user to finish speaking
		sinceInterrupt := time.Since(a.lastInterruptTime)
		if sinceInterrupt < 1500*time.Millisecond && !a.lastInterruptTime.IsZero() {
			waitTime := 1500*time.Millisecond - sinceInterrupt
			a.mu.Unlock()
			log.Printf("[BARGE-IN] Cooling down, waiting %vms for user to finish", waitTime.Milliseconds())
			time.Sleep(waitTime)
			a.mu.Lock()
		}

		fullUtterance := strings.TrimSpace(a.transcriptBuf.String())
		a.transcriptBuf.Reset()
		// Capture current generation ID for this request
		genID := a.currentGenID
		a.mu.Unlock()

		// Reset isProcessing when done
		defer func() {
			a.mu.Lock()
			a.isProcessing = false
			a.mu.Unlock()
		}()

		if fullUtterance == "" {
			return
		}

		log.Printf("[LATENCY] STT → Agent: utterance received at %v", processStart.Format("15:04:05.000"))
		log.Printf("[LATENCY] Processing utterance: %q", fullUtterance)

		// Create cancellable context for this request - can be aborted on barge-in
		requestCtx, cancel := context.WithCancel(a.ctx)
		a.mu.Lock()
		a.cancelRequest = cancel
		a.mu.Unlock()
		defer cancel() // Cleanup when done

		// Get LLM response and stream sentences to TTS immediately (low-latency)
		llmStart := time.Now()
		log.Printf("[LATENCY] LLM request started at %v (delta: +%vms from STT)",
			llmStart.Format("15:04:05.000"), llmStart.Sub(processStart).Milliseconds())

		responseStream, err := a.llmClient.GenerateResponse(requestCtx, fullUtterance)
		if err != nil {
			log.Printf("LLM Error: %v", err)
			return
		}

		// Stream sentences to TTS as they complete - don't wait for full response
		var completeResponse strings.Builder
		var buffer strings.Builder
		firstChunk := true

		for chunk := range responseStream {
			// Check for context cancellation (barge-in) at the start of each chunk
			if requestCtx.Err() != nil {
				log.Printf("[BARGE-IN] LLM streaming cancelled, stopping loop")
				break
			}

			// Check if generation ID changed (barge-in occurred)
			a.mu.Lock()
			currentGen := a.currentGenID
			a.mu.Unlock()
			if genID < currentGen {
				log.Printf("[BARGE-IN] Generation changed (%d -> %d), stopping LLM streaming", genID, currentGen)
				break
			}

			if firstChunk {
				log.Printf("[LATENCY] LLM first chunk at %v (delta: +%vms from LLM start)",
					time.Now().Format("15:04:05.000"), time.Since(llmStart).Milliseconds())
				firstChunk = false
			}

			buffer.WriteString(chunk)
			completeResponse.WriteString(chunk)

			// Sentence boundary detection - send to TTS as soon as sentence ends
			current := buffer.String()
			runes := []rune(current)
			length := len(runes)
			processedPos := 0

			for i := 0; i < length; i++ {
				r := runes[i]
				isEnd := false

				if r == '.' || r == '!' || r == '?' {
					if i+1 >= length {
						// End of buffer - wait for next chunk
					} else if runes[i+1] == ' ' || runes[i+1] == '\n' || runes[i+1] == '\t' {
						isEnd = true
					}
				}

				if isEnd {
					sentence := strings.TrimSpace(string(runes[processedPos : i+1]))
					if sentence != "" {
						// Use a.ctx instead of requestCtx so jobs aren't cancelled when processFn returns
						a.ttsQueue <- ttsJob{
							text:         sentence,
							ctx:          a.ctx,
							generationID: genID,
						}
						log.Printf("[TTS-STREAM] Queued sentence (gen %d): %q", genID, sentence)
					}
					processedPos = i + 1
				}
			}

			// Keep remainder in buffer
			if processedPos > 0 {
				if processedPos < length {
					buffer.Reset()
					buffer.WriteString(string(runes[processedPos:]))
				} else {
					buffer.Reset()
				}
			}
		}

		// Flush remaining text
		remaining := strings.TrimSpace(buffer.String())
		if remaining != "" {
			a.ttsQueue <- ttsJob{
				text:         remaining,
				ctx:          a.ctx,
				generationID: genID,
			}
			log.Printf("[TTS-STREAM] Queued remaining (gen %d): %q", genID, remaining)
		}

		llmEnd := time.Now()
		log.Printf("[LATENCY] LLM complete at %v (total LLM: %vms)",
			llmEnd.Format("15:04:05.000"), llmEnd.Sub(llmStart).Milliseconds())

		response := completeResponse.String()
		if response != "" {
			log.Printf("Agent response: %s", response)
			// Send text response via data channel
			a.sendTextMessage(response)
		}
	}

	const debounceDelay = 500 * time.Millisecond

	// Helper to trigger processing immediately
	triggerProcessing := func() {
		a.mu.Lock()
		if a.debounceTimer != nil {
			a.debounceTimer.Stop()
			a.debounceTimer = nil
		}
		a.mu.Unlock()
		// Execute processing
		processFn()
	}

	// Listen for UtteranceEnd signals (now with faster 500ms config)
	go func() {
		for range a.sttClient.UtteranceEnd {
			log.Printf("[STT] UtteranceEnd detected, triggering processing")
			triggerProcessing()
		}
	}()

	for transcript := range a.sttClient.Transcript {
		if transcript == "" {
			continue
		}

		// Only send audio_stop if we recently sent audio (within last 10s)
		a.mu.Lock()
		sinceLastAudio := time.Since(a.lastAudioSentTime)
		a.mu.Unlock()

		if sinceLastAudio < 10*time.Second {
			log.Printf("[BARGE-IN] Transcript received, interrupting audio")
			a.sendAudioControl("audio_stop")
			a.triggerInterrupt()
		}

		a.mu.Lock()
		if a.transcriptBuf.Len() > 0 {
			a.transcriptBuf.WriteString(" ")
		}
		a.transcriptBuf.WriteString(transcript)

		// Reset debounce timer
		if a.debounceTimer != nil {
			a.debounceTimer.Stop()
		}
		a.debounceTimer = time.AfterFunc(debounceDelay, processFn)
		a.mu.Unlock()
	}

}

// sendTextMessage sends text response via data channel
func (a *VoiceAgent) sendTextMessage(text string) {
	msg := AudioMessage{
		Type: "text",
		Text: text,
	}

	jsonData, err := json.Marshal(msg)
	if err != nil {
		return
	}

	if a.room != nil {
		userData := lksdk.UserData(jsonData)
		a.room.LocalParticipant.PublishDataPacket(
			userData,
			lksdk.WithDataPublishReliable(true),
			lksdk.WithDataPublishTopic("audio"),
		)
	}
}

// onTrackSubscribed handles incoming audio tracks from participants
func (a *VoiceAgent) onTrackSubscribed(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	log.Printf("Subscribed to track %s from %s (codec: %s)",
		track.ID(), rp.Identity(), track.Codec().MimeType)

	if track.Kind() != webrtc.RTPCodecTypeAudio {
		return
	}

	// Start goroutine to read audio from the track
	go a.processAudioTrack(track, rp)
}

// processAudioTrack reads audio data from a track and sends to STT
func (a *VoiceAgent) processAudioTrack(track *webrtc.TrackRemote, rp *lksdk.RemoteParticipant) {
	codecType := track.Codec().MimeType
	log.Printf("Processing audio track from: %s (codec: %s, PayloadType: %d)",
		rp.Identity(), codecType, track.Codec().PayloadType)

	packetCount := 0
	bytesSent := 0

	for {
		a.mu.Lock()
		if a.closed {
			a.mu.Unlock()
			return
		}
		a.mu.Unlock()

		// Read RTP packet
		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			log.Printf("Error reading RTP: %v", err)
			return
		}

		packetCount++

		// The RTP payload is Opus audio (possibly wrapped in RED)
		payload := rtpPacket.Payload

		// Check if this is RED encoded - need to properly extract primary Opus data
		if codecType == "audio/red" {
			// RED header: F(1) + blockPT(7) for primary codec
			// If F=0, it's the primary (last) block header (1 byte)
			// We need to skip all redundant blocks and get to primary
			if len(payload) > 0 {
				// Find the primary block (first byte with F=0)
				offset := 0
				for offset < len(payload) {
					if payload[offset]&0x80 == 0 {
						// F=0, this is the primary block header
						offset++ // Skip the 1-byte header
						break
					}
					// F=1, this is a redundant block header (4 bytes)
					offset += 4
				}
				if offset < len(payload) {
					payload = payload[offset:]
				}
			}
		}

		// Log first packet and every 500th with hex prefix for debugging
		if packetCount == 1 || packetCount%500 == 0 {
			hexPrefix := ""
			if len(payload) >= 4 {
				hexPrefix = fmt.Sprintf("0x%02x%02x%02x%02x", payload[0], payload[1], payload[2], payload[3])
			}
			log.Printf("[AUDIO-DEBUG] Packet #%d: codec=%s, size=%d bytes, prefix=%s",
				packetCount, codecType, len(payload), hexPrefix)
		}

		// Log every 100 packets
		if packetCount%100 == 1 {
			log.Printf("Audio packet #%d, size: %d bytes", packetCount, len(payload))
		}

		// Skip DTX (Discontinuous Transmission) packets - these are silence indicators
		// Opus DTX packets are typically 1-3 bytes and contain no useful audio
		if len(payload) < 3 {
			// Very small packet - likely DTX silence frame, skip
			continue
		}

		// Check for Opus comfort noise / DTX frames
		// Opus TOC byte: first 2 bits are config, if config is certain values it's CELT-only silence
		// Skip packets that are likely comfort noise (very small with specific patterns)
		// A normal Opus speech packet is typically 20-200+ bytes
		if len(payload) < 10 {
			// Very short packet - likely just a silence descriptor
			continue
		}

		// Send to STT - only real audio content now
		if a.sttClient != nil && len(payload) > 0 {
			if err := a.sttClient.SendAudio(payload); err != nil {
				log.Printf("STT Send Error: %v", err)
			} else {
				bytesSent += len(payload)
				if packetCount%100 == 1 {
					log.Printf("Sent %d total bytes to STT", bytesSent)
				}
			}
		}
	}
}

// Stop gracefully shuts down the voice agent
func (a *VoiceAgent) Stop() {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()

	if a.cancelFunc != nil {
		a.cancelFunc()
	}

	close(a.audioBuffer)

	if a.room != nil {
		a.room.Disconnect()
	}

	if a.sttClient != nil {
		a.sttClient.Close()
	}
	if a.llmClient != nil {
		a.llmClient.Close()
	}
	if a.ttsClient != nil {
		a.ttsClient.Close()
	}

	log.Printf("Voice agent stopped")
}

// onDataPacket handles incoming data from the frontend
func (a *VoiceAgent) onDataPacket(data []byte, params lksdk.DataReceiveParams) {
	// Handle data packet
	// Ensure it's text
	var msg map[string]interface{}
	if err := json.Unmarshal(data, &msg); err != nil {
		// likely not JSON or not for us
		return
	}

	if typeStr, ok := msg["type"].(string); ok {
		switch typeStr {
		case "chat_input":
			if text, ok := msg["text"].(string); ok && text != "" {
				log.Printf("Received chat input: %s", text)
				go a.ProcessUserText(text)
			}
		case "interrupt":
			log.Printf("=== [BARGE-IN] Received interrupt signal from frontend ===")
			a.sendAudioControl("audio_stop") // Acknowledge with stop
			a.triggerInterrupt()
			log.Printf("=== [BARGE-IN] Interrupt processing complete ===")
		}
	}
}

// ProcessUserText processes text input (e.g. from browser STT)
func (a *VoiceAgent) ProcessUserText(text string) {
	log.Printf("[TEXT-INPUT] Processing text: %q", text)

	// interrupt
	a.triggerInterrupt()

	// Process response
	go a.processProcessingFlow(text)
}

// processProcessingFlow encapsulates the logic from processTranscripts
// to be reusable for chat input
func (a *VoiceAgent) processProcessingFlow(text string) {
	// Mark processing start
	a.mu.Lock()
	if a.isProcessing {
		a.mu.Unlock()
		return
	}
	a.isProcessing = true
	// Cancel any ongoing request context
	if a.cancelRequest != nil {
		a.cancelRequest()
	}
	// Create new cancellable request context
	reqCtx, cancel := context.WithCancel(a.ctx)
	a.cancelRequest = cancel
	// Capture current generation ID for this request
	genID := a.currentGenID
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.isProcessing = false
		if a.cancelRequest != nil {
			a.cancelRequest() // Clean up context
			a.cancelRequest = nil
		}
		a.mu.Unlock()
	}()

	start := time.Now()
	log.Printf("[LATENCY] Processing utterance: %q", text)

	var completeResponse strings.Builder

	// 1. Generate Response (Gemini)
	llmStart := time.Now()
	log.Printf("[LATENCY] LLM request started at %s", llmStart.Format("15:04:05.000"))

	respChan, err := a.llmClient.GenerateResponse(reqCtx, text)
	if err != nil {
		log.Printf("LLM Error: %v", err)
		return
	}
	log.Printf("[LATENCY] LLM stream started at %s", time.Now().Format("15:04:05.000"))

	// 2. Stream Sentences to TTS Worker
	var buffer strings.Builder
	firstChunk := true

	for chunk := range respChan {
		// Check for context cancellation (barge-in) at the start of each chunk
		if reqCtx.Err() != nil {
			log.Printf("[BARGE-IN] LLM streaming cancelled in processProcessingFlow")
			break
		}

		// Check if generation ID changed (barge-in occurred)
		a.mu.Lock()
		currentGen := a.currentGenID
		a.mu.Unlock()
		if genID < currentGen {
			log.Printf("[BARGE-IN] Generation changed (%d -> %d), stopping LLM streaming", genID, currentGen)
			break
		}

		if firstChunk {
			log.Printf("[LATENCY] LLM first token: %s", time.Now().Format("15:04:05.000"))
			firstChunk = false
		}

		buffer.WriteString(chunk)
		completeResponse.WriteString(chunk)

		// Simple sentence boundary detection
		// Check for punctuation followed by space or newline
		// Optimized: only check if chunk contained punctuation to avoid scanning huge buffer constantly
		// But buffer grows, so we scan from recent position?
		// For simplicity, just check the whole buffer (sentences are short)

		current := buffer.String()
		// Split logic: look for [.!?] followed by space/end?
		// We'll iterate to separate multiple sentences if they came in one chunk

		// Regex is heavy, manual scan:
		processedPos := 0
		runes := []rune(current)
		length := len(runes)

		for i := 0; i < length; i++ {
			r := runes[i]
			isEnd := false

			// Check delimiters
			if r == '.' || r == '!' || r == '?' {
				// Check next char (needs to be space, newline, or End of String)
				if i+1 >= length {
					// End of current buffer - this MIGHT be end of sentence,
					// but wait for next chunk to confirm it's not "Dr." or "3.14"?
					// Actually, for streaming speed, let's assume end-of-buffer punctuation IS a boundary
					// if we are aggressive. But "Dr." at end of chunk is common.
					// Safer: wait for space or assume if we have decent length.
					// Let's implement aggressive: Punctuation + Space OR specific endings.
					// We can just wait for space.
					// If i+1 >= length, we continue and keep in buffer? Yes.
				} else {
					next := runes[i+1]
					if next == ' ' || next == '\n' || next == '\t' {
						isEnd = true
					}
				}
			}

			if isEnd {
				// Found a sentence boundary at 'i'
				sentence := string(runes[processedPos : i+1])
				// Push to worker
				a.ttsQueue <- ttsJob{
					text:         strings.TrimSpace(sentence),
					ctx:          reqCtx,
					generationID: genID,
				}

				// Advance
				processedPos = i + 1
			}
		}

		// Keep the remainder in buffer
		if processedPos > 0 {
			if processedPos < length {
				buffer.Reset()
				buffer.WriteString(string(runes[processedPos:]))
			} else {
				buffer.Reset()
			}
		}
	}

	// 3. Flush remaining text
	remaining := strings.TrimSpace(buffer.String())
	if remaining != "" {
		a.ttsQueue <- ttsJob{
			text:         remaining,
			ctx:          reqCtx,
			generationID: genID,
		}
	}

	fullText := completeResponse.String()
	log.Printf("Agent response (full): %s", fullText)

	// Send text to frontend for display (Full update)
	// Optionally sending partial updates would be cool but full is fine for now
	a.sendAudioControl("text", map[string]interface{}{
		"text": fullText,
	})

	duration := time.Since(start)
	log.Printf("Processing complete in %v", duration)
}

// runTTSWorker consumes sentences and sends them to TTS client sequentially
func (a *VoiceAgent) runTTSWorker() {
	log.Println("TTS Worker started")
	for job := range a.ttsQueue {
		// Check generation ID - skip jobs from interrupted generations
		a.mu.Lock()
		currentGen := a.currentGenID
		a.mu.Unlock()

		if job.generationID < currentGen {
			log.Printf("[TTS-WORKER] Skipping job from old generation %d (current: %d): %q",
				job.generationID, currentGen, job.text)
			continue
		}

		// Check context before processing (handle interruption)
		if job.ctx.Err() != nil {
			log.Printf("[TTS-WORKER] Skipping job due to context cancellation: %q", job.text)
			continue
		}

		// Skip empty
		if job.text == "" {
			continue
		}

		if a.ttsClient != nil {
			start := time.Now()
			log.Printf("[TTS-WORKER] Processing sentence (gen %d): %q", job.generationID, job.text)

			// This call is now SYNCHRONOUS/BLOCKING
			err := a.ttsClient.SendText(job.text)
			if err != nil {
				log.Printf("[TTS-WORKER] Error: %v", err)
			}

			log.Printf("[TTS-WORKER] Sentence complete in %vms", time.Since(start).Milliseconds())
		}
	}
}

// runAudioSender consumes existing ttsAudioChan and sends to LiveKit
func (a *VoiceAgent) runAudioSender() {
	log.Println("Audio Sender started")

	// We use the struct's isPlaying state with locking

	for {
		select {
		case audioData, ok := <-a.ttsAudioChan:
			if !ok {
				return
			}

			// Check for end-of-audio marker (0xFF single byte)
			if len(audioData) == 1 && audioData[0] == 0xFF {
				a.mu.Lock()
				isPlaying := a.isPlaying
				a.isPlaying = false
				a.mu.Unlock()

				if isPlaying {
					a.sendAudioControl("audio_end")
					log.Printf("[AUDIO-CONTROL] Audio playback ended (marker)")
				}
				continue
			}

			a.mu.Lock()
			isPlaying := a.isPlaying
			a.mu.Unlock()

			if !isPlaying {
				a.sendAudioControl("audio_start")
				a.mu.Lock()
				a.isPlaying = true
				a.mu.Unlock()
				log.Printf("[AUDIO-CONTROL] Audio playback started")
			}

			// Forward to frontend via Data Channel
			msg := AudioMessage{
				Type:  "audio",
				Audio: base64.StdEncoding.EncodeToString(audioData),
			}

			jsonData, err := json.Marshal(msg)
			if err == nil {
				if a.room != nil {
					userData := lksdk.UserData(jsonData)
					a.room.LocalParticipant.PublishDataPacket(
						userData,
						lksdk.WithDataPublishReliable(true),
						lksdk.WithDataPublishTopic("audio"),
					)
				}
			}

		case <-time.After(2000 * time.Millisecond):
			// If no audio for 2s, assume utterance ended
			// Increased from 500ms to prevent flickering during TTS generation pauses
			a.mu.Lock()
			isPlaying := a.isPlaying
			if isPlaying {
				a.isPlaying = false
				a.mu.Unlock()
				a.sendAudioControl("audio_end")
				log.Printf("[AUDIO-CONTROL] Audio playback ended (timeout)")
			} else {
				a.mu.Unlock()
			}
		}
	}
}
