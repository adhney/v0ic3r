package main

import (
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/adhney/voice-agent/internal/config"
	"github.com/adhney/voice-agent/internal/server"
)

func main() {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Serve React build from web/frontend/dist
	staticDir := "./web/frontend/dist"

	// Custom handler for SPA routing
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Skip WebSocket endpoint
		if r.URL.Path == "/ws" {
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

	// WebSocket endpoint (must be registered separately)
	http.HandleFunc("/ws", server.HandleWebSocket)

	log.Printf("Server starting on port %s...", cfg.Port)
	log.Printf("Open http://localhost:%s in your browser", cfg.Port)
	addr := fmt.Sprintf(":%s", cfg.Port)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

// Suppress unused import warning
var _ fs.FS
