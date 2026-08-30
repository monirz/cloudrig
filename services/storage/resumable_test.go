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
