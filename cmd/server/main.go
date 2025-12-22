package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/adhney/voice-agent/internal/config"
	lk "github.com/adhney/voice-agent/internal/livekit"
	"github.com/adhney/voice-agent/internal/server"
	"github.com/google/uuid"
)

func main() {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize LiveKit client
	lkClient := lk.NewClient(cfg.LiveKitURL, cfg.LiveKitAPIKey, cfg.LiveKitAPISecret)

	// Serve React build from web/frontend/dist
	staticDir := "./web/frontend/dist"

	// Custom handler for SPA routing
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Skip API and WebSocket endpoints
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/ws" {
			return
		}

		path := staticDir + r.URL.Path

		// Check if file exists
		if _, err := os.Stat(path); os.IsNotExist(err) || strings.HasSuffix(r.URL.Path, "/") && r.URL.Path != "/" {
			// Serve index.html for SPA routing
			http.ServeFile(w, r, staticDir+"/index.html")
			return
		}

		// Serve the static file
		http.ServeFile(w, r, path)
	})

	// LiveKit token endpoint
	http.HandleFunc("/api/livekit/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Generate a unique room name for this conversation
		roomName := "voice-room-" + uuid.New().String()[:8]

		// Generate token for the user
		token, err := lkClient.GenerateToken(roomName, "user-"+uuid.New().String()[:6], false)
		if err != nil {
			http.Error(w, "Failed to generate token", http.StatusInternalServerError)
			return
		}

		// Create room (fire and forget, it may already exist)
		go lkClient.CreateRoom(r.Context(), roomName)

		// Start voice agent in the room
		go startVoiceAgentInRoom(lkClient, roomName, cfg)

		response := map[string]string{
			"token": token,
			"room":  roomName,
			"url":   cfg.LiveKitURL,
		}

		json.NewEncoder(w).Encode(response)
	})

	// WebSocket endpoint (keep for backward compatibility)
	http.HandleFunc("/ws", server.HandleWebSocket)

	log.Printf("Server starting on port %s...", cfg.Port)
	log.Printf("Open http://localhost:%s in your browser", cfg.Port)
	addr := fmt.Sprintf(":%s", cfg.Port)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

// startVoiceAgentInRoom creates and starts a voice agent for a LiveKit room
func startVoiceAgentInRoom(client *lk.Client, roomName string, cfg *config.Config) {
	agentCfg := lk.AgentConfig{
		DeepgramAPIKey:   cfg.DeepgramAPIKey,
		GeminiAPIKey:     cfg.GeminiAPIKey,
		ElevenLabsAPIKey: cfg.ElevenLabsAPIKey,
		CartesiaAPIKey:   cfg.CartesiaAPIKey,
		TTSProvider:      cfg.TTSProvider,
		EnableBargeIn:    cfg.EnableBargeIn,
	}

	agent, err := lk.NewVoiceAgentWithConfig(client, roomName, agentCfg)
	if err != nil {
		log.Printf("Failed to create voice agent: %v", err)
		return
	}

	if err := agent.Start(nil); err != nil {
		log.Printf("Failed to start voice agent in room %s: %v", roomName, err)
		return
	}

	log.Printf("Voice agent with full pipeline started in room: %s", roomName)
	// Agent will continue running until room is closed
}

// Suppress unused import warning
var _ fs.FS
