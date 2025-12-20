package agent

import (
	"context"
)

// STTClient defines the interface for Speech-to-Text services
type STTClient interface {
	Connect(ctx context.Context) error
	SendAudio(data []byte) error
	Close() error
}

// LLMClient defines the interface for Language Model services
type LLMClient interface {
	GenerateResponse(ctx context.Context, prompt string) (<-chan string, error)
}

// TTSClient defines the interface for Text-to-Speech services
type TTSClient interface {
	Connect(ctx context.Context) error
	SendText(text string) error
	Close() error
}
