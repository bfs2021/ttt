package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

func pipeConn(t *testing.T, opts Options) (*Conn, *Codec) {
	t.Helper()
	connR, peerW := io.Pipe()
	peerR, connW := io.Pipe()
	t.Cleanup(func() {
		peerW.Close()
		connW.Close()
	})
	return NewConn(connR, connW, opts), NewCodec(peerR, peerW)
}

func receive(t *testing.T, codec *Codec) Message {
	t.Helper()
	ch := make(chan Message, 1)
	go func() {
		msg, err := codec.Receive()
		if err == nil {
			ch <- msg
		}
	}()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("no message from conn")
		return Message{}
	}
}

func TestConnAnswersPeerRequest(t *testing.T) {
	_, peer := pipeConn(t, Options{
		OnRequest: func(method string, params json.RawMessage) (any, error) {
			if method == "fail" {
				return nil, errors.New("boom")
			}
			if method == "custom" {
				return nil, &Error{Code: 42, Message: "custom"}
			}
			var n []int
			_ = json.Unmarshal(params, &n)
			return n[0] + n[1], nil
		},
	})

	peer.Send(map[string]any{"jsonrpc": "2.0", "id": "a", "method": "add", "params": []int{2, 3}})
	if reply := receive(t, peer); string(reply.ID) != `"a"` || string(reply.Result) != "5" {
		t.Fatalf("add reply = id %s result %s", reply.ID, reply.Result)
	}
	peer.Send(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "fail"})
	if reply := receive(t, peer); reply.Error == nil || reply.Error.Code != CodeInternalError || reply.Error.Message != "boom" {
		t.Fatalf("fail reply error = %+v", reply.Error)
	}
	peer.Send(map[string]any{"jsonrpc": "2.0", "id": 8, "method": "custom"})
	if reply := receive(t, peer); reply.Error == nil || reply.Error.Code != 42 {
		t.Fatalf("custom reply error = %+v", reply.Error)
	}
}

func TestConnWithoutRequestHandlerRepliesMethodNotFound(t *testing.T) {
	_, peer := pipeConn(t, Options{})
	peer.Send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "anything"})
	if reply := receive(t, peer); reply.Error == nil || reply.Error.Code != CodeMethodNotFound {
		t.Fatalf("reply error = %+v", reply.Error)
	}
}

func TestConnNullResultIsSent(t *testing.T) {
	_, peer := pipeConn(t, Options{
		OnRequest: func(string, json.RawMessage) (any, error) { return nil, nil },
	})
	peer.Send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "noop"})
	if reply := receive(t, peer); string(reply.Result) != "null" || reply.Error != nil {
		t.Fatalf("reply = result %q error %+v", reply.Result, reply.Error)
	}
}

func TestConnDeliversNotificationsInOrder(t *testing.T) {
	got := make(chan string, 3)
	_, peer := pipeConn(t, Options{
		OnNotification: func(method string, _ json.RawMessage) { got <- method },
	})
	for _, m := range []string{"a", "b", "c"} {
		peer.Send(map[string]any{"jsonrpc": "2.0", "method": m})
	}
	for _, want := range []string{"a", "b", "c"} {
		select {
		case m := <-got:
			if m != want {
				t.Fatalf("notification = %q, want %q", m, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("notification not delivered")
		}
	}
}

func TestConnCallCancellationForgetsPendingAndReportsID(t *testing.T) {
	cancelled := make(chan int, 1)
	conn, peer := pipeConn(t, Options{OnCancel: func(id int) { cancelled <- id }})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := conn.Call(ctx, "slow", nil)
		errc <- err
	}()
	req := receive(t, peer)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("call error = %v", err)
	}
	if id := <-cancelled; string(req.ID) != "1" || id != 1 {
		t.Fatalf("cancelled id = %d, request id = %s", id, req.ID)
	}
	conn.mu.Lock()
	pending := len(conn.pending)
	conn.mu.Unlock()
	if pending != 0 {
		t.Fatalf("cancelled call left %d pending", pending)
	}
}

func TestConnCloseReleasesCallsBeforeOnClose(t *testing.T) {
	connR, peerW := io.Pipe()
	sent := make(chan struct{}, 4)
	connW := writerFunc(func(p []byte) (int, error) {
		sent <- struct{}{}
		return len(p), nil
	})
	released := make(chan error, 1)
	closed := make(chan error, 1)
	conn := NewConn(connR, connW, Options{OnClose: func(err error) {
		closed <- <-released
	}})
	go func() {
		_, err := conn.Call(context.Background(), "never", nil)
		released <- err
	}()
	<-sent
	peerW.Close()
	select {
	case err := <-closed:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("pending call error = %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnClose never ran")
	}
	<-conn.Done()
	if _, err := conn.Call(context.Background(), "after", nil); err == nil {
		t.Fatal("call after close succeeded")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
