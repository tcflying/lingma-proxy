package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"lingma-ipc-proxy/internal/service"
	"lingma-ipc-proxy/internal/toolemulation"
)

// The final frame of a Responses stream must reconcile with the events the
// client already saw: a whitespace-only text delta stays in the completed
// message byte for byte, and a reasoning item the stream never announced is
// replayed with its original bytes rather than a trimmed re-derivation.

func lifecycle931Events(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			t.Fatalf("event payload is not JSON: %v\n%s", err, payload)
		}
		events = append(events, event)
	}
	return events
}

func TestLifecycle931WhitespaceTextSurvivesCompletedFrame(t *testing.T) {
	w := httptest.NewRecorder()
	e := newOpenAIResponseStreamEmitter(w, w, "resp_ws")
	if err := writeOpenAIResponseMessageStarted(e, "msg_ws", 0); err != nil {
		t.Fatal(err)
	}
	if err := e.Event("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "item_id": "msg_ws",
		"output_index": 0, "content_index": 0, "delta": " \n",
	}); err != nil {
		t.Fatal(err)
	}
	result := &service.ChatResult{Model: "test"}
	writeOpenAIResponseStreamCompleted(e, "resp_ws", 1, "test", result, "msg_ws", true, false, false, " \n")

	var delta, doneText, completedText string
	for _, event := range lifecycle931Events(t, w.Body.String()) {
		switch event["type"] {
		case "response.output_text.delta":
			delta += event["delta"].(string)
		case "response.output_text.done":
			doneText = event["text"].(string)
		case "response.completed":
			for _, rawItem := range event["response"].(map[string]any)["output"].([]any) {
				item := rawItem.(map[string]any)
				if item["type"] != "message" {
					continue
				}
				for _, part := range item["content"].([]any) {
					completedText += part.(map[string]any)["text"].(string)
				}
			}
		}
	}
	if delta != " \n" {
		t.Fatalf("delta text = %q, want the whitespace that was streamed", delta)
	}
	if doneText != delta {
		t.Fatalf("output_text.done = %q, want it to match the streamed bytes %q", doneText, delta)
	}
	if completedText != delta {
		t.Fatalf("completed message text = %q, want it to match the streamed bytes %q", completedText, delta)
	}
}

func TestLifecycle931ReasoningReplayKeepsOriginalBytes(t *testing.T) {
	w := httptest.NewRecorder()
	e := newOpenAIResponseStreamEmitter(w, w, "resp_late")
	result := &service.ChatResult{Model: "test", Text: "answer", ThoughtText: " thought with trailing space \n"}
	// reasoningWanted=true, reasoningOpened=false: the final frame owes the
	// client the full lifecycle of a reasoning item it never streamed.
	writeOpenAIResponseStreamCompleted(e, "resp_late", 1, "test", result, "msg_late", false, true, false, "")

	thought := " thought with trailing space \n"
	var announcedDone, completed []any
	var replayDeltas, replayDone string
	for _, event := range lifecycle931Events(t, w.Body.String()) {
		switch event["type"] {
		case "response.reasoning_summary_text.delta":
			replayDeltas += event["delta"].(string)
		case "response.reasoning_summary_text.done":
			replayDone = event["text"].(string)
		case "response.output_item.done":
			announcedDone = append(announcedDone, event["item"])
		case "response.completed":
			completed = event["response"].(map[string]any)["output"].([]any)
		}
	}
	if replayDeltas != thought {
		t.Fatalf("replay deltas = %q, want the original bytes %q", replayDeltas, thought)
	}
	if replayDone != thought {
		t.Fatalf("replay summary done = %q, want the original bytes %q", replayDone, thought)
	}
	if !reflect.DeepEqual(announcedDone, completed) {
		t.Fatalf("announced done items differ from completed output: done=%#v output=%#v", announcedDone, completed)
	}
}

// The reasoning item the stream DID announce keeps the same contract: Close
// reports the accumulated bytes untouched, so the done item the client saw
// equals the completed snapshot.
func TestLifecycle931AnnouncedReasoningCloseMatchesAccumulated(t *testing.T) {
	w := httptest.NewRecorder()
	e := newOpenAIResponseStreamEmitter(w, w, "resp_open")
	writer := newResponseReasoningWriter(e, "rs_open", 0)
	if err := writer.Delta(" leading space"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Delta(" and trailing \n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	want := " leading space and trailing \n"
	for _, event := range lifecycle931Events(t, w.Body.String()) {
		if event["type"] != "response.output_item.done" {
			continue
		}
		item := event["item"].(map[string]any)
		if item["type"] != "reasoning" {
			continue
		}
		summary := item["summary"].([]any)[0].(map[string]any)
		if summary["text"] != want {
			t.Fatalf("closed reasoning text = %#v, want the accumulated bytes %#v", summary["text"], want)
		}
	}
}

var _ = toolemulation.ToolCall{} // keep the import stable if fixtures shrink
