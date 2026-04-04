package esphome

import (
	"fmt"
	"io"
	"log"
	"net"
	"sync"

	"github.com/skorokithakis/havpe-server/pkg/esphome/api"

	"google.golang.org/protobuf/proto"
)

// Message type IDs from the ESPHome native API proto (id) option.
// These are not generated as constants by protoc-gen-go and must be defined manually.
const (
	MessageTypeHelloRequest                   uint32 = 1
	MessageTypeHelloResponse                  uint32 = 2
	MessageTypeAuthenticationRequest          uint32 = 3
	MessageTypeAuthenticationResponse         uint32 = 4
	MessageTypeDisconnectRequest              uint32 = 5
	MessageTypeDisconnectResponse             uint32 = 6
	MessageTypePingRequest                    uint32 = 7
	MessageTypePingResponse                   uint32 = 8
	MessageTypeDeviceInfoRequest              uint32 = 9
	MessageTypeDeviceInfoResponse             uint32 = 10
	MessageTypeListEntitiesRequest            uint32 = 11
	MessageTypeListEntitiesDoneResponse       uint32 = 19
	MessageTypeSubscribeStatesRequest         uint32 = 20
	MessageTypeSubscribeVoiceAssistantRequest uint32 = 89
	MessageTypeVoiceAssistantRequest          uint32 = 90
	MessageTypeVoiceAssistantResponse         uint32 = 91
	MessageTypeVoiceAssistantEventResponse    uint32 = 92
	MessageTypeVoiceAssistantAudio            uint32 = 106
)

// LockedWriter wraps a net.Conn with a mutex so that concurrent goroutines
// never interleave partial frame writes on the same TCP connection.
type LockedWriter struct {
	Conn net.Conn
	mu   sync.Mutex
}

func (lw *LockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.Conn.Write(p)
}

func SendMessage(writer io.Writer, messageType uint32, message proto.Message) error {
	data, err := proto.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal message type %d: %w", messageType, err)
	}
	if err := WriteFrame(writer, messageType, data); err != nil {
		return fmt.Errorf("write frame type %d: %w", messageType, err)
	}
	return nil
}

func HandlePingRequest(writer io.Writer) error {
	return SendMessage(writer, MessageTypePingResponse, &api.PingResponse{})
}

func HandleDisconnectRequest(writer io.Writer) {
	log.Printf("DisconnectRequest received, sending DisconnectResponse and closing")
	_ = SendMessage(writer, MessageTypeDisconnectResponse, &api.DisconnectResponse{})
}

func SendEvent(writer io.Writer, eventType api.VoiceAssistantEvent, data []*api.VoiceAssistantEventData) error {
	log.Printf("sending event %s", eventType)
	return SendMessage(writer, MessageTypeVoiceAssistantEventResponse, &api.VoiceAssistantEventResponse{
		EventType: eventType,
		Data:      data,
	})
}
