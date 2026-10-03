package integrationtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/getarcaneapp/arcane/cli/v2/internal/config"
)

func TestContainersListJSONContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/environments/0/containers") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"success":false,"error":"not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"success": true,
			"data": [
				{"id":"abc123","names":["/nginx"],"image":"nginx:latest","state":"running","status":"Up 1 hour"}
			],
			"pagination": {"totalPages":1,"totalItems":1,"currentPage":1,"itemsPerPage":20}
		}`))
	}))
	defer srv.Close()

	configPath := writeCLIIntegrationConfigInternal(t, srv.URL)

	outBuf, errOut, err := executeCLIIntegrationCommandInternal(
		t,
		[]string{"--config", configPath, "--log-level", "debug", "containers", "list", "--json"},
	)

	require.NoError(t, err,
		"execute: %v (%s)", err, errOut)

	require.Contains(t, errOut, "Sending request")
	require.NotContains(t, outBuf, "Sending request")
	require.NotContains(t, outBuf, "\x1b")
	var got map[string]any
	{
		unmarshalErr := json.Unmarshal([]byte(strings.TrimSpace(outBuf)), &got)
		require.NoError(t, unmarshalErr,
			"json parse failed: %v\noutput=%s", unmarshalErr, outBuf)
	}

	for _, key := range []string{"success", "data", "pagination"} {
		{
			_, ok := got[key]
			require.True(t, ok,
				"missing key %q in output: %v", key, got)
		}
	}
}

func TestVersionOutput(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		json        bool
		unavailable bool
	}{
		{name: "both versions", status: http.StatusOK, body: `{"currentVersion":"1.2.3","displayVersion":"v1.2.3","revision":"server-revision"}`},
		{name: "server JSON", status: http.StatusOK, body: `{"currentVersion":"1.2.3","displayVersion":"v1.2.3","revision":"server-revision"}`, json: true},
		{name: "unavailable text", status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`, unavailable: true},
		{name: "unavailable JSON", status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`, json: true, unavailable: true},
		{name: "invalid server response", status: http.StatusOK, body: `invalid JSON`, unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/app-version" {
					t.Errorf("unexpected version endpoint: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			args := []string{"--config", writeCLIIntegrationConfigInternal(t, srv.URL), "--no-color", "version"}
			if tc.json {
				args = append(args, "--json")
			}
			out, diagnostics, err := executeCLIIntegrationCommandInternal(t, args)
			if tc.json && tc.unavailable {
				require.Error(t, err)
				require.Empty(t, out)
				return
			}
			require.NoError(t, err)
			if tc.json {
				var got map[string]any
				require.NoError(t, json.Unmarshal([]byte(out), &got))
				require.Equal(t, "v1.2.3", got["displayVersion"])
				require.Equal(t, "server-revision", got["revision"])
				require.NotContains(t, out, "Arcane CLI")
				return
			}
			require.Contains(t, out, "Arcane CLI")
			require.Contains(t, out, config.Version)
			require.Contains(t, out, config.Revision)
			require.Contains(t, out, "Arcane Server")
			if tc.unavailable {
				require.Contains(t, out, "Unavailable")
				require.Contains(t, diagnostics, "Could not get server version")
			} else {
				require.Contains(t, out, "v1.2.3")
				require.Contains(t, out, "server-revision")
			}
		})
	}
}
