package tarantool

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"sync"
)

const greetingSize = 128
const saltSize = 32
const tarantoolVersion = "Tarantool 1.6.8 (Binary)"
const connBufSize = 128 * 1024

type QueryHandler func(query Query) *Result

type IprotoServer struct {
	conn      net.Conn
	reader    *bufio.Reader
	writer    *bufio.Writer
	uuid      string
	salt      []byte
	quit      chan bool
	handler   QueryHandler
	output    chan []byte
	closeOnce sync.Once
}

func NewIprotoServer(uuid string, handler QueryHandler) *IprotoServer {
	return &IprotoServer{
		conn:    nil,
		reader:  nil,
		writer:  nil,
		handler: handler,
		uuid:    uuid,
	}
}

func (s *IprotoServer) Accept(conn net.Conn) {
	s.conn = conn
	s.reader = bufio.NewReaderSize(conn, connBufSize)
	s.writer = bufio.NewWriterSize(conn, connBufSize)
	s.quit = make(chan bool)
	s.output = make(chan []byte, 1024)

	err := s.greet()
	if err != nil {
		conn.Close()
		return
	}

	go s.loop()
}

func (s *IprotoServer) CheckAuth(hash []byte, password string) bool {
	scr, err := scramble(s.salt, password)
	if err != nil {
		return false
	}

	if len(scr) != len(hash) {
		return false
	}

	for i, v := range hash {
		if v != scr[i] {
			return false
		}
	}
	return true
}

func (s *IprotoServer) Close() {
	s.closeOnce.Do(func() {
		close(s.quit)
		s.conn.Close()
	})
}

func (s *IprotoServer) greet() (err error) {
	var line1, line2 string
	var format, greeting string
	var n int

	s.salt = make([]byte, saltSize)
	_, err = rand.Read(s.salt)
	if err != nil {
		return
	}

	line1 = fmt.Sprintf("%s %s", tarantoolVersion, s.uuid)
	line2 = fmt.Sprintf("%s", base64.StdEncoding.EncodeToString(s.salt))

	format = fmt.Sprintf("%%-%ds\n%%-%ds\n", greetingSize/2-1, greetingSize/2-1)
	greeting = fmt.Sprintf(format, line1, line2)

	// send greeting
	n, err = fmt.Fprintf(s.writer, "%s", greeting)
	if err != nil || n != greetingSize {
		return
	}

	return s.writer.Flush()
}

func (s *IprotoServer) loop() {
	go s.read()
	go s.write()
}

func (s *IprotoServer) read() {
	var packet *Packet
	var err error
	var body []byte

	r := s.reader

READER_LOOP:
	for {
		select {
		case <-s.quit:
			break READER_LOOP
		default:
			// read raw bytes
			body, err = readMessage(r)
			if err != nil {
				break READER_LOOP
			}

			packet, err = decodePacket(bytes.NewBuffer(body))
			if err != nil {
				break READER_LOOP
			}

			if packet.request != nil {
				go func(packet *Packet) {
					var res *Result
					var code = byte(packet.code)

					if code == PingRequest {
						s.output <- packIprotoOk(packet.requestID)
					} else {
						res = s.handler(packet.request.(Query))
						body, _ = res.pack(packet.requestID)
						s.output <- body
					}
				}(packet)
			}
		}
	}

	s.Close()
}

func (s *IprotoServer) write() {
	var err error
	var n int

	w := s.writer

WRITER_LOOP:
	for {
		select {
		case messageBody, ok := <-s.output:
			if !ok {
				break WRITER_LOOP
			}
			n, err = w.Write(messageBody)
			if err != nil || n != len(messageBody) {
				break WRITER_LOOP
			}
		case <-s.quit:
			w.Flush()
			break WRITER_LOOP
		default:
			if err = w.Flush(); err != nil {
				break WRITER_LOOP
			}

			// same without flush
			select {
			case messageBody, ok := <-s.output:
				if !ok {
					break WRITER_LOOP
				}
				n, err = w.Write(messageBody)
				if err != nil || n != len(messageBody) {
					break WRITER_LOOP
				}
			case <-s.quit:
				w.Flush()
				break WRITER_LOOP
			}

		}
	}

	s.Close()
}