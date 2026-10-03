package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestJSONV2SerializerUsesV2ResponseSemantics(t *testing.T) {
	type response struct {
		Items []string `json:"items"`
		Count int      `json:"count,omitempty"`
	}

	e := echo.New()
	recorder := httptest.NewRecorder()
	context := e.NewContext(httptest.NewRequest("GET", "/", http.NoBody), recorder)

	err := (jsonV2Serializer{}).Serialize(context, response{}, "")

	require.NoError(t, err,
		"serialize response: %v", err)
	{

		got, want := recorder.Body.String(), `{"items":[],"count":0}`
		require.Equal(t, want, got,
			"serialized response = %s, want %s", got, want)
	}
}

func TestJSONV2SerializerPreservesDurationNanoseconds(t *testing.T) {
	type response struct {
		HeartbeatPeriod time.Duration `json:"heartbeatPeriod"`
	}

	e := echo.New()
	recorder := httptest.NewRecorder()
	context := e.NewContext(httptest.NewRequest("GET", "/", http.NoBody), recorder)

	err := (jsonV2Serializer{}).Serialize(context, response{HeartbeatPeriod: 5 * time.Second}, "")

	require.NoError(t, err,
		"serialize duration: %v", err)
	{

		got, want := recorder.Body.String(), `{"heartbeatPeriod":5000000000}`
		require.Equal(t, want, got,
			"serialized response = %s, want %s", got, want)
	}
}

func TestJSONV2SerializerUsesStrictV2Decoding(t *testing.T) {
	type requestBody struct {
		Name string `json:"name"`
	}

	t.Run("field names are case sensitive", func(t *testing.T) {
		e := echo.New()
		context := e.NewContext(httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"Name":"arcane"}`)), httptest.NewRecorder())

		var body requestBody
		err := (jsonV2Serializer{}).Deserialize(context, &body)

		require.NoError(t, err,
			"deserialize case-variant field: %v", err)

		require.Empty(t, body.Name,
			"case-variant field populated Name with %q", body.Name)
	})

	for _, test := range []struct {
		name string
		body []byte
	}{
		{name: "duplicate names", body: []byte(`{"name":"first","name":"second"}`)},
		{name: "invalid UTF-8", body: []byte{'{', '"', 'n', 'a', 'm', 'e', '"', ':', '"', 0xff, '"', '}'}},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := echo.New()
			context := e.NewContext(httptest.NewRequest("POST", "/", bytes.NewReader(test.body)), httptest.NewRecorder())

			var body requestBody
			err := (jsonV2Serializer{}).Deserialize(context, &body)
			require.Equal(t, http.StatusBadRequest, echo.StatusCode(err),
				"HTTP status = %d, want %d", echo.StatusCode(err), http.StatusBadRequest)
		})
	}
}

func TestEchoSerializerPreservesUnnormalizedStrings(t *testing.T) {
	router := echo.New()
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":" e\u0301 ","password":" secret "}`))
	ctx := router.NewContext(request, httptest.NewRecorder())
	var result normalizationTestBody
	require.NoError(t, (jsonV2Serializer{}).Deserialize(ctx, &result))
	require.Equal(t, " e\u0301 ", result.Name)
	require.Equal(t, " secret ", result.Password)
}

func TestEchoSerializerAllowsWhitespaceOnlyName(t *testing.T) {
	router := echo.New()
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":" "}`))
	ctx := router.NewContext(request, httptest.NewRecorder())
	var result normalizationTestBody
	err := (jsonV2Serializer{}).Deserialize(ctx, &result)
	require.NoError(t, err)
	require.Equal(t, " ", result.Name)
}
