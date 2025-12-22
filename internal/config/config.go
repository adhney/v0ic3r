package config

import (
	"log"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	Port             string
	DeepgramAPIKey   string
	GeminiAPIKey     string
	ElevenLabsAPIKey string
	CartesiaAPIKey   string
	TTSProvider      string
	EnableBargeIn    bool

	// LiveKit
	LiveKitURL       string
	LiveKitAPIKey    string
	LiveKitAPISecret string
}

func LoadConfig() (*Config, error) {
	if err := godotenv.Load(); err != nil {
		log.Println("Warning: No .env file found, relying on system environment variables")
	}

	return &Config{
		Port:             getEnv("PORT", "8080"),
		DeepgramAPIKey:   getEnv("DEEPGRAM_API_KEY", ""),
		GeminiAPIKey:     getEnv("GEMINI_API_KEY", ""),
		ElevenLabsAPIKey: getEnv("ELEVENLABS_API_KEY", ""),
		CartesiaAPIKey:   getEnv("CARTESIA_API_KEY", ""),
		TTSProvider:      getEnv("TTS_PROVIDER", "elevenlabs"),
		EnableBargeIn:    getEnv("ENABLE_BARGE_IN", "true") == "true",
		LiveKitURL:       getEnv("LIVEKIT_URL", "ws://localhost:7880"),
		LiveKitAPIKey:    getEnv("LIVEKIT_API_KEY", "devkey"),
		LiveKitAPISecret: getEnv("LIVEKIT_API_SECRET", "secret"),
	}, nil
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}
