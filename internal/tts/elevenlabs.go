package tts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type ElevenLabsClient struct {
	apiKey      string
	voiceID     string
	modelID     string
	audioChan   chan []byte
	interrupted atomic.Bool     // Set to true to cancel current TTS
	conn        *websocket.Conn // Current WebSocket connection
	connMu      sync.Mutex      // Protects conn
}

func NewElevenLabsClient(apiKey string) *ElevenLabsClient {
	return &ElevenLabsClient{
		apiKey:    apiKey,
		voiceID:   "21m00Tcm4TlvDq8ikWAM", // Default voice (Rachel)
		modelID:   "eleven_flash_v2_5",    // Low latency model
		audioChan: make(chan []byte, 100),
	}
}

// Connect is now a no-op for compatibility. Real connection happens in SpeakAndStream.
func (e *ElevenLabsClient) Connect(ctx context.Context) error {
	log.Println("ElevenLabs: Ready")
	return nil
}

// SendText queues text for TTS. For simplicity, we now use a synchronous approach
// where each call creates a fresh connection, sends text, and reads all audio.
func (e *ElevenLabsClient) SendText(text string) error {
	e.interrupted.Store(false) // Reset interrupt flag for new request
	go e.speakAsync(text)
	return nil
}

// AudioChannel returns the channel where audio chunks are sent
func (e *ElevenLabsClient) AudioChannel() <-chan []byte {
	return e.audioChan
}

// Cancel interrupts the current TTS generation by closing the WebSocket
func (e *ElevenLabsClient) Cancel() {
	e.interrupted.Store(true)
	e.connMu.Lock()
	if e.conn != nil {
		e.conn.Close()
		log.Printf("[TTS] Cancel: WebSocket closed")
	}
	e.connMu.Unlock()
}

// Close cleans up resources
func (e *ElevenLabsClient) Close() {
	e.Cancel()
}

func (e *ElevenLabsClient) speakAsync(text string) {
	asyncStart := time.Now()
	log.Printf("[LATENCY] TTS speakAsync started at %v", asyncStart.Format("15:04:05.000"))

	url := fmt.Sprintf("wss://api.elevenlabs.io/v1/text-to-speech/%s/stream-input?model_id=%s&output_format=mp3_44100_128", e.voiceID, e.modelID)

	header := http.Header{}
	header.Set("xi-api-key", e.apiKey)

	conn, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		log.Printf("ElevenLabs Connect Error: %v", err)
		return
	}

	// Store connection for Cancel()
	e.connMu.Lock()
	e.conn = conn
	e.connMu.Unlock()

	defer func() {
		e.connMu.Lock()
		e.conn = nil
		e.connMu.Unlock()
		conn.Close()
	}()

	connTime := time.Now()
	log.Printf("[LATENCY] TTS WebSocket connected at %v (connection took: %vms)",
		connTime.Format("15:04:05.000"), connTime.Sub(asyncStart).Milliseconds())

	// Send the text
	payload := map[string]interface{}{
		"text":                   text,
		"try_trigger_generation": true,
	}
	if err := conn.WriteJSON(payload); err != nil {
		log.Printf("ElevenLabs Send Error: %v", err)
		return
	}

	// Signal end of input
	endPayload := map[string]string{"text": ""}
	if err := conn.WriteJSON(endPayload); err != nil {
		log.Printf("ElevenLabs End Signal Error: %v", err)
		return
	}

	// Read all audio chunks
	firstAudio := true
	for {
		// Check for interrupt
		if e.interrupted.Load() {
			log.Printf("[TTS] Interrupted, closing WebSocket")
			return
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("DEBUG: ElevenLabs read error: %v", err)
			break
		}

		// Log raw message (truncated)
		msgStr := string(message)
		if len(msgStr) > 200 {
			msgStr = msgStr[:200] + "..."
		}
		log.Printf("DEBUG: ElevenLabs raw: %s", msgStr)

		var response struct {
			Audio   string `json:"audio"`
			IsFinal bool   `json:"isFinal"`
		}

		if err := json.Unmarshal(message, &response); err != nil {
			log.Printf("DEBUG: JSON unmarshal error: %v", err)
			continue
		}

		if response.Audio != "" {
			if firstAudio {
				log.Printf("[LATENCY] TTS first audio chunk at %v (time-to-first-audio: %vms)",
					time.Now().Format("15:04:05.000"), time.Since(asyncStart).Milliseconds())
				firstAudio = false
			}
			decoded, err := base64.StdEncoding.DecodeString(response.Audio)
			if err != nil {
				log.Printf("Base64 decode error: %v", err)
				continue
			}
			log.Printf("DEBUG: TTS audio decoded %d bytes", len(decoded))
			e.audioChan <- decoded
		}

		if response.IsFinal {
			log.Printf("[LATENCY] TTS complete at %v (total TTS: %vms)",
				time.Now().Format("15:04:05.000"), time.Since(asyncStart).Milliseconds())
			break
		}
	}
	// Send end-of-audio signal (empty slice with special length -1 represented as nil will be handled)
	// We'll use a 1-byte marker to signal "end"
	e.audioChan <- []byte{0xFF} // Special end marker
	log.Printf("DEBUG: speakAsync completed, sent end marker")
}

// ReceiveAudio returns the channel where audio chunks are sent
func (e *ElevenLabsClient) ReceiveAudio(ctx context.Context, audioChan chan<- []byte) {
	log.Printf("DEBUG: ReceiveAudio started")
	for {
		select {
		case <-ctx.Done():
			close(audioChan)
			return
		case chunk := <-e.audioChan:
			log.Printf("DEBUG: Forwarding audio chunk %d bytes to agent", len(chunk))
			audioChan <- chunk
		}
	}
}
