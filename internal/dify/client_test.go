package dify

import (
	"strings"
	"testing"
)

func TestReadStreamConcatenatesParts(t *testing.T) {
	sse := "" +
		"data: {\"event\": \"workflow_started\"}\n\n" +
		"data: {\"event\": \"message\", \"answer\": \"Investigating \"}\n\n" +
		"data: {\"event\": \"agent_message\", \"answer\": \"argocd \"}\n\n" +
		"data: {\"event\": \"message\", \"answer\": \"drops in ts-2.\"}\n\n" +
		"data: {\"event\": \"message_end\"}\n\n" +
		"data: [DONE]\n\n"

	got, err := readStream(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	if got != "Investigating argocd drops in ts-2." {
		t.Fatalf("got %q", got)
	}
}

func TestReadStreamMessageReplaceResets(t *testing.T) {
	sse := "" +
		"data: {\"event\": \"message\", \"answer\": \"partial bad\"}\n\n" +
		"data: {\"event\": \"message_replace\", \"answer\": \"cleaned answer\"}\n\n"
	got, err := readStream(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	if got != "cleaned answer" {
		t.Fatalf("got %q", got)
	}
}

func TestReadStreamReturnsAnswerOnAbruptEOF(t *testing.T) {
	// Full answer received, but the stream ends without a trailing newline /
	// clean close (unexpected EOF). The answer must still be returned.
	sse := "data: {\"event\": \"message\", \"answer\": \"complete answer\"}\n\n" +
		"data: {\"event\": \"message\", \"answer\": \" more\"}"
	got, err := readStream(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "complete answer more" {
		t.Fatalf("got %q", got)
	}
}

func TestReadStreamErrorEvent(t *testing.T) {
	sse := "data: {\"event\": \"error\", \"code\": \"bad\", \"message\": \"boom\"}\n\n"
	if _, err := readStream(strings.NewReader(sse)); err == nil {
		t.Fatal("expected error from error event")
	}
}
