package gerr

// Detail is a google.rpc error detail payload. These mirror the proto
// containers so swapping in the generated types later is mechanical.
type Detail interface{ isDetail() }

// ErrorInfo is google.rpc.ErrorInfo: the machine-readable reason.
type ErrorInfo struct {
	// Reason is UPPER_SNAKE here, lowerCamel'd when rendered.
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

// PreconditionFailure is google.rpc.PreconditionFailure: well-formed request,
// state did not permit it.
type PreconditionFailure struct {
	Violations []PreconditionViolation
}

func (PreconditionFailure) isDetail() {}

// PreconditionViolation names one unmet precondition.
type PreconditionViolation struct {
	Type        string
	Subject     string
	Description string
}
