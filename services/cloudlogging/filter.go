package cloudlogging

import (
	"strings"
	"time"

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

// parseFilter understands the subset of the Cloud Logging query language that
// tests and gcloud send: comparisons on the fields below, joined by AND
// (explicit or implicit), OR, NOT and parentheses. A field this does not model
// is a deliberate error rather than a silent match-everything, so a test
// relying on a filter is never misled.
func parseFilter(filter string) (predicate, error) {
	toks, err := tokenize(filter)
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return func(*loggingpb.LogEntry) bool { return true }, nil
	}
	p := &parser{toks: toks}
	pred, err := p.and()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.toks) {
		return nil, invalid("unexpected %q in filter", p.toks[p.pos].text)
	}
	return pred, nil
}

func invalid(format string, args ...any) error {
	return status.Errorf(codes.InvalidArgument, format, args...)
}

type tokKind int

const (
	tokWord tokKind = iota // a field, keyword or bare value
	tokString
	tokOp
	tokLParen
	tokRParen
)

type token struct {
	kind tokKind
	text string
}

var ops = []string{">=", "<=", "!=", "=", ">", "<", ":"}

// tokenize splits a filter, keeping a double-quoted value whole, so
// labels.message="payment failed" never splits on the space inside the value.
// A quoted segment inside a field, as in labels."goog-managed-by", joins it.
func tokenize(s string) ([]token, error) {
	var toks []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '(':
			toks = append(toks, token{tokLParen, "("})
			i++
		case c == ')':
			toks = append(toks, token{tokRParen, ")"})
			i++
		case c == '"':
			str, n, err := quoted(s[i:])
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{tokString, str})
			i += n
		default:
			if op := opAt(s[i:]); op != "" {
				toks = append(toks, token{tokOp, op})
				i += len(op)
				continue
			}
			var b strings.Builder
			for i < len(s) && !strings.ContainsRune(" \t\n()", rune(s[i])) && opAt(s[i:]) == "" {
				if s[i] == '"' {
					str, n, err := quoted(s[i:])
					if err != nil {
						return nil, err
					}
					b.WriteString(str)
					i += n
					continue
				}
				b.WriteByte(s[i])
				i++
			}
			toks = append(toks, token{tokWord, b.String()})
		}
	}
	return toks, nil
}

func opAt(s string) string {
	for _, op := range ops {
		if strings.HasPrefix(s, op) {
			return op
		}
	}
	return ""
}

// quoted reads a double-quoted string at the start of s, with \" escapes.
func quoted(s string) (string, int, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case '"':
			return b.String(), i + 1, nil
		default:
			b.WriteByte(s[i])
		}
	}
	return "", 0, invalid("unterminated quote in filter")
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() (token, bool) {
	if p.pos >= len(p.toks) {
		return token{}, false
	}
	return p.toks[p.pos], true
}

func (p *parser) keyword(kw string) bool {
	t, ok := p.peek()
	if ok && t.kind == tokWord && t.text == kw {
		p.pos++
		return true
	}
	return false
}

// and joins terms until a closing paren or the end: adjacency is AND. In the
// Logging query language OR binds tighter than AND, so a OR b c is (a OR b) c.
func (p *parser) and() (predicate, error) {
	var preds []predicate
	for {
		t, ok := p.peek()
		if !ok || t.kind == tokRParen {
			break
		}
		p.keyword("AND")
		pred, err := p.or()
		if err != nil {
			return nil, err
		}
		preds = append(preds, pred)
	}
	if len(preds) == 0 {
		return nil, invalid("empty expression in filter")
	}
	return func(e *loggingpb.LogEntry) bool {
		for _, pred := range preds {
			if !pred(e) {
				return false
			}
		}
		return true
	}, nil
}

func (p *parser) or() (predicate, error) {
	left, err := p.unary()
	if err != nil {
		return nil, err
	}
	for p.keyword("OR") {
		right, err := p.unary()
		if err != nil {
			return nil, err
		}
		l := left
		left = func(e *loggingpb.LogEntry) bool { return l(e) || right(e) }
	}
	return left, nil
}

func (p *parser) unary() (predicate, error) {
	if p.keyword("NOT") {
		inner, err := p.unary()
		if err != nil {
			return nil, err
		}
		return func(e *loggingpb.LogEntry) bool { return !inner(e) }, nil
	}
	if t, ok := p.peek(); ok && t.kind == tokWord && strings.HasPrefix(t.text, "-") && len(t.text) > 1 {
		p.toks[p.pos].text = t.text[1:]
		inner, err := p.unary()
		if err != nil {
			return nil, err
		}
		return func(e *loggingpb.LogEntry) bool { return !inner(e) }, nil
	}
	return p.primary()
}

func (p *parser) primary() (predicate, error) {
	t, ok := p.peek()
	if !ok {
		return nil, invalid("filter ends early")
	}
	if t.kind == tokLParen {
		p.pos++
		inner, err := p.and()
		if err != nil {
			return nil, err
		}
		if t, ok := p.peek(); !ok || t.kind != tokRParen {
			return nil, invalid("missing ) in filter")
		}
		p.pos++
		return inner, nil
	}
	if t.kind != tokWord || p.pos+2 >= len(p.toks) {
		return nil, invalid("unsupported filter term %q", t.text)
	}
	op, val := p.toks[p.pos+1], p.toks[p.pos+2]
	if op.kind != tokOp || (val.kind != tokWord && val.kind != tokString) {
		return nil, invalid("unsupported filter term %q", t.text)
	}
	p.pos += 3
	return comparison(t.text, op.text, val.text)
}

func comparison(field, op, val string) (predicate, error) {
	switch field {
	case "severity":
		return severityCmp(op, val)
	case "timestamp", "receiveTimestamp":
		return timeCmp(field, op, val)
	}
	get, ok := stringField(field)
	if !ok {
		return nil, invalid("unsupported filter field %q", field)
	}
	switch op {
	case "=":
		return func(e *loggingpb.LogEntry) bool { return get(e) == val }, nil
	case "!=":
		return func(e *loggingpb.LogEntry) bool { return get(e) != val }, nil
	case ":":
		// "has": a case-insensitive substring match.
		want := strings.ToLower(val)
		return func(e *loggingpb.LogEntry) bool { return strings.Contains(strings.ToLower(get(e)), want) }, nil
	}
	return nil, invalid("unsupported operator %q on %q", op, field)
}

// stringField reads the fields a filter can compare as text.
func stringField(field string) (func(*loggingpb.LogEntry) string, bool) {
	switch field {
	case "logName":
		return (*loggingpb.LogEntry).GetLogName, true
	case "resource.type":
		return func(e *loggingpb.LogEntry) string { return e.GetResource().GetType() }, true
	case "insertId":
		return (*loggingpb.LogEntry).GetInsertId, true
	case "trace":
		return (*loggingpb.LogEntry).GetTrace, true
	case "textPayload":
		return (*loggingpb.LogEntry).GetTextPayload, true
	}
	if name, ok := strings.CutPrefix(field, "resource.labels."); ok {
		return func(e *loggingpb.LogEntry) string { return e.GetResource().GetLabels()[name] }, true
	}
	if name, ok := strings.CutPrefix(field, "labels."); ok {
		return func(e *loggingpb.LogEntry) string { return e.GetLabels()[name] }, true
	}
	return nil, false
}

func severityCmp(op, val string) (predicate, error) {
	want, ok := severityRank[strings.ToUpper(val)]
	if !ok {
		return nil, invalid("unknown severity %q", val)
	}
	cmp, err := compare(op)
	if err != nil {
		return nil, err
	}
	return func(e *loggingpb.LogEntry) bool {
		return cmp(severityRank[e.GetSeverity().String()] - want)
	}, nil
}

func timeCmp(field, op, val string) (predicate, error) {
	want, err := time.Parse(time.RFC3339Nano, val)
	if err != nil {
		return nil, invalid("%s wants an RFC 3339 time, got %q", field, val)
	}
	cmp, err := compare(op)
	if err != nil {
		return nil, err
	}
	return func(e *loggingpb.LogEntry) bool {
		ts := e.GetTimestamp()
		if field == "receiveTimestamp" {
			ts = e.GetReceiveTimestamp()
		}
		return cmp(ts.AsTime().Compare(want))
	}, nil
}

// compare turns an operator into a test on the sign of got-want.
func compare(op string) (func(int) bool, error) {
	switch op {
	case "=", ":":
		return func(d int) bool { return d == 0 }, nil
	case "!=":
		return func(d int) bool { return d != 0 }, nil
	case ">":
		return func(d int) bool { return d > 0 }, nil
	case ">=":
		return func(d int) bool { return d >= 0 }, nil
	case "<":
		return func(d int) bool { return d < 0 }, nil
	case "<=":
		return func(d int) bool { return d <= 0 }, nil
	}
	return nil, invalid("unsupported operator %q", op)
}
