package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// countedBody records how many bytes a caller actually pulled off the wire, so a
// read cap is observed directly instead of being inferred from the returned error.
type countedBody struct {
	reader io.Reader
	read   int
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}

func (b *countedBody) Close() error { return nil }

// fixedResponseTransport serves one canned response with a counted body.
type fixedResponseTransport struct {
	status int
	body   *countedBody
}

func (t *fixedResponseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", t.status, http.StatusText(t.status)),
		StatusCode:    t.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{},
		Body:          t.body,
		ContentLength: -1,
		Request:       req,
	}, nil
}

// fixedResponseClient points a Client at one canned response and returns the byte
// counter. chatTestClient supplies a credential file, so neither path touches the
// real login cache or the base-URL candidate scan.
func fixedResponseClient(t *testing.T, status int, body string) (*Client, *countedBody) {
	t.Helper()
	counter := &countedBody{reader: strings.NewReader(body)}
	client := chatTestClient(t, "http://fixed.invalid")
	client.client = &http.Client{Transport: &fixedResponseTransport{status: status, body: counter}}
	return client, counter
}

// 933 P3-R1: the model list is served by a probed candidate host, so its body is
// untrusted. A well-formed but enormous payload used to be read in full and parsed,
// which is a heap the endpoint chooses the size of.
func TestListModelsRejectsResponseOverCap(t *testing.T) {
	payload := `{"chat":[{"id":"` + strings.Repeat("x", maxModelListResponseBytes) + `"}]}`
	if len(payload) <= maxModelListResponseBytes {
		t.Fatalf("fixture is %d bytes, must exceed the %d byte cap", len(payload), maxModelListResponseBytes)
	}
	client, _ := fixedResponseClient(t, http.StatusOK, payload)

	models, err := client.ListModels(context.Background())
	if err == nil {
		t.Fatalf("an over-cap model list was accepted: %d model(s) parsed from %d bytes, cap is %d",
			len(models), len(payload), maxModelListResponseBytes)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(maxModelListResponseBytes)) {
		t.Fatalf("error %q does not name the %d byte cap", err, maxModelListResponseBytes)
	}
}

// 933 P3-R1: the cap has to stop the read, not merely reject the result afterwards.
func TestListModelsReadsAtMostCapBytes(t *testing.T) {
	payload := strings.Repeat("x", maxModelListResponseBytes+4096)
	client, counter := fixedResponseClient(t, http.StatusOK, payload)

	_, _ = client.ListModels(context.Background())
	if counter.read > maxModelListResponseBytes+1 {
		t.Fatalf("read %d bytes of a %d byte body, want at most cap+1 = %d",
			counter.read, len(payload), maxModelListResponseBytes+1)
	}
}

// 933 P3-R1: the cap is inclusive, so a real catalog sitting just under it still
// answers. This is the off-by-one guard for the test above.
func TestListModelsAcceptsResponseUnderCap(t *testing.T) {
	client, _ := fixedResponseClient(t, http.StatusOK, `{"chat":[{"id":"kmodel"}],"inline":[{"id":"inline-model"}]}`)

	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %#v, want the chat and inline entries", models)
	}
}

// 933 P3-R1: the chat error path read the whole failure body only to keep its
// first 1000 runes. A 1 MB error body from an upstream outage is enough to matter
// once four such turns are in flight; the message itself must not change.
func TestChatErrorBodyReadsAtMostCapBytes(t *testing.T) {
	body := "upstream exploded: " + strings.Repeat("E", 1<<20)
	client, counter := fixedResponseClient(t, http.StatusInternalServerError, body)

	_, err := client.Chat(context.Background(), ChatRequest{Model: "kmodel", Prompt: "hi"}, nil)
	if err == nil {
		t.Fatal("a 500 chat response was reported as success")
	}
	if !strings.Contains(err.Error(), "remote chat status 500") ||
		!strings.Contains(err.Error(), "upstream exploded:") {
		t.Fatalf("error %q lost the status prefix or the upstream text", err)
	}
	if counter.read > maxChatErrorResponseBytes+1 {
		t.Fatalf("read %d bytes of a %d byte error body, want at most cap+1 = %d",
			counter.read, len(body), maxChatErrorResponseBytes+1)
	}
}

// 933 P3-R1 ordering guard: capping the model-list read must not cost an over-cap
// error body its status classification. An overloaded gateway that answers 500
// with a huge page still has to read as retryable, or a page that refreshes by
// itself turns into a permanent failure.
func TestModelListErrorBodyOverCapKeepsItsStatus(t *testing.T) {
	body := strings.Repeat("upstream detail ", maxModelListResponseBytes)
	client, counter := fixedResponseClient(t, http.StatusServiceUnavailable, body)

	_, err := client.ListModels(context.Background())
	if err == nil {
		t.Fatal("a 503 model list was reported as success")
	}
	if !errors.Is(err, ErrTransientUpstream) {
		t.Fatalf("error %q lost ErrTransientUpstream once the body exceeded the cap", err)
	}
	if counter.read > maxModelListResponseBytes+1 {
		t.Fatalf("read %d bytes of a %d byte error body, want at most cap+1 = %d",
			counter.read, len(body), maxModelListResponseBytes+1)
	}
}
