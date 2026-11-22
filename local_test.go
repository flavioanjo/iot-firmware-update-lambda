package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func newLocalTestServer(t *testing.T, fw []byte) (*httptest.Server, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	store.add("12345", fakeJob{queueID: 1, firmware: fw})
	srv := httptest.NewServer(newLocalMux(newTestHandler(store).Handle, discardLogger()))
	t.Cleanup(srv.Close)
	return srv, store
}

func get(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func TestLocalServerRoutesAndBinaryBodies(t *testing.T) {
	fw := firmwareOf(2000)
	srv, _ := newLocalTestServer(t, fw)

	for _, route := range localDownloadRoutes {
		t.Run(route, func(t *testing.T) {
			resp, body := get(t, srv.URL+route+"?device_id=12345&offset=-1")
			if resp.StatusCode != 200 || len(body) != 20 {
				t.Fatalf("metadata: status %d, %d bytes", resp.StatusCode, len(body))
			}
			if binary.LittleEndian.Uint32(body[:4]) != 2000 {
				t.Errorf("size field = %d", binary.LittleEndian.Uint32(body[:4]))
			}
			sum := md5.Sum(fw)
			if !bytes.Equal(body[4:], sum[:]) {
				t.Errorf("md5 mismatch")
			}

			resp, body = get(t, srv.URL+route+"?device_id=12345&offset=1024&block_size=1024")
			if resp.StatusCode != 200 || !bytes.Equal(body, fw[1024:2000]) {
				t.Fatalf("chunk: status %d, %d bytes", resp.StatusCode, len(body))
			}
			if resp.Header.Get("X-Firmware-Size") != "2000" || resp.Header.Get("Content-Length") != "976" {
				t.Errorf("headers = %v", resp.Header)
			}

			resp, body = get(t, srv.URL+route+"?device_id=12345&offset=2000&block_size=1024")
			if resp.StatusCode != 200 || !strings.HasPrefix(string(body), "*** MD5 = ") {
				t.Fatalf("eof: status %d body %q", resp.StatusCode, body)
			}

			// Simulator health check: the download URL without parameters.
			resp, body = get(t, srv.URL+route)
			if resp.StatusCode != 200 || string(body) != "OK" {
				t.Errorf("health on download route: %d %q", resp.StatusCode, body)
			}
		})
	}
}

func TestLocalServerHealthRoutes(t *testing.T) {
	srv, _ := newLocalTestServer(t, firmwareOf(10))
	for _, route := range localHealthRoutes {
		resp, body := get(t, srv.URL+route)
		if resp.StatusCode != 200 || string(body) != "OK" {
			t.Errorf("%s: %d %q", route, resp.StatusCode, body)
		}
	}
}

func TestLocalServerPreservesStatusCodes(t *testing.T) {
	srv, _ := newLocalTestServer(t, firmwareOf(10))
	cases := map[string]int{
		"/default/DownloadIOTFirmware?device_id=999&offset=0":                404,
		"/default/DownloadIOTFirmware?device_id=12345&offset=abc":            400,
		"/default/DownloadIOTFirmware?device_id=12345&offset=0&block_size=0": 400,
		"/default/DownloadIOTFirmware?device_id=12345&offset=0&offset=5":     400,
		"/unknown": 404,
	}
	for path, want := range cases {
		if resp, body := get(t, srv.URL+path); resp.StatusCode != want {
			t.Errorf("%s: status %d (%q), want %d", path, resp.StatusCode, body, want)
		}
	}
}

func TestLocalServerRejectsOtherMethods(t *testing.T) {
	srv, store := newLocalTestServer(t, firmwareOf(10))
	resp, err := http.Post(srv.URL+"/default/DownloadIOTFirmware?device_id=12345&offset=-1", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, HEAD" {
		t.Errorf("status %d allow %q", resp.StatusCode, resp.Header.Get("Allow"))
	}
	if len(store.statuses) != 0 {
		t.Errorf("POST must not reach the handler")
	}

	head, err := http.Head(srv.URL + "/default/DownloadIOTFirmware?device_id=12345&offset=0")
	if err != nil {
		t.Fatal(err)
	}
	head.Body.Close()
	if head.StatusCode != 200 || head.Header.Get("Content-Length") != "10" {
		t.Errorf("HEAD: %d content-length %q", head.StatusCode, head.Header.Get("Content-Length"))
	}
}

type ctxKey struct{}

func TestLocalAdapterPropagatesContextAndEvent(t *testing.T) {
	var gotCtx context.Context
	var gotEvent events.APIGatewayProxyRequest
	fn := func(ctx context.Context, e events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
		gotCtx, gotEvent = ctx, e
		return events.APIGatewayProxyResponse{StatusCode: 202, Body: "x"}, nil
	}
	mux := newLocalMux(fn, discardLogger())
	req := httptest.NewRequest(http.MethodGet, "/default/download_firmware?device_id=1&offset=0&offset=7", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, "marker"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 202 {
		t.Errorf("status = %d, want 202", rec.Code)
	}
	if gotCtx == nil || gotCtx.Value(ctxKey{}) != "marker" {
		t.Error("request context was not propagated")
	}
	if gotEvent.HTTPMethod != "GET" || gotEvent.Path != "" {
		t.Errorf("event method %q path %q", gotEvent.HTTPMethod, gotEvent.Path)
	}
	if gotEvent.QueryStringParameters["offset"] != "7" || len(gotEvent.MultiValueQueryStringParameters["offset"]) != 2 {
		t.Errorf("query parameters not mapped like API Gateway: %v %v",
			gotEvent.QueryStringParameters, gotEvent.MultiValueQueryStringParameters)
	}
}

func TestLocalAdapterHandlesFailures(t *testing.T) {
	cases := map[string]lambdaFunc{
		"handler error": func(context.Context, events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
			return events.APIGatewayProxyResponse{}, errors.New("boom")
		},
		"invalid base64": func(context.Context, events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
			return events.APIGatewayProxyResponse{StatusCode: 200, Body: "%%%", IsBase64Encoded: true}, nil
		},
	}
	for name, fn := range cases {
		rec := httptest.NewRecorder()
		newLocalMux(fn, discardLogger()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/default/DownloadIOTFirmware", nil))
		if rec.Code != 500 {
			t.Errorf("%s: status %d, want 500", name, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "boom") {
			t.Errorf("%s: internal error leaked", name)
		}
	}
}
