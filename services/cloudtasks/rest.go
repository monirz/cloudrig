package cloudtasks

import (
	"io"
	"net/http"
	"strings"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"github.com/monirz/cloudrig/core/gerr"
	"github.com/monirz/cloudrig/transport"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// MaxBodyBytes caps a JSON request body. A task payload is small; an unbounded
// read is a way to exhaust the process, and the port is shared.
const MaxBodyBytes = 4 << 20

// REST serves the Cloud Tasks JSON API so `gcloud tasks` works. The Go client
// speaks gRPC; gcloud speaks REST, over the same service.
type REST struct {
	router *transport.Router
	svc    *Service
}

// NewREST wires the routes gcloud drives.
func NewREST(s *Service) *REST {
	a := &REST{router: transport.NewRouter(), svc: s}

	const queues = "/v2/projects/{project}/locations/{location}/queues"
	a.router.Handle(http.MethodGet, queues, a.listQueues)
	a.router.Handle(http.MethodPost, queues, a.createQueue)
	a.router.Handle(http.MethodGet, queues+"/{queue}", a.getQueue)
	a.router.Handle(http.MethodPost, queues+"/{queue}", a.queueVerb)
	a.router.Handle(http.MethodDelete, queues+"/{queue}", a.deleteQueue)

	const tasks = queues + "/{queue}/tasks"
	a.router.Handle(http.MethodGet, tasks, a.listTasks)
	a.router.Handle(http.MethodPost, tasks, a.createTask)
	a.router.Handle(http.MethodGet, tasks+"/{task}", a.getTask)
	a.router.Handle(http.MethodPost, tasks+"/{task}", a.taskVerb)
	a.router.Handle(http.MethodDelete, tasks+"/{task}", a.deleteTask)
	return a
}

// Matches reports whether a route here claims the request; /v2/ is shared with
// Cloud Functions.
func (a *REST) Matches(method, escapedPath string) bool {
	return a.router.Matches(method, escapedPath)
}

func (a *REST) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.router.ServeHTTP(w, r) }

func queueName(p transport.Params) string {
	return "projects/" + p["project"] + "/locations/" + p["location"] + "/queues/" + p["queue"]
}

func (a *REST) createQueue(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	var q cloudtaskspb.Queue
	if err := decode(r, &q); err != nil {
		return err
	}
	// gcloud sends the id as ?queueId= or names it in the body.
	if id := r.URL.Query().Get("queueId"); id != "" {
		q.Name = "projects/" + p["project"] + "/locations/" + p["location"] + "/queues/" + id
	}
	out, err := a.svc.CreateQueue(r.Context(), &cloudtaskspb.CreateQueueRequest{
		Parent: "projects/" + p["project"] + "/locations/" + p["location"],
		Queue:  &q,
	})
	return respond(w, out, err)
}

func (a *REST) getQueue(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.svc.GetQueue(r.Context(), &cloudtaskspb.GetQueueRequest{Name: queueName(p)})
	return respond(w, out, err)
}

func (a *REST) listQueues(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.svc.ListQueues(r.Context(), &cloudtaskspb.ListQueuesRequest{
		Parent: "projects/" + p["project"] + "/locations/" + p["location"],
	})
	return respond(w, out, err)
}

func (a *REST) deleteQueue(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.svc.DeleteQueue(r.Context(), &cloudtaskspb.DeleteQueueRequest{Name: queueName(p)})
	return respond(w, out, err)
}

// queueVerb answers :pause, :resume and :purge, which ride on the name segment.
func (a *REST) queueVerb(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	name, verb, ok := strings.Cut(p["queue"], ":")
	if !ok {
		return unsupportedVerb(p["queue"], "")
	}
	full := "projects/" + p["project"] + "/locations/" + p["location"] + "/queues/" + name

	ctx := r.Context()
	switch verb {
	case "pause":
		out, err := a.svc.PauseQueue(ctx, &cloudtaskspb.PauseQueueRequest{Name: full})
		return respond(w, out, err)
	case "resume":
		out, err := a.svc.ResumeQueue(ctx, &cloudtaskspb.ResumeQueueRequest{Name: full})
		return respond(w, out, err)
	case "purge":
		out, err := a.svc.PurgeQueue(ctx, &cloudtaskspb.PurgeQueueRequest{Name: full})
		return respond(w, out, err)
	}
	return unsupportedVerb(full, verb)
}

func taskParent(p transport.Params) string {
	return queueName(p)
}

func (a *REST) createTask(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	var req cloudtaskspb.CreateTaskRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	req.Parent = taskParent(p)
	out, err := a.svc.CreateTask(r.Context(), &req)
	return respond(w, out, err)
}

func (a *REST) getTask(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	name, verb, _ := strings.Cut(p["task"], ":")
	full := taskParent(p) + "/tasks/" + name
	if verb == "run" {
		out, err := a.svc.RunTask(r.Context(), &cloudtaskspb.RunTaskRequest{Name: full})
		return respond(w, out, err)
	}
	if verb != "" {
		return unsupportedVerb(full, verb)
	}
	out, err := a.svc.GetTask(r.Context(), &cloudtaskspb.GetTaskRequest{Name: full})
	return respond(w, out, err)
}

// taskVerb answers :run, posted to a task.
func (a *REST) taskVerb(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	name, verb, ok := strings.Cut(p["task"], ":")
	full := taskParent(p) + "/tasks/" + name
	if !ok || verb != "run" {
		return unsupportedVerb(full, verb)
	}
	out, err := a.svc.RunTask(r.Context(), &cloudtaskspb.RunTaskRequest{Name: full})
	return respond(w, out, err)
}

func (a *REST) listTasks(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	out, err := a.svc.ListTasks(r.Context(), &cloudtaskspb.ListTasksRequest{Parent: taskParent(p)})
	return respond(w, out, err)
}

func (a *REST) deleteTask(w http.ResponseWriter, r *http.Request, p transport.Params) error {
	name, _, _ := strings.Cut(p["task"], ":")
	out, err := a.svc.DeleteTask(r.Context(), &cloudtaskspb.DeleteTaskRequest{
		Name: taskParent(p) + "/tasks/" + name,
	})
	return respond(w, out, err)
}

func unsupportedVerb(name, verb string) error {
	return gerr.New(gerr.Unimplemented, "unsupported verb "+verb+" on "+name).
		WithHTTPStatus(http.StatusNotImplemented)
}

func decode(r *http.Request, into proto.Message) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		return gerr.New(gerr.InvalidArgument, "reading the request body: "+err.Error()).
			WithHTTPStatus(http.StatusBadRequest)
	}
	if len(body) > MaxBodyBytes {
		return gerr.New(gerr.InvalidArgument, "the request body is too large").
			WithHTTPStatus(http.StatusRequestEntityTooLarge)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := unmarshal.Unmarshal(body, into); err != nil {
		return gerr.New(gerr.InvalidArgument, "malformed JSON body: "+err.Error()).
			WithHTTPStatus(http.StatusBadRequest)
	}
	return nil
}

func respond(w http.ResponseWriter, out proto.Message, err error) error {
	if err != nil {
		st, _ := status.FromError(err)
		return gerr.New(gerr.Code(st.Code()), st.Message()).WithHTTPStatus(httpStatusOf(st.Code()))
	}
	body, marshalErr := marshal.Marshal(out)
	if marshalErr != nil {
		return gerr.New(gerr.Internal, "encoding the response: "+marshalErr.Error())
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	if len(body) == 0 {
		body = []byte("{}")
	}
	_, _ = w.Write(append(body, '\n'))
	return nil
}

func httpStatusOf(c codes.Code) int {
	switch c {
	case codes.OK:
		return http.StatusOK
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return http.StatusBadRequest
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists, codes.Aborted:
		return http.StatusConflict
	case codes.Unimplemented:
		return http.StatusNotImplemented
	}
	return http.StatusInternalServerError
}
