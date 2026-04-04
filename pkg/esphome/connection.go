package esphome

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"github.com/skorokithakis/havpe-server/pkg/esphome/api"

	"google.golang.org/protobuf/proto"
)

const (
	// PingInterval is how often the server proactively sends PingRequest to the device.
	PingInterval = 20 * time.Second
	// ReadDeadline is the duration after which a read on the TCP connection times out.
	ReadDeadline = 60 * time.Second
)

// VoiceHandler is implemented by the voice pipeline to receive voice assistant
// messages from the ESPHome device. HandleConnection dispatches to these methods
// from its read loop.
type VoiceHandler interface {
	HandleVoiceAssistantRequest(writer io.Writer, data []byte) error
	HandleVoiceAssistantAudio(writer io.Writer, data []byte) error
}

// HandleConnection performs the ESPHome handshake and then runs the main read loop
// until the connection ends. It returns nil on a clean disconnect and a non-nil error
// on any failure. onConnected is called once the handshake completes.
func HandleConnection(conn net.Conn, handler VoiceHandler, onConnected func()) error {
	defer conn.Close()

	done := make(chan struct{})
	defer close(done)

	lw := &LockedWriter{Conn: conn}

	if err := conn.SetReadDeadline(time.Now().Add(ReadDeadline)); err != nil {
		return fmt.Errorf("set initial read deadline: %w", err)
	}

	reader := bufio.NewReader(conn)

	if err := SendMessage(lw, MessageTypeHelloRequest, &api.HelloRequest{
		ClientInfo:      "esphome-go-server",
		ApiVersionMajor: 1,
		ApiVersionMinor: 10,
	}); err != nil {
		return fmt.Errorf("send HelloRequest: %w", err)
	}

	helloData, err := readHandshakeResponse(conn, lw, reader, MessageTypeHelloResponse)
	if err != nil {
		return fmt.Errorf("hello handshake: %w", err)
	}
	var helloResponse api.HelloResponse
	if err := proto.Unmarshal(helloData, &helloResponse); err != nil {
		return fmt.Errorf("unmarshal HelloResponse: %w", err)
	}
	log.Printf("HelloResponse: server_info=%q name=%q api_version=%d.%d",
		helloResponse.GetServerInfo(), helloResponse.GetName(),
		helloResponse.GetApiVersionMajor(), helloResponse.GetApiVersionMinor())

	// Authentication (message IDs 3-4) was removed in ESPHome 2026.1.0.
	if err := SendMessage(lw, MessageTypeDeviceInfoRequest, &api.DeviceInfoRequest{}); err != nil {
		return fmt.Errorf("send DeviceInfoRequest: %w", err)
	}

	deviceInfoData, err := readHandshakeResponse(conn, lw, reader, MessageTypeDeviceInfoResponse)
	if err != nil {
		return fmt.Errorf("device info handshake: %w", err)
	}
	var deviceInfoResponse api.DeviceInfoResponse
	if err := proto.Unmarshal(deviceInfoData, &deviceInfoResponse); err != nil {
		return fmt.Errorf("unmarshal DeviceInfoResponse: %w", err)
	}
	log.Printf("DeviceInfoResponse: name=%q model=%q", deviceInfoResponse.GetName(), deviceInfoResponse.GetModel())

	if err := SendMessage(lw, MessageTypeListEntitiesRequest, &api.ListEntitiesRequest{}); err != nil {
		return fmt.Errorf("send ListEntitiesRequest: %w", err)
	}
	if err := drainEntityList(conn, lw, reader); err != nil {
		return fmt.Errorf("entity list: %w", err)
	}

	if err := SendMessage(lw, MessageTypeSubscribeStatesRequest, &api.SubscribeStatesRequest{}); err != nil {
		return fmt.Errorf("send SubscribeStatesRequest: %w", err)
	}
	if err := SendMessage(lw, MessageTypeSubscribeVoiceAssistantRequest, &api.SubscribeVoiceAssistantRequest{
		Subscribe: true,
		Flags:     1,
	}); err != nil {
		return fmt.Errorf("send SubscribeVoiceAssistantRequest: %w", err)
	}

	log.Printf("handshake complete, entering read loop")
	onConnected()

	go func() {
		ticker := time.NewTicker(PingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := SendMessage(lw, MessageTypePingRequest, &api.PingRequest{}); err != nil {
					log.Printf("ping goroutine: send PingRequest: %v", err)
					conn.Close()
					return
				}
			}
		}
	}()

	for {
		messageType, data, err := readFrameWithDeadline(conn, reader)
		if err != nil {
			if err == io.EOF {
				log.Println("connection closed by device")
			} else {
				log.Printf("read frame: %v", err)
			}
			return err
		}

		switch messageType {
		case MessageTypePingRequest:
			if err := HandlePingRequest(lw); err != nil {
				return err
			}
		case MessageTypePingResponse:
			// No-op: device's reply to our proactive PingRequest.
		case MessageTypeDisconnectRequest:
			HandleDisconnectRequest(lw)
			return nil
		case MessageTypeVoiceAssistantRequest:
			if err := handler.HandleVoiceAssistantRequest(lw, data); err != nil {
				return err
			}
		case MessageTypeVoiceAssistantAudio:
			if err := handler.HandleVoiceAssistantAudio(lw, data); err != nil {
				return err
			}
		default:
			log.Printf("ignoring message type %d", messageType)
		}
	}
}

func readFrameWithDeadline(conn net.Conn, reader *bufio.Reader) (uint32, []byte, error) {
	messageType, data, err := ReadFrame(reader)
	if err != nil {
		return 0, nil, err
	}
	if err := conn.SetReadDeadline(time.Now().Add(ReadDeadline)); err != nil {
		return 0, nil, fmt.Errorf("refresh read deadline: %w", err)
	}
	return messageType, data, nil
}

func readHandshakeResponse(conn net.Conn, writer io.Writer, reader *bufio.Reader, expectedType uint32) ([]byte, error) {
	for {
		messageType, data, err := readFrameWithDeadline(conn, reader)
		if err != nil {
			return nil, fmt.Errorf("reading frame (expected type %d): %w", expectedType, err)
		}
		if messageType == MessageTypePingRequest {
			if err := HandlePingRequest(writer); err != nil {
				return nil, err
			}
			continue
		}
		if messageType != expectedType {
			log.Printf("readHandshakeResponse: skipping unexpected message type %d while waiting for type %d", messageType, expectedType)
			continue
		}
		return data, nil
	}
}

func drainEntityList(conn net.Conn, writer io.Writer, reader *bufio.Reader) error {
	for {
		messageType, _, err := readFrameWithDeadline(conn, reader)
		if err != nil {
			return fmt.Errorf("reading entity list frame: %w", err)
		}
		if messageType == MessageTypePingRequest {
			if err := HandlePingRequest(writer); err != nil {
				return err
			}
			continue
		}
		if messageType == MessageTypeListEntitiesDoneResponse {
			log.Printf("ListEntitiesDoneResponse received")
			return nil
		}
		log.Printf("ignoring entity list message type %d", messageType)
	}
}
