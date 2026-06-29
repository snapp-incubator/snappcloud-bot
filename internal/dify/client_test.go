package dify

import (
	"strings"
	"testing"
)

func TestReadStreamConcatenatesParts(t *testing.T) {
	sse := "" +
		"data: {\"event\": \"workflow_started\"}\n\n" +
		"data: {\"event\": \"message\", \"conversation_id\": \"conv-9\", \"answer\": \"Investigating \"}\n\n" +
		"data: {\"event\": \"agent_message\", \"answer\": \"argocd \"}\n\n" +
		"data: {\"event\": \"message\", \"answer\": \"drops in ts-2.\"}\n\n" +
		"data: {\"event\": \"message_end\"}\n\n" +
		"data: [DONE]\n\n"

	got, err := readStream(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	if got.Answer != "Investigating argocd drops in ts-2." {
		t.Fatalf("got %q", got.Answer)
	}
	if got.ConversationID != "conv-9" {
		t.Fatalf("conversation id not captured: %q", got.ConversationID)
	}
}

func TestReadStreamMessageReplaceResets(t *testing.T) {
	sse := "" +
		"data: {\"event\": \"message\", \"answer\": \"partial bad\"}\n\n" +
		"data: {\"event\": \"message_replace\", \"answer\": \"cleaned answer\"}\n\n" +
		"data: {\"event\": \"message_end\"}\n\n"
	got, err := readStream(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	if got.Answer != "cleaned answer" {
		t.Fatalf("got %q", got.Answer)
	}
}

func TestReadStreamReturnsPartialOnAbruptEOFWithNotice(t *testing.T) {
	// Stream cut mid-run (no message_end). The accumulated text must be returned
	// (not discarded), flagged as truncated.
	sse := "data: {\"event\": \"message\", \"answer\": \"complete answer\"}\n\n" +
		"data: {\"event\": \"message\", \"answer\": \" more\"}"
	got, err := readStream(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !strings.HasPrefix(got.Answer, "complete answer more") {
		t.Fatalf("partial answer missing: %q", got)
	}
	if !strings.Contains(got.Answer, "cut off") {
		t.Fatalf("truncation notice missing: %q", got)
	}
}

func TestReadStreamCleanFinishNoNotice(t *testing.T) {
	sse := "data: {\"event\": \"message\", \"answer\": \"done answer\"}\n\n" +
		"data: {\"event\": \"message_end\"}\n\n"
	got, err := readStream(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	if got.Answer != "done answer" {
		t.Fatalf("got %q", got.Answer)
	}
}

func TestReadStreamErrorEvent(t *testing.T) {
	sse := "data: {\"event\": \"error\", \"code\": \"bad\", \"message\": \"boom\"}\n\n"
	if _, err := readStream(strings.NewReader(sse)); err == nil {
		t.Fatal("expected error from error event")
	}
}
