package server

import (
	"log"
	"net/http"

	"github.com/adhney/voice-agent/internal/agent"
	"github.com/adhney/voice-agent/internal/config"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all for demo
	},
}

func HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Upgrade error: %v", err)
		return
	}
	defer conn.Close()

	log.Println("Client connected")

	// Load Config (Ideally passed in, but loading here for prototype)
	cfg, _ := config.LoadConfig()

	orch, err := agent.NewOrchestrator(cfg, conn)
	if err != nil {
		log.Printf("Failed to create orchestrator: %v", err)
		return
	}
	defer orch.Stop()

	if err := orch.Start(); err != nil {
		log.Printf("Orchestrator error: %v", err)
	}
}
