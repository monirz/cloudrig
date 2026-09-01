package functions_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/monirz/cloudrig/core/clock"
	"github.com/monirz/cloudrig/core/events"
	"github.com/monirz/cloudrig/functions"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newRegistry(t *testing.T) *functions.Registry {
	t.Helper()
	r := functions.NewRegistry(clock.NewFake(epoch), nil, functions.Options{})
	t.Cleanup(r.StopAll)
	return r
}

func goFn(name string) functions.Function {
	return functions.Function{Name: name, Source: helloDir, EntryPoint: "HelloHTTP"}
}

func TestRegistryDeployAndLookup(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	r := newRegistry(t)
	if _, ok := r.Handler("", "", "hello"); ok {
		t.Fatal("empty registry resolved a name")
	}

	desc, err := r.Deploy(context.Background(), goFn("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if desc.Runtime != functions.RuntimeGo || desc.State != "ACTIVE" {
		t.Errorf("descriptor = %+v", desc)
	}
	// UpdateTime comes from the injected clock, so it is the fake epoch.
	if !desc.UpdateTime.Equal(epoch) {
		t.Errorf("UpdateTime = %v, want the fake epoch %v", desc.UpdateTime, epoch)
	}
	if _, ok := r.Handler("", "", "hello"); !ok {
		t.Error("deployed function is not resolvable")
	}
	if got, ok := r.Get("", "", "hello"); !ok || got.Name != "hello" {
		t.Errorf("Get = %+v, %v", got, ok)
	}
}

func TestRegistryRedeployReplaces(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	r := newRegistry(t)
	ctx := context.Background()
	if _, err := r.Deploy(ctx, goFn("hello")); err != nil {
		t.Fatal(err)
	}
	first, _ := r.Handler("", "", "hello")

	if _, err := r.Deploy(ctx, goFn("hello")); err != nil {
		t.Fatal(err)
	}
	second, _ := r.Handler("", "", "hello")

	if first == second {
		t.Error("redeploy reused the old instance")
	}
	if got := r.List("", ""); len(got) != 1 {
		t.Errorf("List has %d entries after a redeploy, want 1", len(got))
	}
}

// TestRegistryFailedDeployKeepsTheOldOne is the reason Deploy starts the
// replacement before stopping the incumbent: a broken deploy must not take the
// name down.
func TestRegistryFailedDeployKeepsTheOldOne(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	r := newRegistry(t)
	ctx := context.Background()
	if _, err := r.Deploy(ctx, goFn("hello")); err != nil {
		t.Fatal(err)
	}

	bad := goFn("hello")
	bad.EntryPoint = "NoSuchHandler"
	if _, err := r.Deploy(ctx, bad); err == nil {
		t.Fatal("a broken deploy succeeded")
	}

	h, ok := r.Handler("", "", "hello")
	if !ok {
		t.Fatal("the failed deploy took the function down")
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	if body := get(t, srv.URL+"/?name=x"); !strings.Contains(body, "hello, x") {
		t.Errorf("body = %s", body)
	}
}

func TestRegistryListAndDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	r := newRegistry(t)
	ctx := context.Background()
	for _, name := range []string{"b", "a"} {
		if _, err := r.Deploy(ctx, goFn(name)); err != nil {
			t.Fatal(err)
		}
	}

	got := r.List("", "")
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("List = %+v, want a then b", got)
	}

	if err := r.Delete("", "", "a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Handler("", "", "a"); ok {
		t.Error("deleted function is still resolvable")
	}
	if err := r.Delete("", "", "a"); err == nil {
		t.Error("deleting a missing function succeeded")
	}
}

func TestAdminAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	r := newRegistry(t)
	srv := httptest.NewServer(r.Admin())
	t.Cleanup(srv.Close)

	deploy := func(t *testing.T, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+functions.AdminPath, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	t.Run("deploy", func(t *testing.T) {
		resp := deploy(t, `{"name":"hello","source":"`+helloDir+`","entryPoint":"HelloHTTP"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var desc functions.Descriptor
		if err := json.NewDecoder(resp.Body).Decode(&desc); err != nil {
			t.Fatal(err)
		}
		if desc.Name != "hello" || desc.Runtime != functions.RuntimeGo {
			t.Errorf("descriptor = %+v", desc)
		}
	})

	t.Run("list", func(t *testing.T) {
		var body struct {
			Functions []functions.Descriptor `json:"functions"`
		}
		decode(t, srv.URL+functions.AdminPath, &body)
		if len(body.Functions) != 1 {
			t.Errorf("functions = %+v, want 1", body.Functions)
		}
	})

	t.Run("describe", func(t *testing.T) {
		var desc functions.Descriptor
		decode(t, srv.URL+functions.AdminPath+"/hello", &desc)
		if desc.EntryPoint != "HelloHTTP" {
			t.Errorf("descriptor = %+v", desc)
		}
	})

	t.Run("describe a missing function is 404", func(t *testing.T) {
		resp, err := http.Get(srv.URL + functions.AdminPath + "/nope")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("a broken deploy is 400 with the reason", func(t *testing.T) {
		resp := deploy(t, `{"name":"bad","source":"`+helloDir+`","entryPoint":"NoSuchHandler"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		var env struct {
			Error struct{ Message string } `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(env.Error.Message, "NoSuchHandler") {
			t.Errorf("message = %q, want it to name the entry point", env.Error.Message)
		}
	})

	t.Run("malformed body is 400", func(t *testing.T) {
		resp := deploy(t, `{not json`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("delete", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodDelete, srv.URL+functions.AdminPath+"/hello", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("status = %d, want 204", resp.StatusCode)
		}
	})

	t.Run("unsupported method is 405", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+functions.AdminPath, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", resp.StatusCode)
		}
	})
}

// TestStartupFailureExplainsItself covers the tail buffer: a child that dies
// before listening has usually already said why.
func TestStartupFailureExplainsItself(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a node subprocess")
	}
	if !exists(nodeDir + "/node_modules/.bin/functions-framework") {
		t.Skipf("run: (cd %s && npm i)", nodeDir)
	}
	t.Parallel()

	_, err := functions.Start(context.Background(), functions.Function{
		Name: "hello", Source: nodeDir, Runtime: functions.RuntimeNode20, EntryPoint: "nosuchhandler",
	}, functions.Options{})
	if err == nil {
		t.Fatal("a missing node entry point started successfully")
	}
	if !strings.Contains(err.Error(), "nosuchhandler") {
		t.Errorf("err = %q, want the child's own explanation", err)
	}
}

func decode(t *testing.T, url string, into any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatal(err)
	}
}

// TestDescriptorReportsResolvedValues guards a bug where a fully auto-detected
// deploy reported an empty entry point: the descriptor was built from the
// request rather than from what the resolved function actually runs.
func TestDescriptorReportsResolvedValues(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/solo\n\ngo 1.25\n")
	write(t, dir, "fn.go", `package solo

import "net/http"

func Handler(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }
`)

	r := newRegistry(t)
	// Neither runtime nor entry point given: both must come back resolved.
	desc, err := r.Deploy(context.Background(), functions.Function{Name: "solo", Source: dir})
	if err != nil {
		t.Fatal(err)
	}
	if desc.Runtime != functions.RuntimeGo {
		t.Errorf("Runtime = %q, want go", desc.Runtime)
	}
	if desc.EntryPoint != "Handler" {
		t.Errorf("EntryPoint = %q, want Handler", desc.EntryPoint)
	}
}

func TestLogsEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	r := newRegistry(t)
	if _, err := r.Deploy(context.Background(), goFn("hello")); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(r.Admin())
	t.Cleanup(srv.Close)

	t.Run("a snapshot returns plain text", func(t *testing.T) {
		resp, err := http.Get(srv.URL + functions.AdminPath + "/hello/logs")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("Content-Type = %q", ct)
		}
	})

	t.Run("logs for a missing function are 404", func(t *testing.T) {
		resp, err := http.Get(srv.URL + functions.AdminPath + "/nope/logs")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})

	t.Run("an unknown sub-resource is 404", func(t *testing.T) {
		resp, err := http.Get(srv.URL + functions.AdminPath + "/hello/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})
}

// TestLogsFollowStreams proves a follower sees output as the function produces
// it, rather than when a buffer happens to fill.
func TestLogsFollowStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/chatty\n\ngo 1.25\n")
	write(t, dir, "fn.go", `package chatty

import (
	"fmt"
	"net/http"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	fmt.Printf("handled %s\n", r.URL.Query().Get("n"))
	w.Write([]byte("ok"))
}
`)

	r := newRegistry(t)
	if _, err := r.Deploy(context.Background(), functions.Function{Name: "chatty", Source: dir}); err != nil {
		t.Fatal(err)
	}
	inst, _ := r.Instance("", "", "chatty")

	srv := httptest.NewServer(r.Admin())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + functions.AdminPath + "/chatty/logs?follow=true")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })

	// Drive the function; its output must arrive on the open stream.
	fnSrv := httptest.NewServer(inst)
	t.Cleanup(fnSrv.Close)
	if _, err := http.Get(fnSrv.URL + "/?n=first"); err != nil {
		t.Fatal(err)
	}

	lines := bufio.NewScanner(resp.Body)
	deadline := make(chan struct{})
	go func() {
		defer close(deadline)
		for lines.Scan() {
			if strings.Contains(lines.Text(), "handled first") {
				return
			}
		}
	}()

	select {
	case <-deadline:
	case <-time.After(10 * time.Second):
		t.Fatal("the followed stream never carried the function's output")
	}
}

// TestWatchRedeploysOnChange drives the whole loop with a fake clock: edit the
// source, advance time, and the running function is the new one.
func TestWatchRedeploysOnChange(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/watched\n\ngo 1.25\n")
	source := func(reply string) string {
		return `package watched

import "net/http"

func Handler(w http.ResponseWriter, r *http.Request) { w.Write([]byte("` + reply + `")) }
`
	}
	write(t, dir, "fn.go", source("first"))

	clk := clock.NewFake(epoch)
	var events strings.Builder
	r := functions.NewRegistry(clk, nil, functions.Options{EventLog: &events})
	t.Cleanup(r.StopAll)

	if _, err := r.Deploy(context.Background(), functions.Function{
		Name: "watched", Source: dir, Watch: true,
	}); err != nil {
		t.Fatal(err)
	}

	body := func(t *testing.T) string {
		t.Helper()
		h, ok := r.Handler("", "", "watched")
		if !ok {
			t.Fatal("function is not deployed")
		}
		srv := httptest.NewServer(h)
		defer srv.Close()
		return get(t, srv.URL+"/")
	}

	if got := body(t); got != "first" {
		t.Fatalf("body = %q, want first", got)
	}

	t.Run("an edit is picked up", func(t *testing.T) {
		write(t, dir, "fn.go", source("second"))
		clk.Advance(functions.WatchInterval)

		if got := body(t); got != "second" {
			t.Errorf("body = %q, want second", got)
		}
		if !strings.Contains(events.String(), "redeployed") {
			t.Errorf("events = %q, want a redeploy reported", events.String())
		}
	})

	t.Run("a broken edit leaves the old one serving", func(t *testing.T) {
		write(t, dir, "fn.go", "package watched\nthis does not compile\n")
		clk.Advance(functions.WatchInterval)

		if got := body(t); got != "second" {
			t.Errorf("body = %q, want the previous version still serving", got)
		}
		if !strings.Contains(events.String(), "watch: watched:") {
			t.Errorf("events = %q, want the failure reported", events.String())
		}
	})

	t.Run("fixing it recovers", func(t *testing.T) {
		write(t, dir, "fn.go", source("third"))
		clk.Advance(functions.WatchInterval)

		if got := body(t); got != "third" {
			t.Errorf("body = %q, want third", got)
		}
	})

	t.Run("deleting stops the watcher", func(t *testing.T) {
		if err := r.Delete("", "", "watched"); err != nil {
			t.Fatal(err)
		}
		if clk.Pending() != 0 {
			t.Errorf("pending timers = %d after delete, want 0", clk.Pending())
		}
	})
}

// TestRedeployDoesNotLeaveTwoWatchers guards a leak: each Deploy starts a
// watcher, so replacing a watched function must stop the incumbent's.
func TestRedeployDoesNotLeaveTwoWatchers(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	clk := clock.NewFake(epoch)
	r := functions.NewRegistry(clk, nil, functions.Options{})
	t.Cleanup(r.StopAll)

	f := goFn("hello")
	f.Watch = true
	for i := 0; i < 3; i++ {
		if _, err := r.Deploy(context.Background(), f); err != nil {
			t.Fatal(err)
		}
	}
	if got := clk.Pending(); got != 1 {
		t.Errorf("pending timers = %d after three deploys, want 1", got)
	}
}

// triggerFn writes a Go function that records every delivery and can be made
// to fail, so a test can watch the retry.
func triggerFn(t *testing.T, failTimes int) string {
	t.Helper()

	dir := t.TempDir()
	write(t, dir, "go.mod", "module example.com/trig\n\ngo 1.25\n")
	write(t, dir, "fn.go", fmt.Sprintf(`package trig

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

var seen atomic.Int32

func Handler(w http.ResponseWriter, r *http.Request) {
	n := seen.Add(1)
	fmt.Printf("DELIVERY %%d\n", n)
	if int(n) <= %d {
		http.Error(w, "not yet", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
`, failTimes))
	return dir
}

// TestTriggerFires is the demo: an event published on the bus reaches a
// function that subscribed to it.
func TestTriggerFires(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	bus := events.New()
	clk := clock.NewFake(epoch)
	r := functions.NewRegistry(clk, bus, functions.Options{})
	t.Cleanup(r.StopAll)

	if _, err := r.Deploy(context.Background(), functions.Function{
		Name:   "on-upload",
		Source: triggerFn(t, 0),
		Trigger: functions.EventTrigger{
			EventType: "google.storage.object.finalize",
			Resource:  "uploads",
		},
	}); err != nil {
		t.Fatal(err)
	}
	inst, _ := r.Instance("", "", "on-upload")

	bus.Publish(context.Background(), events.Event{
		Type:     "google.storage.object.finalize",
		Source:   "//storage.googleapis.com/projects/_/buckets/uploads",
		Resource: "projects/_/buckets/uploads/objects/x",
		Service:  "storage.googleapis.com",
		Time:     epoch,
		Data:     map[string]any{"name": "x"},
	})
	bus.Sync()

	got := awaitLog(t, inst, func(log string) bool { return strings.Contains(log, "DELIVERY 1") })
	if !strings.Contains(got, "DELIVERY 1") {
		t.Errorf("the function was not delivered to; its log:\n%s", got)
	}
}

// TestTriggerIgnoresOtherBuckets holds the other half: a trigger scoped to one
// bucket must not run for another.
func TestTriggerIgnoresOtherBuckets(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	bus := events.New()
	r := functions.NewRegistry(clock.NewFake(epoch), bus, functions.Options{})
	t.Cleanup(r.StopAll)

	if _, err := r.Deploy(context.Background(), functions.Function{
		Name:   "scoped",
		Source: triggerFn(t, 0),
		Trigger: functions.EventTrigger{
			EventType: "google.storage.object.finalize",
			Resource:  "wanted",
		},
	}); err != nil {
		t.Fatal(err)
	}
	inst, _ := r.Instance("", "", "scoped")

	bus.Publish(context.Background(), events.Event{
		Type:   "google.storage.object.finalize",
		Source: "//storage.googleapis.com/projects/_/buckets/unwanted",
		Time:   epoch,
	})
	bus.Sync()

	if got := strings.Join(inst.LogSnapshot(), "\n"); strings.Contains(got, "DELIVERY") {
		t.Errorf("a trigger scoped to one bucket ran for another:\n%s", got)
	}
}

// TestTriggerRetries watches the backoff. The wait is on the injected clock, so
// the test advances time rather than waiting.
func TestTriggerRetries(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	bus := events.New()
	clk := clock.NewFake(epoch)
	var log strings.Builder
	r := functions.NewRegistry(clk, bus, functions.Options{EventLog: &log})
	t.Cleanup(r.StopAll)

	// Fails twice, then succeeds.
	if _, err := r.Deploy(context.Background(), functions.Function{
		Name:    "flaky",
		Source:  triggerFn(t, 2),
		Trigger: functions.EventTrigger{EventType: "e"},
	}); err != nil {
		t.Fatal(err)
	}
	inst, _ := r.Instance("", "", "flaky")

	bus.Publish(context.Background(), events.Event{Type: "e", Time: epoch})
	drain(t, bus, clk)

	logged := awaitLog(t, inst, func(log string) bool {
		return strings.Count(log, "DELIVERY") >= 3
	})
	if deliveries := strings.Count(logged, "DELIVERY"); deliveries != 3 {
		t.Errorf("the function saw %d deliveries, want 3 (two failures then success)", deliveries)
	}
	if !strings.Contains(log.String(), "delivered on attempt 3") {
		t.Errorf("the emulator log does not report the recovery:\n%s", log.String())
	}
}

func TestTriggerGivesUp(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a function")
	}
	t.Parallel()

	bus := events.New()
	clk := clock.NewFake(epoch)
	var log strings.Builder
	r := functions.NewRegistry(clk, bus, functions.Options{EventLog: &log})
	t.Cleanup(r.StopAll)

	if _, err := r.Deploy(context.Background(), functions.Function{
		Name:    "broken",
		Source:  triggerFn(t, 99),
		Trigger: functions.EventTrigger{EventType: "e"},
	}); err != nil {
		t.Fatal(err)
	}
	inst, _ := r.Instance("", "", "broken")

	bus.Publish(context.Background(), events.Event{Type: "e", Time: epoch})
	drain(t, bus, clk)

	// Bounded: an event that never succeeds must not be retried forever.
	logged := awaitLog(t, inst, func(log string) bool {
		return strings.Count(log, "DELIVERY") >= functions.MaxDeliveryAttempts
	})
	if got := strings.Count(logged, "DELIVERY"); got != functions.MaxDeliveryAttempts {
		t.Errorf("the function saw %d deliveries, want %d", got, functions.MaxDeliveryAttempts)
	}
	if !strings.Contains(log.String(), "giving up") {
		t.Errorf("the emulator log does not report giving up:\n%s", log.String())
	}
}

// drain advances the fake clock until every published event has settled.
// Delivery waits on the clock, so nothing progresses unless time does.
func drain(t *testing.T, bus *events.Bus, clk *clock.FakeClock) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		bus.Sync()
		close(done)
	}()

	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-done:
			return
		case <-deadline:
			t.Fatal("events never settled")
		default:
			clk.Advance(functions.RetryBackoff * 64)
			time.Sleep(time.Millisecond)
		}
	}
}

// awaitLog returns a function's log once want holds, or after a deadline.
//
// Delivery being finished does not mean the function's output has arrived:
// a function is a child process, its stdout is a pipe, and the goroutine
// draining that pipe runs after the handler has answered. On an idle machine
// the gap is invisible; under load it is wide enough to read an empty log,
// which is how this failed in CI and never here.
//
// It returns whatever it has on timeout, so a real failure still prints the
// log rather than a bare deadline.
func awaitLog(t *testing.T, inst *functions.Instance, want func(string) bool) string {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		logged := strings.Join(inst.LogSnapshot(), "\n")
		if want(logged) || time.Now().After(deadline) {
			return logged
		}
		time.Sleep(5 * time.Millisecond)
	}
}
