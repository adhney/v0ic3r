# Build stage for Frontend
FROM node:18-alpine AS frontend-builder
WORKDIR /app/frontend
COPY web/frontend/package*.json ./
RUN npm ci
COPY web/frontend/ ./
RUN npm run build

# Build stage for Backend
FROM golang:alpine AS backend-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Build the binary statically
RUN CGO_ENABLED=0 GOOS=linux go build -o voice-agent ./cmd/server/main.go

# Final runtime stage
FROM alpine:latest
WORKDIR /app
RUN apk --no-cache add ca-certificates tzdata

# Copy binary from backend builder
COPY --from=backend-builder /app/voice-agent .

# Copy frontend build from frontend builder
# The Go server expects static files in ./web/frontend/dist relative to workdir
COPY --from=frontend-builder /app/frontend/dist ./web/frontend/dist

# Expose port
EXPOSE 8080

# Environment variables with defaults (can be overridden)
ENV PORT=8080
ENV GIN_MODE=release

# Run the binary
CMD ["./voice-agent"]
