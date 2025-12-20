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
	ttsClient *tts.ElevenLabsClient

	// Audio channels
	ttsAudioChan chan []byte

	// Audio processing
	audioBuffer   chan []byte
	transcriptBuf strings.Builder
	debounceTimer *time.Timer

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
}

// AudioMessage sent via data channel
type AudioMessage struct {
	Type       string `json:"type"`
	Audio      string `json:"audio"` // base64 encoded
	SampleRate int    `json:"sampleRate"`
	Text       string `json:"text,omitempty"` // for transcript display
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
	ttsClient := tts.NewElevenLabsClient(cfg.ElevenLabsAPIKey)

	return &VoiceAgent{
		client:       client,
		roomName:     roomName,
		sttClient:    sttClient,
		llmClient:    llmClient,
		ttsClient:    ttsClient,
		audioBuffer:  make(chan []byte, 100),
		ttsAudioChan: make(chan []byte, 50),
		ctx:          ctx,
		cancelFunc:   cancel,
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
	if a.sttClient != nil {
		if err := a.sttClient.Connect(a.ctx); err != nil {
			log.Printf("Failed to connect STT: %v", err)
		}

		// Start transcript processing
		go a.processTranscripts()
	}

	if a.ttsClient != nil {
		if err := a.ttsClient.Connect(a.ctx); err != nil {
			log.Printf("Failed to connect TTS: %v", err)
		}

		// Start TTS audio receiver
		go a.ttsClient.ReceiveAudio(a.ctx, a.ttsAudioChan)
	}

	return nil
}

// sendTTSAudioViaDataChannel sends TTS audio to browser via data channel
func (a *VoiceAgent) sendTTSAudioViaDataChannel() {
	for {
		select {
		case <-a.ctx.Done():
			return
		case pcmData := <-a.ttsAudioChan:
			if len(pcmData) == 0 {
				continue
			}

			log.Printf("Sending TTS audio via data channel: %d bytes", len(pcmData))

			// Encode audio as base64 and send via data channel
			msg := AudioMessage{
				Type:       "audio",
				Audio:      base64.StdEncoding.EncodeToString(pcmData),
				SampleRate: 44100, // Match ElevenLabs output
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
				}
			}
		}
	}
}

// processTranscripts handles incoming transcripts from Deepgram
func (a *VoiceAgent) processTranscripts() {
	const debounceDelay = 1000 * time.Millisecond

	processFn := func() {
		a.mu.Lock()
		fullUtterance := strings.TrimSpace(a.transcriptBuf.String())
		a.transcriptBuf.Reset()
		a.mu.Unlock()

		if fullUtterance == "" {
			return
		}

		log.Printf("Processing utterance: %s", fullUtterance)

		// Get LLM response
		responseStream, err := a.llmClient.GenerateResponse(a.ctx, fullUtterance)
		if err != nil {
			log.Printf("LLM Error: %v", err)
			return
		}

		var fullResponse strings.Builder
		for text := range responseStream {
			fullResponse.WriteString(text)
		}

		response := fullResponse.String()
		if response != "" {
			log.Printf("Agent response: %s", response)

			// Send text response via data channel too
			a.sendTextMessage(response)

			// Send to TTS
			if a.ttsClient != nil {
				if err := a.ttsClient.SendText(response); err != nil {
					log.Printf("TTS Send Error: %v", err)
				}
			}
		}
	}

	// Listen for UtteranceEnd signals
	go func() {
		for range a.sttClient.UtteranceEnd {
			if a.debounceTimer != nil {
				a.debounceTimer.Stop()
			}
			a.debounceTimer = time.AfterFunc(100*time.Millisecond, processFn)
		}
	}()

	for transcript := range a.sttClient.Transcript {
		if transcript == "" {
			continue
		}

		a.mu.Lock()
		if a.transcriptBuf.Len() > 0 {
			a.transcriptBuf.WriteString(" ")
		}
		a.transcriptBuf.WriteString(transcript)
		a.mu.Unlock()

		// Reset debounce timer
		if a.debounceTimer != nil {
			a.debounceTimer.Stop()
		}
		a.debounceTimer = time.AfterFunc(debounceDelay, processFn)
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
