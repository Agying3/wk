package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

const observedRequestID = "cmb-0123456789abcdef0123456789abcdef"

func observedClient(body io.Reader) *Client {
	return testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(body)}, nil
	})
}

func TestDesktopObservationReadsLateUsageAndRealFinish(t *testing.T) {
	stream := "data: {\"id\":\"" + observedRequestID + "\",\"model\":\"actual-model\",\"choices\":[{\"delta\":{\"content\":\"2\"}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"length\"}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":37,\"completion_tokens\":9,\"total_tokens\":46,\"prompt_tokens_details\":{\"cached_tokens\":12}}}\n\n" +
		"data: [DONE]\n\n"
	r, err := observedClient(strings.NewReader(stream)).DesktopChatWithExpertObserved(&auth.Auth{UID: "fixture-account"}, "")
	if err != nil || r.RequestID != observedRequestID || r.FirstTokenAt == 0 || r.FinishedAt < r.StartedAt {
		t.Fatalf("invalid observation: %+v err=%v", r, err)
	}
	for _, event := range r.Events() {
		switch event["eventCode"] {
		case "chat_request_send":
			if event["inputLength"] != 12 {
				t.Fatalf("wrong real input length: %v", event)
			}
		case "chat_message_response", "chat_request_response":
			if event["inputToken"] != float64(37) || event["outputToken"] != float64(9) || event["totalToken"] != float64(46) || event["cachedTokens"] != float64(12) || event["finishReason"] != "length" {
				t.Fatalf("template counters replaced real usage: %v", event)
			}
		}
	}
}

func TestDesktopObservationOmitsUnknownMetrics(t *testing.T) {
	r := DesktopChatResult{ConversationID: "fixture-conversation", RequestID: observedRequestID, ModelID: "fixture-model"}
	for _, event := range r.Events() {
		if event["eventCode"] == "chat_message_response" || event["eventCode"] == "chat_request_response" {
			for _, key := range []string{"inputToken", "outputToken", "totalToken", "cachedTokens", "cachedWriteTokens", "cachedMissTokens", "firstTokenAt"} {
				if _, exists := event[key]; exists {
					t.Fatalf("unknown measurement %s was fabricated: %v", key, event)
				}
			}
		}
	}
}

func TestDesktopObservationDoesNotSucceedOnEarlyID(t *testing.T) {
	prefix := "data: {\"id\":\"" + observedRequestID + "\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
	for _, body := range []io.Reader{
		strings.NewReader(prefix),
		io.MultiReader(strings.NewReader(prefix), failingStreamReader{io.ErrUnexpectedEOF}),
		strings.NewReader(prefix + "data: {\"error\":{\"message\":\"fixture error\"}}\n\n"),
	} {
		if _, err := observedClient(body).DesktopChatWithExpertObserved(&auth.Auth{UID: "fixture-account"}, ""); err == nil {
			t.Fatal("a request ID was mistaken for a successful completed chat")
		}
	}
}

func TestDesktopObservationPreservesLegacyTemplate(t *testing.T) {
	legacy := DesktopChatSequence("fixture-conversation", observedRequestID, "fixture-message", "fast-model", "fast-model")
	if legacy[3]["inputToken"] != 120 || legacy[3]["outputToken"] != 80 {
		t.Fatal("pure event task compatibility changed")
	}
}
