package tts

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type CartesiaClient struct {
	apiKey      string
	voiceID     string
	modelID     string
	audioChan   chan []byte
	interrupted atomic.Bool
	conn        *websocket.Conn
	connMu      sync.Mutex
}

func NewCartesiaClient(apiKey string) *CartesiaClient {
	voiceID := os.Getenv("CARTESIA_VOICE_ID")
	if voiceID == "" {
		voiceID = "f786b574-daa5-4673-aa0c-cbe3e8534c02" // "Katie" (American Female)
	}

	return &CartesiaClient{
		apiKey:    apiKey,
		voiceID:   voiceID,
		modelID:   "sonic-english", // or "sonic-3"
		audioChan: make(chan []byte, 100),
	}
}

// AudioChannel returns the channel where audio chunks are sent
func (c *CartesiaClient) AudioChannel() <-chan []byte {
	return c.audioChan
}

// Cancel stops the current TTS generation
func (c *CartesiaClient) Cancel() {
	c.interrupted.Store(true)
	c.connMu.Lock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.connMu.Unlock()
}

// SendText implements the TTSClient interface
// Updated to be synchronous for sequential streaming
func (c *CartesiaClient) SendText(text string) error {
	c.interrupted.Store(false)
	c.speakAsync(text)
	return nil
}

func (c *CartesiaClient) speakAsync(text string) {
	log.Printf("[CARTESIA] speakAsync started for: %q", text)
	url := fmt.Sprintf("wss://api.cartesia.ai/tts/websocket?api_key=%s&cartesia_version=2024-06-10", c.apiKey)

	c.connMu.Lock()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		c.connMu.Unlock()
		log.Printf("[CARTESIA] WebSocket Dial Error: %v", err)
		return
	}
	c.conn = conn
	c.connMu.Unlock()
	log.Printf("[CARTESIA] Connected to WebSocket")

	defer func() {
		c.connMu.Lock()
		if c.conn != nil {
			c.conn.Close()
			c.conn = nil
		}
		c.connMu.Unlock()
	}()

	// Construct request
	requestID := uuid.New().String()
	payload := map[string]interface{}{
		"context_id": requestID,
		"model_id":   c.modelID,
		"transcript": text,
		"voice": map[string]string{
			"mode": "id",
			"id":   c.voiceID, // Ensure voiceID is valid
		},
		"output_format": map[string]interface{}{
			"container":   "raw",
			"encoding":    "pcm_s16le",
			"sample_rate": 44100,
		},
	}

	// Add logging for voice ID being used
	log.Printf("[CARTESIA] Using Voice ID: %s", c.voiceID)

	if err := conn.WriteJSON(payload); err != nil {
		log.Printf("[CARTESIA] WriteJSON Error: %v", err)
		return
	}
	log.Printf("[CARTESIA] Request payload sent")

	// Read loop
	for {
		if c.interrupted.Load() {
			log.Printf("[CARTESIA] Interrupted")
			return
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[CARTESIA] ReadMessage Error: %v", err)
			} else {
				log.Printf("[CARTESIA] Connection closed: %v", err)
			}
			break
		}

		// Log message length
		log.Printf("[CARTESIA] Received message size: %d", len(message))

		var response map[string]interface{}
		if err := json.Unmarshal(message, &response); err != nil {
			log.Printf("[CARTESIA] JSON Unmarshal Error: %v", err)
			continue
		}

		// Check for error field in response
		if errMsg, ok := response["error"].(string); ok {
			log.Printf("[CARTESIA] API Error: %s", errMsg)
		}

		// Handle chunk
		if dataStr, ok := response["data"].(string); ok {
			audioData, err := base64.StdEncoding.DecodeString(dataStr)
			if err != nil {
				log.Printf("[CARTESIA] Base64 Decode Error: %v", err)
				continue
			}
			log.Printf("[CARTESIA] Received audio chunk: %d bytes (raw PCM)", len(audioData))

			// Prepend WAV header to valid PCM chunk
			// Since frontend decodeAudioData needs a header, and we are sending "raw"
			// we wrap each chunk in a WAV header.
			wavChunk := audioData

			c.audioChan <- wavChunk
		} else if done, ok := response["done"].(bool); ok && done {
			// Generation finished
			log.Printf("[CARTESIA] Generation done")
			break
		}
	}
}

// prependWAVHeader adds a standard WAV header to raw PCM data
func prependWAVHeader(pcm []byte, sampleRate int) []byte {
	// WAV Header is 44 bytes
	header := make([]byte, 44)
	dataLen := len(pcm)
	totalLen := dataLen + 36

	// RIFF/WAVE header
	copy(header[0:4], []byte("RIFF"))
	putUint32(header[4:8], uint32(totalLen))
	copy(header[8:12], []byte("WAVE"))

	// fmt chunk
	copy(header[12:16], []byte("fmt "))
	putUint32(header[16:20], 16)                   // Subchunk1Size (16 for PCM)
	putUint16(header[20:22], 1)                    // AudioFormat (1 for PCM)
	putUint16(header[22:24], 1)                    // NumChannels (1 mono)
	putUint32(header[24:28], uint32(sampleRate))   // SampleRate
	putUint32(header[28:32], uint32(sampleRate*2)) // ByteRate (SampleRate * NumChannels * BitsPerSample/8)
	putUint16(header[32:34], 2)                    // BlockAlign (NumChannels * BitsPerSample/8)
	putUint16(header[34:36], 16)                   // BitsPerSample

	// data chunk
	copy(header[36:40], []byte("data"))
	putUint32(header[40:44], uint32(dataLen))

	return append(header, pcm...)
}

func putUint32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

func putUint16(b []byte, v uint16) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
}

// Close cleans up resources
func (c *CartesiaClient) Close() {
	c.Cancel()
}
