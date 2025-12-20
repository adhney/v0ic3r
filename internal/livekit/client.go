package livekit

import (
	"context"
	"log"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

// Client manages LiveKit room connections and token generation
type Client struct {
	url       string
	apiKey    string
	apiSecret string
}

// NewClient creates a new LiveKit client
func NewClient(url, apiKey, apiSecret string) *Client {
	return &Client{
		url:       url,
		apiKey:    apiKey,
		apiSecret: apiSecret,
	}
}

// GenerateToken creates a JWT token for a participant to join a room
func (c *Client) GenerateToken(roomName, participantIdentity string, isAgent bool) (string, error) {
	at := auth.NewAccessToken(c.apiKey, c.apiSecret)

	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     roomName,
	}

	at.SetVideoGrant(grant).
		SetIdentity(participantIdentity).
		SetValidFor(24 * time.Hour)

	if isAgent {
		t := true
		grant.CanUpdateOwnMetadata = &t
	}

	return at.ToJWT()
}

// CreateRoom creates a new LiveKit room
func (c *Client) CreateRoom(ctx context.Context, roomName string) error {
	roomClient := lksdk.NewRoomServiceClient(c.url, c.apiKey, c.apiSecret)

	_, err := roomClient.CreateRoom(ctx, &livekit.CreateRoomRequest{
		Name: roomName,
	})
	if err != nil {
		log.Printf("Failed to create room: %v", err)
		return err
	}

	log.Printf("Created LiveKit room: %s", roomName)
	return nil
}

// JoinRoomAsAgent connects to a room as the AI agent
func (c *Client) JoinRoomAsAgent(ctx context.Context, roomName string, callback *lksdk.RoomCallback) (*lksdk.Room, error) {
	token, err := c.GenerateToken(roomName, "voice-agent", true)
	if err != nil {
		return nil, err
	}

	room, err := lksdk.ConnectToRoomWithToken(c.url, token, callback)
	if err != nil {
		log.Printf("Failed to join room: %v", err)
		return nil, err
	}

	log.Printf("Agent joined room: %s", roomName)
	return room, nil
}

// DeleteRoom removes a LiveKit room
func (c *Client) DeleteRoom(ctx context.Context, roomName string) error {
	roomClient := lksdk.NewRoomServiceClient(c.url, c.apiKey, c.apiSecret)
	_, err := roomClient.DeleteRoom(ctx, &livekit.DeleteRoomRequest{
		Room: roomName,
	})
	return err
}
