package jsonrpc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

type Request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// Message is any message read from the peer, including requests that must be
// answered. ID stays raw so a string id can be echoed back exactly as sent.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// HasID treats "id": null like an absent id; some peers send it on
// notifications.
func (m *Message) HasID() bool {
	return len(m.ID) > 0 && string(m.ID) != "null"
}

func (m *Message) IsNotification() bool {
	return !m.HasID() && m.Method != ""
}

func (m *Message) IsRequest() bool {
	return m.HasID() && m.Method != ""
}

// Reply answers a peer request. Result must be set (to the JSON literal null
// if nothing else) unless Error is, because the protocol requires exactly one
// of the two.
type Reply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

const (
	CodeMethodNotFound = -32601
	CodeInternalError  = -32603
)

type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

type Codec struct {
	reader           *bufio.Reader
	writer           io.Writer
	mu               sync.Mutex
	maxContentLength int
}

func NewCodec(r io.Reader, w io.Writer) *Codec {
	return &Codec{
		reader: bufio.NewReader(r),
		writer: w,
	}
}

// SetMaxContentLength makes Receive reject messages larger than n bytes before
// allocating them; 0 means no limit. Set it for peers that are not trusted.
func (c *Codec) SetMaxContentLength(n int) {
	c.maxContentLength = n
}

func (c *Codec) Send(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := io.WriteString(c.writer, header); err != nil {
		return err
	}
	_, err = c.writer.Write(data)
	return err
}

func (c *Codec) Receive() (Message, error) {
	contentLen := -1
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return Message{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if val, ok := strings.CutPrefix(line, "Content-Length: "); ok {
			n, err := strconv.Atoi(val)
			if err != nil {
				return Message{}, fmt.Errorf("bad Content-Length: %w", err)
			}
			if c.maxContentLength > 0 && n > c.maxContentLength {
				return Message{}, fmt.Errorf("message of %d bytes exceeds the %d byte limit", n, c.maxContentLength)
			}
			contentLen = n
		}
	}
	if contentLen < 0 {
		return Message{}, fmt.Errorf("missing Content-Length header")
	}
	body := make([]byte, contentLen)
	if _, err := io.ReadFull(c.reader, body); err != nil {
		return Message{}, err
	}
	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		return Message{}, err
	}
	return msg, nil
}
