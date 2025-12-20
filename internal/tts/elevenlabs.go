package tts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/gorilla/websocket"
)

type ElevenLabsClient struct {
	apiKey    string
	voiceID   string
	modelID   string
	audioChan chan []byte
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
	go e.speakAsync(text)
	return nil
}

func (e *ElevenLabsClient) speakAsync(text string) {
	url := fmt.Sprintf("wss://api.elevenlabs.io/v1/text-to-speech/%s/stream-input?model_id=%s&output_format=mp3_44100_128", e.voiceID, e.modelID)

	header := http.Header{}
	header.Set("xi-api-key", e.apiKey)

	conn, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		log.Printf("ElevenLabs Connect Error: %v", err)
		return
	}
	defer conn.Close()

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
	for {
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
			decoded, err := base64.StdEncoding.DecodeString(response.Audio)
			if err != nil {
				log.Printf("Base64 decode error: %v", err)
				continue
			}
			log.Printf("DEBUG: TTS audio decoded %d bytes", len(decoded))
			e.audioChan <- decoded
		}

		if response.IsFinal {
			log.Printf("DEBUG: TTS response is final")
			break
		}
	}
	log.Printf("DEBUG: speakAsync completed")
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

func (e *ElevenLabsClient) Close() error {
	close(e.audioChan)
	return nil
}
