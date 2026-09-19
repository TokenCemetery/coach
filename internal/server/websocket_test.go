package server

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The key and accept pair below is the worked example of RFC 6455 §1.3, so the
// handshake is checked against the specification rather than against a value
// recomputed with the same code it is meant to verify.
const (
	exampleKey    = "dGhlIHNhbXBsZSBub25jZQ=="
	exampleAccept = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
)

func socketRequest(t *testing.T, server *httptest.Server, query, version, key string) (*http.Response, net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	request := "GET /embywebsocket" + query + " HTTP/1.1\r\nHost: coach\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: " + version + "\r\nSec-WebSocket-Key: " + key + "\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	return response, conn, reader
}

func writeClientFrame(t *testing.T, conn net.Conn, opcode byte, payload []byte) {
	t.Helper()
	if len(payload) > 125 {
		t.Fatalf("test frame too long: %d", len(payload))
	}
	mask := []byte{0x37, 0xFA, 0x21, 0x3D}
	frame := append([]byte{0x80 | opcode, 0x80 | byte(len(payload))}, mask...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}
}

func readServerFrame(t *testing.T, reader *bufio.Reader) (byte, []byte) {
	t.Helper()
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		t.Fatal(err)
	}
	if header[0]&0x80 == 0 {
		t.Fatal("server sent a fragmented frame")
	}
	if header[1]&0x80 != 0 {
		t.Fatal("server frames must not be masked")
	}
	length := int(header[1] & 0x7F)
	if length == 126 {
		extended := make([]byte, 2)
		if _, err := io.ReadFull(reader, extended); err != nil {
			t.Fatal(err)
		}
		length = int(binary.BigEndian.Uint16(extended))
	} else if length == 127 {
		t.Fatal("unexpected 64-bit frame length")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatal(err)
	}
	return header[0] & 0x0F, payload
}

func expectSocketMessage(t *testing.T, reader *bufio.Reader) (string, json.RawMessage) {
	t.Helper()
	opcode, payload := readServerFrame(t, reader)
	if opcode != opText {
		t.Fatalf("opcode = %d, want text", opcode)
	}
	var message struct {
		MessageType string
		Data        json.RawMessage
	}
	if err := json.Unmarshal(payload, &message); err != nil {
		t.Fatalf("message is not JSON: %v", err)
	}
	return message.MessageType, message.Data
}

func expectCloseFrame(t *testing.T, reader *bufio.Reader, code uint16) {
	t.Helper()
	opcode, payload := readServerFrame(t, reader)
	if opcode != opClose {
		t.Fatalf("opcode = %d, want close", opcode)
	}
	if len(payload) < 2 || binary.BigEndian.Uint16(payload) != code {
		t.Fatalf("close payload = %v, want code %d", payload, code)
	}
}

func TestWebSocketUpgradeRequiresTokenAndVersion(t *testing.T) {
	_, h, _ := newTestServer(t)
	server := httptest.NewServer(h)
	defer server.Close()
	token := login(t, h)

	for _, c := range []struct {
		name    string
		query   string
		version string
		key     string
		status  int
	}{
		{"no token", "", "13", exampleKey, 401},
		{"unknown token", "?api_key=not-a-token", "13", exampleKey, 401},
		{"unsupported version", "?api_key=" + token, "8", exampleKey, 400},
		{"malformed key", "?api_key=" + token, "13", "short", 400},
	} {
		t.Run(c.name, func(t *testing.T) {
			response, _, _ := socketRequest(t, server, c.query, c.version, c.key)
			if response.StatusCode != c.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, c.status)
			}
		})
	}
}

func TestWebSocketKeepAliveAndControlFrames(t *testing.T) {
	_, h, _ := newTestServer(t)
	server := httptest.NewServer(h)
	defer server.Close()
	token := login(t, h)

	response, conn, reader := socketRequest(t, server, "?api_key="+token+"&deviceId=test-device", "13", exampleKey)
	if response.StatusCode != 101 {
		t.Fatalf("status = %d, want 101", response.StatusCode)
	}
	if got := response.Header.Get("Sec-WebSocket-Accept"); got != exampleAccept {
		t.Fatalf("Sec-WebSocket-Accept = %q, want %q", got, exampleAccept)
	}
	if got := response.Header.Get("Sec-WebSocket-Extensions"); got != "" {
		t.Fatalf("no extension was offered, server answered %q", got)
	}

	// The reference announces the timeout in seconds as soon as the socket opens.
	kind, data := expectSocketMessage(t, reader)
	if kind != "ForceKeepAlive" {
		t.Fatalf("first message = %q, want ForceKeepAlive", kind)
	}
	if string(data) != "60" {
		t.Fatalf("ForceKeepAlive data = %s, want 60", data)
	}

	writeClientFrame(t, conn, opText, []byte(`{"MessageType":"KeepAlive"}`))
	if kind, _ := expectSocketMessage(t, reader); kind != "KeepAlive" {
		t.Fatalf("answer to KeepAlive = %q", kind)
	}

	// A listener Coach cannot feed is accepted silently, not answered with an
	// invented event; the socket must stay usable afterwards.
	writeClientFrame(t, conn, opText, []byte(`{"MessageType":"SessionsStart","Data":"0,1500"}`))
	writeClientFrame(t, conn, opPing, []byte("alive"))
	opcode, payload := readServerFrame(t, reader)
	if opcode != opPong || string(payload) != "alive" {
		t.Fatalf("ping answered with opcode %d and payload %q", opcode, payload)
	}

	// A fragmented text message must be reassembled before it is parsed.
	mask := []byte{0x11, 0x22, 0x33, 0x44}
	first, second := []byte(`{"MessageType":`), []byte(`"KeepAlive"}`)
	frame := append([]byte{opText, 0x80 | byte(len(first))}, mask...)
	for i, b := range first {
		frame = append(frame, b^mask[i%4])
	}
	frame = append(frame, 0x80|opContinuation, 0x80|byte(len(second)))
	frame = append(frame, mask...)
	for i, b := range second {
		frame = append(frame, b^mask[i%4])
	}
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}
	if kind, _ := expectSocketMessage(t, reader); kind != "KeepAlive" {
		t.Fatalf("answer to fragmented KeepAlive = %q", kind)
	}

	writeClientFrame(t, conn, opClose, binary.BigEndian.AppendUint16(nil, closeNormal))
	expectCloseFrame(t, reader, closeNormal)
}

func TestWebSocketRejectsMalformedFrames(t *testing.T) {
	_, h, _ := newTestServer(t)
	server := httptest.NewServer(h)
	defer server.Close()
	token := login(t, h)

	for _, c := range []struct {
		name  string
		frame []byte
		code  uint16
	}{
		// RFC 6455 §5.1: every frame from a client is masked.
		{"unmasked", []byte{0x80 | opText, 0x02, 'h', 'i'}, closeProtocol},
		// No extension was negotiated, so a reserved bit cannot be interpreted.
		{"reserved bit", []byte{0x80 | 0x40 | opText, 0x80, 0, 0, 0, 0}, closeProtocol},
		// A control frame is never fragmented.
		{"fragmented ping", []byte{opPing, 0x80, 0, 0, 0, 0}, closeProtocol},
		// A continuation without a message in progress has nothing to continue.
		{"stray continuation", []byte{0x80 | opContinuation, 0x80, 0, 0, 0, 0}, closeProtocol},
		{"oversized", append([]byte{0x80 | opText, 0xFF}, binary.BigEndian.AppendUint64(nil, 1<<20)...), closeTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			response, conn, reader := socketRequest(t, server, "?api_key="+token, "13", exampleKey)
			if response.StatusCode != 101 {
				t.Fatalf("status = %d, want 101", response.StatusCode)
			}
			if kind, _ := expectSocketMessage(t, reader); kind != "ForceKeepAlive" {
				t.Fatalf("first message = %q", kind)
			}
			if _, err := conn.Write(c.frame); err != nil {
				t.Fatal(err)
			}
			expectCloseFrame(t, reader, c.code)
		})
	}
}
