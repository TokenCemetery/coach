package server

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/TokenCemetery/coach/internal/state"
)

// Emby Web rewrites its API address to ws://<server>/embywebsocket?api_key=…&deviceId=…
// and expects a plain RFC 6455 endpoint there. Coach implements the server half
// on the standard library rather than taking its first external dependency: the
// server side needs the handshake, unmasking of client frames and the text,
// close, ping and pong opcodes. No extension is negotiated, so there is no
// compression state to carry.
//
// The GUID and the SHA-1 below are the fixed handshake constant of RFC 6455 §4.2.2.
// They prove neither identity nor integrity; authentication is the access token,
// checked before the upgrade.
const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	// The reference server announces a 60 s timeout in ForceKeepAlive and drops
	// silent sockets after it. Coach keeps that budget but refreshes it from any
	// frame, not only from the application-level KeepAlive: Emby Web never sends
	// that message, so protocol pings — which browsers answer inside the network
	// stack — are what proves a web client is still there.
	socketTimeout = 60 * time.Second
	socketPing    = 20 * time.Second
	socketWrite   = 10 * time.Second
	// A message is a control command, never bulk data; anything larger is a
	// client defect or an attempt to make the server buffer without bound.
	socketMaxMessage = 64 << 10
	socketLimit      = 64
	socketQueue      = 32
)

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

const (
	closeNormal   = 1000
	closeProtocol = 1002
	closeTooLarge = 1009
)

var (
	errSocketProtocol = errors.New("websocket protocol error")
	errSocketTooLarge = errors.New("websocket message too large")
)

type socket struct {
	conn                     net.Conn
	r                        *bufio.Reader
	mu                       sync.Mutex
	out                      chan []byte
	token, userID, sessionID string
}

// writeFrame sends one unfragmented frame. Server frames are never masked.
func (s *socket) writeFrame(opcode byte, payload []byte) error {
	frame := make([]byte, 2, len(payload)+10)
	frame[0] = 0x80 | opcode
	switch n := len(payload); {
	case n < 126:
		frame[1] = byte(n)
	case n <= 0xFFFF:
		frame[1] = 126
		frame = binary.BigEndian.AppendUint16(frame, uint16(n))
	default:
		frame[1] = 127
		frame = binary.BigEndian.AppendUint64(frame, uint64(n))
	}
	frame = append(frame, payload...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(socketWrite)); err != nil {
		return err
	}
	_, err := s.conn.Write(frame)
	return err
}

func (s *socket) send(message any) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return s.writeFrame(opText, body)
}

func (s *socket) closeWith(code uint16, reason string) {
	_ = s.writeFrame(opClose, append(binary.BigEndian.AppendUint16(nil, code), reason...))
	_ = s.conn.Close()
}

// readFrame returns one unmasked frame. Client frames must be masked (RFC 6455
// §5.1), and since no extension was negotiated a set reserved bit is an error
// rather than something to skip over.
func (s *socket) readFrame() (opcode byte, final bool, payload []byte, err error) {
	var header [2]byte
	if _, err = io.ReadFull(s.r, header[:]); err != nil {
		return 0, false, nil, err
	}
	if header[0]&0x70 != 0 || header[1]&0x80 == 0 {
		return 0, false, nil, errSocketProtocol
	}
	final, opcode = header[0]&0x80 != 0, header[0]&0x0F
	length := uint64(header[1] & 0x7F)
	switch length {
	case 126:
		var extended [2]byte
		if _, err = io.ReadFull(s.r, extended[:]); err != nil {
			return 0, false, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err = io.ReadFull(s.r, extended[:]); err != nil {
			return 0, false, nil, err
		}
		length = binary.BigEndian.Uint64(extended[:])
	}
	// Control frames carry a short payload and are never fragmented.
	if opcode >= opClose && (!final || length > 125) {
		return 0, false, nil, errSocketProtocol
	}
	if length > socketMaxMessage {
		return 0, false, nil, errSocketTooLarge
	}
	var mask [4]byte
	if _, err = io.ReadFull(s.r, mask[:]); err != nil {
		return 0, false, nil, err
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(s.r, payload); err != nil {
		return 0, false, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return opcode, final, payload, nil
}

// serveSocket owns the connection until the peer goes away.
func (s *Server) serveSocket(sock *socket) {
	done := make(chan struct{})
	writerDone := make(chan struct{})
	defer func() {
		_ = sock.conn.Close()
		close(done)
		<-writerDone
	}()
	go func() {
		defer close(writerDone)
		ticker := time.NewTicker(socketPing)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case message := <-sock.out:
				if _, err := s.store.Authenticate(sock.token); err != nil {
					_ = sock.conn.Close()
					return
				}
				if sock.writeFrame(opText, message) != nil {
					_ = sock.conn.Close()
					return
				}
			case <-ticker.C:
				if _, err := s.store.Authenticate(sock.token); err != nil {
					_ = sock.conn.Close()
					return
				}
				if sock.writeFrame(opPing, nil) != nil {
					// Closing here unblocks the read below, which ends the handler.
					_ = sock.conn.Close()
					return
				}
			}
		}
	}()

	var (
		message    []byte
		kind       byte
		fragmented bool
	)
	for {
		if sock.conn.SetReadDeadline(time.Now().Add(socketTimeout)) != nil {
			return
		}
		opcode, final, payload, err := sock.readFrame()
		if err != nil {
			switch {
			case errors.Is(err, errSocketProtocol):
				sock.closeWith(closeProtocol, "protocol error")
			case errors.Is(err, errSocketTooLarge):
				sock.closeWith(closeTooLarge, "message too large")
			default:
				_ = sock.conn.Close()
			}
			return
		}
		if _, err := s.store.Authenticate(sock.token); err != nil {
			return
		}
		switch opcode {
		case opPing:
			if sock.writeFrame(opPong, payload) != nil {
				_ = sock.conn.Close()
				return
			}
		case opPong:
			// The read deadline above already counted this as a sign of life.
		case opClose:
			sock.closeWith(closeNormal, "")
			return
		case opText, opBinary, opContinuation:
			// A continuation may only follow an unfinished message, and a new
			// data frame may only start when none is in progress.
			if (opcode == opContinuation) != fragmented {
				sock.closeWith(closeProtocol, "unexpected fragment")
				return
			}
			if opcode == opContinuation {
				message = append(message, payload...)
			} else {
				kind, message = opcode, payload
			}
			if len(message) > socketMaxMessage {
				sock.closeWith(closeTooLarge, "message too large")
				return
			}
			if fragmented = !final; fragmented {
				continue
			}
			if kind == opText {
				if s.socketMessage(sock, message) != nil {
					_ = sock.conn.Close()
					return
				}
			}
			message = nil
		default:
			sock.closeWith(closeProtocol, "unknown opcode")
			return
		}
	}
}

// UserDataChanged needs no listener registration. Other listener commands are
// ignored until their events are implemented. Malformed JSON keeps the socket open.
func (s *Server) socketMessage(sock *socket, raw []byte) error {
	var message struct{ MessageType string }
	if json.Unmarshal(raw, &message) != nil {
		return nil
	}
	if strings.EqualFold(message.MessageType, "KeepAlive") {
		return sock.send(object{"MessageType": "KeepAlive"})
	}
	return nil
}

// websocketAccept validates the upgrade request and computes the accept value
// of RFC 6455 §4.2.2. Version 13 is the only one the reference clients speak.
func websocketAccept(r *http.Request) (string, bool) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !headerHasToken(r, "Connection", "upgrade") {
		return "", false
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return "", false
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if raw, err := base64.StdEncoding.DecodeString(key); err != nil || len(raw) != 16 {
		return "", false
	}
	sum := sha1.Sum([]byte(key + websocketGUID))
	return base64.StdEncoding.EncodeToString(sum[:]), true
}

func headerHasToken(r *http.Request, header, token string) bool {
	for _, v := range r.Header.Values(header) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func (s *Server) websocket(w http.ResponseWriter, r *http.Request, token string, session state.Session) {
	accept, ok := websocketAccept(r)
	if !ok {
		fail(w, 400, "InvalidRequest")
		return
	}
	// Each socket holds a goroutine and a connection for as long as the peer
	// keeps it, so the count is capped even though callers are authenticated.
	if s.sockets.Add(1) > socketLimit {
		s.sockets.Add(-1)
		w.Header().Set("Retry-After", "5")
		fail(w, 503, "TooManyConnections")
		return
	}
	defer s.sockets.Add(-1)
	conn, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		fail(w, 500, "UpgradeUnsupported")
		return
	}
	defer func() { _ = conn.Close() }()
	sock := &socket{conn: conn, r: buffered.Reader, out: make(chan []byte, socketQueue), token: token, userID: session.UserID, sessionID: session.ID}
	if !s.registerSocket(sock) {
		return
	}
	defer s.removeSocket(sock)
	if conn.SetWriteDeadline(time.Now().Add(socketWrite)) != nil {
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n")); err != nil {
		return
	}
	// Read through the hijacked reader: it may already hold bytes the client
	// pipelined after the handshake.
	// The reference announces the timeout in seconds as soon as the socket
	// opens; native clients answer with KeepAlive at half that rate.
	if sock.send(object{"MessageType": "ForceKeepAlive", "Data": int(socketTimeout / time.Second)}) != nil {
		return
	}
	s.serveSocket(sock)
}
