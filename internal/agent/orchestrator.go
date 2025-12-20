package agent

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/adhney/voice-agent/internal/config"
	"github.com/adhney/voice-agent/internal/llm"
	"github.com/adhney/voice-agent/internal/stt"
	"github.com/adhney/voice-agent/internal/tts"
	"github.com/gorilla/websocket"
)

type Orchestrator struct {
	sttClient *stt.DeepgramClient
	llmClient *llm.GeminiClient
	ttsClient *tts.ElevenLabsClient
	wsConn    *websocket.Conn

	ctx        context.Context
	cancelFunc context.CancelFunc
}

func NewOrchestrator(cfg *config.Config, ws *websocket.Conn) (*Orchestrator, error) {
	ctx, cancel := context.WithCancel(context.Background())

	sttC := stt.NewDeepgramClient(cfg.DeepgramAPIKey)

	llmC, err := llm.NewGeminiClient(ctx, cfg.GeminiAPIKey)
	if err != nil {
		cancel()
		return nil, err
	}

	ttsC := tts.NewElevenLabsClient(cfg.ElevenLabsAPIKey)

	return &Orchestrator{
		sttClient:  sttC,
		llmClient:  llmC,
		ttsClient:  ttsC,
		wsConn:     ws,
		ctx:        ctx,
		cancelFunc: cancel,
	}, nil
}

func (o *Orchestrator) Start() error {
	if err := o.sttClient.Connect(o.ctx); err != nil {
		return err
	}
	if err := o.ttsClient.Connect(o.ctx); err != nil {
		return err
	}

	var readyMsg map[string]string
	if err := o.wsConn.ReadJSON(&readyMsg); err != nil {
		log.Printf("Failed to read ready signal: %v", err)
		return err
	}
	log.Println("Client Ready Signal Received")

	ttsAudioChan := make(chan []byte, 100)
	go o.ttsClient.ReceiveAudio(o.ctx, ttsAudioChan)

	go func() {
		for audioChunk := range ttsAudioChan {
			if err := o.wsConn.WriteMessage(websocket.BinaryMessage, audioChunk); err != nil {
				log.Printf("Browser WS Write Error: %v", err)
				return
			}
		}
	}()

	greeting := "Hi! Welcome to Medicare Services. I can help you book appointments, answer questions about our doctors, or assist with prescriptions. How can I help you today?"
	o.wsConn.WriteJSON(map[string]string{"role": "agent", "text": greeting})
	if err := o.ttsClient.SendText(greeting); err != nil {
		log.Printf("Greeting TTS Error: %v", err)
	}

	errChan := make(chan error, 1)

	// Read Audio from Browser -> Send to STT
	go func() {
		for {
			_, message, err := o.wsConn.ReadMessage()
			if err != nil {
				log.Printf("Browser WS Read Error: %v", err)
				errChan <- err
				return
			}
			if err := o.sttClient.SendAudio(message); err != nil {
				log.Printf("STT Send Error: %v", err)
			}
		}
	}()

	// Process transcripts with hybrid debounce:
	// - Deepgram's UtteranceEnd signals potential end (1s silence)
	// - We wait an additional 500ms to catch any continuation
	go o.processTranscriptsHybrid()

	return <-errChan
}

func (o *Orchestrator) processTranscriptsHybrid() {
	var buffer strings.Builder
	var mu sync.Mutex
	var processingTimer *time.Timer

	const additionalWait = 500 * time.Millisecond // Extra wait after UtteranceEnd

	processFn := func() {
		mu.Lock()
		fullUtterance := strings.TrimSpace(buffer.String())
		buffer.Reset()
		mu.Unlock()

		if fullUtterance == "" {
			return
		}

		log.Printf("Processing complete utterance: %s", fullUtterance)

		responseStream, err := o.llmClient.GenerateResponse(o.ctx, fullUtterance)
		if err != nil {
			log.Printf("LLM Error: %v", err)
			return
		}

		var fullResponse string
		for text := range responseStream {
			log.Printf("Agent: %s", text)
			o.wsConn.WriteJSON(map[string]string{"role": "agent", "text": text})
			fullResponse += text
		}

		if fullResponse != "" {
			if err := o.ttsClient.SendText(fullResponse); err != nil {
				log.Printf("TTS Send Error: %v", err)
			}
		}
	}

	// Goroutine to collect transcripts
	go func() {
		for transcript := range o.sttClient.Transcript {
			if transcript == "" {
				continue
			}

			mu.Lock()
			if buffer.Len() > 0 {
				buffer.WriteString(" ")
			}
			buffer.WriteString(transcript)
			mu.Unlock()

			// If timer was running, stop it (we got more speech)
			if processingTimer != nil {
				processingTimer.Stop()
				processingTimer = nil
			}
		}
	}()

	// Listen for UtteranceEnd signals from Deepgram
	for range o.sttClient.UtteranceEnd {
		// Cancel any existing timer
		if processingTimer != nil {
			processingTimer.Stop()
		}

		// Wait a bit more to catch late transcripts
		processingTimer = time.AfterFunc(additionalWait, processFn)
	}
}

func (o *Orchestrator) Stop() {
	o.cancelFunc()
	o.sttClient.Close()
	o.llmClient.Close()
	o.ttsClient.Close()
}
