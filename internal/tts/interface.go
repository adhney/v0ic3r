package tts

// TTSClient defines the interface for Text-to-Speech clients
type TTSClient interface {
	// SendText sends text to be converted to speech
	SendText(text string) error

	// Cancel stops any ongoing speech generation
	Cancel()

	// AudioChannel returns the channel where audio chunks are sent
	AudioChannel() <-chan []byte

	// Close cleans up resources
	Close()
}
