package cloudlogging

import (
	"strings"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// predicate decides whether an entry matches a filter.
type predicate func(*loggingpb.LogEntry) bool

// severityRank orders severities so >= comparisons work.
var severityRank = map[string]int{
	"DEFAULT": 0, "DEBUG": 100, "INFO": 200, "NOTICE": 300,
	"WARNING": 400, "ERROR": 500, "CRITICAL": 600, "ALERT": 700, "EMERGENCY": 800,
}

// parseFilter understands the common Cloud Logging filter terms a test or
// gcloud sends, joined by implicit AND: logName=..., severity>=ERROR (and the
// other comparators), and resource.type=... A term this does not model is a
// deliberate error rather than a silent match-everything, so a test relying on
// a filter is never misled.
func parseFilter(filter string) (predicate, error) {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return func(*loggingpb.LogEntry) bool { return true }, nil
	}

	var preds []predicate
	for _, term := range splitTerms(filter) {
		p, err := parseTerm(term)
		if err != nil {
			return nil, err
		}
		preds = append(preds, p)
	}
	return func(e *loggingpb.LogEntry) bool {
		for _, p := range preds {
			if !p(e) {
				return false
			}
		}
		return true
	}, nil
}

// splitTerms breaks a filter into terms on whitespace and AND, but never
// inside a double-quoted value — so labels.message="payment failed" stays one
// term rather than splitting on the space in the value.
func splitTerms(filter string) []string {
	var terms []string
	var cur strings.Builder
	inQuote := false

	flush := func() {
		if cur.Len() > 0 {
			terms = append(terms, cur.String())
			cur.Reset()
		}
	}
	for _, r := range filter {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case (r == ' ' || r == '	') && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()

	out := terms[:0]
	for _, t := range terms {
		if t != "AND" {
			out = append(out, t)
		}
	}
	return out
}

func parseTerm(term string) (predicate, error) {
	// severity comparisons: severity>=ERROR, severity=INFO, severity>WARNING
	if strings.HasPrefix(term, "severity") {
		return parseSeverity(term)
	}

	key, op, val, ok := splitKV(term)
	if !ok || op != "=" {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported filter term %q", term)
	}
	val = unquote(val)

	switch key {
	case "logName":
		return func(e *loggingpb.LogEntry) bool { return e.GetLogName() == val }, nil
	case "resource.type":
		return func(e *loggingpb.LogEntry) bool { return e.GetResource().GetType() == val }, nil
	case "insertId":
		return func(e *loggingpb.LogEntry) bool { return e.GetInsertId() == val }, nil
	case "trace":
		return func(e *loggingpb.LogEntry) bool { return e.GetTrace() == val }, nil
	}
	// labels.<name>=value
	if name, found := strings.CutPrefix(key, "labels."); found {
		return func(e *loggingpb.LogEntry) bool { return e.GetLabels()[name] == val }, nil
	}
	return nil, status.Errorf(codes.InvalidArgument, "unsupported filter field %q", key)
}

func parseSeverity(term string) (predicate, error) {
	for _, op := range []string{">=", "<=", ">", "<", "="} {
		if rest, found := strings.CutPrefix(term, "severity"+op); found {
			want, ok := severityRank[strings.ToUpper(unquote(rest))]
			if !ok {
				return nil, status.Errorf(codes.InvalidArgument, "unknown severity %q", rest)
			}
			op := op
			return func(e *loggingpb.LogEntry) bool {
				got := severityRank[e.GetSeverity().String()]
				switch op {
				case ">=":
					return got >= want
				case "<=":
					return got <= want
				case ">":
					return got > want
				case "<":
					return got < want
				default:
					return got == want
				}
			}, nil
		}
	}
	return nil, status.Errorf(codes.InvalidArgument, "unsupported severity term %q", term)
}

// splitKV splits key<op>value on the first operator.
func splitKV(term string) (key, op, val string, ok bool) {
	if i := strings.Index(term, "="); i >= 0 {
		return term[:i], "=", term[i+1:], true
	}
	return "", "", "", false
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
