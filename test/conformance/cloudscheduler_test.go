package conformance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	scheduler "cloud.google.com/go/scheduler/apiv1"
	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"github.com/monirz/cloudrig"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func csClient(t *testing.T) (*scheduler.CloudSchedulerClient, *cloudrig.Emulator, context.Context) {
	t.Helper()
	t.Parallel()

	emu := cloudrig.MustStart(t)
	ctx := context.Background()
	c, err := scheduler.NewCloudSchedulerClient(ctx,
		option.WithEndpoint(emu.Endpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("NewCloudSchedulerClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, emu, ctx
}

const csParent = "projects/test-project/locations/us-central1"

type hits struct {
	mu sync.Mutex
	n  int
}

func (h *hits) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.n++
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}
func (h *hits) count() int { h.mu.Lock(); defer h.mu.Unlock(); return h.n }

// TestJobRecursOnTheClock is the headline: an hourly job fires once per hour of
// advanced time, not once per real hour. A wall-clock emulator cannot do this.
func TestJobRecursOnTheClock(t *testing.T) {
	c, emu, ctx := csClient(t)

	target := &hits{}
	srv := httptest.NewServer(target.handler())
	t.Cleanup(srv.Close)

	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{
		Parent: csParent,
		Job: &schedulerpb.Job{
			Name:     csParent + "/jobs/hourly",
			Schedule: "0 * * * *", // top of every hour
			Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{
				Uri: srv.URL, HttpMethod: schedulerpb.HttpMethod_POST,
			}},
		},
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// Advance six hours; the job should have fired six times.
	for i := 0; i < 6; i++ {
		emu.FakeClock(t).Advance(time.Hour)
		emu.SyncScheduler()
	}
	waitHits(t, target, 6)
}

// TestPubsubJobPublishes covers the other common target: a scheduled publish
// must be a real message a subscriber receives, not just an internal event.
func TestPubsubJobPublishes(t *testing.T) {
	c, emu, ctx := csClient(t)

	const topic = "projects/test-project/topics/cron"
	const sub = "projects/test-project/subscriptions/cron-worker"
	ps := pubsubClientAt(t, emu)
	if _, err := ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name: sub, Topic: topic, AckDeadlineSeconds: 10,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{
		Parent: csParent,
		Job: &schedulerpb.Job{
			Name:     csParent + "/jobs/pub",
			Schedule: "0 * * * *",
			Target: &schedulerpb.Job_PubsubTarget{PubsubTarget: &schedulerpb.PubsubTarget{
				TopicName: topic, Data: []byte("tick"),
			}},
		},
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	emu.FakeClock(t).Advance(time.Hour)
	emu.SyncEvents()

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var got string
	_ = ps.Subscriber(sub).Receive(rctx, func(_ context.Context, m *pubsub.Message) {
		got = string(m.Data)
		m.Ack()
		cancel()
	})
	if got != "tick" {
		t.Errorf("received %q, want the scheduled message tick", got)
	}
}

// TestRunJobFiresNow covers forcing a job ahead of schedule.
func TestRunJobFiresNow(t *testing.T) {
	c, _, ctx := csClient(t)

	target := &hits{}
	srv := httptest.NewServer(target.handler())
	t.Cleanup(srv.Close)

	job, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{
		Parent: csParent,
		Job: &schedulerpb.Job{
			Name:     csParent + "/jobs/manual",
			Schedule: "0 0 1 1 *", // once a year
			Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{
				Uri: srv.URL, HttpMethod: schedulerpb.HttpMethod_POST,
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RunJob(ctx, &schedulerpb.RunJobRequest{Name: job.GetName()}); err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	waitHits(t, target, 1)
}

// TestPausedJobDoesNotFire holds pause and resume.
func TestPausedJobDoesNotFire(t *testing.T) {
	c, emu, ctx := csClient(t)

	target := &hits{}
	srv := httptest.NewServer(target.handler())
	t.Cleanup(srv.Close)

	name := csParent + "/jobs/pausable"
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{
		Parent: csParent,
		Job: &schedulerpb.Job{
			Name: name, Schedule: "* * * * *", // every minute
			Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{
				Uri: srv.URL, HttpMethod: schedulerpb.HttpMethod_POST,
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PauseJob(ctx, &schedulerpb.PauseJobRequest{Name: name}); err != nil {
		t.Fatal(err)
	}

	emu.FakeClock(t).Advance(10 * time.Minute)
	emu.SyncScheduler()
	if got := target.count(); got != 0 {
		t.Errorf("a paused job fired %d times", got)
	}

	if _, err := c.ResumeJob(ctx, &schedulerpb.ResumeJobRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	emu.FakeClock(t).Advance(2 * time.Minute)
	emu.SyncScheduler()
	waitHits(t, target, 1)
}

func TestCloudSchedulerErrors(t *testing.T) {
	c, _, ctx := csClient(t)

	// An invalid cron is rejected up front.
	if _, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{
		Parent: csParent,
		Job: &schedulerpb.Job{
			Name: csParent + "/jobs/bad", Schedule: "not a cron",
			Target: &schedulerpb.Job_HttpTarget{HttpTarget: &schedulerpb.HttpTarget{Uri: "http://x"}},
		},
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("invalid schedule = %v, want InvalidArgument", err)
	}
	if _, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{
		Name: csParent + "/jobs/ghost",
	}); status.Code(err) != codes.NotFound {
		t.Errorf("missing job = %v, want NotFound", err)
	}
}

func waitHits(t *testing.T, h *hits, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d fires arrived", h.count(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// pubsubClientAt dials the same emulator with the Pub/Sub client, for tests
// that need to observe what a Pub/Sub-target job published.
func pubsubClientAt(t *testing.T, emu *cloudrig.Emulator) *pubsub.Client {
	t.Helper()
	c, err := pubsub.NewClient(context.Background(), "test-project",
		option.WithEndpoint(emu.Endpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("pubsub.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// csREST sends a JSON request to the scheduler REST API — the surface gcloud
// uses; the Go client never touches it.
func csREST(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s: body %q: %v", method, url, raw, err)
		}
	}
	return resp.StatusCode, out
}

// TestSchedulerRESTLifecycle walks what gcloud does: create, list, the :verb
// forms, and delete.
func TestSchedulerRESTLifecycle(t *testing.T) {
	t.Parallel()

	emu := cloudrig.MustStart(t)
	base := emu.BaseURL() + "/v1/projects/p/locations/us-central1/jobs"

	code, job := csREST(t, http.MethodPost, base,
		`{"name":"projects/p/locations/us-central1/jobs/nightly","schedule":"0 9 * * *","httpTarget":{"uri":"http://127.0.0.1:9/","httpMethod":"POST"}}`)
	if code != http.StatusOK || job["name"] != "projects/p/locations/us-central1/jobs/nightly" {
		t.Fatalf("create = %d %v", code, job)
	}

	if code, _ := csREST(t, http.MethodGet, base+"/nightly", ""); code != http.StatusOK {
		t.Errorf("get = %d", code)
	}
	if code, _ := csREST(t, http.MethodPost, base+"/nightly:pause", ""); code != http.StatusOK {
		t.Errorf("pause = %d", code)
	}
	if code, _ := csREST(t, http.MethodPost, base+"/nightly:resume", ""); code != http.StatusOK {
		t.Errorf("resume = %d", code)
	}
	if code, _ := csREST(t, http.MethodPost, base+"/nightly:run", ""); code != http.StatusOK {
		t.Errorf("run = %d", code)
	}
	if code, _ := csREST(t, http.MethodDelete, base+"/nightly", ""); code != http.StatusOK {
		t.Errorf("delete = %d", code)
	}
	if code, _ := csREST(t, http.MethodGet, base+"/nightly", ""); code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", code)
	}
}

// TestSchedulerRESTAndGRPCShareState holds one service behind two APIs.
func TestSchedulerRESTAndGRPCShareState(t *testing.T) {
	c, emu, ctx := csClient(t)

	base := emu.BaseURL() + "/v1/projects/test-project/locations/us-central1/jobs"
	if code, _ := csREST(t, http.MethodPost, base,
		`{"name":"`+csParent+`/jobs/shared","schedule":"0 * * * *","pubsubTarget":{"topicName":"projects/test-project/topics/t"}}`); code != http.StatusOK {
		t.Fatalf("REST create = %d", code)
	}
	if _, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: csParent + "/jobs/shared"}); err != nil {
		t.Fatalf("a job made over REST is invisible to gRPC: %v", err)
	}
}
