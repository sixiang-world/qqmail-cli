package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

const (
	hangingCLITestAddress = "QQMAIL_MCP_TEST_HANG_ADDRESS"
	lifecycleTestTimeout  = 6 * time.Second
)

func TestMain(m *testing.M) {
	if addr := os.Getenv(hangingCLITestAddress); addr != "" {
		conn, err := net.DialTimeout("tcp", addr, lifecycleTestTimeout)
		if err != nil {
			os.Exit(1)
		}
		// The connection signals that the CLI is running, and closing it
		// proves the process stopped without relying on process IDs or sleeps.
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		_, _ = io.Copy(io.Discard, conn)
		_ = conn.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func hangingCLIFixture(t *testing.T) *net.TCPListener {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("QQMAIL_CLI_BIN", binary)
	t.Setenv(hangingCLITestAddress, listener.Addr().String())
	return listener
}

func awaitChild(t *testing.T, listener *net.TCPListener) net.Conn {
	t.Helper()
	if err := listener.SetDeadline(time.Now().Add(lifecycleTestTimeout)); err != nil {
		t.Fatal(err)
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("fake CLI did not start: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func awaitChildExit(t *testing.T, conn net.Conn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(lifecycleTestTimeout)); err != nil {
		t.Fatal(err)
	}
	// Windows may reset the connection when the subprocess is killed.
	const winsockConnectionReset = syscall.Errno(10054) // WSAECONNRESET
	n, err := conn.Read(make([]byte, 1))
	reset := errors.Is(err, syscall.ECONNRESET) || (runtime.GOOS == "windows" && errors.Is(err, winsockConnectionReset))
	if n != 0 || (!errors.Is(err, io.EOF) && !reset) {
		t.Fatalf("fake CLI did not exit promptly: %v", err)
	}
}

func startMCPServer(t *testing.T) (*io.PipeWriter, <-chan []byte, <-chan error) {
	t.Helper()
	in, input := io.Pipe()
	output, out := io.Pipe()
	lines := make(chan []byte, 128)
	done := make(chan error, 1)
	t.Cleanup(func() {
		_ = input.Close()
		_ = in.Close()
		_ = output.Close()
		_ = out.Close()
	})
	go func() {
		done <- serve(in, out)
		_ = out.Close()
	}()
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			lines <- append([]byte(nil), scanner.Bytes()...)
		}
	}()
	writeMCPRequest(t, input, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if response := awaitMCPResponse(t, lines); response.ID != "init" || response.Error != nil {
		t.Fatalf("initialization failed: %+v", response)
	}
	writeMCPRequest(t, input, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return input, lines, done
}

func writeMCPRequest(t *testing.T, input io.Writer, line string) {
	t.Helper()
	if _, err := fmt.Fprintln(input, line); err != nil {
		t.Fatal(err)
	}
}

func callHangingCLI(t *testing.T, input io.Writer, id string) {
	t.Helper()
	writeMCPRequest(t, input, `{"jsonrpc":"2.0","id":`+id+`,"method":"tools/call","params":{"name":"qqmail_folders","arguments":{}}}`)
}

type mcpTestResponse struct {
	ID    string `json:"id"`
	Error *struct {
		Code int `json:"code"`
	} `json:"error"`
}

func awaitMCPResponse(t *testing.T, lines <-chan []byte) mcpTestResponse {
	t.Helper()
	select {
	case line := <-lines:
		var response mcpTestResponse
		if err := json.Unmarshal(line, &response); err != nil {
			t.Fatalf("invalid MCP response %q: %v", line, err)
		}
		return response
	case <-time.After(lifecycleTestTimeout):
		t.Fatal("MCP response timed out")
		return mcpTestResponse{}
	}
}

func awaitMCPExit(t *testing.T, lines <-chan []byte, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(lifecycleTestTimeout):
		t.Fatal("server did not stop after stdin closed")
	}
	select {
	case line, ok := <-lines:
		if ok {
			t.Fatalf("unexpected MCP response: %s", line)
		}
	case <-time.After(lifecycleTestTimeout):
		t.Fatal("server output did not close")
	}
}

func TestMCPCancellationStopsChildWithoutResponding(t *testing.T) {
	for _, tc := range []struct{ name, id string }{
		{"exact ID", `"to-stop"`},
		{"escaped ID", `"to\u002dstop"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := hangingCLIFixture(t)
			input, lines, done := startMCPServer(t)
			callHangingCLI(t, input, tc.id)
			child := awaitChild(t, listener)
			writeMCPRequest(t, input, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"to-stop"}}`)
			awaitChildExit(t, child)
			writeMCPRequest(t, input, `{"jsonrpc":"2.0","id":"ping","method":"ping"}`)
			if resp := awaitMCPResponse(t, lines); resp.ID != "ping" || resp.Error != nil {
				t.Fatalf("expected only ping response, got %+v", resp)
			}
			_ = input.Close()
			awaitMCPExit(t, lines, done)
		})
	}
}

func TestMCPDisconnectCancelsHangingCLI(t *testing.T) {
	listener := hangingCLIFixture(t)
	input, lines, done := startMCPServer(t)
	callHangingCLI(t, input, `"disconnect"`)
	child := awaitChild(t, listener)
	_ = input.Close()
	awaitChildExit(t, child)
	awaitMCPExit(t, lines, done)
}

func TestMCPRejectsToolCallBurstWhileAtCapacity(t *testing.T) {
	listener := hangingCLIFixture(t)
	input, lines, done := startMCPServer(t)
	var children []net.Conn
	for i := range maxConcurrent {
		callHangingCLI(t, input, fmt.Sprintf(`"active-%d"`, i))
		children = append(children, awaitChild(t, listener))
	}
	const burstSize = 32
	for i := range burstSize {
		callHangingCLI(t, input, fmt.Sprintf(`"burst-%d"`, i))
	}
	writeMCPRequest(t, input, `{"jsonrpc":"2.0","id":"ping","method":"ping"}`)
	for i := range burstSize {
		resp := awaitMCPResponse(t, lines)
		if resp.ID != fmt.Sprintf("burst-%d", i) || resp.Error == nil || resp.Error.Code != -32000 {
			t.Fatalf("tool call was not immediately rejected at capacity: %+v", resp)
		}
	}
	if resp := awaitMCPResponse(t, lines); resp.ID != "ping" || resp.Error != nil {
		t.Fatalf("server did not handle ping at capacity: %+v", resp)
	}
	_ = input.Close()
	for _, child := range children {
		awaitChildExit(t, child)
	}
	awaitMCPExit(t, lines, done)
}
