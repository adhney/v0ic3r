package llm

import (
	"context"
	"fmt"
	"log"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

type GeminiClient struct {
	client *genai.Client
	model  *genai.GenerativeModel
	chat   *genai.ChatSession
}

func NewGeminiClient(ctx context.Context, apiKey string) (*GeminiClient, error) {
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		return nil, fmt.Errorf("failed to create gemini client: %w", err)
	}

	model := client.GenerativeModel("gemini-2.0-flash")
	model.SetTemperature(0.7)
	model.SystemInstruction = genai.NewUserContent(genai.Text(`You are a friendly Medicare assistant for a healthcare website. Your role is to help patients:
- Book appointments with doctors
- Answer questions about doctors and their specialties
- Help with prescription refills and medication questions
- Provide general healthcare guidance

Keep your responses short, warm, and conversational - like a helpful receptionist. Use simple language. If you don't know something specific, offer to help them find the right resource. Always be empathetic and patient-focused.

Example responses:
- "I can help you book an appointment! Do you have a preferred doctor or should I suggest one based on what you need?"
- "For prescription refills, I'll need your prescription number. Do you have that handy?"
- "Dr. Smith specializes in cardiology and is available next Tuesday at 2pm. Would that work for you?"`))

	// Start a chat session to maintain conversation history
	chat := model.StartChat()

	return &GeminiClient{
		client: client,
		model:  model,
		chat:   chat,
	}, nil
}

func (g *GeminiClient) GenerateResponse(ctx context.Context, prompt string) (<-chan string, error) {
	outputChan := make(chan string)

	go func() {
		defer close(outputChan)

		// Use SendMessageStream to maintain conversation history
		iter := g.chat.SendMessageStream(ctx, genai.Text(prompt))
		for {
			resp, err := iter.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				log.Printf("Gemini stream error: %v", err)
				break
			}

			if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
				for _, part := range resp.Candidates[0].Content.Parts {
					if txt, ok := part.(genai.Text); ok {
						outputChan <- string(txt)
					}
				}
			}
		}
	}()

	return outputChan, nil
}

func (g *GeminiClient) Close() {
	g.client.Close()
}
