// Package gerr is cloudrig's canonical error type: a code, an explicit HTTP
// status, and the reason string clients branch on. It knows of no service.
package gerr

// Code is a canonical error code. Values match google.rpc.Code numerically, so
// a gRPC rendering is later a cast rather than a translation.
type Code int32

const (
	OK                 Code = 0
	Canceled           Code = 1
	Unknown            Code = 2
	InvalidArgument    Code = 3
	DeadlineExceeded   Code = 4
	NotFound           Code = 5
	AlreadyExists      Code = 6
	PermissionDenied   Code = 7
	ResourceExhausted  Code = 8
	FailedPrecondition Code = 9
	Aborted            Code = 10
	OutOfRange         Code = 11
	Unimplemented      Code = 12
	Internal           Code = 13
	Unavailable        Code = 14
	DataLoss           Code = 15
	Unauthenticated    Code = 16
)

// codeNames appear verbatim as the envelope's "status", so spelling matters —
// note CANCELLED, Google's spelling and not Go's.
var codeNames = map[Code]string{
	OK:                 "OK",
	Canceled:           "CANCELLED",
	Unknown:            "UNKNOWN",
	InvalidArgument:    "INVALID_ARGUMENT",
	DeadlineExceeded:   "DEADLINE_EXCEEDED",
	NotFound:           "NOT_FOUND",
	AlreadyExists:      "ALREADY_EXISTS",
	PermissionDenied:   "PERMISSION_DENIED",
	ResourceExhausted:  "RESOURCE_EXHAUSTED",
	FailedPrecondition: "FAILED_PRECONDITION",
	Aborted:            "ABORTED",
	OutOfRange:         "OUT_OF_RANGE",
	Unimplemented:      "UNIMPLEMENTED",
	Internal:           "INTERNAL",
	Unavailable:        "UNAVAILABLE",
	DataLoss:           "DATA_LOSS",
	Unauthenticated:    "UNAUTHENTICATED",
}

func (c Code) String() string {
	if n, ok := codeNames[c]; ok {
		return n
	}
	return "UNKNOWN"
}

// canonicalHTTP is a fallback, not the primary path: Google's REST surfaces
// diverge from it (GCS answers 412 where this says 400). See PLAN.md D-B.
var canonicalHTTP = map[Code]int{
	OK:                 200,
	Canceled:           499,
	Unknown:            500,
	InvalidArgument:    400,
	DeadlineExceeded:   504,
	NotFound:           404,
	AlreadyExists:      409,
	PermissionDenied:   403,
	ResourceExhausted:  429,
	FailedPrecondition: 400,
	Aborted:            409,
	OutOfRange:         400,
	Unimplemented:      501,
	Internal:           500,
	Unavailable:        503,
	DataLoss:           500,
	Unauthenticated:    401,
}
