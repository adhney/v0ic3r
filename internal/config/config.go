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
	}, nil
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}
