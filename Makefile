# Voice Agent Development

.PHONY: dev livekit server frontend build clean

# Run everything together (LiveKit + Go server with hot reload)
dev:
	@echo "Starting development environment..."
	@make -j3 livekit server frontend-dev

# Start LiveKit server
livekit:
	@echo "Starting LiveKit server..."
	@cd ~/livekit-local && docker run --rm \
		-p 7880:7880 \
		-p 7881:7881 \
		-p 7882:7882/udp \
		-v $(HOME)/livekit-local/livekit.yaml:/livekit.yaml \
		livekit/livekit-server \
		--config /livekit.yaml \
		--node-ip=127.0.0.1

# Start Go server with Air hot reload
server:
	@echo "Starting Go server with hot reload..."
	@air -c .air.toml

# Start frontend dev server (optional, for HMR during development)
frontend-dev:
	@echo "Starting frontend dev server..."
	@cd web/frontend && npm run dev

# Build production frontend
frontend-build:
	@echo "Building frontend..."
	@cd web/frontend && npm run build

# Build Go server
build:
	@echo "Building Go server..."
	@go build -o bin/voice-agent cmd/server/main.go

# Clean build artifacts
clean:
	@rm -rf bin/
	@rm -rf web/frontend/dist/
	@rm -rf tmp/

# Install development tools
setup:
	@echo "Installing Air for hot reload..."
	@go install github.com/cosmtrek/air@latest
	@echo "Installing frontend dependencies..."
	@cd web/frontend && npm install
	@echo "Setup complete!"

# Run without hot reload (builds frontend first)
run:
	@echo "Building frontend..."
	@cd web/frontend && npm run build
	@echo "Starting server..."
	@go run cmd/server/main.go
