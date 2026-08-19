package metrics

import "testing"

func TestClassifyToolError(t *testing.T) {
	cases := map[string]string{
		"tools/call: context deadline exceeded":           "timeout",
		"dial tcp: connection refused":                    "unreachable",
		"initialize: http 403: ":                          "auth",
		`get pod default/x: pods "x" not found`:           "not_found",
		"either 'node' or 'pod' is required":              "bad_args",
		"decode arguments: json: cannot unmarshal string": "bad_args",
		"upstream returned 503":                           "server_error",
		"something entirely unexpected":                   "other",
	}
	for in, want := range cases {
		if got := ClassifyToolError(in); got != want {
			t.Errorf("ClassifyToolError(%q) = %q, want %q", in, got, want)
		}
	}
}
