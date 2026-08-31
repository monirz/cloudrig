package pubsub

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newService(t *testing.T) *Service {
	t.Helper()
	return New(store.NewMemory(), clock.NewFake(epoch), nil)
}

func TestValidName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		kind    string
		wantErr bool
	}{
		{name: "projects/p/topics/t", kind: "topics"},
		{name: "projects/p/subscriptions/s", kind: "subscriptions"},
		{name: "topics/t", kind: "topics", wantErr: true},
		{name: "projects/p/topics", kind: "topics", wantErr: true},
		{name: "projects//topics/t", kind: "topics", wantErr: true},
		{name: "projects/p/topics/", kind: "topics", wantErr: true},
		// A subscription name where a topic is wanted.
		{name: "projects/p/subscriptions/s", kind: "topics", wantErr: true},
		{name: "", kind: "topics", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name+" as "+tc.kind, func(t *testing.T) {
			t.Parallel()
			err := validName(tc.name, tc.kind)
			if tc.wantErr && err == nil {
				t.Errorf("accepted %q", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("rejected %q: %v", tc.name, err)
			}
		})
	}
}

func TestProjectOf(t *testing.T) {
	t.Parallel()

	if got := projectOf("projects/my-project/topics/t"); got != "my-project" {
		t.Errorf("projectOf = %q, want my-project", got)
	}
	if got := projectOf("nonsense"); got != "" {
		t.Errorf("projectOf(nonsense) = %q, want empty", got)
	}
}

func TestTakeMovesMessagesToOutstanding(t *testing.T) {
	t.Parallel()

	s := newService(t)
	const sub = "projects/p/subscriptions/s"
	for _, body := range []string{"a", "b", "c"} {
		s.backlog[sub] = append(s.backlog[sub], &pubsubpb.PubsubMessage{
			MessageId: body, Data: []byte(body),
		})
	}

	got := s.take(sub, 2)
	if len(got) != 2 || string(got[0].GetMessage().GetData()) != "a" {
		t.Fatalf("take = %d messages, first %q", len(got), got[0].GetMessage().GetData())
	}
	if len(s.backlog[sub]) != 1 {
		t.Errorf("backlog has %d left, want 1", len(s.backlog[sub]))
	}
	// Taken messages are outstanding until acknowledged, so a nack can return
	// them.
	if len(s.outstanding[sub]) != 2 {
		t.Errorf("outstanding = %d, want 2", len(s.outstanding[sub]))
	}

	if rest := s.take(sub, 10); len(rest) != 1 {
		t.Errorf("take of the remainder = %d, want 1", len(rest))
	}
	if empty := s.take(sub, 10); empty != nil {
		t.Errorf("take on an empty backlog = %v, want nil", empty)
	}
}

// TestSignalDoesNotBlock holds the rule that a publish never waits on a
// subscriber: the signal channel is buffered and the send non-blocking, so one
// pending wake-up is as good as many.
func TestSignalDoesNotBlock(t *testing.T) {
	t.Parallel()

	s := newService(t)
	const sub = "projects/p/subscriptions/s"

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.mu.Lock()
		for i := 0; i < 1000; i++ {
			s.signal(sub)
		}
		s.mu.Unlock()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("signal blocked with nothing reading")
	}

	// One wake-up is waiting, and only one.
	select {
	case <-s.waitCh(sub):
	default:
		t.Error("no signal was delivered")
	}
	select {
	case <-s.waitCh(sub):
		t.Error("more than one signal was queued")
	default:
	}
}

func TestMessageIDsIncrease(t *testing.T) {
	t.Parallel()

	s := newService(t)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := s.nextMessageID()
		if seen[id] {
			t.Fatalf("message id %q handed out twice", id)
		}
		seen[id] = true
	}
}

func TestKeysAreNamespaced(t *testing.T) {
	t.Parallel()

	// Topics and subscriptions must not collide even with the same short name.
	if topicKey("projects/p/topics/x") == subscriptionKey("projects/p/subscriptions/x") {
		t.Error("a topic and a subscription share a key")
	}
	if !strings.HasPrefix(topicKey("projects/p/topics/x"), topicPrefix) {
		t.Error("the topic key is not under the topic prefix")
	}
}

// TestMessagePayloadIsGen1Shaped pins the envelope a first-generation function
// reads. Data is base64 on the wire, so a handler that decodes it must find
// something to decode.
func TestMessagePayloadIsGen1Shaped(t *testing.T) {
	msg := &pubsubpb.PubsubMessage{
		Data:        []byte("order-42"),
		Attributes:  map[string]string{"region": "eu"},
		MessageId:   "7",
		PublishTime: timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}

	got := messagePayload(msg)
	want := map[string]any{
		"@type":       MessageType,
		"data":        base64.StdEncoding.EncodeToString([]byte("order-42")),
		"messageId":   "7",
		"publishTime": "2026-01-01T00:00:00Z",
		"attributes":  map[string]string{"region": "eu"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("payload =\n%#v\nwant\n%#v", got, want)
	}
}

// TestMessagePayloadOmitsEmptyAttributes keeps the envelope close to the real
// one, which has no attributes key when a message carries none.
func TestMessagePayloadOmitsEmptyAttributes(t *testing.T) {
	got := messagePayload(&pubsubpb.PubsubMessage{Data: []byte("x")})
	if _, ok := got["attributes"]; ok {
		t.Errorf("attributes present for a message with none: %#v", got)
	}
}

// TestSourceOfMatchesATopicTrigger is the join between the two packages: a
// trigger scoped to a bare topic name has to match the source this builds.
func TestSourceOfMatchesATopicTrigger(t *testing.T) {
	src := SourceOf("projects/p/topics/orders")
	if want := "//pubsub.googleapis.com/projects/p/topics/orders"; src != want {
		t.Fatalf("SourceOf = %q, want %q", src, want)
	}
	if !strings.HasSuffix(src, "/orders") {
		t.Errorf("a trigger on the bare name %q would not match %q", "orders", src)
	}
}
