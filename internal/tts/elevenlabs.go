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
	url := fmt.Sprintf("wss://api.elevenlabs.io/v1/text-to-speech/%s/stream-input?model_id=%s&output_format=pcm_16000", e.voiceID, e.modelID)

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
			// Connection closed or error - we're done
			break
		}

		var response struct {
			Audio   string `json:"audio"`
			IsFinal bool   `json:"isFinal"`
		}

		if err := json.Unmarshal(message, &response); err != nil {
			continue
		}

		if response.Audio != "" {
			decoded, err := base64.StdEncoding.DecodeString(response.Audio)
			if err != nil {
				log.Printf("Base64 decode error: %v", err)
				continue
			}
			e.audioChan <- decoded
		}

		if response.IsFinal {
			break
		}
	}
}

// ReceiveAudio returns the channel where audio chunks are sent
func (e *ElevenLabsClient) ReceiveAudio(ctx context.Context, audioChan chan<- []byte) {
	for {
		select {
		case <-ctx.Done():
			close(audioChan)
			return
		case chunk := <-e.audioChan:
			audioChan <- chunk
		}
	}
}

func (e *ElevenLabsClient) Close() error {
	close(e.audioChan)
	return nil
}
