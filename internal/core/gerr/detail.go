package gerr

// Detail is a google.rpc error detail payload.
//
// These mirror the proto containers rather than flattening them — BadRequest
// holds field violations, it is not itself one — so that swapping in the real
// generated types later is mechanical. Step 1 ships no gRPC dependency: an
// unused grpc.NewServer is not a seam, it is a dependency with no test.
type Detail interface{ isDetail() }

// ErrorInfo is google.rpc.ErrorInfo: the machine-readable reason for a failure.
type ErrorInfo struct {
	// Reason is UPPER_SNAKE_CASE here, matching the proto convention, and is
	// lowerCamel'd when rendered into the JSON envelope.
	Reason   string
	Domain   string
	Metadata map[string]string
}

func (ErrorInfo) isDetail() {}

// BadRequest is google.rpc.BadRequest: the request itself was malformed.
type BadRequest struct {
	FieldViolations []FieldViolation
}

func (BadRequest) isDetail() {}

// FieldViolation names one bad field and why.
type FieldViolation struct {
	Field       string
	Description string
}

// PreconditionFailure is google.rpc.PreconditionFailure: the request was
// well-formed but the system state did not permit it.
type PreconditionFailure struct {
	Violations []PreconditionViolation
}

func (PreconditionFailure) isDetail() {}

// PreconditionViolation names one unmet precondition. Type is the kind of
// precondition, Subject the resource it applied to.
type PreconditionViolation struct {
	Type        string
	Subject     string
	Description string
}
