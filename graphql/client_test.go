package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

type staticDoer struct {
	response *http.Response
}

func (d staticDoer) Do(*http.Request) (*http.Response, error) {
	return d.response, nil
}

func makeServer(t *testing.T, responseCode int, responseBody any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(responseCode)
		err := json.NewEncoder(w).Encode(responseBody)
		if err != nil {
			t.Fatalf("Failed to write response: %v", err)
		}
	}))
}

func makeRequest(server *httptest.Server) (*Response, error) {
	client := NewClient(server.URL, server.Client())
	req := &Request{Query: "query { test }"}
	resp := &Response{}

	err := client.MakeRequest(context.Background(), req, resp)
	return resp, err
}

func TestMakeRequestHTTPError(t *testing.T) {
	testCases := []struct {
		expectedError      *HTTPError
		serverResponseBody any
		name               string
		serverResponseCode int
	}{
		{
			name:               "PlainTextError",
			serverResponseCode: http.StatusBadRequest,
			serverResponseBody: "Bad Request",
			expectedError: &HTTPError{
				Response: Response{
					Errors: gqlerror.List{
						&gqlerror.Error{
							Message: "\"Bad Request\"\n",
						},
					},
				},
				StatusCode: http.StatusBadRequest,
			},
		},
		{
			name:               "JSONErrorWithExtensions",
			serverResponseCode: http.StatusTooManyRequests,
			serverResponseBody: map[string]any{
				"errors": []map[string]any{
					{
						"message": "Rate limit exceeded",
						"type":    "RATE_LIMITED",
						"extensions": map[string]any{
							"code": "RATE_LIMIT_EXCEEDED",
						},
					},
				},
			},
			expectedError: &HTTPError{
				Response: Response{
					Errors: gqlerror.List{
						&gqlerror.Error{
							Message: "Rate limit exceeded",
							Extensions: map[string]interface{}{
								"code": "RATE_LIMIT_EXCEEDED",
								"type": "RATE_LIMITED",
							},
						},
					},
				},
				StatusCode: http.StatusTooManyRequests,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			server := makeServer(t, tc.serverResponseCode, tc.serverResponseBody)
			defer server.Close()
			_, err := makeRequest(server)

			assert.Error(t, err)
			var httpErr *HTTPError
			assert.True(t, errors.As(err, &httpErr), "Error should be of type *HTTPError")
			assert.Equal(t, tc.expectedError.Response, httpErr.Response)
			assert.Equal(t, tc.expectedError.StatusCode, httpErr.StatusCode)
		})
	}
}

func TestMakeRequestHTTPErrorHeaders(t *testing.T) {
	testCases := []struct {
		name string
		body string
	}{
		{
			name: "GraphQL error response",
			body: `{"errors":[{"message":"Rate limit exceeded"}]}`,
		},
		{
			name: "invalid JSON response",
			body: "Bad Gateway",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			responseHeaders := make(http.Header)
			responseHeaders.Set("Retry-After", "60")
			responseHeaders.Set("X-RateLimit-Reset", "1752793200")
			responseHeaders.Set("X-RateLimit-Remaining", "0")
			httpResponse := &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     responseHeaders,
				Body:       io.NopCloser(strings.NewReader(tc.body)),
			}
			client := NewClient("https://example.com/graphql", staticDoer{response: httpResponse})

			err := client.MakeRequest(context.Background(), &Request{}, &Response{})
			require.Error(t, err)

			var httpErr *HTTPError
			require.ErrorAs(t, err, &httpErr)
			assert.Equal(t, "60", httpErr.Headers.Get("Retry-After"))
			assert.Equal(t, "1752793200", httpErr.Headers.Get("X-RateLimit-Reset"))
			assert.Equal(t, "0", httpErr.Headers.Get("X-RateLimit-Remaining"))

			responseHeaders.Set("Retry-After", "120")
			assert.Equal(t, "60", httpErr.Headers.Get("Retry-After"))
		})
	}
}

func TestMakeRequestHTTPErrors(t *testing.T) {
	server := makeServer(t, http.StatusOK, map[string]any{
		"errors": []map[string]any{
			{
				"message": "Rate limit exceeded",
				"type":    "RATE_LIMITED",
			},
			{
				"message": "Resource not accessible",
				"type":    "FORBIDDEN",
			},
			{
				"message": "Standard GraphQL error",
				"extensions": map[string]any{
					"code": "STANDARD_ERROR",
				},
			},
			{
				"message": "Top-level type takes precedence",
				"type":    "RATE_LIMITED",
				"extensions": map[string]any{
					"type": "LEGACY_RATE_LIMIT",
				},
			},
		},
	})
	defer server.Close()
	_, err := makeRequest(server)

	assert.Error(t, err)
	var gqlErr gqlerror.List
	assert.True(t, errors.As(err, &gqlErr), "Error should be of type *gqlerror.List")
	assert.Equal(t, gqlerror.List{
		&gqlerror.Error{
			Message: "Rate limit exceeded",
			Extensions: map[string]interface{}{
				"type": "RATE_LIMITED",
			},
		},
		&gqlerror.Error{
			Message: "Resource not accessible",
			Extensions: map[string]interface{}{
				"type": "FORBIDDEN",
			},
		},
		&gqlerror.Error{
			Message: "Standard GraphQL error",
			Extensions: map[string]interface{}{
				"code": "STANDARD_ERROR",
			},
		},
		&gqlerror.Error{
			Message: "Top-level type takes precedence",
			Extensions: map[string]interface{}{
				"type": "RATE_LIMITED",
			},
		},
	}, gqlErr)
}

func TestMakeRequestSuccess(t *testing.T) {
	server := makeServer(t, http.StatusOK, map[string]interface{}{
		"data": map[string]string{"test": "success"},
	})
	defer server.Close()
	resp, err := makeRequest(server)

	assert.NoError(t, err)
	assert.Equal(t, map[string]interface{}{"test": "success"}, resp.Data)
}
