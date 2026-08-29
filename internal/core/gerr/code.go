// Package gerr is cloudrig's canonical error type. Handlers construct errors
// here rather than with errors.New, so that every failure carries a canonical
// code, an explicit HTTP status, and the reason string clients actually branch
// on.
//
// gerr knows nothing about any service. Service packages state their own status
// and reason at each construction site; the canonical table below is a fallback
// for internal and unexpected failures, not the primary path. See PLAN.md.
package gerr

// Code is a canonical error code. Values match google.rpc.Code numerically, so
// that adding a gRPC rendering later is a cast rather than a translation.
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

// codeNames are the google.rpc.Code enum names. They appear verbatim as the
// JSON envelope's "status" field, so the spelling matters — note CANCELLED,
// which is Google's spelling and not Go's.
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

// canonicalHTTP is the gRPC-to-HTTP mapping published for gRPC-first APIs.
//
// It is a fallback, deliberately not the primary path. Google's REST surfaces
// diverge from it constantly — GCS answers a failed precondition with 412 where
// this table says 400, and returns 404 for a missing generation that also fails
// one. A service that leans on this table ships wrong statuses by omission, and
// the symptom is a real client's retry policy misbehaving months later. Service
// handlers call WithHTTPStatus at every user-visible construction site.
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
