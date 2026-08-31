package storage

import "testing"

func TestParseContentRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		header            string
		start, end, total int64
		query             bool
		wantErr           bool
	}{
		{name: "a chunk with the total known", header: "bytes 0-262143/1048576",
			start: 0, end: 262143, total: 1048576},
		{name: "a chunk with the total unknown", header: "bytes 262144-524287/*",
			start: 262144, end: 524287, total: -1},
		{name: "a status query", header: "bytes */1048576",
			start: 0, end: -1, total: 1048576, query: true},
		{name: "a query with no total", header: "bytes */*",
			start: 0, end: -1, total: -1, query: true},
		{name: "a single byte", header: "bytes 0-0/1", start: 0, end: 0, total: 1},
		// No header at all means the whole object in one request.
		{name: "absent", header: "", start: 0, end: -1, total: -1},

		{name: "no bytes prefix", header: "0-10/20", wantErr: true},
		{name: "no total", header: "bytes 0-10", wantErr: true},
		{name: "a non-numeric start", header: "bytes a-10/20", wantErr: true},
		{name: "a non-numeric end", header: "bytes 0-b/20", wantErr: true},
		{name: "a non-numeric total", header: "bytes 0-10/c", wantErr: true},
		{name: "no range separator", header: "bytes 010/20", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			start, end, total, query, err := parseContentRange(tc.header)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("accepted %q", tc.header)
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected %q: %v", tc.header, err)
			}
			if start != tc.start || end != tc.end || total != tc.total || query != tc.query {
				t.Errorf("got (%d, %d, %d, %v), want (%d, %d, %d, %v)",
					start, end, total, query, tc.start, tc.end, tc.total, tc.query)
			}
		})
	}
}

// TestMultipartBoundary covers the quoting real clients use.
//
// gcloud sends boundary='===============123==' — single-quoted, and containing
// "=", which is not a token character. RFC 2045 recognises neither, so
// mime.ParseMediaType rejects the whole header and every gcloud upload failed
// with 400 until the fallback existed.
func TestMultipartBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contentType string
		want        string
	}{
		{"the Go client, unquoted", `multipart/related; boundary=abc123`, "abc123"},
		{"double quoted", `multipart/related; boundary="abc123"`, "abc123"},
		{"gcloud, single quoted with equals", `multipart/related; boundary='===============5970272403554411136=='`,
			"===============5970272403554411136=="},
		{"a further parameter", `multipart/related; boundary='abc'; charset=utf-8`, "abc"},
		{"spaces around the value", `multipart/related; boundary= abc `, "abc"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := multipartBoundary(tc.contentType)
			if err != nil {
				t.Fatalf("rejected %q: %v", tc.contentType, err)
			}
			if got != tc.want {
				t.Errorf("boundary = %q, want %q", got, tc.want)
			}
		})
	}

	for _, bad := range []string{
		"multipart/related",
		"multipart/related; boundary=",
		"multipart/related; boundary=''",
		"",
	} {
		t.Run("rejects "+bad, func(t *testing.T) {
			t.Parallel()
			if _, err := multipartBoundary(bad); err == nil {
				t.Errorf("accepted %q", bad)
			}
		})
	}
}
