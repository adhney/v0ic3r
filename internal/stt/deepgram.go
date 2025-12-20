package stt

import (
	"context"
	"log"

	msginterfaces "github.com/deepgram/deepgram-go-sdk/pkg/api/listen/v1/websocket/interfaces"
	interfaces "github.com/deepgram/deepgram-go-sdk/pkg/client/interfaces"
	client "github.com/deepgram/deepgram-go-sdk/pkg/client/listen"
)

type DeepgramClient struct {
	conn           *client.WSCallback
	apiKey         string
	Transcript     chan string
	UtteranceReady chan bool // Signals when user has finished speaking
}

func NewDeepgramClient(apiKey string) *DeepgramClient {
	return &DeepgramClient{
		apiKey:         apiKey,
		Transcript:     make(chan string, 100),
		UtteranceReady: make(chan bool, 10),
	}
}

func (d *DeepgramClient) Connect(ctx context.Context) error {
	clientOptions := interfaces.ClientOptions{
		APIKey: d.apiKey,
	}

	tOptions := interfaces.LiveTranscriptionOptions{
		Model:          "nova-3",
		Language:       "en-US",
		SmartFormat:    true,
		Encoding:       "linear16",
		SampleRate:     16000,
		Channels:       1,
		InterimResults: true,
		UtteranceEndMs: "1500", // Wait 1.5s of silence before firing UtteranceEnd
	}

	callback := &DeepgramReceiver{d: d}

	conn, err := client.NewWebSocketUsingCallback(ctx, "", &clientOptions, &tOptions, callback)
	if err != nil {
		log.Printf("Deepgram connection failed: %v", err)
		return err
	}
	log.Println("Deepgram WebSocket Connected Successfully")
	d.conn = conn

	if b := d.conn.Connect(); !b {
		log.Println("Deepgram Connect() returned false")
	} else {
		log.Println("Deepgram Connect() initiated")
	}

	return nil
}

func (d *DeepgramClient) SendAudio(data []byte) error {
	if d.conn == nil {
		return nil
	}
	_, err := d.conn.Write(data)
	return err
}

func (d *DeepgramClient) Close() error {
	return nil
}

// DeepgramReceiver implements msginterfaces.LiveMessageCallback
type DeepgramReceiver struct {
	d *DeepgramClient
}

func (r *DeepgramReceiver) Open(or *msginterfaces.OpenResponse) error {
	log.Println("STT Connected")
	return nil
}

func (r *DeepgramReceiver) Message(mr *msginterfaces.MessageResponse) error {
	if len(mr.Channel.Alternatives) > 0 {
		alt := mr.Channel.Alternatives[0]
		if len(alt.Transcript) > 0 && mr.IsFinal {
			// Send final transcript chunks immediately
			r.d.Transcript <- alt.Transcript
		}
	}
	return nil
}

func (r *DeepgramReceiver) Metadata(md *msginterfaces.MetadataResponse) error { return nil }
func (r *DeepgramReceiver) SpeechStarted(ssr *msginterfaces.SpeechStartedResponse) error {
	return nil
}

// UtteranceEnd fires when Deepgram detects the user has stopped speaking
func (r *DeepgramReceiver) UtteranceEnd(ur *msginterfaces.UtteranceEndResponse) error {
	log.Println("UtteranceEnd detected - user finished speaking")
	select {
	case r.d.UtteranceReady <- true:
	default:
		// Channel full, skip
	}
	return nil
}

func (r *DeepgramReceiver) Close(cr *msginterfaces.CloseResponse) error { return nil }
func (r *DeepgramReceiver) Error(er *msginterfaces.ErrorResponse) error {
	log.Printf("STT Error: %v", er)
	return nil
}
func (r *DeepgramReceiver) UnhandledEvent(byData []byte) error { return nil }
