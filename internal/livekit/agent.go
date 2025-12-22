package livekit

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	go a.sendTTSAudioViaDataChannel()

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
				goto done
			}
		}
	done:
		// Notify frontend to stop audio
		a.sendAudioControl("audio_stop")
		log.Printf("[BARGE-IN] Interrupt triggered, TTS cancelled, transcript cleared")

		a.mu.Lock()
		a.interruptPlayback = false
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

			if isPlaying {
				log.Printf("[BARGE-IN] User started speaking while TTS playing, triggering interrupt")
				a.triggerInterrupt()
			}
		}
	}
}

// processTranscripts handles incoming transcripts from Deepgram
func (a *VoiceAgent) processTranscripts() {

	processFn := func() {
		processStart := time.Now()

		a.mu.Lock()
		// Prevent concurrent processing - only one LLM/TTS request at a time
		if a.isProcessing {
			a.mu.Unlock()
			log.Printf("[BARGE-IN] Skipping processing, already in progress")
			return
		}
		a.isProcessing = true

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

		// Get LLM response
		llmStart := time.Now()
		log.Printf("[LATENCY] LLM request started at %v (delta: +%vms from STT)",
			llmStart.Format("15:04:05.000"), llmStart.Sub(processStart).Milliseconds())

		responseStream, err := a.llmClient.GenerateResponse(requestCtx, fullUtterance)
		if err != nil {
			log.Printf("LLM Error: %v", err)
			return
		}

		var fullResponse strings.Builder
		firstChunk := true
		var firstChunkTime time.Time
		for text := range responseStream {
			if firstChunk {
				firstChunkTime = time.Now()
				log.Printf("[LATENCY] LLM first chunk at %v (delta: +%vms from LLM start)",
					firstChunkTime.Format("15:04:05.000"), firstChunkTime.Sub(llmStart).Milliseconds())
				firstChunk = false
			}
			fullResponse.WriteString(text)
		}
		llmEnd := time.Now()
		log.Printf("[LATENCY] LLM complete at %v (total LLM: %vms)",
			llmEnd.Format("15:04:05.000"), llmEnd.Sub(llmStart).Milliseconds())

		response := fullResponse.String()
		if response != "" {
			log.Printf("Agent response: %s", response)

			// Send text response via data channel
			a.sendTextMessage(response)

			// Send full response to TTS
			if a.ttsClient != nil {
				ttsStart := time.Now()
				log.Printf("[LATENCY] TTS request started at %v (delta: +%vms from process start)",
					ttsStart.Format("15:04:05.000"), ttsStart.Sub(processStart).Milliseconds())

				if err := a.ttsClient.SendText(response); err != nil {
					log.Printf("TTS Send Error: %v", err)
				} else {
					log.Printf("[LATENCY] TTS request sent at %v (total pipeline: %vms)",
						time.Now().Format("15:04:05.000"), time.Since(processStart).Milliseconds())
				}
			}
		}
	}

	const debounceDelay = 1200 * time.Millisecond

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

	// Listen for UtteranceEnd signals
	go func() {
		for range a.sttClient.UtteranceEnd {
			log.Printf("[LATENCY] STT UtteranceEnd detected at %v", time.Now().Format("15:04:05.000"))
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
	log.Printf("Processing audio track from: %s (codec: %s)",
		rp.Identity(), track.Codec().MimeType)

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

		// Check if this is RED encoded
		if len(payload) > 1 && track.Codec().MimeType == "audio/red" {
			if len(payload) > 4 {
				payload = payload[4:]
			}
		}

		// Log every 100 packets
		if packetCount%100 == 1 {
			log.Printf("Audio packet #%d, size: %d bytes", packetCount, len(payload))
		}

		// Send to STT
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

	if typeStr, ok := msg["type"].(string); ok && typeStr == "chat_input" {
		if text, ok := msg["text"].(string); ok && text != "" {
			log.Printf("Received chat input: %s", text)
			go a.ProcessUserText(text)
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

	// 2. Accumulate and Send to TTS
	// We accumulate full text for now to match current simple logic
	for chunk := range respChan {
		completeResponse.WriteString(chunk)
	}

	fullText := completeResponse.String()
	log.Printf("Agent response: %s", fullText)

	// Send to TTS
	if fullText != "" {
		ttsStart := time.Now()
		log.Printf("[LATENCY] TTS request started at %s", ttsStart.Format("15:04:05.000"))
		a.ttsClient.SendText(fullText)
	}

	// Send text to frontend for display
	a.sendAudioControl("text", map[string]interface{}{
		"text": fullText,
	})

	duration := time.Since(start)
	log.Printf("Processing complete in %v", duration)
}
