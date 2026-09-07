package gateway

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestStreamIdleReadCloserTimesOutBlockedRead(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	defer writer.Close()
	source := newStreamIdleReadCloser(reader, 20*time.Millisecond)
	defer source.Close()
	started := time.Now()
	_, err := source.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("Read() error = nil")
	}
	if !source.TimedOut() {
		t.Fatal("TimedOut() = false")
	}
	if elapsed := time.Since(started); elapsed < 15*time.Millisecond {
		t.Fatalf("Read() elapsed = %s, want timeout delay", elapsed)
	}
}

func TestStreamIdleReadCloserDoesNotCountDownstreamDelay(t *testing.T) {
	t.Parallel()

	source := newStreamIdleReadCloser(
		io.NopCloser(strings.NewReader("ab")),
		20*time.Millisecond,
	)
	defer source.Close()
	buffer := make([]byte, 1)
	if count, err := source.Read(buffer); count != 1 || err != nil {
		t.Fatalf("first Read() = %d, %v", count, err)
	}
	time.Sleep(40 * time.Millisecond)
	if source.TimedOut() {
		t.Fatal("downstream delay triggered idle timeout")
	}
	if count, err := source.Read(buffer); count != 1 || err != nil {
		t.Fatalf("second Read() = %d, %v", count, err)
	}
}
