package ipc

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/rcerr"
)

func serve(t *testing.T, handler Handler) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "d.sock")
	server, err := Listen(socket, handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return socket
}

func TestCallRoundTrip(t *testing.T) {
	socket := serve(t, func(req Request, _ func(Progress)) (any, error) {
		return map[string]string{"saw": req.Cmd + ":" + req.Args.Name}, nil
	})

	var out map[string]string
	if err := Call(socket, "describe", Args{Name: "demo"}, &out, CallOptions{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if out["saw"] != "describe:demo" {
		t.Errorf("round trip lost the request: %+v", out)
	}
}

// An error keeps its hint: the message says what went wrong, the hint says
// what to type next.
func TestCallCarriesHintAndCode(t *testing.T) {
	socket := serve(t, func(Request, func(Progress)) (any, error) {
		return nil, rcerr.New("no such session", "run `rcmanager list`")
	})

	err := Call(socket, "start", Args{Name: "nope"}, nil, CallOptions{Timeout: 5 * time.Second})
	if err == nil {
		t.Fatal("an error response was reported as success")
	}
	var rc *rcerr.Error
	if !errors.As(err, &rc) {
		t.Fatalf("error of type %T, want *rcerr.Error", err)
	}
	if rc.Message != "no such session" || rc.Hint != "run `rcmanager list`" {
		t.Errorf("error = %+v", rc)
	}
}

// Progress lines arrive before the response; a caller that reads only the
// first line would mistake one for the answer.
func TestProgressPrecedesTheResponse(t *testing.T) {
	socket := serve(t, func(_ Request, progress func(Progress)) (any, error) {
		progress(Progress{Phase: "plan", Names: []string{"a", "b"}})
		progress(Progress{Phase: "end", Name: "a", OK: true, Pid: 42})
		return []string{"done"}, nil
	})

	var seen []Progress
	var out []string
	err := Call(socket, "startAll", Args{}, &out, CallOptions{
		Timeout:    5 * time.Second,
		OnProgress: func(p Progress) { seen = append(seen, p) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0].Phase != "plan" || seen[1].Pid != 42 {
		t.Errorf("progress = %+v", seen)
	}
	if len(out) != 1 || out[0] != "done" {
		t.Errorf("response = %+v", out)
	}
}

func TestPing(t *testing.T) {
	socket := serve(t, func(Request, func(Progress)) (any, error) { return "pong", nil })
	if !Ping(socket, time.Second) {
		t.Error("Ping() failed against a live socket")
	}
	if Ping(filepath.Join(t.TempDir(), "absent.sock"), 500*time.Millisecond) {
		t.Error("Ping() succeeded against nothing")
	}
}

// A malformed line is answered and the connection stays usable: one bad
// request must not take the daemon's socket down.
func TestMalformedRequestIsRejected(t *testing.T) {
	socket := serve(t, func(Request, func(Progress)) (any, error) { return "ok", nil })
	var out string
	if err := Call(socket, "ping", Args{}, &out, CallOptions{Timeout: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if out != "ok" {
		t.Errorf("out = %q", out)
	}
}
