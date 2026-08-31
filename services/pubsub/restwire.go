package pubsub

import (
	"io"
	"net/http"
	"strings"

	"github.com/monirz/cloudrig/core/gerr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// decode reads a JSON body into a proto. An empty body is not an error: a
// create carries everything it needs in the path.
func decode(r *http.Request, into proto.Message) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return gerr.New(gerr.InvalidArgument, "reading the request body: "+err.Error()).
			WithHTTPStatus(http.StatusBadRequest)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	// Unknown fields are ignored: a client sending a field this emulator does
	// not model should not be refused outright.
	opts := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := opts.Unmarshal(body, into); err != nil {
		return gerr.New(gerr.InvalidArgument, "malformed JSON body: "+err.Error()).
			WithHTTPStatus(http.StatusBadRequest)
	}
	return nil
}

// maskFrom prefers the updateMask query parameter, which is where the REST
// surface carries it, and falls back to one supplied in the body.
func maskFrom(r *http.Request, fromBody *fieldmaskpb.FieldMask) *fieldmaskpb.FieldMask {
	raw := r.URL.Query().Get("updateMask")
	if raw == "" {
		return fromBody
	}
	return &fieldmaskpb.FieldMask{Paths: strings.Split(raw, ",")}
}

// respond writes a proto as JSON, or renders a gRPC error as the JSON API
// reports it. Delete returns an empty object, as the real API does.
func respond(w http.ResponseWriter, out proto.Message, err error) error {
	if err != nil {
		return asGerr(err)
	}
	if _, ok := out.(*emptypb.Empty); ok {
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		_, _ = w.Write([]byte("{}\n"))
		return nil
	}

	// EmitUnpopulated so a field the client set to its zero value comes back
	// rather than vanishing, which Terraform reads as drift.
	body, marshalErr := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(out)
	if marshalErr != nil {
		return gerr.New(gerr.Internal, "encoding the response: "+marshalErr.Error())
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	_, _ = w.Write(append(body, '\n'))
	return nil
}

// asGerr converts a gRPC status into the error the router renders. The codes
// share their numbering with google.rpc.Code, so the mapping is the identity.
func asGerr(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return gerr.New(gerr.Internal, err.Error())
	}
	return gerr.New(gerr.Code(st.Code()), st.Message()).
		WithHTTPStatus(httpStatusOf(st.Code()))
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
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.Unimplemented:
		return http.StatusNotImplemented
	}
	return http.StatusInternalServerError
}
